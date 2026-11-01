// Nimbus visualizer, entry point. Plain ES modules, no build step, no CDN: it
// ships inside the Go binary and works offline.
//
// This file does three things only -- load the initial state, apply live
// events to it, and wire the controls. Everything else lives in a module:
//
//   types.js      badges, colours and labels per resource type   (pure)
//   graph.js      forest building and tree layout                (pure)
//   state.js      the shared mutable state
//   api.js        every server call, and fixture mode
//   dom.js        $, SVG element helper, tab switching
//   render.js     drawing the topology view
//   feed.js       the activity feed
//   inspector.js  the selected resource, its blobs and its secrets
//   export.js     SVG and PNG download
//
// The pure modules are unit tested by node, in test/ui.

import { $, switchTab } from "./dom.js";
import { state } from "./state.js";
import * as api from "./api.js";
import { addToFeed, clearFeed, setShowReads } from "./feed.js";
import { render, scheduleRender, select } from "./render.js";
import { downloadSVG, downloadPNG } from "./export.js";

// ---- data loading ---------------------------------------------------------
async function loadInfo() {
  try {
    state.info = await api.getInfo();
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
  const list = await api.getResources(state.cloud);
  state.resources.clear();
  for (const r of list) state.resources.set(r.id.toLowerCase(), r);
  render();
}

async function refreshOne(id) {
  const r = await api.getResource(id);
  if (!r) return;
  state.resources.set(r.id.toLowerCase(), r);
  scheduleRender();
}

// ---- live events ----------------------------------------------------------
// Events older than the page are history replayed by the server: they go in
// the feed, but the graph already has the current state from loadAll().
const pageLoadedAt = Date.now();
function onEvent(e) {
  const isReplay = new Date(e.time).getTime() < pageLoadedAt;
  addToFeed(e, !isReplay, select);
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

function connect() {
  let first = true;
  api.subscribe({
    onEvent,
    onOpen: (how) => {
      $("#conn-dot").className = "dot ok";
      $("#conn-text").textContent = how;
      if (!first) loadAll(); // resync after a reconnect
      first = false;
    },
    onError: () => {
      $("#conn-dot").className = "dot down";
      $("#conn-text").textContent = "reconnecting…";
    },
  });
}

// ---- wiring ---------------------------------------------------------------
document.querySelectorAll(".tab").forEach((t) =>
  t.addEventListener("click", () => switchTab(t.dataset.tab)));
$("#filter").addEventListener("input", scheduleRender);
window.addEventListener("resize", scheduleRender);
$("#show-subs").addEventListener("change", scheduleRender);
$("#show-reads").addEventListener("change", (ev) => setShowReads(ev.target.checked));
$("#clear-feed").addEventListener("click", clearFeed);
$("#reset").addEventListener("click", async () => {
  if (!confirm("Delete every emulated resource?")) return;
  await api.reset();
});
$("#export-svg").addEventListener("click", () => downloadSVG($("#graph")));
$("#export-png").addEventListener("click", () => downloadPNG($("#graph")));
$("#copy-json").addEventListener("click", () => {
  const r = state.resources.get(state.selected);
  if (r && navigator.clipboard) navigator.clipboard.writeText(JSON.stringify(r.body, null, 2));
});

loadInfo().then(loadAll).catch(() => {}).finally(connect);
