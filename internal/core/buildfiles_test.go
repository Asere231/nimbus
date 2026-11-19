package core

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// No Go file in this repository is silently excluded from the build.
//
// `functionapp_arm_test.go` held eight tests and NEVER COMPILED. Go reads
// a trailing `_arm` in a filename as the GOARCH suffix for ARM, so on
// every amd64 machine -- and on all three CI legs -- the file landed in
// IgnoredGoFiles and `go test ./...` passed without it. Seven of those
// eight tests were correct and one was failing, and nothing said so for
// as long as the name stood.
//
// That is a worse failure than a test that passes for the wrong reason,
// because there is nothing to read: no skip, no warning, no mention in
// the output. The only visible symptom was `-run` matching nothing, which
// looks like a typo in the pattern.
//
// The suffixes that bite are every GOOS and GOARCH value Go knows --
// _arm, _386, _amd64, _linux, _windows, _js, _wasm and dozens more -- and
// a package name is a perfectly natural thing to end a file with. Hence a
// test rather than a convention: `go list` is asked directly.
func TestNoGoFileIsExcludedFromTheBuild(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	// -e so a package that fails to load does not abort the listing, and
	// run from the MODULE ROOT so "./..." really means the whole module.
	//
	// The first version of this left the working directory alone, which
	// for a test is its own package directory -- so it listed
	// internal/core and nothing else, and passed while the file it was
	// written to catch sat excluded two directories away. A guard against
	// silent exclusion that was itself silently scoped.
	cmd := exec.Command("go", "list", "-e", "-json", "./...")
	cmd.Dir = repoRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list failed, nothing to check against: %v", err)
	}

	type pkg struct {
		ImportPath      string
		IgnoredGoFiles  []string
		InvalidGoFiles  []string
		IgnoredOtherFil []string `json:"IgnoredOtherFiles"`
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	var checked int
	for dec.More() {
		var p pkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("reading go list output: %v", err)
		}
		checked++

		// A file Go ignores because of a build constraint it cannot
		// satisfy here. A deliberate one would carry a //go:build line,
		// which this repository does not use -- so any entry is an
		// accident, and an accidental GOARCH suffix is the likely cause.
		for _, f := range p.IgnoredGoFiles {
			t.Errorf("%s: %s is EXCLUDED from the build and never compiles here.\n\n"+
				"Go reads a trailing _<goos> or _<goarch> in a filename as a build "+
				"constraint -- _arm, _386, _linux, _windows and many more -- so a file "+
				"named after a package or a vendor can disappear silently. Rename it.",
				p.ImportPath, f)
		}
		for _, f := range p.InvalidGoFiles {
			t.Errorf("%s: %s is INVALID and not compiled", p.ImportPath, f)
		}
	}

	if checked == 0 {
		t.Fatal("go list returned no packages; this test is not looking where it thinks")
	}
}
