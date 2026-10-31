package azure

import (
	"encoding/json"
	"reflect"
	"testing"
)

// The exact shape azure-cli produces: everything serialised normally, then the
// closing braces chopped off and the template spliced back in under a bare
// key. Taken from resource/custom.py, which builds it as
//
//	partial_request[:-2] + ", template:" + template + "}}"
const azDeploymentBody = `{"properties": {"mode": "Incremental", "parameters": {}, template:{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "resources": []
}}}`

func TestReadBodyAcceptsAzDeploymentJSONC(t *testing.T) {
	var want map[string]any
	fixed := quoteBareKeys([]byte(azDeploymentBody))
	if err := json.Unmarshal(fixed, &want); err != nil {
		t.Fatalf("the repaired body is still not JSON: %v\n%s", err, fixed)
	}
	props, _ := want["properties"].(map[string]any)
	if props["mode"] != "Incremental" {
		t.Errorf("mode = %v", props["mode"])
	}
	tmpl, ok := props["template"].(map[string]any)
	if !ok {
		t.Fatalf("template is %T, want an object", props["template"])
	}
	if tmpl["contentVersion"] != "1.0.0.0" {
		t.Errorf("contentVersion = %v", tmpl["contentVersion"])
	}
}

// Valid JSON must come through byte-identical in meaning, and the repair must
// not touch anything inside a string -- a template is full of ARM expressions
// containing braces, colons and commas.
func TestQuoteBareKeysLeavesValidJSONAlone(t *testing.T) {
	cases := []string{
		`{"a":1,"b":[1,2,{"c":true}],"d":null}`,
		`{"expr":"[concat('a', variables('b'))]","n":{"deep":{"x":false}}}`,
		`{"tricky":"a string with {braces}, a colon: and a bare word: here"}`,
		`{"escaped":"quote \" then {x: 1}"}`,
		`[{"k":"v"},{"k2":2}]`,
		`{"true":1,"null":2}`,
	}
	for _, src := range cases {
		var before, after any
		if err := json.Unmarshal([]byte(src), &before); err != nil {
			t.Fatalf("bad test input %s: %v", src, err)
		}
		if err := json.Unmarshal(quoteBareKeys([]byte(src)), &after); err != nil {
			t.Errorf("%s: repaired copy no longer parses: %v", src, err)
			continue
		}
		if !reflect.DeepEqual(before, after) {
			t.Errorf("%s: repair changed the value\nbefore: %#v\nafter:  %#v", src, before, after)
		}
	}
}

// Bare literals in VALUE position are values, not keys, and must stay bare so
// they keep their type.
func TestQuoteBareKeysDoesNotQuoteValues(t *testing.T) {
	got := string(quoteBareKeys([]byte(`{a:true,b:false,c:null}`)))
	want := `{"a":true,"b":false,"c":null}`
	if got != want {
		t.Errorf("quoteBareKeys = %s, want %s", got, want)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatal(err)
	}
	if m["a"] != true || m["b"] != false || m["c"] != nil {
		t.Errorf("values changed type: %#v", m)
	}
}
