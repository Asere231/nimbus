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

// The cluster's OIDC issuer, which every EKS Terraform module runs
// data.tls_certificate against.
func TestAClusterReportsItsOIDCIssuer(t *testing.T) {
	s := newEKSTestServer(t)
	s.cfg.TLSURL = "https://localhost:4704"
	makeIRSACluster(t, s, "irsa")

	_, doc := restJSON(t, s, http.MethodGet, "/clusters/irsa", "")
	cluster, _ := doc["cluster"].(map[string]any)
	identity, _ := cluster["identity"].(map[string]any)
	oidc, _ := identity["oidc"].(map[string]any)
	issuer := str(oidc["issuer"])
	if !strings.HasPrefix(issuer, "https://localhost:4704/oidc/") {
		t.Fatalf("the issuer is %q", issuer)
	}

	// DERIVED from the cluster's name, which a re-read cannot prove
	// because the issuer is stored on the cluster -- so it is asserted
	// directly. It matters because the verification path has to map a
	// token's `iss` back to a cluster without reading the store, and
	// because a value that moved would invalidate every token already
	// issued against it.
	if oidcID("irsa") != oidcID("irsa") || !strings.HasSuffix(issuer, oidcID("irsa")) {
		t.Errorf("the issuer id is not derived from the cluster name")
	}

	// Two clusters get DIFFERENT issuers, as real EKS gives.
	makeIRSACluster(t, s, "other")
	_, otherDoc := restJSON(t, s, http.MethodGet, "/clusters/other", "")
	oc, _ := otherDoc["cluster"].(map[string]any)
	oi, _ := oc["identity"].(map[string]any)
	oo, _ := oi["oidc"].(map[string]any)
	if str(oo["issuer"]) == issuer {
		t.Error("two clusters share one issuer, so one OIDC provider would cover both")
	}
}

// With no TLS listener there is no issuer to serve, so the cluster
// reports none rather than advertising a URL that refuses.
func TestNoTLSListenerMeansNoIssuer(t *testing.T) {
	s := newEKSTestServer(t)
	s.cfg.TLSURL = ""
	makeIRSACluster(t, s, "plain")

	_, doc := restJSON(t, s, http.MethodGet, "/clusters/plain", "")
	cluster, _ := doc["cluster"].(map[string]any)
	if _, present := cluster["identity"]; present {
		t.Errorf("a cluster with no TLS listener claims an identity: %v", cluster["identity"])
	}
}

// The discovery document and the JWKS, which are fetched by clients that
// sign nothing -- so they must be routed before anything reads a
// credential scope.
func TestTheIssuerServesDiscoveryAndKeys(t *testing.T) {
	s := newEKSTestServer(t)
	s.cfg.TLSURL = "https://localhost:4704"
	key, err := core.EnsureSigningKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s.useSigningKey(key)
	makeIRSACluster(t, s, "irsa")
	id := oidcID("irsa")

	// Unsigned: no Authorization header at all.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/oidc/"+id+"/.well-known/openid-configuration", nil))
	if rec.Code != 200 {
		t.Fatalf("discovery = %d %s", rec.Code, rec.Body)
	}
	var cfg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	want := "https://localhost:4704/oidc/" + id
	// The issuer must match the token's `iss` EXACTLY or every
	// verification fails, so it is worth asserting rather than assuming.
	if str(cfg["issuer"]) != want {
		t.Errorf("discovery says issuer %q, want %q", cfg["issuer"], want)
	}
	if str(cfg["jwks_uri"]) != want+"/keys" {
		t.Errorf("jwks_uri is %q", cfg["jwks_uri"])
	}

	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oidc/"+id+"/keys", nil))
	if rec.Code != 200 {
		t.Fatalf("keys = %d %s", rec.Code, rec.Body)
	}
	var jwks map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil {
		t.Fatal(err)
	}
	keys, _ := jwks["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("the JWKS has %d keys", len(keys))
	}
	jwk, _ := keys[0].(map[string]any)
	if jwk["kid"] != key.KeyID() {
		t.Errorf("the served kid %v is not the signing key's %q", jwk["kid"], key.KeyID())
	}

	// With NO signing key there is nothing to publish, and the JWKS says
	// so rather than answering an empty key set -- which a consumer would
	// read as "this issuer signs nothing" and cache.
	bare := newEKSTestServer(t)
	bare.cfg.TLSURL = "https://localhost:4704"
	makeIRSACluster(t, bare, "irsa")
	rec = httptest.NewRecorder()
	bare.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oidc/"+id+"/keys", nil))
	if rec.Code != 404 {
		t.Errorf("a server with no signing key served a JWKS: %d %s", rec.Code, rec.Body)
	}

	// A document nobody serves is a 404 naming itself, not a silent empty.
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oidc/"+id+"/nonsense", nil))
	if rec.Code != 404 {
		t.Errorf("an unknown OIDC document = %d", rec.Code)
	}
}

// makeIRSACluster creates a cluster the way a client does.
func makeIRSACluster(t *testing.T, s *Server, name string) {
	t.Helper()
	rec, _ := restJSON(t, s, http.MethodPost, "/clusters", `{
		"name": "`+name+`",
		"roleArn": "arn:aws:iam::000000000000:role/eks",
		"resourcesVpcConfig": {"subnetIds": ["subnet-1", "subnet-2"]}
	}`)
	if rec.Code != 200 {
		t.Fatalf("creating %s: %d %s", name, rec.Code, rec.Body)
	}
}
