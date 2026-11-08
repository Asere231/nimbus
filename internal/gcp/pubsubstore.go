package gcp

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Where Pub/Sub's messages live: beside the resource store, not in it.
//
// A topic and a subscription are resources -- they are created, read,
// listed and destroyed by Terraform, and they belong in the graph. The
// messages on them are data, and a thousand of them must not become a
// thousand boxes in the visualizer. This is the same split AWS made for
// DynamoDB's items and S3's objects, and the reason is the same.
//
// They are in memory and bounded, like CloudWatch Logs': they do not
// survive a restart even with --persist. That is the harmless direction to
// diverge in -- a queue that lost its backlog, rather than a resource that
// forgot it existed.

// maxQueued is how many messages a subscription holds before the oldest
// are dropped. Generous for a local apply, small enough that a runaway
// publisher cannot grow the process without bound.
const maxQueued = 10000

// defaultAckDeadline is Pub/Sub's own default, in seconds.
const defaultAckDeadline = 10

// message is one published message, in the shape a client reads it.
type message struct {
	ID          string            `json:"messageId"`
	Data        string            `json:"data,omitempty"` // base64, exactly as it arrived
	Attributes  map[string]string `json:"attributes,omitempty"`
	PublishTime string            `json:"publishTime"`
	OrderingKey string            `json:"orderingKey,omitempty"`
}

// queued is a message waiting on one subscription.
type queued struct {
	msg message
	// ackID identifies this DELIVERY, not the message: the same message
	// delivered twice has two ack ids, which is what lets a client
	// acknowledge the attempt it actually received.
	ackID string
	// leaseUntil is when an unacknowledged delivery becomes available
	// again. Zero means nobody holds it.
	leaseUntil time.Time
	deliveries int
}

type messageStore struct {
	mu sync.Mutex
	// bySub is keyed on a subscription's resource name.
	bySub map[string][]*queued
	seq   atomic.Uint64
}

func newMessageStore() *messageStore {
	return &messageStore{bySub: map[string][]*queued{}}
}

// nextID numbers messages and ack ids. Pub/Sub's own ids are opaque
// strings, so a counter is as valid as anything -- and it makes a log
// readable, which a random id does not.
func (m *messageStore) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, m.seq.Add(1))
}

// publish appends a message to one subscription's queue.
func (m *messageStore) publish(sub string, msg message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := append(m.bySub[sub], &queued{msg: msg, ackID: m.nextID("ack")})
	if len(q) > maxQueued {
		q = q[len(q)-maxQueued:]
	}
	m.bySub[sub] = q
}

// pull leases up to max available messages and returns them.
//
// A message whose lease has expired is available again, which is how
// Pub/Sub redelivers something nobody acknowledged. Nothing here is
// ordered beyond arrival: Pub/Sub does not promise ordering without an
// ordering key, and pretending to would be inventing a guarantee.
func (m *messageStore) pull(sub string, max int, deadline time.Duration) []*queued {
	if max <= 0 {
		max = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	var out []*queued
	for _, q := range m.bySub[sub] {
		if len(out) >= max {
			break
		}
		if q.leaseUntil.After(now) {
			continue
		}
		q.leaseUntil = now.Add(deadline)
		q.deliveries++
		// A new ack id per delivery, as Pub/Sub does.
		q.ackID = m.nextID("ack")
		out = append(out, q)
	}
	return out
}

// ack removes the deliveries a client acknowledged. An ack id it no longer
// recognises is ignored rather than refused: a late ack for a message that
// was already acknowledged is normal, not an error.
func (m *messageStore) ack(sub string, ackIDs []string) int {
	want := map[string]bool{}
	for _, id := range ackIDs {
		want[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := make([]*queued, 0, len(m.bySub[sub]))
	removed := 0
	for _, q := range m.bySub[sub] {
		if want[q.ackID] {
			removed++
			continue
		}
		kept = append(kept, q)
	}
	m.bySub[sub] = kept
	return removed
}

// modifyAckDeadline extends or releases a lease. A deadline of zero makes
// the message available immediately, which is how a client says "I cannot
// handle this, give it to someone else".
func (m *messageStore) modifyAckDeadline(sub string, ackIDs []string, seconds int) {
	want := map[string]bool{}
	for _, id := range ackIDs {
		want[id] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range m.bySub[sub] {
		if !want[q.ackID] {
			continue
		}
		if seconds <= 0 {
			q.leaseUntil = time.Time{}
			continue
		}
		q.leaseUntil = time.Now().Add(time.Duration(seconds) * time.Second)
	}
}

// drop forgets a subscription's queue, when the subscription is deleted.
func (m *messageStore) drop(sub string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.bySub, sub)
}

// depth is how many messages a subscription is holding, which the
// inspector shows.
func (m *messageStore) depth(sub string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.bySub[sub])
}
