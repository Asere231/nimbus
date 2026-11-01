// The replay engine: rebuilding the graph as it stood partway through a run.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { steps, stateAt, callFor, summarise } from "../../internal/ui/static/replay.js";
import { buildForest } from "../../internal/ui/static/graph.js";

const RG = "/subscriptions/s/resourceGroups/rg";
const VNET = RG + "/providers/Microsoft.Network/virtualNetworks/v";

let clock = 0;
function ev(e) {
  clock += 1000;
  return { time: new Date(1790000000000 + clock).toISOString(), cloud: "azure", ...e };
}
const created = (id, type, name, parentId) =>
  ev({ kind: "resource", action: "create", resourceId: id, resourceType: type, name, parentId });
const deleted = (id, type, name, parentId) =>
  ev({ kind: "resource", action: "delete", resourceId: id, resourceType: type, name, parentId });

test("steps keeps only what changed, in order", () => {
  const events = [
    ev({ kind: "request", action: "call", method: "POST", path: "/token", status: 200 }),
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    ev({ kind: "request", action: "call", method: "PUT", path: RG, status: 201 }),
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
  ];
  assert.deepEqual(steps(events).map((e) => e.name), ["rg", "v"]);
  assert.deepEqual(steps([]), []);
  assert.deepEqual(steps(undefined), []);
});

test("stateAt rebuilds the graph one step at a time", () => {
  const events = [
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
  ];
  assert.equal(stateAt(events, 0).size, 0, "before the first step nothing exists");
  assert.equal(stateAt(events, 1).size, 1);
  assert.equal(stateAt(events, 2).size, 2);
  assert.equal(stateAt(events, 99).size, 2, "past the end is the end");
  assert.equal(stateAt(events, -1).size, 0, "before the start is the start");
});

// The one property the whole timeline rests on: an event names the resource
// completely, so a replayed card can be drawn with no store lookup at all.
test("a replayed resource can be drawn from its event alone", () => {
  const events = [
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
  ];
  const map = stateAt(events, 2);
  const roots = buildForest(map);
  assert.equal(roots.length, 1, "the replayed resources still form a tree");
  assert.equal(roots[0].r.name, "rg");
  assert.equal(roots[0].children[0].r.name, "v");
  assert.equal(roots[0].children[0].backing, undefined);
  assert.equal(map.get(VNET.toLowerCase()).backing, "state",
    "with no store entry to borrow from, a replayed card is drawn as accepted-only");
});

test("a resource deleted and created again does not leave a ghost", () => {
  const events = [
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
    deleted(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
  ];
  assert.equal(stateAt(events, 1).size, 1);
  assert.equal(stateAt(events, 2).size, 0, "the delete must remove it");
  assert.equal(stateAt(events, 3).size, 1, "and the re-create must bring back exactly one");
});

test("a reset clears everything replayed so far", () => {
  const events = [
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    created(VNET, "Microsoft.Network/virtualNetworks", "v", RG),
    ev({ kind: "reset", action: "reset" }),
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
  ];
  assert.equal(stateAt(events, 2).size, 2);
  assert.equal(stateAt(events, 3).size, 0);
  assert.equal(stateAt(events, 4).size, 1);
});

test("a resource that still exists is replayed with its real body", () => {
  const live = new Map([[VNET.toLowerCase(), {
    id: VNET, type: "Microsoft.Network/virtualNetworks", name: "v", parentId: RG,
    backing: "modelled", body: { properties: { addressSpace: { addressPrefixes: ["10.0.0.0/16"] } } },
  }]]);
  const map = stateAt([created(VNET, "Microsoft.Network/virtualNetworks", "v", RG)], 1, live);
  const r = map.get(VNET.toLowerCase());
  assert.equal(r.backing, "modelled");
  assert.ok(r.body.properties, "the live body should be used when the resource is still there");
});

// The request that caused a change is logged just AFTER it: the store
// publishes the change mid-request, and ServeHTTP publishes the request once
// it has a status and a duration. Looking backwards showed the previous,
// unrelated call -- visible on a real apply as the route table's PUT sitting
// under the creation of the NSG.
test("callFor finds the request that caused a step, which comes after it", () => {
  const events = [
    ev({ kind: "request", action: "call", method: "POST", path: "/token", status: 200 }),
    created(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    ev({ kind: "request", action: "call", method: "PUT", path: RG, status: 201 }),
  ];
  const call = callFor(events, 0);
  assert.equal(call.method, "PUT", "the PUT that made the change, not the token call before it");
  assert.equal(call.path, RG);
  assert.equal(callFor([], 0), null);

  // A change with no request anywhere near it -- the subscription Nimbus
  // creates at startup, or a pod the cluster watcher noticed -- has no call
  // behind it, and inventing one would be a lie.
  const alone = [
    { kind: "resource", action: "create", resourceId: RG, resourceType: "x", name: "rg",
      time: new Date(1790000000000).toISOString() },
    { kind: "request", action: "call", method: "GET", path: "/other", status: 200,
      time: new Date(1790000060000).toISOString() },
  ];
  assert.equal(callFor(alone, 0), null, "a request a minute later did not cause this");
  assert.equal(callFor([created(RG, "x", "rg", "")], 0), null);
});

// Three resources created inside the same millisecond is normal -- azurerm
// PUTs independent resources concurrently -- and a JavaScript Date cannot
// tell them apart, so the order has to come from the array the server sent.
test("callFor uses event order, not timestamps, within a millisecond", () => {
  const same = new Date(1790000000000).toISOString();
  const events = [
    { kind: "resource", action: "create", resourceId: "/rt", resourceType: "x", name: "rt", time: same },
    { kind: "request", action: "call", method: "PUT", path: "/rt", status: 201, time: same },
    { kind: "resource", action: "create", resourceId: "/nsg", resourceType: "x", name: "nsg", time: same },
    { kind: "request", action: "call", method: "PUT", path: "/nsg", status: 201, time: same },
  ];
  assert.equal(callFor(events, 0).path, "/rt");
  assert.equal(callFor(events, 1).path, "/nsg", "the NSG's own PUT, not the route table's");
});

test("summarise describes a run in one line", () => {
  const line = summarise({
    start: "2026-10-28T15:04:11Z", end: "2026-10-28T15:04:53Z",
    created: 18, updated: 0, deleted: 0, requests: 42, steps: 18,
  });
  assert.match(line, /18 created/);
  assert.match(line, /42s/);
  assert.match(summarise({ start: "2026-10-28T15:04:11Z", end: "2026-10-28T15:04:11Z" }), /no changes/);
});

// The fixture's run is what ?fixture replays, and it is also the only run
// grouping the browser ever sees that Go did not compute -- so it is checked
// against the same rule here.
test("the fixture's run holds every event and steps through each change", () => {
  const doc = JSON.parse(readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url)));
  assert.equal(doc.runs.length, 1, "the fixture is one apply");
  const run = doc.runs[0];
  assert.equal(run.events.length, doc.events.length, "the run should hold the fixture's whole event list");
  assert.equal(run.steps, steps(run.events).length);
  assert.equal(run.steps, run.created + run.updated + run.deleted);

  // Nothing in it may be further apart than core.RunGap, or the server would
  // split this into two runs and the fixture would describe itself
  // differently from the way Nimbus describes a real apply.
  const RUN_GAP_MS = 30000; // internal/core/runs.go
  const times = run.events.map((e) => new Date(e.time).getTime()).sort((a, b) => a - b);
  for (let i = 1; i < times.length; i++) {
    assert.ok(times[i] - times[i - 1] < RUN_GAP_MS,
      `events ${i - 1} and ${i} are ${times[i] - times[i - 1]}ms apart: the server would make these two runs`);
  }

  // Replaying it to the end must reproduce the fixture's own topology.
  const final = stateAt(run.events, run.steps);
  assert.equal(final.size, doc.resources.length,
    "replaying the whole run should end up with every resource in the fixture");
});
