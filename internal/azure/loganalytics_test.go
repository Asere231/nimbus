package azure

import "testing"

// A Log Analytics workspace is a stub here: nothing ingests logs. It exists
// because the AKS monitoring add-on, diagnostic settings and half the Azure
// modules on the registry refuse to apply without one, and because the two
// things they read -- customerId and the shared keys -- are server-generated.
func TestLogAnalyticsWorkspaceServerFields(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-law"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	ws := rg + "/providers/Microsoft.OperationalInsights/workspaces/law1"

	code, body := do(t, srv, "PUT", ws, map[string]any{
		"location":   "eastus",
		"properties": map[string]any{"sku": map[string]any{"name": "PerGB2018"}, "retentionInDays": float64(30)},
	})
	if code != 201 {
		t.Fatalf("PUT workspace = %d: %v", code, body)
	}
	props := body["properties"].(map[string]any)
	if len(str(props["customerId"])) != 36 {
		t.Errorf("customerId = %v, want a GUID", props["customerId"])
	}
	if props["retentionInDays"] != float64(30) {
		t.Errorf("retentionInDays = %v, the client's value must survive", props["retentionInDays"])
	}
	for _, f := range []string{"sku", "features", "workspaceCapping", "createdDate", "publicNetworkAccessForIngestion"} {
		if props[f] == nil {
			t.Errorf("missing %q", f)
		}
	}

	// customerId and createdDate are identity, not state: they must not move.
	_, again := do(t, srv, "PUT", ws, map[string]any{
		"location": "eastus", "properties": map[string]any{"retentionInDays": float64(60)},
	})
	ap := again["properties"].(map[string]any)
	if ap["customerId"] != props["customerId"] || ap["createdDate"] != props["createdDate"] {
		t.Errorf("workspace identity changed on update: %v then %v", props, ap)
	}
	if ap["retentionInDays"] != float64(60) {
		t.Errorf("retentionInDays did not update: %v", ap["retentionInDays"])
	}
}

// Keys come from a POST action, never from a GET -- same rule as storage.
func TestLogAnalyticsSharedKeys(t *testing.T) {
	srv, _ := newTestServer(t)
	rg := sub + "/resourceGroups/rg-lawk"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	ws := rg + "/providers/Microsoft.OperationalInsights/workspaces/law2"
	do(t, srv, "PUT", ws, map[string]any{"location": "eastus"})

	code, keys := do(t, srv, "POST", ws+"/sharedKeys", nil)
	if code != 200 {
		t.Fatalf("sharedKeys = %d: %v", code, keys)
	}
	primary, secondary := str(keys["primarySharedKey"]), str(keys["secondarySharedKey"])
	if primary == "" || secondary == "" {
		t.Fatalf("shared keys missing: %v", keys)
	}
	if primary == secondary {
		t.Error("both shared keys are the same value")
	}
	// Stable: the agent config a client writes from these must keep matching.
	_, again := do(t, srv, "POST", ws+"/sharedKeys", nil)
	if str(again["primarySharedKey"]) != primary {
		t.Error("primarySharedKey changed between calls")
	}
}
