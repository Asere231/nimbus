// The network view's model: what the stored ARM bodies say about addressing
// and filtering, rearranged into VNets, subnets, attachments and peerings.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { networkModel, networkLayout } from "../../internal/ui/static/network.js";

const RG = "/subscriptions/s/resourceGroups/rg";
const P = RG + "/providers/Microsoft.Network";
const VNET = P + "/virtualNetworks/vnet-hub";
const NSG = P + "/networkSecurityGroups/nsg-app";
const RT = P + "/routeTables/rt-app";

// map() flattens one level, so a helper can return a resource AND its
// children together -- vnet() returns the VNet and its subnets, because in
// the store they are separate resources and the view reads them that way.
function map(...list) {
  return new Map(list.flat().map((r) => [r.id.toLowerCase(), r]));
}

function res(id, type, name, body, extra = {}) {
  return { cloud: "azure", id, type, name, parentId: RG, location: "eastus", backing: "modelled", body, ...extra };
}

// A VNet AND its subnet resources. Azure stores subnets twice -- mirrored
// into the VNet's body, because that is what azurerm reads back, and as
// resources of their own -- so the fixture-like shape here carries both,
// and the view reads the resources.
function vnet(subnets = [], prefixes = ["10.20.0.0/16"]) {
  const v = res(VNET, "Microsoft.Network/virtualNetworks", "vnet-hub", {
    properties: {
      addressSpace: { addressPrefixes: prefixes },
      // The mirror, as ARM really returns it: the subnet's own body under
      // its id and name.
      subnets: subnets.map((sn) => ({ id: sn.id, name: sn.name, properties: sn.body.properties })),
    },
  });
  return [v, ...subnets];
}

// A subnet is a RESOURCE that names its VNet by parent id -- the one rule
// all three clouds share, which is what lets one view serve them.
const subnet = (name, prefix, props = {}) =>
  res(VNET + "/subnets/" + name, "Microsoft.Network/virtualNetworks/subnets", name,
    { properties: { addressPrefix: prefix, ...props } }, { parentId: VNET });

test("a VNet becomes a container holding its subnets", () => {
  const m = networkModel(map(vnet([subnet("snet-apps", "10.20.0.0/24"), subnet("snet-data", "10.20.1.0/24")])), "azure");
  assert.equal(m.vnets.length, 1);
  const v = m.vnets[0];
  assert.equal(v.name, "vnet-hub");
  assert.deepEqual(v.prefixes, ["10.20.0.0/16"]);
  assert.deepEqual(v.subnets.map((s) => [s.name, s.prefix]),
    [["snet-apps", "10.20.0.0/24"], ["snet-data", "10.20.1.0/24"]]);
});

// Azure keeps subnets in TWO places: the VNet's body mirrors them, because
// that is what azurerm reads back (the parent/child sync in CLAUDE.md), and
// each is also a resource of its own. Whichever the view reads, a subnet
// must appear ONCE.
//
// The view reads the child resources, because that is the one rule all
// three clouds share -- a subnet is a resource that names its network, by
// parent id here, by vpcId on AWS and by a self link on GCP. This test used
// to assert the mechanism (read the body); it now asserts the property,
// which is what actually matters and holds either way.
test("a subnet is counted once, though Azure stores it in two places", () => {
  // vnet() produces BOTH: the mirrored entry in the VNet's body and the
  // subnet resource. One subnet goes in, so one must come out.
  const m = networkModel(map(vnet([subnet("snet-apps", "10.20.0.0/24")])), "azure");
  assert.equal(m.vnets[0].subnets.length, 1, "the mirror must not add a second one");
  assert.equal(m.vnets[0].subnets[0].prefix, "10.20.0.0/24");
});

test("an NSG and a route table attach to the subnet that names them", () => {
  const m = networkModel(map(
    vnet([subnet("snet-data", "10.20.1.0/24", {
      networkSecurityGroup: { id: NSG }, routeTable: { id: RT },
    })]),
    res(NSG, "Microsoft.Network/networkSecurityGroups", "nsg-app", { properties: {} }),
    res(RT, "Microsoft.Network/routeTables", "rt-app", { properties: {} }),
  ), "azure");
  const sn = m.vnets[0].subnets[0];
  // Two chips, drawn right to left: the route table then the security
  // group. Named by the cloud's own descriptor rather than by the view.
  assert.deepEqual(sn.chips.map((c) => [c.kind, c.name]), [["rt", "rt-app"], ["nsg", "nsg-app"]]);
  assert.ok(sn.chips.every((c) => c.known), "both exist, so both are clickable");
  assert.deepEqual(sn.chips.map((c) => c.label), ["Route table", "Network security group"]);
  assert.equal(m.orphans.length, 0, "an attached NSG is not an orphan");
});

// The one thing this view shows that the tree cannot: an NSG that exists and
// protects nothing.
test("an NSG nothing references is listed as unattached", () => {
  const m = networkModel(map(
    vnet([subnet("snet-apps", "10.20.0.0/24")]),
    res(NSG, "Microsoft.Network/networkSecurityGroups", "nsg-app", { properties: {} }),
    res(P + "/publicIPAddresses/pip1", "Microsoft.Network/publicIPAddresses", "pip1", { properties: {} }),
  ), "azure");
  assert.deepEqual(m.orphans.map((o) => o.name).sort(), ["nsg-app", "pip1"]);
});

test("a subnet naming an NSG that does not exist still shows the attachment", () => {
  const m = networkModel(map(vnet([subnet("snet-data", "10.20.1.0/24", {
    networkSecurityGroup: { id: NSG },
  })])), "azure");
  const sn = m.vnets[0].subnets[0];
  const nsg = sn.chips.find((c) => c.kind === "nsg");
  assert.equal(nsg.name, "nsg-app");
  assert.equal(nsg.known, false, "nothing to click through to, and the chip says so");
});

test("peerings join two VNets and name the far end", () => {
  const other = "/subscriptions/s/resourceGroups/rg2/providers/Microsoft.Network/virtualNetworks/vnet-spoke";
  const peer = res(VNET + "/virtualNetworkPeerings/hub-to-spoke",
    "Microsoft.Network/virtualNetworks/virtualNetworkPeerings", "hub-to-spoke",
    { properties: { remoteVirtualNetwork: { id: other }, peeringState: "Connected" } });
  const m = networkModel(map(
    vnet([]),
    res(other, "Microsoft.Network/virtualNetworks", "vnet-spoke", { properties: { addressSpace: { addressPrefixes: ["10.30.0.0/16"] } } }),
    peer,
  ), "azure");
  assert.equal(m.peerings.length, 1);
  const p = m.peerings[0];
  assert.equal(p.from, VNET.toLowerCase(), "the parent VNet, taken from the peering's own id");
  assert.equal(p.to, other.toLowerCase());
  assert.equal(p.remoteName, "vnet-spoke");
  assert.equal(m.vnets.length, 2);
  // A peering is not a VNet and must not turn up in the unattached column.
  assert.equal(m.orphans.length, 0);
});

test("a peering to a VNet that is not emulated here still has a from", () => {
  const far = "/subscriptions/other/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/elsewhere";
  const m = networkModel(map(vnet([]), res(VNET + "/virtualNetworkPeerings/out",
    "Microsoft.Network/virtualNetworks/virtualNetworkPeerings", "out",
    { properties: { remoteVirtualNetwork: { id: far } } })), "azure");
  assert.equal(m.peerings[0].from, VNET.toLowerCase());
  assert.equal(m.peerings[0].remoteName, "elsewhere");
});

test("the layout grows a VNet box to fit its subnets and never overlaps two", () => {
  const m = networkModel(map(vnet([
    subnet("a", "10.20.0.0/24"), subnet("b", "10.20.1.0/24"), subnet("c", "10.20.2.0/24"),
  ])), "azure");
  // A second, empty VNet below it.
  const other = "/subscriptions/s/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet-zz";
  m.vnets.push({ ...m.vnets[0], id: other, key: other, name: "vnet-zz", subnets: [] });
  const size = networkLayout(m);

  const [first, second] = m.vnets;
  assert.ok(first.h > second.h, "three subnets make a taller box than none");
  assert.ok(second.y >= first.y + first.h, "boxes must not overlap");
  for (const sn of first.subnets) {
    assert.ok(sn.y >= first.y && sn.y + sn.h <= first.y + first.h, "a subnet sits inside its VNet");
    assert.ok(sn.x >= first.x && sn.x + sn.w <= first.x + first.w);
  }
  assert.ok(size.height >= second.y + second.h);
  assert.ok(size.width >= first.x + first.w);
});

test("nothing network-shaped means an empty model, not a crash", () => {
  const m = networkModel(new Map(), "azure");
  assert.deepEqual(m, { vnets: [], peerings: [], orphans: [] });
  const size = networkLayout(m);
  assert.ok(size.width > 0 && size.height > 0);
});

// The fixture is what the network view is eyeballed against, so it has to
// exercise every part of the drawing: an attachment, a peering between two
// VNets, and something unattached.
test("the fixture exercises every part of the network view", () => {
  const doc = JSON.parse(readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url)));
  const m = networkModel(new Map(doc.resources.map((r) => [r.id.toLowerCase(), r])), "azure");
  assert.equal(m.vnets.length, 2, "a hub and a spoke");
  assert.ok(m.vnets.some((v) => v.subnets.some((s) => s.chips.some((c) => c.known))),
    "an attached NSG, since that is the view's point");
  assert.equal(m.peerings.length, 1);
  const p = m.peerings[0];
  assert.ok(m.vnets.some((v) => v.key === p.from) && m.vnets.some((v) => v.key === p.to),
    "the peering should join two VNets that are both drawn");
  assert.ok(m.orphans.some((o) => o.name === "nsg-unused"), "and something unattached");
  networkLayout(m); // must not throw
});

// A virtual machine does not name a subnet. It names its NICs, and a NIC's
// ipConfigurations name the subnet -- so placing a machine takes two hops,
// and this is the only view where that relationship is visible at all: in
// the topology view a VM sits under its resource group, which is where ARM
// really puts it.
const NIC = P + "/networkInterfaces/nic-web";
const VM = RG + "/providers/Microsoft.Compute/virtualMachines/vm-web";

function nic(id, subnetID) {
  return res(id, "Microsoft.Network/networkInterfaces", id.split("/").pop(), {
    properties: { ipConfigurations: [{ name: "internal", properties: { subnet: { id: subnetID } } }] },
  });
}

function machine(id, nicIDs, extra = {}) {
  return res(id, "Microsoft.Compute/virtualMachines", id.split("/").pop(), {
    properties: {
      hardwareProfile: { vmSize: "Standard_B1s" },
      networkProfile: { networkInterfaces: nicIDs.map((n) => ({ id: n })) },
    },
    ...extra,
  }, { backing: "real" });
}

test("a virtual machine is placed in its subnet through its NIC", () => {
  const snet = subnet("snet-apps", "10.20.0.0/24");
  const m = networkModel(map(
    vnet([snet]),
    nic(NIC, snet.id),
    machine(VM, [NIC], { nimbusContainer: "nimbus-vm-vm-web-abc123" }),
  ), "azure");

  const sn = m.vnets[0].subnets[0];
  assert.equal(sn.machines.length, 1);
  assert.equal(sn.machines[0].name, "vm-web");
  assert.equal(sn.machines[0].size, "Standard_B1s");
  // A machine a container really runs for is distinguished from one that is
  // only a record, which is the same thing the topology view's badge says.
  assert.equal(sn.machines[0].running, true);
});

test("a machine whose NIC is missing is not guessed into a subnet", () => {
  const snet = subnet("snet-apps", "10.20.0.0/24");
  // The NIC is referenced but not in the store -- half an apply, or a NIC
  // in another resource group Nimbus never saw.
  const m = networkModel(map(vnet([snet]), machine(VM, [NIC])), "azure");
  assert.equal(m.vnets[0].subnets[0].machines.length, 0);
});

test("a machine with no container is drawn, but not as running", () => {
  const snet = subnet("snet-apps", "10.20.0.0/24");
  const m = networkModel(map(vnet([snet]), nic(NIC, snet.id), machine(VM, [NIC])), "azure");
  const machines = m.vnets[0].subnets[0].machines;
  assert.equal(machines.length, 1);
  assert.equal(machines[0].running, false);
});

test("a NIC attached to a machine is not listed as an orphan", () => {
  const snet = subnet("snet-apps", "10.20.0.0/24");
  const m = networkModel(map(vnet([snet]), nic(NIC, snet.id), machine(VM, [NIC])), "azure");
  assert.ok(!m.orphans.some((o) => o.id === NIC), "the NIC belongs to a machine now");
});

test("a subnet holding machines is taller than one that does not", () => {
  const withVM = subnet("snet-apps", "10.20.0.0/24");
  const empty = subnet("snet-data", "10.20.1.0/24");
  const m = networkModel(map(
    vnet([withVM, empty]),
    nic(NIC, withVM.id),
    machine(VM, [NIC]),
  ), "azure");
  networkLayout(m);

  const [a, b] = m.vnets[0].subnets;
  assert.ok(a.h > b.h, "a subnet with machines needs a row for them");
  // And the second subnet starts below the first, which only works if the
  // layout sums the heights instead of multiplying by a fixed one.
  assert.ok(b.y >= a.y + a.h, `subnets overlap: ${a.y}+${a.h} vs ${b.y}`);
  // The VNet has to be tall enough to contain them both.
  const v = m.vnets[0];
  assert.ok(b.y + b.h <= v.y + v.h, "the last subnet pokes out of its VNet");
});
