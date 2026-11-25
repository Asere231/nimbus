package aws

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// CloudWatch filter patterns, which were matched as a plain SUBSTRING.
//
// That is a fair approximation of the patterns a substring can express
// and badly wrong for the ones it cannot: a JSON pattern compared
// literally against each message matched nothing and came back EMPTY --
// indistinguishable from "nothing has been logged yet", the ordinary
// state of a log group somebody is looking at for the first time.

func TestAFilterPatternIsATermLanguage(t *testing.T) {
	lines := []string{
		"ERROR connection refused",
		"WARN retrying connection",
		"INFO connection established",
		"DEBUG socket opened",
	}
	cases := []struct {
		pattern string
		want    []string
	}{
		{"", lines}, // everything
		{"ERROR", []string{"ERROR connection refused"}},                // one term
		{"connection", lines[:3]},                                      // a substring of three
		{"ERROR refused", []string{"ERROR connection refused"}},        // AND
		{"ERROR established", nil},                                     // AND that matches nothing
		{"?ERROR ?WARN", lines[:2]},                                    // OR
		{"connection -ERROR", lines[1:3]},                              // required and excluded
		{`"connection refused"`, []string{"ERROR connection refused"}}, // a quoted phrase
		{"-connection", []string{"DEBUG socket opened"}},               // excluded only
		{"?ERROR ?WARN -retrying", []string{"ERROR connection refused"}},
	}
	for _, tc := range cases {
		f, err := parseLogFilter(tc.pattern)
		if err != nil {
			t.Fatalf("%q: %v", tc.pattern, err)
		}
		var got []string
		for _, line := range lines {
			if f.matches(line) {
				got = append(got, line)
			}
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%q matched %v, want %v", tc.pattern, got, tc.want)
		}
	}
}

// A quoted phrase is ONE term. Splitting it would silently become two
// required terms that happen to match different lines -- more results,
// quietly, which is the direction a filter must not fail in.
func TestAQuotedPhraseIsOneTerm(t *testing.T) {
	f, err := parseLogFilter(`"connection refused"`)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.required) != 1 || f.required[0] != "connection refused" {
		t.Fatalf("the pattern parsed to %v", f.required)
	}
	if f.matches("refused the connection") {
		t.Error("the phrase matched a line with the words in the wrong order")
	}
	if !f.matches("ERROR connection refused here") {
		t.Error("the phrase did not match a line containing it")
	}
}

// CASE-SENSITIVE, which is CloudWatch's own behaviour -- and the opposite
// of KQL's `contains` on the Azure side. The same project answering the
// same question differently because the two services do.
func TestAFilterPatternIsCaseSensitive(t *testing.T) {
	f, _ := parseLogFilter("ERROR")
	if f.matches("error connection refused") {
		t.Error("an uppercase pattern matched a lowercase line")
	}
	if !f.matches("ERROR connection refused") {
		t.Error("the pattern did not match its own case")
	}
}

// The two forms Nimbus does not implement are REFUSED by name. Matching
// them as a substring returned nothing, which reads as "nothing logged".
func TestAnUnimplementedFilterPatternIsRefused(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/fn"}`)

	for _, pattern := range []string{`{ $.level = "ERROR" }`, `[w1=ERROR, w2]`} {
		if _, err := parseLogFilter(pattern); err == nil {
			t.Errorf("%q was accepted", pattern)
		}
		rec, doc := logs(t, s, "FilterLogEvents",
			`{"logGroupName":"/aws/lambda/fn","filterPattern":`+quoteJSON(pattern)+`}`)
		if rec.Code != 400 {
			t.Errorf("%q was accepted over the wire with %d", pattern, rec.Code)
		}
		if !strings.Contains(str(doc["message"]), "not implemented by Nimbus") {
			t.Errorf("the refusal for %q does not say so: %v", pattern, doc["message"])
		}
	}
}

// Retention really expires events. `now` is a parameter because
// CloudWatch's smallest retention is a DAY, so no policy a client can set
// would fire while anybody is watching.
func TestRetentionExpiresOldEvents(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/fn"}`)

	now := time.Now()
	old := now.Add(-20 * 24 * time.Hour).UnixMilli()
	recent := now.Add(-time.Hour).UnixMilli()
	putEvents(t, s, "/aws/lambda/fn", "s1", old, recent)

	// No policy, nothing expires however old the events are.
	if removed := s.sweepLogRetention(now); removed != 0 {
		t.Errorf("a group with no retention policy lost %d events", removed)
	}

	mustLogs(t, s, "PutRetentionPolicy",
		`{"logGroupName":"/aws/lambda/fn","retentionInDays":14}`)
	if removed := s.sweepLogRetention(now); removed != 1 {
		t.Errorf("the sweep removed %d events, want the one older than 14 days", removed)
	}
	events := s.logs.events("/aws/lambda/fn", "s1")
	if len(events) != 1 || events[0].Timestamp != recent {
		t.Errorf("the surviving events are %v, want just the recent one", events)
	}

	// And deleting the policy stops it again.
	mustLogs(t, s, "DeleteRetentionPolicy", `{"logGroupName":"/aws/lambda/fn"}`)
	putEvents(t, s, "/aws/lambda/fn", "s1", old)
	if removed := s.sweepLogRetention(now); removed != 0 {
		t.Errorf("a deleted policy still removed %d events", removed)
	}
}

// An empty STREAM survives a sweep: a stream with no events is ordinary
// in CloudWatch, and removing it would make retention look as though it
// had deleted the stream too.
func TestRetentionKeepsAnEmptiedStream(t *testing.T) {
	s := newLogsTestServer(t)
	mustLogs(t, s, "CreateLogGroup", `{"logGroupName": "/aws/lambda/fn"}`)
	putEvents(t, s, "/aws/lambda/fn", "s1", time.Now().Add(-40*24*time.Hour).UnixMilli())
	mustLogs(t, s, "PutRetentionPolicy",
		`{"logGroupName":"/aws/lambda/fn","retentionInDays":7}`)

	s.sweepLogRetention(time.Now())
	names := s.logs.streamNames("/aws/lambda/fn")
	if len(names) != 1 || names[0] != "s1" {
		t.Errorf("the stream list is %v, want it to still hold s1", names)
	}
	if n := len(s.logs.events("/aws/lambda/fn", "s1")); n != 0 {
		t.Errorf("the stream still holds %d events", n)
	}
}

// putEvents writes events at the given timestamps, through the API so the
// stream is created the way a client creates one.
func putEvents(t *testing.T, s *Server, group, stream string, at ...int64) {
	t.Helper()
	var entries []string
	for i, ts := range at {
		entries = append(entries, fmt.Sprintf(`{"timestamp": %d, "message": "line %d"}`, ts, i))
	}
	mustLogs(t, s, "CreateLogStream",
		fmt.Sprintf(`{"logGroupName": %q, "logStreamName": %q}`, group, stream))
	mustLogs(t, s, "PutLogEvents", fmt.Sprintf(
		`{"logGroupName": %q, "logStreamName": %q, "logEvents": [%s]}`,
		group, stream, strings.Join(entries, ",")))
}
