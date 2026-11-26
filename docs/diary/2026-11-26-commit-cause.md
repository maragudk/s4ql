# Diary: no error of s4ql wraps the cause of a context's end

The second round of the adversarial review of 2026-11-26 (agy 7.3, `docs/research/adversarial-codebase-2026-11-26.md` on the branch `adversarial-codebase-2`) found that a commit that is not committed wraps the cause of its context, so that with `errgroup.WithContext` it matches another goroutine's `ErrOutcomeUnknown`, SQLite's `*Error`, or `driver.ErrBadConn`. The brief also asked for every other path by which a caller's cause reaches an error that s4ql returns or acts on.

## Step 1: the fix, and the search for other paths

**Author:** builder (commit-cause)

### Prompt Context

**Verbatim prompt:** "1. Bring the test onto your branch, renamed to fit the package's tests, and see it fail at `main`. 2. Fix it: an error that s4ql returns because a context ended matches `context.Canceled` or `context.DeadlineExceeded` (as `ctx.Err()` gives them) and the s4ql error that says what happened to the operation, and never the context's cause. Decide whether the cause's text is still shown in the message without being wrapped (useful to a person, invisible to `errors.Is`), and keep it if it cannot mislead. 3. Find every other place where a context's cause can come into an error that s4ql returns or acts on, in the root package, `internal/engine`, `internal/core`, `internal/sqlite`, and the S3 store: every use of `context.Cause`, and every place that wraps an error received from a call that took the caller's context. [...] The worst form would be s4ql's own code branching on `errors.Is(err, …)` and being steered by a cause the caller chose: look for that first and say plainly what you found. 4. Tests, deterministic, at `-count=50` and under `-race`. `make test` and `make lint`. No long runs. 5. Correct any document that describes the behaviour [...]"
**Interpretation:** fix the commit's error, then audit every source of a caller's cause, with the branching of s4ql's own code first.
**Inferred intent:** no choice of the caller's can make s4ql say something false about an operation, or make s4ql do something it would not do otherwise.

### What I did

The review's test became `TestTx_contextCause` in `/tx_test.go`, with a case for the branch of an unknown outcome and one for the worst form below. `/internal/core/errors.go` has `endedError`, which gives `ctx.Err()` (at first with the cause named in the text, which the self-review below removed), and the three `context.Cause` of `/internal/core/tx.go` use it.

The search found the worst form. A public error of s4ql unwraps to the core's error (`failure.Unwrap` in `/errors.go`), so the error of an instance that stopped wraps `core.ErrStopped` and the cause of the stop: `format.ErrMalformed`, `ErrLogBroken`, or `ErrLocal` with a `*sqlite.Error`. net/http gives `context.Cause` of an ended request (`net/http/transport.go`, Go 1.27.4), and simstore copied that. So when a commit's swap is refused, `publish` reads the manifest with the caller's context (`observe` → `readManifest`), the store's error wraps the cause, and `readManifest` stops the instance for good if the cause matches `format.ErrMalformed`. The test opens a second database whose manifest is not JSON, takes its `ErrStopped` error from `PingContext`, ends a commit's context with it, and at `main` the first instance stopped: "s4ql: commit: not committed: the instance has stopped: s4ql: the instance has stopped: manifest: malformed: …". `inLog`'s `unreadable` and `cacheError`'s `classify` were open to the same, on the same reads.

The fix is at the store, which alone knows which of its sentinels it means: `/internal/store/store.go` now says that no error of a call or of a read of its content wraps the cause, other than `context.Canceled` or `context.DeadlineExceeded`. `s3store.ended` wraps only `ctx.Err()` once the context has ended, with the text of the error; simstore's `ended` gives `ctx.Err()`, or `context.DeadlineExceeded` for a context whose cause is one, which keeps every simulated run as it was, since the simulation's contexts have only `Canceled` and `DeadlineExceeded` as causes. `storetest` has a case for it, which a store that wraps the cause fails (`wrapsCause` in its own test).

The other sources, each with a test: the production host's waits and `sqlite.Stmt.StepContext` give `ctx.Err()`; the engine and the root package call `context.Cause` nowhere and get their errors from the core and the host. The open's `s3store.New(ctx)` loads the SDK's configuration with the caller's context, and nothing branches on its error.

### Why

The core cannot strip a cause from a store's error without losing the store's own sentinels, so the store's contract carries it, as it carries "not applied".

### What worked

The review's three cases failed at `main` as recorded, and the s3store test showed that a failed `Get`, `Create`, or `Swap` of the SDK already carries no cause, while the read of the content does: "s3store: get \"slow-body\": another goroutine: no object at the key, …, after the context ended: context canceled".

### What didn't work

`TestClient_endedContext` in `/internal/store/s3store/s3store_internal_test.go` expected the read of cut content after the end of the context to match `io.ErrUnexpectedEOF`; it now has that only in the text. Nothing reads it: the commit reader takes an error of its source as the source's (`sourceError` in `/internal/format/reader.go`). The tests against a real bucket fail locally without credentials, as they should; they ran with `-short`.

### What I learned

A public error of s4ql is an error of the core in disguise for `errors.Is`, so an application that hands one back as a cause hands the core its own sentinels.

### What was tricky

The simulation tells a timeout from a cancellation by the cause (`/internal/sim/sim.go`), so simstore keeps `DeadlineExceeded` there; a plain `ctx.Err()` would have changed the errors of simulated runs.

### What warrants review

`s3store.ended` and `unknown`, which now nest two `textOf`, and the replacement of the cause's text in `ended`; the guard in `s3store.request.failed`; and the sentence in `/internal/store/store.go`.

### Self-review

Two reviewers through `fabrik:code-review` both found the same steerable path that was left: `s3store.request.failed` took the status and the code of an error answer from the SDK's error with `errors.As`, before `ended` cleaned it. When the end of the context breaks off the body of an error answer, the SDK's error wraps the cause ("failed to copy error response body, %w"), so a cause holding a `smithy.APIError` with `EntityTooLarge`, `PreconditionFailed`, or `NoSuchKey` made a real 400, 412, or 404 whose body broke off into `store.ErrTooLarge`, `ErrChanged`, or `ErrNotFound`. In the core, a manifest that reads as missing is a lost lease. `failed` now records nothing from an error that wraps a cause other than the end's. The first test of it, on the TLS fake, passed with and without the fix, because the cancel came before the client had the headers ("StatusCode: 0"); the test in `TestClient_endedContext` on the `canned` transport, whose body breaks off with the cause, fails without the guard with `s3store: create "k": the content is larger than one write can take`.

Both also found that `endedError` changed the simulation: a context derived from an expired timeout of `internal/sim` reports `Canceled` with the cause `DeadlineExceeded`, which the old `context.Cause` gave as `DeadlineExceeded`. `endedError` and simstore's `ended` now give `DeadlineExceeded` for a context canceled with exactly that cause (`==`, so a caller's cause that only wraps it does not count), which is the same value as before for every context of the simulation. And both read the cause's text in the message as misleading ("not committed: context canceled (cause: s4ql: the outcome of the commit is unknown)"), so the core names no cause, and `s3store.ended` replaces the cause's text with "the end of the context"; `store.Store` now asks that an error not say what the cause says. Comments that named net/http in `store.go` or the simulation's timeouts in simstore were reworded, and the doc comment of `s3store.body` no longer promises `io.ErrUnexpectedEOF` after the end.

Left as they are: the error of an unknown outcome formats its cause with `%v` and does not match `context.Canceled`, as `core.ErrOutcomeUnknown`'s documentation says ("wraps no other error"); and the core still sorts merged errors by `errors.Is` (`readManifest`, `fetch`, `inLog`), so its safety rests on the contract of `store.Store`. Sorting by origin (a parse or the store) would not.

### Future work

The core could tell an error of the store from one of a parse by where it came from, and not by `errors.Is`, which would make it safe also against a store that breaks the new sentence of `store.Store`.

codex 6.4 and agy 7.2, a `driver.Valuer` whose error is `driver.ErrBadConn`, are the same kind of fault through the arguments rather than the context, and stay as the review recorded them.
