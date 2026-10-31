package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Fake PEM blobs. Only renderKubeconfig's base64 handling cares what is in
// them; the TLS path is covered by the opt-in integration test.
const (
	fakeCA   = "-----BEGIN CERTIFICATE-----\nca\n-----END CERTIFICATE-----\n"
	fakeCert = "-----BEGIN CERTIFICATE-----\ncert\n-----END CERTIFICATE-----\n"
	fakeKey  = "-----BEGIN EC PRIVATE KEY-----\nkey\n-----END EC PRIVATE KEY-----\n"
)

// newAKSTestServer wires a server to a Fake Docker whose canned exec output
// stands in for reading k3s's certificates out of the container.
func newAKSTestServer(t *testing.T) (*httptest.Server, *Server, *docker.Fake) {
	t.Helper()
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	f := docker.NewFake()
	f.Images[az.cfg.K3sImage] = true
	f.ExecOutput["cat "+k3sAdminCert] = []byte(fakeCert)
	f.ExecOutput["cat "+k3sAdminKey] = []byte(fakeKey)
	f.ExecOutput["cat "+k3sServerCA] = []byte(fakeCA)
	f.ExecOutput["cat /var/lib/rancher/k3s/server/node-token"] = []byte("K10::node-token\n")
	az.docker = f
	// The readiness probe talks to a real port, which the Fake cannot provide,
	// so tests drive the pieces around it rather than Provision end to end.
	return srv, az, f
}

func aksBody(nodeCount int) map[string]any {
	return map[string]any{
		"location": "eastus",
		"properties": map[string]any{
			"dnsPrefix": "demo",
			"agentPoolProfiles": []any{
				map[string]any{"name": "default", "count": float64(nodeCount), "vmSize": "Standard_DS2_v2"},
			},
		},
	}
}

// The fields azurerm reads straight back after a create. Any of these drifting
// between reads shows up as a permanent diff.
func TestClusterServerFieldsAreFilledAndStable(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo"

	_, body := do(t, srv, "PUT", id, aksBody(2))
	props := body["properties"].(map[string]any)

	for _, f := range []string{"fqdn", "kubernetesVersion", "currentKubernetesVersion", "nodeResourceGroup", "agentPoolProfiles"} {
		if props[f] == nil {
			t.Errorf("missing server-generated field %q", f)
		}
	}
	if body["identity"] == nil {
		t.Error("a cluster must report an identity")
	}

	// A second PUT must not change any of them.
	_, again := do(t, srv, "PUT", id, aksBody(2))
	p2 := again["properties"].(map[string]any)
	for _, f := range []string{"fqdn", "nodeResourceGroup", "kubernetesVersion"} {
		if props[f] != p2[f] {
			t.Errorf("%s changed between writes: %v -> %v", f, props[f], p2[f])
		}
	}
	if fmtAny(body["identity"]) != fmtAny(again["identity"]) {
		t.Error("identity changed between writes; azurerm stores it and would see a diff")
	}
}

// Azure creates an MC_* resource group alongside the cluster, and azurerm
// reads its name back.
func TestClusterCreatesItsNodeResourceGroup(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks2"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo2"
	_, body := do(t, srv, "PUT", id, aksBody(1))

	name := body["properties"].(map[string]any)["nodeResourceGroup"].(string)
	if !strings.HasPrefix(name, "MC_") {
		t.Errorf("nodeResourceGroup = %q, want an MC_ name", name)
	}
	if code, _ := do(t, srv, "GET", sub+"/resourceGroups/"+name, nil); code != 200 {
		t.Errorf("the node resource group was not created: GET = %d", code)
	}
}

// The kubeconfig must be byte-identical every time it is asked for. azurerm
// calls the credential actions on EVERY refresh and stores the result, so one
// that differed between reads would make the plan never settle -- the same
// shape of bug as M2's Content-MD5.
func TestKubeconfigIsStableAcrossCalls(t *testing.T) {
	srv, az, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks3"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo3"
	do(t, srv, "PUT", id, aksBody(1))

	// Stand in for a provisioned cluster.
	az.clusters.put(&cluster{
		ResourceID: id, Name: "demo3", Container: "c", Port: 6443,
		Kubeconfig: renderKubeconfig("demo3", 6443, []byte(fakeCA), []byte(fakeCert), []byte(fakeKey)),
	})

	var first string
	for i := 0; i < 5; i++ {
		code, body := do(t, srv, "POST", id+"/listClusterUserCredential", nil)
		if code != 200 {
			t.Fatalf("listClusterUserCredential = %d", code)
		}
		list, _ := body["kubeconfigs"].([]any)
		if len(list) != 1 {
			t.Fatalf("kubeconfigs = %v", body)
		}
		value, _ := list[0].(map[string]any)["value"].(string)
		if i == 0 {
			first = value
			continue
		}
		if value != first {
			t.Fatalf("the kubeconfig changed between calls %d and 0; the plan would never settle", i)
		}
	}

	// And it must decode to something kubectl would accept.
	raw, err := base64.StdEncoding.DecodeString(first)
	if err != nil {
		t.Fatalf("the kubeconfig is not valid base64: %v", err)
	}
	for _, want := range []string{"apiVersion: v1", "kind: Config", "server: https://127.0.0.1:6443", "current-context: demo3"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("kubeconfig is missing %q:\n%s", want, raw)
		}
	}
}

// Asking for credentials before the cluster is ready must say so, not hand
// back an empty kubeconfig that fails confusingly in kubectl later.
func TestCredentialsBeforeReadyReportNotReady(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks4"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo4"
	do(t, srv, "PUT", id, aksBody(1))

	_, body := do(t, srv, "POST", id+"/listClusterUserCredential", nil)
	errObj, _ := body["error"].(map[string]any)
	if errObj == nil || errObj["code"] != "ClusterNotReady" {
		t.Errorf("credentials before readiness = %v, want a ClusterNotReady error", body)
	}
}

// node_count is real: N nodes means the k3s server plus N-1 agent containers.
func TestScalingAgentsAddsAndRemovesContainers(t *testing.T) {
	_, az, f := newAKSTestServer(t)
	cl := &cluster{ResourceID: "/c", Name: "demo", Container: "nimbus-aks-demo", Network: "net"}
	if _, err := f.Run(context.Background(), docker.RunOptions{Name: cl.Container, Image: az.cfg.K3sImage}); err != nil {
		t.Fatal(err)
	}

	if err := az.scaleAgents(context.Background(), cl, 3); err != nil {
		t.Fatal(err)
	}
	if len(cl.Agents) != 2 {
		t.Errorf("3 nodes should mean 2 agents (the server is a node too), got %d", len(cl.Agents))
	}

	// Scale up.
	if err := az.scaleAgents(context.Background(), cl, 4); err != nil {
		t.Fatal(err)
	}
	if len(cl.Agents) != 3 {
		t.Errorf("after scaling to 4 nodes, agents = %d, want 3", len(cl.Agents))
	}

	// Scale down: the extra containers must actually be removed.
	if err := az.scaleAgents(context.Background(), cl, 2); err != nil {
		t.Fatal(err)
	}
	if len(cl.Agents) != 1 {
		t.Errorf("after scaling to 2 nodes, agents = %d, want 1", len(cl.Agents))
	}
	running := 0
	for _, c := range f.Containers() {
		if c.Labels[labelRole] == "agent" {
			running++
		}
	}
	if running != 1 {
		t.Errorf("%d agent containers still exist, want 1", running)
	}

	// A single-node cluster needs no agents at all.
	if err := az.scaleAgents(context.Background(), cl, 1); err != nil {
		t.Fatal(err)
	}
	if len(cl.Agents) != 0 {
		t.Errorf("a 1-node cluster should have no agents, got %d", len(cl.Agents))
	}
}

// Docker being stopped must produce a clear, actionable error -- not a hang,
// and not a cluster stuck on Creating.
func TestDockerStoppedFailsTheClusterWithAdvice(t *testing.T) {
	srv, az, f := newAKSTestServer(t)
	f.Unavailable = true

	rg := sub + "/resourceGroups/rg-aks5"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo5"
	res := raw(t, srv, "PUT", id, `{"location":"eastus","properties":{"agentPoolProfiles":[{"name":"d","count":1}]}}`)
	op := res.Header.Get("Azure-AsyncOperation")
	if op == "" {
		t.Fatal("a cluster create must advertise an operation")
	}
	u := op[strings.Index(op, "/subscriptions"):]
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[:i]
	}

	body := waitForOperation(t, srv, u, "Failed")
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "Docker") {
		t.Errorf("the failure should name Docker: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "start docker") {
		t.Errorf("the failure should say what to do about it: %q", msg)
	}

	// And the resource must not be left on Creating.
	_, got := do(t, srv, "GET", id, nil)
	if ps := got["properties"].(map[string]any)["provisioningState"]; ps != "Failed" {
		t.Errorf("provisioningState = %v, want Failed", ps)
	}
	_ = az
}

// Container names must be unique per resource ID but stable across restarts,
// or a restart would start a second cluster beside the first.
func TestContainerNamesAreStableAndUnique(t *testing.T) {
	a := "/subscriptions/s/resourceGroups/rg1/providers/Microsoft.ContainerService/managedClusters/demo"
	b := "/subscriptions/s/resourceGroups/rg2/providers/Microsoft.ContainerService/managedClusters/demo"

	if containerName(a, "demo") != containerName(a, "demo") {
		t.Error("container names must be stable for the same resource")
	}
	if containerName(a, "demo") == containerName(b, "demo") {
		t.Error("same cluster name in different resource groups must not collide")
	}
	// Casing of the ID must not matter: ARM resolves IDs case-insensitively.
	if containerName(a, "demo") != containerName(strings.ToUpper(a), "demo") {
		t.Error("container name changed with the casing of the resource ID")
	}
	for _, n := range []string{"My_Cluster!", "", "a-very-long-cluster-name-that-keeps-going-and-going"} {
		got := containerName("/id", n)
		if strings.ContainsAny(got, "_! ") || len(got) > 63 {
			t.Errorf("containerName(%q) = %q is not a legal docker name", n, got)
		}
	}
}

// fmtAny renders a value for comparison, so a nested map can be checked for
// having stayed the same without depending on map iteration order.
func fmtAny(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// azurerm parses identityProfile.kubeletidentity.resourceId as a full
// UserAssignedIdentity ARM ID, not as a name. Putting the node resource
// group's name there fails the apply outright with
//
//	Error: parsing "MC_rg-aks-dev_aks-nimbus-dev_eastus":
//	parsing the UserAssignedIdentity ID: the number of segments didn't match
//
// which says nothing about which field was wrong.
func TestKubeletIdentityResourceIDIsAFullARMID(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks6"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo6"
	_, body := do(t, srv, "PUT", id, aksBody(1))

	props := body["properties"].(map[string]any)
	kubelet := props["identityProfile"].(map[string]any)["kubeletidentity"].(map[string]any)
	resID, _ := kubelet["resourceId"].(string)

	seg := strings.Split(strings.Trim(resID, "/"), "/")
	if len(seg) != 8 {
		t.Fatalf("kubeletidentity resourceId = %q has %d segments; a UserAssignedIdentity ID has 8", resID, len(seg))
	}
	if !strings.Contains(resID, "/providers/Microsoft.ManagedIdentity/userAssignedIdentities/") {
		t.Errorf("kubeletidentity resourceId is not a UserAssignedIdentity ID: %q", resID)
	}
	if !strings.HasPrefix(resID, sub+"/resourceGroups/MC_") {
		t.Errorf("kubelet identity should live in the node resource group: %q", resID)
	}
	for _, f := range []string{"clientId", "objectId"} {
		if v, _ := kubelet[f].(string); len(v) != 36 {
			t.Errorf("kubeletidentity %s = %q, want a GUID", f, v)
		}
	}
}

// azurerm scales the default node pool by PUTting the agentPools CHILD
// resource, not by updating the cluster. The cluster's embedded
// agentPoolProfiles must mirror that, or the next refresh reads the old count
// back and the plan never settles -- the same parent/child sync the VNet and
// its subnets needed in M1.
func TestScalingAnAgentPoolUpdatesTheClusterProfile(t *testing.T) {
	srv, _, _ := newAKSTestServer(t)
	rg := sub + "/resourceGroups/rg-aks7"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerService/managedClusters/demo7"
	do(t, srv, "PUT", id, aksBody(2))

	// Scale the pool the way azurerm does.
	pool := id + "/agentPools/default"
	if code, _ := do(t, srv, "PUT", pool, map[string]any{
		"properties": map[string]any{"count": float64(3), "vmSize": "Standard_DS2_v2"},
	}); code != 200 && code != 201 {
		t.Fatalf("PUT agent pool = %d", code)
	}

	_, body := do(t, srv, "GET", id, nil)
	pools := body["properties"].(map[string]any)["agentPoolProfiles"].([]any)
	if len(pools) != 1 {
		t.Fatalf("cluster has %d pools, want 1", len(pools))
	}
	if got := pools[0].(map[string]any)["count"]; got != float64(3) {
		t.Errorf("cluster still reports count=%v after the pool was scaled to 3", got)
	}

	// A pool that does not exist on the cluster yet is appended, not dropped.
	do(t, srv, "PUT", id+"/agentPools/extra", map[string]any{
		"properties": map[string]any{"count": float64(1), "vmSize": "Standard_DS2_v2"},
	})
	_, body = do(t, srv, "GET", id, nil)
	if n := len(body["properties"].(map[string]any)["agentPoolProfiles"].([]any)); n != 2 {
		t.Errorf("cluster has %d pools after adding one, want 2", n)
	}
}
