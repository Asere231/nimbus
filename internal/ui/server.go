// Package ui serves the visualizer: a small JSON API over the store plus a
// Server-Sent Events stream, and the static web app embedded in the binary
// (so it works fully offline, with no CDN).
package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/core"
)

//go:embed static
var static embed.FS

// Info describes the running emulators, shown in the UI header.
type Info struct {
	Version string            `json:"version"`
	Clouds  map[string]string `json:"clouds"` // cloud -> endpoint
	DataDir string            `json:"dataDir"`
}

// Handler returns the visualizer's HTTP handler. blobs and secrets may be
// nil, in which case those endpoints are simply not registered.
func Handler(store *core.Store, bus *core.Bus, info Info, blobs BlobBrowser, secrets SecretBrowser) http.Handler {
	mux := http.NewServeMux()
	registerBlobRoutes(mux, blobs)
	registerSecretRoutes(mux, secrets)

	// The plan overlay, pushed in by `nimbus plan-view`. One at a time, in
	// memory: it describes what is about to happen, so it is worthless the
	// moment the apply runs and there is nothing to persist.
	var planMu sync.Mutex
	var plan *Overlay

	mux.HandleFunc("/api/plan", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			planMu.Lock()
			current := plan
			planMu.Unlock()
			if current == nil {
				writeJSON(w, map[string]any{"changes": []PlanChange{}})
				return
			}
			writeJSON(w, current)
		case http.MethodPost:
			var incoming Overlay
			if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
				http.Error(w, "bad plan: "+err.Error(), 400)
				return
			}
			planMu.Lock()
			plan = &incoming
			planMu.Unlock()
			writeJSON(w, map[string]any{"status": "loaded", "changes": len(incoming.Changes)})
		case http.MethodDelete:
			planMu.Lock()
			plan = nil
			planMu.Unlock()
			writeJSON(w, map[string]any{"status": "cleared"})
		default:
			http.Error(w, "GET, POST or DELETE", 405)
		}
	})

	sub, _ := fs.Sub(static, "static")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, info)
	})

	mux.HandleFunc("/api/resources", func(w http.ResponseWriter, r *http.Request) {
		cloud := r.URL.Query().Get("cloud")
		list := store.List(func(res *core.Resource) bool { return cloud == "" || res.Cloud == cloud })
		writeJSON(w, list)
	})

	mux.HandleFunc("/api/resource", func(w http.ResponseWriter, r *http.Request) {
		res, err := store.Get(r.URL.Query().Get("id"))
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		writeJSON(w, res)
	})

	// Runs: the activity so far, grouped into bursts. The summaries are what
	// fills the timeline's run picker; a single run carries its events, which
	// is everything the browser needs to replay it.
	mux.HandleFunc("/api/runs", func(w http.ResponseWriter, r *http.Request) {
		runs := core.Runs(bus.AllEvents())
		out := make([]core.Run, 0, len(runs))
		for _, run := range runs {
			run.Events = nil
			out = append(out, run)
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("/api/run", func(w http.ResponseWriter, r *http.Request) {
		run, ok := core.FindRun(bus.AllEvents(), r.URL.Query().Get("id"))
		if !ok {
			http.Error(w, "no such run", 404)
			return
		}
		writeJSON(w, run)
	})

	mux.HandleFunc("/api/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		store.Reset()
		writeJSON(w, map[string]string{"status": "reset"})
	})

	// Live stream: replays recent history, then pushes every new event.
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch, history, cancel := bus.Subscribe()
		defer cancel()
		for _, e := range history {
			send(w, e)
		}
		flusher.Flush()

		ping := time.NewTicker(20 * time.Second)
		defer ping.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				send(w, e)
				flusher.Flush()
			case <-ping.C:
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		}
	})

	return mux
}

func send(w http.ResponseWriter, e core.Event) {
	data, _ := json.Marshal(e)
	fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Seq, data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
