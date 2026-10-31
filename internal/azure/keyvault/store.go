// Package keyvault emulates Azure Key Vault's data plane: the service that
// lives on {vault}.vault.nimbus.localhost rather than on the Resource Manager,
// and that actually holds secret values.
//
// Only secrets are emulated. Keys and certificates answer with a clear error
// instead of an empty success, because a cryptographic operation that silently
// does nothing is worse than one that fails.
package keyvault

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"time"
)

// Store holds every vault's data.
type Store struct {
	mu  sync.RWMutex
	m   map[string]*Vault // keyed by lower-case vault name
	dir string            // persistence directory, empty = memory only

	saveMu sync.Mutex
}

// NewStore creates an empty, in-memory store.
func NewStore() *Store { return &Store{m: map[string]*Vault{}} }

// Vault is one vault's secrets.
type Vault struct {
	Name string
	// Secrets and Deleted are keyed by lower-case secret name. Key Vault
	// names are case-insensitive for lookup but reported as created.
	Secrets map[string]*Secret
	Deleted map[string]*Secret

	mu sync.Mutex
}

// Secret is one secret and all of its versions, newest last.
type Secret struct {
	Name     string
	Versions []*SecretVersion
	// Deleted is set when the secret is in the vault's recycle bin.
	Deleted      time.Time
	RecoveryID   string
	ScheduledFor time.Time
}

// SecretVersion is one version of a secret. Key Vault never overwrites: every
// set creates a version, and old versions stay readable by their ID.
type SecretVersion struct {
	Version     string
	Value       string
	ContentType string
	Tags        map[string]string
	Enabled     bool
	NotBefore   *time.Time
	Expires     *time.Time
	Created     time.Time
	Updated     time.Time
}

// Latest returns the newest version, or nil for a secret with none.
func (s *Secret) Latest() *SecretVersion {
	if len(s.Versions) == 0 {
		return nil
	}
	return s.Versions[len(s.Versions)-1]
}

// Version finds one version by its ID.
func (s *Secret) Version(id string) *SecretVersion {
	for _, v := range s.Versions {
		if strings.EqualFold(v.Version, id) {
			return v
		}
	}
	return nil
}

// CreateVault adds a vault, or returns the existing one. Called by the ARM
// hook when the control-plane resource is created.
func (s *Store) CreateVault(name string) *Vault {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(name)
	if v, ok := s.m[key]; ok {
		return v
	}
	v := &Vault{Name: name, Secrets: map[string]*Secret{}, Deleted: map[string]*Secret{}}
	s.m[key] = v
	return v
}

// Vault returns a vault, or nil.
func (s *Store) Vault(name string) *Vault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[strings.ToLower(name)]
}

// Vaults returns every vault, in name order, for the visualizer.
func (s *Store) Vaults() []*Vault {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Vault, 0, len(s.m))
	for _, v := range s.m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeleteVault forgets a vault and everything in it. This is the PURGE path:
// soft-deleting the ARM resource deliberately leaves the data here, so a
// recovered vault still has its secrets.
func (s *Store) DeleteVault(name string) {
	s.mu.Lock()
	delete(s.m, strings.ToLower(name))
	s.mu.Unlock()
	s.removeFile(name)
}

// --- secrets ---------------------------------------------------------------

// SetSecret adds a version to a secret, creating the secret if needed, and
// returns the new version.
func (v *Vault) SetSecret(name, value, contentType string, tags map[string]string, attrs Attributes) *SecretVersion {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := strings.ToLower(name)

	// Setting a secret that is in the recycle bin is a conflict in Azure, so
	// callers check Deleted first; this only has to not resurrect it silently.
	sec := v.Secrets[key]
	if sec == nil {
		sec = &Secret{Name: name}
		v.Secrets[key] = sec
	}
	now := time.Now().UTC().Truncate(time.Second)
	ver := &SecretVersion{
		Version: newVersion(), Value: value, ContentType: contentType,
		Tags: tags, Enabled: true, Created: now, Updated: now,
		NotBefore: attrs.NotBefore, Expires: attrs.Expires,
	}
	if attrs.Enabled != nil {
		ver.Enabled = *attrs.Enabled
	}
	sec.Versions = append(sec.Versions, ver)
	return ver
}

// Attributes is the subset of Key Vault's attributes object Nimbus keeps.
type Attributes struct {
	Enabled   *bool
	NotBefore *time.Time
	Expires   *time.Time
}

// Secret returns a live secret, or nil.
func (v *Vault) Secret(name string) *Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Secrets[strings.ToLower(name)]
}

// DeletedSecret returns a secret from the recycle bin, or nil.
func (v *Vault) DeletedSecret(name string) *Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.Deleted[strings.ToLower(name)]
}

// List returns the live secrets in name order.
func (v *Vault) List() []*Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sorted(v.Secrets)
}

// ListDeleted returns the recycle bin in name order.
func (v *Vault) ListDeleted() []*Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	return sorted(v.Deleted)
}

func sorted(m map[string]*Secret) []*Secret {
	out := make([]*Secret, 0, len(m))
	for _, s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// DeleteSecret moves a secret to the recycle bin, where it keeps all its
// versions and can be recovered or purged. Returns nil if there was no such
// secret.
func (v *Vault) DeleteSecret(name string, retention time.Duration) *Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := strings.ToLower(name)
	sec := v.Secrets[key]
	if sec == nil {
		return nil
	}
	delete(v.Secrets, key)
	sec.Deleted = time.Now().UTC().Truncate(time.Second)
	sec.ScheduledFor = sec.Deleted.Add(retention)
	v.Deleted[key] = sec
	return sec
}

// RecoverSecret moves a secret back out of the recycle bin.
func (v *Vault) RecoverSecret(name string) *Secret {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := strings.ToLower(name)
	sec := v.Deleted[key]
	if sec == nil {
		return nil
	}
	delete(v.Deleted, key)
	sec.Deleted = time.Time{}
	sec.ScheduledFor = time.Time{}
	v.Secrets[key] = sec
	return sec
}

// PurgeSecret removes a secret from the recycle bin for good.
func (v *Vault) PurgeSecret(name string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := strings.ToLower(name)
	if _, ok := v.Deleted[key]; !ok {
		return false
	}
	delete(v.Deleted, key)
	return true
}

// UpdateVersion changes a version's attributes, tags or content type without
// creating a new version -- Key Vault's PATCH.
func (v *Vault) UpdateVersion(name, version string, contentType *string, tags map[string]string, attrs Attributes) *SecretVersion {
	v.mu.Lock()
	defer v.mu.Unlock()
	sec := v.Secrets[strings.ToLower(name)]
	if sec == nil {
		return nil
	}
	ver := sec.Latest()
	if version != "" {
		ver = sec.Version(version)
	}
	if ver == nil {
		return nil
	}
	if contentType != nil {
		ver.ContentType = *contentType
	}
	if tags != nil {
		ver.Tags = tags
	}
	if attrs.Enabled != nil {
		ver.Enabled = *attrs.Enabled
	}
	if attrs.NotBefore != nil {
		ver.NotBefore = attrs.NotBefore
	}
	if attrs.Expires != nil {
		ver.Expires = attrs.Expires
	}
	ver.Updated = time.Now().UTC().Truncate(time.Second)
	return ver
}

// newVersion makes a Key Vault version identifier: 32 lower-case hex
// characters, which is what the service returns and what clients store.
func newVersion() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
