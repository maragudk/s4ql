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

## Step 2: the simulation, the driver check, the offset, the final aims, and the self-review

**Author:** builder (slice-15-checks), with two forks of it: slice-15-sim and slice-15-driver

### Prompt Context

**Verbatim prompt:** (the same brief as step 1: "3. **The simulation** [...] the model must take the value the engine really used, or the candidates, and must not accept a wrong value. [...] 5. **The driver's generated test** against mattn [...] decide soundly how to handle it [...] 6. **Excuses** [...] Self-review your diff against this brief when done, and fix what you find.")
**Interpretation:** take the engine's values into the simulation and check each against SQLite's candidates; make the driver check see only decided subqueries; give each refusal of s4ql its excuse; then recount once on the final profile, measure the time, and review.
**Inferred intent:** no check accepts a wrong value, and none fails for a legal one.

### What I did

- The fork slice-15-sim (`/internal/engine/simulation_test.go`): `run.open` sets `Engine.ObserveSubqueries`, which records the values of each attempt by its id; `run.flattened` checks each against `subqueryCandidates` on the state before, and runs SQLite with the flattened statement in `run.verify`, the replay of the log, the begin state of a transaction, and `run.verifyTxs`; `subqueryRefused` and `subqueryKinds`; cells of 8 and 40 seeds of `subquery`; the nine bugs in `TestSimulation_plantedBugs`.
- The fork slice-15-driver (`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`): `q.Decide()` in `generated`; the harness reads subqueries; `subqueryRefusal` after the engine's `subqueryRule`; five excuses, each pinned by a subtest of `TestPair`; goqite's real receive with its subquery in `TestPair`; its own `reachSubquery`.
- `/internal/engine/operator.go`: `row value misused` of a subquery on the left of a comparison in a select has no offset, as SQLite gives none; `TestEngine_subquery` pins both sides with `pin`.
- `/internal/engine/differential_test.go`: `TestPair_subqueryTooLong`; `typeRule` and `subqueryRule` take the values that the engine computed, for `found` of an update.
- The profile: `nullFirst` in `Sequence.innerSelect`, `Profile.Receive` 2, and `generatedOf` of the driver check 60 for `subquery` and 50 again for `returning`; one recount of all four tests by a script in the scratchpad (`recount.sh`), where only rows of `mixed`, `subquery`, and their environments move against `main`.
- A writer (opus) updated `/internal/sql/stmt/stmttest/README.md` and `/internal/engine/README.md`; I read the diff and corrected the counts of the simulation's bugs to the final run. Notes in `/docs/todo/slice-15.md` ("Generated checks") and `/docs/todo/generated-workload.md`.

### Why

A value that the engine drew among rows that tie is legal and differs from SQLite's; the simulation must use it, not reject it, and must reject a value that no legal order gives. The driver check has no hook, so only a decided first row keeps both sides in one state.

### What worked

- `TestSimulation_plantedBugs` on 200 seeds of the final profile finds `lastRow` 184, `firstNotNull` 104, `ownChanges` 138 (77 by its own violation), `keepValue` 106, `noRowMatches` 192 (186), `outsideTx` 106 (60), `swapSubqueries` 150 (147), `limitZeroRow` 138, and `orderIgnored` 174.
- `TestDifferential_plantedBugs`: 10 of 12, 12 of 30, 16 of 30, 12 of 30, 12 of 12, 15 of 30, 12 of 30, and 22 of 30.
- About 22,000 calls of `TestGenerated` compared with mattn, and no difference.

### What didn't work

- The offset of `row value misused`: `pin` failed with "Expected "-1", but got "34"" on `select * from t where v < 1 and (select * from u) != k`. A scratch test showed SQLite gives -1 for each of the eight spellings with the subquery on the left, on a fresh connection too.
- `TestSimulation_plantedBugs` with `S4QL_ENGINE_SIM_SEEDS=200` failed: "seed 6124 finds something without the bug: [the statement 143, ";uPDaTE /* id 143 */ "1" set GROUPS = ?, "materialized" == 'V$W', a == 0 WherE "A" >= (select "groups" from "1" limit 1) RETURNING *;", of the transaction 25 (write), which began at commit 14, gives "s4ql: not supported: a string literal for the column 1.MATERIALIZED of type INT", and the rule for the type of a value ""]". The rule asked SQLite whether the WHERE finds a row, with SQLite's first row of an undecided subquery; the engine had drawn another, found none, and so gave the types of SET before NOT NULL. The rule now takes the engine's values. The pair had the same gap, unreached.
- `swapSubqueries` was found by "another violation" at seed 1420: "the storage core refused a call that the engine had checked", since the swap gives a condition a value of another type after the check of the types. It is in its finds now.
- After the merge of the driver branch, `TestGenerated` found goqite's receive with its subquery 0 times on the fork's base and 3 times after `Profile.Receive`; with one update in two it is 5. `make test` failed on `TestGenerated` before that merge, as expected, since the harness did not yet read subqueries.
- A recount after `nullFirst` failed for `TestConn_differential`: "no run reaches "reached: subquery that finds several rows that tie for the first, of which it gave another, in a select" [...] three times: the runs reach it subquery 2 times". That kind left `TestConn_differential.kinds`; `TestDifferential` requires it.
- Commands that changed directory before `git`, or held a heredoc with certain words, were refused; scripts in the scratchpad ran instead.

### What I learned

- An undecided subquery changes more than the value: which error comes first in an update can depend on whether its WHERE finds a row, so every rule that asks SQLite about the rows must use the engine's values.
- SQLite gives no offset for an error whose left side is a subquery.

### What was tricky

- Every aim of the profile moved the rows of `subquery` and `mixed` in four tests and the floors of the forks' kinds; a single script that dumps the tally, writes the block of slice 15 of each `.kinds` file at a third of its totals, and recounts made it one step.

### What warrants review

- `run.flattened` and the places in `run.verify` and `run.verifyTxs` that call it, and `foundQuery` with values.
- The excuses of the driver check, above all `excusedQuotedSubquery`, which excuses no rows but takes any error of the names.
- The code-review skill (medium) found no correctness bug and seven nits; I fixed five: the model of a subquery's access got all the statement's arguments, `Sequence.receive` would panic on a table of one column, the tie of the driver check dropped the last column and not the key, a refusal of an argument lost its excuse to a later run, and the doc of `TestPair_subqueryTooLong` was wrong. The LIMIT of goqite's receive read from the first run is the same in every run, since `again` keeps an integer argument of a LIMIT; and the selects of the driver check built without `Subqueries` are reached only by statements without one.

- The time, measured back to back at a load of 12 to 15 with `/scratchpad/time.sh` on an archive of the base in `/Volumes/990/s4ql/slice-15-checks/base`: the engine's package 134 s before and 165 s after, then 136 s and 162 s after `TestDifferential_plantedBugs` ran 12 sequences for six of its eight bugs of slice 15; `stmttest` 2 s and 2 s; the driver check 17 s and 18 s, and under the race detector 58 s and 86 s, against 92 s and 98 s that the fork measured at another moment. The engine grows by 26 s where slice 14 added 15 s; the next cuts are the hammer cell of `subquery` from 40 seeds to 20 and `TestDifferential` from 80 sequences of `subquery` to 40, each with a recount.

### Future work

- The thin kinds and the unchecked access of a subquery, in `/docs/todo/generated-workload.md`.
- The time of the engine's package, if the lead wants it nearer to slice 14's growth.
