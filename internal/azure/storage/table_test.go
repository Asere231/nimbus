package storage

import "testing"

func put(t *testing.T, tbl *Table, pk, rk string, props map[string]any) {
	t.Helper()
	if props == nil {
		props = map[string]any{}
	}
	tbl.Put(&Entity{PartitionKey: pk, RowKey: rk, Properties: props})
}

func names(es []*Entity) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.PartitionKey + "/" + e.RowKey
	}
	return out
}

func TestTableEntityCRUD(t *testing.T) {
	a := NewStore().CreateAccount("st")
	tbl, created := a.CreateTable("people")
	if !created {
		t.Fatal("table should have been created")
	}
	if _, created := a.CreateTable("people"); created {
		t.Error("creating an existing table must report not-created")
	}

	put(t, tbl, "eng", "ada", map[string]any{"role": "founder", "active": true})
	e := tbl.Entity("eng", "ada")
	if e == nil || e.Properties["role"] != "founder" {
		t.Fatalf("entity not stored: %+v", e)
	}
	if e.ETag == "" || e.Timestamp.IsZero() {
		t.Error("entity should carry an ETag and Timestamp")
	}

	// Merge updates only what it names.
	tbl.Merge(&Entity{PartitionKey: "eng", RowKey: "ada", Properties: map[string]any{"role": "cto"}})
	e = tbl.Entity("eng", "ada")
	if e.Properties["role"] != "cto" {
		t.Error("merge did not update the named property")
	}
	if e.Properties["active"] != true {
		t.Error("merge dropped a property it did not name")
	}

	// Put replaces wholesale.
	put(t, tbl, "eng", "ada", map[string]any{"role": "ceo"})
	if _, still := tbl.Entity("eng", "ada").Properties["active"]; still {
		t.Error("put should replace the entity, not merge into it")
	}

	if !tbl.DeleteEntity("eng", "ada") {
		t.Error("delete should succeed")
	}
	if tbl.DeleteEntity("eng", "ada") {
		t.Error("deleting a missing entity should report false")
	}
	if !a.DeleteTable("people") || a.DeleteTable("people") {
		t.Error("table delete semantics are wrong")
	}
}

func TestTableODataFilter(t *testing.T) {
	a := NewStore().CreateAccount("st")
	tbl, _ := a.CreateTable("t")
	put(t, tbl, "eng", "ada", map[string]any{"role": "cto", "level": float64(5)})
	put(t, tbl, "eng", "bob", map[string]any{"role": "dev", "level": float64(3)})
	put(t, tbl, "ops", "cat", map[string]any{"role": "dev", "level": float64(3)})

	for _, tc := range []struct {
		filter string
		want   []string
	}{
		{"", []string{"eng/ada", "eng/bob", "ops/cat"}},
		{"PartitionKey eq 'eng'", []string{"eng/ada", "eng/bob"}},
		{"RowKey eq 'cat'", []string{"ops/cat"}},
		{"role eq 'dev'", []string{"eng/bob", "ops/cat"}},
		{"PartitionKey eq 'eng' and role eq 'dev'", []string{"eng/bob"}},
		{"PartitionKey eq 'ops' and role eq 'dev' and RowKey eq 'cat'", []string{"ops/cat"}},
		{"PartitionKey eq 'nobody'", []string{}},
		// Results come back in (PartitionKey, RowKey) order, always.
		{"role eq 'dev' and level eq 3", []string{"eng/bob", "ops/cat"}},
	} {
		got := names(tbl.Query(tc.filter))
		if len(got) != len(tc.want) {
			t.Errorf("filter %q returned %v, want %v", tc.filter, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("filter %q returned %v, want %v", tc.filter, got, tc.want)
				break
			}
		}
	}
}

// An operator we do not support must not silently exclude rows: returning too
// much is visible in a test, returning wrongly-empty results is not.
func TestUnsupportedFilterOperatorDoesNotSilentlyExclude(t *testing.T) {
	a := NewStore().CreateAccount("st")
	tbl, _ := a.CreateTable("t")
	put(t, tbl, "p", "a", map[string]any{"level": float64(5)})

	if n := len(tbl.Query("level gt 1")); n != 1 {
		t.Errorf("an unsupported operator returned %d rows; it must not exclude on a guess", n)
	}
	// A supported clause alongside an unsupported one still applies.
	if n := len(tbl.Query("PartitionKey eq 'nope' and level gt 1")); n != 0 {
		t.Errorf("the supported half of the filter was not applied: %d rows", n)
	}
}

func TestParseEntityPath(t *testing.T) {
	for _, tc := range []struct{ in, table, pk, rk string }{
		{"people(PartitionKey='eng',RowKey='ada')", "people", "eng", "ada"},
		{"people()", "people", "", ""},
		{"people", "people", "", ""},
		{"t(PartitionKey='a b',RowKey='c,d')", "t", "a b", "c"}, // commas in keys are not escaped by clients we target
	} {
		table, pk, rk := parseEntityPath(tc.in)
		if table != tc.table || pk != tc.pk || rk != tc.rk {
			t.Errorf("parseEntityPath(%q) = (%q,%q,%q), want (%q,%q,%q)",
				tc.in, table, pk, rk, tc.table, tc.pk, tc.rk)
		}
	}
}

func TestTablesPath(t *testing.T) {
	for _, tc := range []struct {
		in    string
		name  string
		isTbl bool
	}{
		{"Tables", "", true},
		{"Tables('people')", "people", true},
		{"people(PartitionKey='a',RowKey='b')", "", false},
		{"people", "", false},
	} {
		name, ok := tablesPath(tc.in)
		if ok != tc.isTbl || name != tc.name {
			t.Errorf("tablesPath(%q) = (%q,%v), want (%q,%v)", tc.in, name, ok, tc.name, tc.isTbl)
		}
	}
}
