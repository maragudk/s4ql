// A model of the storage design of s4ql, in JavaScript, for the simulator on the project site.
//
// This is not the Go code. It follows docs/spec.md ("Concurrency" and "Constraints"),
// internal/core/README.md, and the Go code in internal/core where the documents are silent. Where it
// is simpler than the design, the page says so.
//
// The model is a discrete-event simulation in virtual time. Nothing in it reads a real clock or
// draws a random number from the platform: one seed drives every choice, and the same seed with the
// same recorded actions replays the same run, step for step. The activities of an instance are
// generator functions, which yield to the scheduler when they sleep, take a lock, or send a request
// to S3, much like the coroutines of FoundationDB's simulation.

// Times in milliseconds of virtual time.
export const LEASE_MS = 30_000;
export const RENEW_AFTER_MS = 20_000;
export const WATCH_MS = 5_000;
export const STORE_TIMEOUT_MS = 5_000;
export const RETRY_SOON_MS = 100;
export const RETRY_AFTER_MS = 1_000;
// How long the application waits for a write before it gives up. It is longer than a takeover, so
// that a write that stays open while its instance stalls can still reach its swap.
export const CALLER_TIMEOUT_MS = 45_000;
export const EMPTY_LOG = "0".repeat(64);
export const MANIFEST_KEY = "manifest.json";
export const EPOCH = Date.UTC(2026, 10, 16, 12, 0, 0);
export const DEFAULT_SEED = 0x7334716c;

const BACKOFF = -1;

// margin that a spare waits beyond the holder's duration, as `margin` in internal/core/core.go.
export function margin(duration) {
  return Math.max(2_000, duration / 10);
}

export function formatDuration(ms) {
  return Math.round(ms / 1000) + "s";
}

export function parseDuration(s) {
  const m = /^(\d+)s$/.exec(s);
  return m ? Number(m[1]) * 1000 : NaN;
}

export function commitKey(name) {
  return "commits/" + name.slice(0, 2) + "/" + name;
}

// SHA-256, in plain JavaScript, so that Node and every browser agree and hashing stays synchronous.

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

const rotr = (x, n) => (x >>> n) | (x << (32 - n));

export function sha256(bytes) {
  const h = new Uint32Array([0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19]);
  const length = bytes.length;
  const padded = Math.ceil((length + 9) / 64) * 64;
  const buf = new Uint8Array(padded);
  buf.set(bytes);
  buf[length] = 0x80;
  const view = new DataView(buf.buffer);
  view.setUint32(padded - 8, Math.floor(length / 0x20000000));
  view.setUint32(padded - 4, (length * 8) >>> 0);
  const w = new Uint32Array(64);
  for (let off = 0; off < padded; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = view.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const a = w[i - 15];
      const b = w[i - 2];
      const s0 = rotr(a, 7) ^ rotr(a, 18) ^ (a >>> 3);
      const s1 = rotr(b, 17) ^ rotr(b, 19) ^ (b >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }
    let a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5], g = h[6], k = h[7];
    for (let i = 0; i < 64; i++) {
      const s1 = rotr(e, 6) ^ rotr(e, 11) ^ rotr(e, 25);
      const ch = (e & f) ^ (~e & g);
      const t1 = (k + s1 + ch + K[i] + w[i]) | 0;
      const s0 = rotr(a, 2) ^ rotr(a, 13) ^ rotr(a, 22);
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (s0 + maj) | 0;
      k = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
    }
    h[0] += a; h[1] += b; h[2] += c; h[3] += d; h[4] += e; h[5] += f; h[6] += g; h[7] += k;
  }
  const out = new Uint8Array(32);
  const outView = new DataView(out.buffer);
  for (let i = 0; i < 8; i++) outView.setUint32(i * 4, h[i]);
  return out;
}

const encoder = new TextEncoder();

export function sha256Hex(text) {
  const bytes = typeof text === "string" ? encoder.encode(text) : text;
  let s = "";
  for (const b of sha256(bytes)) s += b.toString(16).padStart(2, "0");
  return s;
}

// A small deterministic generator, sfc32, seeded from the seed and the name of a stream. Each part
// of the model draws from its own stream, so that turning one thing on does not change the
// randomness of another.

function mix32(x) {
  x = Math.imul(x ^ (x >>> 16), 0x21f0aaad);
  x = Math.imul(x ^ (x >>> 15), 0x735a2d97);
  return (x ^ (x >>> 15)) >>> 0;
}

export class Rand {
  constructor(seed, stream = "") {
    let s = mix32((seed >>> 0) ^ 0x9e3779b9);
    for (let i = 0; i < stream.length; i++) s = mix32(s ^ stream.charCodeAt(i));
    this.a = mix32(s + 1);
    this.b = mix32(s + 2);
    this.c = mix32(s + 3);
    this.d = mix32(s + 4);
    for (let i = 0; i < 12; i++) this.u32();
  }
  u32() {
    const t = (((this.a + this.b) | 0) + this.d) | 0;
    this.d = (this.d + 1) | 0;
    this.a = this.b ^ (this.b >>> 9);
    this.b = (this.c + (this.c << 3)) | 0;
    this.c = (this.c << 21) | (this.c >>> 11);
    this.c = (this.c + t) | 0;
    return t >>> 0;
  }
  float() {
    return this.u32() / 4294967296;
  }
  // int from lo to hi, both included.
  int(lo, hi) {
    return lo + Math.floor(this.float() * (hi - lo + 1));
  }
  chance(p) {
    return this.float() < p;
  }
  pick(list) {
    return list[Math.floor(this.float() * list.length)];
  }
  hex(chars) {
    let s = "";
    while (s.length < chars) s += this.u32().toString(16).padStart(8, "0");
    return s.slice(0, chars);
  }
}

// A clock of one machine: a reading that runs at a rate from a base time. Each machine has a wall
// clock and a monotonic clock. Both keep running while the process is stalled, as they do in a
// process pause.
class Clock {
  constructor(offset) {
    this.offset = offset;
    this.base = 0;
    this.rate = 1;
  }
  read(t) {
    return this.offset + this.rate * (t - this.base);
  }
  setRate(t, rate) {
    this.offset = this.read(t);
    this.base = t;
    this.rate = rate;
  }
  step(t, delta) {
    this.offset = this.read(t) + delta;
    this.base = t;
  }
}

// A binary heap of events, in the order of their time and then of when they were scheduled.
class Heap {
  constructor() {
    this.items = [];
  }
  get size() {
    return this.items.length;
  }
  peek() {
    return this.items[0];
  }
  static less(a, b) {
    return a.t < b.t || (a.t === b.t && a.seq < b.seq);
  }
  push(e) {
    const items = this.items;
    items.push(e);
    let i = items.length - 1;
    while (i > 0) {
      const p = (i - 1) >> 1;
      if (!Heap.less(items[i], items[p])) break;
      [items[i], items[p]] = [items[p], items[i]];
      i = p;
    }
  }
  pop() {
    const items = this.items;
    const top = items[0];
    const last = items.pop();
    if (items.length > 0) {
      items[0] = last;
      let i = 0;
      for (;;) {
        const l = 2 * i + 1;
        const r = l + 1;
        let m = i;
        if (l < items.length && Heap.less(items[l], items[m])) m = l;
        if (r < items.length && Heap.less(items[r], items[m])) m = r;
        if (m === i) break;
        [items[i], items[m]] = [items[m], items[i]];
        i = m;
      }
    }
    return top;
  }
}

export function short(name) {
  return name ? name.slice(0, 8) : "";
}

// pos is a position in the log, in words.
export function pos(name) {
  return name === EMPTY_LOG ? "the empty log" : short(name);
}

function manifestText(m) {
  return JSON.stringify(m, null, 2);
}

function makeManifest(head, lease) {
  const m = { version: 0, branches: { main: { head } } };
  if (lease) m.lease = lease;
  return m;
}

function copyManifest(m) {
  return JSON.parse(JSON.stringify(m));
}

export function commitRows(obj) {
  const rows = [];
  for (const change of obj.changes) for (const row of change.upsert.rows) rows.push(row[0]);
  return rows;
}

// The marked points of buggify, with how often each fires when its run has switched it on. Each is
// something legal but unusual.
export const BUGGIFY_POINTS = {
  "s3.conflict": { p: 0.1, text: "S3 refuses a swap from the right version with 409, as AWS documents while another write to the key is on its way" },
  "net.hold": { p: 0.05, text: "a request is held in the network for up to a minute, and arrives long after its client gave up" },
  "net.loseAnswer": { p: 0.04, text: "the answer to a request is lost after S3 applied it" },
  "net.failBeforeSend": { p: 0.04, text: "a request fails before it is sent" },
  "net.damaged": { p: 0.05, text: "a download of a commit object is damaged on its way" },
  "lease.renewEarly": { p: 0.2, text: "the holder renews its lease early" },
  "cache.commitFails": { p: 0.03, text: "the cache fails to commit after the manifest swap, rolled back or not" },
  "tx.slow": { p: 0.08, text: "a write transaction stays open for up to 20 seconds before it commits" },
};

// The planted bugs, each one wrong decision.
export const PLANTED_BUGS = {
  nocas: "The manifest is written without the compare-and-swap",
  ackunknown: "A commit whose swap had no answer counts as committed",
  cachefirst: "A commit reaches the cache before the manifest swap",
  nowait: "A spare takes the lease without waiting for it to time out",
};

export const FAULT_KINDS = {
  any: "any request",
  swap: "a swap of manifest.json",
  upload: "an upload of a commit object",
  read: "a read of manifest.json",
};

class Proc {
  constructor(sim, slot) {
    this.sim = sim;
    this.slot = slot;
    this.id = sim.rand.ids.hex(16);
    this.dead = false;
    this.stalled = false;
    this.deferred = [];
    this.holder = false;
    this.serving = false;
    this.unresolved = false;
    this.stopped = "";
    this.closing = false;
    this.man = null;
    this.version = "";
    this.flights = [];
    this.watch = { version: "", since: 0 };
    this.seen = null;
    this.unsure = false;
    this.sent = { wall: -Infinity, mono: -Infinity };
    this.locks = { writer: { owner: null, waiters: [] }, manifest: { owner: null, waiters: [] } };
    this.leaseTask = null;
    this.changes = 0;
    this.catchingUp = false;
    // pending are the writes of the application that this process runs and that have no outcome yet.
    this.pending = new Set();
    // timers that wait on the monotonic clock of this process.
    this.timers = new Set();
  }
}

export class Sim {
  constructor({ seed = DEFAULT_SEED, instances = 3 } = {}) {
    this.seed = seed >>> 0;
    this.instances = Math.min(3, Math.max(2, instances | 0));
    this.now = 0;
    this.steps = 0;
    this.seq = 0;
    this.heap = new Heap();
    this.rand = {};
    for (const s of ["ids", "salt", "net", "work", "chaos", "buggify", "clock", "boot"]) this.rand[s] = new Rand(this.seed, s);
    this.s3 = new Map();
    this.parsed = new Map();
    this.manifestWrites = 0;
    this.requests = [];
    this.reqSeq = 0;
    this.events = [];
    this.actions = [];
    this.queue = [];
    this.settings = { auto: false, chaos: false, buggify: false, bug: "", slow: false };
    this.armed = [];
    this.valueCounter = 0;
    this.autoToken = 0;
    this.chaosToken = 0;
    this.restartPending = new Set();
    this.stats = {
      acked: 0, notCommitted: 0, unknown: 0, refusedWrites: 0, reads: 0, refusedReads: 0, staleAllowed: 0,
      takes: 0, losses: 0, catchUps: 0, stops: 0, crashes: 0, resolvedCommitted: 0, resolvedNot: 0, resolvedLost: 0,
      buggified: 0, conflicts: 0, lateArrivals: 0,
    };
    this.buggifyOn = {};
    for (const name of Object.keys(BUGGIFY_POINTS)) this.buggifyOn[name] = this.rand.buggify.chance(0.6);
    this.slots = [];
    for (let i = 0; i < this.instances; i++) {
      const label = "ABC"[i];
      const jitter = this.rand.boot.int(-200, 200);
      this.slots.push({
        index: i,
        label,
        disk: { rows: [], position: EMPTY_LOG, failing: false },
        proc: null,
        partitioned: false,
        wallJitter: jitter,
        wall: new Clock(EPOCH + jitter),
        mono: new Clock(this.rand.boot.int(1, 900) * 3_600_000 + this.rand.boot.int(0, 3_599_999)),
        clockFault: { wallShort: false, monoShort: false, monoFast: false },
        lastExit: "",
      });
    }
    this.checker = new Checker(this);
    for (const slot of this.slots) {
      const at = this.rand.boot.int(0, 400);
      this.at(at, null, () => this.start(slot, "starts"));
    }
  }

  // Scheduling.

  at(t, proc, fn) {
    const e = { t: Math.max(this.now, Math.ceil(t)), seq: ++this.seq, proc, fn };
    this.heap.push(e);
    return e;
  }

  // after runs fn when ms have passed on the monotonic clock of p, which is what the timers of a
  // process run on: a clock that runs fast makes its timers fire early in true time. A timer follows
  // a change of the clock's rate while it waits (see armTimer).
  after(p, ms, fn) {
    const timer = { p, target: this.mono(p) + ms, fn, gen: 0 };
    p.timers.add(timer);
    this.armTimer(timer);
  }

  // armTimer schedules the next look of timer, at the true time when the clock of its process is
  // due to reach the target at the clock's present rate. Only the latest look counts, so arming
  // again after a change of the rate replaces the look before.
  armTimer(timer) {
    const p = timer.p;
    const gen = ++timer.gen;
    const left = Math.max(0, timer.target - this.mono(p));
    this.at(this.now + left / p.slot.mono.rate, p, () => {
      if (timer.gen !== gen || !p.timers.has(timer)) return;
      // The time is in whole milliseconds, so the clock can miss its target by a rounding error.
      if (timer.target - this.mono(p) > 1e-6) {
        this.armTimer(timer);
        return;
      }
      p.timers.delete(timer);
      timer.fn();
    });
  }

  rearmTimers(slot) {
    if (slot.proc) for (const timer of slot.proc.timers) this.armTimer(timer);
  }

  // step processes the next recorded action that is due, or the next event. It returns false only
  // when there is nothing at all to do, which does not happen while an instance runs.
  step() {
    const a = this.queue[0];
    if (a && a.n === this.steps) {
      this.queue.shift();
      this.now = Math.max(this.now, a.t);
      this.apply(a);
      this.checker.check();
      return true;
    }
    if (this.heap.size === 0) return false;
    const e = this.heap.pop();
    this.steps++;
    this.now = e.t;
    if (e.proc) {
      if (e.proc.dead) {
        if (e.dropped) e.dropped();
        this.checker.check();
        return true;
      }
      if (e.proc.stalled) {
        e.proc.deferred.push(e);
        this.checker.check();
        return true;
      }
    }
    e.fn();
    this.checker.check();
    return true;
  }

  // advance runs every action and event due up to the virtual time t, and then sets the clock to t.
  // If stopOnViolation, it stops early at the first violation that it finds, and returns false.
  advance(t, { stopOnViolation = false } = {}) {
    t = Math.floor(t);
    const before = this.checker.violations.length;
    for (;;) {
      if (stopOnViolation && this.checker.violations.length > before) return false;
      const a = this.queue[0];
      if (a && a.n === this.steps) {
        if (a.t > t) break;
        this.step();
        continue;
      }
      const next = this.heap.peek();
      if (!next || next.t > t) break;
      this.step();
    }
    this.now = Math.max(this.now, t);
    return true;
  }

  // Actions of the user, which are recorded with the step and the time at which they were applied.

  act(action) {
    const a = { n: this.steps, t: this.now, ...action };
    this.queue = [];
    this.apply(a);
    this.checker.check();
    return a;
  }

  // load actions to replay: each is applied when the run reaches its step.
  load(actions) {
    this.queue = actions.slice().sort((x, y) => x.n - y.n || 0);
  }

  apply(a) {
    const slot = a.i === undefined ? null : this.slots[a.i];
    if (a.i !== undefined && !slot) return;
    this.actions.push(a);
    const who = "you";
    switch (a.type) {
      case "write":
        this.clientWrite(slot, "you");
        break;
      case "read":
        this.clientRead(slot, "you");
        break;
      case "crash":
        this.crash(slot, who);
        break;
      case "restart":
        this.restart(slot, who);
        break;
      case "shutdown":
        this.shutdown(slot, who);
        break;
      case "stall":
        this.stall(slot, who);
        break;
      case "resume":
        this.resume(slot, who);
        break;
      case "partition":
        this.partition(slot, true, who);
        break;
      case "reconnect":
        this.partition(slot, false, who);
        break;
      case "disk":
        this.failDisk(slot, who);
        break;
      case "clock":
        this.clockFault(slot, a.c, who);
        break;
      case "drop":
      case "fail":
        this.arm(a.type, a.k || "any", who);
        break;
      case "slow":
        this.setSlow(!!a.on, who);
        break;
      case "auto":
        this.setAuto(!!a.on);
        break;
      case "chaos":
        this.setChaos(!!a.on);
        break;
      case "buggify":
        this.settings.buggify = !!a.on;
        this.log("you", "setting", a.on ? "Buggify is on. Its marked points do legal but unusual things." : "Buggify is off.");
        break;
      case "bug":
        this.settings.bug = PLANTED_BUGS[a.name] ? a.name : "";
        this.log("you", "setting", this.settings.bug ? "Planted bug: " + PLANTED_BUGS[this.settings.bug] + "." : "No planted bug.");
        break;
      case "heal":
        this.healAll(who);
        break;
    }
  }

  // The log of events, for the timeline.

  log(who, kind, text, level = "info", extra) {
    const e = { n: this.steps, t: this.now, who, kind, text, level };
    if (extra) Object.assign(e, extra);
    this.events.push(e);
    if (this.events.length > 20_000) this.events.splice(0, 5_000);
    return e;
  }

  // Buggify.

  buggify(name) {
    if (!this.settings.buggify || !this.buggifyOn[name]) return false;
    if (!this.rand.buggify.chance(BUGGIFY_POINTS[name].p)) return false;
    this.stats.buggified++;
    return true;
  }

  // The simulated S3: strongly consistent, with a version of each object made from its content, as
  // S3's ETag is, a compare-and-swap on that version, and a create that succeeds only if no object
  // is at the key.

  put(key, body, by) {
    const version = sha256Hex(body).slice(0, 32);
    this.s3.set(key, { body, version, t: this.now, by });
    if (key === MANIFEST_KEY) {
      this.manifestWrites++;
      this.checker.onManifest(JSON.parse(body));
    }
    return version;
  }

  commitObject(name) {
    if (this.parsed.has(name)) return this.parsed.get(name);
    const o = this.s3.get(commitKey(name));
    if (!o) return null;
    const obj = JSON.parse(o.body);
    this.parsed.set(name, obj);
    return obj;
  }

  applyRequest(req) {
    req.arrivedAt = this.now;
    if (req.arrivedAt - req.sentAt > STORE_TIMEOUT_MS) this.stats.lateArrivals++;
    const o = this.s3.get(req.key);
    const done = (result, res) => {
      req.result = result;
      return res;
    };
    switch (req.op) {
      case "get":
        if (!o) return done("not found", { err: "notfound" });
        return done("read", { ok: true, body: o.body, version: o.version });
      case "create":
        if (o) return done("refused: exists", { err: "exists" });
        return done("created", { ok: true, version: this.put(req.key, req.body, req.slot) });
      case "swap":
        if (!o) return done("refused: not found", { err: "notfound" });
        if (!req.unconditional) {
          if (o.version !== req.from) return done("refused: changed (412)", { err: "changed" });
          if (this.buggify("s3.conflict")) {
            this.stats.conflicts++;
            return done("refused: conflict (409)", { err: "conflict" });
          }
        }
        return done(req.unconditional ? "written without the swap" : "swapped", { ok: true, version: this.put(req.key, req.body, req.slot) });
    }
    return done("unknown request", { err: "failed" });
  }

  latency() {
    let ms = this.rand.net.int(8, 60);
    if (this.settings.slow) ms += this.rand.net.int(1_000, 6_000);
    return ms;
  }

  // takeArmed takes the armed fault that applies to the next request of the given kind: a failure
  // before sending first, and only without one, a lost answer.
  takeArmed(kind) {
    const out = { drop: false, fail: false };
    for (const type of ["fail", "drop"]) {
      const i = this.armed.findIndex((a) => a.type === type && (a.kind === "any" || a.kind === kind));
      if (i >= 0) {
        this.armed.splice(i, 1);
        out[type] = true;
        break;
      }
    }
    return out;
  }

  // send a request of the process p to S3. The callback gets the answer, or a timeout after
  // STORE_TIMEOUT_MS, or a failure.
  send(p, req, cb) {
    req.id = ++this.reqSeq;
    req.slot = p.slot.label;
    req.sentAt = this.now;
    req.result = "";
    req.client = "waiting";
    req.answered = false;
    this.requests.push(req);
    if (this.requests.length > 2_000) this.requests.splice(0, 500);
    const armed = this.takeArmed(req.kind);
    const answer = (client, res) => {
      if (req.answered) return false;
      req.answered = true;
      req.client = client;
      req.doneAt = this.now;
      cb(res);
      return true;
    };
    if (armed.fail || this.buggify("net.failBeforeSend")) {
      req.result = "never sent";
      req.fault = armed.fail ? "you" : "buggify";
      this.at(this.now + 1, p, () => answer("failed before it was sent", { err: "failed" }));
      return;
    }
    this.after(p, STORE_TIMEOUT_MS, () => answer("timed out", { err: "timeout" }));
    if (p.slot.partitioned) {
      req.result = "lost on its way";
      req.fault = "partition";
      return;
    }
    let up = this.latency();
    if (this.buggify("net.hold")) {
      up += this.rand.net.int(5_000, 60_000);
      req.fault = "buggify";
      req.held = true;
    }
    const dropAnswer = armed.drop || this.buggify("net.loseAnswer");
    if (dropAnswer) req.fault = armed.drop ? "you" : "buggify";
    this.at(this.now + up, null, () => {
      let res = this.applyRequest(req);
      if (dropAnswer) {
        req.answerLost = true;
        return;
      }
      if (req.op === "get" && res.ok && req.key !== MANIFEST_KEY && this.buggify("net.damaged")) {
        res = { ...res, body: res.body + " " };
        req.damaged = true;
      }
      this.at(this.now + this.latency(), p, () => {
        if (p.slot.partitioned) {
          req.answerLost = true;
          req.fault = req.fault || "partition";
          return;
        }
        if (req.answered) {
          req.lateAnswer = true;
          return;
        }
        answer("answered", res);
      });
    });
  }

  // Tasks: the activities of a process, as generators. A task yields an effect, and the scheduler
  // resumes it with the result.

  spawn(p, name, gen) {
    const task = { p, name, gen, sleep: null, wakeable: false };
    this.at(this.now, p, () => this.drive(task, undefined));
    return task;
  }

  drive(task, value) {
    const p = task.p;
    let v = value;
    for (;;) {
      if (p.dead) return;
      const r = task.gen.next(v);
      if (r.done) return;
      const e = r.value;
      switch (e.op) {
        case "sleep": {
          const token = {};
          task.sleep = token;
          task.wakeable = !!e.wakeable;
          this.after(p, e.ms, () => {
            if (task.sleep !== token) return;
            task.sleep = null;
            this.drive(task, "timer");
          });
          return;
        }
        case "s3":
          this.send(p, e.req, (res) => this.drive(task, res));
          return;
        case "lock": {
          const lock = p.locks[e.name];
          if (!lock.owner) {
            lock.owner = task;
            v = true;
            continue;
          }
          lock.waiters.push({ task, servingOnly: !!e.servingOnly });
          return;
        }
      }
      throw new Error("unknown effect " + e.op);
    }
  }

  unlock(p, name, task) {
    const lock = p.locks[name];
    if (lock.owner !== task) return;
    lock.owner = null;
    const next = lock.waiters.shift();
    if (next) {
      lock.owner = next.task;
      this.at(this.now, p, () => this.drive(next.task, true));
    }
  }

  // wake the lease task of p from its pause, after a change of state.
  wake(p) {
    p.changes++;
    const task = p.leaseTask;
    if (task && task.sleep && task.wakeable) {
      task.sleep = null;
      this.at(this.now, p, () => this.drive(task, "woken"));
    }
  }

  // failWaiters gives up the writers that wait for the writer lock, when p stops serving: a writer
  // that waits fails at once then.
  failWaiters(p) {
    const lock = p.locks.writer;
    const keep = [];
    for (const w of lock.waiters) {
      if (w.servingOnly) this.at(this.now, p, () => this.drive(w.task, false));
      else keep.push(w);
    }
    lock.waiters = keep;
  }

  // Clocks and the lease check.

  clocks(p) {
    return { wall: p.slot.wall.read(this.now), mono: p.slot.mono.read(this.now) };
  }

  mono(p) {
    return p.slot.mono.read(this.now);
  }

  // refusal is why p serves nothing now, or empty if it serves, as refusalLocked in core.go. Each
  // clock is compared only with itself.
  refusal(p) {
    if (!p.holder) return "it does not hold the lease";
    if (p.unresolved) return "a commit's outcome is unknown";
    if (!p.serving) return "it is not caught up";
    const c = this.clocks(p);
    if (!(c.mono - p.sent.mono < LEASE_MS)) return "its lease ran out by its monotonic clock";
    if (!(c.wall - p.sent.wall < LEASE_MS)) return "its lease ran out by its wall clock";
    return "";
  }

  servingErr(p) {
    if (p.stopped) return "it has stopped";
    return this.refusal(p);
  }

  // usableErr is the check of a transaction that is open: it ignores the clocks, because the swap
  // decides.
  usableErr(p) {
    if (p.stopped) return "it has stopped";
    if (!p.holder) return "it lost the lease";
    if (!p.serving) return this.refusal(p);
    return "";
  }

  // Processes.

  start(slot, verb) {
    const p = new Proc(this, slot);
    slot.proc = p;
    slot.disk.failing = false;
    this.restartPending.delete(slot.index);
    this.log(slot.label, "process", `${slot.label} ${verb} as holder ID ${p.id}. Its cache is at ${pos(slot.disk.position)}.`);
    p.leaseTask = this.spawn(p, "lease", this.leaseLoop(p));
  }

  kill(p) {
    p.dead = true;
    for (const e of p.deferred) if (e.dropped) e.dropped();
    p.deferred = [];
    for (const w of p.pending) {
      this.log(p.slot.label, "write", `Write ${w.value} on ${p.slot.label} ends with its process, without an answer. It is committed only if its swap landed.`, "info", { value: w.value, outcome: "lost with the process" });
    }
    p.pending.clear();
  }

  crash(slot, who) {
    const p = slot.proc;
    if (!p) return;
    this.kill(p);
    slot.proc = null;
    slot.lastExit = "crashed";
    this.stats.crashes++;
    this.log(who, "fault", `${slot.label} crashes. Its memory is gone; its cache on disk stays.`);
  }

  restart(slot, who) {
    const p = slot.proc;
    // A process that runs, or that is shutting down and releasing the lease, is not started again.
    if (p && (!p.stopped || p.closing)) return;
    if (p) this.kill(p);
    slot.proc = null;
    this.log(who, "fault", `${slot.label} is started again.`);
    this.start(slot, "starts");
  }

  shutdown(slot, who) {
    const p = slot.proc;
    if (!p || p.stopped) return;
    this.log(who, "fault", `${slot.label} shuts down cleanly.`);
    p.closing = true;
    this.stop(p, "it was shut down");
    this.spawn(p, "close", this.close(p));
  }

  stall(slot, who) {
    const p = slot.proc;
    if (!p || p.stalled) return;
    p.stalled = true;
    this.log(who, "fault", `${slot.label} stalls: the process is paused, and its clocks keep running.`);
  }

  resume(slot, who) {
    const p = slot.proc;
    if (!p || !p.stalled) return;
    p.stalled = false;
    const deferred = p.deferred;
    p.deferred = [];
    for (const e of deferred) {
      const again = this.at(this.now, p, e.fn);
      again.dropped = e.dropped;
    }
    this.log(who, "fault", `${slot.label} resumes after its stall.`);
  }

  partition(slot, on, who) {
    if (slot.partitioned === on) return;
    slot.partitioned = on;
    this.log(who, "fault", on ? `${slot.label} is cut off from S3.` : `${slot.label} reaches S3 again.`);
  }

  failDisk(slot, who) {
    if (slot.disk.failing) return;
    slot.disk.failing = true;
    this.log(who, "fault", `The disk of ${slot.label}'s cache fails. The next local read or write gets an I/O error.`);
  }

  clockFault(slot, c, who) {
    const t = this.now;
    const f = slot.clockFault;
    switch (c) {
      case "back":
        slot.wall.step(t, -60_000);
        f.wallShort = true;
        this.log(who, "fault", `${slot.label}'s wall clock is set back 60 s.`);
        break;
      case "ahead":
        slot.wall.step(t, 60_000);
        this.log(who, "fault", `${slot.label}'s wall clock is set ahead 60 s.`);
        break;
      case "fast":
        slot.mono.setRate(t, 2);
        f.monoFast = true;
        this.log(who, "fault", `${slot.label}'s monotonic clock runs twice as fast.`);
        break;
      case "slow":
        slot.mono.setRate(t, 0.5);
        slot.wall.setRate(t, 0.5);
        f.monoShort = true;
        f.wallShort = true;
        this.log(who, "fault", `Both of ${slot.label}'s clocks run at half speed.`);
        break;
      case "true":
        this.checker.onClocksTrue(slot);
        slot.clockFault = { wallShort: false, monoShort: false, monoFast: false };
        slot.mono.setRate(t, 1);
        slot.wall = new Clock(EPOCH + t + slot.wallJitter);
        this.log(who, "fault", `${slot.label}'s clocks run true again, and its wall clock is set right.`);
        break;
      default:
        return;
    }
    this.rearmTimers(slot);
  }

  arm(type, kind, who) {
    this.armed.push({ type, kind });
    const what = FAULT_KINDS[kind] || "any request";
    this.log(who, "fault", type === "drop" ? `The answer to the next request (${what}) will be lost. S3 still applies it.` : `The next request (${what}) will fail before it is sent.`);
  }

  setSlow(on, who) {
    if (this.settings.slow === on) return;
    this.settings.slow = on;
    this.log(who, "fault", on ? "S3 is slow: each way of a request takes 1 to 6 s more, so some requests time out after 5 s and arrive later." : "S3 is fast again.");
  }

  healAll(who) {
    for (const slot of this.slots) {
      if (slot.proc && slot.proc.stalled) this.resume(slot, who);
      if (slot.partitioned) this.partition(slot, false, who);
      if (slot.wall.rate !== 1 || slot.mono.rate !== 1 || slot.wall.read(this.now) !== EPOCH + this.now + slot.wallJitter) this.clockFault(slot, "true", who);
      if (!slot.proc || slot.proc.stopped) {
        if (slot.proc && slot.proc.closing && !slot.proc.dead) continue;
        this.restart(slot, who);
      }
    }
    for (const slot of this.slots) slot.disk.failing = false;
    this.armed = [];
    this.setSlow(false, who);
    this.log(who, "fault", "Every fault is cleared.");
  }

  // stop p for good, for a local I/O failure, something it cannot read, or a shutdown.
  stop(p, cause) {
    if (p.stopped) return;
    p.stopped = cause;
    p.serving = false;
    this.stats.stops++;
    this.failWaiters(p);
    this.wake(p);
    if (!p.closing) this.log(p.slot.label, "stop", `${p.slot.label} stops for good: ${cause}.`);
  }

  // Requests of a process, as effects.

  req(op, key, kind, extra) {
    return { op: "s3", req: { op, key, kind, ...extra } };
  }

  *readManifest(p) {
    const res = yield this.req("get", MANIFEST_KEY, "read");
    if (res.err === "notfound") return { found: false };
    if (res.err) return { err: res.err };
    return { found: true, man: JSON.parse(res.body), version: res.version };
  }

  // The lease, as in internal/core/lease.go.

  *leaseLoop(p) {
    let failures = 0;
    for (;;) {
      if (p.stopped) break;
      const changes = p.changes;
      let wait;
      if (!p.holder) wait = yield* this.acquire(p);
      else if (!p.serving) wait = yield* this.resumeHolder(p);
      else wait = yield* this.renew(p);
      if (wait === BACKOFF) {
        wait = Math.min(RETRY_AFTER_MS * 2 ** Math.min(failures, 10), Math.max(RETRY_AFTER_MS, WATCH_MS));
        failures++;
      } else {
        failures = 0;
      }
      if (wait > 0 && p.changes === changes && !p.stopped) yield { op: "sleep", ms: Math.ceil(wait), wakeable: true };
    }
    if (!p.closing) yield* this.releaseOnce(p);
  }

  *lockBoth() {
    yield { op: "lock", name: "writer" };
    yield { op: "lock", name: "manifest" };
  }

  // acquire the lease, catch up, and serve, or return how long to wait.
  *acquire(p) {
    const task = p.leaseTask;
    yield* this.lockBoth();
    try {
      if (p.stopped) return 0;
      const r = yield* this.readManifest(p);
      if (r.err) return BACKOFF;
      let previous = "";
      const m = r.found ? r.man : makeManifest(EMPTY_LOG);
      if (r.found && m.lease && m.lease.holder !== p.id) {
        previous = m.lease.holder;
        const duration = parseDuration(m.lease.duration);
        const now = this.mono(p);
        if (r.version !== p.watch.version) {
          p.watch = { version: r.version, since: now };
          this.log(p.slot.label, "lease", `${p.slot.label} watches the manifest at version ${r.version.slice(0, 8)}, whose lease names ${m.lease.holder}. It takes the lease only if the manifest stands still for ${(duration + margin(duration)) / 1000} s.`, "debug");
        }
        p.seen = { holder: m.lease.holder, duration, version: r.version };
        if (this.settings.bug !== "nowait") {
          const left = duration - (now - p.watch.since);
          if (left >= WATCH_MS) return WATCH_MS;
          const remaining = left + margin(duration);
          if (remaining > 0) return Math.min(remaining, WATCH_MS);
        }
      } else {
        p.seen = r.found && m.lease ? { holder: m.lease.holder, duration: parseDuration(m.lease.duration), version: r.version } : null;
      }

      // Every attempt to take the lease draws a new salt.
      const sent = this.clocks(p);
      const next = copyManifest(m);
      next.lease = { holder: p.id, duration: formatDuration(LEASE_MS), renewed: new Date(sent.wall).toISOString(), salt: this.rand.salt.hex(16) };
      const text = manifestText(next);
      const res = r.found
        ? yield this.req("swap", MANIFEST_KEY, "swap", { from: r.version, body: text, unconditional: this.settings.bug === "nocas", purpose: "take" })
        : yield this.req("create", MANIFEST_KEY, "create", { body: text, purpose: "take" });
      if (res.err === "changed" || res.err === "notfound" || res.err === "exists" || res.err === "conflict") return RETRY_SOON_MS;
      if (res.err) {
        p.unsure = true;
        return BACKOFF;
      }
      this.adopt(p, { man: next, sent }, res.version);
      p.watch = { version: "", since: 0 };
      this.stats.takes++;
      if (previous) this.checker.onTake(p.slot);
      const how = !r.found ? "it creates the manifest of a new database" : previous ? `the lease of ${previous} timed out` : "the manifest has no lease entry";
      this.log(p.slot.label, "lease", `${p.slot.label} takes the lease: ${how}.`, "info", { take: true, previous });
      return yield* this.resumeLocked(p);
    } finally {
      this.unlock(p, "manifest", task);
      this.unlock(p, "writer", task);
    }
  }

  adopt(p, f, version) {
    p.man = f.man;
    p.version = version;
    p.flights = [];
    p.unsure = false;
    p.holder = true;
    p.sent = f.sent;
  }

  lose(p, m) {
    p.version = "";
    p.flights = [];
    p.holder = false;
    p.serving = false;
    this.failWaiters(p);
    this.wake(p);
    this.stats.losses++;
    const holder = m && m.lease ? m.lease.holder : "";
    this.log(p.slot.label, "lease", `${p.slot.label} finds that it lost the lease${holder ? " to " + holder : ""}.`);
  }

  *resumeHolder(p) {
    const task = p.leaseTask;
    yield* this.lockBoth();
    try {
      if (p.stopped || !p.holder || p.serving) return 0;
      return yield* this.resumeLocked(p);
    } finally {
      this.unlock(p, "manifest", task);
      this.unlock(p, "writer", task);
    }
  }

  // resumeLocked resolves the swaps whose outcome is unknown, catches the cache up, and serves.
  *resumeLocked(p) {
    if (p.flights.length > 0) {
      const commit = p.flights.find((f) => f.isCommit);
      const o = yield* this.observe(p);
      if (o.kind === "nothing") return BACKOFF;
      if (o.kind === "lost") {
        if (commit) this.resolved(p, commit, "lease lost");
        return 0;
      }
      if (o.kind === "landed") {
        if (commit) this.resolved(p, commit, o.flight.isCommit ? "committed" : "not committed");
      } else {
        const wait = yield* this.renewLocked(p);
        if (p.flights.length > 0 || !p.holder) return wait || RETRY_SOON_MS;
      }
    }
    if (!(yield* this.keepLease(p))) return RETRY_SOON_MS;
    if (!(yield* this.catchUp(p))) return BACKOFF;
    if (!p.stopped) {
      p.serving = true;
      p.unresolved = false;
      this.wake(p);
      this.log(p.slot.label, "lease", `${p.slot.label} serves. Its cache is at ${pos(p.slot.disk.position)}.`);
    }
    return 0;
  }

  resolved(p, f, outcome) {
    const name = short(f.commit);
    if (outcome === "committed") {
      this.stats.resolvedCommitted++;
      this.log(p.slot.label, "resolved", `${p.slot.label} asked S3: the commit ${name} whose outcome was unknown is in the log. It is committed.`, "info", { outcome, commit: f.commit });
    } else if (outcome === "not committed") {
      this.stats.resolvedNot++;
      this.log(p.slot.label, "resolved", `${p.slot.label} renewed its lease from the version the commit ${name} was sent from, so that commit can never land. It is not committed.`, "info", { outcome, commit: f.commit });
    } else {
      this.stats.resolvedLost++;
      this.log(p.slot.label, "resolved", `${p.slot.label} lost the lease with the commit ${name} unknown. Catch-up will apply it if it is in the log.`, "info", { outcome, commit: f.commit });
    }
  }

  newFlight(p, commit, isCommit) {
    const sent = this.clocks(p);
    const next = copyManifest(p.man);
    next.lease = { holder: p.id, duration: formatDuration(LEASE_MS), renewed: new Date(sent.wall).toISOString(), salt: this.rand.salt.hex(16) };
    if (isCommit) next.branches.main.head = commit;
    return { man: next, text: manifestText(next), sent, commit, isCommit, attempts: 0, unknown: false };
  }

  // send one attempt of the swap f, from the version that p knows.
  *sendFlight(p, f) {
    f.attempts++;
    const res = yield this.req("swap", MANIFEST_KEY, "swap", {
      from: p.version, body: f.text, unconditional: this.settings.bug === "nocas", purpose: f.isCommit ? "commit" : "renewal",
    });
    if (res.ok) {
      this.adopt(p, f, res.version);
      return "applied";
    }
    if (res.err === "changed" || res.err === "notfound" || res.err === "exists" || res.err === "conflict") return "refused";
    f.unknown = true;
    if (!p.flights.includes(f)) p.flights.push(f);
    return "unknown";
  }

  // observe the manifest: unchanged, the manifest of one of p's swaps in flight, or another.
  *observe(p) {
    const r = yield* this.readManifest(p);
    if (r.err) return { kind: "nothing" };
    if (r.found && r.version === p.version) return { kind: "unchanged" };
    if (r.found && r.man.lease && r.man.lease.holder === p.id) {
      for (const f of p.flights) {
        if (f.man.lease.salt === r.man.lease.salt && f.man.branches.main.head === r.man.branches.main.head) {
          this.adopt(p, f, r.version);
          return { kind: "landed", flight: f };
        }
      }
    }
    this.lose(p, r.found ? r.man : null);
    return { kind: "lost", man: r.found ? r.man : null };
  }

  *renew(p) {
    const task = p.leaseTask;
    const early = this.buggify("lease.renewEarly");
    const wait = p.sent.mono + RENEW_AFTER_MS - this.mono(p);
    if (wait > 0 && !early) return wait;
    yield { op: "lock", name: "manifest" };
    try {
      if (p.stopped || !p.holder || !p.serving) return 0;
      if (p.sent.mono + RENEW_AFTER_MS > this.mono(p) && !early) return 0;
      return yield* this.renewLocked(p);
    } finally {
      this.unlock(p, "manifest", task);
    }
  }

  *keepLease(p) {
    if (p.sent.mono + RENEW_AFTER_MS <= this.mono(p)) yield* this.renewLocked(p);
    return !p.stopped && p.holder;
  }

  *renewLocked(p) {
    let f = null;
    for (const other of p.flights) if (!other.isCommit) f = other;
    const commit = p.flights.find((x) => x.isCommit);
    if (!f) f = this.newFlight(p, "", false);
    const result = yield* this.sendFlight(p, f);
    if (result === "applied") {
      if (commit) this.resolved(p, commit, "not committed");
      this.log(p.slot.label, "lease", `${p.slot.label} renews its lease.`, "debug");
      return 0;
    }
    if (result === "unknown") {
      this.log(p.slot.label, "lease", `${p.slot.label}'s renewal got no answer. Its lease still counts from the last renewal it knows of.`, "debug");
      return BACKOFF;
    }
    const o = yield* this.observe(p);
    switch (o.kind) {
      case "nothing":
        return BACKOFF;
      case "unchanged":
        return RETRY_SOON_MS;
      case "landed":
        if (commit) this.resolved(p, commit, o.flight.isCommit ? "committed" : "not committed");
        return 0;
      case "lost":
        if (commit) this.resolved(p, commit, "lease lost");
        return 0;
    }
    return 0;
  }

  *releaseOnce(p) {
    const task = p.leaseTask;
    yield { op: "lock", name: "manifest" };
    try {
      if (!p.version) return;
      const m = copyManifest(p.man);
      delete m.lease;
      const res = yield this.req("swap", MANIFEST_KEY, "swap", { from: p.version, body: manifestText(m), unconditional: this.settings.bug === "nocas", purpose: "release" });
      if (res.ok) {
        p.version = "";
        p.holder = false;
        this.log(p.slot.label, "lease", `${p.slot.label} releases the lease, its one try after the stop.`);
      } else {
        p.unsure = true;
        this.log(p.slot.label, "lease", `${p.slot.label}'s one try to release the lease failed. The lease will time out.`);
      }
    } finally {
      this.unlock(p, "manifest", task);
    }
  }

  // close is a clean shutdown: release the lease while the manifest names p, for at most as long
  // as the lease lasts, and then end the process.
  *close(p) {
    yield { op: "lock", name: "manifest" };
    const owner = p.locks.manifest.owner;
    const start = this.mono(p);
    try {
      while (p.version || p.unsure) {
        const r = yield* this.readManifest(p);
        if (!r.err) {
          if (!r.found || !r.man.lease || r.man.lease.holder !== p.id) {
            p.version = "";
            p.unsure = false;
            break;
          }
          const m = copyManifest(r.man);
          delete m.lease;
          const res = yield this.req("swap", MANIFEST_KEY, "swap", { from: r.version, body: manifestText(m), unconditional: this.settings.bug === "nocas", purpose: "release" });
          if (res.ok) {
            p.version = "";
            p.unsure = false;
            p.holder = false;
            this.log(p.slot.label, "lease", `${p.slot.label} releases the lease. The next instance can take it at once.`);
            break;
          }
        }
        if (this.mono(p) - start > LEASE_MS) {
          this.log(p.slot.label, "lease", `${p.slot.label} gives up releasing the lease. It will time out.`);
          break;
        }
        yield { op: "sleep", ms: RETRY_SOON_MS };
      }
    } finally {
      this.unlock(p, "manifest", owner);
    }
    this.kill(p);
    if (p.slot.proc === p) p.slot.proc = null;
    p.slot.lastExit = "shut down";
    this.log(p.slot.label, "process", `${p.slot.label} has shut down.`);
  }

  // Catch-up, as DB.catchUp in internal/core/catchup.go.

  *download(p, name) {
    let mismatches = 0;
    let wait = RETRY_SOON_MS;
    for (;;) {
      if (p.stopped || !p.holder) return null;
      const res = yield this.req("get", commitKey(name), "download");
      if (res.err === "notfound") {
        this.stop(p, `the log is broken: the commit object ${short(name)} is not in the bucket`);
        return null;
      }
      if (res.ok) {
        if (sha256Hex(res.body) === name) return JSON.parse(res.body);
        mismatches++;
        this.log(p.slot.label, "catchup", `The commit object ${short(name)} that ${p.slot.label} downloaded does not match its name. It downloads it again.`);
        if (mismatches === 2) {
          this.stop(p, `the commit object ${short(name)} did not match its name in two downloads in a row`);
          return null;
        }
        continue;
      }
      if (!(yield* this.keepLease(p))) return null;
      yield { op: "sleep", ms: wait };
      wait = Math.min(2 * wait, RETRY_AFTER_MS);
    }
  }

  *catchUp(p) {
    const disk = p.slot.disk;
    const from = disk.position;
    const head = p.man.branches.main.head;
    if (from === head) return true;
    p.catchingUp = true;
    try {
      const missing = [];
      for (let name = head; name !== from; ) {
        if (name === EMPTY_LOG) {
          this.stop(p, `its cache is at ${short(from)}, which is not in the log. It refuses the cache and does not rebuild it`);
          return false;
        }
        if (!(yield* this.keepLease(p))) return false;
        const obj = yield* this.download(p, name);
        if (!obj) return false;
        missing.push({ name, obj });
        name = obj.parent;
      }
      this.log(p.slot.label, "catchup", `${p.slot.label} catches up: ${missing.length} commit object${missing.length === 1 ? "" : "s"} from ${pos(from)} to ${short(head)}.`);
      for (let i = missing.length - 1; i >= 0; i--) {
        yield { op: "sleep", ms: 2 };
        if (p.stopped) return false;
        if (disk.failing) {
          this.stop(p, "a local I/O failure while it applied a commit object to its cache");
          return false;
        }
        disk.rows.push(...commitRows(missing[i].obj));
        disk.position = missing[i].name;
        this.checker.onCache();
      }
      this.stats.catchUps++;
      return true;
    } finally {
      p.catchingUp = false;
    }
  }

  // Writes and reads of the application.

  clientWrite(slot, source) {
    const value = ++this.valueCounter;
    const p = slot.proc;
    if (!p) {
      this.stats.refusedWrites++;
      this.log(slot.label, "write", `Write ${value} to ${slot.label} fails: ${slot.label} is not running.`, source === "you" ? "info" : "debug", { value, outcome: "refused" });
      return;
    }
    const w = { value, source, deadline: this.mono(p) + CALLER_TIMEOUT_MS };
    p.pending.add(w);
    this.at(this.now, p, () => this.spawn(p, "write", this.write(p, w)));
  }

  finishWrite(p, w, outcome, why) {
    p.pending.delete(w);
    const label = p.slot.label;
    const level = w.source === "you" || outcome === "acknowledged" || outcome === "outcome unknown" ? "info" : "debug";
    switch (outcome) {
      case "acknowledged":
        this.stats.acked++;
        this.log(label, "write", `Write ${w.value} on ${label} is acknowledged as ${short(w.name)}.`, level, { value: w.value, outcome, commit: w.name });
        break;
      case "refused":
        this.stats.refusedWrites++;
        this.log(label, "write", `Write ${w.value} on ${label} is refused: ${why}.`, level, { value: w.value, outcome });
        break;
      case "not committed":
        this.stats.notCommitted++;
        this.log(label, "write", `Write ${w.value} on ${label} is not committed: ${why}.`, "info", { value: w.value, outcome });
        break;
      case "outcome unknown":
        this.stats.unknown++;
        this.log(label, "write", `Write ${w.value} on ${label} returns "outcome unknown": ${why}. ${label} serves nothing until it knows.`, "info", { value: w.value, outcome, commit: w.name });
        break;
    }
  }

  // write is a write transaction that inserts one value: Begin, the insert, and Commit, as in
  // internal/core/tx.go.
  *write(p, w) {
    const deadline = w.deadline;
    let why = this.servingErr(p);
    if (why) return this.finishWrite(p, w, "refused", why);
    const got = yield { op: "lock", name: "writer", servingOnly: true };
    const owner = p.locks.writer.owner;
    if (!got) {
      return this.finishWrite(p, w, "refused", this.servingErr(p) || "it stopped serving while the write waited");
    }
    try {
      why = this.servingErr(p);
      if (why) return this.finishWrite(p, w, "refused", why);
      const parent = p.slot.disk.position;
      // The transaction runs: the insert goes into the cache's open transaction.
      const slow = this.buggify("tx.slow");
      yield { op: "sleep", ms: slow ? this.rand.work.int(2_000, 20_000) : this.rand.work.int(20, 200) };
      if (p.stopped) return this.finishWrite(p, w, "not committed", "the instance stopped");
      if (p.slot.disk.failing) {
        this.stop(p, "a local I/O failure in a write transaction");
        return this.finishWrite(p, w, "not committed", "a local I/O failure stopped the instance");
      }
      why = this.usableErr(p);
      if (why) return this.finishWrite(p, w, "not committed", why);

      const wall = this.clocks(p).wall;
      const body = JSON.stringify({
        version: 0,
        parent,
        time: new Date(wall).toISOString(),
        changes: [{ upsert: { table: "t", rows: [[w.value]] } }],
        annotations: [{ sql: "insert into t (v) values (?)", parameters: [w.value] }],
      });
      const name = sha256Hex(body);
      w.name = name;
      if (this.settings.bug === "cachefirst") {
        // The planted bug: the cache has the transaction before the log does.
        p.slot.disk.rows.push(w.value);
        p.slot.disk.position = name;
        this.checker.onCache();
      }

      // The upload needs no lock, and is sent again until it succeeds or the caller gives up.
      let wait = RETRY_SOON_MS;
      for (;;) {
        why = this.usableErr(p);
        if (why) return this.finishWrite(p, w, "not committed", why);
        const res = yield this.req("create", commitKey(name), "upload", { body, commit: name });
        if (res.ok || res.err === "exists") break;
        if (this.mono(p) >= deadline) return this.finishWrite(p, w, "not committed", "the caller gave up during the upload");
        yield { op: "sleep", ms: wait };
        wait = Math.min(2 * wait, RETRY_AFTER_MS);
      }

      const out = yield* this.publish(p, w, name, parent, deadline);
      if (out.result === "published") {
        // The swap is the commit. The cache commits second.
        yield { op: "sleep", ms: this.rand.work.int(1, 3) };
        if (this.settings.bug !== "cachefirst") this.commitCache(p, w, name);
        this.checker.onAck(name, p);
        return this.finishWrite(p, w, "acknowledged");
      }
      if (out.result === "unknown") return this.finishWrite(p, w, "outcome unknown", out.why);
      return this.finishWrite(p, w, "not committed", out.why);
    } finally {
      this.unlock(p, "writer", owner);
    }
  }

  // commitCache commits the transaction to the cache after the swap, or stops the instance.
  commitCache(p, w, name) {
    const disk = p.slot.disk;
    let landed = true;
    let failed = "";
    if (disk.failing) {
      landed = false;
      failed = "a local I/O failure in the commit of the cache after the manifest swap";
    } else if (this.buggify("cache.commitFails")) {
      landed = this.rand.buggify.chance(0.5);
      failed = landed ? "the commit of the cache went through and reported an I/O error" : "the commit of the cache was rolled back with an I/O error";
    }
    if (landed) {
      disk.rows.push(w.value);
      disk.position = name;
      this.checker.onCache();
    }
    if (failed) this.stop(p, failed + ". The write is still acknowledged, because S3 has it");
  }

  // publish swaps the manifest for one whose head is the commit object, and finds out what became
  // of the swap, as Tx.publish in tx.go. It holds the manifest lock until it knows.
  *publish(p, w, name, parent, deadline) {
    yield { op: "lock", name: "manifest" };
    const owner = p.locks.manifest.owner;
    try {
      let f = null;
      let wait = RETRY_SOON_MS;
      for (;;) {
        if (p.stopped && (!f || !f.unknown)) return { result: "unpublished", why: "the instance stopped" };
        if (!f) {
          if (!p.holder) return { result: "unpublished", why: "it lost the lease" };
          if (p.man.branches.main.head !== parent) return { result: "unpublished", why: "the log moved on since the transaction began" };
          f = this.newFlight(p, name, true);
        }
        if (this.mono(p) >= deadline) {
          if (!f.unknown) return { result: "unpublished", why: "the caller gave up" };
          if (this.settings.bug === "ackunknown") return { result: "published" };
          p.serving = false;
          p.unresolved = true;
          this.failWaiters(p);
          this.wake(p);
          return { result: "unknown", why: "the caller gave up before S3 answered" };
        }
        const result = yield* this.sendFlight(p, f);
        if (result === "applied") {
          if (f.unknown) {
            this.stats.resolvedCommitted++;
            this.log(p.slot.label, "resolved", `${p.slot.label} sent the swap of ${short(name)} again, and S3 applied it. It is committed.`, "info", { outcome: "committed", commit: name });
          }
          return { result: "published" };
        }
        if (result === "unknown") {
          this.log(p.slot.label, "commit", `${p.slot.label}'s swap for ${short(name)} got no answer. It may have been applied. ${p.slot.label} asks S3 again.`);
        } else {
          const o = yield* this.observe(p);
          if (o.kind === "landed") {
            if (o.flight === f) {
              this.stats.resolvedCommitted++;
              this.log(p.slot.label, "resolved", `${p.slot.label} read the manifest: its swap of ${short(name)} was applied. It is committed.`, "info", { outcome: "committed", commit: name });
              return { result: "published" };
            }
            // A renewal of p landed, from the version every attempt of the commit was sent from,
            // so none can land any more. The commit is sent again from the new version.
            f = null;
            continue;
          }
          if (o.kind === "lost") {
            if (!f.unknown) return { result: "unpublished", why: "S3 refused the swap: another instance has the lease" };
            return yield* this.inLog(p, o.man ? o.man.branches.main.head : EMPTY_LOG, name, parent, deadline);
          }
        }
        yield { op: "sleep", ms: wait };
        wait = Math.min(2 * wait, RETRY_AFTER_MS);
      }
    } finally {
      this.unlock(p, "manifest", owner);
    }
  }

  // inLog looks for the commit object in the log from head back to the transaction's parent.
  *inLog(p, head, name, parent, deadline) {
    let wait = RETRY_SOON_MS;
    for (let at = head; ; ) {
      if (at === name) {
        this.stats.resolvedCommitted++;
        this.log(p.slot.label, "resolved", `${p.slot.label} lost the lease, and found its commit ${short(name)} in the log. It is committed.`, "info", { outcome: "committed", commit: name });
        return { result: "published" };
      }
      if (at === parent || at === EMPTY_LOG) {
        this.stats.resolvedNot++;
        this.log(p.slot.label, "resolved", `${p.slot.label} lost the lease, and its commit ${short(name)} is not in the log. It is not committed.`, "info", { outcome: "not committed", commit: name });
        return { result: "unpublished", why: "it lost the lease, and the commit is not in the log" };
      }
      const res = yield this.req("get", commitKey(at), "download");
      if (res.ok && sha256Hex(res.body) === at) {
        at = JSON.parse(res.body).parent;
        wait = RETRY_SOON_MS;
        continue;
      }
      if (this.mono(p) >= deadline) {
        p.unresolved = true;
        return { result: "unknown", why: "the caller gave up while it looked for the commit in the log" };
      }
      yield { op: "sleep", ms: wait };
      wait = Math.min(2 * wait, RETRY_AFTER_MS);
    }
  }

  clientRead(slot, source) {
    const p = slot.proc;
    if (!p) {
      this.stats.refusedReads++;
      this.log(slot.label, "read", `A read on ${slot.label} fails: ${slot.label} is not running.`, source === "you" ? "info" : "debug");
      return;
    }
    const e = this.at(this.now, p, () => this.read(p, source));
    e.dropped = () => {
      this.stats.refusedReads++;
      this.log(slot.label, "read", `A read on ${slot.label} fails: the process ended before it ran.`, source === "you" ? "info" : "debug", { outcome: "refused" });
    };
  }

  // read checks the lease by both clocks and reads the cache, in one step.
  read(p, source) {
    const label = p.slot.label;
    const level = source === "you" ? "info" : "debug";
    const why = this.servingErr(p);
    if (why) {
      this.stats.refusedReads++;
      this.log(label, "read", `A read on ${label} is refused: ${why}.`, level, { outcome: "refused" });
      return;
    }
    if (p.slot.disk.failing) {
      this.stop(p, "a local I/O failure in a read of the cache");
      this.stats.refusedReads++;
      this.log(label, "read", `A read on ${label} fails: a local I/O failure.`, level, { outcome: "refused" });
      return;
    }
    const disk = p.slot.disk;
    this.stats.reads++;
    const rows = disk.rows.length;
    const ok = this.checker.onRead(p, disk.position);
    const tail = disk.rows.slice(-3).join(", ");
    this.log(label, "read", `A read on ${label} sees ${rows} row${rows === 1 ? "" : "s"}${rows ? " (… " + tail + ")" : ""} at ${pos(disk.position)}.${ok ? "" : " It misses an acknowledged commit."}`, ok ? level : "info", { outcome: "served", position: disk.position });
  }

  // The automatic workload and the random faults, each a stream of events from the seed.

  setAuto(on) {
    if (this.settings.auto === on) return;
    this.settings.auto = on;
    this.log("you", "setting", on ? "The automatic workload is on: writes and reads to random instances." : "The automatic workload is off.");
    if (on) {
      const token = ++this.autoToken;
      this.at(this.now + this.rand.work.int(100, 600), null, () => this.autoTick(token));
    }
  }

  autoTick(token) {
    if (!this.settings.auto || token !== this.autoToken) return;
    // Half the requests go to an instance that serves, if one does, as a load balancer that checks
    // health would send them, and the rest to any instance.
    const serving = this.slots.filter((s) => s.proc && instanceState(this, s).serving);
    const slot = serving.length > 0 && this.rand.work.chance(0.5) ? this.rand.work.pick(serving) : this.rand.work.pick(this.slots);
    if (this.rand.work.chance(0.4)) this.clientWrite(slot, "auto");
    else this.clientRead(slot, "auto");
    this.at(this.now + this.rand.work.int(150, 1_400), null, () => this.autoTick(token));
  }

  setChaos(on) {
    if (this.settings.chaos === on) return;
    this.settings.chaos = on;
    this.log("you", "setting", on ? "Random faults are on." : "Random faults are off. Faults already set stay until they end.");
    if (on) {
      const token = ++this.chaosToken;
      this.at(this.now + this.rand.chaos.int(1_000, 6_000), null, () => this.chaosTick(token));
    }
  }

  chaosTick(token) {
    if (!this.settings.chaos || token !== this.chaosToken) return;
    const r = this.rand.chaos;
    const who = "chaos";
    // Instances that are down or stopped are started again after a while.
    for (const slot of this.slots) {
      const p = slot.proc;
      const down = !p || (p.stopped && !p.closing);
      if (down && !this.restartPending.has(slot.index)) {
        this.restartPending.add(slot.index);
        this.at(this.now + r.int(1_000, 30_000), null, () => {
          this.restartPending.delete(slot.index);
          if (!slot.proc || (slot.proc.stopped && !slot.proc.closing)) this.restart(slot, who);
        });
      }
    }
    const slot = r.pick(this.slots);
    const p = slot.proc;
    const roll = r.float();
    if (roll < 0.12) {
      if (p) this.crash(slot, who);
    } else if (roll < 0.18) {
      if (p) this.shutdown(slot, who);
    } else if (roll < 0.32) {
      if (p && !p.stalled) {
        this.stall(slot, who);
        this.at(this.now + r.int(2_000, 50_000), null, () => {
          if (slot.proc === p) this.resume(slot, who);
        });
      }
    } else if (roll < 0.46) {
      if (!slot.partitioned) {
        this.partition(slot, true, who);
        this.at(this.now + r.int(1_000, 90_000), null, () => this.partition(slot, false, who));
      }
    } else if (roll < 0.58) {
      this.arm("drop", r.pick(Object.keys(FAULT_KINDS)), who);
    } else if (roll < 0.68) {
      this.arm("fail", r.pick(Object.keys(FAULT_KINDS)), who);
    } else if (roll < 0.76) {
      if (!this.settings.slow) {
        this.setSlow(true, who);
        this.at(this.now + r.int(3_000, 30_000), null, () => this.setSlow(false, who));
      }
    } else if (roll < 0.82) {
      if (p && !p.stopped) this.failDisk(slot, who);
    } else if (roll < 0.9) {
      this.clockFault(slot, r.pick(["back", "ahead", "fast", "slow"]), who);
      this.at(this.now + r.int(5_000, 60_000), null, () => this.clockFault(slot, "true", who));
    }
    this.at(this.now + r.int(2_000, 14_000), null, () => this.chaosTick(token));
  }
}

// The checker of the invariants. It runs after every step, and looks again at what changed.
export const INVARIANTS = {
  acked: "Every acknowledged commit is in the log.",
  chain: "The log is one chain, from the head back to the empty log.",
  ahead: "No cache is ahead of the manifest: every cache's position is in the log.",
  fresh: "A read that starts after a commit was acknowledged sees it.",
  replay: "Replaying the log from empty gives every cache's rows.",
};

class Checker {
  constructor(sim) {
    this.sim = sim;
    this.chain = [EMPTY_LOG];
    this.index = new Map([[EMPTY_LOG, 0]]);
    // flat holds the rows of the log in order, and ends[i] how many of them the log holds up to
    // and with chain[i].
    this.flat = [];
    this.ends = [0];
    // verified[i] is how many rows of slot i's cache are known to equal the start of flat.
    this.verified = sim.slots.map(() => 0);
    this.head = EMPTY_LOG;
    this.acked = new Set();
    this.ackedMax = 0;
    this.violations = [];
    this.seen = new Set();
    this.checks = { acked: 0, chain: 0, ahead: 0, fresh: 0, replay: 0 };
    this.dirty = true;
    // A stale read is allowed only where an instance measured time wrongly in a way that can cause
    // it. shortUntil[i] is until when reads on slot i are excused, because both of its clocks
    // measured too little time: while they do, and two lease durations after. earlyUntil is until
    // when reads on other instances are excused, because the instance earlyBy took the lease while
    // its monotonic clock ran fast; fastUntil[i] is until when a take by slot i counts as early.
    this.shortUntil = sim.slots.map(() => -1);
    this.fastUntil = sim.slots.map(() => -1);
    this.earlyUntil = -1;
    this.earlyBy = -1;
  }

  violate(key, text) {
    const id = key + ":" + text;
    if (this.seen.has(id)) return;
    this.seen.add(id);
    const v = { key, text, n: this.sim.steps, t: this.sim.now };
    this.violations.push(v);
    this.sim.log("checker", "violation", "Invariant violated: " + text, "info", { invariant: key });
  }

  // onClocksTrue records that the clocks of slot run true again from now.
  onClocksTrue(slot) {
    const f = slot.clockFault;
    const now = this.sim.now;
    if (f.wallShort && f.monoShort) this.shortUntil[slot.index] = now + 2 * LEASE_MS;
    // A wait that a fast clock shortened can end up to a lease and its margin later.
    if (f.monoFast) this.fastUntil[slot.index] = now + LEASE_MS + margin(LEASE_MS);
  }

  // onTake records that slot took the lease from another holder.
  onTake(slot) {
    if (slot.clockFault.monoFast || this.sim.now < this.fastUntil[slot.index]) {
      this.earlyUntil = this.sim.now + 2 * LEASE_MS;
      this.earlyBy = slot.index;
    }
  }

  // excuse is why a stale read on slot is allowed, or empty if it is not.
  excuse(slot) {
    const f = slot.clockFault;
    const now = this.sim.now;
    if ((f.wallShort && f.monoShort) || now < this.shortUntil[slot.index]) {
      return `both of ${slot.label}'s clocks measured too little time, so it may have served after its lease ran out`;
    }
    if (now < this.earlyUntil && slot.index !== this.earlyBy) {
      return `${this.sim.slots[this.earlyBy].label}'s monotonic clock ran fast, so it took the lease before ${slot.label}'s lease ran out by ${slot.label}'s clocks`;
    }
    return "";
  }

  onManifest(m) {
    const head = m.branches.main.head;
    this.checks.chain++;
    if (head === this.head) return;
    const walk = [];
    let at = head;
    while (!this.index.has(at)) {
      const obj = this.sim.commitObject(at);
      if (!obj) {
        this.violate("chain", `the manifest's head names ${short(at)}, and that commit object is not in the bucket`);
        return;
      }
      walk.push(at);
      at = obj.parent;
    }
    const meet = this.index.get(at);
    const forked = meet !== this.chain.length - 1;
    if (forked) {
      const cut = this.chain.slice(meet + 1);
      this.violate("chain", `the log forked: the new head ${short(head)} does not follow ${short(this.head)}, and ${cut.map(short).join(", ")} dropped out of the log`);
    }
    for (const name of this.chain.slice(meet + 1)) this.index.delete(name);
    this.chain.length = meet + 1;
    this.ends.length = meet + 1;
    this.flat.length = this.ends[meet];
    for (let k = 0; k < this.verified.length; k++) this.verified[k] = Math.min(this.verified[k], this.flat.length);
    for (let i = walk.length - 1; i >= 0; i--) {
      const name = walk[i];
      this.index.set(name, this.chain.length);
      this.flat.push(...commitRows(this.sim.commitObject(name)));
      this.ends.push(this.flat.length);
      this.chain.push(name);
    }
    this.head = head;
    this.dirty = true;
    this.checks.acked++;
    // Without a fork the log only grew, so every acknowledged commit is still in it at its index.
    if (!forked) return;
    this.ackedMax = 0;
    for (const name of this.acked) {
      const i = this.index.get(name);
      if (i === undefined) this.violate("acked", `the acknowledged commit ${short(name)} is no longer in the log`);
      else this.ackedMax = Math.max(this.ackedMax, i);
    }
  }

  onAck(name, p) {
    this.checks.acked++;
    this.acked.add(name);
    const i = this.index.get(name);
    if (i === undefined) {
      this.violate("acked", `${p.slot.label} acknowledged the commit ${short(name)}, which is not in the log`);
      return;
    }
    this.ackedMax = Math.max(this.ackedMax, i);
  }

  onCache() {
    this.dirty = true;
  }

  // onRead checks a read that saw the cache at position, and reports whether it saw every
  // acknowledged commit.
  onRead(p, position) {
    this.checks.fresh++;
    const i = this.index.get(position);
    if (i === undefined) return true;
    if (i >= this.ackedMax) return true;
    const missed = this.chain[this.ackedMax];
    const excuse = this.excuse(p.slot);
    if (excuse) {
      this.sim.stats.staleAllowed++;
      this.sim.log("checker", "stale", `A read on ${p.slot.label} missed the acknowledged commit ${short(missed)}. The spec allows this only because an instance measured time wrongly: ${excuse}.`);
      return false;
    }
    this.violate("fresh", `a read on ${p.slot.label} at ${pos(position)} missed the acknowledged commit ${short(missed)}`);
    return false;
  }

  check() {
    if (!this.dirty) return;
    this.dirty = false;
    for (const slot of this.sim.slots) {
      const disk = slot.disk;
      this.checks.ahead++;
      const i = this.index.get(disk.position);
      if (i === undefined) {
        this.violate("ahead", `${slot.label}'s cache is at ${short(disk.position)}, which is not in the log`);
        continue;
      }
      this.checks.replay++;
      // The rows of a cache are only ever appended to, so the rows that were checked before and are
      // still in the log need no second look.
      const want = this.ends[i];
      const got = disk.rows;
      let same = want === got.length;
      for (let k = this.verified[slot.index]; same && k < want; k++) same = this.flat[k] === got[k];
      if (same) this.verified[slot.index] = want;
      if (!same) this.violate("replay", `${slot.label}'s cache has ${got.length} rows that differ from the ${want} that replaying the log to its position gives`);
    }
  }
}

// What an instance is doing, in words, for the page.
export function instanceState(sim, slot) {
  const p = slot.proc;
  if (!p) return { word: slot.lastExit === "shut down" ? "shut down" : slot.lastExit === "crashed" ? "crashed" : "not running", serving: false };
  let word;
  if (p.stopped) word = p.closing ? "shutting down" : "stopped";
  else if (!p.holder) word = "waiting for the lease";
  else if (p.unresolved) word = "resolving an unknown outcome";
  else if (!p.serving) word = p.catchingUp ? "catching up" : "taking over";
  else if (sim.refusal(p)) word = "lease ran out by its clock";
  else word = "serving";
  return { word, serving: word === "serving", stalled: p.stalled };
}

// Encoding of actions for a URL: step.time.code, joined by "_".
const CODES = {
  write: "w", read: "r", crash: "c", restart: "R", shutdown: "x", stall: "s", resume: "u", partition: "p",
  reconnect: "h", disk: "d", clock: "k", drop: "D", fail: "F", slow: "S", auto: "A", chaos: "C", buggify: "B", bug: "X", heal: "H",
};
const TYPES = Object.fromEntries(Object.entries(CODES).map(([k, v]) => [v, k]));
const CLOCKS = { back: "b", ahead: "a", fast: "f", slow: "s", true: "t" };
const CLOCKS_BACK = Object.fromEntries(Object.entries(CLOCKS).map(([k, v]) => [v, k]));
const KINDS = { any: "a", swap: "s", upload: "u", read: "g" };
const KINDS_BACK = Object.fromEntries(Object.entries(KINDS).map(([k, v]) => [v, k]));

export function encodeActions(actions) {
  return actions.map((a) => {
    let code = CODES[a.type];
    switch (a.type) {
      case "clock": code += CLOCKS[a.c] + a.i; break;
      case "drop": case "fail": code += KINDS[a.k || "any"]; break;
      case "slow": case "auto": case "chaos": case "buggify": code += a.on ? "1" : "0"; break;
      case "bug": code += a.name || "-"; break;
      case "heal": break;
      default: code += a.i;
    }
    return a.n + "." + a.t + "." + code;
  }).join("_");
}

export function decodeActions(s) {
  if (!s) return [];
  const out = [];
  for (const part of s.split("_")) {
    const m = /^(\d+)\.(\d+)\.([A-Za-z])(.*)$/.exec(part);
    if (!m) return null;
    const type = TYPES[m[3]];
    if (!type) return null;
    const a = { n: Number(m[1]), t: Number(m[2]), type };
    const rest = m[4];
    switch (type) {
      case "clock":
        a.c = CLOCKS_BACK[rest[0]];
        a.i = Number(rest.slice(1));
        if (!a.c || !/^[0-2]$/.test(rest.slice(1))) return null;
        break;
      case "drop": case "fail":
        a.k = KINDS_BACK[rest] || "any";
        break;
      case "slow": case "auto": case "chaos": case "buggify":
        a.on = rest === "1";
        break;
      case "bug":
        a.name = rest === "-" ? "" : rest;
        break;
      case "heal":
        break;
      default:
        if (!/^[0-2]$/.test(rest)) return null;
        a.i = Number(rest);
    }
    out.push(a);
  }
  for (let i = 1; i < out.length; i++) if (out[i].n < out[i - 1].n || out[i].t < out[i - 1].t) return null;
  return out;
}

// replay a run: the seed, the recorded actions, and the virtual time to run to.
export function replay({ seed, instances, actions, until }) {
  const sim = new Sim({ seed, instances });
  sim.load(actions);
  sim.advance(until);
  return sim;
}
