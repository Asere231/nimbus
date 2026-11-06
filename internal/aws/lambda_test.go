package aws

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/funcs"
)

// Every path and body here was captured from aws-cli 1.40.33: the version
// segment is part of the route, and tags live under a different one.

func newLambdaTestServer(t *testing.T) (*Server, *docker.Fake) {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		PublicURL: "http://localhost:4702", DataDir: t.TempDir(),
		// The readiness probe needs a real port, so a create's background
		// start is expected to give up quickly rather than take two
		// minutes over it.
		ClusterTimeout: 50 * time.Millisecond,
	}, store, bus)
	f := docker.NewFake()
	for _, image := range runtimeImages {
		f.Images[image] = true
	}
	s.UseDocker(f)
	return s, f
}

// lambdaCall replays a REST-JSON request with the lambda credential scope.
func lambdaCall(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/lambda/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date, Signature=140c73865eeb")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	}
	return rec, doc
}

func pyZip(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("app.py")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("def handler(event, context):\n    return {\"ok\": True}\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func createWriter(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec, doc := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions", `{
		"FunctionName": "writer", "Runtime": "python3.12",
		"Role": "arn:aws:iam::000000000000:role/lambda", "Handler": "app.handler",
		"Timeout": 10, "MemorySize": 256,
		"Environment": {"Variables": {"TABLE": "items"}},
		"Tags": {"env": "dev"},
		"Code": {"ZipFile": "`+pyZip(t)+`"}}`)
	// 201, not 200: measured.
	if rec.Code != 201 {
		t.Fatalf("create: status %d: %s", rec.Code, rec.Body)
	}
	waitSettled(t, s, "writer")
	return doc
}

// waitSettled blocks until the background start has finished, one way or
// the other. Without it a test's temporary directory is removed while the
// goroutine is still unpacking into it -- which is the same race a client
// would hit by deleting a function mid-create, and the reason the state
// machine has a Pending at all.
func waitSettled(t *testing.T, s *Server, name string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		fn, aerr := s.loadFunction(name)
		if aerr != nil || fn.State != "Pending" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never left Pending", name)
}

func TestCreateFunctionAnswersPendingWithEveryFieldTheProviderReads(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	doc := createWriter(t, s)

	if doc["State"] != "Pending" {
		t.Errorf("State = %v; a container takes seconds to start, so the create cannot claim Active",
			doc["State"])
	}
	want := "arn:aws:lambda:us-east-1:000000000000:function:writer"
	if doc["FunctionArn"] != want {
		t.Errorf("FunctionArn = %v, want %v", doc["FunctionArn"], want)
	}
	for _, f := range []string{"FunctionName", "Runtime", "Role", "Handler", "CodeSize",
		"CodeSha256", "Version", "Timeout", "MemorySize", "LastModified", "RevisionId",
		"PackageType", "Architectures", "TracingConfig", "EphemeralStorage"} {
		if doc[f] == nil {
			t.Errorf("the create response is missing %q", f)
		}
	}
	if doc["Version"] != "$LATEST" {
		t.Errorf("Version = %v", doc["Version"])
	}
}

// CodeSha256 is the digest of the code, not a fresh value: the provider
// compares it between reads to decide whether the code changed, so a value
// that differed every read would be a diff that never settles.
func TestCodeSha256IsDerivedFromTheCode(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	created := createWriter(t, s)
	_, first := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/configuration", "")
	_, second := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/configuration", "")
	if first["CodeSha256"] != created["CodeSha256"] || first["CodeSha256"] != second["CodeSha256"] {
		t.Errorf("CodeSha256 drifted: %v / %v / %v",
			created["CodeSha256"], first["CodeSha256"], second["CodeSha256"])
	}
	// And the same code anywhere gives the same digest.
	if sha256Base64([]byte("x")) != sha256Base64([]byte("x")) {
		t.Error("sha256Base64 is not a function of its input")
	}
}

func TestARuntimeWithNoImageIsRefusedWithTheListOfOnesThatWork(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	rec, doc := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions", `{
		"FunctionName": "old", "Runtime": "python2.7", "Handler": "app.handler",
		"Code": {"ZipFile": "`+pyZip(t)+`"}}`)
	if rec.Code != 400 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(str(doc["message"]), "python3.12") {
		t.Errorf("the refusal should name the runtimes that do work: %v", doc["message"])
	}
}

// The handler reaches Nimbus through AWS_ENDPOINT_URL, which every current
// SDK reads -- and it must name the HOST, because inside the container
// localhost is the container.
func TestTheHandlerEnvironmentPointsBackAtNimbusFromInsideDocker(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	fn, aerr := s.loadFunction("writer")
	if aerr != nil {
		t.Fatal(aerr.Message)
	}
	env := s.lambdaEnv(fn)
	if got := env["AWS_ENDPOINT_URL"]; got != "http://host.docker.internal:4702" {
		t.Errorf("AWS_ENDPOINT_URL = %q", got)
	}
	if env["TABLE"] != "items" {
		t.Errorf("the client's own variables must survive: %v", env)
	}
	if env["AWS_REGION"] != "us-east-1" {
		t.Errorf("AWS_REGION = %q", env["AWS_REGION"])
	}

	// A variable the client set wins over ours: it may be pointing
	// somewhere on purpose.
	fn.Environment = map[string]any{"Variables": map[string]any{
		"AWS_ENDPOINT_URL": "http://elsewhere:1234",
	}}
	if got := s.lambdaEnv(fn)["AWS_ENDPOINT_URL"]; got != "http://elsewhere:1234" {
		t.Errorf("a client's own endpoint was overridden: %q", got)
	}
}

// Invoking a function whose container is not up must say so rather than
// pretend: the provider waits on State, and a 500 would read as broken.
func TestInvokingABeforeItIsReadyNamesTheState(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	rec, doc := lambdaCall(t, s, http.MethodPost,
		"/2015-03-31/functions/writer/invocations", `{"id":"1"}`)
	if rec.Code == 200 {
		t.Fatal("the container cannot be running in a test")
	}
	if !strings.Contains(str(doc["message"]), "writer") {
		t.Errorf("the error should name the function: %v", doc)
	}
}

// The invoke response's BODY is whatever the handler returned, and whether
// it threw is a HEADER. `aws lambda invoke` prints FunctionError from that
// header alone.
func TestInvokeReturnsTheHandlersBytesAndTheErrorHeader(t *testing.T) {
	s, f := newLambdaTestServer(t)
	createWriter(t, s)

	// Stand in for the container: an httptest server speaking RIE.
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Amz-Function-Error", "Unhandled")
		_, _ = w.Write([]byte(`{"errorMessage":"boom"}`))
	}))
	defer handler.Close()

	fn, _ := s.loadFunction("writer")
	container := "nimbus-lambda-writer-test"
	if _, err := f.Run(nil, docker.RunOptions{Name: container, Image: "img"}); err != nil {
		t.Fatal(err)
	}
	f.LogOutput[container] = []byte("about to fail\n")
	s.lambda.Put(&funcs.Function{
		ResourceID: fn.FunctionArn, Name: fn.FunctionName,
		Container: container, Port: hostPort(t, handler.URL),
	})

	rec, _ := lambdaCall(t, s, http.MethodPost,
		"/2015-03-31/functions/writer/invocations", `{"id":"1"}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("X-Amz-Function-Error"); got != "Unhandled" {
		t.Errorf("X-Amz-Function-Error = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "boom") {
		t.Errorf("body = %s", rec.Body)
	}
	if rec.Header().Get("X-Amzn-RequestId") == "" {
		t.Error("an invocation must carry a request id")
	}

	// What the handler printed is now in its log group, with the START and
	// REPORT lines Lambda itself adds.
	logs := logEventsFor(t, s, "/aws/lambda/writer")
	if !strings.Contains(logs, "about to fail") {
		t.Errorf("the handler's output is not in the log group:\n%s", logs)
	}
	for _, want := range []string{"START RequestId:", "END RequestId:", "REPORT RequestId:"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log is missing the %q line:\n%s", want, logs)
		}
	}
}

// Event is fire and forget: 202 with no body.
func TestAnEventInvocationIsAcceptedWithNoBody(t *testing.T) {
	s, f := newLambdaTestServer(t)
	createWriter(t, s)
	handler := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`"ok"`))
	}))
	defer handler.Close()
	fn, _ := s.loadFunction("writer")
	if _, err := f.Run(nil, docker.RunOptions{Name: "c", Image: "img"}); err != nil {
		t.Fatal(err)
	}
	s.lambda.Put(&funcs.Function{ResourceID: fn.FunctionArn, Name: "writer",
		Container: "c", Port: hostPort(t, handler.URL)})

	req := httptest.NewRequest(http.MethodPost, "/2015-03-31/functions/writer/invocations",
		strings.NewReader("{}"))
	req.Header.Set("X-Amz-Invocation-Type", "Event")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/lambda/aws4_request, Signature=x")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 202 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("an Event invocation must answer with no body, got %s", rec.Body)
	}
}

// No resource policy is ResourceNotFoundException -- the one the provider
// tolerates. Anything else fails the refresh.
func TestNoPolicyIsResourceNotFound(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	rec, doc := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/policy", "")
	if rec.Code != 404 || doc["__type"] != "ResourceNotFoundException" {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}

	rec, _ = lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions/writer/policy", `{
		"StatementId": "s1", "Action": "lambda:InvokeFunction",
		"Principal": "events.amazonaws.com"}`)
	if rec.Code != 201 {
		t.Fatalf("add-permission: status %d: %s", rec.Code, rec.Body)
	}
	_, doc = lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer/policy", "")
	// The policy comes back as a JSON STRING inside the response, one
	// encoding layer deep -- the same shape IAM uses.
	policy, ok := doc["Policy"].(string)
	if !ok {
		t.Fatalf("Policy = %T %v", doc["Policy"], doc["Policy"])
	}
	if !strings.Contains(policy, "s1") {
		t.Errorf("Policy = %s", policy)
	}
}

// Tags live under a different version, with the ARN URL-encoded into ONE
// path segment, colons and all.
func TestTagsAreAddressedByAnEncodedARN(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	path := "/2017-03-31/tags/arn%3Aaws%3Alambda%3Aus-east-1%3A000000000000%3Afunction%3Awriter"
	rec, doc := lambdaCall(t, s, http.MethodGet, path, "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	tags := doc["Tags"].(map[string]any)
	if tags["env"] != "dev" {
		t.Errorf("Tags = %v", tags)
	}
}

func TestAFunctionIsFoundByNameOrByARN(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	for _, ref := range []string{
		"writer",
		"arn%3Aaws%3Alambda%3Aus-east-1%3A000000000000%3Afunction%3Awriter",
	} {
		rec, _ := lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/"+ref+"/configuration", "")
		if rec.Code != 200 {
			t.Errorf("%s: status %d", ref, rec.Code)
		}
	}
}

func TestDeletingAFunctionIs204AndRemovesIt(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	createWriter(t, s)
	rec, _ := lambdaCall(t, s, http.MethodDelete, "/2015-03-31/functions/writer", "")
	if rec.Code != 204 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("a 204 must have no body, got %s", rec.Body)
	}
	rec, _ = lambdaCall(t, s, http.MethodGet, "/2015-03-31/functions/writer", "")
	if rec.Code != 404 {
		t.Errorf("the function is still there: %d", rec.Code)
	}
}

// Code can come from our own S3, which is the point of having built S3
// first: a provider that uploads a package to a bucket and then names it.
func TestCodeCanComeFromNimbusOwnS3(t *testing.T) {
	s, _ := newLambdaTestServer(t)
	zipped, err := base64.StdEncoding.DecodeString(pyZip(t))
	if err != nil {
		t.Fatal(err)
	}
	s.objects.put("packages", "writer.zip", zipped, objectInfo{})

	rec, doc := lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions", `{
		"FunctionName": "fromS3", "Runtime": "python3.12", "Handler": "app.handler",
		"Code": {"S3Bucket": "packages", "S3Key": "writer.zip"}}`)
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	waitSettled(t, s, "fromS3")
	if doc["CodeSize"] != float64(len(zipped)) {
		t.Errorf("CodeSize = %v, want %d", doc["CodeSize"], len(zipped))
	}

	rec, doc = lambdaCall(t, s, http.MethodPost, "/2015-03-31/functions", `{
		"FunctionName": "missing", "Runtime": "python3.12", "Handler": "app.handler",
		"Code": {"S3Bucket": "packages", "S3Key": "absent.zip"}}`)
	if rec.Code != 400 || !strings.Contains(str(doc["message"]), "NoSuchKey") {
		t.Errorf("a missing package: %d %s", rec.Code, rec.Body)
	}
}

func hostPort(t *testing.T, url string) int {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(url, "http://"), ":")
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// logEventsFor reads a group's lines back through the API, so the test goes
// the same way a client would.
func logEventsFor(t *testing.T, s *Server, group string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"logGroupName": "`+group+`"}`))
	req.Header.Set("X-Amz-Target", "Logs_20140328.FilterLogEvents")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/logs/aws4_request, Signature=x")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("reading %s: %d %s", group, rec.Code, rec.Body)
	}
	return rec.Body.String()
}
