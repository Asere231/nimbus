package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// CloudWatch filter patterns.
//
// `filterPattern` was matched as a plain SUBSTRING, under a comment
// saying that matching more than asked for is the safe direction. That is
// true of the patterns a substring can approximate, and badly false of
// the ones it cannot: a JSON pattern like `{ $.level = "ERROR" }` was
// compared literally against each message, matched nothing, and came back
// EMPTY -- which is indistinguishable from "nothing has been logged yet",
// the ordinary state of a log group somebody is looking at for the first
// time. The same trap KQL's unparsed operators had.
//
// So the TEXT pattern language is implemented, and the two that are not
// are refused by name.
//
// # The language, for plain text logs
//
//	error                  a term that must appear
//	"error here"           a quoted term, so it can contain spaces
//	error exception        BOTH must appear
//	?error ?warn           EITHER may appear
//	-debug                 must NOT appear
//
// Required, optional and excluded combine the way CloudWatch combines
// them: every required term must match, at least one optional term must
// match if any are given, and no excluded term may match.
//
// Matching is CASE-SENSITIVE, which is worth pinning because it is the
// opposite of KQL's `contains` on the Azure side -- the same project
// answering the same question differently because the two services do.

// logFilter is a parsed filter pattern.
type logFilter struct {
	required []string
	optional []string
	excluded []string
}

// parseLogFilter reads a pattern, refusing the two forms Nimbus does not
// implement rather than letting them match nothing.
func parseLogFilter(pattern string) (*logFilter, error) {
	trimmed := strings.TrimSpace(pattern)
	if trimmed == "" {
		return &logFilter{}, nil
	}
	// A JSON pattern selects on fields inside a JSON message; a
	// space-delimited one names columns. Both need a parser Nimbus does
	// not have, and both are silently empty under a substring match.
	if strings.HasPrefix(trimmed, "{") {
		return nil, fmt.Errorf("a JSON filter pattern (%s) is not implemented by Nimbus; "+
			"use terms, ?optional terms or -excluded terms", trimmed)
	}
	if strings.HasPrefix(trimmed, "[") {
		return nil, fmt.Errorf("a space-delimited filter pattern (%s) is not implemented by "+
			"Nimbus; use terms, ?optional terms or -excluded terms", trimmed)
	}

	f := &logFilter{}
	for _, token := range splitFilterTerms(trimmed) {
		switch {
		case strings.HasPrefix(token, "?"):
			if term := unquoteTerm(token[1:]); term != "" {
				f.optional = append(f.optional, term)
			}
		case strings.HasPrefix(token, "-"):
			if term := unquoteTerm(token[1:]); term != "" {
				f.excluded = append(f.excluded, term)
			}
		default:
			if term := unquoteTerm(token); term != "" {
				f.required = append(f.required, term)
			}
		}
	}
	return f, nil
}

// splitFilterTerms splits on spaces, keeping a quoted term together --
// `"error here"` is ONE term, and splitting it would silently become two
// required terms that happen to match different lines.
func splitFilterTerms(pattern string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	for _, r := range pattern {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			current.WriteRune(r)
		case r == ' ' && !inQuotes:
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

func unquoteTerm(term string) string {
	if len(term) >= 2 && strings.HasPrefix(term, `"`) && strings.HasSuffix(term, `"`) {
		return term[1 : len(term)-1]
	}
	return term
}

// matches decides whether one message passes.
//
// Case-SENSITIVE, which is CloudWatch's own behaviour and the opposite of
// the KQL `contains` on the Azure side.
func (f *logFilter) matches(message string) bool {
	for _, term := range f.excluded {
		if strings.Contains(message, term) {
			return false
		}
	}
	for _, term := range f.required {
		if !strings.Contains(message, term) {
			return false
		}
	}
	if len(f.optional) == 0 {
		return true
	}
	for _, term := range f.optional {
		if strings.Contains(message, term) {
			return true
		}
	}
	return false
}

// --- retention ------------------------------------------------------------

// sweepLogRetention deletes events older than each group's retention
// policy, and returns how many it removed.
//
// `retentionInDays` round-tripped as a setting and nothing ever expired.
// CloudWatch's smallest retention is ONE DAY, so -- like an S3 lifecycle
// rule and unlike a DynamoDB TTL -- no policy a client can set will fire
// while anybody is watching. `now` is a parameter for exactly that
// reason: a test asks what happens a fortnight out, so the behaviour is
// pinned rather than hypothetical.
//
// Note what retention does NOT do here: a group's events are in memory
// and bounded at 10,000, and do not survive a restart. Retention is the
// policy being applied, not the only thing that ever removes an event.
func (s *Server) sweepLogRetention(now time.Time) int {
	removed := 0
	for _, group := range s.logs.groups() {
		days := s.retentionDaysOf(group)
		if days <= 0 {
			continue
		}
		cutoff := now.Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()
		removed += s.logs.expireBefore(group, cutoff)
	}
	return removed
}

// retentionDaysOf reads the policy off the group's stored resource, which
// is where putRetentionPolicy writes it.
func (s *Server) retentionDaysOf(group string) int {
	res, err := s.store.Get(s.logGroupARN(group))
	if err != nil {
		return 0
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return 0
	}
	switch v := body["retentionInDays"].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// startRetentionSweeper applies retention policies until the context is
// cancelled.
func (s *Server) startRetentionSweeper(ctx context.Context) {
	ticker := time.NewTicker(ttlSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweepLogRetention(time.Now())
		}
	}
}
