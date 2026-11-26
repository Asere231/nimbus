# Milestone 25: EKS OIDC and IRSA

## Goal

A pod running on an emulated EKS cluster gets AWS credentials from a
service account, with no node role and no key in the image — and the
`aws_iam_openid_connect_provider` + `aws_iam_role` with a web-identity
trust policy that every EKS module writes applies against Nimbus.

Today an EKS cluster reports no `identity.oidc.issuer` at all, there is no
OIDC provider resource, `sts:AssumeRoleWithWebIdentity` does not exist,
and a `Federated` principal in a trust policy is refused by name.

## Why this is worth a milestone

IRSA is *the* thing to understand about EKS. Everything else about it is
Kubernetes; this is the part that is specifically AWS, and it is the
mechanism people get wrong in production — either by falling back to node
roles that grant every pod the same permissions, or by mounting a key.

It is also the first time Nimbus has to be an **identity provider** rather
than a consumer of one. Every other credential here is claimed and taken
on faith (`sigv4.go`: "the signature is the routing table"). Here Nimbus
signs a token with a key it holds, publishes the public half, and then
verifies that token on the way back in — the one place in the project where
a signature genuinely decides something, as M22's SAS was for Azure.

Entirely inside the stdlib rule: `crypto/rsa`, `crypto/x509`,
`encoding/json` and `encoding/base64` cover a JWT and a JWKS, and the CA
machinery already exists in `internal/core/certs.go`.

## What is already measured

The blocking question was answered before this prompt was written (see the
EKS entry in `docs/ROADMAP.md`), against the `tls` provider 4.x:

- `data.tls_certificate` **refuses `http://`** — `Invalid URL scheme`. The
  issuer cannot live on the AWS endpoint as it stands.
- Nimbus's **existing TLS listener answers it**, returning
  `[CN=Nimbus Local CA (is_ca), CN=localhost]`.
- It needs **the CA trusted**. The measurement that said otherwise --
  "with `SSL_CERT_FILE` and `REQUESTS_CA_BUNDLE` both unset it still
  fetched, so the data source does not verify the chain" -- was taken on
  a machine whose keychain already held the Nimbus CA, so the unset
  variables proved nothing. **CORRECTED** against a CI runner, which
  trusts nothing: `x509: certificate signed by unknown authority`. It
  verifies. `nimbus cert` once is enough, and CI installs the CA into the
  system bundle.
- `host.docker.internal` has been in the certificate's SANs since
  Milestone 2, so one issuer URL is reachable from the host AND from
  inside the k3s container — which `--service-account-issuer` needs.

What the k3s engine already does, so this needs no new mechanism:
`ServerArgs` passes `--kube-apiserver-arg=...` (that is how the token
webhook is configured today) and a file is bind-mounted into the node
(that is how its kubeconfig gets there).

## Decisions (taken by the owner, Nov 2026)

**1. Where does the OIDC issuer live?** It must be HTTPS, which the AWS
endpoint is not.

  - (a) **Serve it on the existing Azure TLS listener** (4701) under an
    AWS-shaped path. Nothing new to start, one certificate, works today —
    but an AWS feature answering on the port called "Azure" is a seam
    somebody will trip over, and `--clouds aws` alone would not serve it.
  - (b) **Give AWS its own TLS listener** on a new port, with the same
    certificate. Honest shape, `--clouds aws` is self-contained, and it
    costs a port and a flag. It also opens the door to the S3/STS data
    planes being reachable over TLS later, which several entries in
    "Later" want.
  - (c) **Serve the issuer over plain HTTP and accept that
    `data.tls_certificate` cannot be used**, documenting that the EKS
    module's canonical pattern does not apply and a thumbprint must be
    hardcoded. Cheapest, and it breaks the thing the milestone is for.

  **DECIDED: (b)** — AWS gets its own TLS listener. The measurement says
  (a) works, so this was a judgement about shape rather than feasibility,
  and (b) is the one that does not need unpicking later. It also makes
  `--clouds aws` self-contained and opens the TLS door that several
  "Later" entries want for the S3 and STS data planes.

**2. How far does IRSA go?**

  - (a) **The full path**: k3s signs projected service-account tokens with
    a key Nimbus holds, a pod reads the token from
    `/var/run/secrets/eks.amazonaws.com/serviceaccount/token`, calls
    `AssumeRoleWithWebIdentity`, and gets credentials the enforcement path
    then honours. A pod really writing to an S3 bucket it was granted and
    being refused one it was not is the done-criterion.
  - (b) **The control plane only**: the issuer, the OIDC provider
    resource, the trust policy, and `AssumeRoleWithWebIdentity` verified
    against a token Nimbus is handed — without wiring k3s's projected
    tokens. Terraform applies cleanly; no pod proves anything.

  **DECIDED: (a)** — all the way to the pod. (b) is the shape of
  divergence this project keeps finding and fixing: a configuration that
  applies cleanly and does nothing. The whole point is the pod.

## Done criteria

1. `aws_eks_cluster` reports `identity[0].oidc[0].issuer`, and
   `data.tls_certificate` against it returns a chain whose
   `certificates[0]` is a CA.
2. `aws_iam_openid_connect_provider` applies, plans clean and destroys.
3. An `aws_iam_role` whose trust policy has a `Federated` principal with
   `StringEquals` on `:sub` and `:aud` applies — and that condition is
   EVALUATED, not merely stored.
4. A pod with a service account annotated
   `eks.amazonaws.com/role-arn` reaches S3 through the role, with no
   credentials in the image.
5. The same pod is REFUSED a bucket the role does not grant, with
   `--enforce-iam` on.
6. `gofmt -l .`, `go vet ./...`, `go test ./...`, `go test -race`, and a
   CI job that proves 4 and 5 on Linux.

## Risks, stated rather than hidden

- **The token is verified for real, so every part of it must be right.**
  `kid`, the JWKS, the signature, `iss`, `aud`, `exp` and `sub`. This is
  the first signature in the project that decides anything, and a wrong
  one fails with "not authorized", naming nothing.
- **k3s's flags are read at startup.** The signing key must be bind-mounted
  before the container starts — the same rule the token webhook's
  kubeconfig taught, where there is no moment between the container
  existing and the file being needed.
- **A projected token has a short lifetime and is re-read from disk.** A
  pod that caches it will fail an hour in; the SDKs re-read, but anything
  hand-rolled in a test must too.
- **`Federated` principals reach the authorization core**, which is shared
  by three clouds. The change must not alter how an AWS identity policy or
  an Azure assignment is evaluated, and the existing tests are the guard.
