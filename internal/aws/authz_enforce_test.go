package aws

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// newEnforcingServer is newTestServer with authorization on.
func newEnforcingServer(t *testing.T, principal string) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL:  "http://localhost:4702",
		EnforceIAM: true, Principal: principal,
	}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

// asKey replays a request whose SigV4 credential scope carries a given
// access key id, which is how a caller claims an identity here.
func asKey(t *testing.T, s *Server, key, service, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if service != "s3" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	}
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+key+"/20261110/us-east-1/"+service+"/aws4_request, "+
			"SignedHeaders=host;x-amz-date, Signature=2c90a61f")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

// makeRole creates a role with one inline policy, through the IAM API, so
// the test exercises the same path Terraform does.
func makeRole(t *testing.T, s *Server, name, document string) {
	t.Helper()
	form := url.Values{"Action": {"CreateRole"}, "Version": {iamVersion}, "RoleName": {name},
		"AssumeRolePolicyDocument": {`{"Version":"2012-10-17","Statement":[]}`}}
	if code, body := query(t, s, "iam", form.Encode()); code != 200 {
		t.Fatalf("creating the role: %d %s", code, body)
	}
	form = url.Values{"Action": {"PutRolePolicy"}, "Version": {iamVersion}, "RoleName": {name},
		"PolicyName": {"inline"}, "PolicyDocument": {document}}
	if code, body := query(t, s, "iam", form.Encode()); code != 200 {
		t.Fatalf("attaching the policy: %d %s", code, body)
	}
}

func s3ErrorOf(t *testing.T, body []byte) s3Error {
	t.Helper()
	var doc s3Error
	if err := xml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the refusal is not an S3 error document: %v\n%s", err, body)
	}
	return doc
}

// The default caller is the account root, which is allowed everything even
// with enforcement on. That is what real root is -- no policies, full
// access -- and it is what keeps every existing example applying with the
// flag turned on.
func TestWithEnforcementOnAnUnmappedCallerIsRootAndAllowed(t *testing.T) {
	s := newEnforcingServer(t, "")

	if rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo", ""); rec.Code != 200 {
		t.Fatalf("creating a bucket as root = %d %s", rec.Code, rec.Body)
	}
	if rec := asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo/a.txt", "hello"); rec.Code != 200 {
		t.Errorf("writing an object as root = %d %s", rec.Code, rec.Body)
	}
}

func TestARoleIsRefusedWhatItsPolicyDoesNotGrant(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "reader", `{"Version":"2012-10-17","Statement":[
	  {"Sid":"ReadOnly","Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],
	   "Resource":["arn:aws:s3:::demo","arn:aws:s3:::demo/*"]}]}`)

	// Root sets the scene, because the role cannot.
	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo", "")
	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo/a.txt", "hello")

	// The role may read...
	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/demo/a.txt", ""); rec.Code != 200 {
		t.Errorf("a granted read = %d %s", rec.Code, rec.Body)
	}
	if rec := asKey(t, s, "reader", "s3", http.MethodGet, "/demo", ""); rec.Code != 200 {
		t.Errorf("a granted list = %d %s", rec.Code, rec.Body)
	}

	// ...and may not write. 403 and not 500: a 5xx is retried five times
	// with backoff, which turns one legible refusal into five.
	rec := asKey(t, s, "reader", "s3", http.MethodPut, "/demo/b.txt", "nope")
	if rec.Code != 403 {
		t.Fatalf("an ungranted write = %d, want 403\n%s", rec.Code, rec.Body)
	}
	doc := s3ErrorOf(t, rec.Body.Bytes())
	if doc.Code != "AccessDenied" {
		t.Errorf("refusal code = %q, want AccessDenied", doc.Code)
	}
	// The message has to name the action and the principal: it is what a
	// person reads out of a failed terraform apply.
	for _, want := range []string{"s3:PutObject", "role/reader"} {
		if !strings.Contains(doc.Message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, doc.Message)
		}
	}
}

// A role identified by the session key AssumeRole minted for it, which is
// how the aws provider's own assume_role {} block arrives.
func TestASessionKeyFromAssumeRoleMapsBackToItsRole(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "writer", `{"Statement":[
	  {"Effect":"Allow","Action":"s3:*","Resource":"*"}]}`)

	form := url.Values{"Action": {"AssumeRole"}, "Version": {stsVersion},
		"RoleArn": {s.iamARN("role", "/", "writer")}, "RoleSessionName": {"session"}}
	code, body := query(t, s, "sts", form.Encode())
	if code != 200 {
		t.Fatalf("AssumeRole = %d %s", code, body)
	}
	var assumed assumeRoleResponse
	if err := xml.Unmarshal(body, &assumed); err != nil {
		t.Fatal(err)
	}
	key := assumed.Result.Credentials.AccessKeyID
	if !strings.HasPrefix(key, "ASIA") {
		t.Fatalf("session key = %q, want an ASIA prefix", key)
	}

	// The key is enough: nothing was recorded when it was minted.
	if rec := asKey(t, s, key, "s3", http.MethodPut, "/mine", ""); rec.Code != 200 {
		t.Errorf("the session key did not map back to its role: %d %s", rec.Code, rec.Body)
	}

	// And GetCallerIdentity says who that is, which is how somebody
	// checks before finding out through a denial.
	r := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(url.Values{"Action": {"GetCallerIdentity"}, "Version": {stsVersion}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	r.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential="+key+"/20261110/us-east-1/sts/aws4_request, Signature=x")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	var who getCallerIdentityResponse
	if err := xml.Unmarshal(rec.Body.Bytes(), &who); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(who.Result.Arn, "role/writer") {
		t.Errorf("GetCallerIdentity says %q, want the assumed role", who.Result.Arn)
	}
}

// --principal naming something that does not exist allows nothing. Not
// root: silently granting everything to a typo is the one outcome that
// would make the flag worse than useless.
func TestAPrincipalThatDoesNotExistIsAllowedNothing(t *testing.T) {
	s := newEnforcingServer(t, "ghost")
	rec := asKey(t, s, "", "s3", http.MethodPut, "/demo", "")
	if rec.Code != 403 {
		t.Fatalf("an unknown --principal = %d, want 403\n%s", rec.Code, rec.Body)
	}
}

// With the flag OFF nothing is refused, which is the milestone's own
// done-criterion and the reason every existing example still applies.
func TestWithEnforcementOffNothingIsRefused(t *testing.T) {
	s := newTestServer(t)
	makeRole(t, s, "reader", `{"Statement":[
	  {"Effect":"Deny","Action":"s3:*","Resource":"*"}]}`)

	if rec := asKey(t, s, "reader", "s3", http.MethodPut, "/demo", ""); rec.Code != 200 {
		t.Errorf("a bucket create was refused with enforcement off: %d %s", rec.Code, rec.Body)
	}
}

// An explicit deny beats an allow through the whole stack, not just in the
// evaluator's unit tests.
func TestAnExplicitDenyIsEnforcedEndToEnd(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "app", `{"Statement":[
	  {"Sid":"Everything","Effect":"Allow","Action":"s3:*","Resource":"*"},
	  {"Sid":"ButNotSecrets","Effect":"Deny","Action":"s3:GetObject",
	   "Resource":"arn:aws:s3:::demo/secret/*"}]}`)

	asKey(t, s, "app", "s3", http.MethodPut, "/demo", "")
	asKey(t, s, "app", "s3", http.MethodPut, "/demo/secret/key.txt", "shh")
	asKey(t, s, "app", "s3", http.MethodPut, "/demo/public/ok.txt", "fine")

	if rec := asKey(t, s, "app", "s3", http.MethodGet, "/demo/public/ok.txt", ""); rec.Code != 200 {
		t.Errorf("an allowed read = %d", rec.Code)
	}
	rec := asKey(t, s, "app", "s3", http.MethodGet, "/demo/secret/key.txt", "")
	if rec.Code != 403 {
		t.Fatalf("a denied read = %d, want 403\n%s", rec.Code, rec.Body)
	}
	if doc := s3ErrorOf(t, rec.Body.Bytes()); !strings.Contains(doc.Message, "ButNotSecrets") {
		t.Errorf("the refusal does not name the statement that decided: %s", doc.Message)
	}
}

// Every denial reaches the activity feed. One that nobody can see is worse
// than no enforcement at all, because then an apply simply fails with
// nothing to look at.
func TestADenialIsOnTheBus(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "reader", `{"Statement":[
	  {"Sid":"ReadOnly","Effect":"Allow","Action":"s3:Get*","Resource":"*"}]}`)

	asKey(t, s, "reader", "s3", http.MethodPut, "/demo/a.txt", "no")

	var found *core.Event
	for _, e := range s.bus.RecentEvents(100) {
		if strings.HasPrefix(e.Note, "denied:") {
			found = &e
			break
		}
	}
	if found == nil {
		t.Fatal("no denial reached the bus")
	}
	// The action, the principal and the reason. There is no rule to name
	// here, because nothing matched: this is the implicit deny, and
	// "nothing grants it" is the accurate thing to report. The wording is
	// deliberately cloud-neutral -- the same sentence is appended to
	// Azure's AuthorizationFailed, where the thing that would have
	// granted it is a role assignment rather than a policy.
	for _, want := range []string{"s3:PutObject", "role/reader", "nothing grants"} {
		if !strings.Contains(found.Note, want) {
			t.Errorf("the feed note does not mention %q: %s", want, found.Note)
		}
	}
	if found.Status != 403 {
		t.Errorf("the denial was recorded with status %d, want 403", found.Status)
	}
}

// S3 is the only service whose body is not buffered, because its bodies
// are the large ones. Every other service's handler must still be able to
// read a body the authorization check has already looked at.
func TestBufferingTheBodyDoesNotBreakAQueryService(t *testing.T) {
	s := newEnforcingServer(t, "")
	form := url.Values{"Action": {"CreateRole"}, "Version": {iamVersion}, "RoleName": {"buffered"},
		"AssumeRolePolicyDocument": {`{"Statement":[]}`}}
	code, body := query(t, s, "iam", form.Encode())
	if code != 200 {
		t.Fatalf("CreateRole as root = %d %s", code, body)
	}
	if !strings.Contains(string(body), "buffered") {
		t.Errorf("the handler did not see the body: %s", body)
	}
}

func TestS3TargetNamesTheActionAndResource(t *testing.T) {
	s := newEnforcingServer(t, "")
	cases := []struct {
		method, path     string
		action, resource string
		mapped           bool
	}{
		{"GET", "/", "s3:ListAllMyBuckets", "*", true},
		{"PUT", "/demo", "s3:CreateBucket", "arn:aws:s3:::demo", true},
		{"DELETE", "/demo", "s3:DeleteBucket", "arn:aws:s3:::demo", true},
		{"GET", "/demo", "s3:ListBucket", "arn:aws:s3:::demo", true},
		{"HEAD", "/demo", "s3:ListBucket", "arn:aws:s3:::demo", true},
		{"GET", "/demo?versioning", "s3:GetBucketVersioning", "arn:aws:s3:::demo", true},
		{"PUT", "/demo?versioning", "s3:PutBucketVersioning", "arn:aws:s3:::demo", true},
		// The inconsistent ones: no "Bucket" in the action name.
		{"GET", "/demo?lifecycle", "s3:GetLifecycleConfiguration", "arn:aws:s3:::demo", true},
		{"GET", "/demo?encryption", "s3:GetEncryptionConfiguration", "arn:aws:s3:::demo", true},
		{"GET", "/demo?cors", "s3:GetBucketCORS", "arn:aws:s3:::demo", true},
		{"GET", "/demo?location", "s3:GetBucketLocation", "arn:aws:s3:::demo", true},
		{"GET", "/demo?versions", "s3:ListBucketVersions", "arn:aws:s3:::demo", true},
		{"GET", "/demo?uploads", "s3:ListBucketMultipartUploads", "arn:aws:s3:::demo", true},
		{"POST", "/demo?delete", "s3:DeleteObject", "arn:aws:s3:::demo/*", true},
		// Objects. A key may contain slashes and is not a path.
		{"PUT", "/demo/a/b.txt", "s3:PutObject", "arn:aws:s3:::demo/a/b.txt", true},
		{"GET", "/demo/a/b.txt", "s3:GetObject", "arn:aws:s3:::demo/a/b.txt", true},
		{"DELETE", "/demo/a.txt", "s3:DeleteObject", "arn:aws:s3:::demo/a.txt", true},
		// Multipart: uploading a part is s3:PutObject, because IAM has no
		// s3:UploadPart.
		{"POST", "/demo/big?uploads", "s3:PutObject", "arn:aws:s3:::demo/big", true},
		{"PUT", "/demo/big?partNumber=1&uploadId=x", "s3:PutObject", "arn:aws:s3:::demo/big", true},
		{"POST", "/demo/big?uploadId=x", "s3:PutObject", "arn:aws:s3:::demo/big", true},
		{"DELETE", "/demo/big?uploadId=x", "s3:AbortMultipartUpload", "arn:aws:s3:::demo/big", true},
		{"GET", "/demo/big?uploadId=x", "s3:ListMultipartUploadParts", "arn:aws:s3:::demo/big", true},
		// A query parameter neither subResource() nor the router knows is
		// ignored by BOTH, so this really is served as a bucket listing
		// and authorizing it as one is right. The classifier mirrors the
		// router; where they would disagree is where a request gets
		// authorized as one thing and served as another.
		{"GET", "/demo?invented", "s3:ListBucket", "arn:aws:s3:::demo", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		got, ok := s.s3Target(r)
		if ok != c.mapped {
			t.Errorf("%s %s: mapped = %v, want %v", c.method, c.path, ok, c.mapped)
			continue
		}
		if !ok {
			continue
		}
		if got.Action != c.action || got.Resource != c.resource {
			t.Errorf("%s %s = %s on %s, want %s on %s",
				c.method, c.path, got.Action, got.Resource, c.action, c.resource)
		}
	}
}

// A HEAD response carries no body, by HTTP's own rule, so the error
// document is discarded and a client reports only "Forbidden". Measured
// against aws provider 5.100.0: a denied HeadObject during a refresh says
// `api error Forbidden: Forbidden` and nothing about which permission was
// missing. Real S3 is no better, so the reason has to travel somewhere
// else -- the activity feed, and this header for anyone reading the
// response itself.
func TestADeniedHeadStillCarriesItsReason(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "blind", `{"Statement":[
	  {"Effect":"Allow","Action":"s3:ListBucket","Resource":"*"}]}`)
	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo", "")
	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo/a.txt", "hi")

	rec := asKey(t, s, "blind", "s3", http.MethodHead, "/demo/a.txt", "")
	if rec.Code != 403 {
		t.Fatalf("a denied HEAD = %d, want 403", rec.Code)
	}
	reason := rec.Header().Get("X-Nimbus-Denied")
	if !strings.Contains(reason, "s3:GetObject") {
		t.Errorf("the header does not carry the reason: %q", reason)
	}
}

// `nimbus can` must agree with what a request actually gets.
//
// The done-criterion the milestone asks for, and the only thing that stops
// the answer drifting from the behaviour: one table, driven BOTH ways --
// an actual request through ServeHTTP, and the question the CLI asks.
//
// The action and resource below are written by hand on purpose. They are
// what a person would type into `nimbus can`, so if the request
// classifier disagrees with that, the disagreement is the finding --
// deriving them from the classifier would only test it against itself.
func TestNimbusCanAgreesWithTheRequestPath(t *testing.T) {
	s := newEnforcingServer(t, "")
	makeRole(t, s, "reader", `{"Statement":[
	  {"Sid":"ReadOnly","Effect":"Allow","Action":["s3:GetObject","s3:ListBucket"],
	   "Resource":["arn:aws:s3:::demo","arn:aws:s3:::demo/*"]}]}`)

	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo", "")
	asKey(t, s, "nimbus", "s3", http.MethodPut, "/demo/a.txt", "hello")

	cases := []struct {
		method, path     string
		action, resource string
	}{
		{http.MethodGet, "/demo/a.txt", "s3:GetObject", "arn:aws:s3:::demo/a.txt"},
		{http.MethodPut, "/demo/b.txt", "s3:PutObject", "arn:aws:s3:::demo/b.txt"},
		{http.MethodDelete, "/demo/a.txt", "s3:DeleteObject", "arn:aws:s3:::demo/a.txt"},
		{http.MethodGet, "/demo", "s3:ListBucket", "arn:aws:s3:::demo"},
	}
	for _, c := range cases {
		rec := asKey(t, s, "reader", "s3", c.method, c.path, "x")
		requestAllowed := rec.Code != http.StatusForbidden

		check := s.ExplainOne("reader", c.action, c.resource)
		if check.Allowed != requestAllowed {
			t.Errorf("%s %s: the request says allowed=%v but `nimbus can %s %s` says %v (%s)",
				c.method, c.path, requestAllowed, c.action, c.resource, check.Allowed, check.Reason)
		}
	}
}
