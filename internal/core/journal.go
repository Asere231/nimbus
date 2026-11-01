package core

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Keeping events on disk, so an apply can still be replayed tomorrow.
//
// The bus already remembers the last 1000 events, but only for as long as the
// process lives, and that ring is also what every new browser tab is sent on
// connect. The journal is deliberately separate from it: it holds far more
// than a tab should ever be handed at once, and it is read on demand by
// /api/runs rather than streamed.
//
// One JSON object per line, appended. The format is chosen so that a crash
// mid-write costs the last line and nothing else -- a truncated final line is
// skipped on load rather than failing the whole file.

// maxJournal is how many events are kept. At roughly 200 bytes an event that
// is a few megabytes, and comfortably more than any single apply produces.
const maxJournal = 20000

// Journal is an append-only event log on disk.
type Journal struct {
	mu   sync.Mutex
	file *os.File
	path string
	n    int // lines written since the last trim
}

// OpenJournal opens (and if necessary trims) the journal in dataDir.
func OpenJournal(dataDir string) (*Journal, error) {
	path := filepath.Join(dataDir, "events.jsonl")
	j := &Journal{path: path}
	if err := j.trim(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j.file = f
	return j, nil
}

// Append writes one event. Errors are dropped: a visualizer convenience must
// never be able to fail an API call.
func (j *Journal) Append(e Event) {
	if j == nil || j.file == nil {
		return
	}
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	_, _ = j.file.Write(append(line, '\n'))
	j.n++
	// Trimming reopens the file, so it only happens occasionally rather than
	// on every write past the limit.
	if j.n >= maxJournal/4 {
		j.n = 0
		_ = j.file.Close()
		j.file = nil
		if err := j.trim(); err == nil {
			j.file, _ = os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		}
	}
}

// AppendSnapshot records that these resources existed at this moment.
//
// It is needed because the journal only ever hears about CHANGES, and a
// restart makes no changes: the store loads state.json and every resource in
// it is simply there, unannounced. A replay of the next run would then start
// from an empty world. (The same is true of the default subscription, which
// ensureSubscription skips creating when it is already present.)
//
// These records are written straight to the file and never published, so
// they do not reach the activity feed or any browser -- nothing happened, and
// a feed that said otherwise after every restart would be lying.
func (j *Journal) AppendSnapshot(resources []*Resource) {
	if j == nil || j.file == nil || len(resources) == 0 {
		return
	}
	now := time.Now().UTC()
	for _, r := range resources {
		j.Append(Event{
			Time: now, Kind: "snapshot", Action: "exists", Cloud: r.Cloud,
			ResourceID: r.ID, ResourceType: r.Type, Name: r.Name, ParentID: r.ParentID,
		})
	}
}

// Events returns everything in the journal, oldest first.
func (j *Journal) Events() []Event {
	if j == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return readJournal(j.path)
}

// Close flushes and closes the file.
func (j *Journal) Close() error {
	if j == nil || j.file == nil {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	err := j.file.Close()
	j.file = nil
	return err
}

// trim rewrites the file with only the newest maxJournal events, if it has
// grown past that. The caller must not hold the file open.
func (j *Journal) trim() error {
	events := readJournal(j.path)
	if len(events) <= maxJournal {
		return nil
	}
	events = events[len(events)-maxJournal:]
	tmp := j.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			continue
		}
		_, _ = w.Write(append(line, '\n'))
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, j.path)
}

func readJournal(path string) []Event {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	// A single event is small, but an ARM path can be long; the default 64 KB
	// line limit is plenty, and a line over it is skipped rather than fatal.
	sc.Buffer(make([]byte, 0, 8192), 1<<20)
	for sc.Scan() {
		var e Event
		// A half-written final line from a crash fails to parse and is
		// dropped, which is exactly what should happen to it.
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		out = append(out, e)
	}
	return out
}
