package azure

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// getQuery does a GET with extra query parameters, which `do` cannot express.
// Role definition lookups are all about the $filter.
func getQuery(t *testing.T, srv *httptest.Server, path string, q url.Values) (int, map[string]any) {
	t.Helper()
	q.Set("api-version", "2022-04-01")
	req, _ := http.NewRequest("GET", srv.URL+path+"?"+q.Encode(), nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

// azurerm_role_assignment with role_definition_name looks the role up by name
// first. The filter is the only way it can: role definitions are addressed by
// GUID, and nobody writes GUIDs in Terraform.
func TestRoleDefinitionsAreFoundByName(t *testing.T) {
	srv, _ := newTestServer(t)
	path := sub + "/providers/Microsoft.Authorization/roleDefinitions"

	code, body := getQuery(t, srv, path, url.Values{"$filter": {"roleName eq 'AcrPull'"}})
	if code != 200 {
		t.Fatalf("filtered list = %d: %v", code, body)
	}
	list, _ := body["value"].([]any)
	if len(list) != 1 {
		t.Fatalf("filter roleName eq 'AcrPull' returned %d definitions: %v", len(list), body)
	}
	role := list[0].(map[string]any)
	// The real GUID, from Microsoft's built-in roles reference. A made-up one
	// would be stored in Terraform state and in role assignments, so anything
	// compared against real Azure later would disagree.
	if role["name"] != "7f951dda-4ed3-4680-a7ca-43fe172d538d" {
		t.Errorf("AcrPull GUID = %v", role["name"])
	}
	if role["type"] != "Microsoft.Authorization/roleDefinitions" {
		t.Errorf("type = %v", role["type"])
	}
	if role["id"] != path+"/7f951dda-4ed3-4680-a7ca-43fe172d538d" {
		t.Errorf("id = %v, want it scoped to the request", role["id"])
	}
	props := role["properties"].(map[string]any)
	if props["roleName"] != "AcrPull" || props["type"] != "BuiltInRole" {
		t.Errorf("properties = %v", props)
	}
	if _, ok := props["permissions"].([]any); !ok {
		t.Errorf("properties.permissions missing: %v", props)
	}

	// The roles this milestone's example needs, plus the ones any Azure
	// tutorial reaches for.
	for name, guid := range map[string]string{
		"Owner":                         "8e3af657-a8ff-443c-a75c-2fe8c4bcb635",
		"Contributor":                   "b24988ac-6180-42a0-ab88-20f7382dd24c",
		"Reader":                        "acdd72a7-3385-48ef-bd42-f606fba81ae7",
		"Key Vault Secrets User":        "4633458b-17de-408a-b874-0445c86b69e6",
		"Key Vault Secrets Officer":     "b86a8fe4-44ce-4948-aee5-eccb2c155cd7",
		"Key Vault Administrator":       "00482a5a-887f-4fb3-b363-3b7fe8e74483",
		"AcrPush":                       "8311e382-0749-4cb8-b61a-304f252e45ec",
		"Storage Blob Data Contributor": "ba92f5b4-2d11-453d-a403-e96b0029c9fe",
		"Managed Identity Operator":     "f1a07417-d97a-45cb-824c-7a7467783830",
		"Azure Kubernetes Service Cluster User Role": "4abbcc35-e782-43d8-92c5-2d3f1bd2253f",
	} {
		_, got := getQuery(t, srv, path, url.Values{"$filter": {"roleName eq '" + name + "'"}})
		l, _ := got["value"].([]any)
		if len(l) != 1 {
			t.Errorf("%q: %d definitions", name, len(l))
			continue
		}
		if n := l[0].(map[string]any)["name"]; n != guid {
			t.Errorf("%q GUID = %v, want %v", name, n, guid)
		}
	}

	// An unfiltered list is what a client uses to discover roles.
	_, all := getQuery(t, srv, path, url.Values{})
	if l, _ := all["value"].([]any); len(l) < 10 {
		t.Errorf("unfiltered list returned %d definitions", len(l))
	}

	// A role nobody emulates must come back empty rather than wrong: azurerm
	// then says it could not find the role, which is the truth.
	_, none := getQuery(t, srv, path, url.Values{"$filter": {"roleName eq 'Quantum Supervisor'"}})
	if l, _ := none["value"].([]any); len(l) != 0 {
		t.Errorf("unknown role returned %d definitions", len(l))
	}
}

// Having found the GUID, clients fetch the definition by ID -- it is what a
// role_definition_id in state points at.
func TestRoleDefinitionByID(t *testing.T) {
	srv, _ := newTestServer(t)
	path := sub + "/providers/Microsoft.Authorization/roleDefinitions/b24988ac-6180-42a0-ab88-20f7382dd24c"
	code, body := do(t, srv, "GET", path, nil)
	if code != 200 {
		t.Fatalf("GET role definition = %d: %v", code, body)
	}
	if body["properties"].(map[string]any)["roleName"] != "Contributor" {
		t.Errorf("body = %v", body)
	}

	code, body = do(t, srv, "GET", sub+"/providers/Microsoft.Authorization/roleDefinitions/"+newUUID(), nil)
	if code != 404 {
		t.Errorf("unknown role definition = %d, want 404: %v", code, body)
	}
}

// A role assignment is an extension resource: its path hangs off whatever it
// grants access to, which may be a subscription, a resource group or a single
// resource. All three have to work, because all three appear in real configs.
func TestRoleAssignmentLifecycle(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-ra"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	acr := rg + "/providers/Microsoft.ContainerRegistry/registries/acrnimbus"
	do(t, srv, "PUT", acr, map[string]any{"location": "eastus", "sku": map[string]any{"name": "Basic"}})
	principal := stableGUID("some-identity")
	roleDef := sub + "/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d"

	for _, scope := range []string{sub, rg, acr} {
		name := newUUID()
		path := scope + "/providers/Microsoft.Authorization/roleAssignments/" + name
		code, body := do(t, srv, "PUT", path, map[string]any{"properties": map[string]any{
			"roleDefinitionId": roleDef, "principalId": principal,
		}})
		if code != 201 {
			t.Fatalf("PUT assignment at %s = %d: %v", scope, code, body)
		}
		props := body["properties"].(map[string]any)
		if props["scope"] != scope {
			t.Errorf("scope = %v, want %v", props["scope"], scope)
		}
		if props["roleDefinitionId"] != roleDef || props["principalId"] != principal {
			t.Errorf("assignment properties = %v", props)
		}
		if props["principalType"] == nil || props["createdOn"] == nil {
			t.Errorf("missing generated fields: %v", props)
		}
		if body["name"] != name || body["type"] != "Microsoft.Authorization/roleAssignments" {
			t.Errorf("identity = %v / %v", body["name"], body["type"])
		}

		// createdOn must not move, or every refresh is a diff.
		_, again := do(t, srv, "PUT", path, map[string]any{"properties": map[string]any{
			"roleDefinitionId": roleDef, "principalId": principal,
		}})
		if again["properties"].(map[string]any)["createdOn"] != props["createdOn"] {
			t.Error("createdOn changed on re-PUT")
		}

		if code, _ := do(t, srv, "GET", path, nil); code != 200 {
			t.Errorf("GET assignment = %d", code)
		}
		code, _ = do(t, srv, "DELETE", path, nil)
		if code != 200 {
			t.Errorf("DELETE assignment = %d, want 200 (ARM deletes these synchronously)", code)
		}
		if code, _ := do(t, srv, "GET", path, nil); code != 404 {
			t.Errorf("assignment survived delete: %d", code)
		}
	}
}

// Assignments at a resource's scope are listed by clients checking for
// duplicates before creating one.
func TestRoleAssignmentsAreListedAtTheirScope(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-ralist"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	roleDef := sub + "/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7"
	for i := 0; i < 2; i++ {
		do(t, srv, "PUT", rg+"/providers/Microsoft.Authorization/roleAssignments/"+newUUID(),
			map[string]any{"properties": map[string]any{"roleDefinitionId": roleDef, "principalId": stableGUID("p")}})
	}
	code, body := do(t, srv, "GET", rg+"/providers/Microsoft.Authorization/roleAssignments", nil)
	if code != 200 {
		t.Fatalf("list = %d: %v", code, body)
	}
	if l, _ := body["value"].([]any); len(l) != 2 {
		t.Errorf("list returned %d assignments: %v", len(l), body)
	}
}

// A role assignment missing either half is a client bug, and a 400 naming the
// field is far more useful than storing a half-assignment that grants nothing.
func TestRoleAssignmentRequiresRoleAndPrincipal(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-rabad"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	path := rg + "/providers/Microsoft.Authorization/roleAssignments/" + newUUID()

	code, body := do(t, srv, "PUT", path, map[string]any{"properties": map[string]any{
		"principalId": stableGUID("p"),
	}})
	if code != 400 || errCode(body) != "InvalidRoleAssignment" {
		t.Errorf("missing roleDefinitionId = %d %s", code, errCode(body))
	}
	code, body = do(t, srv, "PUT", path, map[string]any{"properties": map[string]any{
		"roleDefinitionId": sub + "/providers/Microsoft.Authorization/roleDefinitions/acdd72a7-3385-48ef-bd42-f606fba81ae7",
	}})
	if code != 400 || errCode(body) != "InvalidRoleAssignment" {
		t.Errorf("missing principalId = %d %s", code, errCode(body))
	}
}
