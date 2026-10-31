// Command nimbus runs the local multi-cloud emulator and its visualizer.
//
//	nimbus start            start Azure + the visualizer
//	nimbus cert             print where the CA certificate is and how to trust it
//	nimbus env azure        print environment variables for Terraform / the Azure SDKs
//	nimbus az register      register Nimbus as a custom cloud for the az CLI
//	nimbus version
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
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

	"github.com/Asere231/nimbus/internal/azure"
	"github.com/Asere231/nimbus/internal/azure/keyvault"
	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/ui"
)

const version = "0.5.0"

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
  nimbus start [--home DIR] [--ui-port 4700] [--azure-port 4701] [--persist]
  nimbus cert  [--home DIR]      where the CA is, and how to trust it on your OS
  nimbus env azure               environment variables for Terraform and the SDKs
  nimbus az register [--run]     register Nimbus as a custom cloud for the az CLI
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

func start(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	home := fs.String("home", defaultHome(), "directory for certificates and saved state")
	uiPort := fs.Int("ui-port", 4700, "visualizer port (http)")
	azPort := fs.Int("azure-port", 4701, "Azure Resource Manager port (https)")
	persist := fs.Bool("persist", false, "save state to disk so it survives restarts")
	host := fs.String("host", "localhost", "hostname clients use to reach Nimbus")
	_ = fs.Parse(args)

	certs, err := core.EnsureCerts(filepath.Join(*home, "certs"))
	if err != nil {
		log.Fatalf("creating certificates: %v", err)
	}
	tlsCfg, err := certs.TLSConfig()
	if err != nil {
		log.Fatalf("loading certificates: %v", err)
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

	azURL := fmt.Sprintf("https://%s:%d", *host, *azPort)
	azCfg := azure.Config{PublicURL: azURL}
	if dataDir != "" {
		azCfg.DataDir = dataDir
	}
	azCfg.KeepClusters = *persist
	az := azure.New(azCfg, store, bus)
	stopBackground := az.Start()
	uiURL := fmt.Sprintf("http://%s:%d", *host, *uiPort)

	azSrv := &http.Server{Addr: fmt.Sprintf(":%d", *azPort), Handler: az, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	uiSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *uiPort),
		Handler:           ui.Handler(store, bus, ui.Info{Version: version, Clouds: map[string]string{"azure": azURL}, DataDir: dataDir}, blobBrowser{az.BlobBrowser()}, secretBrowser{az.SecretBrowser()}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Bind both ports and KEEP the listeners. Probing a port and closing it
	// again leaves a gap in which something else can take it -- including the
	// previous Nimbus you just stopped -- and the failure then surfaces from
	// inside a goroutine, after the banner has already claimed success.
	listeners := make([]net.Listener, 0, 2)
	for _, addr := range []string{azSrv.Addr, uiSrv.Addr} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			for _, open := range listeners {
				open.Close()
			}
			log.Fatalf("port %s is busy: %v", addr, err)
		}
		listeners = append(listeners, ln)
	}

	go func() {
		if err := azSrv.ServeTLS(listeners[0], "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("azure: %v", err)
		}
	}()
	go func() {
		if err := uiSrv.Serve(listeners[1]); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ui: %v", err)
		}
	}()

	fmt.Printf(`
  Nimbus %s
  ─────────────────────────────────────────────
  Visualizer   %s
  Azure (ARM)  %s     metadata_host = "%s:%d"
  CA cert      %s
  State        %s
  ─────────────────────────────────────────────
  First run? Trust the CA once:  nimbus cert
  Press Ctrl+C to stop.

`, version, uiURL, azURL, *host, *azPort, certs.CACert, stateLabel(dataDir))

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	stopBackground()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = azSrv.Shutdown(ctx)
	_ = uiSrv.Shutdown(ctx)

	// Clusters are torn down unless --persist asked for them to survive.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelShutdown()
	if !*persist {
		fmt.Println("  stopping clusters (use --persist to leave them running)...")
	}
	az.Shutdown(shutdownCtx)
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
		suffix := fmt.Sprintf("%s:%d", core.StorageSuffix, *azPort)
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
	kvSuffix := fmt.Sprintf(".%s:%d", core.KeyVaultSuffix, *azPort)
	stSuffix := fmt.Sprintf("%s:%d", core.StorageSuffix, *azPort)

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
