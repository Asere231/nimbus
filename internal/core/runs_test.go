package core

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 28, 15, 4, 11, 0, time.UTC)

func at(sec float64) time.Time {
	return base.Add(time.Duration(sec * float64(time.Second)))
}

func create(sec float64, name string) Event {
	return Event{Kind: "resource", Action: "create", Time: at(sec), ResourceID: "/id/" + name, Name: name}
}

func call(sec float64, path string) Event {
	return Event{Kind: "request", Action: "call", Time: at(sec), Method: "GET", Path: path, Status: 200}
}

func TestRunsSplitOnAQuietGap(t *testing.T) {
	quiet := RunGap.Seconds() + 1
	runs := Runs([]Event{
		create(0, "a"), create(1, "b"), create(3, "c"),
		// ... more than RunGap of nothing ...
		create(3+quiet, "d"), create(4+quiet, "e"),
	})
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	// Newest first.
	if runs[0].Steps != 2 || runs[1].Steps != 3 {
		t.Errorf("steps = %d and %d, want 2 then 3 (newest run first)", runs[0].Steps, runs[1].Steps)
	}
	if !runs[1].Start.Equal(at(0)) || !runs[1].End.Equal(at(3)) {
		t.Errorf("older run spans %v..%v", runs[1].Start, runs[1].End)
	}
}

// Exactly at the threshold the events belong to different runs; just under
// it they do not. Worth pinning, because every other test sits far from the
// boundary and would not notice it moving.
func TestRunGapBoundary(t *testing.T) {
	just := RunGap.Seconds() - 0.001
	if n := len(Runs([]Event{create(0, "a"), create(just, "b")})); n != 1 {
		t.Errorf("a gap just under RunGap made %d runs, want 1", n)
	}
	if n := len(Runs([]Event{create(0, "a"), create(RunGap.Seconds(), "b")})); n != 2 {
		t.Errorf("a gap of exactly RunGap made %d runs, want 2", n)
	}
}

// A cluster being polled produces a steady stream of requests and changes
// nothing. It must not become a run, or the picker fills with entries that
// replay to an empty graph.
func TestPollingAloneIsNotARun(t *testing.T) {
	var events []Event
	for i := 0; i < 20; i++ {
		events = append(events, call(float64(i)*3, "/api/v1/pods"))
	}
	if runs := Runs(events); len(runs) != 0 {
		t.Errorf("got %d runs from nothing but reads, want 0", len(runs))
	}
}

// Requests that happen during an apply belong to it: the scrubber shows the
// call behind each step.
func TestRunKeepsTheRequestsAlongside(t *testing.T) {
	runs := Runs([]Event{
		call(0, "/oauth2/token"),
		create(1, "rg"),
		call(1.2, "/subscriptions/s/resourceGroups/rg"),
		create(2, "vnet"),
	})
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Steps != 2 || r.Requests != 2 || r.Created != 2 {
		t.Errorf("steps=%d requests=%d created=%d, want 2/2/2", r.Steps, r.Requests, r.Created)
	}
	if len(r.Events) != 4 {
		t.Errorf("the run carries %d events, want all 4", len(r.Events))
	}
	// Order is what makes a replay a replay.
	if r.Events[0].Kind != "request" || r.Events[1].Name != "rg" {
		t.Error("events are not in the order they happened")
	}
}

func TestRunsCountActionsSeparately(t *testing.T) {
	runs := Runs([]Event{
		create(0, "a"),
		{Kind: "resource", Action: "update", Time: at(1), ResourceID: "/id/a", Name: "a"},
		{Kind: "resource", Action: "delete", Time: at(2), ResourceID: "/id/a", Name: "a"},
		{Kind: "reset", Action: "reset", Time: at(3)},
	})
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	r := runs[0]
	if r.Created != 1 || r.Updated != 1 || r.Deleted != 1 {
		t.Errorf("created=%d updated=%d deleted=%d, want 1/1/1", r.Created, r.Updated, r.Deleted)
	}
	if r.Steps != 4 {
		t.Errorf("steps = %d, want 4: a reset is a step too", r.Steps)
	}
}

func TestFindRunByID(t *testing.T) {
	apart := RunGap.Seconds() + 1
	runs := Runs([]Event{create(0, "a"), create(apart, "b")})
	want := runs[1]
	got, ok := FindRun([]Event{create(0, "a"), create(apart, "b")}, want.ID)
	if !ok {
		t.Fatalf("run %s not found", want.ID)
	}
	if !got.Start.Equal(want.Start) || len(got.Events) == 0 {
		t.Errorf("found the wrong run, or one without events: %+v", got)
	}
	if _, ok := FindRun(nil, "nope"); ok {
		t.Error("an unknown id must not match a run")
	}
}

// The ID has to survive a restart, because that is the whole point of the
// journal -- sequence numbers do not, they begin again at 1.
func TestRunIDIsDerivedFromItsStartTime(t *testing.T) {
	a := Runs([]Event{create(0, "a"), create(1, "b")})
	b := Runs([]Event{create(0, "a"), create(1, "b")})
	if a[0].ID != b[0].ID {
		t.Errorf("same events gave different ids: %s and %s", a[0].ID, b[0].ID)
	}
	if a[0].ID == "" {
		t.Error("a run has no id")
	}
}

func TestJournalRoundTripsAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	bus := NewBus(10)
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	bus.SetJournal(j)
	for i := 0; i < 5; i++ {
		bus.Publish(Event{Kind: "resource", Action: "create", ResourceID: "/id/r", Name: "r"})
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// A new process: a new bus with an empty ring, reading the same file.
	fresh := NewBus(10)
	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	fresh.SetJournal(j2)
	got := fresh.AllEvents()
	if len(got) != 5 {
		t.Fatalf("after a restart the journal has %d events, want 5", len(got))
	}
	if got[0].Seq != 1 || got[4].Seq != 5 {
		t.Errorf("sequence numbers were not preserved: %d..%d", got[0].Seq, got[4].Seq)
	}
	if len(Runs(got)) != 1 {
		t.Error("the replayed events should still group into one run")
	}
}

// Without a journal, runs still work -- from the ring, for this process only.
func TestAllEventsFallsBackToTheRing(t *testing.T) {
	bus := NewBus(10)
	bus.Publish(Event{Kind: "resource", Action: "create", ResourceID: "/id/r", Name: "r"})
	if len(bus.AllEvents()) != 1 {
		t.Error("with no journal, AllEvents should return the in-memory history")
	}
}

// A line half-written by a crash costs that line and nothing else.
func TestJournalSkipsATruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	j.Append(Event{Seq: 1, Kind: "resource", Action: "create", Name: "a", Time: at(0)})
	j.Append(Event{Seq: 2, Kind: "resource", Action: "create", Name: "b", Time: at(1)})
	j.Close()

	path := filepath.Join(dir, "events.jsonl")
	appendRaw(t, path, `{"seq":3,"kind":"reso`)

	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if got := len(j2.Events()); got != 2 {
		t.Errorf("read %d events past a truncated line, want the 2 good ones", got)
	}
}

func appendRaw(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}
