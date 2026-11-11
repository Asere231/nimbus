// Package azure emulates the Azure Resource Manager (ARM) control plane plus
// the small identity surface that SDKs and Terraform need to get a token.
package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/k3s"
	"github.com/Asere231/nimbus/internal/engines/vm"
)

// Config for the Azure emulator.
type Config struct {
	// PublicURL is how clients reach this server, e.g. https://localhost:4701.
	// It is returned as the login and Resource Manager endpoint in metadata.
	PublicURL string
	TenantID  string
	ClientID  string
	// DataDir, when set, is where the data planes keep their state so it
	// survives a restart: blob content under {DataDir}/storage, vault secrets
	// under {DataDir}/keyvault. Empty = memory only.
	DataDir string

	// K3sImage is the image AKS clusters run. Pinned rather than :latest so a
	// machine that pre-pulled it stays offline-capable, and so the Kubernetes
	// version matches what kubectl expects.
	K3sImage string
	// KubernetesVersion is reported to clients; it should track K3sImage.
	KubernetesVersion string
	// ClusterPortBase is the first host port tried for a cluster's API.
	ClusterPortBase int
	// RegistryImage is the image container registries run.
	RegistryImage string
	// RegistryPortBase is the first host port tried for a registry.
	RegistryPortBase int
	// ClusterTimeout bounds how long a cluster may take to become ready.
	ClusterTimeout time.Duration
	// KeepClusters leaves k3s containers running when Nimbus stops, so a
	// restart reattaches instead of waiting for a fresh boot. Set with
	// --persist; without it clusters are torn down on exit.
	KeepClusters bool

	// EnforceIAM turns RBAC evaluation on. With it off nothing is ever
	// refused, which is how this cloud behaved before Milestone 14 and is
	// still the default: existing examples must apply unchanged.
	EnforceIAM bool
	// Principal is the object id a request with no bearer token is taken
	// to be. Empty means the default identity, which holds no assignments
	// and is treated as unrestricted -- see principal.go.
	Principal string
}

// Defaults for the AKS engine.
const (
	DefaultK3sImage          = "rancher/k3s:v1.34.12-k3s1"
	DefaultKubernetesVersion = "1.34.12"
	DefaultClusterPortBase   = 6443
	DefaultClusterTimeout    = 5 * time.Minute
)

// Server is the Azure plugin.
type Server struct {
	cfg           Config
	store         *core.Store
	bus           *core.Bus
	hooks         map[string]Hook
	storage       *storage.Handler
	keyvault      *keyvault.Handler
	ops           *operations
	deploymentOps *deploymentOperations
	vaults        *deletedVaults
	docker        docker.Client
	// k3s runs the AKS clusters. The engine is shared with AWS's EKS and
	// knows nothing about either -- see internal/engines/k3s.
	k3s        *k3s.Engine
	registries *registries
	// vms runs the virtual machines, on the engine every cloud's
	// instances use. vmMu is held across check-and-act on one, so a boot
	// cannot overtake the delete that tears it down.
	vms  *vm.Engine
	vmMu sync.Mutex
}

// New creates the Azure emulator.
func New(cfg Config, store *core.Store, bus *core.Bus) *Server {
	if cfg.TenantID == "" {
		cfg.TenantID = DefaultTenantID
	}
	if cfg.ClientID == "" {
		cfg.ClientID = DefaultClientID
	}
	if cfg.K3sImage == "" {
		cfg.K3sImage = DefaultK3sImage
	}
	if cfg.KubernetesVersion == "" {
		cfg.KubernetesVersion = DefaultKubernetesVersion
	}
	if cfg.ClusterPortBase == 0 {
		cfg.ClusterPortBase = DefaultClusterPortBase
	}
	if cfg.RegistryImage == "" {
		cfg.RegistryImage = DefaultRegistryImage
	}
	if cfg.RegistryPortBase == 0 {
		cfg.RegistryPortBase = DefaultRegistryPortBase
	}
	if cfg.ClusterTimeout == 0 {
		cfg.ClusterTimeout = DefaultClusterTimeout
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	s := &Server{
		cfg: cfg, store: store, bus: bus,
		hooks:         map[string]Hook{},
		storage:       storage.NewHandler(storage.NewStore()),
		keyvault:      keyvault.NewHandler(keyvault.NewStore()),
		ops:           newOperations(),
		deploymentOps: newDeploymentOperations(),
		vaults:        newDeletedVaults(),
		docker:        docker.NewCLI(),
		registries:    newRegistries(),
	}
	s.k3s = k3s.New(k3s.Config{
		Image:       cfg.K3sImage,
		Version:     cfg.KubernetesVersion,
		PortBase:    cfg.ClusterPortBase,
		Timeout:     cfg.ClusterTimeout,
		KeepRunning: cfg.KeepClusters,
		NamePrefix:  "nimbus-aks",
	}, s.docker, store, "azure")
	s.vms = vm.New(vm.Config{
		NamePrefix:   "nimbus-vm",
		KeepRunning:  cfg.KeepClusters,
		StartTimeout: cfg.ClusterTimeout,
	}, s.docker)
	// The challenge a Key Vault client gets points at our own login endpoint,
	// and asks for a token for Azure's Key Vault audience -- which our token
	// endpoint will happily issue, since it issues tokens for any scope.
	s.keyvault.Login = cfg.PublicURL + "/" + cfg.TenantID
	s.keyvault.Audience = keyVaultAudience
	s.keyvault.BaseURI = s.vaultURI
	if cfg.DataDir != "" {
		if err := s.storage.Store().Persist(filepath.Join(cfg.DataDir, "storage")); err != nil {
			log.Printf("nimbus: storage persistence disabled: %v", err)
		}
		if err := s.keyvault.Store().Persist(filepath.Join(cfg.DataDir, "keyvault")); err != nil {
			log.Printf("nimbus: key vault persistence disabled: %v", err)
		}
	}
	s.storage.OnChange = s.mirrorDataPlaneContainer
	registerNetworkHooks(s)
	registerStorageHooks(s)
	registerContainerHooks(s)
	registerAKSHooks(s)
	registerIdentityHooks(s)
	registerAuthorizationHooks(s)
	registerLogAnalyticsHooks(s)
	registerKeyVaultHooks(s)
	registerDeploymentHooks(s)
	registerACRHooks(s)
	registerVMHooks(s)

	// The default subscription exists from the start, rather than appearing
	// the first time something asks for it. `az login` lists subscriptions
	// before anything else happens: finding none, it creates a tenant-level
	// account and then uses the TENANT id as the subscription id, so every
	// resource lands under a subscription that is not the one the examples
	// and the docs name. Terraform never notices, because it is always
	// configured with an explicit subscription_id.
	s.ensureSubscription(DefaultSubscriptionID)
	return s
}

// Well-known IDs the emulator hands out. Any values work for clients; these
// are just stable defaults so examples can hard-code them.
const (
	DefaultTenantID       = "00000000-0000-0000-0000-000000000001"
	DefaultClientID       = "00000000-0000-0000-0000-000000000002"
	DefaultSubscriptionID = "00000000-0000-0000-0000-000000000000"
	DefaultObjectID       = "00000000-0000-0000-0000-000000000003"
)

// BlobBrowser returns the data-plane handler, which implements the visualizer's
// blob-browsing interface.
func (s *Server) BlobBrowser() *storage.Handler { return s.storage }

// SecretBrowser returns the Key Vault data-plane handler, which implements the
// visualizer's secret-browsing interface.
func (s *Server) SecretBrowser() *keyvault.Handler { return s.keyvault }

// Start begins background work: adopting clusters left running by a previous
// process, and polling each one so the visualizer sees what is inside it.
// Returns a stop function.
// useDocker swaps the Docker client, for tests.
//
// It has to reach BOTH the server and the k3s engine: the server starts
// registry containers itself while the engine starts clusters, and a test
// that set only one of them would silently talk to the real daemon
// through the other.
func (s *Server) useDocker(d docker.Client) {
	s.docker = d
	s.k3s.SetDocker(d)
	s.vms.SetDocker(d)
}

func (s *Server) Start() func() {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// Reattach first, so a restart with --persist picks up existing
		// containers before the watcher starts asking about them.
		reattachCtx, done := context.WithTimeout(ctx, 2*time.Minute)
		s.reattachRegistries(reattachCtx)
		// A cluster whose container is gone or unreachable is Failed: the
		// resource is what Terraform reads, so it has to say so.
		for _, found := range s.k3s.Reattach(reattachCtx, s.store.Exists) {
			state := "Failed"
			if found.Ready {
				state = "Succeeded"
			}
			s.setProvisioningState(found.Cluster.ResourceID, state)
		}
		done()
		s.k3s.Watch(ctx)
	}()
	return cancel
}

// Shutdown stops clusters when they are not meant to outlive Nimbus.
//
// With --persist they are deliberately left running, so a restart reattaches
// instead of waiting for a fresh k3s boot. Without it, leaving containers
// behind would be a surprise: nothing on the machine would say where they
// came from.
func (s *Server) Shutdown(ctx context.Context) {
	if s.cfg.KeepClusters {
		return
	}
	for _, cl := range s.k3s.All() {
		s.k3s.Stop(ctx, cl)
	}
	// Registries too. A registry container that outlives Nimbus is worse than
	// an orphaned cluster: it holds the port its login server names, so the
	// next run either collides with it or picks a different port and leaves
	// pushed images unreachable.
	for _, reg := range s.registries.all() {
		_ = s.docker.Stop(ctx, reg.Container)
		_ = s.docker.Remove(ctx, reg.Container)
	}
}

// storageSuffix is the DNS suffix for the storage data plane, including our
// port. Clients join it as {account}.{service}.{suffix}; see the comment in
// handleMetadata for why the port belongs in it.
func (s *Server) storageSuffix() string {
	suffix := StorageSuffix
	if u, err := url.Parse(s.cfg.PublicURL); err == nil {
		if p := u.Port(); p != "" {
			suffix += ":" + p
		}
	}
	return suffix
}

// ServeHTTP routes every request and records it on the event bus.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: 200}
	note := s.route(rec, r)
	s.bus.Publish(core.Event{
		Kind:       "request",
		Action:     "call",
		Cloud:      "azure",
		Method:     r.Method,
		Path:       r.URL.Path,
		Status:     rec.status,
		DurationMs: float64(time.Since(start).Microseconds()) / 1000,
		Note:       note,
	})
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) (note string) {
	// The storage data plane shares this listener and is told apart by Host:
	// {account}.{service}.storage.nimbus.localhost. ARM requests arrive on the
	// plain host, so this never shadows them.
	if t, ok := storage.ParseHost(r.Host, s.storageSuffix()); ok {
		return s.storage.Serve(w, r, t)
	}
	// The same trick for Key Vault: {vault}.vault.nimbus.localhost.
	if name, ok := keyvault.ParseHost(r.Host, s.keyVaultSuffix()); ok {
		return s.keyvault.Serve(w, r, name)
	}

	// Authorization, in one place in front of ARM.
	//
	// It sits AFTER the data-plane Host checks above, because a Shared Key
	// request carries no principal at all and there is nothing to evaluate
	// it against -- see authorization.go on dataActions. It sits BEFORE
	// the identity paths below, because asking for a token is the
	// bootstrap: there is no principal yet, and refusing it would make
	// the flag unusable. Same shape as AWS's token-webhook exemption.
	if s.cfg.EnforceIAM {
		if denial, note := s.authorize(r); denial != nil {
			writeError(w, denial.Status, denial.Code, "%s", denial.Message)
			return note
		}
	}

	p := r.URL.Path
	switch {
	case p == "/metadata/endpoints":
		s.handleMetadata(w, r)
		return "metadata"
	case strings.HasSuffix(p, "/oauth2/v2.0/token") || strings.HasSuffix(p, "/oauth2/token"):
		s.handleToken(w, r)
		return "token issued"
	case strings.HasSuffix(p, "/.well-known/openid-configuration"):
		s.handleOpenID(w, r)
		return "openid configuration"
	case strings.HasSuffix(p, "/discovery/instance"):
		s.handleInstanceDiscovery(w, r)
		return "instance discovery"
	case isOperationPath(p):
		return s.handleOperation(w, r)
	case p == "/" || p == "":
		writeJSON(w, 200, map[string]any{"emulator": "nimbus", "cloud": "azure", "resourceManager": s.cfg.PublicURL})
		return ""
	}
	return s.handleARM(w, r)
}

// --- helpers ---------------------------------------------------------------

const operationsPath = "/providers/Microsoft.Resources/operations/nimbus-"

func isOperationPath(p string) bool {
	return strings.Contains(strings.ToLower(p), strings.ToLower(operationsPath))
}

// markAsync tells the client this was a long-running operation and where to
// poll, registering an operation that has ALREADY finished.
//
// Most writes in Nimbus really do complete before they are answered, so their
// operation is born Succeeded and the client's first poll ends it.
// Retry-After: 1 keeps clients from waiting the SDK default of 10 seconds per
// resource. Types that genuinely take time use markAsyncOperation instead.
func (s *Server) markAsync(w http.ResponseWriter, t target) {
	s.markAsyncOperation(w, t, s.ops.done(t.ID))
}

// markAsyncOperation points the client at a specific operation.
func (s *Server) markAsyncOperation(w http.ResponseWriter, t target, op *operation) {
	url := fmt.Sprintf("%s/subscriptions/%s%s%s?api-version=2020-01-01",
		s.cfg.PublicURL, t.Subscription, operationsPath, op.ID)
	w.Header().Set("Azure-AsyncOperation", url)
	w.Header().Set("Retry-After", "1")
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("x-ms-request-id", newUUID())
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeError writes an ARM-shaped error, which SDKs parse into readable messages.
func writeError(w http.ResponseWriter, status int, code, format string, args ...any) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": fmt.Sprintf(format, args...)},
	})
}

func readBody(r *http.Request) (map[string]any, error) {
	data, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		// The az CLI sends a deployment's template spliced in under an
		// unquoted key; see jsonc.go. Only retried after a strict parse has
		// failed, so a well-formed body is never rewritten.
		if repaired := quoteBareKeys(data); json.Unmarshal(repaired, &m) == nil {
			return m, nil
		}
		return nil, fmt.Errorf("request body is not a JSON object: %w", err)
	}
	return m, nil
}
