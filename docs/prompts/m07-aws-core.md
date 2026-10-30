# Milestone 7: AWS core — STS, IAM, S3, VPC

## Goal

Add AWS on port 4702 using the same core and visualizer, so the Terraform `aws` provider and the AWS CLI work against Nimbus for identity, storage and networking.

## Background: how AWS differs from Azure

- **One endpoint, many protocols.** Services use different wire formats: Query (form-encoded request, XML response: EC2, IAM, STS), REST-XML (S3), JSON 1.0/1.1 with `X-Amz-Target` (DynamoDB, later), REST-JSON (Lambda, EKS). Route by the SigV4 `Authorization` header's service name, `X-Amz-Target`, or path.
- **Auth is SigV4**: accept any well-formed signature by default; optional strict mode verifies it with configured keys.
- **IDs**: ARNs (`arn:aws:iam::000000000000:role/x`) and prefixed IDs (`vpc-0abc…`). The store's `Resource.ID` is the ARN or ID; `ParentID` links subnets to VPCs, etc.
- **Clients**: the Terraform aws provider takes per-service `endpoints {}` overrides plus `skip_credentials_validation`, `skip_requesting_account_id`, `s3_use_path_style = true` (avoids bucket DNS).

## Refactor first

Before adding AWS, make sure nothing in `internal/core` or `internal/ui` is Azure-specific: cloud-agnostic type styling in the UI (per-cloud `TYPES` tables), cloud tabs enabled when that cloud's server is running, and `nimbus start --clouds azure,aws`. Ask me to review the refactor plan.

## Build

- **STS**: `GetCallerIdentity` (account `000000000000`), `AssumeRole`.
- **IAM**: users, roles, policies (create, get, list, attach, delete), instance profiles. No enforcement.
- **S3** (path-style): buckets (create, head, list, delete, location, versioning, tagging, policy, public access block, encryption config, ownership controls), objects (put, get, head, delete, list v2, multipart upload). Data on disk with `--persist`. azurerm-style lessons apply: the provider reads every sub-configuration after creating a bucket, so each GET must return the right "not configured" error when unset.
- **EC2 networking**: VPCs, subnets, internet gateways, route tables and routes, security groups and rules, NAT gateways (state only), `DescribeAvailabilityZones`, `DescribeAccountAttributes`. Describe calls support the filters the provider uses.
- **Visualizer**: AWS tab; VPC → subnets → route tables/SGs hierarchy; account as the root.

## Done when

- [ ] `examples/terraform/aws-basic`: VPC, 2 subnets, IGW, route table, security group, S3 bucket with versioning, IAM role. Apply, clean plan, destroy.
- [ ] `aws --endpoint-url http://localhost:4702 s3 cp` / `s3 ls` and `aws sts get-caller-identity` work.
- [ ] Azure examples and tests still pass unchanged.

## Watch out for

- EC2 Query responses need the exact XML namespace and `requestId`; describe calls with unknown filters should be ignored, not rejected.
- Decide HTTP vs HTTPS for 4702 (the aws provider accepts plain HTTP endpoints, which avoids cert work); ask me.
