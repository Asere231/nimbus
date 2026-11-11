// Drawing the canvas. The shared part -- picking which resource map is in
// charge, the counts, the filter, sizing the SVG -- lives here; each view
// then draws into it. Topology (cards, containment edges and identity links)
// is in this file; the others have their own modules.

import { $, el, fit, switchTab } from "./dom.js";
import { state } from "./state.js";
import { typeStyle, tierOf, displayName, rootLabel, identityLinks } from "./types.js";
import { buildForest, layout, extent, label, W, H } from "./graph.js";
import { showInspector } from "./inspector.js";
import { networkModel, networkLayout, draw as drawNetwork } from "./network.js";
import { kubernetesModel, kubernetesLayout, draw as drawKubernetes } from "./kubernetes.js";
import { planIndex, withGhosts, actionFor, MARKS } from "./plan.js";

let renderQueued = false;
export function scheduleRender() {
  if (renderQueued) return;
  renderQueued = true;
  requestAnimationFrame(() => { renderQueued = false; render(); });
}

export function select(key) {
  state.selected = key;
  switchTab("inspect");
  render();
}

export function render() {
  const svg = $("#graph");
  svg.innerHTML = "";
  // The timeline replaces the view while it replays a run; every view below
  // draws whichever map is in charge, so replay needs no second renderer.
  // A plan is a prediction laid over the live graph, so the resources it
  // would create are merged in as ghosts -- never into the store itself.
  // While a run is being replayed the plan is irrelevant and is left off.
  const live = state.replay || state.resources;
  const view = state.replay ? live : withGhosts(state.plan, live, state.cloud);
  const count = live.size;
  $("#empty").hidden = count > 0 || state.replay !== null;
  // Three tiers, counted separately: the accepted-only number is the one
  // that should keep falling as more types get modelled.
  const all = [...live.values()];
  const real = all.filter((r) => r.backing === "real").length;
  const accepted = all.filter((r) => r.backing !== "real" && r.backing !== "modelled").length;
  $("#counts").textContent =
    `${count} resources · ${real} running locally · ${accepted} accepted only`;

  const filter = $("#filter").value.trim().toLowerCase();
  // The roots toggle only means anything to the tree, and it is named after
  // whatever the current cloud calls its top container.
  $("#subs-toggle").hidden = state.view !== "topology";
  $("#show-roots-label").textContent = rootLabel(state.cloud);

  const def = viewFor(state.view);
  if (def && def.model) {
    // The cloud, because a view's model may be per-cloud: the network
    // view asks each cloud's table what a network and a subnet are.
    const model = def.model(view, state.cloud);
    // A view with nothing in it has to SAY so. An apply with no VNets in it
    // -- examples/terraform/azure-app is exactly that -- left the network
    // view a blank canvas, which reads as a broken page rather than as a
    // true answer about the stack.
    showEmptyView(def, model, count);
    const size = def.layout(model);
    fitCanvas(svg, size.width, size.height);
    def.draw(svg, model, { selected: state.selected, filter, onSelect: select });
    if (state.selected) showInspector(view.get(state.selected));
    return;
  }

  showEmptyView(null, null, count);
  renderTopology(svg, view, filter);
  if (state.selected) showInspector(view.get(state.selected));
}

// The views the canvas can show.
//
// `clouds` is which clouds a view means anything for: a VPC is not a VNet
// and a Kubernetes cluster is an Azure resource here, so the network view
// is Azure's; the Kubernetes one belongs to every cloud whose clusters the
// k3s engine runs. Topology draws any cloud's parent/child tree, so it has no list.
// app.js builds the switcher from whichever of these the current cloud has.
export const VIEWS = [
  { id: "topology", label: "Topology" },
  {
    id: "network", label: "Network", clouds: ["azure"],
    model: networkModel, layout: networkLayout, draw: drawNetwork,
    empty: (m) => !m.vnets.length && !m.orphans.length,
    emptyTitle: "No networks here",
    emptyBody: "This stack has no virtual networks. Create an <code>azurerm_virtual_network</code> " +
      "and its subnets, their security groups and route tables, and any peering appear here.",
  },
  {
    id: "kubernetes", label: "Kubernetes", clouds: ["azure", "aws", "gcp"],
    model: kubernetesModel, layout: kubernetesLayout, draw: drawKubernetes,
    empty: (m) => !m.clusters.length,
    emptyTitle: "No clusters here",
    emptyBody: "This stack has no Kubernetes clusters. Create an <code>azurerm_kubernetes_cluster</code>, " +
      "an <code>aws_eks_cluster</code> or a <code>google_container_cluster</code> \u2014 Nimbus runs " +
      "any of them as k3s in Docker \u2014 and its namespaces, workloads, services and nodes appear here.",
  },
];

// viewsFor returns the views a cloud has, which is what the switcher offers.
export function viewsFor(cloud) {
  return VIEWS.filter((v) => !v.clouds || v.clouds.includes(cloud));
}

export function viewFor(id) {
  return VIEWS.find((v) => v.id === id) || null;
}

function showEmptyView(def, model, count) {
  const panel = $("#empty-view");
  // With nothing at all in the store the "no resources yet" panel already
  // covers it, and two explanations are worse than one.
  const show = Boolean(def && def.empty && model && def.empty(model)) && count > 0;
  panel.hidden = !show;
  if (!show) return;
  $("#empty-view-title").textContent = def.emptyTitle;
  $("#empty-view-body").innerHTML = def.emptyBody;
}

// fitCanvas sizes the SVG and shrinks a drawing that is wider than the pane.
// The floor is 0.5 rather than something higher because storage adds a
// fourth level (subscription -> resource group -> account -> container) and
// at 0.7 the deepest node lands off-screen, which reads as "the container
// was not created" even though it is there and reachable by scrolling.
function fitCanvas(svg, cw, ch) {
  const avail = $("#canvas").clientWidth || cw;
  const scale = Math.min(1, Math.max(0.5, avail / cw));
  svg.setAttribute("viewBox", `0 0 ${cw} ${ch}`);
  svg.setAttribute("width", Math.round(cw * scale));
  svg.setAttribute("height", Math.round(ch * scale));
}

function renderTopology(svg, view, filter) {
  const pending = planIndex(state.plan);
  const roots = buildForest(view, { showRoots: $("#show-subs").checked });
  const nodes = layout(roots);
  const matches = (r) => !filter || r.name.toLowerCase().includes(filter) || r.type.toLowerCase().includes(filter);

  const { width: cw, height: ch } = extent(nodes);
  fitCanvas(svg, cw, ch);

  const edges = el("g", {}, svg);
  for (const n of nodes) {
    for (const c of n.children) {
      const x1 = n.x + W, y1 = n.y + H / 2, x2 = c.x, y2 = c.y + H / 2, mx = (x1 + x2) / 2;
      el("path", { class: "edge", d: `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}` }, edges);
    }
  }
  drawIdentityLinks(nodes, el("g", {}, svg), state.cloud);

  for (const n of nodes) {
    const r = n.r;
    const [abbr, color] = typeStyle(r.type, r.cloud);
    const classes = ["node", tierOf(r)];
    const pendingAction = actionFor(pending, r);
    if (pendingAction) classes.push("plan-" + pendingAction);
    if (r.ghost) classes.push("ghost");
    if (state.selected === n.key) classes.push("selected");
    if (state.current === n.key) classes.push("current");
    if (!matches(r)) classes.push("dim");
    if (state.entered.has(n.key)) classes.push("enter");
    if (state.flashed.has(n.key)) classes.push("flash");
    const g = el("g", { class: classes.join(" "), transform: `translate(${n.x},${n.y})`, tabindex: "0" }, svg);
    el("rect", { class: "card", width: W, height: H, rx: 9 }, g);
    el("rect", { x: 10, y: 13, width: 40, height: 26, rx: 6, fill: color, "fill-opacity": 0.16, stroke: color, "stroke-opacity": 0.6 }, g);
    const b = el("text", { class: "badge", x: 30, y: 30.5, "text-anchor": "middle", fill: color }, g);
    b.textContent = abbr;
    const nm = el("text", { class: "nm", x: 62, y: 23 }, g);
    nm.textContent = fit(displayName(r), 20);
    const ty = el("text", { class: "ty", x: 62, y: 40 }, g);
    ty.textContent = fit(label(r), 26);
    if (pendingAction) {
      const mark = el("text", { class: "plan-badge", x: W - 10, y: 20, "text-anchor": "end" }, g);
      mark.textContent = MARKS[pendingAction] || "?";
    }
    const title = el("title", {}, g);
    title.textContent = r.ghost
      ? `${r.name}\n${r.type || "unmapped type"}\nto be created by ${r.planAddress}`
      : `${r.name}\n${r.type}\n${r.id}` + (pendingAction ? `\nplan: ${pendingAction}` : "");
    g.addEventListener("mousedown", (ev) => ev.preventDefault()); // no focus-scroll on click
    g.addEventListener("click", () => select(n.key));
    g.addEventListener("keydown", (ev) => { if (ev.key === "Enter") select(n.key); });
  }
  state.entered.clear();
  state.flashed.clear();
}

// Some clouds have a relationship that is not containment -- Azure's role
// assignments grant a principal access to a resource elsewhere in the
// graph. The cloud's own table works out the pairs (types-azure.js); this
// draws them as dashed links, so it never has to know what they mean.
export function drawIdentityLinks(nodes, group, cloud) {
  for (const { from, to, label } of identityLinks(nodes, cloud)) {
    // Routed around the cards rather than through them: the two ends are
    // often in the same column, where a straight line between centres would
    // be completely hidden behind the two nodes it connects.
    const y1 = from.y + H / 2, y2 = to.y + H / 2;
    let d;
    if (Math.abs(from.x - to.x) < 1) {
      const x = from.x, bow = Math.max(6, x - 30);
      d = `M${x} ${y1} C${bow} ${y1}, ${bow} ${y2}, ${x} ${y2}`;
    } else {
      const rightwards = from.x < to.x;
      const x1 = rightwards ? from.x + W : from.x;
      const x2 = rightwards ? to.x : to.x + W;
      const mx = (x1 + x2) / 2;
      d = `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`;
    }
    const path = el("path", { class: "link-identity", d }, group);
    const title = el("title", {}, path);
    title.textContent = `${displayName(from.r)} \u2192 ${displayName(to.r)}: ${label}`;
  }
}
