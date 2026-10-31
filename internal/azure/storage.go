package azure

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

const (
	typeStorageAccount   = "Microsoft.Storage/storageAccounts"
	typeStorageContainer = "Microsoft.Storage/storageAccounts/blobServices/containers"
)

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
		// poll; its SDK client treats a 201 as a protocol error. The delete,
		// confusingly, is the other way round: synchronous, 200.
		CreateStatus: 202,
		DeleteStatus: 200,
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
			s.ensureServiceResources(t)
		},
		AfterDelete: func(s *Server, t target) {
			// Deleting the account deletes its data; there is no soft delete.
			s.storage.Store().DeleteAccount(t.Name)
		},
	})
}

// Containers can be created two ways and must exist both ways.
//
// azurerm 4.x creates them through ARM when azurerm_storage_container is given
// storage_account_id, but azurerm_storage_blob -- and `az`, and every SDK --
// uses the data plane. A container made one way that is invisible the other
// way fails an apply with ContainerNotFound on a container Terraform created
// moments earlier.
func registerContainerHooks(s *Server) {
	s.registerHook(typeStorageContainer, Hook{
		Backing: "real",
		// ARM container delete is synchronous.
		DeleteStatus: 200,
		AfterPut: func(s *Server, t target, body map[string]any) {
			a := s.storage.Store().Account(accountFromContainerID(t.ID))
			if a == nil {
				return
			}
			access := ""
			if props, ok := body["properties"].(map[string]any); ok {
				if p := str(props["publicAccess"]); p != "" && !eq(p, "None") {
					access = strings.ToLower(p)
				}
			}
			a.CreateContainer(t.Name, map[string]string{}, access)
		},
		AfterDelete: func(s *Server, t target) {
			if a := s.storage.Store().Account(accountFromContainerID(t.ID)); a != nil {
				a.DeleteContainer(t.Name)
			}
		},
	})
}

// mirrorDataPlaneContainer is the other direction: a container created by
// `az storage container create` or an SDK exists only in the data-plane store,
// so it is mirrored into core.Store to appear in the graph and the inspector,
// and so a later ARM read finds it.
func (s *Server) mirrorDataPlaneContainer(account, container, action string) {
	acct := s.store.List(func(r *core.Resource) bool {
		return eq(r.Type, typeStorageAccount) && eq(r.Name, account)
	})
	if len(acct) == 0 {
		return
	}
	id := acct[0].ID + "/blobServices/default/containers/" + container
	switch action {
	case "delete":
		_ = s.store.Delete(id)
	case "create":
		if s.store.Exists(id) {
			return
		}
		body, _ := json.Marshal(map[string]any{
			"id": id, "name": container, "type": typeStorageContainer,
			"properties": map[string]any{"publicAccess": "None", "provisioningState": "Succeeded"},
		})
		s.store.Put(&core.Resource{
			Cloud: "azure", ID: id, Type: typeStorageContainer, Name: container,
			ParentID: acct[0].ID, Backing: "real", Body: body,
		})
	}
}

// accountFromContainerID pulls the account out of
// ".../storageAccounts/{account}/blobServices/default/containers/{name}".
func accountFromContainerID(id string) string {
	seg := strings.Split(strings.Trim(id, "/"), "/")
	for i, s := range seg {
		if strings.EqualFold(s, "storageAccounts") && i+1 < len(seg) {
			return seg[i+1]
		}
	}
	return ""
}

// ensureServiceResources materialises the four ARM child resources every
// storage account has: blobServices/default and the file, queue and table
// equivalents.
//
// These are not created by clients -- Azure brings them into being with the
// account -- and azurerm GETs them while waiting for the account to settle.
// If they 404 it polls forever: the apply sits on "Still creating..." and
// never fails, which is a much worse failure than an error.
//
// Created only when absent, so a client that has configured, say, blob
// retention policies does not have them reset by a later account update.
func (s *Server) ensureServiceResources(t target) {
	for _, svc := range []string{"blobServices", "fileServices", "queueServices", "tableServices"} {
		id := t.ID + "/" + svc + "/default"
		if s.store.Exists(id) {
			continue
		}
		body := map[string]any{
			"id":         id,
			"name":       "default",
			"type":       typeStorageAccount + "/" + svc,
			"properties": defaultServiceProps(svc),
		}
		raw, _ := json.Marshal(body)
		s.store.Put(&core.Resource{
			Cloud: "azure", ID: id, Type: typeStorageAccount + "/" + svc,
			Name: "default", ParentID: t.ID, Backing: "real", Body: raw,
		})
	}
}

// defaultServiceProps is what Azure reports for a fresh account. azurerm reads
// these back into blob_properties / queue_properties / share_properties, so
// the shapes have to be present even when everything is switched off.
func defaultServiceProps(svc string) map[string]any {
	cors := map[string]any{"corsRules": []any{}}
	switch svc {
	case "blobServices":
		return map[string]any{
			"cors":                           cors,
			"deleteRetentionPolicy":          map[string]any{"enabled": false},
			"containerDeleteRetentionPolicy": map[string]any{"enabled": false},
			"isVersioningEnabled":            false,
			"changeFeed":                     map[string]any{"enabled": false},
			"restorePolicy":                  map[string]any{"enabled": false},
			"lastAccessTimeTrackingPolicy":   map[string]any{"enable": false},
		}
	case "fileServices":
		return map[string]any{
			"cors":                       cors,
			"shareDeleteRetentionPolicy": map[string]any{"enabled": true, "days": 7},
			"protocolSettings":           map[string]any{"smb": map[string]any{}},
		}
	default: // queueServices, tableServices
		return map[string]any{"cors": cors}
	}
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
