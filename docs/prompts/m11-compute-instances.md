# Milestone 11: compute instances — the primitive that is still missing

## Goal

Make a virtual machine real on all three clouds. Nimbus runs managed
Kubernetes, serverless functions and container services, but not the thing
every cloud course opens with: launch an instance, SSH in, see your
user-data script ran. Azure's `Microsoft.Compute/virtualMachines` already
round-trips from the spec table as an ARM record; AWS answers no
`RunInstances` at all and GCP no `instances` endpoint. This milestone turns
all three into something that actually boots.

## Build

1. **A `vm` engine** in `internal/engines/vm`, cloud-neutral like `k3s` and
   `run`: start a container from an image, inject SSH keys, publish port 22
   on a free host port, run a boot script, stop/start/restart, delete, read
   console output. It must know nothing about any cloud — a plugin supplies
   the resource id, the image, the keys and the script. Reuse `docker`'s
   `Client`/`Fake`, naming, labels and `FreePort`.
2. **EC2 instances**: `RunInstances`, `DescribeInstances` (with the filters
   the provider actually sends), `TerminateInstances`, `StopInstances`,
   `StartInstances`, `RebootInstances`, plus `DescribeImages`, key pairs and
   `DescribeInstanceTypes`. Instances belong to a subnet, so the graph nests
   them under it. Watch the lifecycle states — the provider has waiters that
   expect `pending` → `running` and `shutting-down` → `terminated`.
3. **GCE instances**: `instances.insert/get/list/delete/stop/start/reset`,
   zonal operations, `machineTypes`, boot disks from an image, and the
   `metadata.items` where `ssh-keys` and `startup-script` live.
4. **Azure VMs**: make the existing spec-driven type real. A VM needs its
   NIC and OS disk, so the parent/child mirroring rules apply, and creation
   is long-running. `azurerm_linux_virtual_machine` is the target.
5. **The visualizer**: badges for each cloud's instance type, and instances
   drawn inside the subnet they are in, in the network view as well as the
   topology one. `TestEverySpecTypeHasABadge` will tell you when you are done.

## Measure first

Point each provider at a logging server before writing anything and capture
the real create-and-read sequence. Three things worth knowing early: which
`DescribeInstanceAttribute`-style read-backs each provider makes after a
create (the VPC attribute lesson says there will be more than you think),
how each one decides an instance is ready, and whether any of them refuses
an instance type it does not recognise.

## Ask the owner

- **What backs an instance.** Options: a fixed base image per OS family with
  `sshd` in it; a table mapping well-known AMI/image ids to Docker images; or
  letting the config name a Docker image directly. Recommend one.
- **Instance metadata.** Real IMDS is `169.254.169.254`, which a container
  cannot reach without routing tricks. Say what is possible — a hosts entry
  inside the container, an env var, or not at all — before promising it.
- **Whether SSH is in scope** or an instance just has to run. SSH is the
  payoff for a learner; it is also keys, a daemon and a port per instance.

## Done when

- [ ] `examples/terraform/aws-instance`: VPC + subnet + security group + key
      pair + instance. `ssh` in with the key Terraform created, and the
      user-data script's output is in the console log.
- [ ] `examples/terraform/gcp-instance`: network + instance with a
      `startup-script` in metadata; `gcloud compute instances list` shows it.
- [ ] `examples/terraform/azure-vm`: VNet + NIC + Linux VM, with
      `admin_ssh_key` working.
- [ ] Each one applies, plans clean (`-detailed-exitcode` = 0), stops and
      starts, and destroys leaving no containers.
- [ ] Instances appear inside their subnet in the network view.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
