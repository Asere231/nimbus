package funcs

import (
	"context"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Finding functions again after Nimbus restarts.
//
// With --persist the containers outlive the process -- Shutdown returns
// early on the flag before it reaches this engine -- so a fresh Nimbus has
// to adopt them rather than forget them.
//
// Forgetting them was the old behaviour and the symptom was worse than an
// orphaned container: the store still described the function, so a client
// read it as Active, while Get returned nil, every invoke answered
// ResourceNotReadyException, and Stop could not find the container to
// remove. The container sat plainly Up in `docker ps` and nothing in that
// picture pointed at the cause.
//
// Everything needed is on the container. The resource id and the name are
// labels; the port RIE answers on is the published one, and recovering it
// is the point -- an adopted function with no port cannot be invoked, which
// would make adopting it pointless.
//
// What is deliberately NOT recovered is the code directory. A function's
// code lives under the engine's Dir keyed by a hash of the resource id, so
// CodeDir finds it again without being told; the bind mount inside the
// running container still points at it.

// Found is one adopted function.
type Found struct {
	ResourceID string
	Name       string
	Container  string
	Port       int
	Running    bool
}

// Reattach adopts every function this engine's containers describe.
//
// keep decides which resource ids are still wanted; a container whose
// resource has been deleted is left alone rather than adopted, since
// something else is responsible for cleaning it up. Another cloud's
// functions would carry the same role label and are told apart by `keep`
// rather than by container name, because a resource id is already
// cloud-shaped -- the same division k3s.Reattach and vm.Reattach use.
func (e *Engine) Reattach(ctx context.Context, keep func(resourceID string) bool) []Found {
	if err := e.docker.Available(ctx); err != nil {
		return nil // no Docker, nothing to reattach to
	}
	containers, err := e.docker.List(ctx, docker.LabelRole+"=function")
	if err != nil {
		return nil
	}

	var found []Found
	for _, c := range containers {
		id := c.Labels[docker.LabelResource]
		if id == "" || (keep != nil && !keep(id)) {
			continue
		}
		fn := &Function{
			ResourceID: id,
			Name:       c.Labels[docker.LabelFunction],
			Image:      c.Image,
			Container:  c.Name,
			// RIE always listens on 8080 inside the container; the host
			// side is whatever FreePort found before the restart.
			Port: docker.HostPort(c, "8080/tcp"),
			Dir:  e.CodeDir(id),
		}
		e.Put(fn)
		found = append(found, Found{
			ResourceID: id, Name: fn.Name, Container: c.Name,
			Port: fn.Port, Running: c.Running,
		})
	}
	return found
}
