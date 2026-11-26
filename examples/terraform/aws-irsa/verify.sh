#!/usr/bin/env bash
# The half of IRSA that Terraform cannot show: a real pod, holding a real
# projected token, getting real credentials and being refused the bucket
# its role does not grant.
#
# Run it after `terraform apply`. It needs the `aws` CLI, `kubectl`, `jq`
# and Docker. Nothing it does reaches the internet except the one image
# pull, which a machine that has run examples/terraform/aws-serverless
# already has.
set -euo pipefail

ENDPOINT=${NIMBUS_AWS_ENDPOINT:-http://localhost:4702}
CLUSTER=nimbus
NS=demo
SA=reader
IMAGE=${IRSA_IMAGE:-public.ecr.aws/lambda/python:3.12}
KUBECONFIG_FILE=$PWD/kubeconfig
export AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-nimbus}
export AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-nimbus}
export AWS_DEFAULT_REGION=${AWS_DEFAULT_REGION:-us-east-1}

ROLE_ARN=$(terraform output -raw role_arn)
k () { kubectl --kubeconfig "$KUBECONFIG_FILE" "$@"; }

echo "== 1. a kubeconfig, through the token webhook =="
aws --endpoint-url "$ENDPOINT" eks update-kubeconfig \
  --name "$CLUSTER" --kubeconfig "$KUBECONFIG_FILE" >/dev/null
k get nodes

echo
echo "== 2. where the pod reaches Nimbus =="
# A POD does not inherit the node's /etc/hosts: kubelet GENERATES each
# pod's from scratch, so the node's --add-host alias never reaches it and
# `hostAliases` is the only way in. The address is Docker Desktop's
# gateway on a Mac or Windows and the bridge gateway on Docker Engine, so
# it is READ from the node rather than written down anywhere.
NODE=$(docker ps --filter "label=nimbus.cluster=$CLUSTER" --filter "label=nimbus.role=server" \
  --format '{{.Names}}' | head -1)
if [ -z "$NODE" ]; then echo "no k3s server container for $CLUSTER"; exit 1; fi
# Read from /etc/hosts rather than with `getent`, which the k3s image does
# not have -- it is a minimal image and the lookup fails with "executable
# file not found", which reads as a resolution failure and is a missing
# tool. Only the IPv4 line: Docker writes both, and an IPv6 gateway in a
# hostAliases entry is legal but is not what the pod's HTTP client will
# prefer here.
GATEWAY=$(docker exec "$NODE" cat /etc/hosts \
  | awk '$2 == "host.docker.internal" && $1 ~ /^[0-9.]+$/ {print $1; exit}')
if [ -z "$GATEWAY" ]; then
  echo "the node has no host.docker.internal alias; Nimbus adds one to every"
  echo "container it starts, so this means --add-host did not take effect"
  exit 1
fi
echo "host.docker.internal -> $GATEWAY (read from $NODE)"

echo
echo "== 3. the service account, and a pod that uses it =="
# Real EKS has a mutating admission webhook (the EKS Pod Identity
# Webhook) that injects the projected volume and these three environment
# variables into any pod whose service account carries the role
# annotation. Nimbus does not run one, so the pod spec carries them
# itself. The annotation is still here, because it is what a real cluster
# reads and what anyone copying this example will expect to see.
k delete namespace "$NS" --ignore-not-found --wait >/dev/null 2>&1 || true
k create namespace "$NS" >/dev/null
k -n "$NS" create configmap probe --from-file=probe.py="$PWD/probe.py" >/dev/null
cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $SA
  namespace: $NS
  annotations:
    eks.amazonaws.com/role-arn: $ROLE_ARN
---
apiVersion: v1
kind: Pod
metadata:
  name: reader
  namespace: $NS
spec:
  serviceAccountName: $SA
  restartPolicy: Never
  hostAliases:
    - ip: "$GATEWAY"
      hostnames: ["host.docker.internal"]
  containers:
    - name: reader
      image: $IMAGE
      command: ["python3", "/work/probe.py"]
      env:
        - name: AWS_ROLE_ARN
          value: $ROLE_ARN
        - name: AWS_WEB_IDENTITY_TOKEN_FILE
          value: /var/run/secrets/eks.amazonaws.com/serviceaccount/token
        - name: AWS_ENDPOINT_URL
          value: http://host.docker.internal:4702
        - name: AWS_DEFAULT_REGION
          value: $AWS_DEFAULT_REGION
      volumeMounts:
        - name: aws-token
          mountPath: /var/run/secrets/eks.amazonaws.com/serviceaccount
          readOnly: true
        - name: probe
          mountPath: /work
  volumes:
    - name: aws-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              # The audience the trust policy's :aud condition names.
              # A token minted for anything else is refused by name.
              audience: sts.amazonaws.com
              expirationSeconds: 3600
    - name: probe
      configMap:
        name: probe
YAML

echo
echo "== 4. the proof =="
echo "   (boto3 is given no credentials: it finds AWS_ROLE_ARN and the token"
echo "    file and calls AssumeRoleWithWebIdentity itself)"
k -n "$NS" wait --for=condition=Ready pod/reader --timeout=180s >/dev/null || true
k -n "$NS" logs -f pod/reader
echo
# Note what "who am I" reports: the ROLE's ARN. Real AWS answers
# `arn:aws:sts::...:assumed-role/<role>/<session>` for assumed-role
# credentials, and Nimbus cannot, because the session name is not
# recoverable from the derived access key -- the key maps back to the
# role and that is what authorization needs. A stated divergence, and
# the ARN that matters for a policy is the same either way.
