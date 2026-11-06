package core

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"
	"time"
)

// leaf parses the server certificate EnsureCerts wrote into dir.
func leaf(t *testing.T, c *Certs) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(c.Cert)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(data) // the leaf is first in the chain file
	if blk == nil {
		t.Fatal("server certificate is not PEM")
	}
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return crt
}

// Apple's Security.framework rejects a TLS server certificate whose validity
// SPAN is 825 days or more, with the famously unhelpful message
//
//	x509: "localhost" certificate is not standards compliant
//
// Measured on macOS 27 (arm64): an 824-day span is accepted, 825 is not. The
// span is NotAfter-NotBefore, so backdating NotBefore for clock skew eats into
// the budget — that is what originally broke us, by exactly one hour.
//
// Apple's *published* rule for certificates issued after 2020-09-01 is 398
// days, which is stricter than what the platform currently enforces. We honour
// the published rule so a future tightening cannot break Nimbus.
const appleMaxValiditySpan = 398 * 24 * time.Hour

func TestServerCertMeetsApplePlatformRules(t *testing.T) {
	c, err := EnsureCerts(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	crt := leaf(t, c)

	span := crt.NotAfter.Sub(crt.NotBefore)
	if span > appleMaxValiditySpan {
		t.Errorf("validity span is %.0f days; macOS rejects >= 825 and Apple documents a 398-day maximum.\n"+
			"NotBefore=%s NotAfter=%s\n"+
			"Note the span includes any backdating of NotBefore.",
			span.Hours()/24, crt.NotBefore, crt.NotAfter)
	}

	// NotBefore must still be in the past, or a client whose clock is a little
	// behind ours rejects the certificate as not yet valid.
	if !crt.NotBefore.Before(time.Now()) {
		t.Errorf("NotBefore = %s is not in the past; clock skew will break clients", crt.NotBefore)
	}

	// The remaining Apple requirements, so a future edit cannot regress them.
	var serverAuth bool
	for _, u := range crt.ExtKeyUsage {
		if u == x509.ExtKeyUsageServerAuth {
			serverAuth = true
		}
	}
	if !serverAuth {
		t.Error("missing ExtKeyUsage serverAuth")
	}
	if len(crt.DNSNames) == 0 {
		t.Error("no SAN DNS names: Apple ignores the legacy CommonName")
	}
	var hasLocalhost bool
	for _, n := range crt.DNSNames {
		if n == "localhost" {
			hasLocalhost = true
		}
	}
	if !hasLocalhost {
		t.Errorf("SAN DNS names %v do not include localhost", crt.DNSNames)
	}
	if _, ok := crt.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Errorf("public key is %T; Apple requires ECDSA P-256/384 or RSA >= 2048", crt.PublicKey)
	}
	if crt.SignatureAlgorithm != x509.ECDSAWithSHA256 && crt.SignatureAlgorithm != x509.ECDSAWithSHA384 {
		t.Errorf("signature algorithm %v: Apple requires SHA-256 or stronger", crt.SignatureAlgorithm)
	}
}

func ca(t *testing.T, c *Certs) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(c.CACert)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(data)
	crt, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return crt
}

func TestEnsureCertsIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	beforeLeaf, beforeCA := leaf(t, first), ca(t, first)

	again, err := EnsureCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if leaf(t, again).SerialNumber.Cmp(beforeLeaf.SerialNumber) != 0 {
		t.Error("EnsureCerts reissued a valid server certificate")
	}
	if ca(t, again).SerialNumber.Cmp(beforeCA.SerialNumber) != 0 {
		t.Error("EnsureCerts reissued a valid CA")
	}
}

// The server certificate is short-lived (see certValidity), so it will need
// reissuing about once a year. The CA must survive that: it is trusted in the
// OS keychain, and replacing it makes every client fail until the user runs the
// `nimbus cert` trust command again. Reissuing the leaf alone needs no re-trust.
func TestReissuingTheLeafKeepsTheTrustedCA(t *testing.T) {
	dir := t.TempDir()
	first, err := EnsureCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	beforeLeaf, beforeCA := leaf(t, first), ca(t, first)

	// Simulate an expired or missing leaf.
	if err := os.Remove(first.Cert); err != nil {
		t.Fatal(err)
	}

	again, err := EnsureCerts(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ca(t, again).SerialNumber.Cmp(beforeCA.SerialNumber) != 0 {
		t.Error("reissuing the server certificate replaced the CA; the user would have to re-trust it")
	}
	newLeaf := leaf(t, again)
	if newLeaf.SerialNumber.Cmp(beforeLeaf.SerialNumber) == 0 {
		t.Error("the server certificate was not reissued")
	}
	// And the new leaf must actually chain to the preserved CA.
	pool := x509.NewCertPool()
	pool.AddCert(beforeCA)
	if _, err := newLeaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost"}); err != nil {
		t.Errorf("reissued leaf does not chain to the existing CA: %v", err)
	}
}

func TestExpiringCertIsReissued(t *testing.T) {
	if !needsReissue(time.Now().Add(-time.Hour)) {
		t.Error("an already-expired certificate must be reissued")
	}
	if !needsReissue(time.Now().Add(24 * time.Hour)) {
		t.Error("a certificate expiring tomorrow must be reissued")
	}
	if needsReissue(time.Now().Add(certValidity)) {
		t.Error("a freshly issued certificate must not be reissued")
	}
}

// The storage data plane lives at {account}.{service}.storage.nimbus.localhost.
// A cloud's data-plane names are passed in rather than hard-coded here --
// they are that cloud's business, not the core's. Azure's own coverage is
// checked in internal/azure, which is where those names live. This is the
// mechanism: whatever is handed to EnsureCerts ends up on the certificate.
//
// A wildcard matches exactly ONE label, so a nested name needs a wildcard at
// each level, and that is the caller's job to get right.
func TestServerCertCoversTheNamesItIsGiven(t *testing.T) {
	c, err := EnsureCerts(t.TempDir(), "data.example.localhost", "*.data.example.localhost")
	if err != nil {
		t.Fatal(err)
	}
	crt := leaf(t, c)

	for _, host := range []string{
		"localhost",
		"anything.localhost",
		"data.example.localhost",
		"bucket.data.example.localhost",
	} {
		if err := crt.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %s: %v\n  SANs: %v", host, err, crt.DNSNames)
		}
	}
}

// A certificate issued before a name existed must be replaced on the next
// start, or upgrading users get a TLS failure with no hint that deleting a
// file fixes it. The CA must survive, so no re-trust is needed.
func TestCertMissingNewNamesIsReissued(t *testing.T) {
	want := dnsNames([]string{"new.localhost"})
	if coversAllNames(&x509.Certificate{DNSNames: []string{"localhost", "*.localhost"}}, want) {
		t.Error("a certificate without the new name must be treated as needing reissue")
	}
	if !coversAllNames(&x509.Certificate{DNSNames: want}, want) {
		t.Error("a certificate with every name must not be reissued")
	}
}

// Adding a cloud adds names; dnsNames must not duplicate the base ones or
// drop anything, since the result is what goes on the certificate.
func TestDNSNamesMergesWithoutDuplicates(t *testing.T) {
	got := dnsNames([]string{"a.localhost", "a.localhost", "localhost", ""})
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	if seen["a.localhost"] != 1 || seen["localhost"] != 1 {
		t.Errorf("names = %v, want each one once", got)
	}
	if seen[""] != 0 {
		t.Error("an empty name must not reach the certificate")
	}
	for _, base := range baseDNSNames {
		if seen[base] == 0 {
			t.Errorf("the base name %q was dropped", base)
		}
	}
}
