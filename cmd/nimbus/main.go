// Command nimbus runs the local multi-cloud emulator and its visualizer.
//
//	nimbus start            start Azure + the visualizer
//	nimbus cert             print where the CA certificate is and how to trust it
//	nimbus env azure        print environment variables for Terraform / the Azure SDKs
//	nimbus version
package main

import (
	"context"
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
	"syscall"
	"time"

	"github.com/Asere231/nimbus/internal/azure"
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
	az := azure.New(azure.Config{PublicURL: azURL}, store, bus)
	uiURL := fmt.Sprintf("http://%s:%d", *host, *uiPort)

	azSrv := &http.Server{Addr: fmt.Sprintf(":%d", *azPort), Handler: az, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second}
	uiSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", *uiPort),
		Handler:           ui.Handler(store, bus, ui.Info{Version: version, Clouds: map[string]string{"azure": azURL}, DataDir: dataDir}),
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
	if len(args) == 0 || args[0] != "azure" {
		fmt.Fprintln(os.Stderr, "usage: nimbus env azure")
		os.Exit(2)
	}
	vars := [][2]string{
		{"ARM_METADATA_HOSTNAME", "localhost:4701"},
		{"ARM_SUBSCRIPTION_ID", azure.DefaultSubscriptionID},
		{"ARM_TENANT_ID", azure.DefaultTenantID},
		{"ARM_CLIENT_ID", azure.DefaultClientID},
		{"ARM_CLIENT_SECRET", "nimbus"},
		{"ARM_RESOURCE_PROVIDER_REGISTRATIONS", "none"},
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
