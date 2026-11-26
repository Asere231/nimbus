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
GATEWAY=$(docker exec "$NODE" getent hosts host.docker.internal | awk '{print $1}' | head -1)
if [ -z "$GATEWAY" ]; then echo "the node cannot resolve host.docker.internal"; exit 1; fi
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
echo "   the pod's own identity, as STS reports it:"
k -n "$NS" logs pod/reader | grep -i 'assumed-role' || true
