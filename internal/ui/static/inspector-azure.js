// The inspector panels Azure contributes.
//
// They exist because Azure's data planes do: a container holds blobs and a
// vault holds secrets, and both have an endpoint on the visualizer's own
// API. Another cloud's panels (an S3 bucket's objects, say) belong in its
// own module, which is why these are not in inspector.js.

import { $ } from "./dom.js";
import { getBlobs, blobURL, getSecrets, getSecret, getLogs, getResource } from "./api.js";

// Blobs are not graph nodes -- a container can hold thousands -- so they are
// fetched for whichever container is selected and listed here instead.
async function showBlobs(r) {
  const panel = $("#i-blobs");
  // A null resource means "not your cloud": hide and say nothing.
  const type = r ? (r.type || "").toLowerCase() : "";
  if (!type.endsWith("/blobservices/containers")) {
    panel.hidden = true;
    return;
  }
  const account = accountOf(r.id);
  panel.hidden = false;
  $("#i-blobs-list").innerHTML = "<div class='muted'>Loading…</div>";
  let blobs = [];
  try {
    blobs = await getBlobs(account, r.name);
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
    a.href = blobURL(account, r.name, b.name);
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
  if (!r || (r.type || "").toLowerCase() !== "microsoft.keyvault/vaults") {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;
  $("#i-secrets-list").innerHTML = "<div class='muted'>Loading…</div>";
  let secrets = [];
  try {
    secrets = await getSecrets(r.name);
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
        const body = await getSecret(r.name, s.name);
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

// panels is what inspector.js calls for a selected Azure resource.
// A workspace's lines, and a container app's own output.
//
// Beside the resource that produced them, which is the thing a log viewer
// never gives you: `docker logs` knows the container and not the app, and
// a query needs a customerId somebody has to find first. Selecting the app
// is the shortest path from "what is this" to "what has it been doing".
//
// A workspace shows everything sent to it; an app shows its own lines,
// which means resolving the workspace its ENVIRONMENT names -- the same
// indirection the server's ingestion does, and the reason an app with no
// workspace configured shows nothing here rather than an error.
async function showLogs(r) {
  const panel = $("#i-logs");
  const type = r ? (r.type || "").toLowerCase() : "";
  const isWorkspace = type === "microsoft.operationalinsights/workspaces";
  const isApp = type === "microsoft.app/containerapps";
  if (!isWorkspace && !isApp) {
    panel.hidden = true;
    return;
  }

  const props = (r.body && r.body.properties) || {};
  let workspace = isWorkspace ? props.customerId : await workspaceOfApp(props);
  if (!workspace) {
    // An app whose environment names no workspace. Real Azure has
    // nowhere to put its logs either; saying so beats an empty list that
    // reads as "it has printed nothing".
    panel.hidden = false;
    $("#i-logs-count").textContent = "no workspace";
    $("#i-logs-list").innerHTML =
      "<div class='muted'>This app's environment has no Log Analytics workspace, " +
      "so its output stays in the container console only.</div>";
    return;
  }

  panel.hidden = false;
  $("#i-logs-list").innerHTML = "<div class='muted'>Loading…</div>";
  let rows = [];
  try {
    rows = await getLogs(workspace, isApp ? r.name : "");
  } catch (e) {
    $("#i-logs-list").innerHTML = "<div class='muted'>Could not load logs.</div>";
    return;
  }
  $("#i-logs-count").textContent = rows.length === 1 ? "1 line" : `${rows.length} lines`;
  if (!rows.length) {
    $("#i-logs-list").innerHTML = "<div class='muted'>Nothing has been logged yet.</div>";
    return;
  }
  $("#i-logs-list").replaceChildren(...rows.map(logRow));
}

// workspaceOfApp follows an app to its environment to its workspace.
async function workspaceOfApp(props) {
  const envID = props.managedEnvironmentId;
  if (!envID) return "";
  try {
    const env = await getResource(envID);
    const cfg = ((env.body || {}).properties || {}).appLogsConfiguration || {};
    return (cfg.logAnalyticsConfiguration || {}).customerId || "";
  } catch (e) {
    return "";
  }
}

function logRow(row) {
  const el = document.createElement("div");
  el.className = "log-row";
  const time = document.createElement("span");
  time.className = "log-time muted";
  // Time only: the date is almost always today and the column is narrow.
  time.textContent = String(row.time || "").slice(11, 19);
  const msg = document.createElement("span");
  msg.className = "log-msg";
  msg.textContent = row.message;
  el.append(time, msg);
  return el;
}

export const panels = [showBlobs, showSecrets, showLogs];
