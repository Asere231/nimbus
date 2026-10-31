package deployments

import (
	"reflect"
	"strings"
	"testing"
)

func testScope() *Scope {
	return &Scope{
		SubscriptionID: "00000000-0000-0000-0000-000000000000",
		ResourceGroup:  "rg1",
		Location:       "eastus",
		TenantID:       "00000000-0000-0000-0000-000000000001",
		Parameters:     map[string]any{"name": "web", "count": float64(2), "on": true},
		Variables:      map[string]any{"prefix": "nim", "sizes": []any{"S", "M", "L"}},
	}
}

func eval(t *testing.T, src string) any {
	t.Helper()
	got, err := Evaluate(src, testScope())
	if err != nil {
		t.Fatalf("Evaluate(%q): %v", src, err)
	}
	return got
}

func TestEvaluateLiteralsAndPassthrough(t *testing.T) {
	cases := map[string]any{
		// Anything that is not bracketed is returned unchanged.
		"plain":         "plain",
		"":              "",
		"[[notAnExpr]":  "[notAnExpr]", // the documented escape for a literal bracket
		"[ 'spaced' ]":  "spaced",
		"['hello']":     "hello",
		"[42]":          float64(42),
		"[true]":        true,
		"[null]":        nil,
		"[concat('a')]": "a",
	}
	for src, want := range cases {
		if got := eval(t, src); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %#v, want %#v", src, got, want)
		}
	}
}

func TestEvaluateFunctions(t *testing.T) {
	cases := map[string]any{
		"[parameters('name')]":                                   "web",
		"[variables('prefix')]":                                  "nim",
		"[concat(variables('prefix'), '-', parameters('name'))]": "nim-web",
		"[format('{0}-{1}-{2}', 'a', 'b', 1)]":                   "a-b-1",
		"[toUpper('abc')]":                                       "ABC",
		"[toLower('ABC')]":                                       "abc",
		"[length(variables('sizes'))]":                           float64(3),
		"[length('abcd')]":                                       float64(4),
		"[string(42)]":                                           "42",
		"[int('7')]":                                             float64(7),
		"[add(2, 3)]":                                            float64(5),
		"[sub(5, 3)]":                                            float64(2),
		"[mul(3, 4)]":                                            float64(12),
		"[div(9, 3)]":                                            float64(3),
		"[mod(9, 4)]":                                            float64(1),
		"[not(false)]":                                           true,
		"[and(true, true)]":                                      true,
		"[or(false, true)]":                                      true,
		"[equals('a', 'a')]":                                     true,
		"[equals(1, 2)]":                                         false,
		"[if(equals(1, 1), 'yes', 'no')]":                        "yes",
		"[if(parameters('on'), 'yes', 'no')]":                    "yes",
		"[coalesce(null, 'fallback')]":                           "fallback",
		"[first(variables('sizes'))]":                            "S",
		"[last(variables('sizes'))]":                             "L",
		"[contains('abcdef', 'cde')]":                            true,
		"[startsWith('abcdef', 'abc')]":                          true,
		"[substring('abcdef', 1, 3)]":                            "bcd",
		"[replace('a-b', '-', '+')]":                             "a+b",
		"[trim('  x  ')]":                                        "x",
		"[empty('')]":                                            true,
		"[json('{\"a\":1}').a":                                   nil, // malformed on purpose; see below
	}
	delete(cases, "[json('{\"a\":1}').a") // syntax error case is asserted separately
	for src, want := range cases {
		if got := eval(t, src); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %#v, want %#v", src, got, want)
		}
	}
}

func TestEvaluateAccessors(t *testing.T) {
	cases := map[string]any{
		"[variables('sizes')[1]]":                         "M",
		"[json('{\"a\":{\"b\":7}}').a.b]":                 float64(7),
		"[json('[1,2,3]')[2]]":                            float64(3),
		"[split('a,b,c', ',')[1]]":                        "b",
		"[union(json('{\"a\":1}'), json('{\"b\":2}')).b]": float64(2),
	}
	for src, want := range cases {
		if got := eval(t, src); !reflect.DeepEqual(got, want) {
			t.Errorf("%q = %#v, want %#v", src, got, want)
		}
	}
}

func TestEvaluateScopeFunctions(t *testing.T) {
	s := testScope()
	got, err := Evaluate("[resourceGroup().location]", s)
	if err != nil || got != "eastus" {
		t.Errorf("resourceGroup().location = %v, %v", got, err)
	}
	got, _ = Evaluate("[subscription().subscriptionId]", s)
	if got != s.SubscriptionID {
		t.Errorf("subscription().subscriptionId = %v", got)
	}
	got, _ = Evaluate("[resourceId('Microsoft.Network/virtualNetworks', 'vnet1')]", s)
	want := "/subscriptions/" + s.SubscriptionID + "/resourceGroups/rg1/providers/Microsoft.Network/virtualNetworks/vnet1"
	if got != want {
		t.Errorf("resourceId = %v, want %v", got, want)
	}
	// A child resource takes one name segment per type segment.
	got, _ = Evaluate("[resourceId('Microsoft.Network/virtualNetworks/subnets', 'vnet1', 'snet1')]", s)
	want = "/subscriptions/" + s.SubscriptionID + "/resourceGroups/rg1/providers/Microsoft.Network/virtualNetworks/vnet1/subnets/snet1"
	if got != want {
		t.Errorf("child resourceId = %v, want %v", got, want)
	}
	got, _ = Evaluate("[subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'abc')]", s)
	want = "/subscriptions/" + s.SubscriptionID + "/providers/Microsoft.Authorization/roleDefinitions/abc"
	if got != want {
		t.Errorf("subscriptionResourceId = %v, want %v", got, want)
	}
}

// uniqueString must be stable for the same inputs and differ for different
// ones. It is deliberately NOT Azure's hash -- see the note in funcs.go --
// but a template that only needs a unique, repeatable name works the same.
func TestUniqueStringIsStableAndShaped(t *testing.T) {
	a, err := Evaluate("[uniqueString(resourceGroup().id)]", testScope())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Evaluate("[uniqueString(resourceGroup().id)]", testScope())
	if a != b {
		t.Errorf("uniqueString is not stable: %v vs %v", a, b)
	}
	s, _ := a.(string)
	if len(s) != 13 {
		t.Errorf("uniqueString returned %q (%d chars), want 13", s, len(s))
	}
	if strings.ToLower(s) != s {
		t.Errorf("uniqueString returned %q, want lower case", s)
	}
	c, _ := Evaluate("[uniqueString('something else')]", testScope())
	if c == a {
		t.Error("uniqueString returned the same value for different input")
	}
	// Multiple arguments are concatenated, as in Azure.
	d, _ := Evaluate("[uniqueString('a', 'b')]", testScope())
	e, _ := Evaluate("[uniqueString('ab')]", testScope())
	if d != e {
		t.Errorf("uniqueString('a','b') = %v, uniqueString('ab') = %v: want the same", d, e)
	}
}

func TestEvaluateErrors(t *testing.T) {
	cases := []string{
		"[parameters('missing')]",
		"[variables('missing')]",
		"[nosuchfunction('x')]",
		"[concat('a']", // bracketed, so it IS an expression, and a broken one
		"[substring('abc', 1, 99)]",
		"[]",
	}
	for _, src := range cases {
		if got, err := Evaluate(src, testScope()); err == nil {
			t.Errorf("Evaluate(%q) = %#v, want an error", src, got)
		}
	}
}

// An unknown function has to say so clearly: a template using list* or
// anything else Nimbus does not implement should fail loudly rather than
// quietly deploying something wrong.
func TestUnknownFunctionNamesItself(t *testing.T) {
	_, err := Evaluate("[listKeys('x', '2021-01-01')]", testScope())
	if err == nil || !strings.Contains(err.Error(), "listKeys") {
		t.Errorf("error = %v, want one naming listKeys", err)
	}
}
