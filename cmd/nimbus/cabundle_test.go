package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The combined CA bundle.
//
// `REQUESTS_CA_BUNDLE` REPLACES the trust store rather than adding to it,
// so while it pointed at the Nimbus CA alone `az` could not reach any
// real host -- `az bicep install` and `az upgrade` failed with an SSL
// error blaming the network. Measured both ways against pypi.org: 200
// with the combined bundle, a URLError with the Nimbus CA alone.

func writeCA(t *testing.T, dir, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "certs", "nimbus-ca.pem")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTheCombinedBundleKeepsBothTrustStores(t *testing.T) {
	home := t.TempDir()
	ca := writeCA(t, home, "-----BEGIN CERTIFICATE-----\nNIMBUS\n-----END CERTIFICATE-----\n")

	// A base store deliberately NOT ending in a newline: a bundle that
	// ran its last line into ours would produce a certificate neither
	// side can parse, and nothing would say so.
	original := findBaseTrustStore
	t.Cleanup(func() { findBaseTrustStore = original })
	findBaseTrustStore = func() ([]byte, string) {
		return []byte("-----BEGIN CERTIFICATE-----\nMOZILLA\n-----END CERTIFICATE-----"), "a test"
	}

	path, note := writeCombinedBundle(home, ca)
	if path == ca {
		t.Fatalf("the combined bundle was not written: %s", note)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "MOZILLA") {
		t.Error("the base trust store is missing, so az could not reach a real host")
	}
	if !strings.Contains(text, "NIMBUS") {
		t.Error("the Nimbus CA is missing, so az could not reach Nimbus")
	}
	if strings.Count(text, "BEGIN CERTIFICATE") != 2 {
		t.Errorf("expected two certificates, got %d", strings.Count(text, "BEGIN CERTIFICATE"))
	}
	// A base without a trailing newline must not run into ours.
	if strings.Contains(text, "-----END CERTIFICATE----------BEGIN CERTIFICATE-----") {
		t.Error("the two bundles were joined without a newline between them")
	}
	// Ours LAST, which is the order the comment promises.
	if strings.Index(text, "MOZILLA") > strings.Index(text, "NIMBUS") {
		t.Error("the Nimbus CA should come after the base store")
	}
}

// With no base store there is nothing to combine, and the fallback has to
// SAY so: silently writing a bundle that trusts only us, under a name
// that promises otherwise, would reintroduce the problem with a
// reassuring filename on top.
func TestNoBaseTrustStoreFallsBackLoudly(t *testing.T) {
	home := t.TempDir()
	ca := writeCA(t, home, "-----BEGIN CERTIFICATE-----\nNIMBUS\n-----END CERTIFICATE-----\n")

	original := findBaseTrustStore
	t.Cleanup(func() { findBaseTrustStore = original })
	findBaseTrustStore = func() ([]byte, string) { return nil, "" }

	path, note := writeCombinedBundle(home, ca)
	if path != ca {
		t.Errorf("with no base store the path should fall back to the CA, got %s", path)
	}
	if note == "" {
		t.Fatal("the fallback said nothing")
	}
	for _, want := range []string{"Nimbus CA alone", "real hosts"} {
		if !strings.Contains(note, want) {
			t.Errorf("the warning does not mention %q: %s", want, note)
		}
	}
	// And no file was left behind under the combined name, which would
	// be read as a working bundle by anyone who found it later.
	if _, err := os.Stat(combinedBundlePath(home)); err == nil {
		t.Error("a Nimbus-only bundle was written under the combined name")
	}
}

// A missing CA is reported rather than producing a bundle that silently
// does not trust Nimbus.
func TestAMissingNimbusCAIsReported(t *testing.T) {
	home := t.TempDir()
	path, note := writeCombinedBundle(home, filepath.Join(home, "certs", "absent.pem"))
	if !strings.Contains(note, "could not read the Nimbus CA") {
		t.Errorf("the warning is %q", note)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a bundle was written from a CA that is not there")
	}
}
