# Diary: A faster `browser` job in CI, and a hang of Chromium that fails fast

The job `browser` of `/.github/workflows/ci.yml` built, vetted, and linted the browser's build, and then ran `make test-browser`: the Go tests of `BROWSERPKGS`, compiled for `GOOS=js GOARCH=wasm` with the tag `s4ql_browser`, each test binary in headless Chromium through the runner in `/internal/sqlite/browser/main.go`. A normal run took about 14 minutes, and a hang ran until the timeout of 30 minutes. The branch is `faster-browser-ci` (worktree branch `worktree-agent-aa42ae4ee249b2732`), from `main` at `8d16efc`, with draft pull request #98.

## Step 1: Where the 14 minutes went, and what the hang was

**Author:** faster-browser-ci builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Markus: \"Make the browser CI job faster, 30 mins is too much.\" [...] So there are two problems: the normal run is slow, and a hang costs 30 minutes. [...] 1. **Measure first.** [...] 2. **Find out what the hang was.** [...] 3. **Make the normal run fast.** Target: the `browser` check done in about 5 minutes of wall time or less, without dropping any test. [...] 4. **Make a hang fail fast and say what hung.** [...] 5. **Keep the rest of CI right.**" A later message from the lead: "PR #96 is blocked on this, so finding and fixing the hang now comes before the speed-up. [...] When you have a fix that makes the hang impossible or makes it fail fast with its cause, tell me at once with SendMessage [...] and keep the fix in commits of its own so that it can be merged first."
**Interpretation:** Measure the job from the logs of `main`, find the cause of the hang, give the runner a way to fail fast and say what it saw, then split the tests over jobs that run at the same time.
**Inferred intent:** A browser check that does not hold up every pull request, and a hang that costs minutes and names its cause.

### What I did

The timings of the job on `main`, run 44757417889 (job 136360777414, 14 min 49 s, `gh run view <id> --json jobs`):

- set up, checkout, and setup-go: 15 s (the Go cache hit)
- `make check-browser`: 9 s
- the lint of the browser's build: 6 s
- `go test -race -tags s4ql_browser ./internal/simulator/...`, natively: 2 min 11 s
- the lint of the simulator's native build: 44 s; `simlint` twice: 5 s
- `make test-browser`: 11 min 18 s, of which `internal/core` alone 676.6 s. The others: `internal/sqlite` 22.1 s, `faultvfs` 21.8 s, `internal/sim` 105.6 s, `simstore` 129.2 s, `internal/simulator` 130.9 s, `engine` 1.9 s.

`go test` runs four test binaries at once on the four cores of the runner, so `internal/core` shared the machine with three others for its first two minutes, and then ran alone for nine. Nothing downloads Chromium: the runner image has Google Chrome.

The hang. Three runs ended the same way: 44742111257 on `main` (head 5bd2471), and both attempts of 44754967761 on #96. Each time `internal/sqlite`, `faultvfs`, `internal/core`, and `internal/sim` ended after 1860 s with "browser: the program did not exit within a minute after its -test.timeout", with no output from the program, and the three packages after them passed; in 44754967761 `simstore` took 42.9 s and `internal/simulator` 78.3 s alone, against 129 s and 131 s beside the others. The four that hang are the first four that `go test` starts, at the first minute of the runner. `internal/sqlite` takes 22 s, so a hang inside its tests at the same moment as the other three would be a coincidence. The program printed nothing, not even the panic of Go's own test timeout, which needs Go's scheduler to run; that fits a program that never started.

The old runner saw nothing between the start of Chromium and the exit, so I gave it eyes first (commit 786c981, in `/internal/sqlite/browser/main.go`, `/internal/sqlite/browser/main_test.go`, `/internal/sqlite/browser/worker.js`): it marks each step of the start (Chromium started, the page loaded, the Worker loaded, it fetched its configuration and the program, the program started; `worker.js` now posts `started` before `go.run`), keeps when the program first and last wrote, and reads `=== RUN` and `--- PASS/FAIL/SKIP` lines to know the tests that had started and not ended. `worker.js` sends output that holds `=== RUN ` at once, because a hang in SQLite holds the Worker's thread and its 100 ms timer never fires. It writes all that, with the end of Chromium's output, when it stops Chromium: 30 s after `-test.timeout`, before `go test` kills the runner at the timeout plus a minute, and on SIGINT, SIGTERM, and SIGQUIT. It also starts Chromium again, with a new profile and port, when Chromium has not started the program a minute after its start, or stops before it, up to three times; no code of the program has run then. The tests run the test binary itself as a fake Chromium (`TestMain` with `S4QL_CHROMIUM_FAKE`), in modes that start the program, hang, crash once, or run and hang.

Then I measured on CI with temporary jobs on #98 (commits 4fd3e58, d8c6abc, 0c2128c, removed again):

- `browser-measure`, one unit of the tests per job, one Chromium per runner, run 44766028443. Test step times: `internal/sqlite` 84 s, `faultvfs` 18.5 s, the planted bugs of `internal/core` 276 s and, on another runner, 167 s, `TestSimulation` and `TestFreeRunning` of the core 244 s, the rest of the core 92 s, `internal/sim` 48 s, `simstore` 90 s, `internal/simulator` 88 s, `engine` 68 s. Three of those ten jobs (`sqlite`, `engine`, and the core's simulation, twice in a row there) lost a minute or two to "browser: attempt 1 of 3: chromium did not start the program within 1m0s", and passed after the restart; without the waits, `sqlite` is about 24 s, `engine` 8 s, and the core's simulation about 124 s.
- `browser-start`, six runners that each started four runners at once on `internal/sqlite -test.run ^TestOpen`, three rounds: all 72 starts passed.
- `browser-start`, 16 runners of one `go test` on `internal/sqlite`, half with `--disable-gpu --disable-dev-shm-usage --password-store=basic --use-mock-keychain` added (run 44767356151): 2 of 16 first starts did not start the program within a minute, one with the flags and one without, and both passed after the restart. Their reports: "what the runner saw: chromium started after 71ms; no output from the program", no step after it, so Chromium never requested the page. Chromium's output was a stream of `ERROR:dbus/bus.cc:406] Failed to connect to the bus: Could not parse server address: Unknown address type`, failed calls of `org.freedesktop.DBus.NameHasOwner`, and once, 22 s after the start, `Network service crashed or was terminated, restarting service.`
- `browser-start` with `S4QL_CHROMIUM_STEPS=1` on 16 runners (run 44768068077): no hang; the first Chromium loaded the page after 6.7 s to 32.1 s and started the program after 8.3 s to 37.3 s, about a second after the page.

On the lead's message I reported the cause and 786c981 at once; the lead had it cherry-picked onto `main` as its own pull request.

### Why

The numbers decide the shards and the timeouts, and the hang had to be seen before it could be fixed. A restart is safe only while no code of the program has run, which the new `started` step makes sure of.

### What worked

The fake Chromium made every path of the runner testable without a browser and without timing: a fake that hangs fails the start-up limit however slow the machine, and the test of the signal cancels only after the output it waits for has arrived. Printing the runner's report in CI showed in one run where the start stops.

### What didn't work

- My first local split of the core gave no usable numbers: the three parts in parallel took 335 s, 64 s, and 139 s, more than the whole package alone (246 s), because the machine had a load average of 28 from other work (`uptime`: "load averages: 27.90 27.95 26.36"). All further measurements are from CI.
- The first workflow of `browser-measure` filtered its output with `grep -E '^(--- |    --- |ok|FAIL|PASS|browser:|== load)'` and `cut -c1-220`, which dropped the lines of the runner's report after "browser: attempt 1 of 3" and the times at the ends of the long subtest names. The second experiment printed everything but the test lines.
- `git push -q ... | tail -2` hid a failed push ("connect: can't assign requested address" from the machine's network), and I waited on a run of the old commit; `gh pr checks` also failed with "dial tcp 140.82.121.5:443: connect: can't assign requested address" several times. The pushes and the API calls are now retried in a loop and checked with `git ls-remote`.
- The first fake Chromium used `select {}` to hang, which Go reports as "fatal error: all goroutines are asleep - deadlock!" and exits, so the runner saw a crash and not a hang. It sleeps now.

### What I learned

- The hang is in Chromium's start on a fresh runner, before it requests the page, and has nothing to do with the tests or the CPU. It looks tied to the runner's first minute: no restart failed except one in the job where the first start failed twice, and in the old job the three packages started after 30 minutes passed.
- Runners differ in speed by up to 1.7 times: the same subtests of the planted bugs took 40.0 s and 70.0 s, and 9.6 s and 16.4 s, on two runners. `-parallel 1`, which I tried for one of the two, changes nothing on js/wasm, where `GOMAXPROCS` and so the default of `-parallel` are 1 already.
- The first Chromium of a runner needs up to about 37 s to start the program, so a start-up limit much shorter than a minute would kill healthy starts.
- A `-skip` with `|` at the top level is an alternation of whole patterns, each split at `/` into levels (`splitRegexp` in `testing/match.go`), and an empty alternative matches every test. So `'^A$/^b|C/d'` skips the subtests `b...` of `A` and `C/d`, and `'X|'` would skip everything.

### What was tricky

The hang left no trace in the old runner, and `go test` without `-v` prints nothing of a program until it ends. Without the steps, a Chromium that never loads the page and a test that never returns look the same.

### What warrants review

- `/internal/sqlite/browser/main.go`: the loop of `runner.attempt`, which decides between an exit, a stop, the start-up limit, the deadline, and a signal, and when a restart is allowed.
- Run `go test -race -count 20 ./internal/sqlite/browser/` and `make test-browser BROWSERPKGS=./internal/sqlite`.

### Future work

- The cause inside Chromium is not found: what it waits for on D-Bus or in its network service in the first minute of a runner. The restart makes it cost about a minute.
- The job `site` hung twice the same way ("sitecheck: the check did not end within 10m0s", on #70 and in run 44695519186), and `/internal/simulator/sitecheck/main.go` starts Chromium as the old runner did. It needs the same restart; I left it alone, because the site is outside this task.
- The entry "Seen once: CI's job `browser` hung in four packages at once" in `/docs/todo.md` is answered by this step, and the lead updates `docs/todo.md`.

## Step 2: Six shards, more restarts, the same restart in `sitecheck`, and the reviews

**Author:** faster-browser-ci builder

### Prompt Context

**Verbatim prompt:** (excerpts from the lead's messages) "When that pull request is merged, merge `origin/main` into `faster-browser-ci` [...] and go on with the start-up detection and the speed-up. Run your review of the runner fix as part of your branch; if it finds something, fix it there as a follow-up commit." Later: "Give `internal/simulator/sitecheck` the same protection: if Chromium has not fetched the page within the start-up limit, stop it and start again, with the same number of attempts and the same messages as the browser runner [...] Test it with a fake Chromium as you did for the runner. Keep `make site-check` one command." And on #101: "Keep your change to the `browser` job and the runner; do not redo what #101 does for other jobs".
**Interpretation:** Split the tests in Chromium over parallel jobs by the measured times, make the restart hold up against the data, give sitecheck the same restart, and address both reviews.
**Inferred intent:** A browser check of a few minutes that a hang of Chromium can no longer turn into 30.

### What I did

The shards, in `/Makefile` (`make test-browser BROWSERSHARD=i/6`; without it the target runs everything in one `go test` as before), each one `go test`:

1. the planted bugs of a commit: `-run '^TestSimulation_plantedBugs$/^should_find_a_commit'`;
2. the other planted bugs: `-run '^TestSimulation_plantedBugs$' -skip '^TestSimulation_plantedBugs$/^should_find_a_commit|<BROWSERSKIP>'`;
3. `TestSimulation` and `TestFreeRunning` of the core;
4. the rest of the core: `-skip '^(TestSimulation_plantedBugs|TestSimulation|TestFreeRunning)$|<BROWSERSKIP>'`;
5. `simstore`, `internal/sim`, `internal/sqlite`, `faultvfs`, and `engine`, side by side;
6. every other package of `BROWSERPKGS` (`$(filter-out ...)`), today `internal/simulator`.

The patterns are built from three variables, so the skips of shards 2 and 4 are the complements of the runs of shards 1 to 3 by construction. `browserskip` joins a pattern with `BROWSERSKIP` only when that is not empty, since an empty alternative skips every test. In `/.github/workflows/ci.yml`, the job `browser` keeps the builds, vets, lints, and the native test of the simulator, with `timeout-minutes: 10`. The new matrix job `browser-test` runs `make test-browser BROWSERSHARD=${{ matrix.shard }}/${{ strategy.job-total }} BROWSERTIMEOUT=7m BROWSERFLAGS=-v` with `S4QL_CHROMIUM_STEPS=1` and `timeout-minutes: 12`. The target refuses an `n` that is not `BROWSERSHARDS`, so the matrix and the Makefile cannot drift apart.

Coverage: from the `-v` logs of the first sharded run (run 44769115590), the four shards of the core hold 749 test results with distinct names, the same 749 as a full local run of the core, none missing and none extra. Only the parent `TestSimulation_plantedBugs` appears twice, in shards 1 and 2, each time with its own subtests. The other packages run whole in one shard each.

More data on the start, all with `-v` unless said:

- Run 44769115590 (sharded): 4 restarts in 6 shards, one shard twice in a row. Its Chromium after the restarts loaded the page after 276 ms. The browser check took 3 min 43 s.
- Run 44770131148: 24 runners, half with `DBUS_SYSTEM_BUS_ADDRESS=unix:path=/nonexistent/s4ql`. That job had no `-v`, so `go test` dropped the runner's stderr for passing packages, and my count of restarts read 0. The times of `internal/sqlite` show them anyway: 123 s and 129 s, where a start without a hang takes 10 to 38 s. So two runners with the bus switched off needed two restarts each. One runner without it failed after about three minutes, as three hung starts would; `bash -e` ended the step at `go test`, before it printed the log. The shards in that run: 1 restart in 6.
- Run 44773625888: 1 restart in 6 shards. Browser check 3 min 37 s.

So three attempts were not enough, and switching off the D-Bus system bus did not help. Commit 6e9c274 allows six attempts in all. The first gets a minute, because a first start on a fresh runner needs up to 37 s when it works. Each later one gets 30 s, because a restarted Chromium loads the page within 5 s. That is 3.5 minutes at most before a shard fails. `BROWSERTIMEOUT=7m` leaves room for that plus the slowest shard's 3 minutes on a slow runner.

The review of the runner fix (code-review skill, medium) found a real race. The attempt loop's `select` could take Chromium's stop, or the start-up timer, over an exit or a start that arrived in the same instant. It then reported a failure for a program that had exited, or started a program a second time. Commit d0637ed fixes it:

- It checks for an exit first when Chromium stops.
- It keeps whether the program was fetched and whether it began in atomics, and ends the start at the fetch of the program.
- It sets `cmd.WaitDelay` to 5 s, so a child of Chromium that holds its output cannot block the stop.
- It keeps a line buffer per file descriptor, strips the 0x16 byte of `go test -json`, and caps the unfinished line at 1 KiB.
- It counts the deadline from the start of the runner, and stops after one attempt when no number of attempts is set.

The review of the whole branch, fixed in aaa1518, found three more problems that mattered:

- Shard 4 ran two `go test` one after the other, joined with `&&`. A failure in the core then kept `internal/sqlite` from running, and the two binaries' timeouts together could run past the job's. Its three small packages moved to shard 5.
- A Worker that fetched the program and then never started it waited for the deadline. Now it gets as long again after the fetch (`compile`).
- Nits: the runner's directory was left behind when its build failed; the runner's tests did not clear `S4QL_CHROMIUM_STEPS`; and the shards ignore a `BROWSERPKGS` that is not the default, which the Makefile and the README now say.

`sitecheck` (d1fb421, `/internal/simulator/sitecheck/main.go`): `run` starts Chromium in a loop. For each start, it resets the recorded requests and closes a channel when Chromium asks for the check's page. It stops a Chromium that has not asked for the page within a minute (30 s after a restart), or that stopped before. It then writes `sitecheck: attempt i of 6: ...` with the end of Chromium's output, and `sitecheck: starting Chromium again`. `-timeout` now counts from the load of the page. The tests make the test binary a fake Chromium (`hang`, `pass`, `crash-once`). `make site-check` passes locally. I copied the loop instead of sharing a package with the runner. The runner's loop is tied to its per-attempt server, so sharing would have meant reworking the runner that #99 had just merged, in a file that #101 also changes.

`/internal/sqlite/browser/README.md`: a writer sub-agent updated its section "Tests" (the shard variable, the restart and its reason, `S4QL_CHROMIUM_STEPS`, the two CI jobs). I corrected its numbers twice after later data.

Local checks:

- `make check-browser` passed.
- `make test-browser`, all packages in one `go test`, passed in 4 min 5 s at a load average of 34.
- All six shards run at the same time passed twice in a row: 12 shard runs at load averages of 16 to 28, with the same count of passed tests per shard in both rounds.
- `go test -race -count 20 ./internal/sqlite/browser/` and `-count 10` of `sitecheck` passed.
- `golangci-lint` and `simlint` report nothing over the changed packages.

### Why

`internal/core` alone took 11 of the 14 minutes, so the shards had to split it by test name. Its biggest test, the planted bugs, takes 167 to 276 s by itself, so it is split again by its subtests. The restart has to outlast the stretch in which a runner's Chromium hangs, and the data showed that stretch can be longer than three minutes.

### What worked

Measuring each unit alone on CI gave the balance in one run. The shards came out between 1 and 4 minutes of tests each. The complement patterns made coverage a property of the Makefile, and the `-v` logs proved it.

The wall time of the browser check (from the first start to the last end of `browser` and the six `browser-test` jobs) in the three CI runs of the final code: 3 min 29 s (run 44775245837 on aaa1518, no restart), 4 min 26 s (run 44777295258 on 208bdb3, one restart, shard 2 took 247 s on a slow runner), and 4 min 8 s (run 44778189011 on e7a2416, no restart, shard 2 229 s). Before: 14 min 49 s (run 44757417889) and 13 min 57 s (run 44747038667) on `main`. Shard 2, the planted bugs other than those of a commit, is the longest on a slow runner. The job `browser` takes about 3.5 minutes.

### What didn't work

- My first version of the compile limit re-armed the start-up timer only when it fired, so a test with `startup: time.Hour` waited an hour, and `go test -count 20` hung until I stopped it. The limit now starts at the fetch.
- An earlier version of that test used one 10 ms limit for both the fetch and the compile, so it depended on the fake fetching within 10 ms. It failed with "chromium did not fetch it within 10ms" on most of 10 runs. The compile limit is a field of its own now, and the test gives the fetch an hour.
- A matrix job that ran `go test ...; status=$?` under the default `bash -e` lost its output when `go test` failed.
- Counting restarts with `grep -c 'starting Chromium again'` over a job's log also counted the echo of the script itself. The summaries now match `Z browser: starting Chromium again`.

### What I learned

- Runners differ in speed by up to 1.7 times, and a Chromium's first start on a runner loads the page after 7 to 32 s. Shards need about 2x headroom over a fast runner's time.
- The `browser` job no longer runs Chromium, but it is now the longest job of the browser check, at about 3.5 minutes: its native test of the simulator under the race detector takes 75 to 80 s, and the lints most of the rest.

### What was tricky

Two other branches moved under this one. #99 put 786c981 on `main` as its own merge, so the merge of `origin/main` conflicted in the runner's two files. I kept this branch's versions, which build on 786c981, and #99 holds 786c981 unchanged. #101 changes `sitecheck`'s `run` too; `git merge-tree` shows a conflict in `/internal/simulator/sitecheck/main.go` only.

### What warrants review

- `/Makefile`, `test-browser`: that the six cases together are every test, and `browserskip`.
- `/internal/sqlite/browser/main.go`: the loop of `runner.attempt`, now with the compile phase.
- `/internal/simulator/sitecheck/main.go`: the start loop in `run`.
- The temporary commits 4fd3e58, d8c6abc, 0c2128c, and the TEMPORARY part of 5c4d40a added and removed measuring jobs in the workflow. The branch's final `ci.yml` has none of them (`grep -c TEMPORARY .github/workflows/ci.yml` is 0), and a squash on merge drops them.

### Future work

- Shards 1 and 2 split the planted bugs by name. #101 adds `internal/testshard` (`S4QL_TEST_SHARD=i/n`, by index), which would balance them better once both are on `main`. #101's per-job Go cache (`.github/actions/go`) could also serve `browser` and `browser-test`.
- `/docs/todo.md` is not mine to edit: its entry on the browser hang is answered, and the cause of the `site` hang is very likely this one.
