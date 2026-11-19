# Calling one cloud from another

All three clouds run in **one process**, sharing one store and one
visualizer. So an Azure Function App can write to DynamoDB, a Lambda can
read an Azure Key Vault secret, and a pod can publish to Pub/Sub — things
that in the real clouds would need VPC peering, cross-account roles and
an egress bill.

This page is the measured answer to "does that actually work", including
where it still does not.

- [What was measured](#what-was-measured)
- [The results](#the-results)
- [AWS from anywhere: one environment variable](#aws-from-anywhere-one-environment-variable)
- [Azure from anywhere: handled for you](#azure-from-anywhere-handled-for-you)
- [GCP from anywhere: endpoint overrides and followed links](#gcp-from-anywhere-endpoint-overrides-and-followed-links)
- [From a Kubernetes pod](#from-a-kubernetes-pod)
- [With IAM enforcement on](#with-iam-enforcement-on)
- [Known limits](#known-limits)

---

## What was measured

One `nimbus start --clouds azure,aws,gcp`, one Terraform apply creating
targets in all three clouds, and then the same probe run from **five
different places** — each a genuinely different network path.

| Vantage point | What it is |
|---|---|
| AWS Lambda | `funcs` engine — AWS's base image and its Runtime Interface Emulator |
| Azure Function App | `run` engine — Microsoft's Functions host, image built with a `COPY` |
| Cloud Run service | `run` engine — an ordinary container with a published port |
| EC2 instance | `vm` engine — a container that boots and runs a boot script |
| EKS pod | `k3s` engine — a pod inside real Kubernetes in Docker |

Ten targets, each written *and* read back where the service allows it:

`aws:dynamodb` · `aws:s3` · `aws:logs` · `aws:lambda` (an actual
cross-cloud function call) · `azure:blob` · `azure:queue` ·
`azure:table` · `azure:keyvault` · `gcp:storage` · `gcp:pubsub`

Measured on Docker Desktop (WSL2) on Windows 11, 2026-11-18.

`examples/multicloud` is the neighbour of this page: it applies resources
to all three clouds in one run and draws them side by side, but nothing
in it *calls* across. This page is about code in one cloud reaching a
service in another.

## The results

| From ↓ to → | AWS | Azure | GCP |
|---|---|---|---|
| AWS Lambda | ✅ 3/3 (+ self-invoke skipped) | ✅ 4/4 | ✅ 2/2 |
| Azure Function App | ✅ 4/4 | ✅ 4/4 | ✅ 2/2 |
| Cloud Run service | ✅ 4/4 | ✅ 4/4 | ✅ 2/2 |
| EC2 instance | ✅ 4/4 | ✅ 4/4 | ✅ 2/2 |
| EKS pod | ✅ 4/4 | ✅ 4/4 | ✅ 2/2 |

**49 of 49**, with one skip: a Lambda invoking itself does not terminate.
Every Azure row uses the real data-plane hostname with the certificate
**verified** — which is new, and is the point of the next section.

Reachability is the weaker question, though: the probe is stdlib-only, so
it proves a service *answers*. A real SDK builds its own URLs and
verifies its own TLS, so the SDKs were run separately, in two languages:

| SDK | Result |
|---|---|
| `boto3` → DynamoDB, S3 | ✅ no endpoint in the code |
| `azure-storage-blob` → blob | ✅ real hostname, TLS verified |
| `google-cloud-storage` → upload **and download** | ✅ |
| `@aws-sdk/client-dynamodb` (Node) | ✅ no endpoint in the code |
| `@azure/storage-blob` (Node) | ✅ Shared Key, TLS verified |

## AWS from anywhere: one environment variable

AWS is the easy one, and it is not a coincidence — it is the only cloud
of the three whose SDKs all read a single endpoint variable:

```
AWS_ENDPOINT_URL=http://host.docker.internal:4702
```

So **a handler written against real AWS reaches Nimbus unchanged**. No
endpoint in the code, no client configuration:

```python
import boto3
boto3.client("dynamodb").put_item(...)   # goes to Nimbus
```

That "every current SDK" claim was checked in a second language rather
than repeated: the JavaScript v3 client, given no endpoint at all, read
the variable and reached Nimbus.

Two details:

- It must name **`host.docker.internal`**, not `localhost`. Inside a
  container, localhost is the container. Nimbus passes
  `--add-host host.docker.internal:host-gateway` to every container it
  starts, so the name resolves on Docker Engine as well as Docker
  Desktop.
- S3 is addressed **path-style**, so set
  `AWS_S3_FORCE_PATH_STYLE=true` (or the client equivalent). There is no
  bucket hostname, which is also why AWS needs no certificate.

Nimbus sets the AWS variables itself for a Lambda. For an Azure Function
App or a Cloud Run service you set them as app settings or environment
variables, which is what `examples/` does.

## Azure from anywhere: handled for you

Azure is the hard one, for two structural reasons — and Nimbus now deals
with both, so there is nothing to configure.

**Its data plane is addressed by hostname.** A blob URL is
`https://{account}.blob.storage.nimbus.localhost:4701/...`, built by the
SDK from the account name. Nested `*.localhost` names resolve to loopback
with nothing installed, which is what makes the data plane work on your
machine with no DNS tooling — but loopback *inside a container is the
container*:

```
stxcloudnimbus.blob.storage.nimbus.localhost -> 127.0.0.1
URLError [Errno 111] Connection refused
```

**And it is HTTPS.** Azure clients refuse plain HTTP, so Nimbus serves
TLS from a CA it generates, and that CA is in no container's trust store.

Every container Nimbus starts now gets both:

- `--add-host {name}:host-gateway` for **every addressable data-plane
  hostname that exists** — each storage account's blob, queue, table,
  file and dfs endpoints, each Key Vault, each function app and its SCM
  site, each container app's fqdn. A wildcard is no use here:
  `/etc/hosts` holds names, not patterns, so these are the concrete ones.
- the CA mounted read-only at `/etc/nimbus/nimbus-ca.pem`, with
  `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE` and
  `NODE_EXTRA_CA_CERTS` pointing at it. A value you set yourself wins.

So this works from inside a Lambda, with no `Host` header and nothing
disabled:

```python
from azure.storage.blob import BlobServiceClient
svc = BlobServiceClient(
    account_url="https://stxcloudnimbus.blob.storage.nimbus.localhost:4701",
    credential={"account_name": "stxcloudnimbus", "account_key": key})
svc.get_blob_client("data", "x.txt").upload_blob(b"...", overwrite=True)
```

Fixing only the hostname would have moved the failure from "cannot
resolve" to "certificate verify failed", so both halves go together.

Replacing the trust store is acceptable **here and nowhere else**: Nimbus
is an offline emulator and the only TLS endpoint a container can reach is
Nimbus itself. See the [ordering limit](#known-limits) for the one case
this does not cover.

## GCP from anywhere: endpoint overrides and followed links

GCP needs an endpoint override and no certificate, because it is served
over plain HTTP and addressed by path:

```python
client = storage.Client(
    project="nimbus-local", credentials=AnonymousCredentials(),
    client_options=ClientOptions(api_endpoint="http://host.docker.internal:4703"))
```

An override alone was not enough, and the reason is worth understanding.
A GCP response is full of absolute URLs, and clients treat them two very
different ways: some are **identity** (a resource's self link *is* its
id) and some are instructions to **go somewhere**, which a client follows
verbatim. Uploads worked because the SDK composes that URL itself;
downloads failed because they followed what Nimbus sent:

```
"mediaLink": "http://localhost:4703/storage/v1/b/{b}/o/{o}?alt=media"
→ HTTPConnectionPool(host='localhost', port=4703): Connection refused
```

A **followed** link is now built from the host the request arrived on, so
a client that reached Nimbus on `host.docker.internal` is told to come
back there, while Terraform keeps reading `localhost` and sees no diff —
which matters, because `media_link` and `self_link` are computed
attributes it stores.

Identity is deliberately untouched: Compute self links are store ids, so
a per-request value would make one resource into two. Nothing known
follows one — every client composes Compute URLs from its endpoint — but
if something did, it would still go to `localhost`.

## From a Kubernetes pod

A pod is the one vantage point that needs configuration, and the reason
is pure Kubernetes: **a pod does not inherit its node's `/etc/hosts`.**
Nimbus writes every alias onto the k3s node, and kubelet *generates* each
pod's hosts file from scratch, so none of it reaches a pod:

```
host.docker.internal -> FAILED gaierror [Errno -2] Name or service not known
192.168.65.254       -> 192.168.65.254          (routable)
```

The gateway address is reachable; only the names are missing. Write them
with `hostAliases`, and mount the CA yourself:

```yaml
spec:
  hostAliases:
    - ip: "192.168.65.254"   # Docker Desktop's gateway; differs on Engine
      hostnames:
        - "host.docker.internal"
        - "stxcloudnimbus.blob.storage.nimbus.localhost"
        - "kv-xcloud-nimbus.vault.nimbus.localhost"
  containers:
    - name: app
      env:
        - name: SSL_CERT_FILE
          value: /ca/nimbus-ca.pem
      volumeMounts:
        - { name: ca, mountPath: /ca }
  volumes:
    - name: ca
      configMap: { name: nimbus-ca }   # kubectl create configmap nimbus-ca --from-file=...
```

With that, a pod reached all ten targets. On Docker Engine the address is
the bridge gateway instead, usually `172.17.0.1` — check with
`docker network inspect bridge`.

## With IAM enforcement on

`--enforce-iam` changes nothing about cross-cloud reachability: the whole
17-resource stack applied, and all three container vantage points reached
all ten targets.

That is because an unmapped caller is the account root, which is the
faithful default. Enforcement really does reach these calls — the same
call from the same container, as a role carrying no permissions:

```
nimbus         -> ALLOWED (200)
xcloud-probe   -> REFUSED 403: AccessDeniedException: role arn:aws:iam::000000000000:role/xcloud-probe
                  is not authorized to perform dynamodb:PutItem
```

So a cross-cloud call is subject to exactly the same policies as any
other call, and nothing special-cases it. See [IAM.md](IAM.md).

## Known limits

Stated rather than hidden, because each has a cause worth knowing.

| Limit | Why | What to do |
|---|---|---|
| **A container only resolves the names that existed when it started.** A storage account created *after* a container is not in that container's `/etc/hosts`. | `--add-host` is a start-time flag writing a hosts file, and there is no wildcard. | Nothing, usually: a workload that *uses* a resource references it, which makes Terraform create it first. Measured both ways — an EC2 instance with no reference to the storage account was created alongside it and could not reach Azure; recreated afterwards, it reached all ten targets. If you hit it, replace the container. |
| **A pod gets none of it.** | kubelet generates each pod's `/etc/hosts`, so the node's aliases do not reach one. | [`hostAliases`](#from-a-kubernetes-pod) and mount the CA. A CoreDNS override would be the better fix and is not built. |
| **A GCP *identity* link still says `localhost`.** | A Compute self link is a store id; rewriting it per request would split one resource into two. | Nothing follows one today. If something did, run `nimbus start --host <a name that resolves in both places>`. |
| **Azure Storage data actions are not enforced.** | Shared Key carries no principal, so enforcing them would mean requiring OAuth and breaking every existing example. Key Vault *is* enforced, because its clients send a real token. | Nothing; see [IAM.md](IAM.md) for what is checked. |

Still untested, so neither claimed nor denied: the Go and Java SDKs, and
any service Nimbus does not emulate (there is no SQS, Secrets Manager or
Firestore). Nimbus answers `InvalidAction` naming the service for those,
rather than failing obscurely.
