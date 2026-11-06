package k3s

import (
	"context"
	"fmt"
	"strings"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// scaleAgents brings the number of agent containers to want-1: the k3s server
// container is itself a node, so a cluster of N nodes needs N-1 agents.
func (e *Engine) ScaleAgents(ctx context.Context, cl *Cluster, want int) error {
	if want < 1 {
		want = 1
	}
	target := want - 1

	for len(cl.Agents) > target {
		last := cl.Agents[len(cl.Agents)-1]
		if err := e.docker.Remove(ctx, last); err != nil {
			return fmt.Errorf("removing agent %s: %w", last, err)
		}
		cl.Agents = cl.Agents[:len(cl.Agents)-1]
	}

	for len(cl.Agents) < target {
		name := fmt.Sprintf("%s-agent-%d", cl.Container, len(cl.Agents)+1)
		token, err := e.docker.Exec(ctx, cl.Container, "cat", "/var/lib/rancher/k3s/server/node-token")
		if err != nil {
			return fmt.Errorf("reading the cluster join token: %w", err)
		}
		if _, err := e.docker.Run(ctx, docker.RunOptions{
			Name:       name,
			Image:      e.cfg.Image,
			Privileged: true,
			Network:    cl.Network,
			Labels: map[string]string{
				docker.LabelCluster: cl.Name, docker.LabelResource: cl.ResourceID, docker.LabelRole: "agent",
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
