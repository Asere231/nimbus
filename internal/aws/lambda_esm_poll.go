package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// The poller: one goroutine per mapping, reading a stream and invoking a
// function with what it finds.
//
// This is the piece people get wrong about Lambda. A function does not
// subscribe to anything -- a poller owned by the Lambda service reads the
// stream and calls the function, which is why batch size and starting
// position are configuration of the MAPPING and not of the function, and
// why a function with no mapping is simply never called however much is
// written to the table.

// pollers tracks the running goroutines so a delete can stop one.
type pollers struct {
	mu   sync.Mutex
	stop map[string]context.CancelFunc
}

func newPollers() *pollers { return &pollers{stop: map[string]context.CancelFunc{}} }

// startPoller runs a mapping. Starting one that is already running is a
// no-op rather than a second goroutine: reattach and create both call it,
// and two pollers on one mapping would each deliver every record.
func (s *Server) startPoller(uuid string) {
	s.esm.mu.Lock()
	defer s.esm.mu.Unlock()
	if _, running := s.esm.stop[uuid]; running {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.esm.stop[uuid] = cancel
	go s.poll(ctx, uuid)
}

func (s *Server) stopPoller(uuid string) {
	s.esm.mu.Lock()
	defer s.esm.mu.Unlock()
	if cancel, running := s.esm.stop[uuid]; running {
		cancel()
		delete(s.esm.stop, uuid)
	}
}

// stopPollers stops every one, for shutdown.
func (s *Server) stopPollers() {
	s.esm.mu.Lock()
	defer s.esm.mu.Unlock()
	for uuid, cancel := range s.esm.stop {
		cancel()
		delete(s.esm.stop, uuid)
	}
}

// reattachPollers restarts a poller for every mapping in the store.
//
// Needed for the reason every engine needs reattaching: the MAPPING
// survives a restart because it is in the store, and the goroutine does
// not. Without this a mapping would read Enabled for ever and deliver
// nothing -- the log-group index's bug in another costume.
func (s *Server) reattachPollers() {
	for _, m := range s.allESMs() {
		s.startPoller(m.UUID)
	}
}

func (s *Server) allESMs() []eventSourceMapping {
	var out []eventSourceMapping
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeEventSourceMapping
	}) {
		var m eventSourceMapping
		if json.Unmarshal(res.Body, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// poll is the loop. It re-reads the mapping each tick rather than holding
// it, so an update to BatchSize or Enabled takes effect without
// restarting the goroutine -- and so a delete is noticed.
func (s *Server) poll(ctx context.Context, uuid string) {
	ticker := time.NewTicker(esmPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		m, aerr := s.loadESM(uuid)
		if aerr != nil {
			return // deleted
		}
		if !m.NimbusEnabled {
			continue
		}
		s.pollOnce(ctx, m)
	}
}

// pollOnce reads one batch and delivers it.
func (s *Server) pollOnce(ctx context.Context, m *eventSourceMapping) {
	table, aerr := s.streamTableFor(m.EventSourceArn)
	if aerr != nil {
		return
	}
	records, next := s.streams.after(table, m.NimbusCursor, m.BatchSize)
	if len(records) == 0 {
		return
	}

	// The event a handler receives. Each record gains eventSourceARN,
	// which the stream itself does not carry: a function mapped to two
	// tables has to be able to tell which one a record came from.
	payload := map[string]any{"Records": streamEventRecords(records, m.EventSourceArn)}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	result := "OK"
	if err := s.deliverBatch(ctx, m, body); err != nil {
		result = "PollingError: " + err.Error()
	}

	// The cursor advances either way once the attempts are spent. Real
	// Lambda blocks the shard until the records expire; doing that here
	// would wedge the stream for ever on a handler that always throws,
	// which looks like a hang rather than a failure. The divergence is
	// visible in LastProcessingResult rather than silent.
	_ = s.store.Update(s.esmARN(m.UUID), func(stored map[string]any) error {
		stored["nimbusCursor"] = next
		stored["LastProcessingResult"] = result
		stored["LastModified"] = esmNow()
		return nil
	})
}

// deliverBatch invokes the function, retrying a bounded number of times.
func (s *Server) deliverBatch(ctx context.Context, m *eventSourceMapping, body []byte) error {
	var lastErr error
	for attempt := 0; attempt < esmMaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := s.invokeForMapping(ctx, m, body)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// invokeForMapping calls the function the way the poller does: no HTTP, no
// qualifier, and the handler's own error counts as a failed batch.
//
// A mapping names a function ARN, which may be a qualified one -- a
// mapping onto an ALIAS is how a canary consumer is wired -- so it is
// resolved the same way an invoke is.
func (s *Server) invokeForMapping(ctx context.Context, m *eventSourceMapping, body []byte) error {
	name := lambdaName(m.FunctionArn)
	qualifier := lambdaQualifier(m.FunctionArn, nil)
	fn, aerr := s.resolveQualified(name, qualifier)
	if aerr != nil {
		return errorFrom(aerr)
	}
	if fn.Version != latestQualifier {
		if aerr := s.ensureVersionRunning(fn); aerr != nil {
			return errorFrom(aerr)
		}
	}
	running := s.lambda.Get(fn.FunctionArn)
	if running == nil {
		return errString("the function has no running container")
	}
	timeout := time.Duration(fn.Timeout) * time.Second
	callCtx, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()

	result, err := s.lambda.Invoke(callCtx, running, body, timeout)
	if err != nil {
		return err
	}
	s.recordInvocation(fn, uuidLike(), result)
	if result.FunctionError != "" {
		// A handler that threw is a FAILED batch, not a delivered one.
		// Lambda retries it, and reporting success here would quietly
		// drop every record the handler could not process.
		return errString("the handler returned " + result.FunctionError)
	}
	return nil
}

// streamEventRecords is the Records array a handler receives.
func streamEventRecords(records []streamRecord, eventSourceARN string) []any {
	out := make([]any, 0, len(records))
	for _, rec := range records {
		out = append(out, map[string]any{
			"eventID":        rec.EventID,
			"eventName":      rec.EventName,
			"eventVersion":   rec.EventVersion,
			"eventSource":    rec.EventSource,
			"awsRegion":      rec.AWSRegion,
			"dynamodb":       rec.Dynamodb,
			"eventSourceARN": eventSourceARN,
		})
	}
	return out
}

// stableUUID is a mapping's id: derived from what it connects, so creating
// the same mapping twice is a conflict rather than two pollers on one
// stream. Shaped like a UUID because the provider stores it as one.
func stableUUID(seed string) string {
	sum := sha256.Sum256([]byte("nimbus/aws/esm/" + strings.ToLower(seed)))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

type errString string

func (e errString) Error() string { return string(e) }

func errorFrom(a *apiError) error { return errString(a.Message) }
