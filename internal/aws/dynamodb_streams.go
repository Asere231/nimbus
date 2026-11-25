package aws

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// DynamoDB Streams: the change log a table keeps of its own writes.
//
// A SEPARATE API sharing one endpoint and one credential scope. Its
// target header says `DynamoDBStreams_20120810.X` where the table API's
// says `DynamoDB_20120810.X`, and `serviceOf` routes both to "dynamodb"
// because that is the SigV4 service name for each -- so the prefix of the
// target is what tells them apart. Routing on the credential scope alone
// answers every Streams call as if it were a table call.
//
// # What this emulates, and what it simplifies
//
// Every write to a stream-enabled table appends a record carrying the
// item's keys and, depending on StreamViewType, the new image, the old
// one, or both. `GetShardIterator` hands out a cursor and `GetRecords`
// reads forward from it.
//
// ONE SHARD per table, which is a stated simplification with a useful
// property: real DynamoDB shards by partition key and only guarantees
// order WITHIN a shard, so a single shard gives a total order over the
// table's writes -- MORE than real DynamoDB promises, which is the safe
// direction for a client to be surprised in. Real streams also split
// shards as throughput changes and expire records after 24 hours; nothing
// here splits or expires, and the buffer is bounded instead.
//
// Records are IN MEMORY and do not survive a restart, for the same reason
// CloudWatch log events do not: nothing persists them, and an empty stream
// reads as "nothing has been written yet", which after a restart is true
// of the process doing the reading. The table's items DO persist, so a
// restart leaves a table whose contents have no corresponding records --
// which is exactly what a real stream's 24-hour expiry produces too.

// streamRecordLimit bounds a table's record buffer. Real streams keep 24
// hours; a count is the local equivalent and keeps a loop that writes
// forever from filling memory.
const streamRecordLimit = 10000

// streamShardID is the one shard every table's stream has. A literal
// rather than a derived value, because a client only ever discovers it
// through DescribeStream.
const streamShardID = "shardId-00000000000000000000-nimbus"

// streamRecord is one change, in the shape GetRecords returns.
type streamRecord struct {
	EventID      string         `json:"eventID"`
	EventName    string         `json:"eventName"` // INSERT, MODIFY, REMOVE
	EventVersion string         `json:"eventVersion"`
	EventSource  string         `json:"eventSource"`
	AWSRegion    string         `json:"awsRegion"`
	Dynamodb     map[string]any `json:"dynamodb"`

	// sequence is the ordinal this record was appended at, kept outside
	// the wire shape so an iterator can be resolved without parsing the
	// decimal string back.
	sequence int64 `json:"-"`
}

// streamStore holds every stream-enabled table's records.
type streamStore struct {
	mu sync.Mutex
	// table -> records, oldest first
	records map[string][]streamRecord
	// seq is the sequence number last issued, across every table: real
	// sequence numbers rise within a shard, and one counter makes them
	// unique across the process as well, which costs nothing and means a
	// record's number alone identifies it.
	seq int64
	// trimmed counts the records dropped off the front of each table, so
	// a sequence number stays meaningful after the buffer wraps.
	trimmed map[string]int64
}

func newStreamStore() *streamStore {
	return &streamStore{records: map[string][]streamRecord{}, trimmed: map[string]int64{}}
}

// append records one change. Called with the new and old images, either of
// which may be nil: a create has no old image and a delete has no new one.
func (s *streamStore) append(table, region, viewType string, keys, newImage, oldImage map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	seq := s.seq

	body := map[string]any{
		"ApproximateCreationDateTime": float64(time.Now().Unix()),
		"Keys":                        keys,
		"SequenceNumber":              strconv.FormatInt(seq, 10),
		"SizeBytes":                   float64(approxSize(newImage) + approxSize(oldImage)),
		"StreamViewType":              viewType,
	}
	// StreamViewType decides what travels, and it is the whole reason the
	// setting exists: a KEYS_ONLY stream carrying images would hand a
	// consumer data its configuration said it would not get.
	switch viewType {
	case "NEW_IMAGE":
		putIfPresent(body, "NewImage", newImage)
	case "OLD_IMAGE":
		putIfPresent(body, "OldImage", oldImage)
	case "NEW_AND_OLD_IMAGES":
		putIfPresent(body, "NewImage", newImage)
		putIfPresent(body, "OldImage", oldImage)
	}

	s.records[table] = append(s.records[table], streamRecord{
		EventID:      stableID(table+"/"+strconv.FormatInt(seq, 10), 32),
		EventName:    streamEventName(newImage, oldImage),
		EventVersion: "1.1",
		EventSource:  "aws:dynamodb",
		AWSRegion:    region,
		Dynamodb:     body,
		sequence:     seq,
	})
	if over := len(s.records[table]) - streamRecordLimit; over > 0 {
		s.records[table] = s.records[table][over:]
		s.trimmed[table] += int64(over)
	}
}

// streamEventName is which of the three kinds of change this was. Derived
// from which images are present rather than passed in, so a caller cannot
// label a delete an insert.
func streamEventName(newImage, oldImage map[string]any) string {
	switch {
	case newImage == nil:
		return "REMOVE"
	case oldImage == nil:
		return "INSERT"
	default:
		return "MODIFY"
	}
}

func putIfPresent(body map[string]any, field string, image map[string]any) {
	if image != nil {
		body[field] = image
	}
}

// approxSize is a rough byte count, which is all SizeBytes ever is.
func approxSize(image map[string]any) int {
	if image == nil {
		return 0
	}
	if raw, err := json.Marshal(image); err == nil {
		return len(raw)
	}
	return 0
}

// after returns the records with a sequence number above `from`, up to
// limit, and the sequence to read from next.
func (s *streamStore) after(table string, from int64, limit int) ([]streamRecord, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []streamRecord{}
	next := from
	for _, rec := range s.records[table] {
		if rec.sequence <= from {
			continue
		}
		if len(out) >= limit {
			break
		}
		out = append(out, rec)
		next = rec.sequence
	}
	return out, next
}

// bounds is the oldest and newest sequence numbers a table's stream holds.
func (s *streamStore) bounds(table string) (first, last int64, any bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := s.records[table]
	if len(recs) == 0 {
		return 0, 0, false
	}
	return recs[0].sequence, recs[len(recs)-1].sequence, true
}

func (s *streamStore) dropTable(table string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, table)
	delete(s.trimmed, table)
}

// --- the table's side -----------------------------------------------------

// streamLabel is a stream's timestamp-shaped id. DERIVED from the table,
// not generated: the provider reads `stream_label` on every refresh, so a
// fresh value would be a permanent diff -- the Content-MD5 rule again.
//
// Real labels look like "2026-11-25T04:00:00.000", a creation time. This
// one is stable and differs from what real DynamoDB would produce, in the
// same way `uniqueString()` and a key pair's fingerprint do.
func streamLabel(t *dynamoTable) string {
	return time.Unix(int64(t.CreationDateTime), 0).UTC().Format("2006-01-02T15:04:05.000")
}

// streamARN is the table's ARN with the label appended, which is the shape
// `aws_lambda_event_source_mapping` is given.
func (s *Server) streamARN(t *dynamoTable) string {
	return s.dynamoTableARN(t.TableName) + "/stream/" + streamLabel(t)
}

// streamViewTypeOf is what the table's StreamSpecification asks for, or ""
// when the table has no stream.
func streamViewTypeOf(t *dynamoTable) string {
	if t.StreamSpecification == nil || !boolOf(t.StreamSpecification["StreamEnabled"]) {
		return ""
	}
	// KEYS_ONLY is DynamoDB's own default when the type is left out.
	return orDefault(str(t.StreamSpecification["StreamViewType"]), "KEYS_ONLY")
}

// recordChange appends a stream record if the table has a stream, and does
// nothing if it has not.
//
// One function every write path calls, rather than a branch in each: a
// write that forgot to record would be a change a consumer never hears
// about, which is silence rather than an error -- the hardest kind of gap
// to notice. The keys come from the table's own KeySchema, never from
// guessing which attributes look key-shaped.
func (s *Server) recordChange(t *dynamoTable, newImage, oldImage map[string]any) {
	viewType := streamViewTypeOf(t)
	if viewType == "" {
		return
	}
	image := newImage
	if image == nil {
		image = oldImage
	}
	s.streams.append(t.TableName, s.cfg.Region, viewType,
		keyAttributesOf(t, image), newImage, oldImage)
}

// keyAttributesOf is just the key attributes of an item, which is what
// every record carries whatever the view type is.
func keyAttributesOf(t *dynamoTable, item map[string]any) map[string]any {
	keys := map[string]any{}
	for _, part := range t.KeySchema {
		name := str(part["AttributeName"])
		if v, ok := item[name]; ok {
			keys[name] = v
		}
	}
	return keys
}

// --- the Streams API ------------------------------------------------------

// streamsTargetPrefix is what tells a Streams call from a table call. Both
// post to "/" with the credential scope saying "dynamodb".
const streamsTargetPrefix = "DynamoDBStreams_"

// serveDynamoStreams dispatches the four Streams operations.
func (s *Server) serveDynamoStreams(w http.ResponseWriter, action string, req map[string]any) *apiError {
	switch action {
	case "ListStreams":
		return s.listStreams(w, req)
	case "DescribeStream":
		return s.describeStream(w, req)
	case "GetShardIterator":
		return s.getShardIterator(w, req)
	case "GetRecords":
		return s.getRecords(w, req)
	}
	return invalidAction("DynamoDB Streams", action)
}

// streamTables is every table with a stream enabled.
func (s *Server) streamTables() []*dynamoTable {
	var out []*dynamoTable
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == typeDynamoTable }) {
		var t dynamoTable
		if json.Unmarshal(res.Body, &t) != nil {
			continue
		}
		if streamViewTypeOf(&t) != "" {
			out = append(out, &t)
		}
	}
	return out
}

func (s *Server) listStreams(w http.ResponseWriter, req map[string]any) *apiError {
	wanted := str(req["TableName"])
	streams := []map[string]any{}
	for _, t := range s.streamTables() {
		if wanted != "" && !strings.EqualFold(wanted, t.TableName) {
			continue
		}
		streams = append(streams, map[string]any{
			"StreamArn":   s.streamARN(t),
			"StreamLabel": streamLabel(t),
			"TableName":   t.TableName,
		})
	}
	writeJSON(w, 200, map[string]any{"Streams": streams})
	return nil
}

// loadStream finds the table a StreamArn belongs to.
//
// Matched by rebuilding each stream-enabled table's ARN rather than by
// parsing the one given: the label is derived, so the comparison is exact,
// and a client that sends a stale ARN for a table whose stream was
// disabled and re-enabled gets a clean ResourceNotFoundException instead
// of being served the wrong stream.
func (s *Server) loadStream(req map[string]any) (*dynamoTable, *apiError) {
	given := str(req["StreamArn"])
	if given == "" {
		return nil, dynamoBadRequest("StreamArn is required")
	}
	for _, t := range s.streamTables() {
		if s.streamARN(t) == given {
			return t, nil
		}
	}
	return nil, &apiError{Status: 400, Code: "ResourceNotFoundException",
		Message: "Requested resource not found: Stream: " + given + " not found"}
}

func (s *Server) describeStream(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadStream(req)
	if aerr != nil {
		return aerr
	}
	first, last, has := s.streams.bounds(t.TableName)
	rng := map[string]any{}
	if has {
		rng["StartingSequenceNumber"] = strconv.FormatInt(first, 10)
		// No EndingSequenceNumber: a shard that reports one is CLOSED, and
		// a consumer then stops reading it. This shard never closes,
		// because the table goes on accepting writes.
		_ = last
	}
	writeJSON(w, 200, map[string]any{"StreamDescription": map[string]any{
		"StreamArn":               s.streamARN(t),
		"StreamLabel":             streamLabel(t),
		"StreamStatus":            "ENABLED",
		"StreamViewType":          streamViewTypeOf(t),
		"CreationRequestDateTime": t.CreationDateTime,
		"TableName":               t.TableName,
		"KeySchema":               t.KeySchema,
		"Shards": []any{map[string]any{
			"ShardId":             streamShardID,
			"SequenceNumberRange": rng,
		}},
	}})
	return nil
}

// shardIteratorFor encodes a cursor. It is Nimbus's own opaque string --
// real ones are a long base64 blob with no published shape -- and it
// carries the table and the sequence to read AFTER, so GetRecords needs no
// server-side iterator table to expire or lose across a restart.
func shardIteratorFor(table string, after int64) string {
	return "nimbus-iter:" + table + ":" + strconv.FormatInt(after, 10)
}

func parseShardIterator(iter string) (table string, after int64, ok bool) {
	const prefix = "nimbus-iter:"
	if !strings.HasPrefix(iter, prefix) {
		return "", 0, false
	}
	rest := strings.TrimPrefix(iter, prefix)
	i := strings.LastIndex(rest, ":")
	if i <= 0 {
		return "", 0, false
	}
	n, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil {
		return "", 0, false
	}
	return rest[:i], n, true
}

func (s *Server) getShardIterator(w http.ResponseWriter, req map[string]any) *apiError {
	t, aerr := s.loadStream(req)
	if aerr != nil {
		return aerr
	}
	if id := str(req["ShardId"]); id != streamShardID {
		return &apiError{Status: 400, Code: "ResourceNotFoundException",
			Message: fmt.Sprintf("Shard %q does not belong to this stream; it has one shard, %q",
				id, streamShardID)}
	}
	first, last, has := s.streams.bounds(t.TableName)

	var after int64
	switch typ := str(req["ShardIteratorType"]); typ {
	case "TRIM_HORIZON":
		// Everything still in the buffer, so read from just before the
		// oldest record rather than from zero -- which would be the same
		// thing today and would stop being once the buffer has wrapped.
		if has {
			after = first - 1
		}
	case "LATEST":
		if has {
			after = last
		}
	case "AT_SEQUENCE_NUMBER", "AFTER_SEQUENCE_NUMBER":
		raw := str(req["SequenceNumber"])
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return dynamoBadRequest("SequenceNumber %q is not a sequence number", raw)
		}
		after = n
		if typ == "AT_SEQUENCE_NUMBER" {
			// "At" includes the record itself, so read from one before it.
			after = n - 1
		}
	case "":
		return dynamoBadRequest("ShardIteratorType is required")
	default:
		// Refused rather than defaulted: a cursor that silently started
		// somewhere else would make a consumer miss records or replay
		// them, and neither looks like an error.
		return dynamoBadRequest("ShardIteratorType %q is not one Nimbus implements "+
			"(TRIM_HORIZON, LATEST, AT_SEQUENCE_NUMBER, AFTER_SEQUENCE_NUMBER)", typ)
	}
	writeJSON(w, 200, map[string]any{
		"ShardIterator": shardIteratorFor(t.TableName, after),
	})
	return nil
}

// getRecordsLimit is DynamoDB's own ceiling on one GetRecords call.
const getRecordsLimit = 1000

func (s *Server) getRecords(w http.ResponseWriter, req map[string]any) *apiError {
	iter := str(req["ShardIterator"])
	table, after, ok := parseShardIterator(iter)
	if !ok {
		return &apiError{Status: 400, Code: "ExpiredIteratorException",
			Message: "The shard iterator is not one Nimbus issued: " + iter}
	}
	limit := getRecordsLimit
	if n, err := strconv.Atoi(str(req["Limit"])); err == nil && n > 0 && n < limit {
		limit = n
	} else if n, ok := req["Limit"].(float64); ok && int(n) > 0 && int(n) < limit {
		limit = int(n)
	}

	records, next := s.streams.after(table, after, limit)
	out := make([]any, 0, len(records))
	for _, rec := range records {
		out = append(out, rec)
	}
	writeJSON(w, 200, map[string]any{
		"Records": out,
		// ALWAYS a next iterator, even when nothing came back: an absent
		// one means the shard is CLOSED and a consumer stops polling. A
		// stream on a live table is never closed, so "no records yet" and
		// "no more records ever" must not look the same -- the same
		// distinction CloudWatch Logs needs between its two page tokens.
		"NextShardIterator": shardIteratorFor(table, next),
	})
	return nil
}
