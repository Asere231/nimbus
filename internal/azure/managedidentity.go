package azure

import "strings"

// Managed identities.
//
// This is where Azure's identity model meets its resource model: an identity
// is a resource with three generated GUIDs, and everything that wants to be
// granted access carries one. Nothing here authenticates anything -- Nimbus
// issues a token to anyone who asks (see handleToken) -- but the IDs have to
// exist and have to be stable, because clients store them and then point role
// assignments at them.
const typeUserAssignedIdentity = "Microsoft.ManagedIdentity/userAssignedIdentities"

func registerIdentityHooks(s *Server) {
	s.registerHook(typeUserAssignedIdentity, Hook{
		// Deleting an identity is synchronous in Azure, and its generated
		// client says so plainly when it is not:
		//
		//	unexpected status 202 (202 Accepted) received with no body
		//
		// Measured against azurerm 4.81.0, where it fails the destroy.
		DeleteStatus: 200,
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any) // put() guarantees this
			props["principalId"] = identityGUID(t.ID, "principal")
			props["clientId"] = identityGUID(t.ID, "client")
			props["tenantId"] = s.cfg.TenantID
			return nil
		},
	})
}

// identityGUID derives an identity's GUIDs from its resource ID.
//
// Lower-cased first, because the same identity can be addressed with
// different casing: the identity resource knows its own canonical ID, but a
// resource referring to it in identity.userAssignedIdentities uses whatever
// the author typed. Both must produce the same principal, or a client reading
// the two sees one identity with two principals.
func identityGUID(resourceID, kind string) string {
	return stableGUID(strings.ToLower(resourceID) + "/" + kind)
}

// fillIdentity completes an `identity` block on ANY resource type.
//
// Azure fills these in server-side and clients read them straight back: an
// app's identity.principalId is what a role assignment is pointed at, so a
// missing one breaks the next resource in the graph rather than this one.
// Doing it generically in put() means a type Nimbus has never heard of still
// behaves correctly.
//
// Values already present win, including ones a previous write generated, so
// an identity's principal never changes under a client's feet.
func fillIdentity(s *Server, t target, body, existing map[string]any) {
	ident, _ := body["identity"].(map[string]any)
	if ident == nil {
		return
	}
	var prev map[string]any
	if existing != nil {
		prev, _ = existing["identity"].(map[string]any)
	}

	// "SystemAssigned", "SystemAssigned, UserAssigned" and "SystemAssigned,
	// UserAssigned" with other spacing are all one string in the API.
	// UserAssigned on its own has no system principal, and inventing one would
	// hand the client an ID that can never be authenticated.
	if strings.Contains(strings.ToLower(str(ident["type"])), "systemassigned") {
		carryOver(ident, prev, "principalId", stableGUID(t.ID+"/principal"))
		carryOver(ident, prev, "tenantId", s.cfg.TenantID)
	}

	// Each user-assigned entry is sent as an empty object keyed by the
	// identity's resource ID, and comes back filled in.
	entries, _ := ident["userAssignedIdentities"].(map[string]any)
	for key, v := range entries {
		entry, _ := v.(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		setDefault(entry, "principalId", identityGUID(key, "principal"))
		setDefault(entry, "clientId", identityGUID(key, "client"))
		entries[key] = entry
	}
}

// carryOver keeps a previously generated value if there is one, otherwise sets
// the fallback -- but never overwrites what the client sent.
func carryOver(dst, prev map[string]any, key, fallback string) {
	if v := str(dst[key]); v != "" {
		return
	}
	if prev != nil {
		if v := str(prev[key]); v != "" {
			dst[key] = v
			return
		}
	}
	dst[key] = fallback
}
