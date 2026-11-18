package azure

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"encoding/json"
	"net/http"
	"net/http/httptest"

	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/engines/docker"
)

// Azure Functions: the deploy directory and the Dockerfile that makes it
// work, which is where all the measured knowledge lives.
//
// The host image reports ZERO functions when the code is bind-mounted and
// loads them when the identical tree is baked in with a COPY -- measured
// in both programming models, with and without worker indexing. So a
// function app is an image built per deployment, and these tests are about
// getting that image's inputs right.

// The Dockerfile is written with LF endings, on every platform.
//
// It is GENERATED, so `.gitattributes` -- which pins the repository's
// tracked files to LF and exists because a CRLF checkout made gofmt
// report all 219 Go files -- does not protect it. Docker's parser has
// historically mishandled CRLF on continuation lines, and a build that
// fails on a Windows machine and works everywhere else is the worst kind
// of difference to debug. Writing LF explicitly costs nothing.
func TestTheGeneratedDockerfileUsesLfEndings(t *testing.T) {
	dir := t.TempDir()
	if err := writeFunctionDockerfile(dir, pythonFunctionImage); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("\r")) {
		t.Errorf("the Dockerfile contains a carriage return:\n%q", raw)
	}
	text := string(raw)
	if !strings.Contains(text, "FROM "+pythonFunctionImage) {
		t.Errorf("the Dockerfile does not build from the Functions host image:\n%s", text)
	}
	// The COPY is the whole point: a bind mount makes the host find
	// nothing, and this is what makes it find something.
	if !strings.Contains(text, "COPY . /home/site/wwwroot") {
		t.Errorf("the Dockerfile does not copy the code into wwwroot:\n%s", text)
	}
}

// Container paths in the Dockerfile are forward-slashed even when Nimbus
// is running on Windows, because they are paths inside a Linux image and
// have nothing to do with the host's separator.
func TestTheDockerfileUsesContainerPaths(t *testing.T) {
	dir := t.TempDir()
	if err := writeFunctionDockerfile(dir, pythonFunctionImage); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if strings.Contains(string(raw), `\`) {
		t.Errorf("a backslash reached the Dockerfile; container paths are forward-slashed:\n%s", raw)
	}
}

// --- vendored dependencies -------------------------------------------------

// A deploy with no vendored dependencies is REFUSED, at the apply.
//
// The base image ships the Functions host and the Python worker but not
// `azure-functions`: `import azure.functions` fails outright. Real Azure
// installs requirements.txt at deploy time with a remote build, which
// needs the network -- the one thing this project will not do. So the
// dependencies have to be vendored into .python_packages, which is where
// the image's own PYTHONPATH already points.
//
// Refusing at deploy rather than warning is the decision, and the reason
// is that such an app can never serve a single invocation: every call
// would fail on the import. Failing where the user is looking beats
// failing later in a place that does not mention the cause.
func TestADeployWithoutVendoredDependenciesIsRefused(t *testing.T) {
	dir := t.TempDir()
	// A perfectly good function app, except for its dependencies.
	mustWrite(t, filepath.Join(dir, "host.json"), `{"version":"2.0"}`)
	mustWrite(t, filepath.Join(dir, "function_app.py"), "import azure.functions as func\n")

	err := checkVendoredDependencies(dir)
	if err == nil {
		t.Fatal("a deploy with no .python_packages was accepted; every invocation would fail")
	}
	for _, want := range []string{".python_packages", "pip install", "--target"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it does not say how to fix it: %v",
				want, err)
		}
	}
}

// With them vendored it is accepted.
func TestADeployWithVendoredDependenciesIsAccepted(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "host.json"), `{"version":"2.0"}`)
	pkg := filepath.Join(dir, ".python_packages", "lib", "site-packages", "azure", "functions")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pkg, "__init__.py"), "")

	if err := checkVendoredDependencies(dir); err != nil {
		t.Errorf("a correctly vendored deploy was refused: %v", err)
	}
}

// The directory existing but holding no `azure` package is still a
// refusal, because that is the one import every function app makes.
func TestAnEmptyPackagesDirectoryIsStillRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".python_packages", "lib", "site-packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkVendoredDependencies(dir); err == nil {
		t.Error("an empty .python_packages was accepted; azure.functions would not import")
	}
}

// --- the zip --------------------------------------------------------------

// A deployed zip is unpacked, and its entry names are judged as NAMES.
//
// A zip entry is not a path: `../../etc/passwd` is a legal entry and
// filepath.Join happily writes it outside the deploy directory. The two
// hosts disagree in both directions -- filepath.IsAbs is false for
// "/absolute.py" on Windows, and "C:/x" is an ordinary name on Unix -- so
// the judgement is on the raw, slash-separated name and the same archive
// is refused identically everywhere. This reuses the Lambda engine's
// unpacker for exactly that reason.
func TestADeployedZipIsUnpackedSafely(t *testing.T) {
	dir := t.TempDir()
	archive := zipOf(t, map[string]string{
		"host.json":       `{"version":"2.0"}`,
		"function_app.py": "import azure.functions\n",
		".python_packages/lib/site-packages/azure/__init__.py": "",
	})
	if err := unpackFunctionApp(archive, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "function_app.py")); err != nil {
		t.Errorf("the function was not unpacked: %v", err)
	}
	if err := checkVendoredDependencies(dir); err != nil {
		t.Errorf("the vendored package did not survive the unpack: %v", err)
	}
}

func TestAZipEntryThatEscapesIsRefused(t *testing.T) {
	for _, name := range []string{
		"../escape.py",
		"a/../../escape.py",
		`..\escape.py`,
		"/absolute.py",
		"C:/windows.py",
	} {
		dir := t.TempDir()
		err := unpackFunctionApp(zipOf(t, map[string]string{name: "x"}), dir)
		if err == nil {
			t.Errorf("the entry %q was accepted; it can land outside the deploy directory", name)
		}
	}
}

// A redeploy REPLACES the previous code rather than merging into it.
//
// Otherwise a function removed from the app would keep running, which is
// the same shape as a sidecar dropped from a template: the deploy looks
// applied and the old thing is still there.
func TestARedeployReplacesThePreviousCode(t *testing.T) {
	dir := t.TempDir()
	if err := unpackFunctionApp(zipOf(t, map[string]string{"old.py": "x"}), dir); err != nil {
		t.Fatal(err)
	}
	if err := unpackFunctionApp(zipOf(t, map[string]string{"new.py": "y"}), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "old.py")); err == nil {
		t.Error("a file from the previous deploy survived; a redeploy must replace the tree")
	}
	if _, err := os.Stat(filepath.Join(dir, "new.py")); err != nil {
		t.Errorf("the new deploy is missing: %v", err)
	}
}

// --- the hostname ---------------------------------------------------------

// A function app's hostname has ONE label before the suffix, because that
// is all a wildcard certificate covers -- the rule that shaped the
// container-app FQDN and the storage data plane before it.
func TestAFunctionAppHostnameHasOneLabel(t *testing.T) {
	s := newTestServerForFunctions(t)
	host := s.functionAppHostname("myapp")

	if !strings.HasPrefix(host, "myapp.") {
		t.Errorf("the hostname does not start with the app name: %q", host)
	}
	rest := strings.TrimPrefix(host, "myapp.")
	if strings.Count(stripHostPort(rest), ".") != strings.Count(FunctionsSuffix, ".") {
		t.Errorf("%q has more labels than the wildcard can cover", host)
	}
	// It carries our port, as the storage and Key Vault suffixes do,
	// because binding 443 needs root.
	if !strings.Contains(host, ":") {
		t.Errorf("the hostname does not carry Nimbus's port: %q", host)
	}
}

// The certificate covers it, or every client fails TLS verification on a
// name Nimbus told it to use.
func TestTheCertificateCoversFunctionAppNames(t *testing.T) {
	names := DNSNames()
	want := "*." + FunctionsSuffix
	for _, n := range names {
		if n == want {
			return
		}
	}
	t.Errorf("%q is not in the certificate's names: %v", want, names)
}

// --- helpers --------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newTestServerForFunctions is a Server with the Docker Fake in place.
//
// No unit test may reach the real daemon: a function app's hook builds an
// image and starts a container, and one left behind after the package
// passes is exactly what the Fake exists to prevent.
func newTestServerForFunctions(t *testing.T) *Server {
	t.Helper()
	bus := core.NewBus(100)
	store, err := core.NewStore(bus, "")
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{PublicURL: "https://localhost:4701", DataDir: t.TempDir()}, store, bus)
	s.useDocker(docker.NewFake())
	return s
}

// azurerm reads a site's app settings with POST .../config/appSettings/list.
//
// They are SECRETS, so the read is an action rather than a GET -- the same
// convention as a storage account's listKeys and a container app's
// listSecrets. Measured against azurerm 4.81.0: a 404 here fails the apply
// with "reading App Settings for Linux App Service ... was not found",
// naming a path the provider itself never created.
//
// The settings are a VIEW on the site: they go in as a LIST of
// {name, value} inside the site body and come back out of this path as a
// MAP. Nothing stores them separately.
func TestAppSettingsAreReadWithAListAction(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn1"
	putResource(t, s, site, `{
		"location": "eastus",
		"kind": "functionapp,linux",
		"properties": {
			"siteConfig": {
				"linuxFxVersion": "PYTHON|3.11",
				"appSettings": [
					{"name": "FUNCTIONS_WORKER_RUNTIME", "value": "python"},
					{"name": "GREETING", "value": "hello"}
				]
			}
		}
	}`)

	req := httptest.NewRequest(http.MethodPost,
		site+"/config/appSettings/list?api-version=2024-11-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("POST config/appSettings/list = %d, want 200: %s", rec.Code, rec.Body)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, rec.Body)
	}
	props, _ := doc["properties"].(map[string]any)
	if props["GREETING"] != "hello" {
		t.Errorf("the settings did not come back as a map of name to value: %v", doc)
	}
	if props["FUNCTIONS_WORKER_RUNTIME"] != "python" {
		t.Errorf("a setting is missing: %v", props)
	}
}

// And the GET form answers too, for `az` and anything else that uses it.
func TestAppSettingsAreAlsoReadableWithAGet(t *testing.T) {
	s, rg := newTestServerWithGroup(t)
	site := "/subscriptions/" + DefaultSubscriptionID + "/resourceGroups/" + rg +
		"/providers/Microsoft.Web/sites/fn2"
	putResource(t, s, site, `{
		"location": "eastus", "kind": "functionapp,linux",
		"properties": {"siteConfig": {"linuxFxVersion": "PYTHON|3.11",
			"appSettings": [{"name": "A", "value": "1"}]}}
	}`)

	req := httptest.NewRequest(http.MethodGet, site+"/config/appSettings?api-version=2024-11-01", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET config/appSettings = %d, want 200: %s", rec.Code, rec.Body)
	}
}
