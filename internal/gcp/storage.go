package gcp

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
)

// Cloud Storage over the JSON API.
//
// Buckets are `core.Resource`s; objects are not -- there can be thousands
// of them and their names are case-sensitive, while the store's keys are
// not. That is the same split S3 got in Milestone 7, for the same two
// reasons.
//
// The JSON API addresses everything by path (`/storage/v1/b/{bucket}`), so
// unlike Azure's blob endpoint no bucket name ever becomes a hostname, and
// nothing here needs TLS or DNS.

func (s *Server) serveStorage(w http.ResponseWriter, r *http.Request, parts []string) string {
	// parts: storage / v1 / b [/ {bucket} [/ o [/ {object}]]]
	if len(parts) < 3 || parts[0] != "storage" || parts[2] != "b" {
		writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
		return "storage " + r.URL.Path
	}
	rest := parts[3:]

	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		return s.listBuckets(w)
	case len(rest) == 0 && r.Method == http.MethodPost:
		return s.createBucket(w, r)
	case len(rest) == 1 && r.Method == http.MethodGet:
		return s.getBucket(w, rest[0])
	case len(rest) == 1 && (r.Method == http.MethodPatch || r.Method == http.MethodPut):
		return s.patchBucket(w, r, rest[0])
	case len(rest) == 1 && r.Method == http.MethodDelete:
		return s.deleteBucket(w, r, rest[0])
	}

	// Anywhere Caches, which matter far more than they look.
	//
	// The provider asks for them INSIDE its force_destroy loop, and its own
	// code breaks out of that loop if the call errors -- so a 404 here does
	// not produce a 404 anywhere the user can see it. It silently skips
	// deleting the bucket's objects, and the destroy then fails on our own
	// "bucket is not empty" with nothing to connect the two. Measured
	// against google 6.50.0: the provider listed the object and deleted the
	// bucket without ever trying to delete the object.
	//
	// Nimbus emulates no caches, so the honest answer is an empty list --
	// and an empty list is also what keeps force_destroy working. This is
	// the S3 sub-resource lesson in its most expensive form yet: it is not
	// enough for an unimplemented sub-resource to fail politely.
	if len(rest) == 2 && rest[0] != "" && rest[1] == "anywhereCaches" {
		writeJSON(w, http.StatusOK, map[string]any{
			"kind": "storage#anywhereCaches", "items": []any{},
		})
		return ""
	}

	// `gcloud storage cp` asks for this before it uploads anything, to
	// decide whether the bucket is hierarchical. Measured; a 404 does not
	// stop the upload, but it puts a red line in the activity feed for
	// something that is working.
	if len(rest) == 2 && rest[1] == "storageLayout" && r.Method == http.MethodGet {
		return s.storageLayout(w, rest[0])
	}

	// Object paths. The object name may itself contain slashes, which is
	// why it is rejoined rather than read as one segment -- "img/logo.svg"
	// is one object, not two.
	if len(rest) >= 2 && rest[1] == "o" {
		bucket := rest[0]
		object := strings.Join(rest[2:], "/")
		return s.serveObject(w, r, bucket, object)
	}
	// A resumable upload session: /storage/v1/b/{b}/o with upload_id.
	writeError(w, notFound("Nimbus does not implement %s %s yet", r.Method, r.URL.Path))
	return "storage " + r.Method + " " + r.URL.Path
}

// --- buckets ---------------------------------------------------------------

func (s *Server) loadBucket(name string) (map[string]any, *apiError) {
	res, err := s.store.Get(s.bucketLink(name))
	if err != nil || res.Type != TypeBucket {
		return nil, errorf(http.StatusNotFound, "notFound", "NOT_FOUND",
			"The specified bucket does not exist.")
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return nil, internalError(err)
	}
	return doc, nil
}

func (s *Server) saveBucket(doc map[string]any) *apiError {
	name := str(doc["name"])
	body, err := json.Marshal(doc)
	if err != nil {
		return internalError(err)
	}
	s.store.Put(&core.Resource{
		Cloud: "gcp", ID: s.bucketLink(name), Type: TypeBucket, Name: name,
		ParentID: s.projectSelfLink(), Location: str(doc["location"]),
		Backing: "real", Body: body,
	})
	return nil
}

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request) string {
	var body map[string]any
	if err := readJSON(r, &body); err != nil {
		writeError(w, badRequest("%v", err))
		return "storage create: bad body"
	}
	name := str(body["name"])
	if name == "" {
		writeError(w, badRequest("Required field 'name' not specified"))
		return "storage create: no name"
	}
	if s.store.Exists(s.bucketLink(name)) {
		// GCS's own wording, and a 409: the provider reads this to tell
		// "already yours" from "taken by somebody else".
		writeError(w, errorf(http.StatusConflict, "conflict", "ALREADY_EXISTS",
			"You already own this bucket. Please select another name."))
		return "storage create: exists"
	}

	body["kind"] = "storage#bucket"
	body["id"] = name
	body["selfLink"] = s.bucketLink(name)
	body["projectNumber"] = DefaultProjectNumber
	body["metageneration"] = "1"
	body["location"] = strings.ToUpper(orDefaultString(str(body["location"]), "US"))
	body["locationType"] = "multi-region"
	body["storageClass"] = orDefaultString(str(body["storageClass"]), "STANDARD")
	body["timeCreated"] = timestamp()
	body["updated"] = timestamp()
	body["etag"] = etagOf(name)
	body["versioning"] = orDefault(body["versioning"], map[string]any{"enabled": false})
	body["iamConfiguration"] = orDefault(body["iamConfiguration"], map[string]any{
		"uniformBucketLevelAccess": map[string]any{"enabled": false},
		"publicAccessPrevention":   "inherited",
	})

	if aerr := s.saveBucket(body); aerr != nil {
		writeError(w, aerr)
		return "storage create: " + aerr.Message
	}
	writeJSON(w, http.StatusOK, body)
	return ""
}

func (s *Server) getBucket(w http.ResponseWriter, name string) string {
	doc, aerr := s.loadBucket(name)
	if aerr != nil {
		writeError(w, aerr)
		return "storage get: not found"
	}
	writeJSON(w, http.StatusOK, doc)
	return ""
}

func (s *Server) listBuckets(w http.ResponseWriter) string {
	items := []json.RawMessage{}
	for _, res := range s.store.List(func(r *core.Resource) bool { return r.Type == TypeBucket }) {
		items = append(items, json.RawMessage(res.Body))
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": "storage#buckets", "items": items})
	return ""
}

// patchBucket merges the fields that were sent.
//
// Storage has no updateMask: a PATCH body simply carries the fields that
// change, and a null clears one. The identity fields are never touched.
func (s *Server) patchBucket(w http.ResponseWriter, r *http.Request, name string) string {
	if !s.store.Exists(s.bucketLink(name)) {
		writeError(w, errorf(http.StatusNotFound, "notFound", "NOT_FOUND",
			"The specified bucket does not exist."))
		return "storage patch: not found"
	}
	var patch map[string]any
	if err := readJSON(r, &patch); err != nil {
		writeError(w, badRequest("%v", err))
		return "storage patch: bad body"
	}
	var updated map[string]any
	err := s.store.Update(s.bucketLink(name), func(doc map[string]any) error {
		for field, value := range patch {
			switch field {
			case "name", "id", "selfLink", "kind", "timeCreated", "projectNumber":
				continue
			}
			if value == nil {
				delete(doc, field)
				continue
			}
			doc[field] = value
		}
		doc["updated"] = timestamp()
		// metageneration counts WRITES to the bucket's metadata, and a
		// client uses it for if-match. It is one of the few values here
		// that is meant to change on every write.
		doc["metageneration"] = bumpString(doc["metageneration"])
		updated = doc
		return nil
	})
	if err != nil {
		writeError(w, internalError(err))
		return "storage patch: " + err.Error()
	}
	writeJSON(w, http.StatusOK, updated)
	return ""
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, name string) string {
	if _, aerr := s.loadBucket(name); aerr != nil {
		writeError(w, aerr)
		return "storage delete: not found"
	}
	// A bucket with contents is REFUSED, as GCS refuses it. The provider's
	// force_destroy works by listing and deleting the objects first --
	// measured: it calls GET /b/{b}/o?versions=true before the delete --
	// so a silent cascade here would quietly destroy a bucket that was
	// meant to be kept. The same rule S3 taught in Milestone 7.
	if n := s.objects.count(name); n > 0 {
		writeError(w, errorf(http.StatusConflict, "conflict", "FAILED_PRECONDITION",
			"The bucket you tried to delete is not empty."))
		return "storage delete: not empty"
	}
	_, _ = s.store.Delete(s.bucketLink(name))
	w.WriteHeader(http.StatusNoContent)
	return ""
}

// storageLayout says the bucket is a flat one. A hierarchical namespace
// bucket is a different product with folder operations of its own, and
// Nimbus does not emulate it -- saying so plainly is what keeps a client
// from trying.
func (s *Server) storageLayout(w http.ResponseWriter, name string) string {
	if _, aerr := s.loadBucket(name); aerr != nil {
		writeError(w, aerr)
		return "storage layout: no bucket"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":                  "storage#storageLayout",
		"bucket":                name,
		"location":              "US",
		"locationType":          "multi-region",
		"hierarchicalNamespace": map[string]any{"enabled": false},
		"customPlacementConfig": map[string]any{},
	})
	return ""
}

func orDefaultString(have, fallback string) string {
	if have == "" {
		return fallback
	}
	return have
}

// bumpString increments a number that lives on the wire as a string.
func bumpString(v any) string {
	n, _ := strconv.ParseInt(str(v), 10, 64)
	return strconv.FormatInt(n+1, 10)
}

// sortedNames is a stable listing order, so two reads of a bucket agree and
// a client paging through sees each object once.
func sortedNames(names []string) []string {
	sort.Strings(names)
	return names
}
