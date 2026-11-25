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

// expireBefore drops every event in a group older than `cutoff`, and
// returns how many went. An empty STREAM is kept: a stream with no events
// is an ordinary thing in CloudWatch, and removing it would make a
// retention policy look like it had deleted the stream too.
func (l *logStore) expireBefore(group string, cutoff int64) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	removed := 0
	for stream, events := range l.streams[group] {
		kept := events[:0:0]
		for _, e := range events {
			if e.Timestamp < cutoff {
				removed++
				continue
			}
			kept = append(kept, e)
		}
		l.streams[group][stream] = kept
	}
	return removed
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
//
// It is parented to the ACCOUNT, not to the function that caused it, and
// that is deliberate: a log group is its own resource in real AWS and
// OUTLIVES the function whose name it carries -- deleting a Lambda leaves
// /aws/lambda/<name> behind, holding what it printed. So this one escaping
// the cascade is faithful rather than a leak, unlike the AKS node resource
// group, which real Azure really does delete with its cluster.
//
// The milestone 12 audit checked every store.Put of something its caller
// did not itself create. This is the only one that is meant to survive.
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
		aerr = s.listLogGroupTags(w, req)
	case "TagLogGroup", "TagResource":
		aerr = s.tagLogGroup(w, req)
	case "UntagLogGroup", "UntagResource":
		aerr = s.untagLogGroup(w, req)
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
	// A create can carry tags, and the provider sends them that way rather
	// than following with a TagResource -- so dropping them here is enough on
	// its own to produce a permanent diff.
	if tags := stringMap(req["tags"]); len(tags) > 0 {
		_ = s.mergeLogGroupTags(name, tags)
	}
	writeJSON(w, 200, map[string]any{})
	return nil
}

// --- tags -----------------------------------------------------------------
//
// These used to be accepted and thrown away, under a comment saying they
// were "never read back by anything that matters". The aws provider reads
// them on every refresh: `aws_cloudwatch_log_group` with a `tags` block
// applied cleanly and then wanted the same change on every later plan, for
// ever, with nothing in the diff saying where it came from. Same family as
// the AKS kubeconfig and Content-MD5 -- a value a client re-reads has to be
// the value it wrote.
//
// There are two spellings of every one of these calls, both current API: the
// provider uses TagResource/UntagResource/ListTagsForResource with an ARN,
// while `aws logs tag-log-group` uses TagLogGroup/UntagLogGroup/
// ListTagsLogGroup with a name. They are the same operation on the same
// group, so they share an implementation and differ only in how the group is
// named.

// resolveLogGroup finds the group a tag call is about, by either spelling.
//
// CloudWatch's own log-group ARN ends in ":*" -- the wildcard over the
// group's streams -- and a client composes it both with and without, so a
// comparison has to tolerate either. Resolving by NAME rather than by ARN
// string also keeps the account and region out of it, which a hand-written
// policy or a copied ARN often gets wrong.
func (s *Server) resolveLogGroup(req map[string]any) (string, *apiError) {
	name := str(req["logGroupName"])
	if name == "" {
		if a := str(req["resourceArn"]); a != "" {
			if i := strings.Index(a, ":log-group:"); i >= 0 {
				name = strings.TrimSuffix(a[i+len(":log-group:"):], ":*")
			}
		}
	}
	if name == "" {
		return "", &apiError{Status: 400, Code: "InvalidParameterException",
			Message: "logGroupName or resourceArn is required"}
	}
	if !s.logs.hasGroup(name) {
		return "", &apiError{Status: 400, Code: "ResourceNotFoundException",
			Message: "The specified log group does not exist"}
	}
	return name, nil
}

func (s *Server) listLogGroupTags(w http.ResponseWriter, req map[string]any) *apiError {
	name, aerr := s.resolveLogGroup(req)
	if aerr != nil {
		return aerr
	}
	// An untagged group answers an empty MAP, not an absent field and not an
	// error: every Lambda makes its own log group untagged, so that is the
	// common case, and it is the only other shape the provider tolerates.
	tags := map[string]string{}
	if res, err := s.store.Get(s.logGroupARN(name)); err == nil {
		var body map[string]any
		if jsonInto(res.Body, &body) == nil {
			for k, v := range stringMap(body["tags"]) {
				tags[k] = v
			}
		}
	}
	writeJSON(w, 200, map[string]any{"tags": tags})
	return nil
}

func (s *Server) tagLogGroup(w http.ResponseWriter, req map[string]any) *apiError {
	name, aerr := s.resolveLogGroup(req)
	if aerr != nil {
		return aerr
	}
	if err := s.mergeLogGroupTags(name, stringMap(req["tags"])); err != nil {
		return &apiError{Status: 400, Code: "ResourceNotFoundException", Message: err.Error()}
	}
	writeJSON(w, 200, map[string]any{})
	return nil
}

func (s *Server) untagLogGroup(w http.ResponseWriter, req map[string]any) *apiError {
	name, aerr := s.resolveLogGroup(req)
	if aerr != nil {
		return aerr
	}
	// The keys arrive under "tagKeys" from UntagResource and under "tags"
	// from UntagLogGroup -- where "tags" is a LIST, the one place in this
	// API where that field is not an object.
	keys := stringList(req["tagKeys"])
	if len(keys) == 0 {
		keys = stringList(req["tags"])
	}
	err := s.store.Update(s.logGroupARN(name), func(body map[string]any) error {
		tags := stringMap(body["tags"])
		for _, k := range keys {
			delete(tags, k)
		}
		body["tags"] = tags
		return nil
	})
	if err != nil {
		return &apiError{Status: 400, Code: "ResourceNotFoundException", Message: err.Error()}
	}
	writeJSON(w, 200, map[string]any{})
	return nil
}

// mergeLogGroupTags adds tags to a group, keeping the ones already there.
//
// Through store.Update, which holds the store's lock across read, change and
// write: a create and a TagResource for the same group arrive one after the
// other and a load-mutate-save would be the lost update this repo already
// has a rule about.
func (s *Server) mergeLogGroupTags(name string, add map[string]string) error {
	if len(add) == 0 {
		return nil
	}
	return s.store.Update(s.logGroupARN(name), func(body map[string]any) error {
		tags := stringMap(body["tags"])
		for k, v := range add {
			tags[k] = v
		}
		body["tags"] = tags
		return nil
	})
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
	// A pattern Nimbus cannot apply is REFUSED rather than matched as a
	// substring: a JSON pattern compared literally matches nothing, and
	// an empty answer is indistinguishable from "nothing has been logged
	// yet". See logs_filter.go.
	filter, err := parseLogFilter(str(req["filterPattern"]))
	if err != nil {
		return &apiError{Status: 400, Code: "InvalidParameterException", Message: err.Error()}
	}
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
			if !filter.matches(e.Message) {
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
