package authz_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/authz"
)

// rule is a hand-built Rule, so the core's evaluation order can be tested
// without any cloud's policy syntax in the way. That it is this easy to
// write one is the point of the interface.
type rule struct {
	effect  authz.Effect
	action  string // matched with Glob against the request's action
	err     error  // when non-nil, this rule cannot decide
	applies bool   // forced, when action is empty
	source  string
}

func (r rule) Effect() authz.Effect { return r.effect }

func (r rule) Source() string {
	if r.source == "" {
		return "test rule"
	}
	return r.source
}

func (r rule) Applies(req authz.Request) (bool, error) {
	if r.action != "" && !authz.Glob(r.action, req.Action) {
		// Not about this action at all, so whatever else is wrong with
		// this rule is not this request's problem. A Rule must decide
		// action and resource BEFORE anything that can fail.
		return false, nil
	}
	if r.action == "" && !r.applies {
		return false, nil
	}
	if r.err != nil {
		return false, r.err
	}
	return true, nil
}

func req(action string) authz.Request {
	return authz.Request{
		Principal: authz.Principal{ID: "arn:aws:iam::000000000000:role/app", Kind: "role", Name: "app"},
		Action:    action,
		Resource:  "arn:aws:s3:::demo/thing",
	}
}

func TestNothingIsAllowedByDefault(t *testing.T) {
	d := authz.Evaluate(req("s3:GetObject"), nil)
	if d.Allowed {
		t.Fatalf("an empty policy set allowed s3:GetObject: %+v", d)
	}
	// The reason has to be legible on its own: it ends up in an
	// AccessDenied message a person reads in a terraform failure.
	if !strings.Contains(d.Reason, "s3:GetObject") {
		t.Errorf("the reason does not name the action: %q", d.Reason)
	}
}

func TestAnAllowThatMatchesAllows(t *testing.T) {
	rules := []authz.Rule{rule{effect: authz.Allow, action: "s3:*", source: "policy p/AllowS3"}}
	d := authz.Evaluate(req("s3:GetObject"), rules)
	if !d.Allowed {
		t.Fatalf("a matching allow did not allow: %+v", d)
	}
	if d.Source != "policy p/AllowS3" {
		t.Errorf("the decision does not say which rule decided: %q", d.Source)
	}
}

func TestAnAllowForSomethingElseDoesNotAllow(t *testing.T) {
	rules := []authz.Rule{rule{effect: authz.Allow, action: "s3:*"}}
	if d := authz.Evaluate(req("ec2:RunInstances"), rules); d.Allowed {
		t.Fatalf("an s3 allow allowed an ec2 action: %+v", d)
	}
}

// Explicit deny wins. This is the rule AWS is built on, and it has to hold
// whichever order the rules arrive in -- a policy set is a set.
func TestAnExplicitDenyBeatsAnAllow(t *testing.T) {
	allow := rule{effect: authz.Allow, action: "s3:*", source: "policy wide/AllowS3"}
	deny := rule{effect: authz.Deny, action: "s3:DeleteObject", source: "policy guard/DenyDelete"}

	for _, order := range [][]authz.Rule{{allow, deny}, {deny, allow}} {
		d := authz.Evaluate(req("s3:DeleteObject"), order)
		if d.Allowed {
			t.Fatalf("an explicit deny was overridden: %+v", d)
		}
		if d.Source != "policy guard/DenyDelete" {
			t.Errorf("the decision credits the wrong rule: %q", d.Source)
		}
	}
}

// A rule that cannot decide must never read as "no". It refuses, and the
// refusal names what could not be evaluated -- the FilterExpression lesson:
// an unevaluated condition that silently passes is the worst outcome, and
// one that silently fails is only slightly better.
func TestARuleThatCannotDecideRefuses(t *testing.T) {
	broken := errors.New(`the condition key "aws:MultiFactorAuthPresent" is not supported`)
	rules := []authz.Rule{
		rule{effect: authz.Allow, action: "s3:*", err: broken, source: "policy p/Conditional"},
	}
	d := authz.Evaluate(req("s3:GetObject"), rules)
	if d.Allowed {
		t.Fatalf("a rule that could not be evaluated allowed the request: %+v", d)
	}
	if !strings.Contains(d.Reason, "MultiFactorAuthPresent") {
		t.Errorf("the refusal does not say what it could not evaluate: %q", d.Reason)
	}
	if !d.Unevaluated {
		t.Error("the decision does not record that something was unevaluated")
	}
}

// An allow somewhere else must not paper over a rule that could not be
// evaluated: "cannot decide" is not "no".
func TestAnErrorIsNotRescuedByAnAllow(t *testing.T) {
	rules := []authz.Rule{
		rule{effect: authz.Allow, action: "s3:GetObject", source: "policy good/Allow"},
		rule{effect: authz.Allow, action: "s3:*", err: errors.New("unsupported operator Null"),
			source: "policy odd/Allow"},
	}
	if d := authz.Evaluate(req("s3:GetObject"), rules); d.Allowed {
		t.Fatalf("an unevaluated rule was papered over: %+v", d)
	}
}

// ...but an explicit deny still wins over it, because the answer is the
// same either way and the deny is the more specific thing to report.
func TestADenyBeatsAnError(t *testing.T) {
	rules := []authz.Rule{
		rule{effect: authz.Allow, action: "s3:*", err: errors.New("unsupported"), source: "odd"},
		rule{effect: authz.Deny, action: "s3:GetObject", source: "policy guard/DenyRead"},
	}
	d := authz.Evaluate(req("s3:GetObject"), rules)
	if d.Allowed {
		t.Fatal("denied request was allowed")
	}
	if d.Source != "policy guard/DenyRead" {
		t.Errorf("want the deny credited, got %q", d.Source)
	}
}

// An unrestricted principal skips evaluation entirely. That is handled here
// rather than in each cloud so that no cloud can forget it -- and it is
// what the AWS account root really is: no policies, full access.
func TestAnUnrestrictedPrincipalSkipsEvaluation(t *testing.T) {
	r := req("s3:DeleteObject")
	r.Principal.Unrestricted = true
	rules := []authz.Rule{rule{effect: authz.Deny, action: "*", source: "policy p/DenyAll"}}

	d := authz.Evaluate(r, rules)
	if !d.Allowed {
		t.Fatalf("an unrestricted principal was refused: %+v", d)
	}
	if !strings.Contains(d.Reason, "root") && !strings.Contains(d.Reason, "unrestricted") {
		t.Errorf("the reason does not explain why: %q", d.Reason)
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"s3:*", "s3:GetObject", true},
		{"s3:*", "ec2:GetObject", false},
		{"s3:Get*", "s3:GetObject", true},
		{"s3:Get*", "s3:PutObject", false},
		{"s3:GetObject", "s3:GetObject", true},
		{"s3:GetObject", "s3:GetObjectAcl", false},
		{"s3:?etObject", "s3:GetObject", true},
		{"s3:?etObject", "s3:etObject", false},
		{"arn:aws:s3:::demo/*", "arn:aws:s3:::demo/a/b/c", true},
		{"arn:aws:s3:::demo/*", "arn:aws:s3:::other/a", false},
		// A trailing * must match the empty string, which is how
		// "arn:aws:s3:::demo*" covers the bucket itself.
		{"arn:aws:s3:::demo*", "arn:aws:s3:::demo", true},
		// Several wildcards, the case a naive left-to-right matcher gets
		// wrong by consuming too much at the first one.
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxc", false},
		{"", "", true},
		{"", "x", false},
	}
	for _, c := range cases {
		if got := authz.Glob(c.pattern, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}
