package azure

import (
	"encoding/json"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

const (
	typeVNet   = "Microsoft.Network/virtualNetworks"
	typeSubnet = "Microsoft.Network/virtualNetworks/subnets"
)

// Network resources carry an etag and a resourceGuid, and a VNet embeds its
// subnets. Subnets are ALSO stored as their own resources so that both the
// inline `subnet {}` block and the separate azurerm_subnet resource work, and
// so the visualizer can draw them as children of the VNet.
func registerNetworkHooks(s *Server) {
	s.registerHook(typeVNet, Hook{
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any)
			keepServerFields(props, existing)
			if as, ok := props["addressSpace"].(map[string]any); !ok || len(asList(as["addressPrefixes"])) == 0 {
				return &armError{400, "InvalidAddressSpace", "A virtual network needs properties.addressSpace.addressPrefixes."}
			}
			// No subnets in the request = keep the ones that exist (azurerm_subnet manages them).
			if _, sent := props["subnets"]; !sent {
				if existing != nil {
					if ep, ok := existing["properties"].(map[string]any); ok && ep["subnets"] != nil {
						props["subnets"] = ep["subnets"]
					}
				}
			}
			if props["subnets"] == nil {
				props["subnets"] = []any{}
			}
			// Give inline subnets their canonical fields.
			for _, raw := range asList(props["subnets"]) {
				sn, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := sn["name"].(string)
				sn["id"] = t.ID + "/subnets/" + name
				sn["type"] = typeSubnet
				if sn["etag"] == nil {
					sn["etag"] = etag()
				}
				sp, _ := sn["properties"].(map[string]any)
				if sp == nil {
					sp = map[string]any{}
				}
				sp["provisioningState"] = "Succeeded"
				sn["properties"] = sp
			}
			body["etag"] = etag()
			return nil
		},
		AfterPut: func(s *Server, t target, body map[string]any) {
			// Mirror inline subnets as child resources.
			props := body["properties"].(map[string]any)
			want := map[string]bool{}
			for _, raw := range asList(props["subnets"]) {
				sn, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				name, _ := sn["name"].(string)
				id := t.ID + "/subnets/" + name
				want[strings.ToLower(id)] = true
				b, _ := json.Marshal(sn)
				s.store.Put(&core.Resource{Cloud: "azure", ID: id, Type: typeSubnet, Name: name, ParentID: t.ID, Backing: "state", Body: b})
			}
			for _, child := range s.store.Children(t.ID) {
				if eq(child.Type, typeSubnet) && !want[strings.ToLower(child.ID)] {
					_ = s.store.Delete(child.ID)
				}
			}
		},
	})

	s.registerHook(typeSubnet, Hook{
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any)
			if props["addressPrefix"] == nil && len(asList(props["addressPrefixes"])) == 0 {
				return &armError{400, "InvalidRequestFormat", "A subnet needs properties.addressPrefix or properties.addressPrefixes."}
			}
			body["etag"] = etag()
			return nil
		},
		AfterPut: func(s *Server, t target, body map[string]any) {
			s.updateVNetSubnets(t.ParentID, func(list []any) []any {
				out := []any{}
				replaced := false
				for _, raw := range list {
					if sn, ok := raw.(map[string]any); ok && eq(str(sn["name"]), t.Name) {
						out = append(out, body)
						replaced = true
						continue
					}
					out = append(out, raw)
				}
				if !replaced {
					out = append(out, body)
				}
				return out
			})
		},
		AfterDelete: func(s *Server, t target) {
			s.updateVNetSubnets(t.ParentID, func(list []any) []any {
				out := []any{}
				for _, raw := range list {
					if sn, ok := raw.(map[string]any); ok && eq(str(sn["name"]), t.Name) {
						continue
					}
					out = append(out, raw)
				}
				return out
			})
		},
	})
}

// updateVNetSubnets rewrites the VNet's embedded subnet list without firing
// the VNet hooks (which would recreate or delete child resources).
func (s *Server) updateVNetSubnets(vnetID string, f func([]any) []any) {
	res, err := s.store.Get(vnetID)
	if err != nil {
		return
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		return
	}
	props["subnets"] = f(asList(props["subnets"]))
	res.Body, _ = json.Marshal(body)
	s.store.Put(res)
}

// keepServerFields carries server-generated values across updates.
func keepServerFields(props, existing map[string]any) {
	if existing != nil {
		if ep, ok := existing["properties"].(map[string]any); ok {
			if g, ok := ep["resourceGuid"]; ok {
				props["resourceGuid"] = g
				return
			}
		}
	}
	props["resourceGuid"] = newUUID()
}

func etag() string { return `W/"` + newUUID() + `"` }

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
