package vm

import (
	"context"

	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Finding instances again after Nimbus restarts.
//
// With KeepRunning set the containers outlive the process, so a fresh
// Nimbus has to adopt them rather than start a second set. Everything it
// needs is on the containers themselves: the resource id and the instance
// name are labels.
//
// An adopted instance does NOT get its metadata listener back here. The
// port would be a different one, and the environment variable telling the
// container where to look was fixed when the container started -- so the
// old address is what the instance still believes. The plugin re-serves
// metadata when it next starts or resumes the instance; until then the
// honest state is that metadata is unreachable, not that it moved.

// Found is one adopted instance.
type Found struct {
	ResourceID string
	Name       string
	Container  string
	Running    bool
}

// Reattach adopts every instance this engine's containers describe.
//
// keep decides which resource ids are still wanted; a container whose
// resource has been deleted is left alone rather than adopted, since
// something else is responsible for cleaning it up.
func (e *Engine) Reattach(ctx context.Context, keep func(resourceID string) bool) []Found {
	if err := e.docker.Available(ctx); err != nil {
		return nil
	}
	containers, err := e.docker.List(ctx, docker.LabelRole+"=instance")
	if err != nil {
		return nil
	}

	var found []Found
	for _, c := range containers {
		id := c.Labels[docker.LabelResource]
		// Another cloud's instances carry the same role label. They are
		// told apart by `keep`, not by the container name: a resource id is
		// already cloud-shaped -- an ARN, an ARM path, a self link -- so
		// the plugin recognising its own is both simpler and surer than
		// matching a prefix. Same division as k3s.Reattach.
		if id == "" || (keep != nil && !keep(id)) {
			continue
		}
		i := &Instance{
			ResourceID: id,
			Name:       c.Labels[docker.LabelInstance],
			Image:      c.Image,
			Container:  c.Name,
		}
		e.Put(i)
		found = append(found, Found{
			ResourceID: id, Name: i.Name, Container: c.Name, Running: c.Running,
		})
	}
	return found
}
