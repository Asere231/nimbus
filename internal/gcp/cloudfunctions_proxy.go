package gcp

import (
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Where a function answers.
//
// Its own prefix rather than Cloud Run's, for two reasons. A function and a
// service may legitimately share a name, since they are separate collections
// in separate APIs -- and the two are served by separate engine instances, so
// one lookup could not find both anyway.
//
// The address is STABLE and the host port is not: a container restarting
// lands on whatever port is free, and the provider stores `url` in state. A
// URL carrying the host port would be a permanent diff; this is the same
// reason Cloud Run proxies rather than publishing its port directly.

func (s *Server) serveFunctionProxy(w http.ResponseWriter, r *http.Request) string {
	rest := strings.TrimPrefix(r.URL.Path, FunctionProxyPrefix)
	name := rest
	if i := strings.Index(rest, "/"); i >= 0 {
		name = rest[:i]
	}
	if name == "" {
		writeError(w, notFound("the URL must name a function: %s{function}", FunctionProxyPrefix))
		return "function proxy: no function named"
	}

	fn, location := s.findRunningFunction(name)
	if fn == nil {
		// The two cases are different and the difference is the answer: a
		// function nobody deployed, or one whose container is not up.
		if location == "" {
			writeError(w, notFound("No Cloud Function named %q exists", name))
			return "function proxy: no such function " + name
		}
		writeError(w, errorf(http.StatusServiceUnavailable, "backendError", "UNAVAILABLE",
			"Function %q is not running. It may still be deploying, or its "+
				"container may have exited -- `docker logs` on it will say which.", name))
		return "function proxy: not running " + name
	}
	if err := s.functions.Proxy(w, r, fn, FunctionProxyPrefix+name); err != nil {
		writeError(w, errorf(http.StatusBadGateway, "backendError", "UNAVAILABLE", "%v", err))
		return "function proxy: " + err.Error()
	}
	return ""
}

// findRunningFunction looks a function up by its short name, returning the
// engine's record and the location so the caller can tell "no such function"
// from "not running".
func (s *Server) findRunningFunction(name string) (fn *run.Service, location string) {
	for _, res := range s.store.List(func(r *core.Resource) bool {
		return r.Type == TypeFunction && r.Name == name
	}) {
		if running := s.functions.Get(res.ID); running != nil {
			return running, res.Location
		}
		location = res.Location
		if location == "" {
			location = s.cfg.Region
		}
	}
	return nil, location
}
