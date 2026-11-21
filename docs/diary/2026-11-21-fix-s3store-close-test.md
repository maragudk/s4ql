# Diary: The tests of `s3store.Client.Close` wait for the close that net/http does in a goroutine of its own

A subtest of `TestClient_Close` from pull request #104 failed in CI's sanitizer job on an unrelated pull request. This is the fix, on the branch `fix-s3store-close-test` from `main` after #109.

## Step 1: Find the cause, wait for the event, and harden the other subtests

**Author:** fix-s3store-close-test builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "A test that landed yesterday with pull request #104 failed in CI's sanitizer job [...] `TestClient_Close/should_return_at_once_from_a_close_during_a_write,_which_succeeds,_and_close_its_connection_when_it_has_ended` [...] at `s3store_internal_test.go:215`, "Not true" [...] Fix the test so that it passes or fails for the same reason on every machine: wait for the event itself [...] with a bound only from the side a slower machine cannot break [...] no sleep as synchronisation. If the cause is not the test but the client [...] that is a product fault [...] Go through every other subtest of `TestClient_Close` and `TestOpen_s3`, and the stress test after `Close`, for the same weakness, and fix what you find."
**Interpretation:** Find out why the connection was still open when the write returned, decide whether the test or the client is at fault, and make every check of a close in these tests wait for the close itself.
**Inferred intent:** The sanitizer job, and any slow or loaded machine, must not fail the tests of the close for a reason that is not a fault of the client.

### What I did

**The cause.** Line 215 was `is.True(t, isClosed(conns[0]))` right after the write returned. `Client.Close` during the write calls `CloseIdleConnections`, which sets `closeIdle` in the transport. When the answer comes in, the read loop of net/http (`transport.go`, the branch `if !hasBody || bodyWritable`) tries to put the connection back in the pool before it hands the answer to the call. `tryPutIdleConn` refuses because of `closeIdle`, so the read loop hands the answer to the call, leaves its loop, and closes the connection in its deferred `pc.close`. The call returns, and the check of the test runs, while that goroutine may not have run yet. The client keeps its promise: the connection is closed, only a little after the call returns. That makes it a fault of the test, not of the client.

Reproduced on the old test: `-race -count=1000` of the subtest failed 10 times, all at line 215. Without `-race`, `-count=2000` failed 5 times with GOMAXPROCS 2 and 8, and none with 1. Under `-asan` in Docker (`golang:1.27.3`), `-count=5000` with `GOMAXPROCS=2` failed 5 times, all at line 215.

**The fix.** The dialer of the tests wraps each connection in a `conn` whose `Close` closes a channel. `isClosed` reads that channel without blocking, and `waitClosed` waits on it for each connection it is given, bounded at 30 seconds, far below the idle timeout of 90 seconds after which a leak would be closed too. Every subtest that says a connection is closed now waits for it, and none counts the connections open at once after a call.

**The other subtests.** Every subtest that checked a close at once had the same weakness: net/http also keeps a connection out of the pool, and closes it itself, when its write loop has not reported the end of the request 50 milliseconds after the answer came in (`persistConn.wroteRequest`, `maxWriteWaitBeforeConnReuse`). So:

- "should close every connection [...] also those that are idle" no longer checks that both connections are open before `Close`, and waits for both to close after it.
- "should close the idle connections while the content of a read is open" finds the read's connection through `httptrace.ClientTrace.GotConn`, waits for every other connection to close while the content is open, checks that the read's connection is still open, and waits for it after the content is closed. Its name loses "at once".
- "should send a call after the close" waits for the one connection to close.
- The subtest of the held dial gets the connection from the dialer on the channel `dialed`, and no longer polls `d.conns` with a sleep.

The first run of `-count=300` beside a `-race` run and a GOMAXPROCS run of the same tests failed in the stress subtest with `dial tcp 127.0.0.1:63988: connect: can't assign requested address`, and then hung for 7 minutes, until I sent it SIGQUIT. The client closes a connection after almost every call of a closed client, each active close left a local port in TIME_WAIT, and three runs side by side used up the ephemeral ports of macOS. The hang came next: one of the two writes of the first subtest failed to dial, its `is.NotError` ended its goroutine, and the handler held the other write for ever in `arrived.Wait()`. The fixes:

- The dialer sets `SetLinger(0)` on each connection, so a close resets it and leaves no port in TIME_WAIT. 200 runs of the five subtests that dial added no TIME_WAIT (`netstat -an -p tcp | grep -c TIME_WAIT` went from 2799 to 2119 as others expired).
- `pair` holds the two writes in the handler until both have arrived or one has failed. `createTwo` collects the errors and checks them on the test goroutine.

`TestOpen_s3`, in the root package, already waits on `http.Server.ConnState` with a bound of 30 seconds. It can read 0 open connections before the server has counted a late one, which is a false pass and never a false failure. I left it alone, also because the brief keeps this work inside `internal/store/s3store`. The stress subtest has no timing in it. Its only weakness was the ports.

### Why

A test that checks a close made in another goroutine has to wait for that close. The bound is on the side that a slow machine cannot break: a slower machine makes the close later by milliseconds, and a leak lasts 90 seconds.

### What worked

The fix turned out to be in the test only. With the call of `CloseIdleConnections` taken out of `Client.Close`, the first subtest and the subtest of the open read failed after their 30 seconds. With it taken out of `Client.leave`, the subtest of the call after the close and the subtest of the held dial failed. That is the same coverage as the diary of 2026-11-20 shows.

With load on the machine (the runs side by side, and `make test`), these runs were made on the second commit. The third commit only changes the send on `dialed`, drops an assertion, and renames a subtest, and it passed `-count=50`.

- `-count=300` of `TestClient_Close`: passed.
- `-race -count=100 -timeout 40m`: passed.
- `-asan -count=30` in Docker: passed in 113 s.
- The five subtests that dial, at `-count=2000` with GOMAXPROCS 1 and with 2: passed.
- The subtest of the write, at `-asan -count=5000` with `GOMAXPROCS=2`: passed. The old test failed 5 times there.

`make test`, `make lint`, and `make check-browser` passed.

### What didn't work

The first `make lint` exited 2 while three test runs loaded the machine. I did not keep its output, and the next two runs passed.

The permission system refused three commands as "too complex to verify that it stays inside the worktree": a Python here-doc that would have rewritten part of the test file, and two lines that chained `git checkout` with `docker run`. I used the Edit tool for the first, and split the others into separate commands.

### What I learned

net/http closes a connection in its read loop, after it has handed the answer to the call, in two cases: the pool refuses the connection after `CloseIdleConnections`, or the write loop has not reported within 50 milliseconds. A test can only learn of such a close by waiting for it.

Without linger 0, a test that churns connections on loopback can run macOS out of ephemeral ports when other runs go beside it. Linux reuses TIME_WAIT ports on loopback, so CI did not show this.

### What was tricky

The hang hid the failure that caused it. Only the goroutine dump from SIGQUIT showed one write left in `persistConn.roundTrip`, and the first line of the output showed the failed dial.

### What warrants review

- The first subtest no longer checks that the connections are open before `Close`. If net/http kept both out of the pool, the subtest would pass without `Close` closing them. That needs both writes to hit the 50-millisecond case, and the fault injection above shows that the subtest catches a `Close` that does nothing.
- The code review (medium) found that the doc comment of `Client.Close` says that a connection "is idle when the last request ends and closed then", which leaves out the case of this failure. Its last sentence, about the transport closing each connection that becomes idle after `CloseIdleConnections`, covers that case. I left `s3store.go` unchanged to keep this a change to tests only.

### Future work

None.
