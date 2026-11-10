# FoundationDB research: testing methodology, transactions, layers, lessons for a Go SQL-on-S3 engine

Date: 2026-11-10. Tags: **[V]** = checked against a primary source (paper text, FDB/Record Layer source code at the commit noted, official docs). **[S]** = secondary source (third-party notes, other people's summaries). **[I]** = my inference.

Source snapshots I read locally (shallow clones, read-only):
- `apple/foundationdb` @ `42069e32` (2026-11-09). Paths below are relative to the repo root. Browse at https://github.com/apple/foundationdb/tree/main/<path>
- `FoundationDB/fdb-record-layer` @ HEAD (2026-11). https://github.com/FoundationDB/fdb-record-layer
- FDB SQL Layer: `github.com/FoundationDB/sql-layer` now returns 404. I read the mirror `github.com/jaytaylor/sql-layer` (last commit 2015-03-01).
- SIGMOD 2021 paper PDF https://www.foundationdb.org/files/fdb-paper.pdf (converted with pdftotext). Record Layer paper https://arxiv.org/abs/1901.04452.

This file does not repeat section 3 of `research-testing.md` (synctest, gosim, WASM, the S3 fault list). It builds on it.

---

## Part 1. Testing methodology

### 1.1 Deterministic simulation: what is simulated and how

**Architecture** [V paper §4; `documentation/sphinx/source/engineering.rst`]
- "All database code is deterministic; accordingly multithreaded concurrency is avoided (instead, one database node is deployed per core)." The network, disk, time and the PRNG are all abstracted. "The production implementation is a simple shim to the relevant system calls." (paper §4)
- engineering.rst: "replacing physical interfaces with shims, replacing the main epoll-based run loop with a time-based simulation, and running multiple logical processes as concurrent Flow coroutines". FDB was "built exclusively in simulation for the first 18 months" before it "sent its first real network packet".
- Flow now uses **standard C++20 coroutines** (`design/coroutines.md`). The old actor compiler is gone, so the "cost of Flow" today is mostly the discipline, not a custom language. [V]
- Workloads (the test clients) are also Flow code. They run inside the same simulation and talk to simulated servers over the simulated network (paper §4). [V]

**The scheduler** [V `fdbrpc/sim2.cpp` `runLoop`]
- One thread and one task queue. If nothing is ready, simulated time jumps to the next timer. Ready tasks are popped in priority order and executed one at a time. There is no preemption, so a task runs until it awaits.
- `now()` is simulated time. `timer()` is deliberately skewed: "timer() can be up to 0.1 seconds ahead of now()". It is randomised, so code that confuses the two clocks gets exercised.
- `delay()` is randomly lengthened: with probability 0.25 it adds `MAX_BUGGIFIED_DELAY * pow(random01(), 1000)`. `MAX_BUGGIFIED_DELAY` is 0 unless a knob randomisation sets it to up to 0.2 s (`flow/Knobs.cpp:259`). The run loop also adds a rare extra sleep. These perturb interleavings without any thread scheduler.
- Paper: "simulations usually have about a 10-1 factor of real-to-simulated time" (testing.rst). The simulator fast-forwards idle periods, so long timeouts cost nothing.

**What is simulated**
- **Machines, processes, zones, data centres.** The cluster topology is randomised per run (`fdbserver/SimulatedCluster.cpp`: machine count, datacenters, replication, storage engine, proxy/resolver/tlog counts, regions, "fearless" multi-region configs, TSS, etc.). [V]
- **Kill types** (`fdbrpc/include/fdbrpc/SimulatorKillType.h`): `KillInstantly, InjectFaults, FailDisk, RebootAndDelete, RebootProcessAndDelete, RebootProcessAndSwitch, Reboot, RebootProcess`. These cover reboots with and without data loss and "machines coming back from the dead" (testing.rst). [V]
- **Network** (`sim2.cpp`): per-link latency, plus per-pair and per-process "clogging" (delivery held until time T). `clogPairFor`, `clogInterface` and random connection failures are disabled for a while after each failure. Partitions are modelled via `disconnectPairUntil`. [V]
- **Disk, unsynced writes** (`fdbrpc/include/fdbrpc/AsyncFileNonDurable.h`, `fdbrpc/AsyncFileNonDurable.cpp`): "An async file implementation which wraps another async file and will randomly destroy sectors that it is writing when killed. This is used to simulate a power failure which prevents all written data from being persisted." Writes and truncates are delayed a random time until `sync`. On a kill, each non-durable page write is done correctly with probability 1/3, or else corrupted or not written. Corrupted writes leave individual sectors unwritten (1/4) or write garbage (1/2). "Writes will be durable in a kill with a 10% probability". Partial first/last pages are possible for non-AIO writes. [V]
- **Disk chaos** (`fdbrpc/AsyncFileChaos.h`): per-I/O delay injection and **single-bit flips** in written buffers, on storage files only. These are driven by `DiskFailureInjector` and `BitFlipper`. [V]
- **Disk space** is modelled, including the drive filling up (testing.rst). [V]
- **S3** (relevant to you): `fdbserver/mocks3/MockS3Server.cpp` plus `MockS3ServerChaos.cpp` (design: `design/mocks3server_chaos_design.md`). It runs inside simulation for backup/bulk-load tests. Chaos covers 429/503/500/502, auth errors, connection drops, timeouts, "malformed responses, bit flips". There are per-operation rate multipliers for GET/PUT/DELETE/multipart/list, all drawn from `deterministicRandom()`. Stated philosophy: "**mocks3 should be more intolerant/strict than real s3**". I grepped and found **no support for conditional writes** (`If-Match`, `If-None-Match`, 412). [V]

**Reproduction from a seed** [V]
- `fdbserver -r simulation -f tests/fast/CycleTest.toml -s <seed> -b on|off`. The seed drives everything. Buggify on or off is a separate input (`contrib/TestHarness2/test_harness/run.py` parses exactly this form).
- `AGENTS.md`: "**Determinism is per seed *and* binary.** Any code change shifts the `deterministicRandom()` sequence, so the same seed on a patched binary is a different execution, not an A/B comparison. Reproduce a failure on the unpatched binary, and judge a fix by failure rates over many seeds." Dropbox makes the same point: they treat the commit hash as a test input (below).
- Paper §6.2: adding logging "generally does not affect the deterministic ordering of events, so an exact reproduction is guaranteed". When a bug was found in the wild, "the debugging process was almost always first to improve the capabilities or the fidelity of the simulation until the issue could be reproduced there".

**Detecting accidental nondeterminism ("unseed")** [V]
- At the end of a run, `fdbserver.cpp:2395` prints `Unseed: deterministicRandom()->randomInt(0, 100001)`, which is the next PRNG draw. If any extra or missing random call happened, it differs. The harness reruns the same seed and binary and compares the two values.
- TestHarness2 does this on **5% of runs by default** (`unseed_check_ratio = 0.05`, `contrib/TestHarness2/test_harness/config.py`). It is skipped for restarting tests ("FIXME: support unseed checks for restarting tests").
- Code that is inherently nondeterministic sets `noUnseed = true` (real thread pools, `Net2` multi-threading), and the check is skipped for it.
- `contrib/debug_determinism/README.md`: "99/100 times the source of the nondeterminism is use of uninitialized memory". Other causes it lists are time not taken from `INetwork`, ordering by heap pointer, "free disk space", and **unordered-map iteration order**. It also offers a tool that records every control-flow edge (`-fsanitize-coverage` trace-pc-guard) to `out.bin`, then on rerun stops at the first divergent edge.

### 1.2 BUGGIFY

**Implementation today** [V `flow/include/flow/Buggify.h`]
```cpp
inline double P_GENERAL_BUGGIFIED_SECTION_ACTIVATED{ 0.25 };
inline double P_GENERAL_BUGGIFIED_SECTION_FIRES{ 0.25 };
inline bool buggify(double probability = P_GENERAL_BUGGIFIED_SECTION_FIRES,
                    const std::source_location location = std::source_location::current()) {
  return isGeneralBuggifyEnabled() && getGeneralSBVar(location.file_name(), location.line()) &&
         deterministicRandom()->random01() < probability;
}
```
- **Three gates.** (1) Buggify is enabled for the whole run (`--buggify on`). The harness turns it on for **80% of runs** (`buggify_on_ratio = 0.8`). (2) Each **call site** (file, line) is activated or deactivated **once per run**, with p = 0.25, on first evaluation. The decision is logged in the trace (`g_traceBatch.addBuggify`), so the set of active sites is part of the record. (3) An activated site fires with p = 0.25 each time, or with a custom probability.
- The old `BUGGIFY` / `BUGGIFY_WITH_PROB(x)` macros have become the `buggify()` function in the current tree, which has about 530 call sites in `.cpp` files. There is a separate `CLIENT_BUGGIFY` family for client-library code.
- `EXPENSIVE_VALIDATION` fires on 5% of evaluations when buggify is on. It is a hook for costly self-checks.
- In production the run-level flag is off, so every site is false. The code stays in the shipped binary, gated at runtime. (Alex Miller: test code ships in production, and "only ever evaluates to true in simulation" is what makes liberal placement safe.)

**Placement patterns** [V Alex Miller, https://transactional.blog/simulation/buggify (2021-12-20); V code]
- Do the minimum legal work. Example: contact only a minimal quorum of coordinators, restricted to nodes known alive.
- Force rare branches: `if (rareCondition || buggify())`.
- Inject or lengthen delays.
- Return a legal-but-unusual result: "unnecessarily returning an error from an operation that usually succeeds, injecting a delay in an operation that is usually fast, choosing an unusual value for a tuning parameter" (paper §4).
- Example from `fdbserver/storageserver/storageserver.cpp:1003`: `if (buggify()) UIDofLongest = deterministicRandom()->randomUniqueID();`, which deliberately makes a lookup miss.

**Knob randomisation** [V]
- Knob definitions follow the pattern `init(KNOB, default); if (randomize && buggify()) KNOB = <unusual>;`. Grep counts: 250 sites in `fdbserver/core/ServerKnobs.cpp`, 63 in `fdbclient/ClientKnobs.cpp`, 34 in `flow/Knobs.cpp`. Example: `MAX_COMMIT_BATCH_INTERVAL` goes from 2.0 to 0.5.
- Paper §4: "Randomization of tuning parameters also ensures that specific performance tuning values do not accidentally become necessary for correctness."

**Why it finds bugs plain fault injection misses** [V Miller; paper §4]
- External network and disk faults rarely compose into high-level scenarios. Miller's example is a minimal quorum being used in two consecutive leader elections. Reaching such a state by chance would need "a tremendous number of simulation runs". Buggify lets the code author state where danger lives.
- Per-site activation is the key design choice. Each run turns on a different random ~25% of sites. This is swarm testing (Groce et al., cited as [40] in the paper). It avoids the failure mode where everything perturbed at once drives the system into one small, degenerate region of state space. Paper: "Fault injection distributions are carefully tuned to avoid driving the system into a small state-space caused by an excessive fault rate."

**Late-test damage control** [V Miller; `sim2.cpp`; `fdbserver/tester/test.cpp`]
- After the workload phase, `g_simulator->speedUpSimulation` is set. Destructive injections check this flag and stop, which gives the cluster a chance to recover.
- Liveness is then checked: "recoverability (eventual availability) can be checked by returning the modeled hardware environment ... to a state in which recovery should be possible and verifying that the cluster eventually recovers" (paper §4).

### 1.3 Fault workloads (automatically composed)

- `fdbserver/tester/WorkloadUtils.cpp` `CompoundWorkload::addFailureInjection`: every test spec automatically gets a random set of **failure-injection workloads** added from a registry. A workload can opt out of specific ones via `disableFailureInjectionWorkloads()`; for example ConflictRange opts out of `RandomRangeLock`. The registered injectors are `MachineAttrition`, `RandomClogging`, `Rollback`, `DiskFailureInjection`, `MoveKeys` and `RandomRangeLock`. [V]
- Fault workloads in specs ([V] `tests/fast/CycleTest.toml`): a spec lists correctness and fault workloads side by side. They run concurrently, and each `check()` runs at the end.
  ```toml
  [[test]]
  testTitle = 'Clogged'
      [[test.workload]]  testName = 'Cycle'  transactionsPerSecond = 2500.0  testDuration = 10.0
      [[test.workload]]  testName = 'RandomClogging'  testDuration = 10.0
      [[test.workload]]  testName = 'Rollback'  meanDelay = 10.0
      [[test.workload]]  testName = 'Attrition'  machinesToKill = 10  machinesToLeave = 3  reboot = true
  ```
- **Swizzle-clogging** [V testing.rst]: "pick a random subset of nodes in the cluster. Then, you 'clog' (stop) each of their network connections one by one over a few seconds. Finally, you unclog them in a random order, again one by one". It emerged as the winner of an internal competition to find the toughest bugs.
- **Rollback** workload [V `fdbserver/workloads/Rollback.cpp`]: it clogs a proxy's links to all tLogs but one, kills the proxy, then clogs the remaining tLog. This forces a commit that reached only part of the log to be rolled back during recovery. It is a targeted scenario for the ambiguous-commit path.
- **Correlated failures**: the talk notes describe a Hurst-exponent model. The idea is that the same cause (one rack, one batch of disks) produces clustered failures. [S talk notes, https://alex-ii.github.io/notes/2018/04/29/distributed_systems_with_deterministic_simulation.html]
- **Restarting/upgrade tests** [V `tests/restarting/from_7.3.0/...`]: run with an old binary, end with `SaveAndKill`, then restart the same on-disk state with the new binary (`-1.toml` / `-2.toml` pairs). Paper §6.3: upgrades restart all processes at once, so "we only need to make sure on-disk data is compatible."
- `StorageCorruption`, `DiskFailureInjection`, `TargetedKill`, `KillRegion`, `ClogTlog`, `ClogSingleConnection` and `SuspendProcesses` also exist. (I looked at their names only.)

### 1.4 Correctness workloads and their oracles

Every workload implements `setup()`, `start()`, `check()` and `getMetrics()` (`fdbserver/tester/include/fdbserver/tester/workloads.h`). A global oracle also applies: **any `SevError` trace event anywhere fails the run** (`AGENTS.md`). "Errors carrying an injected fault are auto-downgraded; genuine `SevError`s are not." [V]

| Workload | What it does | Oracle type | Invariant |
|---|---|---|---|
| **Cycle** (`Cycle.cpp`) | N keys form a ring of "next" pointers. Each txn reads r→r2→r3→r4 (3 dependent reads) and rewrites three pointers so r2 and r3 swap places. It also clears a range with an explicit conflict range. | Self-checking invariant | At the end (and in any txn) the data must be a single cycle of length N: node count unchanged, no "cycle got shorter/longer", no missing key mid-txn (`badRead`). It detects lost updates, partial commits, stale or non-serializable reads, and phantom keys. **Retry-safe under `commit_unknown_result`**: a re-executed txn re-reads, so a double apply is just another valid swap. [V] |
| **Increment** (`Increment.cpp`) | Each txn does two atomic `AddValue(+1)`s, one in the first half of the keyspace and one in the second. | Self-checking | Sum(first half) == Sum(second half), which is atomicity. It deliberately does **not** check sum == committed count, because unknown results can double-apply. A rate-below-expected check exists but is only SevWarnAlways. [V] |
| **Sideband** (`Sideband.cpp`) | A mutator commits a unique key and then sends (key, commitVersion) to a checker client over a **separate simulated channel**. The checker then reads it in a new txn. | Self-checking | "CausalConsistencyError" if the checker cannot see a key whose commit it was told about. This tests strict serializability / external consistency across clients. The mutator is idempotent: it reads the key first and counts `keysUnexpectedlyPresent` (an earlier ambiguous attempt that actually committed). [V] |
| **Serializability** (`Serializability.cpp`) | Generate 3 random txns A, B, C (gets, ranges, selectors, mutations, snapshot reads, watches, explicit conflict ranges). Run them as A; B; C (3 commits) from a reset state. Reset again, then run A and B in **one** txn followed by C. | Metamorphic equivalence | Read results and final DB contents must match (`SRL_ResultMismatch`). Results of non-deterministic reads are suppressed. [V] |
| **ConflictRange** (`ConflictRange.cpp`) | Two txns share a read version. One does random sets/clears and commits. The other does a random `getRange` with key selectors and limits, then commits. | Self-checking, precise | If the second **conflicted**, re-run the range and assert the result really changed. Known exceptions are enumerated (limits, selectors into system keys, offsets). If it **committed**, assert the result is unchanged. This checks both false and missed conflicts. The file says buggify and `failConnection` must be off for it "to report properly". [V] |
| **WriteDuringRead** (`WriteDuringRead.cpp`) | Random interleaved reads, writes, clears, atomics and watches inside a txn, issued concurrently (writes while reads are in flight). | **Model**: `std::map<Key,Value> memoryDatabase` and `lastCommittedDatabase` | Every read must equal the model's result, including read-your-writes semantics. On `not_committed` / `commit_unknown_result` / too-large, it **throws away the model and reloads the database** ("initialKeyDensity ... after an unknown result"). Ambiguity is handled by reset, not resolution. [V] |
| **RyowCorrectness**, **ApiCorrectness** (`ApiWorkload.h`, `MemoryKeyValueStore.h`) | Random op sequences against the native API and RYW transactions. | **Model**: in-memory KV store mirroring the DB | Results must equal the model. [V] |
| **FuzzApiCorrectness** | Random, often invalid, API calls with expected-error tables. It enables `CLIENT_BUGGIFY`. | Expected error set | The source itself calls it brittle: "Maybe we should put it out of its misery". [V] |
| **VersionStamp** (`VersionStamp.cpp`) | Versionstamped keys and values. | Model + read-back | On `commit_unknown_result`, it **reads back** the key in a new txn to learn whether the write committed. [V] |
| **AutomaticIdempotency** (`AutomaticIdempotencyWorkload.cpp`) | Txns with idempotency IDs each set a versionstamped key. | Count | "Check that each transaction committed exactly once." [V] |
| **ConsistencyCheck** (`ConsistencyCheck.cpp`; runs after every test via `fdbserver/tester/test.cpp`) | Compares all replicas of each shard, checks metadata (teams, server addresses, storage metadata, singletons). | Structural (replica equality) | Runs at the end of every test spec (`runConsistencyCheck`). "Quiescent" checks run only when buggify is off. [V] |
| **SelectorCorrectness, AtomicOps, AtomicOpsApiCorrectness, Unreadable, CommitBugCheck ("Regression tests for 2 commit related bugs"), BackupCorrectness/Restore*, Watches, Inventory** | (names only) | mixed | |

**Model vs self-checking.** FDB mostly uses **self-checking invariants** (Cycle, Increment, Sideband, ConflictRange) and **metamorphic equivalence** (Serializability). These work with many concurrent clients and survive ambiguous commits. **Models** (WriteDuringRead, RYW, ApiCorrectness) are used for single-client API semantics, and they reset whenever an outcome is ambiguous. Pierre Zemb gives the same rule for layer workloads: reference models suit single-client workloads; use conservation laws, structural integrity, and "operation logging" (write an intent/log record in the same txn, then replay at check time) for concurrent ones. [V/S https://pierrezemb.fr/posts/writing-rust-fdb-workloads-that-find-bugs/ (2025-12-09)]

### 1.5 Coverage assurance: TEST() → CODE_PROBE

- Paper §4 (still the old name): "a developer concerned that a new piece of code may rarely be invoked with a full buffer can add the line `TEST( buffer.is_full() );` and analysis of simulation results will tell them how many distinct simulation runs achieved that condition. If the number is too low, or zero, they can add buggification, workload, or fault injection functionality". [V]
- Today: `CODE_PROBE(condition, "comment", annotations...)` (`flow/include/flow/CodeProbe.h`), with about 718 uses. Each probe is a static singleton registered at startup. On its first hit it emits a `CodeCoverage` trace event (File, Line, Condition, Covered, Comment). Annotations include `probe::decoration::rare` (75 uses), `probe::context::sim2/net2` and `probe::assert::simOnly`. Probes compile in all builds. [V]
- Aggregation: `contrib/TestHarness2/test_harness/results.py` sums per-probe hit counts across an **ensemble** (a Joshua job of many runs). It reports missed probes and missed non-rare probes. `hit_per_runs_ratio = 20000` means a probe is OK if it is hit at least once per ~20,000 runs. Its docstring says this is "a SEPARATE, opt-in analysis"; "an unhit CODE_PROBE never fails a run or the ensemble". **So coverage is a monitored metric, not a merge gate** (at least in the open-source harness). [V]
- Test selection (`run.py` `choose_test`): 99% of the time it picks the test with the least accumulated runtime × priority, and 1% of the time the test with the fewest runs. Tests under `tests/rare` get lower priority. This keeps the CPU budget balanced across specs. [V]
- Mapping to Antithesis "sometimes" assertions: CODE_PROBE is a sometimes-assertion ("this situation occurs in at least one run"). Antithesis docs: "code coverage only covers *locations*, while sometimes assertions cover *situations*". A sometimes-assertion that never fires means either the state is unreachable or your tests are too weak. Antithesis also uses each one as a checkpoint to restart exploration from, which compounds rare events. FDB's harness does not do that. [V https://antithesis.com/docs/best_practices/sometimes_assertions/; I for the comparison]

### 1.6 Scale and process

- testing.rst: "tens of thousands of simulations every night ... we estimate that we have run the equivalent of roughly one trillion CPU-hours of simulation". Note the word "equivalent", which includes intensity scaling. engineering.rst repeats "a trillion CPU-hours". [V]
- Record Layer paper (2019) gives a much more concrete figure: "In the past year, we have run more than 250 million simulations equivalent to more than 1870 years and 3.5 million CPU-hours." The trillion figure is clearly an inflated "equivalent" metric. **Treat 3.5M CPU-hours/year as the real order of magnitude.** [V both; I on the interpretation]
- **Joshua** (https://github.com/FoundationDB/fdb-joshua): "How about using FoundationDB to test FoundationDB?" Agents pull "ensembles" (tarballs with a `joshua_test` script) from a coordinating FDB cluster, run them, and record pass/fail. Agents "die on failure" and are rescheduled by the environment. Priority runs from 1 to 200. `AGENTS.md`: bulk correctness runs on the internal Joshua cluster against Release builds. [V]
- Paper §4: "Randomized testing is embarrassingly parallel and FDB developers can and do 'burst' the amount of testing they do immediately before major releases". Finding bugs on the commit that introduced them makes triage "trivial". [V]
- Triage: a failure is reproduced from (binary, test file, seed, buggify flag). Then you add `TraceEvent`s and rerun, which works because logging does not change event order. `joshua_logtool` collects logs for failures (`run.py`). [V]
- **Performance testing** is separate: the nightly "Circus" environment runs the same workload code on real clusters. Paper §4 Limitations: "Simulation is not able to reliably detect performance issues, such as an imperfect load balancing algorithm." [V]
- **Real hardware ("Sinkhole")**: a cluster of real machines with networked power switches. Per the talk notes and the moonpool history page, it found bugs in other software (dependencies, OS/packaging), reportedly **not** in FDB itself. Paper §6.2: ZooKeeper "was deleted after real-world fault injection found two independent bugs in Zookeeper (circa 2010) and was replaced by a de novo Paxos implementation written in Flow". [S https://alex-ii.github.io/notes/2018/04/29/..., https://pierrez.github.io/moonpool/part1-why/04-history.html; V paper for ZooKeeper]
- **Production claims** [V paper §6.2]: CloudKit ran FDB "for more than 0.5M disk years without a single data corruption event"; "no inconsistent data replicas have ever been found in our production clusters". Constant replica consistency checks also run in production (consistency scan).
- **Jepsen claim**: Will Wilson (Antithesis launch post, 2024-02-13) says Kyle Kingsbury "didn't even bother testing it with Jepsen, because he didn't think he'd find anything", and that the company "only ever had one or two bugs reported by a customer". [V that Wilson says it; https://antithesis.com/blog/is_something_bugging_you/] **I could not find Kingsbury's own statement.** Every copy I found is secondhand.
- The paper's own framing (§7): Jepsen-style approaches "lack deterministic reproducibility", and model checking "can only verify the correctness of a model rather than of the actual implementation". [V]

### 1.7 Limitations and honest criticism

- **Paper §4 Limitations** [V]: (1) no reliable detection of performance issues; (2) "unable to test third-party libraries or dependencies, or even first-party code not implemented in Flow. As a consequence, we have largely avoided taking dependencies on external systems"; (3) "bugs in critical dependent systems, such as a filesystem or the operating system, or misunderstandings of their contract, can lead to bugs in FDB. For example, several bugs have resulted from the true operating system contract being weaker than it was believed to be."
- **Talk notes** [S]: "simulation is only as good as our understanding of OS & hardware". The simulator may not be brutal enough. Developers may unconsciously learn to write code that passes their own simulator. The suggested remedy is a second simulator that developers don't test against.
- **Oracles weaken under chaos** [V]: ConflictRange needs buggify off. Quiescence checks are skipped when buggify is on. Increment cannot assert exact counts. Some properties are only checkable in calm runs, so you need both calm and chaotic runs.
- **Seeds don't survive code changes** [V AGENTS.md]: a found bug must become a dedicated regression spec (e.g. `CommitBugCheck.cpp`, `tests/.../bugs`).
- **Testing is not proof** [V FDB wiki, "Brainstorming: Making FoundationDB more resilient against bugs", last edited by Markus Pilman 2020-01-15, https://github.com/apple/foundationdb/wiki/Brainstorming:-Making-FoundationDB-more-resilient-against-bugs]: "it can only prove the existence of bugs - never the absence". Goal: "bugs should only be able to cause crashes or stalls, but not data corruption or data loss". Proposals: per-component **runtime invariant checks** (crash early), a **verification engine** that checks commit batches before they reach the log, and "Buggify++" (inject byzantine faults, then use restart tests to verify that no corruption reached disk). Existing defences: storage checksums, and FlatBuffers file identifiers so that a mis-typed message crashes rather than being misread.
- **Cost** [V Antithesis DST page, https://antithesis.com/resources/deterministic_simulation_testing/]: the approach is "generally impractical for systems already in production". Wilson: FDB had "the benefit of a green-field project" and "no dependencies (or at least, we didn't once we'd deleted them all)". Moonpool's summary of the cost: "a custom language, a simulation-first architecture and two years of infrastructure before the product". [S]
- **Layers above FDB did not get DST** [V/I]: the Record Layer and SQL Layer are Java and run their tests against a real FDB cluster (`docs/sphinx/source/Building.md` in the Record Layer). Zemb (2025): Rust is "the only language besides Flow" that can run inside FDB's simulator, via `foundationdb-simulation` ("external workloads"). His first simulated seed found `commit_unknown_result` double-incrementing atomic counters in a layer. **The famous testing covered the KV core, not the SQL layers.** [V Zemb; I that this generalises]
- **What Antithesis was founded to fix** [V Wilson post; Antithesis DST page]: making FDB-style testing available without rewriting the system in a deterministic runtime. It is a **deterministic hypervisor** that runs ordinary software (e.g. a whole docker-compose) and controls time, entropy, thread scheduling and I/O. Exploration is guided by assertions. The company was founded in 2018. Caveat on their own page: DST platforms "still require external dependencies to be mocked or otherwise plugged". [V]
- **Not found**: a public, detailed list of FDB correctness bugs that escaped simulation into production. The paper alludes to OS-contract bugs without listing them.

---

## Part 2. Transaction design (relevant to us)

- **OCC + MVCC = SSI, strict serializability** [V paper §2.4]: the client gets a read version (GRV) "no less than any previously issued transaction commit version". Reads happen at that version. Writes are buffered on the client, and read-your-writes is merged client-side. At commit the proxy gets a commit version "larger than any existing read versions or commit versions". **Resolvers** check, per range, whether `lastCommit[r] > Tx.readVersion` for any read range (Algorithm 1). If not, the txn is admitted and its write ranges are added to `lastCommit`. Then it is made durable on all designated LogServers, and finally acked. Commit versions are gap-free (the previous LSN is sent along) and advance at about 1M versions/s. "Because Tx observes the results of all previous committed transactions, FDB achieves strict serializability." The commit version is chosen *before* conflict checking, which allows batching (one resolver handles about 280K TPS).
- **Read/write conflict ranges** [V]: single keys become ranges, so phantoms are prevented by range conflicts. Snapshot reads add no read conflict range. You can add or remove conflict ranges explicitly. Read-only txns commit locally with no server contact. Resolvers are range-partitioned and **may produce false aborts** (a txn admitted by some resolvers but rejected by others still updates their `lastCommit`). The paper accepts this cost.
- **Atomic ops** (add, and, min/max, compare-and-clear, set-versionstamp) write without reading, so they never add read conflicts. "Only write-read conflicts can still happen." [V paper §2.4]
- **Limits and why** [V paper §2.2, §6.4; known-limitations.rst]: keys ≤ 10 KB, values ≤ 100 KB, txn ≤ 10 MB "affected data" (written keys and values plus read/write conflict range keys; values read don't count). The docs say to redesign if a txn exceeds 1 MB. **5 s MVCC window**: "to limit the memory usage of the transaction system and storage servers, because the multi-version data is stored in the memory of Resolvers and StorageServers". After 5 s, reads get `transaction_too_old` and commits fail. The paper's view: exceeding it "often exposes inefficiency in the application". Long jobs are split into many small txns (the TaskBucket pattern; the Record Layer's online indexer commits every 4 s / 100 records / 900 KB).
- **Ambiguous commit: `commit_unknown_result`** [V developer-guide.rst]: it arises when the connection to the commit proxy is lost, or the proxy fails mid-commit. **The guarantee:** "at the point of time where you receive this error, the transaction either committed or not and if it didn't commit, it will never commit in the future ... it is guaranteed that the transaction is not in-flight anymore". By contrast, `transaction_timed_out` and `operation_cancelled` lack that guarantee: the abandoned commit "could get committed at a later point in time". The standard `@fdb.transactional` retry loop **does** retry `commit_unknown_result`, so non-idempotent txns can double-apply.
- **Idempotency patterns** [V developer-guide.rst; automatic-idempotency.rst]: (1) generate IDs *outside* the retry loop; (2) inside the loop, check for the txn's own unique side effect and return early if present (the deposit example with a `depositKey`). **Automatic idempotency** (experimental, "not recommended for use in production"): the client attaches a 16-byte random ID. The commit proxy stores the IDs in `\xff\x02/idmp/<commitVersion BE><batchIndexHi>`. After an ambiguous outcome the client calls `determineCommitStatus` and can recover the commit version and versionstamp; a cleaner expires old IDs (`design/idempotency_ids.md`). Rationale: non-idempotent txns are "probably the biggest 'gotcha'", and users "won't discover it organically".
- **Versionstamps** [V api-common.rst.inc; design/tuple.md]: 10 bytes = 8-byte big-endian commit version plus 2-byte batch order. They are unique per committed txn and monotonic, but not sequential. The tuple layer adds a 2-byte user version (96-bit, type code 0x33) for ordering within a txn. Uses: append-only logs and queues without conflicts, "VERSION" indexes (Record Layer), and the **metadata version key** (below).
- **Snapshot reads** [V developer-guide.rst]: they permit the read values to be modified concurrently without conflict. The intended pattern is a snapshot range read followed by `add_read_conflict_key` on just the key you depend on. The Record Layer warns: "Bugs due to incorrect manual conflict ranges are very hard to find ... layers should generally define abstractions ... rather than relying on individual client applications to relax isolation requirements" (RL paper §10.1).

---

## Part 3. Layers: SQL and relational on an ordered KV store

**Tuple layer** [V design/tuple.md]
- An order-preserving, type-tagged encoding. Each element starts with a type code: null 0x00; bytes 0x01 and string 0x02 are null-terminated with 0x00 escaped as 0x00 0xFF; nested tuple 0x05; integers 0x0c–0x1c are variable length with the code giving sign and length; floats/doubles use transformed IEEE bits; false/true 0x26/0x27; UUID 0x30; versionstamps 0x32/0x33.
- A common tuple prefix gives a common byte prefix, so prefix range scans equal tuple-prefix queries.
- The Go implementation is `bindings/go/src/fdb/tuple`, but it **imports the cgo `fdb` package**, so importing it pulls in libfdb_c. Port it (Apache-2.0) rather than import it. [V]

**Directory layer**: maps long paths to short integer prefixes, allocated with a sliding-window allocator (RL paper §2). [V]

**FDB SQL Layer** (Java, ex-Akiban, 2013–2015; mirror https://github.com/jaytaylor/sql-layer)
- It spoke the PostgreSQL wire protocol (`fdb-sql-layer-pg`) and used the FoundationDB SQL parser. [V code]
- Storage keys are a per-object directory prefix plus a tuple-packed "hKey" (`server/store/FDBStoreDataHelper.java`, `packedTuple(...)`). hKeys come from Akiban's **table-group** model, where child rows are interleaved under their parent's key. Indexes have their own prefixed trees. [V code for the encoding; the table-group description is from my Akiban background knowledge, I]
- **Schema/metadata** (`server/store/FDBSchemaManager.java` header comment) [V]:
  - The schema is stored as protobuf ("AIS") under `schemaManager/protobuf/<schema>`, alongside `generation`, `dataVersion` and `metaDataVersion` keys.
  - "All consumers of getAis() do a full read of the generation key to determine the proper version." "All DDL executors increment the generation while making the AIS changes." Because every txn reads `generation`, **every DDL conflicts with every concurrent DML txn**, which is a simple, correct schema-change fence.
  - **Online DDL**: `online/<id>/dml/<tid> => hKeys of concurrent DML`, plus a changeset and error. Concurrent DML records which rows it touched; per the comment "Written in the same transaction executing DML, checked in session executing DDL". The online DDL then reconciles those rows (`server/store/OnlineHelper.java`).
- **Testing** [V code]: about 397 YAML SQL test files (`fdb-sql-layer-test-yaml`, including a `bugs/` folder of 49 regressions) and randomized differential tests. For example, `RandomSemiJoinTestDT` generates 2 random queries, runs each, computes `IN` / `NOT IN` / `EXISTS` results by hand, and compares them with the engine's combined query. There is also a random-cost YAML runner.
- **What was hard**: I found **no public retrospective**. The original repository and the foundationdb.com docs are gone.

**Record Layer** (SIGMOD 2019, https://arxiv.org/abs/1901.04452; repo https://github.com/FoundationDB/fdb-record-layer)
- Records are protobuf messages. The primary key is a key expression evaluated to a tuple. A record can be split across several KV pairs, so replacing one needs a range clear. Secondary index key = indexed tuple + primary key, mapping to empty. **Index maintenance happens in the same txn as the record write** ("ensuring that indexes are always consistent with the data"). Each index lives in its own subspace and is dropped with one range clear. [V §6]
- **Metadata versioning** [V §5]: "single-stream, non-branching, monotonically increasing". Each record store has a header KV holding its metadata version, a storage-format version and an application version. These are compared on every open. Metadata is aggressively cached by clients. **Future-work worry**: the header is read on every open, so it becomes a hot key. They suggested caching with invalidation. The FDB core later added `\xff/metadataVersion` (design/metadata-version.md): a versionstamp returned *with every read version* from the master, so clients can validate caches without a storage read. [V]
- **Online index builds** [V §6; docs/sphinx/source/IndexBuilding.md]: a new index starts **write-only**, meaning live writes maintain it but queries can't use it. The `OnlineIndexer` scans in many small txns (defaults: 100 records, 900,000 bytes, 4,000 ms, 10,000 records/s, BATCH priority), persists progress in a `RangeSet`, and finally marks the index **readable**. Stores under about 200 records are rebuilt in the opening txn. An experimental "mutual indexing" mode lets parallel builders claim fragments. The `RangeSet` "keeps non-idempotent indexes such as COUNT consistent".
- **Continuations** [V §3–4]: cursors are stateless. Any scan can stop at a limit (records, bytes, time) and return an opaque continuation. This is how work fits in 5 s and how throttling works.
- **Lessons they report** [V §10]: "Building layers ... remains a complex engineering challenge". The single-threaded client network thread became a bottleneck when futures completed on it. Manual conflict-range bugs are "very hard to find". Code-defined metadata is unsafe across mixed versions, so they use an authoritative metadata store. Relational semantics surprised users (one extent for all types, so type-prefixed primary keys were added later). Multi-tenancy "is remarkably difficult to add to an existing system".
- **Relational/SQL layer today** [V repo]: modules `fdb-relational-api/core/jdbc/grpc/server/cli`. "FRL does not perform in-memory sorting or aggregation. Operations like ORDER BY, GROUP BY, and aggregates require underlying indexes" (`SQL_Getting_Started.md`). Queries must stream within the txn limits.
- **Relational testing** [V `docs/sphinx/source/Building.md`; `yaml-tests/`]: about 280 files in `yaml-tests/src/test/resources`, mostly `.yamsql` tests (schema template, setup, test blocks with expected results), many paired with `.metrics.yaml` files.
  - The metrics files snapshot **EXPLAIN plans and planner counters** (task_count, transform_count, and so on), which guards against plan regressions.
  - The same YAML suite runs under several configurations. `maxRows: 1` forces a continuation after every row, which exercises resume logic. `rpcTest` uses in-process gRPC/JDBC. `mixedModeTest` "alternat[es] between the current version and some older version" against the 10 most recent releases, testing backward compatibility of data and continuations. Each test records the earliest version it holds for in `supported_version`.
  - The nightly build adds randomized tests with seeds (`RandomizedTestUtils`, `tests.iterations`) and a random locale.

---

## Part 4. Lessons for a single-library Go engine on S3

### 4.1 Techniques that transfer, with concrete Go mechanisms

1. **Buggify, including the per-site activation.** [I, modelled on V Buggify.h]
   - Gate it with a build tag. Under `//go:build !dst`, `func Buggify(site string) bool { return false }` is inlined, and the compiler removes the dead branch. Under `//go:build dst`, it uses a seeded `*rand.Rand` from the simulation context, plus `map[string]bool` activation decided on first use with p = 0.25, firing p = 0.25. Enable it in about 80% of runs and log the activated sites.
   - Prefer explicit string site IDs (`buggify.Site("commit.skip-cache")`) to `runtime.Caller`. This keeps IDs stable across edits, though not seed stability.
   - **madsim's `buggify()` omits per-site activation** (it simply fires 25% of the time; https://github.com/madsim-rs/madsim/blob/main/madsim/src/sim/buggify.rs). Don't copy that simplification. Per-site activation gives FDB its swarm diversity.
   - Prior art for the shape: Antithesis Go SDK `assert.Sometimes/Always/Reachable`, whose no-op file is under `//go:build no_antithesis_sdk` (https://github.com/antithesishq/antithesis-sdk-go/blob/main/assert/assert_noop.go). pingcap/failpoint (`failpoint.Inject("name", func(){...})` plus `failpoint-ctl` code rewriting) and etcd gofail are Go failpoint systems. They are **name-activated** (targeted), not randomly swarm-activated. You can build buggify on top by activating a random subset of names per seed. [V READMEs]
   - Good buggify sites for your engine [I]: skip or evict the local cache entry; pretend the cache is cold; return a stale-but-legal cached manifest so the code must hit the CAS-retry path; split a commit into the minimum or maximum number of objects; force multipart with a tiny part size; insert a delay between "PUT data objects" and "CAS manifest"; force compaction or GC to run now; choose the smallest legal page or row-group size; and make `List` return results in pages of 1.
2. **Knob randomisation.** [I from V] Put every tunable (cache sizes, page/segment sizes, multipart thresholds, retry counts and backoffs, compaction triggers, GC grace periods, batch sizes) in one `Knobs` struct with `Randomize(r *rand.Rand)` under the dst tag. FDB has about 347 such sites. Correctness must not depend on any tuning value.
3. **The sans-I/O core with a seeded event loop** (already recommended in research-testing.md §3).
   - **Go example: the Resonate legacy server** (Go, https://github.com/resonatehq/resonate-legacy-server). Its kernel is submission/completion queues (`internal/kernel/bus` SQE/CQE). The test loop calls `system.Tick(time)` with a seeded `*rand.Rand` (`test/dst/dst.go`).
   - Its DST I/O (`internal/aio/aio_dst.go`) injects, with probability p, either "simulated failure before processing" or a failure **after** processing (the effect happened but the reply is lost). It iterates maps via `util.OrderedRange` for determinism. Histories are checked for linearizability with **porcupine** against a model (`test/dst/model.go`). This is the closest Go analogue to FDB I found.
   - Resonate later rewrote the server in Rust. [V repo metadata]
4. **Unseed / doublecheck.** [I from V] At the end of each run, emit `rng.Uint64()` and a hash of the ordered (op, result) event trace. On a sample of runs (FDB: 5%; for a sans-I/O core you can afford 100%) rerun the same seed in-process and compare. Turso's simulator `--doublecheck` and madsim's `Runtime::check_determinism` do the same. Map iteration, `select`, goroutine scheduling, `time.Now` and top-level `math/rand` are the Go equivalents of FDB's "unordered map / non-INetwork time" causes.
5. **Fault injectors composed automatically into every correctness workload.** [I from V WorkloadUtils.cpp] Use a registry of injectors (object-store errors and latency, ambiguous PUT, zombie delayed PUT, process crash at the k-th I/O, cache-disk corruption, clock skew between engine instances). Each run picks a random subset. A workload must explicitly opt out when its oracle can't tolerate a fault, as ConflictRange does.
6. **End-of-run protocol.** [I from V] Stop faults (FDB's `speedUpSimulation`). Require **liveness**: all clients finish within a bounded number of sim-ticks. Then run a **consistency check** (your bucket fsck: every referenced object exists, checksums match, index entries equal table rows, and each engine instance's cache equals bucket truth). Finally run each workload's `check()`. Also: any `slog` at Error level not tagged as injected fails the run. FDB auto-downgrades errors carrying injected faults; in Go, wrap injected errors in a sentinel type (`errors.Is(err, ErrInjected)`).
7. **CODE_PROBE counters.** [I from V] `probe.Hit("commit.cas-lost-race")` and `probe.HitIf(cond, id)` keep atomic counters in a global registry. Declare every probe ID in one table. A nightly job aggregates across N seeds and **fails if a non-rare probe was hit zero times**. FDB itself only reports this; with no human reviewers, make it a gate. Mark known-rare probes with a minimum-runs threshold, like FDB's `rare` decoration.
8. **Calm and chaos runs.** [V FDB pattern] Some oracles (exact counts, precise-conflict checks, performance floors) only work with faults off. Run every workload both ways.
9. **Upgrade/restart tests.** [V FDB restarting tests; V RL mixedModeTest] Keep old engine versions as Go module versions or a vendored binary in CI. Write a bucket with version N-k, then open, read, write and compact with N, then optionally go back to N-k if you promise downgrade. The bucket format is your only durable contract, so this matters more for you than for FDB.
10. **Determinism caveat for regressions.** [V AGENTS.md] Seeds break on any code change, so convert every found bug into a named, explicit scenario test. Dropbox includes the commit hash in the repro tuple.

### 4.2 What does not transfer, and why

- **Whole-program determinism, including scheduling.** [I] FDB gets it from one thread and cooperative coroutines. The Go runtime schedules goroutines preemptively and `testing/synctest` controls only time. Only the sans-I/O core can be fully deterministic. Everything around it (the SDK, net/http, TLS, goroutine pools) is "mostly" deterministic. Antithesis is the only way to get a fully deterministic run of the whole real binary (WarpStream did exactly this for Go; see below).
- **Simulating your dependencies.** [V paper] FDB's answer was to delete them (ZooKeeper went). You cannot delete S3. Its real semantics (conditional-write races, 409 ConditionalRequestConflict, Tigris consistency modes) must be learned from real-service tests, then encoded in your fake. Follow FDB's MockS3 rule, "**more intolerant/strict than real s3**". FDB's MockS3 has **no conditional writes**, so there is nothing to borrow there.
- **Cluster-level faults** (partitions between servers, clogging tLogs, recovery). [I] You have no servers. Your "cluster" is N engine instances (in your tests: N engine objects in one process, each with its own cache) sharing one bucket. The useful analogues are swizzle-clogging *the object-store path of selected instances* (stall and unstall their requests in random order), and killing or restarting instances mid-commit.
- **Resolvers and the 5 s limit as designed.** [I] FDB's limit exists because MVCC history lives in resolver and storage memory. Your analogue is how long old manifests, segments and snapshot objects must survive before GC deletes them. **A hard maximum txn and snapshot lifetime is what makes GC safe.** Choose one explicitly (FDB's 5 s and the Record Layer's 4 s chunking show that short txns plus continuations are workable) and enforce it with an error like `transaction_too_old`.
- **Server-side idempotency tables.** [I] FDB keeps idempotency IDs at the proxy. You can embed the txn's random ID in the commit object itself and resolve ambiguity by reading it back (VersionStamp and Sideband do this).
  - But FDB's key guarantee, "if it didn't commit, it will never commit in the future", does not come free on S3: a delayed conditional PUT may still land.
  - To recreate the guarantee, **fence before concluding "not committed"**: make a conditional write that occupies or advances the slot you raced for (e.g. `If-None-Match: *` on the next manifest version with a "noop/abort" body, or an `If-Match` bump). Afterwards the stale PUT can never succeed, and reading the slot tells you who won.
  - Test this path heavily with a buggified "zombie PUT" injector.

### 4.3 The smallest FDB-style workload set worth copying (SQL transactions)

Each one runs with N engine instances × M client goroutines, random fault injectors, buggify on in about 80% of runs, and calm and chaos variants. [I, each derived from the V workload above]

1. **Cycle (SQL)**: `ring(id PK, next)`. Each txn does 3 dependent `SELECT next ... WHERE id=?` reads and swaps two nodes with 3 `UPDATE`s. Check that the data forms a single cycle of length N, at the end and on every read. It catches lost updates, write skew, partial commits after a crash, stale cache reads and broken snapshots. It is retry-safe under ambiguous commits.
2. **Bank/Increment**: transfers between accounts (conservation of the total), plus paired increments of two counters in different tables or index ranges (equality). Check the total and the pair equality. Never check exact counts unless outcomes are resolved.
3. **Sideband (strict serializability across instances)**: instance A commits row K and passes (K, commit version) to instance B through a Go channel. B's next new txn must see K. This is the workload that catches **stale manifest and cache reuse**, likely the main risk of an S3 engine with local caches. Use check-then-insert so retries are idempotent.
4. **Model-based single-client SQL**: random INSERT/UPDATE/DELETE/SELECT (including range predicates and read-your-writes inside the txn) against an in-memory model (a Go `map` plus a sorted index; or, for SQL semantics, differential against PostgreSQL as research-testing.md §2 describes). Reset or re-sync the model on any ambiguous outcome, as WriteDuringRead does.
5. **Serializability equivalence (metamorphic)**: random txns A, B, C. Run A; B; C from a known state, then (A+B in one txn); C from the same state. Results and final state must be equal. Add SQL query metamorphisms (Turso's "Select-Select-Optimizer" and the SQL Layer's `RandomSemiJoinTestDT` style).
6. **ConflictRange / phantom precision**: two txns at the same snapshot, one with a random `WHERE` range predicate, the other inserting, updating or deleting. If the predicate txn commits, re-running its query at its commit point must give the same rows. If it aborts, check whether the result actually changed and count false aborts (a performance signal). A missed conflict is the correctness failure.
7. **Exactly-once / idempotency**: each logical txn has a client-generated UUID and inserts `(uuid)` plus side effects, retrying through injected ambiguous outcomes. After quiescing, each UUID must appear exactly once (or never, if the call returned a definite failure). This directly tests your `commit_unknown_result` resolution and fencing.
8. **Consistency check after every run**: a bucket fsck, index equal to table, cache equal to bucket, no orphan objects outside the GC grace window, and no referenced object missing.
9. **Restart/upgrade**: as in 4.1 item 9.
10. **Online schema change** (if you support `CREATE INDEX` or `ALTER` on live tables): run Cycle or Bank concurrently with index builds. At the end, the index must equal the base table. Follow the Record Layer's write-only → readable state machine with persisted progress, or the SQL Layer's "every txn reads the schema generation" fence.

### 4.4 Non-Flow projects that approximated FDB-style testing

- **Resonate legacy server (Go)**: tick-driven kernel, seeded rand, pre- and post-processing failure injection in AIO, porcupine linearizability. Described above. https://github.com/resonatehq/resonate-legacy-server [V]
- **WarpStream (Go, via Antithesis)**: the whole SaaS ran under the Antithesis hypervisor as docker-compose (agents, control plane, Kafka clients, a KV store, Postgres, **LocalStack S3**). Faults: network disruption, restarts, thread hangs, latency.
  - Oracles: consumer-side per-partition offset monotonicity, per-producer sequence monotonicity, and records not disappearing in later polls (compaction).
  - Bugs found: a data race in metrics instrumentation that "tens of thousands of hours" of race-detector CI had missed ("caught ... in its first 233 seconds"), and a data-loss bug where a refactor's speculative retry marked a failed flush as successful, so a file that was never persisted could be committed. That second bug is the exact shape of the S3 commit-path bug you need to catch.
  - Source: https://www.warpstream.com/blog/deterministic-simulation-testing-for-our-entire-saas [V]
- **Polar Signals FrostDB (Go)**: WASM/wazero with a modified runtime for "(mostly)" deterministic scheduling. Already in research-testing.md. Their later Rust work settled on state machines plus a single-threaded loop. [V earlier survey]
- **Dropbox Nucleus (Rust)**: nearly all logic runs on one control thread. Tests serialize async work onto it and use a deterministic hasher. Every run has a seed, and "tens of millions of randomized test runs" happen each night.
  - **CanopyCheck** tests the planner with generated trees and QuickCheck-style shrinking. Oracles: termination, no panics, convergence.
  - **Trinity** tests concurrency: it mocks the filesystem, server and timers and reorders or fails intercepted requests. It reruns each seed to verify its own determinism.
  - Admitted gaps: it cannot verify fsync placement (no machine reboot), native-filesystem interleavings, or protocol drift of the server mock. Minimization is weak.
  - Source: https://dropbox.tech/infrastructure/-testing-our-new-sync-engine [V]
- **madsim / RisingWave (Rust)**: a tokio-compatible deterministic runtime, enabled with `--cfg madsim`. It provides drop-in simulated crates, **including `madsim-aws-sdk-s3`**, plus `buggify` (without per-site activation) and `check_determinism`. https://github.com/madsim-rs/madsim [V]
- **sled simulation guide (Rust)**: `receive(msg, at) -> [(msg, dest)]` plus `tick(at)` state machines and a priority-queue discrete-event loop. Check with linearizability-style "is this observed history possible", per-step invariants, and shrink failing request sets. It calls FDB "essentially 'Jepsen-proof'". https://sled.rs/simulation.html [V]
- **TigerBeetle VOPR (Zig)**: see research-testing.md §3. Storage-fault focus: misdirected, phantom and torn writes. [V earlier survey]
- **Turso/Limbo (Rust, embedded SQL — closest in kind to you)**: seeded interaction plans of random SQL plus **properties** (Insert-Select, Select-Select-Optimizer equivalence, TableHasExpectedContent against a shadow model). Flags: `--doublecheck` (run twice and compare), `--differential` (against SQLite), and a "bug base" of saved failing plans. https://github.com/tursodatabase/turso/tree/main/testing/simulator [V README]
- **Clever Cloud Materia (Rust inside FDB's simulator)**: `foundationdb-simulation` external workloads. The first seed found `commit_unknown_result` double increments. Determinism rules: BTreeMap, the context RNG and clock, `db.run()` retries, no `tokio::spawn`. https://pierrezemb.fr/posts/writing-rust-fdb-workloads-that-find-bugs/ [V]
- **Antithesis Go SDK**: `assert.Always/Sometimes/Reachable/Unreachable`, `random.GetRandom`, `lifecycle.SetupComplete`, with a `no_antithesis_sdk` no-op build tag. A ready-made probe/sometimes API, even if you never buy Antithesis. https://github.com/antithesishq/antithesis-sdk-go [V]

---

## Searched for but not found

- Kyle Kingsbury's own words on skipping Jepsen for FDB. Only Will Wilson's account and other secondhand copies exist.
- A transcript of Wilson's Strange Loop 2014 talk. I used third-party notes (alex-ii) and the moonpool history page. The "Sinkhole found no FDB bugs" claim rests on those.
- Alex Miller posts on FDB testing beyond BUGGIFY. The transactional.blog simulation series lists only the index page and BUGGIFY.
- Any public retrospective by the FDB SQL Layer team on what was hard. The original repository returns 404 and the foundationdb.com layer docs are gone, so I inferred design from code only.
- A public list of FDB correctness bugs that escaped simulation into production. The paper alludes to "several bugs" from weaker-than-believed OS contracts without listing them.
- Whether FDB's internal (Apple) Joshua runs gate merges on CODE_PROBE coverage. In the open-source harness it is report-only.
- Conditional-write (CAS) support in FDB's MockS3: none.

## Main URLs

- Paper: https://www.foundationdb.org/files/fdb-paper.pdf (SIGMOD 2021; §2.2 limits, §2.4 txns, §4 simulation, §6 lessons)
- Docs: https://apple.github.io/foundationdb/testing.html, https://apple.github.io/foundationdb/engineering.html, https://apple.github.io/foundationdb/developer-guide.html, https://apple.github.io/foundationdb/known-limitations.html, https://apple.github.io/foundationdb/automatic-idempotency.html
- Code: https://github.com/apple/foundationdb (paths cited inline; `flow/include/flow/Buggify.h`, `flow/include/flow/CodeProbe.h`, `fdbrpc/sim2.cpp`, `fdbrpc/include/fdbrpc/AsyncFileNonDurable.h`, `fdbserver/workloads/*.cpp`, `fdbserver/tester/WorkloadUtils.cpp`, `contrib/TestHarness2/`, `contrib/debug_determinism/README.md`, `design/idempotency_ids.md`, `design/metadata-version.md`, `design/tuple.md`, `design/mocks3server_chaos_design.md`, `AGENTS.md`)
- Joshua: https://github.com/FoundationDB/fdb-joshua
- FDB wiki: https://github.com/apple/foundationdb/wiki/Brainstorming:-Making-FoundationDB-more-resilient-against-bugs
- BUGGIFY: https://transactional.blog/simulation/buggify
- Wilson / Antithesis: https://antithesis.com/blog/is_something_bugging_you/, https://antithesis.com/resources/deterministic_simulation_testing/, https://antithesis.com/docs/best_practices/sometimes_assertions/
- Talk: https://thestrangeloop.com/2014/testing-distributed-systems-w-slash-deterministic-simulation.html; notes https://alex-ii.github.io/notes/2018/04/29/distributed_systems_with_deterministic_simulation.html
- Record Layer: https://arxiv.org/abs/1901.04452, https://github.com/FoundationDB/fdb-record-layer (docs/sphinx/source/IndexBuilding.md, Building.md, SQL_Getting_Started.md; yaml-tests/)
- SQL Layer mirror: https://github.com/jaytaylor/sql-layer
- Others: https://github.com/resonatehq/resonate-legacy-server, https://www.warpstream.com/blog/deterministic-simulation-testing-for-our-entire-saas, https://dropbox.tech/infrastructure/-testing-our-new-sync-engine, https://github.com/madsim-rs/madsim, https://sled.rs/simulation.html, https://github.com/tursodatabase/turso/tree/main/testing/simulator, https://pierrezemb.fr/posts/writing-rust-fdb-workloads-that-find-bugs/, https://pierrez.github.io/moonpool/part1-why/04-history.html, https://github.com/pingcap/failpoint, https://github.com/antithesishq/antithesis-sdk-go
