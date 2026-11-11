package azure

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// Who is calling, on Azure.
//
// The same trade as AWS, reached the same way: Nimbus verifies nothing, so
// identity is whatever the client claims -- which is fine, because the
// question being answered is "would these permissions work", not "is this
// caller genuine". What is needed is a way for a client to claim a
// DIFFERENT identity, so one Nimbus can show two principals getting
// different answers.
//
// Here that mechanism already existed and was unused. `handleToken` issues
// an unsigned JWT whose claims every client parses, and it already echoes
// the request's `client_id` into `appid`. So:
//
//   - the token endpoint derives `oid` from the client_id it was given;
//   - the ARM enforcement point reads `oid` back out of the Bearer token.
//
// An example then sets azurerm's `client_id` to the same GUID it uses as
// `azurerm_role_assignment.principal_id` -- one value in two places, with
// the connection visible in the configuration rather than hidden in a flag.
//
// A caller with no assignments at all is UNRESTRICTED, as AWS's account
// root is. Azure has no root, so this is the one place the two clouds'
// models are genuinely bridged rather than mirrored: the justification is
// not "Azure works like this" but "turning the flag on must not break a
// configuration that says nothing about RBAC". docs/ROADMAP.md says so.

// principalFromClientID turns a client_id into an object id.
//
// A GUID is used as itself, which is what makes the example readable: the
// same value appears as `client_id` and as the assignment's
// `principalId`. Anything else is hashed into a stable GUID, so a
// client_id like "my-app" still identifies something consistently rather
// than being rejected.
func principalFromClientID(clientID string) string {
	if clientID == "" {
		return DefaultObjectID
	}
	if looksLikeGUID(clientID) {
		return strings.ToLower(clientID)
	}
	sum := sha256.Sum256([]byte("nimbus/principal/" + clientID))
	h := fmt.Sprintf("%x", sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func looksLikeGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// resolvePrincipal works out who is asking, from the bearer token's `oid`.
//
// A request with no token, or one whose principal holds no assignments, is
// unrestricted -- so the caller checks that before classifying the request.
func (s *Server) resolvePrincipal(r *http.Request) authz.Principal {
	oid := objectIDFromToken(r.Header.Get("Authorization"))
	if oid == "" {
		oid = s.cfg.Principal
	}
	// The DEFAULT credential is the unrestricted one, and that has to
	// include the principal derived from the default client_id -- not just
	// DefaultObjectID.
	//
	// Measured the hard way: every Azure example sets `client_id` to
	// DefaultClientID, so the token's oid is derived from THAT, and
	// checking only DefaultObjectID refused every request in
	// azure-basic the moment the flag was on. Turning enforcement on must
	// cost nothing for a configuration that says nothing about RBAC,
	// exactly as AWS's account root makes it free there.
	//
	// What it is NOT is "a principal with no assignments is unrestricted".
	// That would mean an apply ran unrestricted until it created its first
	// assignment and then abruptly lost access to everything else -- a
	// worse surprise than either alternative.
	if oid == "" || eq(oid, DefaultObjectID) || eq(oid, principalFromClientID(s.cfg.ClientID)) {
		return authz.Principal{
			ID: DefaultObjectID, Name: "the default identity", Kind: "servicePrincipal",
			Unrestricted: true,
		}
	}
	return authz.Principal{ID: oid, Name: oid, Kind: "servicePrincipal"}
}

// objectIDFromToken decodes the `oid` claim of an unsigned bearer JWT.
//
// Nothing is verified: the token Nimbus issues is signed with the literal
// string "nimbus-local-unsigned", and checking that would prove only that
// Nimbus issued it. The claims are the payload of a three-part dotted
// base64url string, which is the shape every client already parses.
func objectIDFromToken(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	parts := strings.Split(strings.TrimSpace(strings.TrimPrefix(header, prefix)), ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return str(claims["oid"])
}
