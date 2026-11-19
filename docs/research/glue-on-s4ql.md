# What glue needs from s4ql

Date: 2026-11-19. Read: glue at commit `97d5254` of 2026-10-06, and from the module cache goqite v0.4.0, migrate v0.6.0, and scs sqlite3store `v0.0.0-20250417082927-ab20b3feb5e9`. The untracked `.claude/worktrees/csp-nonce` in the glue checkout was not counted.

Question: what must s4ql support so that `maragu.dev/glue` runs on it in place of SQLite? This note compares glue with s4ql's roadmap as of slice 4 being built.

## 1. How glue connects

- Packages: `database/sql` through `github.com/jmoiron/sqlx`, with `mattn/go-sqlite3` imported blank in `sqlitetest/helper.go`. The jobs queue is `maragu.dev/goqite`, migrations are `maragu.dev/migrate`, and sessions are `scs/sqlite3store` (`sqlitestore/sqlitestore.go`).
- Connect (`sql/helper.go`): `sqlx.ConnectContext(ctx, "sqlite3", path+"?_journal=WAL&_timeout=5000&_fk=true&_txlock=immediate")`, under a timeout of 10 s that includes the ping. It then builds two goqite queues on `h.DB.DB` with `SQLFlavorSQLite`.
- An application cannot choose the driver or pass in a `*sql.DB`. The driver name and the DSN are hard-coded. `Helper.DB` is exported, but `Connect` sets it.
- The DSN options against s4ql: `_fk` and `_txlock=immediate` are already how s4ql behaves. `_timeout` corresponds to `BeginTx` waiting for as long as its context allows. WAL does not apply.
- Transactions: glue's `InTx` and goqite's `InTx` use `LevelSerializable`, and migrate uses `BeginTx(nil)`. s4ql supports all three.
- `Ping` runs `select 1` inside `InTx`, so on s4ql each ping is a write transaction.
- sqlx reads every row before it closes them, and nothing runs a statement while rows are open.

## 2. The SQL that glue runs or ships

| Feature | Example | Where |
|---|---|---|
| Tables that are not STRICT, or that have a rowid | `create table glue (version text primary key)` | `sql/migrations/1747220180-migrations.up.sql` |
| A table without a primary key | `create table if not exists migrations (version text not null)` | `migrate.go` |
| Several columns, `not null` | the goqite table (8 columns), sessions (3 columns) | goqite `schema_sqlite.sql`, scs README |
| Expression defaults | `default ('m_' \|\| lower(hex(randomblob(16))))`, and `default (strftime('%Y-%m-%dT%H:%M:%fZ'))` 4 times | goqite schema |
| Literal default | `default 0`, 2 times | goqite schema |
| Trigger | `after update on goqite begin update goqite set updated = strftime(...) where id = old.id; end` | goqite schema |
| Index, with `desc` | `create index ... on goqite (queue, priority desc, created)`; `sessions(expiry)` | goqite schema, scs README |
| Several statements in one Exec | migration files and fixtures | `migrate.go` `apply`, `sqlitetest/options.go` |
| Insert with several columns, and `returning` | `insert into goqite (queue, body, timeout, priority) values (?, ?, ?, ?) returning id` | `goqite.go` |
| Update with arithmetic, a scalar subquery with `order by ... desc, ... limit 1`, and `returning` 2 columns | `ReceiveTx` | `goqite.go` |
| Update and delete with `where ... and` | `update goqite set timeout = ? where queue = ? and id = ?`; `delete from goqite where queue = ? and id = ?` | `goqite.go` |
| Update without `where`, the value as a literal | `update migrations set version = '<v>'` | `migrate.go` |
| Insert without a column list | `insert into migrations values ('')` | `migrate.go` |
| `exists` subquery | `select exists (select * from migrations)` | `migrate.go` |
| `REPLACE INTO`, `$1` parameters, `julianday` | `REPLACE INTO sessions (token, data, expiry) VALUES ($1, $2, julianday($3))` | `sqlite3store.go` |
| Comparison with `julianday('now')` | `SELECT data FROM sessions WHERE token = $1 AND julianday('now') < expiry`, and 2 more | `sqlite3store.go` |
| Comparisons `>=` and `<` | `? >= timeout and received < ?` | `goqite.go` |
| `select` without `from` | `select 1` | `sql/helper.go`, `Ping` |

- Column types: `text`, `blob`, `integer`, and `real`.
- Arguments: Go `string`, named string types (`goqite.ID`), `[]byte` (never nil), and `int`. No `time.Time`: times are passed as text.
- Not used: `rowid`, `autoincrement`, joins, views, FTS, and `json_*`.

## 3. What the tests need on top of that

- Fixtures (`sqlite/testdata/fixtures/*.sql`): `strict` tables that are not `without rowid`, a real literal `10.50`, and 3 statements in each file.
- Tests (`sqlitetest/*_test.go`, `sql/helper_test.go`):
  - `select count(*)`, 5 times
  - `where id = 'user1'`
  - `select 'test' as value`
  - `select count(*) from sqlite_master where type='table'`
  - `from (select 1 as day union select 2 union select 3) as festival where day in (?, ?)`, 2 times, from `sqlx.In`
  - `migrate down`, which runs `drop table glue;`
- `sqlitetest` makes a new database file for every test and deletes it afterwards. On s4ql that means a new bucket prefix and cache directory for every test.
- glue has no example application.

## 4. Conflicts with s4ql's restrictions

- No primary key on `migrations`, so it cannot be `WITHOUT ROWID`. The fix is a change in maragu.dev/migrate.
- No table is `strict, without rowid`. That holds for all 6 tables. Each can be fixed by changing its DDL text.
- `REPLACE INTO` is a conflict resolution that the roadmap rejects for now, and `$1` is a parameter form that slice 2 rejects.
- A trigger. The roadmap's restrictions do not forbid it, and replay of row-based commits never runs it (`docs/todo.md`).
- A read of `sqlite_master`, in one test.
- No conflict was found with double-quoted strings, type conversion (every value has the type of its column), or settings (`pragma`).
- Several processes: glue writes from one process. Its session store adds a cleanup goroutine that deletes every 5 minutes.
- `Connect` is bounded at 10 s, the ping included. s4ql's ping waits until the instance serves, which after a crash is at least one lease duration.

## 5. Gaps, from "needed nearly everywhere" to "needed in one place"

A place is one statement text. Counts are production / tests. **[A]** marks a gap that a small change in glue or its dependencies could remove instead.

1. Columns that are not the key, `not null`, and select of named columns: 14 / 10.
2. `where` with `=`, `<`, `>=`, and `and`; `?` in several positions: 7 / 3.
3. Update and delete: update 4 (the trigger's included), delete 3 / 0.
4. Insert with several columns: 2 / 4. Insert without a column list: 1, in migrate [A].
5. DDL:
   - `strict, without rowid` missing on 6 tables [A]
   - a primary key on `migrations`, 1 [A, in migrate]
   - several statements in one text, 3 or more (every migration file, 2 fixtures) [A only if migrate and the fixture loader split statements; the trigger's `begin ... end;` makes splitting on `;` unsafe]
   - `create index` 2, one with `desc`
   - `create table if not exists` 1
   - defaults: 2 literal, 5 expression [the expression ones are A, if goqite makes ids and times in Go]
6. Functions:
   - `strftime` 5 [A, as for defaults]
   - `julianday` 4 [A]
   - `count(*)`, tests only: 0 / 5
   - `lower`, `hex`, `randomblob`, and `||`: 1 [A]
   - `+` arithmetic: 1
7. `returning`: 2 [A, if goqite makes the id in Go and selects before it updates]
8. Subqueries:
   - a scalar subquery with `order by` and `limit`: 1
   - `exists`: 1 [A]
   - a subquery in `from` with `union` and `in`: 0 / 2 [A]
9. `select` without `from`: 1 (`Ping`) [A: `PingContext`] / 1, and `as` aliases in 2 tests.
10. One place each:
    - `REPLACE INTO` and `$1` parameters, 3 statements [A: glue writes its own `scs.Store` in `sqlitestore`]
    - the trigger, 1 [A: goqite sets `updated` in its 2 updates]
    - `sqlite_master`, 1 test [A]
11. Not SQL: glue must accept a `*sql.DB` or a driver name [A, required]. `sqlx.NewDb(db, "sqlite3")` would keep the `?` binding.

## Files read

- `/Users/maragubot/Developer/glue/sql/helper.go`
- `/Users/maragubot/Developer/glue/sql/migrate.go`
- `/Users/maragubot/Developer/glue/sqlitestore/sqlitestore.go`
- `/Users/maragubot/Developer/glue/sqlitetest/`
- `/Volumes/990/go/mod/maragu.dev/goqite@v0.4.0/goqite.go` and `schema_sqlite.sql`
- `/Volumes/990/go/mod/maragu.dev/migrate@v0.6.0/migrate.go`
- `/Volumes/990/go/mod/github.com/alexedwards/scs/sqlite3store@v0.0.0-20250417082927-ab20b3feb5e9/`
