package azure

import (
	"encoding/json"
	"testing"
)

// The watcher's parsing, against the real shapes k3s returns. The watcher
// itself needs a running cluster; this is everything it does with what comes
// back, which is where the Kubernetes view's data actually comes from.

func decode(t *testing.T, src string) k8sObject {
	t.Helper()
	var item k8sObject
	if err := json.Unmarshal([]byte(src), &item); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	return item
}

// spec.selector is TWO different shapes depending on the kind: a plain map
// on a Service, a LabelSelector with matchLabels on a Deployment or
// ReplicaSet. One Go type for both silently yields no selector -- and with
// no selector, no pod is drawn behind its service.
func TestSelectorHandlesBothKubernetesShapes(t *testing.T) {
	svc := decode(t, `{
	  "metadata": {"name": "nginx", "namespace": "default"},
	  "spec": {"clusterIP": "10.43.91.17", "type": "ClusterIP",
	           "selector": {"app": "nginx"}, "ports": [{"port": 80}]}
	}`)
	if got := selectorOf(typeK8sService, svc.Spec.Selector); got["app"] != "nginx" {
		t.Errorf("service selector = %v, want app=nginx", got)
	}

	dep := decode(t, `{
	  "metadata": {"name": "nginx", "namespace": "default"},
	  "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "nginx"}}},
	  "status": {"readyReplicas": 2}
	}`)
	if got := selectorOf(typeK8sDeployment, dep.Spec.Selector); got["app"] != "nginx" {
		t.Errorf("deployment selector = %v, want app=nginx from matchLabels", got)
	}

	// A Service decoded with the other rule must come back empty rather
	// than wrong, and neither shape may panic.
	if got := selectorOf(typeK8sDeployment, svc.Spec.Selector); len(got) != 0 {
		t.Errorf("a plain map read as a LabelSelector gave %v, want nothing", got)
	}
	if got := selectorOf(typeK8sService, nil); got != nil {
		t.Errorf("no selector at all = %v, want nil", got)
	}
}

func TestPodBodyCarriesRestartsNodeAndItsOwner(t *testing.T) {
	pod := decode(t, `{
	  "metadata": {
	    "name": "nginx-7d8f6c4b59-k2xqv", "namespace": "default",
	    "labels": {"app": "nginx", "pod-template-hash": "7d8f6c4b59"},
	    "ownerReferences": [{"kind": "ReplicaSet", "name": "nginx-7d8f6c4b59"}]
	  },
	  "spec": {"nodeName": "k3s-agent-1"},
	  "status": {"phase": "Running", "containerStatuses": [{"restartCount": 2}, {"restartCount": 1}]}
	}`)
	body := k8sBody(typeK8sPod, "/id", "nginx-7d8f6c4b59-k2xqv", pod)
	props := body["properties"].(map[string]any)

	if body["status"] != "Running" {
		t.Errorf("status = %v", body["status"])
	}
	if props["restarts"] != 3 {
		t.Errorf("restarts = %v, want 3: every container counts, as kubectl does", props["restarts"])
	}
	if props["node"] != "k3s-agent-1" {
		t.Errorf("node = %v", props["node"])
	}
	if props["ownerKind"] != "ReplicaSet" || props["ownerName"] != "nginx-7d8f6c4b59" {
		t.Errorf("owner = %v/%v, want ReplicaSet/nginx-7d8f6c4b59", props["ownerKind"], props["ownerName"])
	}
	labels, _ := props["labels"].(map[string]string)
	if labels["app"] != "nginx" {
		t.Errorf("labels = %v", props["labels"])
	}
}

func TestReplicaSetBodyNamesItsDeploymentAndItsReadiness(t *testing.T) {
	rs := decode(t, `{
	  "metadata": {
	    "name": "nginx-7d8f6c4b59", "namespace": "default",
	    "ownerReferences": [{"kind": "Deployment", "name": "nginx"}]
	  },
	  "spec": {"replicas": 2, "selector": {"matchLabels": {"app": "nginx", "pod-template-hash": "7d8f6c4b59"}}},
	  "status": {"readyReplicas": 2}
	}`)
	body := k8sBody(typeK8sReplicaSet, "/id", "nginx-7d8f6c4b59", rs)
	props := body["properties"].(map[string]any)
	if body["status"] != "2/2 ready" {
		t.Errorf("status = %v, want 2/2 ready", body["status"])
	}
	if props["ownerName"] != "nginx" {
		t.Errorf("ownerName = %v, want the deployment", props["ownerName"])
	}
	sel, _ := props["selector"].(map[string]string)
	if sel["pod-template-hash"] != "7d8f6c4b59" {
		t.Errorf("selector = %v", props["selector"])
	}
}

func TestServiceBodyCarriesPortsAndClusterIP(t *testing.T) {
	svc := decode(t, `{
	  "metadata": {"name": "nginx", "namespace": "default"},
	  "spec": {"clusterIP": "10.43.91.17", "type": "ClusterIP",
	           "selector": {"app": "nginx"}, "ports": [{"port": 80}, {"port": 443}]}
	}`)
	props := k8sBody(typeK8sService, "/id", "nginx", svc)["properties"].(map[string]any)
	if props["clusterIP"] != "10.43.91.17" {
		t.Errorf("clusterIP = %v", props["clusterIP"])
	}
	ports, _ := props["ports"].([]int)
	if len(ports) != 2 || ports[0] != 80 {
		t.Errorf("ports = %v, want 80 and 443", props["ports"])
	}
}

// An object nothing owns -- a node, a bare pod -- has no owner and must not
// make the body unusable.
func TestAnObjectWithNoOwnerIsFine(t *testing.T) {
	node := decode(t, `{
	  "metadata": {"name": "k3s-server"},
	  "status": {"conditions": [{"type": "Ready", "status": "True"}]}
	}`)
	body := k8sBody(typeK8sNode, "/id", "k3s-server", node)
	if body["status"] != "Ready" {
		t.Errorf("status = %v", body["status"])
	}
	props := body["properties"].(map[string]any)
	if props["ownerKind"] != "" || props["ownerName"] != "" {
		t.Errorf("owner = %v/%v, want empty", props["ownerKind"], props["ownerName"])
	}
	if _, ok := props["restarts"]; ok {
		t.Error("only a pod has restarts")
	}
}
