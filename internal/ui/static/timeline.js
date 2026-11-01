// The timeline: a bar under the canvas that replays one run step by step.

import { $ } from "./dom.js";
import { state } from "./state.js";
import { getRuns, getRun } from "./api.js";
import { render } from "./render.js";
import { steps, stateAt, callFor, summarise } from "./replay.js";
import { shortPath } from "./feed.js";

const PLAY_MS = 450; // one step per beat, slow enough to follow

let runs = [];
let current = null;   // the loaded run, events included
let stepList = [];
let at = 0;
let timer = null;

export async function refreshRuns() {
  try {
    runs = await getRuns();
  } catch (_) {
    return;
  }
  const picker = $("#tl-run");
  const keep = picker.value;
  picker.innerHTML = "";
  const liveOption = document.createElement("option");
  liveOption.value = "";
  liveOption.textContent = runs.length ? "Live · pick a run to replay" : "Live";
  picker.appendChild(liveOption);
  for (const r of runs) {
    const o = document.createElement("option");
    o.value = r.id;
    o.textContent = summarise(r);
    picker.appendChild(o);
  }
  picker.value = keep;
  if (picker.value !== keep) picker.value = ""; // the run scrolled out of the journal
  $("#timeline").hidden = runs.length === 0;
}

async function load(id) {
  pause();
  if (!id) return live();
  const run = await getRun(id);
  if (!run) return live();
  current = run;
  stepList = steps(run.events);
  $("#tl-scrub").max = String(stepList.length);
  // Start at the BEGINNING and play.
  //
  // It used to open on the last step, so as not to blank the graph. That
  // was exactly wrong: the end of a run looks identical to the live graph,
  // so picking a run appeared to do nothing at all. Picking a run is a
  // request to watch it happen.
  show(0);
  play();
}

// show draws the graph as it stood after `n` steps.
function show(n) {
  if (!current) return;
  at = Math.min(Math.max(n, 0), stepList.length);
  state.replay = stateAt(current.events, at, state.resources);
  state.current = at > 0 ? String(stepList[at - 1].resourceId || "").toLowerCase() : null;
  $("#tl-scrub").value = String(at);
  $("#tl-step").textContent = `${at} / ${stepList.length}`;

  const step = at > 0 ? stepList[at - 1] : null;
  const call = at > 0 ? callFor(current.events, at - 1) : null;
  $("#tl-call").textContent = step
    ? (call ? `${call.method} ${shortPath(call.path)} → ${call.status}` : `${step.action} ${step.name || ""}`)
    : "Nothing yet — this is the moment before the run started.";
  $("#timeline").classList.add("replaying");
  render();
}

function live() {
  pause();
  current = null;
  stepList = [];
  state.replay = null;
  state.current = null;
  $("#tl-run").value = "";
  $("#tl-step").textContent = "";
  $("#tl-call").textContent = "";
  $("#timeline").classList.remove("replaying");
  render();
}

function pause() {
  if (timer) clearInterval(timer);
  timer = null;
  $("#tl-play").textContent = "▶";
  $("#tl-play").title = "Play";
}

function play() {
  // Play with nothing picked plays the most recent run, rather than being
  // a button that does nothing.
  if (!current) {
    if (runs.length) {
      $("#tl-run").value = runs[0].id;
      load(runs[0].id);
    }
    return;
  }
  if (timer) return pause();
  // Pressing play at the end starts again from the beginning, which is what
  // anyone who has just watched it once wants.
  if (at >= stepList.length) show(0);
  $("#tl-play").textContent = "⏸";
  $("#tl-play").title = "Pause";
  timer = setInterval(() => {
    if (at >= stepList.length) return pause();
    show(at + 1);
  }, PLAY_MS);
}

export function initTimeline() {
  $("#tl-run").addEventListener("change", (ev) => load(ev.target.value));
  $("#tl-first").addEventListener("click", () => { pause(); show(0); });
  $("#tl-last").addEventListener("click", () => { pause(); show(stepList.length); });
  $("#tl-play").addEventListener("click", play);
  $("#tl-live").addEventListener("click", live);
  $("#tl-scrub").addEventListener("input", (ev) => { pause(); show(Number(ev.target.value)); });
  refreshRuns();
}

// While a run is being replayed the graph belongs to the timeline, so a live
// event must not redraw it -- it still goes in the feed and into the live
// store, it just does not appear until you go back to live.
export function isReplaying() {
  return state.replay !== null;
}
