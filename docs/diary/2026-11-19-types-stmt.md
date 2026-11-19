# Diary: Slice 4 of the roadmap in the statement pass, the column types and the literals

Slice 4 adds the column types `integer`, `real`, `text`, and `blob`, and real, string, and blob literals. This task is the statement pass, `internal/sql/stmt`, only; the storage core is another builder's task at the same time, and the engine and the driver come after both. The work is on the branch `types-stmt`, which starts from `origin/binding-types` (pull request #67, which gave the binding `BindDouble`, `ColumnDouble`, `BindBlob`, and `ColumnBlob`).

## Step 1: The types, the literals, and a port of SQLite's reading of reals

**Author:** types-stmt builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Slice 4 adds the column types `integer`, `real`, `text`, and `blob`, and literals of these kinds. **Your task is the statement pass only.** [...] `stmt.CreateTable` tells the type as it was declared (`int` and `integer` stay apart). [...] Literals as the value of an `insert`, each decoded to its value, so that `stmt.Insert` holds a typed value (an integer, a real, text, or a blob) or a parameter [...] A real literal has the same value as in SQLite, bit for bit. [...] Either port SQLite's routine exactly, or use Go's and reject, as not supported, every literal for which you cannot show that both agree [...] A literal that SQLite reads as an infinity (`1e999`) is rejected as not supported [...] `-0.0` is a real with the value negative zero. [...] until the engine's own builder comes, the engine refuses a column type other than `int` and a value other than an integer as not supported [...] `internal/sql/stmt/stmttest` (the generator that other tests use) keeps generating what it generates today; extend it only inside the pass's own tests."
**Interpretation:** Extend the recognizer and the typed form of the pass, decode every literal, prove each decoded value against SQLite, keep the engine and the differential test of the driver working by a temporary refusal in the engine, and update the pinned numbers with reasons.
**Inferred intent:** The front half of slice 4 with the same trust as slices 1 and 2: whatever the pass accepts has exactly SQLite's value, and the next builders can build the engine on a typed form.

### What I did

`/internal/sql/stmt/stmt.go`: `CreateTable.Type` of the new `Type` (`TypeInt` to `TypeBlob`, zero is no type, `String` gives SQLite's recorded name `INT`, `INTEGER`, ...), and `Insert.Value` of the new sealed interface `Value`: `Integer`, `Real`, `Text`, `Blob`, nil for a parameter. `/internal/sql/stmt/recognize.go`: `column` takes one bare word of `columnTypes`; `literal` (was `integer`) decodes an integer, a real, a string (`''` read as `'`), and a blob (`hex.DecodeString`), and rejects `string after a sign`, `blob after a sign`, `infinite real number`, and `TRUE or FALSE`. The name of a type that is not supported is now `column type other than INT, INTEGER, REAL, TEXT, or BLOB`, also for the node type `TypeName` in `/internal/sql/stmt/vocabulary.go`. The names `real number`, `string`, and `blob` are gone from the README's tables.

`/internal/sql/stmt/real.go` is a port of SQLite 3.54.1's `sqlite3AtoF`, `sqlite3Fp10Convert2`, `powerOfTen`, `sqlite3Multiply128`, and `sqlite3Multiply160` (lines 37290 to 37760 of `/internal/sqlite/c/sqlite3.c`), with `math/bits` for the products. The sign and the spaces of `sqlite3AtoF` are left out, since a token of a real literal has neither; the caller negates after the call, as SQLite's `codeReal` does.

Tests: `/internal/sql/stmt/stmt_test.go`'s `checkMeaning` compares the value of every supported literal with SQLite's `select <literal text>` (type, and value by its 64 bits for a real, byte for byte for text and blobs), runs the insert on a table whose column has the value's type, and compares CREATE TABLE's recorded type. `/internal/sql/stmt/check_test.go` has 30 new hand-written cases. `/internal/sql/stmt/generate_test.go` wraps `stmttest`'s generator: one statement in two without a parameter gets another type or literal; reals come from a float64 that `strconv` writes in six forms, so `TestGenerate` also checks that SQLite rounds those to the nearest float. About 30 new near misses. `/internal/sql/stmt/real_test.go` has `TestCheck_reals`, 100,000 texts made to be hard to round, and `FuzzLiteral`.

Outside the pass: `/internal/sql/stmt/stmttest/stmttest.go` sets `Type: stmt.TypeInt` and wraps the integer, and generates what it did. The engine (`/internal/engine/errors.go`, `checkTypes`, called from `check` in `/internal/engine/engine.go`) refuses a column type other than INT (`s4ql: not supported: column type REAL`) and a literal that is not an integer (`s4ql: not supported: a real literal, which is not an integer`), and `NewPlan` panics for them; tests in `/internal/engine/engine_test.go` and `/internal/engine/plan_test.go`. `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go` checks such calls against the engine's rule (`checkTypesRefused`, `excusedTypes`, pinned at 29) and pins two sequence steps.

### Why

A port of the same integer arithmetic gives SQLite's bits for every literal, also those of more than 19 digits where SQLite truncates the mantissa. The other way, Go's `strconv.ParseFloat` with a bound, would have rejected literals that SQLite reads fine, and the bound would have to be argued from the same source anyway.

### What worked

`TestCheck_reals` passed on its first run: 100,000 texts, 81,351 reals and 18,649 infinities, all bit for bit as SQLite, while `strconv.ParseFloat` differs from SQLite on 6,998 of them. SQLite's own example from the comment of `sqlite3AtoF`, `3500000000000000.2500001`, is `3500000000000000.0` in the pass, as in SQLite, and `.5` in Go. `TestGenerate` passed with 20,000 statements, so SQLite rounds the shortest digits of strconv to the nearest float for those. Short fuzz runs found nothing: `FuzzLiteral` 7,192,877 executions in 60 s, `FuzzCheck` 4,915,832 in 45 s, `FuzzGenerate` 30 s. Statement coverage of the package is 98.9%, and every line of `real.go` is covered.

### What didn't work

- Bash with a Python heredoc that edited `stmt.go` was refused by the sandbox: "this command is too complex to verify that it stays inside the worktree". The Edit tool worked.
- The first `go test -short ./internal/sql/stmt/` after the rename failed in every seed of `FuzzCheck` with, for example, `"create table t (v any primary key) strict, without rowid": "column type other than INT, INTEGER, REAL, TEXT, or BLOB" is not in the vocabulary of the README`: the test reads the vocabulary from the README, which I then updated.
- `go test ./internal/sql/stmt/` without `-short` failed on the pinned counts: `1 texts are syntax errors where SQLite does not say [...] but the test pins 3`, `367 [...] pins 360` in `TestGenerate_mutants`, and `985 [...] pins 1046` in `TestGenerate_nearMisses`. A throwaway test that counted the excused texts by form, with and without the replacement, showed why (below); the numbers are pinned anew.
- `cd internal/sqlite/crosscheck && go test ./...` failed in `FuzzSequence` seeds: `"create table t2 (v2 integer primary key) strict, without rowid" with the arguments []interface {}(nil): s4ql gives exec: exec failed with s4ql: not supported: column type INTEGER, where the rule gives ""`. The rule of the driver's differential test took a text that `stmt.Check` supports to be refused only for an argument. It now asks `engine.Check` first. Then `TestGenerated` failed with `texts excused as a column type or a literal that the engine does not support yet, checked against its rule: 29, but the test pins 0`: the 29 are the `integer` create tables of `stmttest.Others`.
- `gofmt` turned `''` in a doc comment into the typographic quote `”`; the comment now says "two quotes read as one".

### What I learned

- In SQLite 3.54.1 `sqlite3Fp10Convert2` is an adaptation of Russ Cox's fpfmt, with a 96-bit table of powers of ten; the mantissa is a `u64` that stops taking digits at about 19, so a literal's later digits never round it.
- SQLite records a standard type of a STRICT table in upper case, whatever the case written, and dequotes `"text"` into TEXT. An unknown type gives `unknown datatype for t.v: "foo"` only in `sqlite3EndTable`, after the parse, so the oracle says the text parses.
- `select -'a'` and `select -x'01'` give the integer 0; `select -0.0` gives negative zero; `select 1e-400` gives 0.0; the build has `DQS=0`, so `select "a"` is `no such column`.
- In a REAL column SQLite stores a real that has an integer value as an integer, so `-0.0` reads back as `0.0`.
- Why the pinned numbers moved: `stmttest`'s names are drawn as before, so only the type and the literal differ. AUTOINCREMENT after a type other than INTEGER is stopped by a grammar action (`AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY`), which the oracle excuses; with INTEGER SQLite parses it and refuses it later for WITHOUT ROWID, so 61 near misses of that form are no longer excused (516 to 455). Two of the three syntax errors where SQLite does not say were mutants of CREATE TABLE statements whose type is now replaced.

### What was tricky

- `difference` in `generate_test.go` compared `a.Value != b.Value`, which would panic for two `Blob` values in interfaces; it compares `show` now.
- Near misses must not become supported: `values (%[3]v.)` makes the real `1.` of an integer, and `values (- %[3]v)` makes `- 1`; both were dropped.
- The engine refuses the types in `check`, so `engine.Check` refuses them too, but the driver's `Prepare` uses `stmt.Check` only, and such a statement prepares and fails when it runs. The driver is out of scope, and the refusal is temporary.

### What warrants review

- `/internal/sql/stmt/real.go` against lines 37290 to 37760 of `/internal/sqlite/c/sqlite3.c`, above all `multiply160`, the `sticky` and `mid1 < mid2` lines, and the precedence of `m&^u64Bit(52) | uint64(1075-e)<<52`.
- `literal` in `/internal/sql/stmt/recognize.go`: the offsets of the new rejects, and that each happens while its token is next.
- `checkTypes` in `/internal/engine/errors.go`, and `checkTypesRefused` in the driver's differential test.

### Future work

- The engine, the core, and the driver for the other types; then the driver's `Prepare` refuses nothing of slice 4.
- Mutation testing of the new code; `internal/sql/mutation` puts `TestCheck_reals` and `FuzzLiteral` in the layer `hand-written` until its lists name them.
- Arguments of the other types, and the refusal of a text argument with a NUL byte.
