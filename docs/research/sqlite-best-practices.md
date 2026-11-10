# SQLite best practice for server-side apps, and what it means for the subset

Compiled 2026-11-10. These notes collect what the SQLite authors and experienced practitioners recommend for SQLite in server-side web applications. The goal is to help choose which parts of SQLite's SQL to support first and which legacy parts to leave out. The notes make no decisions; section 0 summarises and section 5 proposes an order for the slices.

Legend:
- **[V]** verified from a primary source: sqlite.org documentation or forum, a project's own source code, docs or README, or the author's own post. Also used for results measured in the scratch probes described below.
- **[I]** inferred, or taken from a secondary source.

Sources were read at these versions:
- SQLite documentation as published on 2026-11-10. The latest release is 3.54.1 (2026-10-16). [V https://sqlite.org/changes.html]
- Scratch probes: the Homebrew build of the `sqlite3` CLI 3.54.1 on macOS arm64. This build has `DQS=0`, foreign keys off by default, and FTS5 and DBSTAT enabled (checked with `PRAGMA compile_options`). Each probe ran once on one machine, and timings are indicative only. Probes are marked "[V measured]".
- Practitioner posts carry their dates. Advice from before 2022 is marked as dated.

---

## 0. Summary

### 0.1 Practices with the strongest consensus
Every server-side source read agrees on these. [V for each source cited in §2.1]
1. **WAL mode, a busy timeout of 5 to 10 seconds, `synchronous=NORMAL`, and foreign keys on.** These appear in Kerkour (2024), Rails 8's adapter defaults, PocketBase, Litestream's tips, and the maragudk/glue helper.
2. **Start write transactions with `BEGIN IMMEDIATE`.** A deferred transaction that upgrades from read to write can fail at once with `SQLITE_BUSY`, and the busy timeout does not help. The SQLite docs describe the failure. Kerkour, Margheim, Simon Willison and Bert Hubert call it SQLite's biggest footgun. Rails 8 and Django 5.1+ can set it by default.
3. **One writer connection and a pool of reader connections.** Kerkour, Margheim (Rails), PocketBase and Crawshaw (2018) do this.
4. **`STRICT` tables** (Kerkour, Evan Hahn, Anton Zhiyanov, sqlite-utils). The SQLite authors are the dissenters: they argue for flexible typing, but they added STRICT for those who want it.
5. **Never trust the defaults for foreign keys.** Every source turns them on. The SQLite docs say they are off "for backwards compatibility".
6. **Single quotes for strings; no double-quoted string literals.** The SQLite docs call the fallback a "misfeature" and recommend `SQLITE_DQS=0`.
7. **Keyset pagination rather than large `OFFSET`s**, and **upsert (`ON CONFLICT DO UPDATE`) rather than `INSERT OR REPLACE`**.
8. **Many small queries are fine in SQLite.** The SQLite site's "N+1" page and Ben Johnson both say so.

### 0.2 "All tables STRICT and WITHOUT ROWID": what to know before committing
The arguments against come first.
1. **The SQLite docs do not recommend WITHOUT ROWID everywhere.** They recommend it for tables with a non-integer or composite primary key *and* small rows: under about 1/20 of a page, which is about 200 bytes at the default 4 KiB page. They advise benchmarking late in development and keeping it only where it helps. [V https://sqlite.org/withoutrowid.html §4] No practitioner source recommends it as a universal default. [V search, see §6.1]
2. **Large rows get slower.** Content of a WITHOUT ROWID table lives in the B-tree's interior nodes as well as its leaves, so fan-out drops. In my probe, with 100,000 posts of about 2 KB each, every row spilled to an overflow page. Primary-key lookups were about 4.7 times slower than in a STRICT rowid table. [V measured, §1.2.3]
3. **Secondary indexes get bigger and a little slower when keys are text.** Every index entry carries the full primary key rather than an integer rowid. In the probe, a users table with a 34-character text key and two secondary indexes was 29 MB rather than 23 MB. Lookups through a secondary index were about 25% slower. Lookups by primary key were about 1.7 times *faster*. [V measured, §1.2.3; file format §2.5]
4. **No generated integer keys.** In a WITHOUT ROWID table, `INTEGER PRIMARY KEY` is not a rowid alias, `AUTOINCREMENT` is an error, and inserting NULL into the key is an error. Every key must come from the application or a `DEFAULT` expression. The default schemas of Rails, sqlc's examples, and Ben Johnson's wtf all use `INTEGER PRIMARY KEY AUTOINCREMENT`, so none of them work unchanged. [V docs and sources, §1.3]
5. **Random keys hurt inserts into a clustered key.** Anders Murphy (2026) measured UUIDv4 keys in a WITHOUT ROWID table at roughly 15 to 18 times slower than integer keys at 10 million rows. UUIDv7 was about 1.7 times slower. This matters less for a read-heavy workload, but it argues for time-ordered keys. [V author's post; numbers not reproduced]
6. **Some SQLite features do not work with WITHOUT ROWID.** These are `sqlite3_blob` incremental I/O, `sqlite3_update_hook`, `last_insert_rowid()`, and the preupdate hook's rowid arguments. FTS5 external-content tables also need an integer `content_rowid` column, so a table with a text key needs an extra `INTEGER UNIQUE` column for full-text search. [V docs; FTS5 workaround V measured]
7. **STRICT forbids the type names that Go tooling uses to map types.** `DATETIME`, `TIMESTAMP`, `DATE`, `BOOLEAN` and `JSON` are not allowed. mattn/go-sqlite3 decides whether to return `time.Time` from these declared names. sqlc maps them to `time.Time`, `bool` and `json.RawMessage`. Under STRICT, timestamps come back as `string` or `int64` and booleans as `int64`, unless the application adds custom types or sqlc overrides. [V source code, §1.4]
8. **A `SELECT` without `ORDER BY` no longer returns insertion order.** It returns primary-key order. This is legal, since the order is undefined without `ORDER BY`, but some application code relies on it without knowing. [V measured, §1.2.3]
9. **Foreign keys always on, combined with no PRAGMAs, makes the standard table-rebuild migration impossible for any table that other tables reference.** With enforcement on, `DROP TABLE parent` either fails or, with `ON DELETE CASCADE`, silently deletes the child rows. Deferred constraints do not help either. See §4.3. [V measured]

In favour:
- The SQLite author writes that "in a perfect world, there would be no such thing as a 'rowid' and all tables would following the standard semantics implemented as WITHOUT ROWID tables". [V https://sqlite.org/rowidtable.html]
- WITHOUT ROWID removes a whole family of quirks by construction: NULLs in primary keys, rowid aliases and their name shadowing, rowids that `VACUUM` renumbers, the `INTEGER PRIMARY KEY DESC` bug, `AUTOINCREMENT` and `sqlite_sequence`, and `last_insert_rowid()`. [V https://sqlite.org/rowidtable.html, https://sqlite.org/lang_createtable.html]
- A lookup by primary key needs one B-tree search rather than two. [V withoutrowid.html §3; measured 1.7 times faster for small rows]
- STRICT already makes primary-key columns NOT NULL. Together the two options give standard primary-key semantics. [V https://sqlite.org/stricttables.html]

### 0.3 Legacy behaviour: rejection versus exact match
Rejection is enough (the subset need not accept the syntax): double-quoted strings; `[brackets]` and backticks; untyped columns and type names other than the five STRICT types; `AUTOINCREMENT`, rowid aliases, NULL primary keys and `INTEGER PRIMARY KEY DESC` (all made moot by STRICT plus WITHOUT ROWID); the `LIMIT offset, count` comma form; join syntax oddities ("strange join names", `ON` on a comma join, outer joins without `ON`, mixed comma and `JOIN` precedence); bare columns in aggregates; `INSERT OR REPLACE` and the other conflict clauses; FK `MATCH` clauses; keywords used as unquoted identifiers; `ISNULL`/`NOTNULL`; empty `IN ()`; result aliases in `WHERE`; all PRAGMAs; the `localtime`/`utc` date modifiers.

Must be matched exactly, because the syntax is ordinary and has to be accepted: affinity on comparisons (an INTEGER column compared to `'1'` is true) and the conversions STRICT still applies on insert; integer overflow to REAL in arithmetic, but an error in `sum()`; integer division and NULL from division by zero; ASCII-only case folding in `LIKE`, `NOCASE`, `upper()` and `lower()`; NULL ordering; NULLs distinct in UNIQUE; CHECK passing on NULL; `CURRENT_TIMESTAMP`'s format; the rules for `CAST`, `round()` and float rendering; `TRUE`/`FALSE` as 1 and 0; recursive triggers off; foreign-key actions (including cascades); and invalid UTF-8 or NUL bytes, unless the subset rejects such values at bind time. Details are in §3.

---

## 1. Schema design

### 1.1 STRICT tables in practice
- **What STRICT does.** It was added in 3.37.0 (2021-11-27). Every column must declare one of `INT`, `INTEGER`, `REAL`, `TEXT`, `BLOB` or `ANY`. A value that cannot be converted losslessly raises `SQLITE_CONSTRAINT_DATATYPE`. Primary-key columns are implicitly NOT NULL. `PRAGMA integrity_check` verifies the stored types. [V https://sqlite.org/stricttables.html]
- **STRICT still coerces.** It is enforcement *after* the usual affinity conversion, not a ban on conversion. [V stricttables.html; V measured with 3.54.1]:
  - `'12'` and `'  7 '` (with spaces) go into an INTEGER column as integers 12 and 7.
  - `1.0` goes into INTEGER as 1, but `1.5` is rejected.
  - `100` goes into REAL as 100.0, and `'2.5'` goes into REAL as a real.
  - The integer `5` goes into TEXT as the text `'5'`.
  - BLOB into TEXT is rejected, and TEXT into BLOB is rejected.
  - `true` goes into INTEGER as 1.
- **Comparison affinity still applies in queries** on STRICT columns. With `n INTEGER` holding 1, `n = '1'` is true, while `'1' = 1` between two literals is false. [V measured; rules in https://sqlite.org/datatype3.html §4.2] The subset engine must reproduce this; it cannot be avoided by rejecting syntax.
- **Generated columns follow STRICT typing.** This has been enforced since 3.51.0 ("Enforce STRICT typing on computed columns"). [V changes.html; V measured: `n INTEGER AS ('notint')` fails on insert]
- **No `ALTER TABLE` makes a table strict.** You copy the data into a new table, as sqlite-utils' `transform --strict` does. [V Evan Hahn, "Prefer STRICT tables in SQLite", 2026-07-11, https://evanhahn.com/prefer-strict-tables-in-sqlite/; V Simon Willison 2026-07-11, https://simonwillison.net/2026/Jul/11/sqlite-utils/]
- **Older SQLite versions cannot open a database with STRICT tables** (before 3.37.0). [V stricttables.html] This does not matter here, since the cache uses a pinned SQLite. [I]
- **Practitioners.**
  - Kerkour says "Use STRICT tables" and gives `id BLOB NOT NULL PRIMARY KEY, created_at INTEGER NOT NULL`. [V https://kerkour.com/sqlite-for-servers, 2024-02-15]
  - Evan Hahn found "no obvious difference" in performance in a quick test. [V]
  - Anton Zhiyanov presents STRICT as "the same way other DBMS do", with ANY for flexibility. [V https://antonz.org/sqlite-strict-tables/, 2024-05-04]
  - The maragudk/goqite schema is STRICT, with a rowid table and a text key. [V https://github.com/maragudk/goqite/blob/main/schema_sqlite.sql]
- **The SQLite authors' counter-position.** "The Advantages Of Flexible Typing" argues that rigid typing does not prevent bugs. It asks readers to report any real case where STRICT prevented a bug, and it describes STRICT as "for developers who prefer that kind of thing". [V https://sqlite.org/flextypegood.html; changes.html 3.37.0] The decision to use STRICT is already made, and this is the only notable dissent.

### 1.2 WITHOUT ROWID

#### 1.2.1 What the docs say
- It is an optimisation that "adds no new capabilities". WITHOUT ROWID tables and rowid tables "generate the same answers given the same SQL", apart from documented corner cases. It requires 3.8.2 (2013-12-06). [V https://sqlite.org/withoutrowid.html]
- **Recommended** for "tables that have non-integer or composite (multi-column) PRIMARY KEYs and that do not store large strings or BLOBs". In such cases a table "can use about half the amount of disk space and can operate nearly twice as fast". [V withoutrowid.html §1, §4] In a 2013 mailing-list post, Hipp gave measured gains of 5% to 30% less CPU time and up to 55% less disk. [I secondary quote found by search; not opened]
- **Warned against:**
  - For a single-column `INTEGER` primary key, "ordinary rowid tables will run faster". [V §4]
  - When the average row exceeds about 1/20 of a page (about 200 bytes at 4 KiB), "rowid tables tend to work faster for large row sizes". The reason is that content is stored in interior nodes, which reduces fan-out. [V §4]
  - The docs suggest: "simply not worry about WITHOUT ROWID until near the end of product development, then go back and run tests ... retaining the WITHOUT ROWID only in those cases where it helps". [V §4]
- **Differences from rowid tables** [V withoutrowid.html §2]:
  1. A PRIMARY KEY is required.
  2. `INTEGER PRIMARY KEY` is not a rowid alias. It behaves like `INT PRIMARY KEY`, so a NULL is not replaced with a generated value.
  3. `AUTOINCREMENT` is an error.
  4. NOT NULL is enforced on every primary-key column.
  5. `sqlite3_last_insert_rowid()` and `last_insert_rowid()` are not updated. A probe of `last_insert_rowid()` returned 0. [V measured]
  6. No `sqlite3_blob` incremental I/O.
  7. `sqlite3_update_hook()` does not fire.
- **Related.**
  - For the preupdate hook, the rowid arguments are "undefined" for any operation on a WITHOUT ROWID table. [V https://sqlite.org/c3ref/preupdate_blobwrite.html]
  - The session extension has supported WITHOUT ROWID since 3.17.0, but "extra steps are needed". [V https://sqlite.org/sessionintro.html]
  - The spec has the engine write the cache itself, so hooks are not on the main path. [I]
- **Storage.** The table is an index B-tree keyed on the primary-key columns followed by the remaining columns. Each secondary index entry is "the columns that are being indexed followed by the key of the corresponding table row". For WITHOUT ROWID tables, that row key is the whole PRIMARY KEY. [V https://sqlite.org/fileformat2.html §2.4, §2.5]

#### 1.2.2 Who uses it
- No practitioner source read recommends WITHOUT ROWID by default. Search results treat it as a targeted optimisation. [V search; e.g. https://benjamincongdon.me/blog/2025/12/05/TIL-SQLites-WITHOUT-ROWID/ (not opened)]
- None of the server-side schemas inspected use it: goqite, wtf, PocketBase, Rails, Django or sqlc examples. [V sources in §5.1]
- Turso, a from-scratch Rust rewrite of SQLite, lists WITHOUT ROWID as "Partial", behind `--experimental-without-rowid`, and "effectively insert-only". [V https://github.com/tursodatabase/turso/blob/main/COMPAT.md] For a rewrite of SQLite, WITHOUT ROWID was a late feature. That is weak evidence about demand, and it does not apply directly here, because SQLite does the storage in this project. [I]

#### 1.2.3 Scratch probe: STRICT rowid against STRICT WITHOUT ROWID
The probe used the same schema in two layouts, `STRICT` and `STRICT, WITHOUT ROWID`:
- `users(id TEXT PK, email TEXT UNIQUE, name, created)` with an index on `created`, and 100,000 rows of about 100 bytes.
- `posts(id TEXT PK, user_id REFERENCES users, title, body ~2000 chars, created)` with an index on `(user_id, created)`, and 100,000 rows.
- Keys are `'u_'`/`'p_'` followed by 32 hex characters from `randomblob(16)`.
- 4 KiB pages, WAL mode, warm cache, one run each. [V measured]

| Measurement | rowid (STRICT) | WITHOUT ROWID | Ratio |
|---|---|---|---|
| users: table + all indexes | 23 MB (table 10, PK index 4, email 3, created 6) | 29 MB (table 11, email 6, created 12) | 1.26 times larger |
| posts: table | 391 MB, 0 overflow pages | 447 MB, 100,000 overflow pages | every row overflows |
| posts `(user_id, created)` index | 7 MB | 10 MB | |
| 100,000 lookups of users by text PK | 0.125 s | 0.075 s | WITHOUT ROWID about 1.7 times faster |
| 100,000 lookups of users by `email` (secondary) | 0.115 s | 0.143 s | WITHOUT ROWID about 25% slower |
| 20,000 lookups of posts by PK, reading `body` | 0.048 s | 0.23 s | WITHOUT ROWID about 4.7 times slower |
| Bulk load of both tables | 2.3 s | 4.2 s | WITHOUT ROWID about 1.9 times slower |

- An unordered `SELECT ... FROM users LIMIT 200` returned different rows in the two layouts: the first 200 inserted in one, and the first 200 by key in the other. [V measured]
- **Reading [I]:**
  - For small rows looked up by key, such as sessions, tokens, settings, join tables, and lookups by composite key, WITHOUT ROWID wins.
  - For content tables with text bodies or JSON, and tables queried mainly through secondary indexes, it loses. The loss is large when rows exceed the 1/20-page rule.
  - Most web apps have both kinds of table.
  - Two options would soften the cost without giving up the "always WITHOUT ROWID" rule. One is a larger page size for the cache. The other is to move large text into a separate table with a narrow key. Neither was measured.

### 1.3 Primary keys
- **Integer keys (rowid tables).**
  - `INTEGER PRIMARY KEY` is the fastest key. Note that the exact type name "INTEGER" is required, and `INT` or `BIGINT` does not count. [V https://sqlite.org/lang_createtable.html §rowid]
  - `AUTOINCREMENT` "imposes extra CPU, memory, disk space, and disk I/O overhead and should be avoided if not strictly needed". [V https://sqlite.org/autoinc.html]
  - Neither is available under the WITHOUT ROWID decision, where an integer key must be supplied by the application. [V withoutrowid.html]
- **Text compared with blob keys.** Hipp (forum, 2021-03-04) said: "Both approaches should work fine. Storing the hash as a BLOB might be very slightly faster... [Fossil uses text]: Performance is not an issue, and text is easier for developers to deal with when debugging." [V https://sqlite.org/forum/forumpost/c7771e9442]
- **Where keys are generated.**
  - In the database: goqite uses `id text primary key default ('m_' || lower(hex(randomblob(16))))` [V goqite schema]. PocketBase uses `TEXT PRIMARY KEY DEFAULT ('r'||lower(hex(randomblob(7)))) NOT NULL` [V https://github.com/pocketbase/pocketbase/blob/master/migrations/1640988000_init.go].
  - In the application: Kerkour uses a `BLOB` key that the application generates. [V kerkour.com]
  - `randomblob()` is non-deterministic. If keys come from `DEFAULT` expressions, the engine must record the random inputs, which the spec already provides for. [I]
- **UUIDv4, UUIDv7 and ULID.**
  - Anders Murphy (2026-06-05) loaded 10 million rows in batches of 1 million (16-byte blob keys, Clojure, one run each). Last-batch times [V https://andersmurphy.com/2026/06/05/the-perils-of-uuid-primary-keys-in-sqlite.html]:
    - integer rowid: 715 ms
    - UUIDv4 in a WITHOUT ROWID table: 12,586 ms
    - UUIDv7 in a WITHOUT ROWID table: 1,258 ms
    - UUIDv4 in a rowid table with a unique index: 7,119 ms
  - The author's conclusion is to prefer time-ordered keys if UUIDs are used.
  - A second benchmark (lik.ai, 2025-10-15, read only as a summary) found "very little difference between UUIDv4 and UUIDv7", and integers fastest. The two disagree, so treat both with care. [I https://engineered.at/articles/sqlite-primary-key-benchmarks-uuidv7-uuidv4-snowflake-integer]
  - SQLite has no built-in UUID function. An experimental `uuid.c` extension exists. [I search result: https://www.sqlite.org/src/info/1a6e009372cf9571]
- **Consequence for the subset [I].**
  - Under STRICT plus WITHOUT ROWID, the realistic key types are TEXT (prefixed random or time-ordered IDs), BLOB (16-byte UUIDs), INTEGER supplied by the application, and composite keys.
  - Supporting `DEFAULT (expr)` with `randomblob`, `hex`, `lower` and `||` early would let the owner's current schema style work.
  - A time-ordered ID (UUIDv7 or ULID-style, made by the application) avoids most of the cost of random inserts.

### 1.4 Timestamps and dates
- **SQLite has no date type.** Store ISO-8601 TEXT, REAL Julian days, or INTEGER Unix time, and "pick one format". [V https://sqlite.org/quirks.html §3.2]
- **Functions and versions.**
  - `unixepoch()` was added in 3.38.0 (2022-02-22), the `subsec` modifier in 3.42.0, `timediff()` in 3.43.0, `%` substitutions in `strftime` in 3.46.0, and the "end of" modifier and `weekday -N` in 3.54.0. [V changes.html; lang_datefunc.html]
  - `'now'` comes from the VFS clock and is the same for every call within one `sqlite3_step()`. [V https://sqlite.org/lang_datefunc.html]
- **`CURRENT_TIMESTAMP` pitfalls.**
  - It gives UTC as `YYYY-MM-DD HH:MM:SS`, with a space, no `T`, no zone suffix, and whole seconds only. [V lang_createtable.html; V measured]
  - Mixed with ISO-8601 `T` strings, text comparison is wrong: `'2026-11-10 12:00:00' < '2026-11-10T11:00:00'` is true, because a space sorts before `T`. [V measured]
  - `ALTER TABLE ADD COLUMN` cannot use `CURRENT_TIMESTAMP` or any parenthesised default. [V https://sqlite.org/lang_altertable.html]
- **Practitioners disagree. The common choices are:**
  - **TEXT, ISO-8601 UTC with milliseconds.** goqite uses `strftime('%Y-%m-%dT%H:%M:%fZ')`. [V] PocketBase uses `strftime('%Y-%m-%d %H:%M:%fZ')`, with a space separator. [V] Ben Johnson's wtf uses RFC 3339 UTC formatted in Go. [V https://github.com/benbjohnson/wtf/blob/main/sqlite/sqlite.go; schema circa 2021, dated]
  - **INTEGER Unix milliseconds.** Kerkour uses "Unix millisecond timestamps with a custom Time type which offers the best performance". [V kerkour.com]
  - **What Go drivers do.**
    - mattn/go-sqlite3 writes `time.Time` as `2006-01-02 15:04:05.999999999-07:00`. It parses values back to `time.Time` only when the declared type is `date`, `datetime` or `timestamp`. [V https://github.com/mattn/go-sqlite3/blob/master/sqlite3.go, `SQLiteTimestampFormats` and `columnTimestamp`]
    - modernc.org/sqlite writes Go's `String()` format by default, "including any monotonic clock reading". It has `_time_format` and `_time_integer_format` (`unix`, `unix_milli` and others). `_texttotime` applies only to columns declared `DATE`, `DATETIME`, `TIME` or `TIMESTAMP`. [V https://pkg.go.dev/modernc.org/sqlite, v1.62.0]
    - SQLite's date functions accept the mattn format with nanoseconds and an offset. `datetime('2026-11-10 12:00:00.123456789-07:00')` gives `'2026-11-10 19:00:00'`. [V measured]
  - **Consequence [I].** Under STRICT, none of the driver magic keyed on declared types applies. The application must choose one TEXT format, or INTEGER milliseconds, and convert it itself. A single fixed-width format such as `strftime('%Y-%m-%dT%H:%M:%fZ')` sorts correctly as text. Go's default formats with variable fractions or offsets do not sort correctly.
- **Non-determinism [I].**
  - The `localtime` and `utc` modifiers depend on the process time zone and on `localtime_r`, which "normally only works for years between 1970 and 2037". [V lang_datefunc.html §5]
  - For a deterministic simulation, rejecting these modifiers is cleaner than recording the time zone.

### 1.5 Booleans
- There is no boolean type. `TRUE` and `FALSE` (since 3.23.0) are the integers 1 and 0, except that columns named TRUE or FALSE take precedence "for backwards compatibility". [V quirks.html §3.1]
- Under STRICT, use `INTEGER NOT NULL CHECK (x IN (0, 1))`. [I common practice]
- Semantics to match:
  - `x IS TRUE` is true for any non-zero number: `2 IS TRUE` = 1, while `'x' IS TRUE` = 0. [V measured]
  - Values are cast to NUMERIC for truth testing, so `'1english'` is true. [V lang_expr.html]
- PocketBase declares `BOOLEAN DEFAULT FALSE`, which STRICT does not allow. [V]

### 1.6 Money and decimals
- "If you need an exact answer, you should not use binary-64 floating-point values." Only some cent values are exact. [V https://sqlite.org/floatingpoint.html]
- SQLite ships an optional `decimal.c` extension (`decimal_add`, `decimal_sum`, ...). It is not in the core library. [V floatingpoint.html §2.2]
- The usual practice is an INTEGER count of minor units. [I]
- Values to match when floats are involved: `round(0.125, 2)` = 0.13 and `round(-2.5)` = -3.0. Float rendering also changed in recent releases: 3.53.0 added `SQLITE_DBCONFIG_FP_DIGITS`. [V measured; changes.html]

### 1.7 JSON columns and the JSON functions
- The JSON functions have been built in since 3.38.0. `->` and `->>` also arrived in 3.38.0. JSONB, a binary format stored as a BLOB, arrived in 3.45.0. [V https://sqlite.org/json1.html; changes.html]
- **When to use JSONB.** Use the `jsonb_` variant for a function whose output feeds another JSON function. The aggregates are the exception. JSONB's format is internal and must be treated as opaque. [V json1.html]
- **Legacy.** BLOB arguments are read as text JSON if no other reading works "for backward compatibility". The docs advise an `UPDATE` so that legacy JSON stored as a BLOB "is actually stored as TEXT". [V json1.html, 3.45.1 note] Under STRICT, JSON text belongs in a TEXT column and JSONB in a BLOB column.
- **`->` compared with `json_extract`.** `->` always returns JSON text, and `->>` returns an SQL value. `json_extract` returns JSON only for arrays or objects, or with several paths. The SQLite docs contrast `json_extract` with MySQL here. [V json1.html]
- **Validation.**
  - Django's SQLite backend adds `CHECK (JSON_VALID(col) OR col IS NULL)` to JSON fields and stores them as `text`. [V https://github.com/django/django/blob/main/django/db/backends/sqlite3/base.py]
  - The docs suggest `json_valid(x, 6)` for "plausible input" checks. [V json1.html]
- **Indexing JSON.** Generated virtual columns plus an index on them. [V Anton Zhiyanov, "JSON and virtual columns in SQLite", 2022-05-15, https://antonz.org/json-virtual-columns/]
- **Wide use.** Ben Johnson and Kerkour both list JSON support as a reason SQLite suffices. [V https://fly.io/blog/all-in-on-sqlite-litestream/; kerkour.com]

### 1.8 Generated columns
- Added in 3.31.0. VIRTUAL is the default. [V https://sqlite.org/gencol.html]
- Limitations [V gencol.html §2.3]:
  - no `DEFAULT`
  - cannot be part of the PRIMARY KEY
  - only deterministic scalar functions, with no subqueries, aggregates or window functions
  - cannot reference the rowid
  - `ALTER TABLE ADD COLUMN` cannot add a STORED column
- An index on a VIRTUAL column is an expression index. [V gencol.html]
- In STRICT tables, generated columns are type-checked (3.51.0+). [V changes.html]

### 1.9 DEFAULT expressions
- A default may be a literal, a signed number, or "a constant expression in parentheses", evaluated once per inserted row. It may not contain subqueries, column references, bound parameters or double-quoted strings. [V lang_createtable.html]
- Defaults seen in practice: `(strftime(...))`, `('prefix' || lower(hex(randomblob(n))))`, and `0`. [V goqite, PocketBase]
- PocketBase uses double-quoted string defaults (`DEFAULT "base"`, `DEFAULT "[]"`). Those depend on the DQS misfeature and would be rejected by the subset. [V PocketBase init migration]
- Since 3.54.0 the authorizer callback is invoked for SQL functions in DEFAULT clauses. [V changes.html]

### 1.10 CHECK constraints
- A CHECK is evaluated on INSERT and UPDATE only. Its result is cast to NUMERIC, and NULL passes: "If the CHECK expression evaluates to NULL, or any other non-zero value, it is not a constraint violation." It may not contain a subquery. [V lang_createtable.html; NULL passing V measured]
- `ON CONFLICT` is not allowed on CHECK constraints. [V https://sqlite.org/lang_conflict.html]
- Since 3.53.0, `ALTER TABLE ... ADD CONSTRAINT name CHECK (...)` and `DROP CONSTRAINT name` work. [V changes.html 3.53.0 item 3a; V measured on 3.54.1]

### 1.11 NOT NULL discipline
- NOT NULL can only be a column constraint. [V lang_createtable.html]
- In ordinary rowid tables, PRIMARY KEY columns may hold NULLs "due to an historical oversight". [V quirks.html §5] Practitioners respond by always writing `NOT NULL` explicitly. [V menial.co.uk 2025-10-20, https://menial.co.uk/blog/2025/10/20/rowids-and-primary-keys/] STRICT and WITHOUT ROWID both make this unnecessary.
- `ALTER TABLE ... ALTER COLUMN x SET NOT NULL` and `DROP NOT NULL` exist since 3.53.0. [V lang_altertable.html §6]
- Adding a NOT NULL column with `ADD COLUMN` needs a non-NULL default. [V lang_altertable.html]

### 1.12 Foreign keys
- **Off by default.** Enforcement is off "for backwards compatibility". It must be enabled on every connection with `PRAGMA foreign_keys=ON`, which "does nothing inside a multi-statement transaction". Alternatively, compile with `SQLITE_DEFAULT_FOREIGN_KEYS=1`. [V https://sqlite.org/foreignkeys.html; https://sqlite.org/compile.html]
- **Practitioners.**
  - Ben Johnson: "For historical reasons, SQLite does not check foreign key constraints by default... which is kinda insane." [V wtf sqlite.go]
  - Rails 8, PocketBase, Litestream's tips, Kerkour and glue all turn foreign keys on. [V]
- **Indexes on child keys.** These "are not required ... but they are almost always beneficial". Without one, deleting or updating a parent row scans the child table. [V foreignkeys.html §3] wtf indexes `dials(user_id)`. [V]
- **Parent keys.** The parent key must be the PRIMARY KEY or have a UNIQUE index with matching collations. Otherwise a "foreign key mismatch" error is raised at DML time. [V foreignkeys.html]
- **Actions.**
  - `NO ACTION` (the default), `RESTRICT`, `SET NULL`, `SET DEFAULT` and `CASCADE`. `RESTRICT` is checked immediately, even on deferred constraints. `ON UPDATE` fires only if the key value actually changes. [V foreignkeys.html §4]
  - `MATCH` clauses are parsed and ignored. [V]
  - Errors say only "FOREIGN KEY constraint failed", without naming the constraint. [V; measured]
- **Deferred constraints.**
  - `DEFERRABLE INITIALLY DEFERRED` checks at COMMIT. `PRAGMA defer_foreign_keys` defers all constraints temporarily.
  - A constraint's mode is fixed when it is created. Only `DEFERRABLE INITIALLY DEFERRED` defers; the other spellings are immediate. [V foreignkeys.html §4.2]
- **DROP TABLE with enforcement on.** It performs an implicit `DELETE`, which fires FK actions but not triggers. [V foreignkeys.html §5] See §4.3 for what that does to migrations.
- **`INSERT OR REPLACE` and cascades.** The REPLACE strategy deletes the conflicting row, and FK actions fire. In a probe, `INSERT OR REPLACE INTO p VALUES('a','new')` silently deleted every child row under `ON DELETE CASCADE`. `INSERT ... ON CONFLICT(id) DO UPDATE` kept them. [V measured] This is the strongest practical argument for leaving out `REPLACE`.

### 1.13 UNIQUE, partial and expression indexes
- **UNIQUE.** NULLs are distinct from each other, so two NULLs in a UNIQUE column are allowed. [V lang_createtable.html; V measured]
- **Partial indexes.** The planner uses a partial index only when it can prove the query's WHERE implies the index's WHERE. Evan Schwartz found that `0.9` against `.9` was enough to stop the planner using it. [V Evan Schwartz, "Subtleties of SQLite Indexes", 2025-09-29, https://emschwartz.me/subtleties-of-sqlite-indexes/; rules at https://sqlite.org/partialindex.html]
- **Composite index rule.** "Left to right, no skipping, stops at the first range". Prefer "one or a small handful of good composite indexes" over one index per column. [V emschwartz.me]
- **Expression indexes.**
  - 3.53.0 added `REINDEX EXPRESSIONS` "to repair stale expression indexes". 3.54.0 stopped rewriting an expression index when the indexed value is unchanged. [V changes.html]
  - Expression indexes that use `LIKE` become "corrupt" if `case_sensitive_like` changes. That is a reason, already covered by the no-PRAGMA rule, to keep `LIKE` fixed. [V https://sqlite.org/pragma.html#pragma_case_sensitive_like]
- **The LIKE optimisation.** It applies only when the column is indexed with `NOCASE` and `case_sensitive_like` is off, or with `BINARY` and it is on. By default, a `LIKE 'prefix%'` on a BINARY-indexed column cannot use the index. Use `GLOB`, a range query, or a `COLLATE NOCASE` index. [V https://sqlite.org/optoverview.html §5]

---

## 2. Query and transaction practice

### 2.1 Connection settings people use
All values below are verified from each source. They are connection settings, not schema, and for sql4s3 they belong to the cache configuration, not to the SQL surface. [I]

| Source | journal_mode | busy_timeout | synchronous | foreign_keys | Other |
|---|---|---|---|---|---|
| Kerkour, 2024-02-15 | WAL | 5000 (APIs), 15000+ (queues) | NORMAL | ON | `cache_size=1000000000`, `temp_store=memory`; `_txlock=immediate` |
| Rails 8 adapter `DEFAULT_PRAGMAS` | WAL | `timeout:` option | NORMAL | ON | `mmap_size` 128 MB, `journal_size_limit` 64 MB, `cache_size` 2000; `default_transaction_mode: :immediate` |
| PocketBase `core/db_connect.go` | WAL | 10000 (set first, before WAL) | NORMAL | ON | `journal_size_limit` 200 MB, `temp_store` MEMORY, `cache_size` -32000, `_defensive=1` |
| Litestream tips (v0.5.x) | WAL (required) | 5000 | NORMAL | ON | `wal_autocheckpoint=0` under very high write load |
| maragudk/glue `sql/helper.go` | WAL | 5000 | (default) | ON | `_txlock=immediate` |
| phiresky, 2020 (updated 2022; **dated**) | WAL | | NORMAL | | `temp_store=memory`, `mmap_size=30000000000`, `PRAGMA optimize` before close |

Sources: https://kerkour.com/sqlite-for-servers; https://github.com/rails/rails/blob/main/activerecord/lib/active_record/connection_adapters/sqlite3_adapter.rb; https://github.com/pocketbase/pocketbase/blob/master/core/db_connect.go; https://litestream.io/tips/; https://github.com/maragudk/glue/blob/main/sql/helper.go; https://phiresky.github.io/blog/2020/sqlite-performance-tuning/

The SQLite docs on these settings:
- `SQLITE_DEFAULT_WAL_SYNCHRONOUS=1` is a recommended compile option. With NORMAL in WAL mode, "transactions are no longer durable and might rollback following a power failure". [V https://sqlite.org/compile.html; https://sqlite.org/wal.html] The spec makes the cache rebuildable from S3, so NORMAL is safe for the cache. [I]
- `trusted_schema` is ON by default "to maintain backwards compatibility", and "all applications are encouraged to switch this setting off". [V https://sqlite.org/pragma.html#pragma_trusted_schema]
- Shared-cache mode "is an obsolete feature. The use of shared-cache mode is discouraged". [V https://sqlite.org/sharedcache.html] Crawshaw's 2018 notes (dated) and mattn's FAQ still suggest `cache=shared`. [V https://crawshaw.io/blog/one-process-programming-notes; https://github.com/mattn/go-sqlite3]
- The recommended compile-time options include `SQLITE_DQS=0`, `SQLITE_DEFAULT_MEMSTATUS=0`, `SQLITE_DEFAULT_WAL_SYNCHRONOUS=1`, `SQLITE_LIKE_DOESNT_MATCH_BLOBS`, `SQLITE_MAX_EXPR_DEPTH=0`, `SQLITE_OMIT_DECLTYPE`, `SQLITE_OMIT_DEPRECATED`, `SQLITE_OMIT_PROGRESS_CALLBACK`, `SQLITE_OMIT_SHARED_CACHE`, `SQLITE_OMIT_AUTOINIT` and `SQLITE_STRICT_SUBTYPE=1`. [V compile.html §2]
  - `SQLITE_LIKE_DOESNT_MATCH_BLOBS` changes the result of `LIKE` on BLOBs. If the oracle build uses it, the engine must match the oracle's choice. [I]
  - `SQLITE_OMIT_DECLTYPE` would break drivers that read the declared type, such as mattn's time handling. [I from mattn source]

### 2.2 BEGIN IMMEDIATE against deferred transactions
- **SQLite docs.**
  - `BEGIN` is DEFERRED. A write inside a read transaction upgrades it "if possible". If another connection has written in the meantime, it "will fail with SQLITE_BUSY". In WAL mode, `IMMEDIATE` and `EXCLUSIVE` are the same. [V https://sqlite.org/lang_transaction.html]
  - In WAL mode the error is `SQLITE_BUSY_SNAPSHOT`. If X "knows it will eventually want to write", it should issue `BEGIN IMMEDIATE`. [V https://sqlite.org/isolation.html]
  - The busy handler is not invoked when "invoking the busy handler could result in a deadlock". [V https://sqlite.org/c3ref/busy_handler.html]
- **Practitioners.**
  - Kerkour: "by upgrading a transaction after it has started, SQLite will immediately return a SQLITE_BUSY error without respecting the busy_timeout". [V]
  - Stephen Margheim, 2024-04-15: making transactions IMMEDIATE removed nearly all 500 errors in his demo. He also replaced the Ruby busy handler with one that sleeps a fixed 1 ms, because SQLite's backoff "penalizes 'older queries'". [V https://fractaledmind.com/2024/04/15/sqlite-on-rails-the-how-and-why-of-optimal-performance/]
  - Simon Willison, 2025-02-17, linking Bert Hubert, calls it "SQLite's single biggest footgun". [V https://simonwillison.net/2025/Feb/17/sqlite-busy/]
- **Driver support.**
  - Rails 8 defaults to `default_transaction_mode: :immediate`. [V Rails adapter source]
  - Django has the `transaction_mode` option, with values DEFERRED, IMMEDIATE or EXCLUSIVE, since 5.1. [V Django backend source; version from Kerkour]
  - In Go: mattn and modernc have `_txlock=immediate` [V READMEs], and zombiezen has `sqlitex.ImmediateTransaction`. [V https://pkg.go.dev/zombiezen.com/go/sqlite/sqlitex]
- **Trade-off.** `_txlock=immediate` applies to *every* `BeginTx`, so read-only transactions also take the write lock and run one at a time. [I from the mattn option semantics] The fix is the split into read and write pools (§2.3).
- **Consequence for sql4s3 [I].** The spec already runs one writer at a time, with waiting. The upgrade failure can be designed out if the engine's transaction API says up front whether a transaction reads or writes. `BEGIN`, `BEGIN IMMEDIATE`, `COMMIT`, `ROLLBACK` and maybe `SAVEPOINT` are the syntax worth accepting. `BEGIN EXCLUSIVE` adds nothing in WAL mode.

### 2.3 Read and write connections and pools
- Kerkour: "1 Write connection, multiple Read connections". `writeDB.SetMaxOpenConns(1)` and `readDB.SetMaxOpenConns(max(4, runtime.NumCPU()))`. [V]
- Margheim (Rails): a reader pool of read-only connections and a writer pool with one connection. Without the split, writes can starve reads in the pool. [V]
- PocketBase: `ConcurrentDB()` for reads and `NonconcurrentDB()`, "limited only to a single open connection", for writes. Inside a transaction both refer to the same transaction. [V https://github.com/pocketbase/pocketbase/blob/master/core/app.go]
- Crawshaw (2018, dated): "a read-only pool of connections and a single guarded read-write connection in a pool of 1". [V]
- The spec already takes this shape ("one write transaction at a time", parallel reads). [V docs/spec.md]

### 2.4 Prepared statements and parameters
- SQLite parameters are `?`, `?NNN`, `:name`, `@name` and `$name`. Unbound parameters are NULL. [V https://sqlite.org/lang_expr.html §parameters]
- **Statement caches.** Rails caches 1000 statements per connection by default (`statement_limit`) [V Rails adapter docs]. zombiezen caches statements in `Execute` [V]. mattn's `_stmt_cache_size` defaults to 0, meaning disabled [V].
- **Bound parameters and planning.** The planner re-prepares a statement when a parameter bound to the right-hand side of `LIKE` or `GLOB` changes, so that the LIKE optimisation can apply. [V optoverview.html §5] A subset engine with its own planner does not need to copy this, but results must match. [I]
- Hipp's first question in a slow-insert thread was "Are you preparing a new SQL statement for each row?" [V https://sqlite.org/forum/forumpost/5354951e21]
- **Suggested subset priority [I].** Support `?` and `?NNN` first. sqlc's SQLite examples use `?` [V https://github.com/sqlc-dev/sqlc/blob/main/examples/authors/sqlite/query.sql], and it is the usual `database/sql` placeholder for SQLite drivers [I]. Named parameters can come later.

### 2.5 RETURNING
- Added in 3.35.0. It is top-level only, so it is not allowed in triggers, subqueries or CTEs. Row order is arbitrary. Values are taken before AFTER triggers run, and changes made by FK actions or triggers are not reported. Aggregates are not allowed at the top level. [V https://sqlite.org/lang_returning.html]
- In an upsert, RETURNING reports inserted and updated rows. With `DO NOTHING`, a skipped row returns nothing. [V docs; DO NOTHING V measured]
- sqlc has open SQLite issues around `RETURNING` combined with `ORDER BY`/`LIMIT` on UPDATE. [V https://github.com/sqlc-dev/sqlc/issues/3600]

### 2.6 Upsert against INSERT OR REPLACE
- Upsert was added in 3.24.0, and multiple `ON CONFLICT` clauses in 3.35.0. `excluded.` refers to the row that failed to insert. [V https://sqlite.org/lang_upsert.html]
- A parsing wrinkle: in `INSERT ... SELECT ... ON CONFLICT`, the SELECT "should always include a WHERE clause, even if that WHERE clause is just 'WHERE true'". [V lang_upsert.html] That is a reason to support `INSERT ... VALUES ... ON CONFLICT` first. [I]
- `REPLACE` deletes the conflicting rows. Delete triggers fire only if recursive triggers are on, and the update hook is not called. [V lang_conflict.html] FK cascades do fire (§1.12, measured).
- Bug history: an UPSERT bug "introduced by enhancement 3a in version 3.35.0" could put an index out of sync. It is fixed, but it shows these corners are tricky. [V changes.html]
- The decision on upserts is deferred. The evidence favours `ON CONFLICT ... DO NOTHING` and `DO UPDATE` over any `OR REPLACE`. [I]

### 2.7 Pagination
- `OFFSET` "requires time proportional to the offset value". SQLite computes `LIMIT x+y` and discards the first y rows. The docs' recommended alternative uses row values: `WHERE (lastname, firstname) > (?1, ?2) ORDER BY lastname, firstname LIMIT 7`. [V https://sqlite.org/rowvalue.html §3.1]
- LIMIT details to match: a negative LIMIT means no limit, and a negative OFFSET counts as zero. The `LIMIT a, b` comma form reverses the meaning, and "programmers are strongly encouraged to use the form ... that uses the 'OFFSET' keyword". [V https://sqlite.org/lang_select.html §5]
- **Consequence [I].** Keyset pagination needs row-value comparisons, or the expanded `a > ? OR (a = ? AND b > ?)` form, plus `ORDER BY` on an index. Row values are worth an early slice.

### 2.8 Many small queries
- "With SQLite, 200 or more SQL statement per webpage is not a problem." The SQLite site's own timeline pages use the N+1 pattern. [V https://sqlite.org/np1queryprob.html]
- Ben Johnson: "per-query latency drop to 10-20 microseconds". [V https://fly.io/blog/all-in-on-sqlite-litestream/, 2022]
- **Consequence [I].** For sql4s3, the cost of each statement in the engine's parse, plan and execute path matters more than complex optimisation. Caching parsed and planned statements matters.

### 2.9 CTEs and window functions in real apps
- Ben Johnson says SQLite covers "99.9% of what I typically need", naming windowing, CTEs, full-text search and JSON. [V fly.io all-in post]
- Since 3.35.0, a CTE used more than once defaults to MATERIALIZED. [V changes.html 3.35.0] Window functions arrived in 3.25.0 (2018-09-15), in the same release as `RENAME COLUMN`. [V changes.html]
- Turso's rewrite implements CTEs (including `WITH RECURSIVE`) fully but window functions only partly. That suggests CTEs come before window functions in the priority of a compatible rewrite. [V Turso COMPAT.md; inference I]

### 2.10 Full-text search
- FTS5 is a virtual table with an implicit integer rowid. External-content tables point at the content table's rowid or another integer column through `content_rowid`. [V https://sqlite.org/fts5.html]
- Over a WITHOUT ROWID table, `content='doc'` alone fails with "no such column: T.rowid". `content='doc', content_rowid='n'`, with `n INTEGER NOT NULL UNIQUE`, works. [V measured]
- Crawshaw kept FTS tables in a separate attached database, so that backups could skip them. [V crawshaw 2018, dated]
- Virtual tables fall outside the "all tables WITHOUT ROWID" rule and belong to a much later slice. [I]

---

## 3. The legacy list

**Reject** means the subset can avoid the behaviour by refusing syntax. **Match** means the behaviour comes from accepted syntax and must equal SQLite's exactly. **Moot** means STRICT plus WITHOUT ROWID already removes it.

| # | Behaviour | What the authors or practitioners say | Modern alternative | Reject or match |
|---|---|---|---|---|
| 1 | Flexible typing and type affinity on storage | "a feature, not a bug" (quirks §3); STRICT added 3.37.0 | STRICT | Storage: moot with STRICT. STRICT's own lossless coercions (`'12'` into INTEGER, 5 into TEXT) must be **matched** |
| 2 | Affinity applied to comparison operands | datatype3 §4.2 | none | **Match**: `int_col = '1'` is true and `'1' = 1` is false, even on STRICT columns (measured) |
| 3 | Optional datatype (`CREATE TABLE t(a,b)`) | quirks §3.3 | STRICT | Reject (STRICT does) |
| 4 | Type names mapped to affinity by substring (`CHARINT` gives INTEGER) | datatype3 §3.1 | STRICT's five names | Reject (STRICT does) |
| 5 | `ANY` type | | | Reject (decided) |
| 6 | No BOOLEAN; TRUE/FALSE as 1/0; columns named TRUE/FALSE win "for backwards compatibility" | quirks §3.1 | INTEGER with CHECK | **Match** 1/0 semantics; reject columns named TRUE or FALSE |
| 7 | No DATETIME type | quirks §3.2 | pick one format | **Match** the date functions; reject `localtime`/`utc` for determinism (optional) |
| 8 | Foreign keys off by default | "for backwards compatibility" (foreignkeys §2); "kinda insane" (Ben Johnson) | always on | Behaviour fixed on (decided); the actions must be **matched** |
| 9 | NULLs in PRIMARY KEY | "an historical oversight", kept for compatibility (quirks §5) | NOT NULL, STRICT, WITHOUT ROWID | Moot |
| 10 | Rowid, `oid`, `_rowid_` and their shadowing by declared columns; rowids renumbered by `VACUUM` | rowidtable.html: "The designer of SQLite offers his sincere apology for the current mess" | WITHOUT ROWID | Moot (these names become ordinary unknown columns) |
| 11 | `INTEGER PRIMARY KEY DESC` is not a rowid alias | "not by design ... a bug in early versions" | | Moot |
| 12 | Only the exact name `INTEGER` makes a rowid alias | lang_createtable | | Moot |
| 13 | `AUTOINCREMENT` (and its MySQL confusion, quirks §11) | "should be avoided if not strictly needed" (autoinc) | app-supplied keys | Moot (error on WITHOUT ROWID) |
| 14 | Bare columns in aggregate queries | "a feature, not a bug" (lang_select §2.5); arbitrary row; min()/max() special case | standard GROUP BY; window functions | Reject (require every result column to be aggregated or grouped) |
| 15 | `LIKE` case-insensitive for ASCII only; `upper`/`lower`/`NOCASE` ASCII only | quirks §7; ICU optional | ICU, application folding | **Match** (cannot reject `LIKE`) |
| 16 | `case_sensitive_like` PRAGMA | deprecated; "use of the case_sensitive_like pragma is discouraged" | none | Reject (no PRAGMAs) |
| 17 | LIKE on BLOBs | `SQLITE_LIKE_DOESNT_MATCH_BLOBS` recommended | | **Match** whatever the oracle build does; or reject BLOB operands at plan time where types are known |
| 18 | Integer overflow in `+ - *` silently becomes REAL | lang_expr | | **Match** (`9223372036854775807+1` gives REAL 9.2233720368547758e+18, measured) |
| 19 | `sum()` raises "integer overflow"; `total()` never does; `abs(-9223372036854775808)` raises an overflow error | lang_aggfunc; measured | | **Match** |
| 20 | Integer division truncates toward zero; `x/0` and `x%0` give NULL; `%` casts to INTEGER | lang_expr; measured | | **Match** |
| 21 | `CAST` text uses the longest numeric prefix (`'12abc'` gives 12, `'abc'` gives 0) and clamps out-of-range values | lang_expr | | **Match** |
| 22 | Integer and text literals differ (`1='1'` is false) | quirks §13 | | **Match** |
| 23 | Double-quoted strings fall back to literals | "misfeature", kept for legacy (quirks §8); `SQLITE_DQS=0` recommended | single quotes | Reject (decided). The oracle and cache build should use `DQS=0` too |
| 24 | `[bracket]` and backtick identifiers | "not standard SQL ... included in SQLite for compatibility" with MS Access, SQL Server and MySQL (lang_keywords.html) | double quotes | Reject (decided) |
| 25 | Keywords usable as identifiers | "promotes backwards compatibility" (quirks §9) | quote them | Reject unquoted keywords (optional; stricter than SQLite but still a subset) |
| 26 | "Dubious SQL is allowed" (Postel's law) | quirks §10 | | Reject case by case |
| 27 | NUL characters inside text | quirks §12; nulinstr.html | | **Match**, or reject NUL-containing text at bind and literal time (a value-level restriction; a design choice) |
| 28 | Invalid UTF-8 stored unchanged ("Garbage-In, Garbage-Out") | invalidutf.html | | **Match**, or validate at bind time and reject. `CAST(X'96' AS TEXT)` creates invalid text from valid input. Turso chose replacement characters, which breaks exactness |
| 29 | Comma-join precedence wrong | quirks §14; lang_select §9.3 | parentheses; do not mix | Reject mixing comma joins with `JOIN` (join syntax decision deferred) |
| 30 | "Strange JOIN names" (`OUTER LEFT NATURAL`, `LEFT RIGHT JOIN`) | lang_select §9.1: "you ought not" | standard names | Reject |
| 31 | `ON` or `USING` allowed on comma joins and `CROSS JOIN`; outer join without `ON` | lang_select §9.2 | | Reject |
| 32 | `LIMIT offset, count` comma form | "strongly encouraged" to use `OFFSET` (lang_select §5) | `LIMIT n OFFSET m` | Reject |
| 33 | Result-column aliases visible in `WHERE` (`SELECT n*10 AS x ... WHERE x > 15` works) | measured; non-standard | repeat the expression | Reject (optional) |
| 34 | Empty `IN ()` list; `ISNULL`/`NOTNULL` postfix operators | measured; SQLite-specific | `IS NULL` | Reject (optional) |
| 35 | Conflict clauses in DDL (`UNIQUE ON CONFLICT REPLACE`, `NOT NULL ON CONFLICT IGNORE`) and statement-level `OR REPLACE`/`OR IGNORE`/`OR FAIL`/`OR ROLLBACK` | lang_conflict; REPLACE skips the update hook and delete triggers | upsert | Reject. `OR IGNORE` is the most-used of these and could come later |
| 36 | REPLACE substitutes the default for NULL in a NOT NULL column | lang_conflict | | Moot if REPLACE is rejected |
| 37 | FK `MATCH` clause parsed and ignored | foreignkeys.html | | Reject |
| 38 | FK deferral spelling (`DEFERRABLE` alone, `INITIALLY IMMEDIATE`, `NOT DEFERRABLE` are all immediate) | foreignkeys §4.2 | | Accept only `DEFERRABLE INITIALLY DEFERRED` or nothing |
| 39 | `PRAGMA legacy_alter_table` (pre-3.25.0 rename behaviour) | pragma.html | default rename | Reject (no PRAGMAs). Django sets it OFF explicitly |
| 40 | Deprecated PRAGMAs (`count_changes`, `full_column_names`, `short_column_names`, `empty_result_callbacks`, `default_cache_size`, `data_store_directory`, `temp_store_directory`, `case_sensitive_like`) | "deprecated. Do not use them. They exist for historical compatibility." | | Reject |
| 41 | `trusted_schema=ON` default | "for backwards compatibility"; switch it off | `SQLITE_TRUSTED_SCHEMA=0` | Cache build setting, not SQL |
| 42 | Shared-cache mode | "obsolete ... discouraged" | WAL | Build setting (`SQLITE_OMIT_SHARED_CACHE`) |
| 43 | `printf()` as an alias of `format()` | kept "for backwards compatibility" (3.38.0) | `format()` | Reject `printf` (optional) |
| 44 | JSON text stored in BLOBs accepted | json1, 3.45.1 note | TEXT for JSON, BLOB for JSONB | **Match** if JSON functions take BLOBs; or reject BLOB arguments that are not JSONB (hard to do statically) |
| 45 | `CREATE TABLE ... AS SELECT` makes untyped, unconstrained rowid tables | lang_createtable | explicit CREATE plus INSERT ... SELECT | Reject (it cannot produce a STRICT WITHOUT ROWID table) |
| 46 | `CURRENT_TIMESTAMP` gives UTC as `YYYY-MM-DD HH:MM:SS` | lang_createtable | `strftime('%Y-%m-%dT%H:%M:%fZ')` | **Match** if accepted |
| 47 | NULLs sort first in ASC | lang_select §4 | `NULLS LAST` (3.30.0) | **Match** |
| 48 | Non-aggregate HAVING and HAVING without GROUP BY | lang_select §2 | | **Match** if accepted |
| 49 | Recursive triggers off by default | pragma | | **Match** (fixed off, since there are no PRAGMAs) if triggers are accepted |
| 50 | `UPDATE ... ORDER BY/LIMIT` | `SQLITE_ENABLE_UPDATE_DELETE_LIMIT` only in source builds | | Reject |
| 51 | Same-connection reads may or may not see concurrent changes from the same connection | "undefined" (isolation.html) | | Avoid by API design, by not interleaving statements on one connection |

Sources for the table: https://sqlite.org/quirks.html, https://sqlite.org/datatype3.html, https://sqlite.org/lang_expr.html, https://sqlite.org/lang_select.html, https://sqlite.org/lang_aggfunc.html, https://sqlite.org/lang_conflict.html, https://sqlite.org/foreignkeys.html, https://sqlite.org/rowidtable.html, https://sqlite.org/autoinc.html, https://sqlite.org/pragma.html, https://sqlite.org/compile.html, https://sqlite.org/invalidutf.html, https://sqlite.org/nulinstr.html, https://sqlite.org/json1.html, https://sqlite.org/isolation.html, https://sqlite.org/sharedcache.html. Behaviour marked "measured" was checked on 3.54.1.

---

## 4. Schema migration practice

### 4.1 What ALTER TABLE can do (3.54.1)
- These forms are supported: `RENAME TO`, `RENAME COLUMN`, `ADD COLUMN`, `DROP COLUMN` (3.35.0+), and, since 3.53.0, `ALTER COLUMN ... SET/DROP NOT NULL` and `ADD/DROP CONSTRAINT` for CHECK constraints. [V https://sqlite.org/lang_altertable.html; changes.html 3.53.0; CHECK V measured]
- **`ADD COLUMN` restrictions:**
  - no PRIMARY KEY or UNIQUE
  - no `CURRENT_*` or parenthesised default
  - NOT NULL needs a non-NULL default
  - with FKs on, a `REFERENCES` column must default to NULL
  - no STORED generated column
  - since 3.37.0, new CHECK constraints are tested against existing rows [V]
- **`DROP COLUMN` fails** if the column is part of the PK, is UNIQUE, is indexed, appears in a partial index WHERE, appears in another column's CHECK, is used in an FK, is used in a generated column, or appears in a trigger or view. [V]
- **How it works.** ALTER edits the stored `CREATE TABLE` text and reparses the schema. Renames and plain column additions run in constant time, whatever the table size. [V lang_altertable.html §5.1] Since 3.54.0, ALTER refuses to add, drop or rename columns named ROWID, _ROWID_ or OID. [V changes.html]

### 4.2 The 12-step rebuild
The documented procedure for any other change [V lang_altertable.html §8]:
1. Turn foreign keys off.
2. `BEGIN`.
3. Save the indexes, triggers and views.
4. Create the new table under a temporary name.
5. Copy the data.
6. Drop the old table.
7. Rename the new table to the old name.
8. Recreate the indexes, triggers and views.
9. Recreate the affected views.
10. Run `PRAGMA foreign_key_check`.
11. `COMMIT`.
12. Turn foreign keys back on.

The docs warn against the "rename old first" variant, which corrupts references because renames now carry into triggers, views and FKs. [V]

### 4.3 Conflict with "foreign keys always on, no PRAGMAs"
Probes on 3.54.1 with `foreign_keys=ON` [V measured]:
- **`ON DELETE CASCADE` child.** Steps 2 to 11 succeed, and **all child rows are deleted** by the implicit `DELETE` in `DROP TABLE`. The rebuild loses data silently.
- **Plain FK (`NO ACTION`).** `DROP TABLE parent` fails with "FOREIGN KEY constraint failed".
- **`DEFERRABLE INITIALLY DEFERRED`, or `PRAGMA defer_foreign_keys=ON`.** The drop and rename succeed, but `COMMIT` fails with "FOREIGN KEY constraint failed". The violations counted during the drop are not cleared when the renamed table appears.

Conclusion [I]:
- With enforcement always on, a table that any FK references cannot be rebuilt through SQL. This covers changing a column type, adding an FK or UNIQUE, or changing the PK.
- The options are:
  1. Accept that such changes are impossible.
  2. Allow the rebuild as an engine operation outside the SQL surface. This fits the spec: change files are row-based and record schema changes, so a rebuild can be one atomic schema change.
  3. Allow `PRAGMA foreign_keys` outside transactions only for migrations, which would break the "no behaviour-changing settings" rule.
- The 3.53.0 ALTER additions (NOT NULL, CHECK) cover some common changes without a rebuild.

### 4.4 How migration tools handle SQLite
- **Django.** It sets `PRAGMA foreign_keys = ON` and `PRAGMA legacy_alter_table = OFF` on connect. Its schema editor rebuilds tables (`_remake_table`) for most changes, disabling FK checks around them, and uses `RENAME COLUMN` where it can. [V https://github.com/django/django/blob/main/django/db/backends/sqlite3/base.py and schema.py]
- **Rails.** `disable_referential_integrity` sets `defer_foreign_keys = ON` and `foreign_keys = OFF` around the block, then restores them. [V Rails adapter source]
- **golang-migrate.** The sqlite3 driver "will automatically wrap each migration in an implicit transaction by default". `x-no-tx-wrap=true` disables this, so that migrations can contain their own `BEGIN`/`COMMIT`. [V https://github.com/golang-migrate/migrate/blob/master/database/sqlite3/README.md]
- **goose.** Migrations run in a transaction unless the file is annotated `-- +goose NO TRANSACTION`. [V https://pressly.github.io/goose/documentation/annotations/] Because `PRAGMA foreign_keys` is a no-op inside a transaction, a rebuild needs that annotation or a pragma set outside goose. The Flamenco project, for example, disables FKs before running goose. [I secondary sources via search: https://git.supported.systems/blender/flamenco/commit/0ea3cf8c3f13b992d31d89caafe25438a3452a9f]
- **Atlas.** An old version of its SQLite driver adds `PRAGMA foreign_keys = off` to plans that need a rebuild. [I old mirror of the source; current docs not checked]
- **libSQL.** It adds a non-standard `ALTER TABLE ... ALTER COLUMN x TO <new definition>`. It "only applies to newly inserted or updated data - existing rows are not rewritten or revalidated", which is not SQLite behaviour and not allowed in this subset. [V https://github.com/tursodatabase/libsql/blob/main/libsql-sqlite3/doc/libsql_extensions.md]
- **The owner's glue helper** keeps its own `migrations` table with a text `version` key, and up and down SQL files. [V https://github.com/maragudk/glue/tree/main/sql/migrations]
- **Practitioner caveat.** "While you can ALTER TABLE DROP COLUMN since SQLite 3.35, it's generally slow when your tables are large." [V Kerkour]

---

## 5. Usage frequency

I found no survey or corpus study of which SQL features or built-in functions SQLite web apps use. The academic studies found by search are about Android apps and Java data-access layers, not feature frequency (§7). The evidence below is qualitative, from schemas and defaults that I read.

### 5.1 Schemas and defaults inspected

| Source | Keys | Timestamps | Types used | Features used |
|---|---|---|---|---|
| maragudk/goqite (owner, current) [V] | `text primary key default ('m_' \|\| lower(hex(randomblob(16))))` | TEXT `strftime('%Y-%m-%dT%H:%M:%fZ')` defaults | text, blob, integer; STRICT | DEFAULT expressions, AFTER UPDATE trigger that sets `updated`, composite index with `desc` |
| maragudk/glue (owner) [V] | `version text primary key` | | text | WAL, busy_timeout, FKs, `_txlock=immediate` |
| PocketBase (Go) [V] | `TEXT PRIMARY KEY DEFAULT ('r'\|\|lower(hex(randomblob(7))))` | TEXT `strftime('%Y-%m-%d %H:%M:%fZ')` | TEXT, BOOLEAN, JSON (not STRICT) | double-quoted string defaults, `CREATE INDEX IF NOT EXISTS`, JSON |
| Ben Johnson, wtf (Go, circa 2021, dated) [V] | `INTEGER PRIMARY KEY AUTOINCREMENT` | TEXT RFC 3339 from Go | INTEGER, TEXT | `REFERENCES ... ON DELETE CASCADE`, UNIQUE, composite UNIQUE, composite PK `(dial_id, "timestamp")`, FK index |
| sqlc examples (authors, booktest) [V] | `integer PRIMARY KEY AUTOINCREMENT` | `datetime NOT NULL DEFAULT CURRENT_TIMESTAMP` | integer, text, varchar, datetime | CHECK, DEFAULT literals, composite index |
| Rails 8 `NATIVE_DATABASE_TYPES` [V] | `integer PRIMARY KEY AUTOINCREMENT NOT NULL` | `datetime` | varchar, float, decimal, boolean, json, blob | |
| Django `data_types` [V] | `integer ... AUTOINCREMENT` (AutoField) | `datetime` | bool, varchar(n), decimal, char(32) for UUID, text for JSON | `JSON_VALID` CHECK |
| sqlc Go type mapping [V] | | `date`/`datetime`/`timestamp` give `time.Time` | `boolean` gives `bool`, `json` gives `json.RawMessage`, `any` gives `any`, other names give `string` | |

Sources: https://github.com/maragudk/goqite/blob/main/schema_sqlite.sql, https://github.com/maragudk/glue, https://github.com/pocketbase/pocketbase, https://github.com/benbjohnson/wtf/blob/main/sqlite/migration/00000000.sql, https://github.com/sqlc-dev/sqlc/tree/main/examples, https://github.com/sqlc-dev/sqlc/blob/main/internal/codegen/golang/sqlite_type.go, the Rails and Django sources above.

Observations [I]:
- Mainstream ORMs and generators emit non-STRICT type names and `AUTOINCREMENT` integer keys. None of them would load into sql4s3 unchanged.
- Hand-written Go schemas, the owner's and PocketBase's, already use text keys from `randomblob` defaults and strftime text timestamps. That style fits STRICT plus WITHOUT ROWID well.
- The features that recur in every schema: `CREATE TABLE`, `PRIMARY KEY`, `NOT NULL`, `UNIQUE`, `DEFAULT`, `REFERENCES` with `ON DELETE CASCADE`, `CREATE INDEX` (often composite), and simple CHECKs.
- The functions that recur: `strftime`, `randomblob`, `hex`, `lower`, `||`, the aggregates `count` and `max`, and `json_*`.
- Ben Johnson and Kerkour both name CTEs, window functions, FTS and JSON as what makes SQLite enough.

### 5.2 Suggested order of slices [I]
Based on the above, and on what a Go web app on SQLite does per request (read by key, list with filter, sort and limit, insert, update by key, delete by key):
1. `SELECT` of literals and expressions: storage classes, arithmetic with overflow to REAL, integer division, NULL logic, comparison, `CAST`, `||`, `IS`, `CASE`, `typeof`, `coalesce`, `length`, `lower`, `upper`.
2. `CREATE TABLE ... STRICT, WITHOUT ROWID` with the five types, `PRIMARY KEY` (single and composite), `NOT NULL` and constant `DEFAULT`; `INSERT ... VALUES` with `?` parameters; `SELECT ... WHERE pk = ?`; `UPDATE` and `DELETE` by key; `BEGIN`/`COMMIT`/`ROLLBACK`. This includes affinity on comparisons and STRICT's coercions on insert.
3. `CREATE INDEX` and `UNIQUE`; `WHERE` with `AND`/`OR`/`IN`/`BETWEEN`/`LIKE`; `ORDER BY` (with `NULLS FIRST/LAST`), `LIMIT ... OFFSET`, and row values for keyset pagination; `count`, `sum`, `max`, `min`, `avg`, `GROUP BY`, `HAVING`, and `DISTINCT`.
4. `REFERENCES` with all actions, always enforced; `CHECK`; `DEFAULT (expr)` with `strftime`, `randomblob`, `hex` and `lower`; the date and time functions without `localtime`.
5. Inner and left joins, with the syntax decision still open; subqueries (`IN`, `EXISTS`, scalar).
6. Upsert (`ON CONFLICT ... DO NOTHING`, `DO UPDATE` with `excluded.`) and `RETURNING`.
7. Triggers, at least the "set `updated` after update" pattern; views; `ALTER TABLE ADD/RENAME/DROP COLUMN` and the 3.53.0 constraint forms.
8. CTEs (including recursive), window functions, `group_concat`/`string_agg`, aggregate `FILTER` and `ORDER BY`.
9. JSON functions and `->`/`->>`; generated columns; partial and expression indexes.
10. FTS5 and other virtual tables.

---

## 6. Where sources disagree

### 6.1 WITHOUT ROWID everywhere
- **For.** The SQLite author's "perfect world" remark [V rowidtable.html]. One B-tree per table. Standard PK semantics.
- **Against.** The SQLite docs' own advice to use it only for non-integer or composite keys with small rows, and only after measuring [V withoutrowid.html]. The overflow and secondary-index costs measured in §1.2.3. No practitioner recommends it as a default [V search]. Turso treats it as experimental [V].
- **Middle ground [I].** Keep the rule, but give guidance: time-ordered keys, short keys, and large text in separate tables. Or relax the rule later for content tables, keeping INTEGER keys supplied by the application.

### 6.2 Integer against text or blob keys
- **Integer.** Fastest for rowid tables, with the smallest secondary indexes. "Lookups made by rowid are usually faster than other primary key types". [V menial.co.uk 2025; autoinc.html; Murphy 2026]
- **Text or blob.**
  - Hipp: either works; blob is "very slightly faster", text is "easier ... when debugging" [V forum 2021].
  - Random IDs need no coordination and are safe to expose [I].
  - Owner and PocketBase practice [V].
- **The split on UUID versions.** Murphy found v4 much worse than v7 as a clustered key. The lik.ai summary found "very little difference". [V / I] The benchmarks differ in method and neither is authoritative.

### 6.3 Timestamps
- **TEXT ISO-8601.** Readable, works directly with SQLite's date functions, and sorts correctly if fixed-width and UTC. Used by goqite, PocketBase, wtf, Rails and Django. [V]
- **INTEGER Unix ms.** Smaller and faster, no format drift. Used by Kerkour. [V]
- **The SQLite docs** take no side: "pick one". [V quirks.html §3.2]
- **The known trap.** Mixing `CURRENT_TIMESTAMP` (space, seconds) with `T`/`Z` ISO strings breaks ordering. [V measured]

### 6.4 STRICT
- Practitioners favour it [V]. The SQLite authors argue that flexible typing is better and ask for evidence that STRICT prevents bugs [V flextypegood.html].

### 6.5 Bare columns
- SQLite calls them "a feature, not a bug" and documents the min/max behaviour. Most other engines reject them. [V lang_select.html §2.5] The subset can reject them, at the cost of a convenient "row with the max" idiom. Window functions replace it. [I]

### 6.6 busy_timeout values
- 5 s (Kerkour for APIs, Litestream, glue, Margheim's demo), 10 s (PocketBase), 15 s and more (Kerkour for queues). [V] SQLite gives no recommendation. [I search]

---

## 7. What I could not find
- **Evan Schwartz on server practice.** Found only his 2025 index post (§1.13). The "Prefer STRICT tables" post is by Evan Hahn, not Evan Schwartz.
- **Jason Wyatt / Wafris.** The Wafris post found ("Rearchitecting: Redis to SQLite", 2024-09-23) is by Michael Buckbee. It is about read-heavy lookups, about 3 times faster than local Redis with naive settings. It has no schema or SQL-feature advice. [V https://wafris.org/blog/rearchitecting-for-sqlite] I found nothing by Jason Wyatt.
- **Eli Bendersky.** No SQLite post found. His blog archive did not load for searching.
- **Dan Kennedy.** No forum post relevant to these topics found. The WITHOUT ROWID insert-speed thread has replies from Hipp and Keith Medcalf only.
- **A Hipp forum post on UUID or random keys for WITHOUT ROWID tables.** Not found. The closest are the 2021 blob-or-text thread and the 2022 "alternatives to UUID" thread, which has no Hipp reply.
- **Usage frequency.** No survey, corpus analysis or telemetry of SQL feature use in SQLite web apps. Search found only studies of Android apps (Lyu et al. 2017, PocketData 2015) and Java data-access layers, none with feature counts I could verify. §5 rests on the schemas I inspected.
- **Whether sqlc's SQLite parser accepts `STRICT` and `WITHOUT ROWID` table options.** I did not locate the grammar file. Check before relying on sqlc with this schema style.
- **Current Atlas docs on SQLite rebuilds.** Only an old source mirror.
- **Full text of** the Fly.io WAL internals post (no date on the page), Simon Willison's notes on Margheim's Rails World talk, and the second UUID benchmark (only a summary).

---

## Sources

SQLite (sqlite.org, read 2026-11-10):
- https://sqlite.org/quirks.html
- https://sqlite.org/stricttables.html
- https://sqlite.org/withoutrowid.html
- https://sqlite.org/rowidtable.html
- https://sqlite.org/foreignkeys.html
- https://sqlite.org/autoinc.html
- https://sqlite.org/datatype3.html
- https://sqlite.org/flextypegood.html
- https://sqlite.org/lang_createtable.html
- https://sqlite.org/lang_altertable.html
- https://sqlite.org/lang_expr.html
- https://sqlite.org/lang_select.html
- https://sqlite.org/lang_aggfunc.html
- https://sqlite.org/lang_conflict.html
- https://sqlite.org/lang_upsert.html
- https://sqlite.org/lang_returning.html
- https://sqlite.org/lang_transaction.html
- https://sqlite.org/lang_datefunc.html
- https://sqlite.org/json1.html
- https://sqlite.org/gencol.html
- https://sqlite.org/partialindex.html
- https://sqlite.org/optoverview.html
- https://sqlite.org/rowvalue.html
- https://sqlite.org/np1queryprob.html
- https://sqlite.org/isolation.html
- https://sqlite.org/wal.html
- https://sqlite.org/whentouse.html
- https://sqlite.org/compile.html
- https://sqlite.org/pragma.html
- https://sqlite.org/sharedcache.html
- https://sqlite.org/floatingpoint.html
- https://sqlite.org/invalidutf.html
- https://sqlite.org/nulinstr.html
- https://sqlite.org/fts5.html
- https://sqlite.org/sessionintro.html
- https://sqlite.org/fileformat2.html
- https://sqlite.org/c3ref/busy_handler.html
- https://sqlite.org/c3ref/preupdate_blobwrite.html
- https://sqlite.org/changes.html

SQLite forum:
- https://sqlite.org/forum/forumpost/c7771e9442 (Hipp, blob or text keys, 2021)
- https://sqlite.org/forum/forumpost/5354951e21 (WITHOUT ROWID inserts; Hipp)
- https://sqlite.org/forum/info/caa6aca55a2abf3be720f6c4002426be7423b6eeac598e4a3e9c44e933f5db67 (UUID alternatives, 2022)

Practitioners:
- Sylvain Kerkour, "Optimizing SQLite for servers", 2024-02-15: https://kerkour.com/sqlite-for-servers
- Stephen Margheim, "SQLite on Rails: The how and why of optimal performance", 2024-04-15: https://fractaledmind.com/2024/04/15/sqlite-on-rails-the-how-and-why-of-optimal-performance/
- Simon Willison, 2025-02-17: https://simonwillison.net/2025/Feb/17/sqlite-busy/
- Simon Willison, 2026-07-11: https://simonwillison.net/2026/Jul/11/sqlite-utils/
- Evan Hahn, "Prefer STRICT tables in SQLite", 2026-07-11: https://evanhahn.com/prefer-strict-tables-in-sqlite/
- Evan Schwartz, "Subtleties of SQLite Indexes", 2025-09-29: https://emschwartz.me/subtleties-of-sqlite-indexes/
- Anton Zhiyanov, "STRICT tables in SQLite", 2024-05-04: https://antonz.org/sqlite-strict-tables/
- Anton Zhiyanov, "JSON and virtual columns in SQLite", 2022-05-15: https://antonz.org/json-virtual-columns/
- Anders Murphy, "The perils of UUID primary keys in SQLite", 2026-06-05: https://andersmurphy.com/2026/06/05/the-perils-of-uuid-primary-keys-in-sqlite.html
- menial.co.uk, "Rowids and Primary Keys", 2025-10-20: https://menial.co.uk/blog/2025/10/20/rowids-and-primary-keys/
- phiresky, "SQLite performance tuning", 2020, updated 2022 (dated): https://phiresky.github.io/blog/2020/sqlite-performance-tuning/
- Michael Buckbee (Wafris), "Rearchitecting: Redis to SQLite", 2024-09-23: https://wafris.org/blog/rearchitecting-for-sqlite
- Ben Johnson, "I'm All-In on Server-Side SQLite" (Fly.io): https://fly.io/blog/all-in-on-sqlite-litestream/
- Fly.io, "How SQLite Scales Read Concurrency": https://fly.io/blog/sqlite-internals-wal/
- Ben Johnson, wtf: https://github.com/benbjohnson/wtf
- Litestream tips: https://litestream.io/tips/
- David Crawshaw, "One process programming notes", 2018 (dated): https://crawshaw.io/blog/one-process-programming-notes
- Turso COMPAT.md: https://github.com/tursodatabase/turso/blob/main/COMPAT.md
- libSQL extensions: https://github.com/tursodatabase/libsql/blob/main/libsql-sqlite3/doc/libsql_extensions.md

Go and framework sources:
- mattn/go-sqlite3: https://github.com/mattn/go-sqlite3
- modernc.org/sqlite: https://pkg.go.dev/modernc.org/sqlite
- zombiezen sqlitex: https://pkg.go.dev/zombiezen.com/go/sqlite/sqlitex
- sqlc: https://github.com/sqlc-dev/sqlc
- goose annotations: https://pressly.github.io/goose/documentation/annotations/
- golang-migrate sqlite3: https://github.com/golang-migrate/migrate/tree/master/database/sqlite3
- Rails SQLite3 adapter: https://github.com/rails/rails/blob/main/activerecord/lib/active_record/connection_adapters/sqlite3_adapter.rb
- Django SQLite backend: https://github.com/django/django/tree/main/django/db/backends/sqlite3
- PocketBase: https://github.com/pocketbase/pocketbase
- maragudk/goqite: https://github.com/maragudk/goqite
- maragudk/glue: https://github.com/maragudk/glue
