package azure

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The KQL subset, and the refusal that bounds it.
//
// KQL is a language and this is a pipeline subset. What is worth testing is
// not the operators that work -- those are a few lines each -- but the
// boundary: an operator Nimbus cannot evaluate must be REFUSED AND NAMED,
// never silently matching everything and never silently matching nothing.
//
// "Matches everything" is the DynamoDB FilterExpression trap, which this
// project has paid for once. "Matches nothing" is subtler and just as bad
// here: an empty result is indistinguishable from "no logs have arrived
// yet", which is the most likely state of a workspace somebody is looking
// at for the first time.

func TestTheKQLSubsetFiltersProjectsAndLimits(t *testing.T) {
	rows := []logLine{
		{Time: "2026-11-11T10:00:00Z", Table: "ContainerAppConsoleLogs", App: "web", Message: "started"},
		{Time: "2026-11-11T10:00:01Z", Table: "ContainerAppConsoleLogs", App: "web", Message: "GET /"},
		{Time: "2026-11-11T10:00:02Z", Table: "ContainerAppConsoleLogs", App: "api", Message: "GET /health"},
	}

	cases := []struct {
		query     string
		wantRows  int
		wantCols  []string
		wantFirst string // the Log_s of the first row, when it matters
	}{
		{query: "ContainerAppConsoleLogs", wantRows: 3},
		{query: "ContainerAppConsoleLogs | take 2", wantRows: 2},
		{query: "ContainerAppConsoleLogs | limit 1", wantRows: 1},

		// where, on each supported operator
		{query: `ContainerAppConsoleLogs | where ContainerAppName_s == "api"`, wantRows: 1, wantFirst: "GET /health"},
		{query: `ContainerAppConsoleLogs | where ContainerAppName_s != "api"`, wantRows: 2},
		// `contains` is CASE-INSENSITIVE in KQL -- `contains_cs` is the
		// sensitive one -- so this is deliberate rather than sloppy. It
		// looks like a bug in real output: querying for "GET" against an
		// nginx console also matches "Getting" and "getrlimit".
		{query: `ContainerAppConsoleLogs | where Log_s contains "GET"`, wantRows: 2},
		{query: `ContainerAppConsoleLogs | where Log_s contains "get"`, wantRows: 2},
		{query: `ContainerAppConsoleLogs | where Log_s startswith "get"`, wantRows: 2},
		{query: `ContainerAppConsoleLogs | where Log_s startswith "GET /h"`, wantRows: 1},

		// project narrows the columns, in the order asked for
		{query: "ContainerAppConsoleLogs | project Log_s", wantRows: 3, wantCols: []string{"Log_s"}},
		{query: "ContainerAppConsoleLogs | project Log_s, ContainerAppName_s", wantRows: 3,
			wantCols: []string{"Log_s", "ContainerAppName_s"}},

		// count replaces the result with a single number
		{query: "ContainerAppConsoleLogs | count", wantRows: 1, wantCols: []string{"Count"}},

		// a pipeline, which is the point of the shape
		{query: `ContainerAppConsoleLogs | where ContainerAppName_s == "web" | project Log_s | take 1`,
			wantRows: 1, wantCols: []string{"Log_s"}, wantFirst: "started"},
	}

	for _, c := range cases {
		table, err := runKQL(c.query, rows)
		if err != nil {
			t.Errorf("%s: %v", c.query, err)
			continue
		}
		if len(table.Rows) != c.wantRows {
			t.Errorf("%s: %d rows, want %d", c.query, len(table.Rows), c.wantRows)
		}
		if c.wantCols != nil {
			var got []string
			for _, col := range table.Columns {
				got = append(got, col.Name)
			}
			if strings.Join(got, ",") != strings.Join(c.wantCols, ",") {
				t.Errorf("%s: columns %v, want %v", c.query, got, c.wantCols)
			}
		}
		if c.wantFirst != "" && len(table.Rows) > 0 {
			if got := logValue(table, 0, "Log_s"); got != c.wantFirst {
				t.Errorf("%s: first row %q, want %q", c.query, got, c.wantFirst)
			}
		}
	}
}

// The boundary. Each of these must come back as an error NAMING the thing
// that could not be evaluated -- not an empty table, which reads as "no
// logs yet", and not every row, which is the FilterExpression trap.
func TestAnUnsupportedKQLConstructIsRefusedAndNamed(t *testing.T) {
	rows := []logLine{{Time: "2026-11-11T10:00:00Z", Table: "ContainerAppConsoleLogs", Message: "x"}}

	cases := []struct{ query, name string }{
		{"ContainerAppConsoleLogs | summarize count() by Log_s", "summarize"},
		{"ContainerAppConsoleLogs | extend n = 1", "extend"},
		{"ContainerAppConsoleLogs | order by TimeGenerated desc", "order"},
		{"ContainerAppConsoleLogs | join kind=inner Other on Id", "join"},
		{"ContainerAppConsoleLogs | mv-expand Log_s", "mv-expand"},
		{`ContainerAppConsoleLogs | where Log_s matches regex "a.*"`, "matches"},
		{"ContainerAppConsoleLogs | where isnotempty(Log_s)", "isnotempty"},
	}
	for _, c := range cases {
		table, err := runKQL(c.query, rows)
		if err == nil {
			t.Errorf("%s was not refused (returned %d rows)", c.query, len(table.Rows))
			continue
		}
		if !strings.Contains(err.Error(), c.name) {
			t.Errorf("the refusal of %q does not name %q: %v", c.query, c.name, err)
		}
	}
}

// A table nobody has written to is an EMPTY result, not an error: a
// workspace that exists and has had nothing sent to it is the ordinary
// state of a new one, and the two cases have to be distinguishable.
func TestAnUnknownTableIsEmptyRatherThanAnError(t *testing.T) {
	table, err := runKQL("Heartbeat | take 5", []logLine{
		{Time: "2026-11-11T10:00:00Z", Table: "ContainerAppConsoleLogs", Message: "x"},
	})
	if err != nil {
		t.Fatalf("an unwritten table errored: %v", err)
	}
	if len(table.Rows) != 0 {
		t.Errorf("%d rows for a table nothing has written to", len(table.Rows))
	}
}

// A query with no table name at all cannot be answered, and saying so is
// better than guessing which table was meant.
func TestAQueryWithNoTableIsRefused(t *testing.T) {
	if _, err := runKQL("| take 1", nil); err == nil {
		t.Error("a query with no table name was accepted")
	}
}

// --- the endpoint ---------------------------------------------------------

// The path and the body are measured, not guessed: the `log-analytics` az
// extension POSTs {"query": "..."} to
// {logAnalyticsResourceId}/v1/workspaces/{customerId}/query, and it finds
// that base URL in the cloud metadata document Nimbus already serves.
func TestTheQueryEndpointAnswersTheShapeTheCLIExpects(t *testing.T) {
	srv, _ := newAppServer(t)
	s := serverOf(t, srv)

	mustPut(t, srv, rgPath("rg"), map[string]any{"location": "eastus"})
	code, ws := do(t, srv, http.MethodPut,
		"/subscriptions/"+DefaultSubscriptionID+
			"/resourceGroups/rg/providers/Microsoft.OperationalInsights/workspaces/law",
		map[string]any{"location": "eastus", "properties": map[string]any{}})
	if code >= 300 {
		t.Fatalf("creating the workspace: %d %v", code, ws)
	}
	props, _ := ws["properties"].(map[string]any)
	customerID := str(props["customerId"])
	if customerID == "" {
		t.Fatal("the workspace has no customerId")
	}

	s.logs.append(customerID, logLine{
		Time: "2026-11-11T10:00:00Z", Table: "ContainerAppConsoleLogs",
		App: "web", Message: "hello from nimbus",
	})

	body := `{"query":"ContainerAppConsoleLogs | take 10"}`
	rec := postRaw(t, srv, "/loganalytics/v1/workspaces/"+customerID+"/query", body)
	if rec.Code != 200 {
		t.Fatalf("query = %d %s", rec.Code, rec.Body)
	}
	out := rec.Body.String()
	// The document the CLI parses: tables[].name/columns[]/rows[][].
	for _, want := range []string{`"tables"`, `"PrimaryResult"`, `"columns"`, `"rows"`, "hello from nimbus"} {
		if !strings.Contains(out, want) {
			t.Errorf("the answer is missing %q: %s", want, out)
		}
	}
}

// A workspace nobody created is a 404, not an empty result. The two are
// different questions and the difference is the whole answer: a typo in a
// customer id otherwise reads as "your app has logged nothing".
func TestQueryingAnUnknownWorkspaceIs404(t *testing.T) {
	srv, _ := newAppServer(t)
	rec := postRaw(t, srv, "/loganalytics/v1/workspaces/"+
		"00000000-0000-0000-0000-00000000dead/query", `{"query":"Heartbeat"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("query of an unknown workspace = %d, want 404\n%s", rec.Code, rec.Body)
	}
}

// logValue reads one named column out of a result row, by its position in
// the table's own columns -- not "the first string in the row", which
// finds TimeGenerated.
func logValue(table queryTable, row int, column string) string {
	for i, c := range table.Columns {
		if c.Name == column && row < len(table.Rows) && i < len(table.Rows[row]) {
			s, _ := table.Rows[row][i].(string)
			return s
		}
	}
	return ""
}

// postRaw sends a body with no api-version, which the query data plane
// does not take -- unlike every ARM path, where `do` appends one.
func postRaw(t *testing.T, srv *httptest.Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	serverOf(t, srv).ServeHTTP(rec, req)
	return rec
}
