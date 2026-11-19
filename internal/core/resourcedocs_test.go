package core

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// docs/RESOURCES.md lists exactly the resource types Nimbus has, and not a
// type more or less.
//
// A reference that is written once and then diverges is worse than none:
// it is read, believed, and wrong. This repository has produced that
// failure repeatedly -- an audit found four present-tense claims that were
// false, including a role-catalogue size and a routed-action count that had
// simply moved on -- and the conclusion written down at the time was to
// prefer a claim a test can check to a number in prose.
//
// So the table is checked against the UI's own type tables, which are
// themselves kept in step with the server by TestEverySpecTypeHasABadge.
// Adding a resource and forgetting to document it fails here, naming it.
func TestTheResourceReferenceMatchesTheTypeTables(t *testing.T) {
	root := repoRoot(t)

	doc, err := os.ReadFile(filepath.Join(root, "docs", "RESOURCES.md"))
	if err != nil {
		t.Fatalf("reading the resource reference: %v", err)
	}

	for _, tc := range []struct {
		cloud string
		file  string
		// How a type appears in that cloud's table, and in the document.
		inSource *regexp.Regexp
		inDoc    *regexp.Regexp
	}{
		{
			cloud:    "azure",
			file:     "types-azure.js",
			inSource: regexp.MustCompile(`(?m)^\s*"(microsoft\.[^"]+)":`),
			inDoc:    regexp.MustCompile("(?m)^\\| `(microsoft\\.[^`]+)` \\|"),
		},
		{
			cloud:    "aws",
			file:     "types-aws.js",
			inSource: regexp.MustCompile(`(?mi)^\s*"(aws::[^"]+)":`),
			inDoc:    regexp.MustCompile("(?m)^\\| `(AWS::[^`]+)` \\|"),
		},
		{
			cloud:    "gcp",
			file:     "types-gcp.js",
			inSource: regexp.MustCompile(`(?m)^\s*"([a-z]+\.googleapis\.com/[^"]+)":`),
			inDoc:    regexp.MustCompile("(?m)^\\| `([a-z]+\\.googleapis\\.com/[^`]+)` \\|"),
		},
	} {
		t.Run(tc.cloud, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(root, "internal", "ui", "static", tc.file))
			if err != nil {
				t.Fatalf("reading %s: %v", tc.file, err)
			}

			// Compared case-INSENSITIVELY. The Azure table is lower-cased
			// because ARM resolves ids that way, while the document reads
			// better in the casing a client actually sends -- and which of
			// those a line uses is not what this test is about.
			want := lowered(tc.inSource.FindAllStringSubmatch(string(src), -1))
			got := lowered(tc.inDoc.FindAllStringSubmatch(string(doc), -1))

			if len(want) == 0 {
				t.Fatalf("found no types in %s; this test is not looking where it thinks", tc.file)
			}
			for _, name := range want {
				if !contains(got, name) {
					t.Errorf("%s is in %s but not in docs/RESOURCES.md", name, tc.file)
				}
			}
			for _, name := range got {
				if !contains(want, name) {
					t.Errorf("%s is in docs/RESOURCES.md but not in %s -- "+
						"removed, renamed, or never existed", name, tc.file)
				}
			}
		})
	}
}

func lowered(ms [][]string) []string {
	var out []string
	for _, m := range ms {
		out = append(out, strings.ToLower(m[1]))
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
