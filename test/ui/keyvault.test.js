// A vault holds three kinds of object, and the list has to say which.
//
// Before keys and certificates were listed, a vault holding only keys
// reported itself EMPTY -- a regression introduced by making keys possible at
// all, and the sort of thing nothing would have caught because the panel was
// written when a vault could only hold secrets.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { vaultCount } from "../../internal/ui/static/types-azure.js";

// Read as TEXT, not imported: inspector-azure.js imports api.js, which reads
// `location` at module load, so it cannot be loaded under node at all. The
// other UI tests do the same for the same reason.
const inspector = readFileSync(
  new URL("../../internal/ui/static/inspector-azure.js", import.meta.url),
  "utf8",
);

test("the count names each kind rather than calling everything a secret", () => {
  assert.equal(vaultCount([]), "empty");
  assert.equal(vaultCount([{ kind: "secret" }]), "1 secret");
  assert.equal(vaultCount([{ kind: "key" }, { kind: "key" }]), "2 keys");
  assert.equal(
    vaultCount([{ kind: "secret" }, { kind: "key" }, { kind: "key" }, { kind: "certificate" }]),
    "1 secret, 2 keys, 1 certificate",
  );
  // An object with no kind is a secret, so an older server still reads right.
  assert.equal(vaultCount([{}]), "1 secret");
});

test("the order is secrets, then keys, then certificates", () => {
  // Not alphabetical and not insertion order: a fixed order means the panel
  // does not reshuffle as a vault's contents change.
  const out = vaultCount([{ kind: "certificate" }, { kind: "key" }, { kind: "secret" }]);
  assert.equal(out, "1 secret, 1 key, 1 certificate");
});

test("only a secret gets a reveal button", () => {
  // The absence is the lesson rather than an omission: a key's private half
  // is never returned by anything, so there is nothing a button could show.
  // Asserted against the source because the branch is what matters -- if the
  // guard is removed, a key row grows a button that can only ever fail.
  assert.match(
    inspector,
    /if \(\(s\.kind \|\| "secret"\) === "secret"\) \{/,
    "the reveal button must be behind a kind check",
  );
  const guard = inspector.indexOf('=== "secret") {');
  const reveal = inspector.indexOf('reveal.textContent = "Reveal"');
  assert.ok(guard > 0 && reveal > guard, "Reveal is created outside the secret-only branch");
});
