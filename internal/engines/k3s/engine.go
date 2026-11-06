// Package k3s runs real Kubernetes clusters in Docker.
//
// It is the engine behind Azure's AKS and AWS's EKS, and it knows about
// neither. A cluster here is a k3s server container plus any number of
// agent containers: k3s is a full Kubernetes distribution in a single
// binary, so one privileged container really is a working cluster, and
// `kubectl get nodes` talks to the real API server rather than to anything
// Nimbus wrote.
//
// What a cloud plugin supplies is the name of the thing (its ARM ID or its
// EKS ARN), how many nodes it wants, and what to call the containers. What
// it gets back is a kubeconfig, a client, and a stream of the cluster's
// pods mirrored into the resource store so the visualizer can draw them.
package k3s

import (
	"time"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Defaults are pinned rather than :latest so a machine that pre-pulled the
// image stays offline-capable, and so the version reported to clients
// matches what kubectl actually finds.
const (
	DefaultImage    = "rancher/k3s:v1.34.12-k3s1"
	DefaultVersion  = "1.34.12"
	DefaultPortBase = 6550
	DefaultTimeout  = 4 * time.Minute
)

// Config is how a cloud plugin sets the engine up.
type Config struct {
	// Image is the k3s image to run.
	Image string
	// Version is what clients are told the cluster runs; it should track
	// Image.
	Version string
	// PortBase is the first host port tried for a cluster's API server.
	PortBase int
	// Timeout bounds how long a cluster may take to become ready.
	Timeout time.Duration
	// KeepRunning leaves containers up when Nimbus stops, so a restart
	// finds the cluster still there.
	KeepRunning bool
	// NamePrefix goes in front of every container name, so `docker ps`
	// says which cloud a container belongs to: nimbus-aks, nimbus-eks.
	NamePrefix string
}

func (c Config) withDefaults() Config {
	if c.Image == "" {
		c.Image = DefaultImage
	}
	if c.Version == "" {
		c.Version = DefaultVersion
	}
	if c.PortBase == 0 {
		c.PortBase = DefaultPortBase
	}
	if c.Timeout == 0 {
		c.Timeout = DefaultTimeout
	}
	if c.NamePrefix == "" {
		c.NamePrefix = "nimbus-k3s"
	}
	return c
}

// Engine starts and watches clusters for one cloud.
//
// One engine per cloud rather than one shared: each has its own container
// name prefix and its own registry of what it started, so stopping Azure's
// clusters never reaches into AWS's.
type Engine struct {
	cfg      Config
	docker   docker.Client
	store    *core.Store
	clusters *registry
	// cloud is written onto every mirrored Kubernetes object, so the
	// visualizer files a pod under the tab its cluster belongs to.
	cloud string
}

// New builds an engine. cloud is "azure" or "aws".
func New(cfg Config, d docker.Client, store *core.Store, cloud string) *Engine {
	return &Engine{
		cfg:      cfg.withDefaults(),
		docker:   d,
		store:    store,
		clusters: newRegistry(),
		cloud:    cloud,
	}
}

// Version is what this engine's clusters report running.
func (e *Engine) Version() string { return e.cfg.Version }

// Docker exposes the client, for the few things a plugin does itself --
// Azure's registry containers, for one.
func (e *Engine) Docker() docker.Client { return e.docker }

// SetDocker replaces the client, which tests use to swap in the Fake.
func (e *Engine) SetDocker(d docker.Client) { e.docker = d }

// KeepRunning reports whether containers survive a shutdown.
func (e *Engine) KeepRunning() bool { return e.cfg.KeepRunning }

// Get returns the cluster registered for a resource, or nil.
func (e *Engine) Get(resourceID string) *Cluster { return e.clusters.get(resourceID) }

// Put registers a cluster.
func (e *Engine) Put(cl *Cluster) { e.clusters.put(cl) }

// Remove unregisters a cluster and returns it.
func (e *Engine) Remove(resourceID string) *Cluster { return e.clusters.remove(resourceID) }

// All returns every registered cluster, in a stable order.
func (e *Engine) All() []*Cluster { return e.clusters.all() }

// NewCluster registers a cluster for a resource, picking its container
// names and a free port. It does not start anything.
func (e *Engine) NewCluster(resourceID, name string) (*Cluster, error) {
	if cl := e.clusters.get(resourceID); cl != nil {
		return cl, nil
	}
	port, err := docker.FreePort(e.cfg.PortBase)
	if err != nil {
		return nil, err
	}
	container := docker.Name(e.cfg.NamePrefix, resourceID, name)
	cl := &Cluster{
		ResourceID: resourceID,
		Name:       name,
		Container:  container,
		Network:    container + "-net",
		Port:       port,
	}
	e.clusters.put(cl)
	return cl, nil
}

// ClusterFor returns what a cluster's containers would be called, for the
// case where nothing is registered -- a Nimbus that restarted still has to
// be able to clean up after the one before it.
func (e *Engine) ClusterFor(resourceID, name string) *Cluster {
	container := docker.Name(e.cfg.NamePrefix, resourceID, name)
	return &Cluster{
		ResourceID: resourceID,
		Name:       name,
		Container:  container,
		Network:    container + "-net",
	}
}
