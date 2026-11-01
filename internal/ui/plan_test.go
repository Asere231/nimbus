package ui

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is real output: `terraform plan -out plan.out && terraform
// show -json plan.out` against Nimbus with azurerm 4.81.0 and Terraform
// 1.16.4, with a tag added to a VNet and a public IP added to the config.
// Only the keys this code reads are kept, so the file stays reviewable.
func realPlan(t *testing.T) Overlay {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "test", "ui", "testdata", "terraform-plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	overlay, err := ReadPlan(f)
	if err != nil {
		t.Fatal(err)
	}
	return overlay
}

func TestReadPlanKeepsOnlyRealChanges(t *testing.T) {
	overlay := realPlan(t)
	// Eight resource_changes, six of them no-ops.
	if len(overlay.Changes) != 2 {
		t.Fatalf("got %d changes, want 2: no-ops must be dropped", len(overlay.Changes))
	}
	if overlay.Creates != 1 || overlay.Updates != 1 || overlay.Deletes != 0 {
		t.Errorf("counts = %+v", overlay)
	}
	if got := overlay.Summary(); got != "1 to create, 1 to change" {
		t.Errorf("summary = %q", got)
	}
}

// An update names a resource that already exists, so it carries its ARM ID
// and matches a card directly.
func TestAnUpdateCarriesTheResourceID(t *testing.T) {
	var update PlanChange
	for _, c := range realPlan(t).Changes {
		if c.Action == "update" {
			update = c
		}
	}
	if update.Address != "azurerm_virtual_network.hub" {
		t.Fatalf("update = %+v", update)
	}
	if !strings.HasSuffix(update.ID, "/virtualNetworks/vnet-hub-dev") {
		t.Errorf("id = %q, want the VNet's ARM id", update.ID)
	}
	if update.Type != "Microsoft.Network/virtualNetworks" {
		t.Errorf("type = %q", update.Type)
	}
	if update.Name != "vnet-hub-dev" {
		t.Errorf("name = %q", update.Name)
	}
}

// The awkward one, and the reason the overlay carries a type and a group at
// all: Terraform lists `id` under after_unknown for a create, because the
// provider has not been called yet.
func TestACreateHasNoIDButEnoughToPlaceIt(t *testing.T) {
	var create PlanChange
	for _, c := range realPlan(t).Changes {
		if c.Action == "create" {
			create = c
		}
	}
	if create.Address != "azurerm_public_ip.egress" {
		t.Fatalf("create = %+v", create)
	}
	if create.ID != "" {
		t.Errorf("id = %q, want empty: Terraform does not know it yet", create.ID)
	}
	if create.Type != "Microsoft.Network/publicIPAddresses" {
		t.Errorf("type = %q, want the mapped ARM type", create.Type)
	}
	if create.Name != "pip-egress" || create.ResourceGroup != "rg-platform-dev" {
		t.Errorf("name/group = %q/%q", create.Name, create.ResourceGroup)
	}
}

func TestActionOfCollapsesTerraformsActionLists(t *testing.T) {
	cases := []struct {
		actions []string
		want    string
	}{
		{[]string{"create"}, "create"},
		{[]string{"update"}, "update"},
		{[]string{"delete"}, "delete"},
		{[]string{"no-op"}, ""},
		{[]string{"read"}, ""},
		// A replace is two actions in one entry, in either order:
		// delete-then-create normally, the other way round when the
		// resource has create_before_destroy set.
		{[]string{"delete", "create"}, "replace"},
		{[]string{"create", "delete"}, "replace"},
		{nil, ""},
	}
	for _, c := range cases {
		if got := actionOf(c.actions); got != c.want {
			t.Errorf("actionOf(%v) = %q, want %q", c.actions, got, c.want)
		}
	}
}

// A provider type nobody has mapped still produces a change, so it is drawn
// and labelled rather than silently missing from the overlay.
func TestAnUnmappedTypeStillYieldsAChange(t *testing.T) {
	const plan = `{
	  "format_version": "1.2",
	  "resource_changes": [{
	    "address": "azurerm_something_new.x", "type": "azurerm_something_new", "name": "x",
	    "change": {"actions": ["create"], "before": null, "after": {"name": "thing"},
	               "after_unknown": {"id": true}}
	  }]
	}`
	overlay, err := ReadPlan(strings.NewReader(plan))
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay.Changes) != 1 {
		t.Fatalf("got %d changes", len(overlay.Changes))
	}
	if overlay.Changes[0].Type != "" {
		t.Errorf("type = %q, want empty for a type nobody mapped", overlay.Changes[0].Type)
	}
	if overlay.Changes[0].Address != "azurerm_something_new.x" {
		t.Error("the address is all we have left to label it with")
	}
}

func TestReadPlanRejectsSomethingThatIsNotAPlan(t *testing.T) {
	if _, err := ReadPlan(strings.NewReader(`{"hello": "world"}`)); err == nil {
		t.Error("JSON that is not a plan should be refused")
	} else if !strings.Contains(err.Error(), "format_version") {
		t.Errorf("error = %v, want one that says what is missing", err)
	}
	if _, err := ReadPlan(strings.NewReader("not json at all")); err == nil {
		t.Error("garbage should be refused")
	}
}

func TestEmptyPlanSaysSo(t *testing.T) {
	overlay, err := ReadPlan(strings.NewReader(`{"format_version":"1.2","resource_changes":[
	  {"address":"a.b","type":"azurerm_resource_group","name":"b","change":{"actions":["no-op"]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay.Changes) != 0 {
		t.Errorf("got %d changes", len(overlay.Changes))
	}
	if got := overlay.Summary(); !strings.Contains(got, "no changes") {
		t.Errorf("summary = %q", got)
	}
}

// The endpoint the CLI pushes to, and the browser polls.
func TestPlanEndpointHoldsOneOverlayAtATime(t *testing.T) {
	srv, _, _ := newUI(t)

	var empty Overlay
	if code := getJSON(t, srv.URL+"/api/plan", &empty); code != 200 {
		t.Fatalf("GET /api/plan = %d", code)
	}
	if len(empty.Changes) != 0 {
		t.Error("a fresh server has no plan")
	}

	body := `{"changes":[{"address":"azurerm_public_ip.x","action":"create","name":"pip",
	  "type":"Microsoft.Network/publicIPAddresses","resourceGroup":"rg"}],"creates":1}`
	res, err := http.Post(srv.URL+"/api/plan", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("POST /api/plan = %d", res.StatusCode)
	}

	var loaded Overlay
	getJSON(t, srv.URL+"/api/plan", &loaded)
	if len(loaded.Changes) != 1 || loaded.Changes[0].Action != "create" {
		t.Fatalf("loaded = %+v", loaded)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/plan", nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	var cleared Overlay
	getJSON(t, srv.URL+"/api/plan", &cleared)
	if len(cleared.Changes) != 0 {
		t.Error("DELETE should remove the overlay")
	}

	res, err = http.Post(srv.URL+"/api/plan", "application/json", strings.NewReader("{{{"))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 400 {
		t.Errorf("POST of broken JSON = %d, want 400", res.StatusCode)
	}
}
