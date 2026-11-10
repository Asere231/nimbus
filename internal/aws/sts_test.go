package aws

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "http://localhost:4702"}, store, bus)
	// No unit test may reach the real Docker daemon: a handler that
	// starts a container would leave it running after the package passed.
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

// query replays what aws-cli sends: a POST to "/" with the form body and a
// SigV4 header whose credential scope names the service. Captured from
// aws-cli 1.40.33 against a logging server.
func query(t *testing.T, s *Server, service, body string) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/"+service+"/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date, Signature=140c73865eeb6a69a400e8")
	req.Header.Set("X-Amz-Date", "20261106T004237Z")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestGetCallerIdentity(t *testing.T) {
	s := newTestServer(t)
	code, body := query(t, s, "sts", "Action=GetCallerIdentity&Version=2011-06-15")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}

	var got struct {
		XMLName xml.Name
		Result  struct {
			Arn     string `xml:"Arn"`
			UserID  string `xml:"UserId"`
			Account string `xml:"Account"`
		} `xml:"GetCallerIdentityResult"`
		RequestID string `xml:"ResponseMetadata>RequestId"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshalling: %v\n%s", err, body)
	}
	if got.XMLName.Local != "GetCallerIdentityResponse" {
		t.Errorf("root element = %q", got.XMLName.Local)
	}
	// The namespace is part of the contract: a client parsing with the
	// wrong one sees an empty document rather than an error.
	if got.XMLName.Space != "https://sts.amazonaws.com/doc/2011-06-15/" {
		t.Errorf("namespace = %q", got.XMLName.Space)
	}
	if got.Result.Account != DefaultAccountID {
		t.Errorf("account = %q, want %s", got.Result.Account, DefaultAccountID)
	}
	if got.Result.Arn != "arn:aws:iam::"+DefaultAccountID+":user/nimbus" {
		t.Errorf("arn = %q", got.Result.Arn)
	}
	if !strings.HasPrefix(got.Result.UserID, "AIDA") {
		t.Errorf("user id = %q, want an AIDA-prefixed id", got.Result.UserID)
	}
	if got.RequestID == "" {
		t.Error("every AWS response carries a request id")
	}
}

// Two calls must agree: a client that compares the identity it got before
// with the one it gets now should see no change.
func TestGetCallerIdentityIsStable(t *testing.T) {
	s := newTestServer(t)
	_, first := query(t, s, "sts", "Action=GetCallerIdentity&Version=2011-06-15")
	_, second := query(t, s, "sts", "Action=GetCallerIdentity&Version=2011-06-15")
	id := func(b []byte) string {
		var d struct {
			UserID string `xml:"GetCallerIdentityResult>UserId"`
		}
		_ = xml.Unmarshal(b, &d)
		return d.UserID
	}
	if id(first) != id(second) || id(first) == "" {
		t.Errorf("user id changed between calls: %q then %q", id(first), id(second))
	}
}

func TestAssumeRole(t *testing.T) {
	s := newTestServer(t)
	code, body := query(t, s, "sts",
		"Action=AssumeRole&Version=2011-06-15"+
			"&RoleArn=arn%3Aaws%3Aiam%3A%3A000000000000%3Arole%2Fapp"+
			"&RoleSessionName=demo&DurationSeconds=900")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var got struct {
		AccessKeyID  string `xml:"AssumeRoleResult>Credentials>AccessKeyId"`
		SecretKey    string `xml:"AssumeRoleResult>Credentials>SecretAccessKey"`
		SessionToken string `xml:"AssumeRoleResult>Credentials>SessionToken"`
		Expiration   string `xml:"AssumeRoleResult>Credentials>Expiration"`
		UserArn      string `xml:"AssumeRoleResult>AssumedRoleUser>Arn"`
	}
	if err := xml.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshalling: %v\n%s", err, body)
	}
	if !strings.HasPrefix(got.AccessKeyID, "ASIA") {
		t.Errorf("access key = %q, want the ASIA prefix a temporary key has", got.AccessKeyID)
	}
	if got.SecretKey == "" || got.SessionToken == "" || got.Expiration == "" {
		t.Errorf("incomplete credentials: %+v", got)
	}
	if got.UserArn != "arn:aws:sts::000000000000:assumed-role/app/demo" {
		t.Errorf("assumed role arn = %q", got.UserArn)
	}
}

func TestAssumeRoleNeedsARoleArn(t *testing.T) {
	s := newTestServer(t)
	code, body := query(t, s, "sts", "Action=AssumeRole&Version=2011-06-15")
	if code != 400 {
		t.Fatalf("status %d, want 400: %s", code, body)
	}
	if !strings.Contains(string(body), "ValidationError") {
		t.Errorf("body does not name the problem: %s", body)
	}
}

// The three error envelopes are different documents, not variants. A client
// handed the wrong one says "An error occurred (Unknown)", which is what
// aws-cli printed while this was being built.
func TestErrorEnvelopesDifferPerService(t *testing.T) {
	s := newTestServer(t)

	_, body := query(t, s, "sts", "Action=NoSuchThing&Version=2011-06-15")
	var q struct {
		XMLName xml.Name
		Type    string `xml:"Error>Type"`
		Code    string `xml:"Error>Code"`
	}
	if err := xml.Unmarshal(body, &q); err != nil {
		t.Fatalf("sts error: %v\n%s", err, body)
	}
	if q.XMLName.Local != "ErrorResponse" || q.Code != "InvalidAction" || q.Type != "Sender" {
		t.Errorf("sts error = %+v\n%s", q, body)
	}

	_, body = query(t, s, "ec2", "Action=RunInstances&Version=2016-11-15")
	var e struct {
		XMLName   xml.Name
		Code      string `xml:"Errors>Error>Code"`
		RequestID string `xml:"RequestID"`
	}
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("ec2 error: %v\n%s", err, body)
	}
	// EC2 wraps errors in a list and spells it RequestID, not RequestId.
	if e.XMLName.Local != "Response" || e.Code == "" || e.RequestID == "" {
		t.Errorf("ec2 error = %+v\n%s", e, body)
	}

	// A bucket nobody created, which is the simplest S3 error there is.
	req := httptest.NewRequest(http.MethodGet, "/no-such-bucket/key.txt", nil)
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/s3/aws4_request, Signature=x")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var s3 struct {
		XMLName xml.Name
		Code    string `xml:"Code"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &s3); err != nil {
		t.Fatalf("s3 error: %v\n%s", err, rec.Body)
	}
	if s3.XMLName.Local != "Error" || s3.Code == "" {
		t.Errorf("s3 error = %+v\n%s", s3, rec.Body)
	}
}

// Measured against aws-cli 1.40.33: a 5xx is retried five times with
// backoff. An action that will never work must not be one.
func TestUnimplementedActionsAreNotRetryable(t *testing.T) {
	s := newTestServer(t)
	for _, service := range []string{"sts", "iam", "ec2"} {
		code, body := query(t, s, service, "Action=SomethingNobodyBuilt&Version=1")
		if code != 400 {
			t.Errorf("%s: status %d, want 400 so the client does not retry: %s", service, code, body)
		}
	}
}

func TestAnUnknownServiceIsRefusedClearly(t *testing.T) {
	s := newTestServer(t)
	code, body := query(t, s, "kinesis", "Action=ListStreams&Version=1")
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
	if !strings.Contains(string(body), "kinesis") {
		t.Errorf("the error should name the service: %s", body)
	}
}

// The account is the root of the AWS graph, the way a subscription is the
// root of the Azure one.
func TestTheAccountExistsFromTheStart(t *testing.T) {
	s := newTestServer(t)
	res, err := s.store.Get("arn:aws:iam::" + DefaultAccountID + ":root")
	if err != nil {
		t.Fatalf("no account resource: %v", err)
	}
	if res.Cloud != "aws" || res.Type != "AWS::Account" {
		t.Errorf("account resource = %+v", res)
	}
	// Starting twice must not create it twice, or a restart doubles it.
	New(Config{}, s.store, s.bus)
	if n := len(s.store.List(func(r *core.Resource) bool { return r.Type == "AWS::Account" })); n != 1 {
		t.Errorf("%d accounts, want 1", n)
	}
}

// Every request lands in the activity feed, cloud and all, so one feed
// shows both clouds.
func TestRequestsArePublishedAsAwsEvents(t *testing.T) {
	s := newTestServer(t)
	before := len(s.bus.History())
	query(t, s, "sts", "Action=GetCallerIdentity&Version=2011-06-15")
	events := s.bus.History()[before:]
	if len(events) != 1 {
		t.Fatalf("published %d events, want 1", len(events))
	}
	e := events[0]
	if e.Kind != "request" || e.Cloud != "aws" || e.Status != 200 || e.Method != "POST" {
		t.Errorf("event = %+v", e)
	}
}

// An unimplemented action is flagged in the feed, which is how the next
// checkpoint knows what to build -- the same loop the Azure side uses.
func TestUnimplementedActionsAreNotedInTheFeed(t *testing.T) {
	s := newTestServer(t)
	before := len(s.bus.History())
	query(t, s, "ec2", "Action=RunInstances&Version=2016-11-15")
	events := s.bus.History()[before:]
	if len(events) != 1 || !strings.Contains(events[0].Note, "unimplemented") {
		t.Fatalf("event note = %q, want one mentioning unimplemented", events[0].Note)
	}
	if !strings.Contains(events[0].Note, "RunInstances") {
		t.Errorf("the note should name the action: %q", events[0].Note)
	}
}
