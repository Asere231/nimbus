package azure

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
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
// beside the implementation so the two cannot drift.
const supportedKQL = "where, project, take, limit, count"

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
	counted := false

	for _, stage := range stages[1:] {
		verb, rest := splitVerb(stage)
		switch strings.ToLower(verb) {
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
			n, err := strconv.Atoi(strings.TrimSpace(rest))
			if err != nil || n < 0 {
				return queryTable{}, fmt.Errorf("%q needs a row count, e.g. `%s 10`", verb, verb)
			}
			if n < len(rows) {
				rows = rows[:n]
			}
		case "count":
			counted = true
		default:
			return queryTable{}, fmt.Errorf(
				"the KQL operator %q is not supported by Nimbus, so this query is refused "+
					"rather than answered with a guess (supported: %s)", verb, supportedKQL)
		}
	}

	if counted {
		return queryTable{
			Name:    "PrimaryResult",
			Columns: []queryColumn{{Name: "Count", Type: "long"}},
			Rows:    [][]any{{len(rows)}},
		}, nil
	}

	out := queryTable{Name: "PrimaryResult", Columns: columns, Rows: [][]any{}}
	for _, l := range rows {
		out.Rows = append(out.Rows, rowFor(l, columns))
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

// applyWhere filters on one comparison.
//
// Deliberately one: `and`/`or` are not supported, and a query carrying
// them is refused by name rather than half-evaluated. The Table `$filter`
// surface made the opposite choice years earlier in this project and the
// roadmap records it as matching everything -- which is the thing this
// file exists not to do.
func applyWhere(rows []logLine, expr string) ([]logLine, error) {
	column, op, value, err := parseComparison(expr)
	if err != nil {
		return nil, err
	}
	var out []logLine
	for _, l := range rows {
		have := columnValue(l, column)
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
		if match {
			out = append(out, l)
		}
	}
	return out, nil
}

// whereOperators are the comparisons `where` understands. Ordered longest
// first, so `!contains` is found before `contains` and `==` before `=`.
var whereOperators = []string{"!contains", "startswith", "endswith", "contains", "==", "!="}

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
