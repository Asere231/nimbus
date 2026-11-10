package aws

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"strings"
)

// Key pairs.
//
// `aws_key_pair` does NOT call CreateKeyPair. Terraform always supplies its
// own public key -- the resource has no mode where AWS generates one and
// hands back a private key -- so the action is **ImportKeyPair**, with the
// key base64-encoded in PublicKeyMaterial. Measured against aws provider
// 5.100.0:
//
//	Action=ImportKeyPair&KeyName=measure-key&PublicKeyMaterial=c3NoLWVkMjU1...
//
// A key pair is also the one EC2 resource addressed by NAME as well as by
// id: DescribeKeyPairs takes KeyName.N and DeleteKeyPair takes KeyName.
// Everything else here is found by its id alone.
//
// Nothing ever uses these keys. There is no sshd in an instance, because
// Nimbus is stdlib-only and cannot speak SSH; the public key is written to
// the instance's authorized_keys so the machine looks the way its
// configuration says it does, and `nimbus ssh` gives a shell instead.

const typeKeyPair = "AWS::EC2::KeyPair"

type keyPair struct {
	KeyPairID   string `xml:"keyPairId" json:"keyPairId"`
	KeyName     string `xml:"keyName" json:"keyName"`
	KeyType     string `xml:"keyType" json:"keyType"`
	Fingerprint string `xml:"keyFingerprint" json:"keyFingerprint"`
	// PublicKey is what was imported, decoded. Real AWS does not return it
	// from DescribeKeyPairs unless asked with IncludePublicKey, but it has
	// to be kept: it is what an instance's authorized_keys is built from.
	PublicKey string `xml:"publicKey,omitempty" json:"publicKey"`
	TagSet    []tag  `xml:"tagSet>item" json:"tagSet"`
}

func (k *keyPair) id() string      { return k.KeyPairID }
func (k *keyPair) tags() []tag     { return k.TagSet }
func (k *keyPair) setTags(t []tag) { k.TagSet = t }
func (k *keyPair) field(name string) ([]string, bool) {
	switch name {
	case "key-pair-id":
		return []string{k.KeyPairID}, true
	case "key-name":
		return []string{k.KeyName}, true
	case "fingerprint":
		return []string{k.Fingerprint}, true
	}
	return nil, false
}

type importKeyPairResponse struct {
	XMLName     xml.Name `xml:"ImportKeyPairResponse"`
	Xmlns       string   `xml:"xmlns,attr"`
	RequestID   string   `xml:"requestId"`
	KeyName     string   `xml:"keyName"`
	KeyPairID   string   `xml:"keyPairId"`
	Fingerprint string   `xml:"keyFingerprint"`
	TagSet      []tag    `xml:"tagSet>item,omitempty"`
}

func (s *Server) importKeyPair(w http.ResponseWriter, p params) *apiError {
	name := p.str("KeyName")
	if name == "" {
		return errorf(400, "MissingParameter", "the request must contain the parameter KeyName")
	}
	material := p.str("PublicKeyMaterial")
	if material == "" {
		return errorf(400, "MissingParameter",
			"the request must contain the parameter PublicKeyMaterial")
	}
	// A name is the identity of a key pair, so importing over one would
	// lose the key an instance was already launched with.
	if existing := keyPairByName(s, name); existing != nil {
		return errorf(400, "InvalidKeyPair.Duplicate", "The keypair %q already exists", name)
	}

	// The provider base64-encodes the key text. Real AWS also accepts it
	// raw, so a client that did not encode is taken at its word rather
	// than refused for a difference nobody would debug.
	decoded, err := base64.StdEncoding.DecodeString(material)
	public := strings.TrimSpace(string(decoded))
	if err != nil || public == "" {
		public = strings.TrimSpace(material)
	}

	k := &keyPair{
		KeyPairID:   newID("key"),
		KeyName:     name,
		KeyType:     keyTypeOf(public),
		Fingerprint: fingerprint(public),
		PublicKey:   public,
		TagSet:      readTagSpecifications(p),
	}
	if err := s.save(k, typeKeyPair, name, ""); err != nil {
		return errorf(500, "InternalError", "storing the key pair: %v", err)
	}
	writeXML(w, 200, importKeyPairResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(),
		KeyName: k.KeyName, KeyPairID: k.KeyPairID, Fingerprint: k.Fingerprint,
		TagSet: k.TagSet,
	})
	return nil
}

type describeKeyPairsResponse struct {
	XMLName   xml.Name  `xml:"DescribeKeyPairsResponse"`
	Xmlns     string    `xml:"xmlns,attr"`
	RequestID string    `xml:"requestId"`
	Keys      []keyPair `xml:"keySet>item"`
}

func (s *Server) describeKeyPairs(w http.ResponseWriter, p params) *apiError {
	all := list[keyPair](s, typeKeyPair)

	// KeyName.N and KeyPairId.N narrow the answer, and a name that does not
	// exist is an ERROR with this kind's own code rather than an empty
	// list: the provider matches on the code to tell "already gone" from
	// "something is wrong".
	wantNames := p.list("KeyName", "")
	wantIDs := p.list("KeyPairId", "")
	fs := readFilters(p)

	out := []keyPair{}
	for _, k := range all {
		k := k
		if len(wantNames) > 0 && !contains(wantNames, k.KeyName) {
			continue
		}
		if len(wantIDs) > 0 && !contains(wantIDs, k.KeyPairID) {
			continue
		}
		if !matches(&k, fs) {
			continue
		}
		out = append(out, k)
	}
	// Asked for by name or id and not found is an ERROR with this kind's
	// own code, not an empty list.
	if len(wantNames)+len(wantIDs) > 0 && len(out) == 0 {
		missing := append(append([]string{}, wantNames...), wantIDs...)[0]
		return errorf(400, "InvalidKeyPair.NotFound", "The key pair %q does not exist", missing)
	}

	writeXML(w, 200, describeKeyPairsResponse{
		Xmlns: namespaceFor("ec2"), RequestID: requestID(), Keys: out,
	})
	return nil
}

// deleteKeyPair takes a NAME, which is what the provider sends. Deleting
// one that is not there succeeds: that is what makes a destroy idempotent,
// the same split an S3 object has.
func (s *Server) deleteKeyPair(w http.ResponseWriter, p params) *apiError {
	name, id := p.str("KeyName"), p.str("KeyPairId")
	if name == "" && id == "" {
		return errorf(400, "MissingParameter",
			"the request must contain the parameter KeyName or KeyPairId")
	}
	k := keyPairByName(s, name)
	if k == nil && id != "" {
		k, _ = load(s, id, &keyPair{})
	}
	if k != nil && k.KeyPairID != "" {
		if _, err := s.store.Delete(k.KeyPairID); err != nil {
			return errorf(500, "InternalError", "deleting the key pair: %v", err)
		}
	}
	writeEmpty(w, "DeleteKeyPairResponse")
	return nil
}

func keyPairByName(s *Server, name string) *keyPair {
	if name == "" {
		return nil
	}
	for _, k := range list[keyPair](s, typeKeyPair) {
		if k.KeyName == name {
			k := k
			return &k
		}
	}
	return nil
}

// keyTypeOf reads the algorithm out of an SSH public key's first field.
func keyTypeOf(public string) string {
	switch {
	case strings.HasPrefix(public, "ssh-ed25519"):
		return "ed25519"
	case strings.HasPrefix(public, "ecdsa-"):
		return "ecdsa"
	default:
		return "rsa"
	}
}

// fingerprint is DERIVED from the key material, because the provider reads
// it back on every refresh and a value that differed between reads would be
// a plan that never settles -- the Content-MD5 lesson generalised.
//
// It is NOT the fingerprint real AWS computes. AWS uses MD5 of the DER for
// an imported RSA key and base64 SHA-256 for an ed25519 one, and getting
// from an SSH public key to its DER needs an SSH wire-format parser the
// standard library does not have. So this is stable, unique and
// AWS-shaped -- 16 colon-separated octets -- and differs from the real
// cloud's, exactly like uniqueString(). Only something comparing against a
// value computed in real AWS is affected.
func fingerprint(public string) string {
	sum := sha256.Sum256([]byte("nimbus/aws/keypair/" + public))
	hexed := hex.EncodeToString(sum[:16])
	var b strings.Builder
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexed[i : i+2])
	}
	return b.String()
}
