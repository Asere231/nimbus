package aws

import (
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/authz"
)

// decide compiles a document and evaluates one request against it, which is
// how every test here reads: a real IAM document in, an answer out.
func decide(t *testing.T, document string, req authz.Request) authz.Decision {
	t.Helper()
	rules, err := compileDocument("policy test", document, identityPolicy, "")
	if err != nil {
		t.Fatalf("compiling the document: %v", err)
	}
	return authz.Evaluate(req, rules)
}

func getObject() authz.Request {
	return authz.Request{
		Principal: authz.Principal{
			ID: "arn:aws:iam::000000000000:role/app", Kind: "role", Name: "app",
		},
		Action:   "s3:GetObject",
		Resource: "arn:aws:s3:::demo/data.json",
		Context: map[string]string{
			"aws:PrincipalArn":    "arn:aws:iam::000000000000:role/app",
			"aws:RequestedRegion": "us-east-1",
		},
	}
}

func TestASingleStatementDocumentIsAnObjectOrAnArray(t *testing.T) {
	// The Statement key may be one object or a list of them, and
	// Action/Resource may each be a string or a list. Real documents use
	// every combination -- `aws_iam_policy_document` emits a list of
	// objects with list values, while a hand-written example usually does
	// not -- so all four shapes have to decode.
	docs := []string{
		`{"Version":"2012-10-17","Statement":{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::demo/*"}}`,
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::demo/*"]}]}`,
	}
	for _, doc := range docs {
		if d := decide(t, doc, getObject()); !d.Allowed {
			t.Errorf("not allowed by %s: %s", doc, d.Reason)
		}
	}
}

func TestAnActionWildcardMatchesCaseInsensitively(t *testing.T) {
	// IAM action matching ignores case; resource matching does not.
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:getobject","Resource":"*"}]}`
	if d := decide(t, doc, getObject()); !d.Allowed {
		t.Errorf("s3:getobject did not match s3:GetObject: %s", d.Reason)
	}
}

func TestAResourceThatDoesNotMatchIsNotAllowed(t *testing.T) {
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::other/*"}]}`
	d := decide(t, doc, getObject())
	if d.Allowed {
		t.Fatal("a statement about another bucket allowed this one")
	}
	if !strings.Contains(d.Reason, "arn:aws:s3:::demo/data.json") {
		t.Errorf("the reason does not name the resource: %q", d.Reason)
	}
}

func TestAnExplicitDenyInTheSameDocumentWins(t *testing.T) {
	doc := `{"Statement":[
	  {"Sid":"AllowAll","Effect":"Allow","Action":"s3:*","Resource":"*"},
	  {"Sid":"ProtectSecrets","Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::demo/*"}
	]}`
	d := decide(t, doc, getObject())
	if d.Allowed {
		t.Fatal("an explicit deny was overridden")
	}
	// The Sid is how somebody finds the statement in their own config, so
	// it has to survive into the explanation.
	if !strings.Contains(d.Source, "ProtectSecrets") {
		t.Errorf("the decision does not name the statement: %q", d.Source)
	}
}

func TestNotActionAndNotResource(t *testing.T) {
	// "everything except deleting" allows a read...
	doc := `{"Statement":[{"Effect":"Allow","NotAction":"s3:DeleteObject","Resource":"*"}]}`
	if d := decide(t, doc, getObject()); !d.Allowed {
		t.Errorf("NotAction excluded the wrong action: %s", d.Reason)
	}
	// ...and refuses the one named.
	req := getObject()
	req.Action = "s3:DeleteObject"
	if d := decide(t, doc, req); d.Allowed {
		t.Error("NotAction allowed the action it names")
	}

	doc = `{"Statement":[{"Effect":"Allow","Action":"s3:*","NotResource":"arn:aws:s3:::demo/*"}]}`
	if d := decide(t, doc, getObject()); d.Allowed {
		t.Error("NotResource allowed the resource it names")
	}
}

func TestASupportedConditionIsEvaluated(t *testing.T) {
	doc := `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*",
	  "Condition":{"StringEquals":{"aws:RequestedRegion":"us-east-1"}}}]}`
	if d := decide(t, doc, getObject()); !d.Allowed {
		t.Errorf("a satisfied condition blocked the request: %s", d.Reason)
	}

	req := getObject()
	req.Context["aws:RequestedRegion"] = "eu-west-1"
	d := decide(t, doc, req)
	if d.Allowed {
		t.Error("an unsatisfied condition allowed the request")
	}
	if d.Unevaluated {
		t.Error("a condition that WAS evaluated is reported as unevaluated")
	}
}

// The FilterExpression lesson, one service over: a condition Nimbus cannot
// evaluate must refuse and say so, never silently pass.
func TestAnUnsupportedConditionRefusesAndNamesItself(t *testing.T) {
	cases := []struct{ doc, want string }{
		{`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*",
		   "Condition":{"NumericLessThan":{"aws:MultiFactorAuthAge":"300"}}}]}`, "NumericLessThan"},
		{`{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*",
		   "Condition":{"StringEquals":{"s3:x-amz-acl":"private"}}}]}`, "s3:x-amz-acl"},
	}
	for _, c := range cases {
		d := decide(t, c.doc, getObject())
		if d.Allowed {
			t.Errorf("an unsupported condition allowed the request: %s", c.doc)
		}
		if !d.Unevaluated {
			t.Errorf("the decision does not say it could not be evaluated: %+v", d)
		}
		if !strings.Contains(d.Reason, c.want) {
			t.Errorf("the refusal does not name %q: %q", c.want, d.Reason)
		}
	}
}

// A statement that cannot be evaluated must only affect the requests it is
// actually about. Otherwise one unsupported condition anywhere in a
// principal's policies refuses everything that principal does.
func TestAnUnsupportedConditionOnlyAffectsItsOwnAction(t *testing.T) {
	doc := `{"Statement":[
	  {"Effect":"Allow","Action":"s3:GetObject","Resource":"*"},
	  {"Effect":"Allow","Action":"ec2:RunInstances","Resource":"*",
	   "Condition":{"DateGreaterThan":{"aws:CurrentTime":"2020-01-01T00:00:00Z"}}}
	]}`
	if d := decide(t, doc, getObject()); !d.Allowed {
		t.Errorf("an unrelated unsupported condition refused a granted action: %s", d.Reason)
	}
}

func TestAMalformedDocumentIsRefusedAtCompileTime(t *testing.T) {
	bad := []string{
		`not json at all`,
		// Neither Action nor NotAction: AWS rejects this outright, and
		// guessing what it meant is how a policy ends up granting more
		// than it says.
		`{"Statement":[{"Effect":"Allow","Resource":"*"}]}`,
		// No Resource either way, in an identity-based policy.
		`{"Statement":[{"Effect":"Allow","Action":"s3:*"}]}`,
		// Both Action and NotAction.
		`{"Statement":[{"Effect":"Allow","Action":"s3:*","NotAction":"s3:Delete*","Resource":"*"}]}`,
		`{"Statement":[{"Effect":"Maybe","Action":"s3:*","Resource":"*"}]}`,
	}
	for _, doc := range bad {
		if _, err := compileDocument("policy test", doc, identityPolicy, ""); err == nil {
			t.Errorf("a malformed document compiled without error: %s", doc)
		}
	}
}

// A document that arrives URL-encoded still compiles. IAM returns documents
// encoded and a client that hands one straight back would otherwise store
// a policy that silently grants nothing.
func TestAURLEncodedDocumentIsDecodedFirst(t *testing.T) {
	doc := `%7B%22Statement%22%3A%5B%7B%22Effect%22%3A%22Allow%22%2C%22Action%22%3A%22s3%3A%2A%22%2C%22Resource%22%3A%22%2A%22%7D%5D%7D`
	if d := decide(t, doc, getObject()); !d.Allowed {
		t.Errorf("a URL-encoded document did not take effect: %s", d.Reason)
	}
}

// --- managed policies ------------------------------------------------------

func TestAKnownManagedPolicyIsEvaluated(t *testing.T) {
	rules, err := managedPolicyRules("arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess")
	if err != nil {
		t.Fatalf("a managed policy Nimbus ships failed to compile: %v", err)
	}
	if d := authz.Evaluate(getObject(), rules); !d.Allowed {
		t.Errorf("AmazonS3ReadOnlyAccess did not allow s3:GetObject: %s", d.Reason)
	}
	req := getObject()
	req.Action = "s3:PutObject"
	if d := authz.Evaluate(req, rules); d.Allowed {
		t.Error("a read-only policy allowed a write")
	}
}

// An unknown managed policy refuses, naming itself. Not "grants nothing"
// (which looks like a policy that does not cover the action) and not
// "grants everything" (which is worse): the decision has to say that
// Nimbus does not know what that ARN means.
func TestAnUnknownManagedPolicyRefusesAndNamesItself(t *testing.T) {
	const arn = "arn:aws:iam::aws:policy/AmazonInvented_FullAccess"
	rules, err := managedPolicyRules(arn)
	if err == nil {
		t.Fatal("an unknown managed policy compiled without error")
	}
	// It must still produce a rule, so that evaluation refuses with the
	// explanation rather than the request failing as a server error.
	d := authz.Evaluate(getObject(), rules)
	if d.Allowed {
		t.Fatal("an unknown managed policy allowed the request")
	}
	if !d.Unevaluated || !strings.Contains(d.Reason, arn) {
		t.Errorf("the refusal does not name the policy: %+v", d)
	}
}

func TestEveryBundledManagedPolicyCompiles(t *testing.T) {
	if len(awsManagedPolicies) == 0 {
		t.Fatal("no managed policies are bundled")
	}
	for arn := range awsManagedPolicies {
		if _, err := managedPolicyRules(arn); err != nil {
			t.Errorf("%s does not compile: %v", arn, err)
		}
	}
}

// --- the cache -------------------------------------------------------------

func TestTheSameDocumentsCompileOnce(t *testing.T) {
	c := newPolicyCache()
	docs := []policyDocument{{Source: "policy p", Document: `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`}}

	first := c.rules(docs)
	second := c.rules(docs)
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("want one rule each time, got %d and %d", len(first), len(second))
	}
	if first[0] != second[0] {
		t.Error("the same documents compiled to different rules; the cache is not being used")
	}
	// A different document set must not collide with the first.
	other := c.rules([]policyDocument{{Source: "policy p", Document: `{"Statement":[{"Effect":"Deny","Action":"s3:*","Resource":"*"}]}`}})
	if other[0] == first[0] {
		t.Error("two different document sets share a cache entry")
	}
}
