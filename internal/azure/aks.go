package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

const (
	typeManagedCluster = "Microsoft.ContainerService/managedClusters"
	typeAgentPool      = "Microsoft.ContainerService/managedClusters/agentPools"
)

// AKS is the first resource that takes real time to create, and the first
// backed by something you can see from outside Nimbus: `docker ps` shows the
// container, and `kubectl` talks to the actual Kubernetes API inside it.
func registerAKSHooks(s *Server) {
	s.registerHook(typeManagedCluster, Hook{
		Backing: "real",
		// Azure answers a cluster create 201 and makes the client poll; the
		// delete is 202 and also polled.
		CreateStatus: 201,

		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props, _ := body["properties"].(map[string]any)
			if props == nil {
				props = map[string]any{}
				body["properties"] = props
			}

			// Server-generated fields azurerm reads straight back. They must
			// be stable across updates or the plan never settles.
			dns := str(props["dnsPrefix"])
			if dns == "" {
				dns = t.Name
				props["dnsPrefix"] = dns
			}
			props["fqdn"] = fmt.Sprintf("%s-%s.hcp.%s.nimbus.localhost",
				strings.ToLower(dns), shortHash(t.ID), str(body["location"]))

			version := str(props["kubernetesVersion"])
			if version == "" {
				version = s.cfg.KubernetesVersion
			}
			props["kubernetesVersion"] = version
			props["currentKubernetesVersion"] = version

			// Azure creates a second resource group holding the cluster's
			// infrastructure, and azurerm reads its name back.
			if str(props["nodeResourceGroup"]) == "" {
				props["nodeResourceGroup"] = fmt.Sprintf("MC_%s_%s_%s",
					t.ResourceGroup, t.Name, str(body["location"]))
			}

			keepServerFields(props, existing)
			normaliseAgentPools(props)

			if body["identity"] == nil {
				body["identity"] = map[string]any{
					"type":        "SystemAssigned",
					"principalId": stableGUID(t.ID + "/principal"),
					"tenantId":    s.cfg.TenantID,
				}
			}
			if props["identityProfile"] == nil {
				props["identityProfile"] = map[string]any{
					"kubeletidentity": map[string]any{
						"clientId":   stableGUID(t.ID + "/kubelet-client"),
						"objectId":   stableGUID(t.ID + "/kubelet-object"),
						"resourceId": props["nodeResourceGroup"],
					},
				}
			}
			setDefault(props, "enableRBAC", true)
			setDefault(props, "supportPlan", "KubernetesOfficial")
			setDefault(props, "networkProfile", map[string]any{
				"networkPlugin": "kubenet", "loadBalancerSku": "standard",
				"serviceCidr": "10.0.0.0/16", "dnsServiceIP": "10.0.0.10",
				"podCidr": "10.244.0.0/16", "outboundType": "loadBalancer",
			})
			return nil
		},

		AfterPut: func(s *Server, t target, body map[string]any) {
			// The node resource group is a real resource group in Azure, and
			// `az aks` and some modules look it up.
			props, _ := body["properties"].(map[string]any)
			s.ensureNodeResourceGroup(t, str(props["nodeResourceGroup"]), str(body["location"]))
		},

		// Provision runs in the background: the PUT has already been answered
		// with provisioningState Creating and an operation to poll.
		Provision: func(s *Server, t target, body map[string]any) error {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout+time.Minute)
			defer cancel()

			cl := s.clusters.get(t.ID)
			if cl == nil {
				port, err := freePort(s.cfg.ClusterPortBase)
				if err != nil {
					return err
				}
				cl = &cluster{
					ResourceID: t.ID, Name: t.Name,
					Container: containerName(t.ID, t.Name),
					Network:   containerName(t.ID, t.Name) + "-net",
					Port:      port,
				}
				s.clusters.put(cl)
			}

			if err := s.startCluster(ctx, cl); err != nil {
				s.clusters.remove(t.ID)
				return err
			}

			props, _ := body["properties"].(map[string]any)
			want := totalNodeCount(props)
			if err := s.scaleAgents(ctx, cl, want); err != nil {
				return err
			}
			s.recordClusterEndpoint(t, cl)
			return nil
		},

		Deprovision: func(s *Server, t target) {
			cl := s.clusters.remove(t.ID)
			if cl == nil {
				// Nothing registered -- it may predate this process. Remove by
				// the name we would have used, so a restart still cleans up.
				cl = &cluster{Container: containerName(t.ID, t.Name), Network: containerName(t.ID, t.Name) + "-net"}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.stopCluster(ctx, cl)
		},
	})

	// Agent pools are children of the cluster; scaling one changes how many
	// agent containers run.
	s.registerHook(typeAgentPool, Hook{
		Backing: "real",
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props, _ := body["properties"].(map[string]any)
			if props == nil {
				props = map[string]any{}
				body["properties"] = props
			}
			setDefault(props, "count", float64(1))
			setDefault(props, "vmSize", "Standard_DS2_v2")
			setDefault(props, "osType", "Linux")
			setDefault(props, "mode", "System")
			setDefault(props, "orchestratorVersion", s.cfg.KubernetesVersion)
			setDefault(props, "currentOrchestratorVersion", s.cfg.KubernetesVersion)
			return nil
		},
		Provision: func(s *Server, t target, body map[string]any) error {
			cl := s.clusters.get(t.ParentID)
			if cl == nil {
				return fmt.Errorf("the cluster for agent pool %q is not running", t.Name)
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
			defer cancel()
			return s.scaleAgents(ctx, cl, s.desiredNodeCount(t.ParentID))
		},
	})
}

// --- cluster helpers --------------------------------------------------------

// ensureNodeResourceGroup creates the MC_* resource group Azure makes
// alongside a cluster.
func (s *Server) ensureNodeResourceGroup(t target, name, location string) {
	if name == "" {
		return
	}
	id := "/subscriptions/" + t.Subscription + "/resourceGroups/" + name
	if s.store.Exists(id) {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "name": name, "type": "Microsoft.Resources/resourceGroups",
		"location":   location,
		"properties": map[string]any{"provisioningState": "Succeeded"},
		"tags":       map[string]any{"nimbus-managed-by": t.ID},
	})
	s.store.Put(&core.Resource{
		Cloud: "azure", ID: id, Type: "Microsoft.Resources/resourceGroups",
		Name: name, ParentID: "/subscriptions/" + t.Subscription,
		Location: location, Backing: "real", Body: body,
	})
}

// recordClusterEndpoint writes the live connection details onto the stored
// resource once the cluster is up, so the inspector shows them.
func (s *Server) recordClusterEndpoint(t target, cl *cluster) {
	res, err := s.store.Get(t.ID)
	if err != nil {
		return
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		return
	}
	props["nimbusEndpoint"] = fmt.Sprintf("https://127.0.0.1:%d", cl.Port)
	props["nimbusContainer"] = cl.Container
	res.Body, _ = json.Marshal(body)
	s.store.Put(res)
}

// normaliseAgentPools fills the fields azurerm reads back from each pool.
func normaliseAgentPools(props map[string]any) {
	pools := asList(props["agentPoolProfiles"])
	for _, raw := range pools {
		p, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		setDefault(p, "count", float64(1))
		setDefault(p, "vmSize", "Standard_DS2_v2")
		setDefault(p, "osType", "Linux")
		setDefault(p, "osSKU", "Ubuntu")
		setDefault(p, "mode", "System")
		setDefault(p, "type", "VirtualMachineScaleSets")
		setDefault(p, "maxPods", float64(110))
		setDefault(p, "provisioningState", "Succeeded")
	}
	if pools == nil {
		props["agentPoolProfiles"] = []any{}
	}
}

// totalNodeCount adds up every pool's count.
func totalNodeCount(props map[string]any) int {
	total := 0
	for _, raw := range asList(props["agentPoolProfiles"]) {
		if p, ok := raw.(map[string]any); ok {
			if n, ok := p["count"].(float64); ok {
				total += int(n)
			}
		}
	}
	if total <= 0 {
		total = 1
	}
	return total
}

// desiredNodeCount re-reads the stored cluster to find the current total.
func (s *Server) desiredNodeCount(clusterID string) int {
	res, err := s.store.Get(clusterID)
	if err != nil {
		return 1
	}
	var body map[string]any
	if json.Unmarshal(res.Body, &body) != nil {
		return 1
	}
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		return 1
	}
	return totalNodeCount(props)
}

// scaleAgents brings the number of agent containers to want-1: the k3s server
// container is itself a node, so a cluster of N nodes needs N-1 agents.
func (s *Server) scaleAgents(ctx context.Context, cl *cluster, want int) error {
	if want < 1 {
		want = 1
	}
	target := want - 1

	for len(cl.Agents) > target {
		last := cl.Agents[len(cl.Agents)-1]
		if err := s.docker.Remove(ctx, last); err != nil {
			return fmt.Errorf("removing agent %s: %w", last, err)
		}
		cl.Agents = cl.Agents[:len(cl.Agents)-1]
	}

	for len(cl.Agents) < target {
		name := fmt.Sprintf("%s-agent-%d", cl.Container, len(cl.Agents)+1)
		token, err := s.docker.Exec(ctx, cl.Container, "cat", "/var/lib/rancher/k3s/server/node-token")
		if err != nil {
			return fmt.Errorf("reading the cluster join token: %w", err)
		}
		if _, err := s.docker.Run(ctx, docker.RunOptions{
			Name:       name,
			Image:      s.cfg.K3sImage,
			Privileged: true,
			Network:    cl.Network,
			Labels: map[string]string{
				labelCluster: cl.Name, labelResource: cl.ResourceID, labelRole: "agent",
			},
			Env: map[string]string{
				// Agents reach the server by container name on the shared
				// network, so no port publishing is needed.
				"K3S_URL":   fmt.Sprintf("https://%s:6443", cl.Container),
				"K3S_TOKEN": strings.TrimSpace(string(token)),
			},
			Args:  []string{"agent"},
			TmpFS: []string{"/run", "/var/run"},
		}); err != nil {
			return fmt.Errorf("starting agent %s: %w", name, err)
		}
		cl.Agents = append(cl.Agents, name)
	}
	return nil
}

// --- credential actions -----------------------------------------------------

// clusterAction answers the credential POSTs.
//
// azurerm calls these on EVERY refresh, so they must be fast and return the
// same bytes each time. The kubeconfig is built once when the cluster becomes
// ready and then handed back verbatim.
func (s *Server) clusterAction(t target, _ map[string]any) (any, bool) {
	res, err := s.store.Get(t.ID)
	if err != nil || !eq(res.Type, typeManagedCluster) {
		return nil, false
	}
	switch strings.ToLower(t.Action) {
	case "listclusterusercredential", "listclusteradmincredential", "listclustermonitoringusercredential":
	default:
		return nil, false
	}

	cl := s.clusters.get(t.ID)
	if cl == nil || len(cl.Kubeconfig) == 0 {
		// Credentials before the cluster is ready: Azure answers with an
		// error rather than an empty kubeconfig, which would otherwise be
		// written to a file and fail confusingly later.
		return map[string]any{"error": map[string]any{
			"code":    "ClusterNotReady",
			"message": "The cluster is still provisioning and has no credentials yet.",
		}}, true
	}

	name := "clusterUser"
	if strings.EqualFold(t.Action, "listClusterAdminCredential") {
		name = "clusterAdmin"
	}
	return map[string]any{"kubeconfigs": []any{map[string]any{
		"name":  name,
		"value": base64.StdEncoding.EncodeToString(cl.Kubeconfig),
	}}}, true
}

// --- reattaching after a restart -------------------------------------------

// reattachClusters finds containers from a previous run and adopts them.
//
// With --persist the containers are deliberately left running so a restart is
// quick. Without reattaching, Nimbus would not know about them: a later delete
// would leave them running forever, and a cluster stored as "Creating" would
// stay that way, which is the state that actually matters -- Terraform reads
// the resource, not the old operation.
func (s *Server) reattachClusters(ctx context.Context) {
	if err := s.docker.Available(ctx); err != nil {
		return // no Docker, nothing to reattach to
	}
	found, err := s.docker.List(ctx, labelRole+"=server")
	if err != nil {
		return
	}
	for _, c := range found {
		id := c.Labels[labelResource]
		if id == "" || !s.store.Exists(id) {
			continue
		}
		port := publishedPort(c)
		if port == 0 || !c.Running {
			s.setProvisioningState(id, "Failed")
			continue
		}
		cl := &cluster{
			ResourceID: id, Name: c.Labels[labelCluster],
			Container: c.Name, Network: c.Name + "-net", Port: port,
		}
		if agents, err := s.docker.List(ctx, labelRole+"=agent"); err == nil {
			for _, a := range agents {
				if strings.EqualFold(a.Labels[labelResource], id) {
					cl.Agents = append(cl.Agents, a.Name)
				}
			}
		}
		s.clusters.put(cl)

		// Rebuild the kubeconfig from the container's own certificates. They
		// have not changed, so it is byte-identical to the one handed out
		// before the restart -- which is what keeps the plan clean.
		if err := s.waitForCluster(ctx, cl); err != nil {
			s.setProvisioningState(id, "Failed")
			continue
		}
		s.setProvisioningState(id, "Succeeded")
	}
}

func publishedPort(c docker.Container) int {
	hostPort := c.Ports["6443/tcp"]
	if i := strings.LastIndex(hostPort, ":"); i >= 0 {
		var p int
		if _, err := fmt.Sscanf(hostPort[i+1:], "%d", &p); err == nil {
			return p
		}
	}
	return 0
}

// shortHash is a stable 8-character tag derived from a string, for names that
// must be unique but readable.
func shortHash(s string) string { return stableHex(s, 4) }

// stableGUID derives a deterministic GUID from a string, so identity IDs stay
// the same across restarts -- azurerm stores them and compares on refresh.
func stableGUID(s string) string {
	h := stableHex(s, 16)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}
