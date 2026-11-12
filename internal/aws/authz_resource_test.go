package aws

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A policy attached to the RESOURCE rather than to the caller.
//
// Milestones 13 and 14 evaluated only what a caller holds. A bucket policy
// is the other half of AWS's model, and it is the half that grants public
// access -- so these tests are about two things that look similar and are
// not: a principal allowed because its OWN policy says so, and one allowed
// only because the bucket's policy says so.

// anonymous replays an UNSIGNED request, which is what `aws s3
// --no-sign-request` and a browser send. It carries no Authorization
// header at all, so there is no access key to resolve.
func anonymous(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// putBucketPolicy attaches a policy to a bucket through the S3 API, so the
// test drives the same path Terraform does.
func putBucketPolicy(t *testing.T, s *Server, bucket, document string) {
	t.Helper()
	rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/"+bucket+"?policy", document)
	if rec.Code != 200 && rec.Code != 204 {
		t.Fatalf("putting the bucket policy: %d %s", rec.Code, rec.Body)
	}
}

// seedBucket creates a bucket and one object as root, which is allowed
// everything. Named apart from s3_test.go's makeBucket because this one
// signs as root and leaves an object to read.
func seedBucket(t *testing.T, s *Server, name string) {
	t.Helper()
	if rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/"+name, ""); rec.Code != 200 {
		t.Fatalf("creating the bucket: %d %s", rec.Code, rec.Body)
	}
	if rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/"+name+"/a.txt", "hello"); rec.Code != 200 {
		t.Fatalf("writing the object: %d %s", rec.Code, rec.Body)
	}
}

// The milestone's headline: a principal whose own policies grant NOTHING
// can read an object, and the only reason is the bucket's policy. This is
// the thing identity-based evaluation could not express.
func TestABucketPolicyAloneCanGrantAccess(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	seedBucket(t, s, "closed")
	// A role with an inline policy that grants nothing relevant, so an
	// allow cannot come from the caller's side.
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"sts:GetCallerIdentity","Resource":"*"}]}`)

	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"LetTheReaderRead","Effect":"Allow",
		 "Principal":{"AWS":"arn:aws:iam::000000000000:role/reader"},
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Errorf("reading the open bucket = %d %s, want 200", rec.Code, rec.Body)
	}
	rec := asKey(t, s, "reader", "s3", http.MethodGet, "/closed/a.txt", "")
	if rec.Code != 403 {
		t.Fatalf("reading the closed bucket = %d %s, want 403", rec.Code, rec.Body)
	}
	if got := s3ErrorOf(t, rec.Body.Bytes()).Code; got != "AccessDenied" {
		t.Errorf("the refusal's code is %q, want AccessDenied", got)
	}
}

// The decision has to NAME the bucket policy. A denial explained without
// its source teaches nothing, and the whole reason /api/authz is backed by
// this evaluator is that the explanation cannot drift from the behaviour.
func TestTheDecisionNamesTheBucketPolicy(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"sts:GetCallerIdentity","Resource":"*"}]}`)
	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"LetTheReaderRead","Effect":"Allow",
		 "Principal":{"AWS":"arn:aws:iam::000000000000:role/reader"},
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	check := s.ExplainOne("reader", "s3:GetObject", "arn:aws:s3:::open/a.txt")
	if !check.Allowed {
		t.Fatalf("ExplainOne says no: %s", check.Reason)
	}
	if !strings.Contains(check.Rule, "bucket policy") {
		t.Errorf("the rule is %q, which does not say it is the bucket policy", check.Rule)
	}
	if !strings.Contains(check.Rule, "LetTheReaderRead") {
		t.Errorf("the rule is %q, which does not name the statement", check.Rule)
	}
}

// An explicit Deny wins wherever it is written. This is the rule that
// makes a bucket policy a security control rather than a convenience, and
// getting it backwards is silent.
func TestADenyInTheBucketPolicyBeatsAnAllowInTheCallersOwn(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`)

	// The caller's own policy allows everything, so without the bucket
	// policy this read succeeds -- which is worth asserting, or the test
	// below could pass for the wrong reason.
	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Fatalf("the caller's own policy should allow this: %d %s", rec.Code, rec.Body)
	}

	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"NoReadingHere","Effect":"Deny","Principal":"*",
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", "")
	if rec.Code != 403 {
		t.Fatalf("after the deny = %d %s, want 403", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); !strings.Contains(body, "NoReadingHere") {
		t.Errorf("the refusal does not name the statement that denied it:\n%s", body)
	}
}

// A statement in a resource policy applies only to the principals it
// names. Without the Principal element a bucket policy grants to everyone,
// which is the failure mode this test exists for.
func TestABucketPolicyGrantsOnlyThePrincipalItNames(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"sts:GetCallerIdentity","Resource":"*"}]}`)
	makeRole(t, s, "stranger", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"sts:GetCallerIdentity","Resource":"*"}]}`)

	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:role/reader"},
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Errorf("the named role = %d %s, want 200", rec.Code, rec.Body)
	}
	if rec := asKey(t, s, "stranger", "s3", http.MethodGet, "/open/a.txt", ""); rec.Code != 403 {
		t.Errorf("a role the policy does not name = %d, want 403", rec.Code)
	}
}

// A resource policy with no Principal is invalid in AWS, and the tempting
// reading -- "it applies to everybody" -- is the most dangerous one
// available. It refuses and names itself, like an unreadable managed
// policy and an unmapped action.
func TestAResourcePolicyWithNoPrincipalRefuses(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)

	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", "")
	if rec.Code != 403 {
		t.Fatalf("a bucket policy with no Principal = %d, want 403", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Principal") {
		t.Errorf("the refusal does not say what is wrong with the policy:\n%s", body)
	}
}

// An identity policy may NOT carry a Principal -- AWS rejects one. Nimbus
// refuses the document rather than ignoring the element, because ignoring
// it would read as "this applies to everyone", which is what the author
// did not mean.
func TestAnIdentityPolicyWithAPrincipalRefuses(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"*"}]}`)

	rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", "")
	if rec.Code != 403 {
		t.Fatalf("an identity policy carrying a Principal = %d, want 403", rec.Code)
	}
}

// --- anonymous, and public buckets ---------------------------------------

// An unsigned request is NOT the account root. It used to be: resolveCaller
// fell through to rootCaller, which is unrestricted -- so with enforcement
// on, anyone who sent no credentials at all could do anything. Nothing
// about a public bucket can be demonstrated until this is its own
// principal.
func TestAnUnsignedRequestIsAnonymousRatherThanRoot(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")

	if rec := anonymous(t, s, http.MethodGet, "/open/a.txt", ""); rec.Code != 403 {
		t.Errorf("an unsigned read of a private bucket = %d, want 403", rec.Code)
	}
	if rec := anonymous(t, s, http.MethodPut, "/open/b.txt", "hi"); rec.Code != 403 {
		t.Errorf("an unsigned write = %d, want 403", rec.Code)
	}
}

// `Principal: "*"` is how a bucket is made public, and how it is
// ACCIDENTALLY made public, which is the single most instructive thing in
// cloud permissions.
func TestAPublicBucketPolicyLetsAnAnonymousCallerRead(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"PublicRead","Effect":"Allow","Principal":"*",
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	rec := anonymous(t, s, http.MethodGet, "/open/a.txt", "")
	if rec.Code != 200 {
		t.Fatalf("an anonymous read of a public bucket = %d %s, want 200", rec.Code, rec.Body)
	}
	if got := rec.Body.String(); got != "hello" {
		t.Errorf("the object read back as %q", got)
	}
	// Public for reading is not public for writing: the statement names
	// one action.
	if rec := anonymous(t, s, http.MethodPut, "/open/b.txt", "hi"); rec.Code != 403 {
		t.Errorf("an anonymous write to a read-public bucket = %d, want 403", rec.Code)
	}
}

// The public access block is the override, and it is the case a learner
// most needs: the policy still says `*` and the bucket is not public. A
// badge that ignored this would be wrong exactly where it matters.
func TestAPublicAccessBlockOverridesAPublicPolicy(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"PublicRead","Effect":"Allow","Principal":"*",
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)
	if rec := anonymous(t, s, http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Fatalf("the bucket is not public to begin with: %d", rec.Code)
	}

	rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/open?publicAccessBlock",
		`<PublicAccessBlockConfiguration><RestrictPublicBuckets>true`+
			`</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	if rec.Code != 200 && rec.Code != 204 {
		t.Fatalf("setting the public access block: %d %s", rec.Code, rec.Body)
	}

	if rec := anonymous(t, s, http.MethodGet, "/open/a.txt", ""); rec.Code != 403 {
		t.Errorf("an anonymous read with the block on = %d, want 403", rec.Code)
	}
	// A NAMED principal the policy grants is unaffected: the block
	// restricts public access, not the policy entirely.
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"*"}]}`)
	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Errorf("a named principal with the block on = %d %s, want 200", rec.Code, rec.Body)
	}
}

// Enforcement OFF must change nothing. The anonymous principal is the one
// part of this milestone that can leak into the default path.
func TestWithEnforcementOffAnUnsignedRequestStillWorks(t *testing.T) {
	s := newTestServer(t)
	seedBucket(t, s, "open")

	if rec := anonymous(t, s, http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Errorf("an unsigned read with enforcement off = %d %s, want 200", rec.Code, rec.Body)
	}
}

// --- the derived "public" flag the visualizer badges --------------------

// bucketBody reads the stored bucket, which is what the visualizer sees.
func bucketBody(t *testing.T, s *Server, name string) *bucket {
	t.Helper()
	b, aerr := s.loadBucket(name)
	if aerr != nil {
		t.Fatalf("loading %s: %v", name, aerr.Message)
	}
	return b
}

// The badge is derived by the EVALUATOR, in saveBucket, so it cannot
// disagree with what an anonymous request actually gets. These assert the
// flag and the request together, because agreeing is the whole point.
func TestAPublicBucketIsFlaggedAsPublic(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")

	if b := bucketBody(t, s, "open"); b.Public {
		t.Fatal("a bucket with no policy is flagged public")
	}

	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"PublicRead","Effect":"Allow","Principal":"*",
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	b := bucketBody(t, s, "open")
	if !b.Public {
		t.Error("a bucket whose policy grants * is not flagged public")
	}
	if !strings.Contains(b.PublicReason, "PublicRead") {
		t.Errorf("the reason is %q, which does not name the statement", b.PublicReason)
	}
	if rec := anonymous(t, s, http.MethodGet, "/open/a.txt", ""); rec.Code != 200 {
		t.Errorf("the flag says public but the request got %d", rec.Code)
	}
}

// The case a learner most needs: the policy still says `*` and the bucket
// is not public. A badge that read the JSON instead of asking the
// evaluator would be wrong exactly here.
func TestAPublicPolicyBehindAnAccessBlockIsNotFlaggedPublic(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Sid":"PublicRead","Effect":"Allow","Principal":"*",
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::open/*"}]}`)

	rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/open?publicAccessBlock",
		`<PublicAccessBlockConfiguration><RestrictPublicBuckets>true`+
			`</RestrictPublicBuckets></PublicAccessBlockConfiguration>`)
	if rec.Code != 200 && rec.Code != 204 {
		t.Fatalf("setting the public access block: %d %s", rec.Code, rec.Body)
	}

	b := bucketBody(t, s, "open")
	if b.Public {
		t.Errorf("a bucket behind a public access block is flagged public (%s)", b.PublicReason)
	}
	// The policy itself is untouched: it still says `*`, which is the
	// gap the badge exists to close rather than to hide.
	if !strings.Contains(b.Policy, `"Principal":"*"`) {
		t.Error("the policy was altered; it must come back byte for byte")
	}
}

// A bucket granted to a NAMED principal is not public. The two are easy
// to conflate, and conflating them is how a badge cries wolf.
func TestABucketGrantedToANamedPrincipalIsNotPublic(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "shared")
	putBucketPolicy(t, s, "shared", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"arn:aws:iam::000000000000:role/reader"},
		 "Action":"s3:GetObject","Resource":"arn:aws:s3:::shared/*"}]}`)

	if b := bucketBody(t, s, "shared"); b.Public {
		t.Errorf("a bucket shared with one role is flagged public (%s)", b.PublicReason)
	}
}

// Deleting the policy un-flags it. The flag is derived on every write, so
// there is no stale state to invalidate -- but a flag that only ever went
// one way would be worse than none.
func TestRemovingThePolicyRemovesTheFlag(t *testing.T) {
	s := newEnforcingServer(t, "")
	seedBucket(t, s, "open")
	putBucketPolicy(t, s, "open", `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject",
		 "Resource":"arn:aws:s3:::open/*"}]}`)
	if !bucketBody(t, s, "open").Public {
		t.Fatal("not public to begin with")
	}

	if rec := asKey(t, s, "nimbus", "s3", http.MethodDelete, "/open?policy", ""); rec.Code >= 300 {
		t.Fatalf("deleting the policy: %d %s", rec.Code, rec.Body)
	}
	if b := bucketBody(t, s, "open"); b.Public {
		t.Errorf("still flagged public after the policy was deleted (%s)", b.PublicReason)
	}
}
