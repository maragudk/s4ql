# Diary: Close two weak spots of the shape check

A follow-up to `/docs/diary/2026-11-16-tree-shape-check.md`. The shape check in `/internal/sql/shapetest` cannot see a fault that prints the same tokens in the same order, such as where a column's type ends and its constraints begin, and it compared REGEXP, MATCH, and GLOB ... ESCAPE only by their errors. This work closes both.

## Step 1: Requirements and reading

**Author:** shape-check-weak-spots-builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for a focused follow-up in s4ql (`maragu.dev/s4ql`, repo `maragudk/s4ql`): "Close two weak spots of the shape check"." Weak spot 1: "Close it by comparing **what our tree says** with **what SQLite says about the created table**, directly, with no printing in between [...] write the derivation from our tree by SQLite's documented and observed rules, name the source function for each rule [...] and keep it in the test package: it is a test's model of SQLite, not the engine's." Weak spot 2: "give the binding the smallest addition that lets a test connection have `regexp` and `match` functions [...] Keep it out of anything that is not a test's connection, documented as such, tested in the binding's own tests including under the address sanitizer."
**Interpretation:** Model in `shapetest` what SQLite's `build.c` records for a CREATE TABLE (and ALTER TABLE ... ADD, CREATE INDEX, CREATE VIEW), derive it from the tree, compare it with PRAGMAs; prove it by mutations; add stub functions to the binding so the operators prepare and run.
**Inferred intent:** Leave no tree fault that SQLite can see but the check cannot.

### What I did

I read `/AGENTS.md`, `/internal/sql/shapetest/README.md`, the package's code, the parts of `/internal/sql/parser/README.md` on GENERATED and constraints, the DDL nodes in `/internal/sql/ast/ddl.go`, `/internal/sqlite/README.md` and `/internal/sqlite/sqlite.go`, and the older diary. Then the functions of the amalgamation that the model follows: `sqlite3AddColumn`, `sqlite3AddNotNull`, `sqlite3AddDefaultValue`, `sqlite3AddPrimaryKey`, `sqlite3AddCheckConstraint`, `sqlite3AddCollateType`, `sqlite3AddGenerated`, `sqlite3CreateForeignKey`, `sqlite3DeferForeignKey`, `sqlite3CreateIndex`, `sqlite3EndTable`, `convertToWithoutRowidTable`, `sqlite3DequoteToken`, `sqlite3Dequote`, `sqlite3DbSpanDup`, and the PRAGMAs `table_xinfo` and `foreign_key_list` in `pragma.c`, together with the rules of `parse.y` around `ccons`, `tcons`, `refargs`, and `tconscomma`. I built the `sqlite3` shell from the vendored source with `options.h` into the scratchpad to try rules by hand. The branch is `shape-check-weak-spots`, from `origin/main` at `e485b60`.

### Why

The model must be SQLite's rules, not a guess at them, so each rule was read in the source and tried in the shell before it was written.

### What worked

The shell answered every question in seconds, for example that `create table t(a constraint c1 check(a>0) check(a<5))` names both CHECKs `c1`.

### What didn't work

Nothing yet.

### What I learned

No PRAGMA shows a column's collation, whether a foreign key is deferred, or the names of CHECKs. An index on every column shows the collations in `pragma index_xinfo`, and the program of `insert into t default values` holds a `Halt` with P1 275 and the name for every CHECK, and an `FkCounter` with P1 = isDeferred, or a `Halt` 787, for every foreign key, newest first.

### What was tricky

Deciding what to observe for deferral. A behavioural probe (insert a violating row, see whether the statement or the COMMIT fails) needs rows that violate one foreign key only; the program of an INSERT tells it directly.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The model of what a tree declares

**Author:** shape-check-weak-spots-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "for every `CREATE TABLE` statement [...] derive from our tree, for each column: its name, its declared type text as SQLite records it, whether it is `NOT NULL`, its default value text, its primary-key position, whether it is hidden/generated (and stored or virtual), its collation; and for the table: its foreign keys [...], its indexes from `UNIQUE`/`PRIMARY KEY` constraints [...], `WITHOUT ROWID`, `STRICT`, and autoincrement."
**Interpretation:** A model that replays the parser's actions over the tree, and an observation that reads the same facts from SQLite.
**Inferred intent:** A direct comparison, with no printing between the tree and SQLite.

### What I did

`/internal/sql/shapetest/declared.go` holds `declTable`, which applies the column and table constraints in order as the actions of `parse.y` do, and `observeTable`, which reads `pragma table_list`, `table_xinfo`, `foreign_key_list`, `index_list`, `index_xinfo`, `sqlite_schema` for `sqlite_sequence`, the program of an INSERT, and the collations through an index `s4ql_collations` that it makes and drops. Both give a `declaration` of lines that `compareDeclarations` compares. `statement` in `/internal/sql/shapetest/check.go` runs it on the original's connection, after the original ran, against the tree shown, and sets `Result.Declared`; a difference is a fault that starts with `declaredFault`. The same model covers ALTER TABLE ... ADD COLUMN and ADD CHECK, the key of CREATE INDEX, and the column names of CREATE VIEW. `/internal/sql/shapetest/declared_test.go` runs the 55 statements of `Declared` in `/internal/sql/shapetest/export_test.go`, one or more for each rule. The report counts the statements compared this way by kind.

### Why

What SQLite records is what an engine has to reproduce, and it is exactly what a wrong boundary in the tree would change.

### What worked

The first corpus run of the model matched SQLite on all 7,367 CREATE TABLE statements but one class, and the hand-written statements passed but three that were my own mistakes.

### What didn't work

- The first corpus run had 88 faults, all of one rule: `"CREATE TABLE t0(a /* */,b,c,dc,d,e, PRI, PRIMARY\nKEY(a,c,b,c,a,c,a,d,e))"` gave `column 5: by the tree [...] primary key 9 [...] by SQLite [...] primary key 8`. `pragma table_xinfo` looks for a column in the first nCol columns of the primary key's index only (`for(k=1; k<=pTab->nCol && pPk->aiColumn[k-1]!=i; k++){}`), so a key with repeated columns gives nCol+1. The model does the same.
- Three of my statements failed in SQLite: two primary keys in one table, `check("x")` on a column `a` (`no such column: "x"`), and `rowid` in CREATE INDEX (`no such column: rowid`; an index cannot name the rowid).
- A table with only generated columns fails with `must have at least one non-generated column`, which made one statement of `Declared` useless until I added a column.

### What I learned

SQLite's rules that the model had to follow, each tried in the shell: the type is the text from the first token to the last; `sqlite3DequoteToken` takes the quotes off when no quote is inside and drops the last byte whatever it is, so `[int] x` becomes `int] `; `sqlite3Dequote` then keeps only the first quoted part, so `'abc' def` becomes `abc`; a CONSTRAINT name names every CHECK after it up to the next column or a comma between table constraints, and the one at the end of the last column names the first table CHECK, because the comma before the table constraints is not `tconscomma`; DEFERRABLE applies to the foreign key made last, of any earlier column; two UNIQUE or PRIMARY KEY with the same columns and collations are one index, and the primary key takes over the earlier one; `MATCH` of a foreign key is read and dropped, and the PRAGMA always says `NONE`.

### What was tricky

The primary key: whether a single INTEGER column is the rowid depends on the order only for a column constraint (`sqlite3AddPrimaryKey` gets the sort order 0 for a table constraint), WITHOUT ROWID turns the rowid back into an index and drops repeated columns of the key, and STRICT and WITHOUT ROWID add NOT NULL to key columns.

### What warrants review

`declared.go`, rule by rule against the functions it names, and the observations of deferral and CHECK names from the program of an INSERT.

### Future work

None.

## Step 3: Stubs of regexp, match, and glob in the binding

**Author:** shape-check-weak-spots-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "Find out exactly which forms fall under this and why each fails (for `GLOB ... ESCAPE` and row-value misuse the cause may be another one; say what it is)."
**Interpretation:** Make the operators prepare and run on the check's connections, and explain what is still compared only by an error.
**Inferred intent:** Compare every operator by bytecode and values where SQLite can.

### What I did

`sqlite.Conn.AddTestFunctions` in `/internal/sqlite/sqlite.go` registers static C stubs for `regexp` and `match` with two or three arguments and `glob` with three. Each gives the byte order of its first two arguments' texts, -1, 0, or 1, plus with a third argument 3 times that plus the order of the second to the third, and NULL for a NULL argument. `TestConn_AddTestFunctions` in `/internal/sqlite/sqlite_test.go` checks the errors without them, the values, the order of the operands (`X REGEXP Y` is `regexp(Y, X)`), NULL, the built-in two-argument GLOB, and a CHECK and an index that use them. The check's connections in `/internal/sql/shapetest/schema.go` and `check.go` call it.

### Why

The three causes are different: SQLite has no `regexp` (`no such function: regexp`); its `match` is a placeholder for virtual tables, registered by `sqlite3RegisterPerConnectionBuiltinFunctions` through `sqlite3_overload_function`, that fails when it runs (`unable to use function MATCH in the requested context`); and its `glob` has two arguments, while `X GLOB Y ESCAPE Z` calls it with three (`wrong number of arguments to function glob()`). One stub covers all three.

### What worked

Pairs compared by bytecode and values went from 3,084 to 3,610 of 3,721, and those only by an error from 439 to 111; all 111, and all 783 triples only by an error, hold a row value, where SQLite says `row value misused` (or `IN(...) element has 1 term - expected 2`, `sub-select returns 1 columns - expected 2`).

### What didn't work

My first expectations in the binding's test were wrong, not the stubs: SQLite writes the function name as written in the message (`no such function: regexp`, not `REGEXP`), `v1 regexp 'm' >= 0` is `v1 regexp ('m' >= 0)` because `>=` binds tighter, and I miscounted `3*cmp('b','a') + cmp('a','a')` as 4.

### What I learned

User functions take no collation unless SQLite marks them, so a COLLATE moved over REGEXP, MATCH, or GLOB changes nothing, which the mutations now show.

### What was tricky

Nothing beyond the precedence of REGEXP.

### What warrants review

The C in the preamble of `sqlite.go`: `sqlite3_value_text` before `sqlite3_value_bytes`, and the out-of-memory path.

### Future work

None.

## Step 4: Proof by damage, and the mutation counts

**Author:** shape-check-weak-spots-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "Prove the new check can fail: damage trees on purpose (move the last word of a type into a constraint or the first constraint into the type, attach a `CONSTRAINT name` or a conflict clause to the neighbouring constraint, move `COLLATE` or `DEFAULT` between columns, change a foreign key's action from one clause to the other) and show each is caught; report the detection rate per damage, and explain or close every miss."
**Interpretation:** New kinds of damage in `/internal/sql/shapetest/mutate_internal_test.go`, rates per kind and per constraint, an explanation for every undetected case, and the two old rows measured again.
**Inferred intent:** Evidence that the comparison of declarations sees what the printed comparison cannot.

### What I did

Seven new kinds: GENERATED ALWAYS from the type into AS (expected never detected, with its reason in `same`), the last word of a type into AS as its ALWAYS, the first constraint of a column into its type, CONSTRAINT name after its constraint in a column, ON CONFLICT to the next constraint, ON DELETE and ON UPDATE swapped, and the form of DEFAULT (a name as a column reference, a sign as a unary operator). They also run on every tenth CREATE TABLE and ALTER TABLE of the corpus, and on `Declared`. The report has a column for the cases that only the declarations detected, and a breakdown by what was done for these kinds. `mutate` counts `Result.Declared` as a comparison only for damage to declarations.

### Why

The first four kinds keep the tokens in their order, which is exactly the fault the printed comparison cannot see; the others change the order and show that both comparisons agree.

### What worked

The constraint moved into the type: 773 detected, none undetected, 649 of them by the declarations alone. The last word of a type into AS: 3 of 3, all by the declarations. DEFAULT: 29 of 29, 9 by the declarations alone (the names read as column references). On the old sample alone, "move a column constraint" is 146 detected, 9 undetected, 3 not compared, and "move CONSTRAINT name" 1 detected and 2 undetected, as before: both change the order of the tokens, so the printed comparison already saw what SQLite sees.

### What didn't work

Nothing failed; the first run had too few sites for the new kinds (one each for ON CONFLICT and the actions), which the corpus sample of DDL fixed.

### What I learned

Every undetected case is one SQLite records the same: a DEFERRABLE moved to a column without REFERENCES applies to the same foreign key; a CONSTRAINT name moved off anything but a CHECK, or so that it still comes before the same CHECKs, or before a comma that ends it, changes nothing SQLite keeps; GENERATED ALWAYS at the end of a type is dropped; a COLLATE moved over REGEXP, MATCH, or GLOB changes nothing, because a user function takes no collation.

### What was tricky

Not counting a damage as compared when only the declarations ran but the damage was to an expression: the review caught that.

### What warrants review

The `same` reason of the GENERATED ALWAYS kind, and the explanations in `/internal/sql/shapetest/README.md`.

### Future work

None.

## Step 5: Review, fuzzing, documentation, and run times

**Author:** shape-check-weak-spots-builder

### Prompt Context

**Verbatim prompt:** (excerpt) "The check's fuzz test (`FuzzCheck`) includes the new comparison for `CREATE TABLE`; run it on several cores until quiet (at least 50 million executions with no finding); every finding becomes a regression case." and "Self-review before you report."
**Interpretation:** Competing reviewers over the diff, fixes, a long fuzz run on the final code, the READMEs by a writer, and the measurements.
**Inferred intent:** A check whose own faults are gone before anyone relies on it.

### What I did

The code-review skill gave ten points. I fixed: the probe index `s4ql_collations` collided with a statement that uses the name (`create table s4ql_collations(a)` failed with `there is already a table named s4ql_collations`), so it now takes a free name; a CREATE VIEW IF NOT EXISTS of a name the guessed schema holds compared the stand-in table (`column of the view 0: by the tree x, by SQLite s4ql_c`), so `schema.holdsTarget` covers views, per database; the table is now found in the schema the statement names; `mutate` counted declarations for unrelated damage; the guard of the long summary line was lost; `pragma table_xinfo` ran twice; and the dedup of indexes existed three times, now `addIndex`. I left the AUTOINCREMENT observation per schema, with a comment that the guessed schema never makes an AUTOINCREMENT table. The writer pointed out that SQLite refuses the rowid in an index, so I removed the rowid branch of `indexColumn`, which no accepted statement reached. A writer sub-agent updated `/internal/sql/shapetest/README.md` and `/internal/sqlite/README.md` from a facts file; I reviewed them and filled in the fuzzing and run times. `/docs/todo.md` said the boundary and REGEXP were not compared; it says now how they are.

### Why

Each of the two collisions would have been a fault of the check reported as one of the parser.

### What worked

`FuzzCheck` with 6 workers: run 8 made 35,737,982 executions in 20 minutes before the review's fixes, and run 9 made 59,494,380 in 30 minutes on the final code, both without a finding, at a load of 40 to 65 from other sessions. No finding means no new regression case. The parser's tree had no fault anywhere: the corpus, the pairs and triples, the hand-written statements, and the fuzzer agree with SQLite on what the tree declares.

### What didn't work

My first regression case for the view, `create view v(x) as select 1 from temp.v`, failed in SQLite: `view v cannot reference objects in database temp`. The case is now `create temp view v(x) as select 1 from main.v`, and the IF NOT EXISTS view went to `Structure`, because it is rightly not compared. A shell command with a heredoc for `python3` was refused by the sandbox (`this command is too complex to verify that it stays inside the worktree`), so I edited with the Edit tool and `perl -pi` instead.

### What I learned

At a load of 40 to 55 wall times say little. Back to back with the code of `main`, exported with `git archive` to the scratchpad: the plain run of the package took 52 s with 62 s of CPU before and 39 s with 65 s of CPU after; with `-race`, 79 s with 45 s of CPU before and 116 s with 49 s after. `make test-asan FUZZTIME=30s` passed in 790 s at a load of about 50, with 85 s for `internal/sql/shapetest` under the sanitizer and 80 s for `internal/sqlite`, whose new test runs there too.

### What was tricky

Waiting on long runs without polling: the fuzz runs and the sanitizer each took half an hour on a shared machine.

### What warrants review

`observeColumns` and `locate` in `/internal/sql/shapetest/declared.go`, and the new section "What the tree declares" of the README.

### Future work

None from this work.
