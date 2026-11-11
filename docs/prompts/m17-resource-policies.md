# Milestone 17: policies attached to the thing, not to the caller

## Goal

Milestones 13 and 14 made Nimbus able to refuse a request, on all three
clouds, by evaluating what the CALLER holds: IAM policies, Azure role
assignments, GCP bindings. The other half of every cloud's model is a
policy attached to the RESOURCE — and all of it round-trips today and
decides nothing:

- an **S3 bucket policy** (`?policy`) is stored and returned byte for byte,
  because the provider reads it back, and is never consulted;
- a **Pub/Sub topic's** and a **Cloud Run service's** own `getIamPolicy` /
  `setIamPolicy` are recorded; only the project policy is evaluated;
- **Azure `dataActions`** are in the role catalogue and unenforced, so a
  "Key Vault Secrets User" or "Storage Blob Data Reader" assignment governs
  nothing;
- a **Lambda resource policy** is stored so `GetPolicy` can answer.

Two of these are worth more than the others, and the prompt should say so
rather than treating the list as uniform work. A bucket policy is how
public access is granted and how it is accidentally granted, which is the
single most instructive thing in cloud permissions. And `dataActions` are
the reason an Azure learner cannot currently see why reading a secret is a
different permission from reading the vault.

## Build

1. **Resource policies in the evaluator.** The engine already takes a
   `[]authz.Rule` from whatever source; what is new is that a request now
   gathers rules from TWO places — the caller's and the resource's — and
   the clouds disagree about how those combine. On AWS an allow in either
   is enough for a same-account caller and an explicit deny in either
   wins; on GCP a resource binding is simply another binding. Get this
   right from the documented semantics, not by analogy: it is the one part
   of this milestone where being wrong is silent.
2. **`allUsers` and public access.** `bindingNames` in
   `internal/gcp/authz.go` already honours `allUsers` and
   `allAuthenticatedUsers`. S3's equivalent is a bucket policy with
   `"Principal": "*"`, and the public-access-block sub-resource that
   overrides it — which is the most useful thing here, because "this bucket
   is public and here is the statement that made it" is exactly what a
   learner needs to see.
3. **Azure `dataActions`, which need a principal first.** This is the
   obstacle, and it should be settled before any code: Shared Key carries
   no principal at all, so there is nothing to evaluate a blob data action
   against. Key Vault's data plane uses a bearer token and CAN be
   enforced; storage's cannot, unless the SAS/OAuth path is modelled.
   State what is enforceable and enforce that, rather than half-enforcing
   storage.
4. **The explanation has to grow with it.** `/api/authz` currently reports
   the caller's rules. Once a resource's own policy can decide, the report
   must say WHICH policy decided — "the bucket policy" or "your role" —
   because a decision explained without its source teaches nothing. The
   inspector and `nimbus can` both read that endpoint, and the agreement
   test must still hold.

## Measure first

- **What does the aws provider do with a bucket policy mid-apply?** It
  reads `?policy` back on every refresh; find out whether it compares the
  JSON semantically or textually (the roadmap already records that it must
  come back byte for byte, which suggests textually).
- **Does `aws s3 ls` on a public bucket send credentials at all?**
  `serviceOf` falls back to S3 for an unsigned request, and an unsigned
  request to a public bucket is the one case where "no credentials" is not
  an error. That is the path `allUsers` exists for.
- **What does the Key Vault data plane send?** The challenge flow is
  already implemented (go-autorest needs a tenant in the authorization
  URL); check what the token carries and whether `oid` is in it, since
  that is what a data-action check would read.

## Ask the owner

- **Whether to do Azure storage's data plane at all.** Shared Key cannot
  carry a principal, so enforcing blob data actions would mean requiring
  OAuth for the data plane, which would break `azure-storage` and the SDK
  harness. Recommend enforcing Key Vault's data plane only, and saying
  plainly in the roadmap that storage's cannot be without a change nobody
  has asked for.
- **How far to take combination semantics.** Same-account allow-either is
  AWS's real rule; cross-account requires both, and Nimbus has one
  account. Implementing only the single-account rule is probably right, but
  it is a divergence to state rather than discover.
- **Whether a public bucket should be flagged in the visualizer.** A badge
  on a bucket whose policy grants `*` would be the most useful single thing
  this milestone could produce, and it is also the kind of inference that
  is wrong in interesting cases (a policy that grants `*` but is overridden
  by a public access block).

## Done when

- [ ] `examples/terraform/aws-bucket-policy-enforced` (or similar): a
      bucket a principal can read ONLY because of the bucket policy, and a
      second that it cannot, both under `--enforce-iam`.
- [ ] An explicit `Deny` in a bucket policy beats an `Allow` in the
      caller's own policy, with a test.
- [ ] A GCP resource-level binding governs its own resource and nothing
      else, with a test.
- [ ] Key Vault data-plane actions are enforced against the bearer
      token's principal, or the roadmap says exactly why not.
- [ ] `/api/authz` names which policy decided, and the inspector shows it.
- [ ] The `nimbus can` agreement test still passes on all three clouds.
- [ ] With `--enforce-iam` off, every example in the repo still applies and
      destroys as it does today.
- [ ] `docs/ROADMAP.md` loses "authorization is identity-based only" and
      replaces it with whatever is genuinely still not evaluated.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
