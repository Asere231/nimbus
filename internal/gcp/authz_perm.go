package gcp

import (
	"net/http"
	"strings"
)

// Which permission a Google API request needs.
//
// GCP's permission names are `{service}.{collection}.{verb}` --
// `compute.networks.create`, `storage.buckets.get` -- which looks as
// derivable as Azure's and is nearly so. Three things get in the way, all
// measured rather than guessed:
//
//   - **Cloud Storage abbreviates its collections in the URL.** `/b/{b}/o`
//     is buckets and objects, so a classifier reading the segment
//     literally asks about `storage.b.get`, which no role grants. There is
//     no rule to derive that from; it is a table.
//   - **Colon verbs are not path segments.** `topics/{t}:publish` and
//     `nodePools/{p}:setSize` put the verb after a colon, where Compute's
//     lifecycle verbs are trailing segments (`instances/{i}/stop`). Three
//     conventions in one cloud, as docs/ROADMAP.md already notes.
//   - **A path cannot be read by guessing which segments are plural.**
//     The first attempt did, since nearly every Google collection is --
//     and so is a Cloud Run service NAMED `s`, which made
//     `/v2/projects/p/locations/l/services/s` come out as `run.s.list`.
//
// The structure IS derivable, though: after the service prefix, the
// version and `projects/{id}`, a Google path is scope segments followed by
// strictly alternating collection/name pairs. That is what this walks.
//
// An unmapped shape REFUSES, naming itself, as every other cloud's does: a
// gap in this classifier must not become a permission nobody granted.

// storageCollections expands Cloud Storage's abbreviated path segments.
var storageCollections = map[string]string{
	"b": "buckets",
	"o": "objects",
}

// permissionFor names the permission a request needs and the resource it
// acts on. An empty permission means "nothing here knows", and the caller
// refuses rather than guessing.
func permissionFor(r *http.Request, parts []string) (permission, resource string) {
	service := apiFor(parts)
	if service == "" {
		return "", ""
	}
	// GKE's permissions are spelled `container.*`, after the product's old
	// name -- not `gke.*`, which is only what the router calls it.
	prefix := service
	if service == apiGKE {
		prefix = "container"
	}

	collection, name, verb := shapeOf(r, parts)
	if collection == "" || verb == "" {
		return "", ""
	}
	// An upload names its object in the QUERY, not the path:
	// POST /upload/storage/v1/b/{bucket}/o?name=reports/today.txt. Without
	// this the resource is empty, and a refusal reads "denied on  to
	// serviceAccount ...", which tells nobody which object.
	if name == "" && collection == "objects" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		// Never empty: a collection-level call acts on the project, and
		// saying so beats a blank in a refusal a person has to read.
		name = "the project"
	}
	return prefix + "." + collection + "." + verb, name
}

// shapeOf reduces a path to the collection it addresses, the resource name
// within it (empty for a collection-level call) and the verb.
func shapeOf(r *http.Request, parts []string) (collection, name, verb string) {
	// A colon verb belongs to the LAST segment: topics/{t}:publish.
	trimmed := make([]string, len(parts))
	copy(trimmed, parts)
	colonVerb := ""
	if n := len(trimmed); n > 0 {
		if i := strings.LastIndex(trimmed[n-1], ":"); i >= 0 {
			colonVerb = trimmed[n-1][i+1:]
			trimmed[n-1] = trimmed[n-1][:i]
			if trimmed[n-1] == "" {
				trimmed = trimmed[:n-1]
			}
		}
	}

	i := 0
	// The service prefix and the version, either of which may be absent:
	// the version belongs to the endpoint for some services and to the
	// client for others, as router.go explains at length.
	for i < len(trimmed) && isPrefixSegment(trimmed[i]) {
		i++
	}

	// projects/{id}. A path addressing the project ITSELF stops here.
	project := ""
	if i+1 < len(trimmed) && trimmed[i] == "projects" {
		project = trimmed[i+1]
		i += 2
	}

	// Scope segments, which look exactly like collections and are not.
	for i < len(trimmed) {
		switch trimmed[i] {
		case "global", "aggregated":
			i++
			continue
		case "regions", "zones", "locations":
			if i+1 < len(trimmed) {
				i += 2
				continue
			}
		}
		break
	}

	if i >= len(trimmed) {
		if project == "" {
			return "", "", ""
		}
		return "projects", project, verbFor(r, true, colonVerb)
	}

	// What is left alternates collection, name, collection, name...
	rest := trimmed[i:]
	// Whether a resource was NAMED, tracked apart from the name itself:
	// an object's name is composed with its bucket's below, and the
	// composed value is non-empty even for a collection-level call. The
	// verb depends on the former -- GET /b/demo/o is a LIST.
	container, named := "", false
	for j := 0; j < len(rest); j += 2 {
		container = name
		collection = rest[j]
		name = ""
		named = j+1 < len(rest)
		if named {
			name = rest[j+1]
		}
		if expanded, ok := storageCollections[collection]; ok {
			collection = expanded
		}
		// An object name is a flat key that MAY CONTAIN SLASHES --
		// `img/logo.svg` is one object -- so this leaf takes the whole
		// remainder rather than one segment, and stops: otherwise the
		// alternation reads the rest of the key as more collections.
		// Only `objects` stops, not every abbreviated segment: `b` is
		// followed by the bucket AND then by `o`, so breaking there made
		// every object call ask about `storage.buckets.*`.
		if collection == "objects" {
			if j+1 < len(rest) {
				name = strings.Join(rest[j+1:], "/")
			}
			// An object is named WITH ITS BUCKET, which is the one
			// place a resource's container is part of its identity
			// here. Two reasons, and both are load-bearing: a bucket's
			// own IAM binding has to reach its objects, which is what
			// everybody means by granting access to a bucket; and a
			// refusal reading "denied on today.txt" names an object in
			// no particular bucket, which tells nobody anything.
			if container != "" && name != "" {
				name = container + "/" + name
			} else if container != "" {
				name = container
			}
			break
		}
	}
	return collection, name, verbFor(r, named, colonVerb)
}

// isPrefixSegment reports a leading service or version segment, which
// carries no permission meaning.
func isPrefixSegment(seg string) bool {
	switch seg {
	case "compute", "storage", "upload", "iam", "v1", "v1beta1", "v2", "v3":
		return true
	}
	return false
}

// verbFor maps the method to a permission verb.
//
// A colon verb wins when there is one, because it says exactly what is
// being asked. Otherwise POST means `create` against a collection and
// `update` against a resource -- GCP uses POST for both. A Pub/Sub topic
// is created with PUT on its own URL, which is one of this cloud's three
// conventions for "create".
func verbFor(r *http.Request, named bool, colonVerb string) string {
	if colonVerb != "" {
		return colonVerb
	}
	switch r.Method {
	case http.MethodGet:
		if named {
			return "get"
		}
		return "list"
	case http.MethodPost:
		if named {
			return "update"
		}
		return "create"
	case http.MethodPut:
		return "create"
	case http.MethodPatch:
		return "update"
	case http.MethodDelete:
		return "delete"
	}
	return ""
}
