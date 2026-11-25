package aws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// DynamoDB Streams: the change log a table keeps of its own writes, which
// `StreamSpecification` round-tripped as a setting without ever being.
//
// Note the DIFFERENT target prefix. Streams is a separate API sharing one
// endpoint and one credential scope, so the prefix is all that tells a
// Streams call from a table call -- and routing on the scope alone answers
// every Streams call as if it were a table call.

// streamsCall replays a Streams call. Note the target PREFIX: that is the
// only thing distinguishing it from a table call on the wire.
func streamsCall(t *testing.T, s *Server, action, body string) (int, map[string]any) {
	t.Helper()
	req := dynamoRequestFor("DynamoDBStreams_20120810."+action, body)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, rec.Body)
		}
	}
	return rec.Code, doc
}

// dynamoRequestFor builds a JSON 1.0 request with an arbitrary target, so
// a test can reach the Streams API and the classifier alike.
func dynamoRequestFor(target, body string) *http.Request {
	if body == "" {
		body = "{}"
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", target)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/dynamodb/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=81bac114f836")
	return req
}

// dynamoCall is the table API, returning the status rather than the
// recorder so the two read alike here.
func dynamoCall(t *testing.T, s *Server, action, body string) (int, map[string]any) {
	t.Helper()
	rec, doc := dynamo(t, s, action, body)
	return rec.Code, doc
}

func makeStreamTable(t *testing.T, s *Server, name, viewType string) string {
	t.Helper()
	code, _ := dynamoCall(t, s, "CreateTable", `{
		"TableName": "`+name+`",
		"AttributeDefinitions": [{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema": [{"AttributeName":"id","KeyType":"HASH"}],
		"BillingMode": "PAY_PER_REQUEST",
		"StreamSpecification": {"StreamEnabled": true, "StreamViewType": "`+viewType+`"}}`)
	if code != 200 {
		t.Fatalf("creating %s: %d", name, code)
	}
	// The ARN comes off DescribeTable, which is where the provider reads
	// it -- not composed by the test, so a change to the shape shows up
	// here rather than being papered over.
	_, desc := dynamoCall(t, s, "DescribeTable", `{"TableName":"`+name+`"}`)
	table, _ := desc["Table"].(map[string]any)
	arn := str(table["LatestStreamArn"])
	if arn == "" {
		t.Fatalf("DescribeTable reports no LatestStreamArn: %v", table)
	}
	if label := str(table["LatestStreamLabel"]); label == "" {
		t.Error("DescribeTable reports no LatestStreamLabel")
	}
	return arn
}

// recordsOf reads a stream from the beginning.
func recordsOf(t *testing.T, s *Server, arn string) []map[string]any {
	t.Helper()
	code, iter := streamsCall(t, s, "GetShardIterator", `{
		"StreamArn": "`+arn+`", "ShardId": "`+streamShardID+`",
		"ShardIteratorType": "TRIM_HORIZON"}`)
	if code != 200 {
		t.Fatalf("GetShardIterator: %d %v", code, iter)
	}
	code, got := streamsCall(t, s, "GetRecords",
		`{"ShardIterator": "`+str(iter["ShardIterator"])+`"}`)
	if code != 200 {
		t.Fatalf("GetRecords: %d %v", code, got)
	}
	if str(got["NextShardIterator"]) == "" {
		// An absent NextShardIterator means the shard is CLOSED and a
		// consumer stops polling, which is never true of a live table.
		t.Error("GetRecords returned no NextShardIterator")
	}
	raw, _ := got["Records"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// A table with no stream has no stream: no ARN on its description, and
// nothing listed. An empty string rather than an absent field would make
// the provider flatten a stream block the configuration never had.
func TestATableWithoutAStreamHasNone(t *testing.T) {
	s := newDynamoTestServer(t)
	code, _ := dynamoCall(t, s, "CreateTable", `{
		"TableName": "plain",
		"AttributeDefinitions": [{"AttributeName":"id","AttributeType":"S"}],
		"KeySchema": [{"AttributeName":"id","KeyType":"HASH"}],
		"BillingMode": "PAY_PER_REQUEST"}`)
	if code != 200 {
		t.Fatalf("creating the table: %d", code)
	}
	_, desc := dynamoCall(t, s, "DescribeTable", `{"TableName":"plain"}`)
	table, _ := desc["Table"].(map[string]any)
	if _, present := table["LatestStreamArn"]; present {
		t.Errorf("a table with no stream reports LatestStreamArn: %v", table["LatestStreamArn"])
	}
	_, listed := streamsCall(t, s, "ListStreams", `{}`)
	if streams, _ := listed["Streams"].([]any); len(streams) != 0 {
		t.Errorf("ListStreams found %d streams, want 0", len(streams))
	}
}

// The three kinds of change, and the order they happened in.
func TestAStreamRecordsEveryKindOfChange(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "NEW_AND_OLD_IMAGES")

	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"1"}}}`)
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"2"}}}`)
	dynamoCall(t, s, "DeleteItem", `{"TableName":"events","Key":{"id":{"S":"1"}}}`)

	records := recordsOf(t, s, arn)
	if len(records) != 3 {
		t.Fatalf("the stream holds %d records, want 3", len(records))
	}
	want := []string{"INSERT", "MODIFY", "REMOVE"}
	for i, name := range want {
		if got := str(records[i]["eventName"]); got != name {
			t.Errorf("record %d is %q, want %q", i, got, name)
		}
	}

	// A record carries its keys, and the images the view type asks for.
	first, _ := records[0]["dynamodb"].(map[string]any)
	keys, _ := first["Keys"].(map[string]any)
	if len(keys) != 1 || keys["id"] == nil {
		t.Errorf("the record's Keys are %v, want just the key attribute", keys)
	}
	if _, has := first["OldImage"]; has {
		t.Error("an INSERT carries an OldImage")
	}
	if _, has := first["NewImage"]; !has {
		t.Error("an INSERT under NEW_AND_OLD_IMAGES carries no NewImage")
	}
	middle, _ := records[1]["dynamodb"].(map[string]any)
	if _, has := middle["OldImage"]; !has {
		t.Error("a MODIFY carries no OldImage")
	}
	last, _ := records[2]["dynamodb"].(map[string]any)
	if _, has := last["NewImage"]; has {
		t.Error("a REMOVE carries a NewImage")
	}
	if _, has := last["OldImage"]; !has {
		t.Error("a REMOVE carries no OldImage")
	}

	// Sequence numbers rise, and are decimal STRINGS on the wire.
	var previous string
	for i, rec := range records {
		body, _ := rec["dynamodb"].(map[string]any)
		seq, ok := body["SequenceNumber"].(string)
		if !ok {
			t.Fatalf("record %d's SequenceNumber is %T, want a string", i, body["SequenceNumber"])
		}
		if seq <= previous && previous != "" {
			t.Errorf("record %d's sequence %q does not follow %q", i, seq, previous)
		}
		previous = seq
	}
}

// The view type decides what travels, and it is the whole reason the
// setting exists: a KEYS_ONLY stream carrying images would hand a consumer
// data its configuration said it would not get.
func TestTheViewTypeDecidesWhatARecordCarries(t *testing.T) {
	cases := []struct {
		viewType         string
		wantNew, wantOld bool
	}{
		{"KEYS_ONLY", false, false},
		{"NEW_IMAGE", true, false},
		{"OLD_IMAGE", false, true},
		{"NEW_AND_OLD_IMAGES", true, true},
	}
	for _, tc := range cases {
		s := newDynamoTestServer(t)
		arn := makeStreamTable(t, s, "events", tc.viewType)
		dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"1"}}}`)
		dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"v":{"N":"2"}}}`)

		records := recordsOf(t, s, arn)
		if len(records) != 2 {
			t.Fatalf("%s: %d records, want 2", tc.viewType, len(records))
		}
		body, _ := records[1]["dynamodb"].(map[string]any)
		if _, has := body["NewImage"]; has != tc.wantNew {
			t.Errorf("%s: NewImage present=%v, want %v", tc.viewType, has, tc.wantNew)
		}
		if _, has := body["OldImage"]; has != tc.wantOld {
			t.Errorf("%s: OldImage present=%v, want %v", tc.viewType, has, tc.wantOld)
		}
		// The keys and the view type are on every record whatever it is.
		if str(body["StreamViewType"]) != tc.viewType {
			t.Errorf("%s: the record says StreamViewType %q", tc.viewType, body["StreamViewType"])
		}
		if keys, _ := body["Keys"].(map[string]any); len(keys) != 1 {
			t.Errorf("%s: the record's Keys are %v", tc.viewType, keys)
		}
	}
}

// An iterator reads FORWARD, and reading again from where the last read
// finished gives only what is new -- which is the whole mechanism a
// consumer runs on.
func TestAnIteratorReadsForward(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "NEW_IMAGE")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"}}}`)

	_, iter := streamsCall(t, s, "GetShardIterator", `{
		"StreamArn": "`+arn+`", "ShardId": "`+streamShardID+`",
		"ShardIteratorType": "TRIM_HORIZON"}`)
	_, first := streamsCall(t, s, "GetRecords",
		`{"ShardIterator": "`+str(iter["ShardIterator"])+`"}`)
	if got, _ := first["Records"].([]any); len(got) != 1 {
		t.Fatalf("the first read got %d records, want 1", len(got))
	}
	next := str(first["NextShardIterator"])

	// Nothing new yet, and the iterator must still come back -- "no
	// records yet" and "no more records ever" must not look the same.
	_, empty := streamsCall(t, s, "GetRecords", `{"ShardIterator": "`+next+`"}`)
	if got, _ := empty["Records"].([]any); len(got) != 0 {
		t.Errorf("reading again returned %d records, want 0", len(got))
	}
	if str(empty["NextShardIterator"]) == "" {
		t.Error("an empty read closed the shard")
	}

	// Write, and the same cursor picks it up.
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"2"}}}`)
	_, third := streamsCall(t, s, "GetRecords",
		`{"ShardIterator": "`+str(empty["NextShardIterator"])+`"}`)
	got, _ := third["Records"].([]any)
	if len(got) != 1 {
		t.Fatalf("after a write the cursor got %d records, want 1", len(got))
	}
	rec, _ := got[0].(map[string]any)
	body, _ := rec["dynamodb"].(map[string]any)
	image, _ := body["NewImage"].(map[string]any)
	id, _ := image["id"].(map[string]any)
	if str(id["S"]) != "2" {
		t.Errorf("the cursor returned item %v, want id 2", image)
	}
}

// LATEST starts at the end, which is how a consumer that only wants new
// changes avoids replaying the history.
func TestLatestSkipsWhatIsAlreadyThere(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"old"}}}`)

	_, iter := streamsCall(t, s, "GetShardIterator", `{
		"StreamArn": "`+arn+`", "ShardId": "`+streamShardID+`",
		"ShardIteratorType": "LATEST"}`)
	_, got := streamsCall(t, s, "GetRecords",
		`{"ShardIterator": "`+str(iter["ShardIterator"])+`"}`)
	if records, _ := got["Records"].([]any); len(records) != 0 {
		t.Errorf("LATEST returned %d existing records, want 0", len(records))
	}

	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"new"}}}`)
	_, after := streamsCall(t, s, "GetRecords",
		`{"ShardIterator": "`+str(got["NextShardIterator"])+`"}`)
	if records, _ := after["Records"].([]any); len(records) != 1 {
		t.Errorf("after a write LATEST's cursor got %d records, want 1", len(records))
	}
}

// AT and AFTER differ by exactly one record, which is the only thing that
// distinguishes them and the easiest pair to get backwards.
func TestAtAndAfterASequenceNumberDifferByOne(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")
	for _, id := range []string{"1", "2", "3"} {
		dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"`+id+`"}}}`)
	}
	records := recordsOf(t, s, arn)
	middle, _ := records[1]["dynamodb"].(map[string]any)
	seq := str(middle["SequenceNumber"])

	read := func(typ string) int {
		_, iter := streamsCall(t, s, "GetShardIterator", `{
			"StreamArn": "`+arn+`", "ShardId": "`+streamShardID+`",
			"ShardIteratorType": "`+typ+`", "SequenceNumber": "`+seq+`"}`)
		_, got := streamsCall(t, s, "GetRecords",
			`{"ShardIterator": "`+str(iter["ShardIterator"])+`"}`)
		n, _ := got["Records"].([]any)
		return len(n)
	}
	if at := read("AT_SEQUENCE_NUMBER"); at != 2 {
		t.Errorf("AT_SEQUENCE_NUMBER got %d records, want 2 (the named one and the one after)", at)
	}
	if after := read("AFTER_SEQUENCE_NUMBER"); after != 1 {
		t.Errorf("AFTER_SEQUENCE_NUMBER got %d records, want 1", after)
	}
}

// An iterator type nobody implemented is REFUSED, not defaulted: a cursor
// that silently started somewhere else makes a consumer miss records or
// replay them, and neither looks like an error.
func TestAnUnknownIteratorTypeIsRefused(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")

	code, doc := streamsCall(t, s, "GetShardIterator", `{
		"StreamArn": "`+arn+`", "ShardId": "`+streamShardID+`",
		"ShardIteratorType": "AT_TIMESTAMP"}`)
	if code != 400 {
		t.Errorf("an unknown iterator type was accepted with %d", code)
	}
	if !strings.Contains(str(doc["message"]), "AT_TIMESTAMP") {
		t.Errorf("the refusal does not name the type: %v", doc["message"])
	}
	// So is an iterator Nimbus did not issue, and a shard that is not the
	// one shard a stream has.
	if code, _ := streamsCall(t, s, "GetRecords", `{"ShardIterator": "someone-elses"}`); code != 400 {
		t.Errorf("a foreign iterator was accepted with %d", code)
	}
	if code, _ := streamsCall(t, s, "GetShardIterator", `{
		"StreamArn": "`+arn+`", "ShardId": "shardId-nonsense",
		"ShardIteratorType": "TRIM_HORIZON"}`); code != 400 {
		t.Errorf("an unknown shard was accepted with %d", code)
	}
	// And a stream ARN for nothing.
	if code, _ := streamsCall(t, s, "DescribeStream",
		`{"StreamArn": "arn:aws:dynamodb:us-east-1:000000000000:table/nope/stream/x"}`); code != 400 {
		t.Errorf("an unknown stream was accepted with %d", code)
	}
}

// Deleting an item that is not there succeeds, and a consumer must not
// hear about a change that did not happen.
func TestADeleteOfNothingIsNotAChange(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")

	dynamoCall(t, s, "DeleteItem", `{"TableName":"events","Key":{"id":{"S":"never-existed"}}}`)
	if records := recordsOf(t, s, arn); len(records) != 0 {
		t.Errorf("deleting an absent item produced %d records, want 0", len(records))
	}
}

// A batch write records every change in it, which is the path easiest to
// forget: it does not go through putItem or deleteItem.
func TestABatchWriteIsRecorded(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"gone"}}}`)

	code, _ := dynamoCall(t, s, "BatchWriteItem", `{"RequestItems": {"events": [
		{"PutRequest": {"Item": {"id": {"S": "a"}}}},
		{"PutRequest": {"Item": {"id": {"S": "b"}}}},
		{"DeleteRequest": {"Key": {"id": {"S": "gone"}}}}
	]}}`)
	if code != 200 {
		t.Fatalf("batch write: %d", code)
	}
	records := recordsOf(t, s, arn)
	if len(records) != 4 {
		t.Fatalf("the stream holds %d records, want 4 (one put plus three in the batch)", len(records))
	}
	names := ""
	for _, rec := range records[1:] {
		names += str(rec["eventName"]) + " "
	}
	if names != "INSERT INSERT REMOVE " {
		t.Errorf("the batch recorded %q", names)
	}
}

// An UpdateItem records a MODIFY with the images either side of it, which
// is the path a real consumer sees most.
func TestAnUpdateRecordsBothImages(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "NEW_AND_OLD_IMAGES")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"},"n":{"N":"1"}}}`)
	code, _ := dynamoCall(t, s, "UpdateItem", `{
		"TableName":"events","Key":{"id":{"S":"1"}},
		"UpdateExpression":"SET n = :two",
		"ExpressionAttributeValues":{":two":{"N":"2"}}}`)
	if code != 200 {
		t.Fatalf("update: %d", code)
	}
	records := recordsOf(t, s, arn)
	if len(records) != 2 {
		t.Fatalf("%d records, want 2", len(records))
	}
	body, _ := records[1]["dynamodb"].(map[string]any)
	if str(records[1]["eventName"]) != "MODIFY" {
		t.Errorf("the update recorded %q", records[1]["eventName"])
	}
	oldImage, _ := body["OldImage"].(map[string]any)
	newImage, _ := body["NewImage"].(map[string]any)
	oldN, _ := oldImage["n"].(map[string]any)
	newN, _ := newImage["n"].(map[string]any)
	if str(oldN["N"]) != "1" || str(newN["N"]) != "2" {
		t.Errorf("the images are old=%v new=%v, want 1 then 2", oldImage, newImage)
	}
}

// The stream goes with the table, or a table created again under the same
// name would inherit the previous one's change log.
func TestDeletingATableDropsItsStream(t *testing.T) {
	s := newDynamoTestServer(t)
	makeStreamTable(t, s, "events", "KEYS_ONLY")
	dynamoCall(t, s, "PutItem", `{"TableName":"events","Item":{"id":{"S":"1"}}}`)
	if code, _ := dynamoCall(t, s, "DeleteTable", `{"TableName":"events"}`); code != 200 {
		t.Fatal("deleting the table failed")
	}

	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")
	if records := recordsOf(t, s, arn); len(records) != 0 {
		t.Errorf("a table recreated under the same name inherited %d records", len(records))
	}
}

// A Streams call is classified against the STREAM, not the table -- and
// the body for these calls carries no table name to build one from.
func TestStreamsActionsAreClassifiedAgainstTheStream(t *testing.T) {
	s := newDynamoTestServer(t)
	arn := makeStreamTable(t, s, "events", "KEYS_ONLY")

	r := dynamoRequestFor("DynamoDBStreams_20120810.DescribeStream", `{"StreamArn":"`+arn+`"}`)
	got, ok := s.dynamoTarget(r)
	if !ok {
		t.Fatal("a Streams call was not classified at all")
	}
	if got.Action != "dynamodb:DescribeStream" {
		t.Errorf("the action is %q", got.Action)
	}
	if got.Resource != arn {
		t.Errorf("the resource is %q, want the stream ARN %q", got.Resource, arn)
	}

	// A GetRecords names nothing but an opaque iterator, so there is
	// nothing to narrow to and "*" is the honest answer.
	r = dynamoRequestFor("DynamoDBStreams_20120810.GetRecords", `{"ShardIterator":"nimbus-iter:events:0"}`)
	got, _ = s.dynamoTarget(r)
	if got.Resource != "*" {
		t.Errorf("GetRecords names resource %q, want \"*\"", got.Resource)
	}
}
