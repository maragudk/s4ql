# Diary: a commit object that the store can never take

`internal/store/s3store` refuses content above 5 GiB with an error that the store contract does not name, so `internal/core` took it as an unknown outcome and uploaded the commit object again until its context ended. The goal is a commit that fails at once and cleanly when its commit object is too large for the store, with a sentinel in the contract's "not applied" family, a size limit in `simstore` to test it, and the bullet in `docs/todo.md` deleted.

## Step 1: The brief

**Author:** builder

### Prompt Context

**Verbatim prompt:**

> You are a builder for the project s4ql (Go module `maragu.dev/s4ql`). You work in your own isolated git worktree. The lead's branch is `main`; your worktree starts from `origin/main`, which equals `main` (commit `186ccdb`). Check that with `git log -1` and reset onto `main` if it differs. Commit your work on your branch. Do not push and do not open a pull request; the lead does that. Report your branch name at the end.
>
> Read first: `AGENTS.md` (binding rules: `encoding/json/v2`, tests must not depend on timing, the README stays lean), `docs/spec.md`, `docs/todo.md` (the item "The S3 client and S3's checksums", bullet "a commit object above 5 GiB"), `internal/store/store.go`, `internal/store/s3store/README.md`, `internal/core/README.md`, and the commit path in `internal/core/tx.go`.
>
> ## The problem
>
> `internal/store/s3store` refuses an object above 5 GiB with `s3store.ErrTooLarge` before it sends anything. The store contract (`internal/store/store.go`) names four errors that mean "not applied" (`ErrNotFound`, `ErrExists`, `ErrChanged`, `ErrChecksum`); every other error means the outcome is unknown. So `internal/core` takes `ErrTooLarge` as an unknown outcome and tries the upload of the commit object again until its context ends. A transaction that can never be stored should fail at once.
>
> ## What to build
>
> A commit whose commit object the store can never accept because of its size fails cleanly:
>
> - The store contract gets a sentinel for it in the "not applied" family, for example `store.ErrTooLarge`: the store did not apply the write, and sending the same content again will never succeed. Document it in the contract next to the other four. `s3store` returns it (keep one name for the caller to test with `errors.Is`; whether `s3store.ErrTooLarge` stays as a wrapper or goes is your call).
> - `internal/core`: when the upload of the commit object fails with it, `Tx.Commit` returns a definite error that a caller can test with `errors.Is` (for example `core.ErrTooLarge`). It is not a temporary error and not "outcome unknown". Nothing was committed: the manifest is untouched, the cache is unchanged, the spool file is removed, the transaction is over. The instance keeps the lease and keeps serving, and the next transaction commits normally. No retry. The event is logged like any other failed commit (see the log levels in `internal/core/telemetry.go`; a failed commit is Warn; use the context-aware `LogAttrs(ctx, ...)`).
> - Decide what the core does if a manifest swap ever returns the same error (the manifest is tiny, so this is nearly unreachable). Pick the simplest behaviour that is safe, and say in a comment why.
> - `internal/store/simstore` gets a way to set a size limit, so the behaviour can be tested without 5 GiB of data. If a case fits `internal/store/storetest` in a general way, add it; if it does not fit cleanly, leave the suite alone and test `simstore` and `s3store` each in its own tests.
> - Tests: a test of the core that shows the clean failure and that the next transaction works, and a test that would fail if the core fell back to retrying. Check that by breaking the handling once by hand and seeing the test fail. Follow the timing rule in `AGENTS.md`: set up fakes before the code that uses them starts, run new tests with `go test -count=50`, also under `-race`, and run `make test-asan` for anything that waits.
> - Docs: delete the bullet from `docs/todo.md` (built things are deleted from that list), and update `internal/store/s3store/README.md` and `internal/core/README.md` where they describe the old behaviour. Do not change `docs/spec.md`; its wording needs Markus's agreement. If you think the spec should say something about a transaction that is too large, put the proposed sentence in your report instead.
> - Do not touch `README.md`.
>
> Details beyond this are yours to decide. Keep the change small.
>
> Another builder works in parallel on a simulation workload in which a stuck write lands minutes late. It changes how long `simstore` can hold a write in flight, and adds workloads in the tests of `internal/core`. Keep your changes to `simstore` local and small so the two branches merge easily.
>
> ## Diary
>
> Use the diary skill. Create `docs/diary/2026-11-13-commit-too-large.md` and write its first step from this brief before you start, then a step per unit of work. Commit the diary with the code.
>
> ## Before you finish
>
> Run `make lint` and `make test` (look at the `Makefile` for the exact targets, including the simulation linter). Review your own diff against this brief. Commit with short plain messages: code identifiers in backticks with their package (`store.ErrTooLarge`), written through a single-quoted here-doc so the shell does not eat the backticks, and no attribution lines.
>
> ## Report
>
> Your final message is read by the lead, not by Markus. Give: the branch name; what you built and the decisions you took (error names, the manifest case, where the size limit lives in `simstore`); how the tests show it; anything that surprised you; anything you want decided.

**Interpretation:** add `store.ErrTooLarge` to the contract as a fifth "not applied" error, return it from `s3store` and from a size limit in `simstore`, and make `Tx.Commit` stop at the first such refusal of the upload with a definite `core.ErrTooLarge`, leaving the instance serving. Decide and document the manifest case.

**Inferred intent:** a transaction that can never be stored must not hold the writer, the caller, and the store hostage until a context ends, and the caller must be able to tell it apart from a temporary failure and from an unknown outcome.

### What I did

Read the brief, `AGENTS.md`, `docs/todo.md`, `internal/store/store.go`, `internal/store/s3store/s3store.go` and its README, `internal/core/tx.go` (`Tx.Commit`, `Tx.upload`, `Tx.publish`), `internal/core/lease.go` (`refusal`, `DB.send`, `DB.renewLocked`), `internal/core/errors.go`, `internal/core/telemetry.go`, `internal/store/simstore`, and the test helpers of `internal/core` (`world`, `bubble`, `scriptStore`, `committing`).

### Why

To find where the error is classified and what the commit path already does on a failed upload, before changing anything.

### What worked

The commit path already does almost everything that the brief asks for a failed upload: `Tx.Commit` rolls the cache back, the deferred `spool.close` removes the spool file, `Tx.end` releases the writer lock, and the deferred event logs a Warn with outcome `not_committed` through `DB.event`, which calls `LogAttrs(ctx, ...)`. Only `Tx.upload` needs to stop retrying, and say why.

### What didn't work

Nothing yet.

### What I learned

`refusal` in `internal/core/lease.go` is the one place where the core decides that a store error means "not applied". It is used by every manifest write: the swap of a commit or a renewal (`DB.send`), the take, and the release. Taking an error as an unknown outcome is always safe, only slower: the core then asks again.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The sentinel, the limit in `simstore`, and the commit that fails at once

**Author:** builder

### Prompt Context

**Verbatim prompt:** (the brief of step 1)
**Interpretation:** build it, test first.
**Inferred intent:** as in step 1.

### What I did

- `/internal/store/store.go`: `store.ErrTooLarge` as the fifth error that means "not applied", in the package comment, next to the other four, and in the doc comments of `Create` and `Swap`. Its doc says that a write of the same content will never be applied.
- `/internal/store/s3store/s3store.go`: `s3store.ErrTooLarge` is gone, and the client returns `store.ErrTooLarge`, so there is one name to test with `errors.Is`. `MaxSize` stays. The package comment and three other comments that said "the four errors" now say "the errors that mean that the write was not applied". `/internal/store/s3store/s3store_test.go` uses the new name, and its two lists of the "not applied" errors have the fifth.
- `/internal/store/simstore`: `Options.MaxSize`, an `int` of bytes, zero for no limit. `Client.checkSize` refuses a larger create or swap with `store.ErrTooLarge` before it is sent, and traces it. It draws no random number and counts no request, so it changes no seed's trace when it is off. `TestClient_tooLarge` in `client_test.go` shows a refused create and swap, a create at the limit, and that the refused ones are not applied, with faults on that would otherwise make a sent write fail or land late.
- `/internal/core/errors.go`: `core.ErrTooLarge`. `/internal/core/tx.go`: `Tx.upload` returns an error that wraps it at the first `store.ErrTooLarge`, with the store's error as text only. `Tx.Commit` then does what it does for every failed upload already: it rolls the cache back, removes the spool file, ends the transaction, and logs the event at Warn with outcome `not_committed`. The doc comments of `Tx.Commit` and `Tx.upload` name the new error.
- `/internal/core/lease.go`: the manifest case. `refusal` leaves `store.ErrTooLarge` out on purpose, with a comment.
- Tests in `/internal/core/tx_test.go` (`TestTx_Commit_tooLarge`) and `/internal/core/lease_test.go` (`TestLease_tooLarge`).
- Docs: the bullet deleted from `/docs/todo.md`; `/internal/store/s3store/README.md` names `store.ErrTooLarge`; `/internal/core/README.md` has two bullets under "The commit".

### Why

The brief asks for a definite error in the "not applied" family and a commit that does not retry. The commit path already cleaned up after a failed upload, so the change in the core is one `case` in `Tx.upload`.

### What worked

Test first. With `core.ErrTooLarge` declared and the handling not yet written, `go test ./internal/core -run TestTx_Commit_tooLarge -count=1` failed with the old behaviour, retried for the hour of the test's context:

```
tx_test.go:1383: Expected "the commit object is too large for the store" (type *errors.errorString), but got "commit: not committed: upload of the commit object bbe21b35…: context deadline exceeded, after: simstore: 11932 bytes, more than 2048: the content is larger than one write can take" (type *fmt.wrapError)
```

After the fix it passed. Then I broke the handling by hand so that it tried once more before it gave up (`case errors.Is(err, store.ErrTooLarge) && *attempts >= 2:`), and the test failed on the count of uploads, as it should:

```
tx_test.go:1385: Expected "1", but got "2" (type int)
tx_test.go:1385: the upload was sent again
```

The new tests passed with `-count=50`, with `-race -count=50`, and with `-race -count=200 -cpu 1,4` while `make test` loaded the machine. `make lint` reported no issues.

### What didn't work

The first version of the core test upserted one row with an 8 KiB blob, and failed with `invalid: a value of kind blob in the column "v" of the table "t1", whose type is TYPE_INT`: the schema has only `TYPE_INT` so far, and `tableOf` makes integer columns. The test now upserts a thousand integer rows, about 12 KB, against a limit of 2 KiB.

### What I learned

The manifest is a few hundred bytes and a commit object of one small change is smaller, so a size limit alone cannot make a store take the lease and refuse the commit's manifest swap. `TestLease_tooLarge` therefore shows the manifest case on the take: with a limit of 16 bytes the instance keeps asking for the lease, does not stop, and refuses reads with `ErrNoLease`.

### What was tricky

The manifest case. Both ways are safe: taking `store.ErrTooLarge` as a refusal is true, and taking it as of unknown outcome is never wrong, only slower. I chose unknown, because it needs no new code path: a renewal or a commit swap then goes the way that existing tests of lost answers already cover, and a renewal does not spin at `retrySoon` on an "unchanged" manifest, as a refusal would. A stop of the instance was the third choice; it would need a new cause of `ErrStopped` for a case that only a store set up wrong reaches.

### What warrants review

- The comment on `refusal` in `/internal/core/lease.go`, and whether "unknown" is the behaviour the lead wants there.
- `Tx.upload` wraps `core.ErrTooLarge` and not `store.ErrTooLarge`: the store's error is in the text only, as the core does for other store errors that it turns into its own.
- `TestLease_tooLarge` does not tell "unknown" from "refusal" apart; it pins only that the instance asks again and does not stop. Telling them apart would mean counting attempts against the back-off, a detail of timing.

### Future work

None that falls out of this. The spec says nothing of a transaction that is too large; a proposed sentence is in the report to the lead.

## Step 3: Self-review, and what came of it

**Author:** builder

### Prompt Context

**Verbatim prompt:** (the brief of step 1, its section "Before you finish")
**Interpretation:** review the diff with the code-review skill, act on what holds, and run the checks.
**Inferred intent:** hand the lead a branch that is right, not only green.

### What I did

Ran the code-review skill at medium effort on the uncommitted diff. It gave eight findings. I acted on four:

- **S3's own refusal of a large write was not mapped.** A store whose limit is below 5 GiB answers `400 EntityTooLarge`, which the client reported as an unknown outcome, so the core would still have retried until its context ended. `request.refusal` in `/internal/store/s3store/s3store.go` now reports it as `store.ErrTooLarge`, under the same rule of one attempt as the other refusals, and `TestClient_answers` has a row for it on a create and on a swap. `/internal/store/s3store/README.md` says that this rests on AWS's list of error codes alone.
- **The event of a too-large write said `not_sent`,** as for a bad key. `/internal/store/s3store/telemetry.go` has `outcomeTooLarge`, `too_large`, at Warn, for both the client's own check and the store's answer.
- **The model of `simstore`'s tests did not know the fifth error.** `attempt.definite` in `/internal/store/simstore/simstore_test.go` now counts `store.ErrTooLarge`. No workload sets a limit, so this changes no run.
- **The error of the commit repeated itself.** `Tx.upload` now returns `fmt.Errorf("%w: %v", ErrTooLarge, err)`: the store's error already holds the key, with the name, and the sizes.

I did not act on four:

- **The manifest case as a refusal.** The reviewer argued that a commit whose manifest swap is refused as too large should say "not committed", not "outcome unknown". True for commits, but a refusal makes a renewal and a take loop at `retrySoon`, with a read and a swap each time, for as long as the store is set up wrong. Unknown backs off. The case needs a store that refuses a few hundred bytes. I kept unknown, and left the question for the lead.
- **`remaining` in `s3store` passing a sentinel through.** It does not: it formats the errors of `Seek` with `%v`.
- **A case in `storetest`.** The suite does not know a store's limit, so a general case would need a new parameter of the harness, which the brief said to avoid unless it fits cleanly.
- **`simstore` reads the content before it checks its size.** It is a test store, and moving the check into `readContent` would touch more of `simstore` than the brief wants, with another builder changing it.

### Why

The first finding was a real gap: without it, the change worked only for the client's own limit of 5 GiB, not for a store whose limit is lower.

### What worked

`make test` passed (core 55 s), and `make lint` reported no issues, before and after the fixes. `make test-asan FUZZTIME=1s` passed in Docker on the code before the review fixes, which do not touch any code that waits. The new tests of the core passed again with `-race -count=50`.

### What didn't work

Nothing failed in this step.

### What I learned

The reviewer's first finding is the kind that a test against the fake cannot find unless someone thinks of the code: no store was seen to send `EntityTooLarge`.

### What was tricky

Deciding which findings to leave. The manifest case has two safe answers, and each is worse at something.

### What warrants review

- The new row of `TestClient_answers` and the `EntityTooLarge` case of `request.refusal`.
- Whether `too_large` at Warn is the level the lead wants for a write refused as too large.

### Future work

If a store is ever seen to send `EntityTooLarge`, the README line on it can say so.
