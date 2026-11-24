# Milestone 22: a SAS that actually decides

## Goal

A Shared Access Signature is how Azure delegates access to storage without
handing over the account key, and Nimbus accepts any that is merely
SHAPED like one. `internal/azure/storage/auth.go` checks that `sig`, `se`
and `sp` are present and returns `AuthSAS`. Nothing verifies the
signature, nothing enforces the expiry, and nothing checks that `sp`
actually permits the operation being attempted.

The roadmap already states what that costs, and it is the whole point of
the emulator:

> it means a SAS cannot be used to *test* that permissions are wired
> correctly.

After this milestone a SAS grants exactly what it says, for exactly as
long as it says, and a wrong one is refused with the reason.

## Why this is worth a milestone

**It corrects a conclusion this project drew and over-generalised.**
CLAUDE.md says, from Milestone 17:

> Only Key Vault's data plane can be enforced, and the reason is
> credentials. A data action is evaluated against a principal; **Storage's
> Shared Key carries none**, so enforcing blob data actions would mean
> requiring OAuth there and breaking `azure-storage` and the SDK harness.

Every word of that is true *about Shared Key*. It became "Azure storage's
data plane cannot be enforced", and that is false: **a SAS carries its own
authorization**. `sp=` is the permission set, `se=` the expiry, `sr=` the
scope, `sig=` a signature over all of it, computed with a key Nimbus
itself issued. It needs no principal, no OAuth, and no change to the
Shared Key path — the two schemes coexist in the same `Authenticate`
switch today.

So this is not "add enforcement to storage". It is: the one Azure access
mechanism that was always verifiable was being waved through, and a
sentence about a different mechanism is why nobody looked.

**It completes the authorization arc.** M13 built the decision engine,
M14 enforced RBAC on three clouds, M17 added resource policies and
`dataActions`. SAS is the last Azure access mechanism that decides
nothing. After it, all three of Azure's ways of granting access are real.

## What already exists

Read these before designing anything — most of the machinery is there.

- **`Sign(accountKey, stringToSign)`** in `auth.go` is already base64
  HMAC-SHA256 with the account key. A SAS signature uses the *same
  primitive* with a *different string-to-sign*. Nothing cryptographic
  needs writing.
- **`Authenticate` already distinguishes `AuthSAS`** from Shared Key,
  Bearer and Anonymous, and already returns a `(result, message)` pair, so
  there is a place to put a refusal and a reason.
- **Nimbus issues the account keys** (`nimbus env azure
  --storage-account` prints a connection string), so it can verify a
  signature a client computed. This is the half that makes the milestone
  possible at all.
- **`StringToSign` for Shared Key** is a worked example of this exact
  shape of problem, with a test (`TestStringToSign`) named after "the
  cases that bite".

## Measure first

The string-to-sign is the whole milestone, and it is NOT guessable. Get
the real thing out of real clients before writing a line:

- **There is more than one SAS, and they sign different strings.** A
  SERVICE SAS (`sr=b` or `sr=c`, scoped to one blob or container) and an
  ACCOUNT SAS (`ss=`, `srt=`, scoped across services) have different
  field orders and different lengths. A single implementation that
  assumes one will silently fail the other. Find out which one each
  client generates.
- **The field list is VERSIONED by `sv=`.** Azure adds fields in new
  versions and the string-to-sign grows; signing the wrong number of
  newlines produces a mismatch that names nothing. Decide which `sv`
  values to support and refuse the rest BY NAME rather than guessing.
- **Generate real tokens three ways and compare them**: `az storage blob
  generate-sas --full-uri`, `data.azurerm_storage_account_sas`, and the
  Python SDK's `generate_blob_sas`. Record the exact query strings in the
  prompt's findings, because they are the spec.
- **What does a client do with a 403 from a SAS?** Before enforcing
  anything, find out whether a refusal fails cleanly or triggers a retry
  storm — the `aws` lesson about 5xx being retried five times applies to
  whatever Azure's SDKs do here.
- **Does `azure-storage` or azurerm ever send a SAS Nimbus issued?** If
  nothing in the existing examples generates one, the milestone has to
  bring its own example, and that example is the proof.

## Build

1. **Verify the signature.** Build the SAS string-to-sign per version and
   scheme, sign it with each account key, and compare. A mismatch is a
   403 that QUOTES the string-to-sign it used, exactly as the Shared Key
   path already does — that one line is what makes this debuggable.
2. **Enforce the expiry**, and the start time (`st`) if present. An
   expired SAS is refused with its expiry in the message, because "403"
   alone sends people looking at permissions.
3. **Enforce the permissions.** Map each data-plane operation to the
   letter it needs (`r` read, `w` write, `a` add, `c` create, `d` delete,
   `l` list) and refuse when `sp` lacks it. This is the part that makes a
   SAS testable, which is the milestone's stated purpose.
4. **Enforce the scope.** `sr=b` is one blob, `sr=c` a container; an
   account SAS's `ss`/`srt` say which services and resource types. A SAS
   for one container must not open another.
5. **Refuse what is not implemented, by name.** User Delegation SAS
   (`skoid`/`sktid`, signed with a key from OAuth) is a different
   mechanism; `sip` IP ranges and `spr` protocol are checkable and may be
   out of scope. Whatever is not enforced must SAY it is not enforced
   rather than passing silently — the `FilterExpression` rule.

## Done criteria

- [ ] A SAS generated by `az storage blob generate-sas` works, and the
      same token with one character of the signature changed is refused.
- [ ] A SAS whose `se` has passed is refused, and the message says so.
- [ ] A read-only SAS (`sp=r`) can read and CANNOT write, and the refusal
      names the permission it lacked.
- [ ] A SAS for container A cannot read container B.
- [ ] Shared Key, Bearer and anonymous access all behave exactly as they
      do today — `examples/terraform/azure-storage` and
      `test/storage-sdk` pass unchanged.
- [ ] An example demonstrates the point: a SAS handed to something that
      uses it, and a second one that is refused for a stated reason.
- [ ] CLAUDE.md's M17 note is CORRECTED — not deleted. It was right about
      Shared Key and wrong to generalise, and both halves are worth
      keeping.
- [ ] `docs/ROADMAP.md` loses "SAS tokens are accepted on shape alone"
      and says what is enforced and what is not.
- [ ] Full suite and CI green on all three legs.

## Ask the owner

- **Which `sv` versions to support.** Supporting one and refusing the rest
  by name is honest and cheap; supporting several means several
  strings-to-sign. Recommend: whatever `az` and the Python SDK emit
  today, named explicitly, everything else refused.
- **User Delegation SAS.** It needs an OAuth-issued key and is a genuinely
  different mechanism. Recommend leaving it out and saying so, since the
  milestone's value is in the two kinds people actually generate locally.
- **Whether a failed SAS should quote its string-to-sign.** Shared Key
  does, and it is the single most useful thing in that error — but a SAS
  string-to-sign contains the permissions and expiry, not the key, so
  there is no secret in it. Recommend yes, for the same reason.

## What NOT to do

- Do not require OAuth on the storage data plane. That was M17's reason
  for leaving this alone and it is still right: it would break
  `azure-storage` and the SDK harness, and a SAS needs no principal.
- Do not touch the Shared Key path. It is the one scheme that was already
  verified properly, and it is the control that proves a SAS regression
  is a SAS regression.
