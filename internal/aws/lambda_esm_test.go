package aws

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Event source mappings: the poller that invokes a function from a stream.
//
// The thing worth pinning is that a function does NOT subscribe to
// anything. A poller owned by the Lambda service reads the stream and
// calls the function, which is why a function with no mapping is never
// called however much is written to the table.

// fakeRIE stands in for the Runtime Interface Emulator inside a
// function's container: the engine invokes over real HTTP at
// 127.0.0.1:{port}, so a local server on that port exercises the REAL
// invoke path rather than a seam added for the test.
//
// Driving the production path matters here. A test-only hook on the
// engine would have proved the poller calls SOMETHING; this proves it
// speaks the protocol a handler answers, with the payload a handler
// receives.
type fakeRIE struct {
	srv *httptest.Server

	mu            sync.Mutex
	calls         int
	lastPayload   []byte
	functionError string
	response      []byte
}

func newFakeRIE(t *testing.T) *fakeRIE {
	t.Helper()
	r := &fakeRIE{response: []byte(`{"ok":true}`)}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.calls++
		r.lastPayload = body
		fnErr, resp := r.functionError, r.response
		r.mu.Unlock()

		if fnErr != "" {
			// Whether a handler threw is a HEADER, not a shape in the
			// body -- which is what the engine reads.
			w.Header().Set("X-Amz-Function-Error", fnErr)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRIE) port(t *testing.T) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(r.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func (r *fakeRIE) calledTimes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *fakeRIE) payload() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.lastPayload...)
}

func (r *fakeRIE) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls, r.lastPayload = 0, nil
}

func (r *fakeRIE) setFunctionError(e string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.functionError = e
}

// esmServer is a Lambda server with a function whose "container" is the
// fake RIE above, registered in the engine the way a real start would.
func esmServer(t *testing.T) (*Server, *fakeRIE) {
	t.Helper()
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	rie := newFakeRIE(t)
	s.lambda.Put(&funcs.Function{
		ResourceID: s.lambdaARN("writer"),
		Name:       "writer",
		Container:  "nimbus-fake-writer",
		Port:       rie.port(t),
		Started:    time.Now(),
	})
	return s, rie
}

func esmCall(t *testing.T, s *Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	rec, doc := lambdaCall(t, s, method, path, body)
	return rec.Code, doc
}

// makeStreamingTable creates a table with a stream and returns its ARN.
func makeStreamingTable(t *testing.T, s *Server, name string) string {
	t.Helper()
	code, _ := dynamoCall(t, s, "CreateTable", `{
		"TableName": "`+name+`",
		"AttributeDefinitions": [{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema": [{"AttributeName":"id","KeyType":"HASH"}],
		"BillingMode": "PAY_PER_REQUEST",
		"StreamSpecification": {"StreamEnabled": true, "StreamViewType": "NEW_AND_OLD_IMAGES"}}`)
	if code != 200 {
		t.Fatalf("creating %s: %d", name, code)
	}
	_, desc := dynamoCall(t, s, "DescribeTable", `{"TableName":"`+name+`"}`)
	table, _ := desc["Table"].(map[string]any)
	return str(table["LatestStreamArn"])
}

// A mapping must name an event source that EXISTS and that Nimbus can
// poll. A mapping reporting Enabled and silently delivering nothing is
// indistinguishable from a handler that is never reached.
func TestAMappingRefusesAnEventSourceItCannotPoll(t *testing.T) {
	s, _ := esmServer(t)

	cases := []struct{ arn, expect string }{
		{"arn:aws:dynamodb:us-east-1:000000000000:table/nope/stream/2026-01-01T00:00:00.000",
			"no stream enabled"},
		{"arn:aws:kinesis:us-east-1:000000000000:stream/events", "DynamoDB streams only"},
		{"arn:aws:sqs:us-east-1:000000000000:queue", "DynamoDB streams only"},
	}
	for _, tc := range cases {
		code, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
			`{"EventSourceArn":"`+tc.arn+`","FunctionName":"writer"}`)
		if code != 400 {
			t.Errorf("%s was accepted with %d", tc.arn, code)
		}
		if !strings.Contains(str(doc["message"]), tc.expect) {
			t.Errorf("the refusal for %s does not say why: %v", tc.arn, doc["message"])
		}
	}

	// A table with a stream is accepted.
	arn := makeStreamingTable(t, s, "events")
	code, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer"}`)
	if code != 202 {
		t.Fatalf("a real stream was refused: %d %v", code, doc)
	}
	if str(doc["State"]) != "Enabled" {
		t.Errorf("a new mapping reports State %q, want Enabled", doc["State"])
	}
	if str(doc["UUID"]) == "" {
		t.Error("the mapping has no UUID")
	}
	// Creating the same mapping twice is a conflict, not two pollers on
	// one stream -- which would deliver every record to the handler twice.
	if code, _ := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer"}`); code != 409 {
		t.Errorf("a duplicate mapping was accepted with %d", code)
	}
}

// The central claim: writing to the table really invokes the handler, with
// the records in the payload.
func TestAMappingDeliversStreamRecordsToTheHandler(t *testing.T) {
	s, rie := esmServer(t)
	arn := makeStreamingTable(t, s, "events")

	// The Docker fake answers every invocation, and remembers the payload
	// it was given -- which is what the assertion is about.

	code, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer","StartingPosition":"TRIM_HORIZON","BatchSize":10}`)
	if code != 202 {
		t.Fatalf("creating the mapping: %d %v", code, doc)
	}
	uuid := str(doc["UUID"])

	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"1"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"2"}}}`)

	payload := waitForInvocation(t, rie)
	var event struct {
		Records []struct {
			EventName      string         `json:"eventName"`
			EventSourceARN string         `json:"eventSourceARN"`
			Dynamodb       map[string]any `json:"dynamodb"`
		} `json:"Records"`
	}
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("the handler was given something that is not a stream event: %v\n%s", err, payload)
	}
	if len(event.Records) == 0 {
		t.Fatal("the handler was invoked with no records")
	}
	if event.Records[0].EventName != "INSERT" {
		t.Errorf("the first record is %q, want INSERT", event.Records[0].EventName)
	}
	// eventSourceARN is added by the POLLER: the stream itself does not
	// carry it, and a function mapped to two tables has to tell them apart.
	if event.Records[0].EventSourceARN != arn {
		t.Errorf("the record names source %q, want %q", event.Records[0].EventSourceARN, arn)
	}
	if event.Records[0].Dynamodb["Keys"] == nil {
		t.Error("the record carries no Keys")
	}

	// The mapping reports what happened, in the field real Lambda uses.
	waitFor(t, func() bool {
		_, got := esmCall(t, s, http.MethodGet, "/2015-03-31/event-source-mappings/"+uuid, "")
		return str(got["LastProcessingResult"]) == "OK"
	}, "LastProcessingResult never became OK")
}

// A DISABLED mapping delivers nothing, and enabling it starts delivery
// without recreating anything.
func TestADisabledMappingDeliversNothing(t *testing.T) {
	s, rie := esmServer(t)
	arn := makeStreamingTable(t, s, "events")

	_, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer","Enabled":false,"StartingPosition":"TRIM_HORIZON"}`)
	uuid := str(doc["UUID"])
	if str(doc["State"]) != "Disabled" {
		t.Errorf("a mapping created with Enabled=false reports %q", doc["State"])
	}

	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"}}}`)
	time.Sleep(3 * esmPollInterval)
	if n := rie.calledTimes(); n != 0 {
		t.Fatalf("a disabled mapping invoked the handler %d times", n)
	}

	// Enabling it is a partial update, and the records that were written
	// while it was off are still there to deliver.
	if code, _ := esmCall(t, s, http.MethodPut, "/2015-03-31/event-source-mappings/"+uuid,
		`{"Enabled":true}`); code != 202 {
		t.Fatal("enabling the mapping failed")
	}
	waitForInvocation(t, rie)
}

// LATEST skips what is already in the stream; TRIM_HORIZON does not. This
// is the pair that is easiest to get backwards, and getting it wrong
// either replays every record a table ever saw or silently drops them.
func TestStartingPositionDecidesWhatIsReplayed(t *testing.T) {
	for _, tc := range []struct {
		position string
		want     bool // should the existing record be delivered?
	}{{"TRIM_HORIZON", true}, {"LATEST", false}} {
		s, rie := esmServer(t)
		arn := makeStreamingTable(t, s, "events")

		// Written BEFORE the mapping exists.
		dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"before"}}}`)

		esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
			`{"EventSourceArn":"`+arn+`","FunctionName":"writer","StartingPosition":"`+tc.position+`"}`)

		time.Sleep(4 * esmPollInterval)
		got := rie.calledTimes() > 0
		if got != tc.want {
			t.Errorf("%s: the existing record was delivered=%v, want %v", tc.position, got, tc.want)
		}

		// Either way, a record written AFTER the mapping is delivered.
		dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"after"}}}`)
		waitForInvocation(t, rie)
	}
}

// Deleting the mapping stops delivery: a handler must not go on being
// invoked by something the client removed.
//
// Note what this does NOT prove. The poller re-reads its mapping every
// tick and returns when it is gone, so delivery would stop even without
// stopPoller -- which is there for PROMPTNESS, so a deleted mapping does
// not get one more tick's worth of records. Injecting its removal leaves
// this test green, and that is honest rather than a gap: the behaviour
// being asserted really does hold either way.
func TestDeletingAMappingStopsDelivery(t *testing.T) {
	s, rie := esmServer(t)
	arn := makeStreamingTable(t, s, "events")

	_, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer","StartingPosition":"TRIM_HORIZON"}`)
	uuid := str(doc["UUID"])
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"}}}`)
	waitForInvocation(t, rie)

	if code, _ := esmCall(t, s, http.MethodDelete,
		"/2015-03-31/event-source-mappings/"+uuid, ""); code != 202 {
		t.Fatal("deleting the mapping failed")
	}
	if code, _ := esmCall(t, s, http.MethodGet,
		"/2015-03-31/event-source-mappings/"+uuid, ""); code != 404 {
		t.Error("the deleted mapping still reads")
	}

	rie.reset()
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"2"}}}`)
	time.Sleep(4 * esmPollInterval)
	if n := rie.calledTimes(); n != 0 {
		t.Errorf("a deleted mapping invoked the handler %d times", n)
	}
}

// A mapping survives a restart in the store; its POLLER does not. Without
// reattaching one, a mapping reads Enabled for ever and delivers nothing
// -- the log-group index's bug in another costume.
//
// The restart goes through the CLOUD's own reattach, not through
// reattachPollers directly. The first version of this test called the
// latter, and removing the call from Server.reattach left it green --
// which is this repo's signature trap, named in CLAUDE.md about
// vm.Engine.Reattach: "a unit test proving a method works while nothing
// calls it". Found by injecting exactly that.
func TestARestartRestartsThePollers(t *testing.T) {
	s, rie := esmServer(t)
	arn := makeStreamingTable(t, s, "events")

	esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer","StartingPosition":"TRIM_HORIZON"}`)

	// Stop every poller, as a process exit would, then reattach as a
	// restart does. The mapping is still in the store throughout.
	s.stopPollers()
	rie.reset()
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"while-down"}}}`)
	time.Sleep(3 * esmPollInterval)
	if n := rie.calledTimes(); n != 0 {
		t.Fatalf("a stopped poller delivered %d times", n)
	}

	s.reattach(context.Background())
	// Server.reattach rebuilds the function engine's registry from Docker,
	// which in this test has no containers -- so the stand-in is
	// registered again here, exactly as a real reattach re-adopts the real
	// container. The poller is already running by now and picks it up on
	// its next tick, which is also what happens for real.
	s.lambda.Put(&funcs.Function{
		ResourceID: s.lambdaARN("writer"), Name: "writer",
		Container: "nimbus-fake-writer", Port: rie.port(t), Started: time.Now(),
	})
	waitForInvocation(t, rie)
}

// A handler that THREW is a failed batch, not a delivered one. Reporting
// success would quietly drop every record the handler could not process.
func TestAHandlerErrorIsAFailedBatch(t *testing.T) {
	s, rie := esmServer(t)
	arn := makeStreamingTable(t, s, "events")
	rie.setFunctionError("Unhandled")

	_, doc := esmCall(t, s, http.MethodPost, "/2015-03-31/event-source-mappings",
		`{"EventSourceArn":"`+arn+`","FunctionName":"writer","StartingPosition":"TRIM_HORIZON"}`)
	uuid := str(doc["UUID"])
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"}}}`)

	waitFor(t, func() bool {
		_, got := esmCall(t, s, http.MethodGet, "/2015-03-31/event-source-mappings/"+uuid, "")
		return strings.Contains(str(got["LastProcessingResult"]), "Unhandled")
	}, "the failure never reached LastProcessingResult")

	// It was RETRIED, and then the poller moved past it rather than
	// wedging the stream for ever -- a stated divergence from real Lambda,
	// which blocks the shard until the records expire.
	if n := rie.calledTimes(); n < esmMaxAttempts {
		t.Errorf("the batch was attempted %d times, want at least %d", n, esmMaxAttempts)
	}
	rie.reset()
	rie.setFunctionError("")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"2"}}}`)
	waitForInvocation(t, rie)
}

// --- helpers ---------------------------------------------------------------

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForInvocation(t *testing.T, rie *fakeRIE) []byte {
	t.Helper()
	var payload []byte
	waitFor(t, func() bool {
		payload = rie.payload()
		return len(payload) > 0
	}, "the poller never invoked the handler")
	return payload
}
