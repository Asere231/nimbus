package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

// Watching what is actually running inside a cluster.
//
// Once a cluster is up, Nimbus polls its Kubernetes API and mirrors nodes,
// deployments, pods and services into the resource store as children of the
// AKS resource. The existing event bus then streams them to the browser, so
// the visualizer needs no Kubernetes knowledge at all -- it draws resources,
// and these are just more resources.
//
// Polling rather than watching: the Kubernetes watch API is a long-lived
// chunked stream with resourceVersion bookkeeping and re-sync semantics, and
// a poll every couple of seconds is plenty for something a person is looking
// at. The cost is one request per collection per interval against a local
// container.

// Types used for the mirrored objects. They are not real ARM types -- nothing
// in Azure calls a pod a resource -- so they sit under a Nimbus namespace to
// make that obvious in the inspector.
const (
	typeK8sNode       = "Nimbus.Kubernetes/nodes"
	typeK8sDeployment = "Nimbus.Kubernetes/deployments"
	typeK8sPod        = "Nimbus.Kubernetes/pods"
	typeK8sService    = "Nimbus.Kubernetes/services"
)

// watchInterval is how often each running cluster is polled.
const watchInterval = 3 * time.Second

// startClusterWatcher polls every running cluster until ctx is cancelled.
func (s *Server) startClusterWatcher(ctx context.Context) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, cl := range s.clusters.all() {
				s.syncCluster(ctx, cl)
			}
		}
	}
}

// k8sObject is the slice of a Kubernetes object the visualizer needs.
type k8sObject struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		UID       string            `json:"uid"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		NodeName  string `json:"nodeName"`
		Replicas  *int   `json:"replicas"`
		ClusterIP string `json:"clusterIP"`
		Type      string `json:"type"`
		Ports     []struct {
			Port int `json:"port"`
		} `json:"ports"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		ReadyReplicas     int    `json:"readyReplicas"`
		AvailableReplicas int    `json:"availableReplicas"`
		Conditions        []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// k8sList is the shape of a Kubernetes collection response.
type k8sList struct {
	Items []k8sObject `json:"items"`
}

// syncCluster mirrors one cluster's contents into the resource store.
func (s *Server) syncCluster(ctx context.Context, cl *cluster) {
	client := s.clusterClient(cl)
	if client == nil {
		return
	}

	type collection struct {
		path string
		typ  string
	}
	// Only the kinds worth drawing. Everything k3s runs in kube-system is
	// filtered below, or the graph would be mostly infrastructure.
	wanted := []collection{
		{"/api/v1/nodes", typeK8sNode},
		{"/apis/apps/v1/deployments", typeK8sDeployment},
		{"/api/v1/pods", typeK8sPod},
		{"/api/v1/services", typeK8sService},
	}

	seen := map[string]bool{}
	for _, c := range wanted {
		list, err := fetchList(ctx, client, cl.Port, c.path)
		if err != nil {
			// A cluster that has gone away stops being polled; its resources
			// are cleaned up when the ARM resource is deleted.
			return
		}
		for _, item := range list.Items {
			ns := item.Metadata.Namespace
			if isSystemNamespace(ns) {
				continue
			}
			name := item.Metadata.Name
			id := fmt.Sprintf("%s/kubernetes/%s/%s", cl.ResourceID, strings.TrimPrefix(c.typ, "Nimbus.Kubernetes/"), name)
			if ns != "" {
				id = fmt.Sprintf("%s/kubernetes/%s/%s/%s", cl.ResourceID,
					strings.TrimPrefix(c.typ, "Nimbus.Kubernetes/"), ns, name)
			}
			seen[strings.ToLower(id)] = true

			body, _ := json.Marshal(map[string]any{
				"id": id, "name": name, "type": c.typ,
				"namespace": ns,
				"status":    objectStatus(c.typ, item),
				"properties": map[string]any{
					"namespace": ns,
					"status":    objectStatus(c.typ, item),
					"node":      item.Spec.NodeName,
					"clusterIP": item.Spec.ClusterIP,
					"labels":    item.Metadata.Labels,
				},
			})
			// Put is idempotent and only publishes an event when something
			// changed, so re-storing an unchanged pod every few seconds does
			// not spam the activity feed.
			s.putIfChanged(&core.Resource{
				Cloud: "azure", ID: id, Type: c.typ, Name: name,
				ParentID: cl.ResourceID, Backing: "real", Body: body,
			})
		}
	}

	// Anything previously mirrored for this cluster that is gone now.
	prefix := strings.ToLower(cl.ResourceID + "/kubernetes/")
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return strings.HasPrefix(strings.ToLower(r.ID), prefix)
	}) {
		if !seen[strings.ToLower(res.ID)] {
			_, _ = s.store.Delete(res.ID)
		}
	}
}

// putIfChanged avoids republishing identical objects, which would otherwise
// fill the activity feed with an update per pod every poll.
func (s *Server) putIfChanged(r *core.Resource) {
	if existing, err := s.store.Get(r.ID); err == nil {
		if string(existing.Body) == string(r.Body) {
			return
		}
	}
	s.store.Put(r)
}

// isSystemNamespace hides the machinery k3s runs for itself.
func isSystemNamespace(ns string) bool {
	switch ns {
	case "kube-system", "kube-public", "kube-node-lease":
		return true
	}
	return false
}

// objectStatus reduces each kind to the one word the visualizer colours by.
func objectStatus(typ string, item k8sObject) string {
	switch typ {
	case typeK8sPod:
		return item.Status.Phase // Running, Pending, Succeeded, Failed
	case typeK8sNode:
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" {
				if c.Status == "True" {
					return "Ready"
				}
				return "NotReady"
			}
		}
		return "Unknown"
	case typeK8sDeployment:
		want := 1
		if item.Spec.Replicas != nil {
			want = *item.Spec.Replicas
		}
		return fmt.Sprintf("%d/%d ready", item.Status.ReadyReplicas, want)
	case typeK8sService:
		return item.Spec.Type
	}
	return ""
}

// clusterClient builds (and does not cache) an HTTPS client for a cluster.
func (s *Server) clusterClient(cl *cluster) *http.Client {
	// The PEM blobs are kept on the cluster when it becomes ready, so nothing
	// has to read them back out of the kubeconfig -- parsing YAML is the one
	// thing the stdlib cannot do, and generating it was the easy direction.
	if len(cl.CA) == 0 || len(cl.Cert) == 0 || len(cl.Key) == 0 {
		return nil
	}
	client, err := apiClient(cl.CA, cl.Cert, cl.Key)
	if err != nil {
		return nil
	}
	return client
}

func fetchList(ctx context.Context, client *http.Client, port int, path string) (*k8sList, error) {
	url := fmt.Sprintf("https://127.0.0.1:%d%s", port, path)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", path, res.Status)
	}
	var list k8sList
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}
