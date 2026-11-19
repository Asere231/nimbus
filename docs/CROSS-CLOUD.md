# Calling one cloud from another

All three clouds run in **one process**, sharing one store and one
visualizer. So an Azure Function App can write to DynamoDB, a Lambda can
read an Azure Key Vault secret, and a GKE pod can publish to Pub/Sub —
things that in the real clouds would need VPC peering, cross-account
roles and an egress bill.

This page is the measured answer to "does that actually work", including
the two places where it does not.

- [What was measured](#what-was-measured)
- [The results](#the-results)
- [AWS from anywhere: one environment variable](#aws-from-anywhere-one-environment-variable)
- [Azure from anywhere: a hostname and a certificate](#azure-from-anywhere-a-hostname-and-a-certificate)
- [GCP from anywhere: endpoint overrides, and the link trap](#gcp-from-anywhere-endpoint-overrides-and-the-link-trap)
- [From a Kubernetes pod](#from-a-kubernetes-pod)
- [Known shortcomings](#known-shortcomings)

---

## What was measured

One `nimbus start --clouds azure,aws,gcp`, one Terraform apply creating
targets in all three clouds, and then the same probe run from **five
different places** — each of which is a genuinely different network path.

| Vantage point | What it is |
|---|---|
| AWS Lambda | `funcs` engine — AWS's base image and its Runtime Interface Emulator |
| Azure Function App | `run` engine — Microsoft's Functions host, image built with a `COPY` |
| Cloud Run service | `run` engine — an ordinary container with a published port |
| EC2 instance | `vm` engine — a container that boots and runs a boot script |
| EKS pod | `k3s` engine — a pod inside real Kubernetes in Docker |

Seven targets, two writes-and-reads-back each where the service allows
it:

`aws:dynamodb` · `aws:s3` · `aws:logs` · `azure:blob` ·
`azure:keyvault` · `gcp:storage` · `gcp:pubsub`

Measured on Docker Desktop (WSL2) on Windows 11, 2026-11-18.

`examples/multicloud` is the neighbour of this page: it applies resources
to all three clouds in one run and draws them side by side, but nothing
in it *calls* across. This page is about code in one cloud reaching a
service in another.

## The results

| From ↓ to → | AWS | Azure | GCP |
|---|---|---|---|
| AWS Lambda | ✅ 3/3 | ✅ 2/2 | ✅ 2/2 |
| Azure Function App | ✅ 3/3 | ✅ 2/2 | ✅ 2/2 |
| Cloud Run service | ✅ 3/3 | ✅ 2/2 | ✅ 2/2 |
| EC2 instance | ✅ 3/3 | ✅ 2/2 | ✅ 2/2 |
| EKS pod | ✅ 3/3 | ✅ 2/2 | ✅ 2/2 |

**35 of 35 reachable.** But reachability is the weaker question. The
probe is deliberately stdlib-only, so it proves a service *answers*; it
does not prove a real SDK can be pointed at it, because an SDK builds
its own URLs and verifies its own TLS. So the SDKs were run too, from a
pod:

| SDK | Result |
|---|---|
| `boto3` → DynamoDB | ✅ wrote and read back |
| `boto3` → S3 | ✅ put and got |
| `azure-storage-blob` → blob | ✅ uploaded and downloaded — with a host alias and the CA mounted |
| `google-cloud-storage` → upload | ✅ |
| `google-cloud-storage` → **download** | ❌ followed `mediaLink` to `localhost:4703` |

That last row is the one real failure, and the
[link trap](#gcp-from-anywhere-endpoint-overrides-and-the-link-trap)
explains it.

## AWS from anywhere: one environment variable

AWS is the easy one, and it is not a coincidence — it is the only cloud
of the three whose SDKs all read a single endpoint variable:

```
AWS_ENDPOINT_URL=http://host.docker.internal:4702
```

Every current AWS SDK honours it, so **a handler written against real
AWS reaches Nimbus unchanged**. No endpoint in the code, no client
configuration:

```python
import boto3
boto3.client("dynamodb").put_item(...)   # goes to Nimbus
```

Two details:

- It must name **`host.docker.internal`**, not `localhost`. Inside a
  container, localhost is the container. Nimbus passes
  `--add-host host.docker.internal:host-gateway` to every container it
  starts, so the name resolves on Docker Engine as well as Docker
  Desktop.
- S3 is addressed **path-style**, so set
  `AWS_S3_FORCE_PATH_STYLE=true` (or the equivalent client config).
  There is no bucket hostname, which is also why AWS needs no
  certificate.

Nimbus already sets the AWS variables itself for a Lambda. For an Azure
Function App or a Cloud Run service you set them as app settings or
environment variables, which is what `examples/` does.

## Azure from anywhere: a hostname and a certificate

Azure is the hard one, for two reasons that are both structural rather
than oversights.

**Its data plane is addressed by hostname.** A blob URL is
`https://{account}.blob.storage.nimbus.localhost:4701/...`, and the SDK
builds that from the account name. Nested `*.localhost` names resolve to
loopback on macOS and Windows with nothing installed — which is what
makes the storage data plane work on the host with no DNS tooling — but
loopback *inside a container is the container*. Measured from inside a
Function App container:

```
stxcloudnimbus.blob.storage.nimbus.localhost -> 127.0.0.1
URLError [Errno 111] Connection refused
```

**And it is HTTPS.** Azure clients refuse plain HTTP, so Nimbus serves
TLS from a CA it generates. That CA is trusted on your machine; it is
not in any container's trust store.

Both are fixable, and with both fixed the **real Azure SDK works** — the
proof above is `azure-storage-blob` uploading and downloading from a
pod. What it needs:

1. **Point the account hostname at the host.** In Kubernetes that is
   `hostAliases`; for a plain container it is
   `--add-host {account}.blob.storage.nimbus.localhost:host-gateway`.
2. **Mount the CA** (`~/.nimbus/certs/nimbus-ca.pem`) and pass it to the
   client — `connection_verify=` for the Python SDK,
   `REQUESTS_CA_BUNDLE` for anything requests-based.

```yaml
# the pod spec that made the real Azure SDK work
hostAliases:
  - ip: "192.168.65.254"          # Docker Desktop's gateway; differs on Engine
    hostnames:
      - "host.docker.internal"
      - "stxcloudnimbus.blob.storage.nimbus.localhost"
```

Nimbus does **not** add the data-plane alias for you today — see
[shortcomings](#known-shortcomings).

## GCP from anywhere: endpoint overrides, and the link trap

GCP is reachable with an endpoint override and no certificate, because
it is served over plain HTTP and addressed by path:

```python
from google.cloud import storage
from google.auth.credentials import AnonymousCredentials
from google.api_core.client_options import ClientOptions

client = storage.Client(
    project="nimbus-local",
    credentials=AnonymousCredentials(),
    client_options=ClientOptions(api_endpoint="http://host.docker.internal:4703"))
```

That uploads fine. The **download fails**, and the reason is worth
understanding because it is not a bug in the override:

```
HTTPConnectionPool(host='localhost', port=4703): Connection refused
```

A GCP resource's identity *is* its self link — the full URL it answers
on — so every document Nimbus returns carries absolute URLs built from
`--host`, which defaults to `localhost`:

```json
"selfLink":  "http://localhost:4703/storage/v1/b/xcloud-gcs-nimbus/o/sdk-pod.txt",
"mediaLink": "http://localhost:4703/storage/v1/b/xcloud-gcs-nimbus/o/sdk-pod.txt?alt=media"
```

The SDK **follows `mediaLink` verbatim** rather than rebuilding it from
the endpoint override, so the request goes to the container's own
loopback. Anything that *constructs* its URL works; anything that
*follows a returned one* does not.

The lever is `nimbus start --host`, which is what every self link is
built from. Setting it to a name that resolves both on the host and
inside containers makes the returned links usable from both — at the
cost of your host-side clients needing that name too. The default
favours the host, since that is where Terraform runs.

## From a Kubernetes pod

A pod is the one vantage point that needs explicit configuration, and
the reason is pure Kubernetes:

**A pod does not inherit the node's `/etc/hosts`.** Nimbus starts the k3s
node with `--add-host host.docker.internal:host-gateway`, and that entry
really is on the node:

```
192.168.65.254  host.docker.internal
```

But kubelet *generates* each pod's `/etc/hosts` from scratch, so inside
a pod:

```
host.docker.internal -> FAILED gaierror [Errno -2] Name or service not known
192.168.65.254       -> 192.168.65.254          (routable)
```

The gateway address is reachable; only the name is missing.
`hostAliases` is Kubernetes' own way of writing a line into that
generated file, and with it a pod reached all seven targets.

```yaml
spec:
  hostAliases:
    - ip: "192.168.65.254"
      hostnames: ["host.docker.internal"]
```

The address is **Docker Desktop's** gateway. On Docker Engine it is the
bridge gateway instead, usually `172.17.0.1` — check with
`docker network inspect bridge`.

## Known shortcomings

Stated rather than hidden, because each has a cause worth knowing.

| Shortcoming | Why | Workaround |
|---|---|---|
| **Nimbus does not alias Azure data-plane hostnames into containers.** It adds `host.docker.internal` unconditionally but not `{account}.blob.storage.nimbus.localhost`, so the real Azure SDK cannot reach blob storage from a Lambda, Container App or Cloud Run service without help. | The alias list would have to be rebuilt as storage accounts are created, after the container already exists. One place would change: `runArgs` in `internal/engines/docker/docker.go`. | Add the alias yourself (`hostAliases` in Kubernetes), or address the data plane through `host.docker.internal:4701` with an explicit `Host` header, which is what the probe does. |
| **The Nimbus CA is not in any container's trust store.** | Nothing mounts it. Azure is the only cloud this affects, because it is the only one served over TLS. | Mount `~/.nimbus/certs/nimbus-ca.pem` and point the client at it. |
| **GCP SDK calls that follow a server-returned URL go to `localhost`.** Uploads work, media downloads do not. | A GCP id *is* its self link, so the links are absolute and built from `--host`. | `nimbus start --host <a name that resolves in both places>`, or fetch object bytes with a URL you build yourself. |
| **Azure Storage cannot be reached with a bearer identity that Nimbus enforces.** Shared Key is verified; Bearer is accepted on shape alone. | Shared Key carries no principal, so enforcing data actions there would break every existing example. Key Vault *is* enforced, because its clients send a real token. | None needed for reachability; see [IAM.md](IAM.md) for what is and is not checked. |

What is **not** a shortcoming: the probe disabling TLS verification and
setting a `Host` header by hand. That is the probe being stdlib-only on
purpose, so it could answer "is this reachable" from five runtimes
without vendoring three SDKs. The SDK run is what answers the stricter
question, and it is reported separately above.
