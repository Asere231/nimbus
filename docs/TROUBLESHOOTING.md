# Troubleshooting

Symptoms, in the words the tool actually prints. Most entries here are cases
where the error names the wrong cause, which is why they are worth writing
down.

The first thing to try in any case: open <http://localhost:4700>, turn on
**"show all API calls"**, and watch the activity feed while you reproduce
it. Nimbus flags 4xx and 5xx responses and calls it does not implement, so
the feed usually names the problem directly.

- [Certificates and TLS](#certificates-and-tls)
- [Windows](#windows)
- [Docker](#docker)
- [Terraform](#terraform)
- [CLIs](#clis)
- [Persistence](#persistence)

---

## Certificates and TLS

### `x509: certificate signed by unknown authority`

The CA is not trusted by whatever is making the request. Run `nimbus cert`
and follow what it prints.

On **Windows** this is the likely cause even if you set `SSL_CERT_FILE`: Go's
own HTTP client honours that variable, but the **Terraform azurerm provider
does not**, so the certificate must be in the Windows store.

On **Linux and macOS**, `SSL_CERT_FILE=~/.nimbus/certs/nimbus-ca.pem` works
per-command. Note it *replaces* the system roots rather than adding to them,
so keep it to the one shell.

### `curl: (60) schannel: the revocation status is unknown`

Windows' bundled `curl` uses schannel, which refuses any certificate whose
revocation it cannot check — and a local CA has no CRL or OCSP to check,
ever. `--cacert` does not help.

```bash
curl --ssl-no-revoke https://localhost:4701/...
```

### `x509: "localhost" certificate is not standards compliant` (macOS)

A server certificate whose validity *span* is too long. Apple caps it; Nimbus
issues for 397 days, under the documented 398. If you see this, delete
`~/.nimbus/certs` and run `nimbus cert` again.

### `az` fails with an SSL error immediately

`az cloud register` fetches the metadata document over TLS as its very first
act, so the CA has to be trusted *before* that command. Use
`nimbus az register --run`, which sets `REQUESTS_CA_BUNDLE` on each child
process.

### `az` complains about being behind a proxy

MSAL validates an unknown authority against the real
`login.microsoftonline.com`. `nimbus az register` sets
`az config set core.instance_discovery=false`, which turns that off — the
switch Azure Stack uses. If you registered the cloud by hand, set it
yourself.

## Windows

### `An Application Control policy has blocked this file`

**Smart App Control.** It blocks unsigned binaries by reputation, which
includes freshly built Go binaries — `go run`, `go test` and eventually
`nimbus.exe` itself. There is no exclusion mechanism, and renaming,
relocating, changing build flags and `Unblock-File` all fail.

The only remedy is turning it off: **Settings → Privacy & security → Windows
Security → App & browser control → Smart App Control → Off**. This is
permanent — Windows cannot re-enable it without a full reset.

### "The specified log group does not exist" (but it does)

**Git Bash path conversion.** MSYS rewrites any argument shaped like a Unix
path, so `--log-group-name /aws/lambda/x` arrives as
`C:/Program Files/Git/aws/lambda/x`. The same hits `docker exec … /var/task`.

```bash
MSYS_NO_PATHCONV=1 aws logs describe-log-streams --log-group-name /aws/lambda/x
```

PowerShell never has this problem, and is the better shell for Nimbus on
Windows generally.

### `Python was not found; run without arguments to install from the Microsoft Store`

Running `gcloud` from Git Bash. The SDK's bash wrapper looks for `python` on
PATH and finds Windows' Store stub, which is a redirector rather than an
interpreter. Use **`gcloud.cmd`**, or set `CLOUDSDK_PYTHON` to the SDK's
bundled interpreter. PowerShell and cmd are unaffected.

### `kubectl` says `exec: executable aws failed with exit code 253`

The kubeconfig shells out to `aws eks get-token` on every call, so the AWS
credentials and region must be set in the shell running **`kubectl`**, not
just the one that ran `update-kubeconfig`.

### A `terraform init` modifies four lock files

Expected once: the lock files gain a `windows_amd64` hash. Commit it or
discard it; it recurs only if the hashes are removed.

## Docker

### `docker is not available`

Nimbus needs a running daemon for anything container-backed. Everything else
keeps working — control planes, storage data planes, IAM, the visualizer.

### A cluster you just created answers 401

`kubectl` saying *"You must be logged in to the server"* rather than a
connection error means the API server is up and could not reach the token
webhook. On Docker Engine (Linux), `host.docker.internal` does not exist;
Nimbus passes `--add-host host.docker.internal:host-gateway` on every
container, which needs **Docker 20.10+**.

### A container is still running after `terraform destroy`

Should not happen any more; every engine is adopted on startup, so a delete
can always find its container. If you hit it, clean up with:

```bash
docker rm -f $(docker ps -aq --filter "name=nimbus-")
```

### An apply sits in `CREATING` for minutes and then fails

Usually an image pull on a slow connection. Pre-pull while online:

```bash
docker pull public.ecr.aws/lambda/python:3.12
docker pull rancher/k3s:v1.34.12-k3s1
```

## Terraform

### A plan is never clean — the same change every time

Nimbus is echoing a field differently from the way it was sent. This is a
bug; please report it with the resource type and the diff. A clean
`terraform plan -detailed-exitcode` returning 0 is the project's own bar for
every example.

### `Planning failed` after revoking a permission

Expected, and real clouds behave identically. The apply that would restore
the permission refreshes first, and the refresh is what fails. Use
`terraform plan -refresh=false`.

### An apply fails naming the provider block, not a resource

With `--enforce-iam`, azurerm populates a resource-provider cache when the
provider *configures*, before any resource exists — and
`resource_provider_registrations = "none"` does not skip it. A principal
with no roles fails there. Grant it a role in a separate root module applied
first.

### Resource-group creates take ~20 seconds, deletes 10

Client-side waits inside the azurerm provider, not Nimbus. Nothing Nimbus
returns changes them.

## CLIs

### `An error occurred (Unknown) when calling the … operation`

The AWS CLI could not parse the error document. That is a Nimbus bug — the
three AWS error envelopes are genuinely different documents. Please report
it with the command.

### `Invalid base64: "{"id":"1"}"` from `aws lambda invoke`

aws-cli v2 reads an inline `--payload` as base64. Use a file, which both
versions read verbatim:

```bash
printf '{"id":"1"}' > payload.json
aws lambda invoke --function-name f --payload fileb://payload.json out.json
```

### `gcloud` warns about "universe domain" on every command

Expected noise from endpoint overrides. Everything works through it.

### A `gcloud` command says Nimbus does not implement a collection

A genuine gap — that is the error Nimbus gives for an API it has not
modelled. Please report the exact command; the activity feed will show the
path it asked for.

## Persistence

### State vanished after a restart

Without `--persist` everything is in memory by design. With it, state is
under `~/.nimbus/data`.

### A store written before v0.18.1 has odd leftovers

Object names are now encoded before becoming filenames. Ordinary lowercase
keys are unaffected, but a key that previously collided or failed to write
may leave one orphan file from before. Clearing `~/.nimbus/data` is the
simple answer; nothing reads the orphan.

### GCP resource ids changed after a port change

A GCP resource's id is its self link, which embeds the port. Ids in a
persisted state file are only valid for the port that wrote them — the same
price Azure's storage suffix and ACR's login server pay for being
addressable. Keep the port stable, or start fresh.
