// The run of the simulator page as data: its settings and schedule, the request that the engine
// takes for it, the link that holds it, and the words for what the engine reports. Nothing here
// touches the page, so the module can be read and checked on its own.
//
// The request and the events are those of internal/simulator/README.md, which is the reference.

export const DEFAULT_SEED = 0x7334716c;
export const MAX_SEED = Number.MAX_SAFE_INTEGER;
export const EMPTY_LOG = "0".repeat(64);

// PRESET is simstore.AllFaults, which the tests of the core run with.
export const PRESET = Object.freeze({
  lostRequest: 0.02, lostResponse: 0.02, hang: 0.2, conflict: 0.1, slow: 0.02, slowBy: 45000,
  brokenRead: 0.02, cutOff: 0.005, cutOffFor: 40000, outage: 0.002, outageFor: 20000,
  late: 0.5, afterCrash: 0.5, lateBy: 60000,
});

// FAULTS are the faults of the store that happen at random, with what each does. A probability is
// for each request, and a duration, in milliseconds, is an upper bound.
export const FAULTS = [
  { key: "lostRequest", kind: "p", text: "A request is lost on its way to S3." },
  { key: "lostResponse", kind: "p", text: "S3 handles a request, and its response is lost on the way back." },
  { key: "hang", kind: "p", text: "The caller of a lost request gets no answer and waits until it gives up." },
  { key: "slow", kind: "p", text: "One way of a request takes longer, by up to:" },
  { key: "slowBy", kind: "d" },
  { key: "brokenRead", kind: "p", text: "The content of a read breaks off." },
  { key: "cutOff", kind: "p", text: "A request cuts its process off from S3, for up to:" },
  { key: "cutOffFor", kind: "d" },
  { key: "outage", kind: "p", text: "A request makes S3 unreachable for every process, for up to:" },
  { key: "outageFor", kind: "d" },
  { key: "conflict", kind: "p", text: "S3 refuses a correct swap while another write to the key is in flight." },
  { key: "late", kind: "p", text: "A write whose caller gave up is still applied later, by up to:" },
  { key: "afterCrash", kind: "p", text: "A write whose sender crashed is still applied later, by the same." },
  { key: "lateBy", kind: "d" },
  { key: "damaged", kind: "p", text: "A download of a commit object arrives with one byte changed." },
];
const FAULT_KEYS = FAULTS.map((f) => f.key);

// PURPOSES of a request, which an armed fault can be for.
export const PURPOSES = {
  any: "any request",
  read: "read of the manifest",
  take: "take of the lease",
  commit: "commit, a swap of the manifest",
  renew: "renewal of the lease",
  release: "release of the lease",
  upload: "upload of a commit object",
  download: "download of a commit object",
};

export const ENDS = [60000, 120000, 300000, 600000];
export const LEASES = [10000, 30000, 60000];
export const LATENCIES = [0, 50, 250, 1000];
export const THINKS = [1000, 5000, 15000];
export const SPANS = [10000, 20000, 40000, 60000];

// defaults of the page's run: three instances for two minutes under the faults of the preset, with a
// crash of b, which takes the lease first under the default seed, and its restart.
export function defaults() {
  return {
    seed: DEFAULT_SEED,
    instances: 3,
    end: 120000,
    lease: 30000,
    latency: 50,
    buggify: false,
    faults: { ...PRESET },
    callers: 2,
    think: 5000,
    writes: 40,
    schedule: [
      { at: 40000, do: "crash", instance: "b" },
      { at: 60000, do: "restart", instance: "b" },
    ],
  };
}

export const instanceName = (i) => String.fromCharCode(97 + i);

// request of the engine for the run.
export function request(run) {
  return {
    seed: run.seed,
    end: run.end,
    instances: run.instances,
    leases: [run.lease],
    latency: run.latency,
    faults: { ...run.faults },
    buggify: run.buggify,
    workload: { callers: run.callers, think: run.think, writes: run.writes },
    schedule: run.schedule.map((a) => ({ ...a })),
  };
}

// The kinds of actions, by the parameters they take after the instance.
const INSTANCE_ACTIONS = new Set(["write", "read", "crash", "restart", "close", "open", "stopWall", "startWall", "stopMonotonic", "startMonotonic"]);

// insert the action into the schedule after every action at its time or before it, so that the
// actions before its time keep their indexes, and the run before that time stays the same.
export function insert(schedule, action) {
  let i = 0;
  while (i < schedule.length && schedule[i].at <= action.at) i++;
  return [...schedule.slice(0, i), action, ...schedule.slice(i)];
}

// The link holds the run after "#": v=1, then the seed in hexadecimal, the settings, the faults, the
// schedule, and the time shown, t, in milliseconds. A link without v=1 is not one of this page.

function faultsText(f) {
  if (FAULT_KEYS.every((k) => (f[k] || 0) === (PRESET[k] || 0))) return "all";
  const set = FAULT_KEYS.filter((k) => f[k]);
  if (set.length === 0) return "none";
  return set.map((k) => k + ":" + f[k]).join(",");
}

function actionText(a) {
  const parts = [a.at, a.do];
  if (INSTANCE_ACTIONS.has(a.do)) parts.push(a.instance);
  else if (a.do === "cutOff") parts.push(a.instance, a.for);
  else if (a.do === "shiftWall") parts.push(a.instance, a.by);
  else if (a.do === "outage") parts.push(a.for);
  else if (a.do === "loseAnswer" || a.do === "failRequest") {
    parts.push(a.request);
    if (a.instance) parts.push(a.instance);
  } else if (a.do === "workload") parts.push(a.on ? 1 : 0);
  return parts.join(".");
}

export function encode(run, t) {
  const p = [
    "v=1",
    "seed=" + run.seed.toString(16),
    "n=" + run.instances,
    "end=" + run.end,
    "lease=" + run.lease,
    "latency=" + run.latency,
    "faults=" + faultsText(run.faults),
    "buggify=" + (run.buggify ? 1 : 0),
    "callers=" + run.callers,
    "think=" + run.think,
    "writes=" + run.writes,
  ];
  if (run.schedule.length) p.push("s=" + run.schedule.map(actionText).join("~"));
  if (t !== undefined && t > 0) p.push("t=" + Math.round(t));
  return p.join("&");
}

const integer = (s, min, max) => {
  if (!/^-?\d+$/.test(s)) return null;
  const n = Number(s);
  return Number.isSafeInteger(n) && n >= min && n <= max ? n : null;
};

function parseFaults(s) {
  if (s === "all") return { ...PRESET };
  if (s === "none") return {};
  const f = {};
  for (const kv of s.split(",")) {
    const [k, v] = kv.split(":");
    const fault = FAULTS.find((x) => x.key === k);
    if (!fault || k in f || !/^\d+(\.\d+)?(e-?\d+)?$/.test(v || "")) return null;
    const n = Number(v);
    if (fault.kind === "p" ? n > 1 : n > 1800000) return null;
    f[k] = n;
  }
  return f;
}

function parseAction(s, run) {
  const f = s.split(".");
  const at = integer(f[0], 0, run.end - 1);
  const name = f[1];
  if (at === null) return null;
  const inst = (x) => x !== undefined && /^[a-e]$/.test(x) && x.charCodeAt(0) - 97 < run.instances;
  if (INSTANCE_ACTIONS.has(name)) return f.length === 3 && inst(f[2]) ? { at, do: name, instance: f[2] } : null;
  if (name === "cutOff" || name === "shiftWall") {
    if (f.length !== 4 || !inst(f[2])) return null;
    const n = name === "cutOff" ? integer(f[3], 1, 1800000) : integer(f[3], -86400000, 86400000);
    if (n === null || n === 0) return null;
    return name === "cutOff" ? { at, do: name, instance: f[2], for: n } : { at, do: name, instance: f[2], by: n };
  }
  if (name === "outage") {
    const n = f.length === 3 ? integer(f[2], 1, 1800000) : null;
    return n === null ? null : { at, do: name, for: n };
  }
  if (name === "calm") return f.length === 2 ? { at, do: name } : null;
  if (name === "loseAnswer" || name === "failRequest") {
    if (!(f[2] in PURPOSES) || f.length > 4 || (f.length === 4 && !inst(f[3]))) return null;
    return f.length === 4 ? { at, do: name, request: f[2], instance: f[3] } : { at, do: name, request: f[2] };
  }
  if (name === "workload") return f.length === 3 && (f[2] === "0" || f[2] === "1") ? { at, do: name, on: f[2] === "1" } : null;
  return null;
}

// decode the link after "#". It returns null for a link that is not one of this page, or that has
// anything that the page cannot use.
export function decode(hash) {
  const fields = new Map();
  for (const part of hash.replace(/^#/, "").split("&")) {
    if (!part) continue;
    const i = part.indexOf("=");
    if (i < 0) return null;
    const k = part.slice(0, i);
    if (fields.has(k)) return null;
    fields.set(k, part.slice(i + 1));
  }
  if (fields.get("v") !== "1") return null;
  const known = ["v", "seed", "n", "end", "lease", "latency", "faults", "buggify", "callers", "think", "writes", "s", "t"];
  for (const k of fields.keys()) if (!known.includes(k)) return null;
  const run = defaults();
  const seed = fields.get("seed");
  if (seed !== undefined) {
    if (!/^[0-9a-f]{1,14}$/.test(seed) || parseInt(seed, 16) > MAX_SEED) return null;
    run.seed = parseInt(seed, 16);
  }
  const num = (k, min, max, set) => {
    if (!fields.has(k)) return true;
    const n = integer(fields.get(k), min, max);
    if (n === null) return false;
    set(n);
    return true;
  };
  if (!num("n", 1, 5, (n) => (run.instances = n))) return null;
  if (!num("end", 1, 1800000, (n) => (run.end = n))) return null;
  if (!num("lease", 1000, 600000, (n) => (run.lease = n))) return null;
  if (!num("latency", 0, 60000, (n) => (run.latency = n))) return null;
  if (!num("buggify", 0, 1, (n) => (run.buggify = n === 1))) return null;
  if (!num("callers", 0, 8, (n) => (run.callers = n))) return null;
  if (!num("think", 0, 1800000, (n) => (run.think = n))) return null;
  if (!num("writes", 0, 100, (n) => (run.writes = n))) return null;
  if (fields.has("faults")) {
    const f = parseFaults(fields.get("faults"));
    if (!f) return null;
    run.faults = f;
  }
  run.schedule = [];
  if (fields.get("s")) {
    const items = fields.get("s").split("~");
    if (items.length > 1000) return null;
    for (const s of items) {
      const a = parseAction(s, run);
      if (!a) return null;
      if (run.schedule.length && a.at < run.schedule[run.schedule.length - 1].at) return null;
      run.schedule.push(a);
    }
  }
  let t = 0;
  if (fields.has("t")) {
    t = integer(fields.get("t"), 0, run.end);
    if (t === null) return null;
  }
  return { run, t };
}

// Words.

export function clock(ms) {
  const sign = ms < 0 ? "−" : "";
  ms = Math.abs(ms);
  const m = Math.floor(ms / 60000);
  const s = ((ms % 60000) / 1000).toFixed(3).padStart(6, "0");
  return sign + m + ":" + s;
}

export function span(ms) {
  ms = Math.abs(ms);
  if (ms >= 60000 && ms % 60000 === 0) return ms / 60000 + " min";
  if (ms >= 1000) return +(ms / 1000).toFixed(ms % 1000 ? 1 : 0) + " s";
  return Math.round(ms) + " ms";
}

export const short = (name) => (name === EMPTY_LOG ? "the empty log" : name ? name.slice(0, 8) : "?");
const atHead = (name) => (!name || name === EMPTY_LOG ? ", while the log is empty" : ", at the head " + short(name));

export function keysText(keys) {
  if (!keys || keys.length === 0) return "no keys";
  if (keys.length <= 6) return (keys.length === 1 ? "key " : "keys ") + keys.join(", ");
  return keys.length + " keys, " + keys.slice(0, 3).join(", ") + " … " + keys.slice(-2).join(", ");
}

export function actionWords(a) {
  switch (a.do) {
    case "write": return `Write on ${a.instance}`;
    case "read": return `Read on ${a.instance}`;
    case "crash": return `Crash ${a.instance}`;
    case "restart": return `Restart ${a.instance}`;
    case "close": return `Close the database of ${a.instance}`;
    case "open": return `Open the database of ${a.instance}`;
    case "cutOff": return `Cut ${a.instance} off from S3 for ${span(a.for)}`;
    case "shiftWall": return `Shift the wall clock of ${a.instance} ${a.by < 0 ? "back" : "forward"} by ${span(a.by)}`;
    case "stopWall": return `Stop the wall clock of ${a.instance}`;
    case "startWall": return `Start the wall clock of ${a.instance}`;
    case "stopMonotonic": return `Stop the monotonic clock of ${a.instance}`;
    case "startMonotonic": return `Start the monotonic clock of ${a.instance}`;
    case "loseAnswer": return `Lose the answer to the next ${PURPOSES[a.request]}${a.instance ? " from " + a.instance : ""}`;
    case "failRequest": return `Fail the next ${PURPOSES[a.request]}${a.instance ? " from " + a.instance : ""} before it is sent`;
    case "outage": return `Make S3 unreachable for ${span(a.for)}`;
    case "calm": return "Calm S3: no new random faults";
    case "workload": return a.on ? "Switch the workload on" : "Switch the workload off";
  }
  return a.do;
}

const REASONS = {
  not_holder: "it does not hold the lease",
  outcome_unknown: "it does not know yet whether its last commit went through",
  not_caught_up: "its cache is behind the log",
  lease_expired_monotonic: "its lease ran out by its monotonic clock",
  lease_expired_wall: "its lease ran out by its wall clock",
};

const PHASES = {
  to_store: "on its way to S3",
  held: "held in the network, to be applied later",
  stuck: "stuck in the network",
  to_caller: "on its way back",
  done: "done",
};
export const phaseWords = (p) => PHASES[p] || p;

function requestWhat(e) {
  const commit = e.key && e.key.startsWith("db/commits/") ? short(e.key.slice(-64)) : "";
  switch (e.purpose) {
    case "read": return "read of the manifest";
    case "download": return "download of " + commit;
    case "upload": return "upload of " + commit;
    case "take": return "take of the lease";
    case "commit": return "commit swap of the manifest";
    case "renew": return "renewal swap of the manifest";
    case "release": return "release swap of the manifest";
  }
  return e.method + " of " + e.key;
}
export const requestWords = requestWhat;

// Classes of events in the log: a "routine" event is hidden unless the visitor asks for every event,
// and a "loud" one is drawn inverted.

// describe an event in words. names maps holder IDs to the processes that opened them. It returns
// {text, routine, loud, visitor}.
export function describe(e, names) {
  const who = e.process || e.instance || "";
  const holder = (id) => (id ? (names.get(id) || short(id)) : "nobody");
  const r = (text, more = {}) => ({ text, ...more });
  switch (e.kind) {
    case "process":
      switch (e.what) {
        case "started": return r(e.t === 0 ? `Process ${who} starts.` : `Process ${who} starts on a copy of the cache that the crashed process left. Its monotonic clock starts at 0.`);
        case "opened": return r(`${who} opens its database, as holder ${short(e.holder)}.`, { routine: e.t === 0 });
        case "open_failed": return r(`${who} cannot open its database: ${e.error}.`);
        case "stopped": return r(`The storage core of ${who} has stopped: ${e.error}. The process closes the database and opens it again.`);
        case "closed": return r(e.error ? `${who} closed its database, with an error: ${e.error}.` : `${who} has closed its database.`);
        case "crashed": return r(`Process ${who} crashes.`, { loud: false });
      }
      break;
    case "clock": {
      const what = {
        shiftWall: `The wall clock of ${who} shifts ${e.by < 0 ? "back" : "forward"} by ${span(e.by || 0)}, to ${clock(e.wall)}.`,
        stopWall: `The wall clock of ${who} stops at ${clock(e.wall)}.`,
        startWall: `The wall clock of ${who} goes on from ${clock(e.wall)}.`,
        stopMonotonic: `The monotonic clock of ${who} stops at ${clock(e.monotonic)}, and with it every sleep and timeout of the process.`,
        startMonotonic: `The monotonic clock of ${who} goes on from ${clock(e.monotonic)}.`,
      }[e.what];
      return r(what || `A clock of ${who} changes.`);
    }
    case "core": {
      const a = e.attrs || {};
      const err = a.error ? `: ${a.error}` : "";
      switch (e.event) {
        case "lease":
          switch (a["lease.transition"]) {
            case "taken":
              return a.outcome === "taken"
                ? r(a["lease.previous_holder"] ? `${who} takes the lease over from ${holder(a["lease.previous_holder"])}${atHead(a["commit.name"])}.` : `${who} takes the lease${atHead(a["commit.name"])}.`)
                : r(`${who} tried to take the lease and failed${err}.`, { routine: true });
            case "renewed":
              if (a.outcome === "renewed") return r(`${who} renews the lease.`, { routine: true });
              if (a.outcome === "unknown") return r(`${who} sent a renewal of the lease and got no answer, so it does not know whether it went through${err}.`);
              if (a.outcome === "conflict") return r(`S3 refused the renewal of ${who}, and the manifest is unchanged.`);
              return r(`${who} failed to renew the lease${err}.`);
            case "lost": return r(`${who} learns that it lost the lease to ${holder(a["lease.holder"])}.`);
            case "released": return r(a.outcome === "released" ? `${who} releases the lease.` : `${who} failed to release the lease${err}.`);
          }
          break;
        case "commit":
          switch (a.outcome) {
            case "committed": return r(`${who} commits ${short(a["commit.name"])}.`);
            case "empty": return r(`${who} has nothing to commit.`, { routine: true });
            case "committed_and_stopped": return r(`${who} commits ${short(a["commit.name"])}, and then its cache fails, so the core stops.`);
            case "unknown": return r(`${who} does not know whether its commit ${short(a["commit.name"])} went through${err}. It refuses new transactions until it knows.`);
            case "not_committed": return r(`The commit of ${who} failed, and nothing is committed${err}.`);
          }
          break;
        case "catch_up":
          if (a.outcome === "caught_up") return r(`${who} catches up from ${short(a["catch_up.from"])} to ${short(a["catch_up.to"])}, with ${a["catch_up.commits"] || 0} commit object${a["catch_up.commits"] === 1 ? "" : "s"}.`);
          return r(`The catch-up of ${who} from ${short(a["catch_up.from"])} to ${short(a["catch_up.to"])} failed after ${a["catch_up.commits"] || 0} commit object${a["catch_up.commits"] === 1 ? "" : "s"}${err}.`);
        case "refusal":
          return r(`${who} refuses a ${a.operation}: ${REASONS[a.reason] || a.reason}.`, { routine: true });
        case "unknown_outcome": {
          const kind = a["unknown.kind"] === "commit" ? `its commit ${short(a["commit.name"])}` : "its renewal";
          const out = { committed: "it is committed", not_committed: "it is not committed", renewed: "the lease is renewed", lease_lost: "the lease is lost" }[a.outcome] || a.outcome;
          return r(`${who} settles the unknown outcome of ${kind}: ${out}.`);
        }
        case "busy": return r(`The cache of ${who} was busy during a ${a.operation}.`);
        case "stop": return r(a.outcome === "closed" ? `The storage core of ${who} stops for the close.` : `The storage core of ${who} stops for good: ${a.outcome}${err}.`);
      }
      return r(`${who}: ${e.event} ${a.outcome || ""}.`, { routine: e.level === "DEBUG" });
    }
    case "call": {
      const visitor = e.caller === "visitor";
      const thing = e.op === "write" ? `write of key ${e.value}` : "read";
      const call = (visitor ? "Your " : "A ") + thing + " on " + who;
      const routine = !visitor;
      switch (e.what) {
        case "started": return r(`${call} starts.`, { routine, visitor });
        case "committed": return r(`${call} is committed.`, { routine, visitor });
        case "unknown": return r(`${call} ends with an unknown outcome.`, { visitor });
        case "not_committed": return r(`${call} is not committed.`, { visitor });
        case "abandoned": return r(`${call} failed before its commit and was rolled back.`, { routine, visitor });
        case "served": return r(`${call} is served: ${keysText(e.keys)}, at ${short(e.position)}.`, { routine, visitor });
        case "refused": return r(`${call} is refused: ${e.error}.`, { routine, visitor });
        case "stopped": return r(`${call} ends, because the database is closed or stopped.`, { routine, visitor });
        case "busy": return r(`${call} found the cache busy.`, { routine, visitor });
        case "timed_out": return r(`${call} timed out before it started.`, { routine, visitor });
        case "failed": return r(`${call} failed: ${e.error}.`, { routine, visitor });
      }
      break;
    }
    case "request": {
      const name = e.request ? `request ${e.request} of ${who}` : `a request of ${who}`;
      const req = `${name} (${requestWhat(e)})`;
      const Req = req[0].toUpperCase() + req.slice(1);
      const routine = true;
      switch (e.what) {
        case "sent": return r(`${who} sends request ${e.request}: ${requestWhat(e)}.`, { routine });
        case "slow": return r(`${Req} is slow on its way ${e.way}, by ${span(e.by)}.`);
        case "never_applied": return r(`${Req} reaches S3 too late, and is never applied.`);
        case "held": return r(`${Req} arrives after its caller gave up. S3 applies it ${span(e.by)} later.`);
        case "lost": return r(`${Req} is lost on its way to S3.`);
        case "stuck": return r(`${Req} is stuck in the network for ${span(e.by)}.`);
        case "arrived": return r(`${Req} arrives at S3 after being stuck.`);
        case "applied": return r(`S3 applies ${req}.`, { routine });
        case "refused": return r(`S3 refuses ${req}: ${e.error}.`, { routine: e.purpose === "take" });
        case "read": return r(`S3 reads ${e.key} for ${name}.`, { routine });
        case "read_broken": return r(`The content that S3 sends for ${req} breaks off.`);
        case "not_found": return r(`S3 has nothing at ${e.key} for ${name}.`, { routine });
        case "answered": return r(`The answer to ${name} arrives.`, { routine });
        case "no_caller": return r(`The answer to ${name} arrives, and nobody waits for it any more.`, { routine });
        case "response_lost": return r(`The answer to ${req} is lost on its way back.`);
        case "gave_up": return r(`${who} gives up waiting for ${name}.`, { routine });
        case "no_answer": return r(`No answer to ${req} comes, and ${who} waits until it gives up.`);
        case "returned": return r(`${name[0].toUpperCase() + name.slice(1)} returns to the storage core${e.error ? ", with an error" : ""}.`, { routine });
        case "damaged": return r(`The download for ${name} arrives with one byte changed.`);
        case "answer_lost": return r(`Your armed fault takes the answer to ${req}.`, { visitor: true });
        case "failed": return r(`Your armed fault fails a ${requestWhat(e)} of ${who} before it is sent.`, { visitor: true });
        case "too_large": return r(`${Req} is too large for S3, and is not sent.`);
      }
      break;
    }
    case "store":
      switch (e.what) {
        case "cut_off": return r(`${who} is cut off from S3 for ${span(e.for)}.`);
        case "outage": return r(`S3 is unreachable for every process for ${span(e.for)}.`);
        case "calm": return r("S3 makes no new random faults from now on.");
      }
      return r(e.text || "S3 does something the page does not know.");
    case "action":
      return r(e.what === "done" ? `You: ${actionWords(e)}.` : `You: ${actionWords(e)}. Ignored: ${e.text}.`, { visitor: true });
    case "check":
      return r(`Invariant ${e.invariant} is violated${e.excused ? ", and the design excuses it" : ""}: ${e.text}.`, { loud: !e.excused });
    case "buggify":
      return r(`Buggify fires: ${e.site.replace(/^core: /, "")}.`);
  }
  return r(`${e.kind} ${e.what || ""}`.trim() + ".", { routine: true });
}

// INVARIANTS that the engine checks, with when.
export const INVARIANTS = [
  { key: "chain", text: "The manifest moves its head only to a commit object that is in S3 and whose parent is the head before.", when: "At every write of the manifest" },
  { key: "acknowledged", text: "A commit that was acknowledged is in the log.", when: "At each acknowledgement, and at the end" },
  { key: "not_committed", text: "A write that ended as not committed is not in the log.", when: "At the end" },
  { key: "once", text: "No key is in the log twice.", when: "At the end" },
  { key: "state", text: "A read saw a state that the log had.", when: "At every read" },
  { key: "fresh", text: "A read saw every write that was acknowledged before it started. The design excuses a miss on a process whose two clocks both went wrong.", when: "At every read, and at the end" },
];
