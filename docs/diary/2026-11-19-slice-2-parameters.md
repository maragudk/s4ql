# Diary: Slice 2 of the roadmap, parameters

Slice 2 lets an argument stand where the integer literal of a slice-1 `insert` stands: `db.ExecContext(ctx, "insert into t1 (v1) values (?)", 1)`, directly and through a prepared statement. The work is on the branch `slice-2-parameters`, which starts from `origin/main` at `10ed900`.

## Step 1: The statement pass and the engine

**Author:** slice 2 builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Slice 1 is built end to end [...] Today any argument gives `s4ql: not supported: parameters`. Slice 2 lets an argument stand where the integer value of an `insert` stands [...] A plain `?` in the place where slice 1 has the integer literal of an `insert`. Nowhere else [...] An argument that is a Go integer and fits a signed 64-bit integer [...] Both directly [...] and through a prepared statement [...] The number of arguments must equal the number of `?` in the text. Any other number is an error, the same for a direct call and a prepared one, and the statement does not run [...] an ordinary error with a message that starts with `s4ql: ` and says both numbers; it is not `ErrNotSupported`. The commit object keeps the arguments in the annotation of the statement [...] `Statement.parameters` [...] Do not change the format [...] rejected as not supported [...] `?1`, `:name`, `@name`, `$name`, and an argument made with `sql.Named` [...] An argument of any other type [...] A parameter inside an expression, such as `values (-?)` [...] The text is judged first, then the arguments, then the instance [...] use that machinery for the new texts with `?` (including what SQLite says about `?0`, `? ?`, `?1`, and the like), and keep its \"Known gaps\" honest. Values of arguments never go into logs."
**Interpretation:** Teach `internal/sql/stmt` the plain `?` as the value of an insert and how many parameters a statement has, checked against SQLite; teach `internal/engine` to take arguments, count them after the text, bind them, and annotate the commit with them. The driver comes in the next step.
**Inferred intent:** The first public use of parameters, with the same trust in correctness as slice 1: everything that is accepted behaves as in SQLite, and everything else is refused before anything runs.

### What I did

In `/internal/sql/stmt`, `Insert` got a field `Parameter int` (1 for a `?`, 0 for a literal), and the interface `Statement` got `Parameters() int`, the number of parameters as `sqlite3_bind_parameter_count` gives it. The recognizer (`/internal/sql/stmt/recognize.go`) takes an `ast.Variable` whose text is `?` as the value, and rejects `?NNN` as `numbered parameter`, `:a`, `@a`, `$a`, and `#a` as `named parameter`, and `-?` as `parameter after a sign`. Those three names are in the README's table of checks, which the tests read.

The tests of the pass (`/internal/sql/stmt/stmt_test.go`) bind arguments now: `checkMeaning` runs an insert of a parameter in SQLite with the parameter bound to one of `MinInt64, -1, 0, 1, MaxInt64` (picked by the length of the text), and for every supported statement compares `Parameters()` with SQLite's count. The binding has no call for `sqlite3_bind_parameter_count`, so the test reads SQLite's count as the largest index that `sqlite3_bind_int64` takes before it gives `SQLITE_RANGE` (25). That keeps `internal/sqlite` unchanged, so no run under the sanitizer is needed. `/internal/sql/stmt/check_test.go` has 2 supported cases and 27 near misses with a parameter, each checked by `verify` against the oracle. The near misses of the generator replaced `values (?)` with `(?1)`, `(:a)`, `(-?)`, and `(?, ?)`. `TestAllowList` allows `Variable` and `Variable.Name`.

In `/internal/engine`, `Conn.Exec` and `Conn.Query` take `args ...*s4qlpb.Value`. `prepare` checks the text, then the number of arguments (`countArguments`, which gives an error that matches the new sentinel `ErrArguments`), then that each is an integer (else an error that matches `stmt.ErrNotSupported`), all before anything touches the database. The plan has a new expression `*Parameter{Number}`, which `Format` prints as `?1`, and `eval` reads it from the arguments. `write` hands the arguments to `annotate`, which hands them to `core.Tx.Annotate`, which already took parameters. `engine.Check(src, n)` gives the same errors of the text and of the count without a database and without an event, for the driver. The event of a statement gets the outcome `wrong_arguments` at Debug for `ErrArguments`; without it, the error fell to `core_error` at Warn.

The tests: `TestEngine_parameters` pins inserts of parameters against SQLite through `pin`, which now takes arguments and compares the state of both after a write (values at the limits, the duplicate key against a literal and against the stored names, no such table, no such column, a syntax error with arguments), compares the count of changes, checks the wrong number of arguments for every kind of statement, the refusal of a value that is not an integer (text, real, blob, NULL, an empty value, nil), the order text then arguments then instance (with an ended context and on a closed instance), and replay with an empty cache. `TestEngine_annotations` reads the commit objects of the simulated store and finds the arguments in `Statement.parameters`. `TestEngine_events` checks that no value of an argument is in an event of the engine or of the core. `TestBuild` builds a sink with an argument, and two plans with a parameter and no argument give an internal error.

### Why

The pass stays the one gate on what is supported, and the count of parameters belongs to the text, so it comes from the pass. The engine owns the rule that the count must match, so that the driver and every other caller of the engine share it. Binding through a plan node keeps the plan as plain data that says where the argument goes.

### What worked

The oracle and `verify` took the new texts without change: `?0` and `?32767`, which SQLite refuses with `variable number must be between ?1 and ?32766`, are "not supported", which `verify` accepts because SQLite's verdict is not a syntax error. `#1` and `(? ?)` are syntax errors that the parser already gives as SQLite does. The pinned counts of excused texts (`excused`) did not move.

### What didn't work

`go test -short ./internal/sql/stmt/` failed twice on the first run:

- `TestCheck/should_give_the_right_answer_for_a_parameter_in_a_default`: `Expected "s4ql: not supported: DEFAULT (at offset 34)", but got "near "?": syntax error"`. SQLite's grammar takes only a term after DEFAULT, so `default ?` is a syntax error. The case now expects the syntax error.
- `TestGenerate_nearMisses`: `"insert into GLOB (IGNORE) values (?)", a near miss of slice 1, is supported`. The template list of near misses had `values (?)`, which is now supported.

A Python script given inline to Bash with `&&` after it was refused by the sandbox of the worktree ("this command is too complex to verify that it stays inside the worktree"); a script in the scratchpad, run with the worktree as an argument, worked.

### What I learned

`database/sql` calls `NumInput` of a prepared statement and, if it is not -1, refuses another number of arguments itself with `sql: expected %d arguments, got %d`, before the driver sees the call. It also calls the driver's `CheckNamedValue` before anything else, and an error there comes before the text is judged. Both shape the driver in the next step.

### What was tricky

Where the count belongs. The driver must give the error of the text before that of the arguments, and must convert Go values before the engine sees them. The engine checks the count itself, and exposes `Check(src, n)` so that the driver can give the text and the count first on the path where an argument cannot be converted, without parsing twice on the path where all is well.

### What warrants review

- `parameter` and the new `case *ast.Variable` in `integer` in `/internal/sql/stmt/recognize.go`: the offsets of the rejects, and that `isNext` guards each.
- `prepare` and `Check` in `/internal/engine/engine.go`, and `checkArguments` and `countArguments` in `/internal/engine/errors.go`: the order of the checks, and the wording `s4ql: wrong number of arguments: 2 for a statement with 1 parameter`.
- The new outcome `wrong_arguments` in `/internal/engine/telemetry.go`.

### Future work

The generators `stmttest` and the differential test of the driver do not make arguments yet; the lead gives that to a later builder.

## Step 2: The driver, the differential test of the driver, and the documents

**Author:** slice 2 builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Tests of the public driver through `database/sql`: direct and prepared, the wrong number of arguments (too few, too many, on a text without `?`), each rejected type, `sql.Named`, the order of errors (syntax error before argument errors), and a restart with an empty cache. [...] A test that reads the commit object and finds the arguments in the statement's annotation. [...] Tests that pinned \"any argument is not supported\" are changed to the new behaviour, not deleted. The differential test in `internal/sqlite/crosscheck/drivercheck` must still agree with its oracle; adjust it only as far as needed for that. [...] `docs/roadmap.md` gets a section \"Slice 2: parameters\" after slice 1 [...] `docs/todo.md`: update the lines that say parameters are not supported, and record every decision of detail you made"
**Interpretation:** Wire the arguments through the driver in `/conn.go`, with the order text, then arguments, then instance; change the tests that pinned "not supported: parameters"; adapt the differential test of the driver; write the roadmap's section and the decisions in the to-do list.
**Inferred intent:** An application can use `?` through `database/sql` with integers, and gets an error of s4ql's for everything else before anything runs.

### What I did

`/conn.go`: `ExecContext` and `QueryContext` call `arguments(query, args)`, which converts each argument with `argument` and, only when one cannot be converted, asks `engine.Check(query, len(args))` first, so that the error of the text and then of the number win over the error of the argument. `CheckNamedValue` now runs `driver.DefaultParameterConverter` and keeps a value that it refuses as `unconverted{value, err}`, which `argument` reports after the text. `prepared` passes its arguments through, and `NumInput` stays -1. The legacy `Exec` and `Query` of a prepared statement built `make([]driver.NamedValue, len(args))`, which dropped the values; they now keep them by position (`named`). `withArguments` is gone. `/errors.go` maps an error that wraps `stmt.ErrNotSupported` without being a `*stmt.NotSupportedError` to `ErrNotSupported`, for the engine's refusal of a value that is not an integer. The package documentation in `/s4ql.go` describes parameters.

Tests in `/conn_test.go`: `TestParameters` (direct and prepared, several runs; every integer type and a `driver.Valuer`; SQLite's errors through a parameter equal to those of a literal; the wrong number of arguments through five calls, without a sentinel; each refused type and `sql.Named` with its exact message; the annotations of the commit objects read from the store with `format.NewCommitReader`, and a restart on an empty cache). The subtest of `TestExec` that pinned "not supported: parameters" now checks that the text is judged first for eight kinds of arguments, also `(? ?)`. `TestPrepare`, `TestConn_Raw`, and `TestStop` pin the new errors; `TestStop` checks that a stopped instance gives the errors of the text and of the arguments and the stop for an insert with a right argument. `/errors_test.go` checks the mapping of the engine's errors of arguments through an export of `publicError`.

`/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: the call "exec with an argument" passes `int64(1)` as before. A statement without a parameter now gets the driver's error of the number, which `checkArguments` checks against `engine.Check` and does not compare with mattn; a statement with one parameter runs and is compared with mattn. The pinned sequences have the new errors, and a few new texts: `(?1)`, `(:v)`, `(-?)`, `(? ?)`, and an insert of a parameter that succeeds and then meets its key. `TestGenerated` required the kind `insert not supported`, which only an argument gave; it now requires `insert wrong number of arguments`. The pinned counts of excused texts (816 and 14) did not move.

Documents: `/docs/roadmap.md` has "Slice 2: parameters"; `/docs/todo.md` has a section "Parameters" with every decision of detail, and the line of the driver points there; the READMEs of `/internal/engine` and `/internal/sql/stmt` describe parameters.

### Why

The driver is where Go values become values of the engine, and where `database/sql`'s own checks (`NumInput`, `CheckNamedValue`) would otherwise answer first with errors that are not s4ql's.

### What worked

`TestParameters` passed on its first run, with every exact message. `go test . -run 'TestParameters|TestExec|TestPrepare|TestConn_Raw|TestStop' -race -count=20` passed. `make test` and `make lint` passed, as did `go build -tags s4ql_browser ./...` and `make check-browser`.

### What didn't work

`go test .` without `-short` fails in `TestOpen_realBucket`: `s3test: the tests against a real bucket cannot run: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are set neither in the environment nor in .../.env`. The worktree has no credentials, and the brief keeps me inside it, so I ran the package with `-skip TestOpen_realBucket`; `make test` runs with `-short`. CI's job with the credentials runs it.

`GOOS=js GOARCH=wasm go build -tags s4ql_browser ./...` fails in `internal/sql/oracle` with `c.DenyProcessPragmas undefined`; that package is not one of the packages built for the browser (`BROWSERPKGS` in `/Makefile`), and it fails the same way without this change. `make check-browser` passes.

### What I learned

`database/sql` hands the driver's `CheckNamedValue` the raw value, and does not call a `driver.Valuer` first when the driver has that method; without the default conversion inside it, an `int` would reach the driver as `int`, not `int64`.

### What was tricky

The error of an argument that cannot be converted must still come after the error of the text, while `database/sql` asks the driver about each argument before the driver sees the text. Keeping the refused value with its error, and reporting it later, solved it without failing in `CheckNamedValue`.

### What warrants review

- `arguments`, `argument`, and `CheckNamedValue` in `/conn.go`: the order of the errors, and the choice to report a failing `driver.Valuer` as "not supported" with its error, which `/docs/todo.md` flags for the lead.
- `checkArguments` and the changed block of `compare` in `/internal/sqlite/crosscheck/drivercheck/drivercheck_test.go`: that nothing that s4ql runs escapes the comparison with mattn.

### Future work

The generators `stmttest` and the sequences of `drivercheck` make no arguments; with them, the differential test would compare an insert of a parameter at random. A simulation workload with parameters, planted bugs, and mutation testing are for a later builder, as the brief says.
