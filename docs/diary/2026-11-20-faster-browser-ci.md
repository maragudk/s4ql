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
