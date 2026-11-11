package azure

// The hostnames Azure's data planes live under.
//
// These are Azure's business, not the core's: a client builds
// {account}.{service}.{suffix} or {vault}.{suffix} from the metadata
// document and connects to it, so the names have to resolve to loopback and
// the server certificate has to cover them. Nested *.localhost names do
// resolve to loopback on macOS and Windows with nothing installed, which is
// why no DNS tooling is needed -- see CLAUDE.md.
//
// A TLS wildcard matches exactly ONE label, so each level needs its own
// entry: "*.storage.nimbus.localhost" does not cover
// "st1.blob.storage.nimbus.localhost".

// StorageSuffix is the DNS suffix the storage data plane lives under.
const StorageSuffix = "storage.nimbus.localhost"

// KeyVaultSuffix is the DNS suffix Key Vault's data plane lives under.
const KeyVaultSuffix = "vault.nimbus.localhost"

// DNSNames is every name a certificate needs for Azure's data planes.
// cmd/nimbus passes it to core.EnsureCerts when the azure cloud is running.
func DNSNames() []string {
	return []string{
		StorageSuffix,
		"*." + StorageSuffix,
		"*.blob." + StorageSuffix,
		"*.queue." + StorageSuffix,
		"*.table." + StorageSuffix,
		"*.file." + StorageSuffix,
		"*.dfs." + StorageSuffix,
		KeyVaultSuffix,
		"*." + KeyVaultSuffix,
		ContainerAppsSuffix,
		"*." + ContainerAppsSuffix,
	}
}
