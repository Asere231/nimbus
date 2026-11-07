// Nimbus visualizer, entry point. Plain ES modules, no build step, no CDN: it
// ships inside the Go binary and works offline.
//
// This file does three things only -- load the initial state, apply live
// events to it, and wire the controls. Everything else lives in a module:
//
//   types.js      badges, colours and labels, dispatched per cloud (pure)
//   types-*.js    one table per cloud                            (pure)
//   graph.js      forest building and tree layout                (pure)
//   state.js      the shared mutable state
//   api.js        every server call, and fixture mode
//   dom.js        $, SVG element helper, tab switching
//   render.js     drawing the topology view
//   feed.js       the activity feed
//   inspector.js  the selected resource, its blobs and its secrets
//   export.js     SVG and PNG download
//   network.js    the network view: VNets, subnets and peerings
//   kubernetes.js the Kubernetes view: namespaces, workloads and services
//   plan.js       the terraform plan overlay                     (pure)
//   replay.js     rebuilding the graph partway through a run  (pure)
//   timeline.js   the replay bar under the canvas
//
// The pure modules are unit tested by node, in test/ui.

import { $, switchTab } from "./dom.js";
import { state } from "./state.js";
import * as api from "./api.js";
import { addToFeed, clearFeed, setShowReads } from "./feed.js";
import { render, scheduleRender, select, viewsFor } from "./render.js";
import { downloadSVG, downloadPNG } from "./export.js";
import { initTimeline, refreshRuns, isReplaying } from "./timeline.js";
import { summarise as summarisePlan } from "./plan.js";

// ---- clouds and views -----------------------------------------------------
// Which clouds exist is not a fact the page can know: it depends on what
// `nimbus start --clouds` was told to run. /api/info reports the endpoints,
// and the tabs are built from that, so a build with one cloud shows one tab
// and nothing is greyed out pretending to be coming later.
const CLOUD_LABELS = { azure: "Azure", aws: "AWS", gcp: "GCP" };

// SNIPPETS is the provider block shown on an empty graph, per cloud.
const SNIPPETS = {
  azure: (url) => `provider "azurerm" {
  features {}
  metadata_host   = "${url.replace(/^https?:\/\//, "")}"
  subscription_id = "00000000-0000-0000-0000-000000000000"
  tenant_id       = "00000000-0000-0000-0000-000000000001"
  client_id       = "00000000-0000-0000-0000-000000000002"
  client_secret   = "nimbus"
  resource_provider_registrations = "none"
}`,
  aws: (url) => `provider "aws" {
  region                      = "us-east-1"
  access_key                  = "nimbus"
  secret_key                  = "nimbus"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  s3_use_path_style           = true

  endpoints {
    sts = "${url}"
    iam = "${url}"
    ec2 = "${url}"
    s3  = "${url}"
  }
}`,
  gcp: (url) => `provider "google" {
  project      = "nimbus-local"
  region       = "us-central1"
  access_token = "nimbus"

  compute_custom_endpoint          = "${url}/compute/v1/"
  storage_custom_endpoint          = "${url}/storage/v1/"
  iam_custom_endpoint              = "${url}/"
  resource_manager_custom_endpoint = "${url}/v1/"
}`,
};

function renderCloudTabs() {
  const clouds = Object.keys((state.info && state.info.clouds) || {}).sort();
  const nav = $("#clouds");
  nav.innerHTML = "";
  if (!clouds.includes(state.cloud)) state.cloud = clouds[0] || state.cloud;
  for (const name of clouds) {
    const b = document.createElement("button");
    b.className = "cloud" + (name === state.cloud ? " active" : "");
    b.dataset.cloud = name;
    b.textContent = CLOUD_LABELS[name] || name;
    b.addEventListener("click", () => selectCloud(name));
    nav.appendChild(b);
  }
  renderViewTabs();
  showSnippet();
}

// The switcher offers the views this cloud has: a VPC is not a VNet, so the
// network and Kubernetes views are Azure's and simply are not there for AWS.
function renderViewTabs() {
  const available = viewsFor(state.cloud);
  if (!available.some((v) => v.id === state.view)) state.view = available[0].id;
  const nav = $("#views");
  nav.innerHTML = "";
  for (const v of available) {
    const b = document.createElement("button");
    b.className = "view" + (v.id === state.view ? " active" : "");
    b.dataset.view = v.id;
    b.textContent = v.label;
    b.addEventListener("click", () => {
      state.view = v.id;
      renderViewTabs();
      render();
    });
    nav.appendChild(b);
  }
}

function showSnippet() {
  const url = ((state.info && state.info.clouds) || {})[state.cloud];
  const make = SNIPPETS[state.cloud];
  $("#snippet").textContent = url && make ? make(url) : "";
}

async function selectCloud(name) {
  if (name === state.cloud) return;
  state.cloud = name;
  state.selected = null;
  state.replay = null;
  renderCloudTabs();
  await loadAll();
}

// ---- data loading ---------------------------------------------------------
async function loadInfo() {
  try {
    state.info = await api.getInfo();
    renderCloudTabs();
  } catch (_) { /* server not up yet */ }
}

async function loadAll() {
  const list = await api.getResources(state.cloud);
  state.resources.clear();
  for (const r of list) state.resources.set(r.id.toLowerCase(), r);
  render();
}

// The run list is rebuilt a moment after activity stops, rather than on every
// event: a run does not exist until the gap that ends it has passed, so
// asking sooner just returns the same list again.
let runsTimer = null;
function runsSoon() {
  if (runsTimer) clearTimeout(runsTimer);
  runsTimer = setTimeout(refreshRuns, 6000);
}

async function refreshOne(id) {
  const r = await api.getResource(id);
  if (!r) return;
  state.resources.set(r.id.toLowerCase(), r);
  if (!isReplaying()) scheduleRender();
}

// The plan overlay is pushed in by `nimbus plan-view` while the page is
// already open, so it is polled rather than waited for. Half a minute is
// plenty: a plan is something you look at, not something that races.
async function loadPlan() {
  const overlay = await api.getPlan();
  const had = state.plan && (state.plan.changes || []).length;
  state.plan = overlay && (overlay.changes || []).length ? overlay : null;
  $("#planbar").hidden = !state.plan;
  $("#plan-summary").textContent = state.plan ? summarisePlan(state.plan) : "";
  if (!!state.plan !== !!had) render();
  else if (state.plan) render();
}

// ---- live events ----------------------------------------------------------
// Events older than the page are history replayed by the server: they go in
// the feed, but the graph already has the current state from loadAll().
const pageLoadedAt = Date.now();
function onEvent(e) {
  const isReplay = new Date(e.time).getTime() < pageLoadedAt;
  addToFeed(e, !isReplay, select);
  if (isReplay) return;
  runsSoon();
  if (e.kind === "reset") { state.resources.clear(); state.selected = null; if (!isReplaying()) scheduleRender(); return; }
  if (e.kind !== "resource" || (e.cloud && e.cloud !== state.cloud)) return;
  const key = e.resourceId.toLowerCase();
  if (e.action === "delete") {
    state.resources.delete(key);
    if (state.selected === key) state.selected = null;
    if (!isReplaying()) scheduleRender();
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
$("#plan-clear").addEventListener("click", async () => { await api.clearPlan(); await loadPlan(); });
$("#export-svg").addEventListener("click", () => downloadSVG($("#graph")));
$("#export-png").addEventListener("click", () => downloadPNG($("#graph")));
$("#copy-json").addEventListener("click", () => {
  const r = state.resources.get(state.selected);
  if (r && navigator.clipboard) navigator.clipboard.writeText(JSON.stringify(r.body, null, 2));
});

initTimeline();
loadPlan().catch(() => {});
setInterval(() => loadPlan().catch(() => {}), 30000);
loadInfo().then(loadAll).catch(() => {}).finally(connect);
