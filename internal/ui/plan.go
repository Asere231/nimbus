package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Reading `terraform show -json <plan>` and reducing it to an overlay the
// visualizer can draw on the graph: what is about to be created, changed or
// destroyed, before anything has been.
//
// The awkward part, and the reason this is not a one-liner:
//
//	A RESOURCE ABOUT TO BE CREATED HAS NO ID.
//
// Terraform lists `id` under after_unknown for a create, because the ID is
// whatever the provider decides when the call is made -- measured against
// Terraform 1.16.4 and azurerm 4.81.0, where the create of a public IP has
// after.name, after.location and after.resource_group_name but an unknown
// after.id. Updates and deletes are easy by comparison: before.id is always
// there, because the resource already exists.
//
// So a create is matched by what IS known -- the provider type mapped to an
// ARM type, the name, and the resource group -- and drawn as a ghost next to
// the group it will appear in. A create we cannot place still shows, labelled
// with its Terraform address, rather than being quietly dropped.

// PlanChange is one pending change.
type PlanChange struct {
	Address       string `json:"address"`                 // azurerm_virtual_network.hub
	Action        string `json:"action"`                  // create, update, delete, replace
	ID            string `json:"id,omitempty"`            // ARM ID, when the plan knows it
	Type          string `json:"type,omitempty"`          // Microsoft.Network/virtualNetworks
	Name          string `json:"name,omitempty"`          //
	ResourceGroup string `json:"resourceGroup,omitempty"` // where a create will land
}

// Overlay is a whole plan, reduced.
type Overlay struct {
	Created  time.Time    `json:"created"`
	Source   string       `json:"source,omitempty"`
	Changes  []PlanChange `json:"changes"`
	Creates  int          `json:"creates"`
	Updates  int          `json:"updates"`
	Deletes  int          `json:"deletes"`
	Replaces int          `json:"replaces"`
}

// tfPlan is the slice of `terraform show -json` output that matters. The
// format is versioned (format_version 1.2 as of Terraform 1.16) and read
// defensively: an unknown field is ignored, and a plan that parses to no
// changes is reported as such rather than guessed at.
type tfPlan struct {
	FormatVersion    string `json:"format_version"`
	TerraformVersion string `json:"terraform_version"`
	ResourceChanges  []struct {
		Address string `json:"address"`
		Type    string `json:"type"`
		Name    string `json:"name"`
		Change  struct {
			Actions      []string       `json:"actions"`
			Before       map[string]any `json:"before"`
			After        map[string]any `json:"after"`
			AfterUnknown map[string]any `json:"after_unknown"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// ReadPlan parses `terraform show -json` output into an overlay.
func ReadPlan(r io.Reader) (Overlay, error) {
	var plan tfPlan
	if err := json.NewDecoder(r).Decode(&plan); err != nil {
		return Overlay{}, fmt.Errorf("reading the plan: %w", err)
	}
	if plan.FormatVersion == "" {
		return Overlay{}, fmt.Errorf("this is not `terraform show -json` output: no format_version")
	}

	out := Overlay{Created: time.Now().UTC()}
	for _, rc := range plan.ResourceChanges {
		action := actionOf(rc.Change.Actions)
		if action == "" {
			continue // no-op or read: nothing to draw
		}
		change := PlanChange{Address: rc.Address, Action: action}

		// before.id for anything that already exists; after.id only when the
		// plan actually knows it (it is listed in after_unknown otherwise).
		change.ID = str(rc.Change.Before["id"])
		if change.ID == "" && !unknown(rc.Change.AfterUnknown, "id") {
			change.ID = str(rc.Change.After["id"])
		}
		change.Type = armType(rc.Type)
		change.Name = str(rc.Change.After["name"])
		if change.Name == "" {
			change.Name = str(rc.Change.Before["name"])
		}
		change.ResourceGroup = str(rc.Change.After["resource_group_name"])
		if change.ResourceGroup == "" {
			change.ResourceGroup = str(rc.Change.Before["resource_group_name"])
		}

		switch action {
		case "create":
			out.Creates++
		case "update":
			out.Updates++
		case "delete":
			out.Deletes++
		case "replace":
			out.Replaces++
		}
		out.Changes = append(out.Changes, change)
	}
	sort.SliceStable(out.Changes, func(i, j int) bool { return out.Changes[i].Address < out.Changes[j].Address })
	return out, nil
}

// Summary is the one line `nimbus plan-view` prints.
func (o Overlay) Summary() string {
	if len(o.Changes) == 0 {
		return "no changes: the plan is a no-op"
	}
	parts := []string{}
	for _, p := range []struct {
		n    int
		word string
	}{{o.Creates, "to create"}, {o.Updates, "to change"}, {o.Replaces, "to replace"}, {o.Deletes, "to destroy"}} {
		if p.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", p.n, p.word))
		}
	}
	return strings.Join(parts, ", ")
}

// actionOf reduces Terraform's action list to one word. A replace is two
// actions in one entry, in either order: delete-then-create normally,
// create-then-delete when the resource has create_before_destroy set.
func actionOf(actions []string) string {
	switch len(actions) {
	case 1:
		switch actions[0] {
		case "create", "update", "delete":
			return actions[0]
		}
		return "" // no-op, read
	case 2:
		if (actions[0] == "delete" && actions[1] == "create") ||
			(actions[0] == "create" && actions[1] == "delete") {
			return "replace"
		}
	}
	return ""
}

func unknown(afterUnknown map[string]any, key string) bool {
	v, ok := afterUnknown[key]
	if !ok {
		return false
	}
	b, _ := v.(bool)
	return b
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// armTypes maps the azurerm resource types used in the examples to the ARM
// types the graph is keyed by. It is deliberately partial: a type that is
// not here still draws, labelled with its Terraform address, which is
// honest about what we know. Add a line when an example needs one.
var armTypes = map[string]string{
	"azurerm_resource_group":               "Microsoft.Resources/resourceGroups",
	"azurerm_virtual_network":              "Microsoft.Network/virtualNetworks",
	"azurerm_subnet":                       "Microsoft.Network/virtualNetworks/subnets",
	"azurerm_network_security_group":       "Microsoft.Network/networkSecurityGroups",
	"azurerm_route_table":                  "Microsoft.Network/routeTables",
	"azurerm_public_ip":                    "Microsoft.Network/publicIPAddresses",
	"azurerm_network_interface":            "Microsoft.Network/networkInterfaces",
	"azurerm_nat_gateway":                  "Microsoft.Network/natGateways",
	"azurerm_storage_account":              "Microsoft.Storage/storageAccounts",
	"azurerm_storage_container":            "Microsoft.Storage/storageAccounts/blobServices/containers",
	"azurerm_kubernetes_cluster":           "Microsoft.ContainerService/managedClusters",
	"azurerm_kubernetes_cluster_node_pool": "Microsoft.ContainerService/managedClusters/agentPools",
	"azurerm_container_registry":           "Microsoft.ContainerRegistry/registries",
	"azurerm_key_vault":                    "Microsoft.KeyVault/vaults",
	"azurerm_user_assigned_identity":       "Microsoft.ManagedIdentity/userAssignedIdentities",
	"azurerm_role_assignment":              "Microsoft.Authorization/roleAssignments",
	"azurerm_log_analytics_workspace":      "Microsoft.OperationalInsights/workspaces",
	"azurerm_service_plan":                 "Microsoft.Web/serverfarms",
	"azurerm_linux_web_app":                "Microsoft.Web/sites",
	"azurerm_windows_web_app":              "Microsoft.Web/sites",
	"azurerm_linux_virtual_machine":        "Microsoft.Compute/virtualMachines",
	"azurerm_windows_virtual_machine":      "Microsoft.Compute/virtualMachines",
	"azurerm_managed_disk":                 "Microsoft.Compute/disks",
}

func armType(tfType string) string {
	return armTypes[strings.ToLower(tfType)]
}
