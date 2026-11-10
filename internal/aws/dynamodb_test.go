package aws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Every body here was captured from aws-cli 1.40.33 against a logging
// server, not written from the documentation.

func newDynamoTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "http://localhost:4702", DataDir: t.TempDir()}, store, bus)
	// No unit test may reach the real Docker daemon: a handler that
	// starts a container would leave it running after the package passed.
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

// dynamo replays a JSON 1.0 call: the action is in X-Amz-Target, the body is
// JSON, the path is "/", and the credential scope still says the service.
func dynamo(t *testing.T, s *Server, action, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	if body == "" {
		body = "{}"
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-amz-json-1.0")
	req.Header.Set("X-Amz-Target", "DynamoDB_20120810."+action)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/dynamodb/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date;x-amz-target, Signature=81bac114f836")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, doc
}

func mustDynamo(t *testing.T, s *Server, action, body string) map[string]any {
	t.Helper()
	rec, doc := dynamo(t, s, action, body)
	if rec.Code != 200 {
		t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body)
	}
	return doc
}

const createItems = `{"AttributeDefinitions": [{"AttributeName": "id", "AttributeType": "S"},` +
	` {"AttributeName": "ts", "AttributeType": "N"}], "TableName": "items",` +
	` "KeySchema": [{"AttributeName": "id", "KeyType": "HASH"},` +
	` {"AttributeName": "ts", "KeyType": "RANGE"}], "BillingMode": "PAY_PER_REQUEST",` +
	` "Tags": [{"Key": "env", "Value": "dev"}]}`

func withItemsTable(t *testing.T) *Server {
	t.Helper()
	s := newDynamoTestServer(t)
	mustDynamo(t, s, "CreateTable", createItems)
	return s
}

// The action comes from a HEADER here, not from the path and not from an
// Action parameter. A service routed on the credential scope alone would
// answer every DynamoDB call the same way.
func TestTheActionComesFromTheTargetHeader(t *testing.T) {
	s := newDynamoTestServer(t)
	doc := mustDynamo(t, s, "ListTables", "{}")
	if _, ok := doc["TableNames"]; !ok {
		t.Fatalf("ListTables = %v", doc)
	}
	rec, _ := dynamo(t, s, "NoSuchAction", "{}")
	if rec.Code != 400 {
		t.Errorf("an unknown action must be 400, not %d: a 5xx is retried five times", rec.Code)
	}
}

func TestCreateAndDescribeATable(t *testing.T) {
	s := newDynamoTestServer(t)
	doc := mustDynamo(t, s, "CreateTable", createItems)
	desc := doc["TableDescription"].(map[string]any)
	// Nothing has to be provisioned, so a table is usable at once -- the
	// provider waits for ACTIVE before it goes on.
	if desc["TableStatus"] != "ACTIVE" {
		t.Errorf("TableStatus = %v", desc["TableStatus"])
	}
	if want := "arn:aws:dynamodb:us-east-1:000000000000:table/items"; desc["TableArn"] != want {
		t.Errorf("TableArn = %v, want %v", desc["TableArn"], want)
	}

	doc = mustDynamo(t, s, "DescribeTable", `{"TableName": "items"}`)
	table := doc["Table"].(map[string]any)
	// Every field the provider reads back on a refresh. Any of these
	// missing is a perpetual diff rather than an error.
	for _, f := range []string{"TableName", "TableArn", "TableId", "KeySchema",
		"AttributeDefinitions", "BillingModeSummary", "ProvisionedThroughput",
		"ItemCount", "TableSizeBytes", "CreationDateTime"} {
		if table[f] == nil {
			t.Errorf("DescribeTable is missing %q", f)
		}
	}
	if bm := table["BillingModeSummary"].(map[string]any); bm["BillingMode"] != "PAY_PER_REQUEST" {
		t.Errorf("BillingMode = %v", bm["BillingMode"])
	}
	// PAY_PER_REQUEST still reports a throughput, of zeroes.
	if tp, ok := table["ProvisionedThroughput"].(map[string]any); !ok || tp["ReadCapacityUnits"] != 0.0 {
		t.Errorf("ProvisionedThroughput = %v", table["ProvisionedThroughput"])
	}

	// A second describe must agree with the first, byte for byte.
	again := mustDynamo(t, s, "DescribeTable", `{"TableName": "items"}`)
	first, _ := json.Marshal(table)
	second, _ := json.Marshal(again["Table"])
	if string(first) != string(second) {
		t.Errorf("a table changed between reads:\n%s\n%s", first, second)
	}
}

// The error envelope is a third shape again: the __type carries a FULLY
// QUALIFIED exception name, and botocore keeps only what follows the '#'.
// A bare code reads as the empty string there.
func TestTheErrorTypeIsQualified(t *testing.T) {
	s := newDynamoTestServer(t)
	rec, doc := dynamo(t, s, "DescribeTable", `{"TableName": "absent"}`)
	// Every DynamoDB error is a 400, including "not there".
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	want := "com.amazonaws.dynamodb.v20120810#ResourceNotFoundException"
	if doc["__type"] != want {
		t.Errorf("__type = %v, want %v", doc["__type"], want)
	}
	if got := rec.Header().Get("x-amzn-ErrorType"); got != want {
		t.Errorf("x-amzn-ErrorType = %q", got)
	}
	if doc["message"] == "" {
		t.Error("an error must carry a message")
	}
}

func TestTagsAndTheCallsReadBackAfterACreate(t *testing.T) {
	s := withItemsTable(t)
	arn := `{"ResourceArn": "arn:aws:dynamodb:us-east-1:000000000000:table/items"}`

	doc := mustDynamo(t, s, "ListTagsOfResource", arn)
	tags := doc["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "env" {
		t.Errorf("Tags = %v", tags)
	}

	// These two are read straight after a create and must answer with a
	// document rather than an error, like S3's unset sub-resources.
	doc = mustDynamo(t, s, "DescribeContinuousBackups", `{"TableName": "items"}`)
	pitr := doc["ContinuousBackupsDescription"].(map[string]any)["PointInTimeRecoveryDescription"]
	if pitr.(map[string]any)["PointInTimeRecoveryStatus"] != "DISABLED" {
		t.Errorf("point-in-time recovery = %v", pitr)
	}
	doc = mustDynamo(t, s, "DescribeTimeToLive", `{"TableName": "items"}`)
	if doc["TimeToLiveDescription"].(map[string]any)["TimeToLiveStatus"] != "DISABLED" {
		t.Errorf("TimeToLiveDescription = %v", doc["TimeToLiveDescription"])
	}

	mustDynamo(t, s, "UpdateTimeToLive",
		`{"TableName": "items", "TimeToLiveSpecification": {"Enabled": true, "AttributeName": "expires"}}`)
	doc = mustDynamo(t, s, "DescribeTimeToLive", `{"TableName": "items"}`)
	ttl := doc["TimeToLiveDescription"].(map[string]any)
	if ttl["TimeToLiveStatus"] != "ENABLED" || ttl["AttributeName"] != "expires" {
		t.Errorf("TimeToLiveDescription = %v", ttl)
	}
}

// --- items ----------------------------------------------------------------

func TestAnItemSurvivesARoundTripUnchanged(t *testing.T) {
	s := withItemsTable(t)
	mustDynamo(t, s, "PutItem",
		`{"TableName": "items", "Item": {"id": {"S": "1"}, "ts": {"N": "5"}, "note": {"S": "hi"}}}`)

	doc := mustDynamo(t, s, "GetItem",
		`{"TableName": "items", "Key": {"id": {"S": "1"}, "ts": {"N": "5"}}}`)
	item, ok := doc["Item"].(map[string]any)
	if !ok {
		t.Fatalf("GetItem = %v", doc)
	}
	// The wire shape is kept verbatim: a number is a decimal STRING, and
	// decoding it into a float64 and printing it back is how "1e+06"
	// appears where "1000000" was written.
	if n := item["ts"].(map[string]any)["N"]; n != "5" {
		t.Errorf("ts = %v (%T), want the string \"5\"", n, n)
	}
	if item["note"].(map[string]any)["S"] != "hi" {
		t.Errorf("note = %v", item["note"])
	}
}

// A number keeps every digit it was written with, which a float64 cannot
// hold: DynamoDB allows 38 significant digits.
func TestABigNumberIsNotRoundedByTheRoundTrip(t *testing.T) {
	s := withItemsTable(t)
	big := "123456789012345678901234567890"
	mustDynamo(t, s, "PutItem",
		`{"TableName": "items", "Item": {"id": {"S": "big"}, "ts": {"N": "1"},`+
			` "count": {"N": "`+big+`"}}}`)
	doc := mustDynamo(t, s, "GetItem",
		`{"TableName": "items", "Key": {"id": {"S": "big"}, "ts": {"N": "1"}}}`)
	if got := doc["Item"].(map[string]any)["count"].(map[string]any)["N"]; got != big {
		t.Errorf("count = %v, want %s", got, big)
	}
}

// A key is the TYPE and the value, not the value alone: {"N":"5"} and
// {"S":"5"} are different keys.
func TestAKeysTypeIsPartOfIt(t *testing.T) {
	s := newDynamoTestServer(t)
	mustDynamo(t, s, "CreateTable", `{"TableName": "k",
		"AttributeDefinitions": [{"AttributeName": "id", "AttributeType": "S"}],
		"KeySchema": [{"AttributeName": "id", "KeyType": "HASH"}],
		"BillingMode": "PAY_PER_REQUEST"}`)
	mustDynamo(t, s, "PutItem", `{"TableName": "k", "Item": {"id": {"S": "5"}, "which": {"S": "string"}}}`)
	doc := mustDynamo(t, s, "GetItem", `{"TableName": "k", "Key": {"id": {"N": "5"}}}`)
	if _, ok := doc["Item"]; ok {
		t.Errorf("a numeric key found a string-keyed item: %v", doc)
	}
}

// A missing item is an EMPTY response, not an error -- that is how a client
// asks "is this there?" without catching an exception.
func TestGettingAnAbsentItemIsNotAnError(t *testing.T) {
	s := withItemsTable(t)
	rec, doc := dynamo(t, s, "GetItem",
		`{"TableName": "items", "Key": {"id": {"S": "nope"}, "ts": {"N": "1"}}}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if _, ok := doc["Item"]; ok {
		t.Errorf("expected no Item, got %v", doc)
	}
}

// Deleting an item that is not there succeeds, which is what makes a
// destroy idempotent -- unlike a bucket, which refuses while it has
// contents.
func TestDeletingAnAbsentItemSucceeds(t *testing.T) {
	s := withItemsTable(t)
	rec, _ := dynamo(t, s, "DeleteItem",
		`{"TableName": "items", "Key": {"id": {"S": "nope"}, "ts": {"N": "1"}}}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestAMissingKeyAttributeIsAValidationError(t *testing.T) {
	s := withItemsTable(t)
	rec, doc := dynamo(t, s, "PutItem", `{"TableName": "items", "Item": {"id": {"S": "1"}}}`)
	if rec.Code != 400 || !strings.Contains(str(doc["__type"]), "ValidationException") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(str(doc["message"]), "ts") {
		t.Errorf("the message must name the key that was missing: %v", doc["message"])
	}
}

func TestUpdateItemSetsAddsAndRemoves(t *testing.T) {
	s := withItemsTable(t)
	key := `"Key": {"id": {"S": "1"}, "ts": {"N": "5"}}`

	// An update to an item that is not there creates it, carrying the key.
	doc := mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "SET note = :n", "ReturnValues": "ALL_NEW",
		"ExpressionAttributeValues": {":n": {"S": "hi"}}}`)
	attrs := doc["Attributes"].(map[string]any)
	if attrs["note"].(map[string]any)["S"] != "hi" || attrs["id"] == nil {
		t.Fatalf("ALL_NEW = %v", attrs)
	}

	// ADD on an attribute nobody initialised is how a counter is kept.
	doc = mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "ADD hits :one", "ReturnValues": "ALL_NEW",
		"ExpressionAttributeValues": {":one": {"N": "1"}}}`)
	if doc["Attributes"].(map[string]any)["hits"].(map[string]any)["N"] != "1" {
		t.Fatalf("after one ADD: %v", doc["Attributes"])
	}
	doc = mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "ADD hits :one", "ReturnValues": "UPDATED_NEW",
		"ExpressionAttributeValues": {":one": {"N": "1"}}}`)
	attrs = doc["Attributes"].(map[string]any)
	if attrs["hits"].(map[string]any)["N"] != "2" {
		t.Errorf("after two ADDs: %v", attrs)
	}
	// UPDATED_NEW is only what CHANGED, so the untouched note is not in it.
	if _, ok := attrs["note"]; ok {
		t.Errorf("UPDATED_NEW returned an attribute that did not change: %v", attrs)
	}

	// SET with arithmetic, and a #name alias.
	doc = mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "SET #h = #h + :ten REMOVE note", "ReturnValues": "ALL_NEW",
		"ExpressionAttributeNames": {"#h": "hits"},
		"ExpressionAttributeValues": {":ten": {"N": "10"}}}`)
	attrs = doc["Attributes"].(map[string]any)
	if attrs["hits"].(map[string]any)["N"] != "12" {
		t.Errorf("hits = %v, want 12", attrs["hits"])
	}
	if _, ok := attrs["note"]; ok {
		t.Errorf("REMOVE left the attribute behind: %v", attrs)
	}
}

func TestIfNotExistsLeavesAnExistingValueAlone(t *testing.T) {
	s := withItemsTable(t)
	key := `"Key": {"id": {"S": "1"}, "ts": {"N": "5"}}`
	mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "SET created = if_not_exists(created, :now)",
		"ExpressionAttributeValues": {":now": {"N": "1000"}}}`)
	doc := mustDynamo(t, s, "UpdateItem", `{"TableName": "items", `+key+`,
		"UpdateExpression": "SET created = if_not_exists(created, :now)", "ReturnValues": "ALL_NEW",
		"ExpressionAttributeValues": {":now": {"N": "2000"}}}`)
	if got := doc["Attributes"].(map[string]any)["created"].(map[string]any)["N"]; got != "1000" {
		t.Errorf("created = %v, want the first value 1000", got)
	}
}

// A ConditionExpression is what makes "write only if absent" work, and its
// failure is its own exception rather than a validation error.
func TestAFailedConditionHasItsOwnException(t *testing.T) {
	s := withItemsTable(t)
	item := `"Item": {"id": {"S": "1"}, "ts": {"N": "5"}}`
	mustDynamo(t, s, "PutItem", `{"TableName": "items", `+item+`}`)
	rec, doc := dynamo(t, s, "PutItem", `{"TableName": "items", `+item+`,
		"ConditionExpression": "attribute_not_exists(id)"}`)
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.HasSuffix(str(doc["__type"]), "#ConditionalCheckFailedException") {
		t.Errorf("__type = %v", doc["__type"])
	}
}

func TestQueryByKeyWithARangeConditionAndAFilter(t *testing.T) {
	s := withItemsTable(t)
	for _, body := range []string{
		`{"id": {"S": "a"}, "ts": {"N": "1"}, "note": {"S": "first"}}`,
		`{"id": {"S": "a"}, "ts": {"N": "2"}, "note": {"S": "second"}}`,
		`{"id": {"S": "a"}, "ts": {"N": "10"}, "note": {"S": "tenth"}}`,
		`{"id": {"S": "b"}, "ts": {"N": "1"}, "note": {"S": "other"}}`,
	} {
		mustDynamo(t, s, "PutItem", `{"TableName": "items", "Item": `+body+`}`)
	}

	doc := mustDynamo(t, s, "Query", `{"TableName": "items",
		"KeyConditionExpression": "id = :v",
		"ExpressionAttributeValues": {":v": {"S": "a"}}}`)
	items := doc["Items"].([]any)
	if len(items) != 3 {
		t.Fatalf("Count = %v, Items = %v", doc["Count"], items)
	}
	// Numbers sort numerically, so 10 comes after 2 -- a string sort would
	// put it first and nothing would say why.
	order := []string{}
	for _, it := range items {
		order = append(order, str(it.(map[string]any)["ts"].(map[string]any)["N"]))
	}
	if strings.Join(order, ",") != "1,2,10" {
		t.Errorf("sort-key order = %v", order)
	}

	// Descending.
	doc = mustDynamo(t, s, "Query", `{"TableName": "items",
		"KeyConditionExpression": "id = :v", "ScanIndexForward": false,
		"ExpressionAttributeValues": {":v": {"S": "a"}}}`)
	first := doc["Items"].([]any)[0].(map[string]any)
	if str(first["ts"].(map[string]any)["N"]) != "10" {
		t.Errorf("descending query started at %v", first["ts"])
	}

	// A range condition, which is why a key condition cannot be split on
	// " AND ": BETWEEN contains one.
	doc = mustDynamo(t, s, "Query", `{"TableName": "items",
		"KeyConditionExpression": "id = :v AND ts BETWEEN :lo AND :hi",
		"ExpressionAttributeValues": {":v": {"S": "a"}, ":lo": {"N": "2"}, ":hi": {"N": "9"}}}`)
	if doc["Count"] != 1.0 {
		t.Errorf("BETWEEN matched %v items", doc["Count"])
	}

	doc = mustDynamo(t, s, "Query", `{"TableName": "items",
		"KeyConditionExpression": "id = :v AND ts > :t",
		"FilterExpression": "begins_with(note, :p)",
		"ExpressionAttributeValues": {":v": {"S": "a"}, ":t": {"N": "1"}, ":p": {"S": "t"}}}`)
	if doc["Count"] != 1.0 || doc["ScannedCount"] != 2.0 {
		t.Errorf("Count = %v, ScannedCount = %v; a filter narrows what the key condition found",
			doc["Count"], doc["ScannedCount"])
	}
}

func TestScanFiltersAndPagesByKey(t *testing.T) {
	s := withItemsTable(t)
	for i, id := range []string{"a", "b", "c"} {
		_ = i
		mustDynamo(t, s, "PutItem",
			`{"TableName": "items", "Item": {"id": {"S": "`+id+`"}, "ts": {"N": "1"},
			  "kind": {"S": "note"}}}`)
	}
	doc := mustDynamo(t, s, "Scan", `{"TableName": "items"}`)
	if doc["Count"] != 3.0 {
		t.Fatalf("Scan found %v", doc["Count"])
	}
	doc = mustDynamo(t, s, "Scan", `{"TableName": "items",
		"FilterExpression": "kind = :k AND attribute_not_exists(gone)",
		"ExpressionAttributeValues": {":k": {"S": "note"}}}`)
	if doc["Count"] != 3.0 {
		t.Errorf("filtered scan found %v", doc["Count"])
	}

	// A page hands back the KEY of the last item it returned, which the
	// client passes as ExclusiveStartKey -- not an offset, so an insert
	// between pages cannot shift what follows.
	doc = mustDynamo(t, s, "Scan", `{"TableName": "items", "Limit": 2}`)
	last, ok := doc["LastEvaluatedKey"].(map[string]any)
	if !ok || len(last) != 2 {
		t.Fatalf("LastEvaluatedKey = %v", doc["LastEvaluatedKey"])
	}
	start, _ := json.Marshal(last)
	doc = mustDynamo(t, s, "Scan",
		`{"TableName": "items", "ExclusiveStartKey": `+string(start)+`}`)
	if doc["Count"] != 1.0 {
		t.Errorf("the second page held %v items", doc["Count"])
	}

	doc = mustDynamo(t, s, "Scan", `{"TableName": "items", "Select": "COUNT"}`)
	if _, ok := doc["Items"]; ok {
		t.Error("Select=COUNT must return no items at all")
	}
	if doc["Count"] != 3.0 {
		t.Errorf("Count = %v", doc["Count"])
	}
}

// An expression Nimbus cannot parse must be REFUSED. The tempting failure
// is to treat it as matching everything, which turns an unimplemented
// filter into a silently wrong answer discovered nowhere near here.
func TestAnUnsupportedExpressionIsRefusedNotIgnored(t *testing.T) {
	s := withItemsTable(t)
	mustDynamo(t, s, "PutItem",
		`{"TableName": "items", "Item": {"id": {"S": "1"}, "ts": {"N": "5"}}}`)
	for _, expr := range []string{
		"(kind = :k OR kind = :j)",
		"size(note) > :n",
		"profile.name = :k",
	} {
		rec, doc := dynamo(t, s, "Scan", `{"TableName": "items",
			"FilterExpression": `+quoteJSON(expr)+`,
			"ExpressionAttributeValues": {":k": {"S": "x"}, ":j": {"S": "y"}, ":n": {"N": "1"}}}`)
		if rec.Code == 200 {
			t.Errorf("%q was accepted and returned %v; it must be refused", expr, doc["Count"])
			continue
		}
		if !strings.HasSuffix(str(doc["__type"]), "#ValidationException") {
			t.Errorf("%q: __type = %v", expr, doc["__type"])
		}
	}
}

func TestBatchWriteAndBatchGet(t *testing.T) {
	s := withItemsTable(t)
	mustDynamo(t, s, "BatchWriteItem", `{"RequestItems": {"items": [
		{"PutRequest": {"Item": {"id": {"S": "1"}, "ts": {"N": "1"}}}},
		{"PutRequest": {"Item": {"id": {"S": "2"}, "ts": {"N": "2"}}}}]}}`)
	doc := mustDynamo(t, s, "Scan", `{"TableName": "items"}`)
	if doc["Count"] != 2.0 {
		t.Fatalf("after the batch: %v", doc["Count"])
	}

	// UnprocessedItems must be PRESENT even when empty: a client reads it
	// before checking whether there is anything in it.
	doc = mustDynamo(t, s, "BatchWriteItem", `{"RequestItems": {"items": [
		{"DeleteRequest": {"Key": {"id": {"S": "1"}, "ts": {"N": "1"}}}}]}}`)
	if _, ok := doc["UnprocessedItems"]; !ok {
		t.Errorf("BatchWriteItem = %v", doc)
	}

	doc = mustDynamo(t, s, "BatchGetItem", `{"RequestItems": {"items": {"Keys": [
		{"id": {"S": "2"}, "ts": {"N": "2"}},
		{"id": {"S": "gone"}, "ts": {"N": "9"}}]}}}`)
	got := doc["Responses"].(map[string]any)["items"].([]any)
	if len(got) != 1 {
		t.Errorf("BatchGetItem returned %v; a key that is not there is simply absent", got)
	}
}

func TestATableAndItsItemsAreDeletedTogether(t *testing.T) {
	s := withItemsTable(t)
	mustDynamo(t, s, "PutItem",
		`{"TableName": "items", "Item": {"id": {"S": "1"}, "ts": {"N": "5"}}}`)
	doc := mustDynamo(t, s, "DescribeTable", `{"TableName": "items"}`)
	if doc["Table"].(map[string]any)["ItemCount"] != 1.0 {
		t.Errorf("ItemCount = %v", doc["Table"].(map[string]any)["ItemCount"])
	}

	mustDynamo(t, s, "DeleteTable", `{"TableName": "items"}`)
	rec, _ := dynamo(t, s, "DescribeTable", `{"TableName": "items"}`)
	if rec.Code != 400 {
		t.Fatalf("the table is still there: %d", rec.Code)
	}
	// Recreating the name must not find the old rows.
	mustDynamo(t, s, "CreateTable", createItems)
	doc = mustDynamo(t, s, "Scan", `{"TableName": "items"}`)
	if doc["Count"] != 0.0 {
		t.Errorf("a recreated table held %v old items", doc["Count"])
	}
}

func quoteJSON(s string) string {
	body, _ := json.Marshal(s)
	return string(body)
}
