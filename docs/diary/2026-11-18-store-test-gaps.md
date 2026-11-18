# Diary: Close the gaps that the audit of the stores, the simulation, the lint, and the formats found

An independent audit of the stores, the simulation, `simlint`, and the formats found no dishonest test, but one claim that the code does not support and several checks that can be weakened without a test failing, each proven by a planted fault. This work closes them, on the branch `store-test-gaps`, without touching production code in `internal/core`.

## Step 1: The brief, and a plan

**Author:** store-test-gaps builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "An independent audit of the stores, the simulation, the linter, and the formats found no dishonest test. It found one claim the code does not support and several checks that can be weakened without a test failing, each proven by planting a fault. Your job is to close them. [...] 1. **The storage core has never run against real S3.** [...] Add an integration test that runs the real `internal/core` on the real `s3store` against a real bucket: open, create a table, commit rows, close; open again with an empty cache and replay the log to the same rows; a second instance takes the lease after the first releases it and sees the commits; a commit whose manifest swap loses to another writer fails as the spec says. [...] 2. **The conformance suite does not pin key identity.** [...] Add keys that differ only in case, a key with `//`, and a key of 1,024 bytes, with cases that fail for a folding or collapsing store. Add both wrong stores to the suite's self-test [...]. 3. **`simlint` has holes.** [...] `time.Now()`, `time.After`, and a blocking receive inside the communication clauses of a `select` that has a default are not reported [...] A range over a channel whose type is a type parameter (`[C ~chan int]`) is not reported [...] `host.New()` is not forbidden in library code [...] CI never lints the files built only for `js && wasm` [...] 4. **`simstore`'s "applied, then no answer ever" has no test.** [...] Add a case with a lost response and a hang that asserts the write is in the history and the caller gave up. [...] 5. **One timing-dependent check.** `internal/store/s3store/s3store_test.go:499` sleeps 200 ms and then asserts that no late read happened [...] Rework it so the order of events is forced, not waited for."
**Interpretation:** Five independent pieces of test work, plus a real-bucket run of the new integration test and of the existing real-bucket tests. Production code may change only in `simlint` (its rules) and nowhere in `internal/core`.
**Inferred intent:** Make every claim about the tests true, and make each check fail for the fault it is meant to catch, with a planted fault as proof.

### What I did

Read `AGENTS.md`, the spec's rules on the lease and the commit, `internal/simlint/simlint.go` and its testdata, `internal/store/storetest`, the real-bucket helpers of `internal/store/s3store/s3store_test.go`, the request path of `internal/store/simstore/client.go`, and the API of `internal/core`. The plan, in the order I build it:

1. `simlint`: inspect the operands of a select's communication clauses, report a range over a type parameter whose type set holds a channel, forbid `host.New` outside the exempt packages, and run `simlint` over the browser's build in `make lint` and in CI's browser job.
2. `simstore`: a test with `LostResponse: 1` and `Hang: 1`, which asserts that the caller gave up at its deadline and the write is in the history.
3. `s3store`: replace the sleep of 200 ms with `httptrace.ClientTrace.WroteRequest`, which net/http calls once the transport has finished with the body, so that no read of the body can follow it.
4. `storetest`: a case for keys that differ only in case or in a doubled slash, and keys of the longest length the store takes, and two wrong stores in the self-test.
5. The integration test of `internal/core` on `s3store`, in the test file of `s3store`, which already has the helpers for a real bucket.

### Why

The order puts the small, self-contained pieces first, so that each can be committed alone, and the real-bucket work last, since it needs the credentials and the shared network.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

- `internal/sim` is exempt from `simlint` already, so forbidding `host.New` does not flag `internal/sim/free.go`.
- The prefix of a database in `internal/core` cannot have an empty path segment, so the core never writes a key with `//`. The case for `//` is about a store that would merge two databases whose prefixes differ that way, or any two keys.
- The conformance suite runs on the real bucket under a prefix of about 44 bytes, so a case with a key of 1024 bytes needs to know how long a key the harness can take.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.

## Step 2: The lint, the hang of `simstore`, the late reads of `s3store`, and the identity of keys

**Author:** store-test-gaps builder

### Prompt Context

**Verbatim prompt:** (as in step 1) points 2 to 5 of the brief.
**Interpretation:** Close each hole with a test or testdata that fails without the fix, and show the failure.
**Inferred intent:** Each check of the stores and the lint fails for the fault it names.

### What I did

**`simlint`** (`/internal/simlint/simlint.go`). Testdata first: in `/internal/simlint/testdata/src/library/library.go`, a select with a default and one without, whose clauses call `time.After` and `time.Now`, send the result of a receive, receive from a receive, and assign a received value to a map index that is itself a receive; a range over `C ~chan int`, `R ~<-chan int`, and `U interface{ ~chan int | ~<-chan int }`; and ranges over slice, map, and integer type parameters that must stay quiet. `host.New` needs a package inside the module, because `library` cannot import an internal package, so it is in a new testdata package, `/internal/simlint/testdata/src/maragu.dev/s4ql/internal/wiring/wiring.go`, and the host's stub got `Host` and `New`. `go test ./...` in `/internal/simlint` then reported 12 missing diagnostics. The fix:

- `commOperands` gives the expressions of a clause's communication without the receive or send itself: the channel and value of a send, the channel of a receive (through parentheses, with `ast.Unparen`), and the left side of an assignment. Each is inspected as any other code.
- `isChan` reports a channel, or an interface whose embedded types or union terms hold one. The underlying type of a type parameter is the interface of its constraint, so one function covers both.
- `host.New` is in the map `forbidden`. The packages that may make the production host, `internal/host` and `internal/sim`, are exempt already.
- `make lint` runs `simlint` a third time, with `GOOS=js GOARCH=wasm GOFLAGS=-tags=s4ql_browser` over `BROWSERPKGS`, and CI's `browser` job has one new step that does the same with the binary that the step before it built. I planted `func plantedReceive(ch chan int) { <-ch }` in `/internal/sqlite/faultvfs/faultvfs_browser.go`: the browser's run reported it, with exit status 3, and the native run did not, with exit status 0. Then I reverted it.

**`simstore`** (`/internal/store/simstore/client_test.go`). `TestClient_hang` runs a create and then a swap under `Faults{LostResponse: 1, Hang: 1}`, each with a context of an hour. It asserts that each call fails with `context.DeadlineExceeded` only after an hour of simulated time, and that each write is in the history and in the object. With `hang := false && s.chance(f.Hang)` at `/internal/store/simstore/client.go:323`, `go test ./internal/store/simstore/` failed only in the new test: `client_test.go:240: simulated fault: the response to request 1 of a was lost on its way back`. Reverted with `git checkout`.

**`s3store`** (`/internal/store/s3store/s3store_test.go`). The test of late reads no longer sleeps. It puts an `httptrace.ClientTrace` with `WroteRequest` on the context of the create: net/http calls it from a deferred function at the end of `Request.write` (checked in `net/http/request.go` of Go 1.27.4), after which it never reads the body. After the create returns, the fake store drains the connection with `io.Copy(io.Discard, conn)` before it closes it, and the test waits for `WroteRequest` and then checks that no read came late. The SDK's trace in the client composes with the test's: `httptrace.WithClientTrace` keeps hooks that are on the context already.

**`storetest`** (`/internal/store/storetest/storetest.go`). A new case, `keyIdentity`, creates the first key of each group alone, checks that the others of the group are not found, then creates them, and reads each back. The groups: `case`, `Case`, `CASE`; `ÆØÅ`, `æøå`; `d//k`, `d/k`, `d///k`; two keys of the longest length that differ in their last byte. `Env` has a new field, `LongestKey`, because the real-bucket harness puts a prefix of about 44 bytes in front of every key; it sets `1024 - len(prefix)`. Three wrong stores in `/internal/store/storetest/storetest_test.go`, all one type `renamed` with another function of the key: `strings.ToLower`, collapsing `//`, and cutting keys at 1000 bytes. Before the case existed, each of the three passed all 17 cases (`the cases that failed: []`). With it, each is found.

### Why

Each of these is a gap that the audit proved by a planted fault. Every new check here fails on the plant or on the wrong store that it is for.

### What worked

- The type parameter's `Underlying()` being its constraint's interface made `isChan` short.
- Planting a violation in a browser-only file was the quickest proof that the new lint run sees those files.

### What didn't work

- The first testdata imported `maragu.dev/s4ql/internal/host` from the package `library`: `src/library/library.go:20:2: use of internal package maragu.dev/s4ql/internal/host not allowed`. Hence the package `wiring` under `maragu.dev/s4ql/internal`.
- A union `~chan int | ~chan string` does not compile for a range: `cannot range over u (variable of type U constrained by interface{~chan int | ~chan string}): channels chan int and chan string have different element types`. The testdata uses `~chan int | ~<-chan int`.
- The first rework of the late-read test closed the connection without draining it. To see whether it could catch a late read at all, I ran it in a scratch test with a plain `http.Client` (no SDK, no guard), which does read late: it saw a late read in 7 of 10 runs, because the store never read, so the transport sat on a full socket buffer. With the drain, 30 of 30 runs saw a late read. The scratch test is deleted. The old test with its sleep had the same blind spot.
- A shell command with `python3` and a heredoc, and one that ran `git` after a `cd` to a computed directory, were refused by the sandbox of the worktree. I used Edit, and plain commands from the worktree's root.

### What I learned

- `WroteRequest` is the event that ends every read of a request's body by net/http, whether the body was sent whole or the write failed, so a test can wait for it instead of a time.
- A blind spot of a negative check ("nothing happened after X") is best measured with a client that is known to do the bad thing.

### What was tricky

The `select` without a default was already reported, but its clauses were not inspected either; the fix covers both, and the testdata has a case of each.

### What warrants review

- `commOperands` assumes that the right side of a clause's assignment is exactly one receive, as the language requires.
- The CI step reuses `$RUNNER_TEMP/simlint` from the step before it in the `browser` job.

### Future work

None.

## Step 3: The storage core on a real bucket, the runs on Tigris, and self-review

**Author:** store-test-gaps builder

### Prompt Context

**Verbatim prompt:** (as in step 1) point 1 of the brief, and "Then run it against the bucket. [...] Run the existing real-bucket tests once as well and report the result. If the test finds a fault in the core or the client, stop and report it; do not fix production code in `internal/core` yourself." And: "Self-review your diff before you report."
**Interpretation:** A test that runs the real `internal/core` on the real `s3store` against Tigris, gated like the other real-bucket tests, under a prefix that is deleted at the end, without a production dependency of the core on the client.
**Inferred intent:** Make the claim that commits, replay, and the lease run on a real S3-compatible store true.

### What I did

`TestClient_core` is in `/internal/store/s3store/s3store_test.go`, next to `realBucket`, `realClient`, and `newPrefix`, which it uses. So the core depends on the client only in a test of the client's package, and the browser's build of `internal/core` does not pull in the AWS SDK. The database's prefix is the test's prefix plus `db`, because the core refuses a prefix with an empty segment. Each instance has a lease of ten minutes and watches four times a second. Three subtests, each under its own prefix:

1. Two commits (a table and two rows; one row more and a delete), close, then: the manifest has no lease, two commit objects are under `commits/`, and the head is one of them; an instance with an empty cache replays the same rows; the first cache opens at the same rows.
2. `b` opens while `a` serves, and `b.Read` and `b.Begin` fail with `core.ErrNoLease`; `a` commits and closes; `b.Wait` returns and `b` reads `a`'s rows, commits one more, closes; `a` reopens on its cache, one commit behind, and catches up.
3. `a` begins a transaction; the test releases `a`'s lease in the manifest by hand with a swap (no correct instance takes a held lease); `b` takes the free lease, commits, and closes; then `a`'s commit fails with `core.ErrNoLease` and not `core.ErrOutcomeUnknown`, a wrapper around `a`'s store has counted a swap refused with `store.ErrChanged`, `a` takes the lease again and serves `b`'s row without its own, and a fresh instance replays the same.

The order in 3 is forced by the test, except for `a`'s renewal, due after two thirds of ten minutes. If it came first, its swap would be the refused one, and the commit would fail at its check of the lease, with the same error. Every assertion holds in both orders. Once, with a temporary `slog` logger on the instances, I read the events of a run: `a`'s commit uploaded its object, its swap took 198 ms and was refused, `a` logged `lease.transition=lost`, and the commit's event was a warning. The logger is not committed.

Runs against the project's Tigris bucket, with `set -a && . /Users/maragubot/Developer/s4ql/.env && set +a && S4QL_S3_BUCKET=$BUCKET go test ...` from the worktree (the values were never printed):

- `TestClient_core`: 3 of 3 subtests pass, about 8 to 10 s; again with `-count=5`, and with `-race -count=3` together with the conformance suite.
- The whole package with `-v`: `TestClient_conformance` 18 of 18 cases pass, among them the new case of key identity; `TestClient_realBucket` 5 of 5 pass, none skipped; `TestClient_core` passes; 21.9 s in all.

Self-review with the code-review skill found no bug that breaks anything. I took five of its points: the late-read test now closes the hijacked connection in a cleanup, so that a transport that sends the whole body and keeps the connection does not leave the fake's handler in `io.Copy`; `release` in the core's test tries ten times and then fails; `keyIdentity` refuses a `LongestKey` below 8 or above 1024 with an error and not a panic in `strings.Repeat`; the comment of the select in `simlint` now says that the left side of a receive runs after the select chose the clause, as the spec says; and the comment on `host.New` says that only exempt packages make the host today, and that a package that must make it and keep the other rules needs an exception that the lint does not have. I left one point: the list of browser packages is written out in the new CI step, as it is in the golangci-lint step next to it, and not taken from `BROWSERPKGS` of the `Makefile`. The other builder is changing the CI file now, and one place for the list fits a change of its own. A second call of `WroteRequest` would panic on the closed channel; I kept that, since the client sends a write once and a second call would be a fault to see.

Docs: `/internal/store/s3store/README.md` names the new test and the keys of the new case; `/internal/sim/README.md` says that the lint runs over the browser's files in the job `browser`; `/docs/todo.md`, under the tests of the storage core and the S3 client, says where the core runs on a real bucket and what the integration tests are.

### Why

The README of the project claimed what only this test now does. The forced release in 3 is the one way to make a commit's swap lose while the instance still believes it holds the lease, which is the case where only the store's compare-and-swap protects the log.

### What worked

The test passed on Tigris on the first run, and found no fault in the core or the client.

### What didn't work

- `make test-asan FUZZTIME=30s` failed on the shared machine, with a load average of about 26: `panic: test timed out after 10m0s` in `internal/core`, `internal/sim`, `internal/sql/parser`, `internal/sql/parsetest`, `internal/sql/shapetest`, and `internal/sql/stmt`, and `internal/sql/oracle` passed after 614 s. This branch changes no code that those packages build. The three store packages passed in that run. I ran the six again in the same container with `go test -asan -shuffle on -timeout 90m`, and all passed, in 250 to 383 s each. The fuzz half of the target did not run, because the target stops at the first failure; its fuzz tests are in packages that this branch does not touch.
- `git status --short && docker info ...` and `git diff origin/main > file` piped with other commands were refused by the sandbox of the worktree as too complex; plain separate commands worked.

### What I learned

- After a failed commit, its commit object stays in the bucket with no manifest that names it, so a test of the log counts the objects only where no commit failed.
- A test that needs a holder's lease to last can set it long: a released lease is taken at once, so a long lease costs a test nothing.

### What was tricky

Choosing how another writer gets in while `a` holds the lease. A forged manifest that names another holder would keep `a` out for ten minutes; a manifest without a lease lets a real second instance take it and commit, so the writer that wins is a real instance of the core.

### What warrants review

- `TestClient_core` is in the test file of `s3store`, so `go test ./internal/store/s3store/` now builds the core and SQLite.
- `make lint`'s `simlint` line now has three runs joined by `&&`; the other builder may change the same target.

### Future work

- `simlint` has no exception for a package that must make the production host and keep the other rules; the package of the public driver will need one.
- CI could take the list of browser packages from the `Makefile` instead of writing it out twice.
