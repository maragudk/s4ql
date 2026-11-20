# Diary: Mutation testing of slice 7, `where` on `select`

Slice 7 of the roadmap went through the statement pass (#80), the engine, the core's scan by the key, and the driver (#81), and the generated checks (#82). This task runs the mutation tester `internal/sql/mutation` over the code that slice 7 added or changed, closes the gaps that surviving mutants show or lists them as equivalent, and merges `checkCompared` into `checkType` of the engine. The branch is `where-mutation`, from `origin/main` at `65161cb`, after the merge of #85.

## Step 1: The selection of mutants, and one `checkType`

**Author:** where-mutation builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Scope of the mutants: the code slice 7 added or changed. In the engine: `keyAccess`, `tighter`, `Filter` and its three-valued logic, `compare`, `checkCompared`, the planted bug `openBound`, `AccessOf`, and the `scanAll` path. In core: `ScanRange` on `ReadTx` and `Tx`, `Bound`, `checkBounds`. In the driver: what slice 7 changed in `conn.go`/`rows.go`. In the statement package: the recognition of `where` (`Condition`, the operators). [...] 1. Every mutant is killed by a test, or is listed in `equivalent.txt` with a reason that a reader can check [...] 2. Merge the duplication between `checkCompared` and `checkType` in `internal/engine/errors.go` [...]. One function with the two callers, messages unchanged, verified by the tests that exist and by the mutants. [...] 4. Other builders are working right now [...]. Keep your changes to `equivalent.txt` to appended or clearly separate entries, and stay out of `cache.go`'s upgrade code [...]"
**Interpretation:** Select the functions of slice 7 as the round of slices 5 and 6 did, by `git blame`, merge the two checks of the type before the run so that the run mutates the merged function, and run each package with `-first`.
**Inferred intent:** The same trust in slice 7 as in slices 1 to 6, without rerunning what earlier rounds covered, and with small merges against the builders that run at the same time.

### What I did

`go run ./internal/sql/mutation -list -pkgs internal/sql/stmt,internal/engine,internal/core,.` lists 11,561 mutants. A throwaway script in the scratchpad (`wm/select.py`) runs `git blame -w --line-porcelain HEAD` on each file, and selects every function with a mutant on a line whose commit is in `5f5cb40..faba8c6`, the commits of #80, #81, and #82, with all its mutants. Of the large functions that slice 7 changed in a line or two, only the mutants of those lines: `Conn.exec`, `Conn.queryText`, `Conn.query`, `newEngine`, `Conn.queryIn`, `Tx.spoolCommit`, `recognizer.insert`, and `recognizer.selectCmd`. That gives 1,213 mutants:

- `stmt`, 313: `recognizer.conditions`, `condition`, `isCondition`, `leftColumn`, `term`, `depth`, `isBinary`, `isValue`, `isTrueOrFalse`, `comparisons`, `maxDepth`, `Operator.String`, `OpEqual`, `Select.Parameters`, and the lines of `insert` and `selectCmd`.
- The engine, 686: `scan.rows`, `filter.*`, `condition.holds`, `compare`, `keyAccess`, `tighter`, `buildQuery`, `build.node`, `build.filter`, `checkCompared`, `NewPlan`, `formatNode`, `bound.core`, the two `scanRange` of `storage.go`, `bugs.reader` and `openBounds.scanRange` of the planted bug, and the lines above.
- The core, 202: `conn.scan`, `table.checkBounds`, `ReadTx.Scan`, `ReadTx.ScanRange`, `Tx.Scan`, `Tx.ScanRange`, and the line of `Tx.spoolCommit`. `Bound` is a type and has no mutant.
- The driver, 12: `check` of `conn.go`, whose call of `engine.Check` slice 7 removed. Slice 7 did not change `rows.go`.

`engine.AccessOf` is in `export_test.go`, and the tester mutates no test code, so it is out of reach; the mutants of `keyAccess` show whether the check that uses it works.

`checkCompared` was `checkType` with "compared with" in place of "for" in its message. Now `checkType` in `/internal/engine/errors.go` takes `use`, the words between the value and the column, and `insert.check` and `build.filter` in `/internal/engine/operator.go` pass "for" and "compared with". The messages are unchanged, and the short tests of the engine pass. The engine's README names `checkType` for the conditions now.

### Why

The merge comes before the run so that the run mutates the function as it is after the merge, with both callers' tests against it.

### What worked

The script of the round of slices 5 and 6, rewritten in a few lines, gave the selection in one run.

### What didn't work

Nothing yet.

### What I learned

`check` of the driver changed in slice 7 although `git diff 5f5cb40 faba8c6 -- conn.go` is empty: #81 removed the line that the pass's builder had added in the same range, and `git blame` gives the lines around it to the commit of the removal.

### What was tricky

Nothing yet.

### What warrants review

- The selection: whether a function of slice 7 is missing, above all one that only the generated checks of #82 touched in non-test code (`stmttest` is code for tests and is not mutated).
- The doc comment of `checkType`, now for both callers.

### Future work

The runs of `stmt`, the engine, the core, and the driver, and their survivors.
