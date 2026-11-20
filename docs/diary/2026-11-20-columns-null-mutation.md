# Diary: Mutation testing of slices 5 and 6, several columns and `null`

Slices 5 ("several columns") and 6 (`null`) of the roadmap went through the statement pass (#72, #75), the engine and the driver (#73, #78), the core (#77), and the generated checks (#74, #79). This task runs the mutation tester `internal/sql/mutation` over the code that they added or changed, closes the gaps that surviving mutants show or lists them as equivalent, closes the open mutants of the core that slice 4 and the core of slice 6 left, and makes the thin planted bug `convertReal` robust again. The branch is `columns-null-mutation`, from `origin/null-checks` (#79).

## Step 1: The selection of mutants, the open mutants of the core, and `convertReal`

**Author:** columns-null-mutation builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "1. **Mutation testing of the code that slices 5 and 6 added or changed.** Find it with `git log` and `git diff` since the merge of pull request #71 [...], leaving out what the mutation testing of slice 4 (pull request #76) already covered and has not changed since. [...] 3. **Known open mutants, to close here if you can:** six in the core that drop the error of creating or dropping a temporary table inside a transaction (`Tx.touch`, `Tx.dropTouched`, `Tx.dropTemps`) [...]; two in `conn.head` in `internal/core/cache.go` [...]. 4. **One planted bug of the engine's simulation has become thin:** `convertReal` is found in only 5 of 200 seeds [...]. Make it robust again without weakening what it shows [...]"
**Interpretation:** Mutate only the functions with lines that slices 5 and 6 changed, rerun only survivors, write the tests that the eight open mutants of the core need, and give `convertReal` more chances to be found.
**Inferred intent:** The same trust in slices 5 and 6 as in slices 1 to 4, in a run that takes less time than that of slice 4.

### What I did

The selection is by function. `go run ./internal/sql/mutation -list -pkgs internal/sql/stmt,internal/engine,internal/core,internal/format,.` lists 12,225 mutants. A throwaway script in the scratchpad runs `git blame -w --line-porcelain HEAD` on each mutated file and keeps the lines whose commit is in `f842ffb..HEAD` but not in `eee399f^1..eee399f^2`, the commits of #76 alone; every function with a mutant on such a line is selected, with all its mutants. That gives 2,749 mutants in 70 functions: 608 in `stmt` (`recognizer.column`, `createTable`, `insert`, `literal`, `parameter`, `resultColumn`, `selectCmd`, `isNull`, `maxParameters`, `CreateTable.ColumnsError`, `CreateTable.Key`, `Insert.Parameters`, `maxColumns`, `fieldNames`), 1,056 in the engine, 934 in the core, 97 in the format (`checkCreateTable`, `checkPrimaryKey`), and 54 in the driver (`argument`, `check` of `conn.go`). A second script turns the list into a `-run` expression per package of the form `^(file (func|func) |...)`. The runs work on `rsync` copies of the worktree in the scratchpad, `stmt` and the engine with 3 workers each and `-factor 5`, the core with 3 workers and `-factor 3`, on a copy that already has the new tests of the core.

The tests of the core, in `/internal/core/core_test.go` and `/internal/core/tx_test.go`:

- `TestOpen_cache` "should fail to open a cache of version 1 or 2 as a local failure, and never as a cache of another version or database, whichever read of the open fails": for a cache of each version, a copy of the file for each try, and a fault of `faultvfs` that fails the read after `after` reads, for `after` from 0 until an open succeeds. Every open that fails must be `ErrLocal` and neither `ErrCacheVersion` nor `ErrCacheDatabase`. Version 1 has 10 reads in its open, version 2 has 4. Both mutants of `conn.head` give `it records 0 positions` at the read 3 of version 1.
- `TestTx_tooBig` "should fail the transaction, and write no commit object, where the temporary tables of the keys that it touched cannot be created or dropped": a transaction upserts 4,000 keys of 900 bytes into a table that existed, so that its temporary tables spill to the temporary file, and then the reads and writes of the temporary files fail at the upsert into a second table (its temporary tables are created), at `DropTable` of the first, or at the commit. For the commit, the keys are of rows that the transaction inserts and deletes, so the commit object is one row, and a wrapper of the spool file (`armingSpool`, through `Options.WrapSpools`) arms the fault at the one write of the spool, after the commit read the temporary tables and before it drops them. Each step must fail with `ErrLocal` and leave the head of the log as it was.

Each of the eight mutants, applied by hand through `go test -overlay` with a throwaway script, fails the new tests: the create gives `sqlite: no such table: temp.s4ql_touched_2 (code 1)`, which is no local failure, and the drops give nil.

`convertReal`: in `/internal/engine/simulation_test.go`, `TestSimulation_plantedBugs` runs it in the hammer workload in place of the default one. With `S4QL_ENGINE_SIM_SEEDS=200`, 20 of 200 seeds find it there, 19 by its own violation, against 5 in the default workload, and the default run finds it the second time at seed 39, where it did at seed 62. The run of all twelve bugs passes, each found twice by its own violation, and every seed that finds a bug finds nothing without it. `/internal/engine/README.md` says so.

### Why

The run of slice 4 took hours because each survivor runs every stage, the tests of every package that depends on the mutated one; mutating only the changed functions keeps the number of survivors small. A copy of the tree with the new tests of the core kills the eight known mutants in the first run, instead of in a rerun.

The sequences of `stmttest` are shared by the engine's differential test, `drivercheck`, the pass, and the simulation, and each of them pins counts of what the sequences reach; a builder of the next slice changes them at the same time. Giving the generator whole reals for INT columns more often would change every random stream and every pinned count. The workload of the bug is local to the simulation, and the check that finds it, the type of every value of the log, is the same in both workloads.

### What worked

Failing one read after a number of reads, for every number until the open succeeds, reaches every statement of the open without naming one, and the test needs no knowledge of which page a statement reads. The spool's wrapper gives a point in the commit between its reads of the temporary tables and their drop, which no other hook has.

### What didn't work

- The first form of the script ran in one Bash call with a heredoc and `git -C`, and the sandbox refused it: "This agent is isolated in the worktree [...], but this command is too complex to verify that it stays inside the worktree." The script went into a file in the scratchpad, run with `python3 -I`.
- The first try at the commit armed the fault for every write of the temporary files before `Commit`: the mutant of `dropTemps` failed the commit anyway, `commit: the instance has stopped: local I/O failure: commit: sqlite: disk I/O error (code 778)`, because the commit's read of the temporary tables, which needs room in SQLite's page cache, spills a page first. Hence the arm at the write of the spool.
- A first threshold of `failed > 5` opens for the reads of the open failed with `only 4 opens of version 2 failed`.
- `vfs.Failures()` counts all failures since the VFS was made, not since `Fail`, so the test compares it with its value before each open.

### What I learned

A failed write of a spill puts SQLite's pager of the temporary database into its error state, and a statement of the core after it fails with `disk I/O error`, which stops the instance as a local failure. A read of a cache of version 2 at open reads only 4 times from the files: the header, page 1, and the page of `s4ql_cache`; the page cache keeps them across the statements of the open.

### What was tricky

A drop of a temporary table needs the temporary file only when the page cache of the temporary database is full of dirty pages, since it can evict a clean page without a write. Right after the upserts the cache is full of dirty pages; after the commit's read of the tables it is mostly clean, so at the commit only the reads of the drop fail, which is why the fault there is of reads and writes.

### What warrants review

- `/internal/core/tx_test.go`: the subtest of the temporary tables and `armingSpool`, and whether the comment on why the drop needs the file holds.
- `/internal/core/core_test.go`: the loop over the reads of the open, and the close of an instance that opened with the fault armed, whose error is ignored because the lease task can read the cache before the fault is disarmed.
- The choice of the hammer workload for `convertReal` over a change of the sequences.

### Future work

The results of the runs of `stmt`, the engine, the core, the format, and the driver.

## Step 2: The survivors of `stmt`, the engine, the format, and the driver, and a faster run of the core

**Author:** columns-null-mutation builder

### Prompt Context

**Verbatim prompt:** (from the brief, as in step 1) "**For each mutant that survives:** either it is equivalent, and goes on the list with the reason, or a test is missing, and you add the test that kills it. Prefer strengthening a generated or table-driven test over a one-off test." And from the lead during the run: "Markus decided on 2026-11-20 that the in-place upgrade of a version 1 cache to version 2 is removed [...]. So spend no more effort on mutants of `upgrade` and `checkNoNull` in `internal/core/cache.go`, or on the parts of `setUp` and `head` that exist only for the upgrade: do not write tests for them and do not add equivalents for them; list them in your report as \"left, code to be removed\"."
**Interpretation:** Classify each survivor, with a probe of the original against the mutant where the reason is not obvious, and leave the upgrade alone.
**Inferred intent:** A list of equivalent mutants that holds only what no test can tell apart, and no wasted work on code that is about to go.

### What I did

The first runs: `stmt` 608 mutants in 50 min, the engine 1,056 in 1 h 25 min, the driver 54 in a few minutes, the format 97 in 37 min, each with 2 or 3 workers at a load of 25 to 30 on 10 cores.

To tell the reject of a mutant from the original's, a throwaway probe in the scratchpad adds a test file to `internal/sql/stmt` through `-overlay` that prints `stmt.Check` for a list of texts, and a script runs it on the original and on each change of one line, and diffs the outputs. It showed that the guards `r.isNext(...)` of `recognizer.createTable`, `recognizer.column`, and `recognizer.resultColumn` matter only when an earlier token of the same column or select is left untaken, which no case of `TestCheck` had: ON CONFLICT or ASC before a second PRIMARY KEY or a NULL, NOT NULL ON CONFLICT in a table without a key, and DISTINCT before `*`, TRUE, or a literal. These are cases of `TestCheck` in `/internal/sql/stmt/check_test.go` now. The guards of a parameter beyond SQLite's limit and of a column named twice in the list of an insert are reached only by trees that the parser never gives, which `TestRecognize` in `/internal/sql/stmt/recognize_internal_test.go` gives now, as it does for the other parameter guard.

The engine's `pin` in `/internal/engine/engine_test.go` compares the offset of every error with SQLite's. That killed seven mutants of the offsets of `noSuchResultColumn`, `valuesSupplied`, and `valuesForColumns`, and showed a difference from SQLite: `CreateTable.ColumnsError` gave `duplicate column name` and `too many columns` the offset of the column's name, where SQLite gives -1. The offset is not public and s4ql does not promise it, but the fix is one value: `Offset: -1` in `/internal/sql/stmt/stmt.go`, and `checkCreateTable` in `/internal/sql/stmt/stmt_test.go` compares it with SQLite's.

Other gaps: `TestInsert` has a row of two values from one expression; `TestNewPlan` a select of one column; `TestValues` a row of another number of values than its positions; and the reader's and the writer's tables of refusals in `/internal/format` say why every `create table` is refused, with a key outside the columns at another position than the number of columns, so that the swap of the two numbers shows.

The core: the hand-written stage of the core took 4 min 52 s on the code as it is, with `TestTx_*`, which kill the mutants of `Tx.Insert`, at the end of it, so a mutant that they kill took minutes. `-first` of `/internal/sql/mutation/main.go` and `run.go` names the hand-written tests of the mutated package that run first, in a stage of their own; with 15 of the core's tests (22 s) first, a killed mutant takes about 15 s. The core run also mutates only the changed lines of the five large functions that #76 mutated and slices 5 and 6 touched only in a line or two (`conn.load`, `table.checkValue`, `Tx.spoolCommit`, `Tx.spoolTouched`, and `Open`): 76 mutants of 520. After the lead's message, `conn.upgrade` and `conn.checkNoNull` are out of the run, which leaves 411.

New tests of the core from its survivors: `TestOpen_cache` fails every lock, write, and sync of the make of a new cache, and requires SQLite's I/O error and a cache that the next open makes; it refuses a database that holds tables and no `s4ql_cache`; the test of the reads of the open keeps the store out of reach, so that the lease task does nothing with the cache, and requires that an open that succeeds had no failed read. `TestTx_null` checks why a NULL is refused, and that the cache, of the transaction and of a replay, declares a column that does not take NULL as NOT NULL.

### Why

A guard that only an untaken earlier token reaches looks redundant in every common text, so the probe of many near texts at once was the fastest way to find the one text that tells it apart.

### What worked

The probe script and `mutant.sh`, which apply one change through `-overlay` and run one test, check a candidate test against a mutant in seconds, where the tool needs minutes of planning.

### What didn't work

- The scratchpad is shared by sessions: a directory `mut` of another session was there, and my `mutant.sh` had written into it before I moved my files to `cnm/`.
- `Write` refused `.../scratchpad/probe/zz_probe_test.go`: "This write was blocked because the path is spelled in a form that cannot be safely resolved"; `probe` there was a file of another session.
- A test that also failed every lock of the open found only one lock per open (`only 1 opens of version 1 failed at a lock`): in the journal mode wal the connection takes the lock of the file once. I dropped it.
- `fmt.Sprint("engine: internal error: ...")` in the new subtest of `TestValues` failed with `but got "s4ql: internal error: a row of 2 values for 1 columns"`.
- The first core run took 9 min to plan and then 17 min for its first survivor; with the tests of `Tx` at the end of its hand-written stage, it would have taken more than a day.

### What I learned

- In the journal mode wal, `begin immediate` on a new cache touches no file that `faultvfs` fails, and the query of `sqlite_schema` after the pragma of wal reads no page, so their errors are reachable only by busy or for want of memory.
- A planted bug that the default run finds rarely makes its mutants time out in the simulation, since the test runs all 100 seeds; a rerun with `-min-time 5m` gives each a verdict.

### What was tricky

The keys of equal mutants in one function are numbered by an order that is not the order of the lines: in `conn.setUp`, `c.forget() =>` is the forget before the commit at line 242, and `#2` is the one at line 239.

### What warrants review

- The new cases of `TestCheck` and `TestRecognize`, and the reasons of the new entries of `equivalent.txt`.
- `Offset: -1` of `ColumnsError`.
- `-first` in the tool.

### Future work

The rest of the core's run and the reruns of the survivors on the final tests.
