package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// Teaching a cluster to pull from an emulated registry.
//
// A registry's login server is `localhost:<port>`, which is exactly right on
// the machine and useless inside the cluster: there, localhost is the k3s
// container. So each node gets a containerd MIRROR that rewrites that host to
// the registry container, reachable by name once it has joined the cluster's
// Docker network.
//
// The mirror is written two ways, because which one takes effect depends on
// the k3s build:
//
//   - certs.d/<host>/hosts.toml under the agent's containerd directory, which
//     containerd re-reads on the next pull when its config sets config_path.
//     This is the path that works for a registry created AFTER the cluster,
//     which Terraform does routinely -- nothing says ACR is applied first.
//   - /etc/rancher/k3s/registries.yaml, which k3s reads only at startup. It
//     costs one more file and covers the case where config_path is not set,
//     from the next restart onwards.
//
// Writing both is cheap. Pretending the first always works would produce a
// pull failure inside the cluster that says nothing about Nimbus.

const certsDir = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"
const containerdConfig = "/var/lib/rancher/k3s/agent/etc/containerd/config.toml"

// configureRegistryMirrors writes every known registry's mirror into every
// node of one cluster.
func (s *Server) configureRegistryMirrors(ctx context.Context, cl *cluster) {
	regs := s.registries.all()
	if len(regs) == 0 {
		return
	}
	for _, reg := range regs {
		s.mirrorRegistryInCluster(ctx, cl, reg)
	}
}

// announceRegistryToClusters does the same for one new registry across every
// cluster that already exists.
func (s *Server) announceRegistryToClusters(ctx context.Context, reg *registry) {
	for _, cl := range s.clusters.all() {
		s.mirrorRegistryInCluster(ctx, cl, reg)
	}
}

func (s *Server) mirrorRegistryInCluster(ctx context.Context, cl *cluster, reg *registry) {
	// The registry has to be ON the cluster's network before its name
	// resolves from inside the nodes.
	if err := s.docker.ConnectNetwork(ctx, cl.Network, reg.Container); err != nil {
		s.bus.Publish(core.Event{
			Kind: "resource", Action: "update", Cloud: "azure",
			ResourceID: cl.ResourceID, ResourceType: typeManagedCluster, Name: cl.Name,
			Note: fmt.Sprintf("could not attach registry %s to the cluster network: %v", reg.Name, err),
		})
		return
	}

	endpoint := fmt.Sprintf("http://%s:5000", reg.Container)
	hosts := fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true\n",
		endpoint, endpoint)
	yaml := fmt.Sprintf("mirrors:\n  %q:\n    endpoint:\n      - %q\n", reg.LoginServer(), endpoint)

	for _, node := range append([]string{cl.Container}, cl.Agents...) {
		// certs.d is per registry host, and the directory name is the host
		// exactly as the image reference writes it, port and all.
		dir := fmt.Sprintf("%s/%s", certsDir, reg.LoginServer())
		if err := s.writeInContainer(ctx, node, dir+"/hosts.toml", hosts); err != nil {
			s.bus.Publish(core.Event{
				Kind: "resource", Action: "update", Cloud: "azure",
				ResourceID: cl.ResourceID, ResourceType: typeManagedCluster, Name: cl.Name,
				Note: fmt.Sprintf("could not write the registry mirror on %s: %v", node, err),
			})
			continue
		}
		_ = s.writeInContainer(ctx, node, "/etc/rancher/k3s/registries.yaml", yaml)
	}

	// Say plainly when the live path is not available, rather than letting it
	// surface later as an image pull failure inside the cluster.
	if !s.containerdReadsCertsDir(ctx, cl.Container) {
		s.bus.Publish(core.Event{
			Kind: "resource", Action: "update", Cloud: "azure",
			ResourceID: cl.ResourceID, ResourceType: typeManagedCluster, Name: cl.Name,
			Note: fmt.Sprintf("registry %s mirrored, but this k3s build only reads registry "+
				"configuration at startup -- recreate the cluster to pull from it", reg.Name),
		})
	}
}

// writeInContainer writes a file inside a container without worrying about
// shell quoting: the content goes in base64-encoded and is decoded there.
func (s *Server) writeInContainer(ctx context.Context, container, path, content string) error {
	dir := path[:strings.LastIndex(path, "/")]
	script := fmt.Sprintf("mkdir -p '%s' && echo '%s' | base64 -d > '%s'",
		dir, base64.StdEncoding.EncodeToString([]byte(content)), path)
	_, err := s.docker.Exec(ctx, container, "sh", "-c", script)
	return err
}

// containerdReadsCertsDir reports whether this k3s build points containerd at
// the certs.d directory, which is what makes a mirror take effect without a
// restart.
func (s *Server) containerdReadsCertsDir(ctx context.Context, container string) bool {
	out, err := s.docker.Exec(ctx, container, "sh", "-c", "cat "+containerdConfig)
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "config_path")
}
