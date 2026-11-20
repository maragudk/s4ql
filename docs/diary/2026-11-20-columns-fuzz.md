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
