package gcp

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Every path and body in this file was captured from the google provider
// 6.50.0 against a logging server. The clusterBody below is the real
// create body, trimmed of the fields nothing reads back.
//
// newGKETestServer wires Docker out, so nothing a test does can start a
// container: the readiness probe needs a real port, which no fake can
// provide. These tests drive the API surface; the engine itself is covered
// by internal/engines/k3s and proven by the gcp-gke example.
func newGKETestServer(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: base, DataDir: t.TempDir()}, store, bus)
	f := docker.NewFake()
	f.Unavailable = true
	s.UseDocker(f)
	return s
}

const gkeLocation = "us-central1-a"

const clusterPath = "/v1/projects/nimbus-local/locations/us-central1-a/clusters"

// The body is WRAPPED in a `cluster` object. Nothing else in GCP does
// that, and a handler reading the body directly would create a cluster
// with no name.
const clusterBody = `{"cluster": {
	"name": "nimbus-gke",
	"initialNodeCount": 1,
	"network": "projects/nimbus-local/global/networks/default",
	"nodeConfig": {"oauthScopes": ["https://www.googleapis.com/auth/monitoring"]},
	"resourceLabels": {"goog-terraform-provisioned": "true"},
	"shieldedNodes": {"enabled": true}
}}`

func TestCreateClusterReadsTheWrappedBodyAndStartsProvisioning(t *testing.T) {
	s := newGKETestServer(t)
	doc := must(t, s, http.MethodPost, clusterPath+"?alt=json&prettyPrint=false", clusterBody)

	// The create answers an operation, in Compute's shape -- `status`, not
	// `done`. Measured: the provider does not poll it, but gcloud does.
	if doc["status"] != "DONE" {
		t.Errorf("operation status = %v, want DONE", doc["status"])
	}
	if doc["operationType"] != "CREATE_CLUSTER" {
		t.Errorf("operationType = %v", doc["operationType"])
	}

	// The provider then polls the CLUSTER. With Docker unavailable the
	// background provision fails, but the name must have survived the
	// unwrapping either way -- that is what this asserts.
	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	if cluster["name"] != "nimbus-gke" {
		t.Fatalf("cluster name = %v, want nimbus-gke (the `cluster` wrapper was not unwrapped)", cluster["name"])
	}
	if cluster["location"] != gkeLocation || cluster["zone"] != gkeLocation {
		t.Errorf("location/zone = %v/%v, want %s", cluster["location"], cluster["zone"], gkeLocation)
	}
	if cluster["selfLink"] != base+clusterPath+"/nimbus-gke" {
		t.Errorf("selfLink = %v", cluster["selfLink"])
	}
}

// The provider decides "create it" from a 404 with the right reason, and
// it asks BEFORE it creates -- measured, the GET comes first.
func TestGetMissingClusterIsANotFoundTheProviderRecognises(t *testing.T) {
	s := newGKETestServer(t)
	rec, doc := call(t, s, http.MethodGet, clusterPath+"/absent", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	e, _ := doc["error"].(map[string]any)
	if e == nil || e["status"] != "NOT_FOUND" {
		t.Fatalf("error = %v, want status NOT_FOUND", doc["error"])
	}
}

// A cluster must report PROVISIONING until its container is up and then
// RUNNING, because that field is the ONLY thing the provider waits on.
// Measured: four PROVISIONING reads in a row and it waited for all of them.
func TestANewClusterIsProvisioningWithAReason(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")

	// With Docker unavailable this settles on ERROR; either is acceptable
	// here, but it must never be RUNNING, which would tell the provider a
	// cluster is ready when nothing is listening.
	switch cluster["status"] {
	case "PROVISIONING", "ERROR":
	default:
		t.Fatalf("status = %v, want PROVISIONING or ERROR", cluster["status"])
	}
	if cluster["statusMessage"] == "" || cluster["statusMessage"] == nil {
		t.Error("a cluster that is not running must say why")
	}
}

func TestClusterCreateRefusesADuplicate(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	rec, doc := call(t, s, http.MethodPost, clusterPath, clusterBody)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body)
	}
	e, _ := doc["error"].(map[string]any)
	if e == nil || e["status"] != "ALREADY_EXISTS" {
		t.Errorf("error = %v, want ALREADY_EXISTS", doc["error"])
	}
}

func TestClusterListAcceptsTheLocationWildcard(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)

	// `locations/-` is GCP's own wildcard, and `nimbus gcp kubeconfig`
	// depends on it: it finds a cluster without being told the location.
	doc := must(t, s, http.MethodGet, "/v1/projects/nimbus-local/locations/-/clusters", "")
	clusters, _ := doc["clusters"].([]any)
	if len(clusters) != 1 {
		t.Fatalf("got %d clusters from the wildcard, want 1", len(clusters))
	}
}

// --- node pools -----------------------------------------------------------

const nodePoolBody = `{"nodePool": {
	"name": "nimbus-pool",
	"initialNodeCount": 2,
	"config": {"machineType": "e2-medium"},
	"management": {"autoRepair": true, "autoUpgrade": true}
}}`

// The parent/child mirror, for the fourth time in this project: Azure's
// subnets, AKS's agent pools, GCP's subnetworks, and now this. A client
// writes the CHILD and reads the result off the PARENT.
func TestANodePoolIsMirroredIntoItsClusterSoTheNextRefreshSeesIt(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)

	pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	if pool["name"] != "nimbus-pool" {
		t.Fatalf("node pool name = %v (the `nodePool` wrapper was not unwrapped)", pool["name"])
	}

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	pools, _ := cluster["nodePools"].([]any)
	found := false
	for _, p := range pools {
		entry, _ := p.(map[string]any)
		if entry != nil && entry["name"] == "nimbus-pool" {
			found = true
			if got := poolCount(entry); got != 2 {
				t.Errorf("the mirrored pool has %d nodes, want 2", got)
			}
		}
	}
	if !found {
		t.Fatal("the cluster does not list its own node pool: the next refresh " +
			"reads the count off the PARENT, so the plan would never settle")
	}
}

// setSize is a POST to a sub-path, not a PATCH, and both count fields have
// to move: a client reads whichever one it set.
func TestSetSizeScalesThePoolAndTheMirror(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools/nimbus-pool/setSize",
		`{"nodeCount": 3}`)

	pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	if poolCount(pool) != 3 {
		t.Errorf("pool count = %v, want 3", pool["nodeCount"])
	}
	if pool["initialNodeCount"] != float64(3) {
		t.Errorf("initialNodeCount = %v, want 3: a client reads whichever field it set",
			pool["initialNodeCount"])
	}

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	for _, p := range cluster["nodePools"].([]any) {
		entry := p.(map[string]any)
		if entry["name"] == "nimbus-pool" && poolCount(entry) != 3 {
			t.Errorf("the mirror still says %d nodes after a scale to 3", poolCount(entry))
		}
	}
}

func TestDeletingANodePoolRemovesItFromTheCluster(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)
	must(t, s, http.MethodDelete, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	for _, p := range cluster["nodePools"].([]any) {
		if entry, _ := p.(map[string]any); entry != nil && entry["name"] == "nimbus-pool" {
			t.Fatal("the cluster still lists a deleted pool, so the next plan " +
				"would try to delete it again")
		}
	}
}

func TestNodePoolOnAMissingClusterIsANotFound(t *testing.T) {
	s := newGKETestServer(t)
	rec, _ := call(t, s, http.MethodPost, clusterPath+"/absent/nodePools", nodePoolBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// --- the token webhook ----------------------------------------------------

// The API server rejects a response whose apiVersion differs from its
// request's and says nothing useful about why -- the EKS lesson, which
// applies here unchanged.
func TestTokenReviewEchoesTheApiVersionAndGrantsClusterAdmin(t *testing.T) {
	s := newGKETestServer(t)
	doc := must(t, s, http.MethodPost, TokenReviewPath,
		`{"apiVersion": "authentication.k8s.io/v1beta1", "kind": "TokenReview",
		  "spec": {"token": "whatever-kubectl-sent"}}`)

	if doc["apiVersion"] != "authentication.k8s.io/v1beta1" {
		t.Errorf("apiVersion = %v, want the request's own", doc["apiVersion"])
	}
	status, _ := doc["status"].(map[string]any)
	if status == nil || status["authenticated"] != true {
		t.Fatalf("status = %v, want authenticated", doc["status"])
	}
	user, _ := status["user"].(map[string]any)
	groups, _ := user["groups"].([]any)
	admin := false
	for _, g := range groups {
		admin = admin || g == "system:masters"
	}
	if !admin {
		t.Error("without system:masters kubectl authenticates and is then forbidden")
	}

	// The token must never come back out.
	spec, _ := doc["spec"].(map[string]any)
	if spec != nil && spec["token"] != nil && spec["token"] != "" {
		t.Errorf("the token was echoed back: %v", spec["token"])
	}
}

// Measured against the provider 6.50.0, which failed the apply with
//
//	json: cannot unmarshal number into Go struct field
//	Operation.progress of type container.OperationProgress
//
// naming neither GKE nor Nimbus. Compute's `progress` is a number and the
// container API's is an object: the same field name in two
// almost-identical documents, with different types.
func TestAGKEOperationDoesNotCarryComputesNumericProgress(t *testing.T) {
	s := newGKETestServer(t)
	doc := must(t, s, http.MethodPost, clusterPath, clusterBody)
	if _, present := doc["progress"]; present {
		t.Fatalf("the operation carries `progress` (%#v); the container client "+
			"unmarshals it as an object and refuses a number", doc["progress"])
	}

	// Compute's still has it, because Compute's really is a number.
	net := must(t, s, http.MethodPost,
		"/compute/v1/projects/nimbus-local/global/networks",
		`{"name": "n1", "autoCreateSubnetworks": false}`)
	if net["progress"] != float64(100) {
		t.Errorf("a Compute operation's progress = %#v, want 100", net["progress"])
	}
}

// `remove_default_node_pool = true` is in almost every real GKE config,
// and it makes the provider DELETE default-pool right after the create.
// Measured: with the pool existing only as an entry in the cluster's
// array, the apply failed on a 404 for a pool Terraform had just been
// told about.
func TestTheDefaultPoolIsARealResourceSoItCanBeRemoved(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)

	pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/default-pool", "")
	if pool["name"] != "default-pool" {
		t.Fatalf("default-pool = %v, want a readable node pool", pool)
	}
	must(t, s, http.MethodDelete, clusterPath+"/nimbus-gke/nodePools/default-pool", "")

	rec, _ := call(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/default-pool", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("after deleting it the pool still reads %d", rec.Code)
	}
	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	for _, p := range cluster["nodePools"].([]any) {
		if entry, _ := p.(map[string]any); entry != nil && entry["name"] == "default-pool" {
			t.Error("the cluster still lists the removed default pool")
		}
	}
}

// Both fields are ForceNew, and the provider reads them out of
// `networkConfig` rather than the top-level fields the create sent them
// in. Measured: with only the top-level ones filled, a plan after a
// successful apply wanted to REPLACE the cluster.
func TestTheNetworkComesBackWhereTheProviderLooksForIt(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, `{"cluster": {
		"name": "nimbus-gke",
		"initialNodeCount": 1,
		"network": "projects/nimbus-local/global/networks/nimbus-gke-net",
		"subnetwork": "projects/nimbus-local/regions/us-central1/subnetworks/nodes",
		"networkConfig": {}
	}}`)

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	cfg, _ := cluster["networkConfig"].(map[string]any)
	if cfg == nil {
		t.Fatal("networkConfig is missing: the provider reads the network from it")
	}
	if cfg["network"] != "projects/nimbus-local/global/networks/nimbus-gke-net" {
		t.Errorf("networkConfig.network = %v", cfg["network"])
	}
	if cfg["subnetwork"] != "projects/nimbus-local/regions/us-central1/subnetworks/nodes" {
		t.Errorf("networkConfig.subnetwork = %v", cfg["subnetwork"])
	}
}

// A create sends initialNodeCount and the provider reads nodeCount back.
// Measured: filling only the field that arrived left every plan wanting to
// change the count from 0 to what it already was.
func TestANewPoolReportsBothCountFields(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)

	pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	if pool["nodeCount"] != float64(2) {
		t.Errorf("nodeCount = %#v, want 2", pool["nodeCount"])
	}
	if pool["initialNodeCount"] != float64(2) {
		t.Errorf("initialNodeCount = %#v, want 2", pool["initialNodeCount"])
	}
}

// The subtlest thing in this milestone. `node_count` is not read from the
// API at all: the provider computes it as
//
//	initialNodeCount / len(instanceGroupUrls)
//
// so a pool with no instance groups reports ZERO -- by division, not by
// absence -- and every plan after a clean apply wanted to change the count
// from 0 to what it already was. Nothing in the diff mentioned instance
// groups.
func TestAPoolHasInstanceGroupsOrItsCountComputesToZero(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)

	for _, path := range []string{"nimbus-pool", "default-pool"} {
		pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/"+path, "")
		groups, _ := pool["instanceGroupUrls"].([]any)
		if len(groups) != 1 {
			t.Fatalf("%s has %d instance groups, want exactly one per zone: "+
				"the provider divides by this", path, len(groups))
		}
	}

	// And it must not move between reads, or the derived count moves with it.
	first := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	second := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	if first["instanceGroupUrls"].([]any)[0] != second["instanceGroupUrls"].([]any)[0] {
		t.Error("the instance group URL changed between reads, so the node count would too")
	}
}

// Scaling changes the node count, not the number of zones, so the groups
// stay as they were -- and the count the provider derives still follows.
func TestScalingDoesNotChangeTheInstanceGroups(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)
	before := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")

	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools/nimbus-pool/setSize",
		`{"nodeCount": 4}`)
	after := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")

	if len(after["instanceGroupUrls"].([]any)) != len(before["instanceGroupUrls"].([]any)) {
		t.Error("scaling changed the number of instance groups; it should change the count")
	}
	if after["nodeCount"] != float64(4) {
		t.Errorf("nodeCount = %v, want 4", after["nodeCount"])
	}
}

// The provider does not read node_count from the container API -- that API
// does not report it. It takes the pool's instanceGroupUrls, GETs each one
// from the COMPUTE API and divides the summed targetSize by their number.
// Found by reading the provider's source after the plan would not settle.
func TestTheInstanceGroupManagerReportsThePoolsCount(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)

	pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
	url := pool["instanceGroupUrls"].([]any)[0].(string)
	path := strings.TrimPrefix(url, base)

	igm := must(t, s, http.MethodGet, path, "")
	if igm["targetSize"] != float64(2) {
		t.Fatalf("targetSize = %#v, want the pool's 2: the provider divides by this",
			igm["targetSize"])
	}

	// And it follows a scale, or the derived count goes stale.
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools/nimbus-pool/setSize",
		`{"nodeCount": 5}`)
	igm = must(t, s, http.MethodGet, path, "")
	if igm["targetSize"] != float64(5) {
		t.Errorf("after a scale targetSize = %v, want 5", igm["targetSize"])
	}
}

// A manager nobody owns must 404 rather than report a size, or a stale URL
// in someone's state reads as a real pool.
func TestAnUnownedInstanceGroupManagerIsANotFound(t *testing.T) {
	s := newGKETestServer(t)
	rec, _ := call(t, s, http.MethodGet,
		"/compute/v1/projects/nimbus-local/zones/us-central1-a/instanceGroupManagers/gke-nope-grp", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// The create must report the endpoint, not only the eventual read.
//
// Measured: with the endpoint filled in only once the cluster became
// RUNNING, it was EMPTY in Terraform state after a successful apply and
// the next plan reported it as changed outside of Terraform. The provider
// writes state from a read taken while the cluster is still PROVISIONING.
func TestANewClusterAlreadyKnowsItsEndpoint(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	endpoint, _ := cluster["endpoint"].(string)
	if !strings.HasPrefix(endpoint, "127.0.0.1:") {
		t.Fatalf("endpoint = %q while PROVISIONING; a client reads state from "+
			"exactly this moment", endpoint)
	}
}

// The verb is a COLON SUFFIX on the pool's name, not a path segment.
// Measured: a scale failed with "Nimbus does not emulate POST
// .../nodePools/nimbus-workers:setSize yet" against a handler that
// expected /setSize as its own segment.
func TestSetSizeArrivesAsAColonVerbOnTheName(t *testing.T) {
	for _, path := range []string{
		"/nimbus-gke/nodePools/nimbus-pool:setSize",
		"/nimbus-gke/nodePools/nimbus-pool/setSize",
	} {
		s := newGKETestServer(t)
		must(t, s, http.MethodPost, clusterPath, clusterBody)
		must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)
		must(t, s, http.MethodPost, clusterPath+path, `{"nodeCount": 3}`)

		pool := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke/nodePools/nimbus-pool", "")
		if pool["nodeCount"] != float64(3) {
			t.Errorf("%s did not scale the pool: nodeCount = %v", path, pool["nodeCount"])
		}
	}
}

// `gcloud container clusters list` reads NUM_NODES from the cluster's
// currentNodeCount, not from its pools. The provider never looks at it --
// it derives its count from the instance groups -- so this only ever
// showed up in the CLI, reporting one node for a two-node cluster.
func TestTheClusterReportsItsTotalNodeCountForTheCLI(t *testing.T) {
	s := newGKETestServer(t)
	must(t, s, http.MethodPost, clusterPath, clusterBody)
	must(t, s, http.MethodPost, clusterPath+"/nimbus-gke/nodePools", nodePoolBody)

	cluster := must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	// default-pool's 1 plus the new pool's 2.
	if cluster["currentNodeCount"] != float64(3) {
		t.Errorf("currentNodeCount = %v, want 3", cluster["currentNodeCount"])
	}

	must(t, s, http.MethodDelete, clusterPath+"/nimbus-gke/nodePools/default-pool", "")
	cluster = must(t, s, http.MethodGet, clusterPath+"/nimbus-gke", "")
	if cluster["currentNodeCount"] != float64(2) {
		t.Errorf("after removing the default pool currentNodeCount = %v, want 2",
			cluster["currentNodeCount"])
	}
}
