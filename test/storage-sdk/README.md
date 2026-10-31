# Azure SDK for Python harness

Drives the Nimbus storage **data plane** with Microsoft's own client libraries
— the same ones `az storage` is built on. `../sdkcompat` covers the ARM control
plane with go-azure-sdk; this covers blobs, queues and tables, which speak XML
and OData rather than ARM's JSON.

```bash
./bin/nimbus start --persist                      # terminal 1
cd examples/terraform/azure-storage && terraform apply -auto-approve
cd ../../../test/storage-sdk && ./run.sh          # terminal 2
```

The account must already exist. `run.sh` defaults to `stnimbusdata`, which is
what the `azure-storage` example creates; pass another name as the first
argument. The account key is read back from the running server through ARM, so
nothing is hard-coded.

What it checks, beyond "did it return 200":

- a 2 MB upload, which the SDK splits into blocks and reassembles with Put
  Block List, compared byte for byte on the way back
- a byte-range read
- metadata set and read back
- prefix + delimiter listing, the thing that makes blob storage look like it
  has directories
- **a wrong account key being rejected** — an emulator that waves bad
  credentials through teaches you the wrong thing
- queue visibility: a peek does not consume, a receive hides, and a pop
  receipt is needed to delete
- table OData filters, and that a merge leaves unnamed properties alone

Dependencies are pinned in `requirements.txt`. They live here rather than in
the main Go module, which stays stdlib-only.
