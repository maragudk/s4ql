# Diary: The cache refuses every table it did not make

An independent review of the core step of slice 11 (pull request #111) found, by running, that `conn.load` in `internal/core/cache.go` read tables made by hand as tables of the core's, because it read a table back only through `pragma_table_info`, which hides much of a declaration. This task closes that gap for tables, indexes, and the other objects of the cache. The work is on the branch `cache-load-strict`, from `origin/default-core` at `0e89413`.

## Step 1: The comparison of statements, the check of objects at the open, and the generated proof

**Author:** cache-load-strict builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`conn.load` (and `loadIndexes` for indexes, if it has the same gap: check) accepts a table or index only if it is exactly what the core would have written for the schema it read: the reviewer's proposal is to compare the text in `sqlite_schema.sql` with the core's own `createSQL()` of the table it read back [...] the alternative is to check `strict` and without-rowid from `pragma_table_list` and to read columns with `pragma_table_xinfo`, which closes fewer. Choose, and give the reason. [...] The cache must also refuse objects that are not tables or indexes of the layout at all: views, triggers, a table in the cache whose name the core's own tables do not explain. [...] Measure what it adds to a point read on a table with and without indexes [...] the cache's layout version does not change, because no cache the core wrote becomes invalid: show that with the generated test."
**Interpretation:** Make the cache trust only what it wrote, byte for byte, refuse with `ErrCacheVersion` and the advice to use a new cache, and prove with random schemas that nothing the core writes is refused.
**Inferred intent:** The core trusts the cache to hold what the log says; an altered cache must never be read as one of its own.

### What I did

`internal/core/cache.go`:
- `conn.load` reads `type, name, sql` from `sqlite_schema` where `tbl_name` is the name and the type is a table or a trigger, in the one statement that found the table before, so it adds no statement. A trigger refuses the table. After the columns are read from `pragma_table_info` as before, it compares the stored text with `kept(t.createSQL())`.
- `kept` turns a statement of the core into the text that SQLite keeps: `CREATE TABLE ` or `CREATE INDEX `, then the statement from the name on. SQLite drops the schema name `main.` and keeps the rest as written.
- `conn.checkObjects`, the last step of `conn.setUp`, so of `Open`: one scan of `sqlite_schema` that accepts `s4ql_cache` declared exactly as `conn.make` declares it (its statement is now the constant `cacheTableSQL`), tables of SQLite's own (`sqlite_` names, such as `sqlite_stat1` from `analyze`), tables of names that are not reserved, and indexes on those, and refuses everything else.
- `foreign`, the error for all of this, wraps `ErrCacheVersion` and ends with the advice to open with a new cache. The older refusals of `conn.load` and `conn.loadIndexes` use it too, so they now carry the advice; their reasons are unchanged.

`internal/core/index.go`: `conn.loadIndexes` reads the text of each index in the statement that reads its columns, and compares it with `kept(t.createIndexSQL(ix))`.

Tests, in `internal/core/cache_test.go`, `TestOpen_cache_foreign`:
- 8 objects refused at the open, each leaving the main file as it was: two views, two triggers, two tables of reserved names, an index on `s4ql_cache`, and `s4ql_cache` declared without `check (id = 1)`.
- `analyze` (which makes `sqlite_stat1`) is accepted.
- 23 tables `t9` refused at the first look-up, each in a read and in a write, with the instance stopped and the file unchanged. pragma_table_info reports each one as a table of the core's, so before this change each was read as one. The reviewer's five cases are among them. The others: no `strict`, foreign keys (on a column and as a table constraint), `on conflict replace`, `primary key desc`, the key as a table constraint, `unique`, a collation outside the key, a named constraint, unquoted names, a second space, keywords in capitals, no space after the comma, the options in the other order, and a trailing comment.
- A trigger added while the instance runs, refused at the next first look-up.
- 7 indexes refused at the first call that needs the indexes: `collate binary` (which the old check already refused, since the pragma reports it as `binary` in lowercase), `asc`, the table's name in capitals, unquoted names, a trailing space, no space before the parenthesis, and the column's name in capitals.
- The generated proof: 100 rounds (15 under `-short`). Each round creates 1 to 3 transactions of 1 to 4 tables, with names drawn from `hostile` (quotes, control characters, keywords, 2000 bytes) or from random characters, 1 to 6 columns of every type, a key of any of them in any order, columns that do or do not take NULL, defaults of every type, of NULL, and random real bits. Each table gets rows and up to 3 random indexes. The round reads it back after a close and an open, and after a replay into an empty cache.

`internal/core/index_test.go`: one case of `TestOpen_cache_indexes`, a table made by hand with `unique`, is gone, because `Get` now refuses that table before the indexes are read. Its refusal is in the new test.

### Why

Comparing statements, not reading more pragmas: the text is the one place where SQLite keeps everything that a statement declared, so one comparison refuses every hole at once, also the holes that nobody has listed. `pragma_table_list` and `pragma_table_xinfo` would add `strict`, the rowid, and generated columns, but no pragma reports a check, a conflict clause, or an explicit `default (1)`, so each of those would stay open until someone found it. The cost of the comparison is that a table that means the same but is spelled otherwise is refused too. That is right for a cache: the core never writes another spelling, and a new cache costs only a catch-up.

### What worked

- The full `go test -short ./internal/core/` passed after the change, with `TestSchemaReads` unchanged: no statement was added.
- The tests at the length limit, which create tables and indexes whose kept text is exactly as long as the limit and read them back after a restart and a replay, pass. That is the proof for names at the limit.

### What didn't work

- First version: the text of each index came in the first statement of `conn.loadIndexes`, beside `pragma_index_list`, with `order by s.rowid`. Two subtests of `TestTx_tooBig` failed: `tx_test.go:2775: Expected nil error, but got "sqlite: string or blob too big (code 18)"`. The sorter's record held the name and the text of an index whose text was as long as the limit of 1000. So the text now comes from a scalar subquery in the statement on `pragma_index_xinfo`, which has no `order by`. The code checks the order of `seqno` itself and reads the text only from the first row.
- A table of the generated test with `create table if not exists ... /* a comment */` was refused, as expected. But `create index if not exists "i9" on "t1" ("v" desc, "k" desc)` was accepted. SQLite keeps an index from its name on, so `if not exists` leaves no trace, and the text equals the core's. That is an index of the core's in every respect, so it is not a refusal case.
- `create table ...; create trigger ...` as one case of the look-up test failed at `Open`, because `checkObjects` refuses the trigger first. The trigger has its own case at the open, and one while the instance runs.
- A Python one-liner that rewrote two files in one Bash call was refused: "this command is too complex to verify that it stays inside the worktree". Smaller calls and the Edit tool worked.

### What I learned

- SQLite keeps a table's statement up to the last token before the end of the statement, so a trailing comment or space after `without rowid` is kept. An index's statement ends at its last token.
- `pragma_index_xinfo` reports a collation that is named in the statement as it was written (`binary`), and the default one as `BINARY`.

### What was tricky

- The length limit. Any statement that sorts must not carry the text, since a sorter's record holds every column of the result.

### What warrants review

- `conn.checkObjects` accepts any table whose name starts with `sqlite_` in any case. SQLite refuses to create such names unless `writable_schema` is on.
- A view or a trigger added while an instance runs: a trigger is refused at the next first look-up of its table. A view is refused only at the next open, and it can only clash with a name, which SQLite then refuses.
- The cost. It adds no statement. Building and comparing the text costs about 1 µs for a table of six columns and under 1 µs for an index, from a benchmark of `createSQL` and `kept`. The first look-up of a table plus a get, with `conn.forget` before each as at the end of a transaction, takes about 110 µs for a table without indexes, and about 400 µs with its five indexes read. A benchmark of that, with test binaries built from the old and the new `cache.go` and `index.go` and run in turns, 15 runs each, showed no significant difference (p from 0.16 to 0.87). The machine was busy with other builders, and the runs varied by 10% to 74%. A first benchmark through `ReadTx.Get` varied by up to 124% and was useless. The benchmarks were scratch files and are not committed.

### Self-review

The code-review skill (medium) gave six findings:
- **A trigger on `s4ql_cache` added while an instance runs (important).** It is refused only at the next open. Not fixed, and documented in the README instead. A process that writes to a running cache can also change its rows directly, which no check sees. A trigger that skips the update of the position fails the commit anyway, since `conn.setPosition` counts the changed row.
- **A virtual table whose module is missing** failed the look-up with `no such module`, not `ErrCacheVersion`. Fixed: `conn.load` refuses a statement that does not start with `CREATE TABLE ` before the pragma runs. A new subtest plants such a table with `writable_schema`. With the check switched off, it failed with `sqlite: no such module: nomodule (code 1)`.
- **The check of `seqno` did not wrap `ErrCacheVersion`.** Fixed.
- **Docs that still said a `unique` table answers `Get`.** Fixed in the doc comment of `conn.loadIndexes` and in the README.
- **The advice blames an outside change also for structural refusals.** Kept as it is: the core makes no table without a key index or with a gap in its key, so only an outside change can cause those.
- **A trigger on a name with no table (such as on a view made after the open) stops the instance.** Kept on purpose, with a comment in the code: the core makes no trigger.

### Future work

None found.
