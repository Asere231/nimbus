package gcp

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
)

// GCP IAM, evaluated.
//
// The third policy language on `internal/authz`, and the one that settles
// whether that core was really cloud-neutral: it has needed no change for
// any of them.
//
// GCP's model is the simplest of the three on paper -- a policy is a list
// of bindings, each a role and the members holding it, and a role is a set
// of permissions. What it shares with Azure, and does NOT share with AWS,
// is that a binding is a pure GRANT: there is no Deny, and no statement
// ordering to resolve. So, as with an Azure role assignment, a binding
// compiles to ONE rule answering "does this grant this permission", and
// the engine's deny-wins ordering simply never fires.
//
// Two things about scope:
//
//   - A binding in the PROJECT policy grants everywhere, because every
//     resource here belongs to the one project. That makes project-level
//     inheritance trivial where Azure's needed segment-aligned prefix
//     matching.
//   - A binding on a RESOURCE's own policy (a bucket, a topic, a Cloud Run
//     service) grants on that resource only.
//
// `roles/editor` is the one role that needs an exclusion rather than a
// plain allow-list, exactly as Azure's Contributor did -- which is why
// bindingRule carries `excluded` and is shaped like Azure's
// assignmentRule. Two clouds arriving at the same shape independently is
// the strongest evidence the interface is the right one.

// bindingRule is one binding -- a role held by a member -- ready to
// evaluate.
type bindingRule struct {
	source      string
	resource    string // "" means the whole project
	permissions []string
	excluded    []string
	unsupported error
}

func (b *bindingRule) Effect() authz.Effect { return authz.Allow }
func (b *bindingRule) Source() string       { return b.source }

func (b *bindingRule) Applies(req authz.Request) (bool, error) {
	// Scope first, so a binding on another resource says nothing about
	// this request -- including when its role is one Nimbus cannot read.
	if !coversResource(b.resource, req.Resource) {
		return false, nil
	}
	if b.unsupported != nil {
		return false, b.unsupported
	}
	return grantsPermission(b.permissions, b.excluded, req.Action), nil
}

func grantsPermission(permissions, excluded []string, permission string) bool {
	permission = strings.ToLower(permission)
	matched := false
	for _, p := range permissions {
		if authz.Glob(p, permission) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	for _, p := range excluded {
		if authz.Glob(p, permission) {
			return false
		}
	}
	return true
}

func eqFold(a, b string) bool { return strings.EqualFold(a, b) }

// --- the role catalogue ---------------------------------------------------

// gcpRole is a curated role: what it grants, and what it takes back.
type gcpRole struct {
	Permissions []string
	Excluded    []string
}

// gcpRoles is the catalogue, and it is a CURATION rather than a copy.
//
// Google publishes thousands of permissions across hundreds of predefined
// roles; transcribing them is a milestone's work and a dataset that drifts
// silently. So this holds the three basic roles plus the predefined ones an
// example needs, expressed as the wildcard patterns those roles really
// amount to -- and **any role not here REFUSES, naming itself**, which is
// the same choice as an unknown AWS managed policy or an unknown Azure
// role definition.
//
// The basic roles are honest approximations and are documented as such in
// docs/ROADMAP.md: real `roles/viewer` is an enormous explicit list, and
// `*.get` plus `*.list` is what it comes to in practice.
var gcpRoles = map[string]gcpRole{
	"roles/owner": {Permissions: []string{"*"}},

	// Editor is everything except handing out access, which is the one
	// role here that cannot be a plain allow-list -- the same shape as
	// Azure's Contributor.
	"roles/editor": {
		Permissions: []string{"*"},
		Excluded:    []string{"*.setIamPolicy"},
	},

	"roles/viewer": {
		Permissions: []string{"*.get", "*.list", "*.getIamPolicy", "*.testIamPermissions"},
	},

	"roles/storage.admin":        {Permissions: []string{"storage.*"}},
	"roles/storage.objectAdmin":  {Permissions: []string{"storage.objects.*"}},
	"roles/storage.objectViewer": {Permissions: []string{"storage.objects.get", "storage.objects.list"}},
	"roles/storage.objectCreator": {
		Permissions: []string{"storage.objects.create"},
	},

	"roles/compute.admin": {Permissions: []string{"compute.*"}},
	// networkAdmin manages networks and does NOT hand out access --
	// Compute networks have no IAM policy of their own. The `.*` patterns
	// would have granted `setIamPolicy` by accident, which is the single
	// worst permission for an approximation to be loose about, so it is
	// excluded. Caught by a test, not by reading the catalogue.
	"roles/compute.networkAdmin": {
		Permissions: []string{
			"compute.networks.*", "compute.subnetworks.*", "compute.firewalls.*",
			"compute.routes.*", "compute.routers.*",
		},
		Excluded: []string{"*.setIamPolicy"},
	},
	"roles/compute.viewer": {Permissions: []string{"compute.*.get", "compute.*.list"}},

	"roles/pubsub.admin":     {Permissions: []string{"pubsub.*"}},
	"roles/pubsub.publisher": {Permissions: []string{"pubsub.topics.publish"}},

	"roles/container.admin":  {Permissions: []string{"container.*"}},
	"roles/container.viewer": {Permissions: []string{"container.*.get", "container.*.list"}},

	"roles/iam.serviceAccountAdmin": {Permissions: []string{"iam.serviceAccounts.*"}},
	"roles/run.admin":               {Permissions: []string{"run.*"}},
	"roles/run.viewer":              {Permissions: []string{"run.*.get", "run.*.list"}},
}

// --- compiling a principal's bindings -------------------------------------

// rulesFor gathers every binding naming a member and compiles each.
//
// The project policy is read from the store on each request, which is
// cheap and means a binding set seconds ago takes effect at once -- the
// same reasoning as Azure's assignments, and unlike AWS's IAM documents
// there is no JSON policy to re-parse, so no cache is needed.
func (s *Server) rulesFor(member string) []authz.Rule {
	var rules []authz.Rule
	policy := s.storedPolicy()
	bindings, _ := policy["bindings"].([]any)
	for i, raw := range bindings {
		b, _ := raw.(map[string]any)
		if b == nil {
			continue
		}
		if !bindingNames(b, member) {
			continue
		}
		rules = append(rules, compileBinding(
			fmt.Sprintf("project IAM binding #%d (%s)", i+1, str(b["role"])),
			str(b["role"]), ""))
	}
	// And every binding attached to a resource of its own. The two sets
	// are simply concatenated: a resource binding is another binding,
	// which is GCP's whole model and why this cloud needed no
	// combination rule of its own.
	return append(rules, s.resourceRulesFor(member)...)
}

// bindingNames reports whether a binding's members include this one.
// Members are written exactly as a client sends them --
// `serviceAccount:app@project.iam.gserviceaccount.com` -- which is also
// how a caller claims an identity here, so no translation is needed.
func bindingNames(b map[string]any, member string) bool {
	members, _ := b["members"].([]any)
	for _, m := range members {
		if eqFold(str(m), member) {
			return true
		}
		// allUsers and allAuthenticatedUsers are the two members that are
		// not an identity. Honoured because a bucket made public with
		// them really is readable by anyone.
		switch str(m) {
		case "allUsers", "allAuthenticatedUsers":
			return true
		}
	}
	return false
}

// compileBinding turns one binding into a rule. A role Nimbus does not
// bundle REFUSES and names itself -- never grants nothing, which is
// indistinguishable from a role that does not cover the permission.
func compileBinding(source, role, resource string) authz.Rule {
	rule := &bindingRule{source: source, resource: resource}

	def, ok := gcpRoles[role]
	if !ok {
		rule.unsupported = fmt.Errorf(
			"Nimbus does not bundle the role %s, so it cannot tell what that binding grants; "+
				"bundled roles: %s", role, strings.Join(bundledRoleNames(), ", "))
		return rule
	}
	rule.permissions = lowerAll(def.Permissions)
	rule.excluded = lowerAll(def.Excluded)
	return rule
}

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(s)
	}
	return out
}

func bundledRoleNames() []string {
	out := make([]string, 0, len(gcpRoles))
	for name := range gcpRoles {
		out = append(out, name)
	}
	// Stable, so an error message does not change between runs.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// --- the enforcement point ------------------------------------------------

// authorize decides whether a request may proceed, returning nil when it
// may and the refusal when it may not, plus the note for the feed.
func (s *Server) authorize(r *http.Request, parts []string) (*apiError, string) {
	p := s.resolvePrincipal(r)
	// Unrestricted before classification: whether a permission is mapped
	// is a question about policy, and this identity has none. The lesson
	// AWS's root paid for.
	if p.Unrestricted {
		return nil, ""
	}

	permission, resource := permissionFor(r, parts)
	// testIamPermissions needs NO permission, as GetCallerIdentity does on
	// AWS: it is the call you make to find out what you may do, and a
	// setup where you cannot ask that until you already have access is one
	// nobody can debug. Found by a test that bound storage.objectViewer
	// and was then refused the very question.
	if strings.HasSuffix(permission, ".testIamPermissions") {
		return nil, ""
	}
	if permission == "" {
		return permissionDenied(p, r.Method+" "+r.URL.Path,
				fmt.Sprintf("Nimbus cannot yet work out which permission %s %s needs, "+
					"so with --enforce-iam it is refused rather than allowed", r.Method, r.URL.Path)),
			"denied: unmapped request " + r.Method + " " + r.URL.Path
	}

	d := authz.Evaluate(authz.Request{
		Principal: p,
		Action:    permission,
		Resource:  resource,
	}, s.rulesFor(p.ID))
	if d.Allowed {
		return nil, ""
	}
	return permissionDenied(p, permission, d.Reason),
		"denied: " + permission + " for " + p.ID + " -- " + d.Reason
}

// permissionDenied is Google's refusal: 403 with the PERMISSION_DENIED
// status. 403 and never 5xx -- a retried refusal is five lines in the feed
// instead of one, as AWS's errors.go already says.
func permissionDenied(p authz.Principal, permission, reason string) *apiError {
	return errorf(http.StatusForbidden, "forbidden", "PERMISSION_DENIED",
		"Permission '%s' denied on resource (or it may not exist). %s", permission, reason)
}

// heldPermissions filters a list down to the ones the caller actually has,
// which is what testIamPermissions is for. With enforcement off every
// permission is held, because nothing is refused.
// `resource` is what the question is about. It is not optional: a
// binding attached to one resource is scoped to it, so asking with an
// empty resource reports a member holding none of the permissions its
// own resource policy grants -- which is the opposite of what this call
// exists to prevent.
func (s *Server) heldPermissions(r *http.Request, resource string, asked []string) []string {
	if !s.cfg.EnforceIAM {
		return asked
	}
	p := s.resolvePrincipal(r)
	if p.Unrestricted {
		return asked
	}
	rules := s.rulesFor(p.ID)
	held := []string{}
	for _, permission := range asked {
		d := authz.Evaluate(authz.Request{
			Principal: p, Action: permission, Resource: resource,
		}, rules)
		if d.Allowed {
			held = append(held, permission)
		}
	}
	return held
}

// --- explaining ------------------------------------------------------------

// ExplainAccess answers what a member may do to one resource.
//
// The permissions are derived from the store's own type string, which
// carries the service: `compute.googleapis.com/Network` is
// `compute.networks.*`. Same derivation as the enforcement point's, so
// the two cannot disagree about what a permission is called.
func (s *Server) ExplainAccess(principal, resourceID, resourceType string) authz.Report {
	p := s.principalFor(principal)
	report := authz.Report{
		Principal: p.ID, Kind: p.Kind, Unrestricted: p.Unrestricted,
		Enforcing: s.cfg.EnforceIAM, Resource: resourceID,
	}
	prefix, collection := permissionPartsFor(resourceType)
	if prefix == "" {
		return report
	}
	actions := []string{
		prefix + "." + collection + ".get",
		prefix + "." + collection + ".create",
		prefix + "." + collection + ".delete",
		prefix + "." + collection + ".setIamPolicy",
	}
	// A bucket's CONTENTS are what people actually ask about, and they are
	// a different permission family from the bucket itself -- so a member
	// holding roles/storage.objectAdmin showed four denials on a bucket it
	// could write every object in. The same distinction S3 needs between
	// s3:ListBucket on the bucket and s3:GetObject on `bucket/*`.
	if collection == "buckets" {
		actions = append(actions,
			"storage.objects.list", "storage.objects.get",
			"storage.objects.create", "storage.objects.delete")
	}
	report.Checks = authz.Explain(p, resourceID, actions, s.rulesFor(p.ID))
	return report
}

// ExplainOne answers a single question, which is what `nimbus can` asks.
func (s *Server) ExplainOne(principal, action, resource string) authz.Check {
	p := s.principalFor(principal)
	return authz.Explain(p, resource, []string{action}, s.rulesFor(p.ID))[0]
}

func (s *Server) principalFor(principal string) authz.Principal {
	if principal == "" {
		principal = s.cfg.Principal
	}
	if !isMember(principal) {
		return authz.Principal{
			ID: "nimbus-default", Name: "the default identity",
			Kind: "serviceAccount", Unrestricted: true,
		}
	}
	kind, name, _ := strings.Cut(principal, ":")
	return authz.Principal{ID: principal, Name: name, Kind: kind}
}

// permissionPartsFor turns a store type into a permission prefix and
// collection: "compute.googleapis.com/Subnetwork" is compute and
// subnetworks.
func permissionPartsFor(resourceType string) (prefix, collection string) {
	host, kind, ok := strings.Cut(resourceType, "/")
	if !ok || kind == "" {
		return "", ""
	}
	prefix, _, _ = strings.Cut(host, ".")
	if prefix == "" {
		return "", ""
	}
	// Kind is singular and TitleCase; a collection is plural and
	// lowerCamelCase. `ServiceAccount` becomes `serviceAccounts`.
	collection = strings.ToLower(kind[:1]) + kind[1:] + "s"
	return prefix, collection
}
