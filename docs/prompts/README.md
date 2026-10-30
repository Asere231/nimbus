# Milestone prompts

One prompt per milestone. Start each Claude Code session from the `nimbus` folder with:

```
Work on docs/prompts/<file>. Start in plan mode: read the prompt, CLAUDE.md and
docs/ROADMAP.md, inspect the relevant code, and show me your plan before editing.
```

Do them in order. Each one ends at a clean commit with the milestone marked done in `docs/ROADMAP.md`.

| File | Milestone | Needs on your machine |
|---|---|---|
| `m01-finish-azure-core.md` | Finish M1: prove Terraform, harden, CI | Go, Terraform |
| `m02-storage.md` | Storage data plane (Blob, Queue, Table) | + Azure CLI |
| `m03-aks.md` | AKS on k3s | + Docker, kubectl |
| `m04-identity-secrets-acr.md` | Key Vault, managed identities, ACR | + Docker |
| `m05-breadth.md` | API-spec coverage, ARM/Bicep deployments, `az` CLI | + Bicep CLI |
| `m06-visualizer-v2.md` | Timeline, diff, Kubernetes view, export | Browser |
| `m07-aws-core.md` | AWS: IAM, S3, VPC | + AWS CLI |
| `m08-aws-compute.md` | EKS, Lambda, DynamoDB | + Docker |
| `m09-gcp-core.md` | GCP: projects, networks, Cloud Storage | + gcloud CLI |
| `m10-gcp-compute.md` | GKE, Pub/Sub, Cloud Run | + Docker |

## If a session goes off track

- **It starts the next milestone:** "Stop. Only the done-criteria in the prompt. Note the rest under Later in ROADMAP.md."
- **It guesses a protocol detail:** "Show me where the client does that: the SDK source or a debug log line."
- **It gets stuck in a loop on one error:** "Stop. Summarise what you tried, what the client sent, and what we returned. Then propose two different approaches."
- **The session is long:** "Get to a green commit, write what's left at the top of ROADMAP.md, and stop."
