// The inspector panel: everything about the selected resource, plus the two
// data-plane listings (a container's blobs, a vault's secrets) that are too
// numerous to be graph nodes.

import { $ } from "./dom.js";
import { TIER_LABELS, tierOf, displayName } from "./types.js";
import { panels as azurePanels } from "./inspector-azure.js";
import { panels as awsPanels } from "./inspector-aws.js";
import { showAccess } from "./access.js";

// Each cloud contributes the extra panels its data planes justify: Azure's
// blob and secret listings, and whatever AWS grows. A resource is shown the
// panels of its own cloud and no others, and every panel decides for itself
// whether it applies.
const PANELS = { azure: azurePanels, aws: awsPanels };

export function showInspector(r) {
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
  // Access is shared rather than per-cloud: the server answers in one
  // shape for all three, so there is nothing here to dispatch on.
  void showAccess(r);
  for (const name of Object.keys(PANELS)) {
    const mine = name === String(r.cloud || "").toLowerCase();
    for (const panel of PANELS[name]) panel(mine ? r : null);
  }
}

export function highlight(json) {
  const esc = String(json).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  return esc.replace(/("(\\u[a-fA-F0-9]{4}|\\[^u]|[^\\"])*"(\s*:)?|\b(true|false|null)\b|-?\d+(\.\d*)?([eE][+-]?\d+)?)/g, (m) => {
    let cls = "n";
    if (/^"/.test(m)) cls = /:$/.test(m) ? "k" : "s";
    else if (/true|false|null/.test(m)) cls = "b";
    return `<span class="${cls}">${m}</span>`;
  });
}
