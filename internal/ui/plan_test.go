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

// References between PENDING resources, read from a real `terraform show
// -json` of examples/terraform/gcp-basic.
//
// This is the one relationship a plan cannot express the usual way. A
// subnet's `network` is listed under after_unknown exactly as its own id
// is -- the network does not exist yet, so neither does the value -- and
// the relationship survives only in the configuration section, as
// `"references": ["google_compute_network.main.id",
// "google_compute_network.main"]`. Without it the network view cannot
// draw a pending subnet inside its pending network.
func TestAPlanCarriesReferencesBetweenPendingResources(t *testing.T) {
	f, err := os.Open(filepath.Join("..", "..", "test", "ui", "testdata", "terraform-plan-gcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	overlay, err := ReadPlan(f)
	if err != nil {
		t.Fatal(err)
	}

	byAddress := map[string]PlanChange{}
	for _, c := range overlay.Changes {
		byAddress[c.Address] = c
	}

	// The subnet names the network it will join.
	subnet, ok := byAddress["google_compute_subnetwork.private"]
	if !ok {
		t.Fatalf("the fixture has no subnet: %v", byAddress)
	}
	if len(subnet.References) != 1 || subnet.References[0] != "google_compute_network.main" {
		t.Errorf("the subnet references %v, want just the network", subnet.References)
	}
	// Terraform lists BOTH "…main.id" and "…main" for one reference, so a
	// duplicate here would mean the address was being parsed rather than
	// matched against the changes that exist.
	if _, isAttribute := byAddress["google_compute_network.main.id"]; isAttribute {
		t.Error("an attribute reference was taken for a resource address")
	}

	// The object names its bucket, which is the same mechanism on a
	// different pair -- so this is not a one-off for networking.
	object := byAddress["google_storage_bucket_object.readme"]
	if len(object.References) != 1 || object.References[0] != "google_storage_bucket.data" {
		t.Errorf("the object references %v, want just the bucket", object.References)
	}

	// A resource that points at nothing pending carries nothing.
	if refs := byAddress["google_compute_network.main"].References; len(refs) != 0 {
		t.Errorf("the network references %v, want none", refs)
	}
	if refs := byAddress["google_service_account.app"].References; len(refs) != 0 {
		t.Errorf("the service account references %v, want none", refs)
	}
}

// Only an address that is ITSELF pending is carried. A reference to a
// variable, a data source or a resource that already exists is matched by
// ID like anything else, and carrying it would give the browser an
// address it could never resolve to a ghost.
func TestOnlyPendingAddressesAreCarried(t *testing.T) {
	plan := `{
      "format_version": "1.2",
      "resource_changes": [
        {"address": "aws_subnet.a", "type": "aws_subnet", "name": "a",
         "change": {"actions": ["create"], "after": {"name": "a"}, "after_unknown": {"id": true}}}
      ],
      "configuration": {"root_module": {"resources": [
        {"address": "aws_subnet.a", "expressions": {
           "vpc_id": {"references": ["aws_vpc.already_there.id", "aws_vpc.already_there"]},
           "cidr_block": {"constant_value": "10.0.1.0/24"},
           "tags": {"Name": {"references": ["var.prefix"]}}
        }}
      ]}}
    }`
	overlay, err := ReadPlan(strings.NewReader(plan))
	if err != nil {
		t.Fatal(err)
	}
	if len(overlay.Changes) != 1 {
		t.Fatalf("expected one change, got %d", len(overlay.Changes))
	}
	if refs := overlay.Changes[0].References; len(refs) != 0 {
		t.Errorf("references to things that are not pending were carried: %v", refs)
	}
}
