package azure

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

func newACRTestServer(t *testing.T) (*httptest.Server, *Server, *docker.Fake) {
	t.Helper()
	srv, _ := newTestServer(t)
	az := serverOf(t, srv)
	f := docker.NewFake()
	f.Images[az.cfg.RegistryImage] = true
	az.docker = f
	return srv, az, f
}

// The fields azurerm reads straight back after creating a registry. The one
// that matters most is loginServer: it is what somebody types into
// `docker push`, and what the image reference in a pod spec has to say.
func TestRegistryServerFieldsAreFilledAndStable(t *testing.T) {
	srv, _, _ := newACRTestServer(t)
	rg := sub + "/resourceGroups/rg-acr"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerRegistry/registries/acrnimbus"

	code, body := do(t, srv, "PUT", id, map[string]any{
		"location": "eastus", "sku": map[string]any{"name": "Basic"},
		"properties": map[string]any{},
	})
	if code != 201 {
		t.Fatalf("PUT registry = %d: %v", code, body)
	}
	props := body["properties"].(map[string]any)
	login := str(props["loginServer"])
	// localhost, deliberately: Docker treats a localhost registry as insecure
	// by default, so `docker push` needs no machine configuration at all.
	if !strings.HasPrefix(login, "localhost:") {
		t.Errorf("loginServer = %q, want localhost:<port>", login)
	}
	for _, f := range []string{"creationDate", "adminUserEnabled", "provisioningState"} {
		if props[f] == nil {
			t.Errorf("missing %q: %v", f, props)
		}
	}

	// Stable: the login server ends up in state, in image references and in
	// the cluster's containerd mirror, so it must not move between reads.
	_, again := do(t, srv, "PUT", id, map[string]any{
		"location": "eastus", "sku": map[string]any{"name": "Basic"},
		"properties": map[string]any{"adminUserEnabled": true},
	})
	ap := again["properties"].(map[string]any)
	if ap["loginServer"] != login || ap["creationDate"] != props["creationDate"] {
		t.Errorf("registry identity changed on update: %v then %v", props, ap)
	}
	if ap["adminUserEnabled"] != true {
		t.Errorf("adminUserEnabled did not update: %v", ap["adminUserEnabled"])
	}
	_, got := do(t, srv, "GET", id, nil)
	if got["properties"].(map[string]any)["loginServer"] != login {
		t.Error("loginServer changed between reads")
	}
}

// A registry's port has to survive a restart, because the port is inside the
// login server, which is inside image references people have already pushed.
func TestRegistryPortIsTakenFromTheStoredLoginServer(t *testing.T) {
	srv, az, _ := newACRTestServer(t)
	rg := sub + "/resourceGroups/rg-acrport"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerRegistry/registries/acrkeepport"
	_, body := do(t, srv, "PUT", id, map[string]any{"location": "eastus", "properties": map[string]any{}})
	login := str(body["properties"].(map[string]any)["loginServer"])

	// Forget everything in memory, as a restart does, and write again.
	az.registries.remove(id)
	_, after := do(t, srv, "PUT", id, map[string]any{"location": "eastus", "properties": map[string]any{}})
	if str(after["properties"].(map[string]any)["loginServer"]) != login {
		t.Errorf("loginServer moved after a restart: %v, want %v",
			after["properties"].(map[string]any)["loginServer"], login)
	}
	if reg := az.registries.get(id); reg == nil || fmt.Sprintf("localhost:%d", reg.Port) != login {
		t.Errorf("registry port not recovered from the stored login server: %+v", reg)
	}
}

// Registry names become container names and DNS-ish labels, so Azure's rule
// (5-50 alphanumeric characters) is enforced here rather than trusted to the
// client.
func TestRegistryNameIsValidated(t *testing.T) {
	srv, _, _ := newACRTestServer(t)
	rg := sub + "/resourceGroups/rg-acrbad"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	for _, name := range []string{"acr", "has-dash", "has_underscore", strings.Repeat("a", 51)} {
		code, body := do(t, srv, "PUT", rg+"/providers/Microsoft.ContainerRegistry/registries/"+name,
			map[string]any{"location": "eastus", "properties": map[string]any{}})
		if code != 400 {
			t.Errorf("registry name %q accepted with %d (%v)", name, code, errCode(body))
		}
	}
}

func TestRegistryCredentialsAreStable(t *testing.T) {
	srv, _, _ := newACRTestServer(t)
	rg := sub + "/resourceGroups/rg-acrcreds"
	do(t, srv, "PUT", rg, map[string]any{"location": "eastus"})
	id := rg + "/providers/Microsoft.ContainerRegistry/registries/acrcreds"
	do(t, srv, "PUT", id, map[string]any{"location": "eastus",
		"properties": map[string]any{"adminUserEnabled": true}})

	code, creds := do(t, srv, "POST", id+"/listCredentials", nil)
	if code != 200 {
		t.Fatalf("listCredentials = %d: %v", code, creds)
	}
	if creds["username"] != "acrcreds" {
		t.Errorf("username = %v, Azure uses the registry name", creds["username"])
	}
	passwords, _ := creds["passwords"].([]any)
	if len(passwords) != 2 {
		t.Fatalf("got %d passwords, want 2: %v", len(passwords), creds)
	}
	first := passwords[0].(map[string]any)
	if first["name"] != "password" || str(first["value"]) == "" {
		t.Errorf("password shape = %v", first)
	}
	_, again := do(t, srv, "POST", id+"/listCredentials", nil)
	if fmtAny(again) != fmtAny(creds) {
		t.Error("credentials changed between calls")
	}

	// A GET of the registry must never carry them, as with storage keys.
	_, body := do(t, srv, "GET", id, nil)
	if strings.Contains(fmtAny(body), str(first["value"])) {
		t.Error("GET of a registry exposed its password")
	}
}

// The container Nimbus starts for a registry: the image, the published port
// and the labels a restart needs to find it again.
func TestStartRegistryRunsTheContainer(t *testing.T) {
	_, az, f := newACRTestServer(t)
	reg := &registry{
		ResourceID: sub + "/resourceGroups/rg/providers/Microsoft.ContainerRegistry/registries/acrrun",
		Name:       "acrrun", Container: "nimbus-acr-acrrun-abcd1234", Port: 5001,
	}
	if err := az.startRegistry(context.Background(), reg); err != nil {
		t.Fatalf("startRegistry: %v", err)
	}
	c, err := f.Inspect(context.Background(), reg.Container)
	if err != nil {
		t.Fatalf("container not started: %v", err)
	}
	if c.Image != az.cfg.RegistryImage {
		t.Errorf("image = %q, want %q", c.Image, az.cfg.RegistryImage)
	}
	// Bound to loopback only: an emulator should not put an unauthenticated
	// registry on the network the machine is attached to.
	if host := c.Ports["5000/tcp"]; !strings.HasPrefix(host, "127.0.0.1:") || !strings.HasSuffix(host, "5001") {
		t.Errorf("published port = %q, want 127.0.0.1:5001", host)
	}
	if c.Labels[labelResource] != reg.ResourceID || c.Labels[labelRole] != "registry" {
		t.Errorf("labels = %v", c.Labels)
	}
}

// Docker stopped must produce an ARM error naming the cause, not a hang: the
// same rule M3 set for clusters.
func TestDockerStoppedFailsTheRegistryWithAdvice(t *testing.T) {
	_, az, f := newACRTestServer(t)
	f.Unavailable = true
	err := az.startRegistry(context.Background(), &registry{
		ResourceID: "/x", Name: "acrdown", Container: "nimbus-acr-down", Port: 5002,
	})
	if err == nil {
		t.Fatal("starting a registry with Docker down must fail")
	}
	if !strings.Contains(err.Error(), "Docker") {
		t.Errorf("error does not mention Docker: %v", err)
	}
}

func TestRegistryContainerNamesAreStableAndUnique(t *testing.T) {
	a := registryContainerName("/subscriptions/s/resourceGroups/rg1/providers/Microsoft.ContainerRegistry/registries/acr1", "acr1")
	b := registryContainerName("/subscriptions/s/resourceGroups/rg2/providers/Microsoft.ContainerRegistry/registries/acr1", "acr1")
	if a == b {
		t.Error("registries of the same name in different groups share a container name")
	}
	if a != registryContainerName(strings.ToUpper("/subscriptions/s/resourceGroups/rg1/providers/Microsoft.ContainerRegistry/registries/acr1"), "acr1") {
		t.Error("container name depends on the casing of the resource ID")
	}
	if !strings.HasPrefix(a, "nimbus-acr-acr1-") {
		t.Errorf("container name %q should be readable in docker ps", a)
	}
}

// A registry's login server is localhost:<port>, which means something
// different inside the cluster than it does on the machine. Each node gets a
// containerd mirror rewriting it to the registry container, which is only
// reachable once that container has joined the cluster's network.
func TestRegistryIsMirroredIntoEveryNode(t *testing.T) {
	_, az, f := newACRTestServer(t)
	f.ExecAny = true
	f.ExecOutput["cat "+containerdConfig] = []byte(`[plugins."io.containerd.grpc.v1.cri".registry]
  config_path = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"`)

	cl := &cluster{
		ResourceID: sub + "/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks1",
		Name:       "aks1", Container: "nimbus-aks-aks1-1234", Network: "nimbus-aks-aks1-1234-net",
		Agents: []string{"nimbus-aks-aks1-1234-agent-1"},
	}
	az.clusters.put(cl)
	for _, node := range append([]string{cl.Container}, cl.Agents...) {
		if _, err := f.Run(context.Background(), docker.RunOptions{Name: node, Image: az.cfg.K3sImage}); err != nil {
			t.Fatal(err)
		}
	}
	reg := &registry{
		ResourceID: sub + "/resourceGroups/rg/providers/Microsoft.ContainerRegistry/registries/acrmirror",
		Name:       "acrmirror", Container: "nimbus-acr-acrmirror-9999", Port: 5003,
	}
	az.registries.put(reg)

	az.announceRegistryToClusters(context.Background(), reg)

	calls := strings.Join(f.Calls, "\n")
	if !strings.Contains(calls, "connect-network "+cl.Network+" "+reg.Container) {
		t.Errorf("the registry did not join the cluster network:\n%s", calls)
	}
	// Both the server and the agent: a pod can be scheduled on either, and an
	// agent that cannot pull fails in a way that looks like a bad image.
	for _, node := range []string{cl.Container, cl.Agents[0]} {
		if !strings.Contains(calls, "exec "+node) {
			t.Errorf("no mirror written on %s:\n%s", node, calls)
		}
	}
	// The mirror is keyed by the host exactly as an image reference spells it,
	// and points at the registry container rather than at localhost.
	if !strings.Contains(calls, certsDir+"/localhost:5003") {
		t.Errorf("mirror not written under the login server's host:\n%s", calls)
	}
	if !strings.Contains(calls, "/etc/rancher/k3s/registries.yaml") {
		t.Errorf("registries.yaml not written as the restart-time fallback:\n%s", calls)
	}
}

// If this k3s build only reads registry configuration at startup, say so in
// the feed. Otherwise it surfaces much later as an image pull failure inside
// the cluster, which says nothing about Nimbus.
func TestMirrorWarnsWhenContainerdNeedsARestart(t *testing.T) {
	_, az, f := newACRTestServer(t)
	f.ExecAny = true
	f.ExecOutput["cat "+containerdConfig] = []byte("[plugins]\n # no config_path here\n")
	cl := &cluster{ResourceID: "/c", Name: "aks1", Container: "c1", Network: "n1"}
	az.clusters.put(cl)
	if _, err := f.Run(context.Background(), docker.RunOptions{Name: "c1", Image: az.cfg.K3sImage}); err != nil {
		t.Fatal(err)
	}
	reg := &registry{ResourceID: "/r", Name: "acrold", Container: "r1", Port: 5004}

	events, _, unsubscribe := az.bus.Subscribe()
	defer unsubscribe()
	az.announceRegistryToClusters(context.Background(), reg)

	deadline := time.After(2 * time.Second)
	for {
		select {
		case e := <-events:
			if strings.Contains(e.Note, "only reads registry configuration at startup") {
				return
			}
		case <-deadline:
			t.Fatal("no warning published when containerd cannot reload mirrors")
		}
	}
}

// A registry container can outlive Nimbus -- a restart without --persist
// forgets the resource but not the container. If it is published on a
// different port than the resource now says, the resource wins: its login
// server is already inside image references and cluster mirrors.
func TestStaleRegistryContainerOnTheWrongPortIsReplaced(t *testing.T) {
	_, az, f := newACRTestServer(t)
	reg := &registry{ResourceID: "/r", Name: "acrmoved", Container: "nimbus-acr-acrmoved-0000", Port: 5002}

	// Left behind by a previous run, on the port that run had chosen.
	if _, err := f.Run(context.Background(), docker.RunOptions{
		Name: reg.Container, Image: az.cfg.RegistryImage,
		Ports: map[string]string{"127.0.0.1:5001": "5000"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := az.startRegistry(context.Background(), reg); err != nil {
		t.Fatalf("startRegistry: %v", err)
	}
	c, err := f.Inspect(context.Background(), reg.Container)
	if err != nil {
		t.Fatalf("container missing after replacement: %v", err)
	}
	if got := containerPort(c, "5000/tcp"); got != 5002 {
		t.Errorf("container is published on %d, want the resource's port 5002", got)
	}
}
