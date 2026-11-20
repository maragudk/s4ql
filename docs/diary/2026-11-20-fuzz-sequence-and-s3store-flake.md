# Diary: A fuzz finding of `FuzzSequence`, and a test of the S3 client that depends on timing

A measurement of two days of CI runs turned up two failures that nobody had looked at: `FuzzSequence` of `internal/sqlite/crosscheck/drivercheck` failed once in the job `asan-fuzz` on `main`, and a subtest of `TestClient_connections` in `internal/store/s3store` failed in 4 of 142 runs of the job `test`. Each is a branch and a pull request of its own: `fix-fuzz-sequence` from `origin/main` at `093b646`, and `fix-s3store-flake` on top of it, so that it carries this diary.

## Step 1: The input of `FuzzSequence` that failed after #73

**Author:** fuzz-sequence and s3store-flake builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`FuzzSequence` in the driver check (`internal/sqlite/crosscheck/drivercheck`) failed in the `asan-fuzz` job on `main` on 2026-11-20 at 00:58 (run 44650069801, job 136002504138, after pull request #73): `insert into t3 values (2, 'b', 'c')` got `not supported: a string literal for the column t3.v3 of type REAL` where the test's rule expected no error (""). [...] Rebuild the input from the log and run it against today's `origin/main`. Say whether it still fails. Find out which side is wrong [...] If it no longer fails, find the commit that fixed it (bisect over `main` if cheap) and still add the input to the corpus."
**Interpretation:** Rebuild the corpus file byte for byte, run it on `main` and on the commit of the run, and decide from SQLite and the rule of exact types whether s4ql or the test's rule was wrong.
**Inferred intent:** Know that the finding is closed, and keep the input in the seed corpus so that it stays covered.

### What I did

The job's log prints every file under `testdata/fuzz` after a failure. `gh run view 44650069801 --log --job 136002504138` gave the three lines of `c7060d373c508f7a`. Its SHA-256 starts with `c7060d373c508f7a`, which is the name that Go gives a corpus file, so the bytes are exact. The input creates one table, `t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid`, and runs inserts and selects on it; no table is dropped, and no transaction is rolled back. `insert into t3 values (2, 'b', 'c')` gives a text for the REAL column `v3`. SQLite 3.54.1 refuses it too: "cannot store TEXT value in REAL column t3.v3". By the rule of exact types, s4ql refuses it with its "not supported", and the test excuses that only if its rule gives the same refusal.

- At `ac45bbf`, the head of the run (#73), the input fails as in the log. `pair.typeRefusal` looked at the type of the first value only, against the column with `pk = 1`: the key, an INT, with the value 2. So it gave "" for every insert whose key had the right type.
- At `8514960^` it fails, and at `8514960` ("Check every value of an insert of several columns in `drivercheck`") it passes. That commit is in #74 (`a832e68`), merged right after #73, and not in #73.
- On `origin/main` at `093b646` it passes, pinned and not pinned. The rule is `stmttest.TypeRule` now, which looks at every column in the order of the table.

The fault was in the test, not in s4ql. I added the file as it was to `internal/sqlite/crosscheck/drivercheck/testdata/fuzz/FuzzSequence/`, and a case to `TestTypeRule` in `internal/sql/stmt/stmttest/rule_test.go`: `insert into t values ('x', 2, 'c', null)`, a text for the REAL column after a key of the right type, which gives the refusal for `t.b`.

`go test -run='^$' -fuzz='^FuzzSequence$' -fuzztime=5m ./internal/sqlite/crosscheck/drivercheck` passed with 9,184 executions and 48 new interesting inputs, on a machine that other builders' tests loaded at the same time. Under the address sanitizer, in the `golang:1.27.3` container of `make test-asan`, for this package only: `go test -asan -short -shuffle on ./drivercheck` in `internal/sqlite/crosscheck` passed in 159 s, and `make fuzz FUZZDIRS=internal/sqlite/crosscheck/drivercheck FUZZFLAGS=-asan FUZZTIME=30s` passed with 263 executions after a baseline of 64 inputs, the new one among them.

The rate was low, often 0 per second, and while it ran another test failed with `connect: can't assign requested address`. `netstat` showed 32,604 established connections on 127.0.0.1, and `lsof` gave from 3,400 to 4,200 of them to each worker of the fuzz run. Each input opens an instance of s4ql against the one fake store of the worker, each instance has an `http.Transport` of its own, and nothing closes its idle connections when the instance closes, so they stay for the `IdleConnTimeout` of 90 seconds. That is a leak in the product, small for an application that opens one instance, and I recorded it in `docs/todo.md` under the S3 client and did not fix it.

### Why

The input was found by a fuzz run of 30 seconds, and nothing in the seed corpus had a value of another type after the key in a table of several columns with every column given; the case in `TestTypeRule` says the same thing at the level of the rule.

### What worked

The cleanup step of the job, which prints every corpus file, made the input exact without an artifact.

### What didn't work

My first copy of the input from the log stopped one line early, without `bool(true)`, and `go test` refused it: `wrong number of values in corpus entry: 2, want 3`. The target has had three arguments since before #73.

### What I learned

A finding of a fuzz run on `main` can be fixed by the next pull request before anyone looks at it. Here the run of `asan-fuzz` on the merge of #73 found what #74, which was already open, fixed an hour later.

### What was tricky

Telling the leak of connections from a slow machine: the fuzz run's rate of 0 per second looked like load until `lsof` counted the connections per process.

### What warrants review

- The claim about the leak in `docs/todo.md`, which I read from the code of `s4ql.go` and `internal/store/s3store` and from the counts of `lsof`, and did not prove with a test.
- Whether the case in `TestTypeRule` belongs there or is one too many: the rule already had a case of a value of another type in another column, with an argument for a TEXT column.

### Future work

The idle connections of a closed instance, in `docs/todo.md`. They may also slow the fuzz jobs of CI, where Linux has about 28,000 local ports.

## Step 2: An answer that races with the end of the context

**Author:** fuzz-sequence and s3store-flake builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`TestClient_connections/\"should end a write and a read whose answer is slow when the context ends\"` in `internal/store/s3store` (`s3store_test.go` around line 415) failed in 4 of 142 runs of the `test` job, on four different branches: it got \"the answer has no ETag\" instead of `context.DeadlineExceeded`. [...] Decide what is right and say why: either the client must return the context's error first when the context has ended (then that is a fix in the client, with a test that forces the order), or both orders are legal for the client and the test must force one or accept both. Consider what a caller of the store needs: an operation whose context ended must not be reported as a broken answer from S3 [...]"
**Interpretation:** Find out how the answer of the fake reaches the client after the deadline, decide which error the client owes its caller then, and make the test independent of the race.
**Inferred intent:** A test of the job `test` that never fails by chance, and a client whose errors say why a call ended.

### What I did

The fake answers 200 without headers once the server sees the request's context end, which is when the client closes the connection after its deadline. `persistConn.roundTrip` of net/http, on the end of the context, first takes a response that its read loop has already parsed, and returns that ("Check for a response racing the context close, and use the response if available"). On a loaded machine the fake's answer can come in between the deadline and the close of the connection, and then `Client.Get` saw a 200 without an ETag and reported "the answer has no ETag", an unexpected answer, with nothing of the deadline in its error. The log of run 44715954848 shows it: `s3store: get "k": the answer has no ETag`. `Client.Create` has the same window, with "outcome unknown: the answer has no ETag".

I decided that the client takes such an answer if it shows what happened, and otherwise reports the end of the context:

- A success, and the errors of `store.Store` (`ErrNotFound` of a read, and the refusals of a write), are what the store did, whenever they come in. A write that the store answers with success is applied, and an unknown outcome in its place would only make the core find out by reading.
- Any other answer, or an error of the SDK, after the end of the context wraps the context's error, through `ended` in `internal/store/s3store/s3store.go`. The end of the context may be what broke the answer, as here. The call is logged as `failed` (a read) or `unknown` (a write) and not as `unexpected`, which is for a store that misbehaves.

Why it matters to a caller: `internal/core` decides by its own `ctx.Err()` and by the sentinels of `store.Store`, and treats both errors the same in its retries, so it was not wrong. But its per-request timeouts (`StoreTimeout`, `ObjectTimeout`) end the context of a request and not the caller's, and the error of the request then reaches the caller of the core, where "the answer has no ETag" reads as a broken store and `errors.Is(err, context.DeadlineExceeded)` is false. The package doc of `s3store` says the rule now.

The new subtest "should wrap the error of a context that ended before the answer came in, unless the answer shows what happened" forces the order: `ending`, a context of the test, reports `context.DeadlineExceeded` from `Err` once the fake's hook has called `end`, while its `Done` stays open, so the transport always takes the answer and the client always finds the context ended. It checks a read and a create answered with no ETag and with a 500, and that a success, a 412 of a create, and a 404 of a read are still taken. Before the fix it failed with `no-etag s3store: get "no-etag": the answer has no ETag`, the error of CI. The flaky subtest is unchanged: both of its orders now give `context.DeadlineExceeded`.

The two subtests, by `-run 'TestClient_connections/(should_end_a_write_and_a_read_whose_answer_is_slow|should_wrap_the_error_of_a_context_that_ended_before)'` with `-short`: `-count=500` passed in 81 s; with 20 busy loops on the 10 cores (load averages from 19 to 34) `-count=500` passed in 81 s, and `-race -count=200` passed in 38 s (load 34 to 38). With the client of `main` put back, the old subtest passed 500 times under the same load: the race is rarer on this machine than in CI, and the new subtest is what shows the fault.

The review of the code-review skill (medium) found six things. I fixed three. The content of a read that fails after the end of the context did not wrap the context's error; `body.Read` in `/internal/store/s3store/content.go` now passes such a failure through `ended`, and the new subtest reads a body that the fake cuts after the end (before the fix: `s3store: get "cut-body": unexpected EOF`). The error of a write printed the context's error twice, once from `ended` and once from `unknown`; `unknown` now keeps the text of its error and wraps the context's error through `textOf`, which adds no text, and the subtest counts it once. A swap refused with 412 after the end is now checked too. The internal tests of `body` made a `request` without a context, which `ended` needs; they pass `t.Context()` now. I left three. A real broken answer that comes in just before a deadline and is checked just after it is reported as the end of the context, not as `unexpected`. A 403 after the end wraps `context.DeadlineExceeded` too. And `unknown` keeps `Canceled` over `DeadlineExceeded` if the error of the SDK wraps the one and the context ended with the other. Each of these needs the deadline to fall within the few microseconds between the answer and its check, and none changes what the core does.

After the fixes, under the same load: `-count=500` passed in 83 s, and `-race -count=200` in 39 s, at load averages from 18 to 41.

### Why

The other way, a test that accepts both errors, would have kept a client that tells its caller of a broken store when the caller's own deadline ended the call.

### What worked

The comment in net/http's `roundTrip` named the race at once, and the log of CI showed that it was the read and not the write.

### What didn't work

The first run of the package's tests failed in two other subtests with `connect: can't assign requested address`: the fuzz run of step 1 held most of the machine's local ports. They passed once it had ended.

### What I learned

A context whose deadline has passed can still be followed by a complete answer from net/http, and the SDK does not look at the context after the answer, so a client has to.

### What was tricky

Forcing the order without a sleep. Nothing in the client can be hooked between the answer and its check, and a real deadline or cancel races with the transport. `ending` breaks the rule of `context.Context` that `Err` is nil while `Done` is open, on purpose and in a test only. It stands for the moment after a deadline at which the transport has not acted on it yet.

### What warrants review

- The decision that a success or a refusal after the end of the context is taken, and that only the other answers are reported as the end of the context.
- `ending`, which breaks the contract of `context.Context`: whether a reviewer accepts it as the way to force the order.

### Future work

None.
