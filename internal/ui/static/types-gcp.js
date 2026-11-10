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
