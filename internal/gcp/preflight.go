package gcp

import (
	"net/http"
)

// The two APIs `gcloud functions deploy` checks BEFORE it deploys.
//
// Neither is Cloud Functions. gcloud asks Service Usage whether run,
// cloudbuild and artifactregistry are enabled, and asks Cloud Build for the
// project's default build service account -- and with no endpoint override
// for either, both go to the REAL Google and come back 403 and 401. Measured
// with `--log-http`: four requests to googleapis.com in the middle of what
// looks like a local deploy, and the error is Google's "missing required
// authentication credential", which names no Nimbus path and sends you to a
// devconsole page.
//
// That is the reason these exist at all. A user pointing gcloud at Nimbus has
// every right to expect nothing to leave the machine, and the fix is two
// answers plus two endpoint overrides in `nimbus gcp register`.
//
// They are deliberately the THINNEST possible answers. Service enablement and
// Cloud Build are whole products; emulating them because one client's
// pre-flight asks after them would be scope with no visible end. What is
// served is exactly what the pre-flight reads and nothing else, and anything
// further under either path says so rather than inventing a reply.

// serveServiceUsage answers "is this API enabled". Always yes: a Nimbus
// project has whatever Nimbus implements, and an API Nimbus does not
// implement fails at the call rather than at the enablement check -- which is
// the more useful place for it to fail, because the message can name the path.
func (s *Server) serveServiceUsage(w http.ResponseWriter, r *http.Request, parts []string) string {
	// /v1/projects/{project}/services/{service}
	if len(parts) < 5 || r.Method != http.MethodGet {
		writeError(w, notFound("Nimbus does not emulate %s %s yet", r.Method, r.URL.Path))
		return "serviceusage: unrecognised path"
	}
	service := parts[4]
	writeJSON(w, http.StatusOK, map[string]any{
		"name":   "projects/" + s.cfg.ProjectID + "/services/" + service,
		"config": map[string]any{"name": service},
		"state":  "ENABLED",
		"parent": "projects/" + s.cfg.ProjectID,
	})
	return ""
}

// serveCloudBuild answers only the default service account lookup.
//
// Nimbus builds images with the local Docker daemon, so there is no Cloud
// Build to emulate and no build to poll. The account is derived from the
// project number, which is what the real one looks like.
func (s *Server) serveCloudBuild(w http.ResponseWriter, r *http.Request, parts []string) string {
	// /v1/projects/{project}/locations/{location}/defaultServiceAccount
	if len(parts) < 6 || r.Method != http.MethodGet {
		writeError(w, notFound(
			"Nimbus does not emulate Cloud Build (%s %s). Images are built with the local "+
				"Docker daemon, so there is no build to submit or poll.", r.Method, r.URL.Path))
		return "cloudbuild: not emulated"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"serviceAccountEmail": DefaultProjectNumber + "-compute@developer.gserviceaccount.com",
	})
	return ""
}
