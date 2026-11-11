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
// Virtual machines are the one thing that takes two hops. A VM does not
// name a subnet: it names its network interfaces, and a NIC's
// ipConfigurations name the subnet. So the model follows
// VM -> NIC -> subnet, and a VM whose NIC is missing is simply not drawn
// in a subnet rather than guessed at. In the TOPOLOGY view a VM sits under
// its resource group, which is where ARM really puts it; this view is the
// only place the network relationship is visible.
//
// The model and the layout are pure; only draw() touches the DOM.

import { el, fit } from "./dom.js";
import { typeStyle, networkShape } from "./types.js";

// Geometry. A VNet box is wide because a subnet row has to hold a name, a
// CIDR and two attachment chips without any of them being truncated.
export const VW = 470, TITLE_H = 46, SUB_H = 48, SUB_GAP = 8, BOX_PAD = 12;
export const COL_GAP = 90, ROW_GAP = 30, PAD = 28, CHIP_W = 96, CHIP_H = 24;
export const PEER_LANE = 34; // room to the left for peering arcs
export const ORPHAN_W = 230, ORPHAN_H = 46;
// A row of machine chips under a subnet's name and CIDR, when it has any.
export const VM_CHIP_W = 112, VM_CHIP_H = 26, VM_ROW_H = 34, VM_CHIPS_MAX = 3;

const lower = (s) => String(s || "").toLowerCase();
const leaf = (id) => String(id || "").split("/").filter(Boolean).pop() || "";

// networkModel turns the resource map into what the network view draws.
//
// Cloud-neutral: everything that differs between the three comes from that
// cloud's NETWORK descriptor through types.js. The one fact this rests on
// is uniform across all three -- **a subnet is a stored resource that
// NAMES its network** -- Azure by parent id, AWS by vpcId, GCP by a
// `network` self link. Azure also mirrors its subnets into the VNet's
// body, which is what this used to read; the resources and the mirror are
// always in step, and asking the resources is what generalises.
export function networkModel(resources, cloud) {
  const shape = networkShape(cloud);
  const all = [...resources.values()];
  if (!shape) return { vnets: [], peerings: [], orphans: [] };

  const byID = new Map(all.map((r) => [lower(r.id), r]));
  const attached = new Set(); // ids drawn inside a network, so not orphans
  const isType = (r, t) => Boolean(t) && lower(r.type) === t;

  // Instances, keyed by the subnet they sit in.
  //
  // Some clouds name a subnet directly and some take two hops: an Azure VM
  // names its network interfaces, and a NIC names the subnet. A descriptor
  // with `hopType` gets the second hop resolved here, which is why the
  // view holds every resource rather than only the networking ones.
  const hopSubnets = new Map();
  if (shape.hopType && shape.hopSubnet) {
    for (const r of all) {
      if (!isType(r, shape.hopType)) continue;
      const subnet = shape.hopSubnet(r);
      if (subnet) hopSubnets.set(lower(r.id), subnet);
    }
  }

  const machinesBySubnet = new Map();
  for (const r of all) {
    if (!isType(r, shape.instanceType)) continue;
    let subnetKey = "";
    if (shape.instanceSubnet) {
      subnetKey = shape.instanceSubnet(r);
    } else if (shape.instanceHops) {
      for (const hop of shape.instanceHops(r)) {
        if (hopSubnets.has(hop)) {
          subnetKey = hopSubnets.get(hop);
          attached.add(hop); // the NIC is accounted for, not an orphan
          break;
        }
      }
    }
    // An instance whose subnet is missing is simply not drawn in one
    // rather than guessed at.
    if (!subnetKey) continue;
    if (!machinesBySubnet.has(subnetKey)) machinesBySubnet.set(subnetKey, []);
    machinesBySubnet.get(subnetKey).push({
      id: r.id,
      key: lower(r.id),
      name: r.name,
      // A machine a container really runs for is worth distinguishing
      // from one that is only a record.
      running: Boolean(r.body && r.body.nimbusContainer),
      size: shape.instanceLabel ? shape.instanceLabel(r) : "",
    });
  }
  for (const list of machinesBySubnet.values()) {
    list.sort((a, b) => a.name.localeCompare(b.name));
  }

  // Subnets, grouped by the network each one names.
  const subnetsByNetwork = new Map();
  for (const r of all) {
    if (!isType(r, shape.subnetType)) continue;
    const netKey = shape.networkOf(r);
    const chips = (shape.chipsOf ? shape.chipsOf(r) : []).map((c) => {
      attached.add(lower(c.id));
      return {
        kind: c.kind,
        id: c.id,
        label: c.label,
        name: leaf(c.id),
        known: byID.has(lower(c.id)),
      };
    });
    if (!subnetsByNetwork.has(netKey)) subnetsByNetwork.set(netKey, []);
    subnetsByNetwork.get(netKey).push({
      id: r.id,
      key: lower(r.id),
      name: r.name,
      prefix: shape.cidrOf(r),
      chips,
      machines: machinesBySubnet.get(lower(r.id)) || [],
    });
  }

  const vnets = [];
  for (const r of all) {
    if (!isType(r, shape.networkType)) continue;
    const subnets = subnetsByNetwork.get(lower(r.id)) || [];
    subnets.sort((a, b) => a.name.localeCompare(b.name));
    vnets.push({
      id: r.id,
      key: lower(r.id),
      name: r.name,
      location: r.location || "",
      prefixes: shape.prefixesOf ? shape.prefixesOf(r) : [],
      subnets,
      r,
    });
  }
  vnets.sort((a, b) => a.name.localeCompare(b.name));

  // A cross-network relationship, where a cloud has one. Azure's peerings
  // are the only example; a peering whose remote network is not emulated
  // here still draws, as a link that goes nowhere, because that is the
  // truth.
  const peerings = [];
  if (shape.peeringType && shape.peeringEnds) {
    for (const r of all) {
      if (!isType(r, shape.peeringType)) continue;
      const ends = shape.peeringEnds(r);
      peerings.push({
        name: r.name,
        from: ends.from,
        to: ends.to,
        remoteName: leaf(ends.to),
        state: ends.state || "",
      });
    }
  }

  // Networking resources that belong to nobody: a security group or route
  // table nothing references, a public IP, a gateway. Worth showing -- an
  // unattached rule is usually a mistake, and it is invisible in the tree.
  const orphans = all
    .filter((r) => {
      if (!shape.isNetworkResource || !shape.isNetworkResource(r.type)) return false;
      if (isType(r, shape.subnetType) || isType(r, shape.networkType)) return false;
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
    // Subnet rows are no longer a fixed height: one with machines in it
    // grows by a chip row, so the VNet's height has to be summed rather
    // than multiplied.
    let sy = v.y + TITLE_H;
    for (const sn of v.subnets) {
      sn.x = v.x + BOX_PAD;
      sn.y = sy;
      sn.w = VW - 2 * BOX_PAD;
      sn.h = SUB_H + (sn.machines.length ? VM_ROW_H : 0);
      sy += sn.h + SUB_GAP;
    }
    v.h = sy - v.y + BOX_PAD - SUB_GAP;
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

      // Machines, in a row under the name and CIDR. A subnet with more
      // than a few is summarised rather than overflowing its box.
      if (sn.machines.length) {
        let mx = 12;
        const shown = sn.machines.slice(0, VM_CHIPS_MAX);
        for (const m of shown) {
          const mg = el("g", {
            class: "net-vm" + (m.running ? " running" : "") + (selected === m.key ? " selected" : ""),
            transform: `translate(${mx},${SUB_H - 6})`,
            tabindex: "0",
          }, sg);
          el("rect", { width: VM_CHIP_W, height: VM_CHIP_H, rx: 6 }, mg);
          const mt = el("text", { x: VM_CHIP_W / 2, y: 17, "text-anchor": "middle" }, mg);
          mt.textContent = fit(m.name, 14);
          const mtip = el("title", {}, mg);
          mtip.textContent = `${m.name}\n${m.size}\n` +
            (m.running ? "a container is running for this machine" : "recorded, not running") +
            `\n${m.id}`;
          mg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(m.key); });
          mg.addEventListener("keydown", (ev) => { if (ev.key === "Enter") onSelect(m.key); });
          mx += VM_CHIP_W + 8;
        }
        if (sn.machines.length > shown.length) {
          const more = el("text", { class: "net-cidr", x: mx, y: SUB_H + 11 }, sg);
          more.textContent = `+${sn.machines.length - shown.length} more`;
        }
      }

      // Whatever this cloud attaches to a subnet, right-aligned on its
      // first row. The chips and their human labels come from the cloud's
      // own descriptor: Azure has two (a security group and a route
      // table), AWS has one, and GCP has none at all, because its
      // firewalls attach to the NETWORK and match on tags. A view that
      // named them itself would be inventing a model for two clouds.
      let cx = sn.w - 12 - CHIP_W;
      for (const chip of sn.chips) {
        const cg = el("g", { class: "net-chip " + chip.kind, transform: `translate(${cx},${(SUB_H - CHIP_H) / 2})` }, sg);
        el("rect", { width: CHIP_W, height: CHIP_H, rx: 6 }, cg);
        const ct = el("text", { x: CHIP_W / 2, y: 16, "text-anchor": "middle" }, cg);
        ct.textContent = fit(chip.name, 12);
        const ctip = el("title", {}, cg);
        ctip.textContent = chip.label + ": " + chip.name;
        if (chip.known) {
          cg.addEventListener("click", (ev) => { ev.stopPropagation(); onSelect(lower(chip.id)); });
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
    const [abbr, color, human] = typeStyle(o.type, o.cloud);
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
