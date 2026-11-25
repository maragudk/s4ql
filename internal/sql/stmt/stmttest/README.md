# The generator of statements

`internal/sql/stmt/stmttest` writes random statements for tests. It is code for tests only. It has two parts:

- **`Generator` writes one statement.** It gives the text and the typed form of [`stmt`](../README.md) that the text holds. The tests of the statement pass use it directly. They turn each feature on with an option, such as `Generator.Exists` or `Generator.OrderBy`. It draws from the one `Rand` that the test gives it.
- **`Sequence` writes a series of calls that meet at a few tables.** `Sequence.Next` gives a `Call`: a text, its arguments, and whether it runs as a query. A sequence drives a `Generator` through its hooks. The differential tests of the engine, the simulation of the engine, and the driver check run sequences.

The package also holds the rules that tests check answers against (`rule.go`, and `arithmetic.go` for arithmetic), and the check of what a generated test reaches (`reached.go`).

This document is about `Sequence`: its profiles, its random streams, the numbers that pin it, and how to add to it. The reasons for the design are in [the diary of the workload profiles](../../../../docs/diary/2026-11-21-workload-profiles.md).

## Profiles

A sequence is a function of its seed alone. `NewSequence(seed)` takes nothing else.

The seed names the profile of the sequence. A `Profile` gives a weight to each kind of statement, and odds to each area of the generator. An area whose odds are 0 is off.

- **`Profiles` is the list of the profiles.** The place of a profile in the list is its slot.
- **`ProfileOf(seed)` gives the profile at the slot `seed % Slots`.** `Slots` is 32. A slot that no profile has gives `Mixed`.
- **`Profile.Seed(k)` gives seed number `k` of a profile, from 0.** It is `k*Slots` plus the slot of the profile.

So one number replays a run. The flags `-seed` and `-replay` of the engine's tests need no second flag for the profile.

The one thing that a test sets on a sequence beside its seed is `Sequence.Decide`. The driver check calls it before its first call, because through `database/sql` it cannot see which of several first rows of a subquery s4ql took. From then on, every subquery that the sequence writes has an ORDER BY that ends with the primary key of its table, or that is the key alone, so its first row is decided. The draws that `Decide` adds are from the stream of the area `subquery`, so no draw of another area moves.

### The fourteen profiles

`mixed` has every area on. Each other profile weighs one area, over the statements that the area needs to do its work.

Five areas are on in every profile: `names`, `plan`, `tables`, `inserts`, and `text`. The table gives the other areas that each profile has on. `Profile.on` decides it from the fields of the profile.

| Slot | Name | What it weighs | Other areas that are on |
| --- | --- | --- | --- |
| 0 | `mixed` | Every area, with the odds at which a statement of several features meets each. Bugs also live where areas meet. | `selects`, `changes`, `indexes`, `exists`, `defaults`, `valuefirst`, `arguments`, `order`, `returning`, `subquery`, `arithmetic`, `others` |
| 1 | `tables` | The tables themselves: their columns, types, and keys, and the columns that take NULL. Also the lists and the faults of an insert, whole reals for a column of integers, and a select of all rows. | `selects`, `arguments` |
| 2 | `where` | The select with WHERE, on keys and on other columns. | `selects`, `arguments` |
| 3 | `changes` | The update and the delete. | `selects`, `changes`, `arguments` |
| 4 | `indexes` | The create index, the drop index, and the WHERE that an index serves. | `selects`, `changes`, `indexes`, `arguments` |
| 5 | `exists` | IF NOT EXISTS and IF EXISTS, on tables and indexes that are there and that are not. | `selects`, `changes`, `indexes`, `exists`, `arguments` |
| 6 | `defaults` | The DEFAULT, and the inserts that leave a column out. | `selects`, `changes`, `defaults`, `arguments` |
| 7 | `valuefirst` | The value on the left of a comparison, in the WHERE of a select, an update, and a delete, on keys and on indexes. | `selects`, `changes`, `indexes`, `valuefirst`, `arguments` |
| 8 | `transactions` | The statements that write, and those that read what was written. Its test begins transactions more often. | `selects`, `changes`, `indexes`, `arguments` |
| 9 | `arguments` | The parameters, the arguments around those that s4ql supports, and the texts of `Others`: the errors and the refusals. | `selects`, `changes`, `arguments`, `others` |
| 10 | `order` | The ORDER BY and the LIMIT of a select: on columns of every type, ascending and descending, with NULL and with rows that tie, with WHERE and without. Also the ORDER BY that an index delivers, beside an index that only narrows a range, around a pinned column of its key, with mixed directions, and with a gap in the key, and the LIMIT as a literal and as a `?`, with the arguments that s4ql refuses. | `selects`, `changes`, `indexes`, `arguments`, `order` |
| 11 | `returning` | The RETURNING of an insert, an update, and a delete: `*` and named columns, a column twice, and a column that the table does not have, of every type, with NULL and with defaults. Its statements change no row, one row, and several rows, change the key and the columns of an index, move a row to a key that it did not have, change every row, and fail on a constraint. Its tables have a column with a default before one without, where `*` shows the order of the columns. Also goqite's send and its receive by id. Its test begins transactions more often. | `selects`, `changes`, `indexes`, `defaults`, `arguments`, `returning` |
| 12 | `subquery` | The scalar subquery in the WHERE of a select, an update, and a delete: of no row, one row, and several rows, with an ORDER BY that decides the first row or leaves rows that tie, or with none, and with NULL first. Its subqueries read the table of the statement and another, by an index that serves them and by none, on either side of a column with each operator, with RETURNING, with parameters around them and in them, and two in one WHERE. Some statements compute none. Also the shapes that s4ql refuses: a correlated name, a column of another type, `*` of several columns, and an argument of LIMIT that is no integer from 0. Also goqite's receive: an update of the row whose key a subquery of its own table gives, by goqite's index and with RETURNING. Its selects with subqueries have a LIMIT of their own and subqueries that give rows, and its receives meet queues of messages that tie. Its texts run again as a prepared statement runs them, and its test begins transactions more often. | `selects`, `changes`, `indexes`, `arguments`, `order`, `returning`, `subquery` |
| 13 | `arithmetic` | The arithmetic of the piece of work arithmetic: `+`, `-`, `*`, `/`, and the negation of integers and reals, nested and in parentheses, with literals, parameters, and NULL. It is in the SET of an update, with a column and without one, also of the column that it sets, as `n = n + 1`, and of the key. It is on either side of a comparison of the WHERE of a select, an update, and a delete: a value without a column that serves the key or an index as a `?` does, an expression of a column that serves nothing, a column compared with a column, and two expressions of columns. Its values reach each evaluation error, an integer overflow, a division by zero, and a real result that is not finite, for every row, for some rows, and for none, with a LIMIT whose direction decides which. Its faults are the refusals of the types: an expression of another type than its column, a TEXT or BLOB column in arithmetic, and an argument that is text or a blob. Also the texts of `ArithmeticRefused`, which the statement pass refuses. Its test begins transactions more often, where a statement that fails leaves the transaction usable. | `selects`, `changes`, `indexes`, `arguments`, `order`, `returning`, `arithmetic` |

### The fields of a profile

| Field | What it does |
| --- | --- |
| `Weights` | The weight of each kind of statement. A statement is of a kind as often as the weight of the kind is of the sum. |
| `Aim` | The number of statements in four that `Sequence.aim` aims at the tables as the sequence left them. A create table then names a table that is not there, and every other statement names one that is. With 0, the sequence names its tables at random, and many statements meet no table. |
| `Faults` | The areas whose statements have their faults at the full odds. In every other area, `Sequence.fault` gives each fault a quarter as often. |
| `Columns` | The least number of columns of a table. With 2, each table has a column beside its key, for an index that does not hold the key first. |
| `Where` | The number of selects in four that have a WHERE. |
| `Indexes` | With 0, the sequence has no indexes. With 1, it has one or two names of indexes. One WHERE in two of a table with an index is then drawn for the index. With 2, it has two or three names, and three such WHERE in four. More of its indexes hold every column but the key. More of its WHERE pin every column of an index, and the key after them. |
| `Exists` | One create in `Exists` says IF NOT EXISTS, and one drop in `Exists` says IF EXISTS. |
| `Defaults` | With 0, no table has a DEFAULT. With 1, a third of the tables have none, and a third have one on every column. The last third have one on each column one time in two. With 2, every table has one of the last two forms. |
| `DefaultFirst` | With `Defaults`, one table in `DefaultFirst` of two columns or more has a default on its first column and none on its last, and one on each other column one time in two. So a column with a default comes before one without, where `*` of a RETURNING shows the order of the columns of the table. The other tables draw their defaults as `Defaults` says. |
| `ValueFirst` | One comparison in `ValueFirst` has its value on the left. |
| `Parameters` | One value in `Parameters` is a plain `?`. |
| `Around` | One call in `Around[0]` of a statement with a parameter has arguments around those that s4ql supports. So has one call in `Around[1]` of a statement without a parameter. |
| `Others` | One text in `Others` is one of the texts of the list `Others`, in place of a statement. |
| `Mistyped` | One insert in `Mistyped` gives an integer of a column of INT or INTEGER as the real of the same value. |
| `Transactions` | This field is for the test that runs the sequence. One turn in `Transactions` outside a transaction begins one. |
| `Order` | The number of selects in four that have an ORDER BY, as `Sequence.orderColumns` draws it. |
| `Limit` | One select in `Limit` has a LIMIT. A select whose ORDER BY is aimed at an index beside one of a range always has one. The area `order` is off if `Order` and `Limit` are both 0. |
| `Keys` | One insert in `Keys` gives the key a value of its own statement (`keyOfStep`), which no other such insert gives. The insert then seldom meets a key that exists, so the tables hold more rows. With 1, every insert does. |
| `Shared` | One create index in `Shared`, of a table of which the sequence keeps an index, holds the first column of that index and then another column. So two indexes of a table begin alike, and one can deliver an order where the other narrows a range. |
| `Aimed` | One select in `Aimed`, of a table of which the sequence keeps an index, has a WHERE and an ORDER BY drawn together for the key of one of its indexes (`Sequence.aimedWhere`): goqite's receive, where two indexes begin alike, or a pin of a column at the start or in the middle of a key and an ORDER BY of the rest of it, one time in two with a range of the next column after a pin at the start. |
| `Ties` | One value in `Ties` of an insert, for a column that is not the key and gets no NULL, is one of two values of its type (`tieValue`). So rows tie on a column of an index and differ in the next, where the order of a key and that of an ORDER BY can part. |
| `Returning` | One insert, update, or delete in `Returning` ends with a RETURNING, as `Sequence.returning` draws it: `*` one time in three, and otherwise one to four columns of the shape of its table. One column in four after the first is an earlier one, which the clause then names twice. One in twelve is any name of a column of the sequence, which the table may not have, a fault of the area. The area `returning` is off with 0. |
| `ByKey` | One update in `ByKey` changes the row of a key, as goqite's receive by id does: its WHERE is the key alone, equal to a value that an insert gave it, and its SET has columns outside the key only. The table must have such a column, and the sequence a value of the key. |
| `KeyLeftOut` | One insert in `KeyLeftOut` with a list leaves out a key that has a default, so that the row gets the default as its key, as goqite's send does. Every other such insert leaves the key out one time in four, as in a profile with 0. |
| `NewKey` | One update in `NewKey` of those that `ByKey` leaves moves the row of a key to a new key: its WHERE is the key alone, equal to a value that an insert gave it, and its SET has the key first, with a value that no insert gives (`keyOfStep` of the statement's own number), and then as many columns as another update has. The sequence must have a value of the key. |
| `EveryRow` | One update in `EveryRow` of those that `ByKey` and `NewKey` leave changes every row of its table: it has no WHERE, and its SET has columns outside the key only, so that an update of a table of several rows gives them all. The table must have a column outside the key. |
| `Subquery` | One select, update, or delete in `Subquery` has scalar subqueries in its WHERE, as `Sequence.subqueries` draws them: one time in six two, and otherwise one. Each is the value of a comparison, and its select is the one that `Sequence.innerSelect` draws. The area `subquery` is off with 0. |
| `Receive` | With `Subquery`, one update in `Receive` of those with subqueries is goqite's receive, as `Sequence.receive` draws it: the key equal to a subquery of its own table, with an ORDER BY and LIMIT 1, and three times in four a pin of another column before it, as goqite's `queue = ?`. It always has a RETURNING: `Sequence.receive` sets `received`, which `Sequence.returning` reads. With `Ties`, three receives in four are of a ready queue: each pin is one of the two values of `Ties`, each bound is closed and holds both, and the ORDER BY is of two columns outside the key, the pinned one first if the update has a pin, so that messages tie where they share the second. The second is one time in two the next column of the aimed ORDER BY, which an index delivers, and otherwise another column, which the engine may sort and then draws among the rows that tie. |
| `SubqueryLimit` | With `Subquery`, one select in `SubqueryLimit` with subqueries and without a LIMIT gets one, as `Sequence.order` draws it: 1 one time in two, and otherwise from 2 to 4. So a select whose LIMIT lets rows through computes its subqueries. One select with subqueries in two also gets a first subquery that gives rows (`Sequence.giving`): the comparison is by =, the subquery gives the column that it is compared with, and its LIMIT 0 becomes 1. The subquery must be of the table of the statement and of a named column, and the table must have the column of the comparison. |
| `Again` | With `Subquery`, one call in `Again` is the text of the last select, update, or delete with a subquery again, as a prepared statement runs it. Each argument is the same one time in two, and otherwise another value of its Go type, as `Sequence.Like` gives it. An argument of a LIMIT, of the select or of its subqueries, is otherwise an integer from 0 to 4, which s4ql takes (`Sequence.limits`). |
| `TableNames` | The least number of names of tables of the sequence, or 0 for one or two. With 2, a subquery reads another table than its statement as often as its own. |
| `Arithmetic` | One comparison in `Arithmetic` of the WHERE of a select, an update, a delete, and a subquery, and one assignment in `Arithmetic` of the SET of an update, has arithmetic, as `Sequence.arithmeticWhere` and `Sequence.arithmeticSet` draw it. One select in `2*Arithmetic` has in place of its WHERE one comparison that fails for some rows (`Sequence.failingWhere`), and one in `3*Arithmetic` of the others the key equal to a value without a column (`Sequence.keyWhere`). One call in `8*Arithmetic` is a text of `ArithmeticRefused`, which the statement pass refuses. The area `arithmetic` is off with 0. |

`Aim` and `Faults` are what make a focused profile work. Without them, many statements of a sequence meet a table that is not there, or fail for a fault. Focus alone then does not make a rare kind of outcome more frequent. To aim, the sequence keeps a model of its own statements: the tables that are live (`live`), the indexes that it keeps (`indexed`), and the values that it gave each column (`stored`). The model follows the statements as the sequence wrote them, whether SQLite takes them or not.

### Each profile runs on seeds of its own

A test runs each profile on the seeds `p.Seed(0)`, `p.Seed(1)`, and so on, and keeps a tally for each profile. So a new profile runs on seeds that no other profile has. The other profiles write the same sequences as before, and their tallies do not move.

A profile is only added at the end of `Profiles`. No profile is removed or moved. The slot of a profile is its place in the list, and the seeds of a profile are those of its slot. If a profile moves, it gets other seeds, and every number that is pinned for it moves. `Slots` is more than the number of profiles, so a new profile takes a slot that is free.

A seed whose slot is free writes a sequence of `mixed`. When a profile takes the slot, the seed writes a sequence of that profile. The tests that pin numbers take their seeds from `Profile.Seed`, so they run no such seed.

## Streams

Each area draws its random numbers from a stream of its own. The areas are constants in `profile.go`:

| Area | What it draws |
| --- | --- |
| `names` | The names of the tables and the columns of the sequence, and their spellings. |
| `plan` | Which statement comes next, and the name of its table or its index. |
| `tables` | The shape of a table, and the columns of a create table that differ from the shape. |
| `inserts` | The list and the values of an insert, and its faults. |
| `selects` | The select list and the WHERE of a select. |
| `changes` | The assignments of an update, and the WHERE of an update and a delete. |
| `indexes` | The names of the indexes, the columns of a create index, and the WHERE that an index serves. |
| `exists` | Whether a create or a drop says IF NOT EXISTS or IF EXISTS. |
| `defaults` | The defaults of a shape, those of a create table that differ, and whether an insert leaves out a column with a default. |
| `valuefirst` | Whether a comparison has its value on the left. |
| `arguments` | Whether a value is a parameter, and the arguments around those of a call. |
| `order` | The ORDER BY and the LIMIT of a select, and an argument of the LIMIT that s4ql refuses. |
| `returning` | Whether an insert, an update, or a delete has a RETURNING, and its columns. |
| `subquery` | Whether a select, an update, or a delete has a scalar subquery, and its select. Also goqite's receive, a text that runs again, and an argument of the LIMIT of a subquery that s4ql refuses. |
| `arithmetic` | Whether a comparison of a WHERE or an assignment of SET has arithmetic, and its expressions. Also the texts of `ArithmeticRefused`. |
| `others` | The texts of `Others`. |
| `text` | How a statement is written: its gaps, the case of its keywords, the spelling of its literals and operators, and its semicolons. |

A stream is seeded in one of two ways:

- **By statement** (`Sequence.stream`, a `stepStream`). The stream is seeded from the seed, the name of the area, and the number of the statement in the sequence. It starts anew at each statement. Most draws come from such a stream.
- **By thing** (`Sequence.entity`). The stream is seeded from the seed, the name of the area, and a name, such as the name of a table. The names of the sequence, the names of its indexes, and the shape and the defaults of each table come from such a stream.

The `Generator` of a sequence has the stream of `text` as its `Rand`. It has four more streams, which are not exported: `plan`, `clause` for the area `exists`, `params` for the area `arguments`, and `turned` for the area `valuefirst`. In a generator without a sequence, the four are its one `Rand`.

Four things follow:

- **A draw more or less in one statement moves no number of another statement.**
- **A draw more or less in one area moves no number of another area,** also in the same statement.
- **The shape of a table does not depend on which statement names the table first.**
- **What a test draws for itself comes from `Sequence.Stream(name)`.** Examples are the transactions of `TestConn_differential`, the restarts of `TestDifferential`, and the calls of the driver check (`Sequence.Stream("calls")`). The stream gives the same numbers for the same seed and name, whatever the sequence draws. So the test moves no statement.

### The limit of the streams

The stream of a statement gives the same numbers, whatever the earlier statements drew. The text of the statement can still depend on them. A statement reads what earlier statements left in the model of the sequence:

- the values in `stored`, from which the conditions of a WHERE draw
- the tables in `live`, at which `Sequence.aim` aims
- the indexes in `indexed`, which decide whether a WHERE is drawn for an index
- the last call with a subquery, in `again`, which `Profile.Again` runs again

So a change to the inserts can change the text of a later select, although the select gets the same numbers. A text of `Others` in place of a statement changes later statements too, because the statement whose place it takes never stores its values.

`TestNewSequence` shows the stronger claim for two areas that store nothing, `exists` and `valuefirst`. Other numbers for such an area change no statement outside the area.

### An area that is off

The stream of an area that a profile has off panics at a draw (`offStream`). `TestNewSequence` runs 300 calls of 20 seeds of each profile, so a draw from an area that is off fails the test. That is what makes "off" mean "draws nothing".

## The proof and the pins

Two sets of numbers pin the sequences: a digest for each profile, and the counts of what each run reaches.

### The digests

`TestNewSequence` in `profile_internal_test.go` pins a digest for each profile. The digest is a hash of the first 200 calls of the first three seeds of the profile: their texts, their arguments, and how they run.

A change to the generator of an area can change the digest of a profile only if the profile has the area on. So the digests that change tell which profiles a change reaches. Those are the profiles whose counts you must recount.

If the digest of a profile changes that has the area off, the change reaches more than its area. Find the cause before you put the new digest in the test.

### What a run reaches

Each generated test counts kinds of outcome for each run. A run is the sequences of one profile. In the simulation, a run is the seeds of one cell, named as `default/indexes`, or those of one environment, named as `default`, whose counts are the sums of those of its cells. `stmttest.Reached` checks the counts against two files in `testdata/reached/` of the package of the test, both named after the test:

- **`<Test>.kinds` holds the kinds that the test requires,** one on a line: the least number of the total of the kind, a tab, and the kind. A builder writes this file, and the recount never writes it. It holds one kind at least, no exact kind, and no kind twice, and each least number is 1 at least. Otherwise the check fails.
- **`<Test>.tsv` holds the counts.** The test writes this file. A row is a run, what the run reached when the file was written, the least number that the test requires, and the kind. Do not edit a number by hand.

`Reached.Check` has these rules:

- **A total must reach the number of its kind in `.kinds`.** The total of a kind is what the runs reached of it together. In the simulation, only the environments add to the totals, since their counts are the sums of those of their cells (`Reached.Totaled`). A total below its number fails the check and the recount. So a floor falls only with a change of the `.kinds` file, which a reviewer sees in the diff. If a change of the generator makes a kind rarer on purpose, lower its number by hand, and say why in a comment in the file. The numbers started from the least numbers that `main` had on 2026-11-21, and from a third of the total for a kind that `main` did not require.
- **The least number of a row is a third.** A run must reach each kind of its rows at least a third as often as when the file was written, rounded down.
- **No least number of a row is 0.** A kind that a run reaches fewer than three times has no row for that run. A row with a least number of 0 fails the check. So does a row whose least number is not exactly the third of its count, rounded down. So a least number that was edited by hand alone fails. A row whose count and least number were both lowered, or a row that was deleted, passes the rules of the rows: the totals in `.kinds` are what still hold the floor then. A recount after a kind became rarer writes such rows too.
- **A required kind needs a row in one run at least.** If every run reaches a required kind fewer than three times, the check fails. The recount fails too, and writes nothing. The message says how often each run reaches the kind.
- **A run needs a row.** A run without any row fails the check. Such a run is a profile, a cell, or an environment that was added without a recount.
- **An exact kind is pinned exactly.** `Reached.Exact` names the exact kinds. In the differential tests of the engine and in the driver check, they are the kinds that begin with `excused: `. These count the texts that the test does not compare with SQLite. In the simulation, they are the three kinds of integrity checks of the caches. A run must give an exact kind as often as its row says. A run without a row must not give it.
- **Only required kinds and exact kinds have rows.** A row of another kind fails the check, and so does a row of a run that the test does not have.

Remove a kind from a `.kinds` file only if it is pinned somewhere else, and say where in the commit.

The files of `TestDifferential`, `TestConn_differential`, and `TestSimulation` are in `internal/engine/testdata/reached/`. Those of the driver check are `TestGenerated.kinds` and `TestGenerated.tsv` in `internal/sqlite/crosscheck/drivercheck/testdata/reached/`.

The driver check, `TestGenerated`, runs as many sequences of each profile as `generatedOf` says. It has no slow run: it runs in full and checks its counts in every run, also under the race detector. Two kinds are not in its `.kinds` file, because no profile reaches them three times in its run:

- "reached: value on the left of != finds rows"
- "reached: WHERE with a column that the table does not have under IS NULL or IS NOT NULL, alone no such column"

`TestGenerated` requires each of the two once over all profiles. `TestDifferential` of the engine requires both by profile.

A third kind left the file with the generated checks of arithmetic of 2026-11-25: "reached: insert of NULL beside a value of another type NOT NULL constraint failed". `mixed` reached it three times before them and reaches it once since. `TestGenerated` no longer requires it, and `TestDifferential` does.

Only `mixed` has every area on, so only `mixed` reaches what lies where two areas meet that no focused profile has on together. Two kinds count such a meeting, so that fewer sequences of `mixed` show in the totals: "insert that leaves out a column with a default that an index holds ok", where the index must hold the default, and "value on the left on a table that a create table if not exists found". `TestDifferential`, `TestConn_differential`, and `TestGenerated` require both. In `TestDifferential` and `TestSimulation`, `mixed` runs two thirds of the sequences or seeds that the one sequence of all areas ran before the profiles: 200 of 300, and 100 of 150. In `TestConn_differential` it runs 40 of 60. In `TestGenerated` it runs 128 of 250, the most that the time of its package allows.

### The recount

After a change of the generator, write each `.tsv` file again from a run of its test:

```
S4QL_RECOUNT=1 go test -count=1 -run '^TestDifferential$' ./internal/engine/
S4QL_RECOUNT=1 go test -count=1 -run '^TestConn_differential$' ./internal/engine/
S4QL_RECOUNT=1 go test -count=1 -run '^TestSimulation$' ./internal/engine/
```

For the driver check, run this from `internal/sqlite/crosscheck`:

```
S4QL_RECOUNT=1 go test -count=1 -run '^TestGenerated$' ./drivercheck/
```

A recount needs a full run. Do not pass `-short`, `-race`, or `-asan`. Keep the flags of the test at their defaults, do not set `S4QL_ENGINE_SIM_SEEDS`, and do not filter the subtests of `TestSimulation` with `-run`. In every other run, the tests of the engine run fewer or other sequences and cannot count. `S4QL_RECOUNT=1` then fails the test (`noRecount`), and writes nothing.

Then read the diff of the `.tsv` file. Rows can change only for the runs that the change reaches. If an exact count changes, say why in the commit. The recount does not write the `.kinds` file: if a total falls below its number there, the recount fails and writes nothing, and the message says how often each run reaches the kind.

## Add a statement or an option to an area

1. Put the draw in the hook of the area in `sequence.go`, on the stream of the area. The table after these steps gives the stream of each hook.
2. If `Generator` itself must draw for the option, give it a stream for the area, as `clause` and `turned` are. Set the stream in `newSequence`.
3. Run `go test ./internal/sql/stmt/stmttest/`. The digests of the profiles that have the area on change.
4. Make sure that no other digest changes. Then put the new digests in `TestNewSequence`.
5. If the tests need a rule for the answer, or names for the cases, add a function to `rule.go`. Add a table test for it to `rule_test.go`. `WhereCases`, `ValueFirstCases`, and `IndexRule` are examples.
6. Count the new kinds in the tests. See "Where the kinds are counted" below.
7. Add each new kind to the `.kinds` file of each test that must reach it. Write the kind as the tally has it, after its least number and a tab: a third of its total over the runs of the test.
8. Recount each test, and read the diffs.

Never draw from the stream of another area. Such a draw moves the numbers of that area in the same statement. In a profile that has that area off, it panics.

| Hook or function | Stream |
| --- | --- |
| `create` | `rTables`, and `rDefaults` for the defaults |
| `insert` | `rInserts`, also for the key of `Profile.Keys`, the values of `Profile.Ties`, and the draw of `Profile.KeyLeftOut`, and `rDefaults` for the defaults |
| `selectList`, `where` | `rSelects`, `rOrder` for the draw of `Profile.Aimed`, and `rSubquery` for the draw of `giving` |
| `order` | `rOrder`, also for the LIMIT of `Profile.SubqueryLimit`, and for the LIMIT of 1 or 2 that a select gets three times in four where `failingRow` is set |
| `orderColumns`, `aimedWhere` | The stream that the caller gives: `rOrder`, and `rSubquery` for a subquery |
| `returning` | `rReturning` |
| `update`, `delete`, `changeConditions` | `rChanges`, also for the draws of `Profile.ByKey`, `Profile.NewKey`, and `Profile.EveryRow` in `update` |
| `subqueries`, `innerSelect`, `receive`, `giving` | `rSubquery`. `where`, `update`, and `delete` call `subqueries`, and `where` calls `giving`. |
| `createIndex`, `indexWhere` | `rIndexes`, also for the columns of `Profile.Shared` |
| `indexConditions` | The stream that the caller gives: `rIndexes` for a select, an update, or a delete, and `rSubquery` for a subquery |
| `arithmeticHooks`, `arithmeticWhere`, `arithmeticCondition`, `arithmeticSet`, `arithmeticInner`, `failingWhere`, `keyWhere`, `constantOf`, `failing`, `otherColumn`, `nested` | `rArithmetic`. `arithmeticHooks` wraps the hooks `where`, `update`, and `delete`, and `newSequence` calls it if the profile has the area on. `failingWhere` and `arithmeticCondition` set `failingRow`, which `order` reads without a draw. |
| `shapeOf` | `entity(areaTables, name)`, and `entity(areaDefaults, name)` for the defaults, also for the draw of `Profile.DefaultFirst` |
| `conditions`, `conditionValue` | The stream that the caller gives |
| `Next`, `around`, `Like` | `rArguments` |
| `next` | `rPlan`, `rOthers` for a text of `Others`, `rOrder` for an argument of LIMIT that s4ql refuses, `rSubquery` for `Profile.Again` and for an argument of the LIMIT of a subquery that s4ql refuses, and `rArithmetic` for a text of `ArithmeticRefused` |

`conditions` and `conditionValue` serve the areas `selects`, `changes`, `indexes`, and `subquery`. A draw more in them reaches every profile but `tables`, whose statements have no WHERE.

`next` also draws, from `rPlan`, how a call runs. A select runs through Query seven times in eight, and through Exec the eighth time. So does an insert, an update, or a delete with RETURNING, by the same draw. Every other statement runs through Exec seven times in eight.

### Where the kinds are counted

- **The differential tests of the engine** count in the `reach*` functions of `internal/engine/differential_test.go`: `reach`, `reachInsert`, `reachWhere`, `reachOrder`, `reachValueFirst`, `reachChange`, `reachIndex`, `reachExists`, and `reachReturning`. `pair.reach` calls them for `TestDifferential` and for `TestConn_differential`. It writes each kind into the tally with `reached: ` before its name. The tally also counts each statement by its kind and its outcome, such as `insert with a parameter ok`. For a statement with subqueries, `pair.reach` calls them on the statement with the values that the instance computed in place of the subqueries, and calls `pair.reachSubquery` of `internal/engine/subquery_test.go` for the kinds of the subqueries. For a statement with arithmetic, it calls `pair.reachOfArithmetic` of `internal/engine/expression_test.go`, and `pair.checkAccess` calls `pair.reachServed` there.
- **The simulation of the engine** counts in `tallyOf`, with `subqueryKinds` for a statement with subqueries and `arithmeticKinds` for one with arithmetic, and in `run.event` of `internal/engine/simulation_test.go`.
- **The driver check** counts in its own test, the subqueries in its own `pair.reachSubquery`.

To see what a run reaches, run the test with `-v`. Each test logs the tally of each run.

## Add a feature area

The worked example is slice 13, `order by` and `limit`. It was built by these steps, in the generated checks of slice 13 of 2026-11-21: the area `order`, its stream `rOrder`, the hooks `Sequence.order` and `Sequence.orderColumns`, and the profile `order` at slot 10. Each step says what slice 13 did, and the next area does the same under its own names. Slice 14 followed the same steps on 2026-11-23, for RETURNING: the area `returning`, its stream `rReturning`, the hook `Sequence.returning`, and the profile `returning` at slot 11. Slice 15 followed them on 2026-11-24, for the scalar subquery in a WHERE: the area `subquery`, its stream `rSubquery`, the functions `Sequence.subqueries`, `Sequence.innerSelect`, and `Sequence.receive`, and the profile `subquery` at slot 12. Arithmetic followed them on 2026-11-25: the area `arithmetic`, its stream `rArithmetic`, the hooks that `Sequence.arithmeticHooks` wraps, and the profile `arithmetic` at slot 13.

A `Generator` writes the clause where `Generator.OrderBy` is set, or where the hook `Generator.Order` gives one. `Generator.OrderBy` is for the tests of the statement pass, and a `Sequence` leaves it at 0. A `Sequence` sets `Generator.Order`, to `Sequence.order`, if its profile has the area on. For RETURNING, the option is `Generator.Returning`, the hook is `Generator.Return`, and a `Sequence` sets it to `Sequence.returning`. For the scalar subquery, the option is `Generator.Subquery`, and the hook is `Generator.Inner`. A subquery is the value of a comparison, so the hooks of a WHERE, `Sequence.where`, `Sequence.update`, and `Sequence.delete`, give a condition whose value names a subquery by a number, and `Generator.Inner` gives the select of that number, which the sequence keeps in `Sequence.inner`. Arithmetic has no hook of its own. The hooks `Generator.Where`, `Generator.Update`, and `Generator.Delete` give conditions and assignments that hold a tree of arithmetic, in `Condition.Expression` or `Term.Expression`, and the generator writes the tree (`Generator.given`, `Generator.givenTerm`).

1. **Add the area and its profile in `profile.go`.**
   1. Add a constant for the area, as `areaOrder` is one, with a comment that says what the area draws.
   2. Add a field to `Profile` for the odds of the area. Slice 13 added two, `Order` and `Limit`.
   3. Add a case to `Profile.on`: the area is on if a field of it is not 0. Without a case, the area is on in every profile, and its stream never panics.
   4. If the statements of the area have faults, add the area to `allFaults`.
   5. Add the new profile at the end of `Profiles`. It takes the next free slot. `order` took slot 10, `returning` slot 11, `subquery` slot 12, and `arithmetic` slot 13, so the next is 14. No seed of another profile changes.
   6. Turn the area on in `Mixed`, and add the area to the list of `TestProfileOf`. The test requires that `Mixed` has every area of that list on.
2. **Add the stream and the hook in `sequence.go`.**
   1. Give `Sequence` a stream for the area, as `rOrder` is one. Set it in `newSequence`, as `q.stream(areaOrder)` sets that one.
   2. Write the hook. It draws from that stream only, and gives what the hook of the `Generator` asks for. `Sequence.order` gives `Generator.Order` the columns of the ORDER BY, and the LIMIT.
   3. If the hook needs what another hook drew, keep that in the `Sequence`, without a draw. `Sequence.where` keeps the conditions of a select in `Sequence.selected`, and `Sequence.orderColumns` reads them.
   4. Set the hook of the `Generator` only if the profile has the area on, as `newSequence` sets `q.g.Order`.
3. **Make sure that the focused profiles that exist do not change.** With the area off, nothing may draw for it. If `Order` is nil and `OrderBy` is 0, `Generator.order` returns before any draw. So every sequence of these profiles is the same, byte for byte. Their digests do not change, and no row of theirs in a `.tsv` file moves. For slice 13, the digests of the nine focused profiles stayed as they were, for slice 15 those of the eleven, and for arithmetic those of the twelve.
4. **Know what changes in `mixed`.**
   - A select that gets the clause changes its text, and can change its arguments.
   - Every other statement of `mixed` keeps its text. The clause is written within its own statement. The generator writes it with the stream of `text`, and draws from the stream of `arguments` whether the LIMIT is a `?`. Both streams start anew at the next statement. A select stores nothing that later statements read: no value, no live table, and no index.
   - A select for which the hook gives no clause keeps its text too. The generator draws nothing for it.
   - The counts of `mixed` change. A `LIMIT ?` adds an argument, which moves the exact counts of the excused calls.
5. **Add the rule.** Write the rule for a select with the clause, and the names of its cases, in `rule.go`. Add a table test to `rule_test.go`. Slice 13 widened `ChosenAccess` to the choice with an ORDER BY and a LIMIT, and `MissingColumn` to the columns of the ORDER BY. Slice 15 made the rules read a subquery: `SelectText` writes it in parentheses, `FoundQuery` takes the subqueries of the statement, `SelectOf` gives the select with them, `WhereCases` names a comparison with one, and `WhereRule` passes over it. `KeyAccess` and `ChosenAccess` decline a select with one, whose access depends on the value of the subquery. Arithmetic moved its rules from the engine's tests to `arithmetic.go`: `ArithmeticRefusals` gives every refusal that may come, since the engine gives them in an order of its own; `Justify` judges an evaluation error by SQLite's result of the operator that failed, with `ArithmeticSites`, `Operators`, `SiteGuard`, and `QuirkQuery`; `EvaluationOf` reads such an error from its message; and `CorrelatedRefusal` and `LimitRefusal` give the refusals of a correlated subquery and of an argument of LIMIT. `WhereRule`, `ConditionRule`, `UpdateRule`, and `DeleteRule` still panic for a statement that `HasExpression` reports, and point at `ArithmeticRefusals`.
6. **Count the kinds.** Count them in the `reach*` functions, and in `tallyOf` for the simulation. Add them to the `.kinds` files. Slice 13 added `reachOrder`. Slice 15 added `pair.reachSubquery`, in `subquery_test.go` of the engine. Arithmetic added `pair.reachOfArithmetic` and `pair.reachServed`, in `expression_test.go`, and `arithmeticKinds` for the simulation.
7. **Add the profile to the simulation.** Add a cell with the profile to an environment of `simEnvironments`. A cell that is added adds to what the environment reaches, and takes nothing from it. The default and the hammer environment each have a cell of 6 seeds of `order`. Of `returning`, the default environment has a cell of 6 seeds, and the hammer environment one of 16. Of `subquery`, they have one of 8 and one of 20, and of `arithmetic` one of 8 and one of 12.
8. **Add a planted bug that names the new profile.** `TestSimulation_plantedBugs` gives each bug the profile that weighs what the bug breaks. `TestDifferential_plantedBugs` does the same for a bug of the choice of an access, for a bug of an ordered select, for a bug of RETURNING, for a bug of a scalar subquery, and for a bug of arithmetic. The bugs themselves are in the engine, and `Options.Plant` in `internal/engine/export_test.go` plants one by its name. Slice 13 planted three with the profile `order`: `turnMixed`, `limitFirst`, and `ascendLast`.
9. **Recount each test.** Use the commands of [The recount](#the-recount).
10. **Read the diff of each `.tsv` file.** In the files of the differential tests, only the rows of `mixed` and of the new profile can change. In `TestSimulation.tsv`, a row is a cell or an environment, and an environment sums its cells. So only the rows of the cells of `mixed` and of the new profile can change, and those of the environments that hold them. If another row changes, find the cause before you commit.
11. **Update the digests.** Put the new digest of `mixed` and the digest of the new profile in `TestNewSequence`. The test requires a digest for each profile.

If the rare kinds of the new profile need more sequences, give it twice as many in `sequencesOf` of `TestDifferential`, as `order`, `returning`, `subquery`, and `arithmetic` have.

A profile can need more than its own area to reach its kinds. In the first sequences of `order`, most selects gave no row or one row, because the keys repeat, and two indexes of a table seldom began with the same column. `Profile.Keys` and `Profile.Shared` changed both. Each is a field of another area, the inserts and the indexes, and draws from the stream of that area. Each is 0 in every other profile, so no other profile changed. A review then found that a pin in the middle of a key, an ORDER BY with a gap in a key, and an ORDER BY of mixed directions over rows that tie were each too rare for a fault of them to show. `Profile.Aimed`, `Profile.Ties`, three columns in each table of `order`, more selects, and the forms of an ORDER BY of a key in `Sequence.orderColumns`, a gap one time in five and mixed directions three times in five, changed that. `Aimed` and `Ties` are 0 in every other profile too. The review of the generated checks of slice 14 found the same: an update that moves a row to another key, `*` on a table whose column with a default is not the last, and an update of several rows were each too rare for a fault of them to show. `Profile.NewKey`, `Profile.DefaultFirst`, `Profile.EveryRow`, and more updates in `returning` changed that, and each of the three fields is 0 in every other profile. The profile `subquery` takes `Keys`, `Shared`, `Ties`, and `ByKey` from the other areas, and adds `Profile.TableNames`, a field of the names, so that a subquery often reads another table than its statement. `TableNames` is 0 in every other profile, which then draws the number of its tables as before.

The steps cover the generator and its counts. The checks that slice 13 added to the tests of the engine beside them are in [`docs/todo/slice-13.md`](../../../../docs/todo/slice-13.md), under "Generated checks".

## What runs where

`slow` in `internal/engine/differential_test.go` reports whether a run has `-short`, the race detector, or the address sanitizer. A slow run of a generated test of the engine runs fewer sequences and checks no count: a tenth of the sequences in `TestDifferential`, one in twenty in `TestConn_differential`, and a tenth of the seeds of each cell, one at least, in `TestSimulation`. The driver check has no slow run. The digests of `TestNewSequence` are checked in every run.

| Where | What it runs | The generated tests |
| --- | --- | --- |
| `make test` | `go test -short ./...` | A slow run, without counts. |
| CI, job `sql` | `make test-sql`, which runs `./internal/sql/...` and `./internal/engine/...` without `-short`, `-race`, and `-asan` | The full run. It checks the counts of the `.tsv` files. |
| CI, jobs `test` and `test-engine` | `go test -race`. `test-engine` runs the engine, and `test` runs every package but the core and the engine. | A slow run, without counts. |
| CI, jobs `asan` and `asan-engine` | `make asan-test`, which runs `go test -asan -short`. `asan-engine` runs the engine in shards. | A slow run, without counts. |
| CI, job `crosscheck` | `go test -race ./...` in `internal/sqlite/crosscheck` | The driver check, in full. It checks the counts of its `.tsv` file, also under the race detector. `make test` runs this module too, without `-race` and without `-short`. |
| Nightly, job `generated-engine` | `TestDifferential` with `-sequences 400`, and `TestSimulation` with `S4QL_ENGINE_SIM_SEEDS=100` | Ten times the sequences of each profile, and 100 seeds of each cell. Each step checks the least numbers alone (`Reached.Least`), because the sequences and the seeds of the full run of CI are among its own. The second step runs also when the first fails. |
| Nightly, job `simulation-engine` | `TestSimulation_plantedBugs` with `S4QL_ENGINE_SIM_SEEDS` | Each planted bug on many seeds of its profile. |

A sequence that fails prints its seed, and a seed of the simulation that fails prints the command that replays it.
