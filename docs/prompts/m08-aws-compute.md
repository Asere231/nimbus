# Milestone 8: AWS compute — EKS, Lambda, DynamoDB

## Goal

Reuse the Milestone 3 k3s engine for EKS, run Lambda functions in Docker, and add DynamoDB, so a typical serverless app and a Kubernetes workload both run locally on AWS APIs.

## Build

1. **EKS** (REST-JSON): `CreateCluster` / `DescribeCluster` / `DeleteCluster`, node groups mapped to k3s agents, `status` moving `CREATING` → `ACTIVE`. `aws eks update-kubeconfig` must work: return the k3s endpoint and CA, and make the `aws eks get-token` exec plugin produce a token k3s accepts (simplest: configure k3s with a static token file and return that token).
2. **Lambda**: create/update/delete/invoke functions from a zip; run them in the official AWS Lambda base images (`public.ecr.aws/lambda/<runtime>`), which include the Runtime Interface Emulator, one warm container per function. Environment variables, timeout, `Invoke` sync and `Event` async.
3. **DynamoDB** (JSON 1.0, `X-Amz-Target: DynamoDB_20120810.*`): tables with partition and sort keys, Put/Get/Update/Delete item, Query and Scan with the common condition and filter expressions, GSIs basic. Stored in memory or on disk.
4. **CloudWatch Logs** minimal: Lambda output lands in a log group the AWS CLI can tail.
5. **Visualizer**: EKS clusters reuse the Kubernetes view; Lambda shows invocation counts; DynamoDB shows item counts.

## Done when

- [ ] `examples/terraform/aws-serverless`: DynamoDB table + Lambda (Python) that writes to it + IAM role. `aws lambda invoke` writes an item that `aws dynamodb scan` returns.
- [ ] `examples/terraform/aws-eks`: VPC + EKS + node group; `aws eks update-kubeconfig` then `kubectl get nodes` shows Ready nodes.
- [ ] All earlier milestones' tests still pass.
