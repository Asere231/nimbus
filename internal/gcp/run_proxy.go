package gcp

import (
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// The address a Cloud Run service answers on.
//
// Real Cloud Run gives a service a stable https URL that does not change
// when it is redeployed. A container's published host port is the opposite
// of that: it changes every time the container starts. So `uri` points at
// Nimbus, which proxies through to whichever port the container currently
// has.
//
// That is the same reasoning as the AKS kubeconfig, the stable etag and
// `Content-MD5`, generalised once more: anything a client stores and
// re-reads has to be derived, not generated. Terraform puts `uri` in
// state; a Pub/Sub push subscription puts it in a resource; a person puts
// it in a terminal. None of them should break because the service was
// redeployed.
//
// It also means every invocation passes through the activity feed, which
// is the whole point of this project -- a request to a bare container port
// would be invisible, and the chain "publish -> push -> service answered"
// is exactly what a visualizer can show and a log cannot.
//
// The ACR precedent is worth naming as the road not taken: a registry's
// loginServer IS `localhost:<port>`, because Docker has to reach it and a
// proxy in front of the registry protocol would have bought nothing. Here
// it buys stability and the feed.

// RunProxyPrefix is where services answer. Prefixed so it can never
// collide with a Google API path.
const RunProxyPrefix = "/_nimbus/run/"

// serviceURI is a service's stable address.
func (s *Server) serviceURI(name string) string {
	return s.base() + RunProxyPrefix + name
}

// serveRunProxy forwards a request to the service named in the path.
func (s *Server) serveRunProxy(w http.ResponseWriter, r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, RunProxyPrefix)
	name := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		name = rest[:i]
	}
	if name == "" {
		writeError(w, notFound("the URL must name a service: %srun/{service}", RunProxyPrefix))
		return "run proxy: no service named"
	}

	svc, location := s.findRunningService(name)
	if svc == nil {
		// 404, with the reason, because the two cases are different and
		// the difference is the whole answer: a service nobody created,
		// or one whose container is not up.
		if location == "" {
			writeError(w, notFound("No Cloud Run service named %q exists", name))
			return "run proxy: no such service " + name
		}
		writeError(w, errorf(http.StatusServiceUnavailable, "backendError", "UNAVAILABLE",
			"The service %q exists but its container is not running", name))
		return "run proxy: " + name + " is not running"
	}

	if err := s.services.Proxy(w, r, svc, RunProxyPrefix+name); err != nil {
		writeError(w, errorf(http.StatusBadGateway, "backendError", "UNAVAILABLE",
			"%v", err))
		return "run proxy: " + err.Error()
	}
	return ""
}

// findRunningService looks a service up by its short name. It returns the
// engine's record and the location, so the caller can tell "no such
// service" from "not running": a found-but-stopped service returns a nil
// record and a non-empty location.
func (s *Server) findRunningService(name string) (svc *run.Service, location string) {
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeRunService && r.Name == name
	}) {
		if running := s.services.Get(res.ID); running != nil {
			return running, res.Location
		}
		location = res.Location
		if location == "" {
			location = s.cfg.Region
		}
	}
	return nil, location
}
