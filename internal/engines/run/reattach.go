package run

import (
	"context"
	"strconv"
	"strings"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Finding services again after Nimbus restarts.
//
// With --persist the containers outlive the process, so a fresh Nimbus has
// to adopt them rather than forget them. Forgetting them was the old
// behaviour, and for this engine the symptom was the sharpest of the three:
// a Container App or Cloud Run service kept its FQDN in the store and
// stopped serving, because the proxy map a request goes through was empty
// and svc.Port was zero. The container was up, the resource read back
// perfectly, and the address 503'd.
//
// The proxy itself needs no rebuilding here -- proxyFor builds one lazily
// from svc.Port on the next request -- so restoring the Service with its
// port restores the serving path with it. That is why the port is the one
// field this must get right.
//
// Unlike a function, a service's CONTAINER port is not fixed: a client
// chooses what the image listens on. It is recovered from the published
// mapping rather than guessed, since the engine publishes exactly one.

// Found is one adopted service.
type Found struct {
	ResourceID string
	Name       string
	Container  string
	Port       int
	Running    bool
}

// Reattach adopts every service this engine's containers describe.
//
// keep decides which resource ids are still wanted; a container whose
// resource has been deleted is left alone rather than adopted, since
// something else is responsible for cleaning it up. Azure's container apps
// and GCP's Cloud Run services share this engine and the same role label,
// and are told apart by `keep` -- a resource id is already cloud-shaped, so
// the plugin recognising its own is surer than matching a name prefix.
func (e *Engine) Reattach(ctx context.Context, keep func(resourceID string) bool) []Found {
	if err := e.docker.Available(ctx); err != nil {
		return nil // no Docker, nothing to reattach to
	}
	containers, err := e.docker.List(ctx, docker.LabelRole+"=service")
	if err != nil {
		return nil
	}

	var found []Found
	for _, c := range containers {
		id := c.Labels[docker.LabelResource]
		if id == "" || (keep != nil && !keep(id)) {
			continue
		}
		containerPort, hostPort := onlyPublishedPort(c)
		svc := &Service{
			ResourceID:    id,
			Name:          c.Labels[docker.LabelService],
			Image:         c.Image,
			ContainerPort: containerPort,
			Container:     c.Name,
			Port:          hostPort,
		}
		e.Put(svc)
		found = append(found, Found{
			ResourceID: id, Name: svc.Name, Container: c.Name,
			Port: hostPort, Running: c.Running,
		})
	}
	return found
}

// onlyPublishedPort reads back the one port mapping Start created, as
// (containerPort, hostPort).
//
// The engine publishes exactly one port per service, so there is no
// ambiguity to resolve -- and nothing records what the container port was,
// which is why it is read from the mapping rather than assumed. A container
// publishing nothing yields (0, 0), and a service with no port is reported
// as adopted but unusable rather than silently given a wrong one.
func onlyPublishedPort(c docker.Container) (containerPort, hostPort int) {
	for cp := range c.Ports {
		n, err := strconv.Atoi(strings.TrimSuffix(cp, "/tcp"))
		if err != nil {
			continue
		}
		if p := docker.HostPort(c, cp); p != 0 {
			return n, p
		}
	}
	return 0, 0
}
