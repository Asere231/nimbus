package keyvault

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
)

// A vault's CERTIFICATES.
//
// Same shape as keys and secrets -- a name with ordered versions and the
// soft-delete fields -- for the same reason: Key Vault never overwrites, and
// reusing the shape means the recycle bin needs no new machinery.

// Certificate is one certificate and all of its versions, newest last.
type Certificate struct {
	Name     string
	Versions []*CertVersion
	// RequestID is reported by the certificate OPERATION. It is fixed at
	// create rather than regenerated per read, because a client polling the
	// operation compares it.
	RequestID string

	Deleted      time.Time
	RecoveryID   string
	ScheduledFor time.Time
}

// CertVersion is one issued certificate.
type CertVersion struct {
	Version string
	DER     []byte
	// KeyVersion and SecretVersion tie this certificate to the key version
	// that signed it and the secret version that holds its PEM bundle, so
	// the bundle's `kid` and `sid` point at those rather than at whatever
	// the key's or secret's latest version happens to be.
	//
	// Both MUST be versioned in the response. azurerm parses `sid` with a
	// versioned-id parser and fails the apply with "expected a versioned
	// ID" -- an error that names neither the field nor the certificate, and
	// arrives after the certificate has already been created.
	KeyVersion    string
	SecretVersion string
	// Policy is the raw JSON the client sent, echoed back verbatim. See
	// certPolicyJSON for why it is not normalised.
	Policy  json.RawMessage
	Tags    map[string]string
	Enabled bool
	Created time.Time
	Updated time.Time
}

func (c *Certificate) Latest() *CertVersion {
	if len(c.Versions) == 0 {
		return nil
	}
	return c.Versions[len(c.Versions)-1]
}

func (c *Certificate) Version(id string) *CertVersion {
	if id == "" {
		return c.Latest()
	}
	for _, v := range c.Versions {
		if strings.EqualFold(v.Version, id) {
			return v
		}
	}
	return nil
}

// AddCertificate stores a newly issued certificate as a new version.
func (v *Vault) AddCertificate(name string, der []byte, keyVersion, secretVersion string, policy json.RawMessage, tags map[string]string) *CertVersion {
	ver := &CertVersion{
		Version:       newVersion(),
		DER:           der,
		KeyVersion:    keyVersion,
		SecretVersion: secretVersion,
		Policy:        policy,
		Tags:          tags,
		Enabled:       true,
		Created:       time.Now().UTC(),
		Updated:       time.Now().UTC(),
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.Certs[strings.ToLower(name)]
	if c == nil {
		c = &Certificate{Name: name, RequestID: newVersion()}
		v.Certs[strings.ToLower(name)] = c
	}
	c.Versions = append(c.Versions, ver)
	return ver
}

func (v *Vault) Certificate(name string) *Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Certs[strings.ToLower(name)]
}

func (v *Vault) DeletedCertificate(name string) *Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.DeletedCerts[strings.ToLower(name)]
}

func (v *Vault) ListCertificates() []*Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sortedCerts(v.Certs)
}

func (v *Vault) ListDeletedCertificates() []*Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sortedCerts(v.DeletedCerts)
}

func sortedCerts(m map[string]*Certificate) []*Certificate {
	out := make([]*Certificate, 0, len(m))
	for _, c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

func (v *Vault) DeleteCertificate(name string, retention time.Duration) *Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.Certs[strings.ToLower(name)]
	if c == nil {
		return nil
	}
	delete(v.Certs, strings.ToLower(name))
	c.Deleted = time.Now().UTC()
	c.ScheduledFor = c.Deleted.Add(retention)
	v.DeletedCerts[strings.ToLower(name)] = c
	return c
}

func (v *Vault) RecoverCertificate(name string) *Certificate {
	v.mu.Lock()
	defer v.mu.Unlock()
	c := v.DeletedCerts[strings.ToLower(name)]
	if c == nil {
		return nil
	}
	delete(v.DeletedCerts, strings.ToLower(name))
	c.Deleted = time.Time{}
	c.ScheduledFor = time.Time{}
	v.Certs[strings.ToLower(name)] = c
	return c
}

func (v *Vault) PurgeCertificate(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.DeletedCerts[strings.ToLower(name)]; !ok {
		return false
	}
	delete(v.DeletedCerts, strings.ToLower(name))
	return true
}

// readLimited reads a whole body. Separate from the json decoders used
// elsewhere because a certificate's POLICY has to be kept as RAW JSON to be
// echoed back byte-for-byte, which means reading the body once and unmarshalling
// it twice rather than streaming it.
func readLimited(r io.Reader) ([]byte, error) { return io.ReadAll(r) }
