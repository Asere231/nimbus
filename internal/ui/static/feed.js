// The activity feed: one line per event, newest first.

import { $ } from "./dom.js";
import { typeStyle } from "./types.js";

const feed = () => $("#feed");

// shortPath trims the parts of an ARM path that are the same on every line,
// so the part that differs is what you read first.
export function shortPath(p) {
  return String(p || "")
    .replace(/\/subscriptions\/[^/]+/i, "/sub")
    .replace(/\/resourceGroups\//i, "/rg/")
    .replace(/\/providers\//gi, "/p/");
}

// addToFeed renders one event. onSelect is called with a resource key when a
// resource line is clicked.
export function addToFeed(e, animate, onSelect) {
  const li = document.createElement("li");
  li.dataset.kind = e.kind;
  // Default view: what changed, plus anything that failed or is not
  // implemented. "Show all API calls" adds the raw requests.
  //
  // A note beginning "expected:" means the server MEANT to answer that
  // way -- S3 reports an unset bucket sub-resource as a 404, and the
  // provider reads a dozen of them on every refresh. Flagging those
  // paints a clean plan red and teaches people to ignore the colour.
  const expected = /^expected:/.test(e.note || "");
  const flagged = e.kind === "request" && !expected &&
    (e.status >= 400 || /unimplemented|unrecognised|denied/.test(e.note || ""));
  const quiet = e.kind === "request" && !flagged;
  if (quiet) li.dataset.quiet = "1";
  if (quiet && !$("#show-reads").checked) li.hidden = true;
  if (animate) li.className = "new";

  const t = document.createElement("time");
  t.textContent = new Date(e.time).toLocaleTimeString([], { hour12: false });
  const verb = document.createElement("span");
  const what = document.createElement("span");
  what.className = "what";

  if (e.kind === "request") {
    verb.className = "verb call";
    verb.textContent = e.method;
    const ok = e.status < 400;
    what.innerHTML = `<span class="${ok ? "code-ok" : "code-err"}">${e.status}</span> <span class="path"></span>`;
    what.querySelector(".path").textContent = shortPath(e.path);
    // "denied:" is an authorization refusal, and it carries the only
    // explanation there is -- the principal, the action and the rule that
    // decided. A denial nobody can see is worse than no enforcement,
    // because then an apply just fails with nothing to look at.
    if (e.note && /unimplemented|unrecognised|accepted|expected|denied/.test(e.note)) {
      const n = document.createElement("span");
      n.className = /accepted|expected/.test(e.note) ? "note quiet" : "note";
      n.textContent = e.note;
      what.appendChild(n);
    }
  } else {
    verb.className = "verb " + e.action;
    verb.textContent = e.action;
    if (e.kind === "reset") {
      what.textContent = "All resources deleted";
    } else {
      const [, , human] = typeStyle(e.resourceType, e.cloud);
      what.innerHTML = `<b></b> <span class="t"></span>`;
      what.querySelector("b").textContent = e.name;
      what.querySelector(".t").textContent = human;
      what.style.cursor = "pointer";
      what.onclick = () => onSelect(e.resourceId.toLowerCase());
    }
  }
  li.append(t, verb, what);
  feed().prepend(li);
  while (feed().children.length > 500) feed().lastChild.remove();
}

export function clearFeed() {
  feed().innerHTML = "";
}

export function setShowReads(on) {
  feed().querySelectorAll("li[data-quiet]").forEach((li) => { li.hidden = !on; });
}
