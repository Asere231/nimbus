// Command specgen turns Microsoft's OpenAPI specs into the table Nimbus uses
// to fill server-generated fields on resource types it has no hand-written
// hook for.
//
// It is a development tool, not part of the emulator: it lives in its own
// module so the main module stays stdlib-only, and it is the only thing here
// that touches the network. What ships is its OUTPUT
// (internal/azure/spectypes_gen.go), which is committed; the specs are not.
//
// Usage:
//
//	go run ./tools/specgen            # fetch (cached), generate, write the Go file
//	go run ./tools/specgen -report    # also print what could not be derived
//
// The input is types.json: one entry per resource type, naming the swagger
// file in Azure/azure-rest-api-specs and the ARM path whose PUT defines the
// resource. Adding a type means adding an entry there and re-running.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// rawBase is where spec files are fetched from. Pinned to main: the specs are
// append-only in practice, and the generated file is committed, so a drift
// shows up as a reviewable diff rather than a surprise at runtime.
const rawBase = "https://raw.githubusercontent.com/Azure/azure-rest-api-specs/main/"

type entry struct {
	Type       string `json:"type"`
	APIVersion string `json:"apiVersion"`
	Spec       string `json:"spec"`
	Path       string `json:"path"`
}

// field is one thing the server has to fill in, derived from the spec.
type field struct {
	Path     string // dot path within the resource, e.g. "properties.resourceGuid"
	Kind     string // "string", "number", "boolean", "array", "object"
	ReadOnly bool
	Default  any // from the spec's `default`, nil when it states none
}

func main() {
	var (
		out     = flag.String("out", "../../internal/azure/spectypes_gen.go", "file to write")
		input   = flag.String("types", "types.json", "list of types to generate")
		cache   = flag.String("cache", ".cache", "directory for downloaded specs")
		report  = flag.Bool("report", false, "print types whose fields could not be derived")
		offline = flag.Bool("offline", false, "fail instead of downloading anything missing")
	)
	flag.Parse()

	entries, err := readEntries(*input)
	if err != nil {
		fatal(err)
	}
	loader := &loader{dir: *cache, offline: *offline}

	type result struct {
		entry  entry
		fields []field
		err    error
	}
	results := make([]result, 0, len(entries))
	for _, e := range entries {
		fields, misses, err := deriveWithMisses(loader, e)
		results = append(results, result{e, fields, err})
		if *report {
			for _, m := range misses {
				fmt.Fprintf(os.Stderr, "specgen: %s: unresolved ref in %s\n", e.Type, m)
			}
		}
	}

	// A type with no fields is not necessarily a failure: a few really do
	// declare nothing the server fills (Microsoft.Sql firewall rules are two
	// writable IP addresses and nothing else). An error IS a failure.
	var broken, empty int
	for _, r := range results {
		switch {
		case r.err != nil:
			broken++
			fmt.Fprintf(os.Stderr, "specgen: %s: %v\n", r.entry.Type, r.err)
		case len(r.fields) == 0:
			empty++
			if *report {
				fmt.Fprintf(os.Stderr, "specgen: %s: the spec marks no field read-only and states no default\n", r.entry.Type)
			}
		}
	}

	var b strings.Builder
	writeHeader(&b, len(entries))
	for _, r := range results {
		if len(r.fields) == 0 {
			continue
		}
		writeType(&b, r.entry, r.fields)
	}
	writeFooter(&b)

	if err := os.WriteFile(*out, []byte(b.String()), 0o644); err != nil {
		fatal(err)
	}
	fmt.Printf("specgen: %d types, %d with nothing to fill, %d failed -> %s\n",
		len(entries), empty, broken, *out)
	if broken > 0 {
		os.Exit(1)
	}
}

func readEntries(path string) ([]entry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var es []entry
	if err := json.Unmarshal(raw, &es); err != nil {
		return nil, err
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Type < es[j].Type })
	return es, nil
}

// derive finds the PUT body schema for one type and walks it.
func derive(l *loader, e entry) ([]field, error) {
	fields, _, err := deriveWithMisses(l, e)
	return fields, err
}

// deriveWithMisses also returns the $refs that could not be followed.
func deriveWithMisses(l *loader, e entry) ([]field, []string, error) {
	doc, err := l.load(e.Spec)
	if err != nil {
		return nil, nil, err
	}
	schema, err := putSchema(doc, e.Path)
	if err != nil {
		return nil, nil, err
	}
	w := &walker{loader: l, seen: map[string]bool{}}
	fields := w.walk(node{schema, doc}, "", 0)
	sort.Slice(fields, func(i, j int) bool { return fields[i].Path < fields[j].Path })
	return fields, w.misses, nil
}

// putSchema returns the schema of the resource a PUT at this path creates.
//
// The 200/201 RESPONSE is used rather than the request body: they are the same
// definition in practice, but the response is what the spec marks up with
// readOnly, which is the whole point of reading these files.
func putSchema(doc *document, armPath string) (map[string]any, error) {
	paths, _ := doc.root["paths"].(map[string]any)
	item, ok := paths[armPath].(map[string]any)
	if !ok {
		// Case differences happen between the index and the spec.
		for k, v := range paths {
			if strings.EqualFold(k, armPath) {
				item, _ = v.(map[string]any)
				break
			}
		}
	}
	if item == nil {
		return nil, fmt.Errorf("path %s not in spec", armPath)
	}
	put, ok := item["put"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("path %s has no put", armPath)
	}
	responses, _ := put["responses"].(map[string]any)
	for _, code := range []string{"200", "201"} {
		resp, ok := responses[code].(map[string]any)
		if !ok {
			continue
		}
		if schema, ok := resp["schema"].(map[string]any); ok {
			return schema, nil
		}
	}
	return nil, fmt.Errorf("put has no 200/201 schema")
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "specgen:", err)
	os.Exit(1)
}

func writeHeader(b *strings.Builder, n int) {
	fmt.Fprintf(b, `// Code generated by tools/specgen. DO NOT EDIT.
//
// Derived from Microsoft's OpenAPI specs (Azure/azure-rest-api-specs): for
// each type, the fields the SERVER is responsible for -- marked readOnly in
// the spec -- and the defaults the spec states. internal/azure/spec.go
// decides what to actually put in them; this file only records what the spec
// says exists.
//
// Regenerate with: go run ./tools/specgen   (from tools/specgen)
// %d types.

package azure

// specField is one server-filled field: a dot path into the resource JSON,
// the JSON type the spec gives it, and the spec's default if it states one.
type specField struct {
	Path     string
	Kind     string
	ReadOnly bool
	Default  any
}

// specTypes maps a lower-cased ARM type to its fields.
var specTypes = map[string]specTypeInfo{
`, n)
}

func writeType(b *strings.Builder, e entry, fields []field) {
	fmt.Fprintf(b, "\t%q: {\n\t\tAPIVersion: %q,\n\t\tFields: []specField{\n", strings.ToLower(e.Type), e.APIVersion)
	for _, f := range fields {
		fmt.Fprintf(b, "\t\t\t{Path: %q, Kind: %q, ReadOnly: %t", f.Path, f.Kind, f.ReadOnly)
		if f.Default != nil {
			fmt.Fprintf(b, ", Default: %s", goLiteral(f.Default))
		}
		fmt.Fprint(b, "},\n")
	}
	fmt.Fprint(b, "\t\t},\n\t},\n")
}

func writeFooter(b *strings.Builder) {
	fmt.Fprint(b, `}

// specTypeInfo is everything the specs tell us about one resource type.
type specTypeInfo struct {
	APIVersion string
	Fields     []specField
}
`)
}

func goLiteral(v any) string {
	switch x := v.(type) {
	case string:
		return fmt.Sprintf("%q", x)
	case bool:
		return fmt.Sprintf("%t", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("float64(%d)", int64(x))
		}
		return fmt.Sprintf("float64(%v)", x)
	default:
		raw, _ := json.Marshal(v)
		return fmt.Sprintf("%q", string(raw))
	}
}

// specPath joins a $ref's relative file against the document holding it.
func specPath(base, rel string) string {
	return filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(base), rel)))
}
