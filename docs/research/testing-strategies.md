# Testing strategy research: embedded Go SQL engine on S3/Tigris

Research date: 2026-11-10. Local toolchain: `go1.27.3 darwin/arm64`, `uv` installed, Python 3.9 (system).

Legend: **[V]** = verified from a primary source this session (URL given). **[I]** = my inference or experience; check before relying on it.

---

## 1. Hegel for Go

### Facts
- **Module path:** `hegel.dev/go/hegel`. Install with `go get hegel.dev/go/hegel@latest`. [V] https://pkg.go.dev/hegel.dev/go/hegel, https://hegel.dev
- **Where it lives:** On 2026-11-03 (v0.9.29), the Go frontend moved into the monorepo at `github.com/hegeldev/hegel/frontends/go`. The old `github.com/hegeldev/hegel-go` repo is archived and read-only. The module path did not change. [V] https://raw.githubusercontent.com/hegeldev/hegel/main/frontends/go/CHANGELOG.md, https://github.com/hegeldev/hegel-go
- **Version and cadence:** v0.9.31 was released 2026-11-09. There have been about 30 patch releases since early autumn 2026, several per week. [V] CHANGELOG above.
- **Stability:** The project is in beta. Under its versioning, `0.N.0` releases may break the API ("SemVer shifted right by one decimal place"). The project expects a stable 1.0 "in a few months". [V] https://hegel.dev/compatibility, frontends/go/README.md
- **Architecture and runtime dependency:** The Go library calls **libhegel**, a native Rust engine with a C ABI, through FFI. It uses `github.com/ebitengine/purego` (dlopen without cgo), not cgo. Tagged releases **embed prebuilt libhegel shared libraries**, one per platform (`internal/libhegel/embed_*.go`). At run time the library is extracted to a per-user cache. Since 0.9.12 it falls back to a secure temp cache when sandboxes block writes elsewhere. `HEGEL_LIBHEGEL_PATH` overrides the library location. Untagged builds, such as a `replace` to a local checkout, embed nothing and need `cargo build --release -p hegeltest-c`. [V] go.mod and justfile at https://github.com/hegeldev/hegel/tree/main/frontends/go, and CHANGELOG 0.9.12 and 0.9.29.
  - **No Python or uv dependency in Go.** The old Hegel design used a Python `hegel-core` server through uv, and the installed skill text still hints at that. The current Go library does not use it. [V] go.mod has no such dependency; the README lists none.
  - `go.mod` requires Go **1.26.1+**. It pins purego to an **alpha pseudo-version** (`v0.12.0-alpha.1.0.20261015…`). The `tool` deps (staticcheck, gomutants, go-test-coverage) and `lib/pq` (used by the examples) appear in its require list. Module graph pruning should keep most of them out of your build, but they will show up in `go.sum`. [V] go.mod. The effect on your module graph is [I].
- **Platforms:** Linux amd64/arm64, macOS arm64, and Windows amd64/arm64. There is **no Intel macOS** binary. Musl/Alpine support is not documented. [V] README and compatibility page. The musl point is [I].

### How tests are written [V]
```go
func TestX(t *testing.T) {
    hegel.Test(t, func(ht *hegel.T) {
        xs := hegel.Draw(ht, hegel.Lists(hegel.Integers(math.MinInt, math.MaxInt)))
        ...
        ht.Fatalf(...)
    }, hegel.WithTestCases(5000))
}
```
- Generators: `Integers`, `Floats`, `Text`, `Binary`, `Booleans`, `SampledFrom`, `Lists`, `UniqueLists`, `Maps`, `Optional`, `OneOf`, `Recursive`, `FromRegex`, `Dates`, `Datetimes`, `UUIDs`, `Default[T]()` (derives a generator from a Go type), and combinators `Map`, `Filter`, `FlatMap`, `Composite`. The test-case context offers `Assume`, `Note`, `Event`, `Target` (targeted PBT), and `Fail`.
- Settings: `WithTestCases` (default 100), `WithSeed`, `WithDerandomize`, `WithDatabase` (example database; an empty path disables it), `WithPhases`, `WithProfile`, `WithBackend`, `WithReproductionBlob` (base64 blob for replay), `WithReportMultipleFailures`, `SuppressHealthCheck(FilterTooMuch|TooSlow|…)`.
- `go test -short` lowers case counts automatically (since 0.9.19).
- `Run`/`MustRun` run properties outside `go test`. `hegel.Workload(fn)` builds a standalone CLI binary that runs without a time limit and takes flags such as `--test-cases`. It is meant for soak runs and Antithesis. `BackendURandom` is documented as "intended for running under Antithesis".

### Stateful / model-based testing [V]
- `hegel.RunStateful(tc, machine, opts...)`. Methods named `Rule*` with the signature `func(hegel.TestCase)` become rules, and `Invariant*` methods become invariants. There is no separate precondition API; a rule calls `tc.Assume(false)` to skip itself. `Pool` (`NewPool`, `Add`, `ValuesConsumed`, `ValuesReusable`) shares generated values, such as created keys or open transactions, between rules. It plays the role of Hypothesis "Bundles".
- **Concurrency:** `WithConcurrency()` (up to GOMAXPROCS workers) or `WithBoundedConcurrency(n)` runs rules on concurrent goroutines. `WithRuleGroup(name, rules...)` controls which rules may overlap. Invariants run only between rounds. As of 0.9.31, a long run redraws its concurrency level and enabled rules between rounds. `WithStatefulStepCount` sets the number of rounds (default 50). `WithAlwaysCheckInvariants` checks invariants at every join point instead of sampling them.
- Example workloads ship in the repo. `examples/bank` and `examples/ring` run concurrent SERIALIZABLE transactions over `database/sql` against PostgreSQL-compatible stores and check atomicity, isolation, and durability invariants. **These map closely onto your engine if you expose a `database/sql` driver.** https://github.com/hegeldev/hegel/tree/main/frontends/go/examples

### Shrinking
- Shrinking and the example database come from the engine, which is Hypothesis-style and shrinks the underlying choice sequence. The README shows a minimal counterexample (`[]int{0, 0}`). [V]
- How well shrinking works under `WithConcurrency` is **not documented**. Real goroutine interleavings are not replayed deterministically, so concurrent failures may be flaky to shrink and replay. Treat concurrent stateful runs as bug *finders* and sequential runs as reproducers. [I]

### CI implications
- No toolchain beyond Go is needed with tagged releases. The extracted `.so`/`.dylib` must be writable to a cache directory and loadable; that rules out `noexec` temp mounts. [V for the cache, I for noexec]
- The example database is **disabled automatically in CI**. Failures there do not persist across runs, so log `WithReproductionBlob` output and copy regressions into explicit unit tests. [V]
- Hegel's own CI runs `go test -race` with the library, so `-race` works. [V] justfile `check-test`.
- Pin an exact version. The API is still moving week to week. [I]
- Fallback if FFI is unacceptable: `pgregory.net/rapid` (github.com/flyingmutant/rapid). It is pure Go, MPL-2.0, active (last push 2026-09), and has state-machine support. [V repo status]

---

## 2. Differential testing against PostgreSQL

### The core idea for this project [I]
The dialect is a strict subset of PostgreSQL, so **Postgres is a near-perfect oracle**. Two properties follow:
1. **Acceptance subset.** If your engine accepts a statement, Postgres must accept it with the same schema. If Postgres rejects it, your engine must reject it, ideally with the same SQLSTATE class.
2. **Result equivalence.** For accepted statements, results must match Postgres: values, types, NULL handling, error vs. no error, and rows affected. Compare as a multiset unless the query has `ORDER BY`, in which case compare in order, but only up to ties.

PostgreSQL is mature, and SQLancer found few logic bugs in it compared with SQLite, MySQL, or TiDB. A mismatch is therefore much more likely your bug. [I, consistent with SQLancer literature]

Normalize these before comparing [I]:
- Pin the PG major version.
- Use `C` collation and a UTC `TimeZone`. Fix `DateStyle`, `IntervalStyle`, and `extra_float_digits`.
- Compare floats with care, or keep floats out of generated queries at first.
- PG sorts NULLs last for ASC by default.
- Compare `numeric` scale.
- Integer overflow and division by zero must raise errors.
- Compare SQLSTATE class, not message text.
- Avoid functions with volatile results (`now()`, `random()`).
- Wrap each case in a transaction or a fresh schema.

Run PG with `testcontainers-go` (MIT, active) or `fergusstrange/embedded-postgres` (MIT, active, downloads PG binaries, no Docker). Talk to it with `jackc/pgx` (MIT). [V repo status via GitHub API]

### Generation-side tools
- **SQLancer** (MIT, Java, very active, last push 2026-11-05). It needs DBMS-specific Java code and connects over JDBC. Oracles [V] https://github.com/sqlancer/sqlancer:
  - **PQS** (Pivoted Query Synthesis): picks a random row and builds a query that must return it. Finds WHERE/JOIN logic bugs. **Marked unmaintained.**
  - **NoREC**: compares an optimized query with an unoptimizable rewrite (`SELECT COUNT(*) WHERE p` vs `SELECT SUM(CASE WHEN p …)`). Finds **optimizer and index-path** bugs.
  - **TLP** (Ternary Logic Partitioning): `Q == Q WHERE p ∪ Q WHERE NOT p ∪ Q WHERE p IS NULL`. Variants exist for WHERE, GROUP BY, HAVING, DISTINCT, and aggregates. Finds three-valued-logic, NULL, and aggregate bugs.
  - **DQE** (SELECT/UPDATE/DELETE agree on the same predicate; MySQL only), **DQP** (force different plans), **CERT** (cardinality-estimation performance bugs), **CODDTest** (constant folding and propagation, including subqueries). **QPG** is plan-coverage-guided generation. Implicit oracles cover crashes and internal errors.
  - **To use it on an embedded Go library** you would need a PG wire-protocol shim, for example `jeroenrinzema/psql-wire` (Apache-2.0, active) or the pgx `pgproto3` package. SQLancer's PostgreSQL generator uses far more features than your subset, so expect most statements to be rejected; you would have to cut the generator down or add expected errors. [I]
- **SQLancer++** (Zhong & Rigger, arXiv 2503.21424, v2 Jan 2026) uses one generic generator that *learns which features the DBMS accepts* through Bayesian feature-support estimation. It reported 195 new bugs across 17 DBMSs, and "a new user can often apply it by changing only a connection parameter". It fits a "strict subset" engine well. I found no repo link on the abstract page; check the paper PDF. [V] https://arxiv.org/abs/2503.21424
- **SQLsmith** (anse1/sqlsmith, C++, **GPL-3.0**, active) generates random complex queries from the live schema. It finds crashes, panics, and internal errors, not wrong results; Cockroach's blog notes it "is not able to deduce correctness". [V] https://www.cockroachlabs.com/blog/sqlsmith-randomized-sql-testing/
- **CockroachDB** ported SQLsmith to Go (`pkg/internal/sqlsmith`) and found 40+ panics and races. It also runs the roachtests `unoptimized-query-oracle` (same query with optimizations on and off, compared) and `costfuzz` (perturbed costs) on a schedule. The Go code is under the **CockroachDB Software License** since Nov 2024; it is source-available, not open source, so read it but do not copy it. [V] blog above, https://www.cockroachlabs.com/enterprise-license-update/
- **DuckDB** runs an extended sqllogictest suite in CI with **vector size 2** to stress edge cases. Its "Fuzzer of Ducks" runs SQLancer and SQLsmith on a schedule, reduces failures, files GitHub issues automatically, and **does not fail CI**. [V] https://duckdb.org/docs/current/dev/sqllogictest/overview.html, https://dbtest-workshop.github.io/2022/slides/DuckDB%20Testing%20-%20Present%20and%20Future.pdf
  - Lesson: run engine tests with tiny page, row-group, cache, and object-size limits so that paging, splitting, and compaction run on every test. [I]

### What is practical to build in Go [I]
1. **Generator plus PG oracle in-process.** Write a typed, schema-aware SQL generator *for your subset only*, as Hegel generators so you get shrinking. Run each case against your engine and against PG over pgx. This is the single highest-value harness. Shrinking a SQL mismatch with Hegel gives minimal repros almost for free.
2. **Add TLP and NoREC as metamorphic oracles that don't need PG.** They also run fast in unit tests and catch planner bugs even where PG behaviour is ambiguous.
3. **Parser acceptance differential.** `github.com/pganalyze/pg_query_go/v6` (BSD-3, v6.2.5, cgo, embeds the real PG parser; first build takes about 3 minutes). Check: anything your parser accepts must parse in pg_query, and the normalized ASTs or fingerprints should agree. Fuzz this. [V] https://github.com/pganalyze/pg_query_go

### sqllogictest format and corpora
- **Format:** `statement ok|error`, `query <types> [nosort|rowsort|valuesort]`, `----` results, `hash-threshold`, `skipif/onlyif <engine>`. Its key feature is **"completion mode"**: write prototype scripts and fill in expected results from a *reference engine*. That is exactly the PG-oracle workflow, frozen into regression files. [V] https://raw.githubusercontent.com/gregrahn/sqllogictest/master/about.wiki
- **SQLite's corpus:** about 5.9M queries according to Dolt. The code says "you can redistribute it and/or modify the code as you see fit... GPL, BSD, MIT or CC0". Its results came from SQLite and MySQL, not PG, and it uses SQLite/MySQL semantics such as integer division and type affinity, so much of it will not match PG without regeneration. Its value here is low-to-medium: regenerate results with PG and keep only files that fit your subset. [V] sqllogictest.c header, https://github.com/dolthub/sqllogictest
- **DuckDB corpus** (`test/sql/**/*.test`): MIT, large, with extended syntax (`statement error` with message, `loop`, `require`). The dialect is PG-*like* but differs, so filter it and regenerate expected output against PG. [V for MIT and extensions; I for usability]
- **CockroachDB logictests** (`pkg/sql/logictest/testdata`): PG-flavoured but full of CRDB-specific output and features. Current files are under the **CockroachDB Software License**. Older BSL releases convert to Apache-2.0 after their change date; per Oxide RFD 508, 22.1.9 converted in May 2025. Check the license of the exact tag before vendoring. [V licence history; I re: test-file applicability]
- **PostgreSQL's own regression suite** (`src/test/regress/sql/*.sql` + `expected/*.out`): PostgreSQL License (permissive). It is the most dialect-accurate corpus, but it is not in slt format. Conversion is worth doing for the subset. [I; licence well known]
- **Go runners:** `github.com/dolthub/sqllogictest/go/logictest` has a parser, a harness interface, and a runner, and ignores `hash-threshold`. It is small (26 stars), last pushed 2026-06, and has no license in the GitHub metadata, so check before vendoring. `github.com/rnestertsov/slt` (MIT) is tiny and new. Writing your own parser and runner is about 300 lines and lets you add PG completion mode. [V repo metadata] Rust option: `risinglightdb/sqllogictest-rs` (Apache-2.0).

---

## 3. Deterministic simulation testing (DST) in Go

### `testing/synctest` [V via local `go doc`, Go 1.27.3]
- It has been GA since Go 1.25 (`synctest.Test`, `synctest.Wait`). Go 1.27 added `synctest.Sleep` (time.Sleep + Wait), and `httptest.NewTestServer` provides an **in-memory fake network** that works inside a bubble. https://go.dev/doc/go1.27
- **What it gives you:** a fake clock per bubble that advances only when every goroutine is durably blocked. A test can therefore cover hours of timeouts, retries, lease expiry, and backoff in milliseconds, with deterministic time.
- **What it does not give you:**
  - **Goroutine scheduling order is not controlled**; interleavings stay nondeterministic.
  - `sync.Mutex` and `RWMutex` waits, network I/O, and syscalls are **not** durably blocking. If a goroutine sleeps while holding a mutex that another goroutine waits on, the bubble never goes idle. [I from the docs' definition: expect hangs]
  - Real sockets break idleness. Use `NewTestServer` or `net.Pipe`.
- **Other sources of nondeterminism:** `testing/cryptotest.SetGlobalRandom(t, seed)` makes `crypto/rand` deterministic, but not in parallel tests. That matters because SDK retry jitter and UUIDs often use crypto/rand. `math/rand/v2` top-level functions cannot be seeded, so inject a `*rand.Rand`. Map iteration and `select` among several ready cases are random. [V cryptotest doc; I for the rest]

### How far Go can approximate FoundationDB and TigerBeetle
- **FoundationDB:** the whole cluster runs single-threaded on simulated network, disk, and time. `BUGGIFY` call sites, only active in simulation, are each enabled or disabled per run and then fire with about 25% probability, biasing runs toward dangerous paths. https://transactional.blog/simulation/buggify
- **TigerBeetle VOPR:** seeded simulator with fault injection (drop or reorder packets, partitions, disk read/write corruption), simulated time, and exact replay by seed. "3.3 seconds of VOPR simulation gives you 39 minutes of real-world testing time." https://docs.tigerbeetle.com/about/vopr. See also the 2026 post on protocol-aware DST: https://tigerbeetle.com/blog/2026-08-20-protocol-aware-dst
- **Go options:**
  1. **synctest + injected interfaces (recommended).** Put an `ObjectStore` interface behind the engine, with a seeded fake implementation that injects faults, plus an injected clock and seeded RNG. Time becomes deterministic; scheduling does not. Reproducibility is "mostly". Cheap, idiomatic, no toolchain games. [I]
  2. **Make the core sans-I/O.** Write the commit, manifest, and compaction logic as a pure state machine driven by events: "PUT returned 412", "timer fired". A single-goroutine simulator loop can then drive it with full determinism and full replay. This is the TigerBeetle and FoundationDB discipline applied only where it matters. [I] Polar Signals reached the same conclusion for their Rust rewrite (state machines plus a single-threaded event loop). https://www.polarsignals.com/blog/posts/2025/07/08/dst-rust
  3. **WASM single-thread:** Polar Signals ran FrostDB compiled to WASM (wazero) with a modified Go runtime to get "(mostly)" deterministic scheduling. It works, but it is fragile and needs a runtime fork. https://www.polarsignals.com/blog/posts/2024/05/28/mostly-dst-in-go
  4. **gosim** (source translation of Go into a simulated runtime with machines, network, fs, time, and crashes):
     - Upstream `jellevandenhooff/gosim` is "experimental", last pushed 2024-12. https://github.com/jellevandenhooff/gosim
     - Fork `neria-cloud/gosim` v0.9.2 (2026-10-26) supports Go 1.25/1.26 only (prerelease compilers unsupported), needs `-ldflags=-checklinkname=0 -tags=linkname`, and **aborts on TLS**. It does not claim support for unmodified HTTP stacks. It has 4 stars. **Immature.** https://pkg.go.dev/github.com/neria-cloud/gosim
     - Not recommended as a foundation; possibly worth a spike later.
- **Simulated object store with fault injection: what to model** [I, grounded in AWS semantics below]
  - Latency: per-op distributions with tail spikes.
  - Throttling: 503 SlowDown and 500 InternalError.
  - Connection reset **before** the request is applied.
  - **Request applied, response lost** (timeout or reset after commit). This is the ambiguous outcome, and it is the most important fault for conditional-write protocols: the client must resolve "did my CAS win?" by re-reading.
  - **Delayed apply** ("zombie" write): a PUT that lands after the client gave up and another writer moved on.
  - 409 ConditionalRequestConflict under concurrency, which AWS documents.
  - Stale reads and LISTs if you model Tigris Global or Dual-region buckets (§5).
  - Partial multipart: parts uploaded, Complete fails or is ambiguous.
  - Process crash at every I/O point: count the I/O ops in a run, then re-run the same seed and crash at op k for every k (or a random sample). After each crash, reopen from the bucket and run invariant checks. This is the object-store analogue of SQLite's I/O-error and crash loops (https://sqlite.org/testing.html).
  - S3 PUTs are atomic per object, so torn objects can't happen, but multi-object protocols can stop half-way at any point.
- **Wiring:** run the real `aws-sdk-go-v2` client against an in-process HTTP S3 fake (gofakes3 handler or your own) with a fault-injecting `http.RoundTripper` or handler middleware, served by `httptest.NewTestServer` inside `synctest.Test`. This tests your SDK configuration, retries, and error classification too. Alternatively, inject below the SDK at your own `ObjectStore` interface for speed. Do both. [I]

---

## 4. Transactional correctness and isolation checking

- **Elle** (jepsen-io/elle, Clojure, EPL-2.0, active 2026-10): infers G0, G1a/b/c, G-single, G2, and lost-update anomalies from black-box histories. Use **list-append** for the strongest inference; rw-register is weaker. https://github.com/jepsen-io/elle, paper https://arxiv.org/abs/2003.10554
- **elle-cli** (ligurio, EPL, 35 stars, active 2026-10, v0.1.11): standalone jar that takes EDN or JSON histories. **Requires Java 21+.** Models: list-append, rw-register, bank, counter, set, long-fork, and Knossos cas-register. JSON→EDN conversion can fail; the README suggests `jet`. Fine for CI from Go: write the history as JSON, exec `java -jar`, parse true/false/unknown. [V] https://github.com/ligurio/elle-cli
- **adya** (Rust, `cargo install adya`, v0.1.0 2026-10-06): a JVM-free Elle reimplementation that reads elle-cli JSON. It claims to match Elle on all 34 of 40 differential histories that Elle finished, and to be much faster. It is **one author, 1 star, brand new**, so cross-check with elle-cli if you use it. [V] https://docs.rs/crate/adya/latest
- **Go ports of Elle:** only PingCAP `tipocket/pkg/elle/list_append`, which is partial and old. No credible maintained Go port. [V search]
- **Porcupine** (anishathalye/porcupine, MIT, 1.3k stars, active 2026-09): Go linearizability checker for *single-object / key-partitioned* histories, with HTML visualization. etcd robustness tests use it. Use it for things that should be linearizable per key, such as point reads and writes under strict serializability, or your manifest pointer CAS. It does not do multi-key transactional isolation; that is Elle's job. On ARM, record timestamps carefully (atomics or barriers). [V] https://github.com/anishathalye/porcupine
- **Hermitage** (ept/hermitage, CC BY 4.0): manual two- and three-session scripts for G0, G1a, G1b, G1c, OTV, PMP, P4, G-single, G2-item, and G2, with PG results per level. PG READ COMMITTED prevents up to OTV; REPEATABLE READ (snapshot isolation) also prevents PMP, P4, and G-single; SERIALIZABLE prevents everything. [V] https://github.com/ept/hermitage
  - Port each script to a Go table test with goroutine "sessions" and explicit barriers. Assert the anomaly is prevented at the isolation level you claim. Run the same scripts against PG as a sanity check. Do not require identical abort sets, since PG's SSI aborts conservatively. [I]
- **Multiple processes sharing one bucket** [I]:
  - Go's monotonic clock reading is per process, so `time.Now()` is not comparable across processes for real-time (strict serializability) edges. Use a **single coordinator process** that drives N worker processes, each opening the DB on the same bucket, over pipes or RPC. The coordinator records invoke and complete times. That gives a single Elle/Porcupine history with valid real-time order.
  - Use list-append over a few hot keys to force conflicts.
  - Inject process kills (SIGKILL) and record killed ops as `:info` (indeterminate).
  - Run against both the fault-injecting fake and real Tigris.
  - Hegel's `examples/ring` and `examples/bank` are ready-made invariant workloads of the same kind (bank: conservation of totals; ring: graph topology plus acknowledged-swap counters) and need only a `database/sql` driver.

---

## 5. Crash consistency and fault injection for object-store systems

### What others do
- **SlateDB** (Rust, Apache-2.0): has a dedicated DST crate (`slatedb-dst`) with actors, a clocked object store, and a fault-injecting object store, reportedly run hourly in CI. Its `clippy.toml` bans direct `thread_rng` and `tokio::time::sleep` so DST stays deterministic. Tests use `fail_parallel` failpoints. [V for clippy.toml and tests via docs.rs; the DST crate detail comes from a third-party listing] https://docs.rs/crate/slatedb/0.13.0/source/clippy.toml, https://github.com/slatedb/slatedb
  - Copy the lint idea with a `forbidigo` golangci-lint rule that bans `time.Now`, `time.Sleep`, `math/rand`, and `crypto/rand` outside the clock and RNG packages. [I]
- **WarpStream** (Go): runs the whole SaaS in **Antithesis**, with LocalStack as the S3 store, network faults, and thread pauses. It found a data race in a metrics library that the race detector never caught, and a **data-loss race in the flush path**: a speculative-retry refactor briefly marked a failed file as successful, and a 5 ms poller committed it. Six wall-clock hours covered about 280 hours of application time. [V] https://www.warpstream.com/blog/deterministic-simulation-testing-for-our-entire-saas
- **turbopuffer:** no public testing write-up found. A secondhand interview summary says they test real code paths, not mocks, and simulate partitions, drops, and timeouts. [V search; weak source] **Chroma wal3** describes a WAL on object storage that keeps an ongoing correctness proof (worth reading for the offline invariant checker idea). https://trychroma.com/engineering/wal3
- **Tigris itself** ran in Antithesis from Jul 2025 to Mar 2026: 261 runs, about 54.7k virtual hours. Almost all findings were **cache coherence** bugs: GET after DELETE returned the deleted object; deletes resurfaced under partial regional failure; a rename returned an error although it had committed. All were fixed. Cross-region replication testing was "just starting" as of Apr 2026. Conditional writes were not reported as tested either way. [V] https://antithesis.com/blog/2026/tigris_report/
  - Implication: **test read-after-delete, ambiguous errors, and region placement yourself** against real Tigris.

### Tigris consistency facts relevant to the design and tests [V]
- Strong consistency (read-after-write, read-after-delete, list-after-write) within a region. Conditional ops "evaluate against the latest state".
- **Global and Dual-region buckets are eventually consistent across regions.** For cross-region conditional ops, use Single-region or Multi-region buckets. https://www.tigrisdata.com/docs/concepts/consistency/, https://www.tigrisdata.com/docs/objects/conditionals/
- `X-Tigris-Consistent: true` routes reads and conditional ops to the leader. It replaces a deprecated CAS header. (This comes from Tigris search snippets and a fly.io forum post; the fetched conditionals page did not show it, so verify it.) https://community.fly.io/t/tigris-consistency-questions/24046/5
- Supported conditionals include `If-Match`, `If-None-Match: "*"`, If-(Un)Modified-Since, and copy-source variants. Failure returns 412.
- **Tests to write:** run the integration suite against a Single-region bucket *and* deliberately against a Global bucket from two regions, to prove the engine either refuses or survives the weaker mode.

### AWS S3 semantics to model in fakes [V] https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html
- `If-None-Match: *` and `If-Match: <etag>` on PutObject, CompleteMultipartUpload, and CopyObject.
- 412 on failure. "First write to finish succeeds."
- **409 Conflict** when concurrent with a delete. PutObject may be retried; for MPU you must start a new multipart upload.
- `If-Match` returns **404** when the key was deleted concurrently.
- Conditional writes ignore in-progress MPUs.

### Fakes and proxies: fidelity
| Tool | Status | Conditional writes | Notes |
|---|---|---|---|
| **gofakes3** (johannesboyne, MIT, active 2026-10) | in-process Go, `http.Handler` | PutObject `If-Match` / `If-None-Match: *` checked atomically under the backend lock in `s3mem`. **Not** applied to CompleteMultipartUpload or DeleteObject. No 409. ETag = MD5. [V source read: backend.go `CheckPutConditions`, s3mem `PutObject`] | Best base for an in-process fake. Wrap it with your own fault middleware. Its Backend API is "not yet stable". |
| **Your own in-memory fake** | — | exactly what you model | About 500 lines for Put, Get, Head, List, Delete, and MPU, with the conditionals and fault hooks above. Validate it by running the *same* contract tests against gofakes3, real Tigris, and (optionally) AWS. [I] |
| **Adobe S3Mock** (Java/Docker, Apache-2.0, very active) | container | handles if-match/if-none-match (4.6.0 fixes PUT-with-match on missing keys; 4.10.0 accepts unquoted ETags) [V release notes via search] | Needs a JVM or Docker. Out-of-process. |
| **MinIO** | **CE archived** (maintenance mode Dec 2025, repo archived 2026; no binaries or images since Oct 2025) | supported If-Match and If-None-Match | **Avoid for new work.** [V] https://github.com/minio/minio, https://infoq.com/news/2025/12/minio-s3-api-alternatives/ |
| **Garage** (AGPL-3.0, active) / **SeaweedFS** | container | Garage: unverified. SeaweedFS has an "S3 Conditional Operations" wiki page. | Useful as the S3 container inside Antithesis, which has no internet access. Verify conditional support with your contract tests. [I] |
| **LocalStack** | single image **requires an auth token** since 2026-03-23 | — | Awkward for CI now. [V] https://blog.localstack.cloud/localstack-single-image-next-steps/ |
| **Toxiproxy** (Shopify, MIT, active) | TCP proxy | n/a | Latency, bandwidth, `timeout`, `reset_peer`, `limit_data`, slicer. TCP level only, so it can't return HTTP 503 or 412. Downstream resets after the request was forwarded do produce "applied but response lost". TLS endpoints (Tigris) complicate it because of host and cert mismatch. Prefer an in-process `RoundTripper` fault injector. [I] |

### Offline invariant checker ("fsck for the bucket") [I], high value
Write `cmd/<db>-fsck` and a library function. It should:
- list the bucket and parse every manifest and segment;
- verify checksums, the reference graph (no dangling references), and monotonic version chains;
- check that every acknowledged commit is reachable, that unreferenced objects are only garbage that is safe to delete, and that the schema catalog is consistent.

Call it in `t.Cleanup` of **every** integration, DST, and crash test. It turns silent corruption into an immediate failure and works the same on the fake and on Tigris.

---

## 6. Antithesis

- **Pricing (now public, self-serve):** Teams (Beta) costs **$2,999/month** for 2,400 compute hours, with extra hours at $2.50 and no rollover. It is month-to-month and has a "Sign up" button. Enterprise is custom. The "Open source" tier is just Hegel and Bombadil, not the platform. No free trial is mentioned. [V] https://antithesis.com/pricing. An AWS startup perk gives a 10% first-year discount. [V via search: https://aws.amazon.com/startups/...]
- **What you'd have to build:** Linux **x86-64** container images, plus a docker-compose (or k8s) file that includes every dependency. **No internet access**, so no Tigris; ship an S3-compatible container (S3Mock, Garage, SeaweedFS, or a server built on gofakes3). Add a workload, or "test template", that drives the library; for a library that means a workload binary. `hegel.Workload` plus `BackendURandom` is designed for this. Build with the Go instrumentor (`go get -tool github.com/antithesishq/antithesis-sdk-go/tools/antithesis-go-instrumentor`, or the `antithesis-go-toolexec` GOFLAGS route) for coverage and assertion cataloguing, and ship the symbols file in the config image. [V] https://antithesis.com/docs/getting_started.md, https://antithesis.com/docs/reference/sdk/go/instrumentor.md
- **Realistic for an independent developer?** It is *accessible* now (self-serve) but costs **about $36k/year** at minimum. It is worth it only after the cheap layers (PG differential, in-process fault fake, Elle) stop finding bugs; a burst month near a release is a sensible way to try it. [I]
- **Go SDK** (`github.com/antithesishq/antithesis-sdk-go`, MIT, v0.8.0 2026-08-28, pre-v1). `assert.Always`, `AlwaysOrUnreachable`, `Sometimes`, `Reachable`, `Unreachable`, numeric `AlwaysGreaterThan`…, `AlwaysSome`, and `SometimesAll`. Messages must be string literals. **Outside Antithesis they are no-ops**, unless `ANTITHESIS_SDK_LOCAL_OUTPUT=<file>` is set, in which case they write JSON lines. [V] https://pkg.go.dev/github.com/antithesishq/antithesis-sdk-go/assert
  - **Value outside the platform:** modest. `Sometimes` / `Reachable` work as **fault-path coverage assertions**. With LOCAL_OUTPUT on in CI, a script can fail the build if "commit lost CAS race and retried" or "recovered from ambiguous PUT" never fired in a soak run. You can do the same with your own counters, or with Hegel's `Event`, at zero dependency. Adopting it early mainly costs little and eases a later move to Antithesis. [I]

---

## 7. Go native fuzzing

### Best targets for this engine [I]
1. **Parser:** `FuzzParse(sql string)`. Properties: no panic or hang; parse → format → parse is a fixpoint (AST equality); **accepted ⇒ pg_query accepts** (differential). Seed it with the PG regression `.sql` files and your slt corpus.
2. **On-disk / on-bucket binary decoders:** page, segment, manifest, WAL record, and varint/row encodings. Properties: `Decode(arbitrary)` never panics, OOMs, or loops; it returns an error or a value, and if it returns a value, `Encode(Decode(b))` round-trips. **Torn or flipped bytes must be detected by the checksum** (fuzz with a bit-flip on valid encodings). This is the classic high-yield target.
3. **Key and value encodings with ordering:** order-preserving encodings must satisfy `cmp(enc(a), enc(b)) == cmp(a, b)`. This suits Hegel even better, but fuzz it too.
4. **Expression evaluator:** fuzz bytes → a small expression AST via a tiny decoder; compare against PG, or against a reference interpreter, for integer overflow, NUMERIC, casts, LIKE, and NULL semantics.
5. **Recovery:** fuzz an arbitrary *set of objects* in a fake bucket → `Open()` must either succeed with consistent state (fsck passes) or fail cleanly. This catches recovery code that trusts bucket contents.

### Mechanics and limitations [V/I]
- Corpus lives in `testdata/fuzz/FuzzX/` (commit crashers as regression seeds; plain `go test` runs them). The generated corpus lives in `$GOCACHE/fuzz`; cache it in CI between runs.
- **One fuzz target per `go test -fuzz` invocation**, per package; script a loop with `-fuzztime`. [V via third-party; long-standing]
- Arguments are limited to primitives, `string`, and `[]byte`. There are no structs, no dictionaries or CmpLog, no grammar mode, and no built-in coverage report from a campaign. Nothing new for fuzzing appeared in the Go 1.26 or 1.27 notes. [V for 1.27 notes]
- Workarounds:
  - decode structs from bytes yourself, or use `AdaLogics/go-fuzz-headers` (archived-ish, last push 2024);
  - **Trail of Bits `gosentry`** (BSD-3, May 2026) is a *forked Go toolchain* with LibAFL, struct fuzzing, Nautilus grammar fuzzing (a SQL grammar is a good fit), and race and leak detectors. It is powerful but means a second toolchain, and it is young. [V] https://blog.trailofbits.com/2026/05/12/go-fuzzing-was-missing-half-the-toolkit.-we-forked-the-toolchain-to-fix-it./
- Native Go fuzzing is weak at deep SQL semantics because random bytes rarely form valid SQL. Use it for parsers and decoders, and use the Hegel generator plus PG differential for semantics. [I]

---

## 8. Other high-value items

- **Mutation testing:** this matters especially because no human reviews the code; it measures whether the tests can detect wrong code.
  - **gomutants** (`github.com/szhekpisov/gomutants`, MIT, v0.7.2 2026-10-21, Go 1.26+): uses `go test -overlay` byte patches, runs only the covering tests per mutant, and caches incrementally (120–150× faster on warm reruns). `--changed-since <ref>` limits it to changed lines. It has a GitHub Action, and Gremlins-compatible JSON and HTML output. Hegel itself uses it. [V] https://pkg.go.dev/github.com/szhekpisov/gomutants
  - **Gremlins** (Apache-2.0, 0.x, last push 2026-06) is "made for smallish modules". **go-mutesting** (avito-tech fork, active).
  - Gate on *changed lines* per change rather than the whole module. [I]
- **Coverage:**
  - Use `go test -coverpkg=./... -covermode=atomic`.
  - For multi-process and integration tests, build binaries with `go build -cover` and set `GOCOVERDIR`; merge with `go tool covdata`. This shows what the crash and multi-process harnesses actually reach.
  - Hegel ratchets 100% per-file coverage with `vladopajic/go-test-coverage`; ratchet coverage, don't target a number. [V justfile; `-cover` builds are a known Go ≥1.20 feature]
- **Race detector:**
  - Run all non-fuzz tests with `-race` in CI, including Hegel concurrent state machines (they are compatible). Use `GORACE=halt_on_error=1`.
  - Run some jobs with `-cpu=1,4,16`, and run `-count=N -shuffle=on` nightly to shake out order dependence.
  - As WarpStream found, races in hot paths can still escape `-race`, so add interleaving stress (Hegel concurrency, synctest variations). [V WarpStream]
- **Goroutine leak profile** is GA in Go 1.27 (`runtime/pprof` "goroutineleak"). Check it after tests, or use `go.uber.org/goleak` in `TestMain`. [V go1.27 notes]
- **Static analysis:**
  - `go vet`, `staticcheck`, `golangci-lint` with `forbidigo` to ban nondeterminism outside the clock and RNG packages;
  - `errcheck`/`errorlint`, because ignored S3 errors are data loss;
  - `uber-go/nilaway` (nil-panic analysis);
  - `govulncheck`.
- **Formal model of the commit protocol:** write a TLA+/PlusCal (or P, or Quint) spec of the manifest-CAS commit, GC, and compaction protocol over an object store with ambiguous responses and stale reads. Model-check it before trusting the code. It is the cheapest way to find protocol-level bugs (lost commits, GC deleting live objects) that tests reach only rarely. [I] This is outside Go tooling but very high value per hour for the core protocol.
- **Soak / chaos:**
  - Run nightly long `hegel.Workload` runs (ring, bank, and an Elle list-append workload) against real Tigris, with multiple processes, random SIGKILL, and fsck at the end. Use **unique bucket prefixes per run** and lifecycle rules to clean up.
  - Track Tigris request costs.
  - DuckDB's practice applies: soak and fuzz jobs file issues and do **not** block merges. Commit every minimized failure as a deterministic regression test. [I]
- **"Tiny knobs" builds:** run the suite under build tags or settings for minimum page size, object size, cache size, an aggressive compaction trigger, and retry budgets of 0 and 1. This is the DuckDB "vector size 2" trick. It makes rare structural paths common. [V DuckDB; I adaptation]
- **Upgrade / compat tests:** keep golden bucket snapshots created by older versions in testdata (or a fixed Tigris prefix), open them with the current version, and run fsck plus queries. With no human reviewer, format drift is a silent risk. [I]
- **Error-path injection at the Go level:** a fault-injecting `ObjectStore` that fails the *n*th call, for n = 1..N, for each public operation. This is SQLite's I/O-error and OOM loop (https://sqlite.org/testing.html). It is cheap, exhaustive per operation, and finds cleanup and rollback bugs. [I]

---

## Source list (main)
- Hegel: https://hegel.dev · https://hegel.dev/compatibility · https://pkg.go.dev/hegel.dev/go/hegel · https://github.com/hegeldev/hegel/tree/main/frontends/go (README, CHANGELOG.md, go.mod, justfile, stateful.go, examples/ring, examples/bank) · https://github.com/hegeldev/hegel-go (archived)
- Go: https://go.dev/doc/go1.27 · `go doc testing/synctest`, `testing/cryptotest`, `net/http/httptest.NewTestServer` (local 1.27.3)
- DST: https://www.polarsignals.com/blog/posts/2024/05/28/mostly-dst-in-go · https://www.polarsignals.com/blog/posts/2025/07/08/dst-rust · https://github.com/jellevandenhooff/gosim · https://pkg.go.dev/github.com/neria-cloud/gosim · https://docs.tigerbeetle.com/about/vopr · https://tigerbeetle.com/blog/2026-08-20-protocol-aware-dst · https://transactional.blog/simulation/buggify
- SQL testing: https://github.com/sqlancer/sqlancer · https://arxiv.org/abs/2503.21424 · https://www.cockroachlabs.com/blog/sqlsmith-randomized-sql-testing/ · https://duckdb.org/docs/current/dev/sqllogictest/overview.html · https://github.com/dolthub/sqllogictest · https://github.com/gregrahn/sqllogictest (mirror of sqlite.org/sqllogictest) · https://github.com/pganalyze/pg_query_go · https://www.cockroachlabs.com/enterprise-license-update/ · https://rfd.shared.oxide.computer/rfd/508
- Isolation: https://github.com/jepsen-io/elle · https://github.com/ligurio/elle-cli · https://docs.rs/crate/adya/latest · https://github.com/anishathalye/porcupine · https://github.com/ept/hermitage
- Object stores: https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html · https://www.tigrisdata.com/docs/concepts/consistency/ · https://www.tigrisdata.com/docs/objects/conditionals/ · https://antithesis.com/blog/2026/tigris_report/ · https://www.warpstream.com/blog/deterministic-simulation-testing-for-our-entire-saas · https://github.com/johannesboyne/gofakes3 · https://github.com/adobe/S3Mock · https://github.com/minio/minio · https://blog.localstack.cloud/localstack-single-image-next-steps/ · https://github.com/Shopify/toxiproxy · https://docs.rs/crate/slatedb/0.13.0/source/clippy.toml
- Antithesis: https://antithesis.com/pricing · https://antithesis.com/docs/getting_started.md · https://antithesis.com/docs/reference/sdk/go/instrumentor.md · https://pkg.go.dev/github.com/antithesishq/antithesis-sdk-go/assert
- Fuzz / mutation: https://blog.trailofbits.com/2026/05/12/go-fuzzing-was-missing-half-the-toolkit.-we-forked-the-toolchain-to-fix-it./ · https://pkg.go.dev/github.com/szhekpisov/gomutants · https://github.com/go-gremlins/gremlins
