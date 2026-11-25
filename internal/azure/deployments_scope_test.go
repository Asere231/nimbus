package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Subscription-scope deployments. Every path and body here was captured
// from `az deployment sub create` 2.90.0 against a logging server.
//
// A subscription deployment is NOT a resource-group deployment with a
// different path: `resourceGroup()` is an error there, a resource's ID
// hangs off /subscriptions/{id} rather than off a group, and the main
// thing it creates is resource groups themselves.

const subTemplate = `{
  "$schema": "https://schema.management.azure.com/schemas/2018-05-01/subscriptionDeploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": {
    "rgName": { "type": "string", "defaultValue": "made-by-sub" }
  },
  "resources": [
    {
      "type": "Microsoft.Resources/resourceGroups",
      "apiVersion": "2021-04-01",
      "name": "[parameters('rgName')]",
      "location": "[deployment().location]",
      "tags": { "madeBy": "subscription deployment" }
    }
  ],
  "outputs": {
    "groupId": {
      "type": "string",
      "value": "[subscriptionResourceId('Microsoft.Resources/resourceGroups', parameters('rgName'))]"
    }
  }
}`

func subDeploymentID(name string) string {
	return "/subscriptions/" + DefaultSubscriptionID +
		"/providers/Microsoft.Resources/deployments/" + name
}

// deploySub PUTs a subscription-scope deployment and waits, as `az` does.
// Note the `location` at the TOP LEVEL of the body rather than in
// properties -- there is no group to inherit one from, which is why
// `az deployment sub create` requires --location.
func deploySub(t *testing.T, s *Server, name, location, template string) map[string]any {
	t.Helper()
	body := map[string]any{
		"location": location,
		"properties": map[string]any{
			"mode":     "Incremental",
			"template": json.RawMessage(template),
		},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	id := subDeploymentID(name)
	req := httptest.NewRequest(http.MethodPut, id+"?api-version=2021-04-01", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("PUT subscription deployment: %d %s", rec.Code, rec.Body)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := getResource(t, s, id)
		props, _ := got["properties"].(map[string]any)
		switch props["provisioningState"] {
		case "Succeeded":
			return got
		case "Failed":
			t.Fatalf("the deployment failed: %v", props["error"])
		}
		if time.Now().After(deadline) {
			t.Fatalf("the deployment did not finish: state %v", props["provisioningState"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The central claim: a subscription deployment creates a resource group,
// at the right ID.
func TestASubscriptionDeploymentCreatesAResourceGroup(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	got := deploySub(t, s, "subdep", "westus2", subTemplate)

	// A resource GROUP's id has no `providers` segment -- which is the one
	// place the generic ID builder is wrong, and the type a subscription
	// deployment most often creates.
	want := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/made-by-sub"
	if !s.store.Exists(want) {
		t.Fatalf("the deployment did not create %s", want)
	}
	res, err := s.store.Get(want)
	if err != nil {
		t.Fatal(err)
	}
	if res.Location != "westus2" {
		t.Errorf("the group's location is %q, want the deployment's westus2", res.Location)
	}

	// The OUTPUT has to agree with the resource. A subscriptionResourceId()
	// that produced the providers form would hand a template an id that
	// misses the group it just made.
	props, _ := got["properties"].(map[string]any)
	outputs, _ := props["outputs"].(map[string]any)
	group, _ := outputs["groupId"].(map[string]any)
	if str(group["value"]) != want {
		t.Errorf("subscriptionResourceId gave %q, want %q", group["value"], want)
	}
}

// `resourceGroup()` is REFUSED at subscription scope, as real ARM refuses
// it -- and at SUBMIT time, so the client reads the reason in the response
// it is already waiting on rather than polling into a failure.
func TestResourceGroupIsRefusedAtSubscriptionScope(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	template := `{
      "$schema": "https://schema.management.azure.com/schemas/2018-05-01/subscriptionDeploymentTemplate.json#",
      "contentVersion": "1.0.0.0",
      "resources": [
        { "type": "Microsoft.Resources/resourceGroups", "apiVersion": "2021-04-01",
          "name": "nope", "location": "[resourceGroup().location]" }
      ]
    }`
	body, _ := json.Marshal(map[string]any{
		"location":   "eastus",
		"properties": map[string]any{"template": json.RawMessage(template)},
	})
	req := httptest.NewRequest(http.MethodPut,
		subDeploymentID("bad")+"?api-version=2021-04-01", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("resourceGroup() at subscription scope was accepted with %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "resourceGroup()") {
		t.Errorf("the refusal does not name the function: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "subscription-scope") {
		t.Errorf("the refusal does not say why: %s", rec.Body)
	}
	// And nothing was created.
	if s.store.Exists("/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/nope") {
		t.Error("a refused template created a resource group anyway")
	}
}

// A non-group resource at subscription scope keeps the providers form,
// which is the general rule the resource group is the exception to.
func TestASubscriptionScopeResourceKeepsTheProvidersForm(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	template := `{
      "$schema": "https://schema.management.azure.com/schemas/2018-05-01/subscriptionDeploymentTemplate.json#",
      "contentVersion": "1.0.0.0",
      "resources": [],
      "outputs": {
        "roleId": { "type": "string",
          "value": "[subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b24988ac-6180-42a0-ab88-20f7382dd24c')]" }
      }
    }`
	got := deploySub(t, s, "ids", "eastus", template)
	props, _ := got["properties"].(map[string]any)
	outputs, _ := props["outputs"].(map[string]any)
	role, _ := outputs["roleId"].(map[string]any)
	want := "/subscriptions/" + DefaultSubscriptionID +
		"/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	if str(role["value"]) != want {
		t.Errorf("subscriptionResourceId gave %q, want %q", role["value"], want)
	}
}

// `deployment().location` is what a subscription template uses where a
// group-scope one uses resourceGroup().location, and it comes from the
// body's top-level location.
func TestDeploymentLocationComesFromTheRequest(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	template := `{
      "$schema": "https://schema.management.azure.com/schemas/2018-05-01/subscriptionDeploymentTemplate.json#",
      "contentVersion": "1.0.0.0",
      "resources": [],
      "outputs": { "where": { "type": "string", "value": "[deployment().location]" } }
    }`
	got := deploySub(t, s, "loc", "northeurope", template)
	props, _ := got["properties"].(map[string]any)
	outputs, _ := props["outputs"].(map[string]any)
	where, _ := outputs["where"].(map[string]any)
	if str(where["value"]) != "northeurope" {
		t.Errorf("deployment().location gave %q, want northeurope", where["value"])
	}
}

// A deployment's SCOPE is part of its identity, so the two listings must
// not see each other -- which is not true of the types the cross-group
// subscription list exists for.
func TestTheTwoDeploymentListingsAreSeparate(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	deploySub(t, s, "at-sub", "eastus", subTemplate)
	deploy(t, s, rg, "at-group", nsgAndVnet, nil)

	subNames := deploymentNames(t, s,
		"/subscriptions/"+DefaultSubscriptionID+"/providers/Microsoft.Resources/deployments")
	if strings.Join(subNames, ",") != "at-sub" {
		t.Errorf("the subscription listing has %v, want just at-sub", subNames)
	}

	groupNames := deploymentNames(t, s,
		"/subscriptions/"+DefaultSubscriptionID+"/resourceGroups/"+rg+
			"/providers/Microsoft.Resources/deployments")
	if strings.Join(groupNames, ",") != "at-group" {
		t.Errorf("the group listing has %v, want just at-group", groupNames)
	}
}

func deploymentNames(t *testing.T, s *Server, path string) []string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path+"?api-version=2021-04-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var doc struct {
		Value []struct{ Name string } `json:"value"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("listing is not JSON: %v\n%s", err, rec.Body)
	}
	var names []string
	for _, v := range doc.Value {
		names = append(names, v.Name)
	}
	return names
}

// A resource-group deployment is UNCHANGED, which is what keeps every
// existing example true. The control that proves a scope regression is a
// scope regression.
func TestAGroupDeploymentStillUsesTheGroupScope(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	got := deploy(t, s, rg, "control", nsgAndVnet, nil)
	props, _ := got["properties"].(map[string]any)
	outputs, _ := props["outputs"].(map[string]any)
	vnet, _ := outputs["vnetId"].(map[string]any)
	want := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/tmpl-vnet"
	if str(vnet["value"]) != want {
		t.Errorf("a group deployment's resourceId gave %q, want %q", vnet["value"], want)
	}
}

// A NON-GROUP resource really deployed at subscription scope. This is
// where the original bug would come back: its ID is built on the
// deployment's base, and a base of "/subscriptions/{s}/resourceGroups/"
// -- a group with no name -- is what produced
// "ParentResourceNotFound: Parent resource
// '/subscriptions/.../resourceGroups/' not found" against `az`.
//
// Added after an injection that broke DeploymentBaseID() passed every
// other test in this file: the group is special-cased and
// subscriptionResourceId() passes its own base, so nothing exercised the
// one function that was wrong.
func TestANonGroupResourceDeployedAtSubscriptionScope(t *testing.T) {
	s, _ := newTestServerWithGroup(t)
	template := `{
      "$schema": "https://schema.management.azure.com/schemas/2018-05-01/subscriptionDeploymentTemplate.json#",
      "contentVersion": "1.0.0.0",
      "resources": [
        {
          "type": "Microsoft.Authorization/roleAssignments",
          "apiVersion": "2022-04-01",
          "name": "11111111-2222-3333-4444-555555555555",
          "properties": {
            "roleDefinitionId": "[subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'b24988ac-6180-42a0-ab88-20f7382dd24c')]",
            "principalId": "00000000-0000-0000-0000-0000000000aa",
            "principalType": "ServicePrincipal"
          }
        }
      ],
      "outputs": {
        "assignmentId": { "type": "string", "value": "[resourceId('Microsoft.Authorization/roleAssignments', '11111111-2222-3333-4444-555555555555')]" }
      }
    }`
	deploySub(t, s, "rbac", "eastus", template)

	want := "/subscriptions/" + DefaultSubscriptionID +
		"/providers/Microsoft.Authorization/roleAssignments/11111111-2222-3333-4444-555555555555"
	if !s.store.Exists(want) {
		t.Fatalf("the deployment did not create %s", want)
	}
	// And nothing landed under a group with no name, which is the shape
	// the bug produced.
	for _, res := range s.store.List(nil) {
		if strings.Contains(res.ID, "/resourceGroups//") ||
			strings.HasSuffix(res.ID, "/resourceGroups/") {
			t.Errorf("a resource landed under a nameless group: %s", res.ID)
		}
	}
}
