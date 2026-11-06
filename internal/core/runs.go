package core

import (
	"sort"
	"strings"
	"time"
)

// Grouping events into runs.
//
// A "run" is one burst of work -- usually a `terraform apply`, an
// `az deployment group create`, or a destroy. Nothing tells Nimbus where one
// starts: Terraform does not announce itself, and the provider's requests look
// the same as anybody else's. So a run is inferred from the gaps.
//
// The rule: consecutive events less than RunGap apart belong together, and a
// group is only a run if something actually CHANGED in it. That second half
// matters -- a cluster being polled every three seconds would otherwise
// produce one endless "run" containing no resources at all.

// RunGap is the quiet period that separates one run from the next.
//
// Thirty seconds, and the number is measured rather than guessed. Five
// seconds looked generous until a real `terraform apply` was replayed: it
// came back as two runs, because azurerm's resource-group create runs a
// client-side wait of about twenty seconds during which it sends NOTHING --
// no polling, no re-reads, the longest silence inside any apply we make.
// (The flat ten-second wait on every delete is the same effect, smaller.)
// So the threshold has to clear that, and thirty does with room to spare.
//
// The cost of the larger number is that two things done within half a minute
// of each other merge into one run. That is the right way round: a run with
// a bit too much in it still replays in order, where a run cut in half
// replays only half the stack.
const RunGap = 30 * time.Second

// RunResource is a resource that already existed when a run began. It
// carries what a card needs to be drawn and nothing else.
type RunResource struct {
	Cloud    string `json:"cloud,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type,omitempty"`
	Name     string `json:"name,omitempty"`
	ParentID string `json:"parentId,omitempty"`
}

// Run is one burst of activity, summarised.
type Run struct {
	ID       string    `json:"id"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Created  int       `json:"created"`
	Updated  int       `json:"updated"`
	Deleted  int       `json:"deleted"`
	Requests int       `json:"requests"`
	Steps    int       `json:"steps"` // changes: what the scrubber steps through
	Events   []Event   `json:"events,omitempty"`

	// What already existed when this run started. A replay that began from
	// an empty graph showed only the resources the run itself touched,
	// floating with no resource group or subscription around them -- true
	// of the first run ever and of nothing after it.
	//
	// It is only as complete as the journal: anything created before the
	// oldest record still on disk cannot be recovered, so a very old run
	// replays against less than really existed. Startup writes a snapshot
	// of the store, so a restart does not lose it.
	Baseline []RunResource `json:"baseline,omitempty"`
}

// changed reports whether an event is a step in a replay, as opposed to
// traffic that happened alongside one.
func changed(e Event) bool {
	return e.Kind == "resource" || e.Kind == "reset"
}

// Runs groups events into runs, newest first. The events in each run are kept
// in the order they happened, which for an apply is dependency order: the
// provider could not have created a child before its parent.
func Runs(events []Event) []Run {
	// Snapshots take no part in this. They say what already existed, not
	// that anything happened, so they must not start a run, extend one, or
	// bridge the gap between two -- and a run that began at a snapshot
	// would count that snapshot as "before" nothing, losing the baseline it
	// was written for.
	sorted := make([]Event, 0, len(events))
	for _, e := range events {
		if e.Kind == "snapshot" {
			continue
		}
		sorted = append(sorted, e)
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	var runs []Run
	var group []Event
	flush := func() {
		if run, ok := summarise(group); ok {
			runs = append(runs, run)
		}
		group = nil
	}
	for _, e := range sorted {
		if len(group) > 0 && e.Time.Sub(group[len(group)-1].Time) >= RunGap {
			flush()
		}
		group = append(group, e)
	}
	flush()

	// Newest first: the run someone wants to replay is almost always the one
	// that just finished.
	for i, j := 0, len(runs)-1; i < j; i, j = i+1, j-1 {
		runs[i], runs[j] = runs[j], runs[i]
	}
	return runs
}

// summarise turns a group of events into a Run, or reports that the group
// never changed anything and is therefore not a run.
func summarise(group []Event) (Run, bool) {
	if len(group) == 0 {
		return Run{}, false
	}
	run := Run{
		Start:  group[0].Time,
		End:    group[len(group)-1].Time,
		Events: group,
	}
	for _, e := range group {
		switch {
		case e.Kind == "request":
			run.Requests++
		case e.Action == "create":
			run.Created++
		case e.Action == "update":
			run.Updated++
		case e.Action == "delete":
			run.Deleted++
		}
		if changed(e) {
			run.Steps++
		}
	}
	if run.Steps == 0 {
		return Run{}, false
	}
	// The start time names the run. It survives a restart, unlike the
	// sequence numbers, which begin again at 1 in a new process.
	run.ID = run.Start.UTC().Format("20060102T150405.000Z")
	return run, true
}

// FindRun returns the run with the given ID, with its events and the state
// of the world when it started.
func FindRun(events []Event, id string) (Run, bool) {
	for _, r := range Runs(events) {
		if r.ID != id {
			continue
		}
		r.Baseline = baselineAt(events, r.Start)
		return r, true
	}
	return Run{}, false
}

// baselineAt folds every event before `start` into the set of resources that
// existed at that moment.
func baselineAt(events []Event, start time.Time) []RunResource {
	sorted := append([]Event(nil), events...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })

	existing := map[string]RunResource{}
	order := []string{}
	for _, e := range sorted {
		if e.Time.After(start) {
			break
		}
		// A SNAPSHOT at exactly the run's start time still counts as
		// "before": it is not something that happened at that instant, it
		// is a statement about what already existed. An ordinary event at
		// that instant is part of the run and must not be.
		//
		// This is not a hair-splitting distinction. Windows's clock is
		// coarse -- often a millisecond or worse -- so a restart followed
		// immediately by an apply writes the snapshot and the first event
		// in the same tick, and a strict "before" drops the snapshot from
		// the one baseline it was written for. CI caught it; macOS and
		// Linux never would.
		if e.Kind != "snapshot" && !e.Time.Before(start) {
			continue
		}
		if e.Kind == "reset" {
			existing = map[string]RunResource{}
			order = order[:0]
			continue
		}
		// "snapshot" is how a restart tells the journal what was already
		// there; it counts towards the baseline exactly like a create.
		if (e.Kind != "resource" && e.Kind != "snapshot") || e.ResourceID == "" {
			continue
		}
		key := strings.ToLower(e.ResourceID)
		if e.Action == "delete" {
			delete(existing, key)
			continue
		}
		if _, seen := existing[key]; !seen {
			order = append(order, key)
		}
		existing[key] = RunResource{
			Cloud: e.Cloud, ID: e.ResourceID, Type: e.ResourceType,
			Name: e.Name, ParentID: e.ParentID,
		}
	}
	out := make([]RunResource, 0, len(existing))
	for _, key := range order {
		if r, ok := existing[key]; ok {
			out = append(out, r)
		}
	}
	return out
}
