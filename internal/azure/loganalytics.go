package azure

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"time"
)

// Log Analytics: the workspace resource.
//
// This was a stub for eleven milestones, and its comment said queries
// "would find nothing because nothing was ever sent to it". Since
// Milestone 15 something is: a container app's console is tailed into the
// workspace its environment names, and `az monitor log-analytics query`
// returns the rows. The store is logstore.go and the query surface is
// logquery.go.
//
// What is still true of this file is the identity: the two things clients
// read back are server-generated -- the workspace's customerId, which is
// also what the data plane is addressed by, and its shared keys. Both are
// derived from the resource id so they survive a restart, because an agent
// configuration generated from them would otherwise stop matching.
const typeLogAnalyticsWorkspace = "Microsoft.OperationalInsights/workspaces"

func registerLogAnalyticsHooks(s *Server) {
	s.registerHook(typeLogAnalyticsWorkspace, Hook{
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any)

			// customerId is the workspace's data-plane identity and never
			// changes; createdDate is a creation fact. Both must survive
			// updates or every refresh is a diff.
			props["customerId"] = stableGUID(strings.ToLower(t.ID) + "/customer")
			now := time.Now().UTC().Format(time.RFC3339)
			created := now
			if existing != nil {
				if ep, ok := existing["properties"].(map[string]any); ok {
					if c := str(ep["createdDate"]); c != "" {
						created = c
					}
				}
			}
			props["createdDate"] = created
			props["modifiedDate"] = now

			setDefault(props, "sku", map[string]any{"name": "PerGB2018", "lastSkuUpdate": created})
			setDefault(props, "retentionInDays", float64(30))
			setDefault(props, "features", map[string]any{
				"legacy":        float64(0),
				"searchVersion": float64(1),
				"enableLogAccessUsingOnlyResourcePermissions": true,
			})
			// -1 means no cap, which is what a workspace gets by default.
			setDefault(props, "workspaceCapping", map[string]any{"dailyQuotaGb": float64(-1)})
			setDefault(props, "publicNetworkAccessForIngestion", "Enabled")
			setDefault(props, "publicNetworkAccessForQuery", "Enabled")
			return nil
		},
	})
}

// workspaceAction answers the POST actions on a workspace. Keys come from an
// action and never from a GET, the same rule storage accounts follow.
func (s *Server) workspaceAction(t target) (any, bool) {
	res, err := s.store.Get(t.ID)
	if err != nil || !eq(res.Type, typeLogAnalyticsWorkspace) {
		return nil, false
	}
	if !eq(t.Action, "sharedKeys") {
		return nil, false
	}
	return map[string]any{
		"primarySharedKey":   workspaceKey(res.ID, "primary"),
		"secondarySharedKey": workspaceKey(res.ID, "secondary"),
	}, true
}

// workspaceKey derives a workspace's shared key from its ID, so the agent
// configuration someone generated from it keeps matching after a restart.
// Azure's are base64 of 64 random bytes; these are base64 of a hash, which is
// shorter but the same shape.
func workspaceKey(id, which string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(id) + "/" + which))
	return base64.StdEncoding.EncodeToString(sum[:])
}
