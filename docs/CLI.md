# The `nimbus` command

Every command, every flag, and what each one is for.

```
nimbus start                   run the emulator
nimbus cert                    where the CA is, and how to trust it
nimbus env azure               environment variables for Terraform and the SDKs
nimbus az register             register Nimbus as a custom cloud for the az CLI
nimbus gcp register            point the gcloud CLI at Nimbus
nimbus gcp kubeconfig CLUSTER  a kubeconfig for a GKE cluster
nimbus gcp install-auth-plugin the gke-gcloud-auth-plugin shim
nimbus ssh INSTANCE            a shell on an instance
nimbus plan-view PLAN          show a terraform plan on the graph
nimbus can ACTION RESOURCE     ask whether a principal may do something
nimbus version
```

---

## `nimbus start`

Runs the emulator and the visualizer.

| Flag | Default | Meaning |
|---|---|---|
| `--clouds` | `azure` | which clouds to serve: `azure`, `aws`, `gcp`, comma separated |
| `--persist` | off | save state to disk and leave containers running between restarts |
| `--home DIR` | `~/.nimbus` | where certificates and state live |
| `--host NAME` | `localhost` | the hostname clients use to reach Nimbus |
| `--ui-port N` | `4700` | visualizer (http) |
| `--azure-port N` | `4701` | Azure Resource Manager (https) |
| `--aws-port N` | `4702` | AWS (http) |
| `--gcp-port N` | `4703` | Google Cloud (http) |
| `--enforce-iam` | off | actually refuse requests the caller's policies do not allow |
| `--principal ID` | — | act as this principal rather than the account root |

```bash
nimbus start --clouds azure,aws,gcp --persist
```

**On `--persist`.** It does two things: writes state under `--home`, and
leaves Docker containers running when Nimbus stops so a restart adopts them
rather than starting a second set. Ports are reused, so a function keeps
answering on the address it had.

**On `--enforce-iam`.** Off by default, and with it off nothing is ever
refused. With it on, an unmapped caller is the account **root**, which in
real AWS carries no policies and can do anything — so examples that know
nothing about IAM keep working, and a denial is something you opt into by
claiming a limited identity. See [IAM.md](IAM.md).

## `nimbus cert`

Prints the path to the CA certificate and the exact command to trust it on
your OS. Creates the CA on first use. See
[INSTALL.md](INSTALL.md#trust-the-certificate).

| Flag | Default | Meaning |
|---|---|---|
| `--home DIR` | `~/.nimbus` | where the certificates live |

## `nimbus env azure`

Prints the environment variables Terraform's azurerm provider and the Azure
SDKs need, in your shell's syntax — `export` on Unix, `$env:` on Windows.

| Flag | Default | Meaning |
|---|---|---|
| `--storage-account NAME` | — | also print a connection string for that account |
| `--azure-port N` | `4701` | the port Nimbus serves Azure on |

```bash
eval "$(./bin/nimbus env azure)"                       # macOS / Linux
./bin/nimbus.exe env azure | Invoke-Expression          # PowerShell
```

With `--storage-account` it fetches the account's real key from the running
server, so Nimbus must be running and the account must exist.

## `nimbus az register`

Registers Nimbus as a custom cloud for the `az` CLI, so `az login` and every
`az` command talk to the emulator instead of Azure.

| Flag | Default | Meaning |
|---|---|---|
| `--run` | off | run the `az` commands instead of only printing them |
| `--name NAME` | `nimbus` | name for the registered cloud |
| `--host NAME` | `localhost` | hostname clients use |
| `--azure-port N` | `4701` | the port Nimbus serves Azure on |
| `--home DIR` | `~/.nimbus` | where the CA lives |

```bash
./bin/nimbus az register --run
az login --service-principal -u 00000000-0000-0000-0000-000000000002 \
  -p nimbus --tenant 00000000-0000-0000-0000-000000000001 --allow-no-subscriptions
```

Two things it handles for you, both of which are easy to get wrong:

- `az cloud register` fetches the metadata document over TLS **immediately**,
  so the CA has to be trusted before that command, not before your first
  `az keyvault` call. `--run` sets `REQUESTS_CA_BUNDLE` on each child process.
- MSAL validates an unknown authority against the real
  `login.microsoftonline.com` before it will use it. Nimbus sets
  `core.instance_discovery=false`, the switch Azure Stack uses.

To go back to real Azure: `az cloud set -n AzureCloud`.

## `nimbus gcp register`

Prints (or runs) the `gcloud config` commands that point gcloud at Nimbus.

| Flag | Default | Meaning |
|---|---|---|
| `--run` | off | run the commands instead of only printing them |
| `--gcp-port N` | `4703` | the port Nimbus serves GCP on |
| `--host NAME` | `localhost` | hostname clients use |
| `--project ID` | `nimbus-local` | the project to configure |
| `--configuration NAME` | `nimbus` | the gcloud configuration to write |

Warnings about "universe domain" are expected and harmless — see
[GCLOUD.md](GCLOUD.md).

## `nimbus gcp kubeconfig CLUSTER`

Writes a kubeconfig for a GKE cluster that `kubectl` can use directly.

| Flag | Default | Meaning |
|---|---|---|
| `--out PATH` | stdout | write here instead; `-` is stdout |
| `--context NAME` | the cluster's | name for the context |
| `--project ID` | `nimbus-local` | the project the cluster is in |
| `--host NAME` / `--gcp-port N` | `localhost` / `4703` | where Nimbus is |

`gke-gcloud-auth-plugin` cannot work offline — it exists to exchange a Google
OAuth credential, and under `auth/disable_credentials` there is none — so
Nimbus supplies the client side itself. `nimbus gcp install-auth-plugin`
drops a shim if you would rather use `gcloud container clusters
get-credentials`.

## `nimbus ssh INSTANCE`

A shell on a compute instance, by name or id, on any cloud.

| Flag | Default | Meaning |
|---|---|---|
| `--shell NAME` | `sh` | shell to run |
| `--host NAME` | `localhost` | where Nimbus is |
| `--ui-port N` | `4700` | the visualizer port it asks for resources |

```bash
nimbus ssh web                                  # interactive shell
nimbus ssh web -- cat /var/log/nimbus-boot.log  # run one command
```

**It is not SSH.** Nimbus is stdlib-only and cannot run an SSH server, and no
vendor base image ships `sshd`. An instance is a container, so this is
`docker exec`. The name is `ssh` because that is what you would look for.

A command gets no TTY; an interactive shell does. The boot script's output
is at `/var/log/nimbus-boot.log` on every cloud — EC2 calls it user data,
GCE a startup script and Azure custom data, so the file is named after none
of them.

## `nimbus plan-view PLAN`

Overlays a `terraform plan` on the graph before you apply it.

| Flag | Default | Meaning |
|---|---|---|
| `--ui URL` | `http://localhost:4700` | the visualizer to send it to |
| `--clear` | off | remove the overlay instead of adding one |

```bash
terraform plan -out=tf.plan && terraform show -json tf.plan > plan.json
nimbus plan-view plan.json
```

## `nimbus can ACTION RESOURCE`

Asks whether the current principal may do something, and explains why. It
queries the running server's `/api/authz`, so the answer comes from the same
evaluator that enforces — it cannot drift from the behaviour.

| Flag | Default | Meaning |
|---|---|---|
| `--principal ID` | the server's | ask as this principal instead |
| `--ui URL` | `http://localhost:4700` | the running visualizer to ask |
| `--quiet` | off | print nothing; the exit status is the answer |

```bash
nimbus can s3:GetObject arn:aws:s3:::nimbus-data/readme.txt
nimbus can Microsoft.KeyVault/vaults/secrets/getSecret /subscriptions/.../vaults/kv
```

## `nimbus version`

Prints the version. `--version` and `-v` work too.
