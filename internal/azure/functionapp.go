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
	"time"

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
	return filepath.Join(s.functionRoot(), docker.SanitiseName(docker.Name("app", resourceID, "")))
}

// functionRoot is where every app's deploy directory lives.
//
// Under the data directory when there is one, and under a per-process
// temporary directory when there is not -- which is the ORDINARY case,
// since DataDir is empty without --persist. The first version joined
// DataDir directly, so in memory-only mode the deploy directory came out
// RELATIVE ("functions/app-...") and `docker build` ran against whatever
// the process's working directory happened to be:
//
//	failed to read dockerfile: open Dockerfile: no such file or directory
//
// A path that depends on the caller's cwd is wrong everywhere; it is just
// invisible until something reads it from somewhere else. The temporary
// root matches what memory-only means: the code lives as long as the
// process does.
func (s *Server) functionRoot() string {
	if s.cfg.DataDir != "" {
		return filepath.Join(s.cfg.DataDir, "functions")
	}
	s.funcRootOnce.Do(func() {
		dir, err := os.MkdirTemp("", "nimbus-functions-")
		if err != nil {
			// Nowhere to put code is fatal to a deploy, not to the
			// server, so this falls back to the system temp directory
			// rather than panicking a request.
			dir = filepath.Join(os.TempDir(), "nimbus-functions")
			_ = os.MkdirAll(dir, 0o755)
		}
		s.funcRoot = dir
	})
	return s.funcRoot
}

// prepareDeployDir makes a directory buildable: a host.json so the host
// treats it as a deployment, and Nimbus's own Dockerfile.
//
// Called after EVERY unpack, not once at create. unpackFunctionApp
// replaces the directory -- deliberately, so a function removed from the
// app stops running -- which also deletes the Dockerfile the create
// wrote, and the build then failed with "open Dockerfile: no such file or
// directory" on a deploy that had otherwise worked.
//
// Writing it afterwards settles a second question too: a zip carrying its
// own Dockerfile cannot hijack the build, because Nimbus's is written
// last. A host.json the user deployed IS kept, since it carries their
// extension bundle and logging settings.
func prepareDeployDir(dir, baseImage string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "host.json")); err != nil {
		// An app with no code yet still needs one, or the host logs "the
		// host.json file is missing from the most recent deployment" and
		// finds nothing. With one it starts and 404s, which is what real
		// Azure does for a site nobody has deployed to.
		if err := os.WriteFile(filepath.Join(dir, "host.json"),
			[]byte(defaultHostJSON), 0o644); err != nil {
			return err
		}
	}
	return writeFunctionDockerfile(dir, baseImage)
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
	s.registerHook(typeServicePlan, Hook{
		CreateStatus: 202,
		// And the delete is SYNCHRONOUS: a 202 fails with "unexpected
		// status 202 (202 Accepted) received with no body", the same way
		// a storage account's and a user-assigned identity's do.
		DeleteStatus: 200,
		BeforePut:    beforeServicePlanPut,
	})

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
		// No spec defaults: the generated table invents siteConfig
		// fields azurerm defaults differently, and a nullable object as
		// an explicit null, both of which are permanent diffs. See the
		// field's own comment in arm.go.
		SkipSpecDefaults: true,
		// 202, not 201, for the same reason as the plan above -- measured
		// the same way, with azurerm refusing the 201 and quoting the
		// whole body back.
		CreateStatus: 202,
		// Synchronous delete, for the same reason: the generated client
		// refuses a 202 with no body. Measured on the destroy, which
		// otherwise failed AFTER the container and the image had already
		// been cleaned up -- so the teardown was right and only the
		// status was wrong, which is the most confusing combination.
		DeleteStatus: 200,
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
	if err := prepareDeployDir(dir, image); err != nil {
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
		// Either the site does not exist or this configuration was never
		// set. Both are a 404 to ARM, and the note says which.
		why := "not configured"
		if !s.store.Exists(t.ParentID) {
			why = "no such site"
		}
		writeError(w, 404, "ResourceNotFound",
			"The Resource '%s' was not found.", t.ID)
		return "site config " + t.Name + ": " + why
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
	case "authsettings":
		// Easy Auth, off. azurerm reads it on every read of the app.
		out["properties"] = map[string]any{"enabled": false}
	case "authsettingsv2":
		// The v2 shape is NESTED and the provider walks into it, so an
		// empty properties is not enough -- the platform block has to be
		// there for `enabled` to be read off it.
		out["properties"] = map[string]any{
			"platform":          map[string]any{"enabled": false},
			"globalValidation":  map[string]any{},
			"identityProviders": map[string]any{},
			"login":             map[string]any{},
			"httpSettings":      map[string]any{},
		}
	case "logs":
		// Diagnostic logging, all off. Nested for the same reason.
		out["properties"] = map[string]any{
			"applicationLogs": map[string]any{
				"fileSystem": map[string]any{"level": "Off"},
			},
			"httpLogs":              map[string]any{},
			"detailedErrorMessages": map[string]any{"enabled": false},
			"failedRequestsTracing": map[string]any{"enabled": false},
		}
	case "metadata":
		// A dictionary, like appsettings, and empty.
		out["properties"] = map[string]any{}
	case "slotconfignames":
		// Which app settings and connection strings stick to a slot
		// rather than travelling with a swap. azurerm reads this on
		// EVERY read of a function app, and a 404 fails the apply --
		// found by making unset configurations 404 and watching this one
		// break. An app with no slots has no sticky names, so the lists
		// are simply absent.
		out["properties"] = map[string]any{}
	case "publishingcredentials":
		// azurerm asks for these BEFORE a zip deploy and dereferences the
		// answer without checking it. The generic "unknown action returns
		// an empty object" made the provider PANIC:
		//
		//	panic: runtime error: invalid memory address or nil pointer
		//	dereference
		//	  helpers.GetCredentialsAndPublish(...) publish_app.go:43
		//
		// which is the sharpest version yet of this project's recurring
		// lesson -- it is not enough for an unimplemented thing to fail
		// politely, because a client may be reading that failure as an
		// answer. An empty object is not a safe default here.
		//
		// Nimbus verifies no credentials, so these are a shape rather
		// than a secret. They are derived from the site name so two
		// reads agree, like every other value a client might compare.
		name := siteID[strings.LastIndex(siteID, "/")+1:]
		out["properties"] = map[string]any{
			"name":               name,
			"publishingUserName": "$" + name,
			"publishingPassword": "nimbus",
			"scmUri":             "https://$" + name + ":nimbus@" + s.scmHostname(name),
		}
	case "web":
		if cfg == nil {
			cfg = map[string]any{}
		}
		out["properties"] = cfg
	default:
		// Whatever a client PUT here, if it did.
		if stored, err := s.store.Get(siteID + "/config/" + what); err == nil {
			var sb map[string]any
			if json.Unmarshal(stored.Body, &sb) == nil {
				return sb, true
			}
		}
		// And otherwise NOT an empty object.
		//
		// An empty `properties` is not "nothing is configured" to a
		// client -- it is "this configuration exists and is blank". For
		// `config/backup` that produced a permanent diff: azurerm
		// flattened the empty document into a `backup { enabled = false }`
		// block, and every plan after a clean apply wanted to remove a
		// block the configuration never had.
		//
		// ARM answers 404 for an optional configuration nobody has set,
		// so that is what this answers. Exactly S3's rule that each
		// sub-resource needs its OWN unset answer, and that the generous
		// one is not always the safe one.
		return nil, false
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
	// Two spellings of a zip deploy, and azurerm uses the SECOND.
	//
	// /api/zipdeploy is the one every tutorial shows and the one `az
	// functionapp deployment source config-zip` uses. azurerm 4.81.0
	// publishes through /api/publish?type=zip -- Kudu's newer "one
	// deploy" endpoint -- which was found the hard way: the handler
	// implemented only zipdeploy, so the publish got a 501.
	case r.Method == http.MethodPost && (path == "/api/zipdeploy" || path == "/api/publish"):
		return s.zipDeploy(w, r, svc)

	// Deployment history. The publish is synchronous here -- the image is
	// rebuilt before the response -- so the latest deployment is always a
	// finished, successful one. Kudu's status 4 means success, and
	// `complete` is what a poller waits for; without them a client would
	// wait for a deployment that already happened.
	// Deployment history, under BOTH spellings.
	//
	// azurerm's readiness check before a publish is `GET /deployments`
	// with no /api prefix -- measured by logging what the deployment site
	// refused, after the documented `/api/deployments` turned out not to
	// be the one it asks for. Answering only the /api form failed the
	// apply with "checking deployment service status: waiting for
	// deployment service to be ready: client error: 404 Not Found".
	case r.Method == http.MethodGet && (path == "/api/deployments" || path == "/deployments"):
		writeJSON(w, 200, []any{scmDeployment(svc.Name)})
		return ""
	case r.Method == http.MethodGet &&
		(strings.HasPrefix(path, "/api/deployments/") || strings.HasPrefix(path, "/deployments/")):
		writeJSON(w, 200, scmDeployment(svc.Name))
		return ""

	// Kudu's own identity, which some clients read before deploying.
	case r.Method == http.MethodGet && (path == "/api/scm/info" || path == "/api/environment"):
		writeJSON(w, 200, map[string]any{
			"type": "nimbus", "version": "1", "siteName": svc.Name,
		})
		return ""
	}

	// 404 and NOT 501.
	//
	// This matters more than it looks. azurerm reads a 501 from the
	// deployment site as "the service is not ready yet" and retries FOR
	// EVER -- measured: ten minutes of
	//
	//	[DEBUG] Deployment service came back with a 501 status code, retrying...
	//
	// thirty seconds apart, with the apply never finishing and nothing
	// saying which path was refused. A 404 is what Kudu answers for a
	// path it does not have, so the client either proceeds or fails
	// quickly. The same lesson as Cloud Storage's anywhereCaches 404,
	// except that here the polite failure does not merely mislead the
	// client -- it never terminates.
	writeError(w, 404, "NotFound",
		"Nimbus's deployment site implements the zip deploy (POST /api/zipdeploy or "+
			"POST /api/publish) and the deployment history, and nothing else. %s %s is not "+
			"one of them.", r.Method, r.URL.Path)
	return "scm " + app + ": no such endpoint " + r.Method + " " + r.URL.Path
}

// scmDeployment is a finished, successful deployment in Kudu's shape.
//
// status 4 is Kudu's "success" and `complete` is what a poller waits for.
// Both are true as soon as the response is written, because a deploy here
// rebuilds the image synchronously -- so there is never a deployment in
// progress to report.
func scmDeployment(name string) map[string]any {
	now := time.Now().UTC().Format(time.RFC3339)
	return map[string]any{
		"id":          "nimbus-" + name,
		"status":      4,
		"status_text": "",
		"complete":    true,
		"active":      true,
		"deployer":    "Nimbus",
		"message":     "deployed by Nimbus",
		"progress":    "",
		"start_time":  now,
		"end_time":    now,
	}
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
	// The unpack replaced the directory, which took the Dockerfile with
	// it. Writing it again is what makes the rebuild possible at all.
	if err := prepareDeployDir(dir, svc.BuildBase); err != nil {
		writeError(w, 500, "DeploymentFailed", "preparing the deploy directory: %v", err)
		return "zipdeploy: " + err.Error()
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

// defaultHostJSON is the minimal deployment manifest an app with no code
// of its own gets, so the Functions host treats its directory as a
// deployment and starts. A raw string, so no escape can be mangled.
const defaultHostJSON = `{
  "version": "2.0"
}
`

// beforeServicePlanPut fills what a real server farm reports.
//
// azurerm reads the plan back when UPDATING a function app, and a thin
// body made it fail with
//
//	retrieving Service Plan for App Service ...: <nil>
//
// -- an error whose text is the nil it got, naming nothing. Real Azure
// reports a `kind` and a complete `sku` (tier, size, family, capacity),
// all of which are derivable from the sku name, so they are filled rather
// than invented.
func beforeServicePlanPut(s *Server, t target, body, existing map[string]any) *armError {
	props, _ := body["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		body["properties"] = props
	}
	sku, _ := body["sku"].(map[string]any)
	if sku == nil {
		sku = map[string]any{}
		body["sku"] = sku
	}
	name := str(sku["name"])
	if name == "" {
		name = "Y1"
		sku["name"] = name
	}
	setDefault(sku, "tier", skuTier(name))
	setDefault(sku, "size", name)
	setDefault(sku, "family", name[:1])
	setDefault(sku, "capacity", 0)

	// A Linux plan sets reserved, and azurerm sends it. The kind follows
	// from it: Azure reports "functionapp" for a consumption plan and
	// "linux" for an ordinary Linux App Service plan.
	if body["kind"] == nil || str(body["kind"]) == "" {
		if eq(skuTier(name), "Dynamic") {
			body["kind"] = "functionapp"
		} else if reserved, _ := props["reserved"].(bool); reserved {
			body["kind"] = "linux"
		} else {
			body["kind"] = "app"
		}
	}
	return nil
}

// skuTier is the tier a plan's sku name belongs to.
//
// Only the families Nimbus can be asked about: Y is the consumption plan
// ("Dynamic"), EP is Elastic Premium, and everything else falls back to
// the letter's usual tier. Wrong for an exotic sku, and a tier is not
// something Nimbus acts on -- it is reported because azurerm reads it.
func skuTier(name string) string {
	switch {
	case strings.HasPrefix(name, "Y"):
		return "Dynamic"
	case strings.HasPrefix(name, "EP"):
		return "ElasticPremium"
	case strings.HasPrefix(name, "FC"):
		return "FlexConsumption"
	case strings.HasPrefix(name, "P"):
		return "PremiumV3"
	case strings.HasPrefix(name, "S"):
		return "Standard"
	case strings.HasPrefix(name, "B"):
		return "Basic"
	case strings.HasPrefix(name, "F"):
		return "Free"
	}
	return "Dynamic"
}
