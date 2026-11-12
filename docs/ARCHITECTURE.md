# How Nimbus works

For anyone extending it, debugging it, or wondering why a design is the way
it is. If you only want to *use* Nimbus, [INSTALL.md](INSTALL.md) and
[CLIENTS.md](CLIENTS.md) are the documents you want.

- [The shape of it](#the-shape-of-it)
- [How a request flows](#how-a-request-flows)
- [The store and the event journal](#the-store-and-the-event-journal)
- [The engines](#the-engines)
- [The visualizer](#the-visualizer)
- [Adding a resource type](#adding-a-resource-type)
- [Rules worth knowing before you change anything](#rules-worth-knowing-before-you-change-anything)
- [Tests](#tests)

---

## The shape of it

```
cmd/nimbus/            the CLI
internal/
  azure/ aws/ gcp/     one package per cloud: routing, wire formats, hooks
  core/                the store, the event bus and journal, certificates
  authz/              the decision engine — knows no cloud's policy syntax
  engines/            anything that really runs
    docker/           the container client (and a Fake for tests)
    k3s/              managed Kubernetes (AKS, EKS, GKE)
    funcs/            Lambda
    run/              HTTP services (Cloud Run, Container Apps)
    vm/               compute instances (EC2, GCE, Azure VMs)
  fsname/             turning an object name into a safe filename
  ui/                 the visualizer, embedded with go:embed
tools/specgen/        generates Azure's resource table from Microsoft's specs
test/                 SDK harnesses and the visualizer's node tests
examples/terraform/   one directory per worked example
```

Two rules hold the whole thing together:

1. **The main module is stdlib-only.** No third-party Go dependencies. Test
   harnesses that need real cloud SDKs live in their own module under
   `test/`.
2. **An engine knows about no cloud.** `k3s` serves AKS, EKS and GKE
   unchanged; `run` serves Cloud Run and Container Apps; `vm` serves EC2,
   GCE and Azure VMs. A cloud plugin supplies the resource id, a name prefix
   and its own bookkeeping. If you find yourself putting a cloud's
   vocabulary into an engine, it belongs in the plugin.

## How a request flows

### Azure

1. `azure/server.go` `ServeHTTP` routes metadata, token, OpenID and
   async-operation paths; everything else is ARM.
2. `azure/ids.go` `parseTarget` turns an ARM path into a target — kind,
   type, id, parent.
3. `azure/arm.go` handles it generically: PUT, PATCH, GET, HEAD, DELETE,
   lists and actions.
4. Per-type behaviour lives in **hooks** (`BeforePut`, `AfterPut`,
   `AfterDelete`, `Provision`, `Backing`), registered in files like
   `network.go` and `storage.go`.
5. `core/store.go` stores the JSON and publishes an event.

53 Azure types are modelled from Microsoft's own OpenAPI specs rather than
by hand: `tools/specgen` emits the table and `spec.go` applies it.

### AWS

1. `aws/server.go` `ServeHTTP` records the request and routes it.
2. With `--enforce-iam`, the request's action and resource are worked out,
   the caller is resolved, and `internal/authz` decides.
3. `aws/sigv4.go` `serviceOf` reads the **SigV4 credential scope** to pick
   the service. The path says nothing — every Query service POSTs to `/`.
4. The service's own file handles it: `sts.go`, `ec2.go`, `s3.go`,
   `iam.go`, `lambda.go`, `dynamodb.go`, …

### GCP

Ordinary REST paths, dispatched per service, with a per-collection table in
`compute.go` driving the generic verbs.

## The store and the event journal

`core.Store` holds every resource from every cloud: id, type, name, parent,
cloud, and the body as the client sent it. The parent is what lets the
visualizer nest things without knowing any cloud's rules.

Every change publishes a `core.Event` carrying enough to *draw* the resource
— id, type, name, parent — which is what makes replay possible even for
something since destroyed.

Two subtleties that bite:

- **Use `core.Store.Update`, not get-then-put.** Read-modify-write looks
  safe and is not: two overlapping writers both read the same copy and the
  second restores the first's change. `Update` holds the lock across the
  whole thing. Every parent/child mirror goes through it.
- **A restart is not a change.** Resources loaded from `state.json` are
  simply there, so startup writes a `kind: "snapshot"` record straight to
  the journal — never through the bus, or every restart would fill the
  activity feed with things that did not happen.

## The engines

| Engine | Backs | Notes |
|---|---|---|
| `docker` | all of them | the container client, plus naming, labels, free ports, and a `Fake` for tests |
| `k3s` | AKS, EKS, GKE | real Kubernetes in Docker; watches the cluster and mirrors pods into the store |
| `funcs` | Lambda | AWS's own base images and the Runtime Interface Emulator they ship |
| `run` | Cloud Run, Container Apps | pull, publish a port, wait for it to answer, proxy through |
| `vm` | EC2, GCE, Azure VMs | a container that boots, runs a boot script and stays up |

**Every engine is reattached on startup.** With `--persist` containers
outlive the process deliberately, so a fresh Nimbus adopts them — matching
on labels, letting the plugin decide which resource ids are its own, and
recovering the published port, which is the one thing about a running
container that cannot be derived.

`nimbus ssh` is `docker exec`: Nimbus is stdlib-only and cannot run an SSH
server, and no vendor base image ships `sshd`.

## The visualizer

ES modules, no build step, no CDN — plain HTML, CSS and JS in
`internal/ui/static`, embedded with `//go:embed static`. It must work
offline.

It is multi-cloud by dispatch, never by branching: badges, colours, sibling
order, which type is a container, and any non-containment links all come
from that cloud's own table (`types-azure.js`, `types-aws.js`,
`types-gcp.js`) through `types.js`. A shared module must never key on a type
name — `test/ui/clouds.test.js` fails the build if one does.

Three views (topology, network, Kubernetes), each a descriptor of `model`,
`layout` and `draw`. It replays an apply step by step from the journal,
overlays a `terraform plan`, and exports any view as SVG or PNG.

`?fixture` renders `static/fixture.json` with no server at all.

**A change under `internal/ui/static` needs a rebuild before it reaches the
browser**, because the files are embedded.

## Adding a resource type

The usual task, using Azure as the example:

1. **Write a failing test** in `internal/azure/*_test.go` that replays the
   client's actual calls. Get the shape from a debug log:
   `TF_LOG=DEBUG terraform apply 2> tf.log`.
2. Add a hook file and register it in `New()`.
3. Fill server-generated fields in `BeforePut`; start or stop an engine in
   `AfterPut`/`AfterDelete` and set `Backing: "real"` if one runs.
4. Add a badge and colour in `internal/ui/static/types-azure.js`.
   `TestEverySpecTypeHasABadge` fails until you do.
5. **Prove it with the real client**: add it to an example and run apply,
   `plan -detailed-exitcode` and destroy.

The bar for "done" is a second plan finding nothing to do. Anything else
means a field is echoed differently from the way it was sent.

## Rules worth knowing before you change anything

These were each learned by something breaking.

- **Real clients are the spec**, not the documentation. Match what the
  provider and the SDK actually send.
- **Derive, do not generate.** Anything a client re-reads — a kubeconfig, an
  fqdn, an etag, an identity GUID — must be a function of the resource id,
  byte for byte identical between reads. A value that changes is a permanent
  diff. And it must exist by the time the client first reads it.
- **An empty object echoed back is not the same as absent.** Returning
  `{}` for something the client sent empty makes it plan away configuration
  nobody wrote.
- **A name is not a path.** An object key, a blob name and a zip entry are
  all names that may contain `..`, a colon or a backslash. Judge them as
  names — `internal/fsname`, `cleanKey`, `safeJoin` — never by handing them
  to `filepath`, which disagrees with itself across platforms.
- **An unimplemented thing must fail in the shape the client expects.** A
  missing sub-resource usually has its own error code that the provider is
  written to tolerate; the wrong one turns a working path into a failed
  apply. And a 5xx gets retried five times, so something that will never
  work should answer 400.
- **Refuse what you cannot evaluate.** Treating an unparsed filter as
  "matches everything" turns a gap into a silently wrong answer found
  nowhere near its cause.
- **A registry of mutable things hands out copies**, on the way in *and* on
  the way out. `go test -race` has found five real races of exactly this
  shape, and "on the way in" is the half that gets forgotten.

## Tests

```bash
gofmt -l .          # must print nothing
go vet ./...
go test ./...
go test -race ./... # CI runs this on Linux
node --test 'test/ui/*.test.js'   # the visualizer; a glob, not a directory
```

Docker-backed tests are skipped unless `NIMBUS_DOCKER_TESTS=1`.

Two habits this project relies on:

- **Break it to prove it.** A new test should be confirmed failing against
  the old behaviour. Several tests here were green while asserting nothing —
  one checked a CSS class by searching a `querySelectorAll` argument.
- **Put the system in the state you assert about.** Do not arrive before a
  goroutine changes it. `docker.Fake` has `FailRun` and `BlockRun` so a test
  can hold a container un-started deterministically rather than racing it.
