// Package storage emulates the Azure storage data plane: the XML APIs that
// live at {account}.{service}.{suffix}, separate from the ARM control plane.
//
// ARM creates the account; everything a client then does with blobs, queues
// and tables happens here, authenticated with the account's Shared Key rather
// than a bearer token.
package storage

import (
	"crypto/rand"
	"encoding/base64"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account is one storage account's data plane: its keys and its contents.
type Account struct {
	Name    string
	Keys    [2]string // base64, as listKeys returns them
	Created time.Time

	mu         sync.RWMutex
	containers map[string]*Container
	queues     map[string]*Queue
	tables     map[string]*Table
	// ServiceProps is the blob service settings document, kept as the exact
	// XML last written so it round-trips byte for byte.
	ServiceProps map[Service][]byte
}

// Store holds every emulated storage account. It is the data-plane counterpart
// to core.Store, which keeps only the ARM representation. Blob content is
// arbitrary binary, so it does not belong in core.Store's JSON bodies.
type Store struct {
	mu       sync.RWMutex
	accounts map[string]*Account // keyed by lower-cased account name
}

// NewStore creates an empty in-memory store.
func NewStore() *Store {
	return &Store{accounts: map[string]*Account{}}
}

func key(name string) string { return strings.ToLower(name) }

// CreateAccount adds an account if it does not exist, and returns it. Keys are
// generated once and then stable, so a client that cached a connection string
// keeps working across account updates.
func (s *Store) CreateAccount(name string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a, ok := s.accounts[key(name)]; ok {
		return a
	}
	a := &Account{
		Name: name, Keys: [2]string{newKey(), newKey()}, Created: time.Now().UTC(),
		containers:   map[string]*Container{},
		queues:       map[string]*Queue{},
		tables:       map[string]*Table{},
		ServiceProps: map[Service][]byte{},
	}
	s.accounts[key(name)] = a
	return a
}

// Account returns the account, or nil. Names are case-insensitive: Azure
// account names are lower-case by rule, but clients are not always careful.
func (s *Store) Account(name string) *Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.accounts[key(name)]
}

// DeleteAccount removes an account and everything in it.
func (s *Store) DeleteAccount(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.accounts, key(name))
}

// Accounts lists every account, by name.
func (s *Store) Accounts() []*Account {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Account, 0, len(s.accounts))
	for _, a := range s.accounts {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RegenerateKey replaces key 0 or 1 and returns the account.
func (s *Store) RegenerateKey(name string, which int) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.accounts[key(name)]
	if a == nil || which < 0 || which > 1 {
		return a
	}
	a.Keys[which] = newKey()
	return a
}

// newKey makes an account key shaped like Azure's: 64 random bytes, base64.
// Clients base64-decode it and use it as the HMAC-SHA256 secret, so the length
// matters to them even though the value is arbitrary.
func newKey() string {
	var b [64]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}
