package azure

import (
	"strings"
	"time"
)

const typeStorageAccount = "Microsoft.Storage/storageAccounts"

// Storage accounts are the first resource Nimbus actually backs with something
// real: ARM creates the account here, and the data plane at
// {account}.{service}.{suffix} then serves blobs, queues and tables for it.
//
// azurerm does not wait politely after the ARM create -- it goes straight to
// the data plane to read blob service properties, queue properties and the
// static-website settings, so the account cannot finish applying unless those
// answer.
func registerStorageHooks(s *Server) {
	s.registerHook(typeStorageAccount, Hook{
		Backing: "real",
		// Azure answers a storage account create 202 and makes the client
		// poll; its SDK client treats a 201 as a protocol error.
		CreateStatus: 202,
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			if !validAccountName(t.Name) {
				return &armError{400, "AccountNameInvalid",
					"The storage account named " + t.Name + " is invalid. Account names must be 3 to 24 characters, lower-case letters and numbers only."}
			}
			props := body["properties"].(map[string]any)

			// Server-generated fields the provider reads straight back.
			created := time.Now().UTC().Format(time.RFC3339)
			if existing != nil {
				if ep, ok := existing["properties"].(map[string]any); ok {
					if c, ok := ep["creationTime"].(string); ok {
						created = c // immutable once set
					}
				}
			}
			props["creationTime"] = created
			props["primaryLocation"] = str(body["location"])
			props["statusOfPrimary"] = "available"
			props["primaryEndpoints"] = s.storageEndpoints(t.Name)
			props["provisioningState"] = "Succeeded"

			// Defaults azurerm expects to read back when it did not send them.
			setDefault(props, "supportsHttpsTrafficOnly", true)
			setDefault(props, "allowBlobPublicAccess", false)
			setDefault(props, "minimumTlsVersion", "TLS1_2")
			setDefault(props, "accessTier", "Hot")
			setDefault(props, "isHnsEnabled", false)
			setDefault(props, "allowSharedKeyAccess", true)
			setDefault(props, "publicNetworkAccess", "Enabled")
			setDefault(props, "networkAcls", map[string]any{
				"bypass": "AzureServices", "defaultAction": "Allow",
				"ipRules": []any{}, "virtualNetworkRules": []any{},
			})
			setDefault(props, "encryption", map[string]any{
				"keySource": "Microsoft.Storage",
				"services": map[string]any{
					"blob":  map[string]any{"enabled": true, "keyType": "Account"},
					"file":  map[string]any{"enabled": true, "keyType": "Account"},
					"queue": map[string]any{"enabled": true, "keyType": "Account"},
					"table": map[string]any{"enabled": true, "keyType": "Account"},
				},
			})
			if body["kind"] == nil {
				body["kind"] = "StorageV2"
			}
			return nil
		},
		AfterPut: func(s *Server, t target, body map[string]any) {
			s.storage.Store().CreateAccount(t.Name)
		},
		AfterDelete: func(s *Server, t target) {
			// Deleting the account deletes its data; there is no soft delete.
			s.storage.Store().DeleteAccount(t.Name)
		},
	})
}

// storageEndpoints is the primaryEndpoints block. These carry our port for the
// same reason the metadata suffix does.
func (s *Server) storageEndpoints(account string) map[string]any {
	sfx := s.storageSuffix()
	out := map[string]any{}
	for _, svc := range []string{"blob", "queue", "table", "file", "dfs"} {
		out[svc] = "https://" + account + "." + svc + "." + sfx + "/"
	}
	return out
}

// validAccountName applies Azure's rule: 3-24 characters, lower-case letters
// and digits only. azurerm validates this itself, but `az` and the SDKs do not
// always, and a bad name would otherwise produce a confusing data-plane host.
func validAccountName(n string) bool {
	if len(n) < 3 || len(n) > 24 {
		return false
	}
	for _, r := range n {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func setDefault(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// listKeys and regenerateKey are POST actions on the account, not fields on
// it: Azure never returns keys from a GET.
// An action path is a collection target: for
// ".../storageAccounts/st1/listKeys", t.ID is the account and t.Action is
// "listKeys", so the resource the action applies to is looked up by ID.
func (s *Server) storageAccountAction(t target, body map[string]any) (any, bool) {
	res, err := s.store.Get(t.ID)
	if err != nil || !eq(res.Type, typeStorageAccount) {
		return nil, false
	}
	account := res.Name
	switch strings.ToLower(t.Action) {
	case "listkeys":
		a := s.storage.Store().Account(account)
		if a == nil {
			return nil, false
		}
		return map[string]any{"keys": keyList(a.Keys)}, true
	case "regeneratekey":
		which := 0
		if strings.EqualFold(str(body["keyName"]), "key2") {
			which = 1
		}
		a := s.storage.Store().RegenerateKey(account, which)
		if a == nil {
			return nil, false
		}
		return map[string]any{"keys": keyList(a.Keys)}, true
	}
	return nil, false
}

func keyList(keys [2]string) []any {
	out := []any{}
	for i, k := range keys {
		out = append(out, map[string]any{
			"keyName":      [2]string{"key1", "key2"}[i],
			"value":        k,
			"permissions":  "FULL",
			"creationTime": time.Now().UTC().Format(time.RFC3339),
		})
	}
	return out
}

// accountNameFromID pulls the account name out of an ARM resource ID.
func accountNameFromID(id string) string {
	seg := strings.Split(strings.Trim(id, "/"), "/")
	if len(seg) == 0 {
		return ""
	}
	return seg[len(seg)-1]
}
