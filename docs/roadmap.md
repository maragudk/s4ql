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
- Single quotes mean strings only. A single-quoted string where a name is expected is rejected.
- No `ANY` column type.
- A name is never empty. `""` as the name of a table or a column is rejected, and SQLite accepts it.
- No settings that change behaviour, for now.
- A table whose name starts with `s4ql_` or `sqlite_`, in any case of the letters, is rejected. SQLite reserves `sqlite_` itself, and `s4ql_` is for the tables that s4ql keeps in its cache.
- A table with the name of one of SQLite's built-in virtual tables, such as `json_each` or `pragma_table_info`, is rejected. SQLite reads those tables without a `create table`.
- UTF-8 only. SQL text, identifiers, and text values are always valid UTF-8. A statement whose text is not valid UTF-8 is rejected before it is parsed, and so is a parameter or an expression that would produce text that is not valid UTF-8. SQLite accepts such text.
- No NUL byte in SQL text. A statement whose text has a NUL byte is rejected before it is parsed. SQLite reads such text up to the NUL byte and ignores the rest.

Rejected for now, to be decided later: upserts, and join variants such as comma joins and `NATURAL JOIN`.

## Slices

### Slice 1: one integer column

Status: agreed, not built.

```sql
create table t1 (v1 int primary key) strict, without rowid;

select * from t1;

insert into t1 (v1) values (1);

select * from t1;

drop table t1;
```

Included, without adding syntax:

- Any table name, column name, and integer value, in exactly these statement shapes: one `int primary key` column, `select *` from one table, a single-row `insert` that names the column, and `drop table` with one table name. An integer value is a decimal integer literal, with at most one `-` in front, that fits a signed 64-bit integer. A `+` sign, a hex literal, a digit separator, and a number outside 64 bits are not supported yet.
- Several rows and several tables. `select *` returns rows in primary-key order, as SQLite does for these tables.
- The errors these statements can produce, matching SQLite's: inserting a key twice, creating a table that exists, reading, writing, or dropping a table that does not exist, and inserting into a column that does not exist.
- Each statement is its own transaction.
- Surviving a restart: with an empty cache, replaying the log from S3 gives the same answers.

Everything else is rejected, including a second column, `integer` spelled out, `where`, `begin`, and `drop table if exists`.
