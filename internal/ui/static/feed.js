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
  // Default view: what changed, plus anything that failed or is not implemented.
  // "Show all API calls" adds the raw requests (reads, tokens, writes).
  const flagged = e.kind === "request" && (e.status >= 400 || /unimplemented|unrecognised/.test(e.note || ""));
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
    if (e.note && /unimplemented|unrecognised|accepted/.test(e.note)) {
      const n = document.createElement("span");
      n.className = /accepted/.test(e.note) ? "note quiet" : "note";
      n.textContent = e.note;
      what.appendChild(n);
    }
  } else {
    verb.className = "verb " + e.action;
    verb.textContent = e.action;
    if (e.kind === "reset") {
      what.textContent = "All resources deleted";
    } else {
      const [, , human] = typeStyle(e.resourceType);
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
