package main

import (
	"fmt"
	"strings"
)

// maxDepth bounds how deep into a resource we record fields. Three levels
// reaches properties.x.y, which covers the server-filled fields that matter
// (properties.resourceGuid, properties.hostName, properties.sku.tier) without
// dragging in the enormous nested shapes compute and network specs describe.
const maxDepth = 3

// node is a schema together with the FILE it was declared in. The pairing is
// the whole trick: a $ref inside a schema is relative to that schema's own
// file, so merging an allOf that pulls TrackedResource out of common-types
// must not make the resource's other properties resolve against common-types.
// Getting this wrong silently yields zero fields rather than an error.
type node struct {
	schema map[string]any
	doc    *document
}

// walker turns a resolved schema into a flat list of fields.
type walker struct {
	loader *loader
	seen   map[string]bool // $refs on the current path, so cycles terminate
	// misses records $refs that could not be followed. A bad ref otherwise
	// fails silently -- the schema resolves to nothing and the type simply
	// gains no fields -- which is the hardest kind of bug to notice here.
	misses []string
}

func (w *walker) walk(n node, prefix string, depth int) []field {
	if depth > maxDepth || n.schema == nil {
		return nil
	}
	props := w.properties(n)
	var out []field
	for name, child := range props {
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		// id/name/type are set by put() before the table is applied, and
		// location, tags and identity are the caller's to send.
		if depth == 0 && skipTopLevel[strings.ToLower(name)] {
			continue
		}
		resolved := w.resolve(child)
		kind := jsonKind(resolved.schema)
		readOnly := isReadOnly(child.schema) || isReadOnly(resolved.schema)
		def := resolved.schema["default"]
		if d, ok := child.schema["default"]; ok {
			def = d
		}
		if readOnly || def != nil {
			out = append(out, field{Path: path, Kind: kind, ReadOnly: readOnly, Default: def})
		}
		// Recurse into objects to find nested read-only fields, but not into
		// a read-only object: the applier fills that whole subtree at once.
		if kind == "object" && !readOnly {
			out = append(out, w.walk(resolved, path, depth+1)...)
		}
	}
	return out
}

// resolve follows a $ref, keeping the file the target came from. Siblings of
// the $ref (readOnly, description, default) still apply and win.
func (w *walker) resolve(n node) node {
	ref, ok := n.schema["$ref"].(string)
	if !ok {
		return n
	}
	key := n.doc.path + ref
	if w.seen[key] {
		return node{map[string]any{}, n.doc}
	}
	w.seen[key] = true
	defer delete(w.seen, key)

	target, targetDoc, err := w.loader.resolve(ref, n.doc)
	if err != nil {
		w.misses = append(w.misses, fmt.Sprintf("%s: %v", n.doc.path, err))
		return node{map[string]any{}, n.doc}
	}
	inner := w.resolve(node{target, targetDoc})
	merged := map[string]any{}
	for k, v := range inner.schema {
		merged[k] = v
	}
	for k, v := range n.schema {
		if k != "$ref" {
			merged[k] = v
		}
	}
	return node{merged, inner.doc}
}

// properties returns the schema's properties, each paired with the file it
// came from, with allOf merged in. A property declared directly wins over one
// inherited through allOf, which is how the specs intend it.
func (w *walker) properties(n node) map[string]node {
	n = w.resolve(n)
	out := map[string]node{}
	if own, ok := n.schema["properties"].(map[string]any); ok {
		for name, raw := range own {
			if m, ok := raw.(map[string]any); ok {
				out[name] = node{m, n.doc}
			}
		}
	}
	allOf, _ := n.schema["allOf"].([]any)
	for _, raw := range allOf {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for name, child := range w.properties(node{part, n.doc}) {
			if _, taken := out[name]; !taken {
				out[name] = child
			}
		}
	}
	return out
}

// isReadOnly covers both ways the specs say it. x-ms-mutability listing only
// "read" means the same thing, and several RPs use it instead of readOnly.
func isReadOnly(schema map[string]any) bool {
	if ro, ok := schema["readOnly"].(bool); ok && ro {
		return true
	}
	mut, ok := schema["x-ms-mutability"].([]any)
	if !ok || len(mut) == 0 {
		return false
	}
	for _, m := range mut {
		if s, _ := m.(string); s != "read" {
			return false
		}
	}
	return true
}

func jsonKind(schema map[string]any) string {
	if t, ok := schema["type"].(string); ok {
		if t == "integer" {
			return "number"
		}
		return t
	}
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	if _, ok := schema["allOf"]; ok {
		return "object"
	}
	if _, ok := schema["additionalProperties"]; ok {
		return "object"
	}
	return "string"
}

var skipTopLevel = map[string]bool{
	"id": true, "name": true, "type": true, "location": true,
	"tags": true, "identity": true, "systemdata": true,
}
