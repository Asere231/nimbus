package azure

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/k3s"
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
				version = s.k3s.Version()
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
				// resourceId must be a FULL UserAssignedIdentity ARM ID.
				// azurerm parses it, and a bare name fails the apply with
				// "the number of segments didn't match", which names neither
				// the field nor the resource. Azure puts this identity in the
				// node resource group, named {cluster}-agentpool.
				props["identityProfile"] = map[string]any{
					"kubeletidentity": map[string]any{
						"clientId": stableGUID(t.ID + "/kubelet-client"),
						"objectId": stableGUID(t.ID + "/kubelet-object"),
						"resourceId": fmt.Sprintf(
							"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ManagedIdentity/userAssignedIdentities/%s-agentpool",
							t.Subscription, str(props["nodeResourceGroup"]), t.Name),
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

			cl, err := s.k3s.NewCluster(t.ID, t.Name)
			if err != nil {
				return err
			}
			if err := s.k3s.Start(ctx, cl); err != nil {
				s.k3s.Remove(t.ID)
				return err
			}

			props, _ := body["properties"].(map[string]any)
			want := totalNodeCount(props)
			if err := s.k3s.ScaleAgents(ctx, cl, want); err != nil {
				return err
			}
			// Wait for the nodes to actually REGISTER, not just for their
			// containers to start. Real AKS finishes provisioning when the
			// cluster is usable, and anyone who runs kubectl straight after
			// `terraform apply` -- which the example tells them to do --
			// would otherwise see fewer nodes than they asked for, or none.
			if err := s.k3s.WaitForNodes(ctx, cl, want); err != nil {
				return err
			}
			// Registries that already exist have to be mirrored into the new
			// cluster's nodes, or a pod referring to one cannot pull.
			s.mirrorRegistries(ctx, cl)
			s.recordClusterEndpoint(t, cl)
			return nil
		},

		// The node resource group is Azure's, not the user's: AKS creates
		// it and AKS removes it. Synchronously here rather than in
		// Deprovision, because it is bookkeeping rather than engine work --
		// so a destroy that has returned has really taken it with it.
		AfterDelete: func(s *Server, t target, body map[string]any) {
			s.deleteNodeResourceGroup(t, body)
		},

		Deprovision: func(s *Server, t target) {
			cl := s.k3s.Remove(t.ID)
			if cl == nil {
				// Nothing registered -- it may predate this process. Remove by
				// the name we would have used, so a restart still cleans up.
				cl = s.k3s.ClusterFor(t.ID, t.Name)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.k3s.Stop(ctx, cl)
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
			setDefault(props, "orchestratorVersion", s.k3s.Version())
			setDefault(props, "currentOrchestratorVersion", s.k3s.Version())
			return nil
		},
		AfterPut: func(s *Server, t target, body map[string]any) {
			// Mirror the pool into the cluster's embedded agentPoolProfiles.
			// azurerm scales by writing the CHILD, then reads the count back
			// off the PARENT on the next refresh.
			s.updateClusterPools(t.ParentID, t.Name, body)
		},
		AfterDelete: func(s *Server, t target, body map[string]any) {
			s.updateClusterPools(t.ParentID, t.Name, nil)
		},
		Provision: func(s *Server, t target, body map[string]any) error {
			cl := s.k3s.Get(t.ParentID)
			if cl == nil {
				return fmt.Errorf("the cluster for agent pool %q is not running", t.Name)
			}
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
			defer cancel()
			if err := s.k3s.ScaleAgents(ctx, cl, s.desiredNodeCount(t.ParentID)); err != nil {
				return err
			}
			// New agents start with no registry configuration of their own.
			s.mirrorRegistries(ctx, cl)
			return nil
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
func (s *Server) recordClusterEndpoint(t target, cl *k3s.Cluster) {
	// store.Update, not Get-then-Put: an agent pool's AfterPut writes to this
	// same body, and two read-modify-writes that overlap lose one of the two
	// changes. See the comment on core.Store.Update.
	_ = s.store.Update(t.ID, func(body map[string]any) error {
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			return errNoChange
		}
		props["nimbusEndpoint"] = fmt.Sprintf("https://127.0.0.1:%d", cl.Port)
		props["nimbusContainer"] = cl.Container
		return nil
	})
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

// updateClusterPools rewrites one entry of a cluster's agentPoolProfiles.
// Passing a nil body removes the pool.
//
// Written directly to the store rather than through the ARM put path, because
// re-running the cluster's own hooks here would re-provision the cluster on
// every pool change.
func (s *Server) updateClusterPools(clusterID, poolName string, poolBody map[string]any) {
	// store.Update, not Get-then-Put: the cluster's own background Provision
	// writes to this same body, and two read-modify-writes that overlap lose
	// one of the two changes -- here, the new node count. See the comment on
	// core.Store.Update.
	_ = s.store.Update(clusterID, func(body map[string]any) error {
		props, _ := body["properties"].(map[string]any)
		if props == nil {
			return errNoChange
		}

		// The cluster's profile is the pool's properties plus its name, which
		// is how Azure reports it.
		var entry map[string]any
		if poolBody != nil {
			entry = map[string]any{"name": poolName}
			if p, ok := poolBody["properties"].(map[string]any); ok {
				for k, v := range p {
					entry[k] = v
				}
			}
		}

		out := []any{}
		replaced := false
		for _, raw := range asList(props["agentPoolProfiles"]) {
			p, ok := raw.(map[string]any)
			if ok && eq(str(p["name"]), poolName) {
				if entry != nil {
					out = append(out, entry)
				}
				replaced = true
				continue
			}
			out = append(out, raw)
		}
		if !replaced && entry != nil {
			out = append(out, entry)
		}
		props["agentPoolProfiles"] = out
		normaliseAgentPools(props)
		return nil
	})
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

	cl := s.k3s.Get(t.ID)
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

// shortHash is a stable 8-character tag derived from a string, for names that
// must be unique but readable.
func shortHash(s string) string { return k3s.StableHex(s, 4) }

// stableGUID derives a deterministic GUID from a string, so identity IDs stay
// the same across restarts -- azurerm stores them and compares on refresh.
func stableGUID(s string) string {
	h := k3s.StableHex(s, 16)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// deleteNodeResourceGroup removes the MC_* group a cluster created.
//
// It cannot happen by cascade, and that is structural rather than an
// oversight: a resource group's ParentID is the SUBSCRIPTION, because that
// is where one really lives. Store.Delete reaches descendants by ID prefix
// or by ParentID, and the MC_ group is neither a prefix of the cluster's id
// nor a child of it -- so no cascade can ever find it. It has to be deleted
// by name, by the thing that made it.
//
// Two ways to find it, because the first can be missing. The cluster's own
// body carries properties.nodeResourceGroup, which is exact; a body that
// lost the field -- or a group left by an earlier run -- is found by the
// nimbus-managed-by tag ensureNodeResourceGroup puts on it. Deleting the
// group cascades to what is inside it, which is where AKS puts the
// {cluster}-agentpool identity.
func (s *Server) deleteNodeResourceGroup(t target, body map[string]any) {
	prefix := "/subscriptions/" + t.Subscription + "/resourceGroups/"

	if props, ok := body["properties"].(map[string]any); ok {
		if name := str(props["nodeResourceGroup"]); name != "" {
			if _, err := s.store.Delete(prefix + name); err == nil {
				return
			}
		}
	}

	// No name to go on: find the group this cluster tagged as its own.
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Cloud == "azure" && r.Type == "Microsoft.Resources/resourceGroups"
	}) {
		var stored struct {
			Tags map[string]string `json:"tags"`
		}
		if json.Unmarshal(res.Body, &stored) != nil {
			continue
		}
		if strings.EqualFold(stored.Tags["nimbus-managed-by"], t.ID) {
			_, _ = s.store.Delete(res.ID)
			return
		}
	}
}
