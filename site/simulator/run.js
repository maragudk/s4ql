// The run of the simulator page as data: the request that the engine takes for it, the schedule of
// the visitor's faults, and the words for what happens. Nothing here touches the page, so the module
// can be read and checked on its own.
//
// The request and the events are those of internal/simulator/README.md, which is the reference.

export const EMPTY_LOG = "0".repeat(64);

// A run is as long as the page has asked for. The page asks for CHUNK more before the playback comes
// within LEAD of the end, and stops at LIMIT, past which a run of the engine takes long enough that
// a visitor would notice the wait when adding a fault.
export const CHUNK = 120000;
export const LEAD = 60000;
export const LIMIT = 600000;

// The faults that the page offers, with how long a cut-off or an outage lasts: longer than a lease
// of 30 s, so that the lease runs out while it lasts.
export const SPAN = 40000;

// newSeed picks the seed of a new run, below 2^53, so that it goes through the engine's module as a
// JavaScript number without loss. Only the seed uses the browser's randomness. The run never does.
export function newSeed() {
  const [hi, lo] = crypto.getRandomValues(new Uint32Array(2));
  return (hi % 2 ** 21) * 2 ** 32 + lo;
}

// request of the engine for the run: two instances under the default workload, with no random
// faults, so that every fault is one that the visitor added.
export function request(run) {
  return {
    seed: run.seed,
    end: run.end,
    instances: 2,
    schedule: run.schedule.map((a) => ({ ...a })),
  };
}

export const instanceName = (i) => String.fromCharCode(97 + i);

// insert the action into the schedule after every action at its time or before it, so that the
// actions before its time keep their indexes, and the run before that time stays the same.
export function insert(schedule, action) {
  let i = 0;
  while (i < schedule.length && schedule[i].at <= action.at) i++;
  return [...schedule.slice(0, i), action, ...schedule.slice(i)];
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

export function keysText(keys) {
  if (!keys || keys.length === 0) return "no rows";
  if (keys.length <= 5) return (keys.length === 1 ? "key " : "keys ") + keys.join(", ");
  return keys.length + " rows, keys " + keys.slice(0, 2).join(", ") + " … " + keys.slice(-2).join(", ");
}

// actionWords of an action of the schedule, as the visitor's own words.
export function actionWords(a) {
  switch (a.do) {
    case "crash": return `You crash ${a.instance}.`;
    case "restart": return `You restart ${a.instance}.`;
    case "cutOff": return `You cut ${a.instance} off from S3 for ${span(a.for)}.`;
    case "stopMonotonic": return `You stop the monotonic clock of ${a.instance}, and with it every timer of the process.`;
    case "startMonotonic": return `You start the monotonic clock of ${a.instance} again.`;
    case "outage": return `You make S3 unreachable for ${span(a.for)}.`;
    case "loseAnswer": return "You arm a fault: the answer to the next commit is lost on its way back.";
  }
  return `You: ${a.do}.`;
}

// REASONS of the core for refusing a call, in words.
export const REASONS = {
  not_holder: "it does not hold the lease",
  outcome_unknown: "it does not know whether its last commit went through",
  not_caught_up: "its cache is behind the log",
  lease_expired_monotonic: "its lease ran out by its monotonic clock",
  lease_expired_wall: "its lease ran out by its wall clock",
};

const SWAPS = { take: "take of the lease", commit: "commit", renew: "renewal of the lease", release: "release of the lease" };

// The tiers of the log: "stream" is the reads and writes that the instance with the lease serves,
// "story" is what happens to the lease, the instances, and S3, "you" is a fault that the visitor
// added, and "alarm" is a violation that the engine's checker found. An event without a tier is not
// in the log.

// story gives the words of each event of the run, in order: an array of {text, tier}, with tier
// null for an event that is not in the log. It reads the events from the first on, so the words of
// the events before a time stay the same when a run changes only after that time. schedule is that
// of the request, which the events of the kind action point into.
//
// A refusal of a call is in the log only when it starts a spell of refusals of its instance, or has
// another reason than the one before it, so that the instance without the lease, which refuses every
// call of its application, takes one line. A spell ends when the instance serves a call. Other lines
// that repeat while a fault lasts are kept to one line in the same way.
export function story(events, schedule) {
  const names = new Map(); // holder IDs, by the process that opened them
  const keys = new Map(); // the keys of each commit object, by its name
  const opened = new Set(); // processes that have opened their database once
  const spells = new Map(); // the kinds of line that each instance has said in its spells, by group
  const holder = (id) => (id ? names.get(id) || short(id) : "nobody");
  const out = [];
  for (const e of events) {
    const who = e.process || e.instance || "";
    let w = { text: "", tier: null };
    const say = (text, tier = "story") => (w = { text, tier });
    // once says the line only if the instance has not said the same kind of line in the spell of its
    // group; calm ends the spell of a group, or of every group.
    const inst = e.instance || "";
    if (!spells.has(inst)) spells.set(inst, new Set());
    const said = spells.get(inst);
    const once = (kind, text) => {
      if (said.has(kind)) return;
      said.add(kind);
      say(text);
    };
    const calm = (group) => {
      for (const k of said) if (!group || k.startsWith(group + ":")) said.delete(k);
    };
    switch (e.kind) {
      case "process":
        switch (e.what) {
          case "started":
            calm();
            if (e.process !== e.instance) say(`Process ${who} starts, on a copy of the cache that the crashed process left.`);
            break;
          case "opened":
            names.set(e.holder, e.process);
            if (opened.has(e.process)) say(`${who} opens its database again.`);
            opened.add(e.process);
            break;
          case "open_failed": say(`${who} cannot open its database: ${e.error}.`); break;
          case "stopped": say(`The storage core of ${who} has stopped: ${e.error}. The process opens the database again.`); break;
          case "closed": say(`${who} has closed its database.`); break;
          // A crash is in the log as the visitor's action.
        }
        break;
      case "core": {
        const a = e.attrs || {};
        switch (e.event) {
          case "lease":
            switch (a["lease.transition"]) {
              case "taken":
                if (a.outcome === "taken") {
                  calm("lease");
                  say(a["lease.previous_holder"] ? `${who} takes the lease over from ${holder(a["lease.previous_holder"])}.` : `${who} takes the lease.`);
                }
                break;
              case "renewed":
                if (a.outcome === "renewed") calm("lease");
                else if (a.outcome === "unknown") once("lease:unknown", `${who} gets no answer to its renewal of the lease, so it does not know whether the renewal went through.`);
                else if (a.outcome === "conflict") say(`S3 refuses the renewal of ${who}, and the manifest is unchanged.`);
                else once("lease:failed", `${who} fails to renew the lease.`);
                break;
              case "lost": calm("lease"); say(`${who} learns that ${holder(a["lease.holder"])} holds the lease now.`); break;
              case "released": if (a.outcome === "released") say(`${who} releases the lease.`); break;
            }
            break;
          case "commit": {
            const name = a["commit.name"];
            switch (a.outcome) {
              case "committed": say(`${who} commits ${keysText(keys.get(name))}, as ${short(name)}.`, "stream"); break;
              case "committed_and_stopped": say(`${who} commits ${short(name)}, and then its cache fails, so the core stops.`); break;
              case "unknown": say(`${who} does not know whether its commit ${short(name)} went through.`); break;
            }
            break;
          }
          case "catch_up": {
            const n = a["catch_up.commits"] || 0;
            if (a.outcome === "caught_up") {
              calm("catch");
              say(`${who} catches up with the log: it applies ${n} commit object${n === 1 ? "" : "s"} from S3 to its cache.`);
            } else once("catch:failed", `The catch-up of ${who} fails after ${n} commit object${n === 1 ? "" : "s"}.`);
            break;
          }
          case "unknown_outcome": {
            const kind = a["unknown.kind"] === "commit" ? `its commit ${short(a["commit.name"])}` : "its renewal";
            const result = { committed: "it is committed", not_committed: "it is not committed", renewed: "the lease is renewed", lease_lost: "the lease is lost" }[a.outcome] || a.outcome;
            calm("lease");
            say(`${who} asks S3 and learns the outcome of ${kind}: ${result}.`);
            break;
          }
          case "busy": once("core:busy", `The cache of ${who} is busy.`); break;
          case "stop": if (a.outcome !== "closed") say(`The storage core of ${who} stops: ${a.outcome}.`); break;
        }
        break;
      }
      case "call":
        switch (e.what) {
          case "committed": // in the log as the commit of the core
            calm("call");
            calm("core");
            break;
          case "served":
            calm("call");
            calm("core");
            say(`${who} serves a read: ${keysText(e.keys)}.`, "stream");
            break;
          case "refused": {
            const reason = String(e.error || "").split(": ").pop();
            once("call:refused:" + reason, `${who} refuses calls: ${REASONS[reason] || reason}.`);
            break;
          }
          case "unknown": say(`The write of key ${e.value} on ${who} ends without an answer: it may or may not be committed.`); break;
          case "not_committed": say(`The write of key ${e.value} on ${who} is not committed.`); break;
          case "abandoned": once("call:abandoned", `A write on ${who} fails before its commit and is rolled back.`); break;
          case "stopped": once("call:stopped", `${who} ends its calls: its database is closed or stopped.`); break;
          case "busy": once("call:busy", `A call on ${who} finds the cache busy.`); break;
          case "timed_out": once("call:timed_out", `Calls on ${who} time out before they start.`); break;
          case "failed": once("call:failed", `A call on ${who} fails: ${e.error}.`); break;
        }
        break;
      case "request":
        if (e.what === "applied" && e.object && e.object.commit) keys.set(e.object.commit.name, e.object.commit.keys);
        else if (e.what === "answer_lost") say(`S3 applies the ${SWAPS[e.purpose] || "request"} of ${who}, and the answer is lost on its way back. ${who} cannot tell whether it went through.`);
        else if (e.what === "refused" && e.purpose !== "take" && SWAPS[e.purpose]) say(`S3 refuses the ${SWAPS[e.purpose]} of ${who}: the manifest has another version.`);
        break;
      case "store":
        // A cut-off and an outage are in the log as the visitor's action.
        if (!e.what) say(e.text || "S3 does something the page does not know.");
        break;
      case "action": {
        const a = schedule[e.action] || { do: e.do, instance: e.instance };
        if (e.what === "done") say(actionWords(a), "you");
        else say(`${actionWords(a)} It is ${e.what}: ${e.text}.`, "you");
        break;
      }
      case "check":
        say(`The checker finds a violation of ${e.invariant}${e.excused ? ", which the design excuses" : ""}: ${e.text}.`, "alarm");
        break;
      case "buggify":
        say(`Buggify fires: ${e.site}.`);
        break;
    }
    out.push(w);
  }
  return out;
}
