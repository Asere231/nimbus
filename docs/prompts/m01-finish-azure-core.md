# Milestone 1 (finish): prove the Azure core with real Terraform

## Goal

The code for Milestone 1 exists and passes unit tests plus the `hashicorp/go-azure-sdk` harness, but the real azurerm provider has never run against it. Make `examples/terraform/azure-basic` apply, plan clean, and destroy on my machine, harden what that exposes, and add CI so it stays green.

## Starting state

- Built in a sandbox that could not download Terraform. Verified: `go test ./...`, cross-compiles for macOS and Windows, `test/sdkcompat` passes (environment from `metadata_host`, client-secret token, PUT + polling, paged list, DELETE + polling).
- Not verified: the azurerm provider (its own validation, read-after-create checks, import checks), certificate trust on macOS/Windows, `--persist` across restarts, the visualizer in a real browser session.

## Steps

1. **Environment check.** Ask me to run and paste: `go version`, `terraform version`, OS and CPU (`uname -a` or `systeminfo`). Then have me build, run `./bin/nimbus cert`, run the trust command it prints, and start Nimbus.
2. **SDK harness on my machine.** Have me run `test/sdkcompat` against the running server. If it fails here but passed in the sandbox, it's a certificate-trust or OS issue: fix that first.
3. **Terraform, step by step.** Have me run, from `examples/terraform/azure-basic`:
   - `terraform init`
   - `TF_LOG=DEBUG terraform apply -auto-approve 2> tf.log` (PowerShell: `$env:TF_LOG="DEBUG"; terraform apply -auto-approve 2> tf.log`)
   - `terraform plan -detailed-exitcode`: must exit `0` (no changes). A non-empty plan means we return fields differently from what was sent; fix the round-trip.
   - `terraform destroy -auto-approve`
   For each failure: read the request/response pair from `tf.log`, write a failing test in `internal/azure` that replays it, fix, rebuild, rerun.
4. **Second run.** Apply again into the same Nimbus process (state already empty), then restart Nimbus with `--persist` between apply and destroy to prove persistence.
5. **Import.** `terraform import` the resource group into a fresh state and confirm plan is clean.
6. **Harden.** Add tests for: `PATCH` on a missing resource, `PUT` with invalid JSON, `DELETE` of a resource group with nested resources (cascade + events), case-insensitive IDs in every handler, and the visualizer API (`/api/resources`, `/api/resource`, `/api/reset`, SSE first message).
7. **CI.** Add `.github/workflows/ci.yml`: matrix `ubuntu-latest`, `macos-latest`, `windows-latest`; steps `gofmt` check (Linux only), `go vet`, `go test ./...`, build. A second Linux-only job that installs Terraform (`hashicorp/setup-terraform`), starts Nimbus in the background, trusts the CA with `SSL_CERT_FILE`, and runs apply → plan (exit 0) → destroy on `azure-basic`.
8. **Visualizer check.** Ask me to open http://localhost:4700 during an apply and describe or screenshot it. Fix anything that renders wrong in my browser.

## Ask the owner

- Which azurerm version to pin in the example (default: latest 4.x that `terraform init` picks).

## Done when

- [ ] `azure-basic`: apply succeeds, `plan -detailed-exitcode` exits 0, destroy succeeds, on my Mac (and Windows if I have it available this session).
- [ ] The same works after a Nimbus restart with `--persist`.
- [ ] `terraform import` of the resource group gives a clean plan.
- [ ] CI is green on all three OSes, including the Terraform job.
- [ ] Every bug found has a regression test; new protocol facts are added to `CLAUDE.md`.

## Watch out for

- azurerm normalises some values on read (locations, casing of IDs, address prefixes). A plan that never settles is almost always a field we echo differently.
- `azurerm_subnet_network_security_group_association` is a GET + PUT of the subnet with `properties.networkSecurityGroup.id`. Make sure the VNet's embedded subnet list picks up that change too.
- macOS: Go programs (Terraform providers included) use the system keychain, so `SSL_CERT_FILE` does nothing there; the CA must be trusted in the keychain.
