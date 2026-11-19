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

Status: built. The engine is checked against SQLite, and the `database/sql` driver runs these statements, without parameters, which slice 2 added, and without `db.BeginTx`, which slice 3 added.

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

### Slice 2: parameters

Status: built. The statement pass and the engine are checked against SQLite, and the `database/sql` driver runs an insert with an argument, directly and through a prepared statement. The generated tests make arguments: one insert in three has a `?`, and some calls have none, one too many, one of a type that is not supported, or one with a name. The engine's differential test compares every other call with SQLite and checks those 717 of its 12,000 statements against s4ql's rule; the driver's compares 8,000 calls with mattn/go-sqlite3, also prepared statements run several times, and checks 515 against the rule. The simulation checks that each commit holds the arguments that its caller gave, and finds a planted bug that annotates a wrong one. Mutation testing of the new code left no mutant unexplained.

```go
db.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
```

Included, without adding syntax:

- A plain `?` in the place where slice 1 has the integer literal of an `insert`. Nowhere else: the other statements of slice 1 have no value.
- An argument that is a Go integer and fits a signed 64-bit integer: what `database/sql` turns into an `int64`.
- Both directly, with `db.ExecContext` and its arguments, and through a prepared statement, with `db.PrepareContext` and then `stmt.ExecContext`, any number of times with different arguments.
- The number of arguments must equal the number of `?` in the text. Any other number is an error, the same for a direct call and a prepared one, and the statement does not run. This also holds for a text with no `?` that is given an argument. SQLite would bind NULL for a missing one; s4ql does not. The error is an ordinary error of s4ql's, whose message starts with `s4ql: ` and says both numbers, and not "not supported".
- The commit object keeps the arguments in the annotation of the statement, by position, next to its text.
- The errors of slice 1, matching SQLite's: a key inserted twice through a parameter gives the same error as with a literal.
- Surviving a restart: with an empty cache, replaying the log gives the same rows for rows inserted through parameters.

Rejected as not supported, where SQLite would accept it:

- The other forms of a parameter: `?1`, `:name`, `@name`, `$name`, and an argument made with `sql.Named`.
- An argument of any other type: nil, a string, a float, a bool, a `[]byte`, a `time.Time`. The message says which type.
- A parameter inside an expression, such as `values (-?)`, or in any other place.

The text is judged first, then the arguments, then the instance: a syntax error is SQLite's whatever the arguments are, and an error of the arguments does not depend on whether the instance serves.

### Slice 3: transactions

Status: built. The engine is checked against SQLite: pinned and generated sequences of statements run in transactions on both sides, SQLite's between `begin immediate` and `commit` or `rollback`, or under `pragma query_only`. The `database/sql` driver runs transactions, tested through `database/sql`. The generated comparisons through `database/sql` do not make transactions yet: the differential test of the driver, `internal/sqlite/crosscheck/drivercheck`, only begins a transaction and rolls it back, on both sides, and the generator `internal/sql/stmt/stmttest` makes none. No workload of the simulation begins one.

```go
tx, err := db.BeginTx(ctx, nil)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)
tx.ExecContext(ctx, "insert into t1 (v1) values (?)", 2)
err = tx.Commit()
```

Included, without adding syntax:

- `db.BeginTx(ctx, nil)` and `db.Begin()` begin a write transaction. It takes the place of the single writer when it begins, as SQLite's `begin immediate` does, and holds it until its commit or its roll back. `BeginTx` waits for that place as long as its context allows.
- `db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})` begins a read-only transaction. It reads the state of its beginning, whatever commits meanwhile, and holds up no one.
- The isolation levels `sql.LevelDefault` and `sql.LevelSerializable`, for both kinds of transaction. They are what the transactions give: writers run one after another, and a read-only transaction reads one committed state.
- Everything of slices 1 and 2 inside a transaction: `tx.ExecContext`, `tx.QueryContext`, `tx.QueryRowContext`, `tx.PrepareContext`, and `tx.StmtContext` with a statement prepared on the `db`, with the arguments of slice 2. `tx.Commit` and `tx.Rollback` end the transaction. `conn.BeginTx` on a `sql.Conn` works the same way.
- A write transaction reads its own changes, and nothing outside it sees them before its commit.
- A statement that fails leaves the transaction usable, with the effects of the statements before it, as in SQLite.
- `create table` and `drop table` are transactional: the later statements of the transaction see them, and a roll back undoes them.
- One commit object for each transaction. It holds the statements of the transaction that write and that succeeded, in order, each with its arguments. A transaction that changes nothing writes none.
- A write in a read-only transaction gets SQLite's error `attempt to write a readonly database`, with the code 8 and the extended code 8.
- A commit object that is too large for the store fails the commit with an error that matches the new `s4ql.ErrTooLarge`, and nothing is committed.
- Surviving a restart: with an empty cache, replaying the log gives the same rows and tables for what transactions committed.

Rejected as not supported, where SQLite would accept it:

- The SQL texts `begin`, `commit`, `end`, `rollback`, `savepoint`, and `release`, also inside a transaction, which stays open.
- Every other isolation level, such as `sql.LevelReadCommitted`. Nothing is begun.
- A statement in a transaction while the rows of an earlier query of the transaction are open. For now, this rules out a write inside a loop over the rows of a query: close the rows first.
