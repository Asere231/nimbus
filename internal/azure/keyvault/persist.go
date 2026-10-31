package keyvault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	Name    string            `json:"name"`
	Secrets []persistedSecret `json:"secrets,omitempty"`
	Deleted []persistedSecret `json:"deletedSecrets,omitempty"`
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
	file := vaultFile{Name: v.Name}
	for _, sec := range sorted(v.Secrets) {
		file.Secrets = append(file.Secrets, persistedSecret{Name: sec.Name, Versions: sec.Versions})
	}
	for _, sec := range sorted(v.Deleted) {
		file.Deleted = append(file.Deleted, persistedSecret{
			Name: sec.Name, Versions: sec.Versions, Deleted: sec.Deleted,
			RecoveryID: sec.RecoveryID, ScheduledFor: sec.ScheduledFor,
		})
	}
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
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var file vaultFile
		if json.Unmarshal(raw, &file) != nil {
			continue
		}
		v := s.CreateVault(file.Name)
		v.mu.Lock()
		for _, p := range file.Secrets {
			v.Secrets[strings.ToLower(p.Name)] = &Secret{Name: p.Name, Versions: p.Versions}
		}
		for _, p := range file.Deleted {
			v.Deleted[strings.ToLower(p.Name)] = &Secret{
				Name: p.Name, Versions: p.Versions, Deleted: p.Deleted,
				RecoveryID: p.RecoveryID, ScheduledFor: p.ScheduledFor,
			}
		}
		v.mu.Unlock()
	}
	return nil
}
