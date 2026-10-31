package azure

import (
	"strings"
)

// ARM paths look like:
//
//   /subscriptions/{sub}
//   /subscriptions/{sub}/resourceGroups/{rg}
//   /subscriptions/{sub}/resourceGroups/{rg}/providers/{ns}/{type}/{name}[/{childType}/{childName}...]
//   /subscriptions/{sub}/providers/{ns}/{type}/{name}               (subscription-scoped)
//   {any resource id}/providers/{ns}/{type}/{name}                   (extension resources, e.g. role assignments)
//
// An even number of type/name segments after providers/{ns} addresses one
// resource; an odd number addresses a collection (GET = list) or, on POST, an
// action on the resource before it (e.g. .../storageAccounts/x/listKeys).

// target is what an ARM path points at.
type target struct {
	Kind          string // subscription, subscriptions, resourceGroup, resourceGroups, rgResources, subResources, providers, provider, providerRegister, locations, resource, collection
	Subscription  string
	ResourceGroup string
	Namespace     string
	ID            string // canonical ID of the resource (Kind == resource) or of the collection's parent
	Type          string // full type, e.g. Microsoft.Network/virtualNetworks/subnets
	Name          string
	ParentID      string // for resources: the containing resource (RG, VNet, subscription, ...)
	Action        string // last segment when a collection path is used with POST
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func eq(a, b string) bool { return strings.EqualFold(a, b) }

// parseTarget turns an ARM URL path into a target. ok is false for paths that
// are not ARM paths at all.
func parseTarget(path string) (t target, ok bool) {
	seg := splitPath(path)
	if len(seg) == 0 || !eq(seg[0], "subscriptions") {
		return t, false
	}
	if len(seg) == 1 {
		t.Kind = "subscriptions"
		return t, true
	}
	t.Subscription = seg[1]
	subID := "/subscriptions/" + seg[1]
	rest := seg[2:]

	if len(rest) == 0 {
		t.Kind, t.ID = "subscription", subID
		return t, true
	}

	// Find the LAST providers/{ns} so extension resources resolve to their own type.
	last := -1
	for i := 0; i+1 < len(rest); i++ {
		if eq(rest[i], "providers") {
			last = i
		}
	}

	if last == -1 {
		switch {
		case eq(rest[0], "locations") && len(rest) == 1:
			t.Kind = "locations"
		case eq(rest[0], "providers") && len(rest) == 1:
			t.Kind = "providers"
		case eq(rest[0], "resources") && len(rest) == 1:
			// Every resource in the subscription, with an optional $filter.
			// azurerm's Key Vault cache uses this to map a vault URI back to
			// a resource ID, and fails the whole operation if it 404s.
			t.Kind = "subResources"
			t.ID = subID
		case eq(rest[0], "resourceGroups") && len(rest) == 1:
			t.Kind = "resourceGroups"
		case eq(rest[0], "resourceGroups") && len(rest) == 2:
			t.Kind = "resourceGroup"
			t.ResourceGroup = rest[1]
			t.ID = subID + "/resourceGroups/" + rest[1]
			t.Type = "Microsoft.Resources/resourceGroups"
			t.Name = rest[1]
			t.ParentID = subID
		case eq(rest[0], "resourceGroups") && len(rest) == 3 && eq(rest[2], "resources"):
			t.Kind = "rgResources"
			t.ResourceGroup = rest[1]
			t.ID = subID + "/resourceGroups/" + rest[1]
		case eq(rest[0], "resourceGroups") && len(rest) == 3:
			t.Kind = "rgAction"
			t.ResourceGroup = rest[1]
			t.ID = subID + "/resourceGroups/" + rest[1]
			t.Action = rest[2]
		default:
			return t, false
		}
		return t, true
	}

	// Subscription-level provider operations: /providers/{ns} and /providers/{ns}/register
	if last == 0 && len(rest) == 2 {
		t.Kind, t.Namespace = "provider", rest[1]
		return t, true
	}
	if last == 0 && len(rest) == 3 && (eq(rest[2], "register") || eq(rest[2], "unregister")) {
		t.Kind, t.Namespace, t.Action = "providerRegister", rest[1], rest[2]
		return t, true
	}

	scope := canonicalScope(subID, rest[:last])
	if len(rest) >= 2 && eq(rest[0], "resourceGroups") {
		t.ResourceGroup = rest[1]
	}
	t.Namespace = rest[last+1]
	pairs := rest[last+2:]
	if len(pairs) == 0 {
		return t, false
	}

	base := scope + "/providers/" + t.Namespace
	types := []string{t.Namespace}
	cur := base
	parent := scope
	for i := 0; i < len(pairs); i += 2 {
		types = append(types, pairs[i])
		if i+1 < len(pairs) {
			parent = cur
			if i == 0 {
				parent = scope
			}
			cur = cur + "/" + pairs[i] + "/" + pairs[i+1]
		}
	}

	if len(pairs)%2 == 0 {
		t.Kind = "resource"
		t.ID = cur
		t.Type = strings.Join(types, "/")
		t.Name = pairs[len(pairs)-1]
		t.ParentID = parent
		return t, true
	}

	// Odd: a collection under `cur`, or an action on `cur`.
	t.Kind = "collection"
	t.Type = strings.Join(types, "/")
	t.ID = cur
	if len(pairs) == 1 {
		t.ID = scope
	}
	t.Action = pairs[len(pairs)-1]
	return t, true
}

// canonicalScope rebuilds the scope part of an ID with ARM's canonical casing
// for the well-known segments.
func canonicalScope(subID string, seg []string) string {
	out := subID
	for i := 0; i < len(seg); i++ {
		s := seg[i]
		switch {
		case eq(s, "resourceGroups"):
			s = "resourceGroups"
		case eq(s, "providers"):
			s = "providers"
		}
		out += "/" + s
	}
	return out
}

// normalizeLocation turns "East US" into "eastus", like Azure does.
func normalizeLocation(l string) string {
	return strings.ToLower(strings.ReplaceAll(l, " ", ""))
}
