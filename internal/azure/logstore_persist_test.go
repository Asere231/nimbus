package azure

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A workspace's rows survive a restart with --persist.
//
// They did not, and the file said so honestly -- "bounded, in memory, and
// NOT persisted even with --persist. A workspace survives a restart; its
// rows do not." Being honest about a gap is better than hiding one, but the
// gap is still worth closing: a container app's console is the only record
// of what it did, and Milestone 19's whole lesson was that --persist has to
// mean the resource still WORKS afterwards, not merely that it is listed.
//
// A query against a reloaded workspace that returns nothing is the exact
// ambiguity logquery.go exists to avoid: an empty result is
// indistinguishable from "nothing has been logged yet", which is the
// ordinary state of a new workspace. So losing the rows did not even fail
// loudly -- it looked like a workspace nobody had used.
func TestAWorkspacesRowsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	const customerID = "11111111-2222-3333-4444-555555555555"

	first := newLogStore()
	if err := first.Persist(dir); err != nil {
		t.Fatal(err)
	}
	first.append(customerID,
		logLine{Time: "2026-11-18T00:00:01Z", Table: tableContainerAppConsole,
			App: "web", Stream: "stdout", Message: "listening on 8080"},
		logLine{Time: "2026-11-18T00:00:02Z", Table: tableContainerAppConsole,
			App: "web", Stream: "stdout", Message: "GET / 200"},
	)

	// The restart: a fresh store over the same directory.
	second := newLogStore()
	if err := second.Persist(dir); err != nil {
		t.Fatal(err)
	}
	got := second.all(customerID)
	if len(got) != 2 {
		t.Fatalf("expected both rows back, got %d: %+v", len(got), got)
	}
	if got[0].Message != "listening on 8080" || got[1].Message != "GET / 200" {
		t.Errorf("rows came back changed or out of order: %+v", got)
	}
	// Every column, not just the message: a row whose App is lost cannot be
	// narrowed to one app in the visualizer or by a `where` clause.
	if got[0].App != "web" || got[0].Stream != "stdout" ||
		got[0].Table != tableContainerAppConsole || got[0].Time != "2026-11-18T00:00:01Z" {
		t.Errorf("a column did not round-trip: %+v", got[0])
	}
}

// And the query path finds them, which is what anybody actually does after
// a restart.
func TestAReloadedWorkspaceCanBeQueried(t *testing.T) {
	dir := t.TempDir()
	const customerID = "11111111-2222-3333-4444-555555555555"

	first := newLogStore()
	if err := first.Persist(dir); err != nil {
		t.Fatal(err)
	}
	first.append(customerID, logLine{
		Time: "2026-11-18T00:00:01Z", Table: tableContainerAppConsole,
		App: "web", Stream: "stdout", Message: "hello from before the restart",
	})

	second := newLogStore()
	if err := second.Persist(dir); err != nil {
		t.Fatal(err)
	}
	table, err := runKQL(tableContainerAppConsole+` | where Log_s contains "before the restart"`,
		second.all(customerID))
	if err != nil {
		t.Fatal(err)
	}
	if len(table.Rows) != 1 {
		t.Errorf("a query after the restart found %d rows, expected 1", len(table.Rows))
	}
}

// Deleting a workspace takes its rows off disk too.
//
// Otherwise a customerId that was reused -- or simply a destroy followed by
// an apply -- would surface somebody else's lines, which is worse than
// losing them.
func TestForgettingAWorkspaceRemovesItsRowsFromDisk(t *testing.T) {
	dir := t.TempDir()
	const customerID = "11111111-2222-3333-4444-555555555555"

	s := newLogStore()
	if err := s.Persist(dir); err != nil {
		t.Fatal(err)
	}
	s.append(customerID, logLine{Time: "t", Table: tableContainerAppConsole, Message: "x"})
	s.forget(customerID)

	reloaded := newLogStore()
	if err := reloaded.Persist(dir); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.all(customerID); len(got) != 0 {
		t.Errorf("a forgotten workspace came back with %d rows", len(got))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "1111") {
			t.Errorf("the workspace's file is still on disk: %s", e.Name())
		}
	}
}

// With no data directory nothing is written, which is the default.
func TestWithoutPersistenceNothingIsWritten(t *testing.T) {
	dir := t.TempDir()
	s := newLogStore() // no Persist call
	s.append("11111111-2222-3333-4444-555555555555",
		logLine{Time: "t", Table: tableContainerAppConsole, Message: "x"})

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a store with no data directory wrote %d files", len(entries))
	}
}

// A file Nimbus did not write is ignored and REPORTED, as every other
// store's data directory now is.
func TestAStrayFileInTheLogDirectoryIsReported(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}

	buf := captureLog(t)
	s := newLogStore()
	if err := s.Persist(dir); err != nil {
		t.Fatal(err)
	}
	if said := buf.String(); !strings.Contains(said, "ignoring 2 files") {
		t.Errorf("the strays were not reported: %q", said)
	}
}

// captureLog swaps the standard logger's output for the duration of a test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	out, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(out)
		log.SetFlags(flags)
	})
	return &buf
}
