package azure

import (
	"strings"
	"testing"
	"time"
)

// The KQL subset, widened.
//
// The design does not change: what is implemented is implemented, and
// everything else is REFUSED AND NAMED, because both other answers are
// worse. Treating an unparsed operator as a no-op returns every row (the
// DynamoDB FilterExpression trap), and returning no rows is
// indistinguishable from "nothing has been logged yet", which is the
// ordinary state of a workspace somebody is looking at for the first time.
//
// What widens is the list. These are the operators a person actually
// reaches for after `take`: a filter with two conditions, a sort, a count
// by something, and a time range -- `ago()` above all, which is in
// virtually every real query and was refused outright.

func sampleRows() []logLine {
	return []logLine{
		{Time: "2026-11-18T00:00:01Z", Table: tableContainerAppConsole,
			App: "web", Stream: "stdout", Message: "GET / 200"},
		{Time: "2026-11-18T00:00:02Z", Table: tableContainerAppConsole,
			App: "web", Stream: "stderr", Message: "GET /missing 404"},
		{Time: "2026-11-18T00:00:03Z", Table: tableContainerAppConsole,
			App: "worker", Stream: "stdout", Message: "job done"},
	}
}

func mustQuery(t *testing.T, query string, rows []logLine) queryTable {
	t.Helper()
	table, err := runKQL(query, rows)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return table
}

// --- and / or --------------------------------------------------------------

// `where a and b` used to be refused: applyWhere read exactly one
// comparison. One condition is rarely what a real query has.
func TestWhereSupportsAnd(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+
		` | where ContainerAppName_s == "web" and Stream_s == "stderr"`, sampleRows())
	if len(table.Rows) != 1 {
		t.Fatalf("expected the one stderr row from web, got %d: %v", len(table.Rows), table.Rows)
	}
	if !strings.Contains(table.Rows[0][4].(string), "404") {
		t.Errorf("the wrong row matched: %v", table.Rows[0])
	}
}

func TestWhereSupportsOr(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+
		` | where Stream_s == "stderr" or ContainerAppName_s == "worker"`, sampleRows())
	if len(table.Rows) != 2 {
		t.Errorf("expected the stderr row and the worker row, got %d", len(table.Rows))
	}
}

// `and` binds tighter than `or`, as it does in KQL and in every other
// language. Getting this backwards silently returns the wrong rows, which
// is why it is pinned rather than assumed.
func TestAndBindsTighterThanOr(t *testing.T) {
	// stdout AND worker  OR  stderr  =>  the worker row and the stderr row.
	// If `or` bound tighter it would read stdout AND (worker OR stderr),
	// which matches nothing.
	table := mustQuery(t, tableContainerAppConsole+
		` | where Stream_s == "stdout" and ContainerAppName_s == "worker" or Stream_s == "stderr"`,
		sampleRows())
	if len(table.Rows) != 2 {
		t.Errorf("expected 2 rows with `and` binding tighter, got %d: %v",
			len(table.Rows), table.Rows)
	}
}

// --- order by --------------------------------------------------------------

func TestOrderByDescendingReversesTheRows(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+` | order by TimeGenerated desc`, sampleRows())
	if len(table.Rows) != 3 {
		t.Fatalf("expected every row, got %d", len(table.Rows))
	}
	if table.Rows[0][0] != "2026-11-18T00:00:03Z" {
		t.Errorf("descending order did not put the newest first: %v", table.Rows[0])
	}
}

// `sort by` is the same operator under its other name, and KQL defaults to
// DESCENDING -- which is the opposite of what anyone expects from the word
// and is worth pinning because a silently reversed log is hard to notice.
func TestSortByIsAnAliasAndDefaultsToDescending(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+` | sort by TimeGenerated`, sampleRows())
	if table.Rows[0][0] != "2026-11-18T00:00:03Z" {
		t.Errorf("KQL's sort defaults to descending; got %v first", table.Rows[0][0])
	}
	asc := mustQuery(t, tableContainerAppConsole+` | order by TimeGenerated asc`, sampleRows())
	if asc.Rows[0][0] != "2026-11-18T00:00:01Z" {
		t.Errorf("asc did not put the oldest first: %v", asc.Rows[0])
	}
}

// --- summarize -------------------------------------------------------------

func TestSummarizeCountByGroupsRows(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+
		` | summarize count() by ContainerAppName_s`, sampleRows())
	if len(table.Columns) != 2 {
		t.Fatalf("expected the grouping column and a count: %v", table.Columns)
	}
	if table.Columns[0].Name != "ContainerAppName_s" || table.Columns[1].Name != "count_" {
		t.Errorf("wrong columns: %v", table.Columns)
	}
	got := map[string]any{}
	for _, row := range table.Rows {
		got[row[0].(string)] = row[1]
	}
	if got["web"] != 2 || got["worker"] != 1 {
		t.Errorf("wrong counts: %v", got)
	}
}

// A bare `summarize count()` with no `by` is the whole-table count.
func TestSummarizeCountWithoutByCountsEverything(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+` | summarize count()`, sampleRows())
	if len(table.Rows) != 1 || table.Rows[0][0] != 3 {
		t.Errorf("expected a single row of 3, got %v", table.Rows)
	}
}

// Any other aggregation is refused BY NAME rather than approximated with a
// count, which would be a wrong number presented as a right one.
func TestOtherAggregationsAreRefusedAndNamed(t *testing.T) {
	for _, agg := range []string{"avg(Log_s)", "sum(Log_s)", "dcount(Log_s)", "max(TimeGenerated)"} {
		_, err := runKQL(tableContainerAppConsole+" | summarize "+agg, sampleRows())
		if err == nil {
			t.Errorf("summarize %s was answered rather than refused", agg)
			continue
		}
		if !strings.Contains(err.Error(), agg) && !strings.Contains(err.Error(), "count()") {
			t.Errorf("the refusal of %s names neither it nor what IS supported: %v", agg, err)
		}
	}
}

// --- distinct and top ------------------------------------------------------

func TestDistinctNarrowsToUniqueValues(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+` | distinct ContainerAppName_s`, sampleRows())
	if len(table.Rows) != 2 {
		t.Fatalf("expected two app names, got %d: %v", len(table.Rows), table.Rows)
	}
	if len(table.Columns) != 1 || table.Columns[0].Name != "ContainerAppName_s" {
		t.Errorf("distinct should project just the named column: %v", table.Columns)
	}
}

func TestTopTakesTheFirstRowsInOrder(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+` | top 2 by TimeGenerated desc`, sampleRows())
	if len(table.Rows) != 2 {
		t.Fatalf("top 2 returned %d rows", len(table.Rows))
	}
	if table.Rows[0][0] != "2026-11-18T00:00:03Z" {
		t.Errorf("top ... desc did not start from the newest: %v", table.Rows[0])
	}
}

// --- ago() -----------------------------------------------------------------

// `ago()` is in virtually every real query and was refused outright, which
// made the time range the roadmap complained about unexpressible. Note the
// rows have to be recent for this to mean anything, so they are built from
// the clock rather than from the fixed sample.
func TestAgoNarrowsToATimeWindow(t *testing.T) {
	now := time.Now().UTC()
	rows := []logLine{
		{Time: now.Add(-2 * time.Hour).Format(time.RFC3339), Table: tableContainerAppConsole,
			App: "web", Message: "old"},
		{Time: now.Add(-1 * time.Minute).Format(time.RFC3339), Table: tableContainerAppConsole,
			App: "web", Message: "recent"},
	}

	table := mustQuery(t, tableContainerAppConsole+` | where TimeGenerated > ago(10m)`, rows)
	if len(table.Rows) != 1 {
		t.Fatalf("expected only the recent row, got %d: %v", len(table.Rows), table.Rows)
	}
	if table.Rows[0][4] != "recent" {
		t.Errorf("the wrong row survived the window: %v", table.Rows[0])
	}

	// And a window wide enough for both.
	wide := mustQuery(t, tableContainerAppConsole+` | where TimeGenerated > ago(1d)`, rows)
	if len(wide.Rows) != 2 {
		t.Errorf("a one-day window should hold both rows, got %d", len(wide.Rows))
	}
}

func TestAgoUnitsAreReadable(t *testing.T) {
	for _, spec := range []string{"30s", "5m", "2h", "7d"} {
		if _, err := runKQL(tableContainerAppConsole+
			" | where TimeGenerated > ago("+spec+")", sampleRows()); err != nil {
			t.Errorf("ago(%s) was refused: %v", spec, err)
		}
	}
	if _, err := runKQL(tableContainerAppConsole+
		" | where TimeGenerated > ago(soon)", sampleRows()); err == nil {
		t.Error("ago(soon) should be refused rather than guessed at")
	}
}

// A datetime literal works too, for the query that names an instant.
func TestATimeComparisonAgainstALiteral(t *testing.T) {
	table := mustQuery(t, tableContainerAppConsole+
		` | where TimeGenerated > datetime(2026-11-18T00:00:01Z)`, sampleRows())
	if len(table.Rows) != 2 {
		t.Errorf("expected the two rows after the first, got %d", len(table.Rows))
	}
}

// --- what still refuses ----------------------------------------------------

// The discipline is unchanged: an operator this does not implement is
// refused and named, and the message says what IS available.
func TestAnUnsupportedOperatorStillRefusesAndSaysWhatWorks(t *testing.T) {
	for _, stage := range []string{"extend x = 1", "join kind=inner Other on X", "mv-expand Log_s",
		"render timechart", "parse Log_s with *"} {
		_, err := runKQL(tableContainerAppConsole+" | "+stage, sampleRows())
		if err == nil {
			t.Errorf("%q was answered rather than refused", stage)
			continue
		}
		verb := strings.Fields(stage)[0]
		if !strings.Contains(err.Error(), verb) {
			t.Errorf("the refusal of %q does not name it: %v", stage, err)
		}
		if !strings.Contains(err.Error(), "summarize") {
			t.Errorf("the refusal of %q does not list what is supported: %v", stage, err)
		}
	}
}

// And the refusal message lists every operator that IS implemented, read
// from the same constant the evaluator switches on -- so the two cannot
// drift apart, which is the failure this repo keeps finding in prose.
func TestTheSupportedListNamesEveryImplementedOperator(t *testing.T) {
	for _, verb := range []string{"where", "project", "take", "limit", "count",
		"summarize", "order by", "sort by", "distinct", "top"} {
		if !strings.Contains(supportedKQL, verb) {
			t.Errorf("%q is implemented but missing from the supported list shown to users", verb)
		}
	}
}
