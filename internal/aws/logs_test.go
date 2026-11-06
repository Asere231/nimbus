package aws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// CloudWatch Logs is JSON 1.1 and spells its fields in lowerCamelCase,
// where DynamoDB -- JSON 1.0 behind the same port -- spells them in
// TitleCase. Bodies captured from aws-cli 1.40.33.

func newLogsTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	return New(Config{PublicURL: "http://localhost:4702"}, store, bus)
}

func logs(t *testing.T, s *Server, action, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "Logs_20140328."+action)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/logs/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=abc")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("not JSON: %v\n%s", err, rec.Body)
		}
	}
	return rec, doc
}

func mustLogs(t *testing.T, s *Server, action, body string) map[string]any {
	t.Helper()
	rec, doc := logs(t, s, action, body)
	if rec.Code != 200 {
		t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body)
	}
	return doc
}

func TestLogGroupsStreamsAndEvents(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/writer"}`)

	doc := mustLogs(t, s, "DescribeLogGroups", "{}")
	groups := doc["logGroups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("logGroups = %v", groups)
	}
	group := groups[0].(map[string]any)
	// The field names are the service's, lowerCamelCase.
	for _, f := range []string{"logGroupName", "arn", "creationTime", "storedBytes"} {
		if group[f] == nil {
			t.Errorf("a log group is missing %q", f)
		}
	}

	mustLogs(t, s, "CreateLogStream",
		`{"logGroupName": "/aws/lambda/writer", "logStreamName": "s1"}`)
	mustLogs(t, s, "PutLogEvents", `{"logGroupName": "/aws/lambda/writer",
		"logStreamName": "s1",
		"logEvents": [{"timestamp": 1700000000000, "message": "hello"},
		              {"timestamp": 1700000001000, "message": "goodbye"}]}`)

	doc = mustLogs(t, s, "GetLogEvents",
		`{"logGroupName": "/aws/lambda/writer", "logStreamName": "s1"}`)
	events := doc["events"].([]any)
	if len(events) != 2 || events[0].(map[string]any)["message"] != "hello" {
		t.Fatalf("events = %v", events)
	}
	// Both tokens must be present: a client pages until the token stops
	// changing, and a missing one reads as "start again".
	if doc["nextForwardToken"] == nil || doc["nextBackwardToken"] == nil {
		t.Errorf("GetLogEvents = %v", doc)
	}

	doc = mustLogs(t, s, "DescribeLogStreams", `{"logGroupName": "/aws/lambda/writer"}`)
	stream := doc["logStreams"].([]any)[0].(map[string]any)
	if stream["firstEventTimestamp"] != 1.7e12 {
		t.Errorf("firstEventTimestamp = %v", stream["firstEventTimestamp"])
	}
}

// FilterLogEvents reads ACROSS streams, which is how a group's lines read
// as one log -- and it is what `aws logs tail` uses.
func TestFilterReadsEveryStreamInTimeOrder(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "g"}`)
	mustLogs(t, s, "PutLogEvents", `{"logGroupName": "g", "logStreamName": "late",
		"logEvents": [{"timestamp": 2000, "message": "second"}]}`)
	mustLogs(t, s, "PutLogEvents", `{"logGroupName": "g", "logStreamName": "early",
		"logEvents": [{"timestamp": 1000, "message": "first"}]}`)

	doc := mustLogs(t, s, "FilterLogEvents", `{"logGroupName": "g"}`)
	events := doc["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("events = %v", events)
	}
	if events[0].(map[string]any)["message"] != "first" {
		t.Errorf("events came back out of order: %v", events)
	}
	// Each event says which stream it came from, which is the only way a
	// reader can tell two invocations apart.
	if events[0].(map[string]any)["logStreamName"] != "early" {
		t.Errorf("logStreamName = %v", events[0])
	}

	doc = mustLogs(t, s, "FilterLogEvents",
		`{"logGroupName": "g", "filterPattern": "second"}`)
	if len(doc["events"].([]any)) != 1 {
		t.Errorf("a filter pattern matched %v", doc["events"])
	}
	doc = mustLogs(t, s, "FilterLogEvents",
		`{"logGroupName": "g", "startTime": 1500}`)
	if len(doc["events"].([]any)) != 1 {
		t.Errorf("startTime matched %v", doc["events"])
	}
}

// Writing to a group nobody created is refused, as CloudWatch refuses it:
// a silently created group hides a typo in the name, and then the lines are
// nowhere anybody looks.
func TestWritingToAnUnknownGroupIsRefused(t *testing.T) {
	s := newLogsTestServer(t)
	rec, doc := logs(t, s, "PutLogEvents",
		`{"logGroupName": "nope", "logStreamName": "s", "logEvents": []}`)
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if doc["__type"] != "ResourceNotFoundException" {
		t.Errorf("__type = %v", doc["__type"])
	}
}

func TestCreatingAGroupTwiceIsAlreadyExists(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "g"}`)
	rec, doc := logs(t, s, "CreateLogGroup", `{"logGroupName": "g"}`)
	if rec.Code != 400 || doc["__type"] != "ResourceAlreadyExistsException" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// A retention policy has to survive the read it is written for, like every
// other value a client compares between refreshes.
func TestRetentionIsRememberedAndReadBack(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "g"}`)
	mustLogs(t, s, "PutRetentionPolicy", `{"logGroupName": "g", "retentionInDays": 7}`)
	doc := mustLogs(t, s, "DescribeLogGroups", `{"logGroupNamePrefix": "g"}`)
	group := doc["logGroups"].([]any)[0].(map[string]any)
	if group["retentionInDays"] != 7.0 {
		t.Errorf("retentionInDays = %v", group["retentionInDays"])
	}
	mustLogs(t, s, "DeleteRetentionPolicy", `{"logGroupName": "g"}`)
	doc = mustLogs(t, s, "DescribeLogGroups", `{"logGroupNamePrefix": "g"}`)
	group = doc["logGroups"].([]any)[0].(map[string]any)
	if _, ok := group["retentionInDays"]; ok {
		t.Errorf("retention survived its deletion: %v", group)
	}
}

func TestAnUnknownLogsActionIsFourHundred(t *testing.T) {
	s := newLogsTestServer(t)
	rec, doc := logs(t, s, "StartQuery", `{}`)
	if rec.Code != 400 {
		t.Fatalf("status %d; a 5xx would be retried five times", rec.Code)
	}
	if !strings.Contains(str(doc["message"]), "StartQuery") {
		t.Errorf("the refusal should name the action, for the activity feed: %v", doc["message"])
	}
}

// A log group draws in the visualizer, so it is a resource in the store
// like everything else -- which is also what makes a Lambda's group appear
// the moment the function does.
func TestALogGroupIsAResource(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/writer"}`)
	found := s.store.List(func(r *core.Resource) bool { return r.Type == typeLogGroup })
	if len(found) != 1 || found[0].Name != "/aws/lambda/writer" {
		t.Fatalf("store holds %v", found)
	}
	if found[0].Cloud != "aws" {
		t.Errorf("cloud = %q", found[0].Cloud)
	}
	mustLogs(t, s, "DeleteLogGroup", `{"logGroupName": "/aws/lambda/writer"}`)
	if len(s.store.List(func(r *core.Resource) bool { return r.Type == typeLogGroup })) != 0 {
		t.Error("deleting a group left the resource behind")
	}
}
