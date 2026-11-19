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
