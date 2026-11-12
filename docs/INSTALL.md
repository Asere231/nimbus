# Installing and running Nimbus

Nimbus is a single binary. It has no runtime dependencies, no database, no
container of its own and no network access — it emulates Azure, AWS and
Google Cloud on your machine and never calls a real cloud endpoint.

- [Requirements](#requirements)
- [Build](#build)
- [Trust the certificate](#trust-the-certificate)
- [Run it](#run-it)
- [Where Nimbus keeps things](#where-nimbus-keeps-things)
- [Platform notes](#platform-notes)
- [Uninstalling](#uninstalling)

---

## Requirements

| What | Why | Needed for |
|---|---|---|
| **Go 1.22 or newer** | to build the binary | always |
| **Docker** | anything that really runs a container | Kubernetes, Lambda, Container Apps, Cloud Run, VMs, container registries |
| **Terraform 1.5+** | the examples | the examples |
| `az`, `aws`, `gcloud`, `kubectl` | optional, if you want to drive Nimbus with a cloud CLI | per-cloud |

Nimbus itself is **stdlib-only** — `go build` fetches nothing. The main
module has no third-party dependencies at all.

Everything except Docker is optional. Without Docker you can still use every
control-plane resource, the storage data planes, IAM enforcement and the
visualizer; what you lose is the handful of resources backed by a real
container.

## Build

```bash
go build -o bin/nimbus ./cmd/nimbus
```

On Windows:

```bash
go build -o bin\nimbus.exe .\cmd\nimbus
```

There is no install step and nothing is written outside the repository until
you run it.

## Trust the certificate

Azure clients refuse to talk to anything but HTTPS, so Nimbus serves its
Azure endpoint over TLS using a certificate authority it generates for
itself. You trust that CA once, on your machine.

```bash
./bin/nimbus cert
```

It prints the path to the CA and the exact command for your operating
system. The CA is created on first use and reused afterwards.

| OS | What it prints |
|---|---|
| **macOS** | `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ~/.nimbus/certs/nimbus-ca.pem` |
| **Windows** | `Import-Certificate -FilePath ...\nimbus-ca.pem -CertStoreLocation Cert:\CurrentUser\Root` |
| **Linux** | copy into `/usr/local/share/ca-certificates/` and run `update-ca-certificates`, or set `SSL_CERT_FILE` per command |

Notes that save time:

- **Windows needs the certificate store.** `SSL_CERT_FILE` works for Go's own
  HTTP client but the Terraform **azurerm provider ignores it** there, so the
  import is not optional. `CurrentUser\Root` needs no administrator; Windows
  shows a confirmation dialog, which you answer.
- **AWS and GCP need no certificate at all.** They are served over plain HTTP.
  S3 is addressed path-style, so there is no bucket hostname to certify.
- **Re-running `nimbus cert` is safe.** Adding a new DNS name reissues the
  server certificate and reuses the CA, so you never re-trust.

## Run it

```bash
./bin/nimbus start --clouds azure,aws,gcp
```

| Port | Serves | Protocol |
|---|---|---|
| 4700 | the visualizer and its API | http |
| 4701 | Azure Resource Manager and its data planes | **https** |
| 4702 | AWS | http |
| 4703 | Google Cloud | http |

Open <http://localhost:4700> to watch resources appear as clients create
them. Press Ctrl+C to stop.

All three clouds run in **one process**, sharing one store, one event
journal and one visualizer — which is the point: a single apply can create
resources in all three and you see them side by side.

### Keeping state between runs

```bash
./bin/nimbus start --clouds azure,aws,gcp --persist
```

Without `--persist` everything lives in memory and is gone when you stop.
With it, resources, object/blob contents and the event journal are written
under `~/.nimbus/data`, **and containers are left running** so a restart
picks them up where it left off.

## Where Nimbus keeps things

Everything is under `~/.nimbus` (`%USERPROFILE%\.nimbus` on Windows), and
nothing else on your machine is touched except the certificate you trusted.

```
~/.nimbus/
  certs/        the CA and the server certificate
  data/         only with --persist
    state.json  every resource
    journal/    the event journal the visualizer replays
    s3/         S3 object bytes
    gcs/        Cloud Storage object bytes
    storage/    Azure blob, queue and table contents
    lambda/     unpacked function code
```

Override the location with `--home DIR` or the `NIMBUS_HOME` environment
variable.

## Platform notes

Nimbus is developed on macOS and tested on macOS, Linux and Windows in CI.
These are the differences worth knowing before you start; see
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) for symptoms and fixes.

### macOS

Nothing special. This is the reference platform.

### Linux

- `host.docker.internal` does not exist on Docker Engine, so Nimbus passes
  `--add-host host.docker.internal:host-gateway` to every container it
  starts. Needs Docker 20.10 or newer.
- `SSL_CERT_FILE` works, so you can trust the CA per-command instead of
  system-wide.

### Windows

Everything works, including Docker-backed resources, but four things about
the platform will bite you rather than Nimbus:

- **Smart App Control** is on by default on Windows 11 Home and blocks
  unsigned binaries by reputation — including, eventually, `nimbus.exe`
  itself. There is no exclusion mechanism. Turning it off is permanent
  (Windows cannot re-enable it without a reset). Budget for this.
- **Use PowerShell rather than Git Bash.** Git Bash rewrites any argument
  that looks like a Unix path, so `--log-group-name /aws/lambda/x` silently
  becomes a Windows path and the error blames the wrong thing. If you must
  use Git Bash, set `MSYS_NO_PATHCONV=1`.
- **`curl` on Windows is schannel**, which refuses a local CA because it
  cannot check revocation. Add `--ssl-no-revoke`.
- **`gcloud` must be run as `gcloud.cmd`** from Git Bash, or it finds the
  Microsoft Store's Python stub and dies with "Python was not found".

Docker Desktop (WSL2) needs no special configuration: bind mounts take
ordinary `C:\` paths and `host.docker.internal` resolves.

## Uninstalling

1. Stop Nimbus.
2. Delete `~/.nimbus`.
3. Remove the certificate: delete **"Nimbus Local CA"** from the store you
   added it to (Keychain Access on macOS, `certmgr.msc` on Windows,
   `/usr/local/share/ca-certificates/` on Linux).
4. Delete the repository.

Nimbus creates Docker containers named `nimbus-*`. If you ever stop it
ungracefully with `--persist`, remove any stragglers with:

```bash
docker rm -f $(docker ps -aq --filter "name=nimbus-")
```
