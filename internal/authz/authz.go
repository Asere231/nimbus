// Package authz decides whether a request is allowed, and knows about no
// cloud's policy language.
//
// Every cloud Nimbus emulates records permissions and, until this package
// existed, enforced none of them: IAM policies, Azure role assignments and
// GCP bindings all round-tripped so Terraform could manage them, and no
// request was ever refused. That makes a local apply unable to prove a
// deployment's permissions are right, which is a large part of what this
// project exists to teach.
//
// The division of labour is deliberate and narrow. This package holds the
// shape of a decision -- a principal, an action, a resource, a context, and
// the order in which rules settle it. Each cloud compiles ITS OWN documents
// down to the Rule interface: the words `Sid`, `NotAction`, `roleDefinition`
// and `bindings` appear nowhere here, and if one ever does, it belongs in
// the plugin instead.
//
// What IS shared is the evaluation order, because all three clouds agree on
// it: an explicit deny beats everything, nothing is allowed unless something
// says so, and glob matching with `*` and `?`.
package authz

import (
	"fmt"
	"strings"
)

// Effect is what a rule says when it applies.
type Effect int

const (
	Allow Effect = iota
	Deny
)

func (e Effect) String() string {
	if e == Deny {
		return "Deny"
	}
	return "Allow"
}

// Principal is who is asking. Kind and Name are for the explanation; ID is
// the identity itself (an ARN on AWS, an object id on Azure, an email on
// GCP) and is what a policy's own condition keys compare against.
type Principal struct {
	ID   string
	Name string
	Kind string // "root", "role", "user", "serviceAccount", ...

	// Unrestricted skips policy evaluation altogether. The AWS account
	// root is exactly this: it carries no policies and can do everything,
	// so evaluating an empty rule set against it would deny the world.
	// It lives here, rather than in each cloud, so that no cloud can
	// forget to honour it.
	Unrestricted bool
}

// Request is one thing somebody is trying to do.
type Request struct {
	Principal Principal
	// Action is the cloud's own name for the operation, e.g.
	// "s3:GetObject". Matching is the cloud's business; the core only
	// passes it to the rules.
	Action string
	// Resource is what is being acted on, or "*" when the request names
	// nothing in particular (listing a whole account, for instance).
	Resource string
	// Context carries condition keys the rules may consult, e.g.
	// "aws:RequestedRegion". A key that is absent is not the empty
	// string: a rule must tell the two apart, which is why this is a map
	// and not a struct.
	Context map[string]string
}

// Rule is one statement, compiled by whichever cloud owns it.
//
// Applies must settle the action and the resource BEFORE anything that can
// fail. A rule about `s3:*` has no business refusing an `ec2:RunInstances`
// request because of a condition key it cannot evaluate, and a caller
// relies on that: an error here refuses the whole request.
type Rule interface {
	// Applies reports whether this rule speaks to the request. An error
	// means it CANNOT tell -- an unsupported condition operator, say --
	// which must never be read as "no".
	Applies(Request) (bool, error)
	Effect() Effect
	// Source names this rule well enough for someone to find it: which
	// policy, and which statement within it.
	Source() string
}

// Decision is the answer, and enough of the reasoning to explain itself.
// The explanation is not decoration: it goes into the AccessDenied message
// a person reads in a failed apply, into the activity feed, and (in the
// milestone after this one) into the inspector.
type Decision struct {
	Allowed bool
	// Source is the rule that decided, empty when nothing did and the
	// implicit deny applied.
	Source string
	Reason string
	// Unevaluated records that some rule could not be evaluated. The
	// request is refused either way, but this distinguishes "your policy
	// does not grant this" from "Nimbus cannot tell whether it does",
	// which are very different things to be told.
	Unevaluated bool
}

// Evaluate settles a request against a set of rules.
//
// The order is AWS's, and Azure and GCP agree with it:
//
//  1. an unrestricted principal is allowed, without looking at anything;
//  2. an explicit Deny that applies wins, however many Allows there are;
//  3. a rule that could not be evaluated refuses -- "cannot tell" is not
//     "no", and must not be papered over by an Allow elsewhere;
//  4. an Allow that applies allows;
//  5. otherwise the implicit deny: nothing said yes.
//
// A rule set is a SET, so the answer cannot depend on the order the rules
// arrive in. Every rule is therefore consulted rather than stopping at the
// first match.
func Evaluate(req Request, rules []Rule) Decision {
	if req.Principal.Unrestricted {
		return Decision{
			Allowed: true,
			Reason: fmt.Sprintf("%s is unrestricted, so no policy is evaluated",
				describe(req.Principal)),
		}
	}

	var deny *Decision
	var problem *Decision
	var allow *Decision

	for _, rule := range rules {
		applies, err := rule.Applies(req)
		switch {
		case err != nil:
			if problem == nil {
				problem = &Decision{
					Source:      rule.Source(),
					Unevaluated: true,
					Reason: fmt.Sprintf("%s cannot be evaluated, so %s on %s is refused: %v",
						rule.Source(), req.Action, req.Resource, err),
				}
			}
		case !applies:
			continue
		case rule.Effect() == Deny:
			if deny == nil {
				deny = &Decision{
					Source: rule.Source(),
					Reason: fmt.Sprintf("%s explicitly denies %s on %s",
						rule.Source(), req.Action, req.Resource),
				}
			}
		default:
			if allow == nil {
				allow = &Decision{
					Allowed: true,
					Source:  rule.Source(),
					Reason: fmt.Sprintf("%s allows %s on %s",
						rule.Source(), req.Action, req.Resource),
				}
			}
		}
	}

	switch {
	case deny != nil:
		return *deny
	case problem != nil:
		return *problem
	case allow != nil:
		return *allow
	}
	return Decision{
		// Deliberately neutral wording: "no policy" is AWS's vocabulary,
		// and this sentence is appended to Azure's AuthorizationFailed and
		// Google's PERMISSION_DENIED as well, where the thing that would
		// have granted it is a role assignment or a binding.
		Reason: fmt.Sprintf("nothing grants %s on %s to %s",
			req.Action, req.Resource, describe(req.Principal)),
	}
}

func describe(p Principal) string {
	switch {
	case p.ID != "" && p.Kind != "":
		return p.Kind + " " + p.ID
	case p.ID != "":
		return p.ID
	case p.Name != "":
		return p.Name
	}
	return "the caller"
}

// Glob matches a pattern containing `*` (any run of characters, including
// none) and `?` (exactly one character) against a string, case-sensitively.
//
// It lives in the core because all three clouds match this way, and it is
// written iteratively rather than recursively because the obvious
// left-to-right version gets `a*b*c` wrong: the first `*` consumes too much
// and nothing backtracks. The `star`/`mark` pair below is the backtrack.
func Glob(pattern, s string) bool {
	// The overwhelmingly common cases, worth not looping for.
	if pattern == "*" {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}

	var i, j int // positions in s and pattern
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case j < len(pattern) && (pattern[j] == '?' || pattern[j] == s[i]):
			i++
			j++
		case j < len(pattern) && pattern[j] == '*':
			// Remember where the star was and that it has so far matched
			// nothing, then try to match the rest.
			star, mark = j, i
			j++
		case star >= 0:
			// The attempt failed: let the last star swallow one more
			// character and try again from just after it.
			mark++
			i, j = mark, star+1
		default:
			return false
		}
	}
	// Any stars left over match the empty string.
	for j < len(pattern) && pattern[j] == '*' {
		j++
	}
	return j == len(pattern)
}

// --- explaining, rather than only deciding --------------------------------

// Report answers "what may this principal do to this resource".
//
// A refusal in the activity feed is enough to debug one call; it is not
// enough to learn a permission model, which is what this project exists
// for. So the same engine that refuses a request also answers the
// question in advance, for the visualizer's inspector and for `nimbus
// can` -- and because both read it from one endpoint backed by one
// evaluator, the explanation cannot drift from the behaviour.
//
// Cloud-neutral on purpose: each cloud decides which actions are worth
// asking about and what its rules are called, and the shape of the answer
// is the same for all three.
type Report struct {
	// Principal is who the question was asked about, and Kind is what
	// that cloud calls such a thing: a role, a servicePrincipal, a
	// serviceAccount.
	Principal string `json:"principal"`
	Kind      string `json:"kind,omitempty"`
	// Unrestricted says the principal bypasses policy altogether, which
	// is worth showing rather than listing a column of allows.
	Unrestricted bool `json:"unrestricted,omitempty"`
	// Enforcing is false when --enforce-iam is off. Then every answer
	// below is what WOULD happen, which is useful and must not be
	// mistaken for what is happening.
	Enforcing bool    `json:"enforcing"`
	Resource  string  `json:"resource,omitempty"`
	Checks    []Check `json:"checks"`
}

// Check is one action's answer.
type Check struct {
	Action  string `json:"action"`
	Allowed bool   `json:"allowed"`
	// Rule is the Source() of whatever decided, empty when nothing did
	// and the implicit deny applied.
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason"`
	// Unevaluated marks "Nimbus cannot tell", which is a different thing
	// from "no".
	Unevaluated bool `json:"unevaluated,omitempty"`
}

// Explain evaluates several actions against one rule set, which is what a
// report is made of.
func Explain(p Principal, resource string, actions []string, rules []Rule) []Check {
	out := make([]Check, 0, len(actions))
	for _, action := range actions {
		d := Evaluate(Request{Principal: p, Action: action, Resource: resource}, rules)
		out = append(out, Check{
			Action: action, Allowed: d.Allowed, Rule: d.Source,
			Reason: d.Reason, Unevaluated: d.Unevaluated,
		})
	}
	return out
}
