package aws

import (
	"strconv"
	"testing"
	"time"
)

// Time to live. `UpdateTimeToLive` round-tripped as a setting and nothing
// ever expired -- the shape of divergence this project keeps finding: a
// configuration that applies cleanly and does nothing.

func enableTTL(t *testing.T, s *Server, table, attr string) {
	t.Helper()
	code, _ := dynamoCall(t, s, "UpdateTimeToLive", `{
		"TableName":"`+table+`",
		"TimeToLiveSpecification":{"Enabled":true,"AttributeName":"`+attr+`"}}`)
	if code != 200 {
		t.Fatalf("enabling TTL: %d", code)
	}
	_, desc := dynamoCall(t, s, "DescribeTimeToLive", `{"TableName":"`+table+`"}`)
	d, _ := desc["TimeToLiveDescription"].(map[string]any)
	if str(d["TimeToLiveStatus"]) != "ENABLED" || str(d["AttributeName"]) != attr {
		t.Fatalf("TTL did not round-trip: %v", d)
	}
}

func plainTable(t *testing.T, s *Server, name string) {
	t.Helper()
	code, _ := dynamoCall(t, s, "CreateTable", `{
		"TableName": "`+name+`",
		"AttributeDefinitions": [{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema": [{"AttributeName":"id","KeyType":"HASH"}],
		"BillingMode": "PAY_PER_REQUEST"}`)
	if code != 200 {
		t.Fatalf("creating %s: %d", name, code)
	}
}

func itemCount(t *testing.T, s *Server, table string) int {
	t.Helper()
	_, doc := dynamoCall(t, s, "Scan", `{"TableName":"`+table+`"}`)
	n, _ := doc["Count"].(float64)
	return int(n)
}

func past() string   { return strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10) }
func future() string { return strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) }

// The central claim: an item whose TTL attribute has passed is deleted,
// and one whose has not is left alone.
func TestAnExpiredItemIsSweptAway(t *testing.T) {
	s := newDynamoTestServer(t)
	plainTable(t, s, "sessions")
	enableTTL(t, s, "sessions", "expiresAt")

	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"stale"},"expiresAt":{"N":"`+past()+`"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"fresh"},"expiresAt":{"N":"`+future()+`"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"forever"}}}`)

	if n := itemCount(t, s, "sessions"); n != 3 {
		t.Fatalf("before the sweep the table holds %d items, want 3", n)
	}
	// The sweep is called directly rather than waited for: a test that
	// slept would be racing a ticker, and what is being asserted is what
	// the sweep DOES, not how often it runs.
	if removed := s.sweepExpired(time.Now()); removed != 1 {
		t.Errorf("the sweep removed %d items, want 1", removed)
	}
	if n := itemCount(t, s, "sessions"); n != 2 {
		t.Errorf("after the sweep the table holds %d items, want 2", n)
	}
	if _, got := dynamoCall(t, s, "GetItem", `{"TableName":"sessions","Key":{"id":{"S":"stale"}}}`); got["Item"] != nil {
		t.Error("the expired item is still readable")
	}
	for _, id := range []string{"fresh", "forever"} {
		_, got := dynamoCall(t, s, "GetItem", `{"TableName":"sessions","Key":{"id":{"S":"`+id+`"}}}`)
		if got["Item"] == nil {
			t.Errorf("%q was swept and should not have been", id)
		}
	}
}

// TTL that is not ENABLED expires nothing, whatever the items say. The
// attribute is an ordinary attribute until the table opts in.
func TestNothingExpiresWithoutTTLEnabled(t *testing.T) {
	s := newDynamoTestServer(t)
	plainTable(t, s, "sessions")
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"stale"},"expiresAt":{"N":"`+past()+`"}}}`)

	if removed := s.sweepExpired(time.Now()); removed != 0 {
		t.Errorf("a table without TTL lost %d items", removed)
	}

	// Enabling and then DISABLING it stops expiry again.
	enableTTL(t, s, "sessions", "expiresAt")
	dynamoCall(t, s, "UpdateTimeToLive", `{
		"TableName":"sessions",
		"TimeToLiveSpecification":{"Enabled":false,"AttributeName":"expiresAt"}}`)
	if removed := s.sweepExpired(time.Now()); removed != 0 {
		t.Errorf("a table with TTL disabled lost %d items", removed)
	}
	if n := itemCount(t, s, "sessions"); n != 1 {
		t.Errorf("the table holds %d items, want 1", n)
	}
}

// An attribute Nimbus cannot read as a unix time leaves the item ALONE.
// Real DynamoDB is explicit about this, and deleting too much is the one
// mistake here that cannot be undone -- so a misreading must not delete.
func TestAnUnreadableTTLAttributeExpiresNothing(t *testing.T) {
	s := newDynamoTestServer(t)
	plainTable(t, s, "sessions")
	enableTTL(t, s, "sessions", "expiresAt")

	cases := map[string]string{
		"a string, not a number": `{"id":{"S":"a"},"expiresAt":{"S":"` + past() + `"}}`,
		"not a number at all":    `{"id":{"S":"b"},"expiresAt":{"N":"soon"}}`,
		"the wrong attribute":    `{"id":{"S":"c"},"ttl":{"N":"` + past() + `"}}`,
		"a boolean":              `{"id":{"S":"d"},"expiresAt":{"BOOL":true}}`,
	}
	for name, item := range cases {
		if code, _ := dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":`+item+`}`); code != 200 {
			t.Fatalf("%s: put failed %d", name, code)
		}
	}
	if removed := s.sweepExpired(time.Now()); removed != 0 {
		t.Errorf("the sweep removed %d items it could not read a time from", removed)
	}
	if n := itemCount(t, s, "sessions"); n != len(cases) {
		t.Errorf("the table holds %d items, want %d", n, len(cases))
	}
}

// A TTL deletion reaches the STREAM as an ordinary REMOVE, which is the
// whole point: the pattern people use TTL for is to archive what expired,
// and that needs the removal delivered. The userIdentity is the ONLY
// thing distinguishing it from a client's own DeleteItem.
func TestATTLDeletionReachesTheStreamAsAService(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "sessions", "OLD_IMAGE")
	enableTTL(t, s, "sessions", "expiresAt")

	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"stale"},"expiresAt":{"N":"`+past()+`"}}}`)
	dynamoCall(t, s, "DeleteItem", `{"TableName":"sessions","Key":{"id":{"S":"stale"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"other"},"expiresAt":{"N":"`+past()+`"}}}`)
	s.sweepExpired(time.Now())

	records := recordsOf(t, s, arn)
	if len(records) != 4 {
		t.Fatalf("the stream holds %d records, want 4 (two puts, a delete, an expiry)", len(records))
	}
	clientDelete, expiry := records[1], records[3]
	if str(clientDelete["eventName"]) != "REMOVE" || str(expiry["eventName"]) != "REMOVE" {
		t.Fatalf("expected two REMOVEs, got %q and %q",
			clientDelete["eventName"], expiry["eventName"])
	}
	// A client's own delete carries NO userIdentity.
	if _, has := clientDelete["userIdentity"]; has {
		t.Error("a client's DeleteItem was stamped as a service action")
	}
	// CAPITALISED: the Streams API's own spelling, which is what botocore
	// reads. Lowercase keys came back as an empty object from
	// `aws dynamodbstreams get-records` -- present, and saying nothing.
	identity, _ := expiry["userIdentity"].(map[string]any)
	if str(identity["PrincipalId"]) != "dynamodb.amazonaws.com" || str(identity["Type"]) != "Service" {
		t.Errorf("the expiry's userIdentity is %v, want the service principal", expiry["userIdentity"])
	}
	// And the OLD_IMAGE travels, so a consumer can archive what went.
	body, _ := expiry["dynamodb"].(map[string]any)
	if body["OldImage"] == nil {
		t.Error("the expiry carries no OldImage, so nothing could archive it")
	}
}

// Expiry is at the SECOND, and the boundary is inclusive: real DynamoDB
// expires an item whose timestamp is now, not strictly in the past.
func TestTheExpiryBoundaryIsTheSecond(t *testing.T) {
	s := newDynamoTestServer(t)
	plainTable(t, s, "sessions")
	enableTTL(t, s, "sessions", "expiresAt")
	now := time.Unix(1_800_000_000, 0)

	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"exactly-now"},"expiresAt":{"N":"1800000000"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"a-second-later"},"expiresAt":{"N":"1800000001"}}}`)

	if removed := s.sweepExpired(now); removed != 1 {
		t.Errorf("at the boundary the sweep removed %d, want 1", removed)
	}
	if _, got := dynamoCall(t, s, "GetItem", `{"TableName":"sessions","Key":{"id":{"S":"a-second-later"}}}`); got["Item"] == nil {
		t.Error("an item one second in the future was swept")
	}
}

// The sweeper is wired into the server's own startup, not merely
// written -- this repo's signature trap is a method that works and is
// called by nobody.
func TestTheSweeperRunsOnItsOwn(t *testing.T) {
	s := newDynamoTestServer(t)
	plainTable(t, s, "sessions")
	enableTTL(t, s, "sessions", "expiresAt")
	dynamoCall(t, s, "PutItem", `{"TableName":"sessions","Item":{"id":{"S":"stale"},"expiresAt":{"N":"`+past()+`"}}}`)

	stop := s.Start()
	defer stop()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if itemCount(t, s, "sessions") == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the expired item was never swept by the running server")
}

// The Streams API and the Lambda EVENT spell the identity differently,
// and each client finds nothing in the other's spelling. Measured:
// `aws dynamodbstreams get-records` reported an empty object for
// lowercase keys, because botocore drops a member its model does not
// have; a Python handler reads record["userIdentity"]["principalId"].
func TestTheIdentitySpellingDiffersBetweenTheApiAndTheEvent(t *testing.T) {
	api := map[string]any{"PrincipalId": "dynamodb.amazonaws.com", "Type": "Service"}
	event := lambdaUserIdentity(api)
	if str(event["principalId"]) != "dynamodb.amazonaws.com" || str(event["type"]) != "Service" {
		t.Errorf("the Lambda event spelling is %v", event)
	}
	if lambdaUserIdentity(nil) != nil {
		t.Error("a record with no identity must not gain an empty one")
	}

	// And the whole way through: a record the poller builds carries the
	// event spelling, not the API's.
	records := []streamRecord{{EventName: "REMOVE", UserIdentity: api}}
	out := streamEventRecords(records, "arn:aws:dynamodb:::table/x/stream/y")
	entry, _ := out[0].(map[string]any)
	got, _ := entry["userIdentity"].(map[string]any)
	if str(got["principalId"]) != "dynamodb.amazonaws.com" {
		t.Errorf("the handler would receive %v", entry["userIdentity"])
	}
	if _, wrong := got["PrincipalId"]; wrong {
		t.Error("the API spelling leaked into the Lambda event")
	}
}
