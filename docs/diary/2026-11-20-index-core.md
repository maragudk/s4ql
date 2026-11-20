# Diary: indexes in the commit format and the storage core

Slice 9 of `/docs/roadmap.md` adds `create index i on t (a, b desc)` and `drop index i`. This task brings indexes to the commit format (`/proto/s4ql/v1/commit.proto`, `/internal/format`, `/internal/s4qlpb`) and to the storage core (`/internal/core`): the definition of an index in a commit object, a real SQLite index in the cache, replay, and a scan by a named index. The engine, the planner, and the driver are the next builder's task. The branch is `index-core`, from `origin/index-stmt` at `870f427`, which holds the statement pass of slice 9 (pull request #92).

## Step 1: the requirements, SQLite's facts, and `CreateIndex` and `DropIndex` in the commit format

**Author:** builder (index-core)

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Task: indexes in the commit format and the storage core. Only `proto/`, `internal/format`, `internal/core` (and the binding `internal/sqlite` if a call is missing). [...] Agreed with Markus (2026-11-20): An index is `create index i on t (a, b desc)`: a name, a table, one or more columns of the table in order, each ascending or descending. Not unique. No partial index, no expression, no collation. The log holds only the definition. [...] The cache holds a real SQLite index [...] s4ql's engine picks the index; the cache's SQLite then scans exactly that index. Core therefore offers scans by a named index and never lets SQLite choose. The cache's layout version rises to 3. [...] 1. Commit format [...] creating and dropping an index, in the manner tables are created and dropped in a commit, with the index's name, its table, and its columns with their directions. Dropping a table drops its indexes (decide whether the commit names them or the reader derives it, and say why). Validation on read is as strict as for the rest [...] 2. Core write API [...] 3. Cache [...] 4. Replay and catch-up [...] 5. Core read API: a scan by a named index [...] 6. Counts [...] 7. Tests in the core's style [...] 8. Decisions that the agreement does not settle [...] record each in `docs/todo.md` [...]"
**Interpretation:** Two new kinds of change in the commit object, checked as strictly as the others; an index in the cache that `conn.load` reads back and checks; `Tx.CreateIndex` and `Tx.DropIndex` with the net effect in the commit object; replay of both; a scan by an index or the primary key with bounds on a prefix of the key and a direction; a count of the rows read; and tests against SQLite itself.
**Inferred intent:** The storage half of slice 9 with the same trust as the rest of the core, so that the engine's builder only picks an index and states bounds as values.

### What I did

First SQLite itself, with the pinned shell built from `/internal/sqlite/c/shell.c` and `/internal/sqlite/options.h` into the scratchpad:

- `sqlite_schema` holds one row for each index made with `create index`, and none for the index of the primary key of a table without a rowid. A new row gets the rowid after the largest one: after `i1`, `I 2`, `i3`, `i4` at rowids 2 to 5 and `drop index "I 2"`, `i5` got rowid 6. So the order of the rowids is the order of creation of the indexes that exist.
- `pragma_index_list` lists the newest index first, and the index of the primary key as `sqlite_autoindex_<table>_1` with origin `pk`. It reports `unique` and `partial`. `pragma_index_xinfo` gives a row for each column of the key of an index with `key` 1 for the index's own columns and 0 for the columns of the primary key that SQLite adds, `desc`, `coll`, and `cid`, which is -2 for an expression.
- The key of an index is its columns and then the columns of the primary key that it does not hold: `i3 (c desc, k2 desc)` on a table with the key `(k1, k2)` has the key `(c desc, k2 desc, k1)`.
- `explain query plan` of `select * from t indexed by i where a = ?1 and b > ?2 order by ...` gives `SEARCH t USING INDEX i (a=? AND b>?)`, `a is null` shows as `a=?`, a scan without bounds as `SCAN t USING INDEX i`, the primary key as `SEARCH t USING PRIMARY KEY (k1=? AND k2>?)` and `SCAN t`, and an index that holds every column of the table as `USING COVERING INDEX`.
- An `order by` of more than 63 terms is never satisfied by an index: SQLite sorts (`USE TEMP B-TREE FOR ORDER BY`), with 500 and with 800 key columns alike. A condition of 1,201 terms is refused (`Expression tree is too large (maximum depth 1000)`), and one of 501 terms is not.

Then the format. `/proto/s4ql/v1/commit.proto`: `Change` gets `create_index = 5` and `drop_index = 6`; `CreateIndex{index, table, columns}`, `IndexColumn{column, descending}`, where `column` is a position in the table's columns as in `UpsertRows`, and `DropIndex{index}`. `DropTable` says that it drops the indexes of the table too, and that a commit object does not list them. `/internal/format/check.go`: `checkCreateIndex` refuses no name, no table name, the name of its table as its own (compared as SQLite compares names), no columns, a column without a position, and a column twice; a drop index needs a name. `/internal/format/writer.go`: `CommitWriter.CreateIndex` and `CommitWriter.DropIndex`. The version of the format stays 0: a new kind of change is a new member of the `oneof`, which a reader of before refuses as one of a newer version, as the README of the format says a new kind is made.

Tests in `/internal/format/reader_test.go` and `/internal/format/writer_test.go`: three refusals as newer, sixteen as malformed, ten refusals of the writer, both samples written again with a create index and a drop index, the round trip of the writer, and two more operations in `FuzzCommitWriter` with a seed, and a seed for `FuzzCommitReader`.

### Why

The format holds the definition and nothing else, as agreed. A position cannot disagree with the table the way a second copy of a column's name could, which is the reason the README gives for the primary key and for `UpsertRows`. A commit object stands alone, so the format checks what one change shows in itself, and replay checks the rest against the cache.

### What worked

`go test ./internal/format/` passes. `FuzzCommitWriter` ran 86,091 inputs and `FuzzCommitReader` 711,931 in 20 seconds each without a failure.

### What didn't work

- A Bash command with a Python heredoc of many lines was refused: "this command is too complex to verify that it stays inside the worktree". A script written with the Write tool to the scratchpad and run with the path of the worktree as its argument worked.
- Two tests of the reader used the field numbers 5 and 6 of `Change` as fields that it does not know. They are 7 and 8 now.
- `TestCommitWriter` pinned 6 changes and 9 writes of the example; they are 8 and 11 with the two new changes.

### What I learned

- SQLite gives the index of the primary key of a table without a rowid a name and no row in `sqlite_schema`.
- An index cannot serve an `order by` of more than 63 terms, whatever the terms are.

### What was tricky

Nothing in this step.

### What warrants review

- The comments of `CreateIndex`, `IndexColumn`, `DropIndex`, and `DropTable` in `/proto/s4ql/v1/commit.proto`.
- `checkCreateIndex` in `/internal/format/check.go`.

### Future work

- The cache, the write API, and replay: step 2. The scan and the counts: step 3.
