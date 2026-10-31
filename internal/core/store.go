// Package core holds the cloud-agnostic engine: the resource store, the event
// bus, certificates and persistence. Every cloud plugin (azure, aws, gcp)
// writes resources here, and the visualizer reads from here.
package core

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Resource is the cloud-agnostic record of one emulated resource.
// Body is the exact JSON the cloud API returns for it (for Azure, the ARM
// representation), so plugins can echo it back unchanged.
type Resource struct {
	Cloud     string          `json:"cloud"`     // "azure", "aws", "gcp"
	ID        string          `json:"id"`        // canonical cloud ID (ARM ID, ARN, GCP self link)
	Type      string          `json:"type"`      // e.g. "Microsoft.Network/virtualNetworks"
	Name      string          `json:"name"`      // short name
	ParentID  string          `json:"parentId"`  // containing resource: RG, VNet, subscription, account...
	Location  string          `json:"location"`  // region, if any
	Backing   string          `json:"backing"`   // "state" (accepted only) or "real" (a local engine runs it)
	Body      json.RawMessage `json:"body"`      // the API representation
	CreatedAt time.Time       `json:"createdAt"` //
	UpdatedAt time.Time       `json:"updatedAt"` //
}

// ErrNotFound is returned when a resource does not exist.
var ErrNotFound = errors.New("resource not found")

// Store is a thread-safe, in-memory resource store with optional JSON
// persistence. Keys are case-insensitive, because Azure resource IDs are.
type Store struct {
	mu        sync.RWMutex
	resources map[string]*Resource
	bus       *Bus
	file      string // empty = memory only
	saveMu    sync.Mutex
}

// NewStore creates a store. If dataDir is not empty, state is loaded from and
// saved to dataDir/state.json so it survives restarts.
func NewStore(bus *Bus, dataDir string) (*Store, error) {
	s := &Store{resources: map[string]*Resource{}, bus: bus}
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, err
		}
		s.file = filepath.Join(dataDir, "state.json")
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func key(id string) string { return strings.ToLower(strings.TrimRight(id, "/")) }

// Get returns a copy of the resource with the given ID.
func (s *Store) Get(id string) (*Resource, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.resources[key(id)]
	if !ok {
		return nil, ErrNotFound
	}
	c := *r
	return &c, nil
}

// Exists reports whether a resource exists.
func (s *Store) Exists(id string) bool {
	_, err := s.Get(id)
	return err == nil
}

// Put creates or replaces a resource and publishes a create/update event.
// It returns true if the resource was newly created.
func (s *Store) Put(r *Resource) bool {
	now := time.Now().UTC()
	s.mu.Lock()
	existing, found := s.resources[key(r.ID)]
	if found {
		r.CreatedAt = existing.CreatedAt
	} else {
		r.CreatedAt = now
	}
	r.UpdatedAt = now
	c := *r
	s.resources[key(r.ID)] = &c
	s.mu.Unlock()

	action := "create"
	if found {
		action = "update"
	}
	s.bus.Publish(Event{Kind: "resource", Action: action, Cloud: r.Cloud, ResourceID: r.ID, ResourceType: r.Type, Name: r.Name, ParentID: r.ParentID})
	s.save()
	return !found
}

// Delete removes a resource and everything nested under it (children first),
// publishing a delete event for each. It returns ErrNotFound if absent.
//
// The resources it removed are returned, deepest first, so a caller can run
// per-type cleanup for every one of them -- a type whose hook stops a real
// engine must get the chance to stop it when an ANCESTOR is deleted, not only
// when it is deleted directly.
func (s *Store) Delete(id string) ([]*Resource, error) {
	s.mu.Lock()
	if _, ok := s.resources[key(id)]; !ok {
		s.mu.Unlock()
		return nil, ErrNotFound
	}
	// Collect the resource and its descendants by ID prefix.
	prefix := key(id) + "/"
	var doomed []*Resource
	for k, r := range s.resources {
		if k == key(id) || strings.HasPrefix(k, prefix) || s.isDescendantLocked(r, key(id)) {
			doomed = append(doomed, r)
		}
	}
	// Deepest first, so the visualizer removes leaves before parents.
	sort.Slice(doomed, func(i, j int) bool { return len(doomed[i].ID) > len(doomed[j].ID) })
	for _, r := range doomed {
		delete(s.resources, key(r.ID))
	}
	s.mu.Unlock()

	for _, r := range doomed {
		s.bus.Publish(Event{Kind: "resource", Action: "delete", Cloud: r.Cloud, ResourceID: r.ID, ResourceType: r.Type, Name: r.Name, ParentID: r.ParentID})
	}
	s.save()
	return doomed, nil
}

// isDescendantLocked follows ParentID links (for resources whose IDs are not
// nested by path, such as a resource group's members). Caller holds the lock.
func (s *Store) isDescendantLocked(r *Resource, ancestor string) bool {
	seen := 0
	for p := r.ParentID; p != "" && seen < 32; seen++ {
		if key(p) == ancestor {
			return true
		}
		next, ok := s.resources[key(p)]
		if !ok {
			return false
		}
		p = next.ParentID
	}
	return false
}

// List returns resources matching the filter, sorted by ID.
func (s *Store) List(filter func(*Resource) bool) []*Resource {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Resource, 0, len(s.resources))
	for _, r := range s.resources {
		if filter == nil || filter(r) {
			c := *r
			out = append(out, &c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Children returns the direct children of a resource.
func (s *Store) Children(parentID string) []*Resource {
	return s.List(func(r *Resource) bool { return key(r.ParentID) == key(parentID) })
}

// Reset deletes everything (used by `POST /_nimbus/reset` and tests).
func (s *Store) Reset() {
	s.mu.Lock()
	s.resources = map[string]*Resource{}
	s.mu.Unlock()
	s.bus.Publish(Event{Kind: "reset", Action: "reset"})
	s.save()
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []*Resource
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	for _, r := range list {
		s.resources[key(r.ID)] = r
	}
	return nil
}

func (s *Store) save() {
	if s.file == "" {
		return
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	list := s.List(nil)
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := s.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.file)
}
