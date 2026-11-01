// Every call to the Nimbus server goes through here, which is what makes
// fixture mode possible: with ?fixture in the URL the whole app reads a
// committed JSON file instead of the API, so the visualizer can be opened and
// checked -- in another browser, in light mode, at phone width -- without
// running Terraform, Docker, or even Nimbus itself.

const params = new URLSearchParams(location.search);
export const fixtureMode = params.has("fixture");

let fixturePromise = null;
function fixture() {
  if (!fixturePromise) fixturePromise = fetch("fixture.json").then((r) => r.json());
  return fixturePromise;
}

async function json(url) {
  const res = await fetch(url);
  if (!res.ok) throw new Error(url + ": " + res.status);
  return res.json();
}

export async function getInfo() {
  if (fixtureMode) return (await fixture()).info;
  return json("/api/info");
}

export async function getResources(cloud) {
  if (fixtureMode) {
    return (await fixture()).resources.filter((r) => !cloud || r.cloud === cloud);
  }
  return json("/api/resources?cloud=" + encodeURIComponent(cloud));
}

export async function getResource(id) {
  if (fixtureMode) {
    const want = id.toLowerCase();
    return (await fixture()).resources.find((r) => r.id.toLowerCase() === want) || null;
  }
  try {
    return await json("/api/resource?id=" + encodeURIComponent(id));
  } catch (_) {
    return null;
  }
}

export async function reset() {
  if (fixtureMode) return;
  await fetch("/api/reset", { method: "POST" });
}

// Subscribe to the event stream. In fixture mode the committed events are
// delivered once; they are all in the past, so the app treats them as history
// and fills the activity feed without touching the graph.
export function subscribe({ onEvent, onOpen, onError }) {
  if (fixtureMode) {
    fixture().then((f) => {
      onOpen("fixture");
      for (const e of f.events || []) onEvent(e);
    });
    return;
  }
  const es = new EventSource("/api/events");
  es.onopen = () => onOpen("live");
  es.onerror = () => onError();
  es.onmessage = (msg) => onEvent(JSON.parse(msg.data));
}

export async function getBlobs(account, container) {
  if (fixtureMode) {
    const f = await fixture();
    return (f.blobs || {})[account + "/" + container] || [];
  }
  return json(`/api/storage/blobs?account=${encodeURIComponent(account)}&container=${encodeURIComponent(container)}`);
}

export function blobURL(account, container, name) {
  return `/api/storage/blob?account=${encodeURIComponent(account)}&container=${encodeURIComponent(container)}&name=${encodeURIComponent(name)}`;
}

export async function getSecrets(vault) {
  if (fixtureMode) return ((await fixture()).secrets || {})[vault] || [];
  return json("/api/keyvault/secrets?vault=" + encodeURIComponent(vault));
}

export async function getSecret(vault, name) {
  if (fixtureMode) return { value: "(fixture)" };
  return json(`/api/keyvault/secret?vault=${encodeURIComponent(vault)}&name=${encodeURIComponent(name)}`);
}

export async function getRuns() {
  if (fixtureMode) return ((await fixture()).runs || []).map((r) => ({ ...r, events: undefined }));
  return json("/api/runs");
}

export async function getRun(id) {
  if (fixtureMode) return ((await fixture()).runs || []).find((r) => r.id === id) || null;
  try {
    return await json("/api/run?id=" + encodeURIComponent(id));
  } catch (_) {
    return null;
  }
}
