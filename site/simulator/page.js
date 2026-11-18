// The simulator page. It loads the engine of internal/simulator in a Worker, asks it for a run of two
// instances with a random seed, and plays the trace back: the clock, the field, the log, and the counts
// of calls come from the events of the run, and the instances and the bucket from the state at the
// current time, which the engine folds from the same events.
//
// A fault that the visitor adds goes into the schedule just after the current time, and the page asks
// for the run again. The engine gives the same events before that time, so the run goes on from where
// it was. Before the playback reaches the end of the run, the page asks for a longer run, which has
// the same events up to the old end, so the run goes on, up to R.LIMIT.

import * as R from "./run.js";

const $ = (id) => document.getElementById(id);
const reduce = window.matchMedia("(prefers-reduced-motion: reduce)");
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const plural = (n, one, many = one + "s") => `${n.toLocaleString("en")} ${n === 1 ? one : many}`;

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
let run = fresh(); // the run that the page wants: {seed, end, schedule}
let shown = null; // {run, response} that the page shows
let events = [];
let words = []; // the words of each event, from R.story
let logged = []; // indexes of the events in the log
let calls = {}; // indexes of the calls that ended, by instance and by how
let arms = []; // indexes of the lost answers that the visitor armed
let uses = []; // indexes of the lost answers that a commit used
let names = new Map(); // the process of each holder ID
let cursor = 0;
let playing = false;
let last = 0;
let engine = null;
let runs = 0; // number of runs applied
let state = null; // the state from the engine at stateFor
let stateFor = null; // {runs, t}
let wanted = null; // the newest wish for a run that waits
let flying = null; // the wish whose run is in flight
let stateBusy = false;
let autoplay = false; // whether the page plays once the next run is shown
let failed = false;

// The page tells a check what it does through data attributes on <html>: data-engine is loading,
// ready, or failed, data-runs counts the runs applied, data-busy is "true" while a run is in flight,
// and data-hash is the hash of the run shown.
const root = document.documentElement;
root.dataset.engine = "loading";
window.s4qlPage = {
  get response() { return shown && shown.response; },
  get cursor() { return cursor; },
  get playing() { return playing; },
};

function fresh() {
  return { seed: R.newSeed(), end: R.CHUNK, schedule: [] };
}

function note(text) {
  set($("note"), text);
}

// fail shows what went wrong and stops the page until the visitor starts over, which asks the engine
// for a new run.
function fail(title, text) {
  failed = true;
  autoplay = false;
  setPlaying(false);
  if (!shown) set($("field-note"), title);
  root.dataset.engine = "failed";
  const f = $("failure");
  f.innerHTML = `<strong>${esc(title)}</strong><p>${esc(text)}</p>`;
  f.hidden = false;
  set($("engine-status"), title);
  drawControls();
}

// Running the engine. Only the newest wish is run, and the page shows a response only when no newer
// wish waits. A wish that holds stops the playback until its run is shown: a fault, which changes the
// run from the current time on, and a new run. A longer run changes nothing that the page shows, so
// the playback goes on while the engine runs it.

function rerun(why, hold) {
  wanted = { run: structuredClone(run), why, hold: hold || !!(wanted && wanted.hold) || !!(flying && flying.hold) };
  if (!flying) pump();
}

const holding = () => !!((wanted && wanted.hold) || (flying && flying.hold));

async function pump() {
  while (wanted) {
    const w = wanted;
    wanted = null;
    flying = w;
    setBusy();
    const started = performance.now();
    try {
      const response = await engine.run(R.request(w.run));
      if (wanted) continue;
      apply(w, response, performance.now() - started);
    } catch (e) {
      if (wanted) continue;
      // The page asks only for runs that the engine takes, so a refusal is a fault of the page or of
      // the Worker, and the engine has forgotten the run shown.
      flying = null;
      setBusy();
      fail("The engine refused the run.", `${e.message}. Start over to ask for a new run.`);
      return;
    }
  }
  flying = null;
  setBusy();
}

// setBusy shows whether the playback waits for the engine: for a run that holds it, or at the end of
// the run shown while a longer run is on its way.
function setBusy() {
  root.dataset.busy = flying ? "true" : "false";
  const waits = holding() || !shown || (!!flying && cursor >= shown.run.end && !atLimit());
  $("field").setAttribute("aria-busy", waits ? "true" : "false");
  if (shown) drawControls();
}

function apply(w, response, ms) {
  // A run that differs only in its end has the same events up to the end shown, and so the same
  // state at every time shown.
  const longer = shown && w.run.seed === shown.run.seed && JSON.stringify(w.run.schedule) === JSON.stringify(shown.run.schedule);
  const other = !shown || w.run.seed !== shown.run.seed;
  shown = { run: w.run, response };
  events = response.events;
  words = R.story(events, w.run.schedule);
  logged = [];
  calls = {};
  arms = [];
  uses = [];
  names = new Map();
  events.forEach((e, i) => {
    if (words[i].tier) logged.push(i);
    if (e.kind === "process" && e.what === "opened") names.set(e.holder, e.process);
    if (e.kind === "action" && e.do === "loseAnswer") arms.push(i);
    if (e.kind === "request" && e.what === "answer_lost") uses.push(i);
    if (e.kind === "call" && (e.what === "committed" || e.what === "served" || e.what === "refused")) {
      const c = (calls[e.instance] ||= { committed: [], served: [], refused: [] });
      c[e.what].push(i);
    }
  });
  if (!longer) {
    state = null;
    stateFor = null;
  }
  if (other) cursor = 0;
  runs++;
  root.dataset.runs = String(runs);
  root.dataset.hash = response.hash;
  set($("engine-status"), `The engine ran ${R.span(w.run.end)} of fake time in ${Math.round(ms)} ms.`);
  note(w.why || "");
  if (response.schedulerError) note("The scheduler failed: " + response.schedulerError);
  $("failure").hidden = true;
  root.dataset.engine = "ready";
  drawStrip();
  drawCursor();
  drawState();
  if (autoplay) {
    autoplay = false;
    setPlaying(true);
  }
}

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

const atLimit = () => !!shown && cursor >= R.LIMIT;

function setPlaying(on) {
  playing = on && !!shown && !failed && !atLimit();
  const b = $("play");
  b.setAttribute("aria-pressed", playing ? "true" : "false");
  b.firstChild.textContent = playing ? "Pause " : "Play ";
  last = performance.now();
}

function frame(ts) {
  if (playing && shown && !holding()) {
    const dt = Math.min(100, Math.max(0, ts - last));
    cursor = Math.min(shown.run.end, cursor + dt * Number($("speed").value));
    if (atLimit()) setPlaying(false);
  }
  last = ts;
  if (shown && !failed) {
    // Ask for a longer run ahead of time, so that it is there before the playback needs it: at least
    // R.LEAD of fake time ahead, and 2 s of true time at a high speed.
    const lead = Math.max(R.LEAD, 2000 * Number($("speed").value));
    if (run.seed === shown.run.seed && run.end < R.LIMIT && run.end - cursor <= lead) {
      run.end = Math.min(R.LIMIT, run.end + R.CHUNK);
      rerun("", false);
    }
    drawCursor();
    fetchState(ts);
  }
  requestAnimationFrame(frame);
}

// fetchState asks the engine for the state at the current time, at most every STATE_MS of true time
// while the run plays, since the engine folds it from the first event on.
const STATE_MS = 100;
let stateAsked = -Infinity;

async function fetchState(ts) {
  if (stateBusy || flying || !shown || failed) return;
  if (stateFor && stateFor.runs === runs && stateFor.t === cursor) return;
  if (stateFor && stateFor.runs === runs && playing && ts - stateAsked < STATE_MS) return;
  stateAsked = ts;
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
    fail("The engine could not give the state of the run.", `${e.message}. Start over to ask for a new run.`);
  } finally {
    stateBusy = false;
  }
}

// The field: one bar for each slice of fake time up to the limit, as high as the events in it, drawn
// up to the current time. Marks above it are the visitor's faults.

const SLICES = 240;

function drawStrip() {
  const slice = (t) => Math.min(SLICES - 1, Math.floor((t / R.LIMIT) * SLICES));
  const all = new Array(SLICES).fill(0);
  for (const e of events) all[slice(e.t)]++;
  // A fixed scale, so that the bars keep their heights when the run grows.
  const h = (n) => (n === 0 ? 0 : 3 + 17 * Math.min(1, Math.log(1 + n) / Math.log(1 + 400)));
  let bars = "";
  for (let i = 0; i < SLICES; i++) if (all[i]) bars += `M${i + 0.15} ${28 - h(all[i])}h0.7V28h-0.7z`;
  let marks = "";
  for (const a of shown.run.schedule) marks += `M${(a.at / R.LIMIT) * SLICES} 0h0.6v5h-0.6z`;
  set($("field-note"), "");
  $("strip").setAttribute("viewBox", `0 0 ${SLICES} 28`);
  $("strip").innerHTML =
    `<defs><clipPath id="past"><rect id="past-rect" x="0" y="0" width="0" height="28"/></clipPath></defs>` +
    `<g clip-path="url(#past)"><path fill="currentColor" d="${bars}"/><path fill="currentColor" d="${marks}"/></g>` +
    `<rect id="cursor-line" x="0" y="0" width="0.35" height="28" fill="currentColor"/>`;
}

let drawn = null; // the key of what drawCursor last drew

function drawCursor() {
  const key = runs + ":" + cursor;
  if (key === drawn) return;
  drawn = key;
  const x = (cursor / R.LIMIT) * SLICES;
  $("past-rect").setAttribute("width", String(x));
  $("cursor-line").setAttribute("x", String(Math.min(SLICES - 0.35, x)));
  set($("clock"), R.clock(cursor));
  drawLog();
  drawCalls();
  setBusy();
}

// The log.

function drawLog() {
  const count = countBelow(logged, upTo(cursor));
  const list = $("log");
  const key = runs + ":" + count;
  if (list.dataset.key === key) return;
  list.dataset.key = key;
  const out = [];
  for (let k = count - 1; k >= 0 && out.length < 100; k--) {
    const i = logged[k];
    const e = events[i], w = words[i];
    out.push(`<li class="${w.tier}"><span class="t">${R.clock(e.t)}</span><span class="text">${esc(w.text)}</span></li>`);
  }
  if (count > 100) out.push(`<li class="empty small">${plural(count - 100, "earlier line is", "earlier lines are")} not shown.</li>`);
  if (out.length === 0) out.push(`<li class="empty small">Nothing has happened yet.</li>`);
  list.innerHTML = out.join("");
}

// The calls of each instance that ended up to the current time, from the events.

function drawCalls() {
  const n = upTo(cursor);
  for (const c of cards) {
    const k = calls[c.name] || { committed: [], served: [], refused: [] };
    const w = countBelow(k.committed, n), r = countBelow(k.served, n), x = countBelow(k.refused, n);
    let text = `${plural(w, "write")} and ${plural(r, "read")} served, ${x.toLocaleString("en")} refused`;
    if (x) {
      const reason = String(events[k.refused[x - 1]].error || "").split(": ").pop();
      text += `, the last because ${R.REASONS[reason] || reason}`;
    }
    text += ".";
    set(c.f.calls, text);
  }
}

// The controls.

function drawControls() {
  const ready = !!shown && !failed;
  const end = atLimit();
  $("play").disabled = !ready || end;
  $("again").disabled = !engine;
  $("again").classList.toggle("call", end);
  const lim = $("limit");
  lim.hidden = !end;
  set(lim, end ? `The run has reached ${R.span(R.LIMIT)} of fake time, as far as this page runs it. Start over for a new run.` : "");
  // A fault goes in just after the current time, and needs the state to know which faults apply.
  const off = !ready || holding() || !state || end || Math.floor(cursor) + 1 >= run.end;
  $("outage").disabled = off;
  $("lose").disabled = off;
  for (const c of cards) {
    const st = instanceState(c.name);
    // A crashed process can only be restarted: the engine ignores every other action for it.
    const crashed = !!st && st.state === "crashed";
    for (const b of c.buttons) b.disabled = off || !st || (crashed && b.dataset.a !== "crash");
  }
}

// Instances.

const cards = ["a", "b"].map((name) => {
  const el = document.querySelector(`[data-instance="${name}"]`);
  const f = {};
  for (const node of el.querySelectorAll("[data-f]")) f[node.dataset.f] = node;
  const b = {};
  for (const node of el.querySelectorAll("[data-a]")) {
    b[node.dataset.a] = node;
    node.addEventListener("click", () => instanceFault(name, node.dataset.a));
  }
  return { name, el, f, b, buttons: [...el.querySelectorAll("[data-a]")] };
});

function instanceState(name) {
  return state && state.instances ? state.instances.find((x) => x.instance === name) : null;
}

function instanceFault(name, a) {
  const st = instanceState(name);
  if (!st) return;
  switch (a) {
    case "crash": return add({ do: st.state === "crashed" ? "restart" : "crash", instance: name });
    case "cutOff": return add({ do: "cutOff", instance: name, for: R.SPAN });
    case "clock": return add({ do: st.monotonicStopped ? "startMonotonic" : "stopMonotonic", instance: name });
  }
}

const STATES = {
  starting: "starting",
  closing: "closing its database",
  closed: "database closed",
  stopped: "stopped",
  crashed: "crashed",
  failed: "cannot open its database",
};

function drawState() {
  if (!state || !state.instances) {
    for (const c of cards) set(c.f.word, "…");
    set($("s3-word"), "…");
    drawControls();
    return;
  }
  const t = state.t;
  const m = state.bucket.manifest;
  const log = state.bucket.log;
  const holderName = (id) => names.get(id) || R.short(id);
  for (const c of cards) {
    const st = instanceState(c.name);
    if (!st) continue;
    const named = !!(m && m.lease && st.holder && m.lease.holder === st.holder);
    const writer = st.lease === "held" && named;
    c.el.classList.toggle("lit", writer);
    c.el.classList.toggle("down", st.state === "crashed");
    let word = STATES[st.state];
    if (st.state === "running") word = writer ? "serves" : st.lease === "held" ? "believes it holds the lease" : "stands by";
    set(c.f.word, word || st.state);
    set(c.f.process, st.process !== c.name ? "process " + st.process : "");
    set(c.f.cut, st.cutOffUntil > t ? "cut off" : "");
    set(c.f.stopped, st.monotonicStopped ? "clock stopped" : "");
    let lease;
    if (st.state === "crashed") lease = "None. A crashed process holds nothing.";
    else if (writer) lease = `Held. The manifest names it, renewed at ${R.clock(m.lease.renewed)} by its wall clock, for ${R.span(m.lease.duration)}.`;
    else if (st.lease === "held") lease = `It took the lease and has not learned that it lost it. The manifest names ${m && m.lease ? holderName(m.lease.holder) : "no holder"}.`;
    else lease = m && m.lease ? `None. The manifest names ${holderName(m.lease.holder)}.` : "None.";
    set(c.f.lease, lease);
    let pos;
    if (st.position === R.EMPTY_LOG) pos = log.length ? `The empty log, ${plural(log.length, "commit")} behind the head.` : "The empty log, which is the head.";
    else {
      const i = log.indexOf(st.position);
      pos = R.short(st.position) + (i < 0 ? ", which is not in the log." : i === log.length - 1 ? ", the head of the log." : `, ${plural(log.length - 1 - i, "commit")} behind the head.`);
    }
    set(c.f.pos, pos);
    if (c.f.code.dataset.pos !== st.position) {
      c.f.code.dataset.pos = st.position;
      c.f.code.innerHTML = st.position === R.EMPTY_LOG ? "" : barcodeSVG(st.position, "code");
    }
    // The page offers no fault of the wall clock, so the card tells only whether the monotonic clock
    // runs, and does not change with every frame.
    let clocks = "Both run.";
    if (st.state === "crashed") clocks = "None: the process crashed.";
    else if (st.monotonicStopped) clocks = `The monotonic clock stands still at ${R.clock(st.monotonic)}, and with it every timer of the process.`;
    set(c.f.clocks, clocks);
    set(c.b.crash, st.state === "crashed" ? "Restart" : "Crash");
    set(c.b.clock, st.monotonicStopped ? "Start its monotonic clock" : "Stop its monotonic clock");
  }
  set($("s3-word"), state.outageUntil > t ? "unreachable" : "reachable");
  drawBucket(holderName);
  drawControls();
}

// armed counts the lost answers that the visitor armed up to the current time and that no commit has
// used yet.
function armed() {
  const n = upTo(cursor);
  return countBelow(arms, n) - countBelow(uses, n);
}

function drawBucket(holderName) {
  const b = state.bucket;
  const m = b.manifest;
  const t = state.t;
  const k = armed();
  let rows = "";
  if (m) {
    rows += `<div><dt class="caps">Head</dt><dd><span class="hl">${esc(R.short(m.head))}</span>${m.head === R.EMPTY_LOG ? "" : `, ${plural(b.log.length, "commit")} in the log`}</dd></div>`;
    rows += `<div><dt class="caps">Lease</dt><dd>${m.lease ? `${esc(holderName(m.lease.holder))}, renewed at ${R.clock(m.lease.renewed)} by its wall clock` : "Nobody holds it."}</dd></div>`;
    rows += `<div><dt class="caps">Manifest</dt><dd>${plural(b.manifestWrites, "write")}, each a compare-and-swap on its version.</dd></div>`;
  } else rows += `<div><dt class="caps">Manifest</dt><dd>Not written yet.</dd></div>`;
  if (state.outageUntil > t) rows += `<div><dt class="caps">Outage</dt><dd>Unreachable for every process until ${R.clock(state.outageUntil)}.</dd></div>`;
  if (k > 0) rows += `<div><dt class="caps">Armed</dt><dd>The answer to the next commit will be lost${k > 1 ? `, ${k} times` : ""}.</dd></div>`;
  html($("manifest"), rows);

  const head = m ? m.head : R.EMPTY_LOG;
  const commits = b.commits.slice(-24).reverse();
  let out = commits.map((c) => {
    const cls = c.name === head ? "head" : c.inLog ? "" : "orphan";
    const tag = c.name === head ? "Head of the log" : c.inLog ? "In the log" : "Not in the log";
    return `<li class="commit ${cls}">${barcodeSVG(c.name, "code")}<span class="name">${c.name}</span>` +
      `<div class="meta"><span>Inserts ${R.keysText(c.keys)}. Written by ${esc(c.process)} at ${R.clock(c.at)}.</span><span><span class="caps">Parent</span> ${R.short(c.parent)}</span></div>` +
      `<div class="meta"><span class="caps">${tag}</span></div></li>`;
  }).join("");
  if (!out) out = `<li class="empty small">No commit objects yet. The log is empty, and its head is 64 zeros.</li>`;
  if (b.commits.length > commits.length) out += `<li class="empty small">${plural(b.commits.length - commits.length, "older commit object is", "older commit objects are")} not shown.</li>`;
  html($("commits"), out);
  set($("commit-count"), plural(b.commits.length, "object") + ", newest first.");
}

// Adding a fault.

function add(action) {
  if (!shown || holding() || failed) return;
  const at = Math.floor(cursor) + 1;
  if (at >= run.end) return;
  const a = { at, ...action };
  run.schedule = R.insert(run.schedule, a);
  rerun("", true);
}

// Wiring.

$("play").addEventListener("click", () => setPlaying(!playing));
$("again").addEventListener("click", () => {
  // A run that plays goes on playing. A run that stopped at the limit plays again, unless the visitor
  // asks for reduced motion.
  autoplay = playing || ((atLimit() || failed) && !reduce.matches);
  setPlaying(false);
  if (failed) {
    failed = false;
    $("failure").hidden = true;
    root.dataset.engine = "ready";
  }
  run = fresh();
  rerun("A new run.", true);
});
$("outage").addEventListener("click", () => add({ do: "outage", for: R.SPAN }));
$("lose").addEventListener("click", () => add({ do: "loseAnswer", request: "commit" }));

// Start.

async function start() {
  requestAnimationFrame(frame);

  fetch("engine/build.json").then((r) => (r.ok ? r.json() : null)).then((b) => {
    if (b && /^[0-9a-f]{7,40}$/.test(b.commit || "")) {
      $("build").innerHTML = `commit <a href="https://github.com/maragudk/s4ql/commit/${b.commit}"><code>${b.commit.slice(0, 7)}</code></a> of <code>maragudk/s4ql</code>.`;
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
  autoplay = !reduce.matches;
  rerun("", true);
}

start();
