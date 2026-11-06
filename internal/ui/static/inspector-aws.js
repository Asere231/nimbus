// The inspector panel AWS contributes.
//
// Azure's panels exist because its data planes do -- a container holds
// blobs, a vault holds secrets, and each has an endpoint on the
// visualizer's own API. AWS's compute resources need no endpoint: what is
// worth saying about a function, a table or a cluster is already in the
// body the store holds, so this panel reads that rather than fetching
// anything. One fewer round trip, and it works in ?fixture mode too.

import { $ } from "./dom.js";

// FACTS maps a type to the lines worth pulling out of its body. Each line
// is [label, read], where read returns a string or "" to omit the line.
const FACTS = {
  "aws::lambda::function": (b) => [
    ["Runtime", b.Runtime],
    ["Handler", b.Handler],
    ["State", b.StateReason ? `${b.State} — ${b.StateReason}` : b.State],
    ["Memory", b.MemorySize ? `${b.MemorySize} MB` : ""],
    ["Timeout", b.Timeout ? `${b.Timeout}s` : ""],
    ["Invocations", String(b.nimbusInvocations || 0)],
    ["Log group", b.nimbusLogGroup],
  ],
  "aws::dynamodb::table": (b) => [
    ["Keys", (b.KeySchema || []).map((k) => `${k.AttributeName} (${k.KeyType})`).join(", ")],
    ["Billing", (b.BillingModeSummary || {}).BillingMode],
    ["Items", String(b.ItemCount || 0)],
    ["Size", b.TableSizeBytes ? `${b.TableSizeBytes} B` : "0 B"],
  ],
  "aws::eks::cluster": (b) => [
    ["Version", b.version],
    ["Status", b.status],
    ["Endpoint", b.endpoint],
  ],
  "aws::eks::nodegroup": (b) => [
    ["Status", b.status],
    ["Desired", String((b.scalingConfig || {}).desiredSize ?? "")],
    ["Instance types", (b.instanceTypes || []).join(", ")],
  ],
  "aws::s3::bucket": (b) => [
    ["Region", b.region || b.location],
  ],
};

function showFacts(r) {
  const panel = $("#i-aws");
  // A null resource means "not your cloud": hide and say nothing.
  const read = r ? FACTS[(r.type || "").toLowerCase()] : null;
  if (!read) {
    panel.hidden = true;
    return;
  }
  const rows = read(r.body || {}).filter(([, v]) => v !== undefined && v !== null && v !== "");
  if (!rows.length) {
    panel.hidden = true;
    return;
  }
  const dl = $("#i-aws-facts");
  dl.innerHTML = "";
  for (const [label, value] of rows) {
    const dt = document.createElement("dt");
    dt.textContent = label;
    const dd = document.createElement("dd");
    dd.textContent = value;
    dl.append(dt, dd);
  }
  panel.hidden = false;
}

export const panels = [showFacts];
