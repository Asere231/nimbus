// Unit tests for the visualizer's pure modules, run by node's own test
// runner:
//
//	node --test 'test/ui/*.test.js'
//
// They live here rather than beside the source because internal/ui/static is
// embedded into the binary wholesale by //go:embed -- a test file in there
// would ship to every user.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { buildForest, layout, extent, W, H, PAD }
  from "../../internal/ui/static/graph.js";
import { typeStyle, tierOf, displayName, typeOrder, isPassthrough }
  from "../../internal/ui/static/types.js";
import { roleName } from "../../internal/ui/static/types-azure.js";

// resources builds the Map shape the app keeps: lowercased id -> resource.
function resources(...list) {
  return new Map(list.map((r) => [r.id.toLowerCase(), r]));
}

const SUB = "/subscriptions/s1";
const RG = SUB + "/resourceGroups/rg";
const ST = RG + "/providers/Microsoft.Storage/storageAccounts/st1";
const BSVC = ST + "/blobServices/default";
const CONT = BSVC + "/containers/data";

function res(id, type, name, parentId, extra = {}) {
  return { cloud: "azure", id, type, name, parentId, backing: "state", body: {}, ...extra };
}

test("buildForest nests children under their parent", () => {
  const roots = buildForest(resources(
    res(SUB, "Microsoft.Resources/subscriptions", "s1", ""),
    res(RG, "Microsoft.Resources/resourceGroups", "rg", SUB),
  ));
  assert.equal(roots.length, 1);
  assert.equal(roots[0].r.name, "s1");
  assert.equal(roots[0].children.length, 1);
  assert.equal(roots[0].children[0].r.name, "rg");
});

test("buildForest matches parents case-insensitively, as ARM IDs are", () => {
  const roots = buildForest(resources(
    res(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/v", "Microsoft.Network/virtualNetworks", "v",
      RG.toUpperCase()),
  ));
  assert.equal(roots.length, 1, "the VNet should not be a root: its parent exists in another casing");
  assert.equal(roots[0].children.length, 1);
});

test("buildForest lifts a container past the blobServices node that holds it", () => {
  const roots = buildForest(resources(
    res(ST, "Microsoft.Storage/storageAccounts", "st1", ""),
    res(BSVC, "Microsoft.Storage/storageAccounts/blobServices", "default", ST),
    res(CONT, "Microsoft.Storage/storageAccounts/blobServices/containers", "data", BSVC),
  ));
  assert.equal(roots.length, 1);
  const account = roots[0];
  assert.equal(account.r.name, "st1");
  assert.equal(account.children.length, 1, "blobServices itself must not be drawn");
  assert.equal(account.children[0].r.name, "data", "the container hangs off the account directly");
});

test("passthrough covers all four Azure storage services and nothing else", () => {
  for (const svc of ["blob", "queue", "table", "file"]) {
    assert.ok(isPassthrough(`microsoft.storage/storageaccounts/${svc}services`, "azure"), svc);
  }
  assert.ok(!isPassthrough("microsoft.storage/storageaccounts", "azure"));
  assert.ok(!isPassthrough("microsoft.storage/storageaccounts/blobservices/containers", "azure"));
  // AWS has no addressing-only types, and an unknown cloud must not throw.
  assert.ok(!isPassthrough("aws::s3::bucket", "aws"));
  assert.ok(!isPassthrough("whatever", "nope"));
  assert.ok(!isPassthrough(undefined, undefined));
});

test("showRoots false makes resource groups roots", () => {
  const all = resources(
    res(SUB, "Microsoft.Resources/subscriptions", "s1", ""),
    res(RG, "Microsoft.Resources/resourceGroups", "rg", SUB),
  );
  assert.equal(buildForest(all, { showRoots: false }).length, 1);
  assert.equal(buildForest(all, { showRoots: false })[0].r.name, "rg");
});

test("siblings are ordered by type, then by name", () => {
  const roots = buildForest(resources(
    res(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    res(RG + "/providers/Microsoft.Storage/storageAccounts/st", "Microsoft.Storage/storageAccounts", "st", RG),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/vb", "Microsoft.Network/virtualNetworks", "vb", RG),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/va", "Microsoft.Network/virtualNetworks", "va", RG),
  ));
  assert.deepEqual(roots[0].children.map((n) => n.r.name), ["va", "vb", "st"]);
});

test("typeOrder puts known types first and unknown ones last", () => {
  const order = (t) => typeOrder(t, "azure");
  assert.ok(order("Microsoft.Resources/subscriptions") < order("Microsoft.Storage/storageAccounts"));
  assert.ok(order("Microsoft.Storage/storageAccounts") < order("Contoso.Thing/widgets"));
  assert.equal(order(undefined), order("Contoso.Thing/widgets"), "a missing type must not throw");
  // Each cloud orders by its own list.
  assert.ok(typeOrder("AWS::EC2::VPC", "aws") < typeOrder("AWS::S3::Bucket", "aws"));
  assert.equal(typeOrder("AWS::EC2::VPC", "azure"), typeOrder("Contoso.Thing/widgets", "azure"),
    "an AWS type means nothing to Azure's ordering");
});

test("layout puts each level in its own column and centres a parent on its children", () => {
  const roots = buildForest(resources(
    res(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/va", "Microsoft.Network/virtualNetworks", "va", RG),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/vb", "Microsoft.Network/virtualNetworks", "vb", RG),
  ));
  const nodes = layout(roots);
  assert.equal(nodes.length, 3);
  const rgNode = nodes.find((n) => n.r.name === "rg");
  const kids = rgNode.children;
  assert.equal(rgNode.x, PAD, "a root sits in the first column");
  assert.ok(kids[0].x > rgNode.x, "a child sits to the right of its parent");
  assert.equal(kids[0].x, kids[1].x, "siblings share a column");
  assert.equal(rgNode.y, (kids[0].y + kids[1].y) / 2, "the parent is centred on its children");
  assert.ok(extent(nodes).width >= kids[0].x + W + PAD);
  assert.ok(extent(nodes).height >= kids[1].y + H);
});

test("layout returns parents after their children, so edges draw over cards", () => {
  const roots = buildForest(resources(
    res(RG, "Microsoft.Resources/resourceGroups", "rg", ""),
    res(RG + "/providers/Microsoft.Network/virtualNetworks/v", "Microsoft.Network/virtualNetworks", "v", RG),
  ));
  const names = layout(roots).map((n) => n.r.name);
  assert.deepEqual(names, ["v", "rg"]);
});

test("typeStyle falls back to an abbreviation for an unstyled type", () => {
  const [abbr, color, human] = typeStyle("Contoso.Widgets/superWidgets", "azure");
  assert.equal(human, "superWidgets");
  assert.equal(abbr, "W", "the capitals of the last segment: superWidgets has exactly one");
  assert.ok(color.startsWith("#"));
  // With no capitals to take there is nothing to abbreviate, so the first
  // three letters stand in rather than the badge coming out empty.
  assert.equal(typeStyle("Contoso.Widgets/widgets", "azure")[0], "WID");
  assert.doesNotThrow(() => typeStyle(undefined, undefined), "a missing type must not throw");
});

test("tierOf maps anything unknown to the accepted-only tier", () => {
  assert.equal(tierOf({ backing: "real" }), "real");
  assert.equal(tierOf({ backing: "modelled" }), "modelled");
  assert.equal(tierOf({ backing: "state" }), "state");
  assert.equal(tierOf({}), "state");
});

test("a role assignment is labelled by the role it grants, not its GUID name", () => {
  const r = res("/x", "Microsoft.Authorization/roleAssignments", "7b6a5948-3e2d-41c0-9f8e-7d6c5b4a3928", "", {
    body: { properties: { roleDefinitionId: "/subscriptions/s/providers/Microsoft.Authorization/roleDefinitions/7f951dda-4ed3-4680-a7ca-43fe172d538d" } },
  });
  assert.equal(displayName(r), "AcrPull");
  assert.equal(roleName("/a/b/00000000-1111-2222-3333-444444444444"), "00000000",
    "an unknown role falls back to the start of its GUID");
});

// The committed fixture is what the app renders with ?fixture, and what the
// browser checks are done against. If it ever stops being a well-formed
// topology the page looks broken for reasons that have nothing to do with the
// code, so it is checked here.
test("fixture.json is a complete, well-parented topology", () => {
  const raw = readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url));
  const doc = JSON.parse(raw);
  assert.ok(doc.resources.length > 15, "the fixture should be a real stack, not two boxes");

  const byID = new Map(doc.resources.map((r) => [r.id.toLowerCase(), r]));
  for (const r of doc.resources) {
    assert.ok(r.id && r.type && r.name, "every resource needs an id, type and name: " + r.id);
    assert.ok(["state", "modelled", "real"].includes(r.backing), r.id + " has backing " + r.backing);
    if (r.parentId) {
      assert.ok(byID.has(r.parentId.toLowerCase()), r.id + " points at a parent that is not in the fixture");
    }
  }
  // One tree PER CLOUD, which is what the app draws: the topology view
  // filters to the selected cloud before building the forest, so a forest
  // built across all of them would have a root each and say nothing. This
  // used to assert a single root, back when the fixture was Azure only.
  const clouds = [...new Set(doc.resources.map((r) => r.cloud))];
  assert.ok(clouds.length >= 3, "the fixture should cover all three clouds");
  let drawn = 0;
  for (const cloud of clouds) {
    const mine = doc.resources.filter((r) => r.cloud === cloud);
    const roots = buildForest(new Map(mine.map((r) => [r.id.toLowerCase(), r])));
    assert.equal(roots.length, 1,
      cloud + " should draw as one tree under its own root container");
    drawn += layout(roots).length;
  }
  assert.ok(drawn > 15);

  // Every fixture event must predate the page, or the app would treat it as
  // live and try to fetch resources that only exist in the fixture.
  for (const e of doc.events) {
    assert.ok(new Date(e.time).getTime() < Date.now(), "fixture events must be in the past");
  }
});

// Each cloud's ROOT container is "accepted (state only)" in the server --
// a subscription, an account and a project are all stored with
// Backing: "state", because nothing creates them and nothing runs behind
// them. The fixture said "modelled" for Azure's, which drew it with a
// solid border while the other two were dashed.
//
// Nobody could see that while the fixture had one cloud in it; putting
// three side by side is what made it obvious. The fixture is what this
// view is eyeballed against, so it has to be accurate about the thing it
// is demonstrating.
test("every cloud's root container is state-only, as the server stores it", () => {
  const doc = JSON.parse(readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url)));
  const roots = doc.resources.filter((r) => !r.parentId);
  assert.equal(roots.length, 3, "one root per cloud");
  for (const r of roots) {
    assert.equal(r.backing, "state", r.type + " should be accepted (state only)");
  }
});
