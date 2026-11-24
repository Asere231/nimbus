package core

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every command and every flag must have a reference entry.
//
// `docs/CLI.md` is this project's command reference, and a reference is
// only worth the name if it is COMPLETE: a flag that exists and is not
// written down is one nobody can discover, and a command documented only
// in a bullet list is one nobody can look up. Found by reading rather
// than by a test, which is the problem -- `nimbus gcp install-auth-plugin`
// appeared in the summary list and in prose with no section of its own,
// and its `--dir` flag was the only flag in the whole CLI that nothing
// documented.
//
// So the rule is mechanical now, for the same reason
// TestEveryTestNamedInTheDocsExists is: documentation drifts silently and
// reads exactly as it did before. Reading source in a test is what
// TestEveryModuleImportResolves and TestEverySpecTypeHasABadge already do.
//
// Deliberately NOT asserted here: wording, ordering, or that an example
// is present. Those are judgement, and a test that pinned them would
// fail on every honest edit.

// cliFlag matches a flag DEFINITION in the CLI's source, e.g.
//
//	fs.String("ui-port", 4700, "...")
var cliFlag = regexp.MustCompile(`\b(?:fs|flag)\.(?:String|Int|Bool|Duration)\("([a-z0-9-]+)"`)

// cliHeading matches a command's heading in the reference, e.g.
//
//	## `nimbus gcp kubeconfig CLUSTER`
//
// Everything up to the closing backtick is captured, ARGUMENT
// placeholders included: they are stripped afterwards by the same rule
// the usage block uses, so the two cannot disagree about what a command
// is called.
// docFlag matches a flag named inside a reference table row.
var docFlag = regexp.MustCompile("`--([a-z0-9-]+)")

var cliHeading = regexp.MustCompile("(?m)^## `nimbus ([^`]+)`")

func TestEveryCLIFlagIsDocumented(t *testing.T) {
	root := repoRoot(t)

	defined := map[string]string{} // flag -> file that defines it
	walkCLISource(t, root, func(path, src string) {
		for _, m := range cliFlag.FindAllStringSubmatch(src, -1) {
			defined[m[1]] = filepath.Base(path)
		}
	})
	if len(defined) < 10 {
		t.Fatalf("found only %d flags; the matcher is probably wrong", len(defined))
	}

	// A flag must appear in a TABLE ROW, not merely somewhere on the
	// page.
	//
	// The first version of this test asked only whether the string
	// `--dir` occurred anywhere, and it had NO TEETH: deleting the row
	// that documents the flag left the test green, because the flag is
	// also named in the command's usage line two lines above. That is
	// this repository's "a weak test drifts like no test, and looks
	// safer doing it" in its purest form -- and it was found the way
	// that section says to find it, by breaking the thing on purpose
	// and watching the test not care.
	//
	// The usage line says a flag EXISTS; the table says what it means
	// and what it defaults to. The reference's job is the second one.
	ref := readDoc(t, filepath.Join(root, "docs", "CLI.md"))
	rows := map[string]bool{}
	for _, line := range strings.Split(ref, "\n") {
		if !strings.HasPrefix(line, "| `--") {
			continue
		}
		// One row may document several spellings:
		//   | `--host NAME` / `--gcp-port N` | ... | ... |
		for _, m := range docFlag.FindAllStringSubmatch(line, -1) {
			rows[m[1]] = true
		}
	}
	var missing []string
	for name, file := range defined {
		if !rows[name] {
			missing = append(missing, name+" (defined in "+file+")")
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these flags exist and no table row in docs/CLI.md documents them:\n  %s\n\n"+
			"a usage line says a flag exists; the table says what it means and what it defaults to",
			strings.Join(missing, "\n  "))
	}
}

// A command in the binary's own help must have a SECTION in the
// reference, not merely a line in the summary list.
//
// The summary list is a table of contents; the section is the entry. A
// command with only the former looks documented from the top of the page
// and has nowhere to put its flags, which is exactly how
// `install-auth-plugin` lost `--dir`.
func TestEveryCLICommandHasAReferenceSection(t *testing.T) {
	root := repoRoot(t)

	// Every usage line the binary can print, from EVERY file that prints
	// one -- not just main.go. `nimbus gcp` has a sub-help of its own in
	// gcp.go, and reading only the top-level block is what let
	// `install-auth-plugin` go undocumented: the test passed because it
	// never saw the command. A help text that lives in two places has to
	// be read in two places.
	commands := map[string]bool{}
	walkCLISource(t, root, func(_, src string) {
		for _, line := range strings.Split(src, "\n") {
			// A usage line, exactly: both blocks are raw strings whose
			// entries are indented by two spaces. A comment mentioning
			// a command starts with "//" and a flag set's name is in
			// quotes, so neither is mistaken for a promise to document.
			if !strings.HasPrefix(line, "  nimbus ") {
				continue
			}
			line = strings.TrimSpace(line)
			// The help puts a DESCRIPTION after the command, separated
			// by a run of spaces. Splitting there first is what stops
			// "nimbus env azure   environment variables for ..." being
			// read as a five-word command -- the description's words
			// are lowercase too, so nothing later could tell them apart.
			if i := strings.Index(line, "  "); i > 0 {
				line = line[:i]
			}
			if verbs := commandVerbs(strings.TrimPrefix(line, "nimbus ")); verbs != "" {
				commands[verbs] = true
			}
		}
	})
	if len(commands) < 8 {
		t.Fatalf("parsed only %d commands from the help text: %v", len(commands), commands)
	}

	ref := readDoc(t, filepath.Join(root, "docs", "CLI.md"))
	documented := map[string]bool{}
	for _, m := range cliHeading.FindAllStringSubmatch(ref, -1) {
		if verbs := commandVerbs(m[1]); verbs != "" {
			documented[verbs] = true
		}
	}

	var missing []string
	for c := range commands {
		if !documented[c] {
			missing = append(missing, "nimbus "+c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these commands have no `## ` section in docs/CLI.md:\n  %s\n\n"+
			"a line in the summary list is a table of contents, not a reference entry",
			strings.Join(missing, "\n  "))
	}
}

// commandVerbs reduces a command line to its verbs, dropping ARGUMENT
// placeholders and [--optional] flags: "gcp kubeconfig CLUSTER" and
// "gcp kubeconfig" both become "gcp kubeconfig", so the help text and
// the reference heading are compared on the thing they agree about.
func commandVerbs(s string) string {
	var verbs []string
	for _, f := range strings.Fields(s) {
		if f == "" || strings.HasPrefix(f, "[") || strings.HasPrefix(f, "-") ||
			strings.ToUpper(f) == f {
			break
		}
		verbs = append(verbs, f)
	}
	return strings.Join(verbs, " ")
}

func walkCLISource(t *testing.T, root string, fn func(path, src string)) {
	t.Helper()
	dir := filepath.Join(root, "cmd", "nimbus")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fn(path, string(data))
	}
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The version is stated in three places and they must agree.
//
// `cmd/nimbus/main.go` holds the constant that /api/info serves, README.md
// says "Status (vX)", and docs/CHANGELOG.md's newest release heading names
// it. Nothing checked that they matched, and a release is exactly the moment
// they diverge: bumping the constant and forgetting the README, or cutting a
// changelog heading without bumping the constant, are both one edit away.
//
// The invariant holds whether or not work is in flight. An "## Unreleased"
// section above the newest version heading is the ordinary state between
// releases, and the binary still reports the last RELEASED version — so the
// check is against the newest VERSION heading, not the newest heading.
func TestTheVersionIsTheSameEverywhere(t *testing.T) {
	root := repoRoot(t)

	src := readDoc(t, filepath.Join(root, "cmd", "nimbus", "main.go"))
	m := regexp.MustCompile(`const version = "([0-9]+\.[0-9]+\.[0-9]+)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("could not find the version constant in cmd/nimbus/main.go")
	}
	version := m[1]

	readme := readDoc(t, filepath.Join(root, "README.md"))
	rm := regexp.MustCompile(`\*\*Status \(v([0-9]+\.[0-9]+\.[0-9]+)\)`).FindStringSubmatch(readme)
	if rm == nil {
		t.Fatal("could not find the \"Status (vX)\" line in README.md")
	}
	if rm[1] != version {
		t.Errorf("README.md says v%s and cmd/nimbus says %s", rm[1], version)
	}

	changelog := readDoc(t, filepath.Join(root, "docs", "CHANGELOG.md"))
	cm := regexp.MustCompile(`(?m)^## ([0-9]+\.[0-9]+\.[0-9]+) —`).FindStringSubmatch(changelog)
	if cm == nil {
		t.Fatal("could not find a released version heading in docs/CHANGELOG.md")
	}
	if cm[1] != version {
		t.Errorf("the newest release in docs/CHANGELOG.md is %s and cmd/nimbus reports %s\n\n"+
			"cutting a release means bumping the constant, the README's status line and the "+
			"changelog heading together", cm[1], version)
	}
}
