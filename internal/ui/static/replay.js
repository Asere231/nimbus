// Rebuilding the graph as it stood partway through a run.
//
// The whole timeline rests on one property of core.Event: a resource event
// carries the id, type, name and parent of what changed. That is everything a
// card needs, so a run can be replayed from its events alone -- including the
// resources it created and then destroyed, which are not in the store any
// more and cannot be looked up.
//
// What an event does NOT carry is the resource body or its backing tier. Both
// are taken from the live store when the resource still exists, so replaying
// a run that built what is on screen gives the real thing; anything since
// deleted gets a plain card, correctly labelled, with no body to inspect.
//
// Pure: no DOM, no fetch. Tested by node.

// steps returns the events a scrubber moves through -- the ones that changed
// something, in the order they happened.
export function steps(events) {
  return (events || []).filter((e) => e.kind === "resource" || e.kind === "reset");
}

// stateAt returns the resource map as it stood after applying the first
// `count` steps. `live` is the current store, used to fill in bodies.
export function stateAt(events, count, live = new Map()) {
  const out = new Map();
  const list = steps(events);
  for (const e of list.slice(0, Math.max(0, count))) {
    if (e.kind === "reset") {
      out.clear();
      continue;
    }
    const key = String(e.resourceId || "").toLowerCase();
    if (!key) continue;
    if (e.action === "delete") {
      out.delete(key);
      continue;
    }
    const known = live.get(key);
    out.set(key, known || {
      cloud: e.cloud || "azure",
      id: e.resourceId,
      type: e.resourceType || "",
      name: e.name || "",
      parentId: e.parentId || "",
      location: "",
      // Nothing in the event says which tier this was: "state" is the
      // honest answer for a resource we can no longer look up, and it is
      // also the tier that draws as a dashed outline, which reads correctly
      // as "this is gone".
      backing: "state",
      body: {},
      createdAt: e.time,
      updatedAt: e.time,
    });
  }
  return out;
}

// CALL_WINDOW is how far either side of a step callFor will look.
const CALL_WINDOW = 2000;

// callFor returns the API call that caused step `i` -- which is the NEXT
// request event, not the previous one.
//
// That is backwards from how it reads, and it cost a wrong answer on screen
// before it was noticed. A resource event is published by the store in the
// middle of handling a request, while the request event is published by
// ServeHTTP once the response has been written and its duration is known
// (internal/azure/server.go). So the PUT that created a resource is logged
// microseconds AFTER the creation it caused, and looking backwards finds
// whatever the client happened to ask for beforehand.
//
// Nothing is shown when there is no request close by: a change can come from
// inside Nimbus rather than off the wire -- the default subscription at
// startup, or the cluster watcher mirroring a pod -- and those genuinely
// have no call behind them.
// Order comes from the ARRAY, not from the timestamps. The server sorts a
// run's events with nanosecond precision, and a JavaScript Date keeps only
// milliseconds -- three creates inside the same millisecond all compare
// equal, which put the route table's PUT under the creation of the NSG.
// Timestamps are still used for the window, where a millisecond does not
// matter.
export function callFor(events, i) {
  const list = steps(events);
  if (!list.length) return null;
  const step = list[Math.min(Math.max(i, 0), list.length - 1)];
  const from = events.indexOf(step);
  if (from < 0) return null;
  const when = new Date(step.time).getTime();
  for (let k = from + 1; k < events.length; k++) {
    const e = events[k];
    if (e.kind !== "request") continue;
    if (new Date(e.time).getTime() - when > CALL_WINDOW) break;
    return e;
  }
  return null;
}

// summarise is the one-line description of a run for the picker.
export function summarise(run) {
  const start = new Date(run.start);
  const secs = Math.max(0, Math.round((new Date(run.end) - start) / 1000));
  const parts = [];
  if (run.created) parts.push(`${run.created} created`);
  if (run.updated) parts.push(`${run.updated} updated`);
  if (run.deleted) parts.push(`${run.deleted} deleted`);
  return `${start.toLocaleTimeString([], { hour12: false })} · ${parts.join(", ") || "no changes"} · ${secs}s`;
}
