// The words of the simulator page: the seed of a session, the times, and the log's line for each
// event of the engine. Nothing here touches the page, so the module can be read and checked on its own.
//
// The events are those of internal/simulator/README.md, which is the reference.

export const EMPTY_LOG = "0".repeat(64);

// newSeed picks the seed of a new session, below 2^53, so that it goes through the engine's module as
// a JavaScript number without loss. Only the seed uses the browser's randomness. The session never does.
export function newSeed() {
  const [hi, lo] = crypto.getRandomValues(new Uint32Array(2));
  return (hi % 2 ** 21) * 2 ** 32 + lo;
}

// clock of a fake time in milliseconds, to a tenth of a second: 0:41.2, or 1:02:03.4 after an hour.
export function clock(ms) {
  const sign = ms < 0 ? "−" : "";
  const tenths = Math.floor(Math.abs(ms) / 100);
  const h = Math.floor(tenths / 36000);
  const m = Math.floor(tenths / 600) % 60;
  const s = ((tenths % 600) / 10).toFixed(1).padStart(4, "0");
  return sign + (h ? h + ":" + String(m).padStart(2, "0") : m) + ":" + s;
}

// offset of a clock from the true time, in whole seconds, with its sign: "+60 s", "−45 s".
export function offset(ms) {
  const s = Math.round(ms / 1000);
  return (s < 0 ? "−" : "+") + Math.abs(s) + " s";
}

export const short = (name) => (name === EMPTY_LOG ? "empty" : name ? name.slice(0, 8) : "?");

export function keysText(keys) {
  if (!keys || keys.length === 0) return "no rows";
  if (keys.length <= 3) return (keys.length === 1 ? "key " : "keys ") + keys.join(", ");
  return keys.length + " rows";
}

// REASONS of the core for refusing a call, in words.
export const REASONS = {
  not_holder: "no lease",
  outcome_unknown: "last commit unknown",
  not_caught_up: "cache behind",
  lease_expired_monotonic: "lease ran out by its monotonic clock",
  lease_expired_wall: "lease ran out by its wall clock",
};

// reason of a refused call, from its error, which ends with the reason of the core.
export const reason = (e) => String(e.error || "").split(": ").pop();

// reasonWords of a reason of the core, in words, also for a reason that the page does not know.
export const reasonWords = (r) => REASONS[r] || String(r || "").replaceAll("_", " ") || "refused";

// The client stands outside the instances, so its words hold only what it can see: that it got no
// answer, or the error that the instance answered with. Why an instance did not answer, such as a
// crash or a stop, is for the cards of the instances to say. A call that did not reach the process,
// "down", and one that was on it when it crashed, "lost", look the same to the client: for a write,
// it does not know whether the write happened.

// CAUSES of an instance that answers that it has stopped for good, from the cause in its error, in
// words. The cause comes first after the error of the stop, and wraps the errors below it.
const STOP = "the instance has stopped: ";
const CAUSES = [
  ["closed", "database closed"],
  ["local I/O failure", "local failure"],
  ["the position of the cache is not in the log", "cache not in the log"],
  ["the log is broken", "log broken"],
];

// errorWords of a call of the client that the instance answered with an error, in a few words.
function errorWords(e) {
  const text = String(e.error || "");
  switch (e.what) {
    case "refused": return reasonWords(reason(e));
    case "unknown": return "write outcome unknown";
    case "not_committed": return "write not committed";
    case "abandoned": return "write rolled back";
    case "busy": return "cache busy";
    case "timed_out": return "timed out";
    case "stopped": {
      const cause = text.includes(STOP) ? text.slice(text.indexOf(STOP) + STOP.length) : "";
      const found = CAUSES.find(([t]) => cause.startsWith(t));
      return found ? found[1] : "out of service";
    }
  }
  if (text.includes("no database open")) return "no database open";
  if (text.includes("local I/O failure")) return "local failure";
  return "call failed";
}

// clientFailure of a call of the client that ended without being served, from its event: kind, which
// stays the same over a run of failures of one kind, words for the last failure, and line for the
// log. A call that was served or committed has none. A failure without a kind takes a line each: a
// write that the instance answers with an unknown outcome or as not committed, so that the log
// names its key.
export function clientFailure(e) {
  const i = e.instance;
  switch (e.what) {
    case "committed":
    case "served":
      return null;
    case "down":
    case "lost":
      return e.op === "write"
        ? { kind: "no answer " + i, words: `no answer from ${i}, write outcome unknown`, line: `The client gets no answer from ${i} to the write of key ${e.value}: outcome unknown.` }
        : { kind: "no answer " + i, words: `no answer from ${i}`, line: `The client gets no answer from ${i}.` };
  }
  const words = errorWords(e);
  const keyed = e.what === "unknown" || e.what === "not_committed";
  const line = e.what === "unknown" ? `write of key ${e.value}, outcome unknown` : e.what === "not_committed" ? `write of key ${e.value} not committed` : words;
  return { kind: keyed ? undefined : `error ${i} ${e.what} ${words}`, words: `error from ${i}: ${words}`, line: `The client gets an error from ${i}: ${line}.` };
}

const SWAPS = { take: "take of the lease", commit: "commit", renew: "renewal", release: "release" };

// actionWords of an action event of a session, as the visitor's own words.
function actionWords(e) {
  const i = e.instance;
  switch (e.do) {
    case "crash": return `You crash ${i}.`;
    case "restart": return `You restart ${i}.`;
    case "close": return `You stop ${i}.`;
    case "open": return `You start ${i}.`;
    case "hangNetwork": return e.on ? `You hang the network of ${i}.` : `The network of ${i} no longer hangs.`;
    case "disconnectNetwork": return e.on ? `You disconnect ${i}.` : `You connect ${i} again.`;
    case "failDisk": return e.on ? `You make the disk of ${i} fail.` : `The disk of ${i} works again.`;
    case "shiftWall": return `You move the wall clock of ${i} ${e.by > 0 ? "forward" : "back"} ${Math.abs(e.by) / 1000} s.`;
    case "pauseClocks": return `You pause the clocks of ${i}.`;
    case "client": return `You point the client at ${i}.`;
    case "resumeClocks": return `You resume the clocks of ${i}.`;
  }
  return `You: ${e.do}.`;
}

// The tiers of the log: "stream" is the reads and writes that the instance with the lease serves,
// "story" is what happens to the lease, the instances, and S3, "you" is an action of the visitor, and
// "alarm" is a violation that the engine's checker found and the design does not excuse. An event
// without a tier is not in the log.

// teller gives a function that takes the events of a session one at a time, in order, and returns the
// line of each: {text, tier}, with tier null for an event that is not in the log. It keeps what it
// needs from the events before, such as the keys of each commit object.
//
// A refusal of a call is in the log only when it starts a spell of refusals of its instance, or has
// another reason than the one before it, so that the instance without the lease, which refuses every
// call of its application, takes one line. A spell ends when the instance serves a call. Other lines
// that repeat while an action lasts are kept to one line in the same way. A failure of a call of the
// client is in the log when it begins a run of failures of its kind, which a call that is served, or
// a switch of the client, ends.
export function teller() {
  const names = new Map(); // holder IDs, by the process that opened them
  const keys = new Map(); // the keys of each commit object, by its name
  const opened = new Set(); // processes that have opened their database once
  const spells = new Map(); // the kinds of line that each instance has said in its spells, by group
  const holder = (id) => (id ? names.get(id) || short(id) : "nobody");
  let clientRun = null; // the kind of the run of failures of the client, or null
  return (e) => {
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
            if (e.process !== e.instance) say(`${who} starts on the cache that the crashed process left.`);
            break;
          case "opened":
            calm("process");
            names.set(e.holder, e.process);
            if (opened.has(e.process)) say(`${who} opens its database.`);
            opened.add(e.process);
            break;
          case "open_failed": once("process:open_failed", `${who} cannot open its database.`); break;
          case "stopped": say(`The core of ${who} has stopped.`); break;
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
                else if (a.outcome === "unknown") once("lease:unknown", `${who} gets no answer to its renewal of the lease.`);
                else if (a.outcome === "conflict") say(`S3 refuses the renewal of ${who}.`);
                else once("lease:failed", `${who} fails to renew the lease.`);
                break;
              case "lost": calm("lease"); say(`${who} learns that ${holder(a["lease.holder"])} holds the lease.`); break;
              case "released": if (a.outcome === "released") say(`${who} releases the lease.`); break;
            }
            break;
          case "commit": {
            const name = a["commit.name"];
            switch (a.outcome) {
              case "committed": say(`${who} commits ${keysText(keys.get(name))} as ${short(name)}.`, "stream"); break;
              case "committed_and_stopped": say(`${who} commits ${short(name)}, then its cache fails and the core stops.`); break;
              case "unknown": say(`${who} does not know whether its commit ${short(name)} went through.`); break;
            }
            break;
          }
          case "catch_up": {
            const n = a["catch_up.commits"] || 0;
            if (a.outcome === "caught_up") {
              calm("catch");
              say(`${who} catches up: ${n} commit object${n === 1 ? "" : "s"} from S3.`);
            } else once("catch:failed", `The catch-up of ${who} fails.`);
            break;
          }
          case "unknown_outcome": {
            const kind = a["unknown.kind"] === "commit" ? `commit ${short(a["commit.name"])}` : "renewal";
            const result = { committed: "committed", not_committed: "not committed", renewed: "renewed", lease_lost: "the lease is lost" }[a.outcome] || a.outcome;
            calm("lease");
            say(`${who} asks S3 about its ${kind}: ${result}.`);
            break;
          }
          case "busy": once("core:busy", `The cache of ${who} is busy.`); break;
          case "stop": if (a.outcome !== "closed") say(`The core of ${who} stops: ${String(a.outcome).replaceAll("_", " ")}.`); break;
        }
        break;
      }
      case "call":
        if (e.caller === "client" && e.what !== "started") {
          const f = clientFailure(e);
          if (!f) clientRun = null;
          else if (!f.kind || f.kind !== clientRun) {
            clientRun = f.kind || null;
            say(f.line);
            break;
          } else break;
        }
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
            const r = reason(e);
            once("call:refused:" + r, `${who} refuses calls: ${reasonWords(r)}.`);
            break;
          }
          case "unknown": say(`The write of key ${e.value} on ${who} ends without an answer.`); break;
          case "not_committed": say(`The write of key ${e.value} on ${who} is not committed.`); break;
          case "abandoned": once("call:abandoned", `A write on ${who} is rolled back before its commit.`); break;
          case "stopped": once("call:stopped", `${who} ends its calls: its database is not open.`); break;
          case "busy": once("call:busy", `A call on ${who} finds the cache busy.`); break;
          case "timed_out": once("call:timed_out", `Calls on ${who} time out.`); break;
          case "failed": once("call:failed", `A call on ${who} fails.`); break;
        }
        break;
      case "request":
        if (e.what === "applied" && e.object && e.object.commit) keys.set(e.object.commit.name, e.object.commit.keys);
        else if (e.what === "refused" && e.purpose !== "take" && SWAPS[e.purpose]) say(`S3 refuses the ${SWAPS[e.purpose]} of ${who}: the manifest has another version.`);
        break;
      case "store":
        if (!e.what) say(e.text || "S3 does something the page does not know.");
        break;
      case "action":
        if (e.do === "client" && e.what === "done") clientRun = null;
        if (e.what === "done") say(actionWords(e), "you");
        else say(`${actionWords(e)} ${e.what === "failed" ? "It fails" : "Ignored"}: ${e.text}.`, "you");
        break;
      case "check":
        // The design accepts a stale read on a process whose clocks stood still, and the checker
        // excuses it: the log says so once a spell, in the words of the story.
        if (e.excused && e.invariant === "fresh") once("lease:stale", `${who} serves a stale read after its clocks stood still, which the spec allows.`);
        else say(`The checker finds a violation of ${e.invariant}: ${e.text}.`, "alarm");
        break;
      case "buggify":
        say(`Buggify fires: ${e.site}.`);
        break;
    }
    return w;
  };
}
