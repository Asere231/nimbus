package gcp

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// The GCE metadata server, as far as a container can reach it.
//
// Real GCE metadata is metadata.google.internal, which resolves to
// 169.254.169.254 -- a link-local address a container cannot reach without
// a route only root can add. So Nimbus serves each instance's metadata on
// its own ordinary port and tells the container where it is with
// GCE_METADATA_HOST, which Google's own client libraries read.
//
// That variable is host:port with NO SCHEME, where AWS's
// AWS_EC2_METADATA_SERVICE_ENDPOINT is a full URL. Same idea, different
// spelling, which is why the vm engine hands the plugin both forms and lets
// it choose rather than deciding for it.
//
// Two things real GCE does that this does not: it mints access tokens at
// .../service-accounts/default/token, and it answers only to requests
// carrying Metadata-Flavor: Google. Nimbus mints no tokens -- there is no
// OAuth here at all -- and does not insist on the header, because refusing
// a request for a header nobody can see is a debugging session nobody
// needs.

// instanceMetadata builds the handler for one instance's metadata.
//
// The paths are GCE's: /computeMetadata/v1/... with a directory listing for
// anything ending in "/". Values are plain text; ?recursive=true answers
// JSON, which is what the client libraries actually ask for.
func (s *Server) instanceMetadata(link string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Real GCE sends this on every answer and clients check it.
		w.Header().Set("Metadata-Flavor", "Google")

		res, err := s.store.Get(link)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if json.Unmarshal(res.Body, &body) != nil {
			http.NotFound(w, r)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/computeMetadata/v1/")
		path = strings.TrimPrefix(path, "computeMetadata/v1/")
		doc := s.metadataDocument(body)

		// ?recursive=true answers JSON, and real GCE NESTS it: the path
		// segments are object keys, not a flat map with slashes in the
		// keys. A client walking `instance.attributes["startup-script"]`
		// finds nothing in the flat shape, and nothing says why.
		if r.URL.Query().Get("recursive") == "true" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(nestMetadata(doc, path))
			return
		}
		value, ok := metadataLookup(doc, path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, value)
	})
}

// metadataDocument is the flat path -> value map an instance can read.
func (s *Server) metadataDocument(body map[string]any) map[string]string {
	zone := lastSegment(str(body["zone"]))
	doc := map[string]string{
		"instance/id":                str(body["id"]),
		"instance/name":              str(body["name"]),
		"instance/hostname":          str(body["name"]) + "." + zone + ".c." + s.cfg.ProjectID + ".internal",
		"instance/zone":              "projects/" + s.cfg.ProjectID + "/zones/" + zone,
		"instance/machine-type":      "projects/" + s.cfg.ProjectID + "/zones/" + zone + "/machineTypes/" + lastSegment(str(body["machineType"])),
		"instance/cpu-platform":      str(body["cpuPlatform"]),
		"project/project-id":         s.cfg.ProjectID,
		"project/numeric-project-id": string(numericID(s.cfg.ProjectID)),
	}
	if nics, ok := body["networkInterfaces"].([]any); ok && len(nics) > 0 {
		if nic, ok := nics[0].(map[string]any); ok {
			doc["instance/network-interfaces/0/ip"] = str(nic["networkIP"])
			doc["instance/network-interfaces/0/subnetmask"] = "255.255.255.0"
		}
	}
	// Whatever the instance was given, under instance/attributes -- which
	// is where a startup script lives, and where anything custom goes.
	if meta, ok := body["metadata"].(map[string]any); ok {
		if items, ok := meta["items"].([]any); ok {
			for _, it := range items {
				if item, ok := it.(map[string]any); ok {
					if k := str(item["key"]); k != "" {
						doc["instance/attributes/"+k] = str(item["value"])
					}
				}
			}
		}
	}
	return doc
}

// metadataLookup answers one path, or lists the keys below it.
func metadataLookup(doc map[string]string, path string) (string, bool) {
	if v, ok := doc[path]; ok {
		return v, true
	}
	if path != "" && !strings.HasSuffix(path, "/") {
		return "", false
	}
	var keys []string
	seen := map[string]bool{}
	for k := range doc {
		if !strings.HasPrefix(k, path) {
			continue
		}
		rest := strings.TrimPrefix(k, path)
		if cut := strings.Index(rest, "/"); cut >= 0 {
			rest = rest[:cut+1]
		}
		if rest != "" && !seen[rest] {
			seen[rest] = true
			keys = append(keys, rest)
		}
	}
	if len(keys) == 0 {
		return "", false
	}
	sortStrings(keys)
	return strings.Join(keys, "\n"), true
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}

// nestMetadata turns the flat path -> value map into the nested object
// real GCE answers a recursive query with, rooted at prefix.
func nestMetadata(doc map[string]string, prefix string) map[string]any {
	out := map[string]any{}
	for path, value := range doc {
		if !strings.HasPrefix(path, prefix) {
			continue
		}
		segments := strings.Split(strings.TrimPrefix(path, prefix), "/")
		node := out
		for i, seg := range segments {
			if seg == "" {
				continue
			}
			if i == len(segments)-1 {
				node[seg] = value
				break
			}
			child, ok := node[seg].(map[string]any)
			if !ok {
				child = map[string]any{}
				node[seg] = child
			}
			node = child
		}
	}
	return out
}
