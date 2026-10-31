package azure

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// A cluster is one emulated AKS cluster: a k3s server container, any agent
// containers, and the credentials that reach them.
//
// k3s is a full Kubernetes distribution in a single binary, so one privileged
// container really is a working cluster -- `kubectl get nodes` talks to the
// real API server, not to anything Nimbus wrote.
type cluster struct {
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

// clusters is the in-memory registry of them.
type clusters struct {
	mu sync.Mutex
	m  map[string]*cluster // keyed by lower-cased ARM resource ID
}

func newClusters() *clusters { return &clusters{m: map[string]*cluster{}} }

func (c *clusters) get(id string) *cluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[strings.ToLower(id)]
}

func (c *clusters) put(cl *cluster) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[strings.ToLower(cl.ResourceID)] = cl
}

func (c *clusters) remove(id string) *cluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	cl := c.m[strings.ToLower(id)]
	delete(c.m, strings.ToLower(id))
	return cl
}

func (c *clusters) all() []*cluster {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*cluster, 0, len(c.m))
	for _, cl := range c.m {
		out = append(out, cl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// stableHex returns the first n bytes of a SHA-256 of s, hex-encoded. Used for
// names and IDs that must be unique, readable, and identical across restarts.
func stableHex(s string, n int) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:n])
}

// --- naming and ports -------------------------------------------------------

const (
	// labelCluster marks every container Nimbus starts for a cluster, so they
	// can be found again after a restart.
	labelCluster  = "nimbus.cluster"
	labelResource = "nimbus.resource-id"
	labelRole     = "nimbus.role"
)

// containerName derives a stable, legal container name from the ARM ID.
//
// Resource IDs are long and contain slashes, so the name is the cluster's own
// name plus a short hash of the full ID -- short enough to read in `docker ps`
// and unique enough that two clusters of the same name in different resource
// groups do not collide.
func containerName(resourceID, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(resourceID)))
	return fmt.Sprintf("nimbus-aks-%s-%s", sanitiseName(name), hex.EncodeToString(sum[:4]))
}

func sanitiseName(n string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(n) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "cluster"
	}
	if len(s) > 24 {
		s = s[:24]
	}
	return s
}

// freePort finds a usable host port at or above base, by asking the OS to bind
// it. Checking a port is free by binding it is the only reliable way; a
// registry of "ports we handed out" would not notice something else on the
// machine already using one.
func freePort(base int) (int, error) {
	for port := base; port < base+500; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		_ = ln.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no free port found between %d and %d", base, base+500)
}

// --- provisioning -----------------------------------------------------------

// k3s writes its certificates here. These paths are stable across k3s
// releases; if one is missing, the error names it so the cause is obvious
// rather than showing up later as a TLS failure.
const (
	k3sAdminCert = "/var/lib/rancher/k3s/server/tls/client-admin.crt"
	k3sAdminKey  = "/var/lib/rancher/k3s/server/tls/client-admin.key"
	k3sServerCA  = "/var/lib/rancher/k3s/server/tls/server-ca.crt"
)

// startCluster brings up the k3s server container and waits until the API
// answers through the published port.
func (s *Server) startCluster(ctx context.Context, cl *cluster) error {
	d := s.docker

	if err := d.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs AKS clusters as k3s containers, so Docker must be running. "+
			"Start Docker Desktop (or your runtime) and apply again.", err)
	}

	// Pull only if missing, so an offline machine with the image already
	// present still works -- that is the whole point of docs/OFFLINE.md.
	if present, err := d.ImagePresent(ctx, s.cfg.K3sImage); err == nil && !present {
		if err := d.Pull(ctx, s.cfg.K3sImage); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				s.cfg.K3sImage, err, s.cfg.K3sImage)
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
			Image:      s.cfg.K3sImage,
			Privileged: true, // k3s needs it; inside Docker's VM on macOS and Windows this is contained
			Network:    cl.Network,
			Ports:      map[string]string{fmt.Sprintf("127.0.0.1:%d", cl.Port): "6443"},
			Labels: map[string]string{
				labelCluster:  cl.Name,
				labelResource: cl.ResourceID,
				labelRole:     "server",
			},
			// --disable traefik keeps startup quick; nothing here needs an
			// ingress controller, and it is one less thing to wait for.
			Args:  []string{"server", "--tls-san", "127.0.0.1", "--disable", "traefik"},
			TmpFS: []string{"/run", "/var/run"},
		}); err != nil {
			return fmt.Errorf("starting the k3s container: %w", err)
		}
	}

	return s.waitForCluster(ctx, cl)
}

// waitForCluster blocks until the cluster's API answers, then builds the
// kubeconfig.
func (s *Server) waitForCluster(ctx context.Context, cl *cluster) error {
	deadline := time.Now().Add(s.cfg.ClusterTimeout)

	// 1. Wait for the certificates to exist inside the container.
	var adminCert, adminKey, serverCA []byte
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not write its certificates within %s; check `docker logs %s`",
				s.cfg.ClusterTimeout, cl.Container)
		}
		var err error
		adminCert, err = s.docker.Exec(ctx, cl.Container, "cat", k3sAdminCert)
		if err == nil {
			adminKey, err = s.docker.Exec(ctx, cl.Container, "cat", k3sAdminKey)
		}
		if err == nil {
			serverCA, err = s.docker.Exec(ctx, cl.Container, "cat", k3sServerCA)
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
	cl.Kubeconfig = renderKubeconfig(cl.Name, cl.Port, serverCA, adminCert, adminKey)

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
				s.cfg.ClusterTimeout, cl.Container)
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
func renderKubeconfig(name string, port int, caPEM, certPEM, keyPEM []byte) []byte {
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
func (s *Server) stopCluster(ctx context.Context, cl *cluster) {
	for _, agent := range cl.Agents {
		_ = s.docker.Remove(ctx, agent)
	}
	_ = s.docker.Remove(ctx, cl.Container)
	// The network only exists for this cluster, so it goes too. Removing one
	// still in use fails harmlessly and is ignored.
	_ = s.docker.RemoveNetwork(ctx, cl.Network)
}
