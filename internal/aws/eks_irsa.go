package aws

// Making a cluster sign tokens Nimbus can verify.
//
// IRSA has two halves, and this is the one inside the cluster. The other
// half -- the issuer's discovery document, the JWKS, and
// AssumeRoleWithWebIdentity -- is in eks_oidc.go and sts_webidentity.go.
//
// A projected service-account token is minted by the API SERVER, not by
// Nimbus, so the only way a token can verify against the JWKS is for the
// API server to sign with the key Nimbus publishes. That is what these
// arguments do. Four of them matter:
//
//   - --service-account-signing-key-file: what the API server SIGNS with.
//     Overriding it is the whole point; k3s otherwise generates its own
//     key, which Nimbus has never seen and could never verify against.
//   - --service-account-key-file: what it VERIFIES with. It is repeatable,
//     and k3s's own key is listed as well as ours -- naming only ours
//     would stop the cluster accepting anything signed before the
//     override, and anything its own components signed with it.
//   - --service-account-issuer: goes into every token's `iss`, and has to
//     be the URL the OIDC provider resource was created for, character
//     for character. This is the per-cluster one.
//   - --api-audiences: which audiences the API server will MINT and
//     accept. `sts.amazonaws.com` is what IRSA asks for; the issuer is
//     listed too, because that is Kubernetes' own default and in-cluster
//     clients ask for tokens with it.
//
// The key is bind-MOUNTED, not written afterwards, for the reason the
// token webhook's kubeconfig already taught: k3s reads its arguments at
// startup, so there is no moment between the container existing and the
// file being needed.

// Where the signing key's two halves are mounted inside a k3s container.
const (
	saSignerKeyInContainer = "/etc/rancher/k3s/nimbus-sa-signer-key.pem"
	saSignerPubInContainer = "/etc/rancher/k3s/nimbus-sa-signer.pub.pem"
	// k3sOwnSAKey is where k3s keeps the key it generated for itself. It
	// stays in --service-account-key-file so that overriding the SIGNING
	// key does not make the cluster reject what it signed before.
	k3sOwnSAKey = "/var/lib/rancher/k3s/server/tls/service.key"
)

// irsaServerArgs are the engine-level arguments: the key files, which are
// the same for every cluster because the key belongs to this Nimbus rather
// than to one cluster.
func irsaServerArgs() []string {
	return []string{
		"--kube-apiserver-arg=--service-account-signing-key-file=" + saSignerKeyInContainer,
		"--kube-apiserver-arg=--service-account-key-file=" + saSignerPubInContainer,
		"--kube-apiserver-arg=--service-account-key-file=" + k3sOwnSAKey,
	}
}

// irsaClusterArgs are the per-cluster ones: the issuer, which differs for
// every cluster, and the audiences that follow from it.
func (s *Server) irsaClusterArgs(clusterName string) []string {
	issuer := s.IssuerURL(clusterName)
	if issuer == "" || s.signing == nil {
		return nil
	}
	return []string{
		"--kube-apiserver-arg=--service-account-issuer=" + issuer,
		"--kube-apiserver-arg=--api-audiences=" + irsaAudience + "," + issuer,
	}
}

// irsaAudience is what a pod's projected token is minted for, and what
// every IRSA trust policy puts in its `:aud` condition. The AWS SDKs ask
// for exactly this string.
const irsaAudience = "sts.amazonaws.com"
