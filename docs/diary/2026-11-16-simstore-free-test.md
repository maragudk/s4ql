# Diary: The free-running test of the simulated store that failed by chance

A subtest of `TestClient_free` in `/internal/store/simstore/client_test.go` failed once in CI on a pull request that did not touch the storage code. The work here was to find out why, with certainty, to decide whether the test or the store was wrong, and to make the test assert only what holds in every run. It is on the branch `simstore-free-test`.

## Step 1: Find the cause, and make the test deterministic

**Author:** simstore-free-test builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "You are the builder for a small fix in s4ql [...]: a test that failed once in CI for a reason that looks like chance. [...] In CI's `test` job on a pull request that did not touch the storage code (run 43954364694, job 133604873832, `-test.shuffle 1794862548988606819`): `--- FAIL: TestClient_free/should_take_requests_from_callers_in_parallel_with_every_fault_on (0.01s) client_test.go:92: Not true` [...] 1. **Find out why the history can be empty**, with certainty, from the code [...] Reproduce it [...] and report the measured failure rate. Decide which of these it is: the test asserts something that the code does not promise [...]; the history misses something it should record, or `Store.Wait` returns before held writes have landed, or another real fault in `simstore` [...]; something else. [...] 2. **Make the test deterministic** in what it asserts [...]. Do not just delete the assertion without saying what the test still proves. 3. Look through the other tests of `internal/store/simstore` and `internal/sim` that use `sim.NewFree` or unseeded randomness for the same kind of chance-dependent assertion, and fix those too."
**Interpretation:** Explain the empty history from the code and from traces. Measure how often it happens. Replace the assertion `len(st.History()) > 0` with checks that hold in every run, and check the other free-running tests.
**Inferred intent:** `simstore` is the base of the storage simulation's checks, so a flaky test there must be either a proven false alarm or a real bug. Either way, it must stop failing for no reason.

### What I did

I measured the old subtest first. `go test -c` of `/internal/store/simstore`, then `-test.run 'TestClient_free/should_take_requests' -test.count 2000`: 27 of 2000 runs failed, 1.35%.

The free-running host keeps no trace, so I ran the same workload in a scratch test (not committed) on the seeded scheduler, `sim.New(seed, sim.Options{})`, for 20000 seeds, and looked at the traces of the seeds whose history was empty. 248 of 20000 were empty, 1.24%, which matches the free-running rate. I sorted every empty seed:

- 213 seeds: an outage was drawn by one of the first eight requests, before any request reached the store, and it outlasted all 160 rounds. Every request was a `Get` that failed, so no write was ever sent. In seed 295 the outage started at 0 s and lasted 15.07 s, and the last request was sent at 8.22 s.
- 35 seeds: one or more `Get`s reached the store first and found nothing, so their callers sent a `Create`, but an outage was drawn within the first 50 ms, before any create arrived. I followed each of the 16 writes in 10 such traces to its end. Each one was one of these: it did not reach the store during the outage and was not stuck; it was stuck in the network and arrived while the store was still unreachable (in seed 2420 because `caller-5` was also cut off from 1.07 s for 39.8 s); or it was slow by 17.85 s, its caller gave up, and the draw for `Late` let it be lost.

Every empty history comes from faults that `simstore` documents, so this is not a fault in `simstore`. The test asserted something the code does not promise. With `simstore.AllFaults()`, `Outage` is 0.002 per request and `OutageFor` is 20 s. The chance that one of the first eight requests draws an outage is about 1 − 0.998^8 ≈ 1.6%. A round takes about 0.22 s on average: a failed request returns within the latency, except in the 20% of cases where `Hang` makes the caller wait out its timeout of 1 s. So 20 rounds take about 4.4 s, and an outage from U(0, 20 s) outlasts that with a probability of about 0.78. 1.6% × 0.78 ≈ 1.24%, which is the rate measured.

I also checked that `Store.Wait` cannot return before a held write has landed. `lose` appends the task of a stuck write to `carriers` before its carrier returns, and `Wait` reads the length of `carriers` again on every pass. `send` appends each carrier before it returns, and `Wait` is called only after every caller has returned.

The new subtest, "should take requests from callers in parallel with every fault on, and apply what they saw as one chain of writes", keeps the 8 callers × 20 rounds on `sim.NewFree` with `AllFaults()`. Each caller writes content that names it ("caller-3 7") and records what its reads and successful writes returned, in its own slice. After `st.Wait`:

1. Every version that a caller saw is one of a write in the history, with that write's content. A caller's results appear in history order: each read is of the last write the caller saw or a later one, and each write is later.
2. The store is calmed, the test sleeps `OutageFor`, and a new client, which was never cut off, makes one more write. No request is in flight after the wait, so no new outage can start, and any earlier one has ended. The write must succeed, so the history is never empty.
3. The history is one create and then swaps, each from the version of the write before it. Each write is credited to the client named in its content. The last write is the one that step 2 made, and the object holds it.

In `/internal/sim/free_test.go`, the check `on > 0 && on < 50` of the buggify sites can fail by chance, with a probability of 2^-49. I added a comment that says so, and left the check.

### Why

The purpose of the free-running test is to run the store with real parallelism for the race detector, with every fault on, and to show that it does not deadlock, race, or panic. The old assertion added that something was written, which depends on draws from no seed. Steps 1 and 3 hold in every run for a correct store, and step 2 forces the condition that the old assertion assumed.

### What worked

The seeded scheduler was the right tool for the cause. It runs the same fault code with a trace, and its rate of empty histories, 1.24%, matched the free-running rate, 1.35%, so the traces explain the CI failure and not a different one.

Planted faults showed that the new checks catch real bugs. I changed `apply` in `/internal/store/simstore/client.go` and restored it after each run:

- With the check of the swap's version removed, so that a stale swap is applied, the chain check failed in 49 of 50 runs.
- With `Client: "x"` in the recorded write, the check of the client failed in 50 of 50 runs.
- With a `Get` that returns the write before the last one (a stale read), the check of each caller's order failed in 45 of 50 runs, and the last write failed in 2 more.

### What didn't work

- My first plant used `perl -0pi -e 's/\tcase r.kind == swap && o.version != r.from:\n...//'` and did not match, because the line has two tabs of indentation. The test passed, and only `git diff --stat` showed that nothing had changed. I used Edit after that.
- The first count of empty seeds was piped through `| tail -30`, so only 26 of the 248 per-seed lines survived. I ran it again in 10 parallel subtests, with one counter for each kind of case.
- A loop in zsh, `grep -E "request $id[^0-9]"`, failed with `(eval):1: bad math expression: operand expected at '^0-9'`, because zsh reads `$id[` as an array subscript. I moved it into a bash script in the scratchpad.
- In the first restructure, the callers' checks ran after the last write. Under the planted stale read, the last write failed first, with `is.NotError` at line 130, so the order check never ran and could not be judged. Moving the callers' checks before the calm write made each check independent.

### What I learned

With every fault of `AllFaults()` on, about one run in eighty of a workload of 160 short rounds never reaches the store. That is enough to make any "something happened" assertion on a free-running run flaky. Such an assertion needs either a seed or a step that forces it, as the calm write here does.

`Store.Calm` stops new faults but not those that have started. Its comment says so, and the final write therefore needs both the wait for the store and a sleep of `OutageFor`. A new client is needed too, because a cut-off of an old client can last up to `CutOffFor`, which is 40 s.

### What was tricky

The order check depends on two facts that are easy to miss. The content of each write is unique, so a version identifies one write. And a caller sends its next request only after its previous call returned, so the store applies it later. A write that is stuck and lands late belongs to a call that failed, and so it is not among the results a caller recorded.

### What warrants review

- The comment above the subtest, and the argument for why the calm write always lands, in `/internal/store/simstore/client_test.go`.
- Whether to keep the `2^-49` chance in `/internal/sim/free_test.go` as a documented comment, or to remove the some-on-some-off check.
- To validate: `go test ./internal/store/simstore -run 'TestClient_free/should_take' -count 2000` and `go test -race ./internal/store/simstore -run 'TestClient_free/should_take' -count 500`.

### Future work

- `TestFreeRunning` in `/internal/core/free_test.go` asserts `commits > 0 && reads > 0`, summed over 30 free-running runs with `simstore.AllFaults()`. It is the same kind of assertion, but over 30 runs, so it fails by chance only if none of them commits or reads. That package was outside this brief.
- `host_test.go` in `/internal/host` asserts that 100 draws from crypto/rand are distinct. Two of them are equal with a probability of about 2.7 × 10^-16.
