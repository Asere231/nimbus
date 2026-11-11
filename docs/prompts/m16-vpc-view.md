# Milestone 16: a network view for AWS and GCP

## Goal

The network view draws a VNet, its subnets, their security groups and route
tables, and any peering. It is **Azure-only**, declared that way in
`render.js`'s `VIEWS` registry with `clouds: ["azure"]`. So an AWS VPC with
two subnets, an internet gateway, a route table and a security group — all
fourteen resources of `aws-basic` — has no network drawing at all, and
neither does a GCP VPC with its subnetworks and firewall rules. An EC2 or
GCE instance appears in no network picture anywhere.

The Kubernetes view is the precedent and the reason to expect this to be
cheap: it was written for AKS and serves EKS and GKE unchanged, because the
k3s watcher mirrors pods into the store with the engine's cloud on them and
the view dispatches on that. The question this milestone answers is whether
a *network* generalises the same way, or whether three clouds' networking
models are too different to share a drawing.

## Build

1. **Decide whether it is one view or three.** The three models rhyme but
   do not match: an Azure subnet holds a security group and a route table
   as *associations*; an AWS subnet has a route table association and
   security groups belong to the VPC and attach to interfaces; a GCP
   firewall rule attaches to the NETWORK and matches on network tags, and
   GCP has no per-subnet ACL at all. Read all three type tables before
   choosing, and write down which concept each cloud's drawing cannot
   express.
2. **Generalise the model and the layout** if one view wins, keeping the
   rule `test/ui/clouds.test.js` enforces: a shared module must never key
   on a type name. Everything cloud-specific — which type is a network,
   which is a subnet, what attaches to what — comes from that cloud's table
   through `types.js`, exactly as badges and sibling order already do.
3. **Put instances in it.** This is most of the value: an EC2 or GCE
   instance drawn in the subnet its interface sits in, the way an Azure VM
   already is through its NIC. `network.js` does that for Azure today.
4. **Routing and reachability, as far as is honest.** A route table with a
   `0.0.0.0/0` route to an internet gateway is the difference between a
   public and a private subnet, and drawing that is the single most useful
   thing a network view can say. Nimbus routes nothing — the roadmap says
   NAT gateways are recorded and nothing routes — so the drawing must not
   imply traffic flows. Show the configuration, label it as configuration.

## Measure first

Nothing on the wire here: this is a milestone about drawings, and the
measurement is visual. Apply `aws-basic`, `gcp-basic` and `aws-instance`,
open the view, and look at it. `?fixture` should carry all three clouds'
networks so the layout can be judged without a server — and the fixture is
also the only way to see a big one, since the roadmap notes the Kubernetes
view has never been tested beyond a handful of objects.

## Ask the owner

- **One view or one per cloud.** A single "Network" tab that dispatches is
  the better outcome if the models allow it; three tabs would be honest
  about the differences and would double the layout code. Sketch both
  before building, as Milestone 6 did.
- **How much to draw.** Subnets inside a network with their CIDRs is the
  floor. Security groups, route tables, gateways, peering and instances are
  each a decision — and a view that draws everything draws nothing
  legibly, which is the lesson of every real cloud console.
- **Whether a public subnet should be marked.** It needs a route table with
  a default route to an IGW to be true on AWS, and means something quite
  different on GCP, where there are no public subnets at all.

## Done when

- [ ] The network view draws AWS and GCP networks, with subnets, their
      CIDRs, and whatever else the owner chose.
- [ ] An EC2 and a GCE instance each appear in the subnet they belong to.
- [ ] `test/ui/clouds.test.js` still passes: no shared module keys on a
      type name.
- [ ] `?fixture` carries a network for each of the three clouds, so the
      view can be looked at offline.
- [ ] `test/ui` covers the pure parts — the model and the layout — as
      `network.test.js` already does for Azure.
- [ ] Exported SVG and PNG both work for the new view, carrying their own
      `<style>`: an external stylesheet does not apply to a detached SVG,
      which this project has already learned once.
- [ ] `docs/ROADMAP.md` loses "no EC2 or GCE instance appears in the
      network view" and "the AWS tab has only the topology view".
- [ ] Full test suite and CI green on macOS, Windows and Linux.
