// The simulator page. It loads the engine of internal/simulator in a Worker, starts a live session of
// two instances with a random seed, and waits for Play. While it plays, the page advances the
// session's fake time by the real time that passed, or five times that with turbo, one step at a time,
// and asks for the state after each step. An action of the visitor goes to the engine at once, paused
// or playing, and happens at the session's fake time.
//
// The engine does not keep the events of a session, and neither does the page: it keeps the last
// lines of the log, the counts of the calls, and the bars of the last WINDOW of fake time.

import * as R from "./run.js";

const $ = (id) => document.getElementById(id);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]);
const count = (n) => n.toLocaleString("en");

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

const TURBO = 5; // the speed with turbo, in fake time for real time
const FRAME = 100; // the most real time, in ms, that one animation frame counts, so a hidden tab does not jump ahead
const STEP = 250; // the most real time, in ms, that one step of the session covers; more is dropped
const MAX_ADVANCE = 1800000; // the most fake time that one command advance takes
const COMMITS = 8; // the commit objects that the page asks for and shows
const LINES = 100; // the lines of the log that the page keeps
const WINDOW = 120000; // the fake time that the field shows
const SLICES = 240; // the bars of the field
const SLICE = WINDOW / SLICES;
const JUMP = 60000; // a jump of a wall clock: longer than the lease of 30 s
const MEMORY = 1 << 30; // SQLite's memory at which the page stops

// What the page knows.
let engine = null;
let t = 0; // the fake time of the session
let state = null; // the answer of the engine to now
let playing = false;
let turbo = false;
let halted = false;
let busy = false; // whether a command is on its way to the engine
let acting = false; // whether that command is an action
let debt = 0; // the real time in ms that playback has not yet advanced
let last = 0; // the time of the last animation frame
let lastStep = null; // {real, fake} of the last advance by playback
const jobs = []; // commands that wait: {act} or {by}
const tell = R.teller();
const lines = []; // the last LINES lines of the log, newest first
let linesTotal = 0;
const calls = {}; // by instance: {w, r, x, why}
const bars = new Map(); // events by slice of fake time
const marks = []; // the times of the visitor's actions
const starts = {}; // by instance: the t of the start of its process
const names = new Map(); // the process of each holder ID

// The page tells a check what it does through data attributes on <html>: data-engine is loading,
// ready, or failed, data-busy is "true" while a command is on its way, data-t is the fake time in
// whole ms, data-playing is "true" while it plays, data-lines counts the lines of the log so far, and
// data-commits the commit objects. window.s4qlPage gives the check the state and a hook, skip, that
// advances fake time at once; a visitor reaches it only through the console.
const root = document.documentElement;
root.dataset.engine = "loading";
window.s4qlPage = {
  get t() { return t; },
  get playing() { return playing; },
  get turbo() { return turbo; },
  get state() { return state; },
  get lastStep() { return lastStep; },
  skip(ms) {
    for (let left = ms; left > 0; left -= MAX_ADVANCE) jobs.push({ by: Math.min(left, MAX_ADVANCE) });
    pump();
  },
};

// halt the page for good, with one line on what happened. A reload starts over.
function halt(title, text) {
  halted = true;
  setPlaying(false);
  root.dataset.engine = "failed";
  const f = $("failure");
  f.innerHTML = `<strong>${esc(title)}</strong>` + (text ? `<p>${esc(text)}</p>` : "");
  f.hidden = false;
  set($("status"), "");
  set($("field-note"), "");
  drawControls();
}

// pump sends the next command to the engine, if none is on its way: an action, then a skip of the
// check, then a step of playback. After each it asks for the state. Commands never pile up: a frame
// that comes while one is on its way adds its time to the debt, and the next step covers it.
async function pump() {
  if (busy || halted || !engine) return;
  let job = jobs.shift();
  if (!job && playing && debt >= 1) {
    const real = Math.min(debt, STEP);
    debt = 0;
    job = { by: real * (turbo ? TURBO : 1), real };
  }
  if (!job) return;
  busy = true;
  acting = !!job.act;
  root.dataset.busy = "true";
  try {
    const answer = job.act ? await engine.act(job.act) : await engine.advance(job.by);
    if (job.real) lastStep = { real: job.real, fake: job.by };
    take(answer);
    if (answer.ended) {
      halt("The simulation stopped.", `${answer.schedulerError || "The session ended."} Reload to start over.`);
      return;
    }
    state = (await engine.now(COMMITS)).state;
  } catch (e) {
    halt("The engine stopped.", `${e.message}. Reload to start over.`);
    return;
  } finally {
    busy = false;
    acting = false;
    root.dataset.busy = "false";
  }
  if (engine.memory > MEMORY) {
    halt("The engine has used up its memory.", "Reload to start over.");
    return;
  }
  draw();
  if (jobs.length) pump();
}

// take the events of an answer into what the page keeps.
function take(answer) {
  t = answer.t;
  for (const e of answer.events) {
    const w = tell(e);
    const slice = Math.floor(e.t / SLICE);
    bars.set(slice, (bars.get(slice) || 0) + 1);
    if (w.tier) {
      lines.unshift({ t: e.t, ...w });
      linesTotal++;
    }
    switch (e.kind) {
      case "action": marks.push(e.t); break;
      case "process":
        if (e.what === "started") starts[e.instance] = e.t;
        if (e.what === "opened") names.set(e.holder, e.process);
        break;
      case "call": {
        const c = (calls[e.instance] ||= { w: 0, r: 0, x: 0, why: "" });
        if (e.what === "committed") c.w++;
        else if (e.what === "served") c.r++;
        else if (e.what === "refused") {
          c.x++;
          c.why = R.reason(e);
        }
        break;
      }
    }
  }
  lines.length = Math.min(lines.length, LINES);
  const first = Math.floor(t / SLICE) - SLICES;
  for (const k of bars.keys()) if (k <= first) bars.delete(k);
  while (marks.length && marks[0] < t - WINDOW) marks.shift();
}

// Playback.

function setPlaying(on) {
  playing = on && !!state && !halted;
  debt = 0;
  root.dataset.playing = String(playing);
  set($("play-word"), playing ? "Pause" : "Play");
}

function setTurbo(on) {
  turbo = on;
  $("turbo").setAttribute("aria-pressed", String(on));
}

function frame(ts) {
  if (playing) debt += Math.min(FRAME, Math.max(0, ts - last));
  last = ts;
  pump();
  requestAnimationFrame(frame);
}

// Drawing.

function draw() {
  root.dataset.t = String(Math.floor(t));
  root.dataset.lines = String(linesTotal);
  set($("clock"), R.clock(t));
  drawField();
  drawLog();
  if (!state) return;
  for (const c of cards) drawCard(c);
  drawS3();
  drawControls();
}

// The field: one bar for each slice of fake time, as high as the events in it, up to the present at
// its right end once WINDOW has passed. Marks above the bars are the visitor's actions.
function drawField() {
  const now = Math.floor(t / SLICE);
  const left = Math.max(0, now - SLICES + 1); // the slice at the left end
  const h = (n) => (n === 0 ? 0 : 3 + 17 * Math.min(1, Math.log(1 + n) / Math.log(1 + 40)));
  let d = "";
  for (const [k, n] of bars) if (k >= left) d += `M${k - left + 0.15} ${28 - h(n)}h0.7V28h-0.7z`;
  let m = "";
  for (const at of marks) if (at / SLICE >= left) m += `M${at / SLICE - left} 0h0.6v5h-0.6z`;
  const x = Math.min(SLICES, t / SLICE - left);
  html($("strip"), `<path fill="currentColor" d="${d}${m}"/><rect x="${Math.min(SLICES - 0.35, x)}" y="0" width="0.35" height="28" fill="currentColor"/>`);
}

function drawLog() {
  const out = lines.map((l) => `<li class="${l.tier}"><span class="t">${R.clock(l.t)}</span><span class="text">${esc(l.text)}</span></li>`);
  if (out.length === 0) out.push(`<li class="empty">Nothing yet.</li>`);
  html($("log"), out.join(""));
  set($("lines"), linesTotal > LINES ? `last ${LINES} of ${count(linesTotal)}` : "");
}

// The instances.

const cards = ["a", "b"].map((name) => {
  const el = document.querySelector(`[data-instance="${name}"]`);
  const f = {};
  for (const node of el.querySelectorAll("[data-f]")) f[node.dataset.f] = node;
  const b = {};
  for (const node of el.querySelectorAll("[data-a]")) {
    b[node.dataset.a] = node;
    node.addEventListener("click", () => act(name, node.dataset.a));
  }
  return { name, el, f, b };
});

const instanceState = (name) => (state && state.instances.find((x) => x.instance === name)) || null;
const holderName = (id) => names.get(id) || R.short(id);

// role of an instance: serves when it holds the lease and the manifest names it.
function role(st) {
  const m = state.bucket.manifest;
  const named = !!(m && m.lease && st.holder && m.lease.holder === st.holder);
  if (st.state !== "running") return st.state;
  if (st.lease === "held") return named ? "serves" : "believes";
  return "standby";
}

const WORDS = {
  serves: "serves",
  believes: "thinks it holds the lease",
  standby: "stands by",
  starting: "starting",
  closing: "stopping",
  closed: "stopped",
  stopped: "core stopped",
  crashed: "crashed",
  failed: "cannot open its database",
};

function drawCard(c) {
  const st = instanceState(c.name);
  if (!st) return;
  const r = role(st);
  const m = state.bucket.manifest;
  c.el.dataset.role = r;
  c.el.dataset.state = st.state;
  set(c.f.word, WORDS[r] || r);
  set(c.f.process, st.process !== c.name ? st.process : "");

  let lease = "none";
  if (r === "serves") lease = "held";
  else if (r === "believes") lease = `it thinks so; S3 names ${m && m.lease ? holderName(m.lease.holder) : "nobody"}`;
  set(c.f.lease, lease);

  const log = state.bucket.log;
  const length = state.bucket.logLength || log.length;
  let pos;
  if (st.position === R.EMPTY_LOG) pos = length ? `empty, ${count(length)} behind` : "empty";
  else {
    const i = log.indexOf(st.position);
    const behind = i < 0 ? -1 : log.length - 1 - i;
    pos = R.short(st.position) + (i < 0 ? (length > log.length ? ", behind" : ", not in the log") : behind === 0 ? ", head" : `, ${count(behind)} behind`);
  }
  set(c.f.pos, pos);
  if (c.f.code.dataset.pos !== st.position) {
    c.f.code.dataset.pos = st.position;
    c.f.code.innerHTML = st.position === R.EMPTY_LOG ? "" : barcodeSVG(st.position, "code");
  }

  let clocks;
  if (st.state === "crashed") clocks = "none";
  else {
    const wall = st.wall - state.t;
    const mono = st.monotonic - (state.t - (starts[c.name] || 0));
    const parts = [];
    if (st.wallStopped && st.monotonicStopped) parts.push("paused");
    if (Math.round(wall / 1000)) parts.push("wall " + R.offset(wall));
    if (Math.round(mono / 1000)) parts.push("monotonic " + R.offset(mono));
    clocks = parts.join(", ") || "true time";
  }
  set(c.f.clocks, clocks);

  const k = calls[c.name] || { w: 0, r: 0, x: 0, why: "" };
  set(c.f.calls, `${count(k.w)} writes, ${count(k.r)} reads`);
  set(c.f.refused, k.x ? `${count(k.x)}, last: ${R.REASONS[k.why] || k.why}` : "0");

  set(c.b.crash, st.state === "crashed" ? "Restart" : "Crash");
  set(c.b.stop, st.state === "closed" ? "Start" : "Stop");
  c.b.hang.setAttribute("aria-pressed", String(!!st.networkHung));
  c.b.disconnect.setAttribute("aria-pressed", String(!!st.networkDisconnected));
  c.b.disk.setAttribute("aria-pressed", String(!!st.diskFailing));
  c.b.pause.setAttribute("aria-pressed", String(!!(st.wallStopped && st.monotonicStopped)));
}

// drawControls enables what makes sense now. Every action waits while another is on its way, so that a
// toggle is always decided from the state after the one before.
function drawControls() {
  const ready = !!state && !halted;
  $("play").disabled = !ready;
  $("turbo").disabled = !ready;
  const waiting = acting || jobs.some((j) => j.act);
  for (const c of cards) {
    const st = instanceState(c.name);
    const off = !ready || waiting || !st;
    const crashed = !!st && st.state === "crashed";
    c.b.crash.disabled = off;
    c.b.stop.disabled = off || !(st.state === "running" || st.state === "closed");
    for (const a of ["hang", "disconnect", "disk"]) c.b[a].disabled = off;
    for (const a of ["forward", "back", "pause"]) c.b[a].disabled = off || crashed;
  }
}

function act(name, a) {
  const st = instanceState(name);
  if (!st || halted) return;
  const i = { instance: name };
  let action;
  switch (a) {
    case "crash": action = { do: st.state === "crashed" ? "restart" : "crash", ...i }; break;
    case "stop": action = { do: st.state === "closed" ? "open" : "close", ...i }; break;
    case "hang": action = { do: "hangNetwork", ...i, on: !st.networkHung }; break;
    case "disconnect": action = { do: "disconnectNetwork", ...i, on: !st.networkDisconnected }; break;
    case "disk": action = { do: "failDisk", ...i, on: !st.diskFailing }; break;
    case "forward": action = { do: "shiftWall", ...i, by: JUMP }; break;
    case "back": action = { do: "shiftWall", ...i, by: -JUMP }; break;
    case "pause": action = { do: st.wallStopped && st.monotonicStopped ? "resumeClocks" : "pauseClocks", ...i }; break;
  }
  jobs.push({ act: action });
  drawControls();
  pump();
}

// S3: the manifest and the commit objects.

function drawS3() {
  const b = state.bucket;
  const m = b.manifest;
  const length = b.logLength || b.log.length;
  set($("s3-head"), m && m.head !== R.EMPTY_LOG ? `${R.short(m.head)}, ${count(length)} commits in the log` : "empty");
  set($("s3-lease"), m && m.lease ? `${holderName(m.lease.holder)}, ${m.lease.duration / 1000} s` : "nobody");
  set($("s3-writes"), count(b.manifestWrites));
  const total = b.commitCount || b.commits.length;
  root.dataset.commits = String(total);
  set($("commit-count"), total ? count(total) : "");

  const head = m ? m.head : R.EMPTY_LOG;
  let out = b.commits.slice().reverse().map((c) => {
    const cls = c.name === head ? "head" : c.inLog ? "" : "orphan";
    const tag = c.name === head ? "head" : c.inLog ? "in the log" : "not in the log";
    return `<li class="commit ${cls}">${barcodeSVG(c.name, "code")}` +
      `<div class="meta"><span class="name">${R.short(c.name)}</span><span>${R.keysText(c.keys)}</span><span>${esc(c.process)}</span><span>${R.clock(c.at)}</span><span class="caps">${tag}</span></div></li>`;
  }).join("");
  if (!out) out = `<li class="empty">None yet.</li>`;
  if (total > b.commits.length) out += `<li class="empty">${count(total - b.commits.length)} older</li>`;
  html($("commits"), out);
}

// Wiring.

$("play").addEventListener("click", () => setPlaying(!playing));
$("turbo").addEventListener("click", () => setTurbo(!turbo));

// Start.

async function start() {
  fetch("engine/build.json").then((r) => (r.ok ? r.json() : null)).then((b) => {
    if (b && /^[0-9a-f]{7,40}$/.test(b.commit || "")) {
      $("build").innerHTML = `<a href="https://github.com/maragudk/s4ql/commit/${b.commit}"><code>${b.commit.slice(0, 7)}</code></a>`;
    }
  }).catch(() => {});

  if (typeof WebAssembly !== "object" || typeof Worker !== "function") {
    halt("This browser cannot run the engine.", "It needs WebAssembly and Web Workers.");
    return;
  }
  set($("status"), "Loading the engine, about 3.5 MB.");
  try {
    const { Engine } = await import("./engine/simulator.js");
    const e = await Engine.start();
    const answer = await e.start({ seed: R.newSeed() });
    take(answer);
    state = (await e.now(COMMITS)).state;
    engine = e;
  } catch (e) {
    halt("The engine did not load.", `${e.message}. Reload to try again.`);
    return;
  }
  set($("status"), "");
  set($("field-note"), "");
  root.dataset.engine = "ready";
  root.dataset.busy = "false";
  setPlaying(false);
  draw();
  requestAnimationFrame(frame);
}

start();
