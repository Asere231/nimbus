package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Asere231/nimbus/internal/gcp"
)

// The `nimbus gcp` commands, which exist because of one measured fact:
// `gcloud container clusters get-credentials` cannot produce a working
// kubeconfig against Nimbus on its own.
//
// It writes a kubeconfig whose user runs `gke-gcloud-auth-plugin`, and
// that plugin's whole job is to exchange a Google OAuth credential for a
// Kubernetes token. Nimbus has no OAuth -- `auth/disable_credentials` is
// what makes gcloud talk to it at all -- so the plugin has nothing to
// exchange and nothing ever reaches the cluster. Measured on macOS with
// gcloud 592.0.0: the plugin is not installed by default, and
// USE_GKE_GCLOUD_AUTH_PLUGIN=False falls back to the in-tree `gcp` auth
// provider that kubectl removed in 1.26.
//
// So there are two ways in, and both are here:
//
//   - `nimbus gcp kubeconfig CLUSTER` writes a kubeconfig directly. No
//     gcloud, no plugin, works in CI. This is the GKE equivalent of
//     `aws eks update-kubeconfig`.
//   - `nimbus gcp install-auth-plugin` drops a `gke-gcloud-auth-plugin`
//     shim on PATH, so gcloud's own kubeconfig works unmodified. The shim
//     answers for Nimbus clusters only and hands off to a real plugin for
//     anything else, so it cannot break real GKE.

func gcpCLI(args []string) {
	if len(args) == 0 {
		gcpUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "kubeconfig":
		gcpKubeconfig(args[1:])
	case "install-auth-plugin":
		gcpInstallAuthPlugin(args[1:])
	case "auth-token":
		gcpAuthToken(args[1:])
	case "register":
		gcpRegister(args[1:])
	default:
		gcpUsage()
		os.Exit(2)
	}
}

func gcpUsage() {
	fmt.Fprint(os.Stderr, `usage:
  nimbus gcp kubeconfig CLUSTER [--out PATH] [--gcp-port 4703]
  nimbus gcp install-auth-plugin [--dir DIR]
  nimbus gcp register [--run] [--gcp-port 4703]
  nimbus gcp auth-token            (what the installed plugin shim calls)
`)
}

// --- kubeconfig -----------------------------------------------------------

func gcpKubeconfig(args []string) {
	fs := flag.NewFlagSet("gcp kubeconfig", flag.ExitOnError)
	host := fs.String("host", "localhost", "hostname clients use to reach Nimbus")
	port := fs.Int("gcp-port", 4703, "the port Nimbus serves GCP on")
	project := fs.String("project", gcp.DefaultProjectID, "the project the cluster is in")
	out := fs.String("out", "", "write here instead of stdout; - is stdout")
	context := fs.String("context", "", "name for the context (default: the cluster's)")

	// The cluster name is taken before parsing, because Go's flag package
	// stops at the first non-flag argument: `kubeconfig NAME --out X`
	// would otherwise leave --out unparsed, which is how anyone would
	// actually type it.
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintln(os.Stderr, "usage: nimbus gcp kubeconfig CLUSTER [--out PATH]")
		os.Exit(2)
	}

	base := fmt.Sprintf("http://%s:%d", *host, *port)
	cluster, err := fetchCluster(base, *project, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}
	endpoint := str(cluster["endpoint"])
	if endpoint == "" {
		fmt.Fprintf(os.Stderr, "nimbus: cluster %q has no endpoint yet (status %s). "+
			"Wait for it to finish starting and try again.\n", name, str(cluster["status"]))
		os.Exit(1)
	}
	ca := ""
	if auth, ok := cluster["masterAuth"].(map[string]any); ok {
		ca = str(auth["clusterCaCertificate"])
	}
	ctxName := *context
	if ctxName == "" {
		ctxName = fmt.Sprintf("gke_%s_%s_%s", *project, str(cluster["location"]), name)
	}

	doc := kubeconfigFor(ctxName, endpoint, ca)
	if *out == "" || *out == "-" {
		fmt.Print(doc)
		return
	}
	if err := os.WriteFile(*out, []byte(doc), 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "nimbus: wrote %s\n\nUse it with:\n  KUBECONFIG=%s kubectl get nodes\n",
		*out, *out)
}

// fetchCluster finds a cluster by name, across every location.
//
// The location is not asked for because the server can answer without it:
// `locations/-` lists them all, which is GCP's own wildcard and which
// listClusters already honours.
func fetchCluster(base, project, name string) (map[string]any, error) {
	url := fmt.Sprintf("%s/v1/projects/%s/locations/-/clusters", base, project)
	resp, err := http.Get(url) //nolint:gosec // a local URL this command built
	if err != nil {
		return nil, fmt.Errorf("asking Nimbus for its clusters: %w\n\n"+
			"Is `nimbus start --clouds ...,gcp` running on %s?", err, base)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Nimbus answered %d for %s: %s", resp.StatusCode, url, strings.TrimSpace(string(body)))
	}
	var doc struct {
		Clusters []map[string]any `json:"clusters"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	for _, c := range doc.Clusters {
		if str(c["name"]) == name {
			return c, nil
		}
	}
	var have []string
	for _, c := range doc.Clusters {
		have = append(have, str(c["name"]))
	}
	if len(have) == 0 {
		return nil, fmt.Errorf("no GKE cluster named %q, and this Nimbus has none at all", name)
	}
	return nil, fmt.Errorf("no GKE cluster named %q; this Nimbus has: %s", name, strings.Join(have, ", "))
}

// kubeconfigFor writes a kubeconfig with a static bearer token.
//
// A token rather than an exec plugin, because the token is all the
// cluster needs: k3s is started with a TokenReview webhook pointing back
// at Nimbus, which authenticates anything. See internal/gcp/gke_auth.go
// for why nothing simpler works.
func kubeconfigFor(name, endpoint, caBase64 string) string {
	server := endpoint
	if !strings.HasPrefix(server, "http") {
		server = "https://" + server
	}
	ca := "    insecure-skip-tls-verify: true\n"
	if caBase64 != "" {
		ca = "    certificate-authority-data: " + caBase64 + "\n"
	}
	return "apiVersion: v1\nkind: Config\nclusters:\n" +
		"- name: " + name + "\n  cluster:\n    server: " + server + "\n" + ca +
		"contexts:\n- name: " + name + "\n  context:\n    cluster: " + name +
		"\n    user: " + name + "\n" +
		"current-context: " + name + "\n" +
		"users:\n- name: " + name + "\n  user:\n    token: " + gcp.NimbusToken + "\n"
}

// --- the auth plugin shim -------------------------------------------------

// authPluginName is what gcloud's kubeconfig calls, exactly.
const authPluginName = "gke-gcloud-auth-plugin"

func gcpInstallAuthPlugin(args []string) {
	fs := flag.NewFlagSet("gcp install-auth-plugin", flag.ExitOnError)
	dir := fs.String("dir", filepath.Join(defaultHome(), "bin"), "where to install the shim")
	_ = fs.Parse(args)

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: cannot find my own path: %v\n", err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}

	written, err := writeAuthPlugin(*dir, self)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("# Installed %s\n#\n", strings.Join(written, " and "))
	fmt.Printf("# It answers for Nimbus clusters and hands off to a real %s\n"+
		"# for anything else, so a machine that also uses real GKE is unaffected.\n#\n",
		authPluginName)
	fmt.Println("# Put it on PATH:")
	if runtime.GOOS == "windows" {
		fmt.Printf("$env:PATH = \"%s;$env:PATH\"\n", *dir)
	} else {
		fmt.Printf("export PATH=\"%s:$PATH\"\n", *dir)
	}
	fmt.Println()
	fmt.Println("# Then gcloud's own kubeconfig works:")
	fmt.Println("gcloud container clusters get-credentials CLUSTER --zone ZONE")
	fmt.Println("kubectl get nodes")
}

// writeAuthPlugin drops the shim, which is a two-line script calling back
// into this binary. A script rather than a copied binary so that updating
// Nimbus updates the plugin, and so the file is small enough to read.
func writeAuthPlugin(dir, self string) ([]string, error) {
	var written []string

	sh := filepath.Join(dir, authPluginName)
	script := "#!/bin/sh\n" +
		"# Installed by `nimbus gcp install-auth-plugin`.\n" +
		"# Answers Kubernetes' ExecCredential protocol for Nimbus clusters.\n" +
		"exec " + shellQuote(self) + " gcp auth-token \"$@\"\n"
	if err := os.WriteFile(sh, []byte(script), 0o755); err != nil {
		return nil, err
	}
	written = append(written, sh)

	if runtime.GOOS == "windows" {
		// kubectl runs the command through the shell's own lookup, which
		// on Windows needs an executable extension.
		cmd := filepath.Join(dir, authPluginName+".cmd")
		batch := "@echo off\r\nREM Installed by `nimbus gcp install-auth-plugin`.\r\n" +
			"\"" + self + "\" gcp auth-token %*\r\n"
		if err := os.WriteFile(cmd, []byte(batch), 0o755); err != nil {
			return nil, err
		}
		written = append(written, cmd)
	}
	return written, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// gcpAuthToken implements Kubernetes' client-go credential plugin
// protocol: print an ExecCredential on stdout and exit 0.
//
// With `provideClusterInfo: true` -- which is what gcloud writes -- the
// cluster being authenticated to arrives in KUBERNETES_EXEC_INFO. That is
// what makes it safe to install this on a machine that also talks to real
// GKE: a cluster that is not on this machine is handed to the real plugin
// instead of being answered with a token that would not work there.
func gcpAuthToken(args []string) {
	_ = args
	if !nimbusCluster(os.Getenv("KUBERNETES_EXEC_INFO")) {
		if err := execRealAuthPlugin(); err != nil {
			fmt.Fprintf(os.Stderr, "nimbus: this cluster is not a Nimbus one and no real %s "+
				"was found on PATH: %v\n", authPluginName, err)
			os.Exit(1)
		}
		return // exec replaced the process
	}
	// An hour is far longer than any local apply and short enough that a
	// stale kubeconfig does not look valid for ever.
	doc, err := json.Marshal(map[string]any{
		"apiVersion": "client.authentication.k8s.io/v1beta1",
		"kind":       "ExecCredential",
		"status": map[string]any{
			"token":               gcp.NimbusToken,
			"expirationTimestamp": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "nimbus: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(doc))
}

// nimbusCluster reports whether the cluster kubectl is authenticating to
// is on this machine. No exec info at all counts as yes: someone running
// the command by hand is asking for the Nimbus token.
func nimbusCluster(execInfo string) bool {
	if strings.TrimSpace(execInfo) == "" {
		return true
	}
	var info struct {
		Spec struct {
			Cluster struct {
				Server string `json:"server"`
			} `json:"cluster"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(execInfo), &info); err != nil {
		return true // unreadable: assume ours rather than failing the call
	}
	server := info.Spec.Cluster.Server
	if server == "" {
		return true
	}
	return strings.Contains(server, "127.0.0.1") ||
		strings.Contains(server, "localhost") ||
		strings.Contains(server, "[::1]")
}

// execRealAuthPlugin finds a genuine plugin elsewhere on PATH and replaces
// this process with it, so a real GKE cluster is unaffected by the shim.
// Our own directory is skipped, or the shim would find itself.
func execRealAuthPlugin() error {
	self, _ := os.Executable()
	ourDir := filepath.Dir(self)
	shimDir := filepath.Join(defaultHome(), "bin")

	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" || sameDir(dir, ourDir) || sameDir(dir, shimDir) {
			continue
		}
		candidate := filepath.Join(dir, authPluginName)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() {
			continue
		}
		// Run it as a child and pass its streams straight through, rather
		// than replacing this process: syscall.Exec does not exist on
		// Windows, and CI builds there.
		cmd := exec.Command(candidate, os.Args[3:]...) //nolint:gosec // a plugin already on the user's own PATH
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		os.Exit(0)
	}
	return fmt.Errorf("not on PATH")
}

func sameDir(a, b string) bool {
	ra, err1 := filepath.Abs(a)
	rb, err2 := filepath.Abs(b)
	if err1 != nil || err2 != nil {
		return a == b
	}
	return ra == rb
}

// --- gcloud registration --------------------------------------------------

func gcpRegister(args []string) {
	fs := flag.NewFlagSet("gcp register", flag.ExitOnError)
	host := fs.String("host", "localhost", "hostname clients use to reach Nimbus")
	port := fs.Int("gcp-port", 4703, "the port Nimbus serves GCP on")
	project := fs.String("project", gcp.DefaultProjectID, "the project to configure")
	name := fs.String("configuration", "nimbus", "the gcloud configuration to write")
	run := fs.Bool("run", false, "run the gcloud commands instead of only printing them")
	_ = fs.Parse(args)

	base := fmt.Sprintf("http://%s:%d", *host, *port)
	// `gcloud run` prefixes the REGION onto the HOST, because real Cloud
	// Run lives at us-central1-run.googleapis.com. An IP cannot take a
	// prefix, but a nested *.localhost name can -- and resolves to
	// loopback on macOS and Windows with nothing installed, which
	// Milestone 2 measured. So Cloud Run gets a name where everything
	// else gets an address.
	runBase := fmt.Sprintf("http://run.nimbus.localhost:%d/", *port)

	steps := [][]string{
		{"config", "configurations", "create", *name},
		{"config", "set", "core/project", *project},
		{"config", "set", "auth/disable_credentials", "true"},
		// container and iam take the BARE ORIGIN: both gcloud and the
		// Terraform provider append the version themselves, so an
		// endpoint ending in /v1/ produces /v1/v1/. Measured.
		{"config", "set", "api_endpoint_overrides/container", base + "/"},
		{"config", "set", "api_endpoint_overrides/iam", base + "/"},
		// These carry their version.
		{"config", "set", "api_endpoint_overrides/compute", base + "/compute/v1/"},
		{"config", "set", "api_endpoint_overrides/storage", base + "/storage/v1/"},
		{"config", "set", "api_endpoint_overrides/pubsub", base + "/"},
		{"config", "set", "api_endpoint_overrides/cloudresourcemanager", base + "/"},
		{"config", "set", "api_endpoint_overrides/run", runBase},
	}

	if !*run {
		for _, step := range steps {
			fmt.Println("gcloud", strings.Join(step, " "))
		}
		fmt.Println()
		fmt.Println("# Then, for a GKE cluster kubectl can reach:")
		fmt.Println("nimbus gcp install-auth-plugin")
		return
	}
	for _, step := range steps {
		cmd := exec.Command("gcloud", step...) //nolint:gosec // arguments this command built
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			// Creating a configuration that exists is not a failure worth
			// stopping for -- re-registering after a port change always
			// hits it, exactly as `az cloud register` does.
			if len(step) > 2 && step[2] == "create" {
				if err2 := exec.Command("gcloud", "config", "configurations", "activate", *name).Run(); err2 == nil {
					continue
				}
			}
			fmt.Fprintf(os.Stderr, "nimbus: gcloud %s failed: %v\n", strings.Join(step, " "), err)
			os.Exit(1)
		}
	}
	fmt.Println()
	fmt.Println("gcloud is pointed at Nimbus. For a GKE cluster kubectl can reach:")
	fmt.Println("  nimbus gcp install-auth-plugin")
}

// str reads a string out of a decoded JSON map.
func str(v any) string {
	s, _ := v.(string)
	return s
}
