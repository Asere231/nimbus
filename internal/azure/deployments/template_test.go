package deployments

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, src string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(src), &m); err != nil {
		t.Fatalf("bad test template: %v", err)
	}
	return m
}

func expand(t *testing.T, template string, params map[string]any) *Plan {
	t.Helper()
	plan, err := Expand(parse(t, template), params, testScope())
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	return plan
}

const twoResources = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "prefix": { "type": "string", "defaultValue": "nim" }
  },
  "variables": {
    "vnetName": "[concat(parameters('prefix'), '-vnet')]"
  },
  "resources": [
    {
      "type": "Microsoft.Network/networkSecurityGroups",
      "apiVersion": "2023-09-01",
      "name": "[concat(parameters('prefix'), '-nsg')]",
      "location": "[resourceGroup().location]",
      "properties": {}
    },
    {
      "type": "Microsoft.Network/virtualNetworks",
      "apiVersion": "2023-09-01",
      "name": "[variables('vnetName')]",
      "location": "[resourceGroup().location]",
      "dependsOn": [
        "[resourceId('Microsoft.Network/networkSecurityGroups', concat(parameters('prefix'), '-nsg'))]"
      ],
      "properties": {
        "addressSpace": { "addressPrefixes": ["10.0.0.0/16"] }
      }
    }
  ],
  "outputs": {
    "vnetName": { "type": "string", "value": "[variables('vnetName')]" }
  }
}`

func TestExpandResolvesNamesAndOrder(t *testing.T) {
	plan := expand(t, twoResources, nil)
	if len(plan.Resources) != 2 {
		t.Fatalf("got %d resources, want 2", len(plan.Resources))
	}
	// dependsOn decides the order: the NSG must come first.
	if !strings.HasSuffix(plan.Resources[0].Name, "-nsg") {
		t.Errorf("first resource is %s, want the NSG it depends on", plan.Resources[0].Name)
	}
	vnet := plan.Resources[1]
	if vnet.Name != "nim-vnet" {
		t.Errorf("vnet name = %q, want nim-vnet", vnet.Name)
	}
	wantID := "/subscriptions/" + testScope().SubscriptionID +
		"/resourceGroups/rg1/providers/Microsoft.Network/virtualNetworks/nim-vnet"
	if vnet.ID != wantID {
		t.Errorf("vnet id = %q, want %q", vnet.ID, wantID)
	}
	if vnet.Body["location"] != "eastus" {
		t.Errorf("location = %v, want eastus (from resourceGroup())", vnet.Body["location"])
	}
	// The body must not carry template-only keys.
	for _, key := range []string{"apiVersion", "dependsOn", "condition", "copy"} {
		if _, present := vnet.Body[key]; present {
			t.Errorf("body still carries the template key %q", key)
		}
	}
}

func TestExpandAppliesParameterOverridesAndDefaults(t *testing.T) {
	plan := expand(t, twoResources, map[string]any{"prefix": "app"})
	if plan.Resources[1].Name != "app-vnet" {
		t.Errorf("name = %q, want app-vnet from the supplied parameter", plan.Resources[1].Name)
	}
	// The default is used when nothing is supplied.
	plan = expand(t, twoResources, nil)
	if plan.Resources[1].Name != "nim-vnet" {
		t.Errorf("name = %q, want nim-vnet from the default", plan.Resources[1].Name)
	}
}

func TestExpandRejectsAMissingRequiredParameter(t *testing.T) {
	const tmpl = `{
      "parameters": { "needed": { "type": "string" } },
      "resources": []
    }`
	if _, err := Expand(parse(t, tmpl), nil, testScope()); err == nil {
		t.Error("expected an error for a parameter with no value and no default")
	} else if !strings.Contains(err.Error(), "needed") {
		t.Errorf("error = %v, want one naming the parameter", err)
	}
}

// The az CLI sends parameters in their wrapped form: {"p": {"value": 1}}.
func TestExpandUnwrapsParameterValues(t *testing.T) {
	params := map[string]any{"prefix": map[string]any{"value": "wrapped"}}
	plan := expand(t, twoResources, params)
	if plan.Resources[1].Name != "wrapped-vnet" {
		t.Errorf("name = %q, want the unwrapped parameter value", plan.Resources[1].Name)
	}
}

const copyTemplate = `{
  "parameters": { "count": { "type": "int", "defaultValue": 3 } },
  "resources": [
    {
      "type": "Microsoft.Network/publicIPAddresses",
      "apiVersion": "2023-09-01",
      "name": "[concat('pip-', copyIndex())]",
      "location": "eastus",
      "copy": { "name": "pips", "count": "[parameters('count')]" },
      "properties": { "idleTimeoutInMinutes": "[copyIndex(1)]" }
    }
  ]
}`

func TestExpandCopyLoops(t *testing.T) {
	plan := expand(t, copyTemplate, nil)
	if len(plan.Resources) != 3 {
		t.Fatalf("got %d resources, want 3 from the copy loop", len(plan.Resources))
	}
	for i, r := range plan.Resources {
		wantName := "pip-" + string(rune('0'+i))
		if r.Name != wantName {
			t.Errorf("resource %d is named %q, want %q", i, r.Name, wantName)
		}
		props, _ := r.Body["properties"].(map[string]any)
		if props["idleTimeoutInMinutes"] != float64(i+1) {
			t.Errorf("resource %d: copyIndex(1) = %v, want %d", i, props["idleTimeoutInMinutes"], i+1)
		}
	}
}

func TestExpandConditionSkipsResources(t *testing.T) {
	const tmpl = `{
      "parameters": { "make": { "type": "bool", "defaultValue": false } },
      "resources": [
        {
          "type": "Microsoft.Network/publicIPAddresses",
          "apiVersion": "2023-09-01",
          "name": "maybe",
          "condition": "[parameters('make')]",
          "properties": {}
        }
      ]
    }`
	if plan := expand(t, tmpl, nil); len(plan.Resources) != 0 {
		t.Errorf("got %d resources, want none: the condition is false", len(plan.Resources))
	}
	plan, err := Expand(parse(t, tmpl), map[string]any{"make": true}, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Resources) != 1 {
		t.Errorf("got %d resources, want 1 when the condition is true", len(plan.Resources))
	}
}

// A resource that mentions another through resourceId() in its body depends on
// it even without a dependsOn, which is how Bicep-generated templates often
// read.
func TestExpandOrdersByImplicitReferences(t *testing.T) {
	const tmpl = `{
      "resources": [
        {
          "type": "Microsoft.Network/virtualNetworks",
          "apiVersion": "2023-09-01",
          "name": "vnet",
          "properties": {
            "subnets": [{
              "name": "s1",
              "properties": {
                "networkSecurityGroup": {
                  "id": "[resourceId('Microsoft.Network/networkSecurityGroups', 'nsg')]"
                }
              }
            }]
          }
        },
        {
          "type": "Microsoft.Network/networkSecurityGroups",
          "apiVersion": "2023-09-01",
          "name": "nsg",
          "properties": {}
        }
      ]
    }`
	plan := expand(t, tmpl, nil)
	if plan.Resources[0].Name != "nsg" {
		t.Errorf("order is %s then %s, want the NSG first: the VNet references it",
			plan.Resources[0].Name, plan.Resources[1].Name)
	}
}

func TestExpandDetectsDependencyCycles(t *testing.T) {
	const tmpl = `{
      "resources": [
        {"type": "Microsoft.Network/publicIPAddresses", "apiVersion": "2023-09-01", "name": "a",
         "dependsOn": ["[resourceId('Microsoft.Network/publicIPAddresses', 'b')]"], "properties": {}},
        {"type": "Microsoft.Network/publicIPAddresses", "apiVersion": "2023-09-01", "name": "b",
         "dependsOn": ["[resourceId('Microsoft.Network/publicIPAddresses', 'a')]"], "properties": {}}
      ]
    }`
	_, err := Expand(parse(t, tmpl), nil, testScope())
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "circular") {
		t.Errorf("error = %v, want one about a circular dependency", err)
	}
}

// Outputs are evaluated after the resources exist, because they commonly call
// reference().
func TestOutputsAreEvaluatedAgainstDeployedResources(t *testing.T) {
	plan := expand(t, twoResources, nil)
	scope := testScope()
	scope.Reference = func(id string) (map[string]any, bool) { return nil, false }
	outputs, err := plan.EvaluateOutputs(scope)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := outputs["vnetName"].(map[string]any)
	if out["value"] != "nim-vnet" {
		t.Errorf("output vnetName = %v, want nim-vnet", out["value"])
	}
}

// Nested properties that reference a sibling resource must resolve once that
// sibling is deployed.
func TestReferenceResolvesAfterDeployment(t *testing.T) {
	scope := testScope()
	scope.Reference = func(id string) (map[string]any, bool) {
		if strings.HasSuffix(id, "/networkSecurityGroups/nsg") {
			return map[string]any{"properties": map[string]any{"resourceGuid": "abc"}}, true
		}
		return nil, false
	}
	got, err := Evaluate("[reference(resourceId('Microsoft.Network/networkSecurityGroups', 'nsg')).resourceGuid]", scope)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc" {
		t.Errorf("reference(...).resourceGuid = %v, want abc", got)
	}
}

// A parameter default is usually an expression -- "[resourceGroup().location]"
// is the commonest line in any Bicep file -- and taking it literally leaves
// the expression itself in the resource body. The deploy path hid this by
// evaluating the body again later; what-if did not, and reported Modify for
// resources nothing had touched.
func TestParameterDefaultsAreEvaluated(t *testing.T) {
	const tmpl = `{
      "parameters": {
        "location": { "type": "string", "defaultValue": "[resourceGroup().location]" },
        "prefix":   { "type": "string", "defaultValue": "nim" },
        "name":     { "type": "string", "defaultValue": "[concat(parameters('prefix'), '-pip')]" }
      },
      "resources": [{
        "type": "Microsoft.Network/publicIPAddresses",
        "apiVersion": "2023-09-01",
        "name": "[parameters('name')]",
        "location": "[parameters('location')]",
        "properties": {}
      }]
    }`
	plan := expand(t, tmpl, nil)
	if got := plan.Parameters["location"]; got != "eastus" {
		t.Errorf("location parameter = %#v, want eastus", got)
	}
	// A default that refers to another parameter resolves too, whatever order
	// the map happens to iterate in.
	if got := plan.Parameters["name"]; got != "nim-pip" {
		t.Errorf("name parameter = %#v, want nim-pip", got)
	}
	if got := plan.Resources[0].Body["location"]; got != "eastus" {
		t.Errorf("resource location = %#v, want eastus", got)
	}
}
