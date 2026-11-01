// The topology layout engine: turn a flat map of resources into a forest of
// parent/child nodes, then give every node an x/y. Pure functions over plain
// objects -- no DOM, no fetch -- so node can unit test them.

import { typeStyle } from "./types.js";

// Card geometry, shared with the renderer and the exporters.
export const W = 228, H = 52, GX = 72, GY = 14, PAD = 28;

// Types that exist only so ARM has something to address. A storage account
// really does have blobServices/default and three siblings -- azurerm polls
// them -- but drawing four "default" boxes under every account is noise. They
// are skipped and their children lifted to the grandparent, so a container
// appears directly under its account, which is how people think about it.
export const PASSTHROUGH = /\/(blob|queue|table|file)services$/;

// buildForest takes a Map of lowercased id -> resource and returns the root
// nodes, each with a `children` array. `showSubs` false drops subscription
// nodes, which lifts every resource group to a root.
export function buildForest(resources, { showSubs = true } = {}) {
  const nodes = new Map();
  const hidden = new Map(); // key -> parentId, for lifting children
  for (const [k, r] of resources) {
    const type = (r.type || "").toLowerCase();
    if (!showSubs && type === "microsoft.resources/subscriptions") continue;
    if (PASSTHROUGH.test(type)) {
      hidden.set(k, (r.parentId || "").toLowerCase());
      continue;
    }
    nodes.set(k, { key: k, r, children: [] });
  }
  const roots = [];
  for (const n of nodes.values()) {
    let p = (n.r.parentId || "").toLowerCase();
    // Walk up past any hidden ancestors.
    let guard = 0;
    while (hidden.has(p) && guard++ < 8) p = hidden.get(p);
    const parent = nodes.get(p);
    if (parent) parent.children.push(n); else roots.push(n);
  }
  const byName = (a, b) =>
    typeOrder(a.r.type) - typeOrder(b.r.type) || String(a.r.name).localeCompare(String(b.r.name));
  const sortRec = (list) => { list.sort(byName); list.forEach((n) => sortRec(n.children)); };
  sortRec(roots);
  return roots;
}

// Siblings are ordered so a stack reads the way it was built -- network
// before storage before compute -- rather than alphabetically.
export const ORDER = ["subscriptions", "resourcegroups", "virtualnetworks", "subnets", "networksecuritygroups",
  "storageaccounts", "containers", "managedclusters", "agentpools", "nodes", "deployments", "services", "pods"];
export function typeOrder(type) {
  const last = (type || "").toLowerCase().split("/").pop();
  const i = ORDER.indexOf(last);
  return i === -1 ? ORDER.length : i;
}

// layout assigns x/y in place and returns every node, parents last (so the
// renderer draws children first and a parent's edges on top).
export function layout(roots) {
  let row = 0;
  const placed = [];
  const place = (n, depth) => {
    n.x = PAD + depth * (W + GX);
    if (n.children.length === 0) {
      n.y = PAD + row * (H + GY);
      row++;
    } else {
      n.children.forEach((c) => place(c, depth + 1));
      n.y = (n.children[0].y + n.children[n.children.length - 1].y) / 2;
    }
    placed.push(n);
  };
  roots.forEach((r) => { place(r, 0); row += 0.6; });
  return placed;
}

// extent is the drawing area a laid-out set of nodes needs, padding included.
export function extent(nodes) {
  let maxX = 0, maxY = 0;
  for (const n of nodes) { maxX = Math.max(maxX, n.x + W); maxY = Math.max(maxY, n.y + H); }
  return { width: maxX + PAD, height: maxY + PAD };
}

// label is what a card shows: the type's human name plus its location.
export function label(r) {
  const [, , human] = typeStyle(r.type);
  return human + (r.location ? " · " + r.location : "");
}
