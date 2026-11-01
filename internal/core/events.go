package core

import (
	"sync"
	"sync/atomic"
	"time"
)

// Event is something that happened in the emulator. The visualizer streams
// these to the browser over Server-Sent Events.
type Event struct {
	Seq          int64     `json:"seq"`
	Time         time.Time `json:"time"`
	Kind         string    `json:"kind"`   // "resource", "request", "reset"
	Action       string    `json:"action"` // create, update, delete, read, call, reset
	Cloud        string    `json:"cloud,omitempty"`
	ResourceID   string    `json:"resourceId,omitempty"`
	ResourceType string    `json:"resourceType,omitempty"`
	Name         string    `json:"name,omitempty"`
	ParentID     string    `json:"parentId,omitempty"`
	Method       string    `json:"method,omitempty"`
	Path         string    `json:"path,omitempty"`
	Status       int       `json:"status,omitempty"`
	DurationMs   float64   `json:"durationMs,omitempty"`
	Note         string    `json:"note,omitempty"` // e.g. "unimplemented action: listKeys"
}

// Bus is a simple fan-out pub/sub with a ring buffer of recent events, so a
// browser that connects late still sees what just happened.
type Bus struct {
	mu      sync.Mutex
	subs    map[int]chan Event
	nextID  int
	seq     atomic.Int64
	history []Event
	max     int
	journal *Journal
}

// NewBus creates a bus that remembers the last `keep` events.
func NewBus(keep int) *Bus {
	return &Bus{subs: map[int]chan Event{}, max: keep}
}

// Publish sends an event to every subscriber. Slow subscribers drop events
// rather than block the emulator.
func (b *Bus) Publish(e Event) {
	e.Seq = b.seq.Add(1)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	b.mu.Lock()
	b.history = append(b.history, e)
	if len(b.history) > b.max {
		b.history = b.history[len(b.history)-b.max:]
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
	journal := b.journal
	b.mu.Unlock()
	journal.Append(e)
}

// SetJournal gives the bus somewhere durable to write events. Without one,
// runs can still be replayed, but only those from the current process.
func (b *Bus) SetJournal(j *Journal) {
	b.mu.Lock()
	b.journal = j
	b.mu.Unlock()
}

// AllEvents returns everything available for building runs: the journal when
// there is one, otherwise the in-memory ring.
//
// The two are never merged. Sequence numbers restart at 1 in a new process,
// so events from different runs of Nimbus cannot be deduplicated by seq --
// and they do not need to be, because every published event is journalled,
// which makes the journal a superset of the ring.
func (b *Bus) AllEvents() []Event {
	b.mu.Lock()
	journal := b.journal
	b.mu.Unlock()
	if journal != nil {
		return journal.Events()
	}
	return b.History()
}

// Subscribe returns a channel of events plus the recent history, and a
// function to unsubscribe.
func (b *Bus) Subscribe() (<-chan Event, []Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	hist := append([]Event(nil), b.history...)
	b.mu.Unlock()
	return ch, hist, func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// History returns a copy of the recent events.
func (b *Bus) History() []Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Event(nil), b.history...)
}
