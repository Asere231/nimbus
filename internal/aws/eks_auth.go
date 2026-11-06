package aws

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Making `aws eks update-kubeconfig` produce a kubeconfig that works.
//
// This is the one part of EKS that is not just a different shape of the
// same thing, and the obvious fix does not work. update-kubeconfig writes
// a kubeconfig whose user runs `aws eks get-token`, and that command
// builds its own bearer token entirely client-side: a presigned STS URL,
// signed now and valid for a minute. Nimbus never sees it being made and
// cannot predict it, so a static token file in k3s can never match, and
// an API server given a bearer token it cannot authenticate answers 401
// rather than falling back to anonymous.
//
// Real EKS solves this with a token authentication webhook -- that is
// what aws-iam-authenticator is -- and so does Nimbus. k3s is started
// with
//
//	--kube-apiserver-arg=--authentication-token-webhook-config-file=...
//
// pointing back here, and every token the API server cannot place is sent
// to this endpoint. Nimbus says yes.
//
// Saying yes to everything is the right answer for a local emulator and
// the same decision taken for SigV4 and Shared Key: there is nothing here
// to protect, and a rejection could only ever mean the clock is wrong.
// What matters is that the flow is the REAL one, so what you learn about
// EKS authentication here is true of EKS.

// TokenReviewPath is where the API server asks. It is not an AWS API, so
// it sits outside the service routing on a path nothing else uses.
const TokenReviewPath = "/_nimbus/eks/token-review"

// tokenReview is the slice of the TokenReview object that matters. The
// apiVersion and kind have to be echoed back exactly as sent: the API
// server rejects a response whose apiVersion differs from its request's,
// and the message it logs does not say so.
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
		writeJSON(w, 400, map[string]string{"message": err.Error()})
		return "token review: bad request"
	}
	if req.APIVersion == "" {
		req.APIVersion = "authentication.k8s.io/v1"
	}
	if req.Kind == "" {
		req.Kind = "TokenReview"
	}
	// system:masters is what makes the user a cluster administrator, and
	// it is why `kubectl get nodes` works rather than returning a
	// forbidden error immediately after authenticating.
	req.Status = &tokenReviewStatus{
		Authenticated: true,
		User: tokenReviewUser{
			Username: "nimbus",
			UID:      "nimbus",
			Groups:   []string{"system:masters", "system:authenticated"},
		},
	}
	req.Spec.Token = "" // never echo the token back
	writeJSON(w, 200, req)
	return ""
}

// tokenWebhookKubeconfig is the file k3s reads to find this endpoint.
//
// It is a kubeconfig, which is how every Kubernetes webhook is
// configured. The server address has to be reachable FROM THE CONTAINER,
// which is why it is host.docker.internal rather than localhost -- and
// why that name has been in the Nimbus certificate's SANs since
// Milestone 2.
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

// marshalTokenReview is used by the tests to build a request the way the
// API server does.
func marshalTokenReview(token string) []byte {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec":       map[string]any{"token": token},
	})
	return body
}

// webhookPathInContainer is where the kubeconfig is mounted inside a k3s
// container. Under /etc/rancher, which k3s already owns.
const webhookPathInContainer = "/etc/rancher/k3s/nimbus-token-webhook.yaml"

// writeTokenWebhookConfig puts the kubeconfig somewhere Docker can mount
// it from, and returns that path.
//
// It goes next to the rest of Nimbus's state when there is a data
// directory, and in a temporary directory otherwise -- a bind mount needs
// a real file on the host either way, so this is one of the few things
// that exists on disk even without --persist.
func (s *Server) writeTokenWebhookConfig(cfg Config) (string, error) {
	endpoint := cfg.PublicURL
	if endpoint == "" {
		endpoint = "http://host.docker.internal:4702"
	}
	// The cluster reaches Nimbus from inside Docker, where localhost is
	// the container itself. host.docker.internal is the host, and it has
	// been in the Nimbus certificate's SANs since Milestone 2 for exactly
	// this kind of call.
	endpoint = strings.Replace(endpoint, "localhost", "host.docker.internal", 1)
	endpoint = strings.Replace(endpoint, "127.0.0.1", "host.docker.internal", 1)

	dir := cfg.DataDir
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "nimbus-aws")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "eks-token-webhook.yaml")
	if err := os.WriteFile(path, []byte(tokenWebhookKubeconfig(endpoint)), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
