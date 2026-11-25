package aws

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Event source mappings: the thing that invokes a function from a stream.
//
// This closes a pairing Nimbus made for itself. DynamoDB got streams and
// Lambda got versions, and the roadmap then said, accurately, "nothing
// invokes a Lambda from a stream: you read it yourself" -- which leaves
// out the whole point of a stream. A mapping is the poller in between.
//
// # It is a POLLER, and that is the part worth understanding
//
// Lambda does not "receive" stream events. A managed poller owned by the
// Lambda service reads the stream on a schedule, batches what it finds and
// invokes the function with those records. Everything a mapping configures
// -- batch size, batching window, starting position -- is configuration of
// that poller, which is why a mapping is a resource of its own rather than
// a field on the function. Nimbus's poller is a goroutine per mapping and
// behaves the same way.
//
// # What is simplified, and stated
//
// A real poller runs per SHARD and retries a failed batch until the
// records expire, which can block a shard for hours. Nimbus has one shard
// per table, retries a failed batch a bounded number of times and then
// moves past it, recording the failure on the mapping's
// LastProcessingResult -- the same field real Lambda reports it in. Nothing
// here implements bisect-on-error, destination-on-failure, filter criteria
// or parallelisation factor.

const typeEventSourceMapping = "AWS::Lambda::EventSourceMapping"

// esmPollInterval is how often a mapping looks for new records. Real
// Lambda polls about four times a second for DynamoDB; this is slower
// because nothing here is under load and a tight loop is noise in the
// activity feed.
const esmPollInterval = 500 * time.Millisecond

// esmMaxAttempts bounds how often one batch is retried before the poller
// moves past it. Real Lambda retries until the records expire; doing that
// locally would wedge the stream for ever on a handler that always throws,
// which teaches nothing and looks like a hang.
const esmMaxAttempts = 3

// eventSourceMapping is the stored shape. The field names are the wire's,
// because the provider reads most of them back on every refresh.
type eventSourceMapping struct {
	UUID                  string  `json:"UUID"`
	EventSourceArn        string  `json:"EventSourceArn"`
	FunctionArn           string  `json:"FunctionArn"`
	State                 string  `json:"State"`
	StateTransitionReason string  `json:"StateTransitionReason"`
	LastModified          float64 `json:"LastModified"`
	LastProcessingResult  string  `json:"LastProcessingResult"`
	BatchSize             int     `json:"BatchSize"`
	StartingPosition      string  `json:"StartingPosition,omitempty"`

	MaximumBatchingWindowInSeconds int `json:"MaximumBatchingWindowInSeconds"`

	// Enabled is not a wire field on the way out -- State carries it --
	// but it is what the client sends and what the poller reads.
	NimbusEnabled bool `json:"nimbusEnabled"`
	// NimbusCursor is the sequence number the poller has read up to, so a
	// restart does not replay everything. Stored rather than held in the
	// goroutine, for the reason the log-group index needed reattaching:
	// state discovered in one place and needed in another is lost across a
	// restart.
	NimbusCursor int64 `json:"nimbusCursor"`
}

func (s *Server) esmARN(uuid string) string {
	return arn("lambda", s.cfg.Region, s.cfg.AccountID, "event-source-mapping:"+uuid)
}

// --- the API --------------------------------------------------------------

func (s *Server) createEventSourceMapping(w http.ResponseWriter, r *http.Request) *apiError {
	var req struct {
		EventSourceArn                 string `json:"EventSourceArn"`
		FunctionName                   string `json:"FunctionName"`
		Enabled                        *bool  `json:"Enabled"`
		BatchSize                      int    `json:"BatchSize"`
		StartingPosition               string `json:"StartingPosition"`
		MaximumBatchingWindowInSeconds int    `json:"MaximumBatchingWindowInSeconds"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	if req.EventSourceArn == "" || req.FunctionName == "" {
		return &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "EventSourceArn and FunctionName are both required"}
	}
	fn, aerr := s.loadFunction(req.FunctionName)
	if aerr != nil {
		return aerr
	}
	// The event source has to EXIST, and be something Nimbus can poll.
	// Refusing here beats a mapping that reports Enabled and silently
	// never fires -- which is indistinguishable from a handler that is
	// never reached.
	table, aerr := s.streamTableFor(req.EventSourceArn)
	if aerr != nil {
		return aerr
	}

	m := eventSourceMapping{
		UUID:                 stableUUID(req.EventSourceArn + "/" + fn.FunctionArn),
		EventSourceArn:       req.EventSourceArn,
		FunctionArn:          fn.FunctionArn,
		BatchSize:            orZero(req.BatchSize, 100),
		StartingPosition:     orDefault(req.StartingPosition, "LATEST"),
		LastProcessingResult: "No records processed",
		NimbusEnabled:        req.Enabled == nil || *req.Enabled,

		MaximumBatchingWindowInSeconds: req.MaximumBatchingWindowInSeconds,
	}
	if s.store.Exists(s.esmARN(m.UUID)) {
		return &apiError{Status: 409, Code: "ResourceConflictException",
			Message: "An event source mapping between " + req.EventSourceArn +
				" and " + fn.FunctionArn + " already exists"}
	}
	// TRIM_HORIZON starts at the beginning of what the stream still holds;
	// LATEST skips it. Getting this backwards either replays every record
	// a table ever saw or silently drops the ones already there.
	if strings.EqualFold(m.StartingPosition, "TRIM_HORIZON") {
		m.NimbusCursor = 0
	} else if _, last, has := s.streams.bounds(table); has {
		m.NimbusCursor = last
	}
	m.State = esmState(m.NimbusEnabled)
	m.StateTransitionReason = "User action"
	m.LastModified = esmNow()

	if aerr := s.saveESM(&m); aerr != nil {
		return aerr
	}
	s.startPoller(m.UUID)
	writeJSON(w, 202, m.wire())
	return nil
}

// esmState is what the provider waits on. "Enabled"/"Disabled" rather
// than a bool, because that is the field the API reports.
func esmState(enabled bool) string {
	if enabled {
		return "Enabled"
	}
	return "Disabled"
}

func esmNow() float64 { return float64(time.Now().UnixMilli()) / 1000 }

// wire is the mapping on its way out, without Nimbus's own bookkeeping.
func (m eventSourceMapping) wire() map[string]any {
	return map[string]any{
		"UUID":                           m.UUID,
		"EventSourceArn":                 m.EventSourceArn,
		"FunctionArn":                    m.FunctionArn,
		"State":                          m.State,
		"StateTransitionReason":          m.StateTransitionReason,
		"LastModified":                   m.LastModified,
		"LastProcessingResult":           m.LastProcessingResult,
		"BatchSize":                      m.BatchSize,
		"StartingPosition":               m.StartingPosition,
		"MaximumBatchingWindowInSeconds": m.MaximumBatchingWindowInSeconds,
		"FunctionResponseTypes":          []any{},
	}
}

func (s *Server) loadESM(uuid string) (*eventSourceMapping, *apiError) {
	res, err := s.store.Get(s.esmARN(uuid))
	if err != nil || res.Type != typeEventSourceMapping {
		return nil, &apiError{Status: 404, Code: "ResourceNotFoundException",
			Message: "The resource you requested does not exist."}
	}
	var m eventSourceMapping
	if err := json.Unmarshal(res.Body, &m); err != nil {
		return nil, &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	return &m, nil
}

func (s *Server) saveESM(m *eventSourceMapping) *apiError {
	body, err := json.Marshal(m)
	if err != nil {
		return &apiError{Status: 500, Code: "ServiceException", Message: err.Error()}
	}
	s.store.Put(&core.Resource{
		Cloud:    "aws",
		ID:       s.esmARN(m.UUID),
		Type:     typeEventSourceMapping,
		Name:     m.UUID,
		ParentID: m.FunctionArn,
		Body:     body,
	})
	return nil
}

func (s *Server) getEventSourceMapping(w http.ResponseWriter, uuid string) *apiError {
	m, aerr := s.loadESM(uuid)
	if aerr != nil {
		return aerr
	}
	writeJSON(w, 200, m.wire())
	return nil
}

func (s *Server) listEventSourceMappings(w http.ResponseWriter, r *http.Request) *apiError {
	q := r.URL.Query()
	wantFn := lambdaName(q.Get("FunctionName"))
	wantSrc := q.Get("EventSourceArn")

	out := []map[string]any{}
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == typeEventSourceMapping
	}) {
		var m eventSourceMapping
		if json.Unmarshal(res.Body, &m) != nil {
			continue
		}
		if wantFn != "" && !strings.EqualFold(lambdaName(m.FunctionArn), wantFn) {
			continue
		}
		if wantSrc != "" && !strings.EqualFold(m.EventSourceArn, wantSrc) {
			continue
		}
		out = append(out, m.wire())
	}
	sort.Slice(out, func(i, j int) bool {
		return str(out[i]["UUID"]) < str(out[j]["UUID"])
	})
	writeJSON(w, 200, map[string]any{"EventSourceMappings": out})
	return nil
}

func (s *Server) updateEventSourceMapping(w http.ResponseWriter, r *http.Request, uuid string) *apiError {
	m, aerr := s.loadESM(uuid)
	if aerr != nil {
		return aerr
	}
	var req struct {
		Enabled                        *bool `json:"Enabled"`
		BatchSize                      int   `json:"BatchSize"`
		MaximumBatchingWindowInSeconds *int  `json:"MaximumBatchingWindowInSeconds"`
	}
	if err := readJSON(r, &req); err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterValueException", Message: err.Error()}
	}
	// A field the client left out keeps its value: this is a partial
	// update, and the provider sends only what changed.
	if req.Enabled != nil {
		m.NimbusEnabled = *req.Enabled
		m.State = esmState(m.NimbusEnabled)
	}
	if req.BatchSize > 0 {
		m.BatchSize = req.BatchSize
	}
	if req.MaximumBatchingWindowInSeconds != nil {
		m.MaximumBatchingWindowInSeconds = *req.MaximumBatchingWindowInSeconds
	}
	m.LastModified = esmNow()
	if aerr := s.saveESM(m); aerr != nil {
		return aerr
	}
	writeJSON(w, 202, m.wire())
	return nil
}

func (s *Server) deleteEventSourceMapping(w http.ResponseWriter, uuid string) *apiError {
	m, aerr := s.loadESM(uuid)
	if aerr != nil {
		return aerr
	}
	s.stopPoller(uuid)
	_, _ = s.store.Delete(s.esmARN(uuid))
	m.State = "Deleting"
	writeJSON(w, 202, m.wire())
	return nil
}

// streamTableFor resolves an event source ARN to the table whose stream it
// names, refusing anything Nimbus cannot poll.
//
// Refusing by NAME rather than accepting and never firing: a mapping that
// reports Enabled and silently delivers nothing is indistinguishable from
// a handler that is never reached, which is the worst of both.
func (s *Server) streamTableFor(eventSource string) (string, *apiError) {
	for _, t := range s.streamTables() {
		if strings.EqualFold(s.streamARN(t), eventSource) {
			return t.TableName, nil
		}
	}
	if strings.Contains(strings.ToLower(eventSource), ":table/") {
		return "", &apiError{Status: 400, Code: "InvalidParameterValueException",
			Message: "Stream not found, or the table has no stream enabled: " + eventSource}
	}
	return "", &apiError{Status: 400, Code: "InvalidParameterValueException",
		Message: "Nimbus polls DynamoDB streams only; " + eventSource +
			" is not one (Kinesis, SQS and Kafka sources are not emulated)"}
}
