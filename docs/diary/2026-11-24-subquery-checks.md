# Diary: the generated checks of slice 15, the scalar subquery

Slice 15 adds a scalar subquery in the WHERE of a select, an update, and a delete, the shape of goqite's receive. Its statement pass, engine, and driver were built and reviewed with table tests, and `stmttest.Sequence` wrote no subquery. This step makes the subquery an area of the generator with a profile of its own, counts and requires it in the engine's differential tests, the simulation, and the driver check against mattn/go-sqlite3, plants bugs, and gives the refusals of s4ql their excuses.

## Step 1: the area, the profile, the planted bugs, and the differential tests of the engine

**Author:** builder (slice-15-checks)

### Prompt Context

**Verbatim prompt:** "You are the builder of the generated checks of slice 15 of s4ql (`maragu.dev/s4ql`) [...] Build: 1. **The generated workload** (`internal/sql/stmt/stmttest`): `Sequence` writes subqueries. An area and a profile `subquery` in the way `returning` got them [...] 2. **The engine's differential test** on generated sequences [...] 3. **The simulation** [...] 4. **Planted bugs** (`internal/engine/bugs.go`) [...] 5. **The driver's generated test** against mattn [...] 6. **Excuses** [...] An independent reviewer plants faults of its own after you hand in; a planted fault that every check passes is a finding against this step."
**Interpretation:** follow the recipe "Add a feature area" of `/internal/sql/stmt/stmttest/README.md` for the subquery, aim the profile at the cases that the brief lists, count them per statement kind where they differ, plant bugs and count how often the generated sequences find each, and split the simulation and the driver check off to two forks in worktrees of their own.
**Inferred intent:** trust in the subquery, above all in goqite's receive and in the rule that an undecided first row may be any legal one, rests on the same generated machinery as every slice before, with floors that fail when the generator stops reaching what matters.

### What I did

- `/internal/sql/stmt/stmttest/stmttest.go`: the hook `Generator.Inner` gives the select of a subquery that a hook of a WHERE names by `stmt.Term.Subquery`; `Generator.subquery` writes a given one or one at random. With `Inner` nil the generator draws as before.
- `/internal/sql/stmt/stmttest/profile.go`, `/internal/sql/stmt/stmttest/sequence.go`: the area `subquery` with the stream `rSubquery`; `Profile.Subquery`, `Receive`, `Again`, and `TableNames`; the profile `subquery` at slot 12; `Mixed` with `Subquery` 8. `Sequence.subqueries` turns a WHERE into one with one or two subqueries, on either side, with each operator, and now and then a condition that is never true; `Sequence.innerSelect` draws the select (table, column, WHERE, ORDER BY, LIMIT, and the refused shapes); `Sequence.receive` writes goqite's receive; `Profile.Again` runs the text of the last statement with a subquery again with other arguments; `Sequence.Decide` makes every ORDER BY of a subquery end with the key, for the driver check. `indexConditions` takes its stream as a parameter, a refactor that draws the same numbers.
- `/internal/sql/stmt/stmttest/rule.go`: `SelectText`, `FoundQuery` (which now takes the subqueries), and `RowQueries` write a subquery inline (`subqueryText`); `SelectOf` keeps the subqueries, renumbered; `WhereCases` names "comparison with a subquery"; `WhereRule` passes over one; `KeyAccess` and `ChosenAccess` decline a select with one.
- `/internal/engine/bugs.go`, `subquery.go`, `operator.go`, `tx.go`, `engine.go`, `access.go`: nine planted bugs, `lastRow`, `firstNotNull`, `ownChanges`, `keepValue`, `noRowMatches`, `outsideTx`, `swapSubqueries`, `limitZeroRow`, `orderIgnored`. `Engine.ObserveSubqueries` now gives its function the context of the statement, so that the simulation, whose statements run at once, can tell them apart.
- `/internal/engine/differential_test.go`, `/internal/engine/subquery_test.go`: `pair.reach` counts the outer kinds on the flattened statement; `pair.reachSubquery` counts what the subqueries reach; `checkLength`, `checkType`, `longChange`, and `longCondition` read the literals of a subquery, with `correlatedRule` first; `sequencesOf` doubles `subquery`; eight bugs in `TestDifferential_plantedBugs`. The `.kinds` files got 133 kinds in `TestDifferential` and 9 in `TestConn_differential`, each at a third of its total, and the `.tsv` files were recounted.

### Why

The pair already compared a statement with a subquery through its candidates and `Engine.ObserveSubqueries`, but no sequence carried one, and the helpers of `stmttest` that the pair and the driver check use panicked on a term of a subquery (`boundValue` read `args[-1]`).

### What worked

- No sequence of any profile found a difference from SQLite: `TestDifferential` passed on its 800 sequences with only the counts to recount.
- The planted bugs, 30 sequences of `subquery` each (12 for two): `lastRow` 6 of 12, `firstNotNull` 7 of 30, `ownChanges` 14, `keepValue` 16, `noRowMatches` 12 of 12, `swapSubqueries` 12, `limitZeroRow` 13, `orderIgnored` 5 of 12, before it ran all 30.
- A scratch test that ran the sequences of one profile with a planted bug made each change of the profile a run of seconds.

### What didn't work

- The first sequences of the profile found mostly errors: "no such table" for a subquery of a table that the sequence had dropped, `compared with the column` for a column of another type, and `sub-select returns 3 columns - expected 1` for `*`. The table of the statement now stands in where another table has no column of the type, `*` of a table of several columns is rare, and the WHERE of a subquery has the faults of a select.
- `TestZZScratch` failed with "SQLite gives 18/18: string or blob too big for "select ..." from `subqueryCandidates`, which bound a long blob literal of a subquery as an argument; SQLite turns the literal into NULL in the text. It now gives no candidate there; s4ql refuses the statement for its length first.
- `TestDifferential` failed on `delete from "A B" where "first" <> (select first from "a b" order by "a" limit ?)` with a `sql.NamedArg`: "the engine computed 1 subqueries of a call whose subqueries SQLite does not run". `subqueriesOf` read the named argument as one that no column holds; it reads `unnamed(c.Args)` now.
- `checkType` failed on a delete with a literal longer than the limit in its subquery: "the engine gives 18/18: string or blob too big [...] and the rule "s4ql: not supported: subquery 1, of the column [...]"". The rule of types did not know that a long literal of a subquery comes first.
- An edit with `python3 /dev/stdin <<'EOF'` changed nothing, as the slice 14 diary warned; `python3 -` and script files work.
- goqite's receive changed no row in most runs: its pin had another value than the pin of its subquery. The pin of the statement is now that of its subquery, and the values come from those that inserts gave.

### What I learned

- A focused profile meets "no such table" far more than one expects once a statement names two tables: the second is live only half the time.
- `firstNotNull` is the hardest of the value bugs: it shows only where the first row of a decided order holds NULL and a later one a value.

### What was tricky

- Every draw of the subquery area moves every later statement of `subquery` and of `mixed`, so the forks' recounts of those rows go stale while I tune; I told them so, and recount after the merge.
- A refusal of a correlated subquery comes after the names and before the lengths, so the rule of a long literal must look for it first.

### What warrants review

- `Sequence.innerSelect` and `Sequence.receive`, and whether their docs say what they draw.
- `pair.reach`, which now counts the outer kinds of a statement with subqueries on the flattened statement, NULL in place of a value that the engine did not compute.
- The planted bugs' hooks, above all `changedOneByOne` for `ownChanges` and the reader of `outsideTx` in `Conn.writeIn`.

### Future work

- The simulation and the driver check, in two forks; their results are in the next steps.
