package gcp

import (
	"encoding/json"
	"net/http"
	"sync"
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
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Name          string `json:"name"`
	OperationType string `json:"operationType"`
	TargetLink    string `json:"targetLink"`
	TargetID      string `json:"targetId"`
	Status        string `json:"status"`
	Progress      int    `json:"progress"`
	User          string `json:"user"`
	InsertTime    string `json:"insertTime"`
	StartTime     string `json:"startTime"`
	EndTime       string `json:"endTime"`
	SelfLink      string `json:"selfLink"`
	Region        string `json:"region,omitempty"`
	Zone          string `json:"zone,omitempty"`
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

func (o *operations) put(op *operation) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.byURL[op.SelfLink] = op
	o.order = append(o.order, op.SelfLink)
	if len(o.order) > maxOperations {
		drop := o.order[0]
		o.order = o.order[1:]
		delete(o.byURL, drop)
	}
}

func (o *operations) get(selfLink string) *operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.byURL[selfLink]
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
	if scope != "global" {
		op.Region = s.base() + "/compute/v1/projects/" + s.cfg.ProjectID + "/" + scope
	}
	s.operations.put(op)
	return op
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
