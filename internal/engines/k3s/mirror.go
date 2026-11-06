package k3s

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
)

// Teaching a cluster to pull from a registry that only exists locally.
//
// An emulated registry's login server is `localhost:<port>`, which is
// exactly right on the machine and useless inside the cluster: there,
// localhost is the k3s container. So each node gets a containerd MIRROR
// that rewrites that host to the registry container, reachable by name
// once it has joined the cluster's Docker network.
//
// The mirror is written two ways, because which one takes effect depends
// on the k3s build:
//
//   - certs.d/<host>/hosts.toml under the agent's containerd directory,
//     which containerd re-reads on the next pull when its config sets
//     config_path. This is the path that works for a registry created
//     AFTER the cluster, which Terraform does routinely -- nothing says
//     the registry is applied first.
//   - /etc/rancher/k3s/registries.yaml, which k3s reads only at startup.
//     It costs one more file and covers the case where config_path is not
//     set, from the next restart onwards.
//
// Writing both is cheap. Pretending the first always works would produce a
// pull failure inside the cluster that says nothing about Nimbus.

// Exported because the clouds' own tests assert that a mirror really
// lands in these paths.
const (
	CertsDir         = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"
	ContainerdConfig = "/var/lib/rancher/k3s/agent/etc/containerd/config.toml"
)

// MirrorRegistry points every node of a cluster at a registry container.
//
// host is what image references say (`localhost:5001`); container is the
// registry's Docker name, which is how the nodes will actually reach it.
// Warnings come back rather than going to an event bus, because what to
// call a cluster and which cloud it belongs to are the caller's business.
func (e *Engine) MirrorRegistry(ctx context.Context, cl *Cluster, host, container string) []string {
	var warnings []string

	// The registry has to be ON the cluster's network before its name
	// resolves from inside the nodes.
	if err := e.docker.ConnectNetwork(ctx, cl.Network, container); err != nil {
		return []string{fmt.Sprintf("could not attach registry %s to the cluster network: %v", host, err)}
	}

	endpoint := fmt.Sprintf("http://%s:5000", container)
	hosts := fmt.Sprintf("server = %q\n\n[host.%q]\n  capabilities = [\"pull\", \"resolve\"]\n  skip_verify = true\n",
		endpoint, endpoint)
	yaml := fmt.Sprintf("mirrors:\n  %q:\n    endpoint:\n      - %q\n", host, endpoint)

	for _, node := range append([]string{cl.Container}, cl.Agents...) {
		// certs.d is per registry host, and the directory name is the host
		// exactly as the image reference writes it, port and all.
		dir := fmt.Sprintf("%s/%s", CertsDir, host)
		if err := e.writeInContainer(ctx, node, dir+"/hosts.toml", hosts); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not write the registry mirror on %s: %v", node, err))
			continue
		}
		_ = e.writeInContainer(ctx, node, "/etc/rancher/k3s/registries.yaml", yaml)
	}

	// Say plainly when the live path is not available, rather than letting
	// it surface later as an image pull failure inside the cluster.
	if !e.containerdReadsCertsDir(ctx, cl.Container) {
		warnings = append(warnings, fmt.Sprintf(
			"registry %s mirrored, but this k3s build only reads registry configuration "+
				"at startup -- recreate the cluster to pull from it", host))
	}
	return warnings
}

// writeInContainer writes a file inside a container without worrying about
// shell quoting: the content goes in base64-encoded and is decoded there.
func (e *Engine) writeInContainer(ctx context.Context, container, path, content string) error {
	dir := path[:strings.LastIndex(path, "/")]
	script := fmt.Sprintf("mkdir -p '%s' && echo '%s' | base64 -d > '%s'",
		dir, base64.StdEncoding.EncodeToString([]byte(content)), path)
	_, err := e.docker.Exec(ctx, container, "sh", "-c", script)
	return err
}

// containerdReadsCertsDir reports whether this k3s build points containerd
// at the certs.d directory, which is what makes a mirror take effect
// without a restart.
func (e *Engine) containerdReadsCertsDir(ctx context.Context, container string) bool {
	out, err := e.docker.Exec(ctx, container, "sh", "-c", "cat "+ContainerdConfig)
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "config_path")
}
