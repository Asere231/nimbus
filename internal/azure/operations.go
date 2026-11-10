package azure

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// Async operations, for real this time.
//
// Until M3 every write in Nimbus finished before it was answered, so the
// operation endpoint could honestly report Succeeded for any ID it was handed
// and nothing had to be remembered. AKS breaks that: a k3s container takes
// around thirty seconds to become ready, and the client has to be able to
// watch it happen -- and to be told if it fails, rather than polling a
// resource that sits on "Creating" forever.
//
// ARM's contract is narrow and worth stating, because the SDK depends on all
// of it:
//
//   - A write answers 201/202 with an Azure-AsyncOperation header.
//   - GET on that URL returns {"status": "InProgress"|"Succeeded"|"Failed"}.
//   - A Failed operation carries an "error" object, which is what the client
//     turns into the message the user actually reads.
//   - The URL keeps answering after the operation finishes; clients poll it
//     at least once more.

// operationStatus is ARM's provisioning vocabulary for a long-running call.
type operationStatus string

const (
	opInProgress operationStatus = "InProgress"
	opSucceeded  operationStatus = "Succeeded"
	opFailed     operationStatus = "Failed"
)

// operation is one long-running call's state.
type operation struct {
	ID         string
	ResourceID string
	Status     operationStatus
	Err        *armError
	Started    time.Time
	Ended      time.Time
}

// operations is the in-memory store of them.
//
// Operations are deliberately NOT persisted: they describe work this process
// was doing, and a restarted Nimbus is not doing it any more. A client polling
// across a restart gets a 404, which is honest -- better than resurrecting an
// "InProgress" that nothing will ever finish.
type operations struct {
	mu sync.Mutex
	m  map[string]*operation
}

func newOperations() *operations { return &operations{m: map[string]*operation{}} }

// start registers an operation that is still running.
//
// The registry keeps its OWN copy and the caller gets another, because an
// operation is mutable: finish() writes to it from the background
// goroutine that did the work, while the caller is still serialising its
// own into a response. Sharing the pointer is a real data race -- one the
// detector reports and plain `go test` does not. The same shape turned up
// three times in GCP's two registries in Milestone 11; this is the fourth,
// and the rule is now explicit: **a registry of mutable things hands out
// copies, on the way in and on the way out.**
func (o *operations) start(resourceID string) *operation {
	op := &operation{
		ID: newUUID(), ResourceID: resourceID,
		Status: opInProgress, Started: time.Now().UTC(),
	}
	stored := *op
	o.mu.Lock()
	o.m[op.ID] = &stored
	o.mu.Unlock()
	return op
}

// done registers an operation that has already finished, for the synchronous
// types that make up everything before AKS.
func (o *operations) done(resourceID string) *operation {
	op := o.start(resourceID)
	o.finish(op.ID, nil)
	// Re-read, because start handed back a copy made before it finished.
	if finished := o.get(op.ID); finished != nil {
		return finished
	}
	return op
}

// finish moves an operation to Succeeded, or to Failed with a reason.
func (o *operations) finish(id string, aerr *armError) {
	o.mu.Lock()
	defer o.mu.Unlock()
	op, ok := o.m[id]
	if !ok {
		return
	}
	op.Ended = time.Now().UTC()
	if aerr != nil {
		op.Status, op.Err = opFailed, aerr
		return
	}
	op.Status = opSucceeded
}

// get returns a COPY of an operation, or nil. See start for why.
func (o *operations) get(id string) *operation {
	o.mu.Lock()
	defer o.mu.Unlock()
	op, ok := o.m[id]
	if !ok {
		return nil
	}
	copied := *op
	return &copied
}

// forget drops operations that finished long ago, so a long-lived Nimbus does
// not accumulate them. Clients poll for seconds, not hours.
func (o *operations) forget(olderThan time.Duration) {
	cutoff := time.Now().UTC().Add(-olderThan)
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, op := range o.m {
		if op.Status != opInProgress && !op.Ended.IsZero() && op.Ended.Before(cutoff) {
			delete(o.m, id)
		}
	}
}

// operationIDFromPath pulls the ID out of an operation URL path.
func operationIDFromPath(p string) string {
	i := strings.LastIndex(strings.ToLower(p), strings.ToLower(operationsPath))
	if i < 0 {
		return ""
	}
	return p[i+len(operationsPath):]
}

// handleOperation answers a poll for an operation's state.
func (s *Server) handleOperation(w http.ResponseWriter, r *http.Request) string {
	// Opportunistic cleanup: no timer, and cheap at this scale.
	s.ops.forget(time.Hour)

	id := operationIDFromPath(r.URL.Path)
	op := s.ops.get(id)
	if op == nil {
		// 404, as real ARM does.
		//
		// Measured against azurerm 4.81.0: a 404 here is NOT mistaken for
		// success, so a client polling a stale URL will not believe work it
		// never started had finished -- which is the failure that matters.
		//
		// Restarting Nimbus mid-apply does fail the apply, with
		//
		//	polling after CreateOrUpdate: unimplemented polling status "Unknown"
		//
		// but that comes from the poll hitting a CLOSED PORT while Nimbus is
		// down, not from this response: the status is "Unknown", which is how
		// the SDK reports a connection error, and it never reaches here.
		// Returning something friendlier than a 404 would therefore change
		// nothing, so this matches ARM instead.
		writeError(w, 404, "OperationNotFound",
			"No operation with ID '%s'. Operations do not survive a restart of Nimbus.", id)
		return "unknown operation"
	}

	body := map[string]any{
		"id":        r.URL.Path,
		"name":      op.ID,
		"status":    string(op.Status),
		"startTime": op.Started.Format(time.RFC3339),
	}
	if !op.Ended.IsZero() {
		body["endTime"] = op.Ended.Format(time.RFC3339)
	}
	if op.Err != nil {
		body["error"] = map[string]any{"code": op.Err.Code, "message": op.Err.Message}
	}
	writeJSON(w, 200, body)
	if op.Status == opInProgress {
		return ""
	}
	return ""
}
