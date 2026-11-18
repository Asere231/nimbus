package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Asere231/nimbus/internal/engines/docker"
	"github.com/Asere231/nimbus/internal/engines/funcs"
	"github.com/Asere231/nimbus/internal/engines/run"
)

// Azure Functions, which until Milestone 20 Azure could not do.
//
// AWS Lambda was comparatively cheap because every
// `public.ecr.aws/lambda/<runtime>` image ships the Runtime Interface
// Emulator, the runtime client and the SDK inside it, so `funcs` mounts a
// handler into a container that already knows how to run it. Microsoft
// publishes a host image too -- the roadmap claimed for several milestones
// that there was "no Azure equivalent to lean on", and that was wrong --
// but it expects a DEPLOY rather than a mount, and that difference is the
// whole of this file's design.
//
// Three things were measured before any of this was written:
//
//  1. The host runs offline. It binds :80 and logs "Job host started" with
//     no network, and the only unhealthy component is AzureWebJobsStorage,
//     which an HTTP trigger does not need.
//  2. A BIND MOUNT makes it find nothing. With the code mounted at
//     /home/site/wwwroot it reported "0 functions found (Custom)" for BOTH
//     programming models -- the v2 decorators with and without
//     EnableWorkerIndexing, and the classic v1 function.json -- with
//     nothing in the logs saying why. The identical tree baked in with a
//     COPY reported "1 functions loaded" and answered. So the image is
//     built per deployment.
//  3. The image ships the host and the worker but NOT `azure-functions`,
//     so dependencies have to be vendored. Real Azure installs
//     requirements.txt at deploy time with a remote build, which needs the
//     network.
//
// The build stays offline: the base image is pulled through the ordinary
// path and a COPY touches nothing.

const (
	typeFunctionApp = "Microsoft.Web/sites"
	typeServicePlan = "Microsoft.Web/serverfarms"
	typeSiteConfig  = "Microsoft.Web/sites/config"
	typeSitePublish = "Microsoft.Web/sites/basicPublishingCredentialsPolicies"
)

// FunctionsSuffix is the DNS suffix a function app answers under. Real
// Azure uses azurewebsites.net, so this is the same shape with a name that
// cannot leave the machine. One label of app name goes in front of it,
// because that is all a wildcard certificate covers.
const FunctionsSuffix = "azurewebsites.nimbus.localhost"

// DefaultFunctionAppPortBase is the first host port tried, above the range
// container apps use so the two never race for one.
const DefaultFunctionAppPortBase = 9700

// pythonFunctionImage is the host image a Python function app runs in.
//
// Python only, for now: each runtime is its own image of about a gigabyte
// and this is the one that was measured end to end. The table is a map so
// Node is a small follow-up rather than a redesign.
const pythonFunctionImage = "mcr.microsoft.com/azure-functions/python:4-python3.11"

// functionImages maps a runtime stack to its host image.
//
// Keyed by linuxFxVersion, LOWER-CASED, which is where the stack really
// lives: azurerm's `application_stack { python_version = "3.11" }` becomes
// `linuxFxVersion: "PYTHON|3.11"` in the request body -- measured, and a
// better signal than FUNCTIONS_WORKER_RUNTIME, which says "python" with no
// version at all.
var functionImages = map[string]string{
	"python|3.11": pythonFunctionImage,
}

// linuxFxVersion is the stack a site asks for, lower-cased.
func linuxFxVersion(body map[string]any) string {
	props, _ := body["properties"].(map[string]any)
	cfg, _ := props["siteConfig"].(map[string]any)
	return strings.ToLower(str(cfg["linuxFxVersion"]))
}

// functionImageFor picks the host image, or names what IS available.
//
// A stack Nimbus has no image for is REFUSED rather than run on the
// nearest one: a function app silently running on a different Python minor
// would fail at import time for reasons nothing in the apply mentions.
func functionImageFor(body map[string]any) (string, error) {
	stack := linuxFxVersion(body)
	if stack == "" {
		// No application_stack at all. The worker runtime is the only
		// other signal, and it carries no version, so the one measured
		// image is the honest default.
		if rt := workerRuntime(body); rt == "" || rt == "python" {
			return pythonFunctionImage, nil
		}
		return "", fmt.Errorf("the %q worker runtime has no Nimbus host image", workerRuntime(body))
	}
	if image := functionImages[stack]; image != "" {
		return image, nil
	}
	supported := make([]string, 0, len(functionImages))
	for k := range functionImages {
		supported = append(supported, strings.ToUpper(k))
	}
	sort.Strings(supported)
	return "", fmt.Errorf(
		"Nimbus has no Functions host image for the %q stack. It runs %s; each runtime and "+
			"version is its own image of about a gigabyte, so they are added one at a time "+
			"and the first is the one measured end to end",
		strings.ToUpper(stack), strings.Join(supported, ", "))
}

// wwwroot is where the Functions host looks for a deployment. Not
// configurable: the image sets AzureWebJobsScriptRoot to it and its
// PYTHONPATH underneath it.
const wwwroot = "/home/site/wwwroot"

func (s *Server) functionsSuffix() string { return withPort(FunctionsSuffix, s.cfg.PublicURL) }

// ScmSuffix is where the deployment (Kudu) site lives.
//
// Real Azure gives every site a SECOND hostname, {app}.scm.azurewebsites.net,
// and that is where a zip deploy goes. azurerm finds it by looking through
// the site's hostNameSslStates for the entry whose hostType is
// "Repository"; without one it refuses before sending anything:
//
//	could not determine SCM Site name for Site App Service ... for Zip Deployment
//
// Note it is TWO labels in front of the base suffix, so it needs a
// wildcard SAN of its own -- "*.azurewebsites.nimbus.localhost" does not
// cover "fn.scm.azurewebsites.nimbus.localhost". The same rule that gives
// the storage data plane one SAN per service level.
const ScmSuffix = "scm." + FunctionsSuffix

func (s *Server) scmSuffix() string { return withPort(ScmSuffix, s.cfg.PublicURL) }

// scmHostname is the deployment site's name for one app.
func (s *Server) scmHostname(name string) string {
	return strings.ToLower(name) + "." + s.scmSuffix()
}

// functionAppHostname is what defaultHostName reports and what a client
// connects to.
func (s *Server) functionAppHostname(name string) string {
	return strings.ToLower(name) + "." + s.functionsSuffix()
}

// --- the deploy directory -------------------------------------------------

// functionAppDir is where one app's deployment is unpacked and built from.
//
// One directory per resource, named through the same derivation as
// everything else so a restart finds the same place. It is the docker
// build CONTEXT, which is why nothing else may live in it.
func (s *Server) functionAppDir(resourceID string) string {
	return filepath.Join(s.cfg.DataDir, "functions",
		docker.SanitiseName(docker.Name("app", resourceID, "")))
}

// writeFunctionDockerfile puts the one-line build beside the code.
//
// Written with explicit LF endings on every platform. The file is
// GENERATED, so `.gitattributes` -- which pins the repository's tracked
// files to LF, and exists because a CRLF checkout made gofmt report all
// 219 Go files -- does not protect it. Docker's parser has historically
// mishandled CRLF on continuation lines, and a build that fails only on a
// Windows machine is the worst kind of difference to debug.
//
// The paths in it are CONTAINER paths and stay forward-slashed: they
// address a Linux image and have nothing to do with the host's separator.
func writeFunctionDockerfile(dir, baseImage string) error {
	// The COPY is the entire reason this file exists: the host finds no
	// functions in a bind mount and finds them in a baked-in tree.
	lines := []string{
		"# Generated by Nimbus. A bind mount makes the Functions host report",
		"# \"0 functions found\"; a COPY makes it load them. Measured both ways.",
		"FROM " + baseImage,
		"ENV AzureFunctionsJobHost__Logging__Console__IsEnabled=true",
		"COPY . " + wwwroot,
		"",
	}
	return os.WriteFile(filepath.Join(dir, "Dockerfile"),
		[]byte(strings.Join(lines, "\n")), 0o644)
}

// checkVendoredDependencies refuses a deployment that cannot possibly run.
//
// The base image ships the host and the Python worker but not
// `azure-functions`, so `import azure.functions` fails outright and EVERY
// invocation of such an app would fail on the import. Refusing at deploy
// time rather than warning is deliberate: the apply is where the person is
// looking, and an import error at the first invocation names neither the
// deployment nor the fix.
//
// The check is for the `azure` package specifically rather than for the
// directory, because an empty .python_packages is the same outcome.
func checkVendoredDependencies(dir string) error {
	site := filepath.Join(dir, ".python_packages", "lib", "site-packages")
	if _, err := os.Stat(filepath.Join(site, "azure")); err == nil {
		return nil
	}
	return fmt.Errorf(
		"this function app has no vendored dependencies, so every invocation would fail on "+
			"`import azure.functions`.\n\n"+
			"The Functions host image ships the host and the Python worker but NOT the "+
			"azure-functions package, and real Azure installs requirements.txt for you with a "+
			"remote build -- which needs the network, and Nimbus is offline. Vendor them "+
			"instead, into the directory the image's own PYTHONPATH already points at:\n\n"+
			"    pip install --target .python_packages/lib/site-packages -r requirements.txt\n\n"+
			"then deploy the zip again. (Looked for %s.)",
		filepath.ToSlash(filepath.Join(".python_packages", "lib", "site-packages", "azure")))
}

// unpackFunctionApp replaces a deploy directory with the contents of a zip.
//
// REPLACES: the directory is emptied first, so a function removed from the
// app stops running. Leaving the old tree in place would be the same shape
// as a sidecar dropped from a template -- the deploy looks applied and the
// old thing is still there.
//
// The unpacking is the Lambda engine's, reused rather than rewritten,
// because it already judges a zip entry as a NAME: `../../etc/passwd` is a
// legal entry that filepath.Join would happily write outside the
// directory, and the two hosts disagree in both directions --
// filepath.IsAbs is false for "/absolute.py" on Windows while "C:/x" is an
// ordinary name on Unix. Judging the raw, slash-separated name refuses the
// same archive identically on every machine.
func unpackFunctionApp(archive []byte, dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return funcs.Unpack(archive, dir)
}

// --- the ARM resource -----------------------------------------------------

// isFunctionApp reports whether a Microsoft.Web/sites body is a FUNCTION
// app rather than a web app.
//
// `kind` is how Azure distinguishes them, and azurerm sends
// "functionapp,linux" for azurerm_linux_function_app. A web app is a
// different product that Nimbus does not run, so it keeps the generic
// round-trip behaviour and is not started.
func isFunctionApp(body map[string]any) bool {
	return strings.Contains(strings.ToLower(str(body["kind"])), "functionapp")
}

// workerRuntime reads FUNCTIONS_WORKER_RUNTIME out of the app settings.
//
// azurerm sends the app settings as a LIST of {name, value} under
// properties.siteConfig.appSettings, the same shape as a container's
// environment and GCE's metadata items.
func workerRuntime(body map[string]any) string {
	props, _ := body["properties"].(map[string]any)
	cfg, _ := props["siteConfig"].(map[string]any)
	for _, raw := range toList(cfg["appSettings"]) {
		e, _ := raw.(map[string]any)
		if eq(str(e["name"]), "FUNCTIONS_WORKER_RUNTIME") {
			return strings.ToLower(str(e["value"]))
		}
	}
	return ""
}

func toList(v any) []any {
	out, _ := v.([]any)
	return out
}

func registerFunctionAppHooks(s *Server) {
	// A service plan's create must answer 202, not 201.
	//
	// Measured: azurerm refused the 201 outright with
	//
	//	performing CreateOrUpdate: unexpected status 201 (201 Created)
	//
	// which is the same generated-long-running-client trap storage
	// accounts hit, and the reason Hook.CreateStatus exists. Nothing else
	// about a plan needs behaviour -- azurerm reads nothing back that the
	// spec table does not already fill.
	s.registerHook(typeServicePlan, Hook{CreateStatus: 202})

	// A site's config children answer 200, not 201.
	//
	// azurerm writes several of them after creating the app --
	// `config/azurestorageaccounts` is the first, and it refused the 201
	// with "setting Storage Accounts for Linux App Service: unexpected
	// status 201". A config PUT is an UPDATE of a sub-resource that comes
	// into existence with its parent, which is why ARM answers 200 and
	// why the generated client accepts only that.
	s.registerHook(typeSiteConfig, Hook{
		CreateStatus: 200,
		BeforePut:    beforeSiteConfigPut,
	})

	s.registerHook(typeFunctionApp, Hook{
		Backing: "real",
		// 202, not 201, for the same reason as the plan above -- measured
		// the same way, with azurerm refusing the 201 and quoting the
		// whole body back.
		CreateStatus: 202,
		BeforePut:    beforeFunctionAppPut,
		// The create is long-running for real: it builds an image and
		// waits for the host to answer. azurerm polls the RESOURCE rather
		// than an operation for Container Apps, and the same is expected
		// here -- without Provision the apply would finish while the
		// build was still going and a curl would find nothing.
		Provision:   provisionFunctionApp,
		AfterDelete: deprovisionFunctionApp,
	})
}

func beforeFunctionAppPut(s *Server, t target, body, existing map[string]any) *armError {
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		body["properties"] = props
	}

	// The hostname is filled on the CREATE, not when the app becomes
	// ready. azurerm writes state from a read taken while the resource is
	// still provisioning, so a late hostname would be empty in state for
	// ever -- the GKE endpoint lesson, which generalises to "a derived
	// value also has to exist by the time the client first reads it".
	host := s.functionAppHostname(t.Name)
	scm := s.scmHostname(t.Name)
	props["defaultHostName"] = host
	props["hostNames"] = []any{host, scm}
	props["enabledHostNames"] = []any{host, scm}
	// azurerm finds the deployment site by looking for the entry whose
	// hostType is "Repository", and refuses a zip deploy without one.
	props["hostNameSslStates"] = []any{
		map[string]any{
			"name": host, "hostType": "Standard", "sslState": "Disabled",
		},
		map[string]any{
			"name": scm, "hostType": "Repository", "sslState": "Disabled",
		},
	}
	props["state"] = "Running"
	props["availabilityState"] = "Normal"
	// Derived from the id rather than generated, like every other value a
	// client compares between reads.
	props["outboundIpAddresses"] = "127.0.0.1"
	props["possibleOutboundIpAddresses"] = "127.0.0.1"
	setDefault(props, "httpsOnly", false)
	setDefault(props, "clientAffinityEnabled", false)

	if !isFunctionApp(body) {
		// A web app is a different product. It round-trips and nothing
		// runs, which is honest rather than half-working.
		return nil
	}
	// Refused at the PUT rather than at provisioning time, so the apply
	// fails with the reason instead of a resource that never starts.
	if _, err := functionImageFor(body); err != nil {
		return &armError{400, "UnsupportedRuntime", err.Error()}
	}
	return nil
}

// provisionFunctionApp builds the app's image and starts its host.
//
// A newly created app has no code yet -- azurerm creates the site and then
// deploys a zip to it -- so the host starts with an empty wwwroot and
// serves 404s, which is exactly what real Azure does for a site nobody has
// deployed to. The zip deploy rebuilds and restarts it.
func provisionFunctionApp(s *Server, t target, body map[string]any) error {
	if !isFunctionApp(body) {
		return nil
	}
	image, err := functionImageFor(body)
	if err != nil {
		return err
	}

	dir := s.functionAppDir(t.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A host.json is what makes the host treat the directory as a
	// deployment at all; without one it logs "the host.json file is
	// missing from the most recent deployment". An app with no code yet
	// gets a minimal one so it starts and 404s rather than failing.
	if _, err := os.Stat(filepath.Join(dir, "host.json")); err != nil {
		if err := os.WriteFile(filepath.Join(dir, "host.json"),
			[]byte("{\n  \"version\": \"2.0\"\n}\n"), 0o644); err != nil {
			return err
		}
	}
	if err := writeFunctionDockerfile(dir, image); err != nil {
		return err
	}

	svc := &run.Service{
		ResourceID: t.ID,
		Name:       t.Name,
		// The image is built, not pulled: it exists only here.
		Image:     docker.ImageTag("nimbus-fnapp", t.ID, t.Name),
		BuildDir:  dir,
		BuildBase: image,
		// The host listens on 80 inside the container, which is what
		// ASPNETCORE_URLS in the image says. Not 8080.
		ContainerPort: 80,
		Env:           functionAppEnv(body),
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
	defer cancel()
	return s.funcapps.Start(ctx, svc)
}

// functionAppEnv is the app settings as container environment variables.
//
// AzureWebJobsStorage is deliberately NOT invented. An HTTP-triggered
// function does not need it -- the host reports that one component
// unhealthy and serves perfectly -- and pointing it at Nimbus's own
// storage data plane is a real possibility that has not been measured, so
// claiming it works would be a guess.
func functionAppEnv(body map[string]any) map[string]string {
	out := map[string]string{}
	props, _ := body["properties"].(map[string]any)
	cfg, _ := props["siteConfig"].(map[string]any)
	for _, raw := range toList(cfg["appSettings"]) {
		e, _ := raw.(map[string]any)
		if name := str(e["name"]); name != "" {
			out[name] = str(e["value"])
		}
	}
	// The v2 Python model is indexed by the worker rather than by the
	// host, and the host only asks it to when this is set.
	if _, ok := out["AzureWebJobsFeatureFlags"]; !ok {
		out["AzureWebJobsFeatureFlags"] = "EnableWorkerIndexing"
	}
	return out
}

func deprovisionFunctionApp(s *Server, t target, body map[string]any) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
	defer cancel()

	if svc := s.funcapps.Get(t.ID); svc != nil {
		// Stop takes the container AND the image it built -- a built
		// image is this resource's own and would otherwise accumulate
		// one per app at about a gigabyte each.
		s.funcapps.Stop(ctx, svc)
		s.funcapps.Forget(t.ID)
	} else {
		// Nothing registered: a restart that did not adopt it, or a
		// create that failed. The image may still exist, and its tag is
		// derived from the id, so it can be removed without a registry.
		_ = s.funcapps.Docker().RemoveImage(ctx, docker.ImageTag("nimbus-fnapp", t.ID, t.Name))
	}
	// The deploy directory is this resource's own too.
	_ = os.RemoveAll(s.functionAppDir(t.ID))
}

// --- serving ---------------------------------------------------------------

// serveFunctionApp proxies a request through to the app's host.
func (s *Server) serveFunctionApp(w http.ResponseWriter, r *http.Request, name string) string {
	svc := s.functionAppByName(name)
	if svc == nil {
		writeError(w, 404, "FunctionAppNotFound",
			"No function app named %q is running. Create one, or check `docker ps` if it "+
				"was expected to be up.", name)
		return "function app " + name + ": not running"
	}
	if err := s.funcapps.Proxy(w, r, svc, ""); err != nil {
		return "function app " + name + ": " + err.Error()
	}
	return ""
}

func (s *Server) functionAppByName(name string) *run.Service {
	for _, svc := range s.funcapps.All() {
		if eq(svc.Name, name) {
			return svc
		}
	}
	return nil
}

// beforeSiteConfigPut takes the provisioningState back off a config child.
//
// ARM reports no provisioningState on a site's config sub-resources at
// all, and injecting one does more than add a field nobody asked for: a
// config child's `properties` is often a DICTIONARY -- a map of storage
// account names to their settings for `azurestorageaccounts`, of names to
// values for `appsettings` -- so an extra string key makes the whole
// document fail to unmarshal in the client:
//
//	unmarshaling response body: json: cannot unmarshal string into Go
//	struct field AzureStoragePropertyDictionaryResource.properties of
//	type webapps.AzureStorageInfoValue
//
// Measured against azurerm 4.81.0. The generic write fills
// properties.provisioningState for every type, which is right for a
// resource and wrong for a dictionary, and hooks run afterwards precisely
// so one can say so.
func beforeSiteConfigPut(s *Server, t target, body, existing map[string]any) *armError {
	if props, ok := body["properties"].(map[string]any); ok {
		delete(props, "provisioningState")
	}
	return nil
}

// --- a site's config sub-resources ----------------------------------------

// handleSiteConfig answers a GET on .../sites/{name}/config/{what}.
//
// These are VIEWS on the site rather than children anybody creates.
// azurerm sends the app settings INSIDE the site body and then reads them
// back from a path of their own; looking that path up in the store 404s,
// and the 404 fails the apply:
//
//	reading App Settings for Linux App Service: unexpected status 404
//	(404 Not Found) ... config/appSettings was not found
//
// Measured against azurerm 4.81.0. This is the S3 bucket sub-resource
// lesson in Azure's dialect -- a dozen separate documents a client reads
// straight after a create, each of which has to answer something it
// tolerates rather than an error.
//
// The shapes differ per name and the difference matters: `appsettings` and
// `connectionstrings` are DICTIONARIES under `properties`, while `web` is
// the site's own siteConfig object. An unknown one answers an empty
// `properties` object, which is what ARM reports for a configuration
// nobody has set and the only answer every client tolerates.
func (s *Server) handleSiteConfig(w http.ResponseWriter, t target) string {
	// The publishing-credential policies are a different family with the
	// same problem: azurerm reads them straight after the create and a
	// 404 fails the apply with "retrieving state of FTP Basic Auth for
	// App Service". Real ARM reports a policy for a site nobody has
	// configured, with basic auth allowed.
	if eq(t.Type, typeSitePublish) {
		writeJSON(w, 200, map[string]any{
			"id": t.ID, "name": t.Name, "type": t.Type,
			"properties": map[string]any{"allow": true},
		})
		return ""
	}
	out, ok := s.siteConfigView(t.ParentID, t.Name)
	if !ok {
		writeError(w, 404, "ResourceNotFound",
			"The site %q was not found, so it has no configuration.", t.ParentID)
		return "site config: no site " + t.ParentID
	}
	out["id"] = t.ID
	out["name"] = t.Name
	out["type"] = t.Type
	writeJSON(w, 200, out)
	return ""
}

// siteConfigAction answers the POST form of the same read.
//
// azurerm reads the app settings with `POST .../config/appSettings/list`
// rather than a GET, because they are SECRETS -- the same convention as a
// storage account's listKeys and a container app's listSecrets. It has to
// run before handleAction's existence check, since a config view is not a
// stored resource and that check would 404 it.
func (s *Server) siteConfigAction(t target) (map[string]any, bool) {
	// The POST verb is APPENDED to the type -- parseTarget builds
	// "Microsoft.Web/sites/config/list" for this path, which CLAUDE.md
	// records as how an ARM action's type is formed and which comparing
	// against the bare type silently misses.
	if !eq(t.Action, "list") || !eq(t.Type, typeSiteConfig+"/list") {
		return nil, false
	}
	// A COLLECTION target carries only an ID and an Action -- parseTarget
	// fills Name and ParentID for a resource and leaves them empty here --
	// so both have to come out of the id. Reading t.ParentID instead was
	// the first version of this and it silently looked up the empty
	// string, fell through to the existence check, and 404'd exactly as
	// before the handler existed.
	siteID, what, ok := splitSiteConfigID(t.ID)
	if !ok {
		return nil, false
	}
	out, found := s.siteConfigView(siteID, what)
	if !found {
		return nil, false
	}
	out["id"] = t.ID
	out["name"] = what
	out["type"] = t.Type
	return out, true
}

// splitSiteConfigID cuts ".../sites/{name}/config/{what}" in two.
//
// Case-insensitively, because ARM resolves ids that way and a client may
// send "/config/appSettings" where the stored site has any casing at all.
func splitSiteConfigID(id string) (siteID, what string, ok bool) {
	const sep = "/config/"
	i := strings.LastIndex(strings.ToLower(id), sep)
	if i < 0 {
		return "", "", false
	}
	return id[:i], id[i+len(sep):], true
}

// siteConfigView builds one configuration document from the site itself.
//
// The shapes differ per name and the difference matters: `appsettings` and
// `connectionstrings` are DICTIONARIES under `properties`, while `web` is
// the site's own siteConfig object. Azure's two spellings of one thing
// show up here -- a LIST of {name, value} goes IN with the site and a MAP
// comes back out of this path.
//
// An unknown name answers an empty `properties` object, which is what ARM
// reports for a configuration nobody has set and the only answer every
// client tolerates. This is the S3 bucket sub-resource lesson in Azure's
// dialect: a dozen documents a client reads straight after a create, each
// of which must answer something rather than an error.
func (s *Server) siteConfigView(siteID, what string) (map[string]any, bool) {
	site, err := s.store.Get(siteID)
	if err != nil {
		return nil, false
	}
	var body map[string]any
	if json.Unmarshal(site.Body, &body) != nil {
		return nil, false
	}
	props, _ := body["properties"].(map[string]any)
	cfg, _ := props["siteConfig"].(map[string]any)

	out := map[string]any{}
	switch strings.ToLower(what) {
	case "appsettings":
		settings := map[string]any{}
		for _, raw := range toList(cfg["appSettings"]) {
			e, _ := raw.(map[string]any)
			if name := str(e["name"]); name != "" {
				settings[name] = str(e["value"])
			}
		}
		out["properties"] = settings
	case "connectionstrings":
		out["properties"] = map[string]any{}
	case "web":
		if cfg == nil {
			cfg = map[string]any{}
		}
		out["properties"] = cfg
	default:
		// Whatever a client PUT here, if it did; otherwise empty.
		if stored, err := s.store.Get(siteID + "/config/" + what); err == nil {
			var sb map[string]any
			if json.Unmarshal(stored.Body, &sb) == nil {
				return sb, true
			}
		}
		out["properties"] = map[string]any{}
	}
	return out, true
}

// --- the deployment (SCM / Kudu) site -------------------------------------

// serveScm answers the deployment site for one app.
//
// Real Azure runs a second site alongside every App Service -- Kudu, at
// {app}.scm.azurewebsites.net -- and a zip deploy is a POST of the archive
// to its /api/zipdeploy. That is what azurerm's `zip_deploy_file` does,
// measured: it reads the SCM hostname out of hostNameSslStates and posts
// the bytes there.
//
// Only the one endpoint is implemented. Kudu's surface is enormous (a
// command runner, a file browser, a process explorer) and none of it is
// needed to deploy code, so everything else is refused by name rather
// than answered with something that looks like success.
func (s *Server) serveScm(w http.ResponseWriter, r *http.Request, app string) string {
	svc := s.functionAppByName(app)
	if svc == nil {
		writeError(w, 404, "FunctionAppNotFound",
			"No function app named %q is running, so it has no deployment site.", app)
		return "scm " + app + ": no such app"
	}

	path := strings.ToLower(strings.TrimSuffix(r.URL.Path, "/"))
	switch {
	case r.Method == http.MethodPost && (path == "/api/zipdeploy" || path == "/api/zipdeploy/"):
		return s.zipDeploy(w, r, svc)
	case r.Method == http.MethodGet && path == "/api/deployments":
		// A deployment history. azurerm does not need it, but `az` and the
		// portal ask, and an empty list is the truthful answer for a site
		// whose deployments Nimbus does not keep.
		writeJSON(w, 200, []any{})
		return ""
	}
	writeError(w, 501, "NotImplemented",
		"Nimbus's deployment site implements POST /api/zipdeploy and nothing else; "+
			"%s %s is refused rather than answered with a success it did not perform.",
		r.Method, r.URL.Path)
	return "scm " + app + ": unimplemented " + r.Method + " " + r.URL.Path
}

// zipDeploy unpacks an uploaded archive and rebuilds the app's image.
//
// The rebuild is the whole point: the Functions host finds nothing in a
// bind mount, so new code means a new image and a restarted container.
// That is slower than a mount would be and it is what makes the deploy
// actually work.
func (s *Server) zipDeploy(w http.ResponseWriter, r *http.Request, svc *run.Service) string {
	archive, err := io.ReadAll(io.LimitReader(r.Body, maxDeployZip))
	if err != nil {
		writeError(w, 400, "BadRequest", "reading the deployment archive: %v", err)
		return "zipdeploy: unreadable body"
	}
	if len(archive) == 0 {
		writeError(w, 400, "BadRequest", "the deployment archive is empty")
		return "zipdeploy: empty archive"
	}

	dir := s.functionAppDir(svc.ResourceID)
	if err := unpackFunctionApp(archive, dir); err != nil {
		writeError(w, 400, "BadRequest", "unpacking the deployment archive: %v", err)
		return "zipdeploy: " + err.Error()
	}
	// REFUSED here rather than at the first invocation. An app whose
	// dependencies are not vendored cannot serve a single call -- every
	// one fails on `import azure.functions` -- and the apply is where
	// somebody is looking.
	if err := checkVendoredDependencies(dir); err != nil {
		writeError(w, 400, "MissingDependencies", "%v", err)
		return "zipdeploy: no vendored dependencies"
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ClusterTimeout)
	defer cancel()
	// Start rebuilds the image and replaces the container, which is what
	// a deployment IS here.
	if err := s.funcapps.Start(ctx, svc); err != nil {
		writeError(w, 500, "DeploymentFailed", "%v", err)
		return "zipdeploy: " + err.Error()
	}
	w.WriteHeader(200)
	return "zipdeploy: " + svc.Name + " redeployed"
}

// maxDeployZip bounds an upload. A function app with vendored
// dependencies is a few megabytes; 256 MB is far more than any of them and
// small enough that a mistake cannot exhaust the machine.
const maxDeployZip = 256 << 20
