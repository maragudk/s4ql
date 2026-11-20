# Diary: Slice 9 of the roadmap in the statement pass, `create index` and `drop index`

Slice 9 adds `create index i on t (a, b desc)` and `drop index i`: an index that is not unique, on one or more columns of one table, each with ASC, DESC, or neither. This task is the statement pass, `internal/sql/stmt`, and its generator `stmttest`; the engine and the driver do not run either statement yet, and refuse both as not supported, with the error that the pass gave them before, until their builder comes. The work is on the branch `index-stmt`, from `ac23047` on `main`, which holds pull request #88, the statement pass of `update` and `delete`.

## Step 1: SQLite's behaviour, the typed form, the recognizer, the checks, the generator, and the refusal in the engine

**Author:** index-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: the statement pass for indexes. Only `internal/sql/stmt` and its test helper `internal/sql/stmt/stmttest`. [...] What is agreed with Markus (2026-11-20): In: `create index i on t (a, b desc)` and `drop index i`. One or more columns of the table, each with `asc`, `desc`, or neither (which means `asc`). Not unique. Rejected as not supported (SQLite accepts them): `create unique index`, a partial index (`where`), an index on an expression [...], `collate`, `if not exists` and `if exists` on an index [...], a schema other than what the other statements take, `reindex`, `analyze`, and, in the statements that exist, `indexed by` and `not indexed`, and `explain` / `explain query plan` in front of a statement (check how the pass answers these today and keep or add the answer "not supported" with a name of its own for each). [...] 1. New statement kinds `stmt.CreateIndex` [...] and `stmt.DropIndex` [...] 2. The rule of the package holds without exception [...] Find out from SQLite itself [...] 4. `stmttest`: extend the `Generator` [...] switched off by default [...]"
**Interpretation:** Recognize the two statements token for token, check every supported text against SQLite by creating and dropping the index on a table that tells the columns and their order apart, give each listed reject a name of its own, generate both statements behind an option and a hook that the engine's tests leave off, and make the engine refuse them exactly as before.
**Inferred intent:** The front half of slice 9 with the same trust as slices 1 to 8, so that the engine's builder only has to run the statements.

### What I did

First SQLite itself, with the pinned shell built from `/internal/sqlite/c/shell.c` into the scratchpad, the binding, and the vendored source (`sqlite3CreateIndex` around line 130802 of `/internal/sqlite/c/sqlite3.c`, `sqlite3DropIndex` at 131457, `sqlite3HasExplicitNulls` at 130774, `sqlite3StringToId` at 128636):

- A column named twice: SQLite makes an index of the column twice (`pragma_index_xinfo` shows two key rows of `a`). The pass refuses it as `column named twice`, at the later name, as for INSERT and SET.
- A string in single quotes in the list: `sqlite3StringToId` turns a TK_STRING into a TK_ID before it resolves the column, so `'b'` is the column `b`, and `'zz'` gives `no such column: zz`, never an expression. The pass refuses it as `single-quoted name`, as everywhere. A name in double quotes is a name (the build has `SQLITE_DQS=0`), and gives `no such column: "zz" - should this be a string literal in single-quotes?` when the table lacks it.
- `(a)` is the column `a` in SQLite (its parser keeps no node for the parentheses); the pass refuses it as `parentheses`. `t.a` and `main.t.a` give `the "." operator prohibited in index expressions` once the table is found; the pass refuses them as `column with the name of a table`. `true` and `false` are 1 and 0 where the table has no such column, an index on an expression; the pass refuses them as `TRUE or FALSE`. NULL, CURRENT_TIME, a number, a parameter, `a + 1`, `-a`, and `lower(a)` are expressions, `index on an expression`; COLLATE is `COLLATE`.
- Keywords: 88 are names of a new index (all those that parse there), 89 of the table after ON (`if` too), 84 of a column (those of the select list), and 88 of DROP INDEX. A subtest of `TestCheck` checks each against SQLite and against the pools of the generator.
- `create index main.i on t (a)` works; `temp.i` gives `cannot create a TEMP index on non-TEMP table "t"`; `nosuch.i` gives `unknown database nosuch`. The pass refuses all three as `schema name`. `on main.t` is `near ".": syntax error` in SQLite and the parser. `drop index main.i` works, `drop index temp.i` gives `no such index: temp.i`; `schema name` too.
- The empty list is `near ")": syntax error`. `create temp index` is `near "index": syntax error`.
- NULLS FIRST and NULLS LAST: `unsupported use of NULLS FIRST` (or LAST), the first check of `sqlite3CreateIndex`, before the table is looked up, so on any database. The pass refuses it as `NULLS FIRST or NULLS LAST`, at the NULLS. ASC and DESC: DESC is kept (`pragma_index_xinfo` desc 1, file format 4), ASC and nothing are the same.
- More than 2,000 columns: `too many columns in index` (`sqlite3ExprListCheckLength`), after the table is found and before the columns are looked up, also for 2,001 names that the table does not have. The pass refuses the 2,001st as `index beyond SQLite's limit`. 2,000 columns on a table of 2,000 columns work.
- An index name that starts with `sqlite_`: `object name reserved for internal use: sqlite_x`, after the table is found. `drop index sqlite_autoindex_t_1` gives `index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped`. The pass refuses `sqlite_` and `s4ql_` as `reserved index name` in both statements. The name of an eponymous virtual table, such as `json_each`, is an index name like any other: SQLite creates and drops such an index, and `select * from json_each('[1]')` still works.
- RETURNING, UNIQUE, IF NOT EXISTS, IF EXISTS, a WHERE, REINDEX, and ANALYZE are all accepted by SQLite's parser.

Then the code. `/internal/sql/stmt/stmt.go`: `stmt.CreateIndex{Span, Index, Table Name, Columns []IndexColumn}`, `stmt.IndexColumn{Name Name, Descending bool}`, `stmt.DropIndex{Span, Index Name}`, each with `Bounds`, `Parameters` (0), and `setSpan`. `/internal/sql/stmt/recognize.go`: `createIndex`, `indexColumn`, and `indexName`, and `drop` takes DROP INDEX. `statement` rejects EXPLAIN QUERY PLAN as a name of its own at its EXPLAIN. `/internal/sql/stmt/vocabulary.go`: `CreateIndex.Unique` is `UNIQUE INDEX`, `CreateIndex.IfNotExists.If` is `IF NOT EXISTS`, `OrderingTerm.Nulls` is `NULLS FIRST or NULLS LAST`, and `nodeName` names an `IndexedBy` `INDEXED BY` or `NOT INDEXED`; `reservedPrefix` is taken out of `reservedName` for `indexName`.

The engine: `indexRefusal` in a new file `/internal/engine/index.go`, called once at the end of `check` in `/internal/engine/engine.go`, gives `CREATE INDEX` at the CREATE and `DROP INDEX` at the INDEX, the exact errors that the pass gave before, and `TestCheck_indexes` in `/internal/engine/index_test.go` pins them. The driver's `check` calls `engine.Check` already. The `checkText` wrappers of slice 8 in the engine's and the driver's tests already take any "not supported" of `engine.Check` for a text that is not supported, so they needed no change: `go test -short ./...` passes.

Tests: `checkCreateIndex` and `checkDropIndex` in `/internal/sql/stmt/stmt_test.go`, with `canonical`, `checkPositions`, `noSuchColumn` (taken out of `anyTable.noSuchColumn`), and `db.indexes`. About 90 cases in `/internal/sql/stmt/check_test.go`, two subtests (the keywords in five places, the 2,000 columns), 48 near misses, `difference` for both, and the allow-list. The cases of EXPLAIN QUERY PLAN, INDEXED BY, NOT INDEXED, and DROP INDEX that were there changed with the names.

`/internal/sql/stmt/stmttest/stmttest.go`: positions `IndexName`, `IndexTable`, `IndexColumn` (whose pool is checked with `no such column`, as that of `SelectColumn`), and `DropIndex`; kinds `KindCreateIndex` and `KindDropIndex`; `Generator.Indexes` (one statement of a kind at random in Indexes is a create index or a drop index, each one time in two, drawn after `Changes`), and the hook `Generator.CreateIndex` (the columns, by name and order). The pass's tests set `Indexes = 4`. `TestGenerator_CreateIndex` and `TestGenerator_Indexes` in `/internal/sql/stmt/stmttest/stmttest_test.go`.

### Why

The brief's list of what stays not supported, each under a name; SQLite's behaviour decided the details that the agreement leaves open.

### What worked

- A dump of 54,900 lines, statements, calls, and arguments of `Generator` (five settings of Parameters, Columns, Nulls, Conditions, and Changes) and `Sequence` from 30 seeds, written by a copy of the package from `origin/main` and by the new one, was byte for byte the same (`cmp old.txt new.txt && echo SAME`).
- `TestGenerate` ran every generated create index and drop index through `checkCreateIndex` and `checkDropIndex` against SQLite without a failure on the first run.
- The pinned numbers of the corpus, its mutants, and the grammar did not move, though the corpus holds supported CREATE INDEX statements now (`CREATE INDEX t1w ON t1(w)`).

### What didn't work

- A Bash heredoc with a Python script that held backticks was refused, as in slice 8 ("this command is too complex to verify that it stays inside the worktree"); so was a `for` loop of `git show` into files. Scripts written with the Write tool to the scratchpad worked, and so did one `git show` per command.
- The first `checkCreateIndex` expected one row of `pragma_index_list`; a WITHOUT ROWID table lists the index of its primary key too, `sqlite_autoindex_T_1` of origin `pk`. The query leaves origin `pk` out.
- The first keyword subtest ran every text on one database, and `create index i on t (null)` succeeded there (an index on an expression), so `create index i on t (like)` gave `index i already exists` in place of `no such column: like`. Each text gets a database of its own.

### What I learned

- SQLite reads a single-quoted string in the columns of an index as a name, and parentheses around a name as the name, so neither is an expression; only the pass's rule of single quotes and its rule of the outermost construct refuse them.
- SQLite's order for CREATE INDEX, from `sqlite3CreateIndex`: NULLS FIRST or LAST, then the schema of the name, then `no such table: main.<table>`, then `object name reserved for internal use`, then `there is already a table named <index>`, then `index <index> already exists`, then `too many columns in index`, then each column in the order of the text.

### What was tricky

- The moved pinned numbers. `TestGenerate_mutants` went from 64 to 39 syntax errors and from 196 to 153 not supported where SQLite does not say, and `TestGenerate_nearMisses` from 2,054 to 1,531. A throwaway test grouped the excused texts by SQLite's message and kind: all are of CREATE TABLE (`duplicate column name`, `unknown table option`, `no such collation sequence`; in the near misses 1,044 `table "t" has more than one primary key`, 261 `generated columns cannot be part of the PRIMARY KEY`, 208 `AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`, 18 `non-deterministic functions prohibited in index expressions`), none of an index. They moved because a quarter of the statements are indexes now, so 261 of the 2,000 near-miss statements are CREATE TABLE where 349 were.

### What warrants review

- `createIndex`, `indexColumn`, and `indexName` in `/internal/sql/stmt/recognize.go`, and the names and offsets of the new rejects.
- `checkCreateIndex` and `checkDropIndex` in `/internal/sql/stmt/stmt_test.go`.
- `Generator.indexColumns` in `/internal/sql/stmt/stmttest/stmttest.go`.
- `indexRefusal` in `/internal/engine/index.go`.

### Future work

- The README of the pass, `docs/todo.md`, and the decisions: step 2.
- The engine and the driver for slice 9, which remove `indexRefusal`.
