// Why the current principal can or cannot do something to the selected
// resource.
//
// Shared across all three clouds and keyed on NOTHING: the server answers
// with a cloud-neutral shape -- an action, a verdict, the rule that
// decided -- because the thing that explains is the thing that decides.
// A second implementation here, mapping types to permissions in
// JavaScript, could disagree with the server and would eventually be
// wrong in a way nobody could see. That is also why this module passes
// test/ui/clouds.test.js: there is no type name in it.
//
// `nimbus can` asks the same endpoint with an explicit action.

import { $ } from "./dom.js";
import { getAccess } from "./api.js";

export async function showAccess(r) {
  const panel = $("#i-access");
  if (!r) {
    panel.hidden = true;
    return;
  }

  let report;
  try {
    report = await getAccess(r.id);
  } catch (e) {
    panel.hidden = true;
    return;
  }
  // A cloud with no evaluator, or a type whose actions nobody has named:
  // say nothing rather than imply there is nothing to say.
  if (!report || !report.checks || !report.checks.length) {
    panel.hidden = true;
    return;
  }

  panel.hidden = false;
  $("#i-access-who").textContent = describe(report);
  $("#i-access-list").replaceChildren(...report.checks.map(row));
}

// describe says who was asked about and whether the answer is binding.
// With --enforce-iam off these are what WOULD happen, which is useful and
// must not be mistaken for what is happening.
function describe(report) {
  const who = report.principal || "the default identity";
  if (report.unrestricted) return `${who} — unrestricted`;
  if (!report.enforcing) return `${who} — not enforced`;
  return who;
}

function row(check) {
  const el = document.createElement("div");
  el.className = "access-row";

  const verdict = document.createElement("span");
  // "Cannot tell" is a third answer, and collapsing it into "no" is the
  // mistake this whole engine is written to avoid.
  if (check.unevaluated) {
    verdict.className = "access-verdict unknown";
    verdict.textContent = "?";
  } else {
    verdict.className = "access-verdict " + (check.allowed ? "allow" : "deny");
    verdict.textContent = check.allowed ? "✓" : "✗";
  }

  const action = document.createElement("code");
  action.className = "access-action";
  action.textContent = check.action;

  const why = document.createElement("div");
  why.className = "access-why muted small";
  // The rule when something decided, the reason when nothing did.
  why.textContent = check.rule || check.reason;
  why.title = check.reason;

  const head = document.createElement("div");
  head.className = "access-head";
  head.append(verdict, action);
  el.append(head, why);
  return el;
}
