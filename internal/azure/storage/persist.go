package storage

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/fsname"
)

// Persistence for the storage data plane.
//
// Blob content is arbitrary binary and can be large, so it is NOT folded into
// core.Store's single state.json. Each account gets a directory:
//
//	{dir}/{account}/account.json          keys, service properties, queues, tables
//	{dir}/{account}/blobs/{container}/... one file per blob, content verbatim
//	{dir}/{account}/blobs/{container}/.container.json  container metadata
//
// Blobs stay as plain files so you can look at what an emulated upload
// produced with `cat`, which is most of the point of a local emulator.

// Persist enables saving to dir. Call once, before use. Existing data in dir
// is loaded immediately.
func (s *Store) Persist(dir string) error {
	s.mu.Lock()
	s.dir = dir
	s.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return s.load()
}

// accountFile is the JSON shape written per account. Blob CONTENT is excluded
// -- it lives in its own file -- but blob metadata is here so a blob's
// properties survive even if its bytes are read back lazily.
type accountFile struct {
	Name    string               `json:"name"`
	Keys    [2]string            `json:"keys"`
	Created time.Time            `json:"created"`
	Props   map[string][]byte    `json:"serviceProperties,omitempty"`
	Queues  []persistedQueue     `json:"queues,omitempty"`
	Tables  []persistedTable     `json:"tables,omitempty"`
	Blobs   []persistedBlob      `json:"blobs,omitempty"`
	Contain []persistedContainer `json:"containers,omitempty"`
}

type persistedContainer struct {
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Access   string            `json:"publicAccess,omitempty"`
	ETag     string            `json:"etag"`
	Modified time.Time         `json:"modified"`
}

type persistedBlob struct {
	Container   string            `json:"container"`
	Name        string            `json:"name"`
	ContentType string            `json:"contentType,omitempty"`
	ContentMD5  string            `json:"contentMD5,omitempty"`
	Cache       string            `json:"cacheControl,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	ETag        string            `json:"etag"`
	Modified    time.Time         `json:"modified"`
	File        string            `json:"file"` // path relative to the account dir
}

type persistedQueue struct {
	Name     string            `json:"name"`
	Metadata map[string]string `json:"metadata,omitempty"`
	Messages []*QueueMessage   `json:"messages,omitempty"`
}

type persistedTable struct {
	Name     string    `json:"name"`
	Entities []*Entity `json:"entities,omitempty"`
}

// save writes everything for one account. Called after any mutation; cheap
// enough at emulator scale, and it keeps the on-disk copy always current.
func (s *Store) save(a *Account) {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()
	if dir == "" || a == nil {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	acctDir := filepath.Join(dir, strings.ToLower(a.Name))
	if err := os.MkdirAll(acctDir, 0o755); err != nil {
		return
	}

	f := accountFile{Name: a.Name, Keys: a.Keys, Created: a.Created, Props: map[string][]byte{}}
	for svc, raw := range a.ServiceProps {
		f.Props[string(svc)] = raw
	}

	for _, c := range a.Containers() {
		f.Contain = append(f.Contain, persistedContainer{
			Name: c.Name, Metadata: c.Metadata, Access: c.PublicAccess,
			ETag: c.ETag, Modified: c.Modified,
		})
		for _, b := range c.Blobs("") {
			// Both names go through fsname.Encode, which is the identity
			// for an ordinary lowercase name and escapes what a filesystem
			// cannot hold. The manifest keeps the REAL name beside this
			// path, so load never has to decode it -- but the path still
			// has to be one every machine can write, and the same one on
			// each, or a persisted account stops being portable.
			rel := filepath.Join("blobs",
				fsname.Encode(c.Name), filepath.FromSlash(fsname.Encode(b.Name)))
			// A failed write is REPORTED, not swallowed. It used to `continue`
			// in silence, so a blob whose name the filesystem refused served
			// perfectly from memory and was gone after a restart with nothing
			// saying why. With fsname.Encode in front, a failure here means
			// something real rather than an awkward name.
			full := filepath.Join(acctDir, rel)
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				log.Printf("nimbus: could not persist blob %s/%s: %v", c.Name, b.Name, err)
				continue
			}
			if err := os.WriteFile(full, b.Content, 0o644); err != nil {
				log.Printf("nimbus: could not persist blob %s/%s: %v", c.Name, b.Name, err)
				continue
			}
			f.Blobs = append(f.Blobs, persistedBlob{
				Container: c.Name, Name: b.Name, ContentType: b.ContentType,
				ContentMD5: b.ContentMD5, Cache: b.CacheControl, Metadata: b.Metadata,
				ETag: b.ETag, Modified: b.Modified, File: rel,
			})
		}
	}
	for _, q := range a.Queues() {
		q.mu.Lock()
		msgs := append([]*QueueMessage(nil), q.messages...)
		q.mu.Unlock()
		f.Queues = append(f.Queues, persistedQueue{Name: q.Name, Metadata: q.Metadata, Messages: msgs})
	}
	for _, t := range a.Tables() {
		t.mu.RLock()
		ents := make([]*Entity, 0, len(t.entities))
		for _, e := range t.entities {
			ents = append(ents, e)
		}
		t.mu.RUnlock()
		f.Tables = append(f.Tables, persistedTable{Name: t.Name, Entities: ents})
	}

	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmp := filepath.Join(acctDir, "account.json.tmp")
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(acctDir, "account.json"))
	}
}

// load reads every account back.
func (s *Store) load() error {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		acctDir := filepath.Join(s.dir, e.Name())
		data, err := os.ReadFile(filepath.Join(acctDir, "account.json"))
		if err != nil {
			continue // a directory without a manifest is not ours
		}
		var f accountFile
		if json.Unmarshal(data, &f) != nil {
			continue
		}
		a := &Account{
			Name: f.Name, Keys: f.Keys, Created: f.Created,
			containers: map[string]*Container{}, queues: map[string]*Queue{},
			tables: map[string]*Table{}, ServiceProps: map[Service][]byte{},
		}
		for svc, raw := range f.Props {
			a.ServiceProps[Service(svc)] = raw
		}
		for _, pc := range f.Contain {
			a.containers[strings.ToLower(pc.Name)] = &Container{
				Name: pc.Name, Metadata: pc.Metadata, PublicAccess: pc.Access,
				ETag: pc.ETag, Modified: pc.Modified, blobs: map[string]*Blob{},
			}
		}
		for _, pb := range f.Blobs {
			c := a.containers[strings.ToLower(pb.Container)]
			if c == nil {
				continue
			}
			content, err := os.ReadFile(filepath.Join(acctDir, pb.File))
			if err != nil {
				continue
			}
			c.blobs[pb.Name] = &Blob{
				Name: pb.Name, Content: content, ContentType: pb.ContentType,
				ContentMD5: pb.ContentMD5, CacheControl: pb.Cache, Metadata: pb.Metadata,
				BlobType: "BlockBlob", ETag: pb.ETag, Modified: pb.Modified,
			}
		}
		for _, pq := range f.Queues {
			a.queues[strings.ToLower(pq.Name)] = &Queue{
				Name: pq.Name, Metadata: pq.Metadata, messages: pq.Messages,
			}
		}
		for _, pt := range f.Tables {
			tbl := &Table{Name: pt.Name, entities: map[string]*Entity{}}
			for _, ent := range pt.Entities {
				tbl.entities[entityKey(ent.PartitionKey, ent.RowKey)] = ent
			}
			a.tables[strings.ToLower(pt.Name)] = tbl
		}
		s.accounts[key(a.Name)] = a
	}
	return nil
}

// removeAccountDir deletes an account's data from disk.
func (s *Store) removeAccountDir(name string) {
	s.mu.RLock()
	dir := s.dir
	s.mu.RUnlock()
	if dir == "" {
		return
	}
	_ = os.RemoveAll(filepath.Join(dir, strings.ToLower(name)))
}

// Save persists one account, if persistence is on. Handlers call this after a
// mutation.
func (s *Store) Save(a *Account) { s.save(a) }
