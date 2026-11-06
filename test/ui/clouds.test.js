// The visualizer is multi-cloud: presentation is dispatched on each
// resource's own `cloud`, and the views a cloud offers depend on what that
// cloud actually has. These are the tests that would have caught the Azure
// assumptions the single-cloud version was full of.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync, readdirSync } from "node:fs";

import { typeStyle, typeOrder, displayName, isPassthrough, isRoot, rootLabel, cloudOf, tierOf, isK8sCluster }
  from "../../internal/ui/static/types.js";
import { buildForest } from "../../internal/ui/static/graph.js";
import * as azureTable from "../../internal/ui/static/types-azure.js";
import * as awsTable from "../../internal/ui/static/types-aws.js";

const res = (cloud, id, type, name, parentId = "") =>
  ({ cloud, id, type, name, parentId, backing: "state", body: {} });

test("each cloud is styled from its own table", () => {
  const [azAbbr, , azHuman] = typeStyle("Microsoft.Network/virtualNetworks", "azure");
  assert.equal(azAbbr, "VNET");
  assert.equal(azHuman, "Virtual network");

  const [awsAbbr, , awsHuman] = typeStyle("AWS::EC2::VPC", "aws");
  assert.equal(awsAbbr, "VPC");
  assert.equal(awsHuman, "Virtual private cloud");
});

// The bug this prevents: one shared table, where an AWS type silently picks
// up an Azure badge or the other way round.
test("a type is not styled by another cloud's table", () => {
  const [abbr, , human] = typeStyle("Microsoft.Network/virtualNetworks", "aws");
  assert.notEqual(abbr, "VNET");
  assert.equal(human, "virtualNetworks", "it falls back to abbreviating the segment");
  assert.equal(typeStyle("AWS::EC2::VPC", "azure")[2], "VPC");
});

test("an unknown or missing cloud still draws, without badges", () => {
  assert.equal(cloudOf("gcp"), null);
  assert.doesNotThrow(() => typeStyle("Google::Compute::Network", "gcp"));
  assert.doesNotThrow(() => typeStyle("x", undefined));
  assert.doesNotThrow(() => displayName({ name: "n" }));
  assert.equal(displayName({ cloud: "gcp", name: "n" }), "n");
});

test("each cloud names its own root container", () => {
  assert.ok(isRoot("Microsoft.Resources/subscriptions", "azure"));
  assert.ok(!isRoot("Microsoft.Resources/subscriptions", "aws"));
  assert.ok(isRoot("AWS::Account", "aws"));
  assert.equal(rootLabel("azure"), "subscriptions");
  assert.equal(rootLabel("aws"), "account");
  assert.equal(rootLabel("gcp"), "roots", "an unknown cloud still gets a label for the toggle");
});

// Two clouds' resources live in one store and one graph. They must nest
// within themselves and not across.
test("resources from two clouds build one forest with two roots", () => {
  const SUB = "/subscriptions/s";
  const RG = SUB + "/resourceGroups/rg";
  const ACCT = "arn:aws:::000000000000:account";
  const VPC = "vpc-0a1b2c3d";
  const resources = new Map([
    [SUB.toLowerCase(), res("azure", SUB, "Microsoft.Resources/subscriptions", "s")],
    [RG.toLowerCase(), res("azure", RG, "Microsoft.Resources/resourceGroups", "rg", SUB)],
    [ACCT.toLowerCase(), res("aws", ACCT, "AWS::Account", "000000000000")],
    [VPC.toLowerCase(), res("aws", VPC, "AWS::EC2::VPC", "main", ACCT)],
  ]);
  const roots = buildForest(resources);
  assert.equal(roots.length, 2);
  for (const r of roots) assert.equal(r.children.length, 1);

  // Hiding roots hides each cloud's own.
  assert.equal(buildForest(resources, { showRoots: false }).length, 2);
  assert.deepEqual(
    buildForest(resources, { showRoots: false }).map((n) => n.r.name).sort(),
    ["main", "rg"],
  );
});

test("the tiers mean the same thing whatever the cloud", () => {
  assert.equal(tierOf({ cloud: "aws", backing: "real" }), "real");
  assert.equal(tierOf({ cloud: "aws" }), "state");
});

// Ordering is per cloud, so adding a type to one list cannot reorder the
// other's graph.
test("sibling order comes from the resource's own cloud", () => {
  assert.ok(typeOrder("AWS::EC2::VPC", "aws") < typeOrder("AWS::EC2::Subnet", "aws"));
  assert.ok(typeOrder("Microsoft.Network/virtualNetworks", "azure") <
    typeOrder("Microsoft.Storage/storageAccounts", "azure"));
});

// Every cloud table has to carry the same shape, or a view will reach for a
// field that one of them does not have.
test("every cloud table exports the same shape", () => {
  for (const [name, table] of [["azure", azureTable], ["aws", awsTable]]) {
    assert.equal(typeof table.TYPES, "object", name + " TYPES");
    assert.ok(Array.isArray(table.ORDER), name + " ORDER");
    assert.equal(typeof table.ROOT_TYPE, "string", name + " ROOT_TYPE");
    assert.equal(typeof table.ROOT_LABEL, "string", name + " ROOT_LABEL");
    assert.equal(typeof table.displayName, "function", name + " displayName");
    assert.ok(table.PASSTHROUGH === null || table.PASSTHROUGH instanceof RegExp, name + " PASSTHROUGH");
    // Keys are lower-cased, because every lookup lower-cases the type.
    for (const key of Object.keys(table.TYPES)) {
      assert.equal(key, key.toLowerCase(), `${name}: ${key} must be lower case or it can never match`);
    }
    assert.ok(table.TYPES[table.ROOT_TYPE], name + " should style its own root type");
    // Both clouds run managed Kubernetes on the same engine, so both have
    // to name the type the Kubernetes view groups objects under.
    assert.equal(typeof table.K8S_CLUSTER_TYPE, "string", name + " K8S_CLUSTER_TYPE");
    assert.ok(table.TYPES[table.K8S_CLUSTER_TYPE], name + " should style its own cluster type");
  }
});

// No module outside types-*.js may name a cloud's types, or the next cloud
// has to hunt them down the way this refactor just did.
test("only the per-cloud tables mention a cloud's own type names", () => {
  const dir = new URL("../../internal/ui/static/", import.meta.url);
  // A module whose NAME says which cloud it is may name that cloud's types
  // -- types-azure.js, inspector-azure.js. So may a view render.js declares
  // as one cloud's only. Everything else has to ask types.js.
  const named = /-(azure|aws|gcp)\.js$/;
  const declaredAzureViews = new Set(["network.js"]);
  const offenders = [];
  for (const file of readdirSync(dir)) {
    if (!file.endsWith(".js") || named.test(file) || declaredAzureViews.has(file)) continue;
    const src = readFileSync(new URL(file, dir), "utf8");
    // Comments explain Azure freely; code must not key on its type names.
    const code = src.replace(/\/\/[^\n]*/g, "").replace(/\/\*[\s\S]*?\*\//g, "");
    if (/["'`](microsoft\.|aws::)/i.test(code)) offenders.push(file);
  }
  assert.deepEqual(offenders, [],
    "these modules hard-code cloud type names and should ask types.js instead");
});

// A Kubernetes cluster is each cloud's own type, and recognising the other
// cloud's would draw an EKS cluster's pods inside an AKS one.
test("a managed cluster is recognised only in its own cloud", () => {
  assert.ok(isK8sCluster("Microsoft.ContainerService/managedClusters", "azure"));
  assert.ok(isK8sCluster("AWS::EKS::Cluster", "aws"));
  assert.ok(!isK8sCluster("AWS::EKS::Cluster", "azure"));
  assert.ok(!isK8sCluster("Microsoft.ContainerService/managedClusters", "aws"));
  assert.ok(!isK8sCluster("AWS::EKS::Nodegroup", "aws"));
  assert.ok(!isK8sCluster(undefined, undefined));
});
