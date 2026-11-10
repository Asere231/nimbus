// Package vm runs compute instances as containers, for real.
//
// An instance here is one long-lived container that boots, runs whatever
// user data it was given, and then stays up. That is the part of a virtual
// machine a local emulator can honestly provide: a process tree you can get
// a shell on, with your own script having really run in it.
//
// What it is NOT is a virtual machine. There is no kernel of its own, no
// block device, no BIOS and no SSH daemon -- Nimbus is stdlib-only, so it
// cannot speak SSH, and no vendor-official base image ships sshd. A shell
// comes from `nimbus ssh`, which is `docker exec` underneath. Anything that
// depends on a real kernel (modules, mounts, systemd) will not work, and
// saying so is better than half-emulating it.
//
// Nothing here knows what cloud it is serving. The caller supplies the
// image, the user data, the keys and the metadata handler; what an instance
// id looks like, which states it moves through and what its metadata
// document contains are the plugin's business.
package vm

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

const (
	DefaultStartTimeout = 3 * time.Minute

	// envUserData and envSSHKeys carry base64 into the container, where the
	// bootstrap decodes them. See boot.go for why they are not arguments.
	envUserData = "NIMBUS_USER_DATA"
	envSSHKeys  = "NIMBUS_SSH_KEYS"

	// hostFromContainer is how a container reaches the machine Nimbus runs
	// on. Inside a container localhost is the container; this name is
	// mapped on every container Nimbus starts (see docker.runArgs).
	hostFromContainer = "host.docker.internal"
)

// Config is how a cloud sets the engine up.
type Config struct {
	// NamePrefix starts every container name, so one cloud's instances are
	// recognisable and another cloud's are left alone.
	NamePrefix string
	// StartTimeout bounds how long starting an instance may take. It
	// includes pulling the image, which on a first run is most of it.
	StartTimeout time.Duration
	// KeepRunning leaves containers up when Nimbus stops.
	KeepRunning bool
}

func (c Config) withDefaults() Config {
	if c.NamePrefix == "" {
		c.NamePrefix = "nimbus-vm"
	}
	if c.StartTimeout == 0 {
		c.StartTimeout = DefaultStartTimeout
	}
	return c
}

// Instance is one machine. The caller owns the fields above Container; the
// engine fills in the ones below it.
type Instance struct {
	// ResourceID is the caller's identifier -- the key the registry uses.
	ResourceID string
	Name       string
	Image      string
	// UserData is the script to run at boot, verbatim as the client sent
	// it. It is never parsed here.
	UserData string
	// PublicKeys are written to /root/.ssh/authorized_keys. Nothing reads
	// them, because there is no sshd; they are there so an instance looks
	// the way its configuration says it does, and so a later milestone
	// that does run sshd needs no change here.
	PublicKeys []string
	// Env is extra environment for the container.
	Env map[string]string
	// Network is a Docker network to join, so instances that share one
	// virtual network can reach each other by container name.
	Network string

	// Metadata is served to this instance alone, on its own port. Nil
	// means no metadata service.
	Metadata http.Handler
	// MetadataEnv names the environment variables that tell this instance
	// where its metadata service is. AWS and GCP disagree about both the
	// name and whether the value carries a scheme, so the plugin decides:
	// endpoint is "http://host:port", hostPort is "host:port".
	MetadataEnv func(endpoint, hostPort string) map[string]string

	Container string
	// MetadataPort is the host port this instance's metadata is served on.
	MetadataPort int
	Started      time.Time

	// metadata is the listener's server, kept so Delete can close it.
	metadata *http.Server
	listener net.Listener
}

// Engine starts, stops and adopts instances for one cloud.
type Engine struct {
	cfg    Config
	docker docker.Client

	mu   sync.Mutex
	inst map[string]*Instance
}

func New(cfg Config, d docker.Client) *Engine {
	return &Engine{cfg: cfg.withDefaults(), docker: d, inst: map[string]*Instance{}}
}

// SetDocker swaps the Docker client, which tests use to supply the Fake.
func (e *Engine) SetDocker(d docker.Client) { e.docker = d }

// Docker exposes the client for the few things a plugin does itself.
func (e *Engine) Docker() docker.Client { return e.docker }

// Get returns an instance the engine knows about, or nil.
func (e *Engine) Get(resourceID string) *Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.inst[resourceID]
}

// Put registers an instance without starting anything, which a plugin's
// tests use to stand a container in.
func (e *Engine) Put(i *Instance) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.inst[i.ResourceID] = i
}

// All returns every instance in resource-id order, so a shutdown is
// deterministic.
func (e *Engine) All() []*Instance {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*Instance, 0, len(e.inst))
	for _, i := range e.inst {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ResourceID < out[b].ResourceID })
	return out
}

// Start pulls the image if it is missing, serves the instance's metadata and
// runs the container.
func (e *Engine) Start(ctx context.Context, i *Instance) error {
	if err := e.docker.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs instances as containers, so Docker must be "+
			"running. Start Docker Desktop (or your runtime) and try again.", err)
	}
	if present, err := e.docker.ImagePresent(ctx, i.Image); err == nil && !present {
		if err := e.docker.Pull(ctx, i.Image); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				i.Image, err, i.Image)
		}
	}

	i.Container = docker.Name(e.cfg.NamePrefix, i.ResourceID, i.Name)
	// A container left from a previous run of this same instance is
	// removed: Start means "boot this machine", and the caller uses Resume
	// when it means "switch it back on".
	_ = e.docker.Remove(ctx, i.Container)

	if i.Network != "" {
		if err := e.docker.EnsureNetwork(ctx, i.Network); err != nil {
			return err
		}
	}

	env := map[string]string{}
	for k, v := range i.Env {
		env[k] = v
	}
	// Both of these are base64 so the bootstrap can decode them inside the
	// container; see boot.go.
	if i.UserData != "" {
		env[envUserData] = base64.StdEncoding.EncodeToString([]byte(i.UserData))
	}
	if keys := authorizedKeys(i.PublicKeys); keys != "" {
		env[envSSHKeys] = base64.StdEncoding.EncodeToString([]byte(keys))
	}

	if err := e.serveMetadata(i); err != nil {
		return err
	}
	if i.MetadataPort > 0 && i.MetadataEnv != nil {
		hostPort := fmt.Sprintf("%s:%d", hostFromContainer, i.MetadataPort)
		for k, v := range i.MetadataEnv("http://"+hostPort, hostPort) {
			env[k] = v
		}
	}

	if _, err := e.docker.Run(ctx, docker.RunOptions{
		Name:    i.Container,
		Image:   i.Image,
		Args:    bootCommand(),
		Env:     env,
		Network: i.Network,
		Labels: map[string]string{
			docker.LabelInstance: i.Name,
			docker.LabelResource: i.ResourceID,
			docker.LabelRole:     "instance",
		},
	}); err != nil {
		e.closeMetadata(i)
		return fmt.Errorf("starting instance %s: %w", i.Name, err)
	}

	i.Started = time.Now()
	e.Put(i)
	return nil
}

// Stop switches an instance off, keeping its filesystem.
func (e *Engine) Stop(ctx context.Context, i *Instance) error {
	if i.Container == "" {
		return nil
	}
	return e.docker.Stop(ctx, i.Container)
}

// Resume switches a stopped instance back on. It is deliberately not Start:
// the container, and therefore the root filesystem, is the same one.
func (e *Engine) Resume(ctx context.Context, i *Instance) error {
	if i.Container == "" {
		return fmt.Errorf("instance %s has no container to start", i.Name)
	}
	if err := e.serveMetadata(i); err != nil {
		return err
	}
	return e.docker.Start(ctx, i.Container)
}

// Restart is a stop and a start, which is what a reboot is.
func (e *Engine) Restart(ctx context.Context, i *Instance) error {
	if err := e.Stop(ctx, i); err != nil {
		return err
	}
	return e.Resume(ctx, i)
}

// Delete removes the container and forgets the instance.
func (e *Engine) Delete(ctx context.Context, i *Instance) error {
	e.closeMetadata(i)
	e.mu.Lock()
	delete(e.inst, i.ResourceID)
	e.mu.Unlock()
	if i.Container == "" {
		return nil
	}
	return e.docker.Remove(ctx, i.Container)
}

// Logs is the instance's console output: what its boot script and anything
// it started have printed.
func (e *Engine) Logs(ctx context.Context, i *Instance, since time.Time) ([]byte, error) {
	if i.Container == "" {
		return nil, nil
	}
	return e.docker.Logs(ctx, i.Container, since)
}

// Exec runs a command inside an instance, which is what `nimbus ssh` uses
// for anything non-interactive.
func (e *Engine) Exec(ctx context.Context, i *Instance, cmd ...string) ([]byte, error) {
	if i.Container == "" {
		return nil, fmt.Errorf("instance %s is not running", i.Name)
	}
	return e.docker.Exec(ctx, i.Container, cmd...)
}

// Running reports whether the instance's container is up right now, which
// is the truth a plugin's state field has to agree with.
func (e *Engine) Running(ctx context.Context, i *Instance) bool {
	if i.Container == "" {
		return false
	}
	c, err := e.docker.Inspect(ctx, i.Container)
	return err == nil && c.Running
}

// Shutdown stops what this engine started, unless the caller asked for the
// containers to be left running.
func (e *Engine) Shutdown(ctx context.Context) {
	for _, i := range e.All() {
		e.closeMetadata(i)
		if e.cfg.KeepRunning {
			continue
		}
		_ = e.docker.Remove(ctx, i.Container)
	}
}

// serveMetadata opens this instance's own listener, if it has a handler and
// is not already serving.
//
// The port comes from asking the OS for one rather than from
// docker.FreePort, which binds a port and closes it again: that is
// unavoidable when the number has to be known BEFORE a container publishes
// it, and a needless race for a listener we open ourselves.
//
// It listens on every interface, not loopback, because the client is inside
// a container and reaches the host through the bridge gateway -- the same
// reason Nimbus itself binds ":port".
func (e *Engine) serveMetadata(i *Instance) error {
	if i.Metadata == nil || i.metadata != nil {
		return nil
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("serving metadata for instance %s: %w", i.Name, err)
	}
	i.listener = ln
	i.MetadataPort = ln.Addr().(*net.TCPAddr).Port
	srv := &http.Server{
		Handler:           i.Metadata,
		ReadHeaderTimeout: 10 * time.Second,
	}
	i.metadata = srv
	// srv, not i.metadata: the goroutine would otherwise read the field at
	// call time, and a Delete that nils it first panics inside
	// net/http.Serve -- measured, with a stack that mentions neither this
	// package nor the instance. The same shape as every other background
	// goroutine racing the delete that tears its subject down.
	go func() { _ = srv.Serve(ln) }()
	return nil
}

func (e *Engine) closeMetadata(i *Instance) {
	if i.metadata == nil {
		return
	}
	_ = i.metadata.Close()
	i.metadata, i.listener, i.MetadataPort = nil, nil, 0
}

func itoa(n int) string { return strconv.Itoa(n) }
