package aws

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// newEKSTestServer wires a server to a Docker that is not there, so nothing
// a test does can start a container. The readiness probe talks to a real
// port, which no fake can provide, so these tests drive the API surface and
// leave the engine itself to internal/engines/k3s and the azure-aks example.
func newEKSTestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "http://localhost:4702", DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

// restJSON replays what the EKS SDK sends: a method and a path, a JSON body,
// and a credential scope naming "eks". The path matters here, unlike for the
// Query services -- but the scope is still what routes it.
func restJSON(t *testing.T, s *Server, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization",
		"AWS4-HMAC-SHA256 Credential=nimbus/20261106/us-east-1/eks/aws4_request, "+
			"SignedHeaders=content-type;host;x-amz-date, Signature=140c73865eeb6a69a400e8")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var doc map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("response is not JSON: %v\n%s", err, rec.Body.String())
		}
	}
	return rec, doc
}

func TestCreateClusterAnswersWithACreatingCluster(t *testing.T) {
	s := newEKSTestServer(t)
	rec, doc := restJSON(t, s, http.MethodPost, "/clusters", `{
		"name": "nimbus",
		"roleArn": "arn:aws:iam::000000000000:role/eks",
		"resourcesVpcConfig": {"subnetIds": ["subnet-1", "subnet-2"]},
		"tags": {"env": "dev"}
	}`)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	c, ok := doc["cluster"].(map[string]any)
	if !ok {
		t.Fatalf("no cluster in %v", doc)
	}
	// The provider polls DescribeCluster until this says ACTIVE, so a create
	// that claimed ACTIVE before a container existed would make kubectl fail
	// the moment the apply finished.
	if c["status"] != "CREATING" {
		t.Errorf("status = %v, want CREATING", c["status"])
	}
	if want := "arn:aws:eks:us-east-1:000000000000:cluster/nimbus"; c["arn"] != want {
		t.Errorf("arn = %v, want %v", c["arn"], want)
	}
	if c["version"] == "" || c["version"] == nil {
		t.Error("a cluster must report a Kubernetes version")
	}
	vpc := c["resourcesVpcConfig"].(map[string]any)
	if n := len(vpc["subnetIds"].([]any)); n != 2 {
		t.Errorf("subnetIds = %v", vpc["subnetIds"])
	}
	if vpc["endpointPublicAccess"] != true {
		t.Error("endpointPublicAccess must be true: it is how the provider decides it can reach the API")
	}
}

func TestCreatingTheSameClusterTwiceIsResourceInUse(t *testing.T) {
	s := newEKSTestServer(t)
	restJSON(t, s, http.MethodPost, "/clusters", `{"name":"nimbus"}`)
	rec, doc := restJSON(t, s, http.MethodPost, "/clusters", `{"name":"nimbus"}`)
	if rec.Code != 409 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if doc["__type"] != "ResourceInUseException" {
		t.Errorf("__type = %v", doc["__type"])
	}
}

// A REST-JSON client reads the exception type out of a HEADER as well as the
// body. Without it a 404 arrives as a generic failure rather than as the
// ResourceNotFoundException the provider is written to tolerate, and a
// destroy retries until it times out.
func TestDescribingAnUnknownClusterNamesTheExceptionTwice(t *testing.T) {
	s := newEKSTestServer(t)
	rec, doc := restJSON(t, s, http.MethodGet, "/clusters/absent", "")
	if rec.Code != 404 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("x-amzn-ErrorType"); got != "ResourceNotFoundException" {
		t.Errorf("x-amzn-ErrorType = %q", got)
	}
	if doc["__type"] != "ResourceNotFoundException" {
		t.Errorf("__type = %v", doc["__type"])
	}
}

func TestClustersAndNodegroupsRoundTrip(t *testing.T) {
	s := newEKSTestServer(t)
	// Seeded directly: starting a container needs Docker, and what is under
	// test is the API shape on top of it.
	if aerr := s.saveEKSCluster(&eksCluster{
		Name: "nimbus", Arn: s.eksClusterARN("nimbus"), Status: "ACTIVE",
		Endpoint: "https://127.0.0.1:6600", Version: "1.34",
	}); aerr != nil {
		t.Fatal(aerr.Message)
	}

	_, doc := restJSON(t, s, http.MethodGet, "/clusters", "")
	if names := doc["clusters"].([]any); len(names) != 1 || names[0] != "nimbus" {
		t.Errorf("clusters = %v", doc["clusters"])
	}

	rec, doc := restJSON(t, s, http.MethodPost, "/clusters/nimbus/node-groups", `{
		"nodegroupName": "default",
		"scalingConfig": {"minSize": 1, "maxSize": 3, "desiredSize": 2},
		"subnets": ["subnet-1"]
	}`)
	if rec.Code != 200 {
		t.Fatalf("creating a node group: %d %s", rec.Code, rec.Body)
	}
	ng := doc["nodegroup"].(map[string]any)
	if ng["status"] != "CREATING" {
		t.Errorf("status = %v", ng["status"])
	}
	if ng["amiType"] == nil || ng["capacityType"] == nil || ng["diskSize"] == nil {
		t.Errorf("a node group must report the fields the provider reads back: %v", ng)
	}
	want := "arn:aws:eks:us-east-1:000000000000:nodegroup/nimbus/default"
	if ng["nodegroupArn"] != want {
		t.Errorf("nodegroupArn = %v, want %v", ng["nodegroupArn"], want)
	}

	_, doc = restJSON(t, s, http.MethodGet, "/clusters/nimbus/node-groups", "")
	if names := doc["nodegroups"].([]any); len(names) != 1 || names[0] != "default" {
		t.Errorf("nodegroups = %v", doc["nodegroups"])
	}
	_, doc = restJSON(t, s, http.MethodGet, "/clusters/nimbus/node-groups/default", "")
	if doc["nodegroup"].(map[string]any)["clusterName"] != "nimbus" {
		t.Errorf("describe nodegroup = %v", doc)
	}
}

func TestANodegroupOnAnUnknownClusterIsNotFound(t *testing.T) {
	s := newEKSTestServer(t)
	rec, doc := restJSON(t, s, http.MethodPost, "/clusters/absent/node-groups",
		`{"nodegroupName":"default"}`)
	if rec.Code != 404 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if doc["__type"] != "ResourceNotFoundException" {
		t.Errorf("__type = %v", doc["__type"])
	}
}

// Deleting answers DELETING and leaves the container to a goroutine, which
// is what the provider polls on. It must not 404 straight away, or the
// provider reads the destroy as having failed before it started.
func TestDeletingAClusterReportsDeleting(t *testing.T) {
	s := newEKSTestServer(t)
	if aerr := s.saveEKSCluster(&eksCluster{
		Name: "nimbus", Arn: s.eksClusterARN("nimbus"), Status: "ACTIVE",
	}); aerr != nil {
		t.Fatal(aerr.Message)
	}
	rec, doc := restJSON(t, s, http.MethodDelete, "/clusters/nimbus", "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if doc["cluster"].(map[string]any)["status"] != "DELETING" {
		t.Errorf("status = %v", doc["cluster"])
	}
}

// --- the token webhook ----------------------------------------------------

// `aws eks get-token` builds its bearer token client-side out of a presigned
// STS URL, so Nimbus can never predict it and no static token file can
// match. Real EKS answers that with a token authentication webhook, and so
// does this: the API server asks, and Nimbus says yes.
func TestTokenReviewAuthenticatesAsClusterAdmin(t *testing.T) {
	s := newEKSTestServer(t)
	req := httptest.NewRequest(http.MethodPost, TokenReviewPath,
		strings.NewReader(string(marshalTokenReview("k8s-aws-v1.aHR0cHM6Ly9zdHM"))))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var got tokenReview
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// The API server rejects a response whose apiVersion differs from its
	// request's, and says nothing useful when it does.
	if got.APIVersion != "authentication.k8s.io/v1" || got.Kind != "TokenReview" {
		t.Errorf("echoed %q/%q", got.APIVersion, got.Kind)
	}
	if got.Status == nil || !got.Status.Authenticated {
		t.Fatalf("not authenticated: %s", rec.Body)
	}
	if !contains(got.Status.User.Groups, "system:masters") {
		t.Errorf("groups = %v; without system:masters kubectl is forbidden straight after authenticating",
			got.Status.User.Groups)
	}
	if got.Spec.Token != "" {
		t.Error("the token must not be echoed back")
	}
}

// The webhook is reached FROM INSIDE a container, where the API server signs
// nothing -- so it has to be routed before anything looks for a credential
// scope, and it has to name the host rather than localhost.
func TestTheWebhookIsRoutedWithoutASignature(t *testing.T) {
	s := newEKSTestServer(t)
	req := httptest.NewRequest(http.MethodPost, TokenReviewPath,
		strings.NewReader(string(marshalTokenReview("anything"))))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("an unsigned token review must still be answered: %d %s", rec.Code, rec.Body)
	}
}

func TestTheWebhookConfigAddressesTheHostFromInsideDocker(t *testing.T) {
	dir := t.TempDir()
	s := newEKSTestServer(t)
	path, err := s.writeTokenWebhookConfig(Config{PublicURL: "http://localhost:4702", DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "localhost:4702") {
		t.Error("localhost inside a container is the container; the webhook would never reach Nimbus")
	}
	for _, want := range []string{"host.docker.internal:4702", TokenReviewPath, "kind: Config"} {
		if !strings.Contains(text, want) {
			t.Errorf("webhook kubeconfig is missing %q:\n%s", want, text)
		}
	}
}
