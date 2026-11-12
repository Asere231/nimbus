# Permissions that are actually checked

Most emulators record IAM and enforce nothing, so a policy that would fail in
production passes locally. Nimbus can actually refuse.

```bash
nimbus start --clouds azure,aws,gcp --enforce-iam
```

All three clouds enforce, in one process, with one decision engine that
knows none of their policy languages.

- [How to try it](#how-to-try-it)
- [Who you are](#who-you-are)
- [What each cloud evaluates](#what-each-cloud-evaluates)
- [Asking why](#asking-why)
- [Deliberate limits](#deliberate-limits)

---

## How to try it

Four examples exist for exactly this:

| Example | Shows |
|---|---|
| `examples/terraform/aws-iam-enforced` | a role refused, then granted |
| `examples/terraform/aws-bucket-policy-enforced` | a bucket policy granting access the caller's own policy does not |
| `examples/terraform/azure-rbac-enforced` | a secret readable only with "Key Vault Secrets User" |
| `examples/terraform/gcp-iam-enforced` | a binding on a bucket reaching its objects |

Each applies against an enforcing server and is written to be read.

## Who you are

**Enforcement is opt-in, and an unmapped caller is the account root.** With
`--enforce-iam` off nothing is ever refused. With it on, a caller Nimbus
cannot place is root — which in real AWS carries no policies and can do
anything. That is both the faithful answer and the cheap one: examples that
know nothing about IAM keep working unchanged, and **a denial is something
you opt into by claiming a limited identity**.

Identity costs no extra state anywhere, because the credential *is* the
claim:

| Cloud | Identity comes from |
|---|---|
| AWS | the access key id — a role's derived key maps back to the role, and a key that is literally a role or user name maps to that entity |
| Azure | `client_id` in the token request, echoed into the JWT as `oid` |
| GCP | the bearer token, which *is* the member string (`serviceAccount:...`) |

`--principal ID` on `nimbus start` overrides all of it, which is the quickest
way to see a denial.

Nimbus verifies no signature. The question it answers is "would these
permissions work", not "is this caller genuine".

## What each cloud evaluates

**AWS.** Identity policies and resource policies, concatenated — which *is*
AWS's same-account combination rule, so an explicit `Deny` anywhere beats an
`Allow` anywhere. A resource policy's `Principal` is honoured and required;
an identity policy carrying one is refused, as AWS refuses it. Six managed
policies are bundled, transcribed exactly; an unknown managed ARN refuses and
names itself rather than silently granting nothing.

**Azure.** Role assignments and the real built-in role definitions, with
their real GUIDs. `notActions` are *subtracted*, not denied — an action one
assignment excludes is still allowed if another grants it. Scope inheritance
is segment-aligned, so an assignment at `.../rg` does not govern
`.../rg-two`. `dataActions` are enforced on Key Vault's data plane, which is
why **a Contributor can delete a vault and not read a secret out of it** —
the most surprising true thing about Azure RBAC.

**GCP.** Project and resource bindings, concatenated. The three basic roles
plus about a dozen predefined ones, expressed as the wildcard patterns they
amount to. `testIamPermissions` tells the truth and itself needs no
permission.

**An action nobody has mapped is refused, naming itself.** Uncomfortable and
correct: the alternative turns every gap into a permission nobody granted.
A gap fails loudly, where you can see it.

## Asking why

```bash
nimbus can s3:GetObject arn:aws:s3:::nimbus-data/readme.txt
nimbus can Microsoft.KeyVault/vaults/secrets/getSecret /subscriptions/.../vaults/kv
```

The visualizer's inspector shows the same thing for a selected resource.
Both are backed by the **same evaluator that enforces**, through
`/api/authz`, so the explanation cannot drift from the behaviour.

Every denial is also published to the activity feed with its reason, which
matters more than it sounds: an HTTP **HEAD** reply carries no body, so a
denied `HeadObject` reaches the client as `403 Forbidden` naming no
permission. Real S3 is no better. The feed is where the reason lives.

## Deliberate limits

- **Cross-account anything.** Nimbus has one account per cloud.
  `arn:aws:iam::123:root` in a `Principal` is refused rather than
  approximated, because honouring half of it would be silently too
  generous.
- **Storage data planes other than Key Vault's.** A data action is evaluated
  against a principal, and Azure Storage's Shared Key carries none.
  Enforcing there would mean requiring OAuth and breaking every storage
  example.
- **ACLs.** Stored, never evaluated.
- **The role catalogues are curations.** Azure ships 23 built-ins; GCP's
  basic roles are approximations of enormous real ones. An unknown role
  refuses by name.
- **Conditions.** Only some operators and keys are supported. One that
  cannot be evaluated refuses the requests it is about — and only those,
  never unrelated ones.

### A trap worth knowing

Revoking a permission Terraform needs in order to **read** its own resources
leaves a state no plan can escape: the apply that would restore it refreshes
first, and the refresh fails. `-refresh=false` is the way out. Real AWS
behaves identically, so this is kept rather than designed away — it is in
`examples/terraform/aws-iam-enforced` as a documented trap.
