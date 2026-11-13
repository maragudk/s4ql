# Diary: local files that fail on demand

Mutation testing of `internal/core` changed six decisions of the core without any test failing. Four of them need a local file system that fails in the middle of an operation: a spool file that fails during an upload, a local failure at the end of a read, a failure of the cache during replay, and a spool file whose length cannot be read. The spec says what a local I/O failure does to an instance, and no test shows that the core does it in those four places. This task gives the tests a way to make local file operations fail at a chosen operation and moment, and tests that reach each of the four decisions.

## Step 1: The brief

**Author:** builder (failing file system)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements; the full brief is in the lead's conversation)

> Mutation testing of `internal/core` changed six decisions without any test failing. Four of them need a local file system that fails in the middle of an operation: 1. a spool file that fails during an upload; 2. a local failure at the end of a read; 3. a failure of the cache during replay; 4. a spool file whose length cannot be read. No test can make that happen today, so the code that handles it is not shown to work. The spec says what must happen on a local I/O failure; the tests should show it.
>
> - A way for a test to make local file operations fail on demand, at a chosen operation and a chosen moment, with no dependence on timing and with the same result on every machine and under the simulation harness.
> - For the spool file, which is plain Go file I/O: a small seam that only tests can reach, in the manner of the unexported `Options.bugs`. Production behaviour and the production API do not change.
> - For the cache, which SQLite reads and writes: find out what a clean way is. Candidates: a fault-injecting VFS shim in C next to the binding; real failures of a real file system. [...] prefer something deterministic. Constraints: a production build must not behave differently, and must not carry test machinery that can be switched on by accident. If a clean way exists that is small, build it and cover decisions 2 and 3. If the only clean way is large (for example a full VFS wrapper with many methods), build what covers decisions 1 and 4, and write up the route for the cache [...] without building it.
> - Tests that reach each decision you cover and assert what the spec and `internal/core/README.md` say must happen [...].
> - For each covered decision, apply the mutation that the mutation testing applied [...], see the new test fail, and restore the code. Say which test kills which mutant.
> - If a test shows that the code does the wrong thing: when the fix is clear, small, and inside what the spec says, fix it with the test and put it first in your report. When a fix would change the design or the spec, stop and report what you saw.
> - Keep the run time of `go test ./internal/core/` about where it is [...].
> - Docs: update the list in `internal/core/README.md` and the bullet in `docs/todo.md` to what is still unreachable. Do not change `docs/spec.md` or `README.md`.

**Interpretation:** a test-only seam on the spool file for decisions 1 and 4; for the cache, a judgement on whether a fault-injecting SQLite VFS is small and clean enough to build, and if so tests for decisions 2 and 3 through it; for every covered decision, the mutant from the earlier run applied by hand and shown to fail the new test.

**Inferred intent:** the core's handling of local failures is part of the claim of correctness, and a claim that rests on code no test reaches is not trusted. Markus's concern is trust in correctness.

### What I did

Reset the worktree onto `worktree-agent-a8569e9b46c707813` and merged `origin/main`. Read `AGENTS.md`, `docs/spec.md`, the todo item, `internal/core/README.md`, `spool.go`, `cache.go`, `replay.go`, `read.go`, `tx.go`, `catchup.go`, `errors.go`, the binding and its README, and Step 8 of `/docs/diary/2026-11-12-storage-core.md`, which names the four surviving mutants: `upload-spool-error-retried`, `read-end-swallows-local`, `replay-cache-error-as-broken`, and `spool-length-error-ends`. The mutation runner and its lists were in another session's scratchpad and are gone, so each mutant is rebuilt here from its name and the code. Started a measurement of `go test -count=1 ./internal/core/` as the baseline.

### Why

The run time is a hard limit in CI, so the baseline comes before any change, and the mutants decide whether a test reaches its decision.

### What worked

The four decisions map to four lines: `case content.err != nil` in `Tx.upload`, the `classify(err) == kindOther` check after the rollback in `ReadTx.end`, the `classify(err) != kindOther` check after `apply` in `replay`, and the non-EOF error of `binary.ReadUvarint` in `spool.annotations`.

### What didn't work

Nothing yet.

### What I learned

The tests already inject SQLite errors in one place: `Tx.commitCache` returns a made-up `SQLITE_IOERR` under buggify. Every other local failure in the tests comes from a real file: a directory without write permission, or a cache file that is damaged on disk.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: A seam on the spool file, and decisions 1 and 4

**Author:** builder (failing file system)

### Prompt Context

**Verbatim prompt:** "For the spool file, which is plain Go file I/O: a small seam that only tests can reach, in the manner of the unexported `Options.bugs`. Production behaviour and the production API do not change."
**Interpretation:** an unexported field of `Options` that wraps each spool file, set only from `export_test.go`, and tests that fail one read of the spool file at the two places the mutants changed.
**Inferred intent:** decisions 1 and 4 are shown to work, and nothing in production can switch the seam on.

### What I did

Wrote the two tests first, in `TestDB_stop` in `/internal/core/core_test.go`, next to the tests for a spool file that cannot be written, and the fault helper `spoolFaults` in `/internal/core/spool_test.go`: a wrapper of the `*os.File` that fails the next `n` reads, writes, or seeks of a spool file of one kind with `syscall.EIO`. The build failed as it should: `o.WrapSpools undefined (type *core.Options has no field or method WrapSpools)`.

Then the seam. `spool.f` is now a `spoolFile` (`io.ReadWriteSeeker` and `io.Closer`), and `newSpool` became `DB.newSpool(kind)`, which wraps the file with `Options.local.spool` when that is set. `local` is a new unexported struct of `Options` in `/internal/core/core.go`, which will also hold the name of the cache's VFS. `export_test.go` sets it with `Options.WrapSpools`.

Then the mutants, each a copy of one file through `go test -overlay`, in the session's scratchpad:

- decision 1, `upload-spool-error-retried`: `case content.err != nil:` in `Tx.upload` became `case content.err != nil && false:`. The upload then goes on to the store's error, sleeps, and tries again, and the second read of the spool file succeeds. Killed by "should stop for a failure to read the spool file during the upload, and not try the upload again": `core_test.go:662: Expected "the instance has stopped" (type *errors.errorString), but got "<nil>"`.
- decision 4, `spool-length-error-ends`: `if errors.Is(err, io.EOF) {` after `binary.ReadUvarint` in `spool.annotations` became `if errors.Is(err, io.EOF) || err != nil {`, so a failed read of a length ends the annotations. The commit then goes on without them. Killed by "should stop for a failure to read the length of an annotation from its spool file, and not commit without it": `core_test.go:682: Expected "the instance has stopped" (type *errors.errorString), but got "<nil>"`.

Both tests pass without a mutant.

### Why

The spool file is plain Go, so the seam can be a Go interface. A wrapper fails an operation by counting operations, not by time, so each test gives the same result everywhere.

### What worked

The first read of the spool file of the annotations is the read of the length of the first annotation, because the reader is a `bufio.Reader` and `binary.ReadUvarint` reads its first byte through it. So "fail the first read" reaches decision 4 exactly. The upload of the simulated store reads the whole content before it sends anything, so a failed read leaves no object in the store.

### What didn't work

My first mutant of decision 4, `if err != nil {` in place of `if errors.Is(err, io.EOF) {`, did not build: `spool4.go:6:2: "errors" imported and not used`. Step 8 of the core's diary warned of exactly this. `|| err != nil` keeps the import.

### What I learned

The tests assert the failure count of the fault helper (`faults.failures()` is 1), so a test whose fault never fires does not pass by accident.

### What was tricky

Choosing a one-shot fault for decision 1. A fault that fails every read makes the mutant retry until the context ends, and a test without a deadline hangs. With one failure, the mutant's second upload succeeds and its commit returns nil, which the test reports at once.

### What warrants review

That `Options.local` cannot be set from outside the package: it is unexported, and `WrapSpools` is in `export_test.go`. The production path is one nil check per spool file.

### Future work

None.
