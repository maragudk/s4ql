# Diary: Slice 15 of the roadmap in the engine and the driver, the scalar subquery

Slice 15 adds the scalar subquery of goqite's receive, `update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received`, without the arithmetic in SET. The statement pass is done on `slice-15-stmt`: it reads the subquery into `stmt.Select.Subqueries` and `stmt.Term.Subquery`, and the engine and the driver refuse it. This task is the engine and the driver: plan and run the subquery, remove the refusals, and check it against SQLite. The work is on the branch `slice-15-engine`, from `slice-15-stmt` with `main` merged.

## Step 1: The brief, SQLite's behaviour, and the design

**Author:** subquery-engine builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Make the engine plan and run a statement with a scalar subquery, remove the refusals, and make it work through the driver: `select`, `update`, and `delete`, with arguments, prepared statements, transactions, `returning`, and after a restart with an empty cache. [...] Once, before, same view. [...] No row gives null [...] Several rows give the first. Where the subquery's `order by` does not decide which row is first (none, or a tie), s4ql follows the spec's rule on unspecified order [...] Correlated subqueries are refused with a clear error [...] Exact types. [...] for every pair of types, s4ql gives SQLite's rows or refuses with a clear error. [...] Plans. The subquery is planned as a select is [...] It must not read or hold more than a select with `limit 1` does. [...] Telemetry. One event per statement, as today. Add what someone needs who asks "why was this receive slow or empty" [...]"
**Interpretation:** Build the subquery into the plan, the operators, and the telemetry of the engine, with SQLite's errors in SQLite's order, a draw for the first row where the order is not decided, and the refusals of the slice; then make the differential tests take a subquery, and check the driver with goqite's receive.
**Inferred intent:** goqite's receive running on s4ql with the same trust as the slices before.

### What I did

Read the roadmap's section of slice 15, `/docs/todo/slice-15.md`, the open items in `/docs/todo/sql-front-end.md`, the diary of the statement pass, the notes of the engine step of slice 14, and the engine's plan, operators, access, telemetry, and transactions. Built the pinned shell (3.54.1) in the scratchpad and measured the order of SQLite's errors with three scripts.

### Why

The order of the errors and where SQLite looks a name up decide the phases of the build of a subquery.

### What worked

The facts, which go into `/docs/todo/slice-15.md`:

- In a select, SQLite expands `*` of every subquery before it resolves a name, so `row value misused` for `*` of several columns comes at the comparison, before its column (`nope = (select * from u)` gives `row value misused`), before the deferred error of a test for NULL of a missing column (`nope is null and k = (select * from u)`), and before the names inside the subquery (`k = (select * from u where nope = 1)`), but after the select list (`select nope from t where k = (select * from u)` gives `no such column: nope`).
- Inside the subquery, the names come in its select list, its WHERE, and its ORDER BY, with the deferred error of a test for NULL as in a statement: `(select nope3 from u where nope is null order by nope2)` gives `nope3`, and `(select k from u where nope is null order by nope2)` gives `nope`.
- In an update and a delete, `sub-select returns N columns - expected 1` comes from SQLite's code generator, whose order follows its plan: `update t set v='z' where w = (select * from u3) and k = (select * from u)` gives `2 columns`, the second subquery, since the lookup by the key `k` is coded first. With one such subquery the message is decided; with two it depends on SQLite's plan.
- A correlated name resolves without an error in SQLite, and the names after it are looked up: `k = (select v from u where nope = 1)`, with `v` a column of `t` only, gives `no such column: nope`.

### What didn't work

A Bash command with a here-doc that wrote the SQL script and ran the shell was refused: "this command is too complex to verify that it stays inside the worktree". The scripts are written with the Write tool and run with the shell's input redirected.

### What I learned

SQLite's vector check of a comparison (`row value misused`) runs in `resolveExprStep` before the walk goes down into the operands, and the walk aborts after any node once `pParse->nErr` is set, which is the mechanism of the deferred error of a test for NULL that the engine already models.

### What was tricky

Where the subquery may be undecided, the pair's two instances and SQLite may each pick another row, and their states then part. The design: a test hook that observes the values that an instance computed, and on the pair's other instance uses the first instance's values where SQLite's candidates hold more than one value, and SQLite runs the statement with the subquery replaced by a parameter bound to that value.

### What warrants review

The decisions in `/docs/todo/slice-15.md` once written.

### Future work

The plan, the operators, the telemetry, the tests, the driver.
