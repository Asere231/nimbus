package keyvault

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"hash"
	"math/big"
	"net/http"
	"strings"
)

// The operations that make a key a key.
//
// Everything else in this package moves data about; this is the part where
// the vault DOES something with material it will not hand over. The private
// half is used here and never serialised, which is the property the whole
// service exists to provide.
//
// The full RSA set is implemented rather than just sign/verify, and the
// reason is a client-facing one: azurerm's azurerm_key_vault_key takes
// key_opts = ["sign","verify","wrapKey","unwrapKey","encrypt","decrypt"],
// which Nimbus stores and returns in the JWK's key_ops. A vault that
// advertises six capabilities and refuses four of them lies about itself --
// the same fault as a curated role that cannot do what its own description
// says. EC keys get sign and verify only, because elliptic curves do not
// encrypt; that is arithmetic, not a gap.
//
// An algorithm Nimbus cannot perform is REFUSED BY NAME. Quietly falling back
// to a default would produce a signature or ciphertext the client believes is
// something it is not, which is the worst outcome available here.

// isCryptoOp reports whether a trailing path segment is one of the verbs.
func isCryptoOp(op string) bool {
	switch op {
	case "sign", "verify", "encrypt", "decrypt", "wrapkey", "unwrapkey":
		return true
	}
	return false
}

// cryptoOp performs one operation with a key.
func (h *Handler) cryptoOp(w http.ResponseWriter, r *http.Request, v *Vault, name, version, op string) string {
	k := v.Key(name)
	if k == nil {
		return h.keyNotFound(w, v, name)
	}
	ver := k.Version(version)
	if ver == nil {
		WriteError(w, 404, "KeyNotFound",
			"A key with (name/id) %s/%s was not found in this key vault.", name, version)
		return ""
	}
	if !ver.Enabled {
		WriteError(w, 403, "Forbidden", "Key %s is disabled.", name)
		return "key is disabled: " + name
	}

	var req struct {
		Alg    string `json:"alg"`
		Value  string `json:"value"`
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		WriteError(w, 400, "BadParameter", "The request body is not a valid %s request: %v", op, err)
		return ""
	}
	if req.Alg == "" {
		WriteError(w, 400, "BadParameter", "A %s request must name an algorithm (alg).", op)
		return ""
	}

	// Azure's value fields are base64url, and `sign` is given a DIGEST that
	// the client has already computed -- not the message. Hashing it again
	// here would produce a signature that verifies against nothing.
	in, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(req.Value, "="))
	if err != nil {
		WriteError(w, 400, "BadParameter", "The value is not base64url: %v", err)
		return ""
	}
	id := h.keyID(v, k.Name, ver.Version)

	switch op {
	case "sign":
		sig, ref := signWith(ver, req.Alg, in)
		if ref != "" {
			WriteError(w, 400, "BadParameter", "%s", ref)
			return "unsupported sign algorithm: " + req.Alg
		}
		writeJSON(w, 200, map[string]any{"kid": id, "value": b64url(sig)})
		return "signed with key " + name

	case "verify":
		digest, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(req.Digest, "="))
		if err != nil {
			WriteError(w, 400, "BadParameter", "The digest is not base64url: %v", err)
			return ""
		}
		ok, ref := verifyWith(ver, req.Alg, digest, in)
		if ref != "" {
			WriteError(w, 400, "BadParameter", "%s", ref)
			return "unsupported verify algorithm: " + req.Alg
		}
		writeJSON(w, 200, map[string]any{"kid": id, "value": ok})
		return "verified with key " + name

	case "encrypt", "wrapkey":
		out, ref := encryptWith(ver, req.Alg, in)
		if ref != "" {
			WriteError(w, 400, "BadParameter", "%s", ref)
			return "unsupported encrypt algorithm: " + req.Alg
		}
		writeJSON(w, 200, map[string]any{"kid": id, "value": b64url(out)})
		return op + " with key " + name

	case "decrypt", "unwrapkey":
		out, ref := decryptWith(ver, req.Alg, in)
		if ref != "" {
			WriteError(w, 400, "BadParameter", "%s", ref)
			return "unsupported decrypt algorithm: " + req.Alg
		}
		writeJSON(w, 200, map[string]any{"kid": id, "value": b64url(out)})
		return op + " with key " + name
	}
	WriteError(w, 501, "NotSupported", "Nimbus does not implement the %q operation.", op)
	return "unimplemented crypto operation: " + op
}

// hashFor maps the algorithm suffix to its hash. Note there is no SHA-1
// option: Azure's RSNULL and the SHA-1 signing algorithms are deprecated and
// nothing current asks for them.
func hashFor(alg string) (crypto.Hash, hash.Hash, int) {
	switch {
	case strings.HasSuffix(alg, "256"):
		return crypto.SHA256, sha256.New(), sha256.Size
	case strings.HasSuffix(alg, "384"):
		return crypto.SHA384, sha512.New384(), sha512.Size384
	case strings.HasSuffix(alg, "512"):
		return crypto.SHA512, sha512.New(), sha512.Size
	}
	return 0, nil, 0
}

func signWith(ver *KeyVersion, alg string, digest []byte) ([]byte, string) {
	h, _, size := hashFor(alg)
	switch {
	case strings.HasPrefix(alg, "RS") && ver.RSA != nil:
		if h == 0 {
			return nil, "Unsupported signing algorithm " + alg + "."
		}
		if len(digest) != size {
			return nil, digestLenRef(alg, size, len(digest))
		}
		sig, err := rsa.SignPKCS1v15(rand.Reader, ver.RSA, h, digest)
		if err != nil {
			return nil, "Signing failed: " + err.Error()
		}
		return sig, ""

	case strings.HasPrefix(alg, "PS") && ver.RSA != nil:
		if h == 0 {
			return nil, "Unsupported signing algorithm " + alg + "."
		}
		if len(digest) != size {
			return nil, digestLenRef(alg, size, len(digest))
		}
		// JOSE's PS* uses a salt the length of the digest, not the default.
		sig, err := rsa.SignPSS(rand.Reader, ver.RSA, h, digest,
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h})
		if err != nil {
			return nil, "Signing failed: " + err.Error()
		}
		return sig, ""

	case strings.HasPrefix(alg, "ES") && ver.EC != nil:
		if h == 0 {
			return nil, "Unsupported signing algorithm " + alg + "."
		}
		// JOSE wants the raw R||S pair, each padded to the curve width -- NOT
		// the ASN.1 DER that crypto/ecdsa.SignASN1 produces. A DER signature
		// here is the single easiest way to make something that looks right
		// and verifies nowhere.
		r, s, err := ecdsa.Sign(rand.Reader, ver.EC, digest)
		if err != nil {
			return nil, "Signing failed: " + err.Error()
		}
		n := (ver.EC.Curve.Params().BitSize + 7) / 8
		return append(padLeft(r.Bytes(), n), padLeft(s.Bytes(), n)...), ""
	}
	return nil, algMismatchRef(alg, ver, "signing")
}

func verifyWith(ver *KeyVersion, alg string, digest, sig []byte) (bool, string) {
	h, _, _ := hashFor(alg)
	switch {
	case strings.HasPrefix(alg, "RS") && ver.RSA != nil:
		if h == 0 {
			return false, "Unsupported verification algorithm " + alg + "."
		}
		return rsa.VerifyPKCS1v15(&ver.RSA.PublicKey, h, digest, sig) == nil, ""
	case strings.HasPrefix(alg, "PS") && ver.RSA != nil:
		if h == 0 {
			return false, "Unsupported verification algorithm " + alg + "."
		}
		return rsa.VerifyPSS(&ver.RSA.PublicKey, h, digest, sig,
			&rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: h}) == nil, ""
	case strings.HasPrefix(alg, "ES") && ver.EC != nil:
		n := (ver.EC.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*n {
			return false, ""
		}
		r := new(big.Int).SetBytes(sig[:n])
		s := new(big.Int).SetBytes(sig[n:])
		return ecdsa.Verify(&ver.EC.PublicKey, digest, r, s), ""
	}
	return false, algMismatchRef(alg, ver, "verification")
}

func encryptWith(ver *KeyVersion, alg string, in []byte) ([]byte, string) {
	if ver.RSA == nil {
		return nil, algMismatchRef(alg, ver, "encryption")
	}
	switch strings.ToUpper(alg) {
	case "RSA1_5":
		out, err := rsa.EncryptPKCS1v15(rand.Reader, &ver.RSA.PublicKey, in)
		return out, errRef(err)
	case "RSA-OAEP":
		out, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, &ver.RSA.PublicKey, in, nil)
		return out, errRef(err)
	case "RSA-OAEP-256":
		out, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &ver.RSA.PublicKey, in, nil)
		return out, errRef(err)
	}
	return nil, "Unsupported encryption algorithm " + alg + "; Nimbus implements RSA1_5, RSA-OAEP and RSA-OAEP-256."
}

func decryptWith(ver *KeyVersion, alg string, in []byte) ([]byte, string) {
	if ver.RSA == nil {
		return nil, algMismatchRef(alg, ver, "decryption")
	}
	switch strings.ToUpper(alg) {
	case "RSA1_5":
		out, err := rsa.DecryptPKCS1v15(rand.Reader, ver.RSA, in)
		return out, errRef(err)
	case "RSA-OAEP":
		out, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, ver.RSA, in, nil)
		return out, errRef(err)
	case "RSA-OAEP-256":
		out, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, ver.RSA, in, nil)
		return out, errRef(err)
	}
	return nil, "Unsupported decryption algorithm " + alg + "; Nimbus implements RSA1_5, RSA-OAEP and RSA-OAEP-256."
}

func errRef(err error) string {
	if err == nil {
		return ""
	}
	return "The operation failed: " + err.Error()
}

func digestLenRef(alg string, want, got int) string {
	return "Algorithm " + alg + " expects a " + itoa(want) + "-byte digest and the value is " +
		itoa(got) + " bytes. Note that `sign` takes a DIGEST the client has already computed, " +
		"not the message."
}

func algMismatchRef(alg string, ver *KeyVersion, what string) string {
	if ver.Kty == "EC" {
		return "Algorithm " + alg + " cannot be used for " + what + " with an EC key. Elliptic " +
			"curve keys sign and verify; they do not encrypt."
	}
	return "Algorithm " + alg + " cannot be used for " + what + " with an " + ver.Kty + " key."
}
