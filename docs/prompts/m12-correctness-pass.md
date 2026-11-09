# Milestone 12: the correctness pass — four things that mislead

## Goal

Clear the defects that make Nimbus quietly wrong rather than merely
incomplete. Each of these is already written down under "Later" in
`docs/ROADMAP.md`; none is large; all four are the kind of thing that
teaches a reader something false. A short milestone on purpose.

## Build

1. **An AKS cluster's node resource group must die with its cluster.**
   Deleting the cluster leaves `MC_<rg>_<cluster>_<location>` in the store,
   where real Azure removes it. Noticed during Milestone 10's multicloud
   destroy; it predates that work. The node resource group is created by the
   cluster, so it should be deleted by the cluster's `AfterDelete` — check
   whether anything else is leaking the same way, since a cascade that
   misses one child probably misses others.
2. **Mirror every namespace into the graph, not just `default`.** The k3s
   watcher filters to `default`, so a workload deployed anywhere else
   appears in neither the topology nor the Kubernetes view — which reads as
   "nothing is running" rather than "this view is partial". `kube-system` is
   why the filter exists, so the answer is probably a namespace level in the
   view with the system ones collapsed by default, not an unfiltered dump.
   Affects all three clouds at once, through one engine.
3. **S3 multipart upload.** A file over 8 MB fails outright, because the CLI
   switches protocol on its own at that size: `CreateMultipartUpload`,
   `UploadPart`, `CompleteMultipartUpload`, `AbortMultipartUpload`,
   `ListParts`. GCS already has its resumable protocol; check what Azure
   does with `Put Block`/`Put Block List` before assuming blobs are fine,
   since the roadmap only claims block blobs *exist*.
4. **Seed the activity feed from the journal on connect.** The feed starts
   empty after a restart, because the SSE stream replays the bus's in-memory
   ring and the journal is deliberately not streamed. The timeline is how
   history is reached today; a bounded replay of the most recent journal
   entries on connect would stop the feed looking like nothing happened.
   Snapshots must stay out of it — a restart is not an event.

## Ask the owner

- **How much namespace to show.** A namespace level in the Kubernetes view
  changes a layout you approved sketches for in Milestone 6, so show the
  shape before building it.

## Not in this milestone

Windows has never run Terraform, `az` or `kubectl` against Nimbus — CI's
Windows leg runs the Go tests only, and `nimbus cert` there
(`Import-Certificate` into `CurrentUser\Root`) is untested in anger. Given
that three platform bugs reached `main` by only ever being proven on a Mac,
that is worth its own session. Leave it under "Later" and say so.

## Done when

- [ ] `terraform destroy` on `examples/terraform/azure-aks` leaves no
      `MC_*` resource group in the store, and the graph is empty of it.
- [ ] A pod deployed into a namespace other than `default` appears in the
      Kubernetes view, on all three clouds.
- [ ] `aws s3 cp` of a file larger than 8 MB succeeds, and the bytes come
      back byte-for-byte with `aws s3 cp` in the other direction.
- [ ] The activity feed is populated immediately after a restart with
      `--persist`, and contains no entry for the restart itself.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
