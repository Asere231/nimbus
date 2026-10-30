package azure

import (
	"encoding/base64"
	"testing"
)

func decodeSeg(s string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(s) }

func TestParseTarget(t *testing.T) {
	s := "/subscriptions/s1"
	cases := []struct {
		path               string
		kind, typ, name    string
		id, parent, action string
	}{
		{path: s, kind: "subscription", id: s},
		{path: s + "/resourcegroups", kind: "resourceGroups"},
		{path: s + "/resourceGroups/rg", kind: "resourceGroup", typ: "Microsoft.Resources/resourceGroups", name: "rg", id: s + "/resourceGroups/rg", parent: s},
		{path: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v",
			kind: "resource", typ: "Microsoft.Network/virtualNetworks", name: "v",
			id: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v", parent: s + "/resourceGroups/rg"},
		{path: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/a",
			kind: "resource", typ: "Microsoft.Network/virtualNetworks/subnets", name: "a",
			id:     s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets/a",
			parent: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v"},
		{path: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/subnets",
			kind: "collection", typ: "Microsoft.Network/virtualNetworks/subnets",
			id: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v", action: "subnets"},
		{path: s + "/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/st/listKeys",
			kind: "collection", typ: "Microsoft.Storage/storageAccounts/listKeys",
			id: s + "/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/st", action: "listKeys"},
		{path: s + "/providers/Microsoft.Network/virtualNetworks", kind: "collection", typ: "Microsoft.Network/virtualNetworks", id: s, action: "virtualNetworks"},
		{path: s + "/providers/Microsoft.Network", kind: "provider"},
		{path: s + "/providers/Microsoft.Network/register", kind: "providerRegister", action: "register"},
		// Extension resource: a role assignment scoped to a VNet.
		{path: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/providers/Microsoft.Authorization/roleAssignments/r1",
			kind: "resource", typ: "Microsoft.Authorization/roleAssignments", name: "r1",
			id:     s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v/providers/Microsoft.Authorization/roleAssignments/r1",
			parent: s + "/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/v"},
	}
	for _, c := range cases {
		got, ok := parseTarget(c.path)
		if !ok {
			t.Errorf("%s: not parsed", c.path)
			continue
		}
		if got.Kind != c.kind || got.Type != c.typ || got.Name != c.name || got.ID != c.id || got.ParentID != c.parent || got.Action != c.action {
			t.Errorf("%s:\n got  %+v\n want kind=%s type=%s name=%s id=%s parent=%s action=%s", c.path, got, c.kind, c.typ, c.name, c.id, c.parent, c.action)
		}
	}
	if _, ok := parseTarget("/nope"); ok {
		t.Error("non-ARM path parsed")
	}
}
