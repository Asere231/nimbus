package core

import (
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"os"
	"runtime"
	"testing"
)

// The service-account signing key is the one key in Nimbus whose
// signature genuinely decides something: IRSA's claim is that a pod's
// identity cannot be forged, and this key is all that stands behind it.

func TestTheSigningKeyIsStableAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh load over the same directory is a restart.
	second, err := EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID() != second.KeyID() {
		t.Error("the key id changed across a restart, so every issued token " +
			"would stop verifying and a cached JWKS would be wrong")
	}
	if first.RSAPublicKey().N.Cmp(second.RSAPublicKey().N) != 0 {
		t.Error("a different key was generated on the second load")
	}
}

// The private half is a SECRET in the ordinary sense -- anything holding
// it can mint a pod identity -- and is the one file in the data directory
// that should say so.
func TestThePrivateKeyIsNotWorldReadable(t *testing.T) {
	// Windows has no Unix permission bits: a file mode there drives only
	// the read-only flag, so an assertion about 0600 has to be skipped
	// rather than weakened everywhere. Note `runtime.GOOS` and not an
	// environment variable -- nothing sets `GOOS` in a running test, so a
	// guard reading it can never fire, which is how this reached CI.
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	key, err := EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(key.Private)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("the signing key is mode %o; it should not be readable by anyone else", mode)
	}
	// The public half is meant to be read, including by the k3s node.
	pub, err := os.Stat(key.Public)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Mode().Perm()&0o044 == 0 {
		t.Errorf("the public key is mode %o; it has to be readable", pub.Mode().Perm())
	}
}

// The JWKS has to be the shape an OIDC consumer reads, and the integers
// base64url with NO padding -- JOSE's rule for every number it carries.
func TestTheJWKSIsTheShapeAConsumerExpects(t *testing.T) {
	dir := t.TempDir()
	key, err := EnsureSigningKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	doc := key.JWKS()
	keys, _ := doc["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("the JWKS has %d keys, want 1", len(keys))
	}
	jwk, _ := keys[0].(map[string]any)
	for field, want := range map[string]string{
		"use": "sig", "kty": "RSA", "alg": "RS256",
	} {
		if jwk[field] != want {
			t.Errorf("%s is %v, want %q", field, jwk[field], want)
		}
	}
	if jwk["kid"] != key.KeyID() || jwk["kid"] == "" {
		t.Errorf("the JWKS kid %v does not match the key's %q", jwk["kid"], key.KeyID())
	}

	// n and e must decode back to the real public key. A padded or
	// standard-alphabet encoding parses as a DIFFERENT number, and the
	// failure is a signature that will not verify -- naming nothing.
	n, err := base64.RawURLEncoding.DecodeString(jwk["n"].(string))
	if err != nil {
		t.Fatalf("n is not unpadded base64url: %v", err)
	}
	e, err := base64.RawURLEncoding.DecodeString(jwk["e"].(string))
	if err != nil {
		t.Fatalf("e is not unpadded base64url: %v", err)
	}
	rebuilt := &rsa.PublicKey{
		N: new(big.Int).SetBytes(n),
		E: int(new(big.Int).SetBytes(e).Int64()),
	}
	if rebuilt.N.Cmp(key.RSAPublicKey().N) != 0 || rebuilt.E != key.RSAPublicKey().E {
		t.Error("the JWKS does not rebuild the key it claims to publish")
	}
	for _, field := range []string{"n", "e"} {
		if s := jwk[field].(string); s == "" {
			t.Errorf("%s is empty", field)
		} else if containsAny(s, "=+/") {
			t.Errorf("%s uses padding or the standard alphabet: %q", field, s)
		}
	}
}

func containsAny(s, chars string) bool {
	for _, c := range chars {
		for _, r := range s {
			if r == c {
				return true
			}
		}
	}
	return false
}
