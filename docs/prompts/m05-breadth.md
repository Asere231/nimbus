# Milestone 5: breadth — API specs, ARM/Bicep deployments, the az CLI

## Goal

Move from "accepted" to "modelled" for many resource types at once using Microsoft's own API specs, support template deployments (ARM JSON and Bicep), and make the `az` CLI work against Nimbus as a custom cloud.

## Build

1. **Spec-driven defaults.** Write a generator in `tools/specgen` (its own Go module; may use deps) that reads OpenAPI files from a local clone of `Azure/azure-rest-api-specs` and emits a Go table for the top resource types: read-only properties the server must fill (e.g. `resourceGuid`, `provisioningState`, IDs of nested objects), and default values. The emulator applies the table generically in `put()`. Commit the generated file, not the specs. Ask me which ~50 types to include; propose a list based on the AZ-104/AZ-305 exam scope and common landing zones.
2. **Template deployments** (`Microsoft.Resources/deployments` at resource group and subscription scope): validate and deploy ARM JSON templates. Implement the expression language subset Bicep output uses: `parameters()`, `variables()`, `resourceId()`, `resourceGroup()`, `subscription()`, `concat()`, `format()`, `uniqueString()`, `reference()` (basic), `copy` loops, `dependsOn` ordering. Each resource in the template goes through the normal ARM handler, so it appears in the visualizer. `what-if` returns a basic change list.
3. **az CLI as a custom cloud.** Document and script `az cloud register -n Nimbus --endpoint-resource-manager https://localhost:4701 --endpoint-active-directory https://localhost:4701 ...` then `az login --service-principal`. Implement what MSAL needs (OpenID config, instance discovery with authority validation off, token responses MSAL accepts). Add `nimbus az-setup` that prints or runs the commands.

## Done when

- [ ] `az group create`, `az network vnet create`, `az resource list` work.
- [ ] `az deployment group create --template-file examples/bicep/main.bicep` deploys a VNet + NSG + storage account and they appear in the visualizer.
- [ ] A test matrix (`test/types`) round-trips 50+ resource types through PUT → GET → list → DELETE with spec-correct read-only fields.
- [ ] The accepted-only count in the visualizer header drops accordingly.

## Watch out for

- The specs repo is huge: sparse-checkout `specification/*/resource-manager` only.
- `uniqueString()` must be deterministic and match Azure's algorithm if templates compare names; check a known input/output pair.
