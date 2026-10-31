package azure

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Azure Container Registry, backed by the registry the Docker project itself
// publishes: one `registry:2` container per ACR resource.
//
// The design decision that shapes everything here is the login server.
// Nimbus reports `localhost:<port>`, not something that looks like
// `myacr.azurecr.io`, because Docker treats a registry on localhost as
// insecure by default: `docker push localhost:5001/app:v1` works on a fresh
// machine with no daemon configuration, no certificates and no network. A
// prettier hostname would mean either serving TLS the Docker daemon trusts --
// which lives in its own VM on macOS and Windows, out of our reach -- or
// asking everyone to edit their daemon's insecure-registries list first.
//
// The cost is that k3s cannot resolve `localhost:<port>` to our container,
// since inside the cluster localhost is the cluster. That is what the
// containerd mirror in aks_registry.go is for.
const typeRegistry = "Microsoft.ContainerRegistry/registries"

// Defaults for the registry engine.
const (
	DefaultRegistryImage    = "registry:2"
	DefaultRegistryPortBase = 5001
)

// registry is one emulated ACR: a container and the port it is published on.
type registry struct {
	ResourceID string
	Name       string
	Container  string
	Port       int
}

// LoginServer is what clients push to and pods pull from.
func (r *registry) LoginServer() string { return "localhost:" + strconv.Itoa(r.Port) }

type registries struct {
	mu sync.Mutex
	m  map[string]*registry
}

func newRegistries() *registries { return &registries{m: map[string]*registry{}} }

func (r *registries) get(id string) *registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[strings.ToLower(id)]
}

func (r *registries) put(reg *registry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[strings.ToLower(reg.ResourceID)] = reg
}

func (r *registries) remove(id string) *registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	reg := r.m[strings.ToLower(id)]
	delete(r.m, strings.ToLower(id))
	return reg
}

func (r *registries) all() []*registry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*registry, 0, len(r.m))
	for _, reg := range r.m {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ResourceID < out[j].ResourceID })
	return out
}

// registryContainerName derives a stable container name from the ARM ID, the
// same way clusters do: readable in `docker ps`, unique across resource
// groups, and independent of how the ID was cased.
func registryContainerName(resourceID, name string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(resourceID)))
	return fmt.Sprintf("nimbus-acr-%s-%s", sanitiseName(name), hex.EncodeToString(sum[:4]))
}

func registerACRHooks(s *Server) {
	s.registerHook(typeRegistry, Hook{
		Backing: "real",
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			if !validRegistryName(t.Name) {
				return &armError{400, "InvalidResourceName",
					"The registry name '" + t.Name + "' is invalid. It must be 5 to 50 alphanumeric characters."}
			}
			props := body["properties"].(map[string]any)

			reg, aerr := s.ensureRegistry(t, existing)
			if aerr != nil {
				return aerr
			}
			props["loginServer"] = reg.LoginServer()

			created := time.Now().UTC().Format(time.RFC3339)
			if existing != nil {
				if ep, ok := existing["properties"].(map[string]any); ok {
					if c := str(ep["creationDate"]); c != "" {
						created = c
					}
				}
			}
			props["creationDate"] = created
			setDefault(props, "adminUserEnabled", false)
			setDefault(props, "anonymousPullEnabled", false)
			setDefault(props, "dataEndpointEnabled", false)
			setDefault(props, "publicNetworkAccess", "Enabled")
			setDefault(props, "networkRuleBypassOptions", "AzureServices")
			setDefault(props, "zoneRedundancy", "Disabled")
			setDefault(props, "encryption", map[string]any{"status": "disabled"})
			setDefault(props, "policies", map[string]any{
				"quarantinePolicy":                 map[string]any{"status": "disabled"},
				"trustPolicy":                      map[string]any{"type": "Notary", "status": "disabled"},
				"retentionPolicy":                  map[string]any{"days": float64(7), "status": "disabled"},
				"exportPolicy":                     map[string]any{"status": "enabled"},
				"azureADAuthenticationAsArmPolicy": map[string]any{"status": "enabled"},
				"softDeletePolicy":                 map[string]any{"retentionDays": float64(7), "status": "disabled"},
			})
			if body["sku"] == nil {
				body["sku"] = map[string]any{"name": "Basic", "tier": "Basic"}
			}
			if sku, ok := body["sku"].(map[string]any); ok {
				setDefault(sku, "tier", str(sku["name"]))
			}
			return nil
		},

		// Starting the container takes a second or two -- a pull, the first
		// time -- so it happens in the background like a cluster does.
		Provision: func(s *Server, t target, body map[string]any) error {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			reg := s.registries.get(t.ID)
			if reg == nil {
				return fmt.Errorf("no registry recorded for %s", t.ID)
			}
			if err := s.startRegistry(ctx, reg); err != nil {
				return err
			}
			if err := s.waitForRegistry(ctx, reg); err != nil {
				return err
			}
			// A cluster that already exists has to learn about this registry,
			// since nothing says ACR is created before AKS.
			s.announceRegistryToClusters(ctx, reg)
			return nil
		},

		AfterDelete: func(s *Server, t target, body map[string]any) {
			s.registries.remove(t.ID)
		},
		Deprovision: func(s *Server, t target) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			name := registryContainerName(t.ID, t.Name)
			_ = s.docker.Stop(ctx, name)
			_ = s.docker.Remove(ctx, name)
		},
	})
}

// ensureRegistry finds or creates the bookkeeping for a registry, including
// its port.
//
// The port is recovered from the STORED login server when there is one. It is
// part of the registry's identity: it appears in every image reference that
// was ever pushed and in each cluster's containerd mirror, so a restart that
// handed out a different port would break images that already exist.
func (s *Server) ensureRegistry(t target, existing map[string]any) (*registry, *armError) {
	if reg := s.registries.get(t.ID); reg != nil {
		return reg, nil
	}
	port := 0
	if existing != nil {
		if ep, ok := existing["properties"].(map[string]any); ok {
			port = portOfLoginServer(str(ep["loginServer"]))
		}
	}
	if port == 0 {
		p, err := freePort(s.cfg.RegistryPortBase)
		if err != nil {
			return nil, &armError{500, "NoPortAvailable", err.Error()}
		}
		port = p
	}
	reg := &registry{
		ResourceID: t.ID, Name: t.Name,
		Container: registryContainerName(t.ID, t.Name), Port: port,
	}
	s.registries.put(reg)
	return reg, nil
}

func portOfLoginServer(login string) int {
	i := strings.LastIndex(login, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(login[i+1:])
	if err != nil {
		return 0
	}
	return p
}

// startRegistry runs the registry container, reusing one that is already
// there (a --persist restart).
func (s *Server) startRegistry(ctx context.Context, reg *registry) error {
	d := s.docker
	if err := d.Available(ctx); err != nil {
		return fmt.Errorf("%w\n\nNimbus runs container registries as Docker containers, so Docker must be running. "+
			"Start Docker Desktop (or your runtime) and apply again.", err)
	}
	if present, err := d.ImagePresent(ctx, s.cfg.RegistryImage); err == nil && !present {
		if err := d.Pull(ctx, s.cfg.RegistryImage); err != nil {
			return fmt.Errorf("pulling %s: %w\n\nPre-pull it while online: docker pull %s",
				s.cfg.RegistryImage, err, s.cfg.RegistryImage)
		}
	}

	if existing, err := d.Inspect(ctx, reg.Container); err == nil {
		if !existing.Running {
			return fmt.Errorf("container %s exists but is not running; remove it and apply again", reg.Container)
		}
		// A container left over from a previous run may be published on a
		// different port than the one this registry's login server names --
		// it happens whenever Nimbus restarts without --persist, so the
		// resource is created afresh while the container survives. The ARM
		// resource decides the port, because that is what image references
		// already contain, so the stale container is replaced.
		if port := containerPort(existing, "5000/tcp"); port != reg.Port {
			if err := d.Remove(ctx, reg.Container); err != nil {
				return fmt.Errorf("replacing the registry container published on %d: %w", port, err)
			}
		} else {
			return nil
		}
	} else {
		var nf *docker.NotFoundError
		if !errors.As(err, &nf) {
			return err
		}
	}

	if _, err := d.Run(ctx, docker.RunOptions{
		Name:  reg.Container,
		Image: s.cfg.RegistryImage,
		// Loopback only. The registry has no authentication at all, so
		// publishing it on every interface would put an open, writable
		// registry on whatever network the machine is attached to.
		Ports: map[string]string{fmt.Sprintf("127.0.0.1:%d", reg.Port): "5000"},
		Labels: map[string]string{
			labelRegistry: reg.Name,
			labelResource: reg.ResourceID,
			labelRole:     "registry",
		},
	}); err != nil {
		return fmt.Errorf("starting the registry container: %w", err)
	}
	return nil
}

// waitForRegistry blocks until the registry answers its API version check,
// which is what a `docker push` does first.
func (s *Server) waitForRegistry(ctx context.Context, reg *registry) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/v2/", reg.Port)
	deadline := time.Now().Add(90 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return err
		}
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == 200 || res.StatusCode == 401 {
				return nil
			}
			last = fmt.Errorf("%s answered %s", url, res.Status)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("the registry did not become ready at %s: %v", url, last)
}

// reattachRegistries adopts registry containers left running by a previous
// process, for the same reasons clusters are reattached: otherwise a later
// delete leaves them running forever.
func (s *Server) reattachRegistries(ctx context.Context) {
	if err := s.docker.Available(ctx); err != nil {
		return
	}
	found, err := s.docker.List(ctx, labelRole+"=registry")
	if err != nil {
		return
	}
	for _, c := range found {
		id := c.Labels[labelResource]
		if id == "" || !s.store.Exists(id) {
			continue
		}
		port := containerPort(c, "5000/tcp")
		if port == 0 || !c.Running {
			s.setProvisioningState(id, "Failed")
			continue
		}
		s.registries.put(&registry{
			ResourceID: id, Name: c.Labels[labelRegistry],
			Container: c.Name, Port: port,
		})
		s.setProvisioningState(id, "Succeeded")
	}
}

// registryAction answers listCredentials.
//
// The credentials are decorative: `registry:2` runs without authentication,
// so anything is accepted. They exist because azurerm reads them when
// admin_enabled is set, and because a Kubernetes imagePullSecret built from
// them has to contain something.
func (s *Server) registryAction(t target) (any, bool) {
	res, err := s.store.Get(t.ID)
	if err != nil || !eq(res.Type, typeRegistry) {
		return nil, false
	}
	if !eq(t.Action, "listCredentials") {
		return nil, false
	}
	return map[string]any{
		"username": res.Name,
		"passwords": []any{
			map[string]any{"name": "password", "value": registryPassword(res.ID, "1")},
			map[string]any{"name": "password2", "value": registryPassword(res.ID, "2")},
		},
	}, true
}

func registryPassword(id, which string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(id) + "/password" + which))
	return base64.RawURLEncoding.EncodeToString(sum[:24])
}

// validRegistryName applies Azure's rule: 5-50 alphanumeric characters, no
// dashes. The name becomes part of a container name here, and in Azure part of
// a hostname.
func validRegistryName(n string) bool {
	if len(n) < 5 || len(n) > 50 {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// containerPort reads the host port a container publishes for one of its
// ports, e.g. "5000/tcp".
func containerPort(c docker.Container, port string) int {
	hostPort := c.Ports[port]
	i := strings.LastIndex(hostPort, ":")
	if i < 0 {
		return 0
	}
	p, err := strconv.Atoi(hostPort[i+1:])
	if err != nil {
		return 0
	}
	return p
}
