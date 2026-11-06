package azure

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// Hook lets a resource type add behaviour on top of the generic handler:
// validation, server-generated fields, keeping parents in sync, or starting a
// real local engine (k3s for AKS, a store for blobs, ...).
type Hook struct {
	// BeforePut can modify the body that will be stored, or return an
	// *armError to reject the request. existing is nil on create.
	BeforePut func(s *Server, t target, body map[string]any, existing map[string]any) *armError
	// AfterPut runs once the resource is stored.
	AfterPut func(s *Server, t target, body map[string]any)
	// AfterDelete runs once the resource (and its children) are gone. body is
	// the resource as it was last stored, because a hook cleaning up after a
	// resource usually needs more than its ID -- a vault's location, say.
	AfterDelete func(s *Server, t target, body map[string]any)
	// Backing is "real" when a local engine actually runs this type.
	Backing string
	// CreateStatus overrides the status a successful create returns. Most
	// types are happy with 201, but a few are declared long-running in the
	// SDK and their generated clients reject anything else:
	//
	//	performing Create: unexpected status 201 (201 Created)
	//
	// Storage accounts are the first such type. Zero means 201.
	CreateStatus int
	// DeleteStatus overrides the status a successful delete returns. Most
	// deletes are long-running and answer 202 with an async-operation header,
	// but some are synchronous and their clients reject the 202:
	//
	//	unexpected status 202 (202 Accepted) received with no body
	//
	// Zero means 202.
	DeleteStatus int
	// DeleteReturnsBody makes a successful delete answer with the resource
	// that was removed, which is what ARM does for a few types -- role
	// assignments among them. Most types answer an empty body, so this is
	// opt-in rather than the default.
	DeleteReturnsBody bool
	// Provision runs in the BACKGROUND after the resource is stored, for types
	// backed by something that genuinely takes time to start -- a k3s
	// container, not a map entry.
	//
	// The write answers immediately with provisioningState "Creating" and an
	// operation the client can poll. When Provision returns, the resource
	// moves to Succeeded, or to Failed carrying the error, and the operation
	// does the same. A type without this stays fully synchronous.
	Provision func(s *Server, t target, body map[string]any) error
	// Deprovision is the teardown counterpart, run in the background on
	// delete. AfterDelete still runs synchronously for bookkeeping; this is
	// for stopping engines, which can be slow.
	Deprovision func(s *Server, t target)
}

type armError struct {
	Status  int
	Code    string
	Message string
}

func (s *Server) hookFor(typ string) Hook { return s.hooks[strings.ToLower(typ)] }

func (s *Server) registerHook(typ string, h Hook) { s.hooks[strings.ToLower(typ)] = h }

// handleARM is the generic Resource Manager implementation. It accepts any
// resource type (the "accepted" tier) and defers to hooks for richer ones.
func (s *Server) handleARM(w http.ResponseWriter, r *http.Request) string {
	t, ok := parseTarget(r.URL.Path)
	if !ok {
		writeError(w, 404, "InvalidResourceType", "Nimbus does not recognise the path %q", r.URL.Path)
		return "unrecognised path"
	}
	if t.Subscription != "" {
		s.ensureSubscription(t.Subscription)
	}

	// Role definitions are not stored resources: Azure's built-in roles exist
	// in every subscription and nobody creates them, so they are served from
	// a table instead of the store. Both a list (filtered by role name) and a
	// read by GUID land here.
	if eq(t.Type, typeRoleDefinition) {
		return s.handleRoleDefinitions(w, r, t)
	}
	// Soft-deleted vaults and vault name availability are served the same
	// way: they are subscription-level Key Vault endpoints, not resources
	// anyone creates.
	if isKeyVaultSubscriptionPath(t) {
		return s.handleKeyVaultSubscriptionPath(w, r, t)
	}
	// A deployment's operations list is a collection with no resources of its
	// own: it is served from what the deployment recorded as it ran.
	if isDeploymentOperationsPath(t) && r.Method == http.MethodGet {
		return s.handleDeploymentOperations(w, t)
	}

	switch t.Kind {
	case "subscriptions":
		s.listSubscriptions(w)
	case "subscription":
		s.getSubscription(w, t)
	case "locations":
		s.listLocations(w, t)
	case "providers":
		s.listProviders(w)
	case "provider", "providerRegister":
		writeJSON(w, 200, providerJSON(t.Namespace))
	case "resourceGroups":
		s.listByFilter(w, func(res *core.Resource) bool {
			return eq(res.Type, "Microsoft.Resources/resourceGroups") && eq(res.ParentID, "/subscriptions/"+t.Subscription)
		})
	case "subResources":
		// A subscription-wide resource list. azurerm populates its Key Vault
		// cache from this: it asks for every Microsoft.KeyVault/vaults in the
		// subscription so it can turn a vault URI back into a resource ID.
		want := resourceTypeFilter(r.URL.Query().Get("$filter"))
		subID := "/subscriptions/" + t.Subscription
		s.listByFilter(w, func(res *core.Resource) bool {
			if !strings.HasPrefix(strings.ToLower(res.ID), strings.ToLower(subID)+"/") {
				return false
			}
			if eq(res.Type, "Microsoft.Resources/resourceGroups") {
				return false // groups are not listed as resources
			}
			return want == "" || eq(res.Type, want)
		})
	case "rgResources":
		if !s.store.Exists(t.ID) {
			writeError(w, 404, "ResourceGroupNotFound", "Resource group '%s' could not be found.", t.ResourceGroup)
			return ""
		}
		s.listByFilter(w, func(res *core.Resource) bool { return eq(res.ParentID, t.ID) })
	case "rgAction":
		writeJSON(w, 200, map[string]any{})
		return "unimplemented resource group action: " + t.Action
	case "resourceGroup", "resource":
		return s.handleResource(w, r, t)
	case "collection":
		if r.Method == http.MethodPost {
			return s.handleAction(w, r, t)
		}
		s.listCollection(w, t)
	default:
		// Unreachable today, but a new target kind that forgot its case here
		// would otherwise return an empty 200 and be very hard to debug.
		writeError(w, 500, "InternalError", "Nimbus parsed %q as kind %q and has no handler for it", r.URL.Path, t.Kind)
		return "unhandled target kind: " + t.Kind
	}
	return ""
}

func (s *Server) handleResource(w http.ResponseWriter, r *http.Request, t target) string {
	switch r.Method {
	case http.MethodGet:
		res, err := s.store.Get(t.ID)
		if err != nil {
			notFound(w, t)
			return ""
		}
		writeRaw(w, 200, res.Body)
	case http.MethodHead:
		if s.store.Exists(t.ID) {
			w.WriteHeader(204)
		} else {
			w.WriteHeader(404)
		}
	case http.MethodPut:
		return s.put(w, r, t, false)
	case http.MethodPatch:
		return s.put(w, r, t, true)
	case http.MethodDelete:
		deleted, err := s.store.Delete(t.ID)
		if err != nil {
			w.WriteHeader(204) // ARM: deleting something absent is a no-op
			return "already absent"
		}
		s.runDeleteHooks(deleted)
		s.runDeprovision(deleted)
		if h := s.hookFor(t.Type); h.DeleteStatus != 0 {
			if h.DeleteReturnsBody && len(deleted) > 0 {
				writeRaw(w, h.DeleteStatus, deleted[len(deleted)-1].Body)
				return ""
			}
			w.WriteHeader(h.DeleteStatus)
			return ""
		}
		s.markAsync(w, t)
		w.WriteHeader(202)
	default:
		writeError(w, 405, "MethodNotAllowed", "%s is not supported on %s", r.Method, t.Type)
	}
	return ""
}

// put creates, replaces (PUT) or merges (PATCH) a resource. Everything
// completes synchronously with provisioningState Succeeded, which the Azure
// SDK's provisioning-state poller accepts on its first check.
func (s *Server) put(w http.ResponseWriter, r *http.Request, t target, patch bool) string {
	body, err := readBody(r)
	if err != nil {
		writeError(w, 400, "InvalidRequestContent", "%v", err)
		return ""
	}

	// The parent must exist (a VNet needs its resource group, a subnet its VNet).
	if t.Kind == "resource" && t.ParentID != "" && !s.store.Exists(t.ParentID) {
		if t.ResourceGroup != "" && !s.store.Exists("/subscriptions/"+t.Subscription+"/resourceGroups/"+t.ResourceGroup) {
			writeError(w, 404, "ResourceGroupNotFound", "Resource group '%s' could not be found.", t.ResourceGroup)
			return ""
		}
		writeError(w, 404, "ParentResourceNotFound", "Can not perform requested operation on nested resource. Parent resource '%s' not found.", t.ParentID)
		return ""
	}

	var existing map[string]any
	if res, err := s.store.Get(t.ID); err == nil {
		_ = json.Unmarshal(res.Body, &existing)
	}
	if patch {
		if existing == nil {
			notFound(w, t)
			return ""
		}
		body = mergeMaps(deepCopy(existing), body)
	}

	// Canonical fields every ARM resource carries.
	//
	// id and name come from the path, but only on create. ARM resolves IDs
	// case-insensitively while always answering with the casing used at
	// creation, and a resource's name is immutable. Taking them from the path
	// on every write would let a client that happens to use different casing
	// rename the resource under Terraform's feet, which then sees the name it
	// holds in state disagree with ours on every read.
	if existing != nil {
		if id, ok := existing["id"].(string); ok && id != "" {
			t.ID = id
		}
		if name, ok := existing["name"].(string); ok && name != "" {
			t.Name = name
		}
	}
	// From here on t carries the canonical identity, so hooks, the store key
	// and the async-operation URL all agree with what reads will report.
	body["id"] = t.ID
	body["name"] = t.Name
	body["type"] = t.Type
	if loc, ok := body["location"].(string); ok {
		body["location"] = normalizeLocation(loc)
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	props["provisioningState"] = "Succeeded"
	body["properties"] = props

	// Any type can carry an identity block, so it is completed here rather
	// than in a per-type hook. Hooks run after this and can still override.
	fillIdentity(s, t, body, existing)

	// Fields Microsoft's own specs say the server owns, for the types nobody
	// wrote a hook for. Runs before the hook so a hook always wins.
	applySpecDefaults(t, body)

	h := s.hookFor(t.Type)
	if h.BeforePut != nil {
		if aerr := h.BeforePut(s, t, body, existing); aerr != nil {
			writeError(w, aerr.Status, aerr.Code, "%s", aerr.Message)
			return ""
		}
	}

	raw, _ := json.Marshal(body)
	loc, _ := body["location"].(string)
	// Three tiers, which the visualizer draws and counts: a local engine
	// really runs it, the shape is modelled (a hook or the spec table), or it
	// is merely accepted and stored.
	backing := h.Backing
	if backing == "" {
		backing = "state"
		if s.isModelled(t.Type) {
			backing = "modelled"
		}
	}
	created := s.store.Put(&core.Resource{
		Cloud: "azure", ID: t.ID, Type: t.Type, Name: t.Name, ParentID: t.ParentID,
		Location: loc, Backing: backing, Body: raw,
	})
	if h.AfterPut != nil {
		h.AfterPut(s, t, body)
	}

	status := 200
	if created && !patch {
		status = 201
		if h.CreateStatus != 0 {
			status = h.CreateStatus
		}
	}

	// A type that provisions something real answers now and finishes later.
	if h.Provision != nil && t.Kind == "resource" {
		op := s.ops.start(t.ID)
		s.setProvisioningState(t.ID, "Creating")
		go s.provision(h, t, deepCopy(body), op)

		if res, err := s.store.Get(t.ID); err == nil {
			raw = res.Body
		}
		s.markAsyncOperation(w, t, op)
		writeRaw(w, status, raw)
		return "provisioning " + t.Type
	}
	// Re-read: an AfterPut hook may have enriched the stored body.
	if res, err := s.store.Get(t.ID); err == nil {
		raw = res.Body
	}
	// Only real resources advertise an async operation, matching ARM: a
	// resource group PUT is synchronous there. Adding the header here does NOT
	// speed up azurerm, which ignores it for resource groups — see the note on
	// TestResourceGroupWritesAreSynchronous.
	if t.Kind == "resource" {
		s.markAsync(w, t)
	}
	writeRaw(w, status, raw)
	if t.Kind == "resource" && !s.isModelled(t.Type) {
		return "accepted (state only): " + t.Type
	}
	return ""
}

// runDeprovision starts background teardown for every removed resource that
// has something real behind it. The ARM delete has already been answered, as
// Azure does: the resource is gone from the API immediately while the engine
// winds down.
func (s *Server) runDeprovision(deleted []*core.Resource) {
	for _, res := range deleted {
		h := s.hookFor(res.Type)
		if h.Deprovision == nil {
			continue
		}
		dt, ok := parseTarget(res.ID)
		if !ok {
			dt = target{Kind: "resource", ID: res.ID, Type: res.Type, Name: res.Name, ParentID: res.ParentID}
		}
		go func(h Hook, dt target) {
			defer func() { _ = recover() }() // teardown must never crash the server
			h.Deprovision(s, dt)
		}(h, dt)
	}
}

// provision runs a type's Provision hook and records the outcome on both the
// resource and the operation.
//
// Anything that goes wrong has to land somewhere the user will see it. A
// resource left on "Creating" with no failed operation is the worst outcome:
// Terraform waits, then times out after minutes, with nothing explaining why.
func (s *Server) provision(h Hook, t target, body map[string]any, op *operation) {
	defer func() {
		// A panic in a hook must not take the server down, and must not leave
		// the operation InProgress forever either.
		if r := recover(); r != nil {
			aerr := &armError{Status: 500, Code: "ProvisioningFailed",
				Message: fmt.Sprintf("provisioning %s panicked: %v", t.Type, r)}
			s.failProvision(t, op, aerr)
		}
	}()

	if err := h.Provision(s, t, body); err != nil {
		s.failProvision(t, op, &armError{
			Status: 500, Code: "ProvisioningFailed", Message: err.Error(),
		})
		return
	}
	s.setProvisioningState(t.ID, "Succeeded")
	s.ops.finish(op.ID, nil)
	s.bus.Publish(core.Event{
		Kind: "resource", Action: "update", Cloud: "azure",
		ResourceID: t.ID, ResourceType: t.Type, Name: t.Name,
		Note: "provisioned",
	})
}

func (s *Server) failProvision(t target, op *operation, aerr *armError) {
	s.setProvisioningState(t.ID, "Failed")
	s.ops.finish(op.ID, aerr)
	s.bus.Publish(core.Event{
		Kind: "resource", Action: "update", Cloud: "azure",
		ResourceID: t.ID, ResourceType: t.Type, Name: t.Name,
		Status: aerr.Status, Note: "provisioning failed: " + aerr.Message,
	})
}

// setProvisioningState rewrites just properties.provisioningState on a stored
// resource, leaving everything else as it was.
func (s *Server) setProvisioningState(id, state string) {
	// A resource that was deleted while provisioning simply is not there any
	// more, which is not an error worth reporting to anybody.
	_ = s.store.Update(id, func(body map[string]any) error {
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			props = map[string]any{}
		}
		props["provisioningState"] = state
		body["properties"] = props
		return nil
	})
}

// errNoChange abandons a store.Update without storing anything. It is for
// the case where the body turned out not to be the shape the caller needs,
// which is never worth failing a request over.
var errNoChange = errors.New("nothing to change")

// runDeleteHooks runs AfterDelete for every resource a cascade removed, not
// just the one that was addressed.
//
// store.Delete returns them deepest first, and that order is kept: a child
// cleans up before its parent, so it can still see the parent while doing so.
// Without this, deleting a resource group would silently leave behind whatever
// real engines its contents were running.
func (s *Server) runDeleteHooks(deleted []*core.Resource) {
	for _, res := range deleted {
		h := s.hookFor(res.Type)
		if h.AfterDelete == nil {
			continue
		}
		// Rebuild the target from the stored resource: the path that triggered
		// the delete addressed an ancestor, so t cannot be reused.
		dt, ok := parseTarget(res.ID)
		if !ok {
			dt = target{Kind: "resource", ID: res.ID, Type: res.Type, Name: res.Name, ParentID: res.ParentID}
		}
		var body map[string]any
		_ = json.Unmarshal(res.Body, &body)
		h.AfterDelete(s, dt, body)
	}
}

// handleAction answers POST actions such as listKeys. Unknown actions return
// an empty object and are flagged in the activity feed, so missing features
// are easy to spot while you work.
func (s *Server) handleAction(w http.ResponseWriter, r *http.Request, t target) string {
	body, err := readBody(r)
	if err != nil {
		writeError(w, 400, "InvalidRequestContent", "%v", err)
		return ""
	}
	// validate and what-if are asked about a deployment that does not exist
	// yet -- that is the point of them -- so they come before the existence
	// check every other action needs.
	if s.deploymentAction(w, t, body) {
		return ""
	}
	if t.ID != "" && !s.store.Exists(t.ID) {
		writeError(w, 404, "ResourceNotFound", "The Resource '%s' was not found.", t.ID)
		return ""
	}
	if out, ok := s.clusterAction(t, body); ok {
		writeJSON(w, 200, out)
		return ""
	}
	if out, ok := s.storageAccountAction(t, body); ok {
		writeJSON(w, 200, out)
		return ""
	}
	if out, ok := s.registryAction(t); ok {
		writeJSON(w, 200, out)
		return ""
	}
	if out, ok := s.workspaceAction(t); ok {
		writeJSON(w, 200, out)
		return ""
	}
	writeJSON(w, 200, map[string]any{})
	return "unimplemented action: " + t.Action
}

// resourceTypeFilter pulls the type out of an OData filter such as
// "resourceType eq 'Microsoft.KeyVault/vaults'". An unrecognised filter means
// no filter: over-reporting is recoverable, inventing a match is not.
func resourceTypeFilter(filter string) string {
	low := strings.ToLower(filter)
	i := strings.Index(low, "resourcetype eq ")
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(filter[i+len("resourcetype eq "):])
	if rest == "" || (rest[0] != '\'' && rest[0] != '"') {
		return ""
	}
	quote := rest[0]
	end := strings.IndexByte(rest[1:], quote)
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

func (s *Server) listCollection(w http.ResponseWriter, t target) {
	subID := "/subscriptions/" + t.Subscription
	s.listByFilter(w, func(res *core.Resource) bool {
		if !eq(res.Type, t.Type) {
			return false
		}
		if eq(t.ID, subID) { // subscription-wide list, e.g. all VNets
			return strings.HasPrefix(strings.ToLower(res.ID), strings.ToLower(subID)+"/")
		}
		return eq(res.ParentID, t.ID)
	})
}

func (s *Server) listByFilter(w http.ResponseWriter, f func(*core.Resource) bool) {
	items := []json.RawMessage{}
	for _, res := range s.store.List(f) {
		items = append(items, res.Body)
	}
	writeJSON(w, 200, map[string]any{"value": items})
}

func notFound(w http.ResponseWriter, t target) {
	if t.Kind == "resourceGroup" {
		writeError(w, 404, "ResourceGroupNotFound", "Resource group '%s' could not be found.", t.Name)
		return
	}
	writeError(w, 404, "ResourceNotFound", "The Resource '%s/%s' under resource group '%s' was not found.", t.Type, t.Name, t.ResourceGroup)
}

func writeRaw(w http.ResponseWriter, status int, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("x-ms-request-id", newUUID())
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// --- subscriptions, locations, providers ----------------------------------

func (s *Server) ensureSubscription(sub string) {
	id := "/subscriptions/" + sub
	if s.store.Exists(id) {
		return
	}
	body, _ := json.Marshal(subscriptionJSON(sub, s.cfg.TenantID))
	s.store.Put(&core.Resource{Cloud: "azure", ID: id, Type: "Microsoft.Resources/subscriptions", Name: sub, Backing: "state", Body: body})
}

func subscriptionJSON(sub, tenant string) map[string]any {
	return map[string]any{
		"id":             "/subscriptions/" + sub,
		"subscriptionId": sub,
		"tenantId":       tenant,
		"displayName":    "Nimbus Local",
		"state":          "Enabled",
		"subscriptionPolicies": map[string]any{
			"locationPlacementId": "Public_2014-09-01",
			"quotaId":             "PayAsYouGo_2014-09-01",
			"spendingLimit":       "Off",
		},
		"authorizationSource": "RoleBased",
	}
}

func (s *Server) listSubscriptions(w http.ResponseWriter) {
	s.listByFilter(w, func(r *core.Resource) bool { return eq(r.Type, "Microsoft.Resources/subscriptions") })
}

func (s *Server) getSubscription(w http.ResponseWriter, t target) {
	res, _ := s.store.Get(t.ID)
	writeRaw(w, 200, res.Body)
}

var locations = []string{"eastus", "eastus2", "westus", "westus2", "westus3", "centralus", "southcentralus", "northcentralus", "canadacentral", "brazilsouth", "northeurope", "westeurope", "uksouth", "francecentral", "germanywestcentral", "swedencentral", "eastasia", "southeastasia", "japaneast", "australiaeast", "centralindia", "koreacentral"}

func (s *Server) listLocations(w http.ResponseWriter, t target) {
	out := []any{}
	for _, l := range locations {
		out = append(out, map[string]any{
			"id":          "/subscriptions/" + t.Subscription + "/locations/" + l,
			"name":        l,
			"displayName": l,
			"metadata":    map[string]any{"regionType": "Physical"},
		})
	}
	writeJSON(w, 200, map[string]any{"value": out})
}

// Every namespace reports Registered, so Terraform's automatic provider
// registration finds nothing to do.
var namespaces = []string{
	"Microsoft.Resources", "Microsoft.Network", "Microsoft.Storage", "Microsoft.Compute", "Microsoft.ContainerService",
	"Microsoft.ContainerRegistry", "Microsoft.KeyVault", "Microsoft.Web", "Microsoft.Sql", "Microsoft.DBforPostgreSQL",
	"Microsoft.DBforMySQL", "Microsoft.DocumentDB", "Microsoft.ServiceBus", "Microsoft.EventHub", "Microsoft.EventGrid",
	"Microsoft.Insights", "Microsoft.OperationalInsights", "Microsoft.ManagedIdentity", "Microsoft.Authorization",
	"Microsoft.App", "Microsoft.ContainerInstance", "Microsoft.Cache", "Microsoft.ApiManagement", "Microsoft.Cdn",
	"Microsoft.AppConfiguration", "Microsoft.Monitor", "Microsoft.AlertsManagement", "Microsoft.Security",
	"Microsoft.PolicyInsights", "Microsoft.Maintenance", "Microsoft.Network", "Microsoft.Kubernetes",
	"Microsoft.KubernetesConfiguration", "Microsoft.Advisor", "Microsoft.Automation", "Microsoft.Batch",
	"Microsoft.CognitiveServices", "Microsoft.DataFactory", "Microsoft.Databricks", "Microsoft.Devices",
	"Microsoft.Logic", "Microsoft.MachineLearningServices", "Microsoft.Management", "Microsoft.NotificationHubs",
	"Microsoft.RecoveryServices", "Microsoft.Relay", "Microsoft.Search", "Microsoft.SignalRService",
	"Microsoft.StreamAnalytics", "Microsoft.Synapse", "Microsoft.Dashboard", "Microsoft.DesktopVirtualization",
	"Microsoft.HealthcareApis", "Microsoft.Maps", "Microsoft.Purview", "Microsoft.ServiceFabric",
}

func providerJSON(ns string) map[string]any {
	return map[string]any{
		"id":                "/providers/" + ns,
		"namespace":         ns,
		"registrationState": "Registered",
		"resourceTypes":     []any{},
	}
}

func (s *Server) listProviders(w http.ResponseWriter) {
	out := []any{}
	seen := map[string]bool{}
	for _, ns := range namespaces {
		if seen[strings.ToLower(ns)] {
			continue
		}
		seen[strings.ToLower(ns)] = true
		out = append(out, providerJSON(ns))
	}
	writeJSON(w, 200, map[string]any{"value": out})
}

// --- small JSON helpers ----------------------------------------------------

func deepCopy(m map[string]any) map[string]any {
	raw, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// mergeMaps applies a PATCH body: nested objects merge, everything else replaces.
func mergeMaps(dst, src map[string]any) map[string]any {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				dst[k] = mergeMaps(dv, sv)
				continue
			}
		}
		dst[k] = v
	}
	return dst
}
