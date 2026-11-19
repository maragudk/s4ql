# Diary: Generated checks of transactions through `database/sql`

Slice 3 was merged as `0818823` with hand-written tests of the driver's transactions (`/tx.go`, `/tx_test.go`). The differential test of the driver against `mattn/go-sqlite3`, `/internal/sqlite/crosscheck/drivercheck`, only began a transaction and rolled it back at once. This task makes it run generated transactions, looks for a suspected race between `db.Close()` and a statement of a transaction whose rows are open, and judges two small faults reachable through `sql.Conn.Raw`. The work is on the branch `tx-checks-driver` from `origin/main` at `0818823`.

## Step 1: Generated transactions in `drivercheck`

**Author:** tx-checks-driver builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`TestGenerated`, `TestSequences`, and `FuzzSequence` run sequences in which a generated run of calls happens inside a transaction, on both sides, and compare every outcome, the result of the commit or the rollback, and the tables after. [...] The oracle must begin its transactions as s4ql does (a writer from the beginning), so that both sides mean the same. Where the two legitimately differ by the roadmap [...] the case is checked against s4ql's rule and counted with a pin, never silently excused. Required outcome kinds in `TestGenerated` grow to cover each bullet above. Nothing that s4ql runs may escape the comparison."
**Interpretation:** Make a transaction a state of the pair of databases, with calls that begin, end, and run in it, and drive them from the generator, the hand-written sequences, and the fuzz target.
**Inferred intent:** The driver's transactions are compared with SQLite at random, as its statements and arguments already are.

### What I did

First a probe (a throwaway test in the package, deleted): mattn's `BeginTx` accepts `ReadOnly: true` and then lets an insert through, and accepts every isolation level, `IsolationLevel(99)` too. `_txlock=immediate` in the DSN makes its begin `begin immediate`: a second connection's insert gets `database is locked`. Under `pragma query_only = 1`, a create, an insert, and a drop get `attempt to write a readonly database`. `tx.StmtContext` works with a statement prepared on a `sql.Conn`. `Conn.ExecContext` on a `sql.Conn` whose transaction is open runs in it, and a second `Conn.BeginTx` gets `cannot start a transaction within a transaction`.

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`:

- A `runner` interface (what `sql.DB`, `sql.Conn`, and `sql.Tx` share) beside `queryer`, and `inTx`, a `sql.Tx` with the queryer it began on.
- New calls, appended to `calls` so that the old bytes of the fuzz target keep their meaning: `begin read-only`, `commit`, `rollback`, `prepare outside, exec in the transaction` and `... query ...` (Prepare on the `DB` or `Conn`, run through `tx.StmtContext`), `query, rows left open`, and `query beside the transaction`. `begin` now keeps its transaction open; its text names the isolation level.
- `pair.tx` holds the transaction of both sides. `pair.begin` begins on both (the oracle is opened with `?_txlock=immediate`, and a read-only one runs `pragma query_only = 1`); a level that s4ql refuses is checked against its message and not begun in SQLite; on a pinned connection whose transaction is open, `pair.beginAgain` requires s4ql's `s4ql: a transaction is open on the connection` (no sentinel, no `*s4ql.Error`) and SQLite's own refusal; through the pool, a begin with a transaction open commits it first, because s4ql's second write transaction would wait for the first. `pair.end` resets `query_only`, commits or rolls back both, compares the outcomes, and compares every table.
- `pair.where` sends a call into the transaction, except `query beside`: on a pinned `Conn` it runs on the `Conn` (inside the transaction), through the pool on the `DB` outside it, and only for a select, because a write beside a write transaction would wait.
- `pair.openRows` runs a query again on both sides after its comparison, reads the first row of each, and leaves the rows open. `pair.whileRowsOpen` then requires s4ql's refusal (`rowsOpen`) for a call that s4ql would run (`stmttest.Call.Runs` in every run), not at prepare; a text or argument error is compared as without open rows. An end with rows open is compared too (database/sql closes them first).
- `compare` takes the oracle's runner. Through the pool, mattn's Prepare outside the transaction can fail on a name that only the transaction has; then mattn's statement is prepared in the transaction, which is where database/sql prepares it again, and the case is pinned (`excusedOutside`).
- `tally.add` counts each outcome once plain and once under the scope (`write transaction: ...`, `read-only transaction: ...`, `beside a write transaction: ...`); `pair.end` counts each end with where it ran and what the transaction did. `TestGenerated` requires 60 more kinds.
- `pick` begins a transaction one time in ten outside one, ends it one time in seven inside (a roll back one time in three), and in a transaction picks among the new calls; `pickLevel` picks serializable or another level one time in eight each.
- Six hand-written sequences: a write transaction that commits (arguments, prepared statements in and outside, a failed statement in the middle), one that rolls back (create and drop inside), a read-only transaction, rows left open, isolation levels and a second begin, and a query beside.
- `TestPair` checks that the oracle's write transaction is the writer (`database is locked` with `busy_timeout = 0`); it fails with `_txlock=deferred`.

Pinned now: 1,051 errors at prepare (was 1,120), 12 tables started (13), 364 wrong numbers of arguments (412), 109 arguments not supported (103), and the new 67 isolation levels, 44 statements while rows are open, 28 second begins, and 43 statements prepared outside. 200 sequences make 499 transactions: 240 write commits, 102 write roll backs, 119 read-only commits, 38 read-only roll backs.

Commands: `go test -count=1 ./drivercheck` in `/internal/sqlite/crosscheck`; `go test -count=20 -run 'TestGenerated|TestSequences|TestPair|FuzzSequence' ./drivercheck` (66 s); the same with `-race -count=5`; `go test -run XXX -fuzz FuzzSequence -fuzztime 30s ./drivercheck` (5,482 inputs); `golangci-lint run ./...` (0 issues). Two mutants of `/tx.go` by hand, reverted: `Rollback` calling `c.e.Commit` failed 7 tests, and `BeginTx` ignoring `ReadOnly` failed 4.

### Why

Treating the transaction as state of the pair, and not as one call, lets the same calls run in and out of transactions, and lets the generator, the hand-written sequences, and the fuzz target share one path. Every call that s4ql runs still goes through `compare` or through a check against a rule with a pinned count.

### What worked

The comparison found no difference between s4ql and SQLite in any of the 200 sequences or the hand-written ones. The hand-written outcomes I wrote down from SQLite's rules (for example that a create table after a failed insert reports 0 changes) held on the first run; a deliberately wrong expectation failed, so they are checked.

### What didn't work

- The first run of `TestGenerated` failed in `pair.beginAgain`: `BeginTx with &{Isolation:Default ReadOnly:false} in a transaction: SQLite gives attempt to write a readonly database`. Under `pragma query_only`, SQLite refuses `begin immediate` as a write before it says that a transaction is open; the expected message now depends on the kind of the open transaction.
- Then `whileRowsOpen` failed: s4ql gave `UNIQUE constraint failed: detACH.of` where the rule says the rows are open. The open query (`select * from RELEASE`) had no rows, and `sql.Rows.Next` returning false closes the rows, so nothing was open. `openRows` now closes and forgets rows whose first `Next` finds none.
- `run` closed the statement of `tx.StmtContext` and failed with `Expected nil error, but got "no such table: t1"`: its `Close` returns the error of the prepare in the transaction that failed (the table was dropped there). The end of the transaction closes it, so the test no longer does.
- All `FuzzSequence` seeds after the first failed with `table t1 already exists` in SQLite: the oracle's DSN was `file:<TempDir>/oracle.db?...`, a URI, and the TempDir of `FuzzSequence/seed#N` holds a `#`, which ends a URI's path, so every seed opened the same file. The DSN is now a plain path with `?_txlock=immediate`, which mattn also parses; a probe showed the lock is still taken and the `#` kept.
- The Bash sandbox refused heredocs and a `cd` to `go list -m -f '{{.Dir}}'`; the probes were written with the Write tool instead, and I did not read mattn's source outside the worktree.

### What I learned

The fuzz target's progress shows bursts with seconds of `0/sec` in between; the same happens with the file from `main`, at a load average of about 15 on the machine, so it is not this change. One run of the seeds took 24 s with four seeds at 3 s and 7 s, and five later runs took 0.25 s each.

### What was tricky

Avoiding deadlocks through the pool: a write outside a write transaction waits for the writer's place in s4ql and for SQLite's lock in mattn, so `query beside` runs outside only for a select, a begin with a transaction open commits it first, and the tables are compared only after a transaction ends.

### What warrants review

- `excusedOutside` in `compare`: through the pool, mattn's outer Prepare sees the committed state, and the comparison falls back to mattn's statement prepared in the transaction. That is what database/sql does after the outer Prepare, but it is an excuse; it is pinned at 43.
- `pair.where` for `query beside` through the pool: a non-select runs in the transaction.
- The rate of the new calls in `pick`, and that the counts of `while rows are open` stay small (44 in all).

### Future work

None from this step.
