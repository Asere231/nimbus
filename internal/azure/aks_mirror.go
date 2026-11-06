package azure

import (
	"context"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/k3s"
)

// Pointing AKS clusters at emulated container registries.
//
// Writing the mirror is the engine's job (internal/engines/k3s/mirror.go);
// knowing which registries exist, and saying so in the activity feed when
// one cannot be reached, is Azure's. The split matters because EKS will
// want the same mirroring for ECR without inheriting ACR's bookkeeping.

// mirrorRegistries points one cluster at every registry that exists.
func (s *Server) mirrorRegistries(ctx context.Context, cl *k3s.Cluster) {
	for _, reg := range s.registries.all() {
		s.mirrorOne(ctx, cl, reg)
	}
}

// announceRegistryToClusters does the reverse for a new registry: nothing
// says ACR is created before AKS, so a cluster that already exists has to
// be told.
func (s *Server) announceRegistryToClusters(ctx context.Context, reg *registry) {
	for _, cl := range s.k3s.All() {
		s.mirrorOne(ctx, cl, reg)
	}
}

func (s *Server) mirrorOne(ctx context.Context, cl *k3s.Cluster, reg *registry) {
	for _, warning := range s.k3s.MirrorRegistry(ctx, cl, reg.LoginServer(), reg.Container) {
		s.bus.Publish(core.Event{
			Kind: "resource", Action: "update", Cloud: "azure",
			ResourceID: cl.ResourceID, ResourceType: typeManagedCluster, Name: cl.Name,
			Note: warning,
		})
	}
}
