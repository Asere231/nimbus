package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// document is one swagger file plus the repo-relative path it came from, which
// is what relative $refs inside it are resolved against.
type document struct {
	path string
	root map[string]any
}

// loader fetches spec files and caches them on disk, so a re-run is offline
// and a reviewer can see exactly which bytes the table was derived from.
type loader struct {
	dir     string
	offline bool
	docs    map[string]*document
}

func (l *loader) load(path string) (*document, error) {
	if l.docs == nil {
		l.docs = map[string]*document{}
	}
	if d, ok := l.docs[path]; ok {
		return d, nil
	}
	raw, err := l.bytes(path)
	if err != nil {
		return nil, err
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	d := &document{path: path, root: root}
	l.docs[path] = d
	return d, nil
}

func (l *loader) bytes(path string) ([]byte, error) {
	// The cache mirrors the repo's own layout, so a cached tree is readable
	// and the test fixtures under testdata are laid out the same way.
	local := filepath.Join(l.dir, filepath.FromSlash(path))
	if raw, err := os.ReadFile(local); err == nil {
		return raw, nil
	}
	if l.offline {
		return nil, fmt.Errorf("%s is not cached and -offline was given", path)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	res, err := client.Get(rawBase + path)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", path, res.Status)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(local, raw, 0o644); err != nil {
		return nil, err
	}
	return raw, nil
}

// resolve follows a $ref, which may point into the same file ("#/definitions/X")
// or into another one ("./common.json#/definitions/X", and the deep relative
// paths into common-types that every RP uses). It returns the schema and the
// document it now lives in, because any further $refs inside it are relative
// to THAT file, not the one we started from.
func (l *loader) resolve(ref string, in *document) (map[string]any, *document, error) {
	file, pointer, _ := strings.Cut(ref, "#")
	doc := in
	if file != "" {
		var err error
		doc, err = l.load(specPath(in.path, file))
		if err != nil {
			return nil, nil, err
		}
	}
	node := any(doc.root)
	for _, seg := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if seg == "" {
			continue
		}
		seg = strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("ref %s: %q is not an object", ref, seg)
		}
		node, ok = m[seg]
		if !ok {
			return nil, nil, fmt.Errorf("ref %s: %q not found", ref, seg)
		}
	}
	schema, ok := node.(map[string]any)
	if !ok {
		return nil, nil, fmt.Errorf("ref %s does not point at a schema", ref)
	}
	return schema, doc, nil
}
