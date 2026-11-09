# Milestone 14: Azure RBAC, GCP IAM, and showing why

## Goal

Finish authorization: the other two clouds on the engine Milestone 13 built,
and the surface that makes a refusal teach something instead of just failing
an apply. This is the milestone where "permissions are recorded but never
enforced" stops being true anywhere.

## Build

1. **Azure RBAC.** Role assignments and the built-in role definitions
   already round-trip with the real GUIDs, which is most of the data. What
   is missing is evaluation: `actions`, `notActions`, `dataActions`,
   `notDataActions`, and **scope inheritance** — an assignment at a
   subscription or resource group applies to everything beneath it, which is
   the part a learner most needs to see working. Deny assignments exist in
   real Azure and are rare; say whether they are in scope.
2. **GCP IAM.** Project-level and resource-level bindings, `roles/*`
   expanded to permission sets, and the three-way split between basic,
   predefined and custom roles. The project IAM policy is recorded today;
   `getIamPolicy`/`setIamPolicy` already exist on several resources.
3. **The ARM and Google error shapes.** `AuthorizationFailed` for ARM, with
   the message real Azure sends (it names the principal, the action and the
   scope); `403` with the `PERMISSION_DENIED` status for Google. Check what
   azurerm and the google provider do with a denial mid-apply before
   choosing, the way Milestone 13 did for AWS.
4. **Explain a decision.** A refusal in the feed is enough to debug one
   call; it is not enough to learn a permission model. Add, in the
   inspector, why a principal can or cannot do something to the selected
   resource — the rule that decided, the scope it came from, and what would
   have to change. The views are dispatched per cloud already, so each
   cloud's explanation lives in its own module.
5. **`nimbus can`**, or whatever it should be called: a CLI question —
   "can this principal do this action to this resource" — answering with the
   decision and the reason. The same engine the request path uses, so the
   answer cannot drift from the behaviour.

## Ask the owner

- **Where the explanation goes.** A panel in the inspector, a third tab
   beside the network and Kubernetes views, or an overlay on the graph
   showing what the current principal can reach. Sketch 2–3 before
   building, as Milestone 6 did.
- **How much of each cloud's built-in role catalogue to ship.** Azure has
   hundreds of role definitions and Google thousands of permissions. A
   curated subset with "unknown role = refuse and name it" is probably
   honest; a partial copy that silently allows too much is the failure mode.

## Done when

- [ ] `examples/terraform/azure-rbac-enforced` and
      `examples/terraform/gcp-iam-enforced`: each fails legibly without the
      grant and succeeds with it, under `--enforce-iam`.
- [ ] An Azure role assignment made at a resource group governs a resource
      inside it, with a test for the inheritance specifically.
- [ ] With the flag off, every example in the repo — including
      `examples/multicloud` — applies and destroys as it does today.
- [ ] The inspector explains, for a selected resource, why the current
      principal is allowed or denied, on all three clouds.
- [ ] `nimbus can ...` agrees with what the request path actually does, with
      a test that drives both through the same cases.
- [ ] `README.md`'s per-cloud matrix says enforcement is real, and
      `docs/ROADMAP.md` loses "recorded, never enforced" from all three
      clouds.
- [ ] Full test suite and CI green on macOS, Windows and Linux.
