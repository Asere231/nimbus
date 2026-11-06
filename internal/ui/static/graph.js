// The topology layout engine: turn a flat map of resources into a forest of
// parent/child nodes, then give every node an x/y. Pure functions over plain
// objects -- no DOM, no fetch -- so node can unit test them.
//
// Nothing here knows about any particular cloud: what nests, what sorts
// first and what is hidden all come from the resource's own cloud table in
// types.js.

import { typeStyle, typeOrder, isPassthrough, isRoot } from "./types.js";

// Card geometry, shared with the renderer and the exporters.
export const W = 228, H = 52, GX = 72, GY = 14, PAD = 28;

// buildForest takes a Map of lowercased id -> resource and returns the root
// nodes, each with a `children` array. `showRoots` false drops each cloud's
// top container -- an Azure subscription, an AWS account -- which lifts
// everything below it to a root.
export function buildForest(resources, { showRoots = true } = {}) {
  const nodes = new Map();
  const hidden = new Map(); // key -> parentId, for lifting children
  for (const [k, r] of resources) {
    const type = (r.type || "").toLowerCase();
    if (!showRoots && isRoot(type, r.cloud)) continue;
    if (isPassthrough(type, r.cloud)) {
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
    typeOrder(a.r.type, a.r.cloud) - typeOrder(b.r.type, b.r.cloud) ||
    String(a.r.name).localeCompare(String(b.r.name));
  const sortRec = (list) => { list.sort(byName); list.forEach((n) => sortRec(n.children)); };
  sortRec(roots);
  return roots;
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
  const [, , human] = typeStyle(r.type, r.cloud);
  return human + (r.location ? " · " + r.location : "");
}
