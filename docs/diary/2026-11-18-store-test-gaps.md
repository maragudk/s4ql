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
