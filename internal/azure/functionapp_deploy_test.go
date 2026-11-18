package azure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two bugs the first real zip deploy found, both of them mine.

// The deploy directory is ABSOLUTE, with or without --persist.
//
// It was filepath.Join(s.cfg.DataDir, "functions", ...), and DataDir is
// EMPTY unless --persist is given -- so in the ordinary in-memory mode the
// deploy directory came out as the relative path "functions\app-..." and
// the build ran against whatever the process's working directory happened
// to be:
//
//	docker build -q -t ... functions\app-nimbus-1e5841cd:
//	ERROR: failed to read dockerfile: open Dockerfile: no such file or directory
//
// A path that depends on the caller's cwd is wrong on every platform; it
// just happens to be invisible until something reads it from elsewhere.
func TestTheDeployDirectoryIsAbsoluteWithoutPersist(t *testing.T) {
	s := newTestServerForFunctions(t)
	// newTestServerForFunctions sets a DataDir, so check the memory-only
	// case explicitly -- which is the one that was broken.
	memory := New(Config{PublicURL: "https://localhost:4701"}, s.store, s.bus)

	for name, srv := range map[string]*Server{"with DataDir": s, "memory only": memory} {
		dir := srv.functionAppDir("/subscriptions/x/sites/fn1")
		if !filepath.IsAbs(dir) {
			t.Errorf("%s: the deploy directory %q is relative, so a build depends on the "+
				"process's working directory", name, dir)
		}
	}
}

// Two apps get two directories, or one would deploy over the other.
func TestTwoAppsGetTwoDeployDirectories(t *testing.T) {
	s := newTestServerForFunctions(t)
	a := s.functionAppDir("/subscriptions/x/resourceGroups/rg1/providers/Microsoft.Web/sites/app")
	b := s.functionAppDir("/subscriptions/x/resourceGroups/rg2/providers/Microsoft.Web/sites/app")
	if a == b {
		t.Errorf("two apps named the same in different groups share %q", a)
	}
}

// Unpacking a zip must leave a Dockerfile behind.
//
// unpackFunctionApp REPLACES the directory -- deliberately, so a function
// removed from the app stops running -- which also deletes the Dockerfile
// the create wrote. The build then failed with "open Dockerfile: no such
// file or directory" on a deploy that had otherwise worked perfectly.
//
// So the Dockerfile is written as part of preparing the directory, after
// every unpack rather than once at create. That also settles a question
// the first version left open: a zip containing its own Dockerfile cannot
// hijack the build, because Nimbus writes its own afterwards.
func TestPreparingADeployDirectoryAlwaysLeavesADockerfile(t *testing.T) {
	dir := t.TempDir()

	// A deploy whose archive even contains a competing Dockerfile.
	archive := zipOf(t, map[string]string{
		"host.json":       `{"version":"2.0"}`,
		"function_app.py": "import azure.functions\n",
		"Dockerfile":      "FROM scratch\n",
	})
	if err := unpackFunctionApp(archive, dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareDeployDir(dir, pythonFunctionImage); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("no Dockerfile after the deploy, so the build cannot run: %v", err)
	}
	if !strings.Contains(string(raw), pythonFunctionImage) {
		t.Errorf("the archive's own Dockerfile won; Nimbus must write its own:\n%s", raw)
	}
	// And the deployed code is still there.
	if _, err := os.Stat(filepath.Join(dir, "function_app.py")); err != nil {
		t.Errorf("the function did not survive: %v", err)
	}
}

// An app with no code yet still gets a host.json, so the host starts and
// serves 404s instead of refusing to start -- which is what real Azure
// does for a site nobody has deployed to.
func TestAnEmptyDeployDirectoryGetsAHostJson(t *testing.T) {
	dir := t.TempDir()
	if err := prepareDeployDir(dir, pythonFunctionImage); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "host.json")); err != nil {
		t.Errorf("no host.json, so the host would report the deployment missing: %v", err)
	}
}

// A host.json the user deployed is NOT overwritten: it carries their
// extension bundle and their logging settings.
func TestADeployedHostJsonIsKept(t *testing.T) {
	dir := t.TempDir()
	mine := `{"version":"2.0","logging":{"logLevel":{"default":"Debug"}}}`
	mustWrite(t, filepath.Join(dir, "host.json"), mine)

	if err := prepareDeployDir(dir, pythonFunctionImage); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "host.json"))
	if string(raw) != mine {
		t.Errorf("the deployed host.json was overwritten:\n%s", raw)
	}
}
