// Resource type presentation: the badge, colour and human label for every type
// Nimbus knows about, plus the three backing tiers and the built-in role
// names. Pure data and pure functions -- no DOM, so this module is unit
// tested directly by node.
//
// Short badge + colour per type. Unknown types fall back to an abbreviation
// of the last path segment, so a type nobody has styled still gets a card
// that reads sensibly.
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
// How much Nimbus really knows about a resource: a local engine runs it,
// its shape is modelled (a hook or Microsoft's own API spec), or it is
// merely accepted and stored.
export const TIER_LABELS = {
  real: "local engine",
  modelled: "modelled from the API spec",
  state: "accepted, state only",
};
export const tierOf = (r) =>
  r.backing === "real" ? "real" : r.backing === "modelled" ? "modelled" : "state";

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

export function typeStyle(type) {
  const src = type || "";
  const t = src.toLowerCase();
  if (TYPES[t]) return TYPES[t];
  const last = src.split("/").pop() || src;
  const abbr = last.replace(/[a-z]/g, "").slice(0, 4) || last.slice(0, 3).toUpperCase();
  return [abbr.toUpperCase(), "#8b98a9", last];
}

export function displayName(r) {
  // A role assignment's name is a GUID nobody chose; the role it grants is
  // the only part worth reading.
  if ((r.type || "").toLowerCase() === "microsoft.authorization/roleassignments") {
    const props = (r.body && r.body.properties) || {};
    return roleName(props.roleDefinitionId);
  }
  return (r.body && r.body.displayName) ? r.body.displayName + " (" + r.name.slice(0, 8) + ")" : r.name;
}
