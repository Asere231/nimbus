package core

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
)

// The service-account signing key: the one key in Nimbus whose signature
// genuinely decides something.
//
// Every other credential here is taken on faith -- `internal/aws/sigv4.go`
// says so plainly: "the signature is the routing table", because a local
// emulator rejecting a SigV4 signature could only ever be reporting a
// clock skew. A service-account token is different. IRSA's whole claim is
// that a pod's identity cannot be forged, and the only thing standing
// behind that is this key: k3s signs projected tokens with the private
// half, Nimbus publishes the public half as a JWKS, and
// AssumeRoleWithWebIdentity verifies against it.
//
// RSA rather than the P-256 the certificates use. JWKS and the JOSE
// `RS256` algorithm are what every Kubernetes OIDC consumer expects, and
// an EC key would mean `ES256` and a JWK shape fewer things read. The
// Key Vault work in Milestone 23 already proved both shapes in this
// codebase; this picks the one the ecosystem assumes.

// SigningKey is the key pair, and where its halves live on disk.
type SigningKey struct {
	// Private is a PKCS#8 PEM file. k3s is given this path with
	// --service-account-signing-key-file, which it reads AT STARTUP --
	// so it has to exist before the container does, the same rule the
	// token webhook's kubeconfig taught.
	Private string
	// Public is a PKIX PEM file, for --service-account-key-file.
	Public string

	key *rsa.PrivateKey
}

// EnsureSigningKey loads the key pair from dir, creating it if it is not
// there. Stable across restarts, because a token signed before a restart
// must still verify after one -- and because the OIDC provider resource a
// client created records a thumbprint of what it saw.
func EnsureSigningKey(dir string) (*SigningKey, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &SigningKey{
		Private: filepath.Join(dir, "sa-signer-key.pem"),
		Public:  filepath.Join(dir, "sa-signer.pub.pem"),
	}
	if key, err := loadSigningKey(s.Private); err == nil {
		s.key = key
		return s, nil
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generating the service-account signing key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	// 0600: this is the one file in the data directory that is a secret
	// in the ordinary sense. Anything holding it can mint a pod identity.
	if err := os.WriteFile(s.Private, pem.EncodeToMemory(
		&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.Public, pem.EncodeToMemory(
		&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644); err != nil {
		return nil, err
	}
	s.key = key
	return s, nil
}

func loadSigningKey(path string) (*rsa.PrivateKey, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, fmt.Errorf("%s is not a PEM file", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s does not hold an RSA key", path)
	}
	return key, nil
}

// Public key material, for the JWKS and for verification.
func (s *SigningKey) RSAPublicKey() *rsa.PublicKey { return &s.key.PublicKey }

// KeyID is the `kid` the JWKS advertises and a token carries.
//
// DERIVED from the key, not generated: a kid that changed between reads
// would make every already-issued token unverifiable, and a client caches
// a JWKS. The same rule as the AKS kubeconfig and Content-MD5 -- a value
// something else compares between reads must not move.
//
// Kubernetes computes it as the base64url of the SHA-256 of the DER
// public key, which is what a consumer matching tokens to keys expects.
func (s *SigningKey) KeyID() string {
	der, err := x509.MarshalPKIXPublicKey(&s.key.PublicKey)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(der)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// JWKS is the public key as the document an OIDC consumer fetches.
//
// `n` and `e` are base64url of the raw big-endian bytes with NO padding,
// which is JOSE's rule for every integer it carries -- the same rule an
// EC key's fixed-width x and y follow in Milestone 23's Key Vault work.
func (s *SigningKey) JWKS() map[string]any {
	pub := &s.key.PublicKey
	return map[string]any{"keys": []any{map[string]any{
		"use": "sig",
		"kty": "RSA",
		"kid": s.KeyID(),
		"alg": "RS256",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}
