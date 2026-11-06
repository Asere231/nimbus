package aws

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// CloudWatch Logs: JSON 1.1, and the field names are lowerCamelCase.
//
// Measured from aws-cli 1.40.33: `X-Amz-Target: Logs_20140328.PutLogEvents`,
// body `{"logGroupName": "g", "logStreamName": "s", "logEvents": [...]}`.
// DynamoDB, which is JSON 1.0 behind the same port, spells everything in
// TitleCase -- so "it is JSON, like the other one" is not enough to go on.
// The dialect in the Content-Type differs too (1.1 rather than 1.0), though
// nothing in either direction depends on which it is.
//
// A log group's reason for existing here is Lambda: whatever a handler
// prints lands in /aws/lambda/<name>, so `aws logs filter-log-events` shows
// what really happened inside the container rather than nothing at all.

const (
	typeLogGroup    = "AWS::Logs::LogGroup"
	logsTargetPfx   = "Logs_20140328."
	defaultLogStore = 10000 // events kept per stream
)

// logStore holds log events. In memory only, deliberately: logs are the one
// thing nobody expects to survive a restart of the service that wrote them,
// and a file per stream would be a lot of small writes for data that is
// read once.
type logStore struct {
	mu sync.RWMutex
	// group -> stream -> events, in arrival order
	streams map[string]map[string][]logEvent
}

type logEvent struct {
	Timestamp int64  `json:"timestamp"`
	Message   string `json:"message"`
	// IngestionTime is when CloudWatch received it, which a client shows
	// separately from when the line was written.
	IngestionTime int64 `json:"ingestionTime"`
}

func newLogStore() *logStore {
	return &logStore{streams: map[string]map[string][]logEvent{}}
}

func (l *logStore) ensureGroup(group string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams[group] == nil {
		l.streams[group] = map[string][]logEvent{}
	}
}

func (l *logStore) hasGroup(group string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	_, ok := l.streams[group]
	return ok
}

func (l *logStore) dropGroup(group string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.streams, group)
}

func (l *logStore) ensureStream(group, stream string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams[group] == nil {
		l.streams[group] = map[string][]logEvent{}
	}
	if _, ok := l.streams[group][stream]; !ok {
		l.streams[group][stream] = nil
	}
}

func (l *logStore) append(group, stream string, events []logEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.streams[group] == nil {
		l.streams[group] = map[string][]logEvent{}
	}
	kept := append(l.streams[group][stream], events...)
	// Bounded, so a handler in a loop cannot grow the process without
	// limit. The oldest go first, which is what a retention policy would
	// do anyway.
	if len(kept) > defaultLogStore {
		kept = kept[len(kept)-defaultLogStore:]
	}
	l.streams[group][stream] = kept
}

func (l *logStore) groups() []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.streams))
	for g := range l.streams {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func (l *logStore) streamNames(group string) []string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.streams[group]))
	for s := range l.streams[group] {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func (l *logStore) events(group, stream string) []logEvent {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return append([]logEvent{}, l.streams[group][stream]...)
}

// --- the API --------------------------------------------------------------

func (s *Server) logGroupARN(name string) string {
	return arn("logs", s.cfg.Region, s.cfg.AccountID, "log-group:"+name+":*")
}

// ensureLogGroup creates a group if it is not there, both in the store (so
// it draws in the visualizer) and in the log store.
func (s *Server) ensureLogGroup(name string) {
	if name == "" {
		return
	}
	s.logs.ensureGroup(name)
	id := s.logGroupARN(name)
	if s.store.Exists(id) {
		return
	}
	body := jsonBytes(map[string]any{
		"logGroupName":      name,
		"arn":               id,
		"creationTime":      time.Now().UnixMilli(),
		"storedBytes":       0,
		"metricFilterCount": 0,
	})
	s.store.Put(&core.Resource{
		Cloud: "aws", ID: id, Type: typeLogGroup, Name: name,
		ParentID: s.accountARN(), Location: s.cfg.Region, Backing: "real", Body: body,
	})
}

func (s *Server) serveLogs(w http.ResponseWriter, r *http.Request) string {
	target := r.Header.Get("X-Amz-Target")
	action := target
	if i := strings.LastIndex(target, "."); i >= 0 {
		action = target[i+1:]
	}
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		writeLogsError(w, &apiError{Status: 400, Code: "SerializationException", Message: err.Error()})
		return "logs: unreadable body"
	}
	if req == nil {
		req = map[string]any{}
	}

	var aerr *apiError
	switch action {
	case "CreateLogGroup":
		aerr = s.createLogGroup(w, req)
	case "DeleteLogGroup":
		aerr = s.deleteLogGroup(w, req)
	case "DescribeLogGroups":
		aerr = s.describeLogGroups(w, req)
	case "CreateLogStream":
		aerr = s.createLogStream(w, req)
	case "DescribeLogStreams":
		aerr = s.describeLogStreams(w, req)
	case "PutLogEvents":
		aerr = s.putLogEvents(w, req)
	case "GetLogEvents":
		aerr = s.getLogEvents(w, req)
	case "FilterLogEvents":
		aerr = s.filterLogEvents(w, req)
	case "PutRetentionPolicy", "DeleteRetentionPolicy":
		aerr = s.putRetentionPolicy(w, req, action == "PutRetentionPolicy")
	case "ListTagsLogGroup", "ListTagsForResource":
		// Tags on a log group are accepted and never read back by anything
		// that matters, so an empty map is the honest answer.
		writeJSON(w, 200, map[string]any{"tags": map[string]string{}})
	case "TagLogGroup", "UntagLogGroup", "TagResource", "UntagResource":
		writeJSON(w, 200, map[string]any{})
	default:
		aerr = invalidAction("CloudWatch Logs", action)
	}

	if aerr != nil {
		writeLogsError(w, aerr)
		return "logs " + action + ": " + aerr.Code
	}
	return ""
}

// writeLogsError is JSON 1.1's envelope. Like DynamoDB's but with this
// service's own namespace, and again a 400 for everything.
func writeLogsError(w http.ResponseWriter, e *apiError) {
	status := e.Status
	if status == 0 {
		status = 400
	}
	w.Header().Set("x-amzn-ErrorType", e.Code)
	writeJSON(w, status, map[string]string{"__type": e.Code, "message": e.Message})
}

func logsNotFound(what string) *apiError {
	return &apiError{Status: 400, Code: "ResourceNotFoundException",
		Message: "The specified " + what + " does not exist."}
}

func (s *Server) createLogGroup(w http.ResponseWriter, req map[string]any) *apiError {
	name := str(req["logGroupName"])
	if name == "" {
		return &apiError{Status: 400, Code: "InvalidParameterException",
			Message: "logGroupName is required"}
	}
	if s.logs.hasGroup(name) {
		return &apiError{Status: 400, Code: "ResourceAlreadyExistsException",
			Message: "The specified log group already exists"}
	}
	s.ensureLogGroup(name)
	writeJSON(w, 200, map[string]any{})
	return nil
}

func (s *Server) deleteLogGroup(w http.ResponseWriter, req map[string]any) *apiError {
	name := str(req["logGroupName"])
	if !s.logs.hasGroup(name) {
		return logsNotFound("log group")
	}
	s.logs.dropGroup(name)
	_, _ = s.store.Delete(s.logGroupARN(name))
	writeJSON(w, 200, map[string]any{})
	return nil
}

func (s *Server) describeLogGroups(w http.ResponseWriter, req map[string]any) *apiError {
	prefix := str(req["logGroupNamePrefix"])
	pattern := str(req["logGroupNamePattern"])
	out := []map[string]any{}
	for _, name := range s.logs.groups() {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		if pattern != "" && !strings.Contains(name, pattern) {
			continue
		}
		out = append(out, s.logGroupDoc(name))
	}
	writeJSON(w, 200, map[string]any{"logGroups": out})
	return nil
}

func (s *Server) logGroupDoc(name string) map[string]any {
	doc := map[string]any{
		"logGroupName":      name,
		"arn":               s.logGroupARN(name),
		"creationTime":      time.Now().UnixMilli(),
		"metricFilterCount": 0,
		"storedBytes":       0,
	}
	// Whatever is stored wins, so creationTime and any retention stay put
	// between reads -- a value that changed every read would be a diff.
	if res, err := s.store.Get(s.logGroupARN(name)); err == nil {
		var stored map[string]any
		if jsonInto(res.Body, &stored) == nil {
			for k, v := range stored {
				doc[k] = v
			}
		}
	}
	return doc
}

func (s *Server) createLogStream(w http.ResponseWriter, req map[string]any) *apiError {
	group, stream := str(req["logGroupName"]), str(req["logStreamName"])
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	s.logs.ensureStream(group, stream)
	writeJSON(w, 200, map[string]any{})
	return nil
}

func (s *Server) describeLogStreams(w http.ResponseWriter, req map[string]any) *apiError {
	group := str(req["logGroupName"])
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	prefix := str(req["logStreamNamePrefix"])
	out := []map[string]any{}
	for _, name := range s.logs.streamNames(group) {
		if prefix != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		events := s.logs.events(group, name)
		doc := map[string]any{
			"logStreamName": name,
			"arn":           s.logGroupARN(group) + ":log-stream:" + name,
			"creationTime":  time.Now().UnixMilli(),
			"storedBytes":   0,
		}
		if len(events) > 0 {
			doc["firstEventTimestamp"] = events[0].Timestamp
			doc["lastEventTimestamp"] = events[len(events)-1].Timestamp
			doc["lastIngestionTime"] = events[len(events)-1].IngestionTime
		}
		out = append(out, doc)
	}
	writeJSON(w, 200, map[string]any{"logStreams": out})
	return nil
}

func (s *Server) putLogEvents(w http.ResponseWriter, req map[string]any) *apiError {
	group, stream := str(req["logGroupName"]), str(req["logStreamName"])
	if group == "" || stream == "" {
		return &apiError{Status: 400, Code: "InvalidParameterException",
			Message: "logGroupName and logStreamName are required"}
	}
	// A client that writes to a group nobody created is a common enough
	// mistake that CloudWatch's own answer -- refuse -- is the useful one,
	// because a silently created group hides a typo in a name.
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	var events []logEvent
	now := time.Now().UnixMilli()
	for _, raw := range objList(req["logEvents"]) {
		ts, _ := raw["timestamp"].(float64)
		if ts == 0 {
			ts = float64(now)
		}
		events = append(events, logEvent{
			Timestamp: int64(ts), Message: str(raw["message"]), IngestionTime: now,
		})
	}
	s.logs.ensureStream(group, stream)
	s.logs.append(group, stream, events)
	// The sequence token is part of the response shape even though nothing
	// since 2023 sends one back.
	writeJSON(w, 200, map[string]any{"nextSequenceToken": "49590000000000000000000000000000000000000000000000000000"})
	return nil
}

func (s *Server) getLogEvents(w http.ResponseWriter, req map[string]any) *apiError {
	group, stream := str(req["logGroupName"]), str(req["logStreamName"])
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	events := s.logs.events(group, stream)
	if v, ok := req["startFromHead"].(bool); ok && !v {
		// Newest first, which is what a client reading the tail asks for.
		reverse(events)
	}
	out := []map[string]any{}
	for _, e := range events {
		out = append(out, map[string]any{
			"timestamp": e.Timestamp, "message": e.Message, "ingestionTime": e.IngestionTime,
		})
	}
	writeJSON(w, 200, map[string]any{
		"events": out,
		// Both tokens must be PRESENT: a client pages until the token stops
		// changing, and a missing one reads as "page again from the start".
		"nextForwardToken":  "f/0",
		"nextBackwardToken": "b/0",
	})
	return nil
}

// filterLogEvents reads across every stream in a group, which is what
// `aws logs tail` and the console both use.
func (s *Server) filterLogEvents(w http.ResponseWriter, req map[string]any) *apiError {
	group := str(req["logGroupName"])
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	pattern := str(req["filterPattern"])
	var start, end int64
	if v, ok := req["startTime"].(float64); ok {
		start = int64(v)
	}
	if v, ok := req["endTime"].(float64); ok {
		end = int64(v)
	}

	out := []map[string]any{}
	for _, stream := range s.logs.streamNames(group) {
		if names := strList(req["logStreamNames"]); len(names) > 0 && !containsString(names, stream) {
			continue
		}
		for _, e := range s.logs.events(group, stream) {
			if start > 0 && e.Timestamp < start {
				continue
			}
			if end > 0 && e.Timestamp > end {
				continue
			}
			// A real filter pattern is its own small language; a substring
			// is what nearly every use of it means, and matching more than
			// asked for is the safe direction.
			if pattern != "" && !strings.Contains(e.Message, strings.Trim(pattern, `"`)) {
				continue
			}
			out = append(out, map[string]any{
				"logStreamName": stream, "timestamp": e.Timestamp,
				"message": e.Message, "ingestionTime": e.IngestionTime,
				"eventId": stableID(stream+e.Message, 32),
			})
		}
	}
	// Across streams, by time: a group's lines read as one log.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i]["timestamp"].(int64) < out[j]["timestamp"].(int64)
	})
	writeJSON(w, 200, map[string]any{"events": out, "searchedLogStreams": []any{}})
	return nil
}

func (s *Server) putRetentionPolicy(w http.ResponseWriter, req map[string]any, set bool) *apiError {
	group := str(req["logGroupName"])
	if !s.logs.hasGroup(group) {
		return logsNotFound("log group")
	}
	_ = s.store.Update(s.logGroupARN(group), func(body map[string]any) error {
		if set {
			body["retentionInDays"] = req["retentionInDays"]
		} else {
			delete(body, "retentionInDays")
		}
		return nil
	})
	writeJSON(w, 200, map[string]any{})
	return nil
}

func reverse(events []logEvent) {
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
