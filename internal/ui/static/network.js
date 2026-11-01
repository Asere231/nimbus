// The network view: VNets as containers with their subnets inside, the NSG
// and route table attached to each subnet, and peerings drawn between VNets.
//
// The topology view answers "what exists and what contains what". This one
// answers the question people actually ask of a network -- which address
// range is where, and what is filtering it -- and that is a different shape,
// so it gets its own model and its own layout rather than another tree.
//
// Everything here reads the stored ARM bodies, which already carry all of
// it: a VNet mirrors its subnets into properties.subnets (the parent/child
// sync azurerm requires), and each subnet names its NSG and route table by
// resource ID.
//
// The model and the layout are pure; only draw() touches the DOM.

import { el, fit } from "./dom.js";
import { typeStyle } from "./types.js";

// Geometry. A VNet box is wide because a subnet row has to hold a name, a
// CIDR and two attachment chips without any of them being truncated.
export const VW = 470, TITLE_H = 46, SUB_H = 48, SUB_GAP = 8, BOX_PAD = 12;
export const COL_GAP = 90, ROW_GAP = 30, PAD = 28, CHIP_W = 96, CHIP_H = 24;
export const PEER_LANE = 34; // room to the left for peering arcs
export const ORPHAN_W = 230, ORPHAN_H = 46;

const lower = (s) => String(s || "").toLowerCase();
const leaf = (id) => String(id || "").split("/").filter(Boolean).pop() || "";

// networkModel turns the resource map into what the network view draws.
export function networkModel(resources) {
  const all = [...resources.values()];
  const byID = new Map(all.map((r) => [lower(r.id), r]));
  const attached = new Set(); // ids drawn inside a VNet, so not orphans

  const vnets = [];
  for (const r of all) {
    if (lower(r.type) !== "microsoft.network/virtualnetworks") continue;
    const props = (r.body && r.body.properties) || {};
    const subnets = [];
    for (const sn of props.subnets || []) {
      const sp = sn.properties || {};
      const nsgID = sp.networkSecurityGroup && sp.networkSecurityGroup.id;
      const rtID = sp.routeTable && sp.routeTable.id;
      if (nsgID) attached.add(lower(nsgID));
      if (rtID) attached.add(lower(rtID));
      subnets.push({
        id: sn.id || "",
        key: lower(sn.id),
        name: sn.name || leaf(sn.id),
        prefix: sp.addressPrefix || (sp.addressPrefixes || [])[0] || "",
        nsg: nsgID ? { id: nsgID, name: leaf(nsgID), known: byID.has(lower(nsgID)) } : null,
        route: rtID ? { id: rtID, name: leaf(rtID), known: byID.has(lower(rtID)) } : null,
      });
    }
    vnets.push({
      id: r.id,
      key: lower(r.id),
      name: r.name,
      location: r.location || "",
      prefixes: (props.addressSpace && props.addressSpace.addressPrefixes) || [],
      subnets,
      r,
    });
  }
  vnets.sort((a, b) => a.name.localeCompare(b.name));

  // Peerings are their own resource type; each names the VNet at the far
  // end. A peering whose remote VNet is not emulated here still draws, as a
  // link that goes nowhere, because that is the truth.
  const peerings = [];
  for (const r of all) {
    if (lower(r.type) !== "microsoft.network/virtualnetworks/virtualnetworkpeerings") continue;
    const props = (r.body && r.body.properties) || {};
    const remote = (props.remoteVirtualNetwork && props.remoteVirtualNetwork.id) || "";
    // The parent VNet is everything before "/virtualNetworkPeerings/".
    const from = String(r.id).split(/\/virtualNetworkPeerings\//i)[0];
    peerings.push({
      name: r.name,
      from: lower(from),
      to: lower(remote),
      remoteName: leaf(remote),
      state: props.peeringState || "",
    });
  }

  // Network resources that belong to nobody: an NSG or route table nothing
  // references, a public IP, a NAT gateway. Worth showing -- an unattached
  // NSG is usually a mistake, and it is invisible in the tree view.
  const orphans = all
    .filter((r) => {
      const t = lower(r.type);
      if (!t.startsWith("microsoft.network/")) return false;
      if (t === "microsoft.network/virtualnetworks") return false;
      if (t.includes("/virtualnetworks/")) return false; // subnets, peerings
      return !attached.has(lower(r.id));
    })
    .sort((a, b) => a.type.localeCompare(b.type) || a.name.localeCompare(b.name));

  return { vnets, peerings, orphans };
}

// networkLayout places every box. VNets stack in one column, with anything
// unattached beside them.
export function networkLayout(model) {
  // Peerings bow out to the LEFT of the boxes they join, so when there are
  // any, the whole drawing shifts right to leave them room. Without this the
  // arc is drawn half outside the viewBox and reads as a rendering glitch.
  const left = PAD + (model.peerings.length ? PEER_LANE : 0);
  model.left = left;
  let y = PAD;
  for (const v of model.vnets) {
    v.x = left;
    v.y = y;
    v.w = VW;
    v.h = TITLE_H + v.subnets.length * (SUB_H + SUB_GAP) + BOX_PAD;
    v.subnets.forEach((sn, i) => {
      sn.x = v.x + BOX_PAD;
      sn.y = v.y + TITLE_H + i * (SUB_H + SUB_GAP);
      sn.w = VW - 2 * BOX_PAD;
      sn.h = SUB_H;
    });
    y += v.h + ROW_GAP;
  }

  const ox = left + VW + COL_GAP;
  model.orphans.forEach((o, i) => {
    o.x = ox;
    o.y = PAD + i * (ORPHAN_H + 10);
    o.w = ORPHAN_W;
    o.h = ORPHAN_H;
  });

  const bottom = Math.max(
    y,
    PAD + model.orphans.length * (ORPHAN_H + 10),
  );
  const right = model.orphans.length ? ox + ORPHAN_W : left + VW;
  return { width: right + PAD, height: bottom + PAD };
}

// draw renders the model into an SVG element. onSelect is called with a
// resource key when something drawn from a real resource is clicked.
export function draw(svg, model, { selected, filter, onSelect }) {
  const matches = (name, type) =>
    !filter || lower(name).includes(filter) || lower(type).includes(filter);

  const links = el("g", {}, svg);

  for (const v of model.vnets) {
    const classes = ["net-vnet"];
    if (selected === v.key) classes.push("selected");
    if (!matches(v.name, v.r.type)) classes.push("dim");
    const g = el("g", { class: classes.join(" "), transform: `translate(${v.x},${v.y})`, tabindex: "0" }, svg);
    el("rect", { class: "net-box", width: v.w, height: v.h, rx: 12 }, g);

    const title = el("text", { class: "net-title", x: BOX_PAD, y: 26 }, g);
    title.textContent = fit(v.name, 28);
    const sub = el("text", { class: "net-sub", x: v.w - BOX_PAD, y: 26, "text-anchor": "end" }, g);
    sub.textContent = [v.prefixes.join(", "), v.location].filter(Boolean).join("  ·  ");

    g.addEventListener("click", () => onSelect(v.key));
    g.addEventListener("keydown", (ev) => { if (ev.key === "Enter") onSelect(v.key); });
    const tip = el("title", {}, g);
    tip.textContent = `${v.name}\n${v.prefixes.join(", ")}\n${v.id}`;

    for (const sn of v.subnets) {
      const sg = el("g", {
        class: "net-subnet" + (selected === sn.key ? " selected" : ""),
        transform: `translate(${sn.x - v.x},${sn.y - v.y})`,
        tabindex: "0",
      }, g);
      el("rect", { class: "net-sub-box", width: sn.w, height: sn.h, rx: 8 }, sg);
      const nm = el("text", { class: "net-sub-name", x: 12, y: 20 }, sg);
      nm.textContent = fit(sn.name, 22);
      const cidr = el("text", { class: "net-cidr", x: 12, y: 36 }, sg);
      cidr.textContent = sn.prefix || "no prefix";

      // Attachments, right-aligned inside the subnet row.
      let cx = sn.w - 12 - CHIP_W;
      for (const [kind, att] of [["rt", sn.route], ["nsg", sn.nsg]]) {
        if (!att) continue;
        const cg = el("g", { class: "net-chip " + kind, transform: `translate(${cx},${(sn.h - CHIP_H) / 2})` }, sg);
        el("rect", { width: CHIP_W, height: CHIP_H, rx: 6 }, cg);
        const ct = el("text", { x: CHIP_W / 2, y: 16, "text-anchor": "middle" }, cg);
        ct.textContent = fit(att.name, 12);
        const ctip = el("title", {}, cg);
        ctip.textContent = (kind === "nsg" ? "Network security group: " : "Route table: ") + att.name;
        if (att.known) {
          cg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(lower(att.id)); });
        }
        cx -= CHIP_W + 8;
      }
      sg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(sn.key); });
      sg.addEventListener("keydown", (ev) => { if (ev.key === "Enter") onSelect(sn.key); });
    }
  }

  // Peerings, bowed out to the left so they are not hidden behind the boxes
  // they join -- the same routing the role-assignment links use.
  const byKey = new Map(model.vnets.map((v) => [v.key, v]));
  for (const p of model.peerings) {
    const from = byKey.get(p.from);
    const to = byKey.get(p.to);
    if (!from) continue;
    const y1 = from.y + TITLE_H / 2;
    const y2 = to ? to.y + TITLE_H / 2 : y1 + 40;
    const bow = Math.max(6, (model.left || PAD) - PEER_LANE + 6);
    const d = to
      ? `M${from.x} ${y1} C${bow} ${y1}, ${bow} ${y2}, ${to.x} ${y2}`
      : `M${from.x} ${y1} C${bow} ${y1}, ${bow} ${y2}, ${from.x - 10} ${y2}`;
    const path = el("path", { class: "link-peering", d }, links);
    const tip = el("title", {}, path);
    tip.textContent = to
      ? `Peering ${p.name}: ${leaf(from.id)} ↔ ${p.remoteName}${p.state ? " (" + p.state + ")" : ""}`
      : `Peering ${p.name}: ${p.remoteName} is not emulated here`;
  }

  for (const o of model.orphans) {
    const [abbr, color, human] = typeStyle(o.type);
    const classes = ["node", "net-orphan"];
    if (selected === lower(o.id)) classes.push("selected");
    if (!matches(o.name, o.type)) classes.push("dim");
    const g = el("g", { class: classes.join(" "), transform: `translate(${o.x},${o.y})`, tabindex: "0" }, svg);
    el("rect", { class: "card", width: o.w, height: o.h, rx: 8 }, g);
    el("rect", { x: 8, y: 11, width: 36, height: 24, rx: 5, fill: color, "fill-opacity": 0.16, stroke: color, "stroke-opacity": 0.6 }, g);
    const b = el("text", { class: "badge", x: 26, y: 27, "text-anchor": "middle", fill: color }, g);
    b.textContent = abbr;
    const nm = el("text", { class: "nm", x: 54, y: 20 }, g);
    nm.textContent = fit(o.name, 20);
    const ty = el("text", { class: "ty", x: 54, y: 36 }, g);
    ty.textContent = fit(human, 22);
    const tip = el("title", {}, g);
    tip.textContent = `${o.name}\n${o.type}\nnot attached to any subnet`;
    g.addEventListener("click", () => onSelect(lower(o.id)));
    g.addEventListener("keydown", (ev) => { if (ev.key === "Enter") onSelect(lower(o.id)); });
  }
}
