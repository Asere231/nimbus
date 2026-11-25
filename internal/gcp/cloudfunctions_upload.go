package gcp

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Where `gcloud functions deploy` puts the source.
//
// The two clients get the archive to the platform by DIFFERENT routes, and
// neither is a variant of the other:
//
//	terraform  uploads the zip to a bucket the configuration creates, then
//	           names it in buildConfig.source.storageSource
//	gcloud     asks the SERVICE for an upload URL, PUTs the zip at it, and
//	           passes back the storageSource the service chose
//
// Measured: without generateUploadUrl, `gcloud functions deploy` stops with a
// 404 and never sends a create at all. It is a COLON VERB on the collection
// -- functions:generateUploadUrl -- which is the third convention Google uses
// for "do a thing": a colon verb here and on Pub/Sub's publish, a trailing
// path segment for Compute's instance lifecycle, and `?functionId=` for a
// create.
//
// The URL points back at Nimbus rather than at anything signed. Real Cloud
// Functions returns a signed Cloud Storage URL and Nimbus signs nothing, so
// the honest emulation is an endpoint of its own that accepts the bytes.

// FunctionUploadPrefix is where a source archive is PUT. Prefixed so it can
// never collide with a Google API path.
const FunctionUploadPrefix = "/_nimbus/functions-upload/"

// functionSourceBucket holds the archives gcloud uploads.
//
// A bucket Nimbus owns rather than one the user made, because gcloud never
// names a bucket: it takes whatever the service offers. It is created on
// demand and is deliberately visible in the graph -- the archive really is
// an object in Cloud Storage, and hiding it would make the source of a
// gcloud-deployed function impossible to find.
const functionSourceBucket = "nimbus-gcf-sources"

// generateUploadURL answers POST .../functions:generateUploadUrl.
func (s *Server) generateUploadURL(w http.ResponseWriter, location string) *apiError {
	// A fresh object per call, as the real service does: two deploys of the
	// same function must not race over one name.
	object := fmt.Sprintf("%s/%s.zip", location, hexID(24))
	writeJSON(w, http.StatusOK, map[string]any{
		"uploadUrl": s.base() + FunctionUploadPrefix + object,
		"storageSource": map[string]any{
			"bucket": functionSourceBucket,
			"object": object,
		},
	})
	return nil
}

// serveFunctionUpload receives the archive gcloud PUTs.
//
// A PUT, because that is what a client does with a signed upload URL -- it
// is not a Cloud Storage API call and carries no uploadType, so it cannot go
// through the storage handler.
func (s *Server) serveFunctionUpload(w http.ResponseWriter, r *http.Request) string {
	object := strings.TrimPrefix(r.URL.Path, FunctionUploadPrefix)
	if object == "" {
		writeError(w, badRequest("the upload URL must name an object"))
		return "function upload: no object named"
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		writeError(w, badRequest("a source archive is uploaded with PUT"))
		return "function upload: wrong method"
	}
	// Bounded, because this is an unauthenticated endpoint that writes to
	// disk. 512 MiB is well past any real function and far short of filling
	// a laptop; real Cloud Functions caps a source archive at 100 MB
	// compressed, so nothing legitimate comes near it.
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 512<<20))
	if err != nil {
		writeError(w, badRequest("could not read the source archive: %v", err))
		return "function upload: " + err.Error()
	}
	// The bucket has to EXIST as a resource, not just hold bytes. The object
	// store keeps the archive either way and the function create finds it --
	// but a bucket that is only an entry in a map is invisible in the graph
	// and answers NoSuchBucket to `gcloud storage ls`, which is the
	// documented behaviour for an object whose bucket was never created.
	// Measured: the PUT answered 200 and the bucket listing was empty, so
	// the comment above claiming it is "visible in the graph" was false
	// until this was added.
	if !s.store.Exists(s.bucketLink(functionSourceBucket)) {
		doc := map[string]any{"name": functionSourceBucket}
		s.fillBucket(doc, functionSourceBucket)
		if aerr := s.saveBucket(doc); aerr != nil {
			writeError(w, aerr)
			return "function upload: could not create the source bucket"
		}
	}
	// Not versioned: the source bucket is Nimbus's own, and a redeploy
	// keeping every archive it ever built would grow without bound.
	if _, aerr := s.objects.put(s, functionSourceBucket, object, data, nil, false); aerr != nil {
		writeError(w, aerr)
		return "function upload: " + aerr.Code
	}
	// gcloud reads nothing out of the response but the status.
	w.WriteHeader(http.StatusOK)
	return "uploaded a function source archive (" + numberOf(len(data)) + " bytes)"
}
