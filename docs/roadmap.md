# SQL roadmap

A working document on which SQL the engine supports and what comes next. It changes as slices are agreed and built. The rules that do not change are in [spec.md](spec.md).

## Principles

- **Everything is opt-in.** Nothing is supported until it is listed here, all the way down to `select 1`. Anything else is rejected as not supported.
- **Reject, never reinterpret.** Whatever the engine accepts behaves exactly as in SQLite. Legacy behaviour is left behind by refusing it, not by changing what it means.
- **Small slices.** Support grows one small slice at a time. Each slice goes through every layer and is checked against the oracle before the next one starts.

## Restrictions

These hold for every slice unless this document says otherwise.

- All tables are `STRICT`.
- All tables are `WITHOUT ROWID`. Every table therefore has a primary key, and no integer keys are generated.
- Foreign keys are always enforced.
- Double quotes mean identifiers only. A double-quoted string is rejected, and so are `[brackets]` and backticks around names.
- No `ANY` column type.
- No settings that change behaviour, for now.
- UTF-8 only.

Rejected for now, to be decided later: upserts, and join variants such as comma joins and `NATURAL JOIN`.

## Slices

### Slice 1: one integer column

Status: agreed, not built.

```sql
create table t1 (v1 int primary key) strict, without rowid;

select * from t1;

insert into t1 (v1) values (1);

select * from t1;
```

Included, without adding syntax:

- Any table name, column name, and integer value, in exactly these statement shapes: one `int primary key` column, `select *` from one table, and a single-row `insert` that names the column.
- Several rows and several tables. `select *` returns rows in primary-key order, as SQLite does for these tables.
- The errors these statements can produce, matching SQLite's: inserting a key twice, creating a table that exists, and reading or writing a table that does not exist.
- Each statement is its own transaction.
- Surviving a restart: with an empty cache, replaying the log from S3 gives the same answers.

Everything else is rejected, including a second column, `integer` spelled out, `where`, and `begin`.
