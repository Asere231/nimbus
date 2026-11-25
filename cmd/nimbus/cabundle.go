package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// A CA bundle that trusts Nimbus AND the rest of the internet.
//
// `REQUESTS_CA_BUNDLE` REPLACES the trust store rather than adding to it,
// which is the whole reason this file exists. While it points at the
// Nimbus CA alone, `az` cannot reach any real host: `az bicep install`,
// `az upgrade` and anything else that downloads fail with an SSL error
// that blames the network. The same trap caught `az bicep install` in
// Milestone 5, and the workaround until now was "use a shell without the
// variable set", which is a thing to remember rather than a fix.
//
// So `nimbus az register` writes a COMBINED bundle -- whatever az already
// trusted, plus ours -- and points at that.
//
// The failure mode is deliberately LOUD. If no base bundle can be found,
// the combined file is not written and the caller falls back to the
// Nimbus CA alone with a warning, because silently producing a
// Nimbus-only bundle under a name that promises otherwise would
// reintroduce exactly the problem this removes, with a reassuring
// filename on top.

// combinedBundlePath is where the combined file goes: beside the CA it is
// built from, so `nimbus cert` and this agree about where trust lives.
func combinedBundlePath(home string) string {
	return filepath.Join(home, "certs", "nimbus-ca-bundle.pem")
}

// writeCombinedBundle builds the combined bundle and returns its path.
//
// On failure it returns the Nimbus CA's own path and a reason, so the
// caller can say what was lost rather than pretending.
func writeCombinedBundle(home, nimbusCA string) (path string, warning string) {
	ours, err := os.ReadFile(nimbusCA)
	if err != nil {
		return nimbusCA, fmt.Sprintf("could not read the Nimbus CA at %s: %v", nimbusCA, err)
	}
	base, from := findBaseTrustStore()
	if base == nil {
		return nimbusCA, "could not find the CA bundle az already trusts, so " +
			"REQUESTS_CA_BUNDLE will point at the Nimbus CA alone -- az will " +
			"not be able to reach real hosts while it is set"
	}

	// Ours LAST, separated by the comment line -- whose own leading
	// newline is what keeps a base bundle that ends without one from
	// running into our first line. An explicit newline here as well was
	// redundant, and an injection removing it broke no test, which is
	// the honest signal that it was not doing anything.
	var out []byte
	out = append(out, base...)
	out = append(out, []byte("\n# Nimbus local CA, added by `nimbus az register`\n")...)
	out = append(out, ours...)

	combined := combinedBundlePath(home)
	if err := os.WriteFile(combined, out, 0o644); err != nil {
		return nimbusCA, fmt.Sprintf("could not write %s: %v", combined, err)
	}
	return combined, "built from " + from
}

// findBaseTrustStore is a variable so a test can take the base away and
// check that the fallback is LOUD rather than silently writing a bundle
// that trusts only us. There is no other reason to indirect it.
var findBaseTrustStore = baseTrustStore

// baseTrustStore finds the CA bundle az would otherwise use.
//
// certifi first, because that is what Python's requests -- and therefore
// az -- actually reads, and asking the interpreter is better than
// guessing a path. A system bundle is the fallback, which is what certifi
// is a copy of anyway.
//
// Note the order matters on Windows, where there is no system PEM file at
// all: without certifi there is nothing to combine, and saying so beats
// writing a bundle that silently trusts only us.
func baseTrustStore() (contents []byte, from string) {
	if path := certifiPath(); path != "" {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return body, "certifi at " + path
		}
	}
	for _, path := range systemTrustStores() {
		if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
			return body, path
		}
	}
	return nil, ""
}

// certifiPath asks a Python interpreter where certifi's bundle is.
//
// `az` is a Python program, so on a machine that has az there is almost
// always an interpreter that can answer. Which one does not matter much:
// a certifi bundle is the Mozilla root set, and any recent copy serves
// for reaching real hosts.
func certifiPath() string {
	for _, py := range pythonCandidates() {
		cmd := exec.Command(py, "-c", "import certifi;print(certifi.where())")
		out, err := cmd.Output()
		if err != nil {
			continue
		}
		if path := strings.TrimSpace(string(out)); path != "" {
			return path
		}
	}
	return ""
}

// pythonCandidates is what might be able to import certifi.
//
// On Windows, `python` on PATH is often the Microsoft Store's STUB, which
// prints "Python was not found" and exits 9009 rather than running
// anything -- the trap that breaks `gcloud` in Git Bash. It is still
// tried, because the error is an error and the loop moves on; what
// matters is that it is not the only candidate.
func pythonCandidates() []string {
	if runtime.GOOS == "windows" {
		return []string{"python", "python3", "py"}
	}
	return []string{"python3", "python"}
}

// systemTrustStores is where a PEM bundle usually lives, per platform.
func systemTrustStores() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/etc/ssl/cert.pem",
			"/usr/local/etc/openssl@3/cert.pem",
			"/opt/homebrew/etc/openssl@3/cert.pem",
		}
	case "windows":
		// Windows keeps its roots in a certificate STORE, not a file, so
		// there is nothing to concatenate. certifi is the only route.
		return nil
	default:
		return []string{
			"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
			"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
			"/etc/ssl/ca-bundle.pem",             // openSUSE
		}
	}
}
