package storage

import (
	"fmt"
	"net/http"
)

// serveOther is the placeholder for services whose operations are not built
// yet. It reports the exact call in the activity feed so the visualizer shows
// what to implement next -- the debugging loop CLAUDE.md describes.
func (h *Handler) serveOther(w http.ResponseWriter, r *http.Request, a *Account, svc Service) string {
	WriteError(w, http.StatusNotImplemented, "FeatureNotYetImplemented",
		fmt.Sprintf("Nimbus does not implement %s %s on the %s service yet.", r.Method, r.URL.Path, svc))
	return fmt.Sprintf("unimplemented storage call: %s %s %s%s", svc, r.Method, r.URL.Path, queryNote(r))
}
