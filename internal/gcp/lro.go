package gcp

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// The OTHER kind of long-running operation.
//
// operations.go already has one: Compute's `compute#operation`, with a
// `status` of DONE and a `targetLink`. This file has the one the newer
// APIs use, `google.longrunning.Operation`, which is a different document
// entirely -- a resource NAME rather than a URL, a boolean `done`, and the
// finished resource carried inside `response` with an `@type` on it.
//
// Two of them, because they really are two protocols, and the measurements
// that matter here are about who polls what:
//
//   - **GKE ignores its operation and polls the CLUSTER.** Measured
//     against the provider 6.50.0: after a create it GETs the cluster and
//     keeps GETting it until `status` is RUNNING. Reporting PROVISIONING
//     four times in a row made it wait, patiently, and proceed on the
//     fifth read. So GKE works exactly the way EKS and AKS do -- answer
//     the create at once, finish in the background, flip the status.
//
//   - **Cloud Run ignores the service and polls the OPERATION.** Measured
//     the same way: a create whose operation said `done: true` was
//     accepted immediately and the service's own `reconciling: true` and
//     `CONDITION_PENDING` were never looked at. An operation WITHOUT
//     `done` was polled four times, about ten seconds apart, until it said
//     done. So a Cloud Run create must leave its operation unfinished
//     until the container is actually up, or Terraform reports a service
//     as ready while nothing is listening.
//
// Getting those two backwards is the expensive mistake in this milestone:
// one way round gives a create that returns before anything runs, the
// other an apply that waits for ever.
//
// Like Compute's, these live in memory and not in the store -- a handful
// of boxes nobody asked about would bury the resources that were really
// created -- and an id from before a restart 404s.

// lro is google.longrunning.Operation, as much of it as a client reads.
type lro struct {
	// Name is a resource name, not a URL:
	// projects/{p}/locations/{l}/operations/{op}.
	Name     string         `json:"name"`
	Metadata map[string]any `json:"metadata,omitempty"`
	// Done is absent while the operation is running. A client that sees it
	// true stops polling, so it must not be set before the work is real.
	Done bool `json:"done,omitempty"`
	// Response carries the finished resource, with an @type naming it.
	Response map[string]any `json:"response,omitempty"`
	// Error is a google.rpc.Status, which is NOT the error envelope the
	// REST APIs use: code is an rpc code, not an HTTP status.
	Error *rpcStatus `json:"error,omitempty"`
}

type rpcStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// lros is the bounded registry, same shape and same reasoning as
// operations.go's.
type lros struct {
	mu    sync.Mutex
	byID  map[string]*lro
	order []string
}

const maxLROs = 512

func newLROs() *lros { return &lros{byID: map[string]*lro{}} }

func (l *lros) put(op *lro) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, seen := l.byID[op.Name]; !seen {
		l.order = append(l.order, op.Name)
	}
	l.byID[op.Name] = op
	if len(l.order) > maxLROs {
		drop := l.order[0]
		l.order = l.order[1:]
		delete(l.byID, drop)
	}
}

// get returns a COPY. A Cloud Run operation is finished by the background
// goroutine that started the container, so handing a reader the registry's
// own pointer has it serialising a struct being written to -- a real data
// race, which `go test -race` reports and plain `go test` does not. Found
// while giving Compute's operations the same treatment in Milestone 11;
// it has been here since Milestone 10.
//
// A shallow copy is enough: Done and Error are the fields finish and fail
// write, and Response is REPLACED rather than mutated in place.
func (l *lros) get(name string) *lro {
	l.mu.Lock()
	defer l.mu.Unlock()
	op := l.byID[name]
	if op == nil {
		return nil
	}
	copied := *op
	return &copied
}

// finish marks an operation done and attaches the resource a client should
// read out of it. Called from the background goroutine that did the work.
func (l *lros) finish(name string, typeURL string, resource map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	op := l.byID[name]
	if op == nil {
		return
	}
	op.Done = true
	if resource != nil {
		op.Response = map[string]any{"@type": typeURL}
		for k, v := range resource {
			op.Response[k] = v
		}
	}
}

// fail marks an operation failed. A client reads `error.message` and shows
// it, which is the difference between "something went wrong" and a line
// naming the container that would not start.
func (l *lros) fail(name, message string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if op := l.byID[name]; op != nil {
		op.Done = true
		// 2 is UNKNOWN in google.rpc.Code. Not an HTTP status: this
		// envelope counts differently from the REST one.
		op.Error = &rpcStatus{Code: 2, Message: message}
	}
}

// newLRO registers an operation. done=false leaves it running, which is
// what a Cloud Run create needs; done=true is for work that really is
// finished by the time the response is written.
func (s *Server) newLRO(location, metadataType string, done bool) *lro {
	name := "projects/" + s.cfg.ProjectID + "/locations/" + location +
		"/operations/operation-" + hexID(16)
	op := &lro{
		Name: name,
		Metadata: map[string]any{
			"@type":      metadataType,
			"createTime": timestamp(),
			"target":     name,
			"verb":       "create",
		},
		Done: done,
	}
	s.lros.put(op)
	return op
}

// getLRO answers a poll. An unknown id 404s, as it does in real GCP:
// inventing a `done` for one would turn a client's mistake into a silent
// success, which is the lesson Milestone 4 paid for in Azure.
func (s *Server) getLRO(w http.ResponseWriter, name string) string {
	op := s.lros.get(name)
	if op == nil {
		writeError(w, notFound("Operation %s not found", name))
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

// backgroundTimeout bounds any work a create kicks off: starting a
// cluster, pulling an image. Generous, because a first pull on a slow
// connection really can take minutes, and the client is polling anyway.
const backgroundTimeout = 10 * time.Minute
