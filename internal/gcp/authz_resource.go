package gcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/authz"
	"github.com/Asere231/nimbus/internal/core"
)

// A binding attached to one resource rather than to the project.
//
// This was the smallest of the three clouds' halves of the milestone,
// because Milestone 14 built for it without using it: `bindingRule`
// already carried a `resource` and `compileBinding` already took one,
// both unused. What was missing was gathering them and the containment
// rule that lets a bucket's binding reach the bucket's objects.
//
// Where a resource's policy LIVES is one field on the resource itself,
// `x-nimbus-iam`, which Cloud Run has stored since Milestone 10. Not a
// separate collection: a policy that outlived its resource would be a
// grant nobody can see, and deleting the resource must take it with it.
const iamPolicyField = "x-nimbus-iam"

// storedResourcePolicy reads a resource's own policy, or nil.
func storedResourcePolicy(res *core.Resource) map[string]any {
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return nil
	}
	policy, _ := body[iamPolicyField].(map[string]any)
	return policy
}

// setResourcePolicy writes one, through core.Store.Update so that a
// read-change-write cannot lose a concurrent change to the rest of the
// body -- the lost-update rule this project already pays for elsewhere.
func (s *Server) setResourcePolicy(id string, policy map[string]any) error {
	if policy == nil {
		policy = map[string]any{}
	}
	policy["etag"] = etagOf(id)
	if _, ok := policy["version"]; !ok {
		policy["version"] = 1
	}
	return s.store.Update(id, func(body map[string]any) error {
		body[iamPolicyField] = policy
		return nil
	})
}

// getResourcePolicy reads one back, in the shape a client expects when a
// resource has never had a policy set.
func (s *Server) getResourcePolicy(id string) map[string]any {
	if res, err := s.store.Get(id); err == nil {
		if policy := storedResourcePolicy(res); policy != nil {
			return policy
		}
	}
	return map[string]any{"version": 1, "etag": etagOf(id)}
}

// policyFromRequest decodes a setIamPolicy body.
//
// Two shapes, because the APIs disagree: Pub/Sub, Cloud Run and
// resourcemanager wrap it as {"policy": {...}}, while Cloud Storage PUTs
// the Policy object itself to /b/{bucket}/iam. Accepting both costs one
// branch and means neither client has to be special-cased elsewhere.
func policyFromRequest(r *http.Request) (map[string]any, *apiError) {
	var req map[string]any
	if err := readJSON(r, &req); err != nil {
		return nil, badRequest("%v", err)
	}
	if wrapped, ok := req["policy"].(map[string]any); ok {
		return wrapped, nil
	}
	return req, nil
}

// resourceRulesFor is every resource-level binding that names a member.
//
// It walks the store rather than being told which resource the request
// is about, which is the same trade `rulesFor` and Azure's assignment
// lookup already make: reading the in-memory store per request is cheap,
// and a binding set a second ago takes effect at once. `bindingRule`
// then filters by resource, which it has always been able to do.
func (s *Server) resourceRulesFor(member string) []authz.Rule {
	var rules []authz.Rule
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Cloud == "gcp" }) {
		policy := storedResourcePolicy(res)
		if policy == nil {
			continue
		}
		bindings, _ := policy["bindings"].([]any)
		for i, raw := range bindings {
			b, _ := raw.(map[string]any)
			if b == nil || !bindingNames(b, member) {
				continue
			}
			rules = append(rules, compileBinding(
				fmt.Sprintf("the IAM policy on %s binding #%d (%s)", res.Name, i+1, str(b["role"])),
				str(b["role"]), res.Name))
		}
	}
	return rules
}

// coversResource is how a binding's scope is compared with a request's.
//
// SEGMENT-ALIGNED containment, not a bare prefix and not plain equality.
//
// Equality was what Milestone 14 shipped, and it is wrong for the case
// everybody means by "give this service account access to the bucket": a
// bucket binding has to reach the bucket's OBJECTS, which arrive as
// `reports/today.txt`. A bare prefix is wrong in the other direction --
// `reports` would then govern `reports-archive` -- which is exactly the
// trap Azure's scopeCovers records, so this is the same rule rather than
// a second one.
//
// Case-insensitive, because GCP resource names are compared that way and
// a binding written in a config may not match the casing of a path.
func coversResource(scope, resource string) bool {
	if scope == "" {
		return true // the project policy: everything
	}
	if strings.EqualFold(scope, resource) {
		return true
	}
	return len(resource) > len(scope) &&
		strings.EqualFold(scope, resource[:len(scope)]) &&
		resource[len(scope)] == '/'
}
