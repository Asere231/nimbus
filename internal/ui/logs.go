package ui

import (
	"net/http"
	"strconv"
)

// LogBrowser is the slice of a log store the visualizer needs.
//
// An interface for the same reason BlobBrowser and SecretBrowser are: so
// internal/ui knows nothing about Log Analytics, and CloudWatch Logs can
// implement the same thing later -- a Lambda's output is exactly as worth
// showing beside the function that printed it.
type LogBrowser interface {
	// ListLogs returns a workspace's most recent lines, newest last, up to
	// limit. An app name narrows it to one app's output.
	ListLogs(workspace, app string, limit int) []LogRow
}

// LogRow is one line, in the columns the inspector shows.
type LogRow struct {
	Time    string `json:"time"`
	App     string `json:"app,omitempty"`
	Stream  string `json:"stream,omitempty"`
	Message string `json:"message"`
}

// defaultLogLimit is how many lines the panel asks for. Enough to see what
// an app has been doing, few enough that selecting a busy workspace does
// not send a megabyte to the browser.
const defaultLogLimit = 200

// registerLogRoutes adds the endpoint the inspector uses to show a
// workspace's lines.
func registerLogRoutes(mux *http.ServeMux, logs LogBrowser) {
	if logs == nil {
		return
	}
	mux.HandleFunc("/api/loganalytics/logs", func(w http.ResponseWriter, r *http.Request) {
		workspace := r.URL.Query().Get("workspace")
		if workspace == "" {
			http.Error(w, "workspace is required", http.StatusBadRequest)
			return
		}
		limit := defaultLogLimit
		if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
			limit = n
		}
		rows := logs.ListLogs(workspace, r.URL.Query().Get("app"), limit)
		if rows == nil {
			// An empty list rather than null: the panel iterates it
			// without checking, as every client does.
			rows = []LogRow{}
		}
		writeJSON(w, rows)
	})
}
