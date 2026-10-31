// Nimbus visualizer. Plain JavaScript, no build step, no CDN: it ships inside
// the Go binary so it works offline.
(() => {
  "use strict";

  const $ = (sel) => document.querySelector(sel);
  const svgNS = "http://www.w3.org/2000/svg";

  const state = {
    cloud: "azure",
    resources: new Map(), // lowercased id -> resource
    selected: null,
    entered: new Set(),   // ids to animate in on next render
    flashed: new Set(),   // ids to flash on next render
    info: null,
  };

  // ---- resource type styling ---------------------------------------------
  // Short badge + colour per type. Unknown types fall back to the namespace.
  const TYPES = {
    "microsoft.resources/subscriptions": ["SUB", "#a371f7", "Subscription"],
    "microsoft.resources/resourcegroups": ["RG", "#4aa3ff", "Resource group"],
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
  const TIER_LABELS = {
    real: "local engine",
    modelled: "modelled from the API spec",
    state: "accepted, state only",
  };
  const tierOf = (r) =>
    r.backing === "real" ? "real" : r.backing === "modelled" ? "modelled" : "state";

  // Built-in role names, for labelling assignments and their links. A display
  // convenience only: the authoritative table is builtInRoles in
  // internal/azure/authorization.go, and anything missing here falls back to
  // the GUID, which is still correct, just unfriendly.
  const ROLE_NAMES = {
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
  function roleName(roleDefinitionId) {
    const guid = String(roleDefinitionId || "").split("/").pop().toLowerCase();
    return ROLE_NAMES[guid] || guid.slice(0, 8) || "role";
  }

  function typeStyle(type) {
    const t = (type || "").toLowerCase();
    if (TYPES[t]) return TYPES[t];
    const parts = type.split("/");
    const last = parts[parts.length - 1] || type;
    const abbr = last.replace(/[a-z]/g, "").slice(0, 4) || last.slice(0, 3).toUpperCase();
    return [abbr.toUpperCase(), "#8b98a9", last];
  }

  // ---- data loading ---------------------------------------------------------
  async function loadInfo() {
    try {
      state.info = await (await fetch("/api/info")).json();
      const az = state.info.clouds && state.info.clouds.azure;
      if (az) {
        const host = az.replace(/^https?:\/\//, "");
        $("#snippet").textContent =
`provider "azurerm" {
  features {}
  metadata_host   = "${host}"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"
  resource_provider_registrations = "none"
}`;
      }
    } catch (_) { /* server not up yet */ }
  }

  async function loadAll() {
    const list = await (await fetch("/api/resources?cloud=" + state.cloud)).json();
    state.resources.clear();
    for (const r of list) state.resources.set(r.id.toLowerCase(), r);
    render();
  }

  async function refreshOne(id) {
    const res = await fetch("/api/resource?id=" + encodeURIComponent(id));
    if (!res.ok) return;
    const r = await res.json();
    state.resources.set(r.id.toLowerCase(), r);
    scheduleRender();
  }

  // ---- live events ----------------------------------------------------------
  function connect() {
    const es = new EventSource("/api/events");
    let first = true;
    es.onopen = () => {
      $("#conn-dot").className = "dot ok";
      $("#conn-text").textContent = "live";
      if (!first) loadAll(); // resync after a reconnect
      first = false;
    };
    es.onerror = () => {
      $("#conn-dot").className = "dot down";
      $("#conn-text").textContent = "reconnecting…";
    };
    es.onmessage = (msg) => {
      const e = JSON.parse(msg.data);
      onEvent(e);
    };
  }

  // Events older than the page are history replayed by the server: they go in
  // the feed, but the graph already has the current state from loadAll().
  const pageLoadedAt = Date.now();
  function onEvent(e) {
    const isReplay = new Date(e.time).getTime() < pageLoadedAt;
    addToFeed(e, !isReplay);
    if (isReplay) return;
    if (e.kind === "reset") { state.resources.clear(); state.selected = null; scheduleRender(); return; }
    if (e.kind !== "resource" || (e.cloud && e.cloud !== state.cloud)) return;
    const key = e.resourceId.toLowerCase();
    if (e.action === "delete") {
      state.resources.delete(key);
      if (state.selected === key) state.selected = null;
      scheduleRender();
    } else {
      (e.action === "create" ? state.entered : state.flashed).add(key);
      refreshOne(e.resourceId);
    }
  }

  // ---- activity feed --------------------------------------------------------
  const feed = $("#feed");
  function addToFeed(e, animate) {
    const li = document.createElement("li");
    li.dataset.kind = e.kind;
    // Default view: what changed, plus anything that failed or is not implemented.
    // "Show all API calls" adds the raw requests (reads, tokens, writes).
    const flagged = e.kind === "request" && (e.status >= 400 || /unimplemented|unrecognised/.test(e.note || ""));
    const quiet = e.kind === "request" && !flagged;
    if (quiet) li.dataset.quiet = "1";
    if (quiet && !$("#show-reads").checked) li.hidden = true;
    if (animate) li.className = "new";

    const t = document.createElement("time");
    t.textContent = new Date(e.time).toLocaleTimeString([], { hour12: false });
    const verb = document.createElement("span");
    const what = document.createElement("span");
    what.className = "what";

    if (e.kind === "request") {
      verb.className = "verb call";
      verb.textContent = e.method;
      const ok = e.status < 400;
      what.innerHTML = `<span class="${ok ? "code-ok" : "code-err"}">${e.status}</span> <span class="path"></span>`;
      what.querySelector(".path").textContent = shortPath(e.path);
      if (e.note && /unimplemented|unrecognised|accepted/.test(e.note)) {
        const n = document.createElement("span");
        n.className = /accepted/.test(e.note) ? "note quiet" : "note";
        n.textContent = e.note;
        what.appendChild(n);
      }
    } else {
      verb.className = "verb " + e.action;
      verb.textContent = e.action;
      if (e.kind === "reset") {
        what.textContent = "All resources deleted";
      } else {
        const [, , label] = typeStyle(e.resourceType);
        what.innerHTML = `<b></b> <span class="t"></span>`;
        what.querySelector("b").textContent = e.name;
        what.querySelector(".t").textContent = label;
        what.style.cursor = "pointer";
        what.onclick = () => select(e.resourceId.toLowerCase());
      }
    }
    li.append(t, verb, what);
    feed.prepend(li);
    while (feed.children.length > 500) feed.lastChild.remove();
  }

  function shortPath(p) {
    return p.replace(/\/subscriptions\/[^/]+/i, "/sub").replace(/\/resourceGroups\//i, "/rg/").replace(/\/providers\//gi, "/p/");
  }

  // ---- graph layout and rendering -----------------------------------------
  const W = 228, H = 52, GX = 72, GY = 14, PAD = 28;
  let renderQueued = false;
  function scheduleRender() {
    if (renderQueued) return;
    renderQueued = true;
    requestAnimationFrame(() => { renderQueued = false; render(); });
  }

  // Types that exist only so ARM has something to address. A storage account
  // really does have blobServices/default and three siblings -- azurerm polls
  // them -- but drawing four "default" boxes under every account is noise. They
  // are skipped and their children lifted to the grandparent, so a container
  // appears directly under its account, which is how people think about it.
  const PASSTHROUGH = /\/(blob|queue|table|file)services$/;

  function buildForest() {
    const showSubs = $("#show-subs").checked;
    const nodes = new Map();
    const hidden = new Map(); // key -> parentId, for lifting children
    for (const [k, r] of state.resources) {
      const type = r.type.toLowerCase();
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
    const byName = (a, b) => typeOrder(a.r.type) - typeOrder(b.r.type) || a.r.name.localeCompare(b.r.name);
    const sortRec = (list) => { list.sort(byName); list.forEach((n) => sortRec(n.children)); };
    sortRec(roots);
    return roots;
  }

  const ORDER = ["subscriptions", "resourcegroups", "virtualnetworks", "subnets", "networksecuritygroups",
    "storageaccounts", "containers", "managedclusters", "agentpools", "nodes", "deployments", "services", "pods"];
  function typeOrder(type) {
    const last = type.toLowerCase().split("/").pop();
    const i = ORDER.indexOf(last);
    return i === -1 ? ORDER.length : i;
  }

  function layout(roots) {
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

  function el(name, attrs, parent) {
    const e = document.createElementNS(svgNS, name);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }

  function displayName(r) {
    // A role assignment's name is a GUID nobody chose; the role it grants is
    // the only part worth reading.
    if ((r.type || "").toLowerCase() === "microsoft.authorization/roleassignments") {
      const props = (r.body && r.body.properties) || {};
      return roleName(props.roleDefinitionId);
    }
    return (r.body && r.body.displayName) ? r.body.displayName + " (" + r.name.slice(0, 8) + ")" : r.name;
  }

  function fit(text, max) {
    return text.length > max ? text.slice(0, max - 1) + "…" : text;
  }

  function render() {
    const svg = $("#graph");
    svg.innerHTML = "";
    const roots = buildForest();
    const nodes = layout(roots);
    const count = state.resources.size;
    $("#empty").hidden = count > 0;
    // Three tiers, counted separately: the accepted-only number is the one
    // that should keep falling as more types get modelled.
    const all = [...state.resources.values()];
    const real = all.filter((r) => r.backing === "real").length;
    const accepted = all.filter((r) => r.backing !== "real" && r.backing !== "modelled").length;
    $("#counts").textContent =
      `${count} resources · ${real} running locally · ${accepted} accepted only`;

    const filter = $("#filter").value.trim().toLowerCase();
    const matches = (r) => !filter || r.name.toLowerCase().includes(filter) || r.type.toLowerCase().includes(filter);

    let maxX = 0, maxY = 0;
    for (const n of nodes) { maxX = Math.max(maxX, n.x + W); maxY = Math.max(maxY, n.y + H); }
    // Shrink wide trees so the hierarchy fits. The floor is 0.5 rather than
    // something higher because storage adds a fourth level (subscription ->
    // resource group -> account -> container) and at 0.7 the deepest node
    // lands off-screen, which reads as "the container was not created" even
    // though it is there and reachable by scrolling.
    const cw = maxX + PAD, ch = maxY + PAD;
    const avail = $("#canvas").clientWidth || cw;
    const scale = Math.min(1, Math.max(0.5, avail / cw));
    svg.setAttribute("viewBox", `0 0 ${cw} ${ch}`);
    svg.setAttribute("width", Math.round(cw * scale));
    svg.setAttribute("height", Math.round(ch * scale));

    const edges = el("g", {}, svg);
    for (const n of nodes) {
      for (const c of n.children) {
        const x1 = n.x + W, y1 = n.y + H / 2, x2 = c.x, y2 = c.y + H / 2, mx = (x1 + x2) / 2;
        el("path", { class: "edge", d: `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}` }, edges);
      }
    }
    drawIdentityLinks(nodes, el("g", {}, svg));

    for (const n of nodes) {
      const r = n.r;
      const [abbr, color, label] = typeStyle(r.type);
      const classes = ["node", tierOf(r)];
      if (state.selected === n.key) classes.push("selected");
      if (!matches(r)) classes.push("dim");
      if (state.entered.has(n.key)) classes.push("enter");
      if (state.flashed.has(n.key)) classes.push("flash");
      const g = el("g", { class: classes.join(" "), transform: `translate(${n.x},${n.y})`, tabindex: "0" }, svg);
      el("rect", { class: "card", width: W, height: H, rx: 9 }, g);
      el("rect", { x: 10, y: 13, width: 40, height: 26, rx: 6, fill: color, "fill-opacity": 0.16, stroke: color, "stroke-opacity": 0.6 }, g);
      const b = el("text", { class: "badge", x: 30, y: 30.5, "text-anchor": "middle", fill: color }, g);
      b.textContent = abbr;
      const nm = el("text", { class: "nm", x: 62, y: 23 }, g);
      nm.textContent = fit(displayName(r), 20);
      const ty = el("text", { class: "ty", x: 62, y: 40 }, g);
      ty.textContent = fit(label + (r.location ? " · " + r.location : ""), 26);
      const title = el("title", {}, g);
      title.textContent = `${r.name}\n${r.type}\n${r.id}`;
      g.addEventListener("mousedown", (ev) => ev.preventDefault()); // no focus-scroll on click
      g.addEventListener("click", () => select(n.key));
      g.addEventListener("keydown", (ev) => { if (ev.key === "Enter") select(n.key); });
    }
    state.entered.clear();
    state.flashed.clear();
    if (state.selected) showInspector(state.selected);
  }

  // Role assignments are the one relationship that does not follow the
  // parent/child tree: a principal somewhere in the graph is granted access to
  // a resource somewhere else. They are drawn as dashed links so it is
  // visible, without pretending they are containment.
  function drawIdentityLinks(nodes, group) {
    const byKey = new Map(nodes.map((n) => [n.key, n]));
    // principalId -> the node that OWNS that principal. A user-assigned
    // identity reports it in properties; everything else carries one in
    // `identity`. A role assignment also has a principalId in properties, but
    // it REFERS to a principal rather than owning one -- counting it would
    // make every link start and end at the assignment itself.
    const owners = new Map();
    for (const n of nodes) {
      const type = (n.r.type || "").toLowerCase();
      if (type === "microsoft.authorization/roleassignments") continue;
      const body = n.r.body || {};
      const props = body.properties || {};
      if (type === "microsoft.managedidentity/userassignedidentities" && props.principalId) {
        owners.set(String(props.principalId).toLowerCase(), n);
      }
      if (body.identity && body.identity.principalId) {
        owners.set(String(body.identity.principalId).toLowerCase(), n);
      }
    }
    for (const n of nodes) {
      if ((n.r.type || "").toLowerCase() !== "microsoft.authorization/roleassignments") continue;
      const props = (n.r.body && n.r.body.properties) || {};
      const from = owners.get(String(props.principalId || "").toLowerCase());
      const to = byKey.get(String(props.scope || "").toLowerCase());
      if (!from || !to || from === to) continue;
      // Routed around the cards rather than through them: identity and scope
      // are often in the same column, where a straight line between centres
      // would be completely hidden behind the two nodes it connects.
      const y1 = from.y + H / 2, y2 = to.y + H / 2;
      let d;
      if (Math.abs(from.x - to.x) < 1) {
        const x = from.x, bow = Math.max(6, x - 30);
        d = `M${x} ${y1} C${bow} ${y1}, ${bow} ${y2}, ${x} ${y2}`;
      } else {
        const rightwards = from.x < to.x;
        const x1 = rightwards ? from.x + W : from.x;
        const x2 = rightwards ? to.x : to.x + W;
        const mx = (x1 + x2) / 2;
        d = `M${x1} ${y1} C${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`;
      }
      const path = el("path", { class: "link-identity", d }, group);
      const title = el("title", {}, path);
      title.textContent = `${displayName(from.r)} → ${displayName(to.r)}: ${roleName(props.roleDefinitionId)}`;
    }
  }

  // ---- inspector ------------------------------------------------------------
  function select(key) {
    state.selected = key;
    switchTab("inspect");
    render();
  }

  function showInspector(key) {
    const r = state.resources.get(key);
    $("#inspect-empty").hidden = !!r;
    $("#inspect").hidden = !r;
    if (!r) return;
    $("#i-name").textContent = displayName(r);
    $("#i-type").textContent = r.type;
    const meta = $("#i-meta");
    meta.innerHTML = "";
    const rows = [
      ["Backing", TIER_LABELS[tierOf(r)]],
      ["Location", r.location || "—"],
      ["Created", new Date(r.createdAt).toLocaleString()],
      ["Updated", new Date(r.updatedAt).toLocaleString()],
      ["ID", r.id],
    ];
    for (const [k, v] of rows) {
      const dt = document.createElement("dt"); dt.textContent = k;
      const dd = document.createElement("dd"); dd.textContent = v;
      meta.append(dt, dd);
    }
    $("#i-json").innerHTML = highlight(JSON.stringify(r.body, null, 2));
    showBlobs(r);
    showSecrets(r);
  }

  // Blobs are not graph nodes -- a container can hold thousands -- so they are
  // fetched for whichever container is selected and listed here instead.
  async function showBlobs(r) {
    const panel = $("#i-blobs");
    const type = (r.type || "").toLowerCase();
    if (!type.endsWith("/blobservices/containers")) {
      panel.hidden = true;
      return;
    }
    const account = accountOf(r.id);
    panel.hidden = false;
    $("#i-blobs-list").innerHTML = "<div class='muted'>Loading…</div>";
    let blobs = [];
    try {
      const res = await fetch(`/api/storage/blobs?account=${encodeURIComponent(account)}&container=${encodeURIComponent(r.name)}`);
      blobs = await res.json();
    } catch (e) {
      $("#i-blobs-list").innerHTML = "<div class='muted'>Could not load blobs.</div>";
      return;
    }
    $("#i-blobs-count").textContent = blobs.length === 1 ? "1 blob" : `${blobs.length} blobs`;
    if (!blobs.length) {
      $("#i-blobs-list").innerHTML = "<div class='muted'>This container is empty.</div>";
      return;
    }
    const list = document.createElement("div");
    for (const b of blobs) {
      const row = document.createElement("div");
      row.className = "blob-row";
      const a = document.createElement("a");
      a.href = `/api/storage/blob?account=${encodeURIComponent(account)}&container=${encodeURIComponent(r.name)}&name=${encodeURIComponent(b.name)}`;
      a.textContent = b.name;
      a.title = "Download";
      const size = document.createElement("span");
      size.className = "blob-size";
      size.textContent = humanSize(b.size);
      row.append(a, size);
      list.appendChild(row);
    }
    $("#i-blobs-list").innerHTML = "";
    $("#i-blobs-list").appendChild(list);
  }

  // Secrets are listed for whichever vault is selected, like blobs are for a
  // container. Only NAMES and metadata are fetched: the value has its own
  // endpoint, called when someone presses reveal, so a selected vault never
  // puts secrets on screen -- or in a screenshot -- unasked.
  async function showSecrets(r) {
    const panel = $("#i-secrets");
    if ((r.type || "").toLowerCase() !== "microsoft.keyvault/vaults") {
      panel.hidden = true;
      return;
    }
    panel.hidden = false;
    $("#i-secrets-list").innerHTML = "<div class='muted'>Loading…</div>";
    let secrets = [];
    try {
      const res = await fetch(`/api/keyvault/secrets?vault=${encodeURIComponent(r.name)}`);
      secrets = await res.json();
    } catch (e) {
      $("#i-secrets-list").innerHTML = "<div class='muted'>Could not load secrets.</div>";
      return;
    }
    $("#i-secrets-count").textContent = secrets.length === 1 ? "1 secret" : `${secrets.length} secrets`;
    if (!secrets.length) {
      $("#i-secrets-list").innerHTML = "<div class='muted'>This vault is empty.</div>";
      return;
    }
    const list = document.createElement("div");
    for (const s of secrets) {
      const row = document.createElement("div");
      row.className = "secret-row";
      const name = document.createElement("span");
      name.className = "secret-name" + (s.deleted ? " secret-deleted" : "");
      name.textContent = s.name;
      name.title = s.deleted ? "Deleted, recoverable" : (s.contentType || "");
      const right = document.createElement("span");
      right.className = "secret-meta";
      const versions = document.createElement("span");
      versions.textContent = s.versions === 1 ? "1 version" : `${s.versions} versions`;
      const reveal = document.createElement("button");
      reveal.className = "btn ghost small";
      reveal.textContent = "Reveal";
      reveal.addEventListener("click", async () => {
        reveal.disabled = true;
        reveal.textContent = "…";
        try {
          const res = await fetch(`/api/keyvault/secret?vault=${encodeURIComponent(r.name)}&name=${encodeURIComponent(s.name)}`);
          if (!res.ok) throw new Error(res.status);
          const body = await res.json();
          const value = document.createElement("span");
          value.className = "secret-value";
          value.textContent = body.value === "" ? "(empty)" : body.value;
          reveal.replaceWith(value);
        } catch (e) {
          reveal.disabled = false;
          reveal.textContent = "Reveal";
          reveal.title = "Could not read this secret";
        }
      });
      right.append(versions, document.createTextNode(" "), reveal);
      row.append(name, right);
      list.appendChild(row);
    }
    $("#i-secrets-list").innerHTML = "";
    $("#i-secrets-list").appendChild(list);
  }

  // The account name sits between /storageAccounts/ and the next segment.
  function accountOf(id) {
    const m = /\/storageAccounts\/([^/]+)/i.exec(id || "");
    return m ? m[1] : "";
  }

  function humanSize(n) {
    if (n < 1024) return n + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / 1024 / 1024).toFixed(1) + " MB";
  }

  function highlight(json) {
    const esc = json.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
    return esc.replace(/("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(\.\d*)?([eE][+-]?\d+)?)/g, (m) => {
      let cls = "n";
      if (/^"/.test(m)) cls = /:$/.test(m) ? "k" : "s";
      else if (/true|false|null/.test(m)) cls = "b";
      return `<span class="${cls}">${m}</span>`;
    });
  }

  // ---- wiring ---------------------------------------------------------------
  function switchTab(name) {
    document.querySelectorAll(".tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
    document.querySelectorAll(".panel").forEach((p) => p.classList.toggle("active", p.id === "panel-" + name));
  }
  document.querySelectorAll(".tab").forEach((t) => t.addEventListener("click", () => switchTab(t.dataset.tab)));
  $("#filter").addEventListener("input", scheduleRender);
  window.addEventListener("resize", scheduleRender);
  $("#show-subs").addEventListener("change", scheduleRender);
  $("#show-reads").addEventListener("change", (ev) => {
    feed.querySelectorAll("li[data-quiet]").forEach((li) => { li.hidden = !ev.target.checked; });
  });
  $("#clear-feed").addEventListener("click", () => { feed.innerHTML = ""; });
  $("#reset").addEventListener("click", async () => {
    if (!confirm("Delete every emulated resource?")) return;
    await fetch("/api/reset", { method: "POST" });
  });
  $("#copy-json").addEventListener("click", () => {
    const r = state.resources.get(state.selected);
    if (r && navigator.clipboard) navigator.clipboard.writeText(JSON.stringify(r.body, null, 2));
  });

  loadInfo().then(loadAll).catch(() => {}).finally(connect);
})();
