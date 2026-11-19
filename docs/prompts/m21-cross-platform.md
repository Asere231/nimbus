# Milestone 21: prove it on macOS and Linux

## Goal

Milestones 18 to 20 were driven on **one Windows machine**. Several of the
changes they produced were made *because* of what that machine does, and
none of them has ever been exercised by a person on macOS, nor by anything
on Linux beyond what CI happens to cover.

So this milestone is not new features. It is: run everything, on the other
two platforms, and find out what the Windows-shaped decisions cost there.

The bar is the same one the project uses everywhere else — **a real client
doing a real thing** — not "it compiles".

## Why this is worth a milestone

The pattern this repository keeps producing is a change that is right on the
machine it was written on and wrong somewhere nobody looked:

- `host.docker.internal` worked on a Mac and left the EKS and GKE jobs red
  for three milestones.
- A case-insensitive filesystem merged two object keys into one file, and
  only the Linux CI leg was ever immune.
- `/var/task` was mounted writable for twelve milestones; Docker Desktop
  presents a bind mount permissively, so **only Linux** ever suffered, and
  CI never deployed the same function twice so CI never saw it either.

Every one of those was invisible from where it was written. This milestone
is the deliberate version of that discovery.

## THE CHANGES MADE FOR WINDOWS — read this before testing anything

This is the heart of the milestone. Each item says what was changed, why
Windows needed it, and **what to check on macOS and Linux**. If one of them
turns out to cost something on another platform, that trade-off is the
finding, and it must be written down rather than quietly reverted.

### 1. `.gitattributes` pins the working tree to LF

**Why:** Git for Windows sets `core.autocrlf=true`, and a CRLF checkout made
`gofmt` report all 219 Go files, so the close-out check could not pass
there.

**Established:** Go discards `\r` in raw string literals, and text built
with `\n` escapes never reads a newline from the file — so the vm boot
script, the EKS webhook kubeconfig, the k3s `hosts.toml` and
`registries.yaml` all reach their Linux containers as LF however the repo
was checked out.

**Check on macOS/Linux:** that nothing now has literal CRLF where it
matters, and that `gofmt -l .` is clean on a fresh clone. Low risk — LF is
these platforms' native form — but a fresh clone is cheap.

### 2. The CA is imported into a Windows certificate store

**Why:** the azurerm provider **ignores `SSL_CERT_FILE` on Windows** —
proven by pointing it at a nonexistent file and watching `terraform plan`
succeed anyway. The certificate store is the mechanism there.

**Check on macOS/Linux:** that `SSL_CERT_FILE` still works and nothing
quietly came to depend on a store. macOS uses
`security add-trusted-cert`; Linux uses `update-ca-certificates` or
`SSL_CERT_FILE` per command. **`nimbus cert` must print the right
instruction for each OS** — it has a `switch` on `runtime.GOOS`, and the
macOS and Linux branches have not been read by a person in a while.

### 3. Certificate validity is 397 days

**Why:** macOS rejects long-lived server certificates with
`x509: "localhost" certificate is not standards compliant`, which never
mentions dates. Measured on macOS 27: 824 days passes, 825 fails.

**Check on macOS:** this is a macOS constraint that was *measured on macOS*,
so it should hold — but it is the one item where macOS is the strict
platform, and a regression would be silent on the other two. Confirm a
fresh CA is accepted and that `NotAfter - NotBefore` is still computed as a
plain duration in UTC (`AddDate` works in the local zone and a DST boundary
shifts the span by an hour).

### 4. `internal/fsname` percent-encodes object names

**Why:** NTFS and APFS are both case-insensitive, so two keys differing only
in case merged into one file; `:`, `?`, `*` and `|` also failed on NTFS.

**Check on Linux:** Linux is case-SENSITIVE and tolerant of those
characters, so encoding is pure overhead there — the question is only
whether a store written on Linux still round-trips. `Encode(Decode(x)) == x`
distinguishes a canonical name from a legacy one. **A store written before
`fsname` existed must still load**, on every platform.

### 5. `--add-host host.docker.internal:host-gateway` on every container

**Why:** Docker Desktop provides the name for free; Docker **Engine** does
not, and Linux CI runs Engine.

**Check on macOS:** verified once on Docker Desktop 29.9.2 that the override
still reaches the host, so it is not a Linux-only flag with a macOS cost —
**re-confirm**, because this is now joined by the data-plane aliases below
and the combined flag list is longer than it was.

### 6. Azure data-plane hostnames are aliased into every container

**Why:** `{account}.blob.storage.nimbus.localhost` resolves to `127.0.0.1`,
which inside a container is the container, so the real Azure SDK could not
reach blob storage from a Lambda or a Container App.

**Check on macOS/Linux:** `TestARealContainerResolvesAnAliasToTheHost`
asserts the alias resolves to the same address as `host.docker.internal`.
It has run on Linux (CI) and on Windows. **It has never run on macOS**, and
macOS Docker Desktop has its own gateway arrangement. This is the single
most likely item on this list to behave differently.

### 7. The Nimbus CA is mounted into every container

**Why:** aliasing the hostname without trusting the certificate only moves
the failure to `certificate verify failed`.

**Check on macOS/Linux:** the mount source is a host path, and this project
has already been bitten by Windows and Unix disagreeing about those.
`TestARealContainerCanReadTheMountedCA` covers it and has not run on macOS.
Also confirm the environment variables (`SSL_CERT_FILE`,
`REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`, `NODE_EXTRA_CA_CERTS`) do not
disturb anything on a platform where a container might have other TLS to
do.

### 8. A Lambda's code is mounted READ-ONLY

**Why:** the container runs as root and a Python handler's first import left
`__pycache__` owned by root, which the ordinary user Nimbus runs as could
not then unlink — so redeploying failed and the destroy before it failed
silently. **This is the one item where Linux was the BROKEN platform and
Windows was fine.**

**Check on macOS:** macOS Docker Desktop maps bind-mount ownership its own
way, so the bug probably never existed there — but the **fix** is a
read-only mount, and the question is whether RIE and the Python worker are
content with that on macOS as they are on Linux and Windows.
`TestRedeployingAFunctionThatHasRunSucceeds` is the test; it has never run
on macOS.

### 9. Documented Windows-only workarounds that must NOT leak

These are in `INSTALL.md` and `TROUBLESHOOTING.md` and are Windows advice.
Confirm none of them has crept into code or examples as an unconditional:

- `MSYS_NO_PATHCONV=1` for Git Bash path rewriting
- `--ssl-no-revoke` for schannel curl
- `gcloud.cmd` rather than `gcloud`
- `fileb://` for `aws lambda invoke` payloads (this one IS portable and
  deliberate — both CLI major versions accept it; do not "simplify" it)
- writing invoke output to a file rather than `/dev/stdout`

### 10. What is NOT in the tree, and must stay out

WSL support was built, worked, and was **reverted whole**. Running Nimbus
inside WSL is how a Windows user could keep Smart App Control on. Do not
resurrect it as part of this milestone; see the WSL entries in `CLAUDE.md`
for what it would take and what a retry must cover.

## What to run

On **macOS** and on **Linux**, by hand, with a real Docker daemon:

1. `gofmt -l .`, `go vet ./...`, `go test ./...`, `go test -race ./...`
2. `NIMBUS_DOCKER_TESTS=1 go test ./internal/engines/... -count=1 -v`, and
   confirm **by name** that every `TestAReal*` and
   `TestRedeployingAFunctionThatHasRunSucceeds` reports `--- PASS` rather
   than `--- SKIP`. A skipped test passes and says nothing.
3. `node --test 'test/ui/*.test.js'`
4. `nimbus cert` — read what it prints, follow it, and confirm a Terraform
   apply then trusts the endpoint.
5. Every example under `examples/terraform/`, apply →
   `plan -detailed-exitcode` → destroy, with **zero containers left**.
6. `examples/multicloud` — three clouds, three clusters, one apply.
7. **The sequence CI does not do**: apply, *change a handler*, apply again,
   destroy, apply again. That is what found the `/var/task` bug.
8. The cross-cloud matrix in `docs/CROSS-CLOUD.md`, re-run on each platform.
9. `nimbus ssh` interactively — it allocates a TTY and cannot be driven by
   a tool, so a person has to try it.

## Done criteria

- Both platforms pass 1–9, or every failure is written down with its cause.
- Each of the ten Windows-driven changes above has a line saying what it
  costs on macOS and Linux — **including "nothing", where that is the
  measured answer**.
- `CLAUDE.md` gains the platform facts found, and loses any claim this
  milestone disproves.
- CI is read afterwards (`gh run list`, `gh run view --log-failed`), and the
  by-name PASS check still names every real-daemon test.

## Ask the owner

- Whether a macOS runner with Docker is worth adding to CI, given every
  Docker-backed job is `ubuntu-latest` today and hosted macOS runners
  generally cannot run Linux containers. If it is not possible, say so in
  the docs rather than leaving the gap unexplained.
