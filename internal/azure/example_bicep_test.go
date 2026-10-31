package azure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The example under examples/bicep is what the milestone is judged on, so it
// is deployed here exactly as `az deployment group create` would deploy it:
// the compiled JSON, which is what az actually sends after running the Bicep
// compiler locally.
//
// Deploying it in a Go test means CI proves the example without needing the
// az CLI or the Bicep compiler installed.
func TestBicepExampleDeploys(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "bicep", "main.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	s, rg := newTestServerWithGroup(t)
	got := deploy(t, s, rg, "bicep-example", string(raw), nil)

	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"
	for _, id := range []string{
		base + "Microsoft.Network/networkSecurityGroups/nimbus-nsg",
		base + "Microsoft.Network/virtualNetworks/nimbus-vnet",
	} {
		if !s.store.Exists(id) {
			t.Errorf("the example did not create %s", id)
		}
	}

	// The subnets come from a property-level copy loop, which is what a Bicep
	// `for` inside a property compiles to.
	vnet, err := s.store.Get(base + "Microsoft.Network/virtualNetworks/nimbus-vnet")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(vnet.Body, &body); err != nil {
		t.Fatal(err)
	}
	props, _ := body["properties"].(map[string]any)
	subnets, _ := props["subnets"].([]any)
	if len(subnets) != 2 {
		t.Fatalf("the VNet has %d subnets, want 2 from the copy loop", len(subnets))
	}
	for i, raw := range subnets {
		sn, _ := raw.(map[string]any)
		if sn["name"] != "snet-"+string(rune('0'+i)) {
			t.Errorf("subnet %d is named %v", i, sn["name"])
		}
		sp, _ := sn["properties"].(map[string]any)
		if sp["addressPrefix"] != "10.20."+string(rune('0'+i))+".0/24" {
			t.Errorf("subnet %d prefix = %v", i, sp["addressPrefix"])
		}
		nsgRef, _ := sp["networkSecurityGroup"].(map[string]any)
		if !strings.HasSuffix(toStr(nsgRef["id"]), "/networkSecurityGroups/nimbus-nsg") {
			t.Errorf("subnet %d does not point at the NSG: %v", i, nsgRef["id"])
		}
	}
	// ...and they are mirrored as child resources, by the VNet's own hook.
	if children := s.store.Children(base + "Microsoft.Network/virtualNetworks/nimbus-vnet"); len(children) != 2 {
		t.Errorf("the VNet has %d child subnets in the store, want 2", len(children))
	}

	// The storage account's name comes from uniqueString, so it is not known
	// here -- only that it exists and that the output names it.
	dprops, _ := got["properties"].(map[string]any)
	outputs, _ := dprops["outputs"].(map[string]any)
	nameOut, _ := outputs["storageAccountName"].(map[string]any)
	account := toStr(nameOut["value"])
	if !strings.HasPrefix(account, "st") || len(account) != 15 {
		t.Fatalf("storageAccountName output = %q, want st + 13 characters", account)
	}
	if !s.store.Exists(base + "Microsoft.Storage/storageAccounts/" + account) {
		t.Errorf("the storage account %q was not created", account)
	}

	// nsgRules counts what reference() read back off a deployed resource.
	rules, _ := outputs["nsgRules"].(map[string]any)
	if rules["value"] != float64(1) {
		t.Errorf("nsgRules output = %v, want 1", rules["value"])
	}
}

// Deploying the example twice must be a no-op the second time: that is what
// makes a template safe to re-run, and it is the same stability property the
// Terraform examples assert with `plan -detailed-exitcode`.
func TestBicepExampleIsRepeatable(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "bicep", "main.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, rg := newTestServerWithGroup(t)
	deploy(t, s, rg, "again-1", string(raw), nil)

	base := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg + "/providers/"
	before, err := s.store.Get(base + "Microsoft.Network/virtualNetworks/nimbus-vnet")
	if err != nil {
		t.Fatal(err)
	}
	deploy(t, s, rg, "again-2", string(raw), nil)
	after, err := s.store.Get(base + "Microsoft.Network/virtualNetworks/nimbus-vnet")
	if err != nil {
		t.Fatal(err)
	}
	if string(before.Body) != string(after.Body) {
		t.Errorf("redeploying changed the VNet\nbefore: %s\nafter:  %s", before.Body, after.Body)
	}
}
