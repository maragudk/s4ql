# Non-determinism in a SQL statement

Date: 2026-11-26. Tags: **[V]** = read in a primary source in this run (the vendored amalgamation `internal/sqlite/c/sqlite3.c`, 3.54.1, or a page on sqlite.org), or measured with the Homebrew `sqlite3` shell 3.54.1 on this machine. **[U]** = not confirmed from a primary source. s4ql was read at `main`, commit `3628b9b`: [the spec](../spec.md), [the roadmap](../roadmap.md), and [the todo](../todo.md).

Question: which inputs, other than a statement's text, its parameters, and the committed state of the database, decide what a statement returns or writes, or whether it fails? Is the following list of seven exhaustive against everything SQLite supports: (1) the clock, (2) random numbers, (3) which row or which order where the statement does not fix one, (4) the host's time zone, (5) state of the connection, (6) results that depend on the order of rows, (7) the machine and the build?

The Homebrew shell has more options than s4ql's build (`STAT4`, `ENABLE_OFFSET_SQL_FUNC`, `ENABLE_PERCENTILE`, FTS, R-Tree). s4ql's build, in `internal/sqlite/options.h`, adds only `ENABLE_MATH_FUNCTIONS` and `STRICT_SUBTYPE` of these kinds, and omits `load_extension` **[V]**.

## Summary

- **The list covers the main kinds, but it is not exhaustive, and item 1 is wrong in one case.**
- **Item 1 is wrong for a query of several rows.** SQLite fixes `'now'` for one call of `sqlite3_step()`, not for one statement. `sqlite3VdbeExec` sets `p->iCurrentTime = 0` on every step **[V]**, and lang_datefunc.html says "within the same sqlite3_step() call" **[V]**. A `'now'` with constant arguments is factored out and computed once per run, so it does not show. A `'now'` whose arguments read a column is computed per row, and a select of 100,000 rows gave 41 distinct times **[V]**. A write without `returning`, and a write with `returning` (computed in the first step), gave one value **[V]**. s4ql's rule of one value per statement is still one of SQLite's possible answers.
- **Missing, in order of importance:**
  1. **The order in which a write visits rows decides what it writes or whether it fails.** Item 3 lists only what a statement returns. An `insert ... select ... on conflict do update` with two source rows of one key wrote 200, and with the order reversed wrote 100. `update ... from` with two matches did the same **[V]**. `update t set k = k + 1` fails or succeeds by order; quirks.html §15 says "the success or failure of the statement depends on an arbtrary ordering choice of the query planner" **[V]**.
  2. **Which rows are evaluated decides whether a statement fails.** `select k from t where abs(v) > 0 limit 1` gave row 1 in one order and `integer overflow` in the other **[V]**. s4ql's own notes record it for arithmetic ([todo/arithmetic.md](../todo/arithmetic.md)).
  3. **A representative of equal values.** Where values compare equal but differ, which one comes back depends on order: `max(x collate nocase)` of `'a'` and `'A'` gave either; `distinct` of `1` and `1.0` gave `integer` or `real` **[V]**. Bare columns and `group by` without aggregates pick an "arbitrarily selected row" (lang_select.html) **[V]**.
  4. **The query planner's statistics.** `sqlite_stat1` changed the index a query used, and so its order: `18,148,278,...` against `18,109,148,...` for the same rows **[V]**. Item 3 names the order, not this cause.
  5. **The physical layout of the file.** `sqlite_offset(X)` gives a byte offset (it is flagged deterministic), and `pragma page_count`, `freelist_count`, `schema_version`, `data_version`, and the `rootpage` of `sqlite_schema` read the file, not the rows **[V]**. Two caches with the same commits can differ in them.
  6. **Random names of result columns.** The fifth duplicate name of a column in a subquery, view, or `create table ... as select` gets a random suffix: `a,a:1,a:2,a:3,a:4,a:4240065158`, and another run gave `a:279010675` (`sqlite3ColumnsFromExprList`, `sqlite3_randomness`) **[V]**. It changes names returned, and with `create table ... as select`, the schema written.
  7. **Settings and failures of resources.** Pragmas and `sqlite3_db_config` options (`reverse_unordered_selects`, `case_sensitive_like`, `recursive_triggers`, `defer_foreign_keys`, `foreign_keys`, `query_only`), `sqlite3_limit`, the heap limits, `temp_store`, a full disk, `sqlite3_interrupt`, the progress handler, and `SQLITE_BUSY` **[V]**. Item 5 names only the three counters of the connection; item 7 names limits but not the run-time settings.
  8. **An integer `sum` overflows by order.** `sum` of `max, 1, -1` failed with `integer overflow`, and of `max, -1, 1` gave the maximum **[V]**. Item 6 names `sum` only for reals.
- **Smaller corrections.** Item 6 for reals: SQLite's `sum` uses compensated (Kahan-Babuska-Neumaier) summation, so ordinary inputs gave the same bits in any order over 20,000 values **[V]**; order still decides overflow to infinity (`1e308, 1e308, -1e308` gave `Inf`, the other order `1.0e+308`) **[V]**. Item 7: SQLite 3.54.1 does not use `long double`, and float to text conversion is SQLite's own 64-bit integer code (floatingpoint.html, 3.52.0) **[V]**; the math functions call the host's `libm`, and Go's `math` differs from macOS's `libm` in the last bit for `ln(3)` and `tan(1.2)` **[V]**. `printf('%p', 255)` gives `FF`, so `%p` does not leak a pointer **[V]**.

## Every kind, with its source and its reach

"Reach" says what the kind can change: **W** the rows a statement writes, **R** the rows or values it returns, **F** whether it fails. "s4ql" says whether a statement that s4ql supports today or plans can reach it, by the roadmap.

| # | Kind | Source | Reach | s4ql |
| --- | --- | --- | --- | --- |
| 1 | Clock: `'now'`, `current_time`, `current_date`, `current_timestamp`, `'subsec'` | `sqlite3StmtCurrentTime` caches in `Vdbe.iCurrentTime`, reset to 0 in `sqlite3VdbeExec` on each step; `sqlite3OsCurrentTimeInt64`; lang_datefunc.html "within the same sqlite3_step() call" **[V]** | W, R, F | No functions yet. Decided: the host's clock, once per statement ([spec](../spec.md)) |
| 2 | `'now'`, `'localtime'`, `'utc'`, `'subsec'` in a check, a generated column, an index expression, or a partial index fail | `sqlite3NotPureFunc` errors under `OP_PureFunc`: "non-deterministic use of datetime() in a CHECK constraint" **[V]**; deterministic.html **[V]** | F | No expression in a check, index, or default yet; `default` takes a literal only |
| 3 | Random numbers: `random()`, `randomblob(N)` | `randomFunc`, `randomBlob` call `sqlite3_randomness`, a ChaCha20 stream seeded once per process by `sqlite3OsRandomness` **[V]**; new values per call **[V]** | W, R | No functions yet. Decided: the host's source ([spec](../spec.md)) |
| 4 | Host time zone and its DST rules | `osLocaltime` calls `localtime_r`; `TZ=America/New_York` and `TZ=Asia/Tokyo` gave 08:00 and 21:00 for 12:00 UTC **[V]**; lang_datefunc.html: "the standard C library function localtime_r() is used" **[V]** | W, R | Not reachable; undecided |
| 5 | Order of returned rows: no `order by`, ties under `order by`, `limit` without order, `group by` output | lang_select.html: "the order in which the rows are returned is undefined", ties "undefined" **[V]**; `whereReverseScanOrder` **[V]** | R | Reachable. Shut out by design: the spec varies it |
| 6 | Order of `returning` rows | lang_returning.html: "The output order for the RETURNING rows is arbitrary" **[V]** | R | Reachable; s4ql draws a direction (slice 14) |
| 7 | Scalar subquery of several rows | lang_expr.html: "the first row of the result" **[V]**; gave 100 or 200 by order **[V]** | R, W, F | Reachable in `where` of select, update, delete (slice 15); the drawn direction varies it |
| 8 | Order of visit decides writes: upsert from `insert ... select`, `update ... from` | upsert gave 200 or 100, `update ... from` 200 or 100 **[V]**; lang_update.html: one row "is used", "arbitrary" **[V]** | W | Not yet. Upserts and `insert ... select` are decided; `update ... from` undecided |
| 9 | Order of visit decides failure: constraint checked per row, error of an expression on a row | quirks.html §15 **[V]**; `abs(min)` with `limit 1` **[V]** | F | Reachable with arithmetic (overflow, division by zero) and `limit`; s4ql changes keys in key order, as SQLite does for `without rowid` ([todo/arithmetic.md](../todo/arithmetic.md)) |
| 10 | Representative of equal values: min/max with ties, `distinct`, `group by`, `union`, under `nocase` or integer against real | measured above **[V]**; lang_aggfunc.html: max is "the value that would be returned last in an ORDER BY" **[V]** | R, W | Not yet: no aggregates, `distinct`, or collations. Strict types make integer against real rarer |
| 11 | Bare columns in an aggregate | lang_select.html: "evaluated once for an arbitrarily selected row" **[V]**; quirks.html §6 **[V]**; shell gave 1, and 3 reversed **[V]** | R, W | Not yet; aggregates undecided |
| 12 | Order-dependent aggregates: `group_concat`, `string_agg`, `json_group_array`, `json_group_object` | lang_aggfunc.html: "arbitrary unless an ORDER BY argument is included" **[V]**; reversed gave `3,2,1` **[V]** | R, W | Not yet |
| 13 | Order-dependent sums: integer `sum` overflow; real `sum`, `total`, `avg` overflow to infinity, or last bits | `sumStep`; lang_aggfunc.html: "integer overflow" "if all inputs are integers or NULL" **[V]**; measured **[V]** | R, F | Not yet |
| 14 | Window functions without order: `row_number() over ()` and the rest | no `SQLITE_FUNC_CONSTANT` on any window function (`WINDOWFUNCALL`, `WINDOWFUNCX`, `WINDOWFUNCNOOP`) **[V]** | R, W | Not planned |
| 15 | Query planner statistics: `sqlite_stat1`, `sqlite_stat4`, `analyze`, `pragma optimize` | plan and order changed by a hand-written `sqlite_stat1` **[V]**; pragma.html: optimize may "run ANALYZE" **[V]** | R, F | Shut out: s4ql chooses the access by fixed rules, and rejects names that start with `sqlite_` |
| 16 | Physical layout: `sqlite_offset`, `pragma page_count`, `freelist_count`, `schema_version`, `data_version`, `rootpage`, `dbstat`, `sqlite_dbpage` | corefunc: "the byte offset in the database file" **[V]**; `INLINE_FUNC(sqlite_offset, ...)` has `SQLITE_FUNC_CONSTANT` **[V]** | R | Shut out today: `sqlite_offset` is not in s4ql's build; pragmas and schema reads not supported |
| 17 | Random column names of duplicates in a subquery, view, or `create table ... as select` | `sqlite3ColumnsFromExprList`: `if( cnt>3 ) sqlite3_randomness(...)` **[V]**; measured **[V]** | R (names), W (schema) | Not yet: no subquery in `from`, views and `create table ... as select` open |
| 18 | Rowid of a new row when the largest is taken; `autoincrement` and `sqlite_sequence` | `OP_NewRowid`: "starts picking positive candidate ROWIDs at random" (`useRandomRowid`) **[V]**; autoinc.html **[V]**; `autoincrement` then fails with `SQLITE_FULL` **[V]** | W, R, F | Shut out: `strict, without rowid` with a primary key is required |
| 19 | Connection counters: `changes()`, `total_changes()`, `last_insert_rowid()` | `VFUNCTION`, no `SQLITE_FUNC_CONSTANT` **[V]** | R, W | Not yet as functions; `RowsAffected` counts as SQLite does. An insert into a `without rowid` table leaves `last_insert_rowid()` as it was: 1 from an earlier rowid table **[V]** |
| 20 | Connection settings: `reverse_unordered_selects`, `case_sensitive_like`, `recursive_triggers`, `defer_foreign_keys`, `foreign_keys`, `query_only`, `sqlite3_db_config`, temp tables that shadow `main`, attached databases | pragma.html **[V]**; `like` gave 1 and 0 **[V]** | W, R, F | Shut out: "No settings that change behaviour, for now" |
| 21 | Limits: `sqlite3_limit` (length, columns, expression depth, compound select, variables, trigger depth, LIKE pattern), parser stack | limits.html **[V]**; compile options of the shell **[V]** | F | Reachable: the length limit of 1,000,000,000 is SQLite's |
| 22 | Resources: memory (`SQLITE_NOMEM`, heap limits), temporary files and a full disk, I/O errors | `sqlite3_hard_heap_limit64`; sorter spills to a temporary file ([memory-bounds.md](memory-bounds.md)) **[V]** | F | Reachable in the cache; a local I/O failure stops the instance ([spec](../spec.md)) |
| 23 | Interruption and locks: `sqlite3_interrupt`, progress handler, `SQLITE_BUSY`, busy timeout | `sqlite3VdbeExec` checks `isInterrupted` and `xProgress` **[V]**; [sqlite-busy.md](sqlite-busy.md) **[V]** | F | Reachable in the cache; s4ql maps a cancelled context |
| 24 | Version and build: `sqlite_version()`, `sqlite_source_id()`, `sqlite_compileoption_get/used()` | `DFUNCTION`, `SQLITE_FUNC_SLOCHNG` **[V]** | R | Not yet |
| 25 | Math functions from `libm` | `MFUNCTION` passes `sin`, `pow`, `exp` of the C library **[V]**; Go differs in the last bit for `ln(3)`, `tan(1.2)` **[V]** | R, W | Not yet; s4ql's evaluator in Go would differ from SQLite there |
| 26 | Triggers: a `before` trigger that changes its row; foreign key cascades by row order | lang_createtrigger.html: "the result of the subsequent update or delete operation is undefined" **[V]** | W, F | Triggers deferred; foreign keys undecided |
| 27 | `replace` and `or replace`: which conflicting rows are deleted, by order | lang_conflict.html **[U]** for order | W | Shut out: rejected |
| 28 | Logging: `sqlite_log()`; extensions: `load_extension()` | `SFUNCTION`, `SQLITE_DIRECTONLY`, `SQLITE_FUNC_UNSAFE` **[V]** | none on data (log); any (extension) | `load_extension` is not in the build; virtual tables are rejected by name |

What SQLite does not do: no hash randomization of order, no `long double` in conversions, `printf('%p')` prints the integer argument in hex, and the order of index checks on a violation of two unique indexes stayed the same across connections (`t.b` three times) **[V]**.

## s4ql's own kinds

These have nothing to do with SQLite, as the spec, roadmap, and todo show them.

| Kind | Source | Reach |
| --- | --- | --- |
| Drawn scan direction for a read without a fixed order, and for the rows of `returning` | [spec](../spec.md) "Unspecified row order varies"; [todo/slice-14.md](../todo/slice-14.md) | R; with `limit` or a subquery also F, and so W where the subquery decides the rows |
| Access by fixed rules: a key or index bound guards every other condition | [todo/arithmetic.md](../todo/arithmetic.md) | F: which rows are evaluated |
| Cancelled context | [todo/slice-14.md](../todo/slice-14.md), [todo/slice-03.md](../todo/slice-03.md) | F, and an unknown commit outcome |
| No valid lease, or a lost lease | [spec](../spec.md) "An instance without a valid lease refuses and keeps trying" | F: a temporary error, and an open write transaction rolled back |
| Failed or unknown commit: the manifest swap | [spec](../spec.md) "The manifest swap is the commit", "An unknown commit outcome is resolved from S3" | F, or "outcome unknown" |
| Commit too large: `s4ql.ErrTooLarge` at 5 GiB or the store's limit | [spec](../spec.md) | F, by the store |
| Memory limit of an operator that holds rows | [roadmap](../roadmap.md) "Limits of the core", decided, size open | F |
| Local I/O failure stops the instance | [spec](../spec.md) | F, for good until reopened |
| Single writer: a second writer waits | [spec](../spec.md) "One write transaction at a time" | F only through a context that ends while it waits |

## Built-in functions not flagged deterministic in 3.54.1

A built-in function is deterministic when its `FuncDef` has `SQLITE_FUNC_CONSTANT` (0x800, the same bit as `SQLITE_DETERMINISTIC`). Flags read from the macros in `sqlite3.c` **[V]**:

- `FUNCTION`, `FUNCTION2`, `MFUNCTION`, `JFUNCTION`, `LIKEFUNC`, `INLINE_FUNC`, `TEST_FUNC`, `INTERNAL_FUNCTION`: with `SQLITE_FUNC_CONSTANT`.
- `VFUNCTION`: `BUILTIN|UTF8`, without it.
- `DFUNCTION`: `BUILTIN|SLOCHNG|UTF8`, without it.
- `SFUNCTION`: `BUILTIN|UTF8|DIRECTONLY|FUNC_UNSAFE`, without it.
- `PURE_DATE`: `BUILTIN|SLOCHNG|UTF8|CONSTANT`. Deterministic unless an argument is `'now'`, `'localtime'`, `'utc'`, or `'subsec'`, where `sqlite3NotPureFunc` makes the use non-deterministic.
- `WAGGREGATE`, and the window macros: `BUILTIN|UTF8` plus the flags given, without `CONSTANT` unless it is given.

`SQLITE_FUNC_SLOCHNG` means "Value constant during a single query"; a call with constant arguments of a `CONSTANT` or `SLOCHNG` function is computed once per run (the comment near line 115459) **[V]**.

| Function | Macro | Flags beyond `BUILTIN|UTF8` | In s4ql's build |
| --- | --- | --- | --- |
| `random()` | `VFUNCTION` | none | yes |
| `randomblob(N)` | `VFUNCTION` | none | yes |
| `last_insert_rowid()` | `VFUNCTION` | none | yes |
| `changes()` | `VFUNCTION` | none | yes |
| `total_changes()` | `VFUNCTION` | none | yes |
| `current_time()` | `DFUNCTION` | `SLOCHNG` | yes |
| `current_date()` | `DFUNCTION` | `SLOCHNG` | yes |
| `current_timestamp()` | `DFUNCTION` | `SLOCHNG` | yes |
| `sqlite_version()` | `DFUNCTION` | `SLOCHNG` | yes |
| `sqlite_source_id()` | `DFUNCTION` | `SLOCHNG` | yes |
| `sqlite_compileoption_used(X)` | `DFUNCTION` | `SLOCHNG` | yes |
| `sqlite_compileoption_get(N)` | `DFUNCTION` | `SLOCHNG` | yes |
| `sqlite_log(C, M)` | `SFUNCTION` | `DIRECTONLY`, `FUNC_UNSAFE` | yes |
| `load_extension(X)`, `load_extension(X, Y)` | `SFUNCTION` | `DIRECTONLY`, `FUNC_UNSAFE` | no (`OMIT_LOAD_EXTENSION`) |
| `julianday`, `unixepoch`, `date`, `time`, `datetime`, `strftime`, `timediff` | `PURE_DATE` | `SLOCHNG`, `CONSTANT`; non-deterministic by argument | yes |
| `min(X)`, `max(X)` aggregates | `WAGGREGATE` | `NEEDCOLL`, `MINMAX`, `ANYORDER` | yes |
| `count()`, `count(X)` | `WAGGREGATE` | `ANYORDER` (and `COUNT` for `count(*)`) | yes |
| `sum`, `total`, `avg` | `WAGGREGATE` | none | yes |
| `group_concat(X)`, `group_concat(X, Y)`, `string_agg(X, Y)` | `WAGGREGATE` | none | yes |
| `median`, `percentile`, `percentile_cont`, `percentile_disc` | `WAGGREGATE` | `INNOCUOUS`, `SELFORDER1` | no (`ENABLE_PERCENTILE`) |
| `row_number`, `rank`, `dense_rank` | `WINDOWFUNCX` | `FUNC_WINDOW` | yes |
| `percent_rank`, `cume_dist`, `ntile`, `first_value`, `last_value`, `nth_value` | `WINDOWFUNCALL` | `FUNC_WINDOW` | yes |
| `lead`, `lag` (1 to 3 arguments) | `WINDOWFUNCNOOP` | `FUNC_WINDOW` | yes |

The JSON aggregates `json_group_array`, `jsonb_group_array`, `json_group_object`, and `jsonb_group_object` are `WAGGREGATE` with `SQLITE_DETERMINISTIC` given, although their result depends on the order of their input. An aggregate's flags therefore say nothing about order. `sqlite_offset(X)` is `INLINE_FUNC` with `CONSTANT`, although it reads the file's layout; it needs `ENABLE_OFFSET_SQL_FUNC`, which s4ql's build lacks. Debug-only functions (`datedebug`, `fpdecode`, `parseuri`, `oom`, `sqlite_filestat`, `sqlite_walstat`) and `unknown()` are not in s4ql's build.

## Not determined

- Whether a real `sum` or `avg` with compensated summation can differ in its last bit by order for finite results. Twenty thousand values of mixed magnitude did not; no case was constructed **[U]**.
- How far `libm` differs between macOS, glibc, and musl for the math functions; only macOS was measured, against Go **[U]**.
- Whether a compiler's contraction to fused multiply-add changes SQLite's float conversion code on some target **[U]**.
- The order in which `or replace` deletes rows that conflict on several indexes, and the order of foreign key cascades, were not measured **[U]**.
- Whether SQLite's sorter keeps ties stable when it spills, or with worker threads, was not read **[U]**.
