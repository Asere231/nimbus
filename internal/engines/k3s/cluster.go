package k3s

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A Cluster is one emulated cluster: a k3s server container, any agent
// containers, and the credentials that reach them.
//
// k3s is a full Kubernetes distribution in a single binary, so one privileged
// container really is a working cluster -- `kubectl get nodes` talks to the
// real API server, not to anything Nimbus wrote.
type Cluster struct {
	ResourceID string
	Name       string
	Container  string
	Network    string
	Port       int

	// Kubeconfig is generated once when the cluster becomes ready and then
	// returned byte-for-byte on every credential call.
	//
	// This HAS to be stable. azurerm calls listClusterUserCredential on every
	// refresh and writes the result into state, so a kubeconfig that differed
	// between reads would show as a permanent diff -- the same shape of bug as
	// M2's Content-MD5, where inventing a value per request made the plan
	// never settle.
	Kubeconfig []byte
	// The PEM material behind that kubeconfig, kept so Nimbus can talk to the
	// cluster API itself without re-parsing what it generated.
	CA   []byte
	Cert []byte
	Key  []byte

	Agents []string // agent container names, in pool order
}

// registry is the in-memory set of clusters this engine started.
type registry struct {
	mu sync.Mutex
	m  map[string]*Cluster // keyed by lower-cased resource id
}

func newRegistry() *registry { return &registry{m: map[string]*Cluster{}} }

func (r *registry) get(id string) *Cluster {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[strings.ToLower(id)]
}

func (r *registry) put(cl *Cluster) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[strings.ToLower(cl.ResourceID)] = cl
}

func (r *registry) remove(id string) *Cluster {
	r.mu.Lock()
	defer r.mu.Unlock()
	cl := r.m[strings.ToLower(id)]
	delete(r.m, strings.ToLower(id))
	return cl
}

func (r *registry) all() []*Cluster {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Cluster, 0, len(r.m))
	for _, cl := range r.m {
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// StableHex returns the first n bytes of a SHA-256 of s, hex-encoded. For
// names and ids that must be unique, readable, and identical across
// restarts.
func StableHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:n])
}

// --- naming and ports -------------------------------------------------------

// --- provisioning -----------------------------------------------------------

// k3s writes its certificates here. These paths are stable across k3s
// releases; if one is missing, the error names it so the cause is obvious
// rather than showing up later as a TLS failure.
// Exported so a cloud's tests can stub what reading them returns.
const (
	AdminCertPath = "/var/lib/rancher/k3s/server/tls/client-admin.crt"
	AdminKeyPath  = "/var/lib/rancher/k3s/server/tls/client-admin.key"
	ServerCAPath  = "/var/lib/rancher/k3s/server/tls/server-ca.crt"
)

// startCluster brings up the k3s server container and waits until the API
// answers through the published port.
func (e *Engine) Start(ctx context.Context, cl *Cluster) error {
	d := e.docker

	if err := d.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs managed Kubernetes clusters as k3s containers, so Docker "+
			"must be running. Start Docker Desktop (or your runtime) and apply again.", err)
	}

	// Pull only if missing, so an offline machine with the image already
	// present still works -- that is the whole point of docs/OFFLINE.md.
	if present, err := d.ImagePresent(ctx, e.cfg.Image); err == nil && !present {
		if err := d.Pull(ctx, e.cfg.Image); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				e.cfg.Image, err, e.cfg.Image)
		}
	}

	if err := d.EnsureNetwork(ctx, cl.Network); err != nil {
		return fmt.Errorf("creating the cluster network: %w", err)
	}

	// Reuse an existing container if one is already there -- a restart with
	// --persist reattaches rather than starting a second copy.
	if existing, err := d.Inspect(ctx, cl.Container); err == nil {
		if !existing.Running {
			return fmt.Errorf("container %s exists but is not running; remove it and apply again", cl.Container)
		}
	} else {
		var nf *docker.NotFoundError
		if !errors.As(err, &nf) {
			return err
		}
		if _, err := d.Run(ctx, docker.RunOptions{
			Name:       cl.Container,
			Image:      e.cfg.Image,
			Privileged: true, // k3s needs it; inside Docker's VM on macOS and Windows this is contained
			Network:    cl.Network,
			Ports:      map[string]string{fmt.Sprintf("127.0.0.1:%d", cl.Port): "6443"},
			Labels: map[string]string{
				docker.LabelCluster:  cl.Name,
				docker.LabelResource: cl.ResourceID,
				docker.LabelRole:     "server",
			},
			// --disable traefik keeps startup quick; nothing here needs an
			// ingress controller, and it is one less thing to wait for.
			Args:  []string{"server", "--tls-san", "127.0.0.1", "--disable", "traefik"},
			TmpFS: []string{"/run", "/var/run"},
		}); err != nil {
			return fmt.Errorf("starting the k3s container: %w", err)
		}
	}

	return e.waitReady(ctx, cl)
}

// waitReady blocks until the cluster's API answers, then builds the
// kubeconfig.
func (e *Engine) waitReady(ctx context.Context, cl *Cluster) error {
	deadline := time.Now().Add(e.cfg.Timeout)

	// 1. Wait for the certificates to exist inside the container.
	var adminCert, adminKey, serverCA []byte
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not write its certificates within %s; check `docker logs %s`",
				e.cfg.Timeout, cl.Container)
		}
		var err error
		adminCert, err = e.docker.Exec(ctx, cl.Container, "cat", AdminCertPath)
		if err == nil {
			adminKey, err = e.docker.Exec(ctx, cl.Container, "cat", AdminKeyPath)
		}
		if err == nil {
			serverCA, err = e.docker.Exec(ctx, cl.Container, "cat", ServerCAPath)
		}
		if err == nil && len(adminCert) > 0 && len(adminKey) > 0 && len(serverCA) > 0 {
			break
		}
		if errors.Is(err, docker.ErrUnavailable) {
			return err // Docker went away; no point waiting
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}

	cl.CA, cl.Cert, cl.Key = serverCA, adminCert, adminKey
	cl.Kubeconfig = RenderKubeconfig(cl.Name, cl.Port, serverCA, adminCert, adminKey)

	// 2. Wait for the API to answer through the PUBLISHED port, which is the
	// path kubectl will actually take. Checking readiness inside the container
	// would not prove the port mapping works.
	client, err := apiClient(serverCA, adminCert, adminKey)
	if err != nil {
		return fmt.Errorf("building a client for the new cluster: %w", err)
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/readyz", cl.Port)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("the cluster API did not become ready within %s; check `docker logs %s`",
				e.cfg.Timeout, cl.Container)
		}
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// apiClient builds an HTTPS client that trusts the cluster's CA and presents
// its admin certificate.
//
// This is how Nimbus talks to Kubernetes: plain net/http against the JSON API.
// client-go would be a third-party dependency, which CLAUDE.md rules out, and
// nothing here needs more than GETs of a few collections.
func apiClient(caPEM, certPEM, keyPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the cluster CA could not be parsed")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the cluster admin certificate could not be parsed: %w", err)
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			RootCAs:      pool,
			Certificates: []tls.Certificate{pair},
			MinVersion:   tls.VersionTLS12,
		}},
	}, nil
}

// renderKubeconfig writes a kubeconfig for the cluster.
//
// Generated with a fixed template rather than a YAML library: emitting YAML is
// string formatting, and parsing it is what the stdlib cannot do. The output
// is deterministic -- no timestamps, no map iteration -- because azurerm
// stores it and compares it on every refresh.
func RenderKubeconfig(name string, port int, caPEM, certPEM, keyPEM []byte) []byte {
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: %[1]s
  cluster:
    server: https://127.0.0.1:%[2]d
    certificate-authority-data: %[3]s
contexts:
- name: %[1]s
  context:
    cluster: %[1]s
    user: clusterUser_%[1]s
current-context: %[1]s
users:
- name: clusterUser_%[1]s
  user:
    client-certificate-data: %[4]s
    client-key-data: %[5]s
`, name, port, b64(caPEM), b64(certPEM), b64(keyPEM)))
}

// --- teardown ---------------------------------------------------------------

// stopCluster removes the cluster's containers and its network.
func (e *Engine) Stop(ctx context.Context, cl *Cluster) {
	for _, agent := range cl.Agents {
		_ = e.docker.Remove(ctx, agent)
	}
	_ = e.docker.Remove(ctx, cl.Container)
	// The network only exists for this cluster, so it goes too. Removing one
	// still in use fails harmlessly and is ignored.
	_ = e.docker.RemoveNetwork(ctx, cl.Network)
}
