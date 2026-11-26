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

// instanceOf a process, which the page shows instead of the process: to a visitor, the process that
// a restart starts is still the instance, restarted. The engine names the first process of an
// instance by the instance, and the one that its nth restart starts by the instance and n+1, such as
// a2.
export const instanceOf = (process) => String(process || "").replace(/[0-9]+$/, "");

export function keysText(keys) {
  if (!keys || keys.length === 0) return "no rows";
  if (keys.length <= 3) return (keys.length === 1 ? "key " : "keys ") + keys.join(", ");
  return keys.length + " rows";
}

// commitKeysText of a commit object, for its line in the list: the keys of the rows that it inserts,
// or of those that it deletes, or, for one of the box, the number of its changes.
export function commitKeysText(c) {
  const keys = (c && c.keys) || [], deletes = (c && c.deletes) || [];
  if (keys.length === 0 && deletes.length > 0) return "delete " + (deletes.length <= 3 ? deletes.join(", ") : deletes.length + " rows");
  // A commit object of the box changes other tables than the app's.
  if (keys.length === 0 && c && c.changes) return c.changes === 1 ? "1 change" : `${c.changes} changes`;
  return keysText(keys);
}

// rowsText of the keys that a read gave: how many rows it found. A read of a row whose delete the
// application knows was committed finds none, as it should, and says so.
const rowsText = (e) => {
  const n = (e.keys || []).length;
  return (n === 1 ? "1 row" : `${n} rows`) + (n === 0 && e.expect === "none" ? ", as deleted" : "");
};

// literal of an argument of a statement in SQL: a number as itself, text in single quotes, and null.
const literal = (v) => (v === null || v === undefined ? "null" : typeof v === "string" ? `'${v.replaceAll("'", "''")}'` : String(v));

// sqlText of a statement with its arguments in place of its parameters, in order, as the log shows it.
// It is for the statements of the engine's workload, which hold no ? but their parameters, in no
// string literal, and take only integers.
export function sqlText(sql, args) {
  const a = args || [];
  let i = 0;
  return String(sql || "").replace(/\?/g, () => (i < a.length ? literal(a[i++]) : "?"));
}

// callText of the event of a call: its statement, with its arguments.
export const callText = (e) => sqlText(e.sql, e.args);

// statementsText of a commit object: the statements of its annotations, with their arguments.
export const statementsText = (c) => ((c && c.statements) || []).map((s) => sqlText(s.sql, s.args)).join("; ");

// BOX_TEXT is the most characters of the statements that a line of the log shows.
const BOX_TEXT = 200;

// boxText of a statement, for a line of the log: on one line, and cut with an ellipsis
// after BOX_TEXT characters.
export function boxText(sql) {
  const one = [...String(sql || "").replace(/\s+/g, " ").trim()];
  return one.length > BOX_TEXT ? one.slice(0, BOX_TEXT).join("") + "…" : one.join("");
}

// boxLine of the event that ends a call of the box, or null for one that the log tells otherwise: a
// commit, which the core's event of the commit tells with its statement.
export function boxLine(e) {
  const i = e.instance, sql = boxText(e.sql);
  const n = e.count || 0;
  switch (e.what) {
    case "served": return { text: `${i} serves ${sql}: ${n === 1 ? "1 row" : `${n.toLocaleString("en")} rows`}.`, tier: "stream" };
    case "committed": return e.commit ? null : { text: `${i} runs ${sql}: nothing to commit.`, tier: "stream" };
    case "down":
    case "lost": return { text: `Your statement gets no answer from ${i}${e.what === "lost" ? ", outcome unknown" : ""}.`, tier: "story" };
    case "unknown": return { text: `On ${i}, ${sql} ends without an answer.`, tier: "story" };
    case "not_committed": return { text: `On ${i}, ${sql} is not committed: ${e.error}.`, tier: "story" };
    case "abandoned": return { text: `On ${i}, ${sql} is rolled back: ${e.error}.`, tier: "story" };
  }
  return { text: `On ${i}, ${sql} fails: ${e.error || e.what}.`, tier: "story" };
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

// The requests are those that reach the app, which embeds s4ql in each instance, such as requests
// over HTTP from browsers. The engine's client stands for them: each of its calls is a request, which
// a task of the instance's process handles, so it ends with the process if the app crashes. The words
// of the requests hold only what is seen from outside the app: that a request got no answer, or the
// error that the instance answered with. Why an instance did not answer, such as a crash or a stop, is
// for the cards of the instances to say. A request that did not reach the process, "down", and one
// that was on it when it crashed, "lost", look the same from outside: for a write, nobody outside
// knows whether the write happened.

// CAUSES of an instance that answers that it has stopped for good, from the cause in its error, in
// words. The cause comes first after the error of the stop, and wraps the errors below it.
const STOP = "the instance has stopped: ";
const CAUSES = [
  ["closed", "database closed"],
  ["local I/O failure", "local failure"],
  ["the position of the cache is not in the log", "cache not in the log"],
  ["the log is broken", "log broken"],
];

// errorWords of a request that the instance answered with an error, in a few words.
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

// requestFailure of a request that ended without being served, from the event of the engine's call
// of the client: kind, which stays the same over a run of failures of one kind, words for the status
// of the requests, and line for the log. A request that was served or committed has none. A failure
// without a kind takes a line each: a write that the instance answers with an unknown outcome or as
// not committed, so that the log names its key.
export function requestFailure(e) {
  const i = e.instance;
  switch (e.what) {
    case "committed":
    case "served":
      return null;
    case "down":
    case "lost":
      return e.op !== "read"
        ? { kind: "no answer " + i, words: `no answer from ${i}, write outcome unknown`, line: `A request gets no answer from ${i}: ${callText(e)}, outcome unknown.` }
        : { kind: "no answer " + i, words: `no answer from ${i}`, line: `A request gets no answer from ${i}.` };
  }
  const words = errorWords(e);
  const keyed = e.what === "unknown" || e.what === "not_committed";
  const line = e.what === "unknown" ? `${callText(e)}, outcome unknown` : e.what === "not_committed" ? `${callText(e)}, not committed` : words;
  return { kind: keyed ? undefined : `error ${i} ${e.what} ${words}`, words: `error from ${i}: ${words}`, line: `A request gets an error from ${i}: ${line}.` };
}

// NO_REQUESTS is the status of the requests before the first has ended.
export const NO_REQUESTS = "no requests yet";

// requestStatus of the requests after one ended, from the event of its end: whether it failed, and
// words for what it got. A request that was served or committed is ok, from the instance that
// answered; a failure has the words of requestFailure.
export function requestStatus(e) {
  const f = requestFailure(e);
  return f ? { failing: true, words: f.words } : { failing: false, words: `ok from ${e.instance}` };
}

const SWAPS = { take: "take of the lease", commit: "commit", renew: "renewal", release: "release" };

// resetWords of the visitor's reset of the clocks of the instance, which the page composes of actions
// of the engine.
export const resetWords = (i) => `You reset the clocks of ${i}.`;

// resetLine of the action event of a step of the visitor's reset of the clocks, in place of the line
// of the step: the first step that is done says the reset, the others none, and a step that fails or
// is ignored says so of the reset.
export function resetLine(e, first) {
  const words = resetWords(e.instance);
  if (e.what === "done") return first ? { text: words, tier: "you" } : { text: "", tier: null };
  return { text: `${words} ${e.what === "failed" ? "It fails" : "Ignored"}: ${e.text}.`, tier: "you" };
}

// actionWords of an action event of a session, as the visitor's own words.
function actionWords(e) {
  const i = e.instance;
  switch (e.do) {
    case "crash": return `You crash ${i}.`;
    case "restart": return `You restart ${i}.`;
    case "close": return `You stop ${i}.`;
    case "open": return `You start ${i}.`;
    case "hangNetwork": return e.on ? `You hang the connection of ${i} to S3.` : `The connection of ${i} to S3 no longer hangs.`;
    case "disconnectNetwork": return e.on ? `You disconnect ${i} from S3.` : `You connect ${i} to S3 again.`;
    case "failDisk": return e.on ? `You make the disk of ${i} fail.` : `The disk of ${i} works again.`;
    case "shiftWall": return `You move the wall clock of ${i} ${e.by > 0 ? "forward" : "back"} ${Math.abs(e.by) / 1000} s.`;
    case "pauseClocks": return `You pause the clocks of ${i}.`;
    case "resumeClocks": return `You start the clocks of ${i} again.`;
    case "client": return `You send the requests to ${i}.`;
    case "sql": return i ? `You send ${boxText(e.sql)} to ${i}.` : `You send ${boxText(e.sql)}.`;
  }
  return `You: ${e.do}.`;
}

// The tiers of the log: "stream" is the reads and writes that the instance with the lease serves,
// "story" is what happens to the lease, the instances, and S3, and "you" is an action of the visitor.
// An event without a tier is not in the log.

// teller gives a function that takes the events of a session one at a time, in order, and returns the
// line of each: {text, tier}, with tier null for an event that is not in the log. It keeps what it
// needs from the events before, such as the keys of each commit object.
//
// A refusal of a call is in the log only when it starts a spell of refusals of its instance, or has
// another reason than the one before it, so that the instance without the lease, which refuses every
// call of its application, takes one line. A spell ends when the instance serves a call. Other lines
// that repeat while an action lasts are kept to one line in the same way. A failure of a request is in
// the log when it begins a run of failures of its kind, which a request that is served, or sending the
// requests to the other instance, ends.
export function teller() {
  const names = new Map(); // the instances of holder IDs, by the process that opened them
  const current = new Map(); // the holder ID of each process's latest open
  const commits = new Map(); // each commit object that S3 took and the core has not logged, by its name
  const opened = new Set(); // processes that have opened their database once
  const spells = new Map(); // the kinds of line that each instance has said in its spells, by group
  const holder = (id) => (id ? names.get(id) || short(id) : "nobody");
  let failures = null; // the kind of the run of failures of the requests, or null
  return (e) => {
    const who = e.instance || instanceOf(e.process);
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
            if (e.process !== e.instance) say(`${who} starts again, on the cache that it left when it crashed.`);
            break;
          case "opened":
            calm("process");
            names.set(e.holder, who);
            current.set(e.process, e.holder);
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
                  // A process that takes the lease over from an earlier process of its instance
                  // takes it again.
                  const from = a["lease.previous_holder"] ? holder(a["lease.previous_holder"]) : "";
                  say(!from ? `${who} takes the lease.` : from === who ? `${who} takes the lease again.` : `${who} takes the lease over from ${from}.`);
                }
                break;
              case "renewed":
                if (a.outcome === "renewed") calm("lease");
                else if (a.outcome === "unknown") once("lease:unknown", `${who} gets no answer to its renewal of the lease.`);
                else if (a.outcome === "conflict") say(`S3 refuses the renewal of ${who}.`);
                else once("lease:failed", `${who} fails to renew the lease.`);
                break;
              case "lost": {
                calm("lease");
                // The manifest can name the process itself, with another salt or head, or an
                // earlier holder of its instance: a process before a restart, or before a Stop.
                const id = a["lease.holder"], by = holder(id);
                if (id && id === current.get(e.process)) say(`${who} learns that it lost the lease.`);
                else say(by === who ? `${who} learns that its lease from before has not run out yet.` : `${who} learns that ${by} holds the lease.`);
                break;
              }
              case "released": if (a.outcome === "released") say(`${who} releases the lease.`); break;
            }
            break;
          case "commit": {
            const name = a["commit.name"];
            const sql = boxText(statementsText(commits.get(name)));
            commits.delete(name);
            switch (a.outcome) {
              case "committed": say(sql ? `${who} commits ${sql} as ${short(name)}.` : `${who} commits ${short(name)}.`, "stream"); break;
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
        if (e.caller === "box") {
          const line = e.what === "started" ? null : boxLine(e);
          if (line) say(line.text, line.tier);
          if (e.what === "served" || e.what === "committed") {
            calm("call");
            calm("core");
          }
          break;
        }
        if (e.caller === "client" && e.what !== "started") {
          const f = requestFailure(e);
          if (!f) failures = null;
          else if (!f.kind || f.kind !== failures) {
            failures = f.kind || null;
            say(f.line);
            break;
          } else break;
        }
        switch (e.what) {
          case "committed": // in the log as the commit of the core, with its insert or delete
            calm("call");
            calm("core");
            break;
          case "served":
            calm("call");
            calm("core");
            say(`${who} serves ${callText(e)}: ${e.error || rowsText(e)}.`, "stream");
            break;
          case "refused": {
            const r = reason(e);
            once("call:refused:" + r, `${who} refuses calls: ${reasonWords(r)}.`);
            break;
          }
          case "unknown": say(`On ${who}, ${callText(e)} ends without an answer.`); break;
          case "not_committed": say(`On ${who}, ${callText(e)} is not committed.`); break;
          case "abandoned": once("call:abandoned", `A write on ${who} is rolled back before its commit.`); break;
          case "stopped": once("call:stopped", `${who} ends its calls: its database is not open.`); break;
          case "busy": once("call:busy", `A call on ${who} finds the cache busy.`); break;
          case "timed_out": once("call:timed_out", `Calls on ${who} time out.`); break;
          case "failed": once("call:failed", `A call on ${who} fails.`); break;
        }
        break;
      case "request":
        if (e.what === "applied" && e.object && e.object.commit) commits.set(e.object.commit.name, e.object.commit);
        else if (e.what === "refused" && e.purpose !== "take" && SWAPS[e.purpose]) say(`S3 refuses the ${SWAPS[e.purpose]} of ${who}: the manifest has another version.`);
        break;
      case "store":
        if (!e.what) say(e.text || "S3 does something the page does not know.");
        break;
      case "action":
        if (e.do === "client" && e.what === "done") failures = null;
        if (e.what === "done") say(actionWords(e), "you");
        else say(`${actionWords(e)} ${e.what === "failed" ? "It fails" : "Ignored"}: ${e.text}.`, "you");
        break;
      case "buggify":
        say(`Buggify fires: ${e.site}.`);
        break;
    }
    return w;
  };
}
