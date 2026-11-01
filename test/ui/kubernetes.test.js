// The Kubernetes view's model: the mirrored objects regrouped into the
// nesting a cluster actually has, and the service-to-pod links the selectors
// imply.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { kubernetesModel, kubernetesLayout, matchesSelector, serviceLinks }
  from "../../internal/ui/static/kubernetes.js";

const AKS = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks1";
const K = AKS + "/kubernetes";

function map(...list) {
  return new Map(list.map((r) => [r.id.toLowerCase(), r]));
}

const cluster = (name = "aks1") => ({
  cloud: "azure", id: AKS, type: "Microsoft.ContainerService/managedClusters", name,
  parentId: "/subscriptions/s/resourceGroups/rg", backing: "real",
  body: { properties: { kubernetesVersion: "1.34.12" } },
});

function obj(kind, name, properties, ns = "default") {
  const id = `${K}/${kind}/${ns}/${name}`;
  return {
    cloud: "azure", id, type: "Nimbus.Kubernetes/" + kind, name, parentId: AKS, backing: "real",
    body: { properties: { namespace: ns, ...properties } },
  };
}

const node = (name) => ({
  cloud: "azure", id: `${K}/nodes/${name}`, type: "Nimbus.Kubernetes/nodes", name,
  parentId: AKS, backing: "real", body: { properties: { status: "Ready" } },
});

// A whole nginx deployment, the way the watcher mirrors one.
function nginx() {
  return [
    cluster(),
    node("k3s-server"),
    obj("deployments", "nginx", { status: "2/2 ready", replicas: 2, selector: { app: "nginx" } }),
    obj("replicasets", "nginx-7d8", { status: "2/2 ready", ownerKind: "Deployment", ownerName: "nginx",
      selector: { app: "nginx", "pod-template-hash": "7d8" } }),
    obj("pods", "nginx-7d8-aaa", { status: "Running", node: "k3s-server", restarts: 0,
      ownerKind: "ReplicaSet", ownerName: "nginx-7d8", labels: { app: "nginx", "pod-template-hash": "7d8" } }),
    obj("pods", "nginx-7d8-bbb", { status: "Running", node: "k3s-server", restarts: 2,
      ownerKind: "ReplicaSet", ownerName: "nginx-7d8", labels: { app: "nginx", "pod-template-hash": "7d8" } }),
    obj("services", "nginx", { status: "ClusterIP", clusterIP: "10.43.0.5", ports: [80],
      selector: { app: "nginx" } }),
  ];
}

test("matchesSelector is a subset test, and an empty selector matches nothing", () => {
  assert.ok(matchesSelector({ app: "nginx" }, { app: "nginx", tier: "web" }),
    "extra labels on the pod are fine");
  assert.ok(!matchesSelector({ app: "nginx", tier: "web" }, { app: "nginx" }),
    "every key in the selector must be present");
  assert.ok(!matchesSelector({ app: "nginx" }, { app: "other" }));
  // Kubernetes treats a service with no selector as one whose endpoints are
  // managed by hand, so "matches everything" would be wrong.
  assert.ok(!matchesSelector({}, { app: "nginx" }));
  assert.ok(!matchesSelector(undefined, { app: "nginx" }));
  assert.ok(!matchesSelector({ app: "nginx" }, undefined));
});

test("objects nest cluster > namespace > deployment > replica set > pod", () => {
  const { clusters } = kubernetesModel(map(...nginx()));
  assert.equal(clusters.length, 1);
  const c = clusters[0];
  assert.equal(c.name, "aks1");
  assert.equal(c.version, "1.34.12");
  assert.equal(c.nodes.length, 1);
  assert.equal(c.namespaces.length, 1);

  const ns = c.namespaces[0];
  assert.equal(ns.name, "default");
  assert.equal(ns.deployments.length, 1);
  const dep = ns.deployments[0];
  assert.equal(dep.name, "nginx");
  assert.equal(dep.replicaSets.length, 1, "the replica set nests under the deployment that owns it");
  assert.equal(dep.replicaSets[0].pods.length, 2, "and the pods under the replica set that owns them");
  assert.equal(ns.loosePods.length, 0);
  assert.equal(dep.replicaSets[0].pods[1].restarts, 2);
});

// Nesting comes from ownerReferences, not from the name. Two deployments
// whose names share a prefix would otherwise swap pods.
test("nesting follows ownerReferences, not name prefixes", () => {
  const objects = [
    cluster(),
    obj("deployments", "web", { status: "1/1 ready" }),
    obj("deployments", "web-api", { status: "1/1 ready" }),
    obj("replicasets", "web-api-xyz", { ownerKind: "Deployment", ownerName: "web-api" }),
    obj("pods", "web-api-xyz-1", { status: "Running", ownerKind: "ReplicaSet", ownerName: "web-api-xyz" }),
  ];
  const ns = kubernetesModel(map(...objects)).clusters[0].namespaces[0];
  const web = ns.deployments.find((d) => d.name === "web");
  const api = ns.deployments.find((d) => d.name === "web-api");
  assert.equal(web.replicaSets.length, 0, "web must not adopt web-api's replica set");
  assert.equal(api.replicaSets.length, 1);
  assert.equal(api.replicaSets[0].pods.length, 1);
});

test("a service links to every pod its selector matches", () => {
  const ns = kubernetesModel(map(...nginx())).clusters[0].namespaces[0];
  const links = serviceLinks(ns);
  assert.equal(links.length, 2, "both pods are behind the service");
  assert.ok(links.every((l) => l.svc.name === "nginx"));

  // A service whose selector matches nothing draws no links at all.
  const lonely = kubernetesModel(map(...nginx(),
    obj("services", "other", { status: "ClusterIP", selector: { app: "nothing" } })));
  const links2 = serviceLinks(lonely.clusters[0].namespaces[0]);
  assert.equal(links2.filter((l) => l.svc.name === "other").length, 0);
});

test("a pod with no replica set still appears in its namespace", () => {
  const objects = [cluster(), obj("pods", "debug", { status: "Running", labels: { app: "debug" } })];
  const ns = kubernetesModel(map(...objects)).clusters[0].namespaces[0];
  assert.equal(ns.loosePods.length, 1);
  assert.equal(ns.deployments.length, 0);
  // ...and a service can still reach it.
  const withSvc = kubernetesModel(map(...objects,
    obj("services", "debug", { selector: { app: "debug" } })));
  assert.equal(serviceLinks(withSvc.clusters[0].namespaces[0]).length, 1);
});

test("a replica set with no deployment is drawn standalone, not dropped", () => {
  const objects = [
    cluster(),
    obj("replicasets", "orphan-rs", { status: "1/1 ready", ownerKind: "Deployment", ownerName: "gone" }),
    obj("pods", "orphan-rs-1", { status: "Running", ownerKind: "ReplicaSet", ownerName: "orphan-rs" }),
  ];
  const ns = kubernetesModel(map(...objects)).clusters[0].namespaces[0];
  assert.equal(ns.deployments.length, 1);
  assert.ok(ns.deployments[0].standalone);
  assert.equal(ns.deployments[0].replicaSets[0].pods.length, 1);
});

test("namespaces are grouped, with default first", () => {
  const objects = [
    cluster(),
    obj("pods", "a", { status: "Running" }, "zzz"),
    obj("pods", "b", { status: "Running" }, "default"),
    obj("pods", "c", { status: "Running" }, "apps"),
  ];
  const c = kubernetesModel(map(...objects)).clusters[0];
  assert.deepEqual(c.namespaces.map((n) => n.name), ["default", "apps", "zzz"]);
});

test("objects are grouped under the cluster that is their parent", () => {
  const AKS2 = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks2";
  const second = { ...cluster("aks2"), id: AKS2 };
  const podElsewhere = {
    ...obj("pods", "other", { status: "Running" }),
    id: AKS2 + "/kubernetes/pods/default/other", parentId: AKS2,
  };
  const { clusters } = kubernetesModel(map(...nginx(), second, podElsewhere));
  assert.equal(clusters.length, 2);
  const [a, b] = clusters;
  assert.equal(a.name, "aks1");
  assert.equal(b.namespaces[0].loosePods.length, 1, "the pod belongs to aks2 only");
  assert.equal(b.namespaces[0].loosePods[0].name, "other");
});

test("the layout nests every box inside its container", () => {
  const model = kubernetesModel(map(...nginx()));
  const size = kubernetesLayout(model);
  const c = model.clusters[0];
  const inside = (child, parent, what) => {
    assert.ok(child.x >= parent.x && child.x + child.w <= parent.x + parent.w, what + " (x)");
    assert.ok(child.y >= parent.y && child.y + child.h <= parent.y + parent.h, what + " (y)");
  };
  const ns = c.namespaces[0];
  inside(ns, c, "namespace inside cluster");
  const dep = ns.deployments[0];
  inside(dep, ns, "deployment inside namespace");
  const rs = dep.replicaSets[0];
  inside(rs, dep, "replica set inside deployment");
  for (const pod of rs.pods) inside(pod, rs, "pod inside replica set");
  for (const svc of ns.services) inside(svc, ns, "service inside namespace");
  assert.ok(size.width >= c.x + c.w && size.height >= c.y + c.h);
});

test("no clusters means an empty drawing, not a crash", () => {
  const model = kubernetesModel(new Map());
  assert.deepEqual(model.clusters, []);
  const size = kubernetesLayout(model);
  assert.ok(size.width > 0 && size.height > 0);
});

test("the fixture shows the nginx pods behind the nginx service", () => {
  const doc = JSON.parse(readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url)));
  const model = kubernetesModel(new Map(doc.resources.map((r) => [r.id.toLowerCase(), r])));
  assert.equal(model.clusters.length, 1);
  const ns = model.clusters[0].namespaces.find((n) => n.name === "default");
  const dep = ns.deployments.find((d) => d.name === "nginx");
  assert.ok(dep, "the fixture should carry a deployment");
  assert.equal(dep.replicaSets.length, 1);
  assert.equal(dep.replicaSets[0].pods.length, 2);
  assert.equal(serviceLinks(ns).length, 2, "both pods sit behind the service");
  kubernetesLayout(model); // must not throw
});
