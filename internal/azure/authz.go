package azure

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"github.com/Asere231/nimbus/internal/core"
)

// Azure RBAC, evaluated.
//
// `internal/authz` holds the decision and the order rules settle in, and
// knows no cloud's policy language. This file is the Azure half: role
// assignments and role definitions compiled into `authz.Rule`s.
//
// Three things make Azure cheaper than AWS was, and one makes it dearer.
//
//   - **The action name is DERIVABLE.** `parseTarget` already yields the
//     resource's full type, and an ARM action is exactly
//     `{type}/{read|write|delete}`, or `{type}/{verb}/action` for a POST.
//     S3 needed a hand-written table because AWS's own names are
//     inconsistent; ARM's are not, so there is no table here to drift.
//   - **Scope inheritance is ID-prefix matching**, the same shape the store
//     already uses to cascade a delete. An assignment at a resource group
//     governs everything beneath it, which is the part of RBAC a learner
//     most needs to see work.
//   - **A wildcard spans segments.** `Microsoft.Storage/*/read` really does
//     match `Microsoft.Storage/storageAccounts/blobServices/containers/read`
//     in Azure, so `authz.Glob` needs no segment awareness -- unlike a TLS
//     wildcard, which matches exactly one label (Milestone 2 learned that
//     the hard way).
//
// The dearer part: **`notActions` are SUBTRACTED, not denied.** An action
// excluded by one assignment's notActions is still allowed if another
// assignment grants it -- which is not what an explicit Deny does. So a
// whole assignment compiles to ONE rule answering "does this assignment
// grant this action", rather than to an Allow per pattern plus a Deny per
// exclusion. That keeps the semantics right and, usefully, needed nothing
// from the core: `authz.Rule` was general enough as written.

// assignmentRule is one role assignment, ready to evaluate.
type assignmentRule struct {
	source     string
	scope      string
	actions    []string
	notActions []string
	// Azure's OTHER action axis, and the one people are surprised by.
	// `actions` govern the RESOURCE -- creating a vault, reading its
	// properties -- and `dataActions` govern what is INSIDE it. They do
	// not mix: Owner and Contributor hold Actions ["*"] and no data
	// actions at all, so a Contributor can delete a key vault and
	// cannot read a secret out of it.
	dataActions    []string
	notDataActions []string
	unsupported    error
}

func (a *assignmentRule) Effect() authz.Effect { return authz.Allow }
func (a *assignmentRule) Source() string       { return a.source }

func (a *assignmentRule) Applies(req authz.Request) (bool, error) {
	// Scope first, so an assignment elsewhere in the tree says nothing
	// about this request -- including when the role behind it is one
	// Nimbus cannot read.
	if !scopeCovers(a.scope, req.Resource) {
		return false, nil
	}
	if a.unsupported != nil {
		return false, a.unsupported
	}
	// Which axis, from the request rather than from the action's shape:
	// a data action is spelled like any other and only the caller knows
	// which plane it arrived on. Carried in Context, so `internal/authz`
	// needed no change for this either.
	if req.Context[PlaneKey] == DataPlane {
		return grantsAction(a.dataActions, a.notDataActions, req.Action), nil
	}
	return grantsAction(a.actions, a.notActions, req.Action), nil
}

// PlaneKey and DataPlane mark a request as a data-plane one, which
// decides whether an assignment's actions or its dataActions apply.
const (
	PlaneKey  = "azure:plane"
	DataPlane = "data"
)

// grantsAction is Azure's own rule: an action is granted when some pattern
// in actions matches it and no pattern in notActions does. The patterns
// were lower-cased at compile time, because RBAC matching ignores case.
func grantsAction(actions, notActions []string, action string) bool {
	action = strings.ToLower(action)
	matched := false
	for _, p := range actions {
		if authz.Glob(p, action) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, p := range notActions {
		if authz.Glob(p, action) {
			return false
		}
	}
	return true
}

// scopeCovers reports whether a scope governs a resource ID.
//
// Segment-aligned, and NOT a plain prefix: a resource group called `rg-two`
// starts with `rg`, so `strings.HasPrefix` alone would silently widen every
// assignment made at `.../resourceGroups/rg` to cover it. Case-insensitive,
// because ARM resolves IDs that way and answers with whatever casing was
// used at creation -- so an assignment and a resource can legitimately
// disagree about it.
func scopeCovers(scope, resource string) bool {
	scope = strings.TrimSuffix(strings.ToLower(scope), "/")
	resource = strings.TrimSuffix(strings.ToLower(resource), "/")
	if scope == "" {
		// "/" is the root scope: the whole tenant.
		return true
	}
	if resource == scope {
		return true
	}
	return strings.HasPrefix(resource, scope+"/")
}

// --- the action an ARM request needs --------------------------------------

// armAction names the permission a request needs, and the scope it acts on.
//
// The scope is what an assignment is compared against: a resource's own ID,
// or for a collection the scope it sits in -- listing the VNets in a
// resource group is an action on that resource group.
//
// An empty action means "nothing here knows what this request needs", and
// the caller refuses rather than guessing: a gap that fails open is a
// permission nobody granted.
func armAction(t target, method string) (action, scope string) {
	verb := ""
	switch method {
	case http.MethodGet, http.MethodHead:
		verb = "read"
	case http.MethodPut, http.MethodPatch:
		verb = "write"
	case http.MethodDelete:
		verb = "delete"
	case http.MethodPost:
		verb = "action"
	default:
		return "", ""
	}

	switch t.Kind {
	case "resource", "collection":
		if method == http.MethodPost {
			// parseTarget puts a POST's verb on the end of Type, so the
			// action name is that plus "/action" -- exactly how Azure
			// spells it.
			return t.Type + "/action", t.ID
		}
		return t.Type + "/" + verb, t.ID

	case "resourceGroup":
		// Not a provider type, and its real action name has
		// `subscriptions/` in the middle: Azure writes
		// Microsoft.Resources/subscriptions/resourceGroups/write.
		return "Microsoft.Resources/subscriptions/resourceGroups/" + verb, t.ID
	case "resourceGroups":
		return "Microsoft.Resources/subscriptions/resourceGroups/read",
			"/subscriptions/" + t.Subscription
	case "rgAction":
		return "Microsoft.Resources/subscriptions/resourceGroups/" + t.Action + "/action", t.ID
	case "rgResources":
		return "Microsoft.Resources/subscriptions/resourceGroups/resources/read", t.ID
	case "subscription":
		return "Microsoft.Resources/subscriptions/read", t.ID
	case "subscriptions":
		return "Microsoft.Resources/subscriptions/read", "/"
	case "subResources":
		return "Microsoft.Resources/subscriptions/resources/read", t.ID
	case "locations":
		return "Microsoft.Resources/subscriptions/locations/read",
			"/subscriptions/" + t.Subscription
	case "providers", "provider":
		return "Microsoft.Resources/providers/read", "/subscriptions/" + t.Subscription
	case "providerRegister":
		return "Microsoft.Resources/subscriptions/providers/register/action",
			"/subscriptions/" + t.Subscription
	}
	return "", ""
}

// --- compiling a principal's assignments ----------------------------------

// rulesFor gathers every role assignment naming a principal and compiles
// each into a rule.
//
// Assignments are read from the store on every request, which is cheap --
// it is in memory -- and means an assignment made seconds ago takes effect
// at once. Compiling is a handful of string lists; there is no policy
// document to re-parse the way AWS's IAM has, so unlike `policyCache` this
// needs no caching.
func (s *Server) rulesFor(principal string) []authz.Rule {
	var rules []authz.Rule
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return eq(r.Type, typeRoleAssignment)
	}) {
		var body map[string]any
		if json.Unmarshal(res.Body, &body) != nil {
			continue
		}
		props, _ := body["properties"].(map[string]any)
		if props == nil || !eq(str(props["principalId"]), principal) {
			continue
		}
		rules = append(rules, compileAssignment(res.Name, props))
	}
	return rules
}

// compileAssignment turns one assignment into a rule. A role definition
// Nimbus does not know compiles to a rule that REFUSES and names it --
// never one that grants nothing, which is indistinguishable from a role
// that simply does not cover the action. The same decision as an unknown
// AWS managed policy.
func compileAssignment(name string, props map[string]any) authz.Rule {
	scope := str(props["scope"])
	defID := str(props["roleDefinitionId"])
	guid := defID
	if i := strings.LastIndex(defID, "/"); i >= 0 {
		guid = defID[i+1:]
	}

	rule := &assignmentRule{scope: scope}

	role := roleByGUID(guid)
	if role == nil {
		rule.source = fmt.Sprintf("role assignment %s at %s", name, scope)
		rule.unsupported = fmt.Errorf(
			"Nimbus does not know the role definition %s, so it cannot tell what that "+
				"assignment grants; it serves Azure's built-in roles only", guid)
		return rule
	}

	rule.source = fmt.Sprintf("role assignment %s (%s) at %s", name, role.Name, scope)
	rule.actions = lowerAll(role.Actions)
	rule.notActions = lowerAll(notActionsFor(role.GUID))
	rule.dataActions = lowerAll(role.DataActions)
	return rule
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

// --- the enforcement point ------------------------------------------------

// authorize decides whether an ARM request may proceed, returning nil when
// it may and the refusal when it may not, plus the note for the activity
// feed. A denial nobody can see is worse than no enforcement, because then
// an apply simply fails.
//
// Only ARM paths are judged. The identity endpoints -- the token, the
// metadata document, OpenID discovery, instance discovery -- are the
// bootstrap: a client has no principal until it has a token, so refusing
// those would make the flag unusable. An operation poll is likewise not a
// resource action; the write that started it was already authorized.
func (s *Server) authorize(r *http.Request) (*armError, string) {
	t, ok := parseTarget(r.URL.Path)
	if !ok {
		return nil, ""
	}

	p := s.resolvePrincipal(r)
	// An unrestricted principal is allowed before the request is even
	// classified. Whether an action is mapped is a question about POLICY,
	// and this identity has none -- the lesson AWS's root paid for.
	if p.Unrestricted {
		return nil, ""
	}

	action, scope := armAction(t, r.Method)
	if action == "" {
		return authorizationFailed(p, r.Method+" "+r.URL.Path, scope,
				fmt.Sprintf("Nimbus cannot yet work out which permission %s %s needs, "+
					"so with --enforce-iam it is refused rather than allowed", r.Method, r.URL.Path)),
			"denied: unmapped ARM request " + r.Method + " " + r.URL.Path
	}

	d := authz.Evaluate(authz.Request{
		Principal: p,
		Action:    action,
		Resource:  scope,
	}, s.rulesFor(p.ID))
	if d.Allowed {
		return nil, ""
	}
	return authorizationFailed(p, action, scope, d.Reason),
		"denied: " + action + " for " + p.ID + " -- " + d.Reason
}

// authorizationFailed is the refusal, in the shape real Azure sends it.
//
// `AuthorizationFailed` with a 403, and a message naming the principal,
// the action and the scope -- which is what Azure's own message does, and
// what makes a failed apply legible without a debug log.
func authorizationFailed(p authz.Principal, action, scope, reason string) *armError {
	return &armError{
		Status: http.StatusForbidden,
		Code:   "AuthorizationFailed",
		Message: fmt.Sprintf(
			"The client '%s' with object id '%s' does not have authorization to perform action '%s' "+
				"over scope '%s' or the scope is invalid. %s",
			p.Name, p.ID, action, scope, reason),
	}
}

// --- explaining ------------------------------------------------------------

// ExplainAccess answers what a principal may do to one resource.
//
// The actions are DERIVED from the resource's own type, which is the same
// property that makes the enforcement point need no table: an ARM type
// plus read/write/delete is exactly what the permissions are called. So
// this cannot drift from what is enforced, because both compute the name
// the same way.
func (s *Server) ExplainAccess(principal, resourceID, resourceType string) authz.Report {
	p := s.principalFor(principal)
	report := authz.Report{
		Principal: p.ID, Kind: p.Kind, Unrestricted: p.Unrestricted,
		Enforcing: s.cfg.EnforceIAM, Resource: resourceID,
	}
	if resourceType == "" {
		return report
	}
	actions := []string{
		resourceType + "/read",
		resourceType + "/write",
		resourceType + "/delete",
	}
	rules := s.rulesFor(p.ID)
	report.Checks = authz.Explain(p, resourceID, actions, rules)

	// A key vault's DATA actions, which are the interesting ones and
	// are invisible from its control-plane permissions: a Contributor
	// passes all three checks above and can read nothing inside. This
	// is the Milestone 14 lesson about a bucket's contents, one cloud
	// over -- a container's own permissions are not what people want to
	// know about it.
	if eq(resourceType, typeKeyVault) {
		for _, action := range keyvault.DataActions() {
			report.Checks = append(report.Checks,
				explainOnPlane(p, resourceID, action, rules))
		}
	}
	return report
}

// ExplainOne answers a single question, which is what `nimbus can` asks.
func (s *Server) ExplainOne(principal, action, resource string) authz.Check {
	p := s.principalFor(principal)
	return explainOnPlane(p, resource, action, s.rulesFor(p.ID))
}

// explainOnPlane evaluates one action on whichever axis it belongs to.
//
// Getting this wrong would be worse than not answering: asked whether a
// Contributor may read a secret, a control-plane evaluation says YES,
// because Contributor holds `Actions: ["*"]`. That is precisely the
// confusion dataActions exist to remove, so the explainer would be
// teaching the thing the enforcement just stopped.
func explainOnPlane(p authz.Principal, resource, action string, rules []authz.Rule) authz.Check {
	req := authz.Request{Principal: p, Action: action, Resource: resource}
	if isDataAction(action) {
		req.Context = map[string]string{PlaneKey: DataPlane}
	}
	d := authz.Evaluate(req, rules)
	return authz.Check{
		Action: action, Allowed: d.Allowed, Rule: d.Source,
		Reason: d.Reason, Unevaluated: d.Unevaluated,
	}
}

// isDataAction asks the data plane itself, rather than guessing from the
// action's shape: `Microsoft.KeyVault/vaults/secrets/delete` is a data
// action and `Microsoft.KeyVault/vaults/delete` is not, and no pattern
// separates those two.
func isDataAction(action string) bool {
	for _, a := range keyvault.DataActions() {
		if eq(a, action) {
			return true
		}
	}
	return false
}

// principalFor resolves a named principal, or the default identity when
// no name is given -- so the inspector has something to show before
// anyone has chosen a principal to ask about.
func (s *Server) principalFor(principal string) authz.Principal {
	if principal == "" {
		principal = s.cfg.Principal
	}
	if principal == "" || eq(principal, DefaultObjectID) ||
		eq(principal, principalFromClientID(s.cfg.ClientID)) {
		return authz.Principal{
			ID: DefaultObjectID, Name: "the default identity",
			Kind: "servicePrincipal", Unrestricted: true,
		}
	}
	return authz.Principal{ID: principal, Name: principal, Kind: "servicePrincipal"}
}
