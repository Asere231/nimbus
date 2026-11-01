// Drawing the canvas. The shared part -- picking which resource map is in
// charge, the counts, the filter, sizing the SVG -- lives here; each view
// then draws into it. Topology (cards, containment edges and identity links)
// is in this file; the others have their own modules.

import { $, el, fit, switchTab } from "./dom.js";
import { state } from "./state.js";
import { typeStyle, tierOf, displayName, roleName } from "./types.js";
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
  const view = state.replay ? live : withGhosts(state.plan, live);
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
  // The subscriptions toggle only means anything to the tree.
  $("#subs-toggle").hidden = state.view !== "topology";

  if (state.view === "network" || state.view === "kubernetes") {
    const build = state.view === "network" ? networkModel : kubernetesModel;
    const place = state.view === "network" ? networkLayout : kubernetesLayout;
    const paint = state.view === "network" ? drawNetwork : drawKubernetes;
    const model = build(view);
    const size = place(model);
    fitCanvas(svg, size.width, size.height);
    paint(svg, model, { selected: state.selected, filter, onSelect: select });
    if (state.selected) showInspector(view.get(state.selected));
    return;
  }

  renderTopology(svg, view, filter);
  if (state.selected) showInspector(view.get(state.selected));
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
  const roots = buildForest(view, { showSubs: $("#show-subs").checked });
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
  drawIdentityLinks(nodes, el("g", {}, svg));

  for (const n of nodes) {
    const r = n.r;
    const [abbr, color] = typeStyle(r.type);
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

// Role assignments are the one relationship that does not follow the
// parent/child tree: a principal somewhere in the graph is granted access to
// a resource somewhere else. They are drawn as dashed links so it is
// visible, without pretending they are containment.
export function drawIdentityLinks(nodes, group) {
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  // principalId -> the node that OWNS that principal. A user-assigned
  // identity reports it in properties; everything else carries one in
  // `identity`. A role assignment also has a principalId in properties, but
  // it REFERS to a principal rather than owning one -- counting it would
  // make every link start and end at the assignment itself.
  const owners = new Map();
  for (const n of nodes) {
    const type = (n.r.type || "").toLowerCase();
    if (type === "microsoft.authorization/roleassignments") continue;
    const body = n.r.body || {};
    const props = body.properties || {};
    if (type === "microsoft.managedidentity/userassignedidentities" && props.principalId) {
      owners.set(String(props.principalId).toLowerCase(), n);
    }
    if (body.identity && body.identity.principalId) {
      owners.set(String(body.identity.principalId).toLowerCase(), n);
    }
  }
  for (const n of nodes) {
    if ((n.r.type || "").toLowerCase() !== "microsoft.authorization/roleassignments") continue;
    const props = (n.r.body && n.r.body.properties) || {};
    const from = owners.get(String(props.principalId || "").toLowerCase());
    const to = byKey.get(String(props.scope || "").toLowerCase());
    if (!from || !to || from === to) continue;
    // Routed around the cards rather than through them: identity and scope
    // are often in the same column, where a straight line between centres
    // would be completely hidden behind the two nodes it connects.
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
    title.textContent = `${displayName(from.r)} → ${displayName(to.r)}: ${roleName(props.roleDefinitionId)}`;
  }
}
