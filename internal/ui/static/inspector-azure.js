// The inspector panels Azure contributes.
//
// They exist because Azure's data planes do: a container holds blobs and a
// vault holds secrets, and both have an endpoint on the visualizer's own
// API. Another cloud's panels (an S3 bucket's objects, say) belong in its
// own module, which is why these are not in inspector.js.

import { $ } from "./dom.js";
import { getBlobs, blobURL, getSecrets, getSecret } from "./api.js";

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
export const panels = [showBlobs, showSecrets];
