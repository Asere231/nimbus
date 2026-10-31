package azure

import "strings"

// Spec-driven defaults.
//
// spectypes_gen.go records what Microsoft's OpenAPI specs say about each type:
// which fields are readOnly (the server owns them) and which carry a default.
// This file decides what actually goes in them, which the specs do NOT say.
//
// Three rules keep that honest:
//
//  1. Never overwrite the client. The table fills gaps; a value that was sent
//     is always kept, including one sent as false or 0.
//  2. Never invent a nested object. A default only applies inside an object
//     the client already established, so a client that sent no `ingress` block
//     does not read one back. Otherwise the config and the API disagree
//     forever, which is the same failure as Content-MD5.
//  3. Everything generated is DERIVED from the resource ID, never random.
//     Two identical writes must produce byte-identical reads, or Terraform
//     plans dirty for ever.
//
// Rule 3 is why a readOnly string we have no sensible value for is left out
// rather than filled with something invented: absent is what Azure returns for
// a field with no value, and it cannot drift.

// isModelled reports whether anything beyond the generic handler knows about
// this type: a hand-written hook, or an entry in the spec table. It is what
// separates the visualizer's "modelled" tier from "accepted (state only)".
func (s *Server) isModelled(typ string) bool {
	key := strings.ToLower(typ)
	if _, ok := s.hooks[key]; ok {
		return true
	}
	_, ok := specTypes[key]
	return ok
}

// applySpecDefaults fills the fields the spec says the server owns. It runs
// before the type's hook, so a hook always has the last word.
func applySpecDefaults(t target, body map[string]any) {
	info, ok := specTypes[strings.ToLower(t.Type)]
	if !ok {
		return
	}
	for _, f := range info.Fields {
		parent, leaf, ok := walkTo(body, f.Path)
		if !ok {
			continue // the enclosing object was not sent: rule 2
		}
		if _, taken := parent[leaf]; taken {
			continue // the client said something: rule 1
		}
		if v, ok := specValue(t, f); ok {
			parent[leaf] = v
		}
	}
}

// walkTo returns the map that should hold the last segment of path, and that
// segment. ok is false when any intermediate object is missing or is not an
// object, which is how rule 2 is enforced.
func walkTo(body map[string]any, path string) (parent map[string]any, leaf string, ok bool) {
	segs := strings.Split(path, ".")
	cur := body
	for _, seg := range segs[:len(segs)-1] {
		next, isMap := cur[seg].(map[string]any)
		if !isMap {
			return nil, "", false
		}
		cur = next
	}
	return cur, segs[len(segs)-1], true
}

// specValue produces the value for one field. The spec's own default wins;
// otherwise the field's name decides, and anything unrecognised is skipped.
func specValue(t target, f specField) (any, bool) {
	if f.Default != nil {
		return f.Default, true
	}
	if !f.ReadOnly {
		return nil, false
	}
	leaf := f.Path[strings.LastIndex(f.Path, ".")+1:]
	lower := strings.ToLower(leaf)

	switch {
	case lower == "provisioningstate":
		return "Succeeded", true
	case lower == "etag":
		return etagFor(t.ID), true
	case strings.HasSuffix(lower, "guid"), lower == "uniqueid", lower == "objectid",
		lower == "principalid", lower == "clientid", lower == "tenantid":
		return identityGUID(t.ID, f.Path), true
	}

	// A read-only collection is answered empty rather than absent: clients
	// iterate these without checking, and an empty list is the truth for a
	// resource nothing has attached to yet.
	switch f.Kind {
	case "array":
		return []any{}, true
	case "object":
		return nil, false // an empty object says less than no object at all
	}
	return nil, false
}
