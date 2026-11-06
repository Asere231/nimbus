package azure

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"testing"

	"github.com/Asere231/nimbus/internal/core"
)

// Azure's data-plane names are passed to core.EnsureCerts by cmd/nimbus, and
// a client reaching {account}.blob.{suffix} needs every level covered. A
// wildcard matches exactly ONE label, so "*.storage.nimbus.localhost" does
// NOT cover "st1.blob.storage.nimbus.localhost" -- each level has its own.
func TestCertificateCoversEveryAzureDataPlaneName(t *testing.T) {
	certs, err := core.EnsureCerts(t.TempDir(), DNSNames()...)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(certs.Cert)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("the server certificate is not PEM")
	}
	crt, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{
		"localhost",
		"storage.nimbus.localhost",
		"st1.blob.storage.nimbus.localhost",
		"st1.queue.storage.nimbus.localhost",
		"st1.table.storage.nimbus.localhost",
		"st1.file.storage.nimbus.localhost",
		"kv1.vault.nimbus.localhost",
	} {
		if err := crt.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %s: %v\n  SANs: %v", host, err, crt.DNSNames)
		}
	}
}
