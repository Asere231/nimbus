// The Kubernetes view: a cluster drawn the way people picture one.
//
// Namespaces hold deployments, a deployment holds its replica sets, and a
// replica set holds its pods -- nesting that comes from each object's
// ownerReferences rather than from guessing at name prefixes. Services sit
// beside them, joined by an arrow to every pod their selector matches, which
// is the relationship `kubectl get all` cannot draw and the one that is
// usually the question. Nodes run along the bottom.
//
// The objects are ordinary resources in the store, mirrored from the real
// cluster by internal/azure/aks_watch.go, so this reads the same map every
// other view does. Model and layout are pure; only draw() touches the DOM.

import { el, fit } from "./dom.js";

export const PAD = 28, CLUSTER_PAD = 16, NS_PAD = 14, DEP_PAD = 12, RS_PAD = 10;
export const HEAD = 34, NS_HEAD = 30, DEP_HEAD = 28, RS_HEAD = 26;
export const POD_H = 34, POD_GAP = 6, POD_W = 250;
export const SVC_W = 190, SVC_H = 56, SVC_GAP = 10, COL_GAP = 40;
export const NODE_W = 150, NODE_H = 42, NODE_GAP = 10, BLOCK_GAP = 18;

const lower = (s) => String(s || "").toLowerCase();
const props = (r) => (r.body && r.body.properties) || {};

// matchesSelector is Kubernetes' own rule for the equality-based selectors a
// Service can express: every key/value in the selector must be present in
// the pod's labels. An EMPTY selector matches nothing here -- Kubernetes
// treats a service with no selector as one whose endpoints are managed by
// hand, so drawing it as "connected to everything" would be a lie.
export function matchesSelector(selector, labels) {
  const keys = Object.keys(selector || {});
  if (!keys.length) return false;
  const have = labels || {};
  return keys.every((k) => have[k] === selector[k]);
}

// kubernetesModel groups every mirrored object under its cluster.
export function kubernetesModel(resources) {
  const all = [...resources.values()];
  const clusters = [];
  const byCluster = new Map();

  for (const r of all) {
    if (lower(r.type) !== "microsoft.containerservice/managedclusters") continue;
    const c = {
      id: r.id, key: lower(r.id), name: r.name, r,
      version: props(r).kubernetesVersion || "",
      namespaces: [], nodes: [],
    };
    clusters.push(c);
    byCluster.set(lower(r.id), c);
  }
  clusters.sort((a, b) => a.name.localeCompare(b.name));

  // Everything mirrored out of a cluster has that cluster as its parent.
  const kinds = { nodes: [], deployments: [], replicasets: [], pods: [], services: [] };
  for (const r of all) {
    const t = lower(r.type);
    if (!t.startsWith("nimbus.kubernetes/")) continue;
    const kind = t.slice("nimbus.kubernetes/".length);
    if (!kinds[kind]) continue;
    kinds[kind].push(r);
  }

  const nsOf = (c, name) => {
    let ns = c.namespaces.find((n) => n.name === name);
    if (!ns) {
      ns = { name, deployments: [], services: [], loosePods: [] };
      c.namespaces.push(ns);
    }
    return ns;
  };
  const clusterOf = (r) => byCluster.get(lower(r.parentId));

  for (const r of kinds.nodes) {
    const c = clusterOf(r);
    if (c) c.nodes.push({ key: lower(r.id), name: r.name, status: props(r).status || "", r });
  }

  // Deployments first, then replica sets into them, then pods into those:
  // one pass per level, so a pod can find the replica set that owns it.
  const depIndex = new Map(); // cluster|ns|name -> deployment
  for (const r of kinds.deployments) {
    const c = clusterOf(r);
    if (!c) continue;
    const p = props(r);
    const dep = {
      key: lower(r.id), name: r.name, status: p.status || "", ns: p.namespace || "default",
      selector: p.selector || {}, replicaSets: [], r,
    };
    nsOf(c, dep.ns).deployments.push(dep);
    depIndex.set(`${c.key}|${dep.ns}|${dep.name}`, dep);
  }

  const rsIndex = new Map();
  const orphanRS = []; // replica sets whose deployment is gone or not mirrored
  for (const r of kinds.replicasets) {
    const c = clusterOf(r);
    if (!c) continue;
    const p = props(r);
    const ns = p.namespace || "default";
    const rs = {
      key: lower(r.id), name: r.name, status: p.status || "", ns,
      selector: p.selector || {}, pods: [], r,
    };
    const dep = depIndex.get(`${c.key}|${ns}|${p.ownerName}`);
    if (dep) dep.replicaSets.push(rs);
    else orphanRS.push({ c, ns, rs });
    rsIndex.set(`${c.key}|${ns}|${rs.name}`, rs);
  }
  // A replica set with no deployment is still drawn, as a bare box in its
  // namespace: it exists in the cluster, so hiding it would be a lie.
  for (const { c, ns, rs } of orphanRS) {
    nsOf(c, ns).deployments.push({
      key: rs.key + "#standalone", name: rs.name, status: rs.status, ns,
      selector: rs.selector, replicaSets: [rs], standalone: true, r: rs.r,
    });
  }

  for (const r of kinds.pods) {
    const c = clusterOf(r);
    if (!c) continue;
    const p = props(r);
    const ns = p.namespace || "default";
    const pod = {
      key: lower(r.id), name: r.name, status: p.status || "", node: p.node || "",
      restarts: p.restarts || 0, labels: p.labels || {}, ns, r,
    };
    const rs = rsIndex.get(`${c.key}|${ns}|${p.ownerName}`);
    if (rs) rs.pods.push(pod);
    else nsOf(c, ns).loosePods.push(pod);
  }

  for (const r of kinds.services) {
    const c = clusterOf(r);
    if (!c) continue;
    const p = props(r);
    const ns = p.namespace || "default";
    nsOf(c, ns).services.push({
      key: lower(r.id), name: r.name, status: p.status || "", ns,
      clusterIP: p.clusterIP || "", ports: p.ports || [], selector: p.selector || {}, r,
    });
  }

  // Deterministic order, and the default namespace first: it is where the
  // thing someone just deployed will be.
  for (const c of clusters) {
    c.namespaces.sort((a, b) =>
      (a.name === "default" ? -1 : b.name === "default" ? 1 : 0) || a.name.localeCompare(b.name));
    c.nodes.sort((a, b) => a.name.localeCompare(b.name));
    for (const ns of c.namespaces) {
      ns.deployments.sort((a, b) => a.name.localeCompare(b.name));
      ns.services.sort((a, b) => a.name.localeCompare(b.name));
      ns.loosePods.sort((a, b) => a.name.localeCompare(b.name));
      for (const dep of ns.deployments) {
        dep.replicaSets.sort((a, b) => a.name.localeCompare(b.name));
        for (const rs of dep.replicaSets) rs.pods.sort((a, b) => a.name.localeCompare(b.name));
      }
    }
  }
  return { clusters };
}

// links returns one entry per service/pod pair the selector matches.
export function serviceLinks(ns) {
  const pods = [];
  for (const dep of ns.deployments) {
    for (const rs of dep.replicaSets) pods.push(...rs.pods);
  }
  pods.push(...ns.loosePods);
  const out = [];
  for (const svc of ns.services) {
    for (const pod of pods) {
      if (matchesSelector(svc.selector, pod.labels)) out.push({ svc, pod });
    }
  }
  return out;
}

// kubernetesLayout places every box and returns the drawing size.
export function kubernetesLayout(model) {
  let y = PAD;
  let widest = 0;

  for (const c of model.clusters) {
    c.x = PAD;
    c.y = y;
    let ny = c.y + HEAD + CLUSTER_PAD;

    for (const ns of c.namespaces) {
      ns.x = c.x + CLUSTER_PAD;
      ns.y = ny;
      let dy = ns.y + NS_HEAD + 8;

      for (const dep of ns.deployments) {
        dep.x = ns.x + NS_PAD;
        dep.y = dy;
        let ry = dep.y + DEP_HEAD + 6;
        for (const rs of dep.replicaSets) {
          rs.x = dep.x + DEP_PAD;
          rs.y = ry;
          rs.pods.forEach((pod, i) => {
            pod.x = rs.x + RS_PAD;
            pod.y = rs.y + RS_HEAD + 4 + i * (POD_H + POD_GAP);
            pod.w = POD_W;
            pod.h = POD_H;
          });
          rs.w = POD_W + 2 * RS_PAD;
          rs.h = RS_HEAD + 4 + Math.max(1, rs.pods.length) * (POD_H + POD_GAP) + 4;
          ry += rs.h + 8;
        }
        dep.w = POD_W + 2 * RS_PAD + 2 * DEP_PAD;
        dep.h = ry - dep.y + 6;
        dy += dep.h + 10;
      }

      // Pods with no replica set behind them sit directly in the namespace.
      ns.loosePods.forEach((pod) => {
        pod.x = ns.x + NS_PAD;
        pod.y = dy;
        pod.w = POD_W;
        pod.h = POD_H;
        dy += POD_H + POD_GAP;
      });

      // Services in their own column to the right of the workloads.
      const svcX = ns.x + NS_PAD + POD_W + 2 * RS_PAD + 2 * DEP_PAD + COL_GAP;
      ns.services.forEach((svc, i) => {
        svc.x = svcX;
        svc.y = ns.y + NS_HEAD + 8 + i * (SVC_H + SVC_GAP);
        svc.w = SVC_W;
        svc.h = SVC_H;
      });
      const svcBottom = ns.services.length
        ? ns.services[ns.services.length - 1].y + SVC_H
        : 0;

      ns.w = (ns.services.length ? svcX + SVC_W : ns.x + NS_PAD + POD_W + 2 * RS_PAD + 2 * DEP_PAD)
        - ns.x + NS_PAD;
      ns.h = Math.max(dy, svcBottom + 8) - ns.y + 6;
      ny += ns.h + BLOCK_GAP;
    }

    // Nodes along the bottom of the cluster box.
    c.nodes.forEach((n, i) => {
      n.x = c.x + CLUSTER_PAD + i * (NODE_W + NODE_GAP);
      n.y = ny;
      n.w = NODE_W;
      n.h = NODE_H;
    });
    const nodeRight = c.nodes.length ? c.nodes[c.nodes.length - 1].x + NODE_W : 0;
    if (c.nodes.length) ny += NODE_H + 4;

    const nsRight = c.namespaces.reduce((m, ns) => Math.max(m, ns.x + ns.w), 0);
    c.w = Math.max(nsRight, nodeRight, c.x + 260) - c.x + CLUSTER_PAD;
    c.h = ny - c.y + CLUSTER_PAD;
    widest = Math.max(widest, c.x + c.w);
    y += c.h + ROW_GAP();
  }

  return { width: Math.max(widest, 360) + PAD, height: Math.max(y, 160) + PAD };
}

function ROW_GAP() { return 28; }

export function draw(svg, model, { selected, filter, onSelect }) {
  const matches = (name) => !filter || lower(name).includes(filter);
  // Selector links are collected and drawn LAST. Drawn first they were
  // behind the namespace and deployment boxes they cross, which are filled,
  // so the links existed in the DOM and were invisible on screen.
  const pending = [];

  for (const c of model.clusters) {
    const g = el("g", { class: "k8s-cluster" + (selected === c.key ? " selected" : ""), transform: `translate(${c.x},${c.y})`, tabindex: "0" }, svg);
    el("rect", { class: "k8s-cluster-box", width: c.w, height: c.h, rx: 14 }, g);
    const t = el("text", { class: "k8s-cluster-title", x: CLUSTER_PAD, y: 23 }, g);
    t.textContent = fit(c.name, 30);
    const meta = el("text", { class: "k8s-meta", x: c.w - CLUSTER_PAD, y: 23, "text-anchor": "end" }, g);
    meta.textContent = [c.version && "v" + c.version,
      c.nodes.length === 1 ? "1 node" : `${c.nodes.length} nodes`].filter(Boolean).join("  ·  ");
    g.addEventListener("click", () => onSelect(c.key));

    for (const ns of c.namespaces) {
      const ng = el("g", { class: "k8s-ns", transform: `translate(${ns.x - c.x},${ns.y - c.y})` }, g);
      el("rect", { class: "k8s-ns-box", width: ns.w, height: ns.h, rx: 10 }, ng);
      const nt = el("text", { class: "k8s-ns-title", x: NS_PAD, y: 20 }, ng);
      nt.textContent = "namespace: " + ns.name;

      for (const dep of ns.deployments) {
        const dg = el("g", {
          class: "k8s-dep" + (selected === dep.key ? " selected" : "") + (matches(dep.name) ? "" : " dim"),
          transform: `translate(${dep.x - ns.x},${dep.y - ns.y})`, tabindex: "0",
        }, ng);
        el("rect", { class: "k8s-dep-box", width: dep.w, height: dep.h, rx: 9 }, dg);
        const dt = el("text", { class: "k8s-dep-title", x: DEP_PAD, y: 19 }, dg);
        dt.textContent = (dep.standalone ? "rs " : "deploy ") + fit(dep.name, 20);
        const ds = el("text", { class: "k8s-status", x: dep.w - DEP_PAD, y: 19, "text-anchor": "end" }, dg);
        ds.textContent = dep.status;
        dg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(dep.key); });

        for (const rs of dep.replicaSets) {
          const rg = el("g", { class: "k8s-rs", transform: `translate(${rs.x - dep.x},${rs.y - dep.y})`, tabindex: "0" }, dg);
          el("rect", { class: "k8s-rs-box", width: rs.w, height: rs.h, rx: 8 }, rg);
          const rt = el("text", { class: "k8s-rs-title", x: RS_PAD, y: 17 }, rg);
          rt.textContent = dep.standalone ? "pods" : "rs " + fit(rs.name, 22);
          rg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(rs.key); });
          if (!rs.pods.length) {
            const none = el("text", { class: "k8s-meta", x: RS_PAD, y: RS_HEAD + 20 }, rg);
            none.textContent = "no pods";
          }
          for (const pod of rs.pods) drawPod(rg, pod, rs, selected, matches, onSelect);
        }
      }

      for (const pod of ns.loosePods) drawPod(ng, pod, ns, selected, matches, onSelect);

      for (const svc of ns.services) {
        const sg = el("g", {
          class: "k8s-svc" + (selected === svc.key ? " selected" : "") + (matches(svc.name) ? "" : " dim"),
          transform: `translate(${svc.x - ns.x},${svc.y - ns.y})`, tabindex: "0",
        }, ng);
        el("rect", { class: "k8s-svc-box", width: svc.w, height: svc.h, rx: 8 }, sg);
        const st = el("text", { class: "k8s-svc-title", x: 12, y: 20 }, sg);
        st.textContent = "svc " + fit(svc.name, 16);
        const sm = el("text", { class: "k8s-meta", x: 12, y: 36 }, sg);
        sm.textContent = [svc.status, svc.clusterIP, svc.ports.map((p) => ":" + p).join(" ")]
          .filter(Boolean).join("  ");
        const sel = el("text", { class: "k8s-sel", x: 12, y: 50 }, sg);
        sel.textContent = Object.entries(svc.selector).map(([k, v]) => `${k}=${v}`).join(" ") || "no selector";
        const tip = el("title", {}, sg);
        tip.textContent = `${svc.name}\n${svc.clusterIP}\nselector: ${sel.textContent}`;
        sg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(svc.key); });
      }

      pending.push(...serviceLinks(ns));
    }

    for (const n of c.nodes) {
      const ng = el("g", {
        class: "k8s-node" + (selected === n.key ? " selected" : "") + (matches(n.name) ? "" : " dim"),
        transform: `translate(${n.x - c.x},${n.y - c.y})`, tabindex: "0",
      }, g);
      el("rect", { class: "k8s-node-box", width: n.w, height: n.h, rx: 8 }, ng);
      const nt = el("text", { class: "k8s-node-name", x: 10, y: 19 }, ng);
      nt.textContent = fit(n.name, 18);
      const ns2 = el("text", { class: "k8s-meta", x: 10, y: 33 }, ng);
      ns2.textContent = n.status;
      ng.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(n.key); });
    }
  }

  const links = el("g", {}, svg);
  for (const { svc, pod } of pending) {
    const x1 = svc.x, y1 = svc.y + SVC_H / 2;
    const x2 = pod.x + pod.w, y2 = pod.y + pod.h / 2;
    const mx = (x1 + x2) / 2;
    const path = el("path", {
      class: "link-selector",
      d: `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`,
    }, links);
    const tip = el("title", {}, path);
    tip.textContent = `${svc.name} \u2192 ${pod.name}`;
  }
}

function drawPod(parent, pod, container, selected, matches, onSelect) {
  const classes = ["k8s-pod", "phase-" + lower(pod.status)];
  if (selected === pod.key) classes.push("selected");
  if (!matches(pod.name)) classes.push("dim");
  const pg = el("g", {
    class: classes.join(" "),
    transform: `translate(${pod.x - container.x},${pod.y - container.y})`,
    tabindex: "0",
  }, parent);
  el("rect", { class: "k8s-pod-box", width: pod.w, height: pod.h, rx: 6 }, pg);
  el("circle", { class: "k8s-phase", cx: 14, cy: pod.h / 2, r: 4 }, pg);
  const nm = el("text", { class: "k8s-pod-name", x: 26, y: pod.h / 2 + 4 }, pg);
  nm.textContent = fit(pod.name, 22);
  const meta = el("text", { class: "k8s-meta", x: pod.w - 10, y: pod.h / 2 + 4, "text-anchor": "end" }, pg);
  meta.textContent = [pod.status, pod.restarts ? `↺ ${pod.restarts}` : ""].filter(Boolean).join("  ");
  const tip = el("title", {}, pg);
  tip.textContent = `${pod.name}\n${pod.status}${pod.node ? " on " + pod.node : ""}` +
    `\nrestarts: ${pod.restarts}`;
  pg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(pod.key); });
}
