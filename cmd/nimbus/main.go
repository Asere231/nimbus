// Command nimbus runs the local multi-cloud emulator and its visualizer.
//
//	nimbus start            start Azure + the visualizer
//	nimbus cert             print where the CA certificate is and how to trust it
//	nimbus env azure        print environment variables for Terraform / the Azure SDKs
//	nimbus az register      register Nimbus as a custom cloud for the az CLI
//	nimbus version
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Asere231/nimbus/internal/aws"
	"github.com/Asere231/nimbus/internal/azure"
	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/gcp"
	"github.com/Asere231/nimbus/internal/ui"
)

const version = "0.9.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "start":
		start(os.Args[2:])
	case "cert":
		printCert(os.Args[2:])
	case "env":
		printEnv(os.Args[2:])
	case "az":
		azCLI(os.Args[2:])
	case "gcp":
		gcpCLI(os.Args[2:])
	case "plan-view":
		planView(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Println("nimbus", version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `nimbus: a local multi-cloud emulator with a live visualizer

Usage:
  nimbus start [--home DIR] [--ui-port 4700] [--clouds azure,aws,gcp] [--persist]
  nimbus cert  [--home DIR]      where the CA is, and how to trust it on your OS
  nimbus env azure               environment variables for Terraform and the SDKs
  nimbus az register [--run]     register Nimbus as a custom cloud for the az CLI
  nimbus gcp register [--run]    point the gcloud CLI at Nimbus
  nimbus gcp kubeconfig CLUSTER  a kubeconfig for a GKE cluster, for kubectl
  nimbus plan-view PLAN          show a terraform plan on the graph before applying it
  nimbus version
`)
}

func defaultHome() string {
	if h := os.Getenv("NIMBUS_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".nimbus"
	}
	return filepath.Join(home, ".nimbus")
}

// knownClouds is every cloud this build can run, in the order they appear in
// the banner. A name not in here is rejected by --clouds rather than quietly
// ignored, so a typo says so instead of starting a server that is not there.
var knownClouds = []string{"azure", "aws", "gcp"}

// parseClouds turns the --clouds list into a set, rejecting anything unknown.
func parseClouds(list string) (map[string]bool, error) {
	want := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		known := false
		for _, k := range knownClouds {
			known = known || k == name
		}
		if !known {
			return nil, fmt.Errorf("unknown cloud %q (this build knows: %s)", name, strings.Join(knownClouds, ", "))
		}
		want[name] = true
	}
	if len(want) == 0 {
		return nil, fmt.Errorf("--clouds listed nothing to run")
	}
	return want, nil
}

// cloudServer is one cloud's HTTP server, with everything start() needs to
// bind it, announce it and stop it again.
type cloudServer struct {
	name   string
	label  string // what the banner calls it
	url    string
	tls    bool
	srv    *http.Server
	extras []string              // certificate names its data planes need
	stop   func(context.Context) // engine teardown, after the HTTP shutdown
	back   func()                // background workers, stopped first
}

func start(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	home := fs.String("home", defaultHome(), "directory for certificates and saved state")
	uiPort := fs.Int("ui-port", 4700, "visualizer port (http)")
	azPort := fs.Int("azure-port", 4701, "Azure Resource Manager port (https)")
	awsPort := fs.Int("aws-port", 4702, "AWS port (http)")
	gcpPort := fs.Int("gcp-port", 4703, "Google Cloud port (http)")
	clouds := fs.String("clouds", "azure", "which clouds to run, comma separated: "+strings.Join(knownClouds, ", "))
	persist := fs.Bool("persist", false, "save state to disk so it survives restarts")
	host := fs.String("host", "localhost", "hostname clients use to reach Nimbus")
	_ = fs.Parse(args)

	running, err := parseClouds(*clouds)
	if err != nil {
		log.Fatal(err)
	}
	bus := core.NewBus(1000)
	dataDir := ""
	if *persist {
		dataDir = filepath.Join(*home, "data")
	}
	store, err := core.NewStore(bus, dataDir)
	if err != nil {
		log.Fatalf("opening state: %v", err)
	}
	// With --persist, events are kept too, so an apply can still be replayed
	// in the visualizer after a restart. Without it they live in the bus's
	// ring for as long as this process does, like the resources themselves.
	if dataDir != "" {
		journal, err := core.OpenJournal(dataDir)
		if err != nil {
			log.Fatalf("opening the event journal: %v", err)
		}
		defer journal.Close()
		bus.SetJournal(journal)
		// Nothing is published for resources that merely loaded from disk,
		// so the journal is told about them directly. Without this a replay
		// after a restart begins from an empty world.
		journal.AppendSnapshot(store.List(nil))
	}

	// Build the clouds that were asked for. Each one contributes its
	// endpoint to the visualizer and the names its data planes need on the
	// certificate, which is why the certificate is issued after this and
	// not before.
	var servers []*cloudServer
	var blobs ui.BlobBrowser
	var secrets ui.SecretBrowser

	if running["azure"] {
		azURL := fmt.Sprintf("https://%s:%d", *host, *azPort)
		azCfg := azure.Config{PublicURL: azURL, KeepClusters: *persist}
		if dataDir != "" {
			azCfg.DataDir = dataDir
		}
		az := azure.New(azCfg, store, bus)
		blobs = blobBrowser{az.BlobBrowser()}
		secrets = secretBrowser{az.SecretBrowser()}
		servers = append(servers, &cloudServer{
			name:  "azure",
			label: fmt.Sprintf(`Azure (ARM)  %s     metadata_host = "%s:%d"`, azURL, *host, *azPort),
			url:   azURL,
			tls:   true,
			srv: &http.Server{Addr: fmt.Sprintf(":%d", *azPort), Handler: az,
				ReadHeaderTimeout: 10 * time.Second},
			extras: azure.DNSNames(),
			back:   az.Start(),
			stop:   az.Shutdown,
		})
	}

	if running["aws"] {
		// Plain HTTP: no AWS client insists on TLS, S3 is addressed
		// path-style so no bucket hostname needs a certificate, and
		// `aws --endpoint-url http://localhost:4702` then works with
		// nothing trusted on the machine.
		awsURL := fmt.Sprintf("http://%s:%d", *host, *awsPort)
		awsCfg := aws.Config{PublicURL: awsURL, KeepClusters: *persist}
		if dataDir != "" {
			awsCfg.DataDir = dataDir
		}
		a := aws.New(awsCfg, store, bus)
		servers = append(servers, &cloudServer{
			name:  "aws",
			label: fmt.Sprintf(`AWS          %s      --endpoint-url %s`, awsURL, awsURL),
			url:   awsURL,
			srv: &http.Server{Addr: fmt.Sprintf(":%d", *awsPort), Handler: a,
				ReadHeaderTimeout: 10 * time.Second},
			back: a.Start(),
			stop: a.Shutdown,
		})
	}

	if running["gcp"] {
		// Plain HTTP, like AWS and for the same reasons: the google
		// provider's *_custom_endpoint settings and gcloud's
		// api_endpoint_overrides take any URL, and Cloud Storage is
		// addressed by path so no bucket name becomes a hostname.
		gcpURL := fmt.Sprintf("http://%s:%d", *host, *gcpPort)
		gcpCfg := gcp.Config{PublicURL: gcpURL, KeepClusters: *persist}
		if dataDir != "" {
			gcpCfg.DataDir = dataDir
		}
		g := gcp.New(gcpCfg, store, bus)
		servers = append(servers, &cloudServer{
			name:  "gcp",
			label: fmt.Sprintf(`GCP          %s      project = "%s"`, gcpURL, g.ProjectID()),
			url:   gcpURL,
			srv: &http.Server{Addr: fmt.Sprintf(":%d", *gcpPort), Handler: g,
				ReadHeaderTimeout: 10 * time.Second},
			back: g.Start(),
			stop: g.Shutdown,
		})
	}

	var certNames []string
	endpoints := map[string]string{}
	for _, c := range servers {
		certNames = append(certNames, c.extras...)
		endpoints[c.name] = c.url
	}
	certs, err := core.EnsureCerts(filepath.Join(*home, "certs"), certNames...)
	if err != nil {
		log.Fatalf("creating certificates: %v", err)
	}
	tlsCfg, err := certs.TLSConfig()
	if err != nil {
		log.Fatalf("loading certificates: %v", err)
	}
	for _, c := range servers {
		if c.tls {
			c.srv.TLSConfig = tlsCfg
		}
	}

	uiURL := fmt.Sprintf("http://%s:%d", *host, *uiPort)
	uiSrv := &http.Server{
		Addr: fmt.Sprintf(":%d", *uiPort),
		Handler: ui.Handler(store, bus,
			ui.Info{Version: version, Clouds: endpoints, DataDir: dataDir}, blobs, secrets),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind every port and KEEP the listeners. Probing a port and closing it
	// again leaves a gap in which something else can take it -- including the
	// previous Nimbus you just stopped -- and the failure then surfaces from
	// inside a goroutine, after the banner has already claimed success.
	all := append([]*http.Server{}, uiSrv)
	for _, c := range servers {
		all = append(all, c.srv)
	}
	listeners := make([]net.Listener, 0, len(all))
	for _, srv := range all {
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			log.Fatalf("port %s is busy: %v", srv.Addr, err)
		}
		listeners = append(listeners, ln)
	}

	go func() {
		if err := uiSrv.Serve(listeners[0]); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ui: %v", err)
		}
	}()
	for i, c := range servers {
		c, ln := c, listeners[i+1]
		go func() {
			var err error
			if c.tls {
				err = c.srv.ServeTLS(ln, "", "")
			} else {
				err = c.srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("%s: %v", c.name, err)
			}
		}()
	}

	lines := []string{fmt.Sprintf("  Visualizer   %s", uiURL)}
	for _, c := range servers {
		lines = append(lines, "  "+c.label)
	}
	fmt.Printf(`
  Nimbus %s
  ─────────────────────────────────────────────
%s
  CA cert      %s
  State        %s
  ─────────────────────────────────────────────
  First run? Trust the CA once:  nimbus cert
  Press Ctrl+C to stop.

`, version, strings.Join(lines, "\n"), certs.CACert, stateLabel(dataDir))

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	for _, c := range servers {
		if c.back != nil {
			c.back()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = uiSrv.Shutdown(ctx)
	for _, c := range servers {
		_ = c.srv.Shutdown(ctx)
	}

	// Engines are torn down unless --persist asked for them to survive.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelShutdown()
	if !*persist && running["azure"] {
		fmt.Println("  stopping clusters (use --persist to leave them running)...")
	}
	for _, c := range servers {
		if c.stop != nil {
			c.stop(shutdownCtx)
		}
	}
}

// blobBrowser adapts the Azure storage handler to the visualizer's interface.
// The adapter lives here, in the one place that already knows about both, so
// neither internal/ui nor the storage plugin has to import the other.
type blobBrowser struct{ h *storage.Handler }

func (b blobBrowser) ListBlobs(account, container string) []ui.BlobInfo {
	out := []ui.BlobInfo{}
	for _, s := range b.h.ListBlobs(account, container) {
		out = append(out, ui.BlobInfo{
			Name: s.Name, Size: s.Size, ContentType: s.ContentType, Modified: s.Modified,
		})
	}
	return out
}

func (b blobBrowser) ReadBlob(account, container, name string) ([]byte, string, bool) {
	return b.h.ReadBlob(account, container, name)
}

func stateLabel(dir string) string {
	if dir == "" {
		return "in memory (use --persist to keep it)"
	}
	return dir
}

func printCert(args []string) {
	fs := flag.NewFlagSet("cert", flag.ExitOnError)
	home := fs.String("home", defaultHome(), "directory for certificates")
	_ = fs.Parse(args)
	certs, err := core.EnsureCerts(filepath.Join(*home, "certs"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("CA certificate: %s\n\n", certs.CACert)
	switch runtime.GOOS {
	case "darwin":
		fmt.Printf("Trust it (macOS, asks for your password):\n  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain \"%s\"\n", certs.CACert)
	case "windows":
		fmt.Printf("Trust it (Windows PowerShell, current user, no admin needed):\n  Import-Certificate -FilePath \"%s\" -CertStoreLocation Cert:\\CurrentUser\\Root\n", certs.CACert)
	default:
		fmt.Printf("Trust it (Debian/Ubuntu):\n  sudo cp \"%s\" /usr/local/share/ca-certificates/nimbus-ca.crt && sudo update-ca-certificates\nOr per command: SSL_CERT_FILE=\"%s\"\n", certs.CACert, certs.CACert)
	}
	fmt.Println("\nTo remove it later, delete \"Nimbus Local CA\" from the same store.")
}

func printEnv(args []string) {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	account := fs.String("storage-account", "", "also print a connection string for this storage account")
	azPort := fs.Int("azure-port", 4701, "the port Nimbus serves Azure on")
	if len(args) == 0 || args[0] != "azure" {
		fmt.Fprintln(os.Stderr, "usage: nimbus env azure [--storage-account NAME]")
		os.Exit(2)
	}
	_ = fs.Parse(args[1:])
	vars := [][2]string{
		{"ARM_METADATA_HOSTNAME", "localhost:4701"},
		{"ARM_SUBSCRIPTION_ID", azure.DefaultSubscriptionID},
		{"ARM_TENANT_ID", azure.DefaultTenantID},
		{"ARM_CLIENT_ID", azure.DefaultClientID},
		{"ARM_CLIENT_SECRET", "nimbus"},
		{"ARM_RESOURCE_PROVIDER_REGISTRATIONS", "none"},
	}
	if *account != "" {
		// `az storage` and the SDKs take a connection string instead of
		// separate settings. Per-service endpoints are spelled out rather than
		// using EndpointSuffix, so no client has to guess our port.
		//
		// The key is whatever the running server issued, so fetch it rather
		// than inventing one: ARM is the only source of truth for it.
		key, err := storageKey(*azPort, *account)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nimbus: could not read the key for %q: %v\n", *account, err)
			fmt.Fprintln(os.Stderr, "Is Nimbus running, and does that account exist?")
			os.Exit(1)
		}
		suffix := fmt.Sprintf("%s:%d", azure.StorageSuffix, *azPort)
		vars = append(vars, [2]string{"AZURE_STORAGE_CONNECTION_STRING",
			storage.ConnectionString(*account, key, suffix)})
	}

	if runtime.GOOS == "windows" {
		for _, v := range vars {
			fmt.Printf("$env:%s = \"%s\"\n", v[0], v[1])
		}
		return
	}
	for _, v := range vars {
		fmt.Printf("export %s=%s\n", v[0], v[1])
	}
}

// azCLI implements `nimbus az register`, which teaches the az CLI about
// Nimbus.
//
// az works from its own list of clouds, so using it against Nimbus means
// registering one. A full custom-cloud story is Milestone 5's job; this is the
// part Key Vault needs, because `az keyvault` builds vault hostnames from the
// cloud's keyvault DNS suffix and would otherwise talk to real Azure.
//
// Two things here were measured against az 2.90.0 and are easy to get wrong:
//
//  1. `az cloud register` immediately GETs /metadata/endpoints from the
//     endpoint it was handed, so the CA has to be trusted BEFORE that command
//     runs, not afterwards. We set REQUESTS_CA_BUNDLE on the child process so
//     --run works whatever the shell has exported.
//  2. MSAL validates an unknown authority by asking the REAL
//     login.microsoftonline.com about it, which both reaches the internet and
//     fails outright once REQUESTS_CA_BUNDLE has replaced the system trust
//     store. `core.instance_discovery=false` turns that off; it is the switch
//     Azure Stack uses (azure/cli/core/_profile.py).
func azCLI(args []string) {
	if len(args) == 0 || args[0] != "register" {
		fmt.Fprintln(os.Stderr, "usage: nimbus az register [--home DIR] [--azure-port 4701] [--name nimbus] [--run]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("az register", flag.ExitOnError)
	home := fs.String("home", defaultHome(), "directory holding the CA certificate")
	azPort := fs.Int("azure-port", 4701, "the port Nimbus serves Azure on")
	name := fs.String("name", "nimbus", "name for the registered cloud")
	host := fs.String("host", "localhost", "hostname clients use to reach Nimbus")
	run := fs.Bool("run", false, "run the az cloud commands instead of only printing them")
	_ = fs.Parse(args[1:])

	base := fmt.Sprintf("https://%s:%d", *host, *azPort)
	ca := filepath.Join(*home, "certs", "nimbus-ca.pem")
	// az wants the Key Vault suffix with a leading dot (".vault.azure.net"),
	// and the storage one without. Both carry our port: nothing can bind 443
	// without root.
	kvSuffix := fmt.Sprintf(".%s:%d", azure.KeyVaultSuffix, *azPort)
	stSuffix := fmt.Sprintf("%s:%d", azure.StorageSuffix, *azPort)

	register := []string{
		"cloud", "register", "-n", *name,
		"--endpoint-resource-manager", base,
		"--endpoint-active-directory", base,
		"--endpoint-active-directory-resource-id", base + "/",
		"--endpoint-active-directory-graph-resource-id", base + "/graph/",
		"--endpoint-management", base,
		"--suffix-keyvault-dns", kvSuffix,
		"--suffix-storage-endpoint", stSuffix,
	}
	steps := [][]string{
		{"config", "set", "core.instance_discovery=false"},
		register,
		{"cloud", "set", "-n", *name},
		{"cloud", "update", "--profile", "latest"},
	}

	// The CA has to be in place before the first az call, so print it first
	// and hand it to every child process.
	fmt.Println("# Trust the Nimbus CA in the shell you use az from:")
	if runtime.GOOS == "windows" {
		fmt.Printf("$env:REQUESTS_CA_BUNDLE = \"%s\"\n", ca)
	} else {
		fmt.Printf("export REQUESTS_CA_BUNDLE=%s\n", ca)
	}
	fmt.Println()

	if *run {
		for _, step := range steps {
			if err := runAz(ca, step); err != nil {
				// `az cloud register` refuses a name it already knows, which
				// re-registering after a port change always hits. The same
				// flags work as `cloud update`.
				if step[1] == "register" {
					update := append([]string{"cloud", "update"}, step[2:]...)
					if err2 := runAz(ca, update); err2 == nil {
						continue
					}
				}
				fmt.Fprintf(os.Stderr, "nimbus: az %s failed: %v\n", step[1], err)
				os.Exit(1)
			}
		}
	} else {
		for _, step := range steps {
			fmt.Println("az", strings.Join(step, " "))
		}
	}

	// Signing in is printed either way: it writes to the az credential cache,
	// which lives outside Nimbus.
	fmt.Println()
	fmt.Println("# Then sign in:")
	fmt.Printf("az login --service-principal -u %s -p nimbus --tenant %s --allow-no-subscriptions\n",
		azure.DefaultClientID, azure.DefaultTenantID)
	fmt.Println()
	fmt.Println("# To go back to real Azure:")
	fmt.Println("az cloud set -n AzureCloud")
	fmt.Println("az config set core.instance_discovery=true")
}

// runAz runs one az command with the Nimbus CA trusted, so `--run` does not
// depend on the caller having exported REQUESTS_CA_BUNDLE first.
func runAz(ca string, args []string) error {
	fmt.Println("+ az", strings.Join(args, " "))
	cmd := exec.Command("az", args...)
	cmd.Env = append(os.Environ(), "REQUESTS_CA_BUNDLE="+ca)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// storageKey asks the running server for an account key through ARM.
func storageKey(port int, account string) (string, error) {
	// The resource group is not known here, so find the account by listing
	// every storage account in the subscription and matching on name.
	listURL := fmt.Sprintf("https://localhost:%d/subscriptions/%s/providers/Microsoft.Storage/storageAccounts?api-version=2023-01-01",
		port, azure.DefaultSubscriptionID)
	client, err := nimbusClient()
	if err != nil {
		return "", err
	}
	res, err := client.Get(listURL)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var list struct {
		Value []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"value"`
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return "", err
	}
	for _, a := range list.Value {
		if !strings.EqualFold(a.Name, account) {
			continue
		}
		keysURL := fmt.Sprintf("https://localhost:%d%s/listKeys?api-version=2023-01-01", port, a.ID)
		kr, err := client.Post(keysURL, "application/json", nil)
		if err != nil {
			return "", err
		}
		defer kr.Body.Close()
		var keys struct {
			Keys []struct {
				Value string `json:"value"`
			} `json:"keys"`
		}
		if err := json.NewDecoder(kr.Body).Decode(&keys); err != nil {
			return "", err
		}
		if len(keys.Keys) == 0 {
			return "", errors.New("the account reported no keys")
		}
		return keys.Keys[0].Value, nil
	}
	return "", fmt.Errorf("no storage account named %q", account)
}

// nimbusClient trusts the Nimbus CA explicitly, so `nimbus env` works whether
// or not the CA has been installed in the OS trust store yet.
func nimbusClient() (*http.Client, error) {
	certs, err := core.EnsureCerts(filepath.Join(defaultHome(), "certs"))
	if err != nil {
		return nil, err
	}
	pool, err := certs.CAPool()
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}, nil
}

// secretBrowser adapts the Key Vault data plane to the visualizer's
// interface, in the same place and for the same reason as blobBrowser.
type secretBrowser struct{ h *keyvault.Handler }

func (b secretBrowser) ListSecrets(vault string) []ui.SecretInfo {
	out := []ui.SecretInfo{}
	for _, s := range b.h.ListSecrets(vault) {
		out = append(out, ui.SecretInfo{
			Name: s.Name, Version: s.Version, Versions: s.Versions,
			ContentType: s.ContentType, Enabled: s.Enabled,
			Updated: s.Updated, Deleted: s.Deleted,
		})
	}
	return out
}

func (b secretBrowser) ReadSecret(vault, name string) (string, bool) {
	return b.h.ReadSecret(vault, name)
}

// planView reads a Terraform plan and shows it on the graph as an overlay,
// so what is about to change is visible before anything changes.
//
// It takes either a saved plan (`terraform plan -out plan.out`), which it
// converts by running `terraform show -json`, or JSON that has already been
// converted. The two are told apart by looking at the file: Terraform's
// saved plan is a zip archive, so it never starts with '{'.
func planView(args []string) {
	fs := flag.NewFlagSet("plan-view", flag.ExitOnError)
	uiURL := fs.String("ui", "http://localhost:4700", "the visualizer to send the plan to")
	clear := fs.Bool("clear", false, "remove the overlay instead of adding one")
	_ = fs.Parse(args)

	if *clear {
		req, _ := http.NewRequest(http.MethodDelete, *uiURL+"/api/plan", nil)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Fatalf("clearing the overlay: %v", err)
		}
		res.Body.Close()
		fmt.Println("Plan overlay cleared.")
		return
	}

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: nimbus plan-view [--ui URL] PLAN")
		fmt.Fprintln(os.Stderr, "   or: nimbus plan-view --clear")
		fmt.Fprintln(os.Stderr, "\nMake a plan first:  terraform plan -out plan.out")
		os.Exit(2)
	}
	path := fs.Arg(0)

	jsonPlan, err := planJSON(path)
	if err != nil {
		log.Fatal(err)
	}
	overlay, err := ui.ReadPlan(bytes.NewReader(jsonPlan))
	if err != nil {
		log.Fatal(err)
	}
	overlay.Source = filepath.Base(path)

	body, err := json.Marshal(overlay)
	if err != nil {
		log.Fatal(err)
	}
	res, err := http.Post(*uiURL+"/api/plan", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("sending the plan to %s: %v\n  Is Nimbus running?", *uiURL, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		out, _ := io.ReadAll(res.Body)
		log.Fatalf("%s rejected the plan: %s", *uiURL, strings.TrimSpace(string(out)))
	}

	fmt.Printf("Plan: %s.\n", overlay.Summary())
	for _, c := range overlay.Changes {
		fmt.Printf("  %-8s %s\n", c.Action, c.Address)
	}
	fmt.Printf("\nShown on the graph at %s. Clear it with: nimbus plan-view --clear\n", *uiURL)
}

// planJSON returns the plan as `terraform show -json` output, converting a
// saved plan file if that is what it was given.
func planJSON(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return data, nil
	}

	// A saved plan only means anything inside the working directory that
	// produced it, so terraform runs there.
	dir := filepath.Dir(path)
	cmd := exec.Command("terraform", "show", "-json", filepath.Base(path))
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running `terraform show -json %s` in %s: %v\n%s",
			filepath.Base(path), dir, err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
