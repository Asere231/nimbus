package k3s

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
// deployments, replica sets, pods and services into the resource store as
// children of whatever resource owns the cluster -- an AKS cluster, an EKS
// cluster, the engine does not care. The existing event bus then streams them to the browser, so
// the visualizer needs no Kubernetes knowledge at all -- it draws resources,
// and these are just more resources.
//
// Polling rather than watching: the Kubernetes watch API is a long-lived
// chunked stream with resourceVersion bookkeeping and re-sync semantics, and
// a poll every couple of seconds is plenty for something a person is looking
// at. The cost is one request per collection per interval against a local
// container.

// Types used for the mirrored objects. No cloud calls a pod one of its own
// resources, so these sit under a Nimbus namespace to make that obvious in
// the inspector -- and being cloud-neutral is also what lets the Kubernetes
// view draw an EKS cluster with no new code.
const (
	typeK8sNode       = "Nimbus.Kubernetes/nodes"
	typeK8sDeployment = "Nimbus.Kubernetes/deployments"
	typeK8sReplicaSet = "Nimbus.Kubernetes/replicasets"
	typeK8sPod        = "Nimbus.Kubernetes/pods"
	typeK8sService    = "Nimbus.Kubernetes/services"
)

// watchInterval is how often each running cluster is polled.
const watchInterval = 3 * time.Second

// startClusterWatcher polls every running cluster until ctx is cancelled.
func (e *Engine) Watch(ctx context.Context) {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, cl := range e.clusters.all() {
				e.sync(ctx, cl)
			}
		}
	}
}

// k8sObject is the slice of a Kubernetes object the visualizer needs.
type k8sObject struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		NodeName  string `json:"nodeName"`
		Replicas  *int   `json:"replicas"`
		ClusterIP string `json:"clusterIP"`
		Type      string `json:"type"`
		Ports     []struct {
			Port int `json:"port"`
		} `json:"ports"`
		// Deliberately raw: the SAME field is two different shapes. A
		// Service's spec.selector is a plain map of labels, while a
		// Deployment's or ReplicaSet's is a LabelSelector object with the
		// labels one level down in matchLabels. Decoding both into one Go
		// type fails for whichever shape it is not, silently leaving the
		// selector empty -- and with no selector, no pod is drawn behind
		// its service.
		Selector json.RawMessage `json:"selector"`
	} `json:"spec"`
	Status struct {
		Phase             string `json:"phase"`
		ReadyReplicas     int    `json:"readyReplicas"`
		AvailableReplicas int    `json:"availableReplicas"`
		Conditions        []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			RestartCount int `json:"restartCount"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// selectorOf returns the labels an object selects on, whichever of the two
// shapes Kubernetes used for them.
func selectorOf(typ string, raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	if typ == typeK8sService {
		var plain map[string]string
		if err := json.Unmarshal(raw, &plain); err != nil {
			return nil
		}
		return plain
	}
	var labelSelector struct {
		MatchLabels map[string]string `json:"matchLabels"`
	}
	if err := json.Unmarshal(raw, &labelSelector); err != nil {
		return nil
	}
	return labelSelector.MatchLabels
}

// restartsOf sums the restarts of every container in a pod, which is the
// number kubectl prints in its RESTARTS column.
func restartsOf(item k8sObject) int {
	total := 0
	for _, c := range item.Status.ContainerStatuses {
		total += c.RestartCount
	}
	return total
}

// owner reports what created this object: a ReplicaSet for a pod, a
// Deployment for a replica set. It is how the Kubernetes view nests them,
// rather than guessing from the name prefix.
func owner(item k8sObject) (kind, name string) {
	if len(item.Metadata.OwnerReferences) == 0 {
		return "", ""
	}
	first := item.Metadata.OwnerReferences[0]
	return first.Kind, first.Name
}

// k8sBody is the stored representation of one mirrored object.
func k8sBody(typ, id, name string, item k8sObject) map[string]any {
	ns := item.Metadata.Namespace
	status := objectStatus(typ, item)
	ownerKind, ownerName := owner(item)
	props := map[string]any{
		"namespace": ns,
		"status":    status,
		"node":      item.Spec.NodeName,
		"clusterIP": item.Spec.ClusterIP,
		"labels":    item.Metadata.Labels,
		"selector":  selectorOf(typ, item.Spec.Selector),
		"ownerKind": ownerKind,
		"ownerName": ownerName,
	}
	if typ == typeK8sPod {
		props["restarts"] = restartsOf(item)
	}
	if item.Spec.Replicas != nil {
		props["replicas"] = *item.Spec.Replicas
	}
	if len(item.Spec.Ports) > 0 {
		ports := make([]int, 0, len(item.Spec.Ports))
		for _, p := range item.Spec.Ports {
			ports = append(ports, p.Port)
		}
		props["ports"] = ports
	}
	return map[string]any{
		"id": id, "name": name, "type": typ,
		"namespace":  ns,
		"status":     status,
		"properties": props,
	}
}

// k8sList is the shape of a Kubernetes collection response.
type k8sList struct {
	Items []k8sObject `json:"items"`
}

// sync mirrors one cluster's contents into the resource store.
func (e *Engine) sync(ctx context.Context, cl *Cluster) {
	client := e.APIClient(cl)
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
		{"/apis/apps/v1/replicasets", typeK8sReplicaSet},
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

			body, _ := json.Marshal(k8sBody(c.typ, id, name, item))
			// Put is idempotent and only publishes an event when something
			// changed, so re-storing an unchanged pod every few seconds does
			// not spam the activity feed.
			e.putIfChanged(&core.Resource{
				Cloud: e.cloud, ID: id, Type: c.typ, Name: name,
				ParentID: cl.ResourceID, Backing: "real", Body: body,
			})
		}
	}

	// Anything previously mirrored for this cluster that is gone now.
	prefix := strings.ToLower(cl.ResourceID + "/kubernetes/")
	for _, res := range e.store.List(func(r *core.Resource) bool {
		return strings.HasPrefix(strings.ToLower(r.ID), prefix)
	}) {
		if !seen[strings.ToLower(res.ID)] {
			_, _ = e.store.Delete(res.ID)
		}
	}
}

// putIfChanged avoids republishing identical objects, which would otherwise
// fill the activity feed with an update per pod every poll.
func (e *Engine) putIfChanged(r *core.Resource) {
	if existing, err := e.store.Get(r.ID); err == nil {
		if string(existing.Body) == string(r.Body) {
			return
		}
	}
	e.store.Put(r)
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
	case typeK8sDeployment, typeK8sReplicaSet:
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
func (e *Engine) APIClient(cl *Cluster) *http.Client {
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
