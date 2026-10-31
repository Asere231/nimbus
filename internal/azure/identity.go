package azure

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// handleMetadata serves the cloud metadata document. The azurerm provider's
// `metadata_host` setting (and ARM_METADATA_HOSTNAME) makes it fetch
// https://{host}/metadata/endpoints?api-version=2022-09-01 and use the
// endpoints in it for everything: login, Resource Manager, DNS suffixes.
//
// The 2022-09-01 schema is a single JSON object (not an array).
func (s *Server) handleMetadata(w http.ResponseWriter, r *http.Request) {
	u := s.cfg.PublicURL
	writeJSON(w, 200, map[string]any{
		"name":    "AzureCloud",
		"portal":  u + "/portal",
		"gallery": u + "/gallery/",
		"authentication": map[string]any{
			"loginEndpoint":    u,
			"audiences":        []string{u, u + "/"},
			"tenant":           "common",
			"identityProvider": "AAD",
		},
		// No trailing slash: go-azure-sdk joins it with "/" + path, and a
		// doubled slash makes its pollers compute the wrong resource path.
		"resourceManager":                       u,
		"media":                                 u + "/media/",
		"graph":                                 u + "/graph/",
		"graphAudience":                         u + "/graph/",
		"microsoftGraphResourceId":              u + "/graph/",
		"batch":                                 u + "/batch/",
		"vmImageAliasDoc":                       u + "/vmimagealiases.json",
		"activeDirectoryDataLake":               u + "/datalake/",
		"sqlManagement":                         u + "/sql/",
		"appInsightsResourceId":                 u + "/appinsights/",
		"appInsightsTelemetryChannelResourceId": u + "/appinsights-telemetry/",
		"attestationResourceId":                 u + "/attestation/",
		"synapseAnalyticsResourceId":            u + "/synapse/",
		"logAnalyticsResourceId":                u + "/loganalytics/",
		"ossrDbmsResourceId":                    u + "/ossrdbms/",
		// Data-plane DNS suffixes. These stay on the .localhost family so that
		// nothing ever resolves to real Azure.
		//
		// The storage suffix carries our PORT, which real Azure's never does.
		// azurerm builds data-plane URLs as `https://%s.blob.%s/` with the
		// suffix as the second argument, so a bare hostname would send it to
		// port 443 -- which needs root to bind. Substituting
		// "storage.nimbus.localhost:4701" produces a valid URL on the port we
		// already serve, and SNI still sees the bare hostname so the
		// certificate matches. See TestStorageSuffixCarriesThePort.
		"suffixes": map[string]any{
			"storage":                             s.storageSuffix(),
			"keyVaultDns":                         s.keyVaultSuffix(),
			"acrLoginServer":                      "acr.nimbus.localhost",
			"sqlServerHostname":                   "sql.nimbus.localhost",
			"mysqlServerEndpoint":                 "mysql.nimbus.localhost",
			"postgresqlServerEndpoint":            "postgres.nimbus.localhost",
			"mariadbServerEndpoint":               "mariadb.nimbus.localhost",
			"azureDataLakeStoreFileSystem":        "datalake.nimbus.localhost",
			"azureDataLakeAnalyticsCatalogAndJob": "datalakeanalytics.nimbus.localhost",
			"azureFrontDoorEndpointSuffix":        "frontdoor.nimbus.localhost",
			"storageSyncEndpointSuffix":           "storagesync.nimbus.localhost",
			"mhsmDns":                             "mhsm.nimbus.localhost",
			"synapseAnalytics":                    "synapse.nimbus.localhost",
			"attestationEndpoint":                 "attest.nimbus.localhost",
		},
	})
}

// handleToken issues a token for any client ID and secret. Clients only parse
// the JWT's claims (oid, tid, appid); nothing verifies the signature, so the
// emulator signs nothing. It is a local tool: there is no real security here.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	tenant := s.cfg.TenantID
	if parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/"); len(parts) > 0 && parts[0] != "" && !eq(parts[0], "common") && !eq(parts[0], "organizations") {
		tenant = parts[0]
	}
	clientID := r.Form.Get("client_id")
	if clientID == "" {
		clientID = s.cfg.ClientID
	}
	aud := s.cfg.PublicURL + "/"
	if scope := r.Form.Get("scope"); scope != "" {
		aud = strings.TrimSuffix(strings.Fields(scope)[0], ".default")
	} else if res := r.Form.Get("resource"); res != "" {
		aud = res
	}
	now := time.Now().Unix()
	exp := now + 3600
	claims := map[string]any{
		"aud":   aud,
		"iss":   fmt.Sprintf("%s/%s/v2.0", s.cfg.PublicURL, tenant),
		"iat":   now,
		"nbf":   now,
		"exp":   exp,
		"oid":   DefaultObjectID,
		"sub":   DefaultObjectID,
		"tid":   tenant,
		"appid": clientID,
		"azp":   clientID,
		"idtyp": "app",
		"ver":   "2.0",
		"roles": []string{},
	}
	writeJSON(w, 200, map[string]any{
		"token_type":     "Bearer",
		"expires_in":     3599,
		"ext_expires_in": 3599,
		"expires_on":     fmt.Sprint(exp),
		"access_token":   fakeJWT(claims),
	})
}

// handleOpenID lets MSAL-based clients (the az CLI, the Azure SDKs for
// Python/JS/.NET) discover the token endpoint.
func (s *Server) handleOpenID(w http.ResponseWriter, r *http.Request) {
	tenant := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]
	base := fmt.Sprintf("%s/%s", s.cfg.PublicURL, tenant)
	writeJSON(w, 200, map[string]any{
		"issuer":                                base + "/v2.0",
		"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
		"token_endpoint":                        base + "/oauth2/v2.0/token",
		"device_authorization_endpoint":         base + "/oauth2/v2.0/devicecode",
		"jwks_uri":                              base + "/discovery/v2.0/keys",
		"tenant_region_scope":                   "NA",
		"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
		"subject_types_supported":               []string{"pairwise"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
		"scopes_supported":                      []string{"openid", "profile", "email", "offline_access"},
	})
}

func (s *Server) handleInstanceDiscovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"tenant_discovery_endpoint": s.cfg.PublicURL + "/common/v2.0/.well-known/openid-configuration",
		"api-version":               "1.1",
		"metadata":                  []any{},
	})
}

func fakeJWT(claims map[string]any) string {
	enc := base64.RawURLEncoding
	h, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "nimbus"})
	c, _ := json.Marshal(claims)
	return enc.EncodeToString(h) + "." + enc.EncodeToString(c) + "." + enc.EncodeToString([]byte("nimbus-local-unsigned"))
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
