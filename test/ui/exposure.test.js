// A resource anyone can reach, badged on the graph.
//
// The judgement is NOT made here. The server derives `public` and
// `publicReason` in saveBucket by asking the authorization evaluator what
// an anonymous principal may do, and this only reads what it was told --
// so the badge cannot disagree with what a request actually gets. The
// case that makes the distinction worth the trouble is a bucket policy
// granting `*` that a public access block neutralises: the policy still
// says `*`, and a badge computed from the JSON here would be wrong.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { exposure } from "../../internal/ui/static/types.js";

const dir = new URL("../../internal/ui/static/", import.meta.url);
const fixture = JSON.parse(readFileSync(new URL("fixture.json", dir), "utf8"));

function bucket(name) {
  const r = fixture.resources.find((x) => x.name === name);
  assert.ok(r, `the fixture has no ${name}`);
  return r;
}

test("a bucket the server flagged public is badged, with its reason", () => {
  const open = exposure(bucket("nimbus-public-site"));
  assert.ok(open, "a public bucket is not badged");
  assert.equal(open.label, "Public");
  assert.match(open.reason, /PublicRead/,
    "the reason does not name the statement that made it public");
});

test("a bucket with no policy is not badged", () => {
  assert.equal(exposure(bucket("nimbus-private-data")), null);
});

// The flag is the server's answer, not a reading of the policy. A bucket
// behind a public access block still HAS a policy granting `*`, and must
// not be badged -- which is only possible because this reads `public`
// rather than the document.
test("a policy granting everyone is not enough on its own", () => {
  const b = JSON.parse(JSON.stringify(bucket("nimbus-public-site")));
  delete b.body.public;
  delete b.body.publicReason;
  assert.ok(b.body.policy.includes('"Principal":"*"'),
    "the policy should still grant everyone, which is the point");
  assert.equal(exposure(b), null,
    "the badge was computed from the policy rather than from the server's answer");
});

test("nothing else is badged, on any cloud", () => {
  for (const r of fixture.resources) {
    if (r.name === "nimbus-public-site") continue;
    assert.equal(exposure(r), null, `${r.cloud} ${r.type} ${r.name} is badged`);
  }
});

test("a cloud with no exposure model answers nothing rather than throwing", () => {
  assert.equal(exposure({ cloud: "azure", type: "Microsoft.Storage/storageAccounts", body: {} }), null);
  assert.equal(exposure({ cloud: "gcp", type: "storage.googleapis.com/Bucket", body: {} }), null);
  assert.equal(exposure({ cloud: "nope", type: "x", body: {} }), null);
  assert.equal(exposure(null), null);
});

// An external stylesheet does not apply to a detached SVG, so anything
// the graph draws has to be styled in export.js as well -- the same rule
// the network view's classes are held to.
test("the badge is styled in the exported SVG", () => {
  const sheet = readFileSync(new URL("export.js", dir), "utf8");
  const css = sheet.slice(sheet.indexOf("const EXPORT_CSS = `") + 20);
  const block = css.slice(0, css.indexOf("`"));
  assert.ok(block.includes(".exposed rect"), "the badge's box has no rule in the export");
  assert.ok(block.includes(".exposed text"), "the badge's label has no rule in the export");
});
