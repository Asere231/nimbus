// Package run serves container images as HTTP services, for real.
//
// A service here is one long-lived container listening on a port, with
// every request proxied straight through to it. That is all Cloud Run is,
// and all Azure Container Apps or AWS App Runner would be: the contract a
// container is written to is "listen on $PORT and answer HTTP", so Nimbus
// implements nothing of the application and the response a client gets is
// the one the image really produced.
//
// It is NOT internal/engines/funcs with a different name, and the three
// differences are the reason this package exists:
//
//   - funcs invokes one fixed path, the Runtime Interface Emulator's
//     /2015-03-31/functions/function/invocations. A service answers any
//     method on any path, so the whole request has to be forwarded.
//   - funcs bind-mounts unpacked code into a runtime image. A service IS
//     its image; there is nothing to mount.
//   - funcs reads X-Amz-Function-Error to tell a thrown exception from a
//     returned value. A service has no such notion: its status code is its
//     answer.
//
// Nothing here knows what cloud it is serving. The caller supplies the
// image, the port the image listens on and the environment; naming the
// service, giving it a URL and deciding what a revision means are the
// plugin's business.
package run

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// DefaultContainerPort is what a container is assumed to listen on, and
// what PORT is set to. 8080 is Cloud Run's own default and the one almost
// every image published for it uses.
const DefaultContainerPort = 8080

const (
	DefaultPortBase     = 9300
	DefaultStartTimeout = 3 * time.Minute
)

// Config is how a cloud sets the engine up.
type Config struct {
	// NamePrefix starts every container name, so one cloud's containers
	// are recognisable and another cloud's are left alone.
	NamePrefix string
	// PortBase is the first host port tried for a service.
	PortBase int
	// StartTimeout bounds how long a container may take to answer. It
	// includes pulling the image, which on a first run is most of it.
	StartTimeout time.Duration
}

func (c Config) withDefaults() Config {
	if c.NamePrefix == "" {
		c.NamePrefix = "nimbus-svc"
	}
	if c.PortBase == 0 {
		c.PortBase = DefaultPortBase
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = DefaultStartTimeout
	}
	return c
}

// Service is one running container.
type Service struct {
	// ResourceID is the caller's identifier -- the key the registry uses.
	ResourceID string
	Name       string
	Image      string
	// Command and Args override the image's entrypoint, when a client
	// asked for that.
	Command []string
	Args    []string
	Env     map[string]string
	// ContainerPort is what the image listens on inside the container.
	ContainerPort int

	Container string
	// Port is the host port it was published on. It changes every time the
	// service is started, which is exactly why a plugin should not put it
	// in a URL a client stores; see the proxy in the GCP plugin.
	Port    int
	Started time.Time
}

// Engine starts, proxies to and stops services for one cloud.
type Engine struct {
	cfg    Config
	docker docker.Client

	mu    sync.Mutex
	svcs  map[string]*Service
	proxy map[string]*httputil.ReverseProxy
}

func New(cfg Config, d docker.Client) *Engine {
	return &Engine{cfg: cfg.withDefaults(), docker: d,
		svcs: map[string]*Service{}, proxy: map[string]*httputil.ReverseProxy{}}
}

// SetDocker swaps the Docker client, which tests use to supply the Fake.
func (e *Engine) SetDocker(d docker.Client) { e.docker = d }

// Docker exposes the client for the few things a plugin does itself.
func (e *Engine) Docker() docker.Client { return e.docker }

// Get returns a service the engine is running, or nil.
func (e *Engine) Get(resourceID string) *Service {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.svcs[resourceID]
}

// Put registers a service without starting anything, which a plugin's
// tests use to stand a container in.
func (e *Engine) Put(svc *Service) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.svcs[svc.ResourceID] = svc
}

// All returns every service in resource-id order, so a shutdown is
// deterministic.
func (e *Engine) All() []*Service {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Service, 0, len(e.svcs))
	for _, svc := range e.svcs {
		out = append(out, svc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// Start pulls the image if it is missing, runs the container and waits
// until it answers.
func (e *Engine) Start(ctx context.Context, svc *Service) error {
	if err := e.docker.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs services as containers, so Docker must be "+
			"running. Start Docker Desktop (or your runtime) and try again.", err)
	}
	if present, err := e.docker.ImagePresent(ctx, svc.Image); err == nil && !present {
		if err := e.docker.Pull(ctx, svc.Image); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				svc.Image, err, svc.Image)
		}
	}
	if svc.ContainerPort == 0 {
		svc.ContainerPort = DefaultContainerPort
	}

	port, err := docker.FreePort(e.cfg.PortBase)
	if err != nil {
		return err
	}
	svc.Port = port
	svc.Container = docker.Name(e.cfg.NamePrefix, svc.ResourceID, svc.Name)

	// A container left from a previous deployment is removed rather than
	// reused: a new revision means a new image or a new environment, and
	// the whole point of deploying is that the next request hits it.
	_ = e.docker.Remove(ctx, svc.Container)

	env := map[string]string{}
	for k, v := range svc.Env {
		env[k] = v
	}
	// PORT is the contract. A container is told where to listen rather
	// than being assumed to have guessed the same number.
	env["PORT"] = fmt.Sprintf("%d", svc.ContainerPort)

	if _, err := e.docker.Run(ctx, docker.RunOptions{
		Name:  svc.Container,
		Image: svc.Image,
		Args:  append(append([]string{}, svc.Command...), svc.Args...),
		Ports: map[string]string{
			fmt.Sprintf("127.0.0.1:%d", port): fmt.Sprintf("%d", svc.ContainerPort),
		},
		Env: env,
		Labels: map[string]string{
			docker.LabelService:  svc.Name,
			docker.LabelResource: svc.ResourceID,
			docker.LabelRole:     "service",
		},
	}); err != nil {
		return fmt.Errorf("starting the service container: %w", err)
	}
	svc.Started = time.Now()

	if err := e.waitReady(ctx, svc); err != nil {
		return err
	}
	e.mu.Lock()
	e.svcs[svc.ResourceID] = svc
	delete(e.proxy, svc.ResourceID) // the port changed, so the proxy must be rebuilt
	e.mu.Unlock()
	return nil
}

// waitReady blocks until the container answers on its port.
//
// Any HTTP status counts as ready, including a 404: what is being waited
// for is a server, not a particular route, and an image whose root path is
// not served is perfectly normal. A GET is used rather than a POST for the
// reason funcs learned the hard way -- a probe that goes through the
// application's own handler can have side effects, and did: an earlier
// Lambda readiness check ran the handler once per deploy.
func (e *Engine) waitReady(ctx context.Context, svc *Service) error {
	deadline := time.Now().Add(e.cfg.StartTimeout)
	client := &http.Client{Timeout: 10 * time.Second}
	url := fmt.Sprintf("http://127.0.0.1:%d/", svc.Port)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("the service container did not answer within %s: %v\n\n"+
		"Check `docker logs %s`", e.cfg.StartTimeout, last, svc.Container)
}

// Proxy forwards one request to a service and writes back whatever it
// answered, byte for byte. prefix is stripped from the path first, so a
// service mounted under a URL prefix sees the paths it expects.
func (e *Engine) Proxy(w http.ResponseWriter, r *http.Request, svc *Service, prefix string) error {
	p, err := e.proxyFor(svc)
	if err != nil {
		return err
	}
	out := r.Clone(r.Context())
	out.URL.Path = StripPrefix(r.URL.Path, prefix)
	out.RequestURI = ""
	p.ServeHTTP(w, out)
	return nil
}

// StripPrefix removes a URL prefix and guarantees what is left starts with
// a slash: a service asked for "/_nimbus/run/hello" must see "/", not "".
// Exported because a plugin's tests assert on the paths a service sees.
func StripPrefix(path, prefix string) string {
	if prefix != "" && len(path) >= len(prefix) && path[:len(prefix)] == prefix {
		path = path[len(prefix):]
	}
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	return path
}

func (e *Engine) proxyFor(svc *Service) (*httputil.ReverseProxy, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p := e.proxy[svc.ResourceID]; p != nil {
		return p, nil
	}
	if svc.Port == 0 {
		return nil, fmt.Errorf("service %s is not running", svc.Name)
	}
	target, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", svc.Port))
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(target)
	// A dead container must not produce a blank 502 with nothing to read:
	// say which service and what went wrong, the way every other error
	// here does.
	p.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, fmt.Sprintf("nimbus: service %q did not answer: %v\n\n"+
			"Check `docker logs %s`", svc.Name, err, svc.Container),
			http.StatusBadGateway)
	}
	e.proxy[svc.ResourceID] = p
	return p, nil
}

// Logs returns what a service has printed since a moment.
func (e *Engine) Logs(ctx context.Context, svc *Service, since time.Time) ([]byte, error) {
	return e.docker.Logs(ctx, svc.Container, since)
}

// Stop removes a service's container.
//
// ALWAYS -- KeepRunning is deliberately not consulted here, for the reason
// funcs.Engine.Stop spells out: it means "leave containers up when NIMBUS
// stops", not "ignore a client that deleted the service". Both callers that
// really should respect the flag, azure.Server.Shutdown and
// gcp.Server.Shutdown, already return early on it before reaching this, so
// the check was redundant there and wrong on the delete paths -- with
// --persist, destroying a Container App or a Cloud Run service left its
// container running for ever.
func (e *Engine) Stop(ctx context.Context, svc *Service) {
	e.mu.Lock()
	delete(e.svcs, svc.ResourceID)
	delete(e.proxy, svc.ResourceID)
	e.mu.Unlock()
	_ = e.docker.Remove(ctx, svc.Container)
}

// Forget drops a service from the registry without touching its container.
func (e *Engine) Forget(resourceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.svcs, resourceID)
	delete(e.proxy, resourceID)
}
