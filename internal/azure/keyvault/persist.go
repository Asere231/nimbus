package keyvault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/datadir"
)

// Persistence for the vault data plane.
//
// One JSON file per vault: {dir}/{vault}.json, holding every secret, every
// version and the recycle bin. Values are stored in PLAIN TEXT, as they are in
// the other direction too -- a local emulator that encrypted them would be
// pretending to offer a protection it cannot, since the key would sit next to
// the file. Treat ~/.nimbus/data as you would a .env.

// Persist enables saving to dir and loads whatever is already there. Call once
// before use.
func (s *Store) Persist(dir string) error {
	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return s.load()
}

// vaultFile is the on-disk shape.
type vaultFile struct {
	Name        string            `json:"name"`
	Contacts    []Contact         `json:"contacts,omitempty"`
	Secrets     []persistedSecret `json:"secrets,omitempty"`
	Deleted     []persistedSecret `json:"deletedSecrets,omitempty"`
	Keys        []persistedKey    `json:"keys,omitempty"`
	DeletedKeys []persistedKey    `json:"deletedKeys,omitempty"`
	Certs       []*Certificate    `json:"certificates,omitempty"`
	DeletedCert []*Certificate    `json:"deletedCertificates,omitempty"`
}

// persistedKey carries a key's PRIVATE half to disk, which is the one place
// it is allowed to go.
//
// That is not a contradiction of "the private half never leaves": it never
// leaves in a RESPONSE, which is what a client could ever see. The data
// directory already holds secret values in plain text, so a key stored there
// is consistent with its neighbours -- and without it a restart loses every
// key, which is the log-group-index bug one service over. The assertion that
// matters is a CLEAN PLAN after a reload, not that the key is still listed.
type persistedKey struct {
	Name         string                `json:"name"`
	Versions     []persistedKeyVersion `json:"versions"`
	Deleted      time.Time             `json:"deleted,omitempty"`
	RecoveryID   string                `json:"recoveryId,omitempty"`
	ScheduledFor time.Time             `json:"scheduledPurge,omitempty"`
}

type persistedKeyVersion struct {
	Version string            `json:"version"`
	Kty     string            `json:"kty"`
	Curve   string            `json:"crv,omitempty"`
	Ops     []string          `json:"keyOps,omitempty"`
	PKCS8   []byte            `json:"pkcs8"`
	Tags    map[string]string `json:"tags,omitempty"`
	Enabled bool              `json:"enabled"`

	NotBefore *time.Time `json:"nbf,omitempty"`
	Expires   *time.Time `json:"exp,omitempty"`
	Created   time.Time  `json:"created"`
	Updated   time.Time  `json:"updated"`
}

type persistedSecret struct {
	Name         string           `json:"name"`
	Versions     []*SecretVersion `json:"versions"`
	Deleted      time.Time        `json:"deleted,omitempty"`
	RecoveryID   string           `json:"recoveryId,omitempty"`
	ScheduledFor time.Time        `json:"scheduledPurge,omitempty"`
}

// Save writes one vault to disk. Called after every mutation: cheap at
// emulator scale, and it keeps the on-disk copy current rather than depending
// on a clean shutdown.
func (s *Store) Save(v *Vault) {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()
	if dir == "" || v == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	v.mu.Lock()
	file := vaultFile{Name: v.Name, Contacts: v.Contacts}
	for _, sec := range sorted(v.Secrets) {
		file.Secrets = append(file.Secrets, persistedSecret{Name: sec.Name, Versions: sec.Versions})
	}
	for _, sec := range sorted(v.Deleted) {
		file.Deleted = append(file.Deleted, persistedSecret{
			Name: sec.Name, Versions: sec.Versions, Deleted: sec.Deleted,
			RecoveryID: sec.RecoveryID, ScheduledFor: sec.ScheduledFor,
		})
	}
	for _, k := range sortedKeys(v.Keys) {
		file.Keys = append(file.Keys, persistKey(k))
	}
	for _, k := range sortedKeys(v.DeletedKeys) {
		file.DeletedKeys = append(file.DeletedKeys, persistKey(k))
	}
	// Certificates need no conversion: a CertVersion holds DER bytes and raw
	// policy JSON, both of which marshal and unmarshal as they are. Only a
	// KEY needs a persisted form, because its private half is a crypto type
	// rather than data.
	file.Certs = sortedCerts(v.Certs)
	file.DeletedCert = sortedCerts(v.DeletedCerts)
	v.mu.Unlock()

	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	path := vaultPath(dir, v.Name)
	// Written through a temporary file: a half-written vault file would lose
	// every secret in it, not just the one being saved.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func (s *Store) removeFile(name string) {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()
	if dir == "" {
		return
	}
	_ = os.Remove(vaultPath(dir, name))
}

func vaultPath(dir, name string) string {
	return filepath.Join(dir, strings.ToLower(name)+".json")
}

func (s *Store) load() error {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	ignored := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			ignored++
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			ignored++
			continue
		}
		var file vaultFile
		if json.Unmarshal(raw, &file) != nil {
			ignored++
			continue
		}
		v := s.CreateVault(file.Name)
		v.mu.Lock()
		v.Contacts = file.Contacts
		for _, p := range file.Secrets {
			v.Secrets[strings.ToLower(p.Name)] = &Secret{Name: p.Name, Versions: p.Versions}
		}
		for _, p := range file.Deleted {
			v.Deleted[strings.ToLower(p.Name)] = &Secret{
				Name: p.Name, Versions: p.Versions, Deleted: p.Deleted,
				RecoveryID: p.RecoveryID, ScheduledFor: p.ScheduledFor,
			}
		}
		for _, p := range file.Keys {
			if k := restoreKey(p); k != nil {
				v.Keys[strings.ToLower(p.Name)] = k
			} else {
				ignored++
			}
		}
		for _, p := range file.DeletedKeys {
			if k := restoreKey(p); k != nil {
				v.DeletedKeys[strings.ToLower(p.Name)] = k
			} else {
				ignored++
			}
		}
		for _, c := range file.Certs {
			v.Certs[strings.ToLower(c.Name)] = c
		}
		for _, c := range file.DeletedCert {
			v.DeletedCerts[strings.ToLower(c.Name)] = c
		}
		v.mu.Unlock()
	}
	datadir.ReportIgnored("key vault", dir, ignored)
	return nil
}
