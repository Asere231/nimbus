package k3s

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Finding clusters again after Nimbus restarts.
//
// With KeepRunning set, the containers outlive the process, so a fresh
// Nimbus has to adopt them rather than start a second set. Everything it
// needs is on the containers themselves: the resource id and the cluster
// name are labels, and the API port is the published one.
//
// Rebuilding the kubeconfig from the container's own certificates matters
// as much as finding the container. They have not changed, so the
// kubeconfig comes out byte-identical to the one handed out before the
// restart -- which is what keeps a Terraform plan clean. See the comment
// on Cluster.Kubeconfig.

// Found is one adopted cluster and whether it is usable.
type Found struct {
	Cluster *Cluster
	// Ready is false when the container is gone, stopped, or its API
	// never answered. The caller decides what that means for its own
	// resource -- Azure marks the cluster Failed.
	Ready bool
}

// Reattach adopts every cluster this engine's containers describe.
//
// keep decides which resource ids are still wanted; a container whose
// resource has been deleted is left alone rather than adopted, since
// something else will clean it up.
func (e *Engine) Reattach(ctx context.Context, keep func(resourceID string) bool) []Found {
	if err := e.docker.Available(ctx); err != nil {
		return nil // no Docker, nothing to reattach to
	}
	servers, err := e.docker.List(ctx, docker.LabelRole+"=server")
	if err != nil {
		return nil
	}
	agents, _ := e.docker.List(ctx, docker.LabelRole+"=agent")

	var out []Found
	for _, c := range servers {
		id := c.Labels[docker.LabelResource]
		if id == "" || (keep != nil && !keep(id)) {
			continue
		}
		port := publishedPort(c)
		if port == 0 || !c.Running {
			out = append(out, Found{Cluster: &Cluster{ResourceID: id, Name: c.Labels[docker.LabelCluster]}})
			continue
		}
		cl := &Cluster{
			ResourceID: id, Name: c.Labels[docker.LabelCluster],
			Container: c.Name, Network: c.Name + "-net", Port: port,
		}
		for _, a := range agents {
			if strings.EqualFold(a.Labels[docker.LabelResource], id) {
				cl.Agents = append(cl.Agents, a.Name)
			}
		}
		e.clusters.put(cl)
		out = append(out, Found{Cluster: cl, Ready: e.waitReady(ctx, cl) == nil})
	}
	return out
}

// publishedPort is the host port a container's API server is on.
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

// WaitForNodes blocks until the cluster reports at least want nodes.
//
// Waiting for them to REGISTER, not merely for their containers to start:
// a cluster is finished when it is usable, and anyone running kubectl
// straight after `terraform apply` would otherwise see fewer nodes than
// they asked for, or none.
func (e *Engine) WaitForNodes(ctx context.Context, cl *Cluster, want int) error {
	client := e.APIClient(cl)
	if client == nil {
		return fmt.Errorf("no client for cluster %s", cl.Name)
	}
	deadline := time.Now().Add(e.cfg.Timeout)
	var last int
	for {
		list, err := fetchList(ctx, client, cl.Port, "/api/v1/nodes")
		if err == nil {
			last = len(list.Items)
			if last >= want {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("only %d of %d nodes joined within %s; check `docker logs %s`",
				last, want, e.cfg.Timeout, cl.Container)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
