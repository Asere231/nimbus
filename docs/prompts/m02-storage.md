# Milestone 2: storage data plane (Blob, Queue, Table)

## Goal

Make `azurerm_storage_account`, `azurerm_storage_container` and blobs work end to end, with real data stored locally, and let `az storage` commands work against it. Storage accounts become the first resource with `Backing: "real"` (solid green border in the visualizer).

## Background

azurerm creates the account through ARM, then talks to the **data plane** at `https://{account}.blob.{suffix}`, `.queue.`, `.table.` (suffix comes from our metadata: `storage.nimbus.localhost`). It reads and writes blob service properties, queue properties and static-website settings right after creating the account, so the account can't apply until the data plane answers. Auth is Shared Key (HMAC of the request with the account key from `listKeys`) or a bearer token.

## Ask the owner first: how account hostnames resolve

`stnimbus.blob.storage.nimbus.localhost` must reach 127.0.0.1 on macOS and Windows. Before writing code, write a 10-line Go program that resolves a few such names and have me run it on my machines. Then present the options with a recommendation:

1. **OS resolves `*.localhost` already** (some systems do): nothing to build.
2. **`nimbus dns add <account>`** writes hosts-file entries (needs admin once per account; Windows `C:\Windows\System32\drivers\etc\hosts`).
3. **Built-in DNS server** on 127.0.0.1:53 plus an OS resolver rule for `nimbus.localhost` (macOS `/etc/resolver/nimbus.localhost`; Windows NRPT rule). Admin once, then automatic.

The TLS certificate must also cover these names: add `*.blob.storage.nimbus.localhost` etc. to the SANs (wildcards match one label only), and handle regeneration for existing installs.

## Build

1. **Routing by Host header**: requests whose host is `{account}.{service}.storage.nimbus.localhost` go to the storage handler on the same HTTPS port. Unknown account → the error Azure returns (`AccountNotFound`-style; check a real response shape in the SDK or docs).
2. **ARM side** (`storage.go` hook): fill `primaryEndpoints`, `primaryLocation`, `statusOfPrimary`, `creationTime`, keys; `POST listKeys` returns two stable keys per account; `regenerateKey` rotates one. Delete account → delete its data.
3. **Data plane**, stdlib only, data under `~/.nimbus/data/storage/{account}` (files for blob content, JSON for metadata) when `--persist`, memory otherwise:
   - Blob: service properties GET/PUT, containers (create, list, get properties, delete, ACL), blobs (Put Blob, Get Blob, Get/Set properties and metadata, Delete, List with prefix/delimiter, Put Block + Put Block List for large uploads).
   - Queue: service properties, create/delete/list queues, put/get/peek/delete messages with visibility timeout.
   - Table: create/delete/list tables, insert/upsert/query/delete entities (OData `$filter` basics: `eq`, `and`).
   - Static website: service properties field accepted and returned.
4. **Auth**: accept Shared Key (verify the signature with the account key, so wrong keys fail like Azure), SAS tokens (accept if well-formed), and bearer tokens.
5. **Visualizer**: containers and blob counts under the account node; inspector lists blobs with size and lets me download one.

## Done when

- [ ] New example `examples/terraform/azure-storage`: account + container + `azurerm_storage_blob` from a local file; apply, `plan -detailed-exitcode` exits 0, destroy.
- [ ] `az storage blob upload` / `download` / `list` work with a connection string from `nimbus env azure`.
- [ ] Azure SDK for Python or JS (my pick; ask) uploads and reads a blob in a short script in `test/`.
- [ ] Data survives a restart with `--persist`.
- [ ] Shared Key signature checking has tests with real signatures (compute expected values with the documented algorithm).

## Watch out for

- Shared Key canonicalisation is exact: header order, empty `Content-Length` for zero, lower-cased `x-ms-` headers, canonicalised resource with query parameters sorted.
- Storage APIs speak XML, not JSON, with `x-ms-version`, `x-ms-request-id`, `ETag` and `Last-Modified` headers.
