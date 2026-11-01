// The plan overlay: what `terraform plan` says is about to happen, drawn on
// the graph before anything happens.
//
// The server has already reduced the plan (internal/ui/plan.go); what is
// left here is matching each pending change to a card. An update or a delete
// names a resource that exists, so it matches by ID. A CREATE has no ID --
// Terraform does not know it until the call is made -- so it becomes a ghost
// card placed in the resource group it will appear in, which the plan does
// know.
//
// Pure: no DOM, no fetch. Tested by node.

const lower = (s) => String(s || "").toLowerCase();

// GHOST_PREFIX marks a synthetic resource. Nothing can collide with it: a
// real ARM ID always starts with /subscriptions.
export const GHOST_PREFIX = "plan:";

export const isGhost = (r) => Boolean(r && r.ghost);

// planIndex maps the changes that name an existing resource, by ID.
export function planIndex(overlay) {
  const byID = new Map();
  for (const c of (overlay && overlay.changes) || []) {
    if (c.id) byID.set(lower(c.id), c);
  }
  return byID;
}

// ghosts returns synthetic resources for the creates that have no ID yet,
// parented to the resource group they name when that group is on screen.
//
// A create whose group is not here still comes back, as a root -- a pending
// change nobody can see is worse than one in an odd place.
export function ghosts(overlay, resources) {
  const groups = new Map();
  for (const r of resources.values()) {
    if (lower(r.type) === "microsoft.resources/resourcegroups") groups.set(lower(r.name), r);
  }

  const out = [];
  for (const c of (overlay && overlay.changes) || []) {
    if (c.action !== "create") continue;
    // A create that somehow does have an ID is matched like anything else.
    if (c.id && resources.has(lower(c.id))) continue;
    const group = c.resourceGroup ? groups.get(lower(c.resourceGroup)) : null;
    out.push({
      cloud: "azure",
      id: GHOST_PREFIX + c.address,
      type: c.type || "",
      // Without a mapped ARM type the Terraform address is all we honestly
      // have, and it is enough to recognise the line in the plan output.
      name: c.name || c.address,
      parentId: group ? group.id : "",
      location: "",
      backing: "state",
      body: {},
      ghost: true,
      planAction: "create",
      planAddress: c.address,
    });
  }
  return out;
}

// withGhosts returns a copy of the resource map with the ghosts added, which
// is what the views draw. The live map is never touched: a plan is a
// prediction, not state.
export function withGhosts(overlay, resources) {
  const list = ghosts(overlay, resources);
  if (!list.length) return resources;
  const merged = new Map(resources);
  for (const g of list) merged.set(lower(g.id), g);
  return merged;
}

// actionFor returns the pending action on a node, if any.
export function actionFor(index, resource) {
  if (!resource) return "";
  if (resource.ghost) return resource.planAction || "create";
  const change = index.get(lower(resource.id));
  return change ? change.action : "";
}

// MARKS is what each action is drawn as, in Terraform's own notation.
export const MARKS = {
  create: "+",
  update: "~",
  delete: "-",
  replace: "-/+",
};

export function summarise(overlay) {
  if (!overlay || !(overlay.changes || []).length) return "";
  const parts = [];
  for (const [n, word] of [
    [overlay.creates, "to create"],
    [overlay.updates, "to change"],
    [overlay.replaces, "to replace"],
    [overlay.deletes, "to destroy"],
  ]) {
    if (n) parts.push(`${n} ${word}`);
  }
  return "Plan: " + (parts.join(", ") || "no changes") +
    (overlay.source ? "  ·  " + overlay.source : "");
}
