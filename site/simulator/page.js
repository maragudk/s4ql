// The simulator page. It loads the engine of internal/simulator in a Worker, asks it for the run that
// the link or the controls describe, and plays the trace back: the clock, the field of events, the
// log, and the checker come from the events of the run, and the instances, the bucket, and what is
// in flight from the state at the current time, which the engine folds from the same events.
//
// A fault that the visitor adds goes into the schedule just after the current time, and the page asks
// for the run again. The engine gives the same events before that time, so the run goes on from where
// it was.

import * as R from "./run.js";

const $ = (id) => document.getElementById(id);
const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const cap = (s) => s[0].toUpperCase() + s.slice(1);
const hex = (n) => "0x" + n.toString(16);

function set(node, text) {
  if (node.textContent !== text) node.textContent = text;
}
function html(node, text) {
  if (node.dataset.html !== text) {
    node.dataset.html = text;
    node.innerHTML = text;
  }
}

// Bar codes of commit names: one bar per bit of the SHA-256, as on the main page.
function barcodeSVG(name, cls) {
  let d = "";
  for (let i = 0; i < 64; i++) {
    const nib = parseInt(name[i], 16);
    for (let b = 0; b < 4; b++) if (nib & (8 >> b)) d += "M" + (i * 4 + b) + " 0h0.72v40h-0.72z";
  }
  return `<svg class="${cls}" viewBox="0 0 256 40" preserveAspectRatio="none" shape-rendering="crispEdges" aria-hidden="true"><path fill="currentColor" d="${d}"/></svg>`;
}

// The mark: the 32 bits of "s4ql" as bars.
{
  let d = "";
  [0x73, 0x34, 0x71, 0x6c].forEach((byte, i) => {
    for (let b = 0; b < 8; b++) if (byte & (128 >> b)) d += "M" + (i * 8 + b) + " 0h1v22h-1z";
  });
  $("mark").innerHTML = `<path fill="currentColor" d="${d}"/>`;
}

// What the page knows.
let run = R.defaults(); // the run that the controls describe
let shown = null; // {run, response} that the page shows
let events = [];
let words = [];
let notable = []; // indexes of the events that are not routine
let cursor = 0;
let drawn = null; // the key of what was last drawn for the cursor
let playing = false;
let last = 0;
let engine = null;
let runs = 0; // number of runs applied
let state = null; // the state from the engine, for stateFor
let stateFor = null; // {runs, t}
let wanted = null;
let busy = false;
let stateBusy = false;
let autoplay = false; // whether the first run starts to play once it is shown

// The page tells a check what it does through data attributes on <html>: data-engine is loading,
// ready, or failed, data-runs counts the runs applied, and data-hash is the hash of the last.
const root = document.documentElement;
root.dataset.engine = "loading";
window.s4qlPage = {
  get response() { return shown && shown.response; },
  get cursor() { return cursor; },
};

function note(text) {
  set($("note"), text);
}

function fail(title, text) {
  root.dataset.engine = "failed";
  const f = $("failure");
  f.innerHTML = `<strong>${esc(title)}</strong><p>${esc(text)}</p>`;
  f.hidden = false;
  set($("engine-status"), title);
}

// The link.

function link(withTime) {
  return location.pathname + location.search + "#" + R.encode(run, withTime ? cursor : 0);
}
function writeLink(withTime) {
  history.replaceState(null, "", link(withTime));
}

// Running the engine. Only the newest wish is run, and the page shows a response only when no newer
// wish waits.

function rerun(why) {
  wanted = { run: structuredClone(run), why };
  if (!busy) pump();
}

async function pump() {
  busy = true;
  setBusy(true);
  while (wanted) {
    const w = wanted;
    wanted = null;
    set($("engine-status"), "The engine runs the code.");
    const started = performance.now();
    try {
      const response = await engine.run(R.request(w.run));
      if (wanted) continue;
      apply(w, response, performance.now() - started);
    } catch (e) {
      if (wanted) continue;
      fail("The engine refused the run.", e.message);
    }
  }
  busy = false;
  setBusy(false);
  drawn = null;
}

function setBusy(on) {
  $("field").setAttribute("aria-busy", on || !shown ? "true" : "false");
  if (shown) drawControls();
}

function apply(w, response, ms) {
  shown = { run: w.run, response };
  events = response.events;
  const names = new Map();
  words = events.map((e) => {
    if (e.kind === "process" && e.what === "opened") names.set(e.holder, e.process);
    return R.describe(e, names);
  });
  shown.names = names;
  notable = [];
  words.forEach((w, i) => { if (!w.routine) notable.push(i); });
  runs++;
  root.dataset.runs = String(runs);
  root.dataset.hash = response.hash;
  cursor = Math.min(cursor, w.run.end);
  set($("engine-status"), `The engine ran ${fmt(w.run.end)} of fake time in ${Math.round(ms)} ms: ${response.steps.toLocaleString("en")} steps of the scheduler, ${events.length.toLocaleString("en")} events.`);
  if (w.why) note(w.why);
  if (response.schedulerError) note("The scheduler failed: " + response.schedulerError);
  $("failure").hidden = true;
  root.dataset.engine = "ready";
  buildInstances();
  drawStrip();
  drawn = null;
  stateFor = null;
  if (autoplay) {
    autoplay = false;
    setPlaying(true);
  }
}

const fmt = (ms) => R.span(ms);

// The time of the run.

function upTo(t) {
  // the number of events with t <= the time.
  let lo = 0, hi = events.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (events[mid].t <= t) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}
function countBelow(list, n) {
  let lo = 0, hi = list.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (list[mid] < n) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

function setCursor(t, pause = true) {
  if (!shown) return;
  if (pause) setPlaying(false);
  cursor = Math.max(0, Math.min(shown.run.end, t));
  drawn = null;
  if (!playing) writeLink(true);
}

function setPlaying(on) {
  if (on && shown && cursor >= shown.run.end) cursor = 0;
  playing = on && !!shown;
  const b = $("play");
  b.setAttribute("aria-pressed", playing ? "true" : "false");
  b.firstChild.textContent = playing ? "Pause " : "Play ";
  last = performance.now();
  if (!playing && shown) writeLink(true);
}

function frame(ts) {
  if (playing && shown && !busy) {
    const dt = Math.min(100, Math.max(0, ts - last));
    cursor = Math.min(shown.run.end, cursor + dt * Number($("speed").value));
    if (cursor >= shown.run.end) setPlaying(false);
  }
  last = ts;
  if (shown) {
    const key = runs + ":" + cursor + ":" + $("every").checked;
    if (key !== drawn) {
      drawn = key;
      drawCursor();
    }
    fetchState();
  }
  requestAnimationFrame(frame);
}

async function fetchState() {
  if (stateBusy || busy || !shown) return;
  if (stateFor && stateFor.runs === runs && stateFor.t === cursor) return;
  stateBusy = true;
  const t = cursor, r = runs;
  try {
    const s = await engine.state(t);
    if (r === runs) {
      state = s;
      stateFor = { runs: r, t };
      drawState();
    }
  } catch (e) {
    fail("The engine could not give the state of the run.", e.message);
  } finally {
    stateBusy = false;
  }
}

// The field: one bar for each slice of fake time, as high as the events in it, drawn wide up to the
// current time and narrow after it. Marks above it are your actions, and marks below it are
// violations of the invariants.

const SLICES = 240;

function drawStrip() {
  const end = shown.run.end;
  const all = new Array(SLICES).fill(0);
  const loud = new Array(SLICES).fill(0);
  const slice = (t) => Math.min(SLICES - 1, Math.floor((t / end) * SLICES));
  events.forEach((e, i) => {
    all[slice(e.t)]++;
    if (!words[i].routine) loud[slice(e.t)]++;
  });
  const max = Math.max(1, ...all);
  const h = (n) => (n === 0 ? 0 : 4 + 22 * (Math.log(1 + n) / Math.log(1 + max)));
  let wide = "", narrow = "";
  for (let i = 0; i < SLICES; i++) {
    if (!all[i]) continue;
    const y = 36 - h(all[i]);
    wide += `M${i + 0.1} ${y}h0.8V36h-0.8z`;
    narrow += `M${i + 0.4} ${y}h0.2V36h-0.2z`;
  }
  let marks = "";
  shown.run.schedule.forEach((a) => {
    marks += `M${(a.at / end) * SLICES} 0h0.6v8h-0.6z`;
  });
  let checks = "";
  events.forEach((e) => {
    if (e.kind === "check") checks += `M${(e.t / end) * SLICES} 38h0.6v6h-0.6z`;
  });
  $("strip").setAttribute("viewBox", `0 0 ${SLICES} 44`);
  $("strip").innerHTML =
    `<defs><clipPath id="past"><rect id="past-rect" x="0" y="0" width="0" height="44"/></clipPath><clipPath id="future"><rect id="future-rect" x="0" y="0" width="${SLICES}" height="44"/></clipPath></defs>` +
    `<path fill="currentColor" clip-path="url(#future)" d="${narrow}"/>` +
    `<path fill="currentColor" clip-path="url(#past)" d="${wide}"/>` +
    `<path fill="currentColor" d="${marks}"/><path fill="currentColor" d="${checks}"/>` +
    `<rect id="cursor-line" x="0" y="0" width="0.35" height="44" fill="currentColor"/>`;
  const scrub = $("scrub");
  scrub.max = String(end);
  scrub.disabled = false;
  set($("end"), R.clock(end));
}

function drawCursor() {
  const end = shown.run.end;
  const x = (cursor / end) * SLICES;
  $("past-rect").setAttribute("width", String(x));
  $("future-rect").setAttribute("x", String(x));
  $("cursor-line").setAttribute("x", String(Math.min(SLICES - 0.35, x)));
  const scrub = $("scrub");
  if (Number(scrub.value) !== Math.round(cursor)) scrub.value = String(Math.round(cursor));
  scrub.setAttribute("aria-valuetext", R.clock(cursor) + " of " + R.clock(end));
  set($("clock"), R.clock(cursor));
  drawLog();
  drawChecks();
  drawSchedule();
  drawControls();
}

// The log of events.

function drawLog() {
  const n = upTo(cursor);
  const every = $("every").checked;
  const count = every ? n : countBelow(notable, n);
  const key = runs + ":" + every + ":" + count;
  const list = $("log");
  if (list.dataset.key === key) return;
  list.dataset.key = key;
  const out = [];
  for (let k = count - 1; k >= 0 && out.length < 200; k--) {
    const i = every ? k : notable[k];
    const e = events[i], w = words[i];
    const cls = w.loud ? "loud" : w.visitor ? "visitor" : "";
    out.push(`<li class="${cls}"><button class="t" type="button" data-t="${e.t}">${R.clock(e.t)}</button><span class="who">${esc(e.process || e.instance || (e.kind === "store" ? "S3" : ""))}</span><span class="text">${esc(w.text)}</span></li>`);
  }
  if (count > 200) out.push(`<li class="empty small">${(count - 200).toLocaleString("en")} earlier events are not shown. Go back in time to see them.</li>`);
  if (out.length === 0) out.push(`<li class="empty small">Nothing has happened yet. Press play, or step to the first event.</li>`);
  list.innerHTML = out.join("");
}

// The checker, from the events of the kind check up to the current time.

function drawChecks() {
  const n = upTo(cursor);
  const by = {};
  let violations = 0, excused = 0;
  for (let i = 0; i < n; i++) {
    const e = events[i];
    if (e.kind !== "check") continue;
    by[e.invariant] = by[e.invariant] || { bad: 0, excused: 0 };
    if (e.excused) { by[e.invariant].excused++; excused++; } else { by[e.invariant].bad++; violations++; }
  }
  const key = runs + ":" + violations + ":" + excused;
  const body = $("checks");
  if (body.dataset.key !== key) {
    body.dataset.key = key;
    body.innerHTML = R.INVARIANTS.map((inv) => {
      const c = by[inv.key] || { bad: 0, excused: 0 };
      const word = c.bad ? `Violated ${c.bad}×` : c.excused ? `Excused ${c.excused}×` : "Kept";
      return `<tr class="${c.bad ? "bad" : ""}"><td class="state caps">${word}</td><th scope="row"><code>${inv.key}</code>: ${esc(inv.text)}</th><td class="when">${esc(inv.when)}</td></tr>`;
    }).join("");
  }
  const chip = $("chip");
  set(chip, violations ? `Checker: ${violations} violated` : `Checker: ${R.INVARIANTS.length} kept`);
  chip.classList.toggle("bad", violations > 0);
  const atEnd = cursor >= shown.run.end;
  const total = shown.response.summary;
  set($("check-note"),
    (excused ? `${excused} read${excused === 1 ? "" : "s"} missed a commit on a process whose two clocks both went wrong. The design accepts that: clocks affect only reads. ` : "") +
    (atEnd ? `The run has ended. Over the whole run the checker found ${total.violations === 0 ? "no violation" : total.violations + " violation" + (total.violations === 1 ? "" : "s")}${total.excused ? `, ${total.excused} of them excused` : ""}.` : "Some checks run only at the end of the run."));
}

// The schedule, with what became of each action once its time has come.

function drawSchedule() {
  const list = $("schedule");
  const s = shown.run.schedule;
  const results = new Map();
  for (const e of events) if (e.kind === "action") results.set(e.action, e);
  const reached = s.filter((a) => a.at <= cursor).length;
  const key = runs + ":" + reached + ":" + s.length;
  $("clear").disabled = s.length === 0 || busy;
  if (list.dataset.key === key) return;
  list.dataset.key = key;
  if (s.length === 0) {
    list.innerHTML = `<li class="empty small">Nothing yet. Add a fault or an action at the current time, from the cards under "Instances" or from "Faults and actions".</li>`;
    return;
  }
  list.innerHTML = s.map((a, i) => {
    const e = results.get(i);
    let status;
    if (a.at > cursor) status = "Comes later.";
    else if (!e) status = "Its time has come, and it is not in the trace yet.";
    else if (e.what === "done") status = "Done.";
    else status = cap(e.what) + ": " + e.text + ".";
    return `<li class="${a.at > cursor ? "later" : ""}"><button class="t" type="button" data-t="${a.at}">${R.clock(a.at)}</button><span class="text">${esc(R.actionWords(a))}<span class="sub small">${esc(status)}</span></span><button class="btn" type="button" data-remove="${i}" aria-label="Remove: ${esc(R.actionWords(a))} at ${R.clock(a.at)}">Remove</button></li>`;
  }).join("");
}

// The controls that depend on the time: whether the workload is on, and whether S3 is calm.

function workloadOn() {
  let on = true;
  for (const a of shown.run.schedule) if (a.do === "workload" && a.at <= cursor) on = a.on;
  return on;
}

function drawControls() {
  const off = busy || !shown || cursor >= shown.run.end - 1;
  for (const id of ["outage", "lose", "fail", "workload"]) $(id).disabled = off;
  const calm = shown.run.schedule.some((a) => a.do === "calm" && a.at <= cursor);
  $("calm").disabled = off || calm;
  set($("calm"), calm ? "S3 is calm" : "Calm S3");
  const on = workloadOn();
  $("workload").setAttribute("aria-pressed", on ? "true" : "false");
  set($("workload"), on ? "Workload on" : "Workload off");
  $("back").disabled = !shown;
  $("step").disabled = !shown;
  $("play").disabled = !shown;
  for (const card of cards) for (const b of card.buttons) b.disabled = off;
}

// Instances.

let cards = [];

function buildInstances() {
  const n = shown.run.instances;
  if (cards.length === n) return;
  const rootEl = $("instances");
  rootEl.replaceChildren();
  cards = [];
  for (let i = 0; i < n; i++) {
    const name = R.instanceName(i);
    const el = document.createElement("article");
    el.className = "inst";
    el.dataset.instance = name;
    el.setAttribute("aria-labelledby", "inst-" + name);
    el.innerHTML = `
      <div class="inst-head">
        <div class="who"><h3 class="letter" id="inst-${name}">${name}</h3><span class="word" data-f="word"></span></div>
        <div class="tags caps"><span data-f="writer"></span><span data-f="cut"></span><span data-f="stopped"></span></div>
      </div>
      <dl class="facts">
        <div><dt class="caps">Process</dt><dd data-f="process"></dd></div>
        <div><dt class="caps">Lease</dt><dd data-f="lease"></dd></div>
        <div><dt class="caps">Cache</dt><dd><span data-f="pos"></span><span data-f="code"></span></dd></div>
        <div><dt class="caps">Clocks</dt><dd data-f="clocks"></dd></div>
        <div><dt class="caps">In flight</dt><dd data-f="flight"></dd></div>
      </dl>
      <div class="inst-actions">
        <div class="row">
          <span class="caps">Application</span>
          <button class="btn" type="button" data-a="write">Write</button>
          <button class="btn" type="button" data-a="read">Read</button>
        </div>
        <div class="row">
          <span class="caps">Process and network</span>
          <button class="btn" type="button" data-a="crash">Crash</button>
          <button class="btn" type="button" data-a="close">Close</button>
          <button class="btn" type="button" data-a="cutOff">Cut off from S3</button>
        </div>
        <div class="row">
          <span class="caps">Clocks</span>
          <button class="btn" type="button" data-a="back">Wall −60 s</button>
          <button class="btn" type="button" data-a="ahead">Wall +60 s</button>
          <button class="btn" type="button" data-a="wall">Stop wall</button>
          <button class="btn" type="button" data-a="mono">Stop monotonic</button>
        </div>
      </div>`;
    rootEl.appendChild(el);
    const f = {};
    for (const node of el.querySelectorAll("[data-f]")) f[node.dataset.f] = node;
    const b = {};
    for (const node of el.querySelectorAll("[data-a]")) {
      b[node.dataset.a] = node;
      node.addEventListener("click", () => instanceAction(name, node.dataset.a));
    }
    cards.push({ name, el, f, b, buttons: [...el.querySelectorAll("[data-a]")] });
  }
  state = null;
  drawState();
}

function instanceState(name) {
  return state && state.instances ? state.instances.find((x) => x.instance === name) : null;
}

function instanceAction(name, a) {
  const st = instanceState(name);
  switch (a) {
    case "write": case "read": return add({ do: a, instance: name });
    case "crash": return add({ do: st && st.state === "crashed" ? "restart" : "crash", instance: name });
    case "close": return add({ do: st && (st.state === "closed" || st.state === "failed") ? "open" : "close", instance: name });
    case "cutOff": return add({ do: "cutOff", instance: name, for: Number($("span").value) });
    case "back": return add({ do: "shiftWall", instance: name, by: -60000 });
    case "ahead": return add({ do: "shiftWall", instance: name, by: 60000 });
    case "wall": return add({ do: st && st.wallStopped ? "startWall" : "stopWall", instance: name });
    case "mono": return add({ do: st && st.monotonicStopped ? "startMonotonic" : "stopMonotonic", instance: name });
  }
}

const STATES = {
  starting: "starting",
  running: "running",
  closing: "closing its database",
  closed: "database closed",
  stopped: "stopped",
  crashed: "crashed",
  failed: "cannot open its database",
};

function drawState() {
  if (!state || !state.instances) {
    for (const c of cards) set(c.f.word, "…");
    return;
  }
  const t = state.t;
  const m = state.bucket.manifest;
  const log = state.bucket.log;
  const names = shown.names;
  const holderName = (id) => names.get(id) || R.short(id);
  for (const c of cards) {
    const st = instanceState(c.name);
    if (!st) continue;
    const named = m && m.lease && st.holder && m.lease.holder === st.holder;
    const writer = st.lease === "held" && named;
    c.el.classList.toggle("lit", writer);
    c.el.classList.toggle("down", st.state === "crashed");
    set(c.f.word, STATES[st.state] || st.state);
    set(c.f.writer, writer ? "writer" : "");
    set(c.f.cut, st.cutOffUntil > t ? "cut off" : state.outageUntil > t ? "S3 down" : "");
    set(c.f.stopped, st.wallStopped || st.monotonicStopped ? "clock stopped" : "");
    set(c.f.process, st.process + (st.holder ? `, holder ${R.short(st.holder)}` : ""));
    let lease;
    if (st.state === "crashed") lease = "None. The lease belongs to a running process.";
    else if (st.lease === "held") {
      lease = named
        ? `Held. The manifest names it, renewed at ${R.clock(m.lease.renewed)} by its wall clock, for ${R.span(m.lease.duration)}.`
        : `It took the lease and has not learned of a loss. The manifest names ${m && m.lease ? holderName(m.lease.holder) : "no holder"}.`;
    } else lease = m ? (m.lease ? `None. The manifest names ${holderName(m.lease.holder)}.` : "None. The manifest names no holder.") : "None. The manifest is not written yet.";
    set(c.f.lease, lease);
    let pos;
    if (st.position === R.EMPTY_LOG) pos = log.length ? `The empty log, ${log.length} commit object${log.length === 1 ? "" : "s"} behind the head.` : "The empty log, which is the head.";
    else {
      const i = log.indexOf(st.position);
      pos = st.position.slice(0, 16) + "… " + (i < 0 ? "is not in the log." : i === log.length - 1 ? "is the head." : `is ${log.length - 1 - i} behind the head.`);
    }
    set(c.f.pos, pos);
    if (c.f.code.dataset.pos !== st.position) {
      c.f.code.dataset.pos = st.position;
      c.f.code.innerHTML = st.position === R.EMPTY_LOG ? "" : barcodeSVG(st.position, "code");
    }
    const off = st.wall - t;
    set(c.f.clocks, st.state === "crashed" ? "None: the process crashed." :
      `Wall ${R.clock(st.wall)}${Math.abs(off) >= 1 ? ` (${off > 0 ? "+" : "−"}${R.span(off)})` : ""}${st.wallStopped ? ", stopped" : ""}. Monotonic ${R.clock(st.monotonic)}${st.monotonicStopped ? ", stopped" : ""}.`);
    const calls = state.calls.filter((x) => x.process === st.process).length;
    const reqs = state.requests.filter((x) => x.process === st.process && x.waiting).length;
    set(c.f.flight, `${calls} call${calls === 1 ? "" : "s"}, ${reqs} request${reqs === 1 ? "" : "s"} waited for.`);
    set(c.b.crash, st.state === "crashed" ? "Restart" : "Crash");
    set(c.b.close, st.state === "closed" || st.state === "failed" ? "Open" : "Close");
    set(c.b.wall, st.wallStopped ? "Start wall" : "Stop wall");
    set(c.b.mono, st.monotonicStopped ? "Start monotonic" : "Stop monotonic");
  }
  drawBucket();
  drawFlight();
}

function drawBucket() {
  const b = state.bucket;
  const m = b.manifest;
  const names = shown.names;
  if (m) {
    const lease = m.lease;
    html($("manifest"),
      `<div><dt class="caps">Head</dt><dd><span class="hl">${R.short(m.head)}</span> ${m.head === R.EMPTY_LOG ? "" : esc(m.head.slice(8, 24)) + "…"}</dd></div>` +
      `<div><dt class="caps">Holder</dt><dd>${lease ? `${R.short(lease.holder)}, of ${esc(names.get(lease.holder) || "?")}` : "none"}</dd></div>` +
      (lease ? `<div><dt class="caps">Duration</dt><dd>${R.span(lease.duration)}</dd></div><div><dt class="caps">Renewed</dt><dd>${R.clock(lease.renewed)}, by the wall clock of the holder</dd></div>` : "") +
      `<div><dt class="caps">Version</dt><dd>${esc((b.manifestVersion || "").slice(0, 16))}</dd></div>`);
    set($("manifest-meta"), `${b.manifestWrites} write${b.manifestWrites === 1 ? "" : "s"} of the manifest so far. The log holds ${b.log.length} commit object${b.log.length === 1 ? "" : "s"}.`);
  } else {
    html($("manifest"), `<div><dt class="caps">Head</dt><dd>Not written yet.</dd></div>`);
    set($("manifest-meta"), "");
  }
  const head = m ? m.head : R.EMPTY_LOG;
  const commits = b.commits.slice(-24).reverse();
  let out = commits.map((c) => {
    const cls = c.name === head ? "head" : c.inLog ? "" : "orphan";
    const tag = c.name === head ? "Head of the log" : c.inLog ? "In the log" : "Not in the log";
    return `<li class="commit ${cls}">${barcodeSVG(c.name, "code")}<span class="name">${c.name}</span>` +
      `<div class="meta"><span>Inserts ${R.keysText(c.keys)}${c.tables ? `, creates table ${c.tables.join(", ")}` : ""}. Written by ${esc(c.process)} at ${R.clock(c.at)}.</span><span><span class="caps">Parent</span> ${R.short(c.parent)}</span></div>` +
      `<div class="meta"><span class="caps">${tag}</span></div></li>`;
  }).join("");
  if (!out) out = `<li class="commits-empty small">No commit objects yet. The log is empty, and its head is 64 zeros.</li>`;
  if (b.commits.length > commits.length) out += `<li class="commits-empty small">${b.commits.length - commits.length} older commit objects are not shown.</li>`;
  html($("commits"), out);
}

function drawFlight() {
  const reqs = state.requests;
  set($("request-count"), `${reqs.length} in flight`);
  html($("requests"), reqs.length === 0 ? `<li class="empty small">No request is in flight.</li>` : reqs.map((r) => {
    const after = r.happened.length ? "Then: " + r.happened.map((w) => w.replace(/_/g, " ")).join(", ") + "." : "";
    const caller = r.waiting ? "The caller waits." : "The caller no longer waits.";
    return `<li><button class="t" type="button" data-t="${r.sent}">${R.clock(r.sent)}</button><span class="who">${esc(r.process)}</span><span class="text">#${r.request} ${esc(R.requestWords(r))}<span class="sub small">${esc(cap(R.phaseWords(r.phase)))}. ${caller} ${esc(after)}</span></span></li>`;
  }).join(""));
  const calls = state.calls;
  set($("call-count"), `${calls.length} in progress`);
  html($("calls"), calls.length === 0 ? `<li class="empty small">No call is in progress.</li>` : calls.map((c) =>
    `<li><button class="t" type="button" data-t="${c.started}">${R.clock(c.started)}</button><span class="who">${esc(c.process)}</span><span class="text">${c.op === "write" ? `A write of key ${c.value}` : "A read"}, by ${esc(c.caller === "visitor" ? "you" : c.caller)}.</span></li>`).join(""));
}

// Adding to the schedule.

function add(action) {
  if (!shown || busy) return;
  const at = Math.floor(cursor) + 1;
  if (at >= run.end) {
    note(`The run ends at ${R.clock(run.end)}. Make it longer under "Settings" to add more.`);
    return;
  }
  if (run.schedule.length >= 1000) {
    note("The schedule holds 1000 actions, which is as many as the engine takes.");
    return;
  }
  const a = { at, ...action };
  run.schedule = R.insert(run.schedule, a);
  writeLink(!playing);
  rerun(`Added at ${R.clock(at)}: ${R.actionWords(a)}. The run is the same up to that time.`);
}

// Settings.

function options(select, values, label, selected) {
  select.innerHTML = values.map((v) => `<option value="${v}"${v === selected ? " selected" : ""}>${esc(label(v))}</option>`).join("");
}

function drawSettings() {
  $("seed").value = hex(run.seed);
  $("seed").removeAttribute("aria-invalid");
  set($("seed-shown"), hex(run.seed));
  options($("n"), [1, 2, 3, 4, 5], String, run.instances);
  options($("end-select"), R.ENDS.includes(run.end) ? R.ENDS : [...R.ENDS, run.end].sort((a, b) => a - b), R.span, run.end);
  options($("lease"), R.LEASES.includes(run.lease) ? R.LEASES : [...R.LEASES, run.lease].sort((a, b) => a - b), R.span, run.lease);
  options($("latency"), R.LATENCIES.includes(run.latency) ? R.LATENCIES : [...R.LATENCIES, run.latency].sort((a, b) => a - b), R.span, run.latency);
  $("buggify").checked = run.buggify;
  options($("callers"), [0, 1, 2, 4, 8], (v) => v + (v === 1 ? " each" : " each"), run.callers);
  options($("think"), R.THINKS.includes(run.think) ? R.THINKS : [...R.THINKS, run.think].sort((a, b) => a - b), R.span, run.think);
  options($("writes"), [0, 20, 40, 60, 80, 100].includes(run.writes) ? [0, 20, 40, 60, 80, 100] : [0, 20, 40, 60, 80, 100, run.writes].sort((a, b) => a - b), (v) => v + "%", run.writes);
  options($("from"), ["", ...Array.from({ length: run.instances }, (_, i) => R.instanceName(i))], (v) => v || "any instance", $("from").value || "");
  set($("workload-text"), `${run.callers} caller${run.callers === 1 ? "" : "s"}, each pausing up to ${R.span(run.think)} between calls, ${run.writes}% writes`);
  $("rates").innerHTML = R.FAULTS.map((f) => {
    const v = run.faults[f.key] || 0;
    const value = f.kind === "p" ? String(v) : String(v / 1000);
    const desc = f.text || "";
    return `<li><label for="f-${f.key}">${desc ? esc(desc) : ""} <code class="key">${f.key}</code></label><span><input type="text" id="f-${f.key}" data-fault="${f.key}" value="${value}" inputmode="decimal" spellcheck="false" autocomplete="off"><span class="unit">${f.kind === "p" ? "" : "s"}</span></span></li>`;
  }).join("");
}

function changeSettings(change, why) {
  if (!engine) return;
  change();
  const dropped = run.schedule.filter((a) => (a.instance && a.instance.charCodeAt(0) - 97 >= run.instances) || a.at >= run.end).length;
  run.schedule = run.schedule.filter((a) => !((a.instance && a.instance.charCodeAt(0) - 97 >= run.instances) || a.at >= run.end));
  cursor = 0;
  drawSettings();
  writeLink(false);
  rerun(why + (dropped ? ` ${dropped} action${dropped === 1 ? "" : "s"} of the schedule did not fit the new run, and went.` : ""));
}

function parseSeed(s) {
  const m = /^\s*(0x)?([0-9a-f]{1,14})\s*$/i.exec(s);
  if (!m) return null;
  const n = parseInt(m[2], 16);
  return n <= R.MAX_SEED ? n : null;
}

// Wiring.

$("play").addEventListener("click", () => setPlaying(!playing));
$("step").addEventListener("click", () => {
  const n = upTo(cursor);
  const next = $("every").checked ? n : notable[countBelow(notable, n)];
  setCursor(next !== undefined && next < events.length ? events[next].t : shown.run.end);
});
$("back").addEventListener("click", () => {
  const lo = countBelow(events.map((e) => e.t), cursor);
  // the last shown event before the cursor.
  let i = lo - 1;
  if (!$("every").checked) while (i >= 0 && words[i].routine) i--;
  setCursor(i >= 0 ? events[i].t : 0);
});
$("scrub").addEventListener("input", (e) => setCursor(Number(e.target.value)));
$("every").addEventListener("change", () => { drawn = null; });
$("copy").addEventListener("click", async () => {
  writeLink(true);
  try {
    await navigator.clipboard.writeText(location.href);
    note("The link is copied. It holds the seed, the settings, every fault you added, and the current time.");
  } catch {
    note("Copy the address from the address bar. It holds the seed, the settings, every fault you added, and the current time.");
  }
});
document.addEventListener("click", (e) => {
  const t = e.target.closest("[data-t]");
  if (t) {
    setCursor(Number(t.dataset.t));
    return;
  }
  const r = e.target.closest("[data-remove]");
  if (r && !busy) {
    const i = Number(r.dataset.remove);
    const a = run.schedule[i];
    run.schedule = run.schedule.filter((_, k) => k !== i);
    writeLink(!playing);
    rerun(`Removed: ${R.actionWords(a)} at ${R.clock(a.at)}. The run is the same up to that time.`);
  }
});
$("clear").addEventListener("click", () => {
  if (!run.schedule.length) return;
  const first = run.schedule[0].at;
  run.schedule = [];
  writeLink(!playing);
  rerun(`Removed every action. The run is the same up to ${R.clock(first)}.`);
});
$("outage").addEventListener("click", () => add({ do: "outage", for: Number($("span").value) }));
$("calm").addEventListener("click", () => add({ do: "calm" }));
$("workload").addEventListener("click", () => add({ do: "workload", on: !workloadOn() }));
const armed = (name) => {
  const a = { do: name, request: $("purpose").value };
  if ($("from").value) a.instance = $("from").value;
  add(a);
};
$("lose").addEventListener("click", () => armed("loseAnswer"));
$("fail").addEventListener("click", () => armed("failRequest"));

options($("span"), R.SPANS, R.span, 20000);
options($("purpose"), Object.keys(R.PURPOSES), (k) => R.PURPOSES[k], "commit");

$("settings").addEventListener("submit", (e) => e.preventDefault());
$("seed").addEventListener("change", () => {
  const s = parseSeed($("seed").value);
  if (s === null) {
    $("seed").setAttribute("aria-invalid", "true");
    note(`A seed is up to 14 hexadecimal digits, at most ${hex(R.MAX_SEED)}, such as 0x7334716c.`);
    return;
  }
  changeSettings(() => (run.seed = s), `A new run with the seed ${hex(s)}.`);
});
$("new-seed").addEventListener("click", () => {
  // Only the choice of a new seed uses the browser's randomness. The run never does.
  const s = crypto.getRandomValues(new Uint32Array(1))[0];
  changeSettings(() => (run.seed = s), `A new run with the seed ${hex(s)}.`);
});
const select = (id, field, why) => $(id).addEventListener("change", () => {
  const v = Number($(id).value);
  changeSettings(() => (run[field] = v), why(v));
});
select("n", "instances", (v) => `A new run with ${v} instance${v === 1 ? "" : "s"}.`);
select("end-select", "end", (v) => `A new run of ${R.span(v)}.`);
select("lease", "lease", (v) => `A new run with leases of ${R.span(v)}.`);
select("latency", "latency", (v) => `A new run with a latency of up to ${R.span(v)}.`);
select("callers", "callers", (v) => `A new run with ${v} caller${v === 1 ? "" : "s"} on each instance.`);
select("think", "think", (v) => `A new run with pauses of up to ${R.span(v)}.`);
select("writes", "writes", (v) => `A new run with ${v}% writes.`);
$("buggify").addEventListener("change", () => {
  const on = $("buggify").checked;
  changeSettings(() => (run.buggify = on), on ? "A new run with buggify on." : "A new run with buggify off.");
});
$("rates").addEventListener("change", (e) => {
  const input = e.target.closest("[data-fault]");
  if (!input) return;
  const f = R.FAULTS.find((x) => x.key === input.dataset.fault);
  const v = Number(input.value);
  const ok = input.value !== "" && Number.isFinite(v) && v >= 0 && v <= (f.kind === "p" ? 1 : 1800);
  if (!ok) {
    input.setAttribute("aria-invalid", "true");
    note(f.kind === "p" ? "A probability is from 0 to 1." : "A time is from 0 to 1800 s.");
    return;
  }
  changeSettings(() => (run.faults = { ...run.faults, [f.key]: f.kind === "p" ? v : Math.round(v * 1000) }), `A new run with ${f.key} at ${input.value}${f.kind === "p" ? "" : " s"}.`);
});
$("faults-all").addEventListener("click", () => changeSettings(() => (run.faults = { ...R.PRESET }), "A new run with the faults that the tests of the core use."));
$("faults-none").addEventListener("click", () => changeSettings(() => (run.faults = {}), "A new run without random faults of S3."));
$("reset").addEventListener("click", () => changeSettings(() => (run = R.defaults()), "The default run."));

// Start.

async function start() {
  const fromLink = R.decode(location.hash);
  if (fromLink) {
    run = fromLink.run;
    cursor = fromLink.t;
  } else if (location.hash.length > 1) {
    note("The link does not hold a run of this page, so it shows the default run.");
  }
  drawSettings();
  requestAnimationFrame(frame);

  fetch("engine/build.json").then((r) => (r.ok ? r.json() : null)).then((b) => {
    if (b && /^[0-9a-f]{7,40}$/.test(b.commit || "")) {
      $("build").innerHTML = `Commit <a href="https://github.com/maragudk/s4ql/commit/${b.commit}"><code>${b.commit.slice(0, 7)}</code></a> of <code>maragudk/s4ql</code>, built into this page when it was deployed.`;
    }
  }).catch(() => {});

  if (typeof WebAssembly !== "object" || typeof Worker !== "function") {
    fail("This browser cannot run the engine.", "The simulator runs the storage code as WebAssembly in a Web Worker, and this browser has no WebAssembly or no Web Workers. A current version of Chrome, Firefox, Safari, or Edge has both.");
    return;
  }
  const began = performance.now();
  set($("engine-status"), "Loading the engine: about 3.5 MB of WebAssembly, then SQLite starts in a Worker.");
  try {
    const { Engine } = await import("./engine/simulator.js");
    engine = await Engine.start();
  } catch (e) {
    fail("The engine did not load.", `The page could not start the storage code in a Worker: ${e.message}. Reloading the page may help; if it does not, the engine is missing from this build of the site.`);
    return;
  }
  set($("engine-status"), `The engine is ready, ${((performance.now() - began) / 1000).toFixed(1)} s after the page asked for it.`);
  rerun(fromLink && fromLink.t > 0 ? `The run from the link, at ${R.clock(fromLink.t)}. Press play to go on.` : "");
  autoplay = !(fromLink && fromLink.t > 0) && !reduce.matches;
}

start();
