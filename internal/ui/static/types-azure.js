// Azure's resource types: the badge, colour and human label for each one,
// plus the sibling ordering and the types that exist only so ARM has
// something to address.
//
// Short badge + colour per type. A type that is not here falls back to an
// abbreviation of the last path segment, so one nobody has styled still
// gets a card that reads sensibly.

export const TYPES = {
  "microsoft.resources/subscriptions": ["SUB", "#a371f7", "Subscription"],
  "microsoft.resources/resourcegroups": ["RG", "#4aa3ff", "Resource group"],
  "microsoft.resources/deployments": ["DEPLOY", "#a371f7", "Template deployment"],
  "microsoft.network/virtualnetworks": ["VNET", "#39c5cf", "Virtual network"],
  "microsoft.network/virtualnetworks/subnets": ["SNET", "#56d4bc", "Subnet"],
  "microsoft.network/networksecuritygroups": ["NSG", "#f0883e", "Network security group"],
  "microsoft.network/publicipaddresses": ["PIP", "#d2a8ff", "Public IP"],
  "microsoft.network/networkinterfaces": ["NIC", "#79c0ff", "Network interface"],
  "microsoft.network/loadbalancers": ["LB", "#ffa657", "Load balancer"],
  "microsoft.network/routetables": ["RT", "#a5d6ff", "Route table"],
  "microsoft.storage/storageaccounts": ["ST", "#3fb950", "Storage account"],
  "microsoft.storage/storageaccounts/blobservices/containers": ["BLOB", "#2ea043", "Blob container"],
  "microsoft.storage/storageaccounts/blobservices": ["BSVC", "#8b949e", "Blob service"],
  "microsoft.storage/storageaccounts/queueservices": ["QSVC", "#8b949e", "Queue service"],
  "microsoft.storage/storageaccounts/tableservices": ["TSVC", "#8b949e", "Table service"],
  "microsoft.storage/storageaccounts/fileservices": ["FSVC", "#8b949e", "File service"],
  "microsoft.containerservice/managedclusters": ["AKS", "#326ce5", "Kubernetes cluster"],
  "microsoft.containerservice/managedclusters/agentpools": ["POOL", "#5b8def", "Node pool"],
  "nimbus.kubernetes/nodes": ["NODE", "#326ce5", "Node"],
  "nimbus.kubernetes/deployments": ["DEPL", "#7aa2f7", "Deployment"],
  "nimbus.kubernetes/pods": ["POD", "#3fb950", "Pod"],
  "nimbus.kubernetes/services": ["SVC", "#ffa657", "Service"],
  "microsoft.containerregistry/registries": ["ACR", "#58a6ff", "Container registry"],
  "microsoft.keyvault/vaults": ["KV", "#e3b341", "Key vault"],
  "microsoft.managedidentity/userassignedidentities": ["ID", "#bc8cff", "Managed identity"],
  "microsoft.authorization/roleassignments": ["ROLE", "#f0883e", "Role assignment"],
  "microsoft.operationalinsights/workspaces": ["LAW", "#8b949e", "Log Analytics"],
  "microsoft.web/serverfarms": ["ASP", "#1f6feb", "App Service plan"],
  "microsoft.web/sites": ["APP", "#388bfd", "App Service"],
  "microsoft.compute/virtualmachines": ["VM", "#f778ba", "Virtual machine"],
  "microsoft.compute/virtualmachines/extensions": ["EXT", "#f9a8d4", "VM extension"],
  "microsoft.compute/virtualmachinescalesets": ["VMSS", "#f778ba", "Scale set"],
  "microsoft.compute/disks": ["DISK", "#d2a8ff", "Managed disk"],
  "microsoft.compute/snapshots": ["SNAP", "#b392f0", "Snapshot"],
  "microsoft.compute/availabilitysets": ["AVSET", "#8b949e", "Availability set"],
  "microsoft.compute/images": ["IMG", "#8b949e", "Image"],
  "microsoft.compute/galleries": ["GAL", "#8b949e", "Compute gallery"],
  "microsoft.compute/sshpublickeys": ["SSH", "#8b949e", "SSH public key"],
  "microsoft.compute/proximityplacementgroups": ["PPG", "#8b949e", "Proximity placement group"],
  "microsoft.network/networksecuritygroups/securityrules": ["RULE", "#f0883e", "Security rule"],
  "microsoft.network/natgateways": ["NAT", "#39c5cf", "NAT gateway"],
  "microsoft.network/routetables/routes": ["ROUTE", "#a5d6ff", "Route"],
  "microsoft.network/virtualnetworks/virtualnetworkpeerings": ["PEER", "#56d4bc", "VNet peering"],
  "microsoft.network/privateendpoints": ["PE", "#79c0ff", "Private endpoint"],
  "microsoft.network/privatednszones": ["PDNS", "#a5d6ff", "Private DNS zone"],
  "microsoft.network/privatednszones/virtualnetworklinks": ["LINK", "#a5d6ff", "Private DNS link"],
  "microsoft.network/dnszones": ["DNS", "#a5d6ff", "DNS zone"],
  "microsoft.network/applicationgateways": ["AGW", "#ffa657", "Application gateway"],
  "microsoft.network/azurefirewalls": ["FW", "#f0883e", "Azure Firewall"],
  "microsoft.network/firewallpolicies": ["FWP", "#f0883e", "Firewall policy"],
  "microsoft.network/bastionhosts": ["BAS", "#d2a8ff", "Bastion host"],
  "microsoft.network/applicationsecuritygroups": ["ASG", "#f0883e", "Application security group"],
  "microsoft.network/virtualnetworkgateways": ["VGW", "#39c5cf", "VNet gateway"],
  "microsoft.network/networkwatchers": ["NW", "#8b949e", "Network watcher"],
  "microsoft.web/sites/slots": ["SLOT", "#388bfd", "Deployment slot"],
  "microsoft.web/staticsites": ["SWA", "#388bfd", "Static web app"],
  "microsoft.insights/components": ["AI", "#bc8cff", "Application Insights"],
  "microsoft.insights/autoscalesettings": ["SCALE", "#8b949e", "Autoscale setting"],
  "microsoft.insights/actiongroups": ["AG", "#8b949e", "Action group"],
  "microsoft.insights/metricalerts": ["ALERT", "#f0883e", "Metric alert"],
  "microsoft.sql/servers": ["SQL", "#3fb950", "SQL server"],
  "microsoft.sql/servers/databases": ["DB", "#2ea043", "SQL database"],
  "microsoft.sql/servers/firewallrules": ["SQLFW", "#8b949e", "SQL firewall rule"],
  "microsoft.documentdb/databaseaccounts": ["COSMOS", "#39c5cf", "Cosmos DB account"],
  "microsoft.cache/redis": ["REDIS", "#f85149", "Redis cache"],
  "microsoft.dbforpostgresql/flexibleservers": ["PG", "#58a6ff", "PostgreSQL server"],
  "microsoft.dbformysql/flexibleservers": ["MYSQL", "#58a6ff", "MySQL server"],
  "microsoft.servicebus/namespaces": ["SB", "#a371f7", "Service Bus namespace"],
  "microsoft.servicebus/namespaces/queues": ["SBQ", "#b392f0", "Service Bus queue"],
  "microsoft.servicebus/namespaces/topics": ["SBT", "#b392f0", "Service Bus topic"],
  "microsoft.eventhub/namespaces": ["EH", "#a371f7", "Event Hubs namespace"],
  "microsoft.eventhub/namespaces/eventhubs": ["HUB", "#b392f0", "Event hub"],
  "microsoft.containerinstance/containergroups": ["ACI", "#58a6ff", "Container group"],
  "microsoft.app/managedenvironments": ["ENV", "#1f6feb", "Container Apps environment"],
  "microsoft.app/containerapps": ["CAPP", "#388bfd", "Container app"],
  "microsoft.logic/workflows": ["FLOW", "#ffa657", "Logic app"],
  "microsoft.authorization/policydefinitions": ["POL", "#8b949e", "Policy definition"],
  "microsoft.authorization/policyassignments": ["PASG", "#8b949e", "Policy assignment"],
};


// Siblings are ordered so a stack reads the way it was built -- network
// before storage before compute -- rather than alphabetically.
export const ORDER = ["subscriptions", "resourcegroups", "virtualnetworks", "subnets", "networksecuritygroups",
  "storageaccounts", "containers", "managedclusters", "agentpools", "nodes", "deployments", "services", "pods"];

// Types that exist only so ARM has something to address. A storage account
// really does have blobServices/default and three siblings -- azurerm polls
// them -- but drawing four "default" boxes under every account is noise.
// They are skipped and their children lifted to the grandparent, so a
// container appears directly under its account, which is how people think
// about it.
export const PASSTHROUGH = /\/(blob|queue|table|file)services$/;

// The type at the top of the tree, which the toolbar's toggle hides.
export const ROOT_TYPE = "microsoft.resources/subscriptions";
export const ROOT_LABEL = "subscriptions";

// The type a resource names when it says which group it belongs to. The
// plan overlay uses it to park a not-yet-created resource in the right
// place; AWS has no equivalent, so it has none.
export const GROUP_TYPE = "microsoft.resources/resourcegroups";

// ROLE_ASSIGNMENT was the FIRST Azure relationship that is not
// containment: a principal somewhere in the graph granted access to a
// resource somewhere else. render.js draws these as dashed links.
export const ROLE_ASSIGNMENT = "microsoft.authorization/roleassignments";

// CONTAINER_APP is the second. An app's ARM parent is its resource group,
// because that is where its id says it lives -- the environment is a
// REFERENCE in properties.managedEnvironmentId, structurally the same kind
// of thing as a role assignment's scope. So it is drawn as a link rather
// than by nesting the app inside the environment, which would make the
// graph disagree with the resource id.
export const CONTAINER_APP = "microsoft.app/containerapps";

// identityLinks returns the from/to pairs to draw for a set of laid-out
// nodes -- every non-containment edge, not only the identity ones it was
// written for. A cloud without such a relationship simply does not export
// it.
export function identityLinks(nodes) {
  // principalId -> the node that OWNS that principal. A user-assigned
  // identity reports it in properties; everything else carries one in
  // `identity`. A role assignment also has a principalId in properties, but
  // it REFERS to a principal rather than owning one -- counting it would
  // make every link start and end at the assignment itself.
  const owners = new Map();
  for (const n of nodes) {
    const type = (n.r.type || "").toLowerCase();
    if (type === ROLE_ASSIGNMENT) continue;
    const body = n.r.body || {};
    const props = body.properties || {};
    if (type === "microsoft.managedidentity/userassignedidentities" && props.principalId) {
      owners.set(String(props.principalId).toLowerCase(), n);
    }
    if (body.identity && body.identity.principalId) {
      owners.set(String(body.identity.principalId).toLowerCase(), n);
    }
  }
  const byKey = new Map(nodes.map((n) => [n.key, n]));
  const out = [];
  for (const n of nodes) {
    if ((n.r.type || "").toLowerCase() !== ROLE_ASSIGNMENT) continue;
    const props = (n.r.body && n.r.body.properties) || {};
    const from = owners.get(String(props.principalId || "").toLowerCase());
    const to = byKey.get(String(props.scope || "").toLowerCase());
    if (!from || !to || from === to) continue;
    out.push({ from, to, label: roleName(props.roleDefinitionId) });
  }

  // A container app to the environment it runs in.
  for (const n of nodes) {
    if ((n.r.type || "").toLowerCase() !== CONTAINER_APP) continue;
    const props = (n.r.body && n.r.body.properties) || {};
    const to = byKey.get(String(props.managedEnvironmentId || "").toLowerCase());
    if (!to || to === n) continue;
    out.push({ from: n, to, label: "environment" });
  }
  return out;
}

// Built-in role names, for labelling assignments and their links. A display
// convenience only: the authoritative table is builtInRoles in
// internal/azure/authorization.go, and anything missing here falls back to
// the GUID, which is still correct, just unfriendly.
export const ROLE_NAMES = {
  "8e3af657-a8ff-443c-a75c-2fe8c4bcb635": "Owner",
  "b24988ac-6180-42a0-ab88-20f7382dd24c": "Contributor",
  "acdd72a7-3385-48ef-bd42-f606fba81ae7": "Reader",
  "18d7d88d-d35e-4fb5-a5c3-7773c20a72d9": "User Access Administrator",
  "00482a5a-887f-4fb3-b363-3b7fe8e74483": "Key Vault Administrator",
  "b86a8fe4-44ce-4948-aee5-eccb2c155cd7": "Key Vault Secrets Officer",
  "4633458b-17de-408a-b874-0445c86b69e6": "Key Vault Secrets User",
  "21090545-7ca7-4776-b22c-e363652d74d2": "Key Vault Reader",
  "7f951dda-4ed3-4680-a7ca-43fe172d538d": "AcrPull",
  "8311e382-0749-4cb8-b61a-304f252e45ec": "AcrPush",
  "ba92f5b4-2d11-453d-a403-e96b0029c9fe": "Storage Blob Data Contributor",
  "2a2b9908-6ea1-4ae2-8e65-a410df84e7d1": "Storage Blob Data Reader",
  "f1a07417-d97a-45cb-824c-7a7467783830": "Managed Identity Operator",
  "4abbcc35-e782-43d8-92c5-2d3f1bd2253f": "AKS Cluster User",
};
export function roleName(roleDefinitionId) {
  const guid = String(roleDefinitionId || "").split("/").pop().toLowerCase();
  return ROLE_NAMES[guid] || guid.slice(0, 8) || "role";
}


// displayName is what a card shows. A role assignment's name is a GUID
// nobody chose; the role it grants is the only part worth reading.
// The type whose children the Kubernetes view draws; see types-aws.js.
export const K8S_CLUSTER_TYPE = "microsoft.containerservice/managedclusters";

export function displayName(r) {
  if ((r.type || "").toLowerCase() === "microsoft.authorization/roleassignments") {
    const props = (r.body && r.body.properties) || {};
    return roleName(props.roleDefinitionId);
  }
  return (r.body && r.body.displayName) ? r.body.displayName + " (" + r.name.slice(0, 8) + ")" : r.name;
}
