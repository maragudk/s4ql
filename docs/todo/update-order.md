# The order in which an update changes its rows

The piece of work that makes an update change its rows in SQLite's order, so that an update of which two rows fail with different errors gives SQLite's error. The adversarial review of 2026-11-26 found the fault (`docs/research/adversarial-codebase-2026-11-26.md` on the branch `adversarial-codebase`, agy 2.2): with an index on `a`, `update t set x = nx, y = ny where a > 0` gave `NOT NULL constraint failed: t.x` in s4ql and `t.y` in SQLite, and with a row that is too long in place of the second NULL, `19/1299` in s4ql and `18: string or blob too big` in SQLite.

## Step 1: what SQLite does, measured

Measured on 2026-11-26 with the shell of SQLite 3.54.1, on `t (k int primary key, a int, b int, x int not null, y int not null, z int not null, nx int, ny int, nz int) strict, without rowid` with the indexes `ia (a)` and `iab (b, a)`, or `iad (a desc)`, and three rows of which each gives NULL to a column of its own that does not take it, so that the error names the row that SQLite changed first. `explain query plan` gave the access. The source of SQLite says the same (`sqlite3Update` in `update.c`, `sqlite3DeleteFrom` in `delete.c`, and the one-pass part of `sqlite3WhereBegin`).

- **An update that SQLite can run in one pass changes each row when its access finds it,** so the rows change in the order of the access: of the primary key for a lookup or a range of the key and for a scan of the table, and of the key of the index for an equality or a range of an index, as the index holds it, so backward for a column of the index that is `desc`. Within the rows that an equality pins, the order is that of the rest of the key of the index, and then of the primary key. SQLite reads an update without ORDER BY forward, always.
- **SQLite runs an update in one pass unless one of these holds,** and then it collects the keys of the rows that it finds first, and changes the rows in the order of the primary key, whatever the access:
  - the update sets a column of the primary key, also to itself, as in `set k = k`;
  - the update sets a column of the index that its access reads by, also to itself (a column of another index does not count);
  - the WHERE has a subquery (a subquery in SET does not count);
  - the statement has RETURNING, which SQLite runs as a trigger;
  - (outside what s4ql supports: a trigger, a foreign key, `or replace`, a nested statement.)
- **A row in one pass is found, checked, and written before the next row is read,** so an error of the WHERE of a later row (SQLite's `string or blob too big` of an expression with a column) comes after the error of an earlier row's SET. With two passes, every WHERE is evaluated before any row changes.
- **The rows of RETURNING come in the order of the primary key,** for an update and a delete, by every access measured: the primary key, its range, an index with an equality, an index with a range, a scan, and a scan through an index. That follows from the rule above: RETURNING is never one pass.
- **A delete can fail on no row in what s4ql supports:** it has no trigger and no foreign key, and RETURNING gives columns alone, so the only error of a row is one of its WHERE, whose message is the same for every row. Its order decides no error. SQLite runs it in one pass under the same rules, without the one on the columns that the statement sets.
- **`changes()` after a statement that fails partway** is the count before the statement for a constraint error and a record that is too long, whatever row failed (the pair checks this already), so the order does not change it.

## Where SQLite's planner decides, and the engine cannot know it

The order of a one-pass update is that of the access that SQLite's planner chooses. The engine's rules (`chooseAccess`) agree with it where one candidate is better by the rules, but SQLite chooses by its estimate of costs, and differs from the rules here, all measured:

- **A scan of every row.** For an update or a delete of a table without a rowid, SQLite reads every row through an index, in the order of the index, where the index holds every column that the WHERE names (none for an update without WHERE) and is narrower than the table by its estimate of the widths of the columns: `update t set x = nx, y = ny` reads by `ia`. The engine reads every row by the primary key. With two such indexes SQLite takes the narrower, and of two that are as wide the one created last.
- **A tie between two indexes.** With `ia (a)` and `ib (b)`, `where a = 1 and b = 1` and `where a > 0 and b > 0` read by the index created last in SQLite; the rules take the one created first.
- **Two bounds against one.** `where k > 0 and a > 0 and a < 9` reads by `ia` in SQLite, which bounds it on both sides; the rules take the primary key.
- **`is null` against `=`.** `where a is null and b = 1` reads by `ib` in SQLite; the rules take `ia`, which they count as pinned by `is null`.
- **`is not null`.** `where a is not null` is a range of `ia` in SQLite; the rules bound nothing by it.
