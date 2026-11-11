package azure

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Key Vault's control plane.
//
// The vault resource itself is ordinary ARM bookkeeping with one field that
// matters enormously: properties.vaultUri. azurerm reads the vault through ARM
// and uses that URI for every secret operation -- its own log line is "looking
// up base uri for secret %q in %s" -- so the data plane's address is decided
// here, and it carries our port for the same reason the storage endpoints do.
//
// The interesting part is soft delete. Deleting a vault in Azure does not free
// its name: the vault moves to a per-location list of deleted vaults and stays
// there until it is purged or recovered. azurerm purges on destroy by default
// (purge_soft_delete_on_destroy) and recovers a soft-deleted vault rather than
// failing (recover_soft_deleted_key_vaults), so both paths have to work or the
// second apply of the same config breaks.
const typeKeyVault = "Microsoft.KeyVault/vaults"

// keyVaultAudience is the resource a vault's 401 challenge asks for a token
// for. It is Azure's real audience rather than a Nimbus URL, because that is
// what clients expect to see and our token endpoint issues tokens for any
// scope anyway.
const keyVaultAudience = "https://vault.azure.net"

// softDeleteRetention is how long a deleted vault is kept, matching Azure's
// default. Nothing here actually expires one: a local emulator is not running
// for 90 days, and a timer that purged vaults behind your back would be a
// worse surprise than one that never fires.
const softDeleteRetention = 90 * 24 * time.Hour

// deletedVault is one soft-deleted vault, kept so its name stays taken and it
// can be recovered.
//
// Like operations, these are NOT persisted: they describe a pending decision
// in this process. A restarted Nimbus has no soft-deleted vaults, so a name
// that was taken becomes free again -- which is a divergence from Azure, but a
// harmless direction to diverge in (a create succeeds where Azure would have
// told you to recover or purge).
type deletedVault struct {
	Name     string
	Location string
	VaultID  string
	Deleted  time.Time
	Body     json.RawMessage // the vault as it was, for recovery
}

type deletedVaults struct {
	mu sync.Mutex
	m  map[string]*deletedVault
}

func newDeletedVaults() *deletedVaults { return &deletedVaults{m: map[string]*deletedVault{}} }

func deletedVaultKey(location, name string) string {
	return strings.ToLower(location + "/" + name)
}

func (d *deletedVaults) put(v *deletedVault) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[deletedVaultKey(v.Location, v.Name)] = v
}

// get finds a soft-deleted vault by location and name.
func (d *deletedVaults) get(location, name string) *deletedVault {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.m[deletedVaultKey(location, name)]
}

// byName finds one by name alone, which is how a create collides with it: the
// client does not say which location the old vault was in.
func (d *deletedVaults) byName(name string) *deletedVault {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, v := range d.m {
		if eq(v.Name, name) {
			return v
		}
	}
	return nil
}

func (d *deletedVaults) remove(location, name string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := deletedVaultKey(location, name)
	_, ok := d.m[key]
	delete(d.m, key)
	return ok
}

func (d *deletedVaults) all() []*deletedVault {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*deletedVault, 0, len(d.m))
	for _, v := range d.m {
		out = append(out, v)
	}
	return out
}

// keyVaultSuffix is the DNS suffix vault hostnames are built on, including our
// port. See the comment on the metadata suffixes: a bare hostname would send
// clients to port 443.
func (s *Server) keyVaultSuffix() string { return withPort(KeyVaultSuffix, s.cfg.PublicURL) }

func (s *Server) vaultURI(name string) string {
	return "https://" + strings.ToLower(name) + "." + s.keyVaultSuffix() + "/"
}

func registerKeyVaultHooks(s *Server) {
	s.registerHook(typeKeyVault, Hook{
		// Azure deletes a vault synchronously and answers 200; the resource is
		// gone from ARM immediately, soft-deleted rather than destroyed.
		DeleteStatus: 200,
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			if !validVaultName(t.Name) {
				return &armError{400, "VaultNameInvalid",
					"The vault name '" + t.Name + "' is invalid. It must be 3 to 24 characters, " +
						"letters, digits and hyphens only, and start with a letter -- the name becomes a DNS label."}
			}
			props := body["properties"].(map[string]any)

			// A name that is soft-deleted is still taken. createMode
			// "recover" is how a client claims it back; without it Azure
			// refuses, and so do we, because silently creating a fresh vault
			// would lose whatever the old one held.
			if existing == nil {
				if dv := s.vaults.byName(t.Name); dv != nil {
					if !eq(str(props["createMode"]), "recover") {
						return &armError{409, "ConflictError",
							"Exists soft deleted vault with the same name '" + t.Name + "'. " +
								"Recover it (createMode: recover) or purge it first."}
					}
					s.vaults.remove(dv.Location, dv.Name)
				}
			}

			props["vaultUri"] = s.vaultURI(t.Name)
			setDefault(props, "tenantId", s.cfg.TenantID)
			setDefault(props, "sku", map[string]any{"family": "A", "name": "standard"})
			setDefault(props, "accessPolicies", []any{})
			setDefault(props, "enabledForDeployment", false)
			setDefault(props, "enabledForDiskEncryption", false)
			setDefault(props, "enabledForTemplateDeployment", false)
			setDefault(props, "enableRbacAuthorization", false)
			setDefault(props, "publicNetworkAccess", "Enabled")
			// Soft delete cannot be turned off in Azure any more, and azurerm
			// depends on it being on: its destroy purges the vault.
			props["enableSoftDelete"] = true
			setDefault(props, "softDeleteRetentionInDays", float64(90))
			setDefault(props, "networkAcls", map[string]any{
				"bypass": "AzureServices", "defaultAction": "Allow",
				"ipRules": []any{}, "virtualNetworkRules": []any{},
			})
			// createMode is an instruction, not state: Azure does not report
			// it back, and leaving it in would show up in a client's diff.
			delete(props, "createMode")
			return nil
		},
		AfterPut: func(s *Server, t target, body map[string]any) {
			// The data plane's vault is created alongside the ARM resource,
			// so a client can set a secret the moment the vault exists.
			s.keyvault.Store().CreateVault(t.Name)
		},
		AfterDelete: func(s *Server, t target, body map[string]any) {
			// The secrets deliberately stay: the vault is soft-deleted, and a
			// recovered vault that came back empty would be a lie.
			loc := str(body["location"])
			s.vaults.put(&deletedVault{
				Name: t.Name, Location: loc, VaultID: t.ID,
				Deleted: time.Now().UTC(), Body: mustJSON(body),
			})
		},
	})
}

// validVaultName applies Azure's rule, which exists because the name becomes a
// DNS label: 3-24 characters, letters, digits and hyphens, starting with a
// letter and not ending with a hyphen.
func validVaultName(n string) bool {
	if len(n) < 3 || len(n) > 24 {
		return false
	}
	for i, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(n)-1 {
				return false
			}
		default:
			return false
		}
	}
	first := n[0]
	return (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')
}

// --- deleted vaults and name availability ----------------------------------

// isKeyVaultSubscriptionPath reports whether a target addresses one of the
// subscription-level Key Vault endpoints Nimbus serves itself: the deleted
// vault list, a single deleted vault, its purge action, or a name-availability
// check. None of these are stored resources.
func isKeyVaultSubscriptionPath(t target) bool {
	low := strings.ToLower(t.Type)
	return strings.HasPrefix(low, "microsoft.keyvault/locations/deletedvaults") ||
		low == "microsoft.keyvault/deletedvaults" ||
		low == "microsoft.keyvault/checknameavailability"
}

func (s *Server) handleKeyVaultSubscriptionPath(w http.ResponseWriter, r *http.Request, t target) string {
	low := strings.ToLower(t.Type)

	switch {
	case low == "microsoft.keyvault/checknameavailability":
		body, err := readBody(r)
		if err != nil {
			writeError(w, 400, "InvalidRequestContent", "%v", err)
			return ""
		}
		name := str(body["name"])
		if dv := s.vaults.byName(name); dv != nil {
			writeJSON(w, 200, map[string]any{
				"nameAvailable": false, "reason": "AlreadyExists",
				"message": "The vault name '" + name + "' is already in use by a soft-deleted vault.",
			})
			return "name taken by a soft-deleted vault: " + name
		}
		if !validVaultName(name) {
			writeJSON(w, 200, map[string]any{
				"nameAvailable": false, "reason": "Invalid",
				"message": "The vault name '" + name + "' is not a valid DNS label.",
			})
			return ""
		}
		writeJSON(w, 200, map[string]any{"nameAvailable": true})
		return ""

	case low == "microsoft.keyvault/deletedvaults":
		// Subscription-wide list.
		out := []any{}
		for _, dv := range s.vaults.all() {
			out = append(out, deletedVaultJSON(t.Subscription, dv))
		}
		writeJSON(w, 200, map[string]any{"value": out})
		return ""
	}

	// Everything else is scoped to one location: .../locations/{loc}/deletedVaults/...
	loc, name := locationAndName(t)
	if name == "" {
		// A list of the deleted vaults in one location.
		out := []any{}
		for _, dv := range s.vaults.all() {
			if eq(dv.Location, loc) {
				out = append(out, deletedVaultJSON(t.Subscription, dv))
			}
		}
		writeJSON(w, 200, map[string]any{"value": out})
		return ""
	}

	if eq(t.Action, "purge") {
		if !s.vaults.remove(loc, name) {
			writeError(w, 404, "VaultNotFound",
				"The soft deleted vault '%s' in location '%s' was not found.", name, loc)
			return ""
		}
		// Purging frees the name and destroys the data, which is what azurerm
		// relies on when it destroys and re-applies the same configuration.
		s.keyvault.Store().DeleteVault(name)
		s.bus.Publish(core.Event{
			Kind: "resource", Action: "delete", Cloud: "azure",
			ResourceType: typeKeyVault, Name: name, Note: "purged",
		})
		// Purge is long-running in Azure, and its client polls afterwards. A
		// plain 200 sends that poller to the deleted-vault URL, where our own
		// 404 is read as a failure:
		//
		//	polling after VaultsPurgeDeleted: unexpected status 404 ...
		//	VaultNotFound
		//
		// Measured against azurerm 4.81.0, where it fails every destroy.
		// Pointing at an already-finished async operation ends the poll on its
		// first try instead.
		s.markAsync(w, t)
		w.WriteHeader(202)
		return "purged vault " + name
	}

	dv := s.vaults.get(loc, name)
	if dv == nil {
		writeError(w, 404, "VaultNotFound",
			"The soft deleted vault '%s' in location '%s' was not found.", name, loc)
		return ""
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, 200, deletedVaultJSON(t.Subscription, dv))
	default:
		writeError(w, 405, "MethodNotAllowed", "%s is not supported on a deleted vault", r.Method)
	}
	return ""
}

// locationAndName pulls the two path parameters out of a
// .../locations/{loc}/deletedVaults/{name}[/purge] target.
func locationAndName(t target) (location, name string) {
	seg := splitPath(t.ID)
	for i, s := range seg {
		if eq(s, "locations") && i+1 < len(seg) {
			location = seg[i+1]
		}
		if eq(s, "deletedVaults") && i+1 < len(seg) {
			name = seg[i+1]
		}
	}
	return location, name
}

func deletedVaultJSON(subscription string, dv *deletedVault) map[string]any {
	var body map[string]any
	_ = json.Unmarshal(dv.Body, &body)
	props, _ := body["properties"].(map[string]any)
	tags, _ := body["tags"].(map[string]any)
	return map[string]any{
		"id": fmt.Sprintf("/subscriptions/%s/providers/Microsoft.KeyVault/locations/%s/deletedVaults/%s",
			subscription, dv.Location, dv.Name),
		"name": dv.Name,
		"type": "Microsoft.KeyVault/deletedVaults",
		"properties": map[string]any{
			"vaultId":            dv.VaultID,
			"location":           dv.Location,
			"deletionDate":       dv.Deleted.Format(time.RFC3339),
			"scheduledPurgeDate": dv.Deleted.Add(softDeleteRetention).Format(time.RFC3339),
			"purgeProtectionEnabled": func() any {
				if props != nil {
					return props["enablePurgeProtection"]
				}
				return nil
			}(),
			"tags": tags,
		},
	}
}

func mustJSON(v any) json.RawMessage {
	raw, _ := json.Marshal(v)
	return raw
}
