# Milestone 4: Key Vault, managed identities, ACR

## Goal

Run the setup real Azure apps use: a container image pushed to Azure Container Registry, pulled by AKS, reading a secret from Key Vault through a managed identity. All local.

## Build

1. **Managed identities** (`Microsoft.ManagedIdentity/userAssignedIdentities`): generate stable `principalId`, `clientId`, `tenantId`. System-assigned `identity` blocks on any resource get IDs filled in too (generic: in the ARM handler, whenever a body has `identity.type` containing `SystemAssigned`).
2. **Role assignments** (`Microsoft.Authorization/roleAssignments`, an extension resource; `parseTarget` already handles the path): store them; `roleDefinitions` list returns the built-in roles Terraform looks up by name (Owner, Contributor, Reader, Key Vault Secrets User, AcrPull, …) with their real GUIDs. Enforcement stays off; record that decision in `CLAUDE.md`.
3. **Key Vault**: ARM side fills `properties.vaultUri` = `https://{name}.vault.nimbus.localhost/`. Data plane on that host (same DNS approach as Milestone 2): secrets set/get/list/delete/versions, soft delete + purge (azurerm purges on destroy by default), and the `401` + `WWW-Authenticate` challenge the Key Vault SDKs expect before they send a token.
4. **ACR**: ARM side for `Microsoft.ContainerRegistry/registries` fills `loginServer`. Backing: one `registry:2` container per registry (reuse the Docker wrapper from Milestone 3) on a mapped port, so `docker push localhost:<port>/app:v1` works. Make the k3s clusters trust and resolve that registry (k3s `registries.yaml` mirror).
5. **Log Analytics workspace** stub: `customerId`, `listKeys`/`sharedKeys` action, so AKS monitoring add-on configs apply.
6. **Visualizer**: identities drawn with dashed links to the resources they're assigned to; Key Vault inspector lists secret names (not values) and versions.

## Ask the owner

- Should secret values ever show in the visualizer? (Recommend: names only, with a "reveal" button.)

## Done when

- [ ] `examples/terraform/azure-app`: RG, ACR, AKS, Key Vault + secret, user-assigned identity, role assignment, and a `kubernetes_deployment` (Terraform kubernetes provider) running an image I pushed to the local ACR that reads the secret. Apply, clean plan, destroy.
- [ ] `az keyvault secret set/show` work against the vault.
- [ ] Destroy purges soft-deleted vaults so a re-apply with the same name works.

## Watch out for

- azurerm's Key Vault resource waits for the vault's DNS name to resolve before continuing; this fails if the Milestone 2 DNS approach doesn't cover `*.vault.nimbus.localhost`.
- The Key Vault data plane uses a different token audience (`https://vault.azure.net` style, from our metadata `keyVaultDns`); the token endpoint must issue tokens for any scope.
