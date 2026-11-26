# Diary: the race of the counter in the store's conformance test

CI's job `test` failed on pull request #160, which changed only tests in `internal/sql/stmt`, in `TestClient_conformance/should_lose_no_increment_of_a_counter_that_racing_clients_swap` of `internal/store/s3store`, against the real bucket. The task was to find whether a conditional swap lost an increment, and if the test was at fault, to make it pass or fail for the same reason under any load.

## Step 1: find the cause

**Author:** s3store-flake

### Prompt Context

**Verbatim prompt:** Investigate one failed CI test in the repository `maragudk/s4ql` (a Go SQL database whose truth is a log in S3). Read-only investigation first; propose a fix but do not commit unless told below. [...] The failure, in CI job 139648803343 of run 45741922004 (pull request #160, which changes only tests in `internal/sql/stmt`): package `internal/store/s3store`, `--- FAIL: TestClient_conformance/should_lose_no_increment_of_a_counter_that_racing_clients_swap (7.81s)`, `storetest.go:557: get "counter": ... GetObject, https response error StatusCode: 408 ... api error RequestCanceled: Request is canceled.   (x5)`, `storetest.go:582: the counter is "21 racer-0-3" after 32 increments`
**Interpretation:** Decide from the log and the code whether the client lost an increment under If-Match, or whether the test is wrong, say where the 408 comes from, how often it happened, and propose a fix.
**Inferred intent:** A lost increment would be a fault in the storage core, so it must be ruled out with evidence, and a flaky test must stop failing CI without hiding such a fault.

### What I did

I read the job's log with `gh api --allow-escape-sequences repos/maragudk/s4ql/actions/jobs/139648803343/logs` (without the flag `gh` refuses the output: "the response contains terminal escape sequences"). `TestClient_conformance` in `/internal/store/s3store/s3store_test.go` runs `storetest.Test` against the bucket that `/.env.test` names, `sql4s3` on `https://t3.storage.dev`, which is Tigris. The client sets `aws.NopRetryer{}` and its transport has no timeout (`/internal/store/s3store/s3store.go`), and `ended(ctx, err)` would have wrapped `context.Canceled` if the test's context had ended, so the five 408 answers came from Tigris. In `raceCounter` of `/internal/store/storetest/storetest.go`, `read` reports a failed get with `t.Errorf` and the racer returns, while the last check still wants 32. The counter's value fits that: racer 0 finished, so 3 racers that finished made 12 increments and the 5 that stopped made 9, which leaves the 11 that were missing. `gh run list --workflow ci.yml --status failure --limit 40` and the logs of the latest ten showed one earlier case, run 45375324120 on 2026-11-24, where a swap's connection was reset by Tigris and the counter was "29 racer-7-3".

### Why

The test could not tell a lost increment from a racer that stopped, since it never counted the swaps that succeeded.

### What worked

The numbers of the counter and of the failed gets were enough to show that nothing contradicts a test fault, and the message of the error showed that the 408 was an answer of the server.

### What didn't work

`gh run view 45741922004 --log-failed > file` wrote an empty file at first, for the same reason of escape sequences.

### What I learned

Tigris answers a GET with `408 RequestCanceled` now and then under 8 racing clients. Its request IDs are numbers and its host ID is empty.

### What was tricky

The message "after 32 increments" names the count that was expected, not the count that happened, which reads like a lost increment at first.

### What warrants review

Nothing in `internal/store/s3store` changed: the case rests on the client retrying nothing and on how `ended` wraps the end of a context.

### Future work

None from this step.

## Step 2: count the swaps, and retry what a real store fails

**Author:** s3store-flake

### Prompt Context

**Verbatim prompt:** Good work. Now apply your fix and land it. Steps: (1) On your branch `s3store-flake`, `git merge origin/main` first (main has moved), then apply the diff to `internal/store/storetest/storetest.go`. (2) Proof before anything else: plant in the fake store a swap that ignores its condition (If-Match) and show the new check fails with a lost increment; plant a swap that applies twice and show it fails as too many; revert both with `git checkout -- <file>`. Also show that a store whose get keeps failing still fails the test. [...]
**Interpretation:** Apply the fix, prove with planted faults in the fake that it still catches a lost and a doubled increment and a store that keeps failing, run the checks, record it, and land it.
**Inferred intent:** A test that fails only for a fault of the store, on any machine and under any load.

### What I did

`raceCounter` now counts, under a mutex, the swaps that succeeded (`applied`) and those whose outcome is unknown (`unknown`). A racer that gets an error from a get, or from a swap that is none of the store's answers, tries again, and gives up with `t.Errorf` and the last error after 5 failures in a row; an answered swap, a success or ErrChanged, resets the count. The bound on ErrChanged counts ErrChanged only. The last read is tried 5 times too. The check is `applied <= count <= applied+unknown`. A new helper `get` reads as `read` does but returns its error. Then I planted four changes in `/internal/store/s3store/fake_test.go`, each reverted with `git checkout -- internal/store/s3store/fake_test.go`, and ran `go test -race -run 'TestClient_conformanceOnFake/should_lose_no_increment' ./internal/store/s3store`:

- If-Match ignored: "the counter is \"9 racer-X\" after 32 swaps that succeeded and 0 whose outcome is unknown", and 6 and 6 in the other runs of 3.
- A swap applied and then answered 412 one time in three: "the counter is \"122 racer-X\" after 32 swaps that succeeded and 0 whose outcome is unknown", 3 of 3.
- Every get of the counter answered 408: each racer "get \"counter\" failed 5 times in a row", and the last read failed.
- One get in ten answered 408: passed 50 of 50 under `-race`.

### Why

Comparing with the swaps that happened, and not with the swaps planned, makes the check independent of how many calls a store fails, while a lost or doubled increment still shows.

### What worked

The planted faults failed in every run, with messages that name the kind of fault.

### What didn't work

My first version gave up after 5 failures of a racer in total. With one get in four failing, a run of `-count=20` failed again and again, with lines such as "racer 2: get \"counter\" failed 5 times, the last with ... 408". A racer under contention makes many gets, so a budget in total grows with contention; 5 in a row does not.

### What I learned

The reset must come only from an answered swap. If a successful get reset the count too, a store whose swaps always fail with an unknown outcome would alternate between 0 and 1 and never give up.

### What was tricky

A swap with an unknown outcome may have been applied, and its racer then makes another increment, so the counter can be above 32 without a fault, which is why the upper bound is `applied+unknown`.

### What warrants review

The order of the switch on the swap's error in `raceCounter`: the errors that mean "not applied" (ErrNotFound, ErrExists, ErrChecksum, ErrTooLarge) still fail at once, and only the rest counts as unknown.

### Future work

Other subtests of the conformance suite against the real bucket read once and fail on a transient error too. None has failed so far, so they were left.
