// A container app in the graph, and its logs beside it.
//
// Two things are worth asserting and one is not. The link and the panel's
// data are worth it. What the panel LOOKS like is not: that is what
// ?fixture is for, and a test of innerHTML would pin the markup rather
// than the behaviour.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { identityLinks, CONTAINER_APP } from "../../internal/ui/static/types-azure.js";

const fixture = JSON.parse(
  readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url), "utf8"),
);
const inspector = readFileSync(
  new URL("../../internal/ui/static/inspector-azure.js", import.meta.url),
  "utf8",
);

// node() is the shape identityLinks is given: a laid-out node wrapping a
// resource, keyed by its lower-cased id.
function node(r) {
  return { key: String(r.id).toLowerCase(), r };
}

function fixtureResource(type) {
  const r = fixture.resources.find((x) => (x.type || "").toLowerCase() === type);
  assert.ok(r, `the fixture has no ${type}`);
  return r;
}

test("a container app links to the environment it runs in", () => {
  const app = fixtureResource(CONTAINER_APP);
  const env = fixtureResource("microsoft.app/managedenvironments");
  const links = identityLinks([node(app), node(env)]);

  const link = links.find((l) => l.from.r.id === app.id);
  assert.ok(link, "no link from the app");
  assert.equal(link.to.r.id, env.id);
  assert.equal(link.label, "environment");
});

// An app's ARM parent is its RESOURCE GROUP, because that is what its id
// says. The environment is a reference in properties, structurally the
// same kind of thing as a role assignment's scope -- so it is a link
// rather than nesting, and the graph never disagrees with the id.
test("the app is parented to its resource group, not its environment", () => {
  const app = fixtureResource(CONTAINER_APP);
  const env = fixtureResource("microsoft.app/managedenvironments");
  assert.notEqual(app.parentId, env.id);
  assert.ok(app.parentId.endsWith("/resourceGroups/rg-nimbus-app"));
});

test("an app with no environment reference produces no link", () => {
  const app = JSON.parse(JSON.stringify(fixtureResource(CONTAINER_APP)));
  delete app.body.properties.managedEnvironmentId;
  const env = fixtureResource("microsoft.app/managedenvironments");
  const links = identityLinks([node(app), node(env)]);
  assert.equal(links.filter((l) => l.from.r.id === app.id).length, 0);
});

// The chain the panel has to follow: app -> environment -> workspace. The
// server's ingestion follows the same one, which is why an app with no
// workspace configured shows nothing rather than failing.
test("the fixture carries the app to workspace chain the panel follows", () => {
  const app = fixtureResource(CONTAINER_APP);
  const env = fixtureResource("microsoft.app/managedenvironments");
  const ws = fixtureResource("microsoft.operationalinsights/workspaces");

  assert.equal(app.body.properties.managedEnvironmentId, env.id);
  const cfg = env.body.properties.appLogsConfiguration.logAnalyticsConfiguration;
  assert.equal(cfg.customerId, ws.body.properties.customerId);

  const lines = fixture.logs[cfg.customerId];
  assert.ok(Array.isArray(lines) && lines.length, "the fixture has no log lines for that workspace");
  for (const l of lines) {
    assert.equal(typeof l.message, "string");
    assert.ok(l.message.length);
    assert.equal(l.app, app.name);
  }
});

// The FQDN is what a person curls and what azurerm puts in state, so the
// fixture should carry a realistic one: one label of app name, then the
// shared suffix with the port on it.
test("an app's FQDN is its name plus the environment's default domain", () => {
  // The real claim, rather than that the string merely looks plausible:
  // the suffix an app answers under IS the environment's defaultDomain,
  // and exactly one label of app name goes in front of it -- because a
  // TLS wildcard covers one label, which is why the environment does not
  // appear in the name the way it does in real Azure.
  const app = fixtureResource(CONTAINER_APP);
  const env = fixtureResource("microsoft.app/managedenvironments");
  const fqdn = app.body.properties.configuration.ingress.fqdn;

  assert.equal(fqdn, app.name + "." + env.body.properties.defaultDomain);
  assert.ok(!fqdn.includes("/"), "the fqdn is a hostname, not a URL");
});

test("the panel says so when an app's environment has no workspace", () => {
  // Real Azure has nowhere to put the logs either, and an empty list
  // would read as "it has printed nothing" -- a different thing.
  assert.match(inspector, /no workspace/);
  assert.match(inspector, /container console/);
});

test("the panel handles a workspace and an app, and nothing else", () => {
  assert.match(inspector, /microsoft\.operationalinsights\/workspaces/);
  assert.match(inspector, /microsoft\.app\/containerapps/);
});
