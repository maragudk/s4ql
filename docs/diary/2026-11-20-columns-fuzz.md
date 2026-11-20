# Diary: Fuzz targets of several columns

The engine and the driver run the several columns of slice 5, the NULL of slice 6, and the WHERE of slice 7, and the generated tests compare them with SQLite. The fuzz targets of the engine were written before or beside tables of several columns: `FuzzTypes` and `FuzzTypes_core` insert one value into tables of one or two columns, and `FuzzWhere` and `FuzzWhere_core` read a table of two columns of one type. No fuzz target lets its input choose the shape of a table of several columns, its lists of columns, and its NULLs. This task adds such targets, tests only. The branch is `columns-fuzz`, from `origin/main` at `402f8b1`.

## Step 1: The requirements

**Author:** columns-fuzz builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "**Fuzz targets of several columns.** The fuzz targets that exist (`FuzzTypes`, `FuzzTypes_core`, `FuzzWhere`, `FuzzWhere_core`, and those of the statement package) were written before or beside tables of several columns and `null`. [...] add what the todo item asks for: fuzz targets whose input drives a table of several columns (with `not null` and nullable columns, a column list on `insert`, named columns on `select`, `null` values) through the engine and through core, compared with SQLite as the existing targets compare. Include a seed corpus. Add them to whatever list CI's fuzz job reads [...] Test code and test helpers only. If a fuzz target finds a real difference from SQLite, do not fix the engine [...] Do not weaken a comparison to make a target pass." A second item of the brief, five nits in the test code of pull request #79, was dropped by the lead during the run: "they were never recorded in the todo and I no longer have them verbatim, so they are not part of your task."
**Interpretation:** A pair of fuzz targets in the engine's tests, on the database in memory and on the storage core, whose bytes choose a table of two to four columns of mixed types, which take NULL or not, with the key at any position, and then a run of inserts and selects on it, every call compared with SQLite through `pair.step`, and the tables compared at the end and after a replay of the log.
**Inferred intent:** The fuzzer can search the space of shapes and lists of columns that the generated tests only sample, at the lowered length limit too, where a record of several values is too long.

### What I did

- Read `docs/todo.md`, the diaries `2026-11-20-types-mutation.md` and `2026-11-20-where-checks.md`, the fuzz targets in `/internal/engine/differential_test.go`, and the target `fuzz` of the `Makefile`. `make fuzz` reads the names of the fuzz tests from the test files of each directory of `FUZZDIRS`, which holds `internal/engine`, so a new target there is in CI's fuzz job without a change to a workflow file.

### Why

The brief asks for the diary's first step from the requirements, before the code.

### What worked

### What didn't work

- The five nits of #79 were not in `docs/todo.md` on `origin/main`; the diary `2026-11-20-columns-null-mutation.md` says they were only in that builder's report. I asked the lead, who dropped the item.

### What I learned

### What was tricky

### What warrants review

### Future work

## Step 2: `FuzzColumns` and `FuzzColumns_core`, their seeds, the review, and the runs

**Author:** columns-fuzz builder

### Prompt Context

**Verbatim prompt:** (the brief, as in step 1) "fuzz targets whose input drives a table of several columns (with `not null` and nullable columns, a column list on `insert`, named columns on `select`, `null` values) through the engine and through core, compared with SQLite as the existing targets compare. Include a seed corpus."
**Interpretation:** As in step 1.
**Inferred intent:** As in step 1.

### What I did

- `/internal/engine/columns_test.go`, a new file, so that no other builder's file changes: `FuzzColumns` (database in memory) and `FuzzColumns_core` (storage core, length limit 1001 to 1255 or the default, then a replay of the log into an empty cache). `columnsCalls` reads the shape (two to four columns `c0` to `c3`, types by the low three bits of a byte, NOT NULL or NULL per column, NOT NULL on the key before, after, or not at all, both orders of the options) and up to 16 inserts and selects. An insert has no list, the table's order, a shuffled list, or a shuffled part; one of four faults (a value more, a value fewer, `x` in the list, a name twice, which is the only call marked not supported); and values from `columnsValue`: a pool of four values per type so that keys meet, a value from the bytes (`whereValue`), NULL, another type, text or a blob of 5 times a byte, an edge (`whereRows`, -0.0 too), or one of `stmttest.Unsupported`, each as a literal or an argument. A select is `*` or one to four names (upper case, double quotes, `x`), on `t` or `T`, with up to three conditions. Every call goes through `pair.step`, so arguments, types, lengths, and both ways of reading are checked as in `TestDifferential`.
- The seeds: a small encoder (`columnsSeedShape`) writes the bytes for five shapes, each with four rows and then one group: lists with NULL, faults, a value of another type in each column with selects of names, WHEREs on each column, and long values at the limit 1001 (for the four shapes with a TEXT or BLOB column). Plus 40 inputs of random bytes from `rand.NewPCG(5, 6)`, half at the limit. No corpus files are committed, as for the other targets of the package.
- A throwaway test (deleted) ran the seeds with a tally: they reach NULL into a column that takes it (literal and argument) and succeed, inserts that leave such a column out, NULL into the key and into a NOT NULL column, every list form, every fault, a value of another type in the key and elsewhere, `string or blob too big` for values and records of TEXT, BLOB, and INT keys, selects of `*` with NULL in the rows, names in another order, a name twice, `x` with and without quotes, and every operator on the key and on other columns.
- A writer sub-agent added a paragraph and two rows of results to `/internal/engine/README.md`, and in `/docs/todo.md` changed the status line of slice 5 and replaced the item of fuzz targets of slice 5 by its one fact that stays open: the core's own model and simulation tests drive `Tx.Upsert`, not `Tx.Insert`.
- `make fuzz` reads the names of the fuzz tests from the test files of `FUZZDIRS`, which has `internal/engine`, so CI's fuzz job runs the new targets without a change to the `Makefile` or a workflow file. They link C, so they are in the group `c`, which runs under the address sanitizer in CI.

### Why

The generated tests sample shapes and lists from the sequence's own rates; a target that reads them from bytes lets the fuzzer search the corners, and its core variant puts records of several long values at the lowered limit.

### What worked

- No fuzz run found a difference from SQLite. Before the review: `FuzzColumns` 617,964 executions in 3 min on 10 workers, `FuzzColumns_core` 4,960 in 3 min. On the final code: `go test -run '^$' -fuzz '^FuzzColumns$' -fuzztime 180s ./internal/engine` 623,602 executions, and `FuzzColumns_core` with `-fuzztime 240s` 18,831. The core target stands at 0 executions per second for stretches, as `FuzzTypes_core` does on `main`.
- The seeds pass with `-count=20`, with `-race -count=10`, and under `-asan` in the Linux container (seeds, and 30 s of each target). `make test`, `make lint`, and `make check-browser` pass.
- Printing the calls that the seeds make, before trusting them, which found the next two faults at once.

### What didn't work

- My first seeds wrote no byte for the order of the options, so every op was read one byte late, and the type of a column was its byte modulo 5, so the flag 8 for NULL changed the type (8 % 5 is TEXT). I made the type the low three bits and wrote the seeds with an encoder instead of by hand.
- Two shell commands with here-docs that wrote throwaway test files were refused: "This agent is isolated in the worktree ..., but this command is too complex to verify that it stays inside the worktree." The Write tool worked.
- A fuzz run whose output went to a file showed nothing for minutes: `go test` buffers in package mode, and the build waited on the review's agents.

### What I learned

- The seeds of a byte-driven target go wrong silently: every input is valid, so a wrong offset gives other, still passing, calls. Only a printout or a tally shows it.

### What was tricky

- Keeping the seed encoder in step with the reader: the order in which `columnsInsert` reads the spelling, the list bytes, the byte of the fault, and the values.

### What warrants review

- The code-review skill (high) found no bug and six nits, all fixed: the names of a select took their column from the whole byte, so the spelling bits moved it (now the low four bits); the shuffled list of the two-column seeds did not move (shuffle byte 0 now); the seeds of the limit did nothing for the shape of four INT columns and only one shape had two long columns (that shape has none now, and a fifth shape of TEXT, an INT key, and BLOB has two); the doc comment of `columnsCalls` left out the bytes that faults read (rewritten); this diary step and the README paragraph were missing.
- The doc comment of `columnsCalls` is the format of the input; check it against `columnsInsert` and `columnsSelect`.

### Future work

- `docs/roadmap.md` line 160 still says that mutation testing and fuzz targets of several columns are to come; the writer noticed it, and the brief keeps the roadmap out of my hands.
- `internal/sql/mutation/run.go` lists `FuzzTypes` and `FuzzTypes_core` in the layer `fuzz seeds`, but not `FuzzWhere` or the new targets, so the runner counts their seeds as hand-written tests. Another builder works in that directory.
- The core's own tests of `Tx.Insert`, as the todo now says.
