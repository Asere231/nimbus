// Google Cloud's resource types.
//
// The keys are lower-cased type strings, the same shape the other two
// tables use. GCP has no canonical type name on the wire -- a network is
// just a network -- so Nimbus uses the names Google's own asset inventory
// gives them (`compute.googleapis.com/Network`), which is the closest thing
// to official and what anyone who has read an IAM condition will recognise.

export const TYPES = {
  "cloudresourcemanager.googleapis.com/project": ["PROJ", "#4285f4", "Project"],
  "compute.googleapis.com/network": ["VPC", "#34a853", "VPC network"],
  "compute.googleapis.com/subnetwork": ["SNET", "#5bb974", "Subnetwork"],
  "compute.googleapis.com/instance": ["VM", "#fbbc04", "Compute instance"],
  "compute.googleapis.com/firewall": ["FW", "#fbbc04", "Firewall rule"],
  "compute.googleapis.com/route": ["ROUTE", "#a5d6ff", "Route"],
  "compute.googleapis.com/router": ["RTR", "#a5d6ff", "Cloud Router"],
  "iam.googleapis.com/serviceaccount": ["SA", "#bc8cff", "Service account"],
  "storage.googleapis.com/bucket": ["GCS", "#ea4335", "Storage bucket"],
  "container.googleapis.com/cluster": ["GKE", "#4285f4", "GKE cluster"],
  "container.googleapis.com/nodepool": ["POOL", "#669df6", "Node pool"],
  "pubsub.googleapis.com/topic": ["TOPIC", "#fbbc04", "Pub/Sub topic"],
  "pubsub.googleapis.com/subscription": ["SUB", "#fdd663", "Subscription"],
  "run.googleapis.com/service": ["RUN", "#34a853", "Cloud Run service"],
  "run.googleapis.com/revision": ["REV", "#81c995", "Revision"],
};

// Siblings read the way a stack is built: the network first, then what sits
// in it, then the things that merely belong to the project.
export const ORDER = ["project", "network", "subnetwork", "instance", "firewall", "route",
  "router", "cluster", "nodepool", "service", "revision", "topic", "subscription",
  "bucket", "serviceaccount"];

// GCP has no addressing-only types to hide, unlike ARM's blobServices.
export const PASSTHROUGH = null;

// The type at the top of the tree, which the toolbar's toggle hides.
export const ROOT_TYPE = "cloudresourcemanager.googleapis.com/project";
export const ROOT_LABEL = "project";

// Everything in GCP hangs off the project, so there is no second grouping
// level for the plan overlay to park a pending create in.
export const GROUP_TYPE = null;

// GCP's managed Kubernetes is GKE, and it is a k3s container like AKS and
// EKS -- mirrored into the store by the same engine, with the same shapes.
// Naming the type here is the whole of what the Kubernetes view needed to
// work for a third cloud.
export const K8S_CLUSTER_TYPE = "container.googleapis.com/cluster";

export function displayName(r) {
  return r.name;
}

// --- the network view's model ----------------------------------------------
//
// What the shared network view needs to know about Google's networking.
// The rule all three clouds share: a subnet is a stored resource that
// NAMES its network -- here by a `network` self link, which IS the
// network's store id, since every GCP id here is the URL it answers on.
//
// Two things are genuinely different and the descriptor says so by what it
// leaves out:
//
//   - **A subnetwork has NO attachments.** A firewall rule attaches to the
//     NETWORK and matches on network tags, so there is nothing to draw on
//     a subnetwork and `chipsOf` is absent rather than returning something
//     invented.
//   - **A custom-mode network has no address range of its own.** The
//     ranges live on the subnetworks, so `prefixesOf` returns nothing and
//     the network's box carries a name and no CIDR -- which is the truth.
export const NETWORK = {
  networkType: "compute.googleapis.com/network",
  subnetType: "compute.googleapis.com/subnetwork",
  instanceType: "compute.googleapis.com/instance",

  prefixesOf: () => [],
  cidrOf: (r) => (r.body || {}).ipCidrRange || "",
  networkOf: (r) => String((r.body || {}).network || "").toLowerCase(),

  // One hop, through the first interface that names a subnetwork. The
  // subnetwork arrives as a partial URL and is stored as a full self link,
  // so it matches the subnetwork's id directly.
  instanceSubnet: (r) => {
    for (const nic of (r.body || {}).networkInterfaces || []) {
      if (nic.subnetwork) return String(nic.subnetwork).toLowerCase();
    }
    return "";
  },
  // A machine type arrives as a URL; the last segment is the name.
  instanceLabel: (r) => String((r.body || {}).machineType || "").split("/").filter(Boolean).pop() || "",

  // Firewalls, routes and routers all hang off the network rather than a
  // subnetwork, so they appear in the unattached column -- which is
  // accurate: this view does not draw what they match on.
  isNetworkResource: (type) => {
    const t = (type || "").toLowerCase();
    return t === "compute.googleapis.com/firewall" || t === "compute.googleapis.com/route" ||
      t === "compute.googleapis.com/router";
  },
};
