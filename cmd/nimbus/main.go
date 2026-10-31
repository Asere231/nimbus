// Command nimbus runs the local multi-cloud emulator and its visualizer.
//
//	nimbus start            start Azure + the visualizer
//	nimbus cert             print where the CA certificate is and how to trust it
//	nimbus env azure        print environment variables for Terraform / the Azure SDKs
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
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/Asere231/nimbus/internal/azure"
	"github.com/Asere231/nimbus/internal/azure/storage"
	"github.com/Asere231/nimbus/internal/core"
	"github.com/Asere231/nimbus/internal/ui"
)

const version = "0.1.0"

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
		azCfg.DataDir = filepath.Join(dataDir, "storage")
	}
	az := azure.New(azCfg, store, bus)
	uiURL := fmt.Sprintf("http://%s:%d", *host, *uiPort)

	azSrv := &http.Server{Addr: fmt.Sprintf(":%d", *azPort), Handler: az, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	uiSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *uiPort),
		Handler:           ui.Handler(store, bus, ui.Info{Version: version, Clouds: map[string]string{"azure": azURL}, DataDir: dataDir}, blobBrowser{az.BlobBrowser()}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	for _, addr := range []string{azSrv.Addr, uiSrv.Addr} {
		if ln, err := net.Listen("tcp", addr); err != nil {
			log.Fatalf("port %s is busy: %v", addr, err)
		} else {
			ln.Close()
		}
	}

	go func() {
		if err := azSrv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("azure: %v", err)
		}
	}()
	go func() {
		if err := uiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = azSrv.Shutdown(ctx)
	_ = uiSrv.Shutdown(ctx)
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
