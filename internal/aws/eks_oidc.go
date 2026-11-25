package aws

import (
	"encoding/json"
	"net/http"
	"strings"
)

// The cluster's OIDC issuer: the discovery document and the JWKS.
//
// This is what makes IRSA possible. A pod's projected service-account
// token is signed by k3s with a key Nimbus holds; anything verifying that
// token fetches the public half from here, and `data.tls_certificate` --
// which every EKS Terraform module runs against the issuer -- fetches the
// TLS chain from the same URL.
//
// # Why it is served over TLS, on a port of its own
//
// Measured against the `tls` provider 4.x: `data.tls_certificate` REFUSES
// an `http://` URL outright with "Invalid URL scheme". Nimbus serves AWS
// over plain HTTP by design -- path-style S3 needs no certificate and no
// DNS -- so the issuer cannot live on that endpoint. AWS has a second,
// TLS listener for it, serving the same handler: the issuer is one more
// path on it, and S3 and STS become reachable over TLS at no extra cost.
//
// The alternative, answering on the Azure listener, works and was
// rejected as a shape: an AWS feature on the port called "Azure" is a
// seam somebody trips over, and `--clouds aws` alone would not serve it.

// oidcPrefix is where a cluster's issuer lives. Real EKS issues
// `https://oidc.eks.<region>.amazonaws.com/id/<24 hex>`; the shape that
// matters is that each cluster has its OWN issuer, because an OIDC
// provider resource is created per cluster and its thumbprint and
// audience are per cluster too.
const oidcPrefix = "/oidc/"

// IssuerURL is the issuer for one cluster. Empty when no TLS listener is
// configured, which is how the cluster knows not to claim an identity it
// cannot serve -- better than advertising a URL that refuses.
func (s *Server) IssuerURL(clusterName string) string {
	if s.cfg.TLSURL == "" {
		return ""
	}
	return s.cfg.TLSURL + oidcPrefix + oidcID(clusterName)
}

// oidcID is the opaque id in the issuer path. DERIVED from the cluster,
// not generated: the provider reads the issuer on every refresh and an
// OIDC provider resource records it, so a value that moved between reads
// would be a permanent diff -- and would invalidate every token already
// issued against it.
func oidcID(clusterName string) string {
	return strings.ToUpper(stableID("oidc/"+clusterName, 32))
}

// serveOIDC answers the two well-known documents, and nothing else.
//
// Routed BEFORE anything looks at a credential scope, like the token
// webhook: these are fetched by clients that sign nothing -- a Terraform
// data source, a Kubernetes API server, an SDK verifying a token.
func (s *Server) serveOIDC(w http.ResponseWriter, r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, oidcPrefix)
	id, doc, found := strings.Cut(rest, "/")
	if id == "" {
		return s.oidcNotFound(w, "no cluster id in the issuer path")
	}
	if !found {
		// The issuer itself. Real EKS answers 404 here; what matters is
		// that it is reachable for the TLS handshake, which is all
		// data.tls_certificate needs.
		return s.oidcNotFound(w, "the issuer has no document of its own")
	}
	switch doc {
	case ".well-known/openid-configuration":
		writeJSON(w, 200, s.openIDConfiguration(id))
		return ""
	case "keys":
		if s.signing == nil {
			return s.oidcNotFound(w, "Nimbus has no service-account signing key")
		}
		writeJSON(w, 200, s.signing.JWKS())
		return ""
	}
	return s.oidcNotFound(w, "no such OIDC document: "+doc)
}

func (s *Server) oidcNotFound(w http.ResponseWriter, why string) string {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(404)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": why})
	return "oidc: " + why
}

// openIDConfiguration is the discovery document.
//
// The fields are the ones an OIDC consumer actually reads: `issuer` must
// match the token's `iss` EXACTLY or every verification fails, and
// `jwks_uri` is where it goes next. The rest are advertised because the
// spec requires them present, not because anything here varies.
func (s *Server) openIDConfiguration(id string) map[string]any {
	issuer := s.cfg.TLSURL + oidcPrefix + id
	return map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              issuer + "/keys",
		"authorization_endpoint":                "urn:kubernetes:programmatic_authorization",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"claims_supported":                      []string{"sub", "iss"},
	}
}
