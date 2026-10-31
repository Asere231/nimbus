package ui

import (
	"net/http"
	"strconv"
	"time"
)

// BlobBrowser is the slice of the storage data plane the visualizer needs.
// Declared here, as an interface, so internal/ui does not import the storage
// package and the two stay independent -- AWS S3 can implement the same thing
// in a later milestone.
type BlobBrowser interface {
	// ListBlobs returns the blobs in one container of one account.
	ListBlobs(account, container string) []BlobInfo
	// ReadBlob returns a blob's content and content type.
	ReadBlob(account, container, name string) (content []byte, contentType string, ok bool)
}

// BlobInfo is what the inspector shows for a blob. Deliberately not the full
// blob: content stays on the server until someone asks for it.
type BlobInfo struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	Modified    time.Time `json:"modified"`
}

// registerBlobRoutes adds the endpoints the inspector uses to list and
// download blobs. Blobs are not graph nodes -- a container can hold thousands
// -- so they are fetched on demand for the selected container instead.
func registerBlobRoutes(mux *http.ServeMux, blobs BlobBrowser) {
	if blobs == nil {
		return
	}

	mux.HandleFunc("/api/storage/blobs", func(w http.ResponseWriter, r *http.Request) {
		account := r.URL.Query().Get("account")
		container := r.URL.Query().Get("container")
		if account == "" || container == "" {
			http.Error(w, "account and container are required", 400)
			return
		}
		list := blobs.ListBlobs(account, container)
		if list == nil {
			list = []BlobInfo{}
		}
		writeJSON(w, list)
	})

	mux.HandleFunc("/api/storage/blob", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		content, ctype, ok := blobs.ReadBlob(q.Get("account"), q.Get("container"), q.Get("name"))
		if !ok {
			http.Error(w, "not found", 404)
			return
		}
		if ctype == "" {
			ctype = "application/octet-stream"
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		// attachment, so clicking a blob in the inspector saves it rather than
		// navigating away from the visualizer.
		w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(q.Get("name")))
		_, _ = w.Write(content)
	})
}
