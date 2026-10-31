package storage

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// The table service is the odd one out: JSON and OData rather than XML, with
// entities keyed by a (PartitionKey, RowKey) pair and addressed in the path as
//
//	/{table}(PartitionKey='p',RowKey='r')
//
// It also signs requests differently -- see StringToSign.

// Entity is one table row: the two keys plus arbitrary properties.
type Entity struct {
	PartitionKey string
	RowKey       string
	Properties   map[string]any
	Timestamp    time.Time
	ETag         string
}

// Table is a named entity collection.
type Table struct {
	Name string

	mu       sync.RWMutex
	entities map[string]*Entity // keyed by partition + "\x00" + row
}

func entityKey(pk, rk string) string { return pk + "\x00" + rk }

func (a *Account) tableMap() map[string]*Table {
	if a.tables == nil {
		a.tables = map[string]*Table{}
	}
	return a.tables
}

// CreateTable adds a table; ok is false if it already exists.
func (a *Account) CreateTable(name string) (*Table, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, exists := a.tableMap()[strings.ToLower(name)]; exists {
		return t, false
	}
	t := &Table{Name: name, entities: map[string]*Entity{}}
	a.tableMap()[strings.ToLower(name)] = t
	return t, true
}

// Table returns a table, or nil.
func (a *Account) Table(name string) *Table {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tables[strings.ToLower(name)]
}

// DeleteTable removes a table and its entities.
func (a *Account) DeleteTable(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := strings.ToLower(name)
	if _, ok := a.tableMap()[k]; !ok {
		return false
	}
	delete(a.tables, k)
	return true
}

// Tables lists tables by name.
func (a *Account) Tables() []*Table {
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*Table, 0, len(a.tables))
	for _, t := range a.tables {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Put inserts or replaces an entity.
func (t *Table) Put(e *Entity) *Entity {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.Timestamp = time.Now().UTC()
	e.ETag = newETag()
	t.entities[entityKey(e.PartitionKey, e.RowKey)] = e
	return e
}

// Merge updates only the properties present, leaving the rest alone.
func (t *Table) Merge(e *Entity) *Entity {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := entityKey(e.PartitionKey, e.RowKey)
	if cur, ok := t.entities[k]; ok {
		for name, v := range e.Properties {
			cur.Properties[name] = v
		}
		cur.Timestamp = time.Now().UTC()
		cur.ETag = newETag()
		return cur
	}
	e.Timestamp = time.Now().UTC()
	e.ETag = newETag()
	t.entities[k] = e
	return e
}

// Entity returns one entity, or nil.
func (t *Table) Entity(pk, rk string) *Entity {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.entities[entityKey(pk, rk)]
}

// DeleteEntity removes one entity.
func (t *Table) DeleteEntity(pk, rk string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := entityKey(pk, rk)
	if _, ok := t.entities[k]; !ok {
		return false
	}
	delete(t.entities, k)
	return true
}

// Query returns entities matching an OData $filter, in key order.
func (t *Table) Query(filter string) []*Entity {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := []*Entity{}
	for _, e := range t.entities {
		if matchesFilter(e, filter) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PartitionKey != out[j].PartitionKey {
			return out[i].PartitionKey < out[j].PartitionKey
		}
		return out[i].RowKey < out[j].RowKey
	})
	return out
}

// matchesFilter supports the subset this milestone scopes: `eq` comparisons
// joined by `and`. Anything else matches everything rather than returning a
// wrong answer silently -- an over-broad result is obvious in a test, a
// wrongly-empty one is not.
func matchesFilter(e *Entity, filter string) bool {
	filter = strings.TrimSpace(filter)
	if filter == "" {
		return true
	}
	for _, clause := range splitAnd(filter) {
		field, want, ok := parseEq(clause)
		if !ok {
			continue // unsupported operator: do not exclude on a guess
		}
		if !equalsProperty(e, field, want) {
			return false
		}
	}
	return true
}

func splitAnd(filter string) []string {
	parts := []string{}
	depth, start := 0, 0
	lower := strings.ToLower(filter)
	for i := 0; i < len(filter); i++ {
		switch filter[i] {
		case '(':
			depth++
		case ')':
			depth--
		case '\'':
			if j := strings.IndexByte(filter[i+1:], '\''); j >= 0 {
				i += j + 1
			}
		}
		if depth == 0 && strings.HasPrefix(lower[i:], " and ") {
			parts = append(parts, filter[start:i])
			start = i + len(" and ")
			i = start - 1
		}
	}
	return append(parts, filter[start:])
}

func parseEq(clause string) (field, value string, ok bool) {
	c := strings.TrimSpace(strings.Trim(strings.TrimSpace(clause), "()"))
	lower := strings.ToLower(c)
	i := strings.Index(lower, " eq ")
	if i < 0 {
		return "", "", false
	}
	field = strings.TrimSpace(c[:i])
	value = strings.TrimSpace(c[i+len(" eq "):])
	value = strings.Trim(value, "'")
	return field, value, field != ""
}

func equalsProperty(e *Entity, field, want string) bool {
	switch field {
	case "PartitionKey":
		return e.PartitionKey == want
	case "RowKey":
		return e.RowKey == want
	}
	v, ok := e.Properties[field]
	if !ok {
		return false
	}
	switch tv := v.(type) {
	case string:
		return tv == want
	case bool:
		return strings.EqualFold(want, map[bool]string{true: "true", false: "false"}[tv])
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(want, "L"), ".0") == trimFloat(tv)
	}
	return false
}

func trimFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// --- HTTP ------------------------------------------------------------------

func (h *Handler) serveTable(w http.ResponseWriter, r *http.Request, a *Account) string {
	path := strings.TrimPrefix(r.URL.Path, "/")

	// "/Tables" and "/Tables('name')" manage tables themselves.
	if name, isTables := tablesPath(path); isTables {
		return h.tablesOp(w, r, a, name)
	}
	table, pk, rk := parseEntityPath(path)
	if table == "" {
		WriteError(w, 400, "InvalidUri", "The request URI is not valid.")
		return "table: unrecognised path " + path
	}
	return h.entityOp(w, r, a, table, pk, rk)
}

// tablesPath recognises "Tables" and "Tables('name')".
func tablesPath(p string) (name string, ok bool) {
	if !strings.HasPrefix(strings.ToLower(p), "tables") {
		return "", false
	}
	rest := p[len("Tables"):]
	if rest == "" || rest == "/" {
		return "", true
	}
	if strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")") {
		return strings.Trim(rest[1:len(rest)-1], "'"), true
	}
	return "", false
}

// parseEntityPath splits "{table}(PartitionKey='p',RowKey='r')" or "{table}()".
func parseEntityPath(p string) (table, pk, rk string) {
	i := strings.Index(p, "(")
	if i < 0 {
		return p, "", ""
	}
	table = p[:i]
	keys := strings.Trim(p[i:], "()")
	for _, part := range strings.Split(keys, ",") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		v := strings.Trim(strings.TrimSpace(kv[1]), "'")
		switch strings.TrimSpace(kv[0]) {
		case "PartitionKey":
			pk = v
		case "RowKey":
			rk = v
		}
	}
	return table, pk, rk
}

type tableEntry struct {
	TableName string `json:"TableName"`
}

func (h *Handler) tablesOp(w http.ResponseWriter, r *http.Request, a *Account, name string) string {
	switch r.Method {
	case http.MethodPost:
		var req tableEntry
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.TableName == "" {
			WriteError(w, 400, "InvalidInput", "A TableName is required.")
			return ""
		}
		if _, created := a.CreateTable(req.TableName); !created {
			WriteError(w, 409, "TableAlreadyExists", "The table specified already exists.")
			return ""
		}
		writeJSON(w, 201, map[string]any{"TableName": req.TableName})
		return ""

	case http.MethodDelete:
		if !a.DeleteTable(name) {
			WriteError(w, 404, "TableNotFound", "The table specified does not exist.")
			return ""
		}
		setCommonHeaders(w)
		w.WriteHeader(204)
		return ""

	case http.MethodGet:
		items := []any{}
		for _, t := range a.Tables() {
			items = append(items, map[string]any{"TableName": t.Name})
		}
		writeJSON(w, 200, map[string]any{"value": items})
		return ""
	}
	WriteError(w, 400, "InvalidUri", "Unsupported table operation.")
	return "table: unsupported op " + r.Method
}

func (h *Handler) entityOp(w http.ResponseWriter, r *http.Request, a *Account, tableName, pk, rk string) string {
	t := a.Table(tableName)
	if t == nil {
		WriteError(w, 404, "TableNotFound", "The table specified does not exist.")
		return ""
	}

	switch r.Method {
	case http.MethodPost: // insert
		e, err := decodeEntity(r)
		if err != nil {
			WriteError(w, 400, "InvalidInput", err.Error())
			return ""
		}
		if t.Entity(e.PartitionKey, e.RowKey) != nil {
			WriteError(w, 409, "EntityAlreadyExists", "The specified entity already exists.")
			return ""
		}
		t.Put(e)
		w.Header().Set("ETag", e.ETag)
		writeJSON(w, 201, entityJSON(e))
		return ""

	case http.MethodPut, "MERGE", http.MethodPatch: // upsert / merge
		e, err := decodeEntity(r)
		if err != nil {
			WriteError(w, 400, "InvalidInput", err.Error())
			return ""
		}
		if pk != "" {
			e.PartitionKey, e.RowKey = pk, rk
		}
		if r.Method == http.MethodPut {
			e = t.Put(e)
		} else {
			e = t.Merge(e)
		}
		w.Header().Set("ETag", e.ETag)
		setCommonHeaders(w)
		w.WriteHeader(204)
		return ""

	case http.MethodDelete:
		if !t.DeleteEntity(pk, rk) {
			WriteError(w, 404, "ResourceNotFound", "The specified resource does not exist.")
			return ""
		}
		setCommonHeaders(w)
		w.WriteHeader(204)
		return ""

	case http.MethodGet:
		if pk != "" && rk != "" {
			e := t.Entity(pk, rk)
			if e == nil {
				WriteError(w, 404, "ResourceNotFound", "The specified resource does not exist.")
				return ""
			}
			w.Header().Set("ETag", e.ETag)
			writeJSON(w, 200, entityJSON(e))
			return ""
		}
		items := []any{}
		for _, e := range t.Query(r.URL.Query().Get("$filter")) {
			items = append(items, entityJSON(e))
		}
		writeJSON(w, 200, map[string]any{"value": items})
		return ""
	}
	WriteError(w, 400, "InvalidUri", "Unsupported entity operation.")
	return "table: unsupported entity op " + r.Method
}

func decodeEntity(r *http.Request) (*Entity, error) {
	var raw map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&raw); err != nil {
		return nil, errInvalidEntity
	}
	e := &Entity{Properties: map[string]any{}}
	for k, v := range raw {
		switch {
		case k == "PartitionKey":
			e.PartitionKey, _ = v.(string)
		case k == "RowKey":
			e.RowKey, _ = v.(string)
		case strings.HasPrefix(k, "odata."), strings.HasSuffix(k, "@odata.type"):
			// OData annotations describe types; the value itself is enough here.
		default:
			e.Properties[k] = v
		}
	}
	if e.PartitionKey == "" && e.RowKey == "" {
		return nil, errInvalidEntity
	}
	return e, nil
}

var errInvalidEntity = &entityError{"The entity could not be parsed; PartitionKey and RowKey are required."}

type entityError struct{ msg string }

func (e *entityError) Error() string { return e.msg }

func entityJSON(e *Entity) map[string]any {
	out := map[string]any{
		"PartitionKey": e.PartitionKey,
		"RowKey":       e.RowKey,
		"Timestamp":    e.Timestamp.Format(time.RFC3339Nano),
		"odata.etag":   e.ETag,
	}
	for k, v := range e.Properties {
		out[k] = v
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json;odata=minimalmetadata;streaming=true;charset=utf-8")
	setCommonHeaders(w)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
