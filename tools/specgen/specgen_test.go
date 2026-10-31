package main

import (
	"reflect"
	"testing"
)

// The fixture under testdata mirrors the shape every Azure spec has: a
// resource whose allOf reaches into common-types, properties flattened behind
// a $ref, readOnly fields, spec defaults, and a nested object that itself
// lives in the other file. Running the walker over it needs no network.
func fixtureEntry() entry {
	return entry{
		Type:       "Microsoft.Widget/widgets",
		APIVersion: "2024-01-01",
		Spec:       "specification/widget/resource-manager/Microsoft.Widget/stable/2024-01-01/widget.json",
		Path:       "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Widget/widgets/{widgetName}",
	}
}

func fixtureLoader() *loader { return &loader{dir: "testdata", offline: true} }

func TestDeriveFixture(t *testing.T) {
	got, err := derive(fixtureLoader(), fixtureEntry())
	if err != nil {
		t.Fatal(err)
	}
	want := []field{
		{Path: "etag", Kind: "string", ReadOnly: true},
		{Path: "properties.colour", Kind: "string", Default: "blue"},
		{Path: "properties.enabled", Kind: "boolean", Default: true},
		{Path: "properties.owner.principalId", Kind: "string", ReadOnly: true},
		{Path: "properties.owner.tenantId", Kind: "string", ReadOnly: true},
		{Path: "properties.provisioningState", Kind: "string", ReadOnly: true},
		{Path: "properties.resourceGuid", Kind: "string", ReadOnly: true},
		{Path: "properties.sku.tier", Kind: "string", ReadOnly: true},
		{Path: "properties.sockets", Kind: "array", ReadOnly: true},
		{Path: "properties.spin", Kind: "number", Default: float64(3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("derived fields differ\ngot:  %+v\nwant: %+v", got, want)
	}
}

// Properties inherited through an allOf that crosses into another file must
// not make the resource's OWN properties resolve against that other file.
// Getting this wrong produces zero fields and no error at all, so it is
// asserted directly rather than left to the golden comparison.
func TestCrossFileAllOfKeepsEachPropertysOwnDocument(t *testing.T) {
	l := fixtureLoader()
	e := fixtureEntry()
	doc, err := l.load(e.Spec)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := putSchema(doc, e.Path)
	if err != nil {
		t.Fatal(err)
	}
	w := &walker{loader: l, seen: map[string]bool{}}
	props := w.properties(node{schema, doc})

	// "properties" is declared in widget.json and must resolve there...
	own, ok := props["properties"]
	if !ok {
		t.Fatal("the resource has no properties property")
	}
	if got := own.doc.path; got != e.Spec {
		t.Errorf("properties came from %s, want %s", got, e.Spec)
	}
	if inner := w.properties(own); len(inner) == 0 {
		t.Error("WidgetProperties resolved to nothing: the $ref was followed in the wrong file")
	}
	// ...while "location", inherited from TrackedResource, comes from the other.
	loc, ok := props["location"]
	if !ok {
		t.Fatal("location was not inherited through allOf")
	}
	if got := loc.doc.path; got != "specification/common-types/v1/types.json" {
		t.Errorf("location came from %s, want the common-types file", got)
	}
}

// Both spellings of "the server owns this field" must be recognised: some
// resource providers use readOnly, others x-ms-mutability: [read].
func TestIsReadOnly(t *testing.T) {
	cases := []struct {
		name   string
		schema map[string]any
		want   bool
	}{
		{"readOnly", map[string]any{"readOnly": true}, true},
		{"writable", map[string]any{"readOnly": false}, false},
		{"plain", map[string]any{"type": "string"}, false},
		{"mutability read", map[string]any{"x-ms-mutability": []any{"read"}}, true},
		{"mutability read+create", map[string]any{"x-ms-mutability": []any{"read", "create"}}, false},
	}
	for _, c := range cases {
		if got := isReadOnly(c.schema); got != c.want {
			t.Errorf("%s: isReadOnly = %v, want %v", c.name, got, c.want)
		}
	}
}

// A cycle (A refs B refs A) must terminate rather than hang, which real specs
// contain: a subnet references an NSG which lists its subnets.
func TestCyclicRefTerminates(t *testing.T) {
	doc := &document{path: "x.json", root: map[string]any{
		"definitions": map[string]any{
			"A": map[string]any{"type": "object", "properties": map[string]any{
				"b": map[string]any{"$ref": "#/definitions/B"},
			}},
			"B": map[string]any{"type": "object", "properties": map[string]any{
				"a":  map[string]any{"$ref": "#/definitions/A"},
				"id": map[string]any{"type": "string", "readOnly": true},
			}},
		},
	}}
	l := &loader{dir: "testdata", offline: true, docs: map[string]*document{"x.json": doc}}
	w := &walker{loader: l, seen: map[string]bool{}}
	start, _, err := l.resolve("#/definitions/A", doc)
	if err != nil {
		t.Fatal(err)
	}
	fields := w.walk(node{start, doc}, "", 0)
	if len(fields) == 0 {
		t.Fatal("expected to reach b.id through the cycle")
	}
	for _, f := range fields {
		if f.Path == "b.id" {
			return
		}
	}
	t.Errorf("b.id not found, got %+v", fields)
}
