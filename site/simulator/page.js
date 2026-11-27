// The simulator page. It loads the engine of internal/simulator in a Worker, starts a live session of
// two instances with a random seed, and waits for Play. In the session, a starts alone and b stands by
// once a serves, and the engine's client calls a, with no other calls in the processes. Each instance
// is the app with s4ql embedded, and the page shows the calls of the client as the requests that reach
// the app, such as requests over HTTP from browsers. While it plays, the page advances the
// session's fake time by the real time that passed, or five times that with turbo, one step at a time,
// and asks for the state after each step. An action of the visitor goes to the engine at once, paused
// or playing, and happens at the session's fake time.
//
// The engine does not keep the events of a session, and neither does the page: it keeps the last
// lines of the log, the counts of the calls, and the bars of the last WINDOW of fake time.
//
// The database lives in the engine's memory only: a reload, like Reset, starts a new session on an
// empty S3.
//
// The box for SQL sends one statement of the visitor at a time to the instance that the requests go
// to, as the engine's action sql. While the page plays, the statement goes in and its answer comes
// with the steps of playback. While it is paused, the page advances the session just until the
// answer arrives, unless the statement is waiting on a fault that holds it there: then it waits, as
// the page does, until Play lets time move.
//
// The page loads every file at the version of its deploy: the HTML of a deploy loads this module
// with the query ?v=<version>, see internal/simulator/serve, and the module loads run.js and the
// engine with the same query, which the engine passes on to its Worker and everything that the
// Worker loads. So a cache never pairs files of two deploys. From a local build the query is empty.
const version = new URL(import.meta.url).search;

let R = null; // the module run.js, which start loads
let S = null; // the module sound.js, which start loads; the page works without it

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

// Bar codes of commit names: one bar per bit of the SHA-256, as on the main page. The page draws the
// same few names again after each step, so it keeps the last of them.
const codes = new Map();
function barcodeSVG(name, cls) {
  const key = cls + name;
  if (codes.has(key)) return codes.get(key);
  if (codes.size > 64) codes.clear();
  let d = "";
  for (let i = 0; i < 64; i++) {
    const nib = parseInt(name[i], 16);
    for (let b = 0; b < 4; b++) if (nib & (8 >> b)) d += "M" + (i * 4 + b) + " 0h0.72v40h-0.72z";
  }
  const svg = `<svg class="${cls}" viewBox="0 0 256 40" preserveAspectRatio="none" shape-rendering="crispEdges" aria-hidden="true"><path fill="currentColor" d="${d}"/></svg>`;
  codes.set(key, svg);
  return svg;
}

// drawMark draws the mark: the 32 bits of "s4ql" as bars.
function drawMark() {
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
const JUMP = 5000; // a jump of a wall clock: short next to the lease of 30 s, so it shows that it changes nothing
const NS = 1e-6; // a nanosecond in ms, the unit in which the engine keeps its clocks
const MEMORY = 1 << 30; // SQLite's memory at which the page stops
// The fake time at which the page stops. The engine's Go heap grows by about 12.5 MB a fake hour and
// cannot be measured from the page, so a day of fake time, some 300 MB, stands in for it.
const HOURS = 24 * 3600000;

// What the page knows.
let engine = null;
let Engine = null; // the class of simulator.js
let resetting = false; // whether Reset is starting a new session
let gen = 0; // the session that the answers belong to, which Reset moves on
let t = 0; // the fake time of the session
let state = null; // the answer of the engine to now
let playing = false;
let turbo = true;
let halted = false;
let busy = false; // whether a command is on its way to the engine
let acting = false; // whether that command is an action
let debt = 0; // the real time in ms that playback has not yet advanced
let last = 0; // the time of the last animation frame
let lastStep = null; // {real, fake} of the last advance by playback
const jobs = []; // commands that wait: {act} or {by}
let tell = null; // the teller of run.js, which start makes
const lines = []; // the last LINES lines of the log, newest first
let linesTotal = 0;
const calls = {}; // by instance: {w, r, x, why}
const requests = { w: 0, d: 0, r: 0, x: 0, status: "", failing: false }; // what the requests got, and the latest
const bars = new Map(); // events by slice of fake time
const marks = []; // the times of the visitor's actions
const names = new Map(); // the process of each holder ID
const stopped = {}; // by instance: whether the visitor's Stop closed the database of its process
const MAX_SQL = 1000; // the most characters of a statement of the box, as the engine takes it
const BOX_ROWS = 50; // the rows of a select that the engine gives the box
// box is the statement of the box: pending while it is on its way, waiting with why it waits while
// the page is paused, the instance that it went to, and the event that ended it, or the refusal of
// the simulator.
const box = { pending: false, waiting: "", instance: "", result: null };

// The page tells a check what it does through data attributes on <html>: data-engine is loading,
// ready, or failed, data-busy is "true" while a command is on its way, data-t is the fake time of the session in
// whole ms, data-playing is "true" while it plays, data-lines counts the lines of the log so far, and
// data-commits the commit objects. The element #requests has data-target, the instance that the
// requests go to, and data-writes, data-deletes, data-reads, and data-failed, what they got. #requests-status has
// data-failing, "true" while the latest request that ended failed. The edge of the requests to each
// instance, [data-wire], has data-on, "true" for the instance that they go to, and so has the
// instance, [data-instance], in data-requests. The edge of each instance to S3, [data-edge="s3"],
// has data-hung and data-disconnected, and its disk, [data-edge="disk"], data-failing, each "true"
// while that fault is on. The box for SQL, #sql, has data-pending, "true" while its statement is on
// its way, and data-waiting, "true" while the paused page lets it wait. window.s4qlPage gives
// the check the state and a hook, skip, that advances fake time at once; a visitor reaches it only
// through the console.
const root = document.documentElement;
root.dataset.engine = "loading";
window.s4qlPage = {
  get t() { return t; },
  get playing() { return playing; },
  get turbo() { return turbo; },
  get state() { return state; },
  get lastStep() { return lastStep; },
  get box() { return box; },
  skip(ms) {
    for (let left = ms; left > 0; left -= MAX_ADVANCE) jobs.push({ by: Math.min(left, MAX_ADVANCE) });
    pump();
  },
};

// halt the page, with one line on what happened, until a reload or Reset. It shows the failure first,
// because the rest needs controls that a page which failed at start-up may lack.
function halt(title, text) {
  halted = true;
  root.dataset.engine = "failed";
  const f = $("failure");
  if (f) {
    f.innerHTML = `<strong>${esc(title)}</strong>` + (text ? `<p>${esc(text)}</p>` : "");
    f.hidden = false;
  }
  try {
    setPlaying(false);
    set($("status"), "");
    drawControls();
    drawBox();
  } catch {
    // The failure is shown, and the page does nothing more.
  }
}

// pump sends the next command to the engine, if none is on its way: an action, then a skip of the
// check, then a step of playback. After each it asks for the state. Commands never pile up: a frame
// that comes while one is on its way adds its time to the debt, and the next step covers it.
async function pump() {
  if (busy || halted || resetting || !engine) return;
  let job = jobs.shift();
  if (!job && playing && debt >= 1) {
    const real = Math.min(debt, STEP);
    debt = 0;
    job = { by: real * (turbo ? TURBO : 1), real };
  }
  if (!job) return;
  // A step of a composed action is decided when its turn comes, from the state after the step before,
  // and is left out if there is nothing to do. So is the step of a paused page until the answer of
  // the box.
  const action = typeof job.act === "function" ? job.act() : job.act;
  if ((job.act && !action) || (job.until && !boxStep())) {
    drawControls();
    drawBox();
    return pump();
  }
  busy = true;
  acting = !!action;
  root.dataset.busy = "true";
  const g = gen;
  let answer, now;
  try {
    answer = action ? await engine.act(action) : job.until ? await engine.advance(MAX_ADVANCE, "sql") : await engine.advance(job.by);
    if (!answer.ended && g === gen) now = await engine.now(COMMITS);
  } catch (e) {
    if (g === gen) halt("The engine stopped.", `${e.message}. ${AGAIN}`);
    return;
  } finally {
    busy = false;
    acting = false;
    root.dataset.busy = "false";
  }
  // An answer of the session before a Reset is dropped.
  if (g !== gen) return;
  try {
    if (job.real) lastStep = { real: job.real, fake: job.by };
    take(answer, job);
    // A statement that has not ended in the longest advance waits for Play.
    if (job.until && box.pending && !playing) box.waiting = "Still waiting for the answer. Play to let time move.";
    if (answer.ended) {
      halt("The simulation stopped.", `${answer.schedulerError || "The session ended."} ${AGAIN}`);
      return;
    }
    state = now.state;
    draw();
  } catch (e) {
    halt("The page failed.", `${e.message}. ${AGAIN}`);
    return;
  }
  if (engine.memory > MEMORY || t >= HOURS) {
    halt("The engine has used up its memory.", AGAIN);
    return;
  }
  const total = state.bucket.commitCount || state.bucket.commits.length;
  if (total >= CAP) {
    fullStop(total);
    return;
  }
  if (jobs.length) pump();
}

// take the events of an answer to the job, if any, into what the page keeps. The steps of a reset of
// the clocks take one line in the log and one mark on the field, at the first.
function take(answer, job = {}) {
  t = answer.t;
  for (const e of answer.events) {
    if (S) S.hear(e, (playing || !!job.act || !!job.until) && !(job.reset && !job.reset.first));
    let w = tell(e);
    if (job.reset && e.kind === "action") w = R.resetLine(e, job.reset.first);
    const slice = Math.floor(e.t / SLICE);
    bars.set(slice, (bars.get(slice) || 0) + 1);
    if (w.tier) note(e.t, w);
    switch (e.kind) {
      case "action":
        if (!job.reset || job.reset.first) marks.push(e.t);
        if (e.what === "done" && (e.do === "close" || e.do === "open")) stopped[e.instance] = e.do === "close";
        if (e.do === "sql") {
          box.instance = e.instance || box.instance;
          if (e.what !== "done") Object.assign(box, { pending: false, waiting: "", result: { refused: e.text } });
        }
        break;
      case "process":
        if (e.what === "started") stopped[e.instance] = false;
        if (e.what === "opened") names.set(e.holder, e.process);
        break;
      case "call": {
        if (e.caller === "box") {
          if (e.what !== "started") Object.assign(box, { pending: false, waiting: "", result: e });
          break;
        }
        if (e.caller === "client" && e.what !== "started") {
          const st = R.requestStatus(e);
          if (e.what === "committed") requests[e.op === "delete" ? "d" : "w"]++;
          else if (e.what === "served") requests.r++;
          else requests.x++;
          requests.status = st.words;
          requests.failing = st.failing;
        }
        const c = (calls[e.instance] ||= { w: 0, d: 0, r: 0, x: 0, why: "" });
        if (e.what === "committed") c[e.op === "delete" ? "d" : "w"]++;
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

// note a line of the log at the time t of the session.
function note(at, w) {
  lines.unshift({ t: at, ...w });
  linesTotal++;
}

// AGAIN is what a visitor can do after the page stopped.
const AGAIN = "Reload or Reset to start over.";

// CAP is the number of commit objects at which the page stops, so that a long session stays small
// for the engine, which holds every object in memory.
const CAP = 2000;

// fullStop halts the page because S3 is full: at the cap of commit objects.
function fullStop(total) {
  halt("S3 is full.", `The database has ${count(total)} commit objects. Reset to start over.`);
}

// Playback.

function setPlaying(on) {
  playing = on && !!state && !halted;
  debt = 0;
  root.dataset.playing = String(playing);
  set($("play-word"), playing ? "Pause" : "Play");
  keepAwake();
}

// The screen stays on while the page plays, where the browser has the Screen Wake Lock API. A lock
// that the browser takes back, as when the tab is hidden, is asked for again when the page is visible
// and plays. A request that fails, as before a first click or key, or at low battery, is ignored and
// tried again at the next click or key.
let awake = null; // the lock that the page holds, or null
let waking = false; // whether a request for the lock is on its way

function keepAwake() {
  if (!playing) {
    const lock = awake;
    awake = null;
    if (lock) lock.release().catch(() => {});
    return;
  }
  if (awake || waking || document.visibilityState !== "visible" || !navigator.wakeLock) return;
  waking = true;
  const done = () => { waking = false; };
  try {
    navigator.wakeLock.request("screen").then((lock) => {
      done();
      if (!playing) {
        lock.release().catch(() => {});
        return;
      }
      awake = lock;
      lock.addEventListener("release", () => {
        if (awake === lock) awake = null;
      });
    }, done);
  } catch {
    done();
  }
}

function setTurbo(on) {
  turbo = on;
  $("turbo").setAttribute("aria-pressed", String(on));
}

// The browser remembers Turbo in localStorage, under TURBO_ITEM, as "on" or "off", across reloads,
// tabs, and visits. Any other value, or storage that the browser refuses, is on, as on a first visit.
const TURBO_ITEM = "s4ql:turbo";

function turboRemembered() {
  try {
    return window.localStorage.getItem(TURBO_ITEM) !== "off";
  } catch {
    return true;
  }
}

function rememberTurbo() {
  try {
    window.localStorage.setItem(TURBO_ITEM, turbo ? "on" : "off");
  } catch {
    // Storage is a convenience.
  }
}

function frame(ts) {
  if (playing) debt += Math.min(FRAME, Math.max(0, ts - last));
  last = ts;
  dance(ts);
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
  drawRequests();
  for (const c of nodes) drawNode(c);
  drawS3();
  drawControls();
  drawBox();
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

let logged = -1; // the lines that drawLog last drew

function drawLog() {
  if (logged === linesTotal) return;
  logged = linesTotal;
  const out = lines.map((l) => `<li class="${l.tier}"><span class="t">${R.clock(l.t)}</span><span class="text">${esc(l.text)}</span></li>`);
  if (out.length === 0) out.push(`<li class="empty">Nothing yet.</li>`);
  html($("log"), out.join(""));
  set($("lines"), linesTotal > LINES ? `last ${LINES} of ${count(linesTotal)}` : "");
}

// The instances.

let nodes = []; // the instances a and b in the diagram, which wire finds

// wire finds the instances in the diagram, with their edges, disks, and clocks, and connects the
// buttons of the page.
function wire() {
  nodes = ["a", "b"].map((name) => {
    const el = document.querySelector(`[data-instance="${name}"]`);
    const f = {};
    for (const node of el.querySelectorAll("[data-f]")) f[node.dataset.f] = node;
    const b = {};
    for (const node of el.querySelectorAll("[data-a]")) {
      b[node.dataset.a] = node;
      node.addEventListener("click", () => act(name, node.dataset.a));
    }
    const s3 = el.querySelector('[data-edge="s3"]'), disk = el.querySelector('[data-edge="disk"]'), clocks = el.querySelector(".clocks");
    return { name, el, f, b, s3, disk, clocks };
  });
  for (const b of document.querySelectorAll("#requests [data-target]")) b.addEventListener("click", () => send(b.dataset.target));
  document.addEventListener("visibilitychange", keepAwake);
  window.addEventListener("pagehide", () => {
    const lock = awake;
    awake = null;
    if (lock) lock.release().catch(() => {});
  });
  for (const type of ["click", "keydown"]) document.addEventListener(type, keepAwake, true);
  $("play").addEventListener("click", () => {
    setPlaying(!playing);
    // A statement of the box that is on its way when the page pauses is answered as on a paused page.
    if (!playing && box.pending && !jobs.some((j) => j.until)) {
      jobs.push({ until: true });
      pump();
    }
  });
  document.addEventListener("keydown", shortcut);
  document.addEventListener("keyup", shortcutUp);
  $("reset").addEventListener("click", () => reset());
  $("turbo").addEventListener("click", () => {
    setTurbo(!turbo);
    rememberTurbo();
  });
  const text = $("sql-text");
  text.addEventListener("input", drawBox);
  text.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
      e.preventDefault();
      runSQL();
    }
  });
  $("sql-form").addEventListener("submit", (e) => {
    e.preventDefault();
    runSQL();
  });
  const mac = /mac|iphone|ipad/i.test((navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || "");
  set($("sql-keys"), mac ? "⌘ Enter" : "Ctrl+Enter");
}

// The keys of the page: Space plays and pauses, and a and b send the requests, and so the SQL of the
// box, to that instance. Each does what a click on its button does, so that a button that is switched
// off ignores its key too. Keys are the page's only where the visitor is not typing or holding a
// modifier: not in a text field, and not with Ctrl, Alt, or Meta, which are the browser's. Space is the
// page's on a focused button as well, as in a media player, so that a focused Play or the stick figure
// does not take it: Enter still presses a focused button. The keys of a held Space do nothing.
function shortcut(e) {
  if (e.ctrlKey || e.altKey || e.metaKey || e.isComposing || typing(e.target)) return;
  if (e.key === " ") {
    e.preventDefault();
    if (!e.repeat) $("play").click();
    return;
  }
  const to = e.key.toLowerCase();
  if (to === "a" || to === "b") document.querySelector(`#requests [data-target="${to}"]`).click();
}

// shortcutUp keeps Space from clicking a focused button when it is let go, which some browsers do
// even when its keydown was kept from it.
function shortcutUp(e) {
  if (e.key === " " && !(e.ctrlKey || e.altKey || e.metaKey || e.isComposing || typing(e.target))) e.preventDefault();
}

// typing says whether the element takes text from the keyboard.
function typing(el) {
  return el instanceof Element && !!el.closest("textarea, input, select, [contenteditable]:not([contenteditable=false])");
}

const instanceState = (name) => (state && state.instances.find((x) => x.instance === name)) || null;
// holderName of a holder ID: its instance, and whether the process that held it is gone, because the
// page names no process, and a lease of a crashed process can stand in S3 until it runs out.
function holderName(id) {
  const p = names.get(id);
  if (!p) return R.short(id);
  const name = R.instanceOf(p), st = instanceState(name);
  return st && st.process !== p ? `${name} before its restart` : name;
}

// role of an instance: serves when it holds the lease and the manifest names it.
function role(st) {
  const m = state.bucket.manifest;
  const named = !!(m && m.lease && st.holder && m.lease.holder === st.holder);
  if (st.state !== "running") return st.state;
  if (st.lease === "held") return named ? "serves" : "believes";
  return "standby";
}

const WORDS = {
  waiting: "not started",
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

function drawNode(c) {
  const st = instanceState(c.name);
  if (!st) return;
  const r = role(st);
  const m = state.bucket.manifest;
  c.el.dataset.role = r;
  c.el.dataset.state = st.state;
  set(c.f.word, WORDS[r] || r);

  let lease = "none";
  if (r === "serves") lease = "held";
  else if (r === "believes") {
    const named = m && m.lease ? holderName(m.lease.holder) : "nobody";
    lease = `it thinks so; S3 names ${named === c.name ? "its lease from before" : named}`;
  }
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
  if (st.state === "crashed" || st.state === "waiting") clocks = "none";
  else {
    // Whether the clocks stand still, and how far the wall clock is off true time. The monotonic
    // clock cannot jump, so after a pause it stays behind true time for good. The core uses it only
    // for durations, so the page says only whether it runs.
    const off = wallOffset(st);
    const wall = Math.round(off / 1000) ? "wall " + R.offset(off) : "";
    clocks = [paused(st) ? "paused" : "", wall].filter(Boolean).join(", ") || "normal";
  }
  set(c.f.clocks, clocks);
  // The dial: its mark outside goes round with true time, and its hand with the wall clock of the
  // instance, once a minute each, so the hand falls behind while the clocks stand still, and stands
  // off the mark by the offset of the wall clock.
  const turn = (ms) => `rotate(${(((ms / 1000) % 60) + 60) % 60 * 6})`;
  c.f.now.setAttribute("transform", turn(state.t));
  c.f.hand.setAttribute("transform", turn(st.wall));
  c.f.hand.style.visibility = clocks === "none" ? "hidden" : "";
  c.clocks.dataset.paused = String(clocks !== "none" && paused(st));

  const k = calls[c.name] || { w: 0, d: 0, r: 0, x: 0, why: "" };
  set(c.f.calls, `${count(k.w)} inserts, ${count(k.d)} deletes, ${count(k.r)} selects`);
  set(c.f.refused, k.x ? `${count(k.x)}, last: ${R.reasonWords(k.why)}` : "0");

  c.el.dataset.requests = String(target() === c.name);
  const crash = st.state === "crashed" ? "Restart" : "Crash", stop = startable(c.name, st) ? "Start" : "Stop";
  set(c.b.crash, crash);
  set(c.b.stop, stop);
  c.b.crash.setAttribute("aria-label", `${crash} ${c.name}`);
  c.b.stop.setAttribute("aria-label", `${stop} ${c.name}`);
  c.s3.dataset.hung = String(!!st.networkHung);
  c.s3.dataset.disconnected = String(!!st.networkDisconnected);
  c.disk.dataset.failing = String(!!st.diskFailing);
  c.b.hang.setAttribute("aria-pressed", String(!!st.networkHung));
  c.b.disconnect.setAttribute("aria-pressed", String(!!st.networkDisconnected));
  c.b.disk.setAttribute("aria-pressed", String(!!st.diskFailing));
  c.b.pause.setAttribute("aria-pressed", String(paused(st)));
}

// drawControls enables what makes sense now. Every action waits while another is on its way, so that a
// toggle is always decided from the state after the one before.
function drawControls() {
  const ready = !!state && !halted;
  $("play").disabled = !ready;
  $("turbo").disabled = !ready;
  $("reset").disabled = !engine || resetting;
  const waiting = acting || jobs.some((j) => j.act);
  for (const c of nodes) {
    const st = instanceState(c.name);
    const off = !ready || waiting || !st;
    const crashed = !!st && st.state === "crashed";
    // An instance that has not started takes only the faults of its connection to S3 and of its disk.
    const notStarted = !!st && st.state === "waiting";
    c.b.crash.disabled = off || notStarted;
    c.b.stop.disabled = off || !(st.state === "running" || startable(c.name, st));
    for (const a of ["hang", "disconnect", "disk"]) c.b[a].disabled = off;
    for (const a of ["forward", "back", "pause"]) c.b[a].disabled = off || crashed || notStarted;
    c.b.reset.disabled = off || crashed || notStarted || !touched(st);
  }
  for (const b of document.querySelectorAll("#requests [data-target]")) b.disabled = !ready || waiting;
}

// The requests, which the engine's client makes.

// target is the instance that the requests go to now.
const target = () => (state && state.client ? state.client.instance : "");

// send the requests to the instance, unless they go there already.
function send(name) {
  if (!state || halted || target() === name) return;
  jobs.push({ act: { do: "client", instance: name } });
  drawControls();
  pump();
}

function drawRequests() {
  const to = target();
  const el = $("requests");
  el.dataset.target = to;
  el.dataset.writes = String(requests.w);
  el.dataset.deletes = String(requests.d);
  el.dataset.reads = String(requests.r);
  el.dataset.failed = String(requests.x);
  for (const b of el.querySelectorAll("[data-target]")) b.setAttribute("aria-pressed", String(b.dataset.target === to));
  for (const w of document.querySelectorAll("[data-wire]")) w.dataset.on = String(w.dataset.wire === to);
  set($("requests-writes"), count(requests.w));
  set($("requests-deletes"), count(requests.d));
  set($("requests-reads"), count(requests.r));
  set($("requests-failed"), count(requests.x));
  set($("requests-status-words"), requests.status || R.NO_REQUESTS);
  $("requests-status").dataset.failing = String(requests.failing);
}

// startable says whether Start opens the database of the instance: after the visitor's Stop, and not
// while its process closes and opens the database by itself after its core stopped.
const startable = (name, st) => st.state === "closed" && !!stopped[name];

function act(name, a) {
  const st = instanceState(name);
  if (!st || halted) return;
  const i = { instance: name };
  let action;
  switch (a) {
    case "crash": action = { do: st.state === "crashed" ? "restart" : "crash", ...i }; break;
    case "stop": action = { do: startable(name, st) ? "open" : "close", ...i }; break;
    case "hang": action = { do: "hangNetwork", ...i, on: !st.networkHung }; break;
    case "disconnect": action = { do: "disconnectNetwork", ...i, on: !st.networkDisconnected }; break;
    case "disk": action = { do: "failDisk", ...i, on: !st.diskFailing }; break;
    case "forward": action = { do: "shiftWall", ...i, by: JUMP }; break;
    case "back": action = { do: "shiftWall", ...i, by: -JUMP }; break;
    case "pause": action = { do: paused(st) ? "resumeClocks" : "pauseClocks", ...i }; break;
    case "reset": return resetClocks(name, st);
  }
  jobs.push({ act: action });
  drawControls();
  pump();
}

// wallOffset of the wall clock of an instance from true time, in ms.
const wallOffset = (st) => st.wall - state.t;

// paused says whether the clocks of the instance stand still. The page pauses and starts both
// together, so it takes either for both.
const paused = (st) => !!(st.wallStopped || st.monotonicStopped);

// touched says whether the visitor has moved the clocks of the instance: whether they stand still,
// or the wall clock is off true time. A monotonic clock that a pause left behind is not touched.
const touched = (st) => paused(st) || Math.abs(wallOffset(st)) >= NS;

// resetClocks of the instance: its clocks run again, and its wall clock reads true time. The engine
// has no action for it, so the page sends resumeClocks if they stand still, and then shiftWall by
// the offset of the wall clock after that, because a pause leaves the wall clock behind. The engine
// keeps its clocks in whole nanoseconds, and rounds the shift to them, so the wall clock lands on
// true time.
function resetClocks(name, st) {
  const steps = [];
  if (paused(st)) steps.push(() => ({ do: "resumeClocks", instance: name }));
  steps.push(() => {
    const now = instanceState(name);
    const off = now ? wallOffset(now) : 0;
    return Math.abs(off) >= NS ? { do: "shiftWall", instance: name, by: -off } : null;
  });
  steps.forEach((f, k) => jobs.push({ act: f, reset: { instance: name, first: k === 0 } }));
  drawControls();
  pump();
}

// The box for SQL.

const chars = (s) => [...s].length;

// runSQL sends the statement of the box, unless one is on its way. While the page is paused, a step
// that advances until its answer follows it.
function runSQL() {
  const text = $("sql-text").value;
  if (!state || halted || resetting || box.pending || chars(text) > MAX_SQL) return;
  Object.assign(box, { pending: true, waiting: "", instance: target(), result: null });
  jobs.push({ act: { do: "sql", sql: text } });
  if (!playing) jobs.push({ until: true });
  drawControls();
  drawBox();
  pump();
}

// boxStep says whether the paused page advances the session until the answer of the box: while the
// statement is on its way, unless the instance that it went to is held by a fault that time must
// pass for, its connection to S3 hanging or its clocks standing still. Then the statement waits.
function boxStep() {
  if (!box.pending || playing || halted) return false;
  const st = instanceState(box.instance);
  const why = st && st.networkHung ? `the connection of ${box.instance} to S3 hangs` : st && paused(st) ? `the clocks of ${box.instance} stand still` : "";
  if (why) box.waiting = `Waiting: ${why}. Play to let time move.`;
  return !why;
}

// boxStatus is the line of the box: what came back, or that the statement waits, with whether it
// failed.
function boxStatus() {
  const n = chars($("sql-text").value);
  if (n > MAX_SQL) return { text: `The box takes at most ${count(MAX_SQL)} characters, and this has ${count(n)}.`, failing: true };
  if (box.pending) return { text: box.waiting || `Sent to ${box.instance}. Waiting for the answer.`, failing: false };
  const e = box.result;
  if (!e) return { text: "Nothing run yet.", failing: false };
  if (e.refused !== undefined) return { text: `Refused by the simulator: ${e.refused}.`, failing: true };
  const i = e.instance;
  const rows = (k) => `${count(k)} ${k === 1 ? "row" : "rows"}`;
  switch (e.what) {
    case "served": {
      const total = e.count || 0, shown = (e.rows || []).length;
      return { text: `${i} serves ${total > shown ? `${count(shown)} of ${rows(total)}` : rows(total)}.`, failing: false };
    }
    case "committed": {
      const changed = e.affected === undefined || e.affected === null ? "" : `${rows(e.affected)} changed`;
      if (e.commit) return { text: `${i} commits ${R.short(e.commit)}${changed ? ": " + changed : ""}.`, failing: false };
      return { text: `${i}: ${changed ? changed + ", " : ""}nothing to commit.`, failing: false };
    }
    case "down":
    case "lost": return { text: `No answer from ${i}.`, failing: true };
    case "abandoned": return { text: `Rolled back on ${i}: ${e.error}`, failing: true };
    case "unknown": return { text: `Outcome unknown on ${i}: ${e.error}`, failing: true };
    case "not_committed": return { text: `Not committed on ${i}: ${e.error}`, failing: true };
    case "refused": return { text: `Refused by ${i}: ${e.error}`, failing: true };
  }
  return { text: `Error from ${i}: ${e.error || e.what}`, failing: true };
}

// cell of a value of a row as SQL writes it: text in quotes, a blob as x'…', a number as itself, and
// NULL in small capitals, dimmed. A value that the engine cut ends in an ellipsis.
function cell(c) {
  if (!c) return `<td class="null"><span class="caps">null</span></td>`;
  const end = c.cut ? "…" : "'";
  const v = c.type === "text" ? `'${c.value.replaceAll("'", "''")}${end}` : c.type === "blob" ? `x'${c.value}${end}` : c.value;
  return `<td title="${esc(v)}">${esc(v)}</td>`;
}

function drawBox() {
  const el = $("sql");
  if (!el) return;
  el.dataset.pending = String(box.pending);
  el.dataset.waiting = String(box.pending && !!box.waiting);
  const to = box.pending ? box.instance : target();
  set($("sql-to"), to ? `to ${to}` : "");
  const run = $("sql-run");
  run.disabled = !state || halted || resetting || box.pending || chars($("sql-text").value) > MAX_SQL;
  set(run, box.pending ? "Running" : "Run");
  const st = boxStatus();
  set($("sql-status"), st.text);
  $("sql-status").dataset.failing = String(st.failing);
  const e = box.result, rowsEl = $("sql-rows");
  const shows = !box.pending && !!e && e.what === "served";
  rowsEl.hidden = !shows;
  html(rowsEl, !shows ? "" : `<table><thead><tr>${(e.columns || []).map((c) => `<th class="caps" scope="col" title="${esc(c)}">${esc(c)}</th>`).join("")}</tr></thead>` +
    `<tbody>${(e.rows || []).slice(0, BOX_ROWS).map((r) => `<tr>${r.map(cell).join("")}</tr>`).join("")}</tbody></table>`);
}

// S3: the manifest and the commit objects.

function drawS3() {
  const b = state.bucket;
  const m = b.manifest;
  const length = b.logLength || b.log.length;
  set($("s3-head"), m && m.head !== R.EMPTY_LOG ? `${R.short(m.head)}, ${count(length)} commits in the log` : "empty");
  // The bar code of the head, as each disk shows that of its cache: a cache at the head has the same.
  const code = $("s3-code"), head = m ? m.head : R.EMPTY_LOG;
  if (code.dataset.pos !== head) {
    code.dataset.pos = head;
    code.innerHTML = head === R.EMPTY_LOG ? "" : barcodeSVG(head, "code");
  }
  set($("s3-lease"), m && m.lease ? `${holderName(m.lease.holder)}, ${m.lease.duration / 1000} s` : "nobody");
  set($("s3-writes"), count(b.manifestWrites));
  set($("s3-path"), `s4ql-sim/${PREFIX}/`);
  const total = b.commitCount || b.commits.length;
  root.dataset.commits = String(total);
  set($("commit-count"), total ? count(total) : "");

  let out = b.commits.slice().reverse().map((c) => {
    const cls = c.name === head ? "head" : c.inLog ? "" : "orphan";
    const tag = c.name === head ? "head" : c.inLog ? "in the log" : "not in the log";
    // The statements of its annotations, each with its arguments as the commit object holds them.
    const sql = (c.statements || []).map((s) => `<li><code>${esc(s.sql)}</code>` +
      ((s.args || []).length ? ` <span class="caps">args</span> ${esc(s.args.map((a) => JSON.stringify(a)).join(", "))}` : "") + `</li>`).join("");
    return `<li class="commit ${cls}">${barcodeSVG(c.name, "code")}` +
      `<div class="meta"><span class="name">${R.short(c.name)}</span><span>${R.commitKeysText(c)}</span><span>${esc(R.instanceOf(c.process))}</span><span>${R.clock(c.at)}</span><span class="caps">${tag}</span></div>` +
      (sql ? `<ul class="sql">${sql}</ul>` : "") + `</li>`;
  }).join("");
  if (!out) out = `<li class="empty">None yet.</li>`;
  if (total > b.commits.length) out += `<li class="empty">${count(total - b.commits.length)} older</li>`;
  html($("commits"), out);
}

// Start.

// SETTINGS of the session: a starts alone, b stands by, and the client calls a.
const SETTINGS = { workload: { callers: 0 }, client: { instance: "a" }, standby: true };

// PREFIX of the database in S3, as the engine names its objects: the key of each starts with it and
// a slash.
const PREFIX = "db";

// begin a session on the engine, on an empty S3.
async function begin(e) {
  take(await e.start({ seed: R.newSeed(), ...SETTINGS }));
  state = (await e.now(COMMITS)).state;
}

// reset empties the database and starts a new session at time 0, paused, with the page as at
// its start. It works while the page plays, is paused, or has stopped; an answer of the session
// before that is still on its way is dropped.
async function reset() {
  if (resetting || !engine) return;
  resetting = true;
  gen++;
  if (S && S.blip) S.blip();
  setPlaying(false);
  jobs.length = 0;
  t = 0;
  state = null;
  halted = false;
  debt = 0;
  lastStep = null;
  tell = R.teller();
  lines.length = 0;
  linesTotal = 0;
  logged = -1;
  for (const k of Object.keys(calls)) delete calls[k];
  Object.assign(requests, { w: 0, d: 0, r: 0, x: 0, status: "", failing: false });
  bars.clear();
  marks.length = 0;
  names.clear();
  for (const k of Object.keys(stopped)) delete stopped[k];
  Object.assign(box, { pending: false, waiting: "", instance: "", result: null });
  $("failure").hidden = true;
  drawControls();
  try {
    try {
      await begin(engine);
    } catch {
      // The Worker may have failed: a new one.
      engine.terminate();
      engine = await Engine.start();
      await begin(engine);
    }
  } catch (e) {
    resetting = false;
    halt("The engine did not start.", `${e.message}. Reload to try again.`);
    return;
  }
  resetting = false;
  root.dataset.engine = "ready";
  setPlaying(false);
  draw();
}

// The dancer: a stick figure on the top line of the frame of the log and the commit objects, which
// dances to the sounds, and strolls along the line when it does not. sound.js tells it, through
// watch, of each figure and tick that plays and when it starts, and the figure takes the move of that
// kind of sound as it starts to sound. It dances only while sound is on and the page plays. It
// strolls whenever it does not dance: with sound off, paused, and before the first Play. Under
// prefers-reduced-motion it stands still at its old place, 40 px from the right end of the line, or
// where the visitor put it, and once the page has halted it stands still where it is.
//
// Turbo brings ticks every few tens of ms, so a pose holds at least HOLD: the figure follows the
// sounds at most five poses a second. A sound that comes while a pose holds waits, unless one of a
// higher rank comes, and is left out once it is STALE. A move is one pose or a few, by the kind of
// its sound; a move of one pose takes turns with its mirror, so that a run of one sound is a dance
// and not a pose that stays. A move cuts into another only from a higher rank; once its poses have
// held, any move may follow, and after REST the figure stands again.
//
// A stroll: it stands a while, walks a stretch of a few steps, and stands again, more standing than
// walking, and turns where the next step would take it past an end of the line. Walking, it moves
// one pixel of its grid each PACE, and its walk cycle moves a foot on the line back by as much
// against the figure, so that the foot stays where it is on the line. A sound stops the walk where
// the figure is, it dances there, and it strolls on from there. Chance decides how long it stands,
// how far it walks, and whether it turns: see strollChance.
//
// The figure is drawn on a grid of DW by DH of its pixels from the joints of each pose, with lines a
// pixel wide and a ring for a head, as crisp as the bars of the page. Its place is a transform by
// whole pixels of that grid from its old place, set by the CSS, and no pixel of any pose goes past
// the ends of the line: a pose that would, such as the fall of a crash near an end, moves the figure
// in until it fits, and it stays there. The SVG #dancer says its pose in data-pose, and in data-moves
// how many dance poses it has taken, for a check of sound; the poses of the walk are walk0 to
// walk11, mirrored when it walks to the left.
//
// The visitor can poke it, with a click, a tap, or Enter on #figure, the element around the
// SVG that is placed and moved. A poke stops what it does, dance or stroll, and it plays one of
// POKES, by chance and never the one of the poke before; PESTERS pokes within PESTER ms make it sulk.
// Then it stands, strolls on, and dances to the next sound. A press on it that moves TAP px picks it
// up: it dangles from the pointer, its legs kicking, and when it is let go it falls onto the top
// line of a frame of STAGES, every framed box of the page: it falls down to the first line below its
// feet, never up, unless no line is below them: see dancerLand. That frame holds it from then on,
// and its line is the stretch of the top line clear of the words and buttons that stand above it:
// see stretch. The frame of .logs always has its whole line. Where the room above a line is lower
// than the figure, it stands in front of what is above, and behind the sticky bar, so that it scrolls
// away with its frame. Under reduced motion a poke is one still pose for STILL ms, and a drop lands
// at once, where it stays. Before the page's animation frame runs, as while the engine loads, it
// takes no poke or drag. Carried, it stays inside the viewport, so that the page never grows wider
// under it: a phone's or a tablet's browser widens the page for good when anything passes its right
// edge, and then it scrolls sideways.
const DW = 15, DH = 18; // the grid of the figure, in its pixels; the feet are on its last row
const PX = 2; // the size in px of a pixel of the grid, as the width of .dancer in index.html sets it
const RIGHT = 40; // px from the right end of the line to the figure's old place, as .dancer's right sets it
const HOLD = 200; // ms, the least time that a dance pose holds
const STALE = 300; // ms, how long after its sound a move may still start
const REST = 450; // ms, how long the last pose of a move stays before the figure stands
const PACE = 110; // ms, the time of a frame of the walk, in which it moves a pixel: 18 px a second
const STRIDE = 6; // the pixels of a step of the walk, half its cycle of 12 frames
const PESTER = 3000; // ms, the time in which PESTERS pokes make it sulk
const PESTERS = 4;
const STILL = 1200; // ms, how long the pose of a poke stays under reduced motion
const TAP = 6; // px, how far a press moves before it picks the figure up and is no longer a poke
const FLAIL = 130; // ms, each pose of the kicking while it is held
const GRAVITY = 0.004; // px per ms², of the fall after a drop
const LAND = 150; // ms, how long it crouches after the fall
const GAP = 8; // px, the least room between the figure and words or a button beside it above a line
const HIT = 8; // px, how far the hit area of .dancer's ::before in index.html reaches past the figure
// STAGES are the frames whose top line it can stand on: every framed box of the page. BLOCK is what
// stands free above a line that the figure keeps clear of, words and buttons; it walks in front of
// the lines of the edges between the frames, and of a frame above a line that is close to it.
const STAGES = ".requests, .node, .box, .s3, .logs, .ask-box";
const BLOCK = ".link > .btn, .link > .to, .ask-head > *";

// POSES by name: the head's centre h, the neck n, the pelvis p, the elbow and hand of each arm, la
// and ra, and the knee and foot of each leg, ll and rl, on the grid, with the line under row 17. l is
// the viewer's left. Each pose has a mirror, named "-" and its name.
const POSES = {
  // Standing, arms at its sides.
  stand: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[9, 8], [10, 11]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // The disco point, for an insert's beep: a hand up to the corner, the other on the hip, the hip out.
  point: { h: [7, 3], n: [7, 6], p: [8, 11], la: [[4, 9], [6, 11]], ra: [[10, 4], [13, 1]], ll: [[6, 14], [5, 17]], rl: [[9, 14], [10, 17]] },
  // Brushing the item off the counter, for a void.
  sweep: { h: [7, 4], n: [7, 7], p: [7, 12], la: [[4, 9], [1, 11]], ra: [[5, 10], [2, 13]], ll: [[5, 14], [5, 17]], rl: [[9, 14], [9, 17]] },
  // A nod to the side, for a select.
  nod: { h: [6, 4], n: [7, 7], p: [7, 11], la: [[5, 9], [4, 11]], ra: [[9, 9], [10, 11]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A hand to the brow, looking for the row, for a select that finds none.
  look: { h: [8, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[12, 5], [11, 2]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A hand to the face, for the error tone.
  facepalm: { h: [7, 4], n: [7, 7], p: [7, 11], la: [[5, 9], [5, 12]], ra: [[11, 7], [8, 4]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A wave to the visitor, for a statement of the box, in two poses.
  wave: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[11, 5], [12, 1]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  wave2: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[11, 5], [10, 1]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // Both arms up, for an instance that opens.
  tada: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 4], [2, 1]], ra: [[10, 4], [12, 1]], ll: [[5, 14], [4, 17]], rl: [[9, 14], [10, 17]] },
  // A crash: the knees give, and it falls flat on the line.
  slump: { h: [6, 7], n: [7, 10], p: [7, 14], la: [[5, 12], [4, 14]], ra: [[9, 12], [10, 14]], ll: [[3, 15], [4, 17]], rl: [[11, 15], [10, 17]] },
  flat: { h: [2, 15], n: [5, 15], p: [10, 15], la: [[6, 13], [7, 11]], ra: [[6, 17], [8, 17]], ll: [[12, 14], [14, 13]], rl: [[12, 16], [14, 17]] },
  // The raised hand of a stop, feet together.
  halt: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [5, 11]], ra: [[12, 6], [12, 3]], ll: [[6, 14], [6, 17]], rl: [[8, 14], [8, 17]] },
  // A takeover: a shove to the side, then both thumbs to its own chest.
  shove: { h: [10, 4], n: [9, 7], p: [7, 11], la: [[11, 7], [14, 7]], ra: [[11, 8], [14, 9]], ll: [[5, 14], [3, 17]], rl: [[8, 14], [9, 17]] },
  mine: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 8], [6, 8]], ra: [[10, 8], [8, 8]], ll: [[5, 14], [4, 17]], rl: [[9, 14], [10, 17]] },
  // The shrug, for requests that start to fail.
  shrug: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 8], [3, 5]], ra: [[10, 8], [11, 5]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A fist pump with a knee up, for requests that work again.
  pump: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[10, 4], [10, 0]], ll: [[5, 13], [6, 15]], rl: [[8, 14], [9, 17]] },
  pump2: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[11, 7], [11, 3]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A tap of the toe, for the blip of a control.
  tap: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[9, 8], [10, 11]], ll: [[6, 14], [5, 17]], rl: [[9, 14], [11, 16]] },
  // The poses of POKES and of a drag, of the figure facing right where it has a front.
  // Down on its haunches, before a jump and after a landing.
  crouch: { h: [7, 6], n: [7, 9], p: [7, 13], la: [[5, 11], [3, 13]], ra: [[9, 11], [11, 13]], ll: [[4, 15], [5, 17]], rl: [[10, 15], [9, 17]] },
  // Startled: arms and legs flung out, in the air, and as it falls after a drop.
  eek: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 5], [2, 2]], ra: [[10, 5], [12, 2]], ll: [[4, 14], [3, 16]], rl: [[10, 14], [11, 16]] },
  // Tripped, falling over backwards, a leg in the air, before it lies flat.
  trip: { h: [5, 4], n: [6, 7], p: [8, 11], la: [[4, 5], [3, 2]], ra: [[9, 5], [12, 4]], ll: [[9, 14], [9, 17]], rl: [[11, 13], [13, 13]] },
  // Sitting up after it, legs out, a hand to its head.
  sit: { h: [4, 8], n: [4, 11], p: [5, 16], la: [[2, 13], [1, 16]], ra: [[6, 12], [7, 9]], ll: [[9, 16], [13, 17]], rl: [[9, 17], [13, 17]] },
  // Dusting itself off, in two poses.
  dust: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[9, 9], [7, 11]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  dust2: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[9, 8], [6, 9]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  // A back flip: tucked into a ball, and upside down at the top.
  ball: { h: [7, 7], n: [7, 9], p: [7, 13], la: [[4, 11], [6, 13]], ra: [[10, 11], [8, 13]], ll: [[5, 11], [5, 14]], rl: [[9, 11], [9, 14]] },
  upside: { h: [7, 14], n: [7, 11], p: [7, 6], la: [[4, 10], [2, 8]], ra: [[10, 10], [12, 8]], ll: [[6, 3], [5, 0]], rl: [[8, 3], [9, 0]] },
  // An arm swept out, and the deep bow after it.
  flourish: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[5, 8], [4, 11]], ra: [[10, 6], [13, 4]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  bow: { h: [12, 10], n: [9, 9], p: [6, 12], la: [[7, 8], [4, 7]], ra: [[9, 12], [11, 13]], ll: [[5, 14], [5, 17]], rl: [[7, 14], [7, 17]] },
  // Cowering, arms over its head; shiver, below, is the same a pixel to the right.
  cower: { h: [7, 9], n: [7, 11], p: [7, 14], la: [[4, 8], [6, 6]], ra: [[10, 8], [8, 6]], ll: [[4, 15], [5, 17]], rl: [[10, 15], [9, 17]] },
  // Poked once too often: arms crossed, and then it sits down and hugs its knees.
  cross: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 8], [9, 9]], ra: [[10, 8], [5, 9]], ll: [[6, 14], [5, 17]], rl: [[8, 14], [9, 17]] },
  sulk: { h: [5, 9], n: [5, 12], p: [5, 16], la: [[7, 13], [9, 13]], ra: [[7, 14], [9, 14]], ll: [[9, 12], [11, 17]], rl: [[10, 13], [12, 17]] },
  // Held up by the head, kicking, in two poses.
  dangle: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 7], [2, 5]], ra: [[10, 8], [12, 10]], ll: [[6, 14], [6, 17]], rl: [[9, 14], [10, 16]] },
  dangle2: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[4, 8], [2, 10]], ra: [[10, 7], [12, 5]], ll: [[5, 14], [4, 16]], rl: [[8, 14], [8, 17]] },
};
{
  const c = POSES.cower, by = ([x, y]) => [x + 1, y];
  POSES.shiver = { h: by(c.h), n: by(c.n), p: by(c.p), la: c.la.map(by), ra: c.ra.map(by), ll: c.ll.map(by), rl: c.rl.map(by) };
}

// MOVES by the kind of sound, of FIGURES and TICKS in sound.js: its rank, and its poses, each with
// the least time in ms that it holds, at least HOLD.
const MOVES = {
  down: { rank: 4, poses: [["slump", 220], ["flat", 900]] },
  failing: { rank: 4, poses: [["shrug", 500]] },
  up: { rank: 3, poses: [["tada", 400]] },
  stop: { rank: 3, poses: [["halt", 400]] },
  takeover: { rank: 3, poses: [["shove", 260], ["mine", 400]] },
  recovered: { rank: 3, poses: [["pump", 200], ["pump2", 200], ["pump", 300]] },
  blip: { rank: 2, poses: [["tap", HOLD]] },
  error: { rank: 2, poses: [["facepalm", 400]] },
  box: { rank: 1, poses: [["wave", HOLD], ["wave2", HOLD], ["wave", HOLD]] },
  scan: { rank: 1, poses: [["point", HOLD]] },
  void: { rank: 1, poses: [["sweep", HOLD]] },
  select: { rank: 0, poses: [["nod", HOLD]] },
  miss: { rank: 0, poses: [["look", HOLD]] },
};

// POKES by name: its poses, each with the time in ms that it holds and the pixels of the grid that
// the figure is up off the line, and still, the one pose that it takes instead under reduced motion.
// One with a front faces the way the figure faces, or with a front of -1 away from it. sulk comes only
// of PESTERS pokes, and the others by chance.
const POKES = {
  startle: { still: "eek", poses: [["crouch", 80], ["eek", 60, 3], ["eek", 60, 6], ["eek", 80, 7], ["eek", 60, 6], ["eek", 60, 3], ["crouch", 140]] },
  flop: { still: "sit", front: 1, poses: [["trip", 160], ["flat", 700], ["sit", 600], ["crouch", 120], ["dust", 150], ["dust2", 150], ["dust", 150], ["dust2", 150]] },
  flip: { still: "tada", poses: [["crouch", 90], ["ball", 70, 4], ["upside", 110, 8], ["ball", 70, 7], ["ball", 70, 3], ["crouch", 140], ["tada", 500]] },
  bow: { still: "bow", front: 1, poses: [["flourish", 250], ["bow", 900], ["flourish", 200]] },
  cower: { still: "cower", poses: [["cower", 70], ["shiver", 70], ["cower", 70], ["shiver", 70], ["cower", 70], ["shiver", 70], ["cower", 400], ["look", 500]] },
  sulk: { still: "sulk", front: -1, poses: [["cross", 600], ["sulk", 2400]] },
};

// The walk to the right, in 12 frames, walk0 to walk11, from the pelvis at x 7. LEG gives, by the
// phase of a leg in the cycle, its knee and its foot as [dx, y] from x 7; the other leg is six
// phases on. From phase 0 to 6 the foot is on the line, and goes from 3 pixels in front of the
// pelvis to 3 behind it, a pixel a frame, as the figure moves a pixel forward: it stays put on the
// line. From phase 7 to 11 it swings forward, lifted, with the knee in front. In frames 0 and 6 both
// feet are on the line, a step apart, and the body is a pixel lower. Each arm swings against the leg
// of its side by SWING, and the head is a pixel forward, to the way it goes.
const LEG = [
  [[2, 14], [3, 17]], [[1, 14], [2, 17]], [[1, 14], [1, 17]], [[0, 14], [0, 17]], [[0, 14], [-1, 17]], [[-1, 14], [-2, 17]],
  [[-1, 14], [-3, 17]], [[0, 14], [-3, 16]], [[1, 13], [-2, 15]], [[3, 14], [0, 16]], [[3, 14], [2, 16]], [[3, 14], [3, 16]],
];
const SWING = [-3, -2, -2, 0, 2, 2, 3, 2, 2, 0, -2, -2];
for (let k = 0; k < 12; k++) {
  const down = k % 6 === 0 ? 1 : 0, j = (k + 6) % 12;
  const leg = (i) => LEG[i].map(([dx, y]) => [7 + dx, y]);
  const arm = (i) => [[7 + Math.round(SWING[i] * 0.6), 9 + down], [7 + SWING[i], (SWING[i] > 1 ? 10 : 11) + down]];
  POSES["walk" + k] = { h: [8, 3 + down], n: [7, 6 + down], p: [7, 11 + down], la: arm(j), ra: arm(k), ll: leg(k), rl: leg(j) };
}

// strolls says whether the pose is one of the stroll, standing or walking, and not of a dance.
const strolls = (name) => name === "stand" || /^-?walk/.test(name);

// figurePath of a pose: the d of a path of its pixels, lines between its joints by Bresenham's
// algorithm and the head, and l and r, its first and last column with a pixel.
function figurePath(pose) {
  const on = new Set();
  const dot = (x, y) => {
    if (x >= 0 && x < DW && y >= 0 && y < DH) on.add(y * DW + x);
  };
  const line = ([x0, y0], [x1, y1]) => {
    const dx = Math.abs(x1 - x0), dy = -Math.abs(y1 - y0), sx = x0 < x1 ? 1 : -1, sy = y0 < y1 ? 1 : -1;
    for (let err = dx + dy; ;) {
      dot(x0, y0);
      if (x0 === x1 && y0 === y1) return;
      const e2 = 2 * err;
      if (e2 >= dy) {
        err += dy;
        x0 += sx;
      }
      if (e2 <= dx) {
        err += dx;
        y0 += sy;
      }
    }
  };
  const [hx, hy] = pose.h;
  for (let d = -1; d <= 1; d++) {
    dot(hx + d, hy - 2);
    dot(hx + d, hy + 2);
    dot(hx - 2, hy + d);
    dot(hx + 2, hy + d);
  }
  line(pose.n, pose.p);
  for (const [a, b] of [pose.la, pose.ra]) {
    line(pose.n, a);
    line(a, b);
  }
  for (const [a, b] of [pose.ll, pose.rl]) {
    line(pose.p, a);
    line(a, b);
  }
  let d = "", l = DW, r = -1;
  for (const i of [...on].sort((a, b) => a - b)) {
    d += `M${i % DW} ${Math.floor(i / DW)}h1v1h-1z`;
    l = Math.min(l, i % DW);
    r = Math.max(r, i % DW);
  }
  return { d, l, r };
}

// mirror of a pose, across the middle of the grid.
function mirror(pose) {
  const m = ([x, y]) => [DW - 1 - x, y];
  const limb = ([a, b]) => [m(a), m(b)];
  return { h: m(pose.h), n: m(pose.n), p: m(pose.p), la: limb(pose.ra), ra: limb(pose.la), ll: limb(pose.rl), rl: limb(pose.ll) };
}

const dancer = {
  el: null, // the SVG #dancer
  box: null, // #figure, around the SVG: it is placed, moved, poked, and dragged
  home: null, // the frame of .logs, which always holds it
  watch: null, // the ResizeObserver of the frame that holds it
  paths: new Map(), // the path of each pose, and of its mirror: {d, l, r}
  pose: "", // the pose it shows
  moves: 0, // the dance poses it has taken so far
  since: -Infinity, // the time of the last dance pose, or of the stand of dancerRest
  move: null, // the move it dances: {rank, poses, i, until, rest}
  next: null, // the move that waits for its turn: {kind, due}
  turns: {}, // by kind: how many moves of the kind it has started, for the mirror
  still: window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false },
  live: false, // whether the page's animation frame runs it, which a poke and a drag need
  poke: null, // the poke that it plays: {poses, i, until}
  poked: "", // the name of the last poke, which the next one is not
  pokes: [], // the times of the pokes of the last PESTER ms
  lift: 0, // the pixels of its grid that it is up off the line
  hold: null, // the press on it: {id, x, y, held, at}: its pointer, where it went down, whether it has picked it up, and where the pointer is since
  fall: null, // the fall after a drop: {t, x, y, to}: its start, from where in px, and the x where it lands
};

// walker is the stroll: where the figure is, which way it faces, and what it does next.
const walker = {
  x: 0, // the pixels of the grid from its old place to where it is, to the right
  shown: "0 0", // the x and the lift that its transform shows
  dir: -1, // 1 when it faces right, -1 left
  width: 0, // the width of the frame in px, inside its border, from a ResizeObserver
  a: 0, b: 0, // the ends of the line, in px from the left of the frame inside its border: see stretch
  walking: false,
  k: 0, // the frame of the walk that it shows
  steps: 0, // the steps of the stretch still to go
  stop: false, // whether the next frame of the walk is the stand that ends the stretch
  due: 0, // the time of the next frame of the walk, or of the next decision
  after: "rest", // what the next decision comes after: start, walk, stand, or rest after a stop
  l: DW, r: -1, // the first and last column of a pixel of any pose of the stroll, which dancerStart finds
};

// strollChance decides what the figure does next, after a walk, a stand, or a rest: { stand: ms } or
// { walk: steps, turn }. It stands 3 to 12 s after a walk or a dance, and 1.5 to 6 s after the page
// loaded; after a stand it walks 1 to 6 steps, turning two times in five, or one time in five stands
// on.
function strollChance(after) {
  const r = Math.random;
  if (after === "stand" && r() < 0.8) return { walk: 1 + Math.floor(r() * 6), turn: r() < 0.4 };
  return { stand: after === "start" ? 1500 + r() * 4500 : 3000 + r() * 9000 };
}

// span of the x at which the pixels of a pose, from column l to column r, lie on the line.
function span(l, r) {
  return [Math.ceil((RIGHT + PX * DW - walker.width + walker.a) / PX - l), Math.floor((RIGHT - walker.width + walker.b) / PX + DW - 1 - r)];
}

// room says whether the figure can go n pixels the way dir, at the span of the stroll.
function room(dir, n) {
  const [lo, hi] = span(walker.l, walker.r), to = walker.x + dir * n;
  return to >= lo && to <= hi;
}

// dancerPath of a pose by its name, made once.
function dancerPath(name) {
  if (!dancer.paths.has(name)) {
    const pose = name.startsWith("-") ? mirror(POSES[name.slice(1)]) : POSES[name];
    dancer.paths.set(name, figurePath(pose));
  }
  return dancer.paths.get(name);
}

// dancerPlace moves the figure in until every pixel of its pose, and of any pose of its stroll, lies
// on the line, and shows it where it is, up off the line by its lift. While it is held or falls, the
// drag places it.
function dancerPlace() {
  if (!dancer.el || !dancer.pose || (dancer.hold && dancer.hold.held) || dancer.fall) return;
  if (walker.width) {
    const p = dancerPath(dancer.pose), [lo, hi] = span(Math.min(p.l, walker.l), Math.max(p.r, walker.r));
    walker.x = Math.max(lo, Math.min(hi, walker.x));
  }
  const lift = dancer.still.matches ? 0 : dancer.lift, shown = walker.x + " " + lift;
  if (walker.shown === shown) return;
  walker.shown = shown;
  dancer.box.style.transform = walker.x || lift ? `translate(${PX * walker.x}px, ${-PX * lift}px)` : "";
}

// dancerPose shows the pose, by its name, at the time now. A pose of the visitor's, of a poke or a
// drag, is not a dance pose.
function dancerPose(name, now, visitor = false) {
  if (!dancer.el) return;
  if (!strolls(name)) dancer.since = now;
  if (dancer.pose !== name) {
    dancer.el.firstElementChild.setAttribute("d", dancerPath(name).d);
    if (!strolls(name) && !visitor) dancer.moves++;
    dancer.pose = name;
    dancer.el.dataset.pose = name;
    dancer.el.dataset.moves = String(dancer.moves);
  }
  dancerPlace();
}

// dancerRest ends what the figure does, dance, walk, or poke, and makes it stand where it is at
// once. Its stroll goes on from there with a decision after a rest.
function dancerRest(now) {
  dancer.poke = null;
  dancer.lift = 0;
  dancer.move = null;
  dancer.next = null;
  walker.walking = false;
  walker.after = "rest";
  walker.due = now;
  dancer.since = now;
  dancerPose("stand", now);
}

// dancerHear is the watcher of sound.js: a sound of the kind starts in delay ms, or, with no kind,
// sound is off, and a dance stops. A poke or a drag goes on.
function dancerHear(kind, delay) {
  if (!kind) {
    if (dancer.poke || dancer.hold || dancer.fall) return;
    if (!strolls(dancer.pose) || dancer.move || dancer.next) dancerRest(performance.now());
    return;
  }
  const m = MOVES[kind];
  if (!m || !playing || halted || dancer.still.matches) return;
  const due = performance.now() + delay;
  if (!dancer.next || m.rank >= MOVES[dancer.next.kind].rank || due > dancer.next.due + STALE) dancer.next = { kind, due };
}

// dance on at the time now, from the animation frame of the page: it kicks while it is held, falls
// after a drop, plays a poke, dances while a move holds it, and strolls otherwise. Under reduced
// motion, and once the page has halted, it stands still, but for what the visitor does.
function dance(now) {
  dancer.live = true;
  if (dancer.hold && dancer.hold.held) return dancerPose(!dancer.still.matches && Math.floor(now / FLAIL) % 2 ? "dangle2" : "dangle", now, true);
  if (dancer.fall) return falling(now);
  if (dancer.poke) return poked(now);
  if (halted || dancer.still.matches) {
    if (dancer.pose !== "stand" || walker.walking || dancer.move || dancer.next) dancerRest(now);
    dancerPlace();
    return;
  }
  if (!playing && (dancer.move || dancer.next || !strolls(dancer.pose))) dancerRest(now);
  if (!danceOn(now)) stroll(now);
}

// danceOn dances on at the time now, and says whether the figure dances, so that it does not stroll.
function danceOn(now) {
  const move = dancer.move, next = dancer.next;
  if (move && now - dancer.since < HOLD) return true;
  // done is whether the move has held each of its poses.
  const done = !move || (move.i === move.poses.length - 1 && now >= move.until);
  if (next && now > next.due + STALE) dancer.next = null;
  else if (next && now >= next.due && now - dancer.since >= HOLD && (done || MOVES[next.kind].rank > move.rank)) {
    const m = MOVES[next.kind];
    const turn = (dancer.turns[next.kind] = (dancer.turns[next.kind] || 0) + 1);
    const flip = m.poses.length === 1 && turn % 2 === 0;
    dancer.move = { rank: m.rank, poses: m.poses.map(([name, ms]) => [flip ? "-" + name : name, ms]), i: 0, until: now + m.poses[0][1] };
    dancer.next = null;
    walker.walking = false;
    dancerPose(dancer.move.poses[0][0], now);
    return true;
  }
  if (!move) return false;
  if (now < move.until) return true;
  if (move.i + 1 < move.poses.length) {
    move.i++;
    move.until = now + move.poses[move.i][1];
    dancerPose(move.poses[move.i][0], now);
  } else if (now >= move.until + REST) dancerRest(now);
  return true;
}

// stroll on at the time now: decide what comes next once a stand is over, and walk a frame each PACE.
// A late frame, as after a hidden tab, moves one pixel and no more.
function stroll(now) {
  const w = walker;
  if (!w.width || now < w.due) return;
  if (!w.walking) {
    const d = strollChance(w.after);
    w.after = "stand";
    w.due = now + (d.stand > 0 ? d.stand : 1000);
    if (!(d.walk >= 1)) return;
    let dir = d.turn ? -w.dir : w.dir;
    if (!room(dir, STRIDE)) dir = -dir;
    if (!room(dir, STRIDE)) return;
    Object.assign(w, { walking: true, dir, k: 1, steps: Math.floor(d.walk), stop: false, due: now + PACE });
    return dancerPose(dir > 0 ? "walk1" : "-walk1", now);
  }
  w.due = now - w.due > PACE ? now + PACE : w.due + PACE;
  w.x += w.dir;
  if (w.stop) {
    Object.assign(w, { walking: false, after: "walk", due: now });
    return dancerPose("stand", now);
  }
  w.k = (w.k + 1) % 12;
  // In frames 0 and 6 a step is done: the stretch ends with the stand in the next frame, when its
  // steps are done or the next step and that stand would take it past the end.
  if (w.k % 6 === 0) w.stop = --w.steps <= 0 || !room(w.dir, STRIDE + 1);
  dancerPose((w.dir > 0 ? "" : "-") + "walk" + w.k, now);
}

// dancerPoke plays a poke at the time now: one by chance, not the one before, or sulk after PESTERS
// pokes in PESTER ms, which starts the count again.
function dancerPoke(now) {
  if (!dancer.live || dancer.hold || dancer.fall) return;
  dancer.pokes = dancer.pokes.filter((t) => now - t < PESTER);
  dancer.pokes.push(now);
  let name = "sulk";
  if (dancer.pokes.length < PESTERS) {
    const names = Object.keys(POKES).filter((n) => n !== "sulk" && n !== dancer.poked);
    name = names[Math.floor(Math.random() * names.length)];
  } else dancer.pokes = [];
  dancer.poked = name;
  const k = POKES[name], m = (k.front || 0) * walker.dir < 0 ? "-" : "";
  dancerRest(now);
  dancerPlay((dancer.still.matches ? [[k.still, STILL]] : k.poses).map(([n, ms, lift]) => [m + n, ms, lift]), now);
}

// dancerPlay starts the poses of a poke at the time now.
function dancerPlay(poses, now) {
  dancer.poke = { poses, i: -1, until: now };
  poked(now);
}

// poked plays on the poke at the time now; after its last pose the figure stands where it is.
function poked(now) {
  const k = dancer.poke;
  if (now < k.until) return;
  if (++k.i === k.poses.length) return dancerRest(now);
  const [name, ms, lift] = k.poses[k.i];
  k.until = now + ms;
  dancer.lift = lift || 0;
  dancerPose(name, now, true);
}

// home of the frame: the left and the bottom of the figure at its old place on the frame's top line,
// with no transform, in px of the viewport.
function home(frame) {
  const r = frame.getBoundingClientRect();
  return { left: r.left + frame.clientLeft + frame.clientWidth - RIGHT - PX * DW, bottom: r.top };
}

// stretch of the frame: the line on which the figure can stand, {a, b} in px from the left of the
// frame inside its border, or null for a frame that is not shown. It is the longest stretch of the
// top line with nothing of BLOCK in the room above it, as high as the figure, and GAP px from
// anything there, so that it never stands in front of a button. Only a line without a stretch as wide
// as the figure, which no layout of the page has, is whole. The frame of .logs always has its whole
// line.
function stretch(frame) {
  const r = frame.getBoundingClientRect(), left = r.left + frame.clientLeft, top = r.top;
  if (!r.width || !r.height) return null;
  const whole = { a: 0, b: frame.clientWidth };
  if (frame === dancer.home) return whole;
  let free = [[left, left + frame.clientWidth]];
  for (const el of document.querySelectorAll(BLOCK)) {
    if (frame.contains(el)) continue;
    const o = el.getBoundingClientRect();
    if (!o.width || !o.height || o.bottom <= top - PX * DH || o.top >= top) continue;
    free = free.flatMap(([a, b]) => [[a, Math.min(b, o.left - GAP)], [Math.max(a, o.right + GAP), b]]).filter(([a, b]) => b > a);
  }
  const [a, b] = free.reduce((w, s) => (s[1] - s[0] > w[1] - w[0] ? s : w), [0, 0]);
  return b - a >= PX * DW ? { a: a - left, b: b - left } : whole;
}

// dancerStage moves the figure into the frame, with its line s, and has it measure the frame.
function dancerStage(frame, s) {
  if (dancer.box.parentElement !== frame) {
    const focused = document.activeElement === dancer.box;
    frame.append(dancer.box);
    if (focused) dancer.box.focus({ preventScroll: true });
    if (dancer.watch) {
      dancer.watch.disconnect();
      dancer.watch.observe(frame);
    }
  }
  walker.width = frame.clientWidth;
  walker.a = s.a;
  walker.b = s.b;
}

// dancerRestage measures the frame that holds the figure again, after a change of layout, and lands
// it on another frame if this one is not shown any more.
function dancerRestage() {
  if (!dancer.box || dancer.hold || dancer.fall) return;
  const frame = dancer.box.parentElement, s = stretch(frame);
  if (s) dancerStage(frame, s);
  else {
    const r = dancer.box.getBoundingClientRect();
    dancerLand(r.left, r.bottom, false);
  }
  dancerPlace();
}

// dancerLand puts the figure, whose left and bottom are where they are in the viewport, onto the top
// line of a frame, by gravity: the first line below its feet, of the frames whose line is level with
// its feet or below them, that has its feet's x on it; if its feet are beside all of them, the
// nearest of them, at the closest point; and only if no line is below its feet, the nearest line of
// all. A frame that its feet are in does not hold it, as its line is above them: it falls on, out of
// the frame. It falls there when fall is true, and is there at once otherwise.
function dancerLand(left, bottom, fall) {
  const now = performance.now(), feet = left + (PX * DW) / 2;
  let best = null;
  for (const frame of document.querySelectorAll(STAGES)) {
    const s = stretch(frame);
    if (!s) continue;
    const r = frame.getBoundingClientRect(), a = r.left + frame.clientLeft + s.a, b = r.left + frame.clientLeft + s.b;
    const dx = Math.max(0, a - feet, feet - b), dy = r.top - bottom;
    // rank: 2 for a line below the feet at their x, 1 for one below them and beside, 0 for one above
    // them; far: how far the feet are from the line, straight down at rank 2.
    const below = dy >= -PX, rank = below ? (dx === 0 ? 2 : 1) : 0, far = rank === 2 ? dy : Math.hypot(dx, dy);
    if (!best || rank > best.rank || (rank === best.rank && far < best.far)) best = { frame, s, rank, far };
  }
  if (!best) return;
  dancerStage(best.frame, best.s);
  const h = home(best.frame), x = left - h.left, y = bottom - h.bottom;
  const p = dancerPath("stand"), [lo, hi] = span(Math.min(p.l, walker.l), Math.max(p.r, walker.r));
  const to = Math.max(lo, Math.min(hi, Math.round(x / PX)));
  dancerRest(now);
  walker.shown = null;
  if (!fall || y > -PX) {
    walker.x = to;
    delete dancer.box.dataset.aloft;
    dancerPlace();
    // A drop on its line, or up onto the nearest line when none is below, lands at once, in a crouch.
    if (fall) dancerPlay([["crouch", LAND]], now);
    return;
  }
  dancer.fall = { t: now, x, y, to };
  falling(now);
}

// falling goes on with the fall at the time now. It falls straight down at GRAVITY, and slides to the
// x where it lands on the way, by whole pixels of the grid; it lands in a crouch.
function falling(now) {
  const f = dancer.fall, e = now - f.t, y = f.y + (GRAVITY * e * e) / 2;
  if (y >= 0) {
    dancer.fall = null;
    delete dancer.box.dataset.aloft;
    walker.x = f.to;
    dancerRest(now);
    return dancerPlay([["crouch", LAND]], now);
  }
  const x = f.x + (PX * f.to - f.x) * (1 - y / f.y), at = (v) => PX * Math.round(v / PX);
  dancer.box.style.transform = `translate(${at(x)}px, ${at(y)}px)`;
  dancerPose("eek", now, true);
}

// dancerGrab, dancerDrag, and dancerDrop are the press on the figure, by Pointer Events, so that a
// mouse, a finger, and a pen work alike: a press that lets go before it moves TAP px is a poke, and
// one that moves picks the figure up and carries it, by its head, until it lets go. The figure has
// the pointer captured, so it hears of the pointer wherever it goes; if it loses it, it is let go.
// While it is carried, a scroll of the page keeps it under the pointer.
function dancerGrab(e) {
  if (!e.isPrimary || e.button !== 0 || !dancer.live || dancer.hold || dancer.fall) return;
  e.preventDefault();
  dancer.box.setPointerCapture(e.pointerId);
  dancer.hold = { id: e.pointerId, x: e.clientX, y: e.clientY, held: false };
}

// dancerCarry places the figure that is carried under the pointer, at x and y in the viewport, by its
// head, and keeps it and its hit area inside the viewport: anything past the right or the bottom edge
// would make the page larger.
function dancerCarry(x, y) {
  const o = home(dancer.box.parentElement), view = document.documentElement;
  dancer.hold.at = [x, y];
  const left = Math.max(HIT, Math.min(view.clientWidth - HIT - PX * DW, x - (PX * DW) / 2));
  const bottom = Math.max(PX * DH + HIT, Math.min(Math.max(view.clientHeight, window.innerHeight), y + PX * (DH - 1)));
  dancer.box.style.transform = `translate(${left - o.left}px, ${bottom - o.bottom}px)`;
}

function dancerDrag(e) {
  const h = dancer.hold;
  if (!h || e.pointerId !== h.id) return;
  if (!h.held) {
    if (Math.hypot(e.clientX - h.x, e.clientY - h.y) < TAP) return;
    dancerRest(performance.now());
    h.held = true;
    dancer.box.dataset.aloft = "";
  }
  dancerCarry(e.clientX, e.clientY);
}

function dancerDrop(e) {
  const h = dancer.hold;
  if (!h || e.pointerId !== h.id) return;
  dancer.hold = null;
  if (!h.held) {
    if (e.type === "pointerup") dancerPoke(performance.now());
    return;
  }
  const r = dancer.box.getBoundingClientRect();
  if (dancer.still.matches) delete dancer.box.dataset.aloft;
  dancerLand(r.left, r.bottom, !dancer.still.matches);
}

// dancerStart finds the figure and starts it standing at its old place, and has it measure the line,
// on which it strolls, now and whenever the line changes width.
function dancerStart() {
  dancer.el = $("dancer");
  dancer.box = $("figure");
  if (!dancer.el || !dancer.box) return;
  dancer.home = dancer.box.parentElement;
  dancer.box.addEventListener("pointerdown", dancerGrab);
  dancer.box.addEventListener("pointermove", dancerDrag);
  dancer.box.addEventListener("pointerup", dancerDrop);
  dancer.box.addEventListener("pointercancel", dancerDrop);
  dancer.box.addEventListener("lostpointercapture", dancerDrop);
  window.addEventListener("scroll", () => dancer.hold && dancer.hold.held && dancerCarry(...dancer.hold.at), { passive: true });
  // A click of no press, as a screen reader sends to a button, is a poke too.
  dancer.box.addEventListener("click", (e) => e.detail === 0 && dancerPoke(performance.now()));
  dancer.box.addEventListener("contextmenu", (e) => e.preventDefault());
  dancer.box.addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    if (!e.repeat) dancerPoke(performance.now());
  });
  for (const name of ["stand", ...Object.keys(POSES).filter((n) => n.startsWith("walk")).flatMap((n) => [n, "-" + n])]) {
    const p = dancerPath(name);
    walker.l = Math.min(walker.l, p.l);
    walker.r = Math.max(walker.r, p.r);
  }
  walker.after = "start";
  dancerPose("stand", performance.now());
  window.addEventListener("resize", dancerRestage);
  if (typeof ResizeObserver !== "function") return;
  dancer.watch = new ResizeObserver(dancerRestage);
  dancer.watch.observe(dancer.home);
}

// start the page: load run.js, connect the page, and start the engine. An error on the way ends in
// the failure box.
async function start() {
  drawMark();
  setTurbo(turboRemembered());
  dancerStart();
  const sound = import("./sound.js" + version).catch(() => null);
  R = await import("./run.js" + version);
  tell = R.teller();
  S = await sound;
  if (S) {
    S.wire($("sound"));
    if (S.watch) S.watch(dancerHear);
  }
  wire();

  fetch("engine/build.json" + version).then((r) => (r.ok ? r.json() : null)).then((b) => {
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
    ({ Engine } = await import("./engine/simulator.js" + version));
    const e = await Engine.start();
    await begin(e);
    engine = e;
  } catch (e) {
    halt("The engine did not load.", `${e.message}. Reload to try again.`);
    return;
  }
  set($("status"), "");
  root.dataset.engine = "ready";
  root.dataset.busy = "false";
  setPlaying(false);
  draw();
  requestAnimationFrame(frame);
}

start().catch((e) => halt("The page did not start.", `${(e && e.message) || e}. Reload to try again.`));
