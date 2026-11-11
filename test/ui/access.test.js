// The access panel renders a cloud-neutral decision shape.
//
// What is worth testing here is NOT which permission a type needs -- that
// lives on the server, deliberately, so the explanation cannot drift from
// the behaviour. It is that this module renders whatever shape it is
// given, including the two cases that are easy to get wrong: an
// unevaluated check is not a denial, and an unrestricted principal is not
// a column of allows.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

const source = readFileSync(new URL("../../internal/ui/static/access.js", import.meta.url), "utf8");
const fixture = JSON.parse(
  readFileSync(new URL("../../internal/ui/static/fixture.json", import.meta.url), "utf8"),
);

test("the fixture carries access reports, so ?fixture exercises the panel", () => {
  assert.ok(fixture.access, "fixture.json has no access reports");
  const reports = Object.values(fixture.access);
  assert.ok(reports.length >= 1);
  for (const report of reports) {
    assert.ok(Array.isArray(report.checks) && report.checks.length, "a report has no checks");
    for (const check of report.checks) {
      assert.equal(typeof check.action, "string");
      assert.equal(typeof check.allowed, "boolean");
      assert.equal(typeof check.reason, "string");
      assert.ok(check.reason.length, `${check.action} has no reason`);
    }
  }
});

test("the fixture includes an unevaluated check, which is a third answer", () => {
  // "Nimbus cannot tell" must not be collapsed into "no": that is the
  // mistake the whole engine is written to avoid, so the panel has to be
  // exercised against one.
  const checks = Object.values(fixture.access).flatMap((r) => r.checks);
  const unknown = checks.filter((c) => c.unevaluated);
  assert.ok(unknown.length >= 1, "no unevaluated check in the fixture");
  for (const c of unknown) {
    assert.equal(c.allowed, false, "an unevaluated check must not read as allowed");
  }
});

test("the fixture includes both an allow and a deny", () => {
  const checks = Object.values(fixture.access).flatMap((r) => r.checks);
  assert.ok(checks.some((c) => c.allowed), "no allowed check");
  assert.ok(checks.some((c) => !c.allowed), "no denied check");
});

test("an allowed check names the rule that decided", () => {
  const allowed = Object.values(fixture.access)
    .flatMap((r) => r.checks)
    .filter((c) => c.allowed);
  for (const c of allowed) {
    assert.ok(c.rule && c.rule.length, `${c.action} is allowed by nothing named`);
  }
});

// The panel is shared across all three clouds, so it must not key on a
// type name -- test/ui/clouds.test.js enforces that for shared modules
// generally, and this says why it matters here: the server answers in one
// shape precisely so this file can stay ignorant.
test("the panel distinguishes unevaluated from denied", () => {
  assert.match(source, /unevaluated/, "the panel ignores the unevaluated flag");
  // The three classes the stylesheet styles.
  for (const cls of ["allow", "deny", "unknown"]) {
    assert.ok(source.includes(cls), `the panel never renders a ${cls} verdict`);
  }
});

test("the panel says when enforcement is off", () => {
  // With --enforce-iam off the answers are what WOULD happen, and
  // presenting them as what IS happening would be the misleading kind of
  // wrong this project cares most about.
  assert.match(source, /enforcing/, "the panel ignores whether enforcement is on");
});
