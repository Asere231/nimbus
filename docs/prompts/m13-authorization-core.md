# Milestone 13: authorization that can say no — the core, and AWS IAM

## Goal

Turn the project's most-repeated honest limitation into its most useful
feature. Role assignments, IAM policies and GCP bindings all round-trip
today, are drawn in the graph, and **no request is ever refused** — so a
local apply cannot prove a deployment's permissions are right, and
permissions are a large part of the thing this project exists to teach.

This milestone builds the decision engine and wires up one cloud: AWS, whose
policy documents are the richest and the most familiar. Azure RBAC and GCP
IAM follow in Milestone 14, so the engine is proven against one real policy
language before it has to serve three.

**Enforcement is opt-in** (`--enforce-iam`). With the flag off, nothing
changes and every existing example still applies — that is a done-criterion,
not an aspiration.

## Build

1. **A cloud-neutral authorization core**, probably `internal/authz`: a
   principal, an action, a resource, and a `Decision` carrying allow/deny,
   the rule that decided it, and enough of the evaluation to explain itself.
   Deny by default when enforcing. It must know nothing about any cloud's
   policy syntax — each cloud compiles its own documents down to whatever
   the core evaluates. If you find yourself putting `Sid` or `notActions` in
   the core, it belongs in the plugin.
2. **AWS IAM evaluation**: identity-based policies first — `Effect`,
   `Action` (with wildcards), `Resource` (with wildcards and ARNs), and an
   explicit-deny-wins rule. Documents are already stored URL-encoded; decode
   once, compile once, and do not re-parse per request. State plainly which
   `Condition` keys are supported and refuse the rest rather than ignoring
   them: an unevaluated condition that silently passes is the
   `FilterExpression` trap again, one service over.
3. **The refusal has to look right.** Each service's own error envelope and
   code — `AccessDenied` for S3, `UnauthorizedOperation` for EC2,
   `AccessDeniedException` for the JSON services — with the message naming
   the action and the principal, as real AWS does. A client that cannot
   recognise a denial retries it five times.
4. **Every denial is visible.** It goes on the bus and into the activity
   feed, flagged, naming the action, the principal and the rule that denied
   it. A denial nobody can see is worse than no enforcement at all, because
   the apply just fails.
5. **Attach the principal to the request.** See below — this is the design
   question the milestone turns on.

## Measure first

Before building, find out what the aws provider does with a denial
mid-apply: whether it retries, how it reports it, and whether a denied read
during a refresh looks like "gone" (which would make Terraform plan a
create). Also capture what `aws sts get-caller-identity` is expected to
return for an assumed role, since that is how a principal usually arrives.

## Ask the owner

- **How a request's principal is determined.** Nimbus never verifies
  signatures, so identity is whatever the client claims. Options: map the
  access key id in the SigV4 credential scope to an IAM user or role;
  honour `sts:AssumeRole` and hand back credentials that map to that role; a
  `--principal` default for anything unmapped; or a mix. Give 2–3 options
  with a recommendation — everything else here depends on the answer.
- **What happens to an unmapped caller** with the flag on: full access (so
  the examples keep working), no access (so the flag means what it says), or
  a named default principal.
- **Whether to ship the built-in AWS managed policies.** They are large; a
  subset plus "unknown managed policy = refuse, and say so" may teach more
  than a partial copy that silently allows too much.

## Done when

- [ ] With `--enforce-iam` off, every existing example applies, plans clean
      and destroys exactly as it does today. This is checked in CI.
- [ ] `examples/terraform/aws-iam-enforced`: an apply that FAILS with a
      legible `AccessDenied` naming the missing action, and succeeds once
      the policy grants it. Both directions are the test.
- [ ] A denied call appears in the activity feed with its principal, action
      and the rule that denied it.
- [ ] An explicit `Deny` beats an `Allow`, and an unsupported `Condition`
      is refused rather than ignored — both with tests.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
