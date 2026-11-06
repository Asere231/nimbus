package aws

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Where items live.
//
// On disk under {DataDir}/dynamodb when --persist is on, in memory
// otherwise -- the same choice the S3 object store makes, for the same
// reason. One file per table, because a table is the unit everything here
// operates on and a whole table is small enough to rewrite.
//
// An item is stored as the map it arrived as: {"id": {"S": "1"}}. Nothing
// is decoded into Go types and re-encoded, so a number keeps the exact
// digits the client sent -- DynamoDB numbers are decimal strings on the
// wire precisely because they do not fit a float, and parsing one into a
// float64 and printing it back is how "1e+06" appears where "1000000" was
// written.

type itemStore struct {
	mu  sync.RWMutex
	dir string // empty = memory only
	// table -> item key -> item
	mem map[string]map[string]map[string]any
}

func newItemStore(dataDir string) *itemStore {
	s := &itemStore{mem: map[string]map[string]map[string]any{}}
	if dataDir != "" {
		s.dir = filepath.Join(dataDir, "dynamodb")
		_ = os.MkdirAll(s.dir, 0o755)
	}
	return s
}

// load reads every table file back, so a restart with --persist still has
// the items. Called once at startup.
func (s *itemStore) load() {
	if s.dir == "" {
		return
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			continue
		}
		var items map[string]map[string]any
		if err := json.Unmarshal(body, &items); err != nil {
			continue
		}
		s.mem[decodeTableFile(e.Name())] = items
	}
}

// flush writes one table out. Called with the lock held.
func (s *itemStore) flush(table string) {
	if s.dir == "" {
		return
	}
	body, err := json.Marshal(s.mem[table])
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(s.dir, encodeTableFile(table)), body, 0o644)
}

// A table name can contain characters that are legal in a name and awkward
// in a filename ('.', '-' are fine; nothing else is allowed by DynamoDB, so
// this is belt and braces rather than a real escape).
func encodeTableFile(table string) string { return table + ".json" }
func decodeTableFile(file string) string  { return strings.TrimSuffix(file, ".json") }

// put stores an item under its key and returns what was there before, so
// PutItem can answer ReturnValues=ALL_OLD.
func (s *itemStore) put(table, key string, item map[string]any) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mem[table] == nil {
		s.mem[table] = map[string]map[string]any{}
	}
	old := s.mem[table][key]
	s.mem[table][key] = item
	s.flush(table)
	return old
}

func (s *itemStore) get(table, key string) map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mem[table][key]
}

func (s *itemStore) delete(table, key string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.mem[table][key]
	delete(s.mem[table], key)
	s.flush(table)
	return old
}

// all returns every item in a table, in key order.
//
// Sorted, because a Query or a Scan that returned items in map order would
// come back differently every time -- and a client paging through with
// ExclusiveStartKey would then see items twice or not at all.
func (s *itemStore) all(table string) []map[string]any {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.mem[table]))
	for k := range s.mem[table] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, s.mem[table][k])
	}
	return out
}

func (s *itemStore) dropTable(table string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.mem, table)
	if s.dir != "" {
		_ = os.Remove(filepath.Join(s.dir, encodeTableFile(table)))
	}
}

// stats is the item count and approximate size DescribeTable reports.
// Counted on read rather than kept in step by every write, which is one
// fewer thing that can drift.
func (s *itemStore) stats(table string) (count, bytes int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, item := range s.mem[table] {
		count++
		if body, err := json.Marshal(item); err == nil {
			bytes += len(body)
		}
	}
	return count, bytes
}
