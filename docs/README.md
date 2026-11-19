# The Nimbus handbook

Nimbus is an offline emulator for Azure, AWS and Google Cloud, with a live
visualizer. All three clouds run in **one process**, sharing one store, one
event journal and one picture — which is the point: a single `terraform
apply` can create resources in all three and you watch them appear side by
side.

It is written from scratch in Go with **no third-party dependencies**, and
it never calls a real cloud endpoint.

---

## Start here

| If you want to… | Read |
|---|---|
| install it and run it | [INSTALL.md](INSTALL.md) |
| point Terraform, `az`, `aws`, `gcloud` or `kubectl` at it | [CLIENTS.md](CLIENTS.md) |
| know every command and flag | [CLI.md](CLI.md) |
| know exactly what it supports | [RESOURCES.md](RESOURCES.md) |
| fix something that went wrong | [TROUBLESHOOTING.md](TROUBLESHOOTING.md) |

## The full set

### Using Nimbus

- **[INSTALL.md](INSTALL.md)** — requirements, building, trusting the
  certificate, where state lives, and the platform notes for macOS, Linux
  and Windows.
- **[CLIENTS.md](CLIENTS.md)** — the provider blocks, environment
  variables and endpoint overrides each client needs. Start here when a
  client cannot see Nimbus.
- **[CLI.md](CLI.md)** — every `nimbus` command and flag, with what each
  one prints.
- **[GCLOUD.md](GCLOUD.md)** — the `gcloud` command set, which needs more
  care than the other CLIs.
- **[OFFLINE.md](OFFLINE.md)** — running with no internet at all, and
  which images to pull before you disconnect.

### What it can do

- **[RESOURCES.md](RESOURCES.md)** — every resource type in all three
  clouds, which ones really run a container, every data plane, and what is
  deliberately *not* modelled.
- **[IAM.md](IAM.md)** — the permissions that are actually checked, in all
  three clouds' policy languages, and how to ask "why was this denied".
- **[CROSS-CLOUD.md](CROSS-CLOUD.md)** — calling one cloud from another,
  measured from five vantage points, including the limits.

### How it works

- **[ARCHITECTURE.md](ARCHITECTURE.md)** — the shape of the code, how a
  request flows, the store and event journal, the engines, the visualizer,
  and how to add a resource type.
- **[CHANGELOG.md](CHANGELOG.md)** — what changed, and why.
- **[ROADMAP.md](ROADMAP.md)** — what is coming, and what was decided
  against.

## The ideas worth knowing before you start

Five things explain most of Nimbus's behaviour.

**Real clients are the spec.** Everything is matched against what the
provider and the SDK actually send, not what the documentation says. Where
the two differ, the wire wins — and the reason is usually written down in a
comment beside the code.

**One store, one picture.** Every resource from every cloud goes into the
same store with an id, a type, a name and a parent. The parent is what lets
the visualizer nest things without knowing any cloud's rules, and it is why
three clouds can share one view.

**An engine knows about no cloud.** Anything that really runs lives in
`internal/engines`: `k3s` serves AKS, EKS and GKE unchanged, `run` serves
Cloud Run, Container Apps and Azure Functions, `vm` serves EC2, GCE and
Azure VMs. A cloud plugin supplies a resource id and a name prefix.

**Derive, do not generate.** Anything a client re-reads — a kubeconfig, an
fqdn, an etag, an identity GUID — is a function of the resource id, byte
for byte identical between reads. A value that changes between reads is a
permanent `terraform plan` diff.

**Refuse what cannot be evaluated.** An unparsed filter treated as "matches
everything" turns a gap into a silently wrong answer found nowhere near its
cause. Nimbus answers an error that names the construct instead.

## The ports

| Port | Serves | Protocol |
|---|---|---|
| 4700 | the visualizer and its API | http |
| 4701 | Azure Resource Manager and its data planes | **https** |
| 4702 | AWS | http |
| 4703 | Google Cloud | http |

Azure is the only one over TLS, because Azure clients refuse anything else.
AWS and GCP need no certificate: S3 is addressed path-style and Cloud
Storage by path, so no bucket ever becomes a hostname.

## Worked examples

Each directory under `examples/terraform/` is a complete, runnable example
with its reasoning in the comments:

| Example | Shows |
|---|---|
| `azure-basic`, `aws-basic` | the smallest thing that works, no Docker |
| `azure-aks`, `aws-eks`, `gcp-gke` | real Kubernetes, one container per node |
| `aws-serverless` | Lambda on AWS's own base image, plus DynamoDB |
| `azure-functions` | Microsoft's Functions host, image built per deploy |
| `azure-containerapp` | a service with a sidecar and replicas |
| `gcp-run-pubsub` | Cloud Run with push delivery |
| `aws-instance`, `azure-vm`, `gcp-instance` | VMs that boot and run a script |
| `aws-iam-enforced`, `azure-rbac-enforced`, `gcp-iam-enforced`, `aws-bucket-policy-enforced` | IAM actually refusing things |
| `../multicloud` | one apply, three clouds, three clusters |
| `../apps/url-shortener` | five services wired together as an application |

## Getting help from Nimbus itself

The visualizer at <http://localhost:4700> is not decoration. With **show
all API calls** turned on, the activity feed is the fastest debugging tool
here: it flags every 4xx and 5xx and every unimplemented action, which is
usually the exact thing to build or fix next.

```powershell
terraform apply
```

```powershell
$env:TF_LOG="DEBUG"; terraform apply 2> tf.log
```

`nimbus can ACTION RESOURCE` answers "would this be allowed, and why" using
the same evaluator that enforces it — so the explanation cannot drift from
the behaviour.
