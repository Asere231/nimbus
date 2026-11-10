package aws

import (
	"encoding/base64"
	"encoding/xml"
	"strings"
	"testing"
)

const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJnimbustestkeyXXXXXXXXXXXXXXXXXXXXXXXX nimbus@test"

func importKey(t *testing.T, s *Server, name, material string) []byte {
	t.Helper()
	return mustEC2(t, s, "Action=ImportKeyPair&KeyName="+name+
		"&PublicKeyMaterial="+base64.StdEncoding.EncodeToString([]byte(material)))
}

// `aws_key_pair` does NOT call CreateKeyPair. Terraform always supplies its
// own public key -- the resource has no mode where AWS generates one -- so
// the action is ImportKeyPair, with the key base64-encoded in
// PublicKeyMaterial. Measured against aws provider 5.100.0:
//
//	Action=ImportKeyPair&KeyName=measure-key&PublicKeyMaterial=c3NoLWVkMjU1...
func TestImportKeyPairIsWhatTerraformActuallyCalls(t *testing.T) {
	s := newTestServer(t)
	body := importKey(t, s, "web-key", testPubKey)

	var got struct {
		XMLName     xml.Name `xml:"ImportKeyPairResponse"`
		KeyName     string   `xml:"keyName"`
		KeyPairID   string   `xml:"keyPairId"`
		Fingerprint string   `xml:"keyFingerprint"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if got.KeyName != "web-key" {
		t.Errorf("keyName = %q", got.KeyName)
	}
	if !strings.HasPrefix(got.KeyPairID, "key-0") {
		t.Errorf("keyPairId = %q, want the key-0 prefix of a modern id", got.KeyPairID)
	}
	if got.Fingerprint == "" {
		t.Error("no keyFingerprint; the provider stores it and would see a permanent diff")
	}
}

// The fingerprint is DERIVED from the key material, not generated. The
// provider reads it back on every refresh, so a value that differs between
// reads is a plan that never settles -- the Content-MD5 lesson again.
//
// It is NOT the fingerprint real AWS would compute: that is MD5 of the DER
// for an RSA key and base64 SHA-256 for ed25519, and parsing an SSH public
// key needs an SSH parser the standard library does not have. Like
// uniqueString, it is stable and unique here and differs from the real
// cloud's, which only matters to something comparing against real AWS.
func TestAKeyFingerprintIsDerivedSoItNeverChanges(t *testing.T) {
	s := newTestServer(t)

	first := fingerprintOf(t, importKey(t, s, "k1", testPubKey))
	for i := 0; i < 3; i++ {
		body := mustEC2(t, s, "Action=DescribeKeyPairs&KeyName.1=k1")
		var got struct {
			Keys []struct {
				Fingerprint string `xml:"keyFingerprint"`
			} `xml:"keySet>item"`
		}
		if err := xml.Unmarshal(body, &got); err != nil || len(got.Keys) != 1 {
			t.Fatalf("describe %d: %v\n%s", i, err, body)
		}
		if got.Keys[0].Fingerprint != first {
			t.Fatalf("fingerprint changed between reads: %q then %q", first, got.Keys[0].Fingerprint)
		}
	}

	// Two different keys must not share a fingerprint, or it says nothing.
	other := fingerprintOf(t, importKey(t, s, "k2", "ssh-rsa AAAAB3NzaC1yc2EAAAAsomethingelse nimbus@test"))
	if other == first {
		t.Error("two different keys got the same fingerprint")
	}
}

func fingerprintOf(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		Fingerprint string `xml:"keyFingerprint"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	return got.Fingerprint
}

// A key pair is addressed by NAME, not only by id -- unlike every other
// EC2 resource. DescribeKeyPairs takes KeyName.N, and DeleteKeyPair takes
// KeyName, which is what the provider sends.
func TestKeyPairsAreAddressedByName(t *testing.T) {
	s := newTestServer(t)
	importKey(t, s, "keep", testPubKey)
	importKey(t, s, "drop", testPubKey)

	if code, out := ec2(t, s, "Action=DeleteKeyPair&KeyName=drop"); code != 200 {
		t.Fatalf("delete by name = %d:\n%s", code, out)
	}

	body := mustEC2(t, s, "Action=DescribeKeyPairs")
	var got struct {
		Keys []struct {
			KeyName string `xml:"keyName"`
		} `xml:"keySet>item"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("%v\n%s", err, body)
	}
	if len(got.Keys) != 1 || got.Keys[0].KeyName != "keep" {
		t.Errorf("after deleting \"drop\", DescribeKeyPairs = %+v", got.Keys)
	}
}

// A describe for a key pair that is not there must ERROR with that kind's
// own code, not answer an empty list: the provider matches on the code to
// tell "already gone" from "something is wrong", and the wrong one makes a
// destroy retry until it times out.
func TestDescribingAMissingKeyPairErrorsWithItsOwnCode(t *testing.T) {
	s := newTestServer(t)
	code, out := ec2(t, s, "Action=DescribeKeyPairs&KeyName.1=nope")
	if code != 400 {
		t.Fatalf("status = %d, want 400:\n%s", code, out)
	}
	if !strings.Contains(string(out), "InvalidKeyPair.NotFound") {
		t.Errorf("error should be InvalidKeyPair.NotFound:\n%s", out)
	}

	// Deleting one that is not there SUCCEEDS, which is what makes a
	// destroy idempotent -- the same split as an S3 object.
	if code, out := ec2(t, s, "Action=DeleteKeyPair&KeyName=nope"); code != 200 {
		t.Errorf("deleting a missing key pair = %d, want 200:\n%s", code, out)
	}
}

// Importing a key twice is a conflict, as it is in real AWS: a name is the
// identity of a key pair, so silently replacing one would lose the key an
// instance was launched with.
func TestImportingTheSameKeyNameTwiceIsRefused(t *testing.T) {
	s := newTestServer(t)
	importKey(t, s, "dup", testPubKey)
	code, out := ec2(t, s, "Action=ImportKeyPair&KeyName=dup&PublicKeyMaterial="+
		base64.StdEncoding.EncodeToString([]byte(testPubKey)))
	if code != 400 || !strings.Contains(string(out), "InvalidKeyPair.Duplicate") {
		t.Errorf("re-importing gave %d:\n%s", code, out)
	}
}
