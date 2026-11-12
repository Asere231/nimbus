# Milestone 18: Windows, properly

## Goal

Nimbus has never been used on Windows. The code is built and unit-tested
there on every push, and that is all: **no Windows machine has ever run
Terraform, `az`, `aws`, `gcloud` or `kubectl` against it.**

That gap is the reason this milestone exists rather than a feature. Three
platform bugs have reached `main` by only ever being proven on a Mac —
`filepath.IsAbs` disagreeing with a slash-separated name, the missing
executable bit, and `host.docker.internal` not existing on Docker Engine
— and each was invisible until something that was not macOS ran it. The
Windows CI leg was added for exactly this and it only compiles and runs
unit tests.

So this milestone is **find out, then fix**. Expect to spend most of it
reading real client output and comparing it with what CLAUDE.md claims,
and expect several of the claims to be wrong. A milestone that ends with
"everything worked" has probably not been thorough enough — but if that
is genuinely the finding, say so plainly and record what was measured.

## What is already known — do not re-measure

From CLAUDE.md's "Host and platform facts", already proven on Windows:

- **Nested `*.localhost` names resolve to loopback with nothing
  installed.** `[System.Net.Dns]::GetHostAddresses` — the `getaddrinfo`
  path the SDKs use — returns `127.0.0.1` for
  `st1.blob.storage.nimbus.localhost`, with no hosts-file edit. **This
  was verified by resolver call only.** Nothing has ever addressed the
  storage data plane, a Key Vault or a Container App through a real
  client on Windows, so the resolution is known and the end-to-end path
  is not.
- **`filepath.IsAbs` is false for `/absolute.py` on Windows**, which is
  why `funcs.safeJoin` and S3's `cleanKey` judge the raw, slash-separated
  name instead of handing it to `filepath`.
- **Windows has no executable bit**, so an unpacked Lambda `bootstrap`
  comes out `-rw-rw-rw-`. Harmless, because the bit matters inside the
  Linux container.
- **Windows's clock is coarse** — a millisecond or worse — so a startup
  snapshot and the first event after it share a timestamp. The run
  grouping treats a snapshot as "at or before" the run it precedes.

## Suspicions worth testing first

These are predictions, not known bugs. Each is cheap to check and each
would be invisible on macOS. **Measure before fixing, and if a prediction
is wrong, record that it was wrong** — a suspicion that was checked and
disproved is worth as much here as a bug.

1. **A case-insensitive filesystem collides two different objects.**
   `internal/aws/s3store.go` maps an S3 key straight onto a path
   (`filepath.Join(s.dir, bucket, filepath.FromSlash(key))`), and Azure's
   blob store does the same. On NTFS, `Readme.txt` and `readme.txt` are
   one file and two S3 objects. Put both, read both back, and see whether
   the second overwrote the first. The same question applies to bucket
   names and to ARM resource ids, though ARM resolves ids
   case-insensitively anyway, so the store is the interesting part.
2. **An S3 key can contain characters Windows forbids in a filename.**
   `:`, `*`, `?`, `"`, `<`, `>` and `|` are legal in an S3 key and
   illegal on NTFS; so are the reserved device names (`CON`, `NUL`,
   `COM1`, …), and Windows strips a trailing dot or space. `a:b` is one
   object in S3. Find out what Nimbus does with it — and note that the
   answer must not be "refuse every key with a colon", because a
   timestamp-shaped key is ordinary.
3. **`nimbus ssh` has no Windows branch at all.** It decides on a TTY
   from what was asked for (deliberately — see CLAUDE.md on `/dev/null`),
   and passes `-t` to `docker exec`. Whether that works from PowerShell,
   from `cmd.exe` and from Git Bash are three different questions.
4. **The CA goes into `Cert:\CurrentUser\Root`** (what `nimbus cert`
   prints). Whether Go's `crypto/x509` on Windows — which is what
   azurerm, the aws provider and the google provider all use — reads the
   CurrentUser root store or only LocalMachine is the thing to find out.
   If it needs LocalMachine, that needs admin and `nimbus cert` is
   printing advice that does not work.
5. **`az`, `gcloud` and `aws` are Python on Windows**, launched through
   shims. `nimbus az register --run` sets `REQUESTS_CA_BUNDLE` on each
   child process; whether that survives the shim is worth seeing.
6. **Docker Desktop on Windows is WSL2.** `host.docker.internal` should
   work, and `docker.runArgs` passes `--add-host
   host.docker.internal:host-gateway` unconditionally — which may be
   redundant or may conflict there. The k3s engine also BIND-MOUNTS the
   token-webhook kubeconfig; a Windows path crossing into a Linux
   container is the thing to watch.
7. **A CRLF checkout.** Git on Windows may convert line endings. Nothing
   in the main module should care, but the `vm` engine's boot script and
   anything read from the repo and sent to a container would — a bash
   script with CRLF fails with `\r: command not found`.

## Build

There is nothing to design. The work is:

1. **Get it running at all.** Build, `nimbus cert`, start, open the
   visualizer. Record every step that needed something the README does
   not mention.
2. **Work through the clouds in this order**, because each is cheaper
   than the last to debug: `azure-basic`, `aws-basic`, `gcp-basic`
   (no Docker), then `azure-storage` (the data plane, and the DNS claim
   end to end), then `aws-serverless` and `azure-containerapp` (Docker),
   then one k3s example (`azure-aks` or `aws-eks`).
3. **Then the CLIs**: `nimbus az register --run` and an `az` call,
   `aws --endpoint-url`, `nimbus gcp register`.
4. **Fix what breaks**, with a test that fails on Windows and passes on
   macOS — or, where that cannot be written, say why in the commit.
5. **Add to the Windows CI leg** whatever can run there without Docker.
   The leg exists and has only ever compiled and unit-tested; an
   `azure-basic` apply would be a real addition.

A fix that makes something work only on Windows, by branching on
`runtime.GOOS`, is a last resort. The three platform bugs already fixed
were all fixed by judging a NAME instead of a path, which works
everywhere — prefer that shape.

## Measure first

- **What does a real client do with the storage data plane on Windows?**
  The DNS claim is resolver-level only. `az storage blob upload` against
  `st1.blob.storage.nimbus.localhost:4701` is the end-to-end version.
- **Does Go trust `Cert:\CurrentUser\Root`?** One `terraform apply` of
  `azure-basic` answers it, and the failure mode if not is an x509 error
  that will blame the network.
- **What is the slowest thing?** macOS has azurerm's ~20 s resource-group
  create and a flat 10 s per delete. Windows may differ, and if an
  example takes materially longer there, the README should say so.
- **Does the visualizer work in Edge?** It is ES modules with no build
  step; nothing should care, but it has only ever been opened in one
  browser.

## Ask the owner

- **How much Docker to do.** The k3s examples are the heaviest thing in
  the repo — minutes and gigabytes — and Docker Desktop on Windows is
  slower again. Recommend doing one k3s cloud rather than all three, and
  saying in the roadmap which one was proven.
- **Whether to add a Docker job to the Windows CI leg.** GitHub's
  `windows-latest` runners have no Docker daemon for Linux containers, so
  the answer is probably no — but it should be stated rather than left
  looking like an oversight.
- **If something is broken in a way that needs a design change** — the
  case-insensitivity question could be one — bring the options rather
  than picking. Storing a key's real name beside its bytes, hashing the
  on-disk name, or refusing the collision are three different trades.

## Done when

- [ ] `azure-basic`, `aws-basic` and `gcp-basic` apply, plan clean
      (`-detailed-exitcode` = 0) and destroy on Windows.
- [ ] `azure-storage` applies and its data plane works through a real
      client, which is the first end-to-end test of the `*.localhost`
      finding on Windows.
- [ ] At least one Docker-backed example runs: a Lambda, a Container App
      or a k3s cluster.
- [ ] `nimbus cert` prints advice that actually works, verified rather
      than assumed.
- [ ] Every difference found is in CLAUDE.md under "Host and platform
      facts", including the ones that turned out NOT to be problems.
- [ ] `docs/ROADMAP.md` loses "Windows, properly. Still only DNS-tested"
      and says what is now proven there and what is not.
- [ ] `gofmt -l .`, `go vet ./...`, `go test ./...` pass on Windows, and
      CI is green on all three legs afterwards.
- [ ] If nothing was broken, that is the finding and the roadmap says so
      — with the list of what was actually exercised, so the claim can be
      trusted.
