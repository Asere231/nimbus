// How a resource is presented, dispatched on which cloud it came from.
//
// Every resource and every event carries a `cloud`, so the badge, colour,
// human label, sibling order and the types worth hiding all come from that
// cloud's own table (types-azure.js, types-aws.js) rather than from one
// map that would grow a prefix convention and a pile of exceptions.
//
// Nothing here is cloud-specific: the tiers are the same everywhere,
// because they describe how much NIMBUS knows about a resource, not what
// the resource is.

import * as azure from "./types-azure.js";
import * as aws from "./types-aws.js";
import * as gcp from "./types-gcp.js";

const CLOUDS = { azure, aws, gcp };

// cloudOf returns a cloud's table, or null when it has none -- an unknown
// cloud still draws, just without badges anybody chose.
export function cloudOf(name) {
  return CLOUDS[String(name || "").toLowerCase()] || null;
}

// How much Nimbus really knows about a resource: a local engine runs it,
// its shape is modelled (a hook or the provider's own API spec), or it is
// merely accepted and stored.
export const TIER_LABELS = {
  real: "local engine",
  modelled: "modelled from the API spec",
  state: "accepted, state only",
};
export const tierOf = (r) =>
  r.backing === "real" ? "real" : r.backing === "modelled" ? "modelled" : "state";

// typeStyle returns [badge, colour, human label] for a type in a cloud.
export function typeStyle(type, cloud) {
  const src = type || "";
  const table = cloudOf(cloud);
  if (table) {
    const styled = table.TYPES[src.toLowerCase()];
    if (styled) return styled;
  }
  // No table, or a type nobody styled: abbreviate the last segment. The
  // capitals of "superWidgets" give "W"; with no capitals to take, the
  // first three letters stand in rather than the badge coming out empty.
  const last = src.split(/[/:]/).filter(Boolean).pop() || src;
  const abbr = last.replace(/[a-z]/g, "").slice(0, 4) || last.slice(0, 3).toUpperCase();
  return [abbr.toUpperCase(), "#8b98a9", last];
}

// displayName is what a card shows, which some types rewrite -- an Azure
// role assignment is named after a GUID nobody chose.
export function displayName(r) {
  const table = cloudOf(r && r.cloud);
  if (table && table.displayName) return table.displayName(r);
  return r.name;
}

// typeOrder places siblings: lower sorts first, unknown types last.
export function typeOrder(type, cloud) {
  const table = cloudOf(cloud);
  if (!table) return Number.MAX_SAFE_INTEGER;
  const last = (type || "").toLowerCase().split(/[/:]/).filter(Boolean).pop();
  const i = table.ORDER.indexOf(last);
  return i === -1 ? table.ORDER.length : i;
}

// isPassthrough reports whether a type exists only so the API has something
// to address, and should be skipped with its children lifted up.
export function isPassthrough(type, cloud) {
  const table = cloudOf(cloud);
  return Boolean(table && table.PASSTHROUGH && table.PASSTHROUGH.test((type || "").toLowerCase()));
}

// isRoot reports whether a type is the cloud's top container -- an Azure
// subscription, an AWS account -- which the toolbar can hide.
export function isRoot(type, cloud) {
  const table = cloudOf(cloud);
  return Boolean(table && table.ROOT_TYPE && table.ROOT_TYPE === (type || "").toLowerCase());
}

// rootLabel names that container for the toggle beside the graph.
export function rootLabel(cloud) {
  const table = cloudOf(cloud);
  return (table && table.ROOT_LABEL) || "roots";
}

// groupType is the type that holds a cloud's resources together, when it
// has one: an Azure resource group. Null for a cloud that has no such idea.
export function groupType(cloud) {
  const table = cloudOf(cloud);
  return (table && table.GROUP_TYPE) || null;
}

// isK8sCluster reports whether a resource is a managed Kubernetes cluster in
// its own cloud -- an AKS cluster or an EKS one. Both are k3s containers
// mirrored by the same engine, so the Kubernetes view asks this rather than
// naming either type.
export function isK8sCluster(type, cloud) {
  const table = cloudOf(cloud);
  return Boolean(table && table.K8S_CLUSTER_TYPE &&
    table.K8S_CLUSTER_TYPE === (type || "").toLowerCase());
}

// networkShape is what the network view needs to know about a cloud's
// networking: which type is a network, which is a subnet, how a subnet
// names its network and what hangs off one. A cloud without a networking
// model returns null and the view says it has nothing to draw.
export function networkShape(cloud) {
  const table = cloudOf(cloud);
  return (table && table.NETWORK) || null;
}

// identityLinks returns the extra, non-containment edges a cloud's graph
// has -- Azure's role assignments. Clouds without any return nothing.
export function identityLinks(nodes, cloud) {
  const table = cloudOf(cloud);
  if (!table || !table.identityLinks) return [];
  return table.identityLinks(nodes);
}
