package core

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every test the documentation names must exist.
//
// This repo keeps finding the same failure mode: a claim that was true, that
// nothing exercises, and that quietly stops being true. `vm.Engine.Reattach`
// was written, unit-tested and called by nobody. The roadmap said only AWS
// enforced, four milestones after Azure and GCP started to. CLAUDE.md's own
// first paragraph said "GCP comes later" long after GCP was finished.
//
// Most of those cannot be caught mechanically. This one can: CLAUDE.md cites
// tests by name as evidence for its claims -- "pinned by a test",
// "TestEverySpecTypeHasABadge fails until you do" -- and a cited test that
// has been renamed or deleted turns the evidence into decoration while
// reading exactly as it did before.
//
// Reading source in a test is what TestEveryModuleImportResolves,
// TestEverySpecTypeHasABadge and TestEveryRoutedActionCanBeAuthorized
// already do, and for the same reason.
func TestEveryTestNamedInTheDocsExists(t *testing.T) {
	root := repoRoot(t)

	// Every Test... function actually defined anywhere in the tree.
	defined := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range regexp.MustCompile(`func (Test\w+)`).FindAllSubmatch(src, -1) {
			defined[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(defined) == 0 {
		t.Fatal("found no test functions at all; this test is not looking where it thinks")
	}

	// A name is only a citation if it looks like one: TestSomethingLikeThis,
	// not the bare word "Tests" in a sentence. A trailing * is a deliberate
	// wildcard -- CLAUDE.md writes TestEveryRouted*ActionCanBeAuthorized for
	// a pair of tests -- so those are checked as a prefix.
	cite := regexp.MustCompile(`Test[A-Z]\w*\*?\w*`)
	for _, doc := range []string{"CLAUDE.md", "README.md",
		filepath.Join("docs", "ARCHITECTURE.md"), filepath.Join("docs", "ROADMAP.md")} {
		src, err := os.ReadFile(filepath.Join(root, doc))
		if err != nil {
			continue // a document that does not exist cites nothing
		}
		for _, name := range cite.FindAllString(string(src), -1) {
			if defined[name] {
				continue
			}
			if i := strings.Index(name, "*"); i >= 0 {
				prefix := name[:i]
				found := false
				for have := range defined {
					if strings.HasPrefix(have, prefix) {
						found = true
						break
					}
				}
				if found {
					continue
				}
			}
			t.Errorf("%s cites %s, which no test defines — "+
				"the evidence for that claim has been renamed or removed", doc, name)
		}
	}
}

// repoRoot walks up from the package directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root")
	return ""
}
