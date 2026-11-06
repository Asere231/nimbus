package aws

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Invoking a function, and where its output goes.
//
// The response's BODY is whatever the handler returned, byte for byte --
// not wrapped in anything. Whether it threw is a HEADER,
// X-Amz-Function-Error, which is how a client tells a raised exception from
// a returned value that happens to look like one. `aws lambda invoke`
// prints "FunctionError": "Unhandled" from that header alone.
//
// Everything the handler printed is captured from the container and written
// to /aws/lambda/<name>, with the REPORT line Lambda itself adds, so
// `aws logs filter-log-events` afterwards shows what really happened.

func (s *Server) invokeFunction(w http.ResponseWriter, r *http.Request, name string) *apiError {
	fn, aerr := s.loadFunction(name)
	if aerr != nil {
		return aerr
	}
	payload, err := io.ReadAll(io.LimitReader(r.Body, 6<<20))
	if err != nil {
		return &apiError{Status: 400, Code: "InvalidRequestContentException", Message: err.Error()}
	}

	running := s.lambda.Get(fn.FunctionArn)
	if running == nil {
		// Pending means the container is still coming up, which is a
		// different problem from a function that failed to start -- and the
		// provider waits on State, so saying which is what lets it wait.
		if fn.State == "Failed" {
			return &apiError{Status: 400, Code: "ResourceConflictException",
				Message: "The function " + fn.FunctionName + " could not be started: " + fn.StateReason}
		}
		return &apiError{Status: 409, Code: "ResourceNotReadyException",
			Message: "The function " + fn.FunctionName + " is still starting. State: " + fn.State}
	}

	// Event means fire and forget: the response is 202 with no body, and
	// the handler runs on regardless of whether anyone is still listening.
	async := r.Header.Get("X-Amz-Invocation-Type") == "Event" ||
		r.URL.Query().Get("InvocationType") == "Event"

	requestID := uuidLike()
	timeout := time.Duration(fn.Timeout) * time.Second

	if async {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), timeout+30*time.Second)
			defer cancel()
			if result, err := s.lambda.Invoke(ctx, running, payload, timeout); err == nil {
				s.recordInvocation(fn, requestID, result)
			}
		}()
		w.Header().Set("X-Amzn-RequestId", requestID)
		w.WriteHeader(202)
		return nil
	}

	ctx, cancel := context.WithTimeout(r.Context(), timeout+30*time.Second)
	defer cancel()
	result, err := s.lambda.Invoke(ctx, running, payload, timeout)
	if err != nil {
		return &apiError{Status: 500, Code: "ServiceException",
			Message: "invoking " + fn.FunctionName + ": " + err.Error()}
	}
	s.recordInvocation(fn, requestID, result)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-RequestId", requestID)
	w.Header().Set("X-Amz-Executed-Version", "$LATEST")
	if result.FunctionError != "" {
		w.Header().Set("X-Amz-Function-Error", result.FunctionError)
	}
	// LogType=Tail returns the last 4 KB of the log, base64 in a header.
	// The same lines that went to the log group, so the two agree.
	if r.Header.Get("X-Amz-Log-Type") == "Tail" || r.URL.Query().Get("LogType") == "Tail" {
		tail := strings.Join(handlerLines(result.Logs), "\n")
		if len(tail) > 4096 {
			tail = tail[len(tail)-4096:]
		}
		w.Header().Set("X-Amz-Log-Result", base64.StdEncoding.EncodeToString([]byte(tail)))
	}
	w.WriteHeader(200)
	_, _ = w.Write(result.Payload)
	return nil
}

// recordInvocation writes the handler's output to its log group and bumps
// the counter the visualizer shows.
func (s *Server) recordInvocation(fn *lambdaFunction, requestID string, result *funcs.Result) {
	group := fn.NimbusLogGroup
	if group == "" {
		group = "/aws/lambda/" + fn.FunctionName
	}
	s.ensureLogGroup(group)
	// Lambda's stream name is a date, a version and a hash. The shape
	// matters more than the hash: tooling parses the date out of it.
	stream := fmt.Sprintf("%s/[$LATEST]%s", time.Now().UTC().Format("2006/01/02"),
		stableID(fn.FunctionArn, 32))
	s.logs.ensureStream(group, stream)

	now := time.Now().UnixMilli()
	var events []logEvent
	events = append(events, logEvent{Timestamp: now, IngestionTime: now,
		Message: fmt.Sprintf("START RequestId: %s Version: $LATEST", requestID)})
	for _, line := range handlerLines(result.Logs) {
		events = append(events, logEvent{Timestamp: now, IngestionTime: now, Message: line})
	}
	events = append(events,
		logEvent{Timestamp: now, IngestionTime: now,
			Message: fmt.Sprintf("END RequestId: %s", requestID)},
		logEvent{Timestamp: now, IngestionTime: now,
			Message: fmt.Sprintf("REPORT RequestId: %s\tDuration: %.2f ms\tBilled Duration: %d ms\t"+
				"Memory Size: %d MB\tMax Memory Used: %d MB",
				requestID, float64(result.Duration.Microseconds())/1000,
				result.Duration.Milliseconds()+1, fn.MemorySize, fn.MemorySize/4)})
	s.logs.append(group, stream, events)

	_ = s.store.Update(fn.FunctionArn, func(body map[string]any) error {
		count, _ := body["nimbusInvocations"].(float64)
		body["nimbusInvocations"] = count + 1
		return nil
	})
}

// handlerLines is what the handler itself printed: the container's output
// with the emulator's own bookkeeping removed.
func handlerLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if strings.TrimSpace(line) == "" || isEmulatorNoise(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// isEmulatorNoise spots the lines the Runtime Interface Emulator writes
// itself rather than the ones the handler did.
//
// Measured: RIE prints its own START/END/REPORT trio, and its REPORT claims
// "Memory Size: 3008 MB" whatever the function was configured with, because
// the emulator does not implement memory limits. It also logs its internal
// state with a "(rapid)" tag, which real Lambda never shows. Passing all of
// that through meant every invocation appeared twice in the log group, once
// truthfully and once with the wrong memory size -- so the emulator's
// bookkeeping is dropped and Nimbus's own lines, which carry the request id
// the client was given and the memory the function really has, are kept.
func isEmulatorNoise(line string) bool {
	if strings.Contains(line, "(rapid)") {
		return true
	}
	for _, prefix := range []string{"START RequestId:", "END RequestId:", "REPORT RequestId:"} {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return true
		}
	}
	return false
}

// sha256Base64 is how Lambda states a package's digest: the SHA-256, base64
// rather than hex. Derived from the code, so two reads agree.
func sha256Base64(data []byte) string {
	sum := sha256.Sum256(data)
	return base64.StdEncoding.EncodeToString(sum[:])
}

func jsonBytes(v any) []byte {
	body, _ := json.Marshal(v)
	return body
}

func jsonInto(body []byte, into any) error {
	return json.Unmarshal(body, into)
}
