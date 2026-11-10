package gcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Compute's long-running operations.
//
// Every Compute write answers with an Operation rather than with the
// resource, and the client polls it until `status` is DONE. Measured
// against the google provider: an operation whose FIRST response already
// says DONE is never fetched again -- the provider reads the status out of
// the response it was handed and goes straight to reading the resource. So
// the fast path costs one object and no second request at all.
//
// The endpoint exists anyway, because gcloud polls even when the answer is
// DONE, and because Milestone 4's Azure lesson was that an unknown
// operation id must 404 the way the real service does rather than be
// invented on the spot.
//
// They are kept in memory and NOT in the store, which is the same choice
// Azure's operations made and for a second reason as well: a five-resource
// apply produces six of them, and six boxes nobody asked about in the graph
// would bury the five that were actually created. A client polling an
// operation from before a restart gets a 404; that is the harmless
// direction to diverge in, as soft-deleted vaults already do.

type operation struct {
	Kind          string `json:"kind,omitempty"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	OperationType string `json:"operationType"`
	TargetLink    string `json:"targetLink"`
	TargetID      string `json:"targetId"`
	Status        string `json:"status"`
	// Progress is a NUMBER in Compute (0-100) and an OBJECT in the
	// container API (`{name, metrics, stages}`): same field name, same
	// field in the same-looking document, different type. Measured the
	// hard way -- a GKE create answered with Compute's `progress: 100`
	// fails in the provider with
	//
	//	json: cannot unmarshal number into Go struct field
	//	Operation.progress of type container.OperationProgress
	//
	// which names neither GKE nor Nimbus. So this is `any`, Compute fills
	// it with 100, and GKE leaves it out: absent is valid everywhere, and
	// nothing reads it.
	Progress   any    `json:"progress,omitempty"`
	User       string `json:"user"`
	InsertTime string `json:"insertTime"`
	StartTime  string `json:"startTime"`
	EndTime    string `json:"endTime"`
	SelfLink   string `json:"selfLink"`
	Region     string `json:"region,omitempty"`
	Zone       string `json:"zone,omitempty"`
}

// operations remembers the recent ones, bounded so a long session cannot
// grow the process. An operation nobody polled within the last few hundred
// writes is one nobody is going to.
type operations struct {
	mu    sync.Mutex
	byURL map[string]*operation
	order []string
}

const maxOperations = 512

func newOperations() *operations {
	return &operations{byURL: map[string]*operation{}}
}

// put stores a COPY, so the registry owns its own memory and the caller's
// pointer is never written to behind its back.
//
// That matters because newOperation hands its operation straight to
// writeJSON: without the copy, the background goroutine's finish() writes
// the same struct the response is being serialised from. A real data race,
// which `go test -race` catches and plain `go test` does not -- the second
// one found in this file in one sitting, both the same shape.
func (o *operations) put(op *operation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	stored := *op
	o.byURL[op.SelfLink] = &stored
	o.order = append(o.order, op.SelfLink)
	if len(o.order) > maxOperations {
		drop := o.order[0]
		o.order = o.order[1:]
		delete(o.byURL, drop)
	}
}

// get returns a COPY, because an operation is no longer immutable: a
// pending one is finished by a background goroutine, and handing a reader
// the registry's own pointer would have it serialising a struct being
// written to. Every other operation here answers DONE at once, which is
// why this was safe until instances arrived.
func (o *operations) get(selfLink string) *operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	op := o.byURL[selfLink]
	if op == nil {
		return nil
	}
	copied := *op
	return &copied
}

// newOperation records a finished operation and returns it.
//
// scope is "global" or "regions/{region}", matching where the operation is
// polled from: a regional resource's operation is NOT at the global URL,
// and a client given the wrong one polls a 404 forever.
func (s *Server) newOperation(scope, opType, targetLink string) *operation {
	name := "operation-" + hexID(16)
	op := &operation{
		Kind:          "compute#operation",
		ID:            numericID(name),
		Name:          name,
		OperationType: opType,
		TargetLink:    targetLink,
		TargetID:      numericID(targetLink),
		// DONE on the spot. Nothing here is actually slow -- unlike AKS or
		// EKS, where a container really does take a minute -- so claiming
		// otherwise would only add a poll.
		Status:     "DONE",
		Progress:   100,
		User:       "nimbus@nimbus.localhost",
		InsertTime: timestamp(),
		StartTime:  timestamp(),
		EndTime:    timestamp(),
		SelfLink:   s.computeLink(scope, "operations", name),
	}
	// A zonal operation carries `zone`, a regional one `region`. They are
	// different fields, and putting a zone in `region` is the kind of thing
	// nothing reads until something does.
	switch {
	case strings.HasPrefix(scope, "zones/"):
		op.Zone = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope
	case scope != "global":
		op.Region = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope
	}
	s.operations.put(op)
	return op
}

// newPendingOperation is an operation that is NOT finished yet, for the one
// kind of Compute write that really is slow: starting an instance's
// container, which includes pulling its image.
//
// Everything else here answers DONE on the spot, because nothing else is
// slow and claiming otherwise would only add a poll. An instance is the
// exception, and the provider handles it the way real Compute expects --
// it polls this operation until DONE and only then reads the instance.
// Answering DONE before the container is up would hand back a RUNNING
// instance with nothing running in it.
func (s *Server) newPendingOperation(scope, opType, targetLink string) *operation {
	op := s.newOperation(scope, opType, targetLink)
	op.Status = "RUNNING"
	op.Progress = 0
	op.EndTime = ""
	return op
}

// finish marks an operation DONE. The pointer is the one in the registry,
// so a poll already in flight sees the change.
func (o *operations) finish(selfLink string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if op := o.byURL[selfLink]; op != nil {
		op.Status = "DONE"
		op.Progress = 100
		op.EndTime = timestamp()
	}
}

// getOperation answers a poll. An id nobody issued is a 404, as it is in
// real GCP -- inventing a DONE for it would turn a client's mistake into a
// silent success.
func (s *Server) getOperation(w http.ResponseWriter, selfLink string) string {
	op := s.operations.get(selfLink)
	if op == nil {
		writeError(w, notFound("The resource '%s' was not found", selfLink))
		return "no such operation"
	}
	body, err := json.Marshal(op)
	if err != nil {
		writeError(w, internalError(err))
		return "operation: " + err.Error()
	}
	w.Header().Set("Content-Type", "application/json; charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	return ""
}

// waitOperationTimeout bounds a `wait` call. Real Compute returns after at
// most two minutes whether the operation finished or not, and the client
// then calls wait again -- so a bound here is the behaviour, not a
// shortcut. Nothing Nimbus starts takes anywhere near that long; this is
// short enough that a stuck operation does not look like a hung client.
const waitOperationTimeout = 30 * time.Second

// waitForOperation blocks until the operation is DONE, then answers it.
//
// This is Compute's operations.wait, which gcloud uses instead of polling.
// It is a POST, and `wait` is a TRAILING PATH SEGMENT on the operation's
// name -- the same spelling instances use for stop and start, and not the
// colon verb GKE uses for setSize.
func (s *Server) waitForOperation(w http.ResponseWriter, r *http.Request, selfLink string) string {
	if s.operations.get(selfLink) == nil {
		writeError(w, notFound("The resource '%s' was not found", selfLink))
		return "no such operation"
	}

	deadline := time.Now().Add(waitOperationTimeout)
	for {
		op := s.operations.get(selfLink)
		if op == nil {
			writeError(w, notFound("The resource '%s' was not found", selfLink))
			return "no such operation"
		}
		// Done, out of time, or the client gave up: answer with whatever
		// the operation says now, which is what real Compute does when its
		// own two minutes run out.
		if op.Status == "DONE" || time.Now().After(deadline) || r.Context().Err() != nil {
			return s.getOperation(w, selfLink)
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-r.Context().Done():
		}
	}
}
