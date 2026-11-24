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
// What S3 holds outlives a reload, as S3 would: bucket.js keeps the tab's database in sessionStorage,
// from the changes that each answer brings, and the page gives it to the engine when it starts the
// session. The instances start on it with empty caches, as on new machines, and fake time goes on
// from where it was. Reset empties the tab's database and starts over.
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
let B = null; // the module bucket.js, which start loads
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
let keeper = null; // the tab's database, from bucket.js
let storageVersion = ""; // the storage version of the engine
let base = 0; // the time of the history at the time 0 of the session
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
  get history() { return base + t; },
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
  if (total >= B.CAP || full) {
    fullStop(total);
    return;
  }
  if (jobs.length) pump();
}

// take the events of an answer to the job, if any, into what the page keeps. The steps of a reset of
// the clocks take one line in the log and one mark on the field, at the first.
function take(answer, job = {}) {
  t = answer.t;
  for (const e of answer.events) if (e.kind === "store" && e.what === "seeded") base = e.by || 0;
  keepBucket(answer);
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
const AGAIN = "Reload to go on with what S3 holds, or Reset to start over.";

// keepBucket keeps the changes of the answer in the tab's database. If the browser's storage refuses
// them, as when large values of the box fill it, S3 is full: the page stops at once, and a reload
// starts over.
function keepBucket(answer) {
  if (keeper.keep(answer, storageVersion, base + answer.t)) full = true;
}
let full = false; // whether the browser's storage refused the changes of an answer

// fullStop halts the page because S3 is full: at the cap of commit objects, or in the browser's
// storage.
function fullStop(total) {
  halt("S3 is full.", full
    ? `The browser's storage for this tab takes no more, at ${count(total)} commit objects. Reset to start over.`
    : `The database has ${count(total)} commit objects. Reset to start over.`);
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
  dance(ts);
  pump();
  requestAnimationFrame(frame);
}

// Drawing.

function draw() {
  root.dataset.t = String(Math.floor(t));
  root.dataset.lines = String(linesTotal);
  set($("clock"), R.clock(base + t));
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
  const out = lines.map((l) => `<li class="${l.tier}"><span class="t">${R.clock(base + l.t)}</span><span class="text">${esc(l.text)}</span></li>`);
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
  $("play").addEventListener("click", () => {
    setPlaying(!playing);
    // A statement of the box that is on its way when the page pauses is answered as on a paused page.
    if (!playing && box.pending && !jobs.some((j) => j.until)) {
      jobs.push({ until: true });
      pump();
    }
  });
  $("reset").addEventListener("click", () => reset());
  $("turbo").addEventListener("click", () => setTurbo(!turbo));
  const text = $("sql-text");
  const kept = keeper.boxText();
  if (kept !== null) text.value = kept;
  text.addEventListener("input", () => {
    // A text over the limit is not kept, so that it cannot fill the browser's storage.
    if (chars(text.value) <= MAX_SQL) keeper.keepBoxText(text.value);
    drawBox();
  });
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
const wallOffset = (st) => st.wall - state.t - base;

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
  set($("s3-path"), `s4ql-sim/${keeper.name}/`);
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
      `<div class="meta"><span class="name">${R.short(c.name)}</span><span>${R.commitKeysText(c)}</span><span>${esc(R.instanceOf(c.process))}</span><span>${R.clock(base + c.at)}</span><span class="caps">${tag}</span></div>` +
      (sql ? `<ul class="sql">${sql}</ul>` : "") + `</li>`;
  }).join("");
  if (!out) out = `<li class="empty">None yet.</li>`;
  if (total > b.commits.length) out += `<li class="empty">${count(total - b.commits.length)} older</li>`;
  html($("commits"), out);
}

// Start.

// SETTINGS of the session: a starts alone, b stands by, and the client calls a.
const SETTINGS = { workload: { callers: 0 }, client: { instance: "a" }, standby: true };

// begin a session on the engine, on the tab's stored database if it has one. If the engine refuses
// the database, the page drops it and begins on an empty one, with a line that says so.
async function begin(e) {
  const stored = keeper.load();
  const settings = { seed: R.newSeed(), ...SETTINGS, prefix: keeper.name };
  let answer, dropped = keeper.note === "damaged";
  try {
    answer = await e.start(stored ? { ...settings, bucket: stored } : settings);
  } catch (err) {
    // Only a refusal of the stored database drops it. Any other failure leaves it for a reload.
    if (!stored || !String(err.message).startsWith("the bucket is refused")) throw err;
    keeper.reset();
    dropped = true;
    answer = await e.start(settings);
  }
  storageVersion = answer.version;
  base = 0;
  take(answer);
  state = (await e.now(COMMITS)).state;
  // The line of a stored database says who waits for the lease that its manifest names, from the
  // state at the start.
  if (dropped) note(answer.t, { tier: "story", text: "What the browser held of S3 could not be read, so S3 starts empty." });
  else if (stored) {
    const n = stored.objects.filter((o) => !o.key.endsWith("/manifest.json")).length;
    note(answer.t, { tier: "story", text: R.storedLine(keeper.name, n, state.bucket.manifest, state.instances) });
  }
}

// reset empties the tab's database and starts a new session at time 0, paused, with the page as at
// its start. It works while the page plays, is paused, or has stopped; an answer of the session
// before that is still on its way is dropped.
async function reset() {
  if (resetting || !engine) return;
  resetting = true;
  gen++;
  if (S && S.blip) S.blip();
  setPlaying(false);
  keeper.reset();
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
  full = false;
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
// dances to the sounds. sound.js tells it, through watch, of each figure and tick that plays and
// when it starts, and the figure takes the move of that kind of sound as it starts to sound. It
// dances only while sound is on, the page plays, and the visitor allows motion, and stands still
// otherwise.
//
// Turbo brings ticks every few tens of ms, so a pose holds at least HOLD: the figure follows the
// sounds at most five poses a second. A sound that comes while a pose holds waits, unless one of a
// higher rank comes, and is left out once it is STALE. A move is one pose or a few, by the kind of
// its sound; a move of one pose takes turns with its mirror, so that a run of one sound is a dance
// and not a pose that stays. A move cuts into another only from a higher rank; once its poses have
// held, any move may follow, and after REST the figure stands again.
//
// The figure is drawn on a grid of DW by DH of its pixels from the joints of each pose, with lines a
// pixel wide and a ring for a head, as crisp as the bars of the page. The SVG #dancer says its pose
// in data-pose, and in data-moves how many times it has changed pose, for a check.
const DW = 15, DH = 18; // the grid of the figure, in its pixels; the feet are on its last row
const HOLD = 200; // ms, the least time that a pose holds
const STALE = 300; // ms, how long after its sound a move may still start
const REST = 450; // ms, how long the last pose of a move stays before the figure stands

// POSES by name: the head's centre h, the neck n, the pelvis p, the elbow and hand of each arm, la
// and ra, and the knee and foot of each leg, ll and rl, on the grid, with the line under row 17. l is
// the viewer's left. up lifts the whole pose by that many rows, for a jump. Each pose has a mirror,
// named "-" and its name.
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
  // Panic, for the alarm: arms flailing over its head, and jumps.
  panic: { h: [7, 3], n: [7, 6], p: [7, 11], la: [[3, 5], [4, 1]], ra: [[11, 4], [13, 2]], ll: [[5, 14], [4, 17]], rl: [[9, 14], [10, 17]] },
  jump: { up: 1, h: [7, 3], n: [7, 6], p: [7, 11], la: [[3, 4], [1, 2]], ra: [[11, 5], [10, 1]], ll: [[6, 14], [6, 17]], rl: [[8, 14], [8, 17]] },
};

// MOVES by the kind of sound, of FIGURES and TICKS in sound.js: its rank, and its poses, each with
// the least time in ms that it holds, at least HOLD.
const MOVES = {
  alarm: { rank: 5, poses: [["panic", 220], ["jump", 220], ["-panic", 220], ["-jump", 220]] },
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

// figurePath of a pose, as the d of a path of its pixels: lines between its joints by Bresenham's
// algorithm, and the head.
function figurePath(pose) {
  const on = new Set();
  const dot = (x, y) => {
    y -= pose.up || 0;
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
  let d = "";
  for (const i of [...on].sort((a, b) => a - b)) d += `M${i % DW} ${Math.floor(i / DW)}h1v1h-1z`;
  return d;
}

// mirror of a pose, across the middle of the grid.
function mirror(pose) {
  const m = ([x, y]) => [DW - 1 - x, y];
  const limb = ([a, b]) => [m(a), m(b)];
  return { up: pose.up, h: m(pose.h), n: m(pose.n), p: m(pose.p), la: limb(pose.ra), ra: limb(pose.la), ll: limb(pose.rl), rl: limb(pose.ll) };
}

const dancer = {
  el: null, // the SVG #dancer
  paths: new Map(), // the d of each pose, and of its mirror
  pose: "", // the pose it shows
  moves: 0, // the changes of pose so far
  since: -Infinity, // the time of the last change of pose
  move: null, // the move it dances: {rank, poses, i, until, rest}
  next: null, // the move that waits for its turn: {kind, due}
  turns: {}, // by kind: how many moves of the kind it has started, for the mirror
  still: window.matchMedia ? window.matchMedia("(prefers-reduced-motion: reduce)") : { matches: false },
};

// dancerPose shows the pose, by its name, at the time now.
function dancerPose(name, now) {
  if (!dancer.el) return;
  dancer.since = now;
  if (dancer.pose === name) return;
  if (!dancer.paths.has(name)) {
    const pose = name.startsWith("-") ? mirror(POSES[name.slice(1)]) : POSES[name];
    dancer.paths.set(name, figurePath(pose));
  }
  dancer.el.firstElementChild.setAttribute("d", dancer.paths.get(name));
  if (dancer.pose) dancer.moves++;
  dancer.pose = name;
  dancer.el.dataset.pose = name;
  dancer.el.dataset.moves = String(dancer.moves);
}

// dancerStand makes the figure stand at once, and forgets its moves.
function dancerStand() {
  dancer.move = null;
  dancer.next = null;
  dancerPose("stand", performance.now());
}

// dancerHear is the watcher of sound.js: a sound of the kind starts in delay ms, or, with no kind,
// sound is off.
function dancerHear(kind, delay) {
  if (!kind) return dancerStand();
  const m = MOVES[kind];
  if (!m || !playing || dancer.still.matches) return;
  const due = performance.now() + delay;
  if (!dancer.next || m.rank >= MOVES[dancer.next.kind].rank || due > dancer.next.due + STALE) dancer.next = { kind, due };
}

// dance on at the time now, from the animation frame of the page.
function dance(now) {
  if (!playing || dancer.still.matches) {
    if (dancer.pose !== "stand" || dancer.move || dancer.next) dancerStand();
    return;
  }
  if (now - dancer.since < HOLD) return;
  const move = dancer.move, next = dancer.next;
  // done is whether the move has held each of its poses.
  const done = !move || (move.i === move.poses.length - 1 && now >= move.until);
  if (next && now > next.due + STALE) dancer.next = null;
  else if (next && now >= next.due && (done || MOVES[next.kind].rank > move.rank)) {
    const m = MOVES[next.kind];
    const turn = (dancer.turns[next.kind] = (dancer.turns[next.kind] || 0) + 1);
    const flip = m.poses.length === 1 && turn % 2 === 0;
    dancer.move = { rank: m.rank, poses: m.poses.map(([name, ms]) => [flip ? "-" + name : name, ms]), i: 0, until: now + m.poses[0][1] };
    dancer.next = null;
    dancerPose(dancer.move.poses[0][0], now);
    return;
  }
  if (!move || now < move.until) return;
  if (move.i + 1 < move.poses.length) {
    move.i++;
    move.until = now + move.poses[move.i][1];
    dancerPose(move.poses[move.i][0], now);
  } else if (now >= move.until + REST) {
    dancer.move = null;
    dancerPose("stand", now);
  }
}

// start the page: load run.js, connect the page, and start the engine. An error on the way ends in
// the failure box.
async function start() {
  drawMark();
  dancer.el = $("dancer");
  dancerStand();
  const sound = import("./sound.js" + version).catch(() => null);
  R = await import("./run.js" + version);
  B = await import("./bucket.js" + version);
  keeper = new B.Keeper();
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
    if (!keeper.stored) note(t, { tier: "story", text: "The browser does not store S3 for this tab: a reload starts over." });
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
