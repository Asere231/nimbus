package azure

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Asere231/nimbus/internal/engines/k3s"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Container Apps: the first Azure type that runs your code.
//
// Until this, Azure was the only one of the three clouds that could not.
// AWS runs a Lambda in AWS's own base image, GCP runs a Cloud Run service
// in any image the daemon can pull, and Azure ran a Kubernetes cluster and
// a VM and nothing in between.
//
// It cost very little, because both halves already existed:
// `internal/engines/run` pulls an image, publishes a port, waits for it to
// answer and proxies through -- it is behind Cloud Run and knows about no
// cloud -- and `microsoft.app/containerapps` was already in the spec table
// with `ingress.fqdn` declared read-only, so the shape round-tripped and
// only the hook was missing.
//
// Three decisions worth more than the code:
//
//   - **The FQDN has ONE label for the app.** Real Azure is
//     `{app}.{envdomain}.{region}.azurecontainerapps.io`, but a TLS
//     wildcard matches exactly one label (Milestone 2 learned that the
//     hard way), so an `{app}.{env}.` shape would need a certificate per
//     environment and could not be issued ahead of time. The environment's
//     `defaultDomain` is therefore the shared suffix, and app names have
//     to be unique across environments -- which real Azure does not
//     require. docs/ROADMAP.md says so.
//   - **The suffix carries our port**, as `storage` and `keyVaultDns`
//     already do, because 443 needs root to bind.
//   - **Requests are PROXIED rather than sent to the container's port.**
//     A published host port changes every time the container starts, and
//     `fqdn` is a value azurerm puts in state and a person puts in a
//     terminal. Proxying also means every request to an app passes through
//     the activity feed, which is the point of the project -- a request
//     straight to a container port would be invisible. The Cloud Run
//     module makes the same argument at length.

const (
	typeManagedEnvironment = "Microsoft.App/managedEnvironments"
	typeContainerApp       = "Microsoft.App/containerApps"
)

// ContainerAppsSuffix is the DNS suffix container apps answer under. One
// label of app name goes in front of it.
const ContainerAppsSuffix = "containerapps.nimbus.localhost"

// DefaultContainerAppPortBase is the first host port tried for an app,
// above the range Cloud Run uses so the two clouds' containers never race
// for one.
const DefaultContainerAppPortBase = 9500

// containerAppsSuffix is the suffix with our port on it, which is what a
// client actually connects to.
func (s *Server) containerAppsSuffix() string {
	return withPort(ContainerAppsSuffix, s.cfg.PublicURL)
}

func registerContainerAppHooks(s *Server) {
	s.registerHook(typeManagedEnvironment, Hook{
		BeforePut: func(s *Server, t target, body, existing map[string]any) *armError {
			props := body["properties"].(map[string]any)
			// The shared domain every app in this environment answers
			// under. Reported because azurerm reads it, and constant
			// because a certificate has to cover it.
			props["defaultDomain"] = s.containerAppsSuffix()
			// A static IP is read back and must not move. Derived from
			// the id, like every other value a client compares.
			props["staticIp"] = "127.0.0.1"
			setDefault(props, "zoneRedundant", false)
			return nil
		},
		// Deleting an environment takes its apps with it, which the
		// store's cascade already does by ID prefix -- an app's ParentID
		// is its resource group, not its environment, so the apps are
		// stopped by their own Deprovision rather than by this.
	})

	s.registerHook(typeContainerApp, Hook{
		Backing:   "real",
		BeforePut: beforeContainerAppPut,
		// The create is long-running for real: it pulls an image. Without
		// this the apply finishes while the pull is still going and a
		// curl against the fqdn finds nothing -- the lesson the VM
		// learned, where `nimbus ssh` right after a successful apply
		// found no container.
		Provision:   provisionContainerApp,
		Deprovision: deprovisionContainerApp,
	})
}

// beforeContainerAppPut fills everything azurerm reads back.
//
// spec.go fills provisioningState, etags, GUIDs and empty arrays, and
// leaves a read-only STRING alone -- so every one of these would otherwise
// be absent, read as "" in state, and plan forever. All of them are
// derived from the resource id, never generated, because azurerm compares
// them between reads.
func beforeContainerAppPut(s *Server, t target, body, existing map[string]any) *armError {
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		body["properties"] = props
	}

	image := containerAppImage(props)
	if image == "" {
		return &armError{400, "InvalidContainerApp",
			"A container app needs properties.template.containers[0].image."}
	}

	// An app with no ingress block is not externally reachable in real
	// Azure and gets no fqdn, so neither the configuration object nor the
	// ingress one is created here. Inventing them would be the thing
	// TestSpecDefaultsDoNotInventNestedObjects exists to catch -- it
	// caught exactly this -- and a client would then see configuration it
	// never wrote and plan it away.
	if cfg, ok := props["configuration"].(map[string]any); ok {
		if ingress, ok := cfg["ingress"].(map[string]any); ok {
			ingress["fqdn"] = t.Name + "." + s.containerAppsSuffix()
		}
	}

	// A revision name in Azure is `{app}--{suffix}`, and the suffix moves
	// when the template changes. Nimbus derives it from the id alone, so
	// it is stable: nothing here runs more than one revision, and a
	// changing name would be a diff with nothing behind it.
	revision := t.Name + "--" + k3s.StableHex(strings.ToLower(t.ID)+"/revision", 4)
	props["latestRevisionName"] = revision
	props["latestReadyRevisionName"] = revision
	props["latestRevisionFqdn"] = revision + "." + s.containerAppsSuffix()
	props["runningStatus"] = "Running"
	props["eventStreamEndpoint"] = s.cfg.PublicURL + "/_nimbus/containerapps/" +
		strings.ToLower(t.Name) + "/eventstream"
	props["customDomainVerificationId"] = k3s.StableHex(strings.ToLower(t.ID)+"/customdomain", 16)
	props["outboundIpAddresses"] = []any{"127.0.0.1"}
	setDefault(props, "workloadProfileName", "Consumption")
	return nil
}

// containerAppImage is the image of the FIRST container in the template.
//
// Real Container Apps run every container in the template as a sidecar
// group. Nimbus runs the first, which is exactly the limit Cloud Run has
// here and is stated in the roadmap rather than hidden: a sidecar is
// stored and not started.
func containerAppImage(props map[string]any) string {
	template, _ := props["template"].(map[string]any)
	containers, _ := template["containers"].([]any)
	if len(containers) == 0 {
		return ""
	}
	first, _ := containers[0].(map[string]any)
	return str(first["image"])
}

// containerAppEnv reads the first container's environment. Azure's shape is
// a list of {name, value} rather than a map, like GCE's metadata items.
func containerAppEnv(props map[string]any) map[string]string {
	out := map[string]string{}
	template, _ := props["template"].(map[string]any)
	containers, _ := template["containers"].([]any)
	if len(containers) == 0 {
		return out
	}
	first, _ := containers[0].(map[string]any)
	entries, _ := first["env"].([]any)
	for _, raw := range entries {
		e, _ := raw.(map[string]any)
		if name := str(e["name"]); name != "" {
			out[name] = str(e["value"])
		}
	}
	return out
}

// containerAppPort is what the image listens on inside the container.
// Azure calls it targetPort; the engine defaults to 8080, which is Cloud
// Run's convention and wrong for most Azure examples, so an explicit
// targetPort matters here.
func containerAppPort(props map[string]any) int {
	cfg, _ := props["configuration"].(map[string]any)
	ingress, _ := cfg["ingress"].(map[string]any)
	if ingress == nil {
		return run.DefaultContainerPort
	}
	switch v := ingress["targetPort"].(type) {
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return run.DefaultContainerPort
}

func provisionContainerApp(s *Server, t target, body map[string]any) error {
	props, _ := body["properties"].(map[string]any)
	image := containerAppImage(props)
	if image == "" {
		return fmt.Errorf("the container app has no image")
	}

	svc := &run.Service{
		ResourceID:    t.ID,
		Name:          t.Name,
		Image:         image,
		Env:           containerAppEnv(props),
		ContainerPort: containerAppPort(props),
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
	defer cancel()
	if err := s.apps.Start(ctx, svc); err != nil {
		return err
	}
	return nil
}

func deprovisionContainerApp(s *Server, t target) {
	svc := s.apps.Get(t.ID)
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
	defer cancel()
	s.apps.Stop(ctx, svc)
	s.apps.Forget(t.ID)
}

// --- the data plane --------------------------------------------------------

// parseAppHost recognises an app's own hostname.
//
// Exactly ONE label before the suffix, because that is all a wildcard
// certificate can cover -- and because the plain ARM host must not be
// mistaken for an app, or every ARM call would be shadowed. The same
// shape as storage.ParseHost and keyvault.ParseHost, which run before
// this one.
func parseAppHost(host, suffix string) (name string, ok bool) {
	h := strings.ToLower(stripHostPort(host))
	sfx := strings.ToLower(stripHostPort(suffix))
	if !strings.HasSuffix(h, "."+sfx) {
		return "", false
	}
	name = strings.TrimSuffix(h, "."+sfx)
	if name == "" || strings.Contains(name, ".") {
		return "", false
	}
	return name, true
}

// serveContainerApp proxies a request through to the app's container.
func (s *Server) serveContainerApp(w http.ResponseWriter, r *http.Request, name string) string {
	svc := s.appByName(name)
	if svc == nil {
		// The two cases are different and the difference is the whole
		// answer: an app nobody created, or one whose container is not
		// running. Both are 404 to a browser; the note says which.
		writeError(w, 404, "ContainerAppNotFound",
			"No container app named %q is running. Create one, or check `docker ps` "+
				"if it was expected to be up.", name)
		return "container app " + name + ": not running"
	}
	if err := s.apps.Proxy(w, r, svc, ""); err != nil {
		return "container app " + name + ": " + err.Error()
	}
	return ""
}

// appByName finds a running app by its short name, which is what a
// hostname carries.
func (s *Server) appByName(name string) *run.Service {
	for _, svc := range s.apps.All() {
		if eq(svc.Name, name) {
			return svc
		}
	}
	return nil
}

// stripHostPort drops a ":port" from a host, leaving the name.
func stripHostPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}
