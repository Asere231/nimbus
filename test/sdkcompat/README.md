# SDK compatibility harness

Drives a running Nimbus with `hashicorp/go-azure-sdk`, the SDK inside the Terraform azurerm provider. It checks environment discovery from `metadata_host`, client-secret auth, PUT + polling, paged lists and DELETE + polling.

```bash
# terminal 1
go run ./cmd/nimbus start
# terminal 2 (CA trusted, or on Linux set SSL_CERT_FILE=~/.nimbus/certs/nimbus-ca.pem)
cd test/sdkcompat && go mod tidy && go run . -endpoint https://localhost:4701
```

The SDK is pinned to `v0.20250513.1112057` so it builds with Go 1.22+. Newer SDK versions need a newer Go.
