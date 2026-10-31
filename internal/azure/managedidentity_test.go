package azure

import (
	"strings"
	"testing"
)

// A user-assigned identity is mostly server-generated: a client sends a
// location and gets three IDs back. azurerm stores all three and compares them
// on every refresh, so they must be stable for the life of the resource.
func TestUserAssignedIdentityHasStableIDs(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-id"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id-app"

	code, body := do(t, srv, "PUT", id, map[string]any{"location": "eastus"})
	if code != 201 {
		t.Fatalf("PUT identity = %d: %v", code, body)
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		t.Fatalf("identity has no properties: %v", body)
	}
	for _, f := range []string{"principalId", "clientId", "tenantId"} {
		v := str(props[f])
		if len(v) != 36 {
			t.Errorf("properties.%s = %q, want a GUID", f, v)
		}
	}
	if props["principalId"] == props["clientId"] {
		t.Error("principalId and clientId must differ")
	}

	// Stable across a re-PUT and a GET: these end up in Terraform state.
	_, again := do(t, srv, "PUT", id, map[string]any{"location": "eastus"})
	ap := again["properties"].(map[string]any)
	if ap["principalId"] != props["principalId"] || ap["clientId"] != props["clientId"] {
		t.Errorf("identity IDs changed on re-PUT: %v then %v", props, ap)
	}
	_, got := do(t, srv, "GET", id, nil)
	gp := got["properties"].(map[string]any)
	if gp["principalId"] != props["principalId"] {
		t.Errorf("GET reports a different principalId: %v", gp)
	}

	// A second identity is a different principal.
	_, other := do(t, srv, "PUT", rg+"/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id-other",
		map[string]any{"location": "eastus"})
	if other["properties"].(map[string]any)["principalId"] == props["principalId"] {
		t.Error("two identities share a principalId")
	}
}

// Any resource can carry an identity block, and the client reads the generated
// principalId straight back out of it -- that is how a role assignment gets
// something to point at. Filling this generically means a type Nimbus has
// never heard of still works.
func TestSystemAssignedIdentityIsFilledInGenerically(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-sai"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	app := rg + "/providers/Microsoft.Web/sites/app1"

	_, body := do(t, srv, "PUT", app, map[string]any{
		"location": "eastus",
		"identity": map[string]any{"type": "SystemAssigned"},
	})
	ident, _ := body["identity"].(map[string]any)
	if ident == nil {
		t.Fatalf("identity block vanished: %v", body)
	}
	principal := str(ident["principalId"])
	if len(principal) != 36 {
		t.Fatalf("identity.principalId = %q, want a GUID", principal)
	}
	if str(ident["tenantId"]) == "" {
		t.Error("identity.tenantId is empty")
	}

	// Unchanged by later writes, including a PATCH that does not mention it.
	_, patched := do(t, srv, "PATCH", app, map[string]any{"tags": map[string]any{"env": "dev"}})
	if p := patched["identity"].(map[string]any)["principalId"]; p != principal {
		t.Errorf("principalId changed on PATCH: %v, want %v", p, principal)
	}
}

// "SystemAssigned, UserAssigned" is one string, not two: the system identity
// still has to be filled in, and every user-assigned entry the client sent as
// an empty object has to come back with the ids of that identity.
func TestCombinedIdentityFillsBothHalves(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-both"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	uaID := rg + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id-app"
	_, ua := do(t, srv, "PUT", uaID, map[string]any{"location": "eastus"})
	want := ua["properties"].(map[string]any)

	// Referred to with different casing, which is what happens when the ID is
	// typed by hand or built from a variable rather than taken from the
	// identity resource. ARM resolves IDs case-insensitively, so this is the
	// same identity and must report the same principal.
	cased := strings.Replace(uaID, "/resourceGroups/", "/resourcegroups/", 1)
	_, body := do(t, srv, "PUT", rg+"/providers/Microsoft.Web/sites/app2", map[string]any{
		"location": "eastus",
		"identity": map[string]any{
			"type":                   "SystemAssigned, UserAssigned",
			"userAssignedIdentities": map[string]any{cased: map[string]any{}},
		},
	})
	ident := body["identity"].(map[string]any)
	if len(str(ident["principalId"])) != 36 {
		t.Errorf("system half not filled: %v", ident)
	}
	entries := ident["userAssignedIdentities"].(map[string]any)
	entry, ok := entries[cased].(map[string]any)
	if !ok {
		t.Fatalf("user-assigned entry missing: %v", entries)
	}
	// The ids must match the identity resource itself, or a client that reads
	// both sees two different principals for one identity.
	if entry["principalId"] != want["principalId"] || entry["clientId"] != want["clientId"] {
		t.Errorf("entry %v does not match the identity resource %v", entry, want)
	}
}

// UserAssigned on its own has no system principal: Azure returns none, and
// inventing one would make an "identity.principal_id" that is never usable.
func TestUserAssignedOnlyHasNoSystemPrincipal(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-uaonly"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	uaID := rg + "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/id-only"
	do(t, srv, "PUT", uaID, map[string]any{"location": "eastus"})

	_, body := do(t, srv, "PUT", rg+"/providers/Microsoft.Web/sites/app3", map[string]any{
		"location": "eastus",
		"identity": map[string]any{
			"type":                   "UserAssigned",
			"userAssignedIdentities": map[string]any{uaID: map[string]any{}},
		},
	})
	ident := body["identity"].(map[string]any)
	if _, ok := ident["principalId"]; ok {
		t.Errorf("UserAssigned identity must not get a system principalId: %v", ident)
	}
}

// The AKS hook fills its own identity and kubelet profile. The generic filler
// must not overwrite either -- M3 proved those exact values are what azurerm
// reads back.
func TestClusterIdentityStillComesFromTheAKSHook(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aksid"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	cl := rg + "/providers/Microsoft.ContainerService/managedClusters/aks-id"
	_, body := do(t, srv, "PUT", cl, aksBody(1))

	ident := body["identity"].(map[string]any)
	if ident["principalId"] != stableGUID(cl+"/principal") {
		t.Errorf("cluster principalId = %v, want the AKS hook's value", ident["principalId"])
	}
	props := body["properties"].(map[string]any)
	kubelet := props["identityProfile"].(map[string]any)["kubeletidentity"].(map[string]any)
	if !strings.HasPrefix(str(kubelet["resourceId"]), "/subscriptions/") {
		t.Errorf("kubelet resourceId = %v", kubelet["resourceId"])
	}
}
