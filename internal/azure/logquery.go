package azure

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Querying a workspace, in as much KQL as can be answered honestly.
//
// The endpoint and the body are measured rather than guessed. The az
// `log-analytics` extension POSTs
//
//	{"query": "ContainerAppConsoleLogs | take 10"}
//
// to `{logAnalyticsResourceId}/v1/workspaces/{customerId}/query`, and it
// finds that base URL in the cloud metadata document Nimbus already
// serves -- `logAnalyticsResourceId`, which has been in `handleMetadata`
// since Milestone 5 and pointed at us the whole time. So `az monitor
// log-analytics query` needed no new CLI plumbing at all: only this path.
// (The command itself is not in az 2.90.0; it lives in the `log-analytics`
// extension, which has to be installed in a shell WITHOUT
// REQUESTS_CA_BUNDLE set, exactly like `az bicep install`.)
//
// **KQL is a language and this is a pipeline subset.** The operators below
// are implemented; everything else is REFUSED AND NAMED. That is the whole
// design, and both wrong answers are worse than the refusal:
//
//   - treating an unparsed operator as a no-op returns EVERY row, which is
//     the DynamoDB FilterExpression trap and found nowhere near its cause;
//   - returning no rows is subtler and just as bad, because an empty
//     result is indistinguishable from "nothing has been logged yet",
//     which is the ordinary state of a workspace somebody is looking at
//     for the first time.

// tableContainerAppConsole is where a container app's stdout lands. Azure's
// own name, so a query copied from a tutorial finds it.
const tableContainerAppConsole = "ContainerAppConsoleLogs"

// The columns a row has, in Azure's own spelling. The `_s` suffix is Log
// Analytics' convention for a string column on a custom table; a real
// query names them exactly like this, so they are not tidied.
var logColumns = []queryColumn{
	{Name: "TimeGenerated", Type: "datetime"},
	{Name: "ContainerAppName_s", Type: "string"},
	{Name: "RevisionName_s", Type: "string"},
	{Name: "Stream_s", Type: "string"},
	{Name: "Log_s", Type: "string"},
}

type queryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// queryTable is one result table. A query answers with exactly one, named
// PrimaryResult, which is what the CLI reads.
type queryTable struct {
	Name    string        `json:"name"`
	Columns []queryColumn `json:"columns"`
	Rows    [][]any       `json:"rows"`
}

// supportedKQL names what this understands, for the refusal message. Kept
// beside the implementation so the two cannot drift, and asserted against
// the implemented verbs by a test -- a list in prose that nothing reads is
// exactly the kind of claim this project keeps finding stale.
const supportedKQL = "where (with and/or), project, extend-free summarize count() [by col], " +
	"order by, sort by, top, distinct, take, limit, count"

// kqlResult is the pipeline's state once an AGGREGATION has happened.
//
// Before that, a stage operates on log rows, which is what `where`,
// `project` and `order by` are about. `summarize`, `distinct` and `count`
// each collapse those into something that is no longer a log line -- a
// group and a number -- so after one of them only `take` and `limit` still
// mean anything here, and every other stage is refused by name. Real KQL
// goes on piping happily; pretending to would mean inventing semantics,
// which is the one thing this file is written not to do.
type kqlResult struct {
	columns []queryColumn
	rows    [][]any
}

// runKQL evaluates the subset against a workspace's rows.
func runKQL(query string, lines []logLine) (queryTable, error) {
	stages := splitPipeline(query)
	if len(stages) == 0 || stages[0] == "" {
		return queryTable{}, fmt.Errorf(
			"a query has to start with a table name, e.g. %q", tableContainerAppConsole+" | take 10")
	}

	table := stages[0]
	if i := strings.IndexAny(table, " \t"); i >= 0 {
		// A table name followed by anything is not something this can
		// read: `Table where x` without a pipe, a time range, a function.
		return queryTable{}, fmt.Errorf(
			"%q is not a table name this emulator can read; a query is a table name "+
				"followed by pipeline stages (%s)", table, supportedKQL)
	}

	// Rows of the named table only. An unknown table is EMPTY rather than
	// an error: a workspace that exists and has had nothing sent to it is
	// the ordinary state of a new one.
	var rows []logLine
	for _, l := range lines {
		if strings.EqualFold(l.Table, table) {
			rows = append(rows, l)
		}
	}
	sortLines(rows)

	columns := logColumns
	var agg *kqlResult

	for _, stage := range stages[1:] {
		verb, rest := splitVerb(stage)
		lower := strings.ToLower(verb)

		// `order by` and `sort by` are two-word verbs, so the "by" has to
		// come off before the column is read -- splitVerb cuts at the
		// first space and knows nothing about that.
		if lower == "order" || lower == "sort" {
			if agg != nil {
				return queryTable{}, errAfterAggregation(verb)
			}
			col, desc, err := parseOrderBy(verb, rest)
			if err != nil {
				return queryTable{}, err
			}
			sortRowsBy(rows, col, desc)
			continue
		}

		// Once something has aggregated, the rows are no longer log lines
		// and only a row limit still means anything.
		if agg != nil {
			switch lower {
			case "take", "limit":
				n, err := rowCount(verb, rest)
				if err != nil {
					return queryTable{}, err
				}
				if n < len(agg.rows) {
					agg.rows = agg.rows[:n]
				}
				continue
			default:
				return queryTable{}, errAfterAggregation(verb)
			}
		}

		switch lower {
		case "where":
			filtered, err := applyWhere(rows, rest)
			if err != nil {
				return queryTable{}, err
			}
			rows = filtered
		case "project":
			cols, err := applyProject(rest)
			if err != nil {
				return queryTable{}, err
			}
			columns = cols
		case "take", "limit":
			n, err := rowCount(verb, rest)
			if err != nil {
				return queryTable{}, err
			}
			if n < len(rows) {
				rows = rows[:n]
			}
		case "top":
			trimmed, err := applyTop(rows, rest)
			if err != nil {
				return queryTable{}, err
			}
			rows = trimmed
		case "summarize":
			res, err := applySummarize(rows, rest)
			if err != nil {
				return queryTable{}, err
			}
			agg = res
		case "distinct":
			res, err := applyDistinct(rows, rest)
			if err != nil {
				return queryTable{}, err
			}
			agg = res
		case "count":
			agg = &kqlResult{
				columns: []queryColumn{{Name: "Count", Type: "long"}},
				rows:    [][]any{{len(rows)}},
			}
		default:
			return queryTable{}, fmt.Errorf(
				"the KQL operator %q is not supported by Nimbus, so this query is refused "+
					"rather than answered with a guess (supported: %s)", verb, supportedKQL)
		}
	}

	if agg != nil {
		return queryTable{Name: "PrimaryResult", Columns: agg.columns, Rows: agg.rows}, nil
	}

	out := queryTable{Name: "PrimaryResult", Columns: columns, Rows: [][]any{}}
	for _, l := range rows {
		out.Rows = append(out.Rows, rowFor(l, columns))
	}
	return out, nil
}

// errAfterAggregation explains why a stage cannot follow a summarize.
func errAfterAggregation(verb string) error {
	return fmt.Errorf(
		"%q cannot follow summarize, distinct or count here: those collapse log rows into "+
			"groups and numbers, and Nimbus does not carry the result on through the rest "+
			"of the pipeline. Only take and limit do. (supported: %s)", verb, supportedKQL)
}

func rowCount(verb, rest string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q needs a row count, e.g. `%s 10`", verb, verb)
	}
	return n, nil
}

// parseOrderBy reads `order by Col [asc|desc]`.
//
// KQL's default is DESCENDING, which is the opposite of what the word
// suggests and of what every SQL dialect does. Getting it wrong silently
// reverses a log, so the default is explicit here and pinned by a test.
func parseOrderBy(verb, rest string) (column string, desc bool, err error) {
	rest = strings.TrimSpace(rest)
	fields := strings.Fields(rest)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "by") {
		return "", false, fmt.Errorf(
			"`%s` needs `by` and a column, e.g. `%s by TimeGenerated desc`", verb, verb)
	}
	fields = fields[1:]

	desc = true // KQL's default
	switch len(fields) {
	case 1:
		column = fields[0]
	case 2:
		column = fields[0]
		switch strings.ToLower(fields[1]) {
		case "asc":
			desc = false
		case "desc":
			desc = true
		default:
			return "", false, fmt.Errorf("%q is not a sort direction; use asc or desc", fields[1])
		}
	default:
		return "", false, fmt.Errorf(
			"`%s` here takes one column and an optional asc/desc, not %q -- sorting on "+
				"several columns is refused rather than half-applied", verb, rest)
	}
	if !knownColumn(column) {
		return "", false, fmt.Errorf("%q is not a column this emulator knows (known: %s)",
			column, columnNames())
	}
	return column, desc, nil
}

// sortRowsBy orders rows on one column, in place.
//
// A stable sort, so rows that compare equal keep the order they were
// ingested in -- which for a log is the order they happened in, and the
// only sensible tie-break.
func sortRowsBy(rows []logLine, column string, desc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := columnValue(rows[i], column), columnValue(rows[j], column)
		if desc {
			return a > b
		}
		return a < b
	})
}

// applyTop is `top N by Col [asc|desc]`: a sort and a limit in one
// operator, which is how it is defined and how people use it.
func applyTop(rows []logLine, rest string) ([]logLine, error) {
	rest = strings.TrimSpace(rest)
	i := strings.IndexAny(rest, " \t")
	if i <= 0 {
		return nil, fmt.Errorf(
			"`top` needs a count and a column, e.g. `top 10 by TimeGenerated desc`")
	}
	n, err := rowCount("top", rest[:i])
	if err != nil {
		return nil, err
	}
	column, desc, err := parseOrderBy("top", strings.TrimSpace(rest[i+1:]))
	if err != nil {
		return nil, err
	}
	sortRowsBy(rows, column, desc)
	if n < len(rows) {
		rows = rows[:n]
	}
	return rows, nil
}

// applySummarize supports `count()`, with or without a `by` column.
//
// Only count(). Every other aggregation is refused BY NAME: an avg or a
// dcount approximated with a row count would be a wrong number presented
// as a right one, which is worse than no answer at all. The output column
// is `count_`, the name real KQL gives it, so a query that goes on to name
// that column is reading the same thing it would in Azure.
func applySummarize(rows []logLine, rest string) (*kqlResult, error) {
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(strings.ToLower(rest), "count()") {
		return nil, fmt.Errorf(
			"`summarize %s` is not supported: Nimbus implements count() only, with an "+
				"optional `by` column. Any other aggregation is refused rather than "+
				"approximated with a count, which would be a wrong number that looks right",
			rest)
	}
	after := strings.TrimSpace(rest[len("count()"):])
	if after == "" {
		return &kqlResult{
			columns: []queryColumn{{Name: "count_", Type: "long"}},
			rows:    [][]any{{len(rows)}},
		}, nil
	}
	fields := strings.Fields(after)
	if !strings.EqualFold(fields[0], "by") {
		return nil, fmt.Errorf(
			"`summarize count() %s` is not readable here; after count() Nimbus understands "+
				"only `by <column>`", after)
	}
	column := strings.TrimSpace(after[len(fields[0]):])
	if strings.Contains(column, ",") {
		return nil, fmt.Errorf(
			"`summarize count() by` here groups on ONE column, not %q -- grouping on several "+
				"is refused rather than silently using the first", column)
	}
	column = strings.TrimSpace(column)
	if !knownColumn(column) {
		return nil, fmt.Errorf("%q is not a column this emulator knows (known: %s)",
			column, columnNames())
	}

	// Counted in first-seen order, so the result is stable between runs.
	// A map's iteration order would reshuffle the rows on every query,
	// which looks like the data changing underneath you.
	counts := map[string]int{}
	var order []string
	for _, l := range rows {
		v := columnValue(l, column)
		if _, seen := counts[v]; !seen {
			order = append(order, v)
		}
		counts[v]++
	}
	col, _ := columnByName(column)
	out := &kqlResult{
		columns: []queryColumn{col, {Name: "count_", Type: "long"}},
		rows:    [][]any{},
	}
	for _, v := range order {
		out.rows = append(out.rows, []any{v, counts[v]})
	}
	return out, nil
}

// applyDistinct projects the named columns and drops repeats, keeping
// first-seen order for the same reason summarize does.
func applyDistinct(rows []logLine, rest string) (*kqlResult, error) {
	cols, err := applyProject(rest)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := &kqlResult{columns: cols, rows: [][]any{}}
	for _, l := range rows {
		row := rowFor(l, cols)
		key := fmt.Sprint(row)
		if seen[key] {
			continue
		}
		seen[key] = true
		out.rows = append(out.rows, row)
	}
	return out, nil
}

// splitPipeline cuts a query on `|`, trimming each stage.
func splitPipeline(query string) []string {
	parts := strings.Split(query, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.TrimSpace(p))
	}
	return out
}

// splitVerb takes the operator off the front of a stage.
func splitVerb(stage string) (verb, rest string) {
	stage = strings.TrimSpace(stage)
	if i := strings.IndexAny(stage, " \t"); i >= 0 {
		return stage[:i], strings.TrimSpace(stage[i+1:])
	}
	return stage, ""
}

// applyWhere filters on a condition, which may now be several comparisons
// joined by `and` and `or`.
//
// One comparison was the original limit, and it was the right place to
// start -- the Table `$filter` surface made the opposite choice years
// earlier in this project and the roadmap still records it as matching
// everything, which is the thing this file exists not to do. But one
// comparison is rarely what a real query has, so the join words are
// implemented properly, with `and` binding tighter than `or` as it does in
// KQL and everywhere else. Anything the splitter cannot read is still
// refused by name.
//
// Parentheses are NOT supported: a query carrying them is refused rather
// than evaluated with the wrong grouping, which is the one outcome worse
// than a refusal here.
func applyWhere(rows []logLine, expr string) ([]logLine, error) {
	cond, err := parseCondition(expr)
	if err != nil {
		return nil, err
	}
	var out []logLine
	for _, l := range rows {
		if cond(l) {
			out = append(out, l)
		}
	}
	return out, nil
}

// parseCondition builds a predicate from `a and b or c`.
//
// `or` is split first and `and` within each part, which is what makes
// `and` bind tighter -- the result reads as (a and b) or c. Getting that
// backwards silently returns the wrong rows, so a test pins it.
func parseCondition(expr string) (func(logLine) bool, error) {
	if strings.ContainsAny(expr, "()") && !strings.Contains(strings.ToLower(expr), "ago(") &&
		!strings.Contains(strings.ToLower(expr), "datetime(") {
		return nil, fmt.Errorf(
			"parentheses are not supported in `where` here, so %q is refused rather than "+
				"evaluated with a grouping Nimbus guessed at", expr)
	}

	var ors []func(logLine) bool
	for _, orPart := range splitKeyword(expr, "or") {
		var ands []func(logLine) bool
		for _, andPart := range splitKeyword(orPart, "and") {
			pred, err := parseOneComparison(andPart)
			if err != nil {
				return nil, err
			}
			ands = append(ands, pred)
		}
		if len(ands) == 0 {
			return nil, fmt.Errorf("`where` has an empty condition in %q", expr)
		}
		all := ands
		ors = append(ors, func(l logLine) bool {
			for _, p := range all {
				if !p(l) {
					return false
				}
			}
			return true
		})
	}
	if len(ors) == 0 {
		return nil, fmt.Errorf("`where` needs a condition")
	}
	any := ors
	return func(l logLine) bool {
		for _, p := range any {
			if p(l) {
				return true
			}
		}
		return false
	}, nil
}

// splitKeyword cuts an expression on a bare keyword, so a column or a
// value containing the letters is left alone.
//
// `RevisionName_s contains "android"` must not be split on the "and"
// inside "android", which is the same word-boundary rule the DynamoDB
// update parser needed for the ADD inside an attribute called ADDRESS.
func splitKeyword(expr, keyword string) []string {
	var parts []string
	rest := expr
	for {
		i := indexKeyword(rest, keyword)
		if i < 0 {
			parts = append(parts, strings.TrimSpace(rest))
			return parts
		}
		parts = append(parts, strings.TrimSpace(rest[:i]))
		rest = rest[i+len(keyword):]
	}
}

// indexKeyword finds a keyword that stands alone, ignoring case, and never
// inside a quoted string -- `Log_s contains "x and y"` is one comparison.
func indexKeyword(expr, keyword string) int {
	lower := strings.ToLower(expr)
	keyword = strings.ToLower(keyword)
	inQuote := byte(0)
	for i := 0; i < len(expr); i++ {
		c := expr[i]
		if inQuote != 0 {
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		if c == '"' || c == '\'' {
			inQuote = c
			continue
		}
		if !strings.HasPrefix(lower[i:], keyword) {
			continue
		}
		if i > 0 && !isSpaceByte(expr[i-1]) {
			continue
		}
		after := i + len(keyword)
		if after < len(expr) && !isSpaceByte(expr[after]) {
			continue
		}
		return i
	}
	return -1
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' }

// parseOneComparison turns a single comparison into a predicate.
func parseOneComparison(expr string) (func(logLine) bool, error) {
	column, op, value, err := parseComparison(expr)
	if err != nil {
		return nil, err
	}
	// A time comparison is about instants, not strings, so it is settled
	// here rather than by comparing the text -- "2026-11-18T00:00:01Z" and
	// an ago() offset are the same instant spelled two ways.
	if isTimeOperator(op) {
		want, err := parseQueryTime(value)
		if err != nil {
			return nil, err
		}
		return func(l logLine) bool {
			have, err := parseQueryTime(columnValue(l, column))
			if err != nil {
				return false
			}
			return timeHolds(op, have, want)
		}, nil
	}
	return func(l logLine) bool {
		return stringHolds(op, columnValue(l, column), value)
	}, nil
}

func isTimeOperator(op string) bool {
	switch op {
	case ">", ">=", "<", "<=":
		return true
	}
	return false
}

func timeHolds(op string, have, want time.Time) bool {
	switch op {
	case ">":
		return have.After(want)
	case ">=":
		return !have.Before(want)
	case "<":
		return have.Before(want)
	case "<=":
		return !have.After(want)
	}
	return false
}

// parseQueryTime reads the spellings a query uses for an instant:
// `ago(10m)`, `datetime(...)`, and a bare RFC 3339 timestamp, which is
// what a row's TimeGenerated holds.
//
// `ago()` is why this exists. It is in virtually every real KQL query and
// was refused outright, which made the time range docs/ROADMAP.md
// complained about unexpressible -- so a query's window was not merely
// ignored, it could not be written at all.
func parseQueryTime(v string) (time.Time, error) {
	v = strings.TrimSpace(strings.Trim(strings.TrimSpace(v), `"'`))
	lower := strings.ToLower(v)

	if strings.HasPrefix(lower, "ago(") && strings.HasSuffix(v, ")") {
		spec := strings.TrimSpace(v[len("ago(") : len(v)-1])
		d, err := parseKQLDuration(spec)
		if err != nil {
			return time.Time{}, err
		}
		return time.Now().UTC().Add(-d), nil
	}
	if strings.HasPrefix(lower, "datetime(") && strings.HasSuffix(v, ")") {
		v = strings.TrimSpace(v[len("datetime(") : len(v)-1])
		v = strings.Trim(v, `"'`)
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05.999999999Z07:00",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
		"2006-01-02",
	} {
		if t, err := time.Parse(layout, v); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf(
		"%q is not a time this emulator can read; use ago(10m), ago(2h), ago(7d) or "+
			"datetime(2026-11-18T00:00:00Z)", v)
}

// parseKQLDuration reads KQL's timespan shorthand.
//
// Deliberately not time.ParseDuration: KQL writes days as `7d`, which Go
// does not accept, and Go accepts `1m30s` and `500ms`, which KQL does not
// spell that way. Reading each other's syntax would be two wrong answers
// rather than one right one.
func parseKQLDuration(spec string) (time.Duration, error) {
	if spec == "" {
		return 0, fmt.Errorf("ago() needs a timespan, e.g. ago(10m)")
	}
	unit := spec[len(spec)-1]
	n, err := strconv.ParseFloat(strings.TrimSpace(spec[:len(spec)-1]), 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf(
			"%q is not a timespan this emulator can read; use a number and s, m, h or d, "+
				"as in ago(90s) or ago(7d)", spec)
	}
	switch unit {
	case 's':
		return time.Duration(n * float64(time.Second)), nil
	case 'm':
		return time.Duration(n * float64(time.Minute)), nil
	case 'h':
		return time.Duration(n * float64(time.Hour)), nil
	case 'd':
		return time.Duration(n * 24 * float64(time.Hour)), nil
	}
	return 0, fmt.Errorf(
		"%q has no unit this emulator knows; use s, m, h or d (KQL's other timespan "+
			"units are refused rather than approximated)", spec)
}

// stringHolds is the original single-comparison matching, unchanged.
func stringHolds(op, have, value string) bool {
	{
		match := false
		switch op {
		case "==":
			match = have == value
		case "!=":
			match = have != value
		case "contains":
			// Case-INSENSITIVE, which is what KQL's `contains` means --
			// `contains_cs` is the sensitive one, and is not implemented.
			// Worth knowing before it looks like a bug: searching an
			// nginx console for "GET" also matches "Getting".
			match = strings.Contains(strings.ToLower(have), strings.ToLower(value))
		case "!contains":
			match = !strings.Contains(strings.ToLower(have), strings.ToLower(value))
		case "startswith":
			match = strings.HasPrefix(strings.ToLower(have), strings.ToLower(value))
		case "endswith":
			match = strings.HasSuffix(strings.ToLower(have), strings.ToLower(value))
		}
		return match
	}
}

// whereOperators are the comparisons `where` understands. Ordered longest
// first, so `!contains` is found before `contains`, `==` before `=` and
// `>=` before `>`.
var whereOperators = []string{"!contains", "startswith", "endswith", "contains",
	"==", "!=", ">=", "<=", ">", "<"}

func parseComparison(expr string) (column, op, value string, err error) {
	expr = strings.TrimSpace(expr)
	for _, candidate := range whereOperators {
		// Surrounded by space, or `==` style with none: `a=="b"` is legal
		// KQL and so is `a == "b"`.
		i := strings.Index(expr, candidate)
		if i <= 0 {
			continue
		}
		column = strings.TrimSpace(expr[:i])
		value = strings.TrimSpace(expr[i+len(candidate):])
		if strings.ContainsAny(column, " \t") {
			// Something between the column and the operator -- a
			// function call, a second term. Not readable here.
			break
		}
		if !knownColumn(column) {
			return "", "", "", fmt.Errorf(
				"%q is not a column this emulator knows (known: %s)", column, columnNames())
		}
		return column, candidate, strings.Trim(value, `"'`), nil
	}
	return "", "", "", fmt.Errorf(
		"this emulator cannot read the filter %q. `where` supports one comparison on one "+
			"column, with ==, !=, contains, !contains, startswith or endswith -- and nothing "+
			"else, rather than guessing at the rest", expr)
}

// applyProject narrows and reorders the columns.
func applyProject(rest string) ([]queryColumn, error) {
	var out []queryColumn
	for _, name := range strings.Split(rest, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if strings.ContainsAny(name, "()=") {
			return nil, fmt.Errorf(
				"`project` can name columns and nothing else here; %q looks like an "+
					"expression, which is refused rather than guessed at", name)
		}
		col, ok := columnByName(name)
		if !ok {
			return nil, fmt.Errorf("%q is not a column this emulator knows (known: %s)",
				name, columnNames())
		}
		out = append(out, col)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("`project` needs at least one column")
	}
	return out, nil
}

func knownColumn(name string) bool {
	_, ok := columnByName(name)
	return ok
}

func columnByName(name string) (queryColumn, bool) {
	for _, c := range logColumns {
		if strings.EqualFold(c.Name, name) {
			return c, true
		}
	}
	return queryColumn{}, false
}

func columnNames() string {
	var names []string
	for _, c := range logColumns {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

func columnValue(l logLine, column string) string {
	switch strings.ToLower(column) {
	case "timegenerated":
		return l.Time
	case "containerappname_s":
		return l.App
	case "revisionname_s":
		return l.Revision
	case "stream_s":
		return l.Stream
	case "log_s":
		return l.Message
	}
	return ""
}

func rowFor(l logLine, columns []queryColumn) []any {
	row := make([]any, 0, len(columns))
	for _, c := range columns {
		row = append(row, columnValue(l, c.Name))
	}
	return row
}

// --- the endpoint ---------------------------------------------------------

// LogAnalyticsPrefix is where the query API answers, matching the
// `logAnalyticsResourceId` the metadata document advertises.
const LogAnalyticsPrefix = "/loganalytics/"

// serveLogAnalytics answers a query against a workspace.
func (s *Server) serveLogAnalytics(w http.ResponseWriter, r *http.Request) string {
	// /loganalytics/v1/workspaces/{customerId}/query
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, LogAnalyticsPrefix), "/")
	seg := strings.Split(rest, "/")
	if len(seg) != 4 || !eq(seg[0], "v1") || !eq(seg[1], "workspaces") || !eq(seg[3], "query") {
		writeError(w, 404, "PathNotFound",
			"Nimbus serves the query API at %sv1/workspaces/{customerId}/query; %s is not that.",
			LogAnalyticsPrefix, r.URL.Path)
		return "log analytics: unknown path " + r.URL.Path
	}
	customerID := seg[2]

	// A workspace nobody created is a 404, and that matters: a typo in a
	// customer id would otherwise read as "your app has logged nothing",
	// which is the same confusion the S3 sub-resources and Key Vault's
	// certificate contacts are careful about.
	if _, ok := s.workspaceCustomerIDs()[strings.ToLower(customerID)]; !ok {
		writeError(w, 404, "WorkspaceNotFound",
			"No Log Analytics workspace has the customer id %q. Create one, or check the id "+
				"in the workspace's properties.customerId.", customerID)
		return "log analytics: no workspace " + customerID
	}

	var req struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "BadRequest", "reading the query: %v", err)
		return "log analytics: unreadable body"
	}
	if strings.TrimSpace(req.Query) == "" {
		writeError(w, 400, "BadRequest", "the request has no query")
		return "log analytics: empty query"
	}

	table, err := runKQL(req.Query, s.logs.all(customerID))
	if err != nil {
		// 400 and not 500: the query is the client's, and the message
		// names what could not be evaluated so it can be rewritten.
		writeError(w, 400, "BadArgumentError", "%v", err)
		return "log analytics: refused a query -- " + err.Error()
	}
	writeJSON(w, 200, map[string]any{"tables": []any{table}})
	return ""
}
