# specgen

Turns Microsoft's OpenAPI specs into the table Nimbus uses to fill
server-generated fields on resource types that have no hand-written hook.

This is a development tool, not part of the emulator. It lives in its own Go
module so the main module stays stdlib-only, and it is the only thing in the
repository that fetches anything from the network. What ships is its **output**
— `internal/azure/spectypes_gen.go`, which is committed. The specs are not.

## Running it

```bash
cd tools/specgen
go run .            # fetch (cached under .cache), generate, write the Go file
go run . -report    # also list types with nothing to fill and unresolved $refs
go run . -offline   # fail rather than download; uses the cache as-is
```

`.cache/` mirrors the layout of `Azure/azure-rest-api-specs`, so you can read
the exact bytes a field was derived from. It is gitignored.

## Adding a type

Add an entry to `types.json`:

```json
{
  "type": "Microsoft.Network/natGateways",
  "apiVersion": "2025-03-01",
  "spec": "specification/network/resource-manager/Microsoft.Network/stable/2025-03-01/natGateway.json",
  "path": "/subscriptions/{subscriptionId}/resourceGroups/{resourceGroupName}/providers/Microsoft.Network/natGateways/{natGatewayName}"
}
```

`path` must be the ARM path whose **PUT** creates the resource, spelled exactly
as the spec spells it. Then re-run the generator and commit the regenerated
file along with the `types.json` change.

## What it extracts, and why

For each type it takes the PUT's 200/201 **response** schema — not the request
body. The two are the same definition in practice, but only the response is
marked up with `readOnly`, which is the entire point of reading these files. It
then walks the schema, following `$ref` across files and merging `allOf`, and
records every field that is either read-only or carries a spec `default`.

Two things worth knowing if you change the walker:

- **Each property must keep the file it was declared in.** A resource's `allOf`
  usually reaches into `common-types`; if the walker lets that file become
  "the" current document, the resource's own `$ref`s resolve against
  common-types, find nothing, and the type silently yields zero fields with no
  error. `TestCrossFileAllOfKeepsEachPropertysOwnDocument` pins this down.
- **An unresolvable `$ref` is reported, not swallowed**, for the same reason.

The table only records what the spec *says*. What actually goes into each field
is decided by `internal/azure/spec.go`, which keeps generated values derived
from the resource ID so repeated reads never differ.

## Tests

`go test ./...` runs against the fixture under `testdata`, which mirrors the
shape of a real spec (cross-file `allOf`, flattened properties, read-only
fields, defaults, a cycle). It needs no network.
