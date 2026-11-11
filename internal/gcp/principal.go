package gcp

import (
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// Who is calling, on GCP.
//
// The same trade as the other two clouds, and here it costs even less.
// There is no OAuth: the provider is handed an `access_token` directly and
// Nimbus has never looked at it (docs/ROADMAP.md says so). So the token
// IS the claim -- and conveniently, GCP writes its policy members in
// exactly the form a token can carry:
//
//	access_token = "serviceAccount:app@nimbus-local.iam.gserviceaccount.com"
//
// which is character-for-character what appears in a binding's `members`.
// No mapping, no derivation, nothing stored: the string in the
// configuration is the string in the policy.
//
// Measured before anything was built on it, because it was the open risk:
// the google provider 6.x passes an access_token through verbatim,
// including one containing `:` and `@`, and it arrives in the
// Authorization header intact.
//
// A token that is NOT member-shaped -- `nimbus`, which is what every
// existing example uses -- is the default identity, which holds no
// bindings and is unrestricted. That is what keeps the flag free when it
// is on, as AWS's account root and Azure's default client_id do.

// memberPrefixes are the member kinds a policy can name. A token has to
// look like one of these to be a claim of identity rather than an opaque
// value; `nimbus` is not, and is therefore the default identity.
var memberPrefixes = []string{
	"serviceAccount:", "user:", "group:", "domain:", "principal:", "principalSet:",
}

// resolvePrincipal works out who is asking, from the bearer token.
func (s *Server) resolvePrincipal(r *http.Request) authz.Principal {
	member := bearerToken(r)
	if member == "" {
		member = s.cfg.Principal
	}
	if !isMember(member) {
		return authz.Principal{
			ID: "nimbus-default", Name: "the default identity",
			Kind: "serviceAccount", Unrestricted: true,
		}
	}
	kind, name, _ := strings.Cut(member, ":")
	return authz.Principal{ID: member, Name: name, Kind: kind}
}

func isMember(token string) bool {
	for _, p := range memberPrefixes {
		if strings.HasPrefix(token, p) && len(token) > len(p) {
			return true
		}
	}
	return false
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, prefix))
}
