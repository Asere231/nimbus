package azure

import (
	"encoding/json"
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
	// AfterDelete runs once the resource (and its children) are gone.
	AfterDelete func(s *Server, t target)
	// Backing is "real" when a local engine actually runs this type.
	Backing string
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
			return s.handleAction(w, t)
		}
		s.listCollection(w, t)
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
		if err := s.store.Delete(t.ID); err != nil {
			w.WriteHeader(204) // ARM: deleting something absent is a no-op
			return "already absent"
		}
		if h := s.hookFor(t.Type); h.AfterDelete != nil {
			h.AfterDelete(s, t)
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

	h := s.hookFor(t.Type)
	if h.BeforePut != nil {
		if aerr := h.BeforePut(s, t, body, existing); aerr != nil {
			writeError(w, aerr.Status, aerr.Code, "%s", aerr.Message)
			return ""
		}
	}

	raw, _ := json.Marshal(body)
	loc, _ := body["location"].(string)
	backing := h.Backing
	if backing == "" {
		backing = "state"
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
	if _, known := s.hooks[strings.ToLower(t.Type)]; !known && t.Kind == "resource" {
		return "accepted (state only): " + t.Type
	}
	return ""
}

// handleAction answers POST actions such as listKeys. Unknown actions return
// an empty object and are flagged in the activity feed, so missing features
// are easy to spot while you work.
func (s *Server) handleAction(w http.ResponseWriter, t target) string {
	if t.ID != "" && !s.store.Exists(t.ID) {
		writeError(w, 404, "ResourceNotFound", "The Resource '%s' was not found.", t.ID)
		return ""
	}
	writeJSON(w, 200, map[string]any{})
	return "unimplemented action: " + t.Action
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
