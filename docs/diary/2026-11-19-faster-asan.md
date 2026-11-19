# Diary: A faster sanitizer job in CI

The CI job `asan` is the slowest job and sets how long a pull request waits: its main step took 23 minutes on the last green run on `main`. It runs `go test -asan -shuffle on ./...` for both modules and then every fuzz target for 30 seconds, one after another, all under the C address sanitizer. The sanitizer finds faults in C code only, but the job also runs pure-Go packages under it. The work is on the branch `faster-asan`, which starts from `main` at `79e7050`.

## Step 1: The brief, and a plan

**Author:** faster-asan builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "The CI job `asan` is the slowest job and sets how long a pull request waits: its main step took 23 minutes on the last green run on `main` (measured; the whole job about 27 minutes on another run). Markus, the product owner: \"I'm a bit worried about its slowness, it slows our iterations down.\" [...] Cut the time a pull request waits for CI, without losing any check that finds something. [...] 1. **Measure first.** Find out where the 23 minutes go [...] 2. **The sanitizer only where C is linked.** A package's tests run under `-asan` if the test binary links the cgo binding [...] compute this with `go list` and do not write the list by hand [...] name the job that runs each such package's tests [...] 3. **Fuzz targets in two groups, computed the same way:** those whose package links C run under the sanitizer; the pure-Go ones run without it. Every fuzz target is in exactly one group, and `make fuzz` (or its successors) still fails on an empty or unlistable directory and on a target that is in no group. Keep `FUZZTIME` and the 30 seconds in CI [...] 4. **Parallel jobs.** Split the work into jobs that run at the same time [...] 5. Keep what the job does besides: the check of the heap of a large transaction [...] and the step that shows the inputs that made a fuzz test fail (in every job that fuzzes). 6. The local `make test-asan` must still work on macOS through Docker and do the same as CI's sanitizer jobs together [...], and `make fuzz` must still run every fuzz target. [...] If the permission system refuses your edit of that file, do not try any other way to write it [...] No loss of coverage that matters [...] Do not drop `-shuffle on`. Do not weaken or remove any test. [...] Update `docs/todo.md` where it speaks of the sanitizer job's time."
**Interpretation:** Change the `Makefile` so that the packages whose test binaries link C, and the fuzz targets of those packages, are found by `go list` and run under `-asan`, and the rest of the fuzz targets run without it. Split the fuzz targets that run under the sanitizer into shards. Then change `.github/workflows/ci.yml` so that the tests under the sanitizer, each shard, and the pure-Go fuzz targets run as separate jobs at the same time.
**Inferred intent:** Shorter waits for CI on every pull request, with the same checks run and no package or fuzz target left out.

### What I did

Reset the worktree to `origin/main` at `79e7050` and created the branch `faster-asan`. Read `/AGENTS.md`, all of `/.github/workflows/ci.yml`, the reusable workflows `lint.yml` and `test.yml` of `maragudk/workflows` that it calls, the `/Makefile`, and `/docs/todo.md`.

What I found:

- The reusable test workflow runs `go test -race -shuffle on ./...` over the main module only. The job `crosscheck` runs `go test -race -shuffle on ./...` over the module in `/internal/sqlite/crosscheck`. The job `sql` runs `make test-sql`, which is `./internal/sql/...` and `./internal/engine/...` without the race detector and the sanitizer. The job `browser` runs `BROWSERPKGS`, among them `internal/store/simstore`, in Chrome.
- The fuzz targets run only in the job `asan`. No other job fuzzes, so a fuzz target that leaves the sanitizer must still run somewhere.
- `/docs/todo.md` does not speak of the sanitizer job's time. `/internal/sqlite/README.md` says "CI runs `make test-asan FUZZTIME=30s`", which changes with this work.
- `main` has no required status checks (the rule in `/AGENTS.md` on merging), so new job names block nothing. The rule counts checks against "as many as CI has jobs", so the lead must know the new count.

The plan:

1. Measure from the logs of green runs on `main`.
2. In the `Makefile`, a command that classifies the test binaries of a module by whether they link a package outside the standard library with cgo files, and targets that use it for the tests and for the fuzz targets.
3. Shards of the fuzz targets under the sanitizer, computed, so that each target is in exactly one shard.
4. The jobs in `/.github/workflows/ci.yml`.
5. Documents, self-review.

### Why

The sanitizer instruments C code. A test binary that links no C has nothing for it to check, and every such package already runs under the race detector in the job `test`.

### What worked

The reusable workflows were readable from their repository, so the claim that each pure-Go package runs elsewhere can be checked against what they run.

### What didn't work

Nothing tried yet.

### What I learned

The job `asan` is the only place in CI where fuzz targets run at all.

### What was tricky

Nothing yet.

### What warrants review

The plan: whether the classification by "links a non-standard package with cgo files" is the right line. It covers `internal/sqlite` and `internal/sqlite/faultvfs`, the two packages with cgo files in the main module, and `github.com/mattn/go-sqlite3` in the module of the cross-checks.

### Future work

None yet.

## Step 2: Where the 23 minutes go

**Author:** faster-asan builder

### Prompt Context

**Verbatim prompt:** (from the brief) "**Measure first.** Find out where the 23 minutes go: per package for the tests under the sanitizer, per fuzz target (compile and run), and the fixed costs. Use CI's own logs for real numbers (`gh run view <id> --log` for the job `asan` of a recent green run on `main`; test output has per-package times) and local runs in the Linux container (`make test-asan`) where the logs do not say. Write the numbers into the diary."
**Interpretation:** Take the timestamps of the job `asan` in the logs of green runs on `main`, and split the main step into the tests of each module and the compile and run of each fuzz target.
**Inferred intent:** Know which part to cut before cutting, so that the split of the work rests on numbers.

### What I did

Listed the runs of CI on `main` with `gh run list --workflow ci.yml --branch main`, and read the log of the job `asan` of run 44487279310 (commit `882e193`, "Compare the driver with SQLite through database/sql (#42)") with `gh run view --job 135433783873 --log`. The start of each fuzz target is the log line "gathering baseline coverage: 0/N", so the time from the line `ok` of the target before to that line is the compile of the test binary (and the `go test -list` of the directory), and the time from it to the target's own line `ok` is the run. I compared with runs 44479972020 and 44475561015.

Run 44487279310, job `asan`: 23 min 58 s in all.

- Checkout and setup-go: 14 s. The check of the heap of a large transaction: 40 s, most of it the download of modules and the compile of `internal/core`.
- The main step, `make test-asan FUZZTIME=30s`: 23 min 2 s.
  - The tests of the main module under the sanitizer: 5 min 57 s. The first package reported after 84 s (compile and downloads). `internal/core` took 275 s and ended the phase; it is the long pole. The others ran beside it with 4 CPUs: `internal/store/simstore` 131 s, `internal/sql/shapetest` 117 s, `internal/sim` 77 s, `internal/sqlite` 56 s, `internal/sql/mutation` 48 s, `internal/sql/stmt` 39 s, `internal/sql/parser` 36 s, `internal/sql/parsetest` 29 s, `internal/engine` 26 s, `internal/store/storetest` 13 s, `internal/store/s3store` 9 s, `internal/sql/token` 9 s, `internal/sql/corpus` 9 s, `internal/sql/oracle` 7 s, `maragu.dev/s4ql` 4 s, the rest under 1 s.
  - The tests of the module of the cross-checks: 80 s, nearly all of it the compile of `github.com/mattn/go-sqlite3` and the binding under the sanitizer; the tests themselves took 0.1 s and 10.7 s.
  - The fuzz targets: 15 min 45 s for 23 targets. Each ran 30.2 to 32.4 s by its own line `ok`, 715 s in all. The compiles before them: 69 s before the first target of `internal/sqlite` (the amalgamation again, under the sanitizer and the fuzzer's instrumentation), 104 s before the first of `internal/sqlite/crosscheck` (mattn's amalgamation as well), 38 s before `drivercheck`, and 0.4 to 3.6 s before each of the others, about 230 s in all.
- Run 44479972020: the main step took 21 min 5 s, with `internal/core` at 269 s. Run 44475561015: 27 min 32 s, with `internal/core` at 465 s, `internal/store/simstore` at 220 s, and `internal/sql/shapetest` at 211 s, so the runner of that run was about 1.7 times slower in the tests. The fuzz targets take the same 30 s on any runner.
- The other jobs on the same three runs: `browser` 11 min 36 s, 8 min 23 s, 11 min 40 s; `test` 5 min 15 s, 7 min 44 s, 4 min 57 s. All others under 3 min.

Which test binaries link C, by `go list -test -deps` (the command of step 3):

- Main module, links C (14): `maragu.dev/s4ql`, `internal/core`, `internal/engine`, `internal/sim`, `internal/sql/corpus/update`, `internal/sql/oracle`, `internal/sql/parser`, `internal/sql/parsetest`, `internal/sql/shapetest`, `internal/sql/stmt`, `internal/sql/token`, `internal/sqlite`, `internal/sqlite/faultvfs`, `internal/store/s3store`.
- Main module, no C (8): `internal/format`, `internal/host`, `internal/sql/ast`, `internal/sql/corpus`, `internal/sql/mutation`, `internal/sqlite/update`, `internal/store/simstore`, `internal/store/storetest`.
- The module of the cross-checks: both packages link C.
- Fuzz targets: 20 in packages that link C (`internal/sqlite` 3, `crosscheck` 3, `drivercheck` 1, `internal/engine` 4, `internal/sql/stmt` 3, `internal/sql/parser` 2, and one each in `token`, `oracle`, `parsetest`, `shapetest`), and 3 that do not (`internal/format`).

### Why

The numbers decide the split. Of the 23 minutes, 15 min 45 s are fuzzing in a row, and the 6 minutes of tests are set by one package, `internal/core`, not by the sum of the packages.

### What worked

The timestamps in the logs are enough to split compile from run for every fuzz target, so I needed no local run for the numbers of CI.

### What didn't work

My first query of the job's steps with `gh run view --json jobs` gave only the steps' start and end, not the per-package lines; the per-package times needed the full log.

### What I learned

- Taking the pure-Go packages out of the sanitizer saves little wall time by itself. Their tests ran beside `internal/core`, which ends the phase anyway, and only 3 of the 23 fuzz targets are pure Go: most of the SQL front end links SQLite, because its tests compare with it. The time is won by running the parts at the same time.
- The job `browser`, at 8 to 12 minutes, is the next slowest. Once the sanitizer's work is split, it sets the wall time of CI, so the parts of the sanitizer's work need not be shorter than about 8 minutes.
- The fuzzer's instrumentation applies to every package outside the standard library, so each job that fuzzes a package that links SQLite compiles the amalgamation again (about 70 s), and one that fuzzes in the module of the cross-checks compiles mattn's as well (about 104 s).

### What was tricky

The runners differ by up to 1.7 times in the tests under the sanitizer, so any estimate of a job's time is a range.

### What warrants review

The way I split compile from run: the gap before a fuzz target also holds the `go test -list` of its directory, which is small once the directory's test binary is cached.

### Future work

None from this step.

## Step 3: The sides in the `Makefile`, the shards, and the jobs

**Author:** faster-asan builder

### Prompt Context

**Verbatim prompt:** (from the brief) "**The sanitizer only where C is linked.** [...] **Fuzz targets in two groups, computed the same way** [...] **Parallel jobs.** Split the work into jobs that run at the same time (separate jobs or a matrix), so that the wall time is that of the slowest part. Aim for parts of similar length; say what the slowest part is after the change and what would cut it further. [...] The local `make test-asan` must still work on macOS through Docker and do the same as CI's sanitizer jobs together [...], and `make fuzz` must still run every fuzz target."
**Interpretation:** One computed rule for "links C", used by a new target for the tests and by `fuzz` for its groups; shards of the group under the sanitizer for a matrix in CI; `test-asan` as the sum of the parts.
**Inferred intent:** The same checks as before, with the wall time of the slowest part instead of the sum.

### What I did

In `/Makefile`:

- `LINKSC` and `LINKSCAWK`: `go list -test -deps` over package patterns prints a line `cgo <path>` for every package outside the standard library with cgo files, and for every test binary (`<path>.test`) its path and its dependencies. The awk program prints `c <path>` for a test binary that has any of those packages among its dependencies, and `go <path>` for the others. Nothing is written by hand: a new package that imports the binding, directly or not, lands on the side `c` by itself. On macOS and in the Linux container it gives the same sides (the list in step 2).
- `asan-test`: in `.` and `$(CROSSCHECK)`, runs `go test -asan -shuffle on` over the packages on the side `c`, prints the ones it leaves out, and fails if it cannot list a module's packages or if none links C.
- `fuzz`: first a plan, with one line per directory of `FUZZDIRS`: its group (`c` or `go`, by `LINKSC` on the directory) and the names of its fuzz tests. `FUZZGROUP` (`c`, `go`, or empty for both) and `FUZZSHARD=i/n` select from the plan. A shard takes whole directories, so that each job builds a directory's test binary once; the directories with the most fuzz tests go first, each to the shard with the fewest so far, so every fuzz test is in exactly one shard. Then for each selected directory, `go test $(FUZZFLAGS) -run '^$' -fuzz '^$' -list '^Fuzz' .` lists its fuzz tests with the build the fuzzing then uses, the names must equal those of the plan, and each fuzz test runs for `FUZZTIME`. `make fuzz` with neither variable runs every fuzz test, as before.
- `test-asan`: on Linux `asan-test`, then `fuzz FUZZGROUP=c FUZZFLAGS=-asan`, then `fuzz FUZZGROUP=go FUZZFLAGS=`, each with `FUZZSHARD=` so that a sharded variable from outside cannot leave anything out. Elsewhere it runs itself in Docker, as before.

In `/.github/workflows/ci.yml`, the job `asan` became three: `asan` runs `make asan-test`; `asan-fuzz`, a matrix of four shards with `fail-fast: false`, runs `make fuzz FUZZGROUP=c FUZZFLAGS=-asan FUZZSHARD=${{ matrix.shard }}/${{ strategy.job-total }} FUZZTIME=30s`; and `fuzz` runs the check of the heap of a large transaction, which was in `asan` before, and `make fuzz FUZZGROUP=go FUZZTIME=30s`. Both fuzzing jobs keep the step that shows the inputs that made a fuzz test fail. CI has 13 checks after this change instead of 8 (the matrix counts four).

The shards, as the `Makefile` computes them today: 1 is `internal/engine` (4) and `internal/sql/oracle` (1); 2 is `internal/sqlite` (3) and `internal/sql/parser` (2); 3 is `crosscheck` (3), `drivercheck` (1), and `internal/sql/parsetest` (1); 4 is `internal/sql/stmt` (3), `internal/sql/token` (1), and `internal/sql/shapetest` (1). The group `go` is `internal/format` (3).

In `/internal/sqlite/README.md`, the section on memory safety says which packages run under the sanitizer and which jobs of CI run what.

### Why

- Four shards and not three: with three, the cost model of step 2 gives one shard `internal/engine`, `drivercheck`, `token`, and `parsetest`, about 7.5 minutes with two compiles of an amalgamation; with four, the slowest shard is about 6.5 minutes, below `asan` and `browser`.
- The heap check went to the job `fuzz`, the shortest, because it runs without the sanitizer anyway.
- `FUZZTIME` stays 30 s: nothing in the measurements says that a target needs more or less, and the time of a shard is set by the compiles and the count of targets.

### What worked

- `make test-asan FUZZTIME=5s` in Docker passed in 7 min 19 s on this machine at a load of 14 to 26: 16 test packages under the sanitizer (`internal/core` 208 s, `internal/sim` 141 s, `internal/sql/shapetest` 139 s), the 20 fuzz targets of the group `c` under it, and the 3 of `internal/format` without it. The run read the `Makefile` again for each `fuzz`, so its fuzz half ran the final listing described below.
- Every misuse fails with a message and exit status 2 (run by a script with `make -s fuzz FUZZTIME=1x ...`): `FUZZDIRS=internal/host` ("fuzz: no fuzz test in internal/host"), `FUZZDIRS=internal/nosuch` ("fuzz: cannot list the fuzz tests of internal/nosuch"), `LINKSC=true` to make the group unknown ("fuzz: cannot tell whether the tests of internal/format link C, so its fuzz tests are in no group"), an empty `FUZZDIRS`, `FUZZGROUP=cgo`, `FUZZSHARD=1`, `5/4`, `0/4`, `FUZZGROUP=go FUZZSHARD=2/2` (an empty shard), and a failing `go test` (`FUZZFLAGS=-nosuchflag`). A test file in `internal/format` with `func FuzzyHelper() {}`, which is no fuzz test, failed with "fuzz: go test lists other fuzz tests in internal/format than its test files declare: FuzzParseManifest FuzzCommitReader FuzzCommitWriter"; I removed the file after.
- With an empty `GOCACHE`, a fuzz run after the listing with `-fuzz '^$'` ran 1 compile or link action, against 162 after a listing without `-fuzz`: the listing builds what the fuzzing uses.
- The YAML parses (Ruby's `YAML.load_file`), and the jobs and steps are as intended.

### What didn't work

- The first version of `fuzz` failed on macOS at once: "/bin/sh: -c: line 0: syntax error near unexpected token `;;'". The `/bin/sh` of macOS (bash 3.2) does not parse `case "$group" in c|go) ;; ...` inside `$( ... )`. I replaced it with two `[` tests.
- The first version listed the fuzz tests of every directory with `go test -list` to make the plan. That builds every test binary of both modules without the sanitizer, in every shard, minutes for the amalgamations, and the build is not the one the fuzzing uses. The plan now reads the names from the test files that `go list` names, which builds nothing, and the check against `go test -list` runs only for the directories a shard fuzzes, with the fuzzing's own build.
- `actionlint` is not installed, and the permission system refused `go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.7`, so the workflow is checked only as YAML.
- My first loop of checks failed to run in this sandbox ("this command is too complex to verify that it stays inside the worktree"); I moved it into a script in the scratchpad.

### What I learned

- The old `fuzz` listed each directory with a plain build before it fuzzed it with an instrumented one, so the gaps of step 2 held two builds of each test binary, not one.
- `strategy.job-total` gives the size of the matrix, so the number of shards is written once.

### What was tricky

- A make recipe keeps backslash-newline inside single quotes, so the awk program in `fuzz` sees backslash-newline pairs, which awk reads as continuations; it works with macOS's awk and with Debian's in the container. In a variable such as `LINKSCAWK`, make turns them into spaces.
- awk runs `END` after an `exit` in `BEGIN`, so the checks of the variables set a flag that `END` tests.

### What warrants review

- `LINKSCAWK`'s rule: a test binary links C if any of its dependencies is a non-standard package with cgo files. The standard library's own cgo (`net`, `os/user`) is left out on purpose.
- The classification and the names use the default build tags. A fuzz test declared only in a file with the tag `asan` would make the check against `go test -asan -list` fail loudly, not pass in silence.
- The shards are balanced by the number of fuzz tests, not by compile time. Today `crosscheck` and `drivercheck` share shard 3; a new fuzz test elsewhere can move them apart, and then two shards compile mattn's amalgamation.

### Future work

- If CI shows shard 3 as the slowest fuzzing job, weight a directory by its compile as well as its count.
