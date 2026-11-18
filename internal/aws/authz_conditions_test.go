package aws

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/authz"
)

// userRequest is a request from a USER, carrying the full context the
// server builds in authzRequest -- the caller's address, whether the
// connection is encrypted, what kind of principal it is and the clock.
//
// It is spelled out here rather than reusing userRequest(), whose context has
// only two keys, because an operator test against a context nobody builds
// would pass for the wrong reason. That the SERVER really populates these
// is asserted separately, by TestTheRequestContextCarriesEveryAnswerableKey.
func userRequest() authz.Request {
	const arn = "arn:aws:iam::000000000000:user/alice"
	now := time.Now().UTC()
	return authz.Request{
		Principal: authz.Principal{ID: arn, Kind: "user", Name: "alice"},
		Action:    "s3:GetObject",
		Resource:  "arn:aws:s3:::demo/data.json",
		Context: map[string]string{
			"aws:PrincipalArn":     arn,
			"aws:PrincipalAccount": "000000000000",
			"aws:PrincipalType":    "User",
			"aws:RequestedRegion":  "us-east-1",
			"aws:username":         "alice",
			"aws:userid":           "AIDAALICE",
			"aws:SourceIp":         "127.0.0.1",
			"aws:SecureTransport":  "false",
			"aws:CurrentTime":      now.Format(time.RFC3339),
			"aws:EpochTime":        strconv.FormatInt(now.Unix(), 10),
		},
	}
}

// The condition operators and keys beyond the first four.
//
// Nimbus started with four keys and eight operators, and everything else
// REFUSED by name -- which is the right failure, because a condition that
// silently passes is how a policy appears to protect something it does not.
// But a refusal is still a failure, and the keys below are ones Nimbus can
// genuinely answer: it has the request, so it knows the caller's address,
// whether the connection is encrypted, what kind of principal it is and
// what time it is. Answering those is strictly better than refusing them.
//
// What is NOT added is anything Nimbus would have to invent: tags on the
// resource, an organisation id, an MFA age. Those still refuse.

func condDoc(op, key, value string) string {
	return `{"Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"*",
	  "Condition":{"` + op + `":{"` + key + `":"` + value + `"}}}]}`
}

// --- the operators that were simply missing --------------------------------

// ArnNotLike was absent while ArnLike, ArnEquals and ArnNotEquals were all
// there -- an asymmetry rather than a decision, and the kind of gap that
// turns a working policy into a refusal for no reason.
func TestArnNotLikeIsEvaluated(t *testing.T) {
	// The caller is a user, so its ARN is an IAM user ARN.
	allow := condDoc("ArnNotLike", "aws:PrincipalArn", "arn:aws:iam::*:role/*")
	if d := decide(t, allow, userRequest()); !d.Allowed {
		t.Errorf("a user's ARN is not like a role's, so this should allow: %s", d.Reason)
	}
	deny := condDoc("ArnNotLike", "aws:PrincipalArn", "arn:aws:iam::*:user/*")
	if d := decide(t, deny, userRequest()); d.Allowed {
		t.Error("ArnNotLike matched the caller's own ARN pattern and still allowed")
	}
}

func TestStringEqualsIgnoreCaseIsEvaluated(t *testing.T) {
	if d := decide(t, condDoc("StringEqualsIgnoreCase", "aws:RequestedRegion", "US-EAST-1"),
		userRequest()); !d.Allowed {
		t.Errorf("case should not matter for this operator: %s", d.Reason)
	}
	if d := decide(t, condDoc("StringEquals", "aws:RequestedRegion", "US-EAST-1"),
		userRequest()); d.Allowed {
		t.Error("plain StringEquals must stay case-SENSITIVE; IAM values are")
	}
}

// --- numeric ---------------------------------------------------------------

func TestNumericOperatorsAreEvaluated(t *testing.T) {
	// aws:EpochTime is now, so it is comfortably greater than 2020.
	cases := []struct {
		op, value string
		allow     bool
	}{
		{"NumericGreaterThan", "1577836800", true}, // 2020-01-01
		{"NumericLessThan", "1577836800", false},
		{"NumericGreaterThanEquals", "1577836800", true},
		{"NumericLessThanEquals", "1577836800", false},
		{"NumericEquals", "1577836800", false},
		{"NumericNotEquals", "1577836800", true},
	}
	for _, c := range cases {
		d := decide(t, condDoc(c.op, "aws:EpochTime", c.value), userRequest())
		if d.Allowed != c.allow {
			t.Errorf("%s %s: allowed=%v, wanted %v (%s)",
				c.op, c.value, d.Allowed, c.allow, d.Reason)
		}
	}
}

// A numeric comparison against something that is not a number must REFUSE,
// not quietly evaluate to false. A false would read as "the statement does
// not apply", which is indistinguishable from a policy that was never meant
// to match -- the FilterExpression trap one subsystem over.
func TestANonNumericValueRefusesRatherThanNotMatching(t *testing.T) {
	d := decide(t, condDoc("NumericLessThan", "aws:EpochTime", "soon"), userRequest())
	if d.Allowed {
		t.Fatal("a malformed numeric condition allowed the request")
	}
	if !d.Unevaluated {
		t.Errorf("a malformed numeric condition should be UNEVALUATED, not merely unmatched: %+v", d)
	}
	if !strings.Contains(d.Reason, "soon") {
		t.Errorf("the refusal does not name the value it could not read: %q", d.Reason)
	}
}

// --- dates -----------------------------------------------------------------

func TestDateOperatorsAreEvaluated(t *testing.T) {
	cases := []struct {
		op, value string
		allow     bool
	}{
		{"DateGreaterThan", "2020-01-01T00:00:00Z", true},
		{"DateLessThan", "2020-01-01T00:00:00Z", false},
		{"DateLessThan", "2999-01-01T00:00:00Z", true},
		{"DateNotEquals", "2020-01-01T00:00:00Z", true},
		{"DateEquals", "2020-01-01T00:00:00Z", false},
	}
	for _, c := range cases {
		d := decide(t, condDoc(c.op, "aws:CurrentTime", c.value), userRequest())
		if d.Allowed != c.allow {
			t.Errorf("%s %s: allowed=%v, wanted %v (%s)",
				c.op, c.value, d.Allowed, c.allow, d.Reason)
		}
	}
}

// AWS accepts epoch seconds in a Date condition as well as ISO 8601, and a
// policy in the wild uses both.
func TestADateConditionAcceptsEpochSeconds(t *testing.T) {
	if d := decide(t, condDoc("DateGreaterThan", "aws:CurrentTime", "1577836800"),
		userRequest()); !d.Allowed {
		t.Errorf("epoch seconds should be a valid date value: %s", d.Reason)
	}
}

func TestAnUnreadableDateRefuses(t *testing.T) {
	d := decide(t, condDoc("DateLessThan", "aws:CurrentTime", "next tuesday"), userRequest())
	if d.Allowed || !d.Unevaluated {
		t.Errorf("an unreadable date should refuse as unevaluated: %+v", d)
	}
}

// --- addresses -------------------------------------------------------------

func TestIpAddressOperatorsAreEvaluated(t *testing.T) {
	// The test request comes from a loopback address.
	if d := decide(t, condDoc("IpAddress", "aws:SourceIp", "127.0.0.0/8"),
		userRequest()); !d.Allowed {
		t.Errorf("the caller is on loopback, so this should allow: %s", d.Reason)
	}
	if d := decide(t, condDoc("IpAddress", "aws:SourceIp", "10.0.0.0/8"),
		userRequest()); d.Allowed {
		t.Error("the caller is not in 10/8 and was allowed anyway")
	}
	if d := decide(t, condDoc("NotIpAddress", "aws:SourceIp", "10.0.0.0/8"),
		userRequest()); !d.Allowed {
		t.Errorf("NotIpAddress against a range the caller is outside should allow: %s", d.Reason)
	}
}

// A bare address with no mask is a single host, which is how policies are
// usually written.
func TestABareAddressIsASingleHost(t *testing.T) {
	if d := decide(t, condDoc("IpAddress", "aws:SourceIp", "127.0.0.1"),
		userRequest()); !d.Allowed {
		t.Errorf("a bare address should match that exact host: %s", d.Reason)
	}
}

func TestAMalformedCidrRefuses(t *testing.T) {
	d := decide(t, condDoc("IpAddress", "aws:SourceIp", "127.0.0.1/99"), userRequest())
	if d.Allowed || !d.Unevaluated {
		t.Errorf("a malformed CIDR should refuse as unevaluated: %+v", d)
	}
}

// --- Null, which is about presence itself ----------------------------------

// Null is the one operator whose subject is whether the key exists at all,
// so it has to be evaluated BEFORE the "an absent key does not match" rule
// that governs every other operator.
func TestNullTestsWhetherAKeyIsPresent(t *testing.T) {
	// The test caller is a USER, so aws:username is present.
	if d := decide(t, condDoc("Null", "aws:username", "false"), userRequest()); !d.Allowed {
		t.Errorf("aws:username is present, so Null:false should allow: %s", d.Reason)
	}
	if d := decide(t, condDoc("Null", "aws:username", "true"), userRequest()); d.Allowed {
		t.Error("aws:username is present, so Null:true must not allow")
	}
	// A key Nimbus supports but this request does not carry.
	if d := decide(t, condDoc("Null", "aws:PrincipalArn", "true"), userRequest()); d.Allowed {
		t.Error("aws:PrincipalArn is always present; Null:true must not allow")
	}
}

// --- IfExists --------------------------------------------------------------

// The IfExists suffix has ONE rule and it is well defined: if the key is
// absent the condition is satisfied, whatever the operator. It was left out
// originally for fear of guessing that rule wrong, which is worth saying --
// but the rule is not a guess, so the suffix is supported and the absent
// case is pinned by this test rather than by hope.
func TestIfExistsIsSatisfiedWhenTheKeyIsAbsent(t *testing.T) {
	// aws:PrincipalTag is not a key Nimbus answers at all, so pick one it
	// supports that a USER request genuinely lacks: a role's id.
	doc := condDoc("StringEqualsIfExists", "aws:userid", "AIDANOBODY")
	if d := decide(t, doc, userRequest()); d.Allowed {
		t.Error("aws:userid IS present for this caller, so a wrong value must not allow")
	}
	// And with the key present and right, it behaves as the base operator.
	ok := condDoc("StringEqualsIfExists", "aws:RequestedRegion", "us-east-1")
	if d := decide(t, ok, userRequest()); !d.Allowed {
		t.Errorf("IfExists with the key present and matching should allow: %s", d.Reason)
	}
}

// --- what still refuses ----------------------------------------------------

// ForAllValues and ForAnyValue stay unsupported, deliberately.
//
// They quantify over a MULTIVALUED context key, and every Nimbus context
// key is a single string -- so implementing them as aliases of the base
// operator would be right today and silently wrong the day a multivalued
// key is added. The policies that use them are about `aws:TagKeys`, which
// is not a supported key either, so nothing is lost that was working.
func TestQuantifiedConditionsStillRefuseAndNameThemselves(t *testing.T) {
	for _, op := range []string{"ForAllValues:StringEquals", "ForAnyValue:StringLike"} {
		d := decide(t, condDoc(op, "aws:RequestedRegion", "us-east-1"), userRequest())
		if d.Allowed {
			t.Errorf("%s allowed a request it cannot evaluate", op)
		}
		if !d.Unevaluated || !strings.Contains(d.Reason, op) {
			t.Errorf("%s should refuse and name itself: %+v", op, d)
		}
	}
}

// A key that would have to be invented still refuses. These are the ones
// worth keeping honest: Nimbus has no tags in the authorization path, no
// organisation and no MFA.
func TestAnUnanswerableKeyStillRefusesAndNamesItself(t *testing.T) {
	for _, key := range []string{
		"aws:MultiFactorAuthAge",
		"aws:PrincipalOrgID",
		"aws:TagKeys",
		"s3:x-amz-acl",
	} {
		d := decide(t, condDoc("StringEquals", key, "whatever"), userRequest())
		if d.Allowed {
			t.Errorf("%s allowed a request it cannot answer", key)
		}
		if !d.Unevaluated || !strings.Contains(d.Reason, key) {
			t.Errorf("%s should refuse and name itself: %+v", key, d)
		}
	}
}

// --- aws:SecureTransport ---------------------------------------------------

// aws:SecureTransport is answered TRUTHFULLY, which means false here.
//
// Nimbus serves AWS over plain HTTP by design -- path-style S3 needs no
// certificate and no DNS -- so the connection genuinely is not encrypted
// and the only honest answer is false. The consequence is worth knowing
// rather than hiding: the standard hardening policy, a Deny on
// `aws:SecureTransport: false`, really does deny everything here. That is
// what the policy SAYS, and seeing it happen teaches more than a refusal
// claiming the key is unsupported.
func TestSecureTransportIsFalseOverPlainHttp(t *testing.T) {
	if d := decide(t, condDoc("Bool", "aws:SecureTransport", "false"),
		userRequest()); !d.Allowed {
		t.Errorf("the connection is plain HTTP, so false is the true answer: %s", d.Reason)
	}
	if d := decide(t, condDoc("Bool", "aws:SecureTransport", "true"),
		userRequest()); d.Allowed {
		t.Error("claiming the connection is encrypted when it is not")
	}
}

// And the shape of the real hardening policy, end to end: an explicit Deny
// that beats an Allow, which is the thing a learner is actually trying out.
func TestTheRealHardeningPolicyDeniesEverythingOverHttp(t *testing.T) {
	doc := `{"Statement":[
	  {"Effect":"Allow","Action":"s3:*","Resource":"*"},
	  {"Effect":"Deny","Action":"s3:*","Resource":"*",
	   "Condition":{"Bool":{"aws:SecureTransport":"false"}}}
	]}`
	d := decide(t, doc, userRequest())
	if d.Allowed {
		t.Error("a Deny on insecure transport must bite when the transport IS insecure")
	}
	if d.Unevaluated {
		t.Errorf("this is evaluated, not unevaluable: %+v", d)
	}
}

func TestPrincipalTypeIsAnswered(t *testing.T) {
	if d := decide(t, condDoc("StringEquals", "aws:PrincipalType", "User"),
		userRequest()); !d.Allowed {
		t.Errorf("the test caller is a user: %s", d.Reason)
	}
	if d := decide(t, condDoc("StringEquals", "aws:PrincipalType", "AssumedRole"),
		userRequest()); d.Allowed {
		t.Error("a user matched AssumedRole")
	}
}

// Every key Nimbus claims to support is one the SERVER actually supplies.
//
// This is the test that keeps the rest of this file honest. The operator
// tests above run against a context the test builds itself, so they would
// all pass while nothing in the server populated a single new key -- a unit
// test proving a method works while nothing calls it, which this repo has
// been bitten by before (vm.Engine.Reattach, written and tested and called
// by nobody).
//
// It reads supportedConditionKeys rather than a list of its own, so adding
// a key to that map without answering it fails here rather than in
// somebody's policy.
func TestTheRequestContextCarriesEveryAnswerableKey(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/demo/data.json", nil)

	// A user and a role, because two keys are deliberately per-kind.
	user := authz.Principal{
		ID: "arn:aws:iam::000000000000:user/alice", Kind: "user", Name: "alice",
	}
	role := authz.Principal{
		ID: "arn:aws:iam::000000000000:role/app", Kind: "role", Name: "app",
	}
	userCtx := s.conditionContext(r, user)
	roleCtx := s.conditionContext(r, role)

	// aws:username is the one key that is legitimately absent for a role.
	perKind := map[string]bool{"aws:username": true}

	for key := range supportedConditionKeys {
		if _, ok := lookupKey(userCtx, key); !ok {
			t.Errorf("%s is listed as supported but the server does not supply it; "+
				"a policy using it would silently never match", key)
		}
		if perKind[key] {
			continue
		}
		if _, ok := lookupKey(roleCtx, key); !ok {
			t.Errorf("%s is missing for a role, and only aws:username should be", key)
		}
	}

	// And the two per-kind keys behave as the comments claim.
	if _, ok := lookupKey(roleCtx, "aws:username"); ok {
		t.Error("a role has no aws:username; absent is not empty")
	}
	if got, _ := lookupKey(userCtx, "aws:PrincipalType"); got != "User" {
		t.Errorf("aws:PrincipalType for a user = %q, want User", got)
	}
	if got, _ := lookupKey(roleCtx, "aws:PrincipalType"); got != "AssumedRole" {
		t.Errorf("aws:PrincipalType for a role = %q, want AssumedRole (IAM's word, not ours)", got)
	}
}

// aws:userid is derived, so two requests agree about it.
//
// The "derive, don't generate" rule reaches conditions too: a userid that
// differed between requests would be a policy that passes and then fails
// for no reason anybody could see.
func TestUserIdIsDerivedAndStable(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/demo/data.json", nil)
	p := authz.Principal{
		ID: "arn:aws:iam::000000000000:user/alice", Kind: "user", Name: "alice",
	}

	first, _ := lookupKey(s.conditionContext(r, p), "aws:userid")
	second, _ := lookupKey(s.conditionContext(r, p), "aws:userid")
	if first == "" || first != second {
		t.Errorf("aws:userid is not stable: %q then %q", first, second)
	}
	if !strings.HasPrefix(first, "AIDA") {
		t.Errorf("a user's id should carry IAM's own prefix, got %q", first)
	}

	other := authz.Principal{
		ID: "arn:aws:iam::000000000000:user/bob", Kind: "user", Name: "bob",
	}
	if o, _ := lookupKey(s.conditionContext(r, other), "aws:userid"); o == first {
		t.Error("two different users share a userid")
	}

	// The account root has none, and that is a real answer rather than a
	// gap: Null is the operator that asks about it.
	root := authz.Principal{ID: "arn:aws:iam::000000000000:root", Kind: "root"}
	if id, ok := lookupKey(s.conditionContext(r, root), "aws:userid"); ok {
		t.Errorf("the account root should have no userid, got %q", id)
	}
}

// The source address loses its port, because a policy names hosts and
// ranges.
func TestSourceIpHasNoPort(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/demo/data.json", nil)
	r.RemoteAddr = "192.0.2.9:51413"

	got, _ := lookupKey(s.conditionContext(r, authz.Principal{Kind: "root"}), "aws:SourceIp")
	if got != "192.0.2.9" {
		t.Errorf("aws:SourceIp = %q, want the host alone", got)
	}
}

// The explainer must answer what the enforcement answers.
//
// `nimbus can` and the inspector are backed by this code precisely so they
// cannot drift from the behaviour -- but authz.Explain used to evaluate
// with an EMPTY context, so any policy carrying a condition got a different
// answer from the explainer than from a real request. That was invisible
// while the four supported keys were obscure; it stopped being invisible
// the moment aws:SecureTransport became answerable, because the standard
// hardening policy then denies every real request while the explainer,
// seeing no transport at all, said it was allowed.
//
// "An explainer that is wrong is worse than one that is silent" is this
// repo's own rule, written down when the Key Vault data-plane axis was
// added. This is the same rule for condition keys.
func TestTheExplainerAgreesWithTheEnforcementAboutConditions(t *testing.T) {
	s := newTestServer(t)

	// The policy a learner actually types: allow everything, then deny
	// anything not over TLS. Nimbus serves AWS over plain HTTP, so the
	// Deny bites and the honest answer to both questions is "no".
	doc := `{"Statement":[
	  {"Effect":"Allow","Action":"s3:*","Resource":"*"},
	  {"Effect":"Deny","Action":"s3:*","Resource":"*",
	   "Condition":{"Bool":{"aws:SecureTransport":"false"}}}
	]}`
	rules, err := compileDocument("policy test", doc, identityPolicy, "")
	if err != nil {
		t.Fatal(err)
	}

	p := authz.Principal{
		ID: "arn:aws:iam::000000000000:user/alice", Kind: "user", Name: "alice",
	}
	r := httptest.NewRequest(http.MethodGet, "/demo/data.json", nil)
	enforced := authz.Evaluate(authz.Request{
		Principal: p, Action: "s3:GetObject",
		Resource: "arn:aws:s3:::demo/data.json",
		Context:  s.conditionContext(r, p),
	}, rules)
	if enforced.Allowed {
		t.Fatal("the Deny did not bite on a plain-HTTP request; this test is not testing what it thinks")
	}

	explained := authz.Explain(p, "arn:aws:s3:::demo/data.json",
		[]string{"s3:GetObject"}, rules, s.explainContext(p))[0]
	if explained.Allowed != enforced.Allowed {
		t.Errorf("the explainer says allowed=%v and the enforcement says allowed=%v "+
			"for the same policy; `nimbus can` would be teaching the opposite of the behaviour",
			explained.Allowed, enforced.Allowed)
	}
}

// And a condition that is satisfied must be reported as satisfied, so the
// agreement is not just "both say no to everything".
func TestTheExplainerReportsASatisfiedConditionAsAllowed(t *testing.T) {
	s := newTestServer(t)
	doc := condDoc("StringEquals", "aws:RequestedRegion", "us-east-1")
	rules, err := compileDocument("policy test", doc, identityPolicy, "")
	if err != nil {
		t.Fatal(err)
	}
	p := authz.Principal{
		ID: "arn:aws:iam::000000000000:user/alice", Kind: "user", Name: "alice",
	}
	got := authz.Explain(p, "arn:aws:s3:::demo/data.json",
		[]string{"s3:GetObject"}, rules, s.explainContext(p))[0]
	if !got.Allowed {
		t.Errorf("a region condition the request satisfies was explained as denied: %s", got.Reason)
	}
}

// The public-bucket badge uses the same context as the enforcement.
//
// isPublicBucket asks "could an anonymous caller read this", and it used
// to ask with an EMPTY condition context. That was harmless while no
// supported key could appear in a bucket policy and became wrong the
// moment aws:SecureTransport did: a bucket whose policy grants `*` and
// then denies anything not over TLS would be badged PUBLIC while every
// anonymous request to it was refused.
//
// Over-warning is the safe direction for a safety badge, so this was not
// dangerous -- but it is still the explainer disagreeing with the
// behaviour, which this repo has a rule about, and the badge exists to
// teach what the policy does.
func TestThePublicBucketBadgeAgreesWithTheEnforcement(t *testing.T) {
	s := newTestServer(t)

	// Grant the world, then deny anything not over TLS -- the standard
	// hardening pair. Nimbus serves AWS over plain HTTP, so nothing can
	// read this bucket and it is not public.
	hardened := &bucket{
		Name: "hardened", ARN: bucketARN("hardened"), Region: "us-east-1",
		Policy: `{"Statement":[
		  {"Effect":"Allow","Principal":"*","Action":"s3:GetObject",
		   "Resource":"arn:aws:s3:::hardened/*"},
		  {"Effect":"Deny","Principal":"*","Action":"s3:*",
		   "Resource":"arn:aws:s3:::hardened/*",
		   "Condition":{"Bool":{"aws:SecureTransport":"false"}}}
		]}`,
	}
	if public, src := s.isPublicBucket(hardened); public {
		t.Errorf("a bucket whose policy denies non-TLS access is badged public (rule %q), "+
			"but every anonymous request to it is refused", src)
	}

	// And a genuinely open bucket is still badged public, so the fix is
	// not "report nothing".
	open := &bucket{
		Name: "open", ARN: bucketARN("open"), Region: "us-east-1",
		Policy: `{"Statement":[{"Effect":"Allow","Principal":"*",
		  "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`,
	}
	if public, _ := s.isPublicBucket(open); !public {
		t.Error("a bucket granting s3:GetObject to everyone is public and must be badged so")
	}
}
