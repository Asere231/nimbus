// The plan overlay in the browser: matching pending changes to cards, and
// inventing ghost cards for the creates that have no ID yet.

import { test } from "node:test";
import assert from "node:assert/strict";

import { planIndex, ghosts, withGhosts, actionFor, summarise, GHOST_PREFIX, MARKS }
  from "../../internal/ui/static/plan.js";
import { buildForest } from "../../internal/ui/static/graph.js";

const SUB = "/subscriptions/00000000-0000-0000-0000-000000000000";
const RG = SUB + "/resourceGroups/rg-platform-dev";
const VNET = RG + "/providers/Microsoft.Network/virtualNetworks/vnet-hub-dev";

function resources(...list) {
  return new Map(list.map((r) => [r.id.toLowerCase(), r]));
}
const res = (id, type, name, parentId) =>
  ({ cloud: "azure", id, type, name, parentId, backing: "modelled", body: {} });

const live = () => resources(
  res(RG, "Microsoft.Resources/resourceGroups", "rg-platform-dev", SUB),
  res(VNET, "Microsoft.Network/virtualNetworks", "vnet-hub-dev", RG),
);

// The shape internal/ui/plan.go produces from the real plan in
// test/ui/testdata/terraform-plan.json. Its own Go test pins that
// conversion; this pins what the browser does with the result.
const overlay = {
  source: "plan.out", creates: 1, updates: 1, deletes: 0, replaces: 0,
  changes: [
    {
      address: "azurerm_public_ip.egress", action: "create",
      type: "Microsoft.Network/publicIPAddresses", name: "pip-egress",
      resourceGroup: "rg-platform-dev",
    },
    {
      address: "azurerm_virtual_network.hub", action: "update", id: VNET,
      type: "Microsoft.Network/virtualNetworks", name: "vnet-hub-dev",
      resourceGroup: "rg-platform-dev",
    },
  ],
};

test("a change with an ID marks the card it names", () => {
  const index = planIndex(overlay);
  const map = live();
  assert.equal(actionFor(index, map.get(VNET.toLowerCase())), "update");
  assert.equal(actionFor(index, map.get(RG.toLowerCase())), "", "nothing is planned for the group");
  assert.equal(actionFor(index, undefined), "");
});

test("IDs are matched case-insensitively, as ARM does", () => {
  const index = planIndex({ changes: [{ address: "a.b", action: "delete", id: VNET.toUpperCase() }] });
  assert.equal(actionFor(index, live().get(VNET.toLowerCase())), "delete");
});

// The create in the real plan has no id -- Terraform lists it under
// after_unknown -- so it cannot match a card and has to become one.
test("a create with no ID becomes a ghost in the group it will appear in", () => {
  const list = ghosts(overlay, live());
  assert.equal(list.length, 1, "only the create needs a ghost");
  const g = list[0];
  assert.equal(g.name, "pip-egress");
  assert.equal(g.type, "Microsoft.Network/publicIPAddresses");
  assert.equal(g.parentId, RG, "parented to the resource group the plan names");
  assert.ok(g.id.startsWith(GHOST_PREFIX), "and marked as not real");
  assert.equal(g.ghost, true);
  assert.equal(actionFor(planIndex(overlay), g), "create");
});

test("a ghost hangs off its resource group in the tree", () => {
  const roots = buildForest(withGhosts(overlay, live()));
  const group = roots[0].children ? roots[0] : null;
  // Root is the resource group here, since no subscription is in the map.
  assert.equal(roots.length, 1);
  assert.equal(roots[0].r.name, "rg-platform-dev");
  const names = roots[0].children.map((n) => n.r.name).sort();
  assert.deepEqual(names, ["pip-egress", "vnet-hub-dev"]);
  assert.ok(group);
});

test("a create for a group that is not on screen still appears, as a root", () => {
  const elsewhere = {
    changes: [{ address: "azurerm_public_ip.x", action: "create", type: "Microsoft.Network/publicIPAddresses",
      name: "pip-x", resourceGroup: "rg-somewhere-else" }],
  };
  const list = ghosts(elsewhere, live());
  assert.equal(list.length, 1);
  assert.equal(list[0].parentId, "", "no group to hang it off, so it stands alone rather than vanishing");
});

test("a create whose type nobody mapped is labelled with its address", () => {
  const unmapped = {
    changes: [{ address: "azurerm_something_new.x", action: "create", type: "", name: "" }],
  };
  const g = ghosts(unmapped, live())[0];
  assert.equal(g.name, "azurerm_something_new.x");
  assert.equal(g.type, "");
});

test("withGhosts never touches the live map", () => {
  const map = live();
  const merged = withGhosts(overlay, map);
  assert.equal(map.size, 2, "the store is state; a plan is a prediction");
  assert.equal(merged.size, 3);
  assert.notEqual(merged, map);
  // With nothing to add, the same map comes back untouched.
  assert.equal(withGhosts({ changes: [] }, map), map);
  assert.equal(withGhosts(null, map), map);
});

test("marks are Terraform's own notation", () => {
  assert.equal(MARKS.create, "+");
  assert.equal(MARKS.update, "~");
  assert.equal(MARKS.delete, "-");
  assert.equal(MARKS.replace, "-/+");
});

test("the bar summarises the plan the way terraform does", () => {
  assert.match(summarise(overlay), /1 to create, 1 to change/);
  assert.match(summarise(overlay), /plan\.out/);
  assert.equal(summarise({ changes: [] }), "", "nothing pending, nothing to say");
  assert.equal(summarise(null), "");
});

// AWS and GCP have no resource group, so a pending create there used to
// float beside the graph with no parent at all. They DO have a single root
// -- an account, a project -- and every resource hangs off it.
const AWS_ACCOUNT = "arn:aws:::000000000000";
const GCP_PROJECT = "http://localhost:4703/v1/projects/nimbus-local";

test("an AWS create is parented to the account, not left floating", () => {
  const map = new Map([
    [AWS_ACCOUNT.toLowerCase(),
      { cloud: "aws", id: AWS_ACCOUNT, type: "AWS::Account", name: "000000000000", parentId: "", body: {} }],
  ]);
  const list = ghosts({
    changes: [{ address: "aws_vpc.main", action: "create", type: "AWS::EC2::VPC", name: "nimbus-vpc" }],
  }, map, "aws");
  assert.equal(list.length, 1);
  assert.equal(list[0].parentId, AWS_ACCOUNT, "the ghost should sit under the account");
  assert.equal(list[0].type, "AWS::EC2::VPC");
});

test("a GCP create is parented to the project", () => {
  const map = new Map([
    [GCP_PROJECT.toLowerCase(),
      { cloud: "gcp", id: GCP_PROJECT, type: "cloudresourcemanager.googleapis.com/Project",
        name: "nimbus-local", parentId: "", body: {} }],
  ]);
  const list = ghosts({
    changes: [{
      address: "google_compute_network.net", action: "create",
      type: "compute.googleapis.com/Network", name: "nimbus-net",
    }],
  }, map, "gcp");
  assert.equal(list.length, 1);
  assert.equal(list[0].parentId, GCP_PROJECT, "the ghost should sit under the project");
});

// With two roots, picking either would be a guess. A floating card is the
// honest answer -- and this cannot happen today, which is exactly why it
// needs a test rather than a comment.
test("two roots mean no parent is guessed", () => {
  const map = new Map([
    ["a", { cloud: "aws", id: "a", type: "AWS::Account", name: "one", parentId: "", body: {} }],
    ["b", { cloud: "aws", id: "b", type: "AWS::Account", name: "two", parentId: "", body: {} }],
  ]);
  const list = ghosts({
    changes: [{ address: "aws_vpc.main", action: "create", type: "AWS::EC2::VPC", name: "v" }],
  }, map, "aws");
  assert.equal(list[0].parentId, "", "a ghost with an ambiguous root must not be parented");
});

// The root fallback is for clouds with NO group type. A cloud that has one
// must not reach for its root instead -- an Azure create whose group is
// not on screen stays unparented even with the subscription right there,
// because "in rg-somewhere-else" is information the subscription would
// throw away.
//
// This replaces a test that asserted an Azure create prefers its group
// over the subscription, which could not fail: the root is never looked up
// for a cloud with a group type, so both orderings gave the same answer.
// Found by injecting the swap and watching nothing break.
test("a cloud with a group type never falls back to its root", () => {
  const map = resources(
    res(SUB, "Microsoft.Resources/subscriptions", "Nimbus Local", ""),
    res(RG, "Microsoft.Resources/resourceGroups", "rg-platform-dev", SUB),
  );
  const elsewhere = {
    changes: [{
      address: "azurerm_public_ip.x", action: "create",
      type: "Microsoft.Network/publicIPAddresses", name: "pip-x",
      resourceGroup: "rg-somewhere-else",
    }],
  };
  const ghost = ghosts(elsewhere, map)[0];
  assert.equal(ghost.parentId, "",
    "the subscription is not a substitute for the group the plan named");
});
