package azure

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// What `az deployment group create` does: PUT the deployment, then poll it
// until provisioningState is terminal.

const nsgAndVnet = `{
  "$schema": "https://schema.management.azure.com/schemas/2019-04-01/deploymentTemplate.json#",
  "contentVersion": "1.0.0.0",
  "parameters": { "prefix": { "type": "string", "defaultValue": "tmpl" } },
  "variables": { "vnetName": "[concat(parameters('prefix'), '-vnet')]" },
  "resources": [
    {
      "type": "Microsoft.Network/virtualNetworks",
      "apiVersion": "2023-09-01",
      "name": "[variables('vnetName')]",
      "location": "[resourceGroup().location]",
      "dependsOn": ["[resourceId('Microsoft.Network/networkSecurityGroups', concat(parameters('prefix'), '-nsg'))]"],
      "properties": { "addressSpace": { "addressPrefixes": ["10.1.0.0/16"] } }
    },
    {
      "type": "Microsoft.Network/networkSecurityGroups",
      "apiVersion": "2023-09-01",
      "name": "[concat(parameters('prefix'), '-nsg')]",
      "location": "[resourceGroup().location]",
      "properties": {}
    }
  ],
  "outputs": {
    "vnetId": {
      "type": "string",
      "value": "[resourceId('Microsoft.Network/virtualNetworks', variables('vnetName'))]"
    },
    "nsgGuid": {
      "type": "string",
      "value": "[reference(resourceId('Microsoft.Network/networkSecurityGroups', concat(parameters('prefix'), '-nsg'))).resourceGuid]"
    }
  }
}`

func deploymentID(rg, name string) string {
	return "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Resources/deployments/" + name
}

// deploy PUTs a deployment and waits for it to finish, as a client would.
func deploy(t *testing.T, s *Server, rg, name, template string, params map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"properties": map[string]any{
		"mode":       "Incremental",
		"template":   json.RawMessage(template),
		"parameters": params,
	}}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	id := deploymentID(rg, name)
	req := httptest.NewRequest(http.MethodPut, id+"?api-version=2021-04-01", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 && rec.Code != 201 {
		t.Fatalf("PUT deployment: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		got, _ := getResource(t, s, id)
		props, _ := got["properties"].(map[string]any)
		switch props["provisioningState"] {
		case "Succeeded":
			return got
		case "Failed":
			t.Fatalf("deployment failed: %v", props["error"])
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment did not finish: state %v", props["provisioningState"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDeploymentCreatesEveryResource(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	got := deploy(t, s, rg, "dep1", nsgAndVnet, nil)

	for _, id := range []string{
		"/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/Microsoft.Network/virtualNetworks/tmpl-vnet",
		"/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/Microsoft.Network/networkSecurityGroups/tmpl-nsg",
	} {
		if !s.store.Exists(id) {
			t.Errorf("the deployment did not create %s", id)
		}
	}

	props, _ := got["properties"].(map[string]any)
	if props["provisioningState"] != "Succeeded" {
		t.Errorf("provisioningState = %v", props["provisioningState"])
	}
	// Outputs, including one that had to wait for a resource to exist.
	outputs, _ := props["outputs"].(map[string]any)
	vnetID, _ := outputs["vnetId"].(map[string]any)
	if !strings.HasSuffix(toStr(vnetID["value"]), "/virtualNetworks/tmpl-vnet") {
		t.Errorf("output vnetId = %v", vnetID["value"])
	}
	nsgGuid, _ := outputs["nsgGuid"].(map[string]any)
	if toStr(nsgGuid["value"]) == "" {
		t.Error("output nsgGuid is empty: reference() did not resolve after deployment")
	}
}

// A deployed resource is an ordinary resource: it goes through the same hooks
// and lands in the store the same way, so the visualizer shows it.
func TestDeployedResourcesGoThroughTheirHooks(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	deploy(t, s, rg, "dep2", nsgAndVnet, nil)

	vnetID := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/tmpl-vnet"
	res, err := s.store.Get(vnetID)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(res.Body, &body); err != nil {
		t.Fatal(err)
	}
	props, _ := body["properties"].(map[string]any)
	if _, ok := props["subnets"]; !ok {
		t.Error("the VNet hook did not run on a resource created by a deployment")
	}
	if res.Backing != "modelled" {
		t.Errorf("backing = %q, want modelled", res.Backing)
	}
}

func TestDeploymentParametersOverrideDefaults(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	deploy(t, s, rg, "dep3", nsgAndVnet, map[string]any{"prefix": map[string]any{"value": "over"}})
	id := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/over-vnet"
	if !s.store.Exists(id) {
		t.Errorf("the supplied parameter was not used: %s does not exist", id)
	}
}

// A broken template must fail the deployment, with the reason readable in the
// deployment itself -- that is all a client has to go on.
func TestDeploymentFailsLoudly(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	const broken = `{"resources":[{"type":"Microsoft.Network/publicIPAddresses","apiVersion":"2023-09-01",
	  "name":"[parameters('nope')]","properties":{}}]}`
	body, _ := json.Marshal(map[string]any{"properties": map[string]any{
		"mode": "Incremental", "template": json.RawMessage(broken),
	}})
	req := httptest.NewRequest(http.MethodPut, deploymentID(rg, "bad")+"?api-version=2021-04-01",
		strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code < 400 {
		t.Fatalf("a template naming an undefined parameter returned %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "nope") {
		t.Errorf("the error does not name the parameter: %s", rec.Body.String())
	}
}

func TestDeploymentValidateAndWhatIf(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	body, _ := json.Marshal(map[string]any{"properties": map[string]any{
		"mode": "Incremental", "template": json.RawMessage(nsgAndVnet),
	}})

	for _, action := range []string{"validate", "whatIf"} {
		req := httptest.NewRequest(http.MethodPost,
			deploymentID(rg, "dep4")+"/"+action+"?api-version=2021-04-01", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", action, rec.Code, rec.Body.String())
		}
		if action == "whatIf" {
			var out struct {
				Status  string `json:"status"`
				Changes []struct {
					ChangeType string `json:"changeType"`
					ResourceID string `json:"resourceId"`
				} `json:"changes"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("whatIf: %v", err)
			}
			if len(out.Changes) != 2 {
				t.Fatalf("whatIf reported %d changes, want 2", len(out.Changes))
			}
			for _, c := range out.Changes {
				if c.ChangeType != "Create" {
					t.Errorf("%s: changeType = %s, want Create", c.ResourceID, c.ChangeType)
				}
			}
		}
	}
	// Neither may have created anything.
	id := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Network/virtualNetworks/tmpl-vnet"
	if s.store.Exists(id) {
		t.Error("validate/whatIf created a resource")
	}
}

// Running what-if against resources that already exist reports NoChange
// rather than Create.
func TestWhatIfSeesExistingResources(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	deploy(t, s, rg, "dep5", nsgAndVnet, nil)

	body, _ := json.Marshal(map[string]any{"properties": map[string]any{
		"mode": "Incremental", "template": json.RawMessage(nsgAndVnet),
	}})
	req := httptest.NewRequest(http.MethodPost,
		deploymentID(rg, "dep5")+"/whatIf?api-version=2021-04-01", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("whatIf: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Changes []struct {
			ChangeType string `json:"changeType"`
		} `json:"changes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Changes) == 0 {
		t.Fatal("whatIf reported no changes at all")
	}
	// NoChange specifically, not merely "not Create": asserting the weaker
	// thing let a real `az deployment group what-if` report Modify for
	// resources nothing had touched, because the stored copy has
	// server-filled fields inside arrays that the template never mentions.
	for _, c := range out.Changes {
		if c.ChangeType != "NoChange" {
			t.Errorf("changeType = %s for an unchanged resource, want NoChange", c.ChangeType)
		}
	}
}

// Deployments are listable, which is how `az deployment group list` works.
func TestDeploymentsAreListed(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	deploy(t, s, rg, "dep6", nsgAndVnet, nil)

	collection := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Resources/deployments"
	req := httptest.NewRequest(http.MethodGet, collection+"?api-version=2021-04-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Value []map[string]any `json:"value"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Value) != 1 {
		t.Fatalf("listed %d deployments, want 1", len(list.Value))
	}

	// ...and each one's operations.
	req = httptest.NewRequest(http.MethodGet,
		deploymentID(rg, "dep6")+"/operations?api-version=2021-04-01", nil)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("operations: %d %s", rec.Code, rec.Body.String())
	}
	var ops struct {
		Value []map[string]any `json:"value"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &ops)
	if len(ops.Value) != 2 {
		t.Errorf("listed %d operations, want one per resource", len(ops.Value))
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
