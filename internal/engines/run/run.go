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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// Sidecars are the other containers in the same template.
	//
	// Azure Container Apps and Cloud Run both define a service as a GROUP
	// of containers sharing one network namespace, so they reach each
	// other on localhost. Only the first one's port is published; the
	// rest are reachable from inside the group and from nowhere else,
	// which is exactly what the two clouds do.
	Sidecars []Sidecar
	// BuildDir, when set, is a directory holding a Dockerfile that is
	// BUILT into Image before the container starts, instead of Image
	// being pulled.
	//
	// Only the Azure Functions host needs this, and the reason is
	// measured: with the code bind-mounted its own base image reports
	// zero functions, and the identical tree baked in with a COPY loads
	// and serves them. What goes IN the Dockerfile is the plugin's
	// business -- the base image and /home/site/wwwroot are Azure's
	// vocabulary and have no place in an engine that serves three clouds.
	BuildDir string
	// BuildBase is the image the Dockerfile's FROM names, pulled before
	// the build so that `docker build` never has to reach a registry --
	// which is the one thing this project will not do. A missing base
	// then produces the ordinary "pre-pull it while online" message
	// rather than a build that silently needs the network.
	BuildBase string
	// Replicas is how many copies of the whole group to run. 0 and 1 both
	// mean one, so a caller that does not know about replicas is
	// unchanged.
	//
	// This is a MINIMUM made real, not autoscaling: nothing here measures
	// load, so there is nothing to scale out against. A cloud's
	// maxReplicas stays inert and says so.
	Replicas int
	// Extra holds replicas 2..N. The first replica is this Service's own
	// Container and Port, so everything that only ever knew about one
	// container keeps working and a reattach still finds the same name.
	Extra []Replica

	Container string
	// Port is the host port it was published on. It changes every time the
	// service is started, which is exactly why a plugin should not put it
	// in a URL a client stores; see the proxy in the GCP plugin.
	Port    int
	Started time.Time
}

// MaxReplicas is the most copies of one service Nimbus will run.
//
// Real Azure allows minReplicas up to 1000. Starting a thousand containers
// on a laptop is not a scaling test, it is a way to lose the machine, so a
// larger count is REFUSED and the refusal says what the cap is -- rather
// than being silently clamped, which would leave a configuration asking
// for 50 and getting 8 with nothing to read.
const MaxReplicas = 8

// Replica is one copy of a service's container group beyond the first.
type Replica struct {
	Container string
	Port      int
	Sidecars  []Sidecar
}

// Sidecar is another container in the same service.
//
// It has no port of its own, by design and not by omission: it shares the
// main container's network namespace, so it listens on whatever it likes
// and the main container reaches it on localhost. Docker refuses to
// publish a port for a container that does not own its namespace, which is
// the same rule said in the other direction.
type Sidecar struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string
	// Container is the name Nimbus gave it, filled in by Start.
	Container string
}

// Engine starts, proxies to and stops services for one cloud.
type Engine struct {
	cfg    Config
	docker docker.Client

	mu    sync.Mutex
	svcs  map[string]*Service
	proxy map[string]*httputil.ReverseProxy
	// built maps a resource id to the image tag Nimbus built for it, so a
	// delete removes that image and never a pulled one.
	built map[string]string
}

func New(cfg Config, d docker.Client) *Engine {
	return &Engine{cfg: cfg.withDefaults(), docker: d,
		svcs: map[string]*Service{}, proxy: map[string]*httputil.ReverseProxy{},
		built: map[string]string{}}
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
	// Every image in the group, not just the first: an app whose cache
	// image is not local must fail on the pull rather than on a container
	// that cannot start.
	for _, image := range svc.images() {
		if present, err := e.docker.ImagePresent(ctx, image); err == nil && !present {
			if err := e.docker.Pull(ctx, image); err != nil {
				return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
					image, err, image)
			}
		}
	}

	// An image to BUILD rather than pull. A function app's image exists
	// only on this machine, so pulling it could only ever fail against a
	// registry that has never heard of it -- which is why svc.images()
	// leaves it out and this runs instead.
	if svc.BuildDir != "" {
		if err := e.buildImage(ctx, svc); err != nil {
			return err
		}
	}
	if svc.ContainerPort == 0 {
		svc.ContainerPort = DefaultContainerPort
	}

	// Refused rather than clamped: a configuration asking for 50 replicas
	// and silently getting 8 is a divergence with nothing to read.
	replicas := svc.Replicas
	if replicas < 1 {
		replicas = 1
	}
	if replicas > MaxReplicas {
		return fmt.Errorf(
			"%s asks for %d replicas, and Nimbus runs at most MaxReplicas (%d): each one is a "+
				"real container, and a few hundred would take the machine down rather than "+
				"demonstrate scaling. Lower minReplicas to %d or fewer",
			svc.Name, svc.Replicas, MaxReplicas, MaxReplicas)
	}

	port, err := docker.FreePort(e.cfg.PortBase)
	if err != nil {
		return err
	}
	svc.Port = port
	svc.Container = replicaName(e.cfg.NamePrefix, svc, 0)

	// A container left from a previous deployment is removed rather than
	// reused: a new revision means a new image or a new environment, and
	// the whole point of deploying is that the next request hits it. The
	// sidecars go too, or the previous revision's would sit beside the new
	// ones inside the same namespace.
	e.removeGroup(ctx, svc)

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

	// The sidecars join the main container's namespace, which is what
	// makes them reachable on localhost from it -- the thing a sidecar is
	// FOR. They are started before the readiness probe, because the main
	// container may well be waiting for one of them.
	svc.Sidecars, err = e.startSidecars(ctx, svc, svc.Container, svc.Sidecars, 0)
	if err != nil {
		return err
	}

	// Replicas 2..N: the same group again, each in its own namespace with
	// its own published port and its own sidecars.
	svc.Extra = nil
	// Each replica's port search starts ABOVE the last one allocated.
	// FreePort asks the OS whether a port is free, and the container that
	// just took one has not necessarily bound it yet -- against the Docker
	// Fake it never will -- so searching from the same base would hand
	// every replica the same port and the second would fail to start.
	nextBase := svc.Port + 1
	for i := 1; i < replicas; i++ {
		rep, err := e.startReplica(ctx, svc, i, env, nextBase)
		if err != nil {
			return err
		}
		svc.Extra = append(svc.Extra, rep)
		nextBase = rep.Port + 1
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

	// Every replica's port, in a stable order. A three-replica service
	// that sent every request to the first would be a worse lie than
	// running one container: the configuration would look honoured while
	// the behaviour was not.
	ports := []int{svc.Port}
	for _, rep := range svc.Extra {
		if rep.Port != 0 {
			ports = append(ports, rep.Port)
		}
	}

	// Round robin, which is what a load balancer in front of identical
	// replicas does and all a local emulator can honestly offer -- there
	// is no health state to weight by and no sessions to be sticky about.
	var next atomic.Uint64
	p := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			port := ports[int(next.Add(1)-1)%len(ports)]
			r.URL.Scheme = "http"
			r.URL.Host = fmt.Sprintf("127.0.0.1:%d", port)
		},
	}
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

// Logs returns what a service has printed since a moment -- from EVERY
// replica, not only the first.
//
// A workspace holding one replica's console out of three is the kind of
// half-working feature this project would rather not have: nothing says
// the other two are missing, and their output is usually where the
// interesting thing is. Each replica's lines are prefixed with its
// container name when there is more than one, because otherwise two
// interleaved consoles are unreadable.
//
// A replica whose container has gone is SKIPPED rather than failing the
// whole read: one container removed behind Nimbus's back must not hide
// the other two's output.
func (e *Engine) Logs(ctx context.Context, svc *Service, since time.Time) ([]byte, error) {
	main, err := e.docker.Logs(ctx, svc.Container, since)
	if len(svc.Extra) == 0 {
		return main, err
	}

	var out []byte
	out = append(out, prefixLines(svc.Container, main)...)
	for _, rep := range svc.Extra {
		raw, err := e.docker.Logs(ctx, rep.Container, since)
		if err != nil {
			continue
		}
		out = append(out, prefixLines(rep.Container, raw)...)
	}
	// The error from the first replica is only reported when NOTHING was
	// read, so one dead container does not lose two live ones' lines.
	if len(out) == 0 {
		return nil, err
	}
	return out, nil
}

// prefixLines tags each line with the container it came from.
func prefixLines(container string, raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	var out []byte
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(container+" | "+line+"\n")...)
	}
	return out
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
	// The whole group. A sidecar outliving its app is the --persist leak
	// one layer down: the store forgets the app while its containers keep
	// running, with nothing left that knows about them.
	e.removeGroup(ctx, svc)
	// And the image, if Nimbus built it for this service alone.
	e.removeBuiltImage(ctx, svc)
}

// Forget drops a service from the registry without touching its container.
func (e *Engine) Forget(resourceID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.svcs, resourceID)
	delete(e.proxy, resourceID)
}

// --- sidecars --------------------------------------------------------------

// images is every image the service needs PULLED, main container first.
//
// A built image is deliberately absent: it exists only on this machine, so
// pulling it could only fail against a registry that has never heard of
// it. Its BASE takes its place, because the build itself must not be the
// thing that reaches the network.
func (svc *Service) images() []string {
	var out []string
	if svc.BuildDir == "" {
		out = append(out, svc.Image)
	} else if svc.BuildBase != "" {
		out = append(out, svc.BuildBase)
	}
	for _, s := range svc.Sidecars {
		if s.Image != "" {
			out = append(out, s.Image)
		}
	}
	return out
}

// sidecarName is what a sidecar's container is called.
//
// Derived from the service's resource id, the replica index and the
// sidecar's own name, so two apps each with a "cache" sidecar do not
// collide, two REPLICAS of one app do not either, and a restart can find
// the same container again -- the same reason docker.Name exists for the
// main one.
func sidecarName(prefix string, svc *Service, replica int, sidecar string) string {
	key := svc.ResourceID + "/" + sidecar
	if replica > 0 {
		key = fmt.Sprintf("%s/%d/%s", svc.ResourceID, replica, sidecar)
	}
	return docker.Name(prefix, key, sidecar)
}

// replicaName is what replica 2..N's main container is called.
//
// Replica 0 keeps the plain name docker.Name gives it, so a service that
// never heard of replicas is byte-for-byte unchanged and a reattach written
// before this existed still finds it.
func replicaName(prefix string, svc *Service, replica int) string {
	if replica == 0 {
		return docker.Name(prefix, svc.ResourceID, svc.Name)
	}
	return docker.Name(prefix, fmt.Sprintf("%s/%d", svc.ResourceID, replica), svc.Name)
}

// startSidecars runs the rest of a group's containers inside that group's
// network namespace.
//
// No ports are published for them: Docker refuses that for a container
// that does not own its namespace, and it would be wrong anyway -- a
// sidecar in Azure is reachable from its app and from nowhere else. PORT
// is still set, because an image written for Container Apps reads it
// whichever container it is.
//
// The namespace is the REPLICA's, not always the first one's: a sidecar
// exists to be reachable on localhost from the container beside it, and
// localhost is per namespace.
func (e *Engine) startSidecars(ctx context.Context, svc *Service, namespaceOwner string,
	sidecars []Sidecar, replica int) ([]Sidecar, error) {

	out := make([]Sidecar, len(sidecars))
	copy(out, sidecars)
	for i := range out {
		s := &out[i]
		s.Container = sidecarName(e.cfg.NamePrefix, svc, replica, s.Name)
		_ = e.docker.Remove(ctx, s.Container)

		env := map[string]string{}
		for k, v := range s.Env {
			env[k] = v
		}
		if _, ok := env["PORT"]; !ok {
			env["PORT"] = fmt.Sprintf("%d", svc.ContainerPort)
		}

		if _, err := e.docker.Run(ctx, docker.RunOptions{
			Name:    s.Container,
			Image:   s.Image,
			Args:    append(append([]string{}, s.Command...), s.Args...),
			Network: docker.NamespaceOf(namespaceOwner),
			Env:     env,
			Labels: map[string]string{
				docker.LabelService:  svc.Name,
				docker.LabelResource: svc.ResourceID,
				docker.LabelRole:     "sidecar",
			},
		}); err != nil {
			// Named, because "the app did not start" when the app's own
			// container is fine is the most confusing version of this.
			return nil, fmt.Errorf("starting the %q sidecar of %s: %w", s.Name, svc.Name, err)
		}
	}
	return out, nil
}

// startReplica runs one more copy of the whole group.
//
// Its own published port, because two containers cannot share one; its own
// namespace, so its sidecars are its own. The image is already pulled by
// the time this runs, so a replica costs a container start and nothing
// else.
func (e *Engine) startReplica(ctx context.Context, svc *Service, index int,
	baseEnv map[string]string, portBase int) (Replica, error) {

	port, err := docker.FreePort(portBase)
	if err != nil {
		return Replica{}, err
	}
	rep := Replica{Container: replicaName(e.cfg.NamePrefix, svc, index), Port: port}
	_ = e.docker.Remove(ctx, rep.Container)

	env := map[string]string{}
	for k, v := range baseEnv {
		env[k] = v
	}
	// A replica needs to be able to tell which one it is -- the closest
	// thing Container Apps has is CONTAINER_APP_REPLICA_NAME, which it
	// sets for exactly this reason.
	env["CONTAINER_APP_REPLICA_NAME"] = rep.Container

	if _, err := e.docker.Run(ctx, docker.RunOptions{
		Name:  rep.Container,
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
		return Replica{}, fmt.Errorf("starting replica %d of %s: %w", index+1, svc.Name, err)
	}

	rep.Sidecars, err = e.startSidecars(ctx, svc, rep.Container, svc.Sidecars, index)
	if err != nil {
		return Replica{}, err
	}
	return rep, nil
}

// removeGroup removes every container of the service: each replica's main
// container and each replica's sidecars.
//
// It works in two ways on purpose. The names it can compute cover the
// ordinary case, and a LABEL sweep covers the two cases where the names
// are unknown: a restart, after which the registry knows the app and
// nothing about its sidecars or extra replicas (--persist keeps them
// running), and a redeploy with fewer replicas or a sidecar the new
// template no longer mentions. Removing only what the Service names
// misses both -- the first IS Milestone 18's --persist leak, and the
// second is the same leak by another road. Both have a test that fails
// naming the leaked container when the sweep is taken out.
func (e *Engine) removeGroup(ctx context.Context, svc *Service) {
	// Anything labelled with this service's resource id, whatever role:
	// replicas are "service" and sidecars are "sidecar", and both belong
	// to the app being removed.
	if found, err := e.docker.List(ctx, docker.LabelResource+"="+svc.ResourceID); err == nil {
		for _, c := range found {
			_ = e.docker.Remove(ctx, strings.TrimPrefix(c.Name, "/"))
		}
	}
	// The names that are known, for a Fake or a daemon whose listing does
	// not include a container that is already stopped.
	for i := range svc.Sidecars {
		if name := svc.Sidecars[i].Container; name != "" {
			_ = e.docker.Remove(ctx, name)
		}
	}
	for _, rep := range svc.Extra {
		for i := range rep.Sidecars {
			if name := rep.Sidecars[i].Container; name != "" {
				_ = e.docker.Remove(ctx, name)
			}
		}
		if rep.Container != "" {
			_ = e.docker.Remove(ctx, rep.Container)
		}
	}
	// And any replica index this Service does not list, which is what a
	// scale-down leaves behind when the new Service knows only of one.
	for i := 1; i < MaxReplicas; i++ {
		_ = e.docker.Remove(ctx, replicaName(e.cfg.NamePrefix, svc, i))
		for _, s := range svc.Sidecars {
			_ = e.docker.Remove(ctx, sidecarName(e.cfg.NamePrefix, svc, i, s.Name))
		}
	}
	if svc.Container != "" {
		_ = e.docker.Remove(ctx, svc.Container)
	}
}

// buildImage builds a service's own image from its deploy directory.
//
// A failure fails the START, naming the image. The alternative -- a
// container that never appears while the resource reports Succeeded -- is
// the shape this project keeps removing, and a generated Dockerfile with
// a mistake in it lands here.
func (e *Engine) buildImage(ctx context.Context, svc *Service) error {
	if err := e.docker.Build(ctx, svc.BuildDir, svc.Image); err != nil {
		return fmt.Errorf("building the image %s for %s: %w\n\n"+
			"The deploy directory is %s; `docker build %s` reproduces it by hand.",
			svc.Image, svc.Name, err, svc.BuildDir, svc.BuildDir)
	}
	e.mu.Lock()
	e.built[svc.ResourceID] = svc.Image
	e.mu.Unlock()
	return nil
}

// removeBuiltImage removes an image Nimbus built for this service, and
// only one it built.
//
// A pulled image is shared with everything else on the machine, so
// removing nginx:alpine because one Container App used it would break
// every other app and cost a re-pull. One built for a single resource is
// that resource's own, and would otherwise accumulate per app -- invisibly,
// at roughly a gigabyte each.
//
// The tag is recomputed from the Service when the registry has forgotten
// it, which is the state a restart leaves: adopting a container tells you
// nothing about whether its image was built.
func (e *Engine) removeBuiltImage(ctx context.Context, svc *Service) {
	e.mu.Lock()
	tag, known := e.built[svc.ResourceID]
	delete(e.built, svc.ResourceID)
	e.mu.Unlock()

	if !known {
		// Only when this Service says it has one; never guess at a tag
		// for a service that pulled its image.
		if svc.BuildDir == "" {
			return
		}
		tag = svc.Image
	}
	if tag != "" {
		_ = e.docker.RemoveImage(ctx, tag)
	}
}
