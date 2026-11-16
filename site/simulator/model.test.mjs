// Tests of the model of the simulator, with Node's built-in test runner and nothing else:
//
//   node site/simulator/model.test.mjs
//
// S4QL_SIM_SEEDS sets the number of seeds of the long run (2000 by default). Nothing here reads a
// real clock: every run is in the model's virtual time.

import { test } from "node:test";
import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { readFileSync } from "node:fs";
import {
  Sim, Rand, sha256Hex, EMPTY_LOG, LEASE_MS, PLANTED_BUGS, commitKey, encodeActions, decodeActions, replay, instanceState, margin,
} from "./model.js";

const SEEDS = Number(process.env.S4QL_SIM_SEEDS || 2000);
const RUN_MS = 240_000;

// fingerprint of a run: everything that it logged and everything that is in the bucket and on the
// disks.
function fingerprint(sim) {
  return JSON.stringify({
    events: sim.events,
    requests: sim.requests.map(({ id, slot, op, key, kind, purpose, sentAt, arrivedAt, result, client, doneAt, answerLost, lateAnswer, held }) => ({ id, slot, op, key, kind, purpose, sentAt, arrivedAt, result, client, doneAt, answerLost, lateAnswer, held })),
    s3: [...sim.s3.entries()],
    disks: sim.slots.map((s) => s.disk),
    steps: sim.steps,
    now: sim.now,
  });
}

// chaosRun is a run with the automatic workload, random faults, and buggify, and the planted bug
// if one is given. It stops at the first violation.
function chaosRun(seed, { bug = "", instances, until = RUN_MS } = {}) {
  const sim = new Sim({ seed, instances: instances ?? 2 + (seed % 2) });
  sim.act({ type: "auto", on: true });
  sim.act({ type: "chaos", on: true });
  sim.act({ type: "buggify", on: true });
  if (bug) sim.act({ type: "bug", name: bug });
  sim.advance(until, { stopOnViolation: true });
  return sim;
}

// holderSlot is the slot of the instance that serves, after running until one does.
function serveOne(sim, limit = 120_000) {
  for (let t = sim.now; t <= limit; t += 50) {
    sim.advance(t);
    const s = sim.slots.find((slot) => slot.proc && instanceState(sim, slot).serving);
    if (s) return s;
  }
  throw new Error("no instance serves");
}

// stepUntil steps the run until cond holds, at most limit steps.
function stepUntil(sim, cond, limit = 100_000) {
  for (let i = 0; i < limit; i++) {
    if (cond()) return;
    sim.step();
  }
  throw new Error("the condition did not come true");
}

function lastWrite(sim, value) {
  return sim.events.filter((e) => e.kind === "write" && e.value === value).at(-1);
}

test("SHA-256 gives the known vectors", () => {
  assert.equal(sha256Hex(""), "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
  assert.equal(sha256Hex("abc"), "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
  assert.equal(sha256Hex("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq"), "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1");
  assert.equal(
    sha256Hex("abcdefghbcdefghicdefghijdefghijkefghijklfghijklmghijklmnhijklmnoijklmnopjklmnopqklmnopqrlmnopqrsmnopqrstnopqrstu"),
    "cf5b16a778af8380036ce59e7b0492370b249b11e8f07a51afac45037afee9d1",
  );
  assert.equal(sha256Hex("a".repeat(1_000_000)), "cdc76e5c9914fb9281a1c7e284d73e67f1809a48a497200e046d39ccc7112cd0");
});

test("SHA-256 agrees with node:crypto at every length up to three blocks, and on text that is not ASCII", () => {
  const r = new Rand(1, "sha");
  const alphabet = ["a", "Z", "0", " ", "\n", "é", "ø", "€", "𝄞", "s4ql"];
  for (let n = 0; n < 200; n++) {
    const bytes = new Uint8Array(n);
    for (let i = 0; i < n; i++) bytes[i] = r.int(0, 255);
    assert.equal(sha256Hex(bytes), createHash("sha256").update(bytes).digest("hex"), `length ${n}`);
    let s = "";
    for (let i = 0; i < n % 40; i++) s += r.pick(alphabet);
    assert.equal(sha256Hex(s), createHash("sha256").update(s, "utf8").digest("hex"), JSON.stringify(s));
  }
});

test("the model reads no real clock and draws no random number from the platform", () => {
  const source = readFileSync(new URL("./model.js", import.meta.url), "utf8");
  for (const banned of ["Date.now", "Math.random", "performance.now", "new Date()", "crypto.", "setTimeout", "setInterval"]) {
    assert.ok(!source.includes(banned), `model.js uses ${banned}`);
  }
});

test("the margin is the larger of two seconds and a tenth of the duration", () => {
  assert.equal(margin(30_000), 3_000);
  assert.equal(margin(10_000), 2_000);
});

test("the same seed and the same actions give the same run, step for step", () => {
  for (const seed of [1, 2, 0x7334716c, 4242]) {
    const a = chaosRun(seed);
    const b = chaosRun(seed);
    assert.equal(fingerprint(a), fingerprint(b), `seed ${seed}`);
    assert.ok(a.events.length > 100);
  }
  assert.notEqual(fingerprint(chaosRun(1)), fingerprint(chaosRun(3)));
});

test("a run with actions at arbitrary moments replays exactly from its encoded actions", () => {
  for (const seed of [5, 6, 7, 8, 9]) {
    // A run as the page drives it: time advances in uneven chunks, single steps come in between, and
    // actions come at any moment.
    const r = new Rand(seed, "user");
    const live = new Sim({ seed, instances: 3 });
    live.act({ type: "auto", on: true });
    const kinds = ["write", "read", "crash", "restart", "shutdown", "stall", "resume", "partition", "reconnect", "disk", "clock", "drop", "fail", "slow", "buggify", "chaos", "bug", "heal"];
    for (let k = 0; k < 120; k++) {
      if (r.chance(0.3)) for (let s = r.int(1, 20); s > 0; s--) live.step();
      else live.advance(live.now + r.int(0, 4_000) + r.float());
      const type = r.pick(kinds);
      const a = { type };
      if (["write", "read", "crash", "restart", "shutdown", "stall", "resume", "partition", "reconnect", "disk", "clock"].includes(type)) a.i = r.int(0, 2);
      if (type === "clock") a.c = r.pick(["back", "ahead", "fast", "slow", "true"]);
      if (type === "drop" || type === "fail") a.k = r.pick(["any", "swap", "upload", "read"]);
      if (["slow", "buggify", "chaos"].includes(type)) a.on = r.chance(0.5);
      if (type === "bug") a.name = r.chance(0.2) ? r.pick(Object.keys(PLANTED_BUGS)) : "";
      live.act(a);
    }
    const end = live.now + 10_000;
    live.advance(end);
    const encoded = encodeActions(live.actions);
    const decoded = decodeActions(encoded);
    assert.deepEqual(decoded, live.actions.map((a) => ({ ...a })));
    const again = replay({ seed, instances: 3, actions: decoded, until: end });
    assert.equal(fingerprint(again), fingerprint(live), `seed ${seed}`);
  }
});

test("decodeActions refuses what it cannot read", () => {
  assert.deepEqual(decodeActions(""), []);
  assert.equal(decodeActions("1.2.q0"), null);
  assert.equal(decodeActions("x"), null);
  assert.equal(decodeActions("1.2.w7"), null);
  assert.equal(decodeActions("5.10.w0_4.10.w1"), null);
});

test(`no invariant is violated over ${SEEDS} seeds with random workloads, random faults, and buggify`, () => {
  const totals = {};
  let seedsWithStaleAllowed = 0;
  let seedsWithTakeover = 0;
  let notServingAfterHeal = [];
  for (let seed = 1; seed <= SEEDS; seed++) {
    const sim = chaosRun(seed);
    const v = sim.checker.violations;
    if (v.length > 0) assert.fail(`seed ${seed}: ${v[0].text} (step ${v[0].n}, ${v[0].t} ms)`);
    for (const [k, n] of Object.entries(sim.stats)) totals[k] = (totals[k] || 0) + n;
    if (sim.stats.staleAllowed > 0) seedsWithStaleAllowed++;
    if (sim.events.some((e) => e.take && e.previous)) seedsWithTakeover++;

    // After every fault is cleared, an instance serves again, and the invariants still hold.
    sim.act({ type: "chaos", on: false });
    sim.act({ type: "buggify", on: false });
    sim.act({ type: "heal" });
    sim.advance(sim.now + 4 * 60_000, { stopOnViolation: true });
    if (sim.checker.violations.length > 0) assert.fail(`seed ${seed} after the faults: ${sim.checker.violations[0].text}`);
    if (!sim.slots.some((s) => s.proc && instanceState(sim, s).serving)) notServingAfterHeal.push(seed);
  }
  assert.deepEqual(notServingAfterHeal, [], "seeds where no instance serves four minutes after the faults were cleared");
  // The run must reach what it is for, or passing says little.
  assert.ok(totals.acked > SEEDS * 5, `acknowledged ${totals.acked}`);
  assert.ok(totals.resolvedCommitted > SEEDS / 20, `resolved as committed ${totals.resolvedCommitted}`);
  assert.ok(totals.resolvedNot > 0, `resolved as not committed ${totals.resolvedNot}`);
  assert.ok(totals.unknown > 0, `outcome unknown ${totals.unknown}`);
  assert.ok(totals.catchUps > SEEDS, `catch-ups ${totals.catchUps}`);
  assert.ok(totals.lateArrivals > 0, `late arrivals ${totals.lateArrivals}`);
  assert.ok(totals.conflicts > 0, `conflicts ${totals.conflicts}`);
  assert.ok(seedsWithTakeover > SEEDS / 4, `seeds with a takeover from a holder ${seedsWithTakeover}`);
  console.log(`# ${SEEDS} seeds:`, JSON.stringify(totals), `seeds with a takeover: ${seedsWithTakeover}, with a stale read the spec allows: ${seedsWithStaleAllowed}`);
});

for (const [bug, text] of Object.entries(PLANTED_BUGS)) {
  test(`the checker finds the planted bug "${text}" within 40 seeds`, () => {
    let found = 0;
    let first = 0;
    for (let seed = 1; seed <= 40; seed++) {
      const sim = chaosRun(seed, { bug });
      if (sim.checker.violations.length > 0) {
        found++;
        first ||= seed;
      }
    }
    assert.ok(found > 0, `no seed of 40 found it`);
    console.log(`# ${bug}: found in ${found} of 40 seeds, first in seed ${first}`);
  });
}

test("a stalled holder resumes after another instance took the lease, and its open write fails the swap", () => {
  const sim = new Sim({ seed: 11, instances: 2 });
  const h = serveOne(sim);
  const w = sim.slots.find((s) => s !== h);
  const oldID = h.proc.id;
  sim.act({ type: "write", i: h.index });
  sim.advance(sim.now + 5_000);
  const value = sim.valueCounter;
  assert.equal(lastWrite(sim, value).outcome, "acknowledged");

  // The next write's transaction is open and its commit object is on its way when the holder stalls.
  sim.act({ type: "write", i: h.index });
  const value2 = sim.valueCounter;
  stepUntil(sim, () => sim.requests.some((r) => r.slot === h.label && r.kind === "upload" && r.commit && !r.answered));
  sim.act({ type: "stall", i: h.index });
  stepUntil(sim, () => instanceState(sim, w).serving);
  const take = sim.events.find((e) => e.take && e.who === w.label);
  assert.ok(take, "the other instance took the lease");
  assert.equal(take.previous, oldID);
  assert.ok(instanceState(sim, w).serving);
  sim.act({ type: "write", i: w.index });
  sim.advance(sim.now + 2_000);
  assert.equal(lastWrite(sim, sim.valueCounter).outcome, "acknowledged");
  assert.ok(h.proc.stalled);

  sim.act({ type: "resume", i: h.index });
  sim.advance(sim.now + 5_000);
  const out = lastWrite(sim, value2);
  assert.equal(out.outcome, "not committed");
  const swap = sim.requests.find((r) => r.slot === h.label && r.purpose === "commit" && r.sentAt > take.t);
  assert.ok(swap, "the stalled holder sent the swap of its commit");
  assert.equal(swap.result, "refused: changed (412)");
  // Its commit object is in the bucket, and not in the log.
  const name = JSON.parse(swap.body).branches.main.head;
  assert.ok(sim.s3.has(commitKey(name)));
  assert.ok(!sim.checker.index.has(name));
  // It refuses reads now.
  sim.act({ type: "read", i: h.index });
  sim.advance(sim.now + 100);
  assert.equal(sim.events.filter((e) => e.kind === "read" && e.who === h.label).at(-1).outcome, "refused");
  assert.deepEqual(sim.checker.violations, []);
});

test("a commit whose answer is lost is resolved from S3 as committed", () => {
  const sim = new Sim({ seed: 12, instances: 3 });
  const h = serveOne(sim);
  sim.act({ type: "drop", k: "swap" });
  sim.act({ type: "write", i: h.index });
  const value = sim.valueCounter;
  sim.advance(sim.now + 10_000);
  const swap = sim.requests.find((r) => r.purpose === "commit" && r.answerLost);
  assert.ok(swap, "the answer to the swap of the commit was lost");
  assert.equal(swap.result, "swapped");
  const out = lastWrite(sim, value);
  assert.equal(out.outcome, "acknowledged");
  assert.ok(sim.events.some((e) => e.kind === "resolved" && e.outcome === "committed" && e.commit === out.commit));
  assert.ok(sim.checker.index.has(out.commit));
  assert.deepEqual(h.disk.rows.at(-1), value);
  assert.deepEqual(sim.checker.violations, []);
});

test("a commit whose caller gave up is resolved by the lease task as committed, and caught up", () => {
  const sim = new Sim({ seed: 13, instances: 3 });
  const h = serveOne(sim);
  sim.act({ type: "drop", k: "swap" });
  sim.act({ type: "write", i: h.index });
  const value = sim.valueCounter;
  stepUntil(sim, () => sim.requests.some((r) => r.purpose === "commit" && r.answerLost));
  // The other instances are down, so that none takes the lease while the holder is cut off.
  for (const s of sim.slots) if (s !== h) sim.act({ type: "crash", i: s.index });
  sim.act({ type: "partition", i: h.index });
  sim.advance(sim.now + 50_000);
  const out = lastWrite(sim, value);
  assert.equal(out.outcome, "outcome unknown");
  assert.equal(instanceState(sim, h).word, "resolving an unknown outcome");
  sim.act({ type: "read", i: h.index });
  sim.advance(sim.now + 100);
  assert.equal(sim.events.filter((e) => e.kind === "read" && e.who === h.label).at(-1).outcome, "refused");

  sim.act({ type: "reconnect", i: h.index });
  sim.advance(sim.now + 10_000);
  assert.ok(sim.events.some((e) => e.kind === "resolved" && e.outcome === "committed" && e.commit === out.commit));
  assert.ok(instanceState(sim, h).serving);
  assert.equal(h.disk.position, out.commit);
  assert.equal(h.disk.rows.at(-1), value);
  assert.deepEqual(sim.checker.violations, []);
});

test("a commit whose swap never arrived is resolved by a renewal as not committed", () => {
  const sim = new Sim({ seed: 14, instances: 2 });
  const h = serveOne(sim);
  // The first swap of the commit fails before it is sent, which to the instance is a swap of unknown
  // outcome, and then the instance is cut off, so every later attempt is lost on its way.
  sim.act({ type: "fail", k: "swap" });
  sim.act({ type: "write", i: h.index });
  const value = sim.valueCounter;
  stepUntil(sim, () => sim.requests.some((r) => r.slot === h.label && r.purpose === "commit" && r.result === "never sent"));
  for (const s of sim.slots) if (s !== h) sim.act({ type: "crash", i: s.index });
  sim.act({ type: "partition", i: h.index });
  sim.advance(sim.now + 50_000);
  assert.equal(lastWrite(sim, value).outcome, "outcome unknown");
  sim.act({ type: "reconnect", i: h.index });
  sim.advance(sim.now + 8_000);
  assert.ok(sim.events.some((e) => e.kind === "resolved" && e.outcome === "not committed"));
  assert.ok(instanceState(sim, h).serving);
  assert.ok(!h.disk.rows.includes(value));
  assert.deepEqual(sim.checker.violations, []);
});

test("after a crash, the next holder catches up from the log, and so does the crashed instance when it comes back", () => {
  const sim = new Sim({ seed: 15, instances: 2 });
  const h = serveOne(sim);
  const w = sim.slots.find((s) => s !== h);
  const values = [];
  for (let k = 0; k < 3; k++) {
    sim.act({ type: "write", i: h.index });
    values.push(sim.valueCounter);
    sim.advance(sim.now + 2_000);
    assert.equal(lastWrite(sim, sim.valueCounter).outcome, "acknowledged");
  }
  // One more commit, and a crash after S3 applied its swap and before the answer came: the log has
  // it, and the cache does not.
  const since = sim.now;
  sim.act({ type: "write", i: h.index });
  values.push(sim.valueCounter);
  stepUntil(sim, () => sim.requests.some((r) => r.slot === h.label && r.purpose === "commit" && r.sentAt >= since && r.result === "swapped" && !r.answered));
  sim.act({ type: "crash", i: h.index });
  assert.deepEqual(h.disk.rows, values.slice(0, 3));

  sim.advance(sim.now + 45_000);
  assert.ok(instanceState(sim, w).serving, "the other instance took over");
  const catchUp = sim.events.find((e) => e.kind === "catchup" && e.who === w.label);
  assert.match(catchUp.text, /4 commit objects from the empty log/);
  assert.deepEqual(w.disk.rows, values);

  sim.act({ type: "write", i: w.index });
  values.push(sim.valueCounter);
  sim.advance(sim.now + 2_000);
  sim.act({ type: "restart", i: h.index });
  sim.advance(sim.now + 2_000);
  assert.equal(instanceState(sim, h).word, "waiting for the lease");
  sim.act({ type: "shutdown", i: w.index });
  sim.advance(sim.now + 8_000);
  assert.ok(instanceState(sim, h).serving, "the restarted instance took the released lease at once");
  const second = sim.events.filter((e) => e.kind === "catchup" && e.who === h.label).at(-1);
  assert.match(second.text, /2 commit objects from/);
  assert.deepEqual(h.disk.rows, values);
  assert.deepEqual(sim.checker.violations, []);
});

test("an instance that starts after a crash waits at least the holder's duration and the margin", () => {
  const sim = new Sim({ seed: 16, instances: 2 });
  const h = serveOne(sim);
  const w = sim.slots.find((s) => s !== h);
  sim.act({ type: "crash", i: h.index });
  sim.act({ type: "crash", i: w.index });
  sim.act({ type: "restart", i: w.index });
  const start = sim.now;
  sim.advance(start + 120_000);
  const take = sim.events.find((e) => e.take && e.who === w.label && e.t > start);
  assert.ok(take.t - start >= LEASE_MS + margin(LEASE_MS), `took the lease after ${take.t - start} ms`);
});

test("clocks affect only reads: a spare whose monotonic clock runs fast takes over early, and no write is lost", () => {
  let staleSeeds = 0;
  for (let seed = 1; seed <= 30; seed++) {
    const sim = new Sim({ seed, instances: 2 });
    const h = serveOne(sim);
    const w = sim.slots.find((s) => s !== h);
    sim.act({ type: "auto", on: true });
    sim.act({ type: "clock", c: "fast", i: w.index });
    sim.act({ type: "partition", i: h.index });
    sim.advance(sim.now + 25_000);
    sim.act({ type: "reconnect", i: h.index });
    sim.advance(sim.now + 60_000);
    // A stale read is reported as allowed, never as a violation, and every other invariant holds.
    assert.deepEqual(sim.checker.violations, [], `seed ${seed}`);
    if (sim.stats.staleAllowed > 0) staleSeeds++;
  }
  assert.ok(staleSeeds > 0, "no seed showed a stale read");
});

test("a cache whose position is not in the log is refused, not rebuilt", () => {
  const sim = new Sim({ seed: 17, instances: 2 });
  const h = serveOne(sim);
  // Under the planted bug the cache has the commit before the swap. A swap that fails then leaves the
  // cache with a position that is not in the log.
  sim.act({ type: "bug", name: "cachefirst" });
  sim.act({ type: "partition", i: h.index });
  sim.act({ type: "write", i: h.index });
  sim.advance(sim.now + 1_000);
  assert.ok(sim.checker.violations.some((v) => v.key === "ahead"));
  sim.act({ type: "bug", name: "" });
  sim.advance(sim.now + 50_000);
  assert.equal(lastWrite(sim, sim.valueCounter).outcome, "not committed");
  const w = sim.slots.find((s) => s !== h);
  sim.act({ type: "crash", i: h.index });
  sim.act({ type: "shutdown", i: w.index });
  sim.act({ type: "reconnect", i: h.index });
  sim.act({ type: "restart", i: h.index });
  sim.advance(sim.now + 60_000);
  const p = h.proc;
  assert.match(p.stopped, /not in the log/);
  assert.notEqual(h.disk.position, EMPTY_LOG);
});
