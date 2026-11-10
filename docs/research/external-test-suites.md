# External test suites and benchmarks for a Postgres-wire subset engine

Research date: 2026-11-10. Tags: **[V]** = verified in a primary source (repo file, official docs, or GitHub API) that I or a delegated research agent actually read; **[I]** = inferred. Local inspection used sparse clones under `scratchpad/ext/` (postgres master @ 952bab8b, 2026-11-10; apache/datafusion-testing @ 64e2529, 2026-10-28; jackc/pgx @ 2026-11-07).

Builds on `research-testing.md` sections 2 and 4 (not repeated here).

---

## 0. The one structural fact that decides most of this

A "strict subset" engine **rejects** some statements that Postgres accepts. Any suite whose unit of comparison is a *whole file of output* (pg_regress, pg_isolation_regress, RisingWave's regress) will report that whole file as failed the moment one unsupported statement appears, and **every later statement in that file that depends on the rejected one also diverges**. So for each suite the questions are:

1. What is the unit of pass/fail (statement, record, test function, file)?
2. Can a deny-list be expressed **outside** the suite's files at that granularity?
3. Are expected results fixed files produced by Postgres, or produced by running a reference Postgres at test time?

| Suite | Unit of comparison | External deny-list possible at | Expected results |
|---|---|---|---|
| pg_regress (`src/test/regress`) | whole file, text diff of psql output | file (custom schedule / test-name args) | fixed `.out` files from PG (+ `_N.out` alternates) |
| pg_isolation_regress (`src/test/isolation`) | whole spec file output | file | fixed `.out` from PG |
| Doltgres capture/replay over pg_regress | **per statement** (wire messages) | statement (their tool has a hard-coded skip list; extensible) | captured live from real PG |
| sqllogictest (any runner) | per record | file (sqllogictest-rs `--skip` regex); per record only with own runner or by `skipif` labels (= editing files) | fixed in file; can be regenerated with "complete/override" mode from PG |
| SQLancer / SQLancer++ | per generated test case (metamorphic oracle) | feature-level (SQLancer++ learns; SQLancer by generator options/expected errors) | none needed (TLP/NoREC self-checking) |
| Jepsen `jepsen.postgres` / `jepsen.sql` | per history (checker verdict) | workload options (e.g. `--no-savepoints`, `--upsert-types`) | none (Elle infers anomalies) |
| Go driver suites (pgx, lib/pq) | per Go test function | test name (`go test -skip regex`) | fixed in test code |
| CockroachDB-style ORM harness | per upstream test | test name (blocklist map) | fixed in upstream tests |

---

## 1. PostgreSQL's own suites

### 1.1 `src/test/regress` (pg_regress)

**What it tests.** Everything: types, operators, functions, DDL, planner, catalogs, psql. [V]

**Size.** 245 `sql/*.sql` files, 280 `expected/*.out` (alternate expected files `foo_1.out`, `foo_2.out` for platform/locale variants), 133,982 lines of SQL on master. [V local count] Doltgres's per-statement capture of the suite counts **42,090 statements**. [V https://github.com/dolthub/doltgresql/pull/3808]

**Format and how it connects.** pg_regress runs each test file through **psql**: `psql -X -a -q -d <db> < sql/foo.sql > results/foo.out 2>&1` [V `pg_regress_main.c` line 75], then `diff`s `results/foo.out` against `expected/foo.out` and every alternate `expected/foo_N.out`, picking the one with the fewest diff lines [V `pg_regress.c` ~1420-1530]. The output is psql's aligned table format, echoed input, NOTICEs, and error text.

**Running against an external server.** `make installcheck` / `installcheck-parallel` / `installcheck-tests TESTS="a b"` [V `GNUmakefile` 103-110]. Flags: `--host/--port`, `--dbname`, `--schedule=FILE`, `--use-existing` (do not drop/create the database and roles), `--expecteddir`, `--launcher`, `--load-extension`, `--create-role`, `--bindir` [V `pg_regress.c` 2099-2131]. Without `--use-existing` it runs `DROP DATABASE IF EXISTS`, `CREATE DATABASE ... TEMPLATE=template0 ENCODING=...` and `ALTER DATABASE ... SET lc_messages/lc_monetary/lc_numeric/lc_time TO 'C'; SET bytea_output TO 'hex'; SET timezone_abbreviations TO 'Default'` [V `pg_regress.c` 2027-2054], and `DROP ROLE IF EXISTS`/`CREATE ROLE ... WITH LOGIN` for extra roles [V 2072-2082]. It sets `PGTZ=America/Los_Angeles` and `PGDATESTYLE='Postgres, MDY'` for the psql clients [V 796-797]. There is no longer an `ignore:` directive in schedules [V: absent from `pg_regress.c`].

**What it demands from the server (beyond SQL).** [V `sql/test_setup.sql`]
- `test_setup` (first in `parallel_schedule`, required by almost every later file) runs `GRANT ALL ON SCHEMA public TO public`, `SET allow_in_place_tablespaces = true; CREATE TABLESPACE regress_tblspace LOCATION ''`, `VACUUM`/`VACUUM ANALYZE` after loads, **server-side** `COPY onek FROM :'filename'` (reads files from the server's filesystem path `PG_ABS_SRCDIR/data/*.data`), `CREATE TABLE ihighway () INHERITS (road)`, the `name` and `point` types, and `CREATE FUNCTION` for C functions in `regress.so` (`binary_coercible`, `expanded_record_*`). psql meta-commands (`\getenv`, `\set`) are used.
- 112 of 245 files use psql backslash commands [V local grep]. Many use `pg_catalog` tables, `EXPLAIN` output, `VACUUM`, and superuser features.
- **Error output is compared byte-for-byte, including the `LINE 1:` echo and the caret position**, which comes from the `P` (position) field of ErrorResponse. E.g. `expected/int4.out` has 38 `ERROR:` lines, each with `LINE 1: ...` and a caret. [V] So error message text, DETAIL/HINT lines and cursor positions must all match PG exactly.
- Even "simple" files mix in things a subset will not have: `int4.sql` calls `pg_input_is_valid()` [V]; RisingWave had to comment out 17 of 180 lines of `int4.sql` [V per agent, https://github.com/risingwavelabs/risingwave/tree/main/src/tests/regress].

**Exclusion granularity.** Per **file** only (custom schedule file or test names on the command line). Within a file: none, other than editing the `.sql` *and* the `.out`. Using `--expecteddir` with our own expected files would be "modifying the suite".

**Verdict.** Unmodified pg_regress through psql is **effectively unusable for a subset engine**: no file except trivial ones will pass, `test_setup` cannot pass without tablespaces/server-side COPY/C functions, and failure cascades. [I, strongly supported by Doltgres/CRDB/RisingWave experience below]

**The workable alternative: per-statement capture/replay (Doltgres model).** [V via agent; files verified to exist]
- DoltgreSQL (Apache-2.0) `testing/go/regression/tool/`: a proxy records every pgproto3 message while **stock pg_regress runs against real Postgres**, writes one binary `.results` file per test, then `replay.go` replays the frontend messages against the engine under test and compares responses statement by statement (ordered and unordered row comparison, an OID map, "partial success" when both sides error with different messages or column names differ). Files: `main.go`, `replay.go`, `compare.go`, `oid_map.go`, `rewrite_copy_from.go`, `run_test.go` etc. [V GitHub API listing] https://github.com/dolthub/doltgresql/tree/main/testing/go/regression/tool
- The suite's inputs and PG's outputs are unmodified; only the *comparison harness* changes. Unit becomes the statement, so a deny-list keyed by (test file, statement) is possible. Cascades still happen (a rejected CREATE TABLE fails later statements), but they are counted, not hidden.
- Their numbers: 51.07% statements succeed (21,496 / 42,090) on 2026-11-10, after two years of work and with 141 emulated `pg_catalog` tables. [V PR #3808 comment] Earlier: 27.2% → 33.8% (Nov 2024), 39.38% (Jul 2025). [V dolthub blog]
- Pitfalls: SASL nonces cannot replay (auth disabled); OIDs differ; psql `\d` catalog queries are auto-failed; captured with a specific PG version.

### 1.2 `src/test/isolation` (isolationtester, pg_isolation_regress)

**What it tests.** Concurrent behaviour: SSI anomalies, row locking, EvalPlanQual, deadlocks, DDL vs DML locking, FK races, ON CONFLICT, MERGE. [V spec list]

**Size.** 141 spec files, 150 expected files, schedule of ~139 entries. [V local count]

**Format.** Custom spec language: `setup {}` / `teardown {}` / `session sN { setup{} step name {SQL} teardown{} }` / `permutation ...`. Without permutation lines, all interleavings are run. Output is isolationtester's own format: `starting permutation: ...`, `step x: SQL`, result tables, `ERROR:  <primary message>` (only severity + primary message, no SQLSTATE, no LINE/caret) [V `isolationtester.c` ~1025], and `step x: ... <waiting ...>` when a step blocks.

**How it connects.** libpq directly, not psql. One control connection plus one per session. At connect it runs `SELECT set_config('application_name', current_setting('application_name') || '/' || $1, false)` via `PQexecParams` (extended protocol), records `PQbackendPID()` (BackendKeyData must give **distinct PIDs** per connection), and prepares `SELECT pg_catalog.pg_isolation_test_session_is_blocked($1, '{pid,...}')`, which it polls to decide whether a step is waiting. Steps run asynchronously with `PQsendQuery`; after `max_step_wait` (default 360 s, `PG_TEST_TIMEOUT_DEFAULT`) it sends a **cancel request**. [V `isolationtester.c` 150-240, 667, 939-973]

**External server.** `make installcheck` or `./pg_isolation_regress --use-existing spec1 spec2 ...` [V README, Makefile]. Some tests need `max_prepared_transactions >= 3` (separate target).

**Expected results are PG-implementation-specific.** The `.out` files encode *which* transaction PG aborts, *at which step* (statement vs COMMIT), and with which message ("could not serialize access due to read/write dependencies among transactions"), plus where PG *blocks* on a lock. Example `simple-write-skew`: 6 permutations, 4 expected `ERROR`s at COMMIT. [V] An engine that is correctly serializable but uses a different mechanism (OCC at commit, single writer, first-committer-wins) will produce *different but valid* outputs. 110 of 150 expected files contain `<waiting ...>`. [V local grep]

**Specs a small SQL subset could run** (no pg_*-functions, triggers, partitions, FOR UPDATE, ON CONFLICT, savepoints, advisory locks, etc. by grep): classroom-scheduling, project-manager, receipt-report (210 permutations), referential-integrity, simple-write-skew, total-cash, two-ids (90 permutations), read-only-anomaly{,-2,-3}, multiple-row-versions, read-write-unique{,-2,-4}, update-conflict-out, lock-update-traversal and a few more. Of these, ~11 have no `<waiting>` in the expected output, but all of them still hard-code PG's SSI abort placement. [V local grep]

**Verdict.** Do not use as pass/fail as-is. Its value is as a **scenario library** (the Cahill/Ports SSI cases with real schemas) to re-check with a semantic oracle: "the committed transactions are serializable" (or the claimed level), not "the same transaction aborted". [I] The adapter would also have to implement `pg_isolation_test_session_is_blocked` honestly (the adapter knows if a session is waiting on the engine). [I]

---

## 2. sqllogictest corpora and runners

Background (format, SQLite corpus, licences) is in `research-testing.md` §2.

### 2.1 Corpora whose expected results come from Postgres

**apache/datafusion-testing `data/sqlite/` — the best find.** [V local clone]
- The SQLite sqllogictest corpus (select1-5, index/*, random/*; `evidence/` dropped: "too many sqlite specific commands"), converted to `.slt` and **completed against PostgreSQL** by `datafusion/sqllogictest/regenerate_sqlite_files.sh`, which strips `onlyif mysql/mssql` blocks, removes `skipif postgresql`, and runs the DataFusion runner with `--include-sqlite --postgres-runner --complete` [V https://raw.githubusercontent.com/apache/datafusion/main/datafusion/sqllogictest/regenerate_sqlite_files.sh].
- Records where DataFusion differs from PG keep DataFusion's answer behind `skipif postgres` (8,379 such records across 61 files) [V grep], so the *unmarked* records are PG-produced answers.
- Size: 595 `.slt` files, 773 MB, ~5.69 M `query` records, ~90 K `statement` records. [V local count] Last commit 2026-10-28 ("Update sqlite expectation for NOT IN list containing NULL"). [V]
- Licence: repo Apache-2.0; `data/sqlite/NOTICE.txt` selects MIT for the SQLite-derived files. [V]
- SQL surface is tiny and fits a subset engine: `CREATE TABLE` with INTEGER/VARCHAR/FLOAT-ish columns, `CREATE INDEX`, `CREATE VIEW`, `INSERT`, `DELETE`, `DROP`, SELECT with joins, scalar and correlated subqueries, `CASE`, `CAST`, `BETWEEN`, `IN` lists/subqueries, `NULL` 3VL, aggregates, `GROUP BY/HAVING`, `ORDER BY`. [V sampled files]
- Catches: results are formatted by **DataFusion's runner normalisation** (numeric through BigDecimal, float formatting, booleans, `control resultmode valuewise`, `hash-threshold 8` with MD5-hashed value lists), so a runner must reproduce that formatting exactly, or hashes mismatch [V `datafusion/sqllogictest/src/engines/postgres_engine/mod.rs` `cell_to_string`]. The runner puts each file in its own schema: `DROP SCHEMA IF EXISTS x CASCADE; CREATE SCHEMA x; SET search_path TO x` [V same file], and uses the simple query protocol (`simple_query`). Integer division, NULL ordering etc. are PG semantics in the unmarked records [I].
- DataFusion's own `test_files/pg_compat/` has only 7 files (`pg_compat_null`, `_simple`, `_timestamptz`, `_type_coercion`, `_types`, `_union`, `_window`) run with `PG_COMPAT=true` against Postgres. [V GitHub API, README] Small, Apache-2.0.

**dolthub/sqllogictest fork (Doltgres).** DoltHub ran the SQLite corpus against stock Postgres and fixed SQLite/MySQL assumptions in tests and runner (I/R equivalence, `3.000`→`3`, PG type names), adding skips for MySQL-only records; Doltgres reports 99% of ~5.6 M queries. [V via agent: https://dolthub.com/blog/2026-07-10-doltgres-99-percent-sql-logic-tests/, https://dolthub.com/blog/2026-08-06-doltgres-1-0/] Licence of the fork repo is unclear in GitHub metadata (see `research-testing.md`); the corpus itself is SQLite's public-domain-style licence. [I]

**Materialize `test/sqllogictest/postgres/`.** A dozen pg_regress files hand-converted to slt, each with a header naming the PG source commit and the PostgreSQL licence; `cockroach/` has 186 CRDB logic tests (some now Apache-2.0 after BSL change dates). Repo is BSL 1.1. Unsupported features handled by deleting files or `halt` at the first divergence. [V via agent, https://github.com/MaterializeInc/materialize/blob/main/test/sqllogictest/cockroach/README.md] Expected results are Materialize's, not PG's. [I]

**RisingWave `e2e_test/**/*.slt` and `src/tests/regress`.** Apache-2.0. The slt files are RisingWave-dialect (streaming, sources, MVs) — expected results are RisingWave's. `src/tests/regress` is a Rust pg_regress clone driving psql in `--mode postgres|risingwave` against shared expected files, enabling only 23 of 211 copied PG test files and using `--@ ` to comment out statements per file (an edit, mirrored in the `.out`). [V via agent]

### 2.2 Runners

**sqllogictest-rs** (risinglightdb). Apache-2.0/MIT, v0.29.1 (2026-02-14), 235 stars. [V GitHub API] CLI `sqllogictest` engines: `postgres` (simple protocol via tokio-postgres), `postgres-extended`, `mysql`, `external` (command template, e.g. a JDBC app) [V `sqllogictest-bin/src/engines.rs`]. Flags: `--override` (write actual results into files = PG completion mode), `--label` (for `skipif/onlyif`), `-j` (parallel; creates/drops a database per file with `CREATE DATABASE`/`DROP DATABASE`), `--junit`, `--fail-fast`, `--partition-id/count`, `--skip <regex>` which filters **file paths only** [V `main.go` 300-315, 447, 531]. Extensions: `statement error <regex>`, SQLSTATE match `(42P01)`, multi-line error after `----`, `retry`, `system ok`, `control substitution`, `let`, `<slt:ignore>` [V README]. No external per-record deny-list.

**Go runners.** dolthub `sqllogictest/go/logictest` (harness interface) and small MIT ones; see `research-testing.md`. A Go runner of our own (~300-500 lines) is the only way to get a per-record deny-list keyed outside the files, plus DataFusion-compatible value normalisation. [I]

---

## 3. SQL standard conformance suites

**elliotchance/sqltest.** MIT, 55 stars, **last push 2024-05-29** (stale). BNF-driven generator producing `*.tests.yml` per SQL:2016 feature ID (135 test files, e.g. E011-02 expands to 70 tests); Python runner in Docker; db configs only for `postgresql/v9_2`, `v9_6` and `sqlite3`. [V repo] A test passes if the **statement executes without error**; there are essentially no expected result rows. [V `E011-02.tests.yml`, `generate_tests.py`] It measures syntax acceptance of standard SQL, which a PG subset will mostly "fail" by design, and it says nothing about result correctness. **Avoid** except possibly as an acceptance-parity probe with PG as the oracle. [I]

**NIST SQL Test Suite v6.0** (1996, NISTIR 5998). Entry/Transitional/Intermediate SQL-92 via embedded C/COBOL/Fortran and interactive SQL scripts; no maintained distribution or slt conversion found (some forks exist inside old projects, e.g. Apache Derby's converted `nist` tests [I]). [V https://www.nist.gov/publications/users-guide-sql-test-suite-version-60-0] **Avoid**: dated, not PG dialect, expected results are documents, conversion effort high. [I]

---

## 4. Random query generators usable over the wire

**SQLancer** (MIT, 1,773 stars, pushed 2026-11-05; last tagged release v2.0.0 in 2022, users build from main). JDBC. Providers include postgres, cockroachdb, yugabyte, materialize, citus, cloudberry, duckdb, datafusion, databend, questdb, sqlite3 and others. [V GitHub API, `src/sqlancer/`] The **postgres provider is too wide for a subset**: it creates/drops databases, `CREATE EXTENSION ... WITH SCHEMA extensions`, tablespaces, statistics, sequences, views, `RESET ROLE`, `SET CONSTRAINTS`, and reads `pg_proc` (`proname, provolatile`), `information_schema.tables JOIN pg_class JOIN pg_namespace` (with `relkind`), `pg_statistic_ext`, `pg_indexes`. [V `PostgresProvider.java`, `PostgresSchema.java`] Would need many expected-error entries or a custom provider (Java). PQS is unmaintained (prior notes).

**SQLancer++** (suyZhong/SQLancerPlusPlus, MIT, 20 stars, pushed 2026-10-26; ASPLOS'26 paper "Scaling Automated Database System Testing"). Generic adaptive generator that learns which features the target accepts; a new DBMS is an enum in `GeneralOptions.java` plus a JDBC URL template in `dbconfigs/jdbc.properties`; it already has `POSTGRESQL`, `COCKROACHDB`, `DOLT` entries. Oracles: `WHERE` (TLP) default, `NoREC`. Setup: tries `CREATE DATABASE`/`USE`, falls back to `DROP TABLE` cleanup; schema is tracked internally (no catalog queries found in `GeneralSchema.java`). Java 11+, Maven. [V README, GitHub API, source grep] **Best fit of the random testers** for a subset engine: no reference needed, self-limiting to supported features. Catch: research-grade code, results are bug reports, not a pass rate; feature-learning means low coverage of rarely-accepted constructs. [I]

**SQLsmith** (GPL-3.0, pushed 2026-07-15). libpqxx. Builds its grammar from the live catalog: `select version()`, `pg_type` with `regnamespace`, `information_schema.tables`, `pg_attribute JOIN pg_class`, constraints, `pg_operator`, `pg_proc` with `unnest(proargtypes)`, aggregates. [V `postgres.cc` 105-302] Crash/internal-error finder only. Interesting property [I]: if the adapter's emulated catalog lists only supported types/operators/functions, SQLsmith generates only supported SQL. Catch: heavy catalog emulation; GPL only matters if we link/distribute it (we would only run it).

**EET** (JZuming/EET, GPL-3.0, 2025-07; OSDI'24 equivalent-expression transformation, supports PG). [V GitHub API] Research code; same catalog-dependence class as SQLsmith. [I]

Other research fuzzers (SQLRight, Squirrel, Griffin, Sedar, Mozi, Radar) need source instrumentation or a specific DBMS harness; not practical over the wire for a Go library. [I]

---

## 5. Isolation / ACID checkers usable over the wire

**Jepsen `jepsen-io/postgres`** (EPL-2.0 / GPL-2+CPE secondary, pushed 2026-07-31). Workloads `append` (Elle list-append), `rw` (Elle rw-register), `internal` (internal consistency), `ledger` (WIP). Runs against an **existing local Postgres without SSH**: `lein run test-all -w append ... --isolation serializable --nemesis none --existing-postgres --node localhost --no-ssh --postgres-user jepsen --postgres-password pw`. [V README] Workloads come from **`jepsen-io/sql`** (pushed 2026-11-09), which is built for "different" SQL databases: options `--isolation`, `--expected-consistency-model`, `--[no-]savepoints`, `--upsert-types` (update-insert-update vs `INSERT ... ON CONFLICT (id) DO UPDATE SET val = CONCAT(t.val, ',', ?)`), `--encodings`, `--key-types primary,secondary`, `--[no-]indirection`, `--max-writes-per-key`, `--key-count`. [V `src/jepsen/sql.clj`, `workload/append.clj`] Tables: `CREATE TABLE IF NOT EXISTS ... (id int not null primary key, sk ..., val text)`; errors are classified by regex on pgjdbc messages ("current transaction is aborted", "duplicate key value"). [V README] Also `internal-sim` (random schemas) and `default-value` (DDL) workloads. [V README]
- Runtime: JDK 21+, Leiningen, pgjdbc; gnuplot/graphviz for reports. [V duckdb README; I for postgres]
- Minimum SQL: CREATE TABLE IF NOT EXISTS, int PK, text, unindexed secondary column, BEGIN/SET TRANSACTION ISOLATION LEVEL, SELECT by key, UPDATE ... SET val = CONCAT(val, ',', ?), INSERT (+ either SAVEPOINT or ON CONFLICT), COMMIT/ROLLBACK, SQLSTATE 40001 handling. [V/I]
- Catch: anomalies are checked semantically (no PG-specific abort placement), so this is the **right** isolation oracle; but it needs JVM and Clojure tooling, and "process kill" faults require the engine in a separate process (the adapter process). Jepsen also has an **embedded-DB pattern** in `jepsen-io/duckdb` (2026-03): local processes embed DuckDB, Jepsen drives them over HTTP, Elle checks, nemesis = process kills. [V README] That pattern fits an embedded Go library even without the wire adapter. [I]

**Hermitage** (ept/hermitage, no licence file via API; README states CC BY 4.0 per prior notes; pushed 2026-01-23). Markdown only: README + `postgres.md`, `cockroachdb.md`, `yugabytedb.md`, etc. **Not executable**; manual two/three-session scripts with PG results per level. [V GitHub API listing] Use as a checklist to port into Go table tests (prior notes).

**IsoVista** (hengxin/IsoVista, MIT, pushed 2026-10-25, 6 stars; VLDB'24). Black-box isolation checker with workload generator and web UI; supports MySQL/PostgreSQL/MariaDB; extends PolySI. [V search + GitHub API] Java, academic; Elle via Jepsen is the stronger, maintained choice. [I] DBCop (amnore/dbcop, 2023, no licence) is dead. [V]

---

## 6. Driver and ORM suites as compatibility checks

**pgx** (MIT; ~1,014 `Test*` functions; tests need `PGX_TEST_DATABASE` and optionally many TLS/auth conn strings). Its setup SQL needs `create extension hstore; create extension ltree; create domain uint64 as numeric(20,0)`, superusers with md5/SCRAM passwords, and `pg_roles`. pgx already ships `skipCockroachDB`/`SkipCockroachDB` and `SkipPostgreSQLVersionLessThan` helpers (60 call sites), keyed off server identification. [V local clone `pgxtest/pgxtest.go` 151-175, `testsetup/postgresql_setup.sql`] Per-test exclusion without editing: `go test -skip 'TestA|TestB'`. Mostly exercises the **adapter** (protocol, type codecs: arrays, composites, ranges, hstore) rather than the engine. [I] lib/pq similar. [I]

**CockroachDB ORM harness** (model to copy, not code: CockroachDB Software License). Per ORM/driver `X.go` + `X_blocklist.go`; clone upstream at a pinned tag, run its suite, parse to JUnit, compare with `blocklist` (expected failures → issue) and `ignorelist` (flaky/irrelevant); fail on *unexpected pass* too and print a regenerated blocklist (ratchet). Suites: pgx, pgjdbc, hibernate, django (fork + allowlist), sqlalchemy, psycopg, asyncpg, npgsql, activerecord, ruby-pg, libpq, rust-postgres, node-postgres, gorm, knex, sequelize, typeorm, gopg, liquibase. Sizes: pgjdbc ~387 entries, npgsql ~385, ruby_pg ~92, pgx/libpq small. [V via agent, https://github.com/cockroachdb/cockroach/tree/master/pkg/cmd/roachtest/tests]

**Django**: native `DatabaseFeatures.django_test_skips` / `django_test_expected_failures` in a backend give per-test exclusion without editing Django. [V via agent] **SQLAlchemy**: dialect "requirements" mechanism similar [I]. **sqlc**: compile-time only, needs a schema parser/engine, not a runtime suite [I]. **GORM**: tests are written against real PG with many features [I].

Realism for a small subset: low until types/catalog are broad; driver suites are mostly adapter tests. Doltgres uses **homegrown smoke tests per client** (`testing/postgres-client-tests/`, ~25 languages/drivers) rather than upstream suites. [V via agent] QuestDB uses one driver-agnostic YAML of steps executed by many driver runners (`compat/`). [V via agent]

---

## 7. Compatibility scorecards

- **PG Scorecard / Postgres Compatibility Index** (secp256k1-sha256/postgres-compatibility-index, MIT, pgscorecard.com). A weighted feature-probe checklist (`pci_autotest.py`, ~340 lines) against PG17: full/partial/none = 1/0.5/0 per feature, category weights. Not a correctness suite. [V via agent]
- No widely-used third-party correctness scorecard exists; vendors publish their own (Doltgres: % of pg_regress statements and % of SLT; CRDB: blocklist sizes). [I]
- Doltgres numbers are the most comparable published figures: pg_regress statements 51.07% (2026-11), SLT ~99% of 5.6 M. [V]

---

## 8. Performance benchmarks (summary; details from delegated research, [V] items read in repo/docs)

| Benchmark | Connects | Licence / status | Minimum features | Early fit |
|---|---|---|---|---|
| **pgbench** | libpq; `-M simple/extended/prepared` | PostgreSQL; ships with PG 18 | `-S`: point SELECT. tpcb-like: BEGIN/END, `UPDATE x = x + :d`, INSERT with CURRENT_TIMESTAMP. Init `-I dtgvp`: CREATE TABLE ... `WITH (fillfactor=100)` (always appended), TRUNCATE, `COPY ... FROM STDIN` (text), VACUUM ANALYZE, ALTER TABLE ADD PRIMARY KEY; `-I G` uses generate_series. At start: `select count(*) from pgbench_branches` (fatal), a partition-detection query on pg_class/pg_partitioned_table/pg_inherits (errors tolerated), vacuum/truncate unless `-n`. For server_version ≥ 14 it queries `pg_class.relkind` and uses `COPY ... WITH (FREEZE ON)`. [V] | **First**. Custom `-f script -n` and own init avoid most of it. |
| **YCSB JDBC** | pgjdbc | Apache-2.0; release 2019, commits 2026-10 | user-created `usertable(key varchar pk, field0..9 varchar)`, point CRUD, `WHERE key >= ? ORDER BY key LIMIT ?`. [V] | Easy; JVM. |
| **sysbench** oltp_* | libpq (ps-mode: PQprepare) | GPL-2.0; last release 2020, dormant | INTEGER/CHAR, PK, secondary index, multi-row INSERT, BETWEEN, SUM, ORDER BY, DISTINCT, prepared BEGIN/COMMIT; retries 40001/40P01. [V] | Second. |
| **go-tpc** tpcc/tpch/ch | lib/pq | Apache-2.0; last commit 2026-01 | TPC-C: DECIMAL, TIMESTAMP, composite PKs, secondary indexes, multi-row INSERT, `FOR UPDATE`, `ORDER BY .. LIMIT 1 FOR UPDATE`, COUNT(DISTINCT) over join, hint comments. TPC-H: 22 queries, Q15 as CTE. [V] | Mid. |
| **BenchBase** | pgjdbc 42.7.4, Java 23 | Apache-2.0; last commit 2025-12, no releases since 2023 | Always runs JDBC `DatabaseMetaData` introspection (`getTables/getColumns/getIndexInfo/getImportedKeys` → heavy pg_catalog) and builds INSERTs from it; TPC-C DDL has FKs with ON DELETE CASCADE. [V] | Late. |
| **HammerDB** TPROC-C/H | Pgtcl/libpq | GPL-3.0; v6.1.1 2026-10 | PL/pgSQL functions/procedures required, `pg_stat_database` polling for TPM, CREATE DATABASE/USER, COPY CSV. [V] | Not realistic. |
| **ClickBench** (postgresql) | psql | **CC BY-NC-SA 4.0** | one 105-column table, 43 queries (GROUP BY, COUNT DISTINCT, REGEXP_REPLACE, DATE_TRUNC, extract, LIKE, OFFSET); script uses sudo/psql/CREATE DATABASE/`\copy ... freeze`/VACUUM ANALYZE. [V] | Subset of queries; licence non-commercial. |
| TPC-H dbgen/qgen | n/a | TPC EULA v2.2.0 | full 22 queries need correlated subqueries, EXISTS, interval/date arithmetic, extract, substring, view or CTE. [V] | Q1/Q6 early. |

Publishing: TPC Fair Use requires "derived from TPC-X" naming, no tpmC/QphH, a disclaimer, and no comparison with official results; the TPC-H EULA restricts public disclosure of results except as non-comparable/research. [V https://www.tpc.org/TPC_Documents_Current_Versions/txt/EULA_v2.2.0.txt] TSBS needs TimescaleDB-specific features; CRDB `workload` is CRDB-dialect and source-available. [I]

---

## 9. How other Postgres-compatible engines use these suites

- **DoltgreSQL (Go, Apache-2.0)**: pg_regress capture/replay, per-statement counts posted on every PR with named regressions/progressions; PRs adding failures cannot merge. SQLite SLT via a PG-corrected fork (99%). Client smoke tests in ~25 drivers. Real-world pg_dump loading (3 of 100+ loaded at first). 141 emulated pg_catalog tables. [V via agent]
- **CockroachDB**: ORM/driver blocklist ratchet (above); pg_regress roachtest on REL_16_0 with ~20 patches, comparing produced `regression.diffs` with ~188 committed golden diff files (weekly, non-release-blocking, "very large deviation from postgres"); SQLite SLT via a glob allowlist; sqlsmith, TLP, costfuzz, unoptimized-query-oracle, Jepsen roachtests. [V via agent]
- **YugabyteDB**: ~130 small `yb_*_schedule` files; `yb.port.*` ported tests kept byte-identical to upstream with `-- YB <reason>` annotations; `yb.depd.*` minimal-dependency setups instead of full test_setup. Jepsen fork. [V via agent]
- **Materialize**: hand-converted PG regress → slt with `halt` at first divergence; CRDB tests (licence-dated). [V via agent]
- **RisingWave**: pg_regress clone, 23 of 211 files, `--@` statement comment-outs. [V via agent]
- **Spanner PGAdapter**: patched PG16 regress with per-statement markers; counts "both errored" as pass (inflates rate); results internal. [V via agent]
- **QuestDB**: driver-agnostic YAML compat cases run by many drivers. [V via agent]
- **Neon/PGlite**: real Postgres; Neon runs full `parallel_schedule` + isolation (storage regression, not dialect); PGlite runs no pg_regress in CI. [V via agent]
- **CedarDB, Aurora DSQL, DuckDB**: no published external-suite results; CedarDB generates pg_catalog virtually from reverse-engineered tool needs. [V via agent]

Common pattern: **nobody runs pg_regress unmodified and gets a pass rate; everybody either patches the files, keeps golden diffs, or re-harnesses per statement.** [I from the above]

---

## 10. Adapter: Go server-side wire libraries and connect-time surface

Commits read by the delegated agent: postgres 952bab8, pgx 21fbf26, psql-wire eef30e8, doltgresql 406444c, pgjdbc 9c6eeaa, lib/pq 451ac10. Line numbers may drift.

### 10.1 Libraries

| Library | Licence / activity | Verdict |
|---|---|---|
| **jackc/pgx/v5 `pgproto3`** | MIT; pgx 14.4k stars, v5.11.0 (2026-09-07) [V] | **Best base.** A message codec only. `Backend.ReceiveStartupMessage` handles StartupMessage (3.0 and 3.2), SSLRequest, CancelRequest and GSSEncRequest. `Receive` decodes every frontend message. All backend messages exist, including ErrorResponse with every field (Position included), NoticeResponse, CopyIn/Out, PortalSuspended, NegotiateProtocolVersion, and BackendKeyData with a variable-length secret. No TLS, no auth logic, no portal/pipeline state machine, no value encoding (use `pgtype.Map`). [V `pgproto3/backend.go`] Expect ~1-2k lines of our own session code. [I] |
| **jeroenrinzema/psql-wire** | Apache-2.0; 242 stars, v0.20.0 (2026-09-23) [V] | A handler framework with portals, row-limited Execute, pipelining, cancel hooks, TLS and COPY FROM. **Gaps:** <br>• ErrorResponse has no `P` (Position) field, so psql's `LINE n:`/caret output cannot match. [V `error.go`, I re-checked] <br>• No NoticeResponse API and no COPY TO STDOUT. <br>• Cleartext auth only. <br>• `server_version` is unconditionally overwritten with `"150000"`, so libpq parses major version 150000. [V `handshake.go` ~L270-276, re-checked] <br>• Pins pgx v5.4.3. |
| **dolthub/doltgresql `server/`** | Apache-2.0; v1.5.0 (2026-11-05) [V] | Reference code: pgproto3-based, SCRAM-SHA-256 (`authentication_scram.go`), COPY OUT, Notice. Coupled to go-mysql-server/vitess. Its BackendKeyData secret is zeroed ("TODO" for cancel), and Execute row limits are ignored. It sends only 4 ParameterStatus values (`server_version=15.17`, `client_encoding`, `standard_conforming_strings`, `in_hot_standby`). [V] |
| apecloud/myduckserver `pgserver/` | Apache-2.0 [V] | A fork of the Doltgres handler. |
| cockroachdb `pkg/sql/pgwire` | CockroachDB Software License [V] | **Not usable.** Use is limited to purposes related to CockroachDB. |
| pgmock, standalone pgproto3, postlite, pgsrv | MIT/Apache [V] | Archived or dead; postlite (a SQLite-behind-pgproto3 proxy) is a useful reference for intercepting catalog queries. |
| fergusstrange/embedded-postgres | MIT, v2.0.1 (2026-10) [V] | Runs a real PG binary: the reference oracle, not an adapter. |

### 10.2 What clients and suites demand

- **ParameterStatus.** PG18's GUC_REPORT set: application_name, scram_iterations, client_encoding, search_path, DateStyle, server_encoding, default_transaction_read_only, server_version, in_hot_standby, session_authorization, integer_datetimes, standard_conforming_strings, IntervalStyle, TimeZone, is_superuser. [V https://www.postgresql.org/docs/current/protocol-flow.html] Send them all, and re-send after SET. [I]
- **libpq/psql/pg_regress.** Startup carries `options`. pg_regress sets `PGOPTIONS="-c intervalstyle=postgres_verbose"`, `PGTZ`, and `PGDATESTYLE` [V `pg_regress.c` 796-813], so the adapter must parse `-c name=value`. psql in `-X -a -q` mode issues no catalog queries for plain SQL. However, 56 of 245 regress files use `\d…`, which touches ~50 catalog objects. The expected outputs contain 2,561 `NOTICE:` lines and 1,957 `LINE n:` lines across 134 files. 21 files use COPY FROM STDIN and 11 use COPY TO STDOUT. [V]
- **isolationtester.** Needs extended-protocol `set_config`/`current_setting`, distinct `PQbackendPID`s, `pg_isolation_test_session_is_blocked` polled every ~10 ms, multi-statement simple queries, and a working CancelRequest (`PQcancelCreate`/`PQcancelBlocking`) on timeout. [V]
- **pgbench.** See §8. Note that `server_version ≥ 14` triggers a `::regclass` catalog query plus `COPY ... WITH (FREEZE ON)` during `-i`. [V]
- **pgx.** The default `QueryExecModeCacheStatement` uses named Parse + Describe, so the server must return parameter OIDs. Results come back **binary** for all built-in OIDs. Simple-protocol mode and `EscapeString` refuse to run unless `standard_conforming_strings=on` and `client_encoding=UTF8`. sslmode=prefer sends SSLRequest ('N' is fine). No automatic catalog queries; only `LoadType(s)` reads pg_type/pg_class/pg_attribute. [V]
- **pgjdbc** (SQLancer, BenchBase, Jepsen, YCSB):
  - **Fatal at connect** unless client_encoding is UTF8, DateStyle starts with ISO, standard_conforming_strings is on or off, and integer_datetimes is on or off. [V `QueryExecutorImpl.java` ~3237-3294]
  - `SET extra_float_digits=3` is sent only if server version < 12. [V]
  - Defaults: extended protocol, named statements after 5 executions, binary transfer. [V]
  - `TypeInfoCache` queries pg_type/pg_namespace/`current_schemas(true)` for unknown OIDs. ResultSetMetaData nullability/base-table calls join pg_class/pg_attribute/pg_attrdef with `pg_get_expr`. DatabaseMetaData uses ~20 catalog objects; BenchBase needs them. [V]
- **lib/pq.** Sends `client_encoding=UTF8` and `datestyle=ISO, MDY` and rejects other values. Parses `server_version` as `%d.%d`. [V]
- **SQLAlchemy.** `select pg_catalog.version()` must match `PostgreSQL (\d+)\.?(\d+)?`. Also `current_schema()`, `show standard_conforming_strings`, `show transaction isolation level`, and an hstore lookup via pg_type/`to_regtype`. [V]
- **Django.** Requires server_version ≥ 16, and calls `set_config('TimeZone', …)` when the zone differs. [V]
- **GORM.** `AutoMigrate` needs information_schema plus pg_type/pg_attribute/`format_type`/pg_indexes; nothing at connect. [V]

### 10.3 Minimal adapter estimates [I]

1. **psql and libpq tools:**
   - Trust auth; 'N' to SSL/GSS requests.
   - `options -c` parsing and the full ParameterStatus set with `server_version` like "17.x" (or 13.x to dodge pgbench's FREEZE path).
   - Multi-statement simple Query.
   - ErrorResponse with Position/Detail/Hint/Context, and NoticeResponse.
   - COPY in/out (text).
   - Extended protocol.
   - Real cancel keys.
   - SET/SHOW/RESET for ~10 GUCs, `set_config`/`current_setting`.
   - VACUUM as a no-op.
2. **pgx:** the above, plus correct ParameterDescription/RowDescription OIDs at Describe, binary codecs (reuse `pgtype`), named statements/Close, and pipelining with error-until-Sync. No catalog needed.
3. **pgjdbc:** the above, plus PortalSuspended, minimal pg_type/pg_namespace, and the ResultSetMetaData join. DatabaseMetaData-grade catalog only for BenchBase.
4. **ORMs:** the above, plus `version()`, `current_schema()`, a few SHOWs, and information_schema/pg_catalog views for migrations. A read-only virtual catalog generated from engine metadata scales better than string interception.

---

## Sources (main)

- PostgreSQL source, `src/test/regress/{pg_regress.c,pg_regress_main.c,parallel_schedule,GNUmakefile,sql/test_setup.sql,expected/int4.out}`, `src/test/isolation/{isolationtester.c,README,Makefile,specs/*,expected/*}`: https://github.com/postgres/postgres
- Doltgres regression tool: https://github.com/dolthub/doltgresql/tree/main/testing/go/regression/tool ; PR comment https://github.com/dolthub/doltgresql/pull/3808 ; blogs https://www.dolthub.com/blog/2024-11-12-doltgres-correctness-update/ , https://www.dolthub.com/blog/2025-07-08-doltgres-correctness-testing/ , https://dolthub.com/blog/2026-07-10-doltgres-99-percent-sql-logic-tests/ , https://dolthub.com/blog/2026-08-06-doltgres-1-0/
- datafusion-testing: https://github.com/apache/datafusion-testing ; regenerate script https://github.com/apache/datafusion/blob/main/datafusion/sqllogictest/regenerate_sqlite_files.sh ; runner https://github.com/apache/datafusion/tree/main/datafusion/sqllogictest ; README https://github.com/apache/datafusion/blob/main/datafusion/sqllogictest/README.md
- sqllogictest-rs: https://github.com/risinglightdb/sqllogictest-rs ; sqllogictest about: https://raw.githubusercontent.com/gregrahn/sqllogictest/master/about.wiki
- sqltest: https://github.com/elliotchance/sqltest ; NIST: https://www.nist.gov/publications/users-guide-sql-test-suite-version-60-0
- SQLancer: https://github.com/sqlancer/sqlancer ; SQLancer++: https://github.com/suyZhong/SQLancerPlusPlus , https://arxiv.org/abs/2503.21424 ; SQLsmith: https://github.com/anse1/sqlsmith ; EET: https://github.com/JZuming/EET
- Jepsen: https://github.com/jepsen-io/postgres , https://github.com/jepsen-io/sql , https://github.com/jepsen-io/duckdb , https://github.com/jepsen-io/elle ; Hermitage: https://github.com/ept/hermitage ; IsoVista: https://github.com/hengxin/IsoVista
- pgx: https://github.com/jackc/pgx ; CRDB roachtests: https://github.com/cockroachdb/cockroach/tree/master/pkg/cmd/roachtest/tests ; YugabyteDB regress README: https://github.com/yugabyte/yugabyte-db/blob/master/src/postgres/src/test/regress/README ; Materialize: https://github.com/MaterializeInc/materialize/tree/main/test/sqllogictest ; RisingWave: https://github.com/risingwavelabs/risingwave/tree/main/src/tests/regress ; PGAdapter: https://github.com/GoogleCloudPlatform/pgadapter/tree/postgresql-dialect/benchmarks/pg_regress ; QuestDB compat: https://github.com/questdb/questdb/tree/master/compat ; CedarDB: https://cedardb.com/blog/postgres_compatibility/ ; PCI: https://github.com/secp256k1-sha256/postgres-compatibility-index
- Benchmarks: https://www.postgresql.org/docs/current/pgbench.html , https://github.com/pingcap/go-tpc , https://github.com/cmu-db/benchbase , https://github.com/akopytov/sysbench , https://github.com/TPC-Council/HammerDB , https://github.com/ClickHouse/ClickBench , https://github.com/brianfrankcooper/YCSB , https://www.tpc.org/TPC_Documents_Current_Versions/txt/EULA_v2.2.0.txt
