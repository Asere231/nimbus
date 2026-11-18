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

	// Grouped by resource id, because a service may be several replicas
	// and they are ONE service. Putting one Service per container was the
	// first version of this: the last container listed won, Extra came
	// back empty, and a three-replica app served from one container with
	// the other two running and unreferenced. Not a leak -- removeGroup
	// sweeps by label -- but a silent halving of what was asked for.
	byResource := map[string][]docker.Container{}
	var order []string
	for _, c := range containers {
		id := c.Labels[docker.LabelResource]
		if id == "" || (keep != nil && !keep(id)) {
			continue
		}
		if _, seen := byResource[id]; !seen {
			order = append(order, id)
		}
		byResource[id] = append(byResource[id], c)
	}

	var found []Found
	for _, id := range order {
		group := byResource[id]
		name := group[0].Labels[docker.LabelService]

		// The primary is identified rather than guessed: Docker's listing
		// order is not the start order, and svc.Port is what the FQDN
		// reaches. Start names replica 0 without an index, so the name it
		// would have had is computable from the labels alone.
		primaryName := docker.Name(e.cfg.NamePrefix, id, name)
		primary, extras := splitPrimary(group, primaryName)

		containerPort, hostPort := onlyPublishedPort(primary)
		svc := &Service{
			ResourceID:    id,
			Name:          name,
			Image:         primary.Image,
			ContainerPort: containerPort,
			Container:     primary.Name,
			Port:          hostPort,
			Replicas:      len(group),
		}
		for _, c := range extras {
			_, port := onlyPublishedPort(c)
			svc.Extra = append(svc.Extra, Replica{Container: c.Name, Port: port})
		}
		e.Put(svc)
		found = append(found, Found{
			ResourceID: id, Name: svc.Name, Container: primary.Name,
			Port: hostPort, Running: primary.Running,
		})
	}
	return found
}

// splitPrimary picks the replica whose port the service is reached on.
//
// The container Start named without an index, when it is still there; the
// first in the listing otherwise, so a service whose primary container was
// removed behind Nimbus's back still comes back serving from a replica
// that exists rather than from nothing. Docker prefixes a listed name with
// "/", which is why both spellings are compared.
func splitPrimary(group []docker.Container, primaryName string) (docker.Container, []docker.Container) {
	for i, c := range group {
		if strings.TrimPrefix(c.Name, "/") == primaryName {
			rest := make([]docker.Container, 0, len(group)-1)
			rest = append(rest, group[:i]...)
			rest = append(rest, group[i+1:]...)
			return c, rest
		}
	}
	return group[0], group[1:]
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
