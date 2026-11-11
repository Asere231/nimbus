package azure

import (
	"net/http"
	"strings"
	"time"
)

// Role-based access control, recorded but not enforced.
//
// Nimbus stores role assignments and serves the built-in role definitions, so
// configurations that grant access apply and round-trip exactly as they would
// against Azure. Nothing CHECKS them: every request Nimbus serves is served
// regardless of who asked and what they were granted. That is a deliberate
// decision -- an emulator that enforced RBAC would mostly teach you how to
// debug Nimbus's own permission bugs -- but it means a local apply cannot
// prove a real deployment has the right roles.
const (
	typeRoleAssignment = "Microsoft.Authorization/roleAssignments"
	typeRoleDefinition = "Microsoft.Authorization/roleDefinitions"
)

// builtInRole is one of Azure's built-in role definitions.
//
// The GUIDs are the real ones, taken from Microsoft's built-in roles
// reference: they end up in Terraform state and inside role assignments, so a
// made-up GUID would make Nimbus state and Azure state disagree in a way
// nobody would notice until a config was pointed at the real thing. The
// The action lists USED TO BE "indicative rather than complete", which was
// fine while nothing read them. Since Milestone 14 they decide whether a
// request is refused, so the ones an example relies on are exact and the
// rest are a documented curation: a role not in this table refuses and
// names itself rather than silently granting too much or too little.
//
// DataActions are recorded and NOT enforced. Azure storage's Shared Key
// carries no principal at all, so there is nothing to evaluate a data
// action against -- which means a "Key Vault Secrets User" assignment
// still governs nothing. docs/ROADMAP.md says so under "Later".
type builtInRole struct {
	GUID        string
	Name        string
	Description string
	Actions     []string
	DataActions []string
}

var builtInRoles = []builtInRole{
	{"8e3af657-a8ff-443c-a75c-2fe8c4bcb635", "Owner",
		"Grants full access to manage all resources, including the ability to assign roles in Azure RBAC.",
		[]string{"*"}, nil},
	{"b24988ac-6180-42a0-ab88-20f7382dd24c", "Contributor",
		"Grants full access to manage all resources, but does not allow you to assign roles in Azure RBAC.",
		[]string{"*"}, nil},
	{"acdd72a7-3385-48ef-bd42-f606fba81ae7", "Reader",
		"View all resources, but does not allow you to make any changes.",
		[]string{"*/read"}, nil},
	{"18d7d88d-d35e-4fb5-a5c3-7773c20a72d9", "User Access Administrator",
		"Lets you manage user access to Azure resources.",
		[]string{"*/read", "Microsoft.Authorization/*"}, nil},

	{"00482a5a-887f-4fb3-b363-3b7fe8e74483", "Key Vault Administrator",
		"Perform all data plane operations on a key vault and all objects in it.",
		[]string{"Microsoft.KeyVault/*/read"}, []string{"Microsoft.KeyVault/vaults/*"}},
	{"b86a8fe4-44ce-4948-aee5-eccb2c155cd7", "Key Vault Secrets Officer",
		"Perform any action on the secrets of a key vault, except manage permissions.",
		[]string{"Microsoft.KeyVault/*/read"}, []string{"Microsoft.KeyVault/vaults/secrets/*"}},
	{"4633458b-17de-408a-b874-0445c86b69e6", "Key Vault Secrets User",
		"Read secret contents.",
		[]string{"Microsoft.KeyVault/*/read"}, []string{"Microsoft.KeyVault/vaults/secrets/getSecret/action"}},
	{"21090545-7ca7-4776-b22c-e363652d74d2", "Key Vault Reader",
		"Read metadata of key vaults and its certificates, keys, and secrets, but not their values.",
		[]string{"Microsoft.KeyVault/*/read"}, []string{"Microsoft.KeyVault/vaults/*/read"}},

	{"7f951dda-4ed3-4680-a7ca-43fe172d538d", "AcrPull", "acr pull",
		[]string{"Microsoft.ContainerRegistry/registries/pull/read"}, nil},
	{"8311e382-0749-4cb8-b61a-304f252e45ec", "AcrPush", "acr push",
		[]string{"Microsoft.ContainerRegistry/registries/pull/read", "Microsoft.ContainerRegistry/registries/push/write"}, nil},

	{"ba92f5b4-2d11-453d-a403-e96b0029c9fe", "Storage Blob Data Contributor",
		"Read, write, and delete Azure Storage containers and blobs.",
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/*"},
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/*"}},
	{"2a2b9908-6ea1-4ae2-8e65-a410df84e7d1", "Storage Blob Data Reader",
		"Read and list Azure Storage containers and blobs.",
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/read"},
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read"}},
	{"b7e6dc6d-f1e8-4753-8033-0f276bb0955b", "Storage Blob Data Owner",
		"Provides full access to Azure Storage blob containers and data, including assigning POSIX access control.",
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/*"},
		[]string{"Microsoft.Storage/storageAccounts/blobServices/containers/blobs/*"}},
	{"974c5e8b-45b9-4653-ba55-5f855dd0fb88", "Storage Queue Data Contributor",
		"Read, write, and delete Azure Storage queues and queue messages.",
		[]string{"Microsoft.Storage/storageAccounts/queueServices/queues/*"},
		[]string{"Microsoft.Storage/storageAccounts/queueServices/queues/messages/*"}},
	{"0a9a7e1f-b9d0-4cc4-a60d-0319b160aaa3", "Storage Table Data Contributor",
		"Read, write, and delete Azure Storage tables and entities.",
		[]string{"Microsoft.Storage/storageAccounts/tableServices/tables/*"},
		[]string{"Microsoft.Storage/storageAccounts/tableServices/tables/entities/*"}},

	{"f1a07417-d97a-45cb-824c-7a7467783830", "Managed Identity Operator",
		"Read and Assign User Assigned Identity",
		[]string{"Microsoft.ManagedIdentity/userAssignedIdentities/*/read",
			"Microsoft.ManagedIdentity/userAssignedIdentities/*/assign/action"}, nil},
	{"e40ec5ca-96e0-45a2-b4ff-59039f2c2b59", "Managed Identity Contributor",
		"Create, Read, Update, and Delete User Assigned Identity",
		[]string{"Microsoft.ManagedIdentity/userAssignedIdentities/*"}, nil},

	{"4abbcc35-e782-43d8-92c5-2d3f1bd2253f", "Azure Kubernetes Service Cluster User Role",
		"List cluster user credential action.",
		[]string{"Microsoft.ContainerService/managedClusters/listClusterUserCredential/action"}, nil},
	{"0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8", "Azure Kubernetes Service Cluster Admin Role",
		"List cluster admin credential action.",
		[]string{"Microsoft.ContainerService/managedClusters/listClusterAdminCredential/action"}, nil},
	{"b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b", "Azure Kubernetes Service RBAC Cluster Admin",
		"Lets you manage all resources in the cluster.",
		[]string{"Microsoft.ContainerService/managedClusters/read"}, nil},
	{"4d97b98b-1d4f-4787-a291-c67834d212e7", "Network Contributor",
		"Lets you manage networks, but not access to them.",
		[]string{"Microsoft.Network/*"}, nil},
}

// roleNotActions holds the exclusions, for the roles that have any.
//
// A sixth column on builtInRole would have been nil for twenty-two of the
// twenty-three entries and would have broken every positional literal in
// the table above. Kept separate instead, which also makes the point: the
// difference between Owner and Contributor is ENTIRELY this.
//
// notActions are SUBTRACTED from actions, not denied -- an action one
// assignment excludes is still allowed if another assignment grants it,
// which is not what an explicit Deny does. authz.go's rule shape follows
// from that.
var roleNotActions = map[string][]string{
	// Contributor: everything except handing out access.
	"b24988ac-6180-42a0-ab88-20f7382dd24c": {
		"Microsoft.Authorization/*/Delete",
		"Microsoft.Authorization/*/Write",
		"Microsoft.Authorization/elevateAccess/Action",
	},
}

// notActionsFor is the exclusion list for a role, empty for most.
func notActionsFor(guid string) []string {
	for known, list := range roleNotActions {
		if eq(known, guid) {
			return list
		}
	}
	return nil
}

func registerAuthorizationHooks(s *Server) {
	s.registerHook(typeRoleAssignment, Hook{
		// ARM deletes a role assignment synchronously and answers with the
		// assignment it removed, rather than the 202-and-poll every real
		// resource gets.
		DeleteStatus:      200,
		DeleteReturnsBody: true,
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any)
			if str(props["roleDefinitionId"]) == "" || str(props["principalId"]) == "" {
				return &armError{400, "InvalidRoleAssignment",
					"A role assignment needs both properties.roleDefinitionId and properties.principalId."}
			}
			// The scope is implied by the path: a role assignment is an
			// extension resource, so its parent IS what it grants access to.
			props["scope"] = t.ParentID
			setDefault(props, "principalType", "ServicePrincipal")

			// createdOn is identity, not state: it must survive updates or
			// every refresh reports a change.
			now := time.Now().UTC().Format(time.RFC3339)
			created := now
			if existing != nil {
				if ep, ok := existing["properties"].(map[string]any); ok {
					if c := str(ep["createdOn"]); c != "" {
						created = c
					}
				}
			}
			props["createdOn"] = created
			props["updatedOn"] = now
			setDefault(props, "createdBy", DefaultObjectID)
			setDefault(props, "updatedBy", DefaultObjectID)
			return nil
		},
	})
}

// --- role definitions ------------------------------------------------------

// handleRoleDefinitions serves the built-in role definitions, which are not
// stored resources: they exist in every subscription and nobody creates them.
//
// Clients reach them two ways. azurerm_role_assignment with
// role_definition_name filters the list by name to find the GUID; anything
// holding a role_definition_id reads it back by ID.
func (s *Server) handleRoleDefinitions(w http.ResponseWriter, r *http.Request, t target) string {
	if r.Method != http.MethodGet {
		writeError(w, 405, "MethodNotAllowed",
			"Nimbus serves the built-in role definitions read-only; custom roles are not emulated.")
		return "custom role definition attempted"
	}

	if t.Kind == "resource" {
		role := roleByGUID(t.Name)
		if role == nil {
			writeError(w, 404, "RoleDefinitionDoesNotExist",
				"The role definition '%s' does not exist. Nimbus serves Azure's built-in roles only.", t.Name)
			return "unknown role definition"
		}
		writeJSON(w, 200, roleDefinitionJSON(*role, strings.TrimSuffix(t.ID, "/"+t.Name)))
		return ""
	}

	scope := t.ID + "/providers/" + typeRoleDefinition
	if strings.HasSuffix(strings.ToLower(t.ID), strings.ToLower(typeRoleDefinition)) {
		scope = t.ID
	}
	wantName := roleNameFilter(r.URL.Query().Get("$filter"))
	out := []any{}
	for _, role := range builtInRoles {
		if wantName != "" && !eq(role.Name, wantName) {
			continue
		}
		out = append(out, roleDefinitionJSON(role, scope))
	}
	writeJSON(w, 200, map[string]any{"value": out})
	if wantName != "" && len(out) == 0 {
		// Visible in the feed: an empty answer here is why a client reports it
		// cannot find a role, and the role may simply not be emulated yet.
		return "no built-in role named " + wantName
	}
	return ""
}

// roleNameFilter pulls the name out of an OData filter.
//
// Clients send `roleName eq 'Contributor'`, sometimes joined with other terms
// such as `atScopeAndBelow()`. Anything else is treated as no filter, which
// returns everything -- over-reporting is recoverable, inventing a match is
// not.
func roleNameFilter(filter string) string {
	low := strings.ToLower(filter)
	i := strings.Index(low, "rolename eq ")
	if i < 0 {
		return ""
	}
	rest := filter[i+len("rolename eq "):]
	rest = strings.TrimSpace(rest)
	if len(rest) == 0 || (rest[0] != '\'' && rest[0] != '"') {
		return ""
	}
	quote := rest[0]
	end := strings.IndexByte(rest[1:], quote)
	if end < 0 {
		return ""
	}
	return rest[1 : 1+end]
}

func roleByGUID(guid string) *builtInRole {
	for i := range builtInRoles {
		if eq(builtInRoles[i].GUID, guid) {
			return &builtInRoles[i]
		}
	}
	return nil
}

func roleDefinitionJSON(role builtInRole, scope string) map[string]any {
	actions := role.Actions
	if actions == nil {
		actions = []string{}
	}
	dataActions := role.DataActions
	if dataActions == nil {
		dataActions = []string{}
	}
	// The SERVED definition has to agree with what is ENFORCED. A client
	// that reads a role's permissions here and then has a request refused
	// on a different basis has no way to work out why -- the same family
	// of bug as a value that differs between reads.
	notActions := notActionsFor(role.GUID)
	if notActions == nil {
		notActions = []string{}
	}
	return map[string]any{
		"id":   scope + "/" + role.GUID,
		"name": role.GUID,
		"type": typeRoleDefinition,
		"properties": map[string]any{
			"roleName":    role.Name,
			"description": role.Description,
			// "type" here, confusingly, is the role's KIND, not the ARM type:
			// BuiltInRole or CustomRole.
			"type":             "BuiltInRole",
			"assignableScopes": []string{"/"},
			"permissions": []any{map[string]any{
				"actions":        actions,
				"notActions":     notActions,
				"dataActions":    dataActions,
				"notDataActions": []string{},
			}},
		},
	}
}
