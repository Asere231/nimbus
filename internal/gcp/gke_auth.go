package gcp

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Making a GKE cluster reachable with kubectl.
//
// This is the same problem EKS had and the same answer, which is the
// finding: a managed-Kubernetes emulator cannot authenticate its own
// clusters, because the token is built on the client side out of cloud
// credentials, and the thing that validates it is the cluster's API
// server, not us. Real EKS solves it with aws-iam-authenticator and real
// GKE with its own identity service; both are token authentication
// webhooks, and so is this.
//
// What is DIFFERENT from EKS is worse, and was measured rather than
// guessed. `aws eks update-kubeconfig` writes a kubeconfig whose exec
// plugin (`aws eks get-token`) produces a presigned STS URL offline, so
// something always reaches the webhook. `gcloud container clusters
// get-credentials` writes one whose exec plugin is
// `gke-gcloud-auth-plugin`, and that plugin's whole job is to exchange a
// Google OAuth credential for a token. Under `auth/disable_credentials` --
// which is what makes gcloud talk to Nimbus at all -- there is no
// credential, so it produces nothing and NOTHING REACHES THIS WEBHOOK.
// Measured on macOS with gcloud 592.0.0: the plugin is not even installed
// by default, and `USE_GKE_GCLOUD_AUTH_PLUGIN=False` falls back to the
// in-tree `gcp` auth provider that kubectl removed in 1.26.
//
// So Nimbus supplies the client side too; see kubeconfig.go. The webhook
// here still accepts anything, which means a machine that DOES have a
// working auth plugin also works without a special case.
//
// Saying yes to every token is the same decision SigV4, Shared Key and
// the GCP bearer token already took: there is nothing here to protect,
// and a refusal could only ever mean a clock was wrong.

// TokenReviewPath is where a cluster's API server asks. Not a Google API,
// so it is prefixed out of the way of every real path.
const TokenReviewPath = "/_nimbus/gke/token-review"

// NimbusToken is the bearer token Nimbus's own kubeconfigs carry. Its
// value does not matter -- the webhook accepts anything -- but a
// recognisable one makes a kubeconfig readable, and makes it obvious in a
// cluster's audit log that the request came from here.
const NimbusToken = "nimbus-gke-token"

// tokenReview is the slice of the TokenReview object that matters.
//
// apiVersion and kind are echoed back exactly as sent: the API server
// rejects a response whose apiVersion differs from its request's, and says
// nothing useful about why.
type tokenReview struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Spec       struct {
		Token string `json:"token"`
	} `json:"spec"`
	Status *tokenReviewStatus `json:"status,omitempty"`
}

type tokenReviewStatus struct {
	Authenticated bool            `json:"authenticated"`
	User          tokenReviewUser `json:"user"`
	Audiences     []string        `json:"audiences,omitempty"`
}

type tokenReviewUser struct {
	Username string   `json:"username"`
	UID      string   `json:"uid"`
	Groups   []string `json:"groups"`
}

// serveTokenReview authenticates whatever it is given.
func (s *Server) serveTokenReview(w http.ResponseWriter, r *http.Request) string {
	var req tokenReview
	if err := readJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return "token review: bad request"
	}
	if req.APIVersion == "" {
		req.APIVersion = "authentication.k8s.io/v1"
	}
	if req.Kind == "" {
		req.Kind = "TokenReview"
	}
	// system:masters is what makes the caller a cluster administrator, so
	// `kubectl get nodes` works rather than being authenticated and then
	// immediately forbidden.
	req.Status = &tokenReviewStatus{
		Authenticated: true,
		User: tokenReviewUser{
			Username: "nimbus",
			UID:      "nimbus",
			Groups:   []string{"system:masters", "system:authenticated"},
		},
	}
	req.Spec.Token = "" // never echo the token back
	writeJSON(w, http.StatusOK, req)
	return ""
}

// webhookPathInContainer is where the kubeconfig is mounted inside a k3s
// container, under a directory k3s already owns.
const webhookPathInContainer = "/etc/rancher/k3s/nimbus-token-webhook.yaml"

// tokenWebhookKubeconfig is the file k3s reads to find this endpoint.
//
// The server address has to be reachable FROM THE CONTAINER, so it is
// host.docker.internal rather than localhost -- which is why that name has
// been in the Nimbus certificate's SANs since Milestone 2.
func tokenWebhookKubeconfig(endpoint string) string {
	return `apiVersion: v1
kind: Config
clusters:
  - name: nimbus
    cluster:
      server: ` + endpoint + TokenReviewPath + `
users:
  - name: nimbus
current-context: nimbus
contexts:
  - name: nimbus
    context:
      cluster: nimbus
      user: nimbus
`
}

// writeTokenWebhookConfig puts the kubeconfig somewhere Docker can bind
// mount it from, and returns that path. A bind mount rather than a write
// into the running container, because k3s reads its arguments at startup:
// there is no moment between the container existing and needing the file.
func (s *Server) writeTokenWebhookConfig(cfg Config) (string, error) {
	endpoint := cfg.PublicURL
	if endpoint == "" {
		endpoint = "http://host.docker.internal:4703"
	}
	endpoint = containerReachable(endpoint)

	dir := cfg.DataDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "nimbus-gcp")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "gke-token-webhook.yaml")
	if err := os.WriteFile(path, []byte(tokenWebhookKubeconfig(endpoint)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// containerReachable rewrites a URL so something inside Docker can reach
// it: in there, localhost is the container itself.
func containerReachable(endpoint string) string {
	endpoint = strings.Replace(endpoint, "localhost", "host.docker.internal", 1)
	return strings.Replace(endpoint, "127.0.0.1", "host.docker.internal", 1)
}
