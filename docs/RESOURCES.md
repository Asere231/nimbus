# Every resource Nimbus understands

A complete list, generated from the source rather than written by hand:
Azure's table comes from `internal/ui/static/types-azure.js`, which
`TestEverySpecTypeHasABadge` keeps in step with the server, and the AWS and
GCP tables come from their own equivalents.

If a type is not here, Nimbus does not model it. That is not silent: an
unknown Azure type still round-trips through the generic ARM path, an
unknown AWS action answers `InvalidAction` naming the service, and the
activity feed shows both in red — which is how you find out.

- [What the markers mean](#what-the-markers-mean)
- [Azure](#azure)
- [AWS](#aws)
- [Google Cloud](#google-cloud)
- [Kubernetes](#kubernetes)
- [What is deliberately not modelled](#what-is-deliberately-not-modelled)

---

## What the markers mean

| Marker | Meaning |
|---|---|
| **runs** | Something really happens: a container starts, an image is built, a cluster comes up. These are backed by an engine (`internal/engines`), and a `terraform apply` of one genuinely waits for it. |
| hook | Hand-written behaviour beyond storing JSON — server-generated fields, parent/child mirroring, long-running creates, data planes, delete cascades. |
| spec | Modelled from Microsoft's own OpenAPI specs by `tools/specgen`. It round-trips faithfully — create, read, update, delete, list, clean plan — and nothing runs. |

"spec" is not second-class. For most resources, round-tripping faithfully
*is* the whole job: a network security group that stores and returns what
the provider sent is indistinguishable from the real one, to Terraform.

## Azure

The data plane for storage, Key Vault, Functions and Log
Analytics is real; see [CLIENTS.md](CLIENTS.md) for how to point a client
at each.

| Type | |
|---|---|
| `microsoft.app/containerapps` | **runs** |
| `microsoft.app/managedenvironments` | hook |
| `microsoft.authorization/policyassignments` | spec |
| `microsoft.authorization/policydefinitions` | spec |
| `microsoft.authorization/roleassignments` | hook |
| `microsoft.cache/redis` | spec |
| `microsoft.compute/availabilitysets` | spec |
| `microsoft.compute/disks` | spec |
| `microsoft.compute/galleries` | spec |
| `microsoft.compute/images` | spec |
| `microsoft.compute/proximityplacementgroups` | spec |
| `microsoft.compute/snapshots` | spec |
| `microsoft.compute/sshpublickeys` | spec |
| `microsoft.compute/virtualmachines` | **runs** |
| `microsoft.compute/virtualmachines/extensions` | spec |
| `microsoft.compute/virtualmachinescalesets` | spec |
| `microsoft.containerinstance/containergroups` | spec |
| `microsoft.containerregistry/registries` | **runs** |
| `microsoft.containerservice/managedclusters` | **runs** |
| `microsoft.containerservice/managedclusters/agentpools` | **runs** |
| `microsoft.dbformysql/flexibleservers` | spec |
| `microsoft.dbforpostgresql/flexibleservers` | spec |
| `microsoft.documentdb/databaseaccounts` | spec |
| `microsoft.eventhub/namespaces` | spec |
| `microsoft.eventhub/namespaces/eventhubs` | spec |
| `microsoft.insights/actiongroups` | spec |
| `microsoft.insights/autoscalesettings` | spec |
| `microsoft.insights/components` | spec |
| `microsoft.insights/metricalerts` | spec |
| `microsoft.keyvault/vaults` | **runs** |
| `microsoft.logic/workflows` | spec |
| `microsoft.managedidentity/userassignedidentities` | hook |
| `microsoft.network/applicationgateways` | spec |
| `microsoft.network/applicationsecuritygroups` | spec |
| `microsoft.network/azurefirewalls` | spec |
| `microsoft.network/bastionhosts` | spec |
| `microsoft.network/dnszones` | spec |
| `microsoft.network/firewallpolicies` | spec |
| `microsoft.network/loadbalancers` | spec |
| `microsoft.network/natgateways` | spec |
| `microsoft.network/networkinterfaces` | spec |
| `microsoft.network/networksecuritygroups` | spec |
| `microsoft.network/networksecuritygroups/securityrules` | spec |
| `microsoft.network/networkwatchers` | spec |
| `microsoft.network/privatednszones` | spec |
| `microsoft.network/privatednszones/virtualnetworklinks` | spec |
| `microsoft.network/privateendpoints` | spec |
| `microsoft.network/publicipaddresses` | spec |
| `microsoft.network/routetables` | spec |
| `microsoft.network/routetables/routes` | spec |
| `microsoft.network/virtualnetworkgateways` | spec |
| `microsoft.network/virtualnetworks` | hook |
| `microsoft.network/virtualnetworks/subnets` | hook |
| `microsoft.network/virtualnetworks/virtualnetworkpeerings` | spec |
| `microsoft.operationalinsights/workspaces` | hook |
| `microsoft.resources/deployments` | hook |
| `microsoft.resources/resourcegroups` | hook |
| `microsoft.resources/subscriptions` | spec |
| `microsoft.servicebus/namespaces` | spec |
| `microsoft.servicebus/namespaces/queues` | spec |
| `microsoft.servicebus/namespaces/topics` | spec |
| `microsoft.sql/servers` | spec |
| `microsoft.sql/servers/databases` | spec |
| `microsoft.sql/servers/firewallrules` | spec |
| `microsoft.storage/storageaccounts` | **runs** |
| `microsoft.storage/storageaccounts/blobservices` | spec |
| `microsoft.storage/storageaccounts/blobservices/containers` | **runs** |
| `microsoft.storage/storageaccounts/fileservices` | spec |
| `microsoft.storage/storageaccounts/queueservices` | spec |
| `microsoft.storage/storageaccounts/tableservices` | spec |
| `microsoft.web/serverfarms` | hook |
| `microsoft.web/sites` | **runs** |
| `microsoft.web/sites/slots` | spec |
| `microsoft.web/staticsites` | spec |
### Azure data planes

Each is reached by **hostname**, which is why each needs its own
certificate SAN and, from inside a container, its own host alias.

| Service | Address | Auth |
|---|---|---|
| Blob | `{account}.blob.storage.nimbus.localhost:4701` | Shared Key (verified) or Bearer (accepted on shape) |
| Queue | `{account}.queue.storage.nimbus.localhost:4701` | as blob |
| Table | `{account}.table.storage.nimbus.localhost:4701` | as blob, but a **different signing scheme** |
| Key Vault | `{vault}.vault.nimbus.localhost:4701` | Bearer, after a 401 challenge; **data actions ARE enforced** |
| Container App | `{app}.containerapps.nimbus.localhost:4701` | none — proxied to your container |
| Function App | `{app}.azurewebsites.nimbus.localhost:4701` | none — proxied to the Functions host |
| Kudu (deploy) | `{app}.scm.azurewebsites.nimbus.localhost:4701` | none |
| Log Analytics | `{logAnalyticsResourceId}/v1/workspaces/{id}/query` | Bearer; KQL subset |

## AWS

Across eight services. Routing is by the **SigV4 credential
scope**, not the URL — every Query service POSTs to `/`.

| Type | |
|---|---|
| `AWS::Account` | hook |
| `AWS::DynamoDB::Table` | hook |
| `AWS::EC2::EIP` | hook |
| `AWS::EC2::Instance` | **runs** |
| `AWS::EC2::InternetGateway` | hook |
| `AWS::EC2::KeyPair` | hook |
| `AWS::EC2::NatGateway` | hook |
| `AWS::EC2::NetworkAcl` | hook |
| `AWS::EC2::Route` | hook |
| `AWS::EC2::RouteTable` | hook |
| `AWS::EC2::SecurityGroup` | hook |
| `AWS::EC2::Subnet` | hook |
| `AWS::EC2::VPC` | hook |
| `AWS::EKS::Cluster` | **runs** |
| `AWS::EKS::Nodegroup` | **runs** |
| `AWS::IAM::InstanceProfile` | hook |
| `AWS::IAM::Policy` | hook |
| `AWS::IAM::Role` | hook |
| `AWS::IAM::User` | hook |
| `AWS::Lambda::Function` | **runs** |
| `AWS::Logs::LogGroup` | hook |
| `AWS::S3::Bucket` | hook |

### AWS services and what they answer

| Service | Scope field | Shape | Notes |
|---|---|---|---|
| STS | `sts` | Query / XML | `GetCallerIdentity` needs no permission, by design |
| EC2 | `ec2` | Query / XML | `<Response><Errors>` envelope, `RequestID` not `RequestId` |
| IAM | `iam` | Query / XML | `<member>` lists, policies URL-encoded, one not-found code |
| S3 | `s3` (or unsigned) | REST / XML | **path-style**, so no TLS and no DNS needed |
| Lambda | `lambda` | REST / JSON | versioned per operation, not per service |
| DynamoDB | `dynamodb` | JSON 1.0 | action in `X-Amz-Target`; every error is 400 |
| CloudWatch Logs | `logs` | JSON 1.1 | lowerCamelCase fields, unlike DynamoDB's TitleCase |
| EKS | `eks` | REST / JSON | plus a TokenReview webhook the cluster calls back on |

## Google Cloud

A resource's id **is its self link**, so ids embed the port.

| Type | |
|---|---|
| `cloudresourcemanager.googleapis.com/Project` | hook |
| `compute.googleapis.com/Firewall` | hook |
| `compute.googleapis.com/Instance` | **runs** |
| `compute.googleapis.com/Network` | hook |
| `compute.googleapis.com/Route` | hook |
| `compute.googleapis.com/Router` | hook |
| `compute.googleapis.com/Subnetwork` | hook |
| `container.googleapis.com/Cluster` | **runs** |
| `container.googleapis.com/NodePool` | **runs** |
| `iam.googleapis.com/ServiceAccount` | hook |
| `pubsub.googleapis.com/Subscription` | hook |
| `pubsub.googleapis.com/Topic` | hook |
| `run.googleapis.com/Revision` | hook |
| `run.googleapis.com/Service` | **runs** |
| `storage.googleapis.com/Bucket` | hook |

### Endpoint overrides

The API version belongs to the **client**, not the service, and the two
clients disagree about the same service — so Nimbus routes a path with the
prefix and without it, and does not pretend there is a rule.

| Setting | Value |
|---|---|
| `compute_custom_endpoint` | `http://localhost:4703/compute/v1/` |
| `storage_custom_endpoint` | `http://localhost:4703/storage/v1/` |
| `iam_custom_endpoint` | `http://localhost:4703/` (the client appends `v1/`) |
| `pubsub_custom_endpoint` | `http://localhost:4703/v1/` (gcloud wants the bare origin) |
| `cloud_run_v2_custom_endpoint` | `http://localhost:4703/v2/` |
| `container_custom_endpoint` | `http://localhost:4703/` |
| `resource_manager_custom_endpoint` | `http://localhost:4703/v1/` |

## Kubernetes

Clusters are **real k3s**, one container per node, for AKS, EKS and GKE
alike. A watcher mirrors workloads into the store as `Nimbus.Kubernetes/*`
so the visualizer's Kubernetes view works for all three with no per-cloud
code. Every namespace is mirrored except `kube-system`, `kube-public` and
`kube-node-lease`.

## What is deliberately not modelled

Stated so you can tell "not built" from "built and broken":

| Not modelled | Why |
|---|---|
| SQS, SNS, Secrets Manager, Firestore, Bigtable | nobody has needed them yet; they answer `InvalidAction` naming the service |
| Azure Storage **data action** enforcement | Shared Key carries no principal, so enforcing would require OAuth and break every existing example. Key Vault **is** enforced |
| Cross-account IAM | Nimbus has one account, so the rule cannot arise |
| `ForAllValues:` / `ForAnyValue:` IAM operators | they quantify over multivalued keys and every key here is a single string |
| `uniqueString()` matching Azure's | the algorithm is unpublished; Nimbus is deterministic but different |
| A DNS server (`nimbus dns`) | unnecessary — nested `*.localhost` resolves to loopback on macOS and Windows with nothing installed |
| Traffic between resources | Nimbus routes nothing, so there is no "public subnet" badge to earn |
