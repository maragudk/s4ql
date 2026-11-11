# SQLITE_BUSY for a single writer among readers

Date: 2026-11-11. Tags: **V** = verified in this run (tested, or read in source or docs, with the reference). **I** = inferred.

Question: why does SQLite return `SQLITE_BUSY` to a single writer in WAL mode when only readers run beside it, in which other ways can `SQLITE_BUSY` or a lock wait reach s4ql, and what are the options?

Short answer: a reader that starts while the writer commits can read the WAL-index header half-updated, and then takes the WAL write lock for a moment to read it again. If the writer's next `begin immediate` arrives in that window, it gets `SQLITE_BUSY` at once. A busy timeout on the writer removes it. A lock in Go around the write connection does not.

Setup: SQLite 3.54.1, the vendored amalgamation `internal/sqlite/c/sqlite3.c`, compiled with `internal/sqlite/options.h`. Line numbers refer to the unmodified vendored `sqlite3.c`.

Method: a C reproduction of the spike's experiment (8 reader threads running `begin; select; commit`, 1 writer running `begin immediate; update; commit` 3000 times), and an instrumented copy of the amalgamation that records, under the shm mutex in `unixShmLock`, who holds shm lock 0 (`WAL_WRITE_LOCK`), from which call site, and why that thread's last `walIndexTryHdr` failed. A counterfactual switch makes a reader that sees a dirty header retry instead of taking the write lock. Linux runs used Docker (golang:1.27.3, gcc, aarch64). The harness and the instrumentation were not published.

## 1. Reproduction (V)

- Go binding, existing spike test, macOS: `begin immediate` failed 17, 103, 16 times per 3000.
- Go binding, temporary test (deleted): the error is `code 5 "database is locked"` on `begin immediate`,
  extended code 5 exactly (not 261/517), with `SQLITE_OPEN_EXRESCODE`. 32, 34, 32 per 3000. With
  `busy_timeout = 1`: 0, 0, 0.
- C harness, macOS, 5 runs: 68, 57, 63, 36, 125. Linux container: 372, 182, 341, 389, 334.
- Readers: no error of any kind on begin, select or commit in any steady-state run (about 100 runs of
  100k-3M reads each, including checkpoints FULL/RESTART/TRUNCATE every 100 writes).

## 2. Root cause (V)

Instrumented run (macOS, three runs, writer failures 38, 52, 70):

```
WRITE busy: requester writer at 70682; holder reader at 69668 (holder's last TryHdr failure = dirty read) x38 / x52 / x70
```

The count equals the writer's `begin immediate` failures in every run, and the origin tracker says each plain
BUSY at `w.begin` came from the shm lock (ofst 0, n 1). Linux: 192 = 192. Counterfactual
(`noreaderlock=1`): 0 failures in 3 runs on macOS and 1 on Linux; the reader then never takes WRITE except
once at startup (isInit).

Mechanism:
1. The writer commits: `walFrames` calls `walIndexWriteHdr` (call at 71217, function at 67846) while holding
   WAL_WRITE_LOCK. It writes `aHdr[1]`, a barrier, then `aHdr[0]` (67855-67857).
2. A reader starting a read transaction (`walTryBeginRead` 69974 -> `walIndexReadHdr` 69614) reads both copies
   without a lock in `walIndexTryHdr` and sees them differ: `return 1; /* Dirty read */` (69574).
3. `walIndexReadHdr` 69656-69668: "If the first attempt failed, it might have been due to a race with a writer.
   So get a WRITE lock and try again." `walLockExclusive(pWal, WAL_WRITE_LOCK, 1)`. If the writer still holds
   it, the reader gets BUSY, which `walTryBeginRead` turns into WAL_RETRY (70043-70068), invisible to the app.
   If the writer has just released it, the reader takes it, re-reads the header, and releases it (69690).
4. If the writer's next `begin immediate` reaches `sqlite3WalBeginWriteTransaction` (70657) in that window,
   "Only one writer allowed at a time. Get the write lock. Return SQLITE_BUSY if unable." (70679-70685).
   In-process, `unixShmLock` returns BUSY at once when a sibling connection holds the lock (46092-46098).
5. With busy_timeout 0 there is no handler, so `btreeBeginTrans` (loop ends 76497-76498) returns SQLITE_BUSY.

Measured in one macOS run: 549 dirty header reads by readers; 401 of their WRITE requests found the writer
holding it (retried internally); 135 succeeded; 38 of those collided with the writer's begin.

## 3. Conditions (V, 5 runs each unless stated, writer failures per 3000)

| Condition | macOS | Linux |
|---|---|---|
| baseline | 36-125 | 182-389 |
| wal_autocheckpoint=0 | 32-119 | 212-439 |
| manual PASSIVE every 100 | 47-77 | |
| manual FULL/RESTART/TRUNCATE every 100 | 29-125 (+ checkpoint itself BUSY 12-30 of 30) | |
| readers reopen a connection per read | 20-154 | 9-61 |
| readers without explicit transaction | 111-300 | 529-1091 |
| synchronous=normal / off | 28-123 / 28-59 | 88-261 (normal) |
| deferred begin | failure moves to the `update`, 14-114 | 136-201 |
| begin exclusive | 57-89 | |
| 1 reader | 0 (x3000); 85, 532, 0, 0, 0 (x30000) | |
| 2 / 4 / 16 readers | 3-18 / 12-84 / 119-258 | 0-20 (2) |
| vfs=unix-excl | 72-131 (instrumented: same mechanism, 90=90) | 128-211 |

Nothing in that list removes it. Changes that remove it (0 in all 5 runs): writer `busy_timeout=1` (macOS and
Linux), `=1000`; a reader/writer gate; counterfactual patch. A spinning busy handler (sched_yield, no sleep):
spin=1 6-13, spin=3 0-1, spin=10 0 on macOS, but 4-26 on Linux, so not reliable.

Latency: with busy_timeout on the writer, the largest `begin immediate` was 1.5-1.6 ms typically, 4.6 ms once
(macOS), 3.8 ms (Linux); 8-30 begins per 3000 took over 1 ms (one 1 ms sleep). Without a timeout the max is
0.1-0.2 ms. With 16 readers on 10 cores, timeout 0 saw single 35 ms and 73 ms begins (cause not traced; I:
descheduling or the read-retry sleeps in `walTryBeginRead`).

## 4. Other ways BUSY or a lock wait reaches us

| Case | When | Code | Retry? | Status |
|---|---|---|---|---|
| Reader takes WRITE after a dirty header read | any commit racing a read start | 5 on writer `begin immediate` (or the first write statement with deferred begin) | yes, the transaction never started | V |
| Recovery | first connection after a crash, or the first statement after all connections closed (fresh -shm, isInit=0) | 261 BUSY_RECOVERY on the first statement of other connections, readers and writer; measured 25062 frames (100 MB WAL) recovered in about 20 ms, warm cache, macOS; others waited 24 ms with a timeout | yes | V |
| Last connection closing | `sqlite3WalClose` takes an EXCLUSIVE db-file lock to checkpoint and delete the WAL (69473-69483) | 5 on the first statement of a new connection; origin: file lock want SHARED, have NONE (7008 times in a churn run) | yes | V |
| BUSY_SNAPSHOT | read transaction upgraded to write after another connection committed | 517, returned at once despite busy_timeout 1000; repeating the statement in the same transaction gives 517 again; after rollback it works | only after rollback and restart; with one writer and `begin immediate` it cannot occur (I) | V |
| BUSY_SNAPSHOT converted | upgrade with no transaction open yet: changed to plain 5 (76477-76483) | 5 | yes | V (source) |
| Checkpoint FULL/RESTART/TRUNCATE | readers hold read marks | checkpoint returns 5 (12-30 of 30 with timeout 0; 1 of 30 even with timeout 1000, which then waits for readers) | yes, or use PASSIVE | V |
| PASSIVE / auto checkpoint | | no error seen; the autocheckpoint result is ignored (`sqlite3WalDefaultHook` 190856) | n/a | V |
| SQLITE_PROTOCOL | `walTryBeginRead` gives up after 100 WAL_RETRYs (WAL_RETRY_PROTOCOL_LIMIT 69917, check at 70006-70010), delays grow to 323 ms, "less than 10 seconds" in total | 15 | rescode says it should be very rare; I: indicates a rogue process or pathological contention | V (source), never seen |
| locking_mode=EXCLUSIVE held by another connection | | every other connection gets 5 | no | V (test and wal.html) |
| BUSY_TIMEOUT 773 | only with SQLITE_ENABLE_SETLK_TIMEOUT, not in our build | | | V (source) |
| LOCKED 6 | same-connection conflict; shared cache is omitted | | programming error | V (docs) |
| Reader WAL_RETRY loop | readers retry internally on BUSY (70043-70068) and on read-mark races; never surfaced in any steady-state run | | | V |

Busy handler coverage (V, source): `btreeBeginTrans` invokes the busy handler for `(rc&0xFF)==SQLITE_BUSY`
only while no transaction is open (76497), so it covers 5 and 261 at transaction start, not 517 inside a
transaction.

## 5. Options

- **busy_timeout on the writer** (binding: nothing, `Exec("pragma busy_timeout = N")` works today). Default
  handler sleeps 1, 2, 5, 10, 15, 20, 25, 25, 25, 50, 50, 100 ms (190109-190110) until the total would exceed
  N. Steady-state cost measured above: about 1.5 ms on less than 1% of begins. Worst-case wait is N; the
  holder in steady state holds WRITE for a header re-read only, but during recovery for the whole WAL scan.
- **busy_timeout on readers**: needed only for recovery and last-close cases. No measurable cost in steady
  state (5 runs with rtimeout=1000: no errors, normal throughput). With it, the churn case had 0 errors.
- **Go retry loop on `begin immediate`**: works (the harness retries `begin immediate` immediately; a failed
  begin left no transaction open: no other error appeared). A tight loop without sleep is the spin handler,
  which was not reliable on Linux. Binding: nothing; the engine needs the primary code (has `Code.Primary()`).
- **C busy handler that spins then sleeps**: needs a static C function in the preamble and
  `sqlite3_busy_handler` (no //export needed). Not reliable alone on Linux at 10 spins.
- **Gate in Go** (RWMutex; readers RLock around the step that starts the read transaction; writer Lock around
  `begin immediate` only): 0 failures, writer begin max 20-130 us. Cost: writer begin waits for read-starts in
  flight, read starts wait for a writer begin; read bodies stay parallel. glibc's reader-preferring rwlock
  starved the writer on Linux (3.0 s vs 1.4 s); Go's RWMutex blocks new readers once a writer waits (I: from
  sync docs). It depends on an internal detail (only `walIndexReadHdr` makes a reader take WRITE); a future
  SQLite version could add another path, and it does not cover recovery or last-close. Binding: the engine
  must know which step starts the read transaction.
- **unix-excl VFS**: verified that it does not remove the race (same mechanism, 72-211 failures). What it
  does (V, source 42437-42446, 45600-45604, 45869; tests on macOS and Linux): the first lock takes a POSIX
  write lock on the whole shared range of the db file, held until the last connection in the process
  closes; in-process connections coordinate in memory (2 connections and 9 connections worked); no -shm file
  is created, the wal-index is heap memory (`sqlite3_malloc64`), so no mmap of -shm and no SIGBUS from it (I
  for the signal part); every other process, including the vendored `sqlite3` shell with any VFS and with
  `.timeout 500`, gets "database is locked (5)" while the process has a connection open; after close the
  shell reads it. If every connection closes, the heap wal-index is freed and the next open recovers from
  the WAL (I, source). Binding: Open needs a VFS name (4th argument of `sqlite3_open_v2`).
- **locking_mode=EXCLUSIVE**: removes shm entirely but locks out every other connection (V: readers got 5
  forever), so it rules out parallel readers.

## 6. Classification of primary codes

Facts per code are on https://sqlite.org/rescode.html. Classes: IO = local I/O or environment failure, stop
the instance; T = transient, retry is right; P = programming error (ours or the binding's); C = caller's SQL
or data, return to caller; N = not an error / not returned.

0 OK N; 100 ROW N; 101 DONE N; 1 ERROR C (generic SQL error) or P; 2 INTERNAL P (SQLite bug), stop;
3 PERM IO; 4 ABORT C (callback or rollback aborted a statement); 5 BUSY T at transaction start, P if seen
where the design says impossible after the timeout; 517 BUSY_SNAPSHOT T only by restarting the transaction;
6 LOCKED P; 7 NOMEM resource failure, see recommendation; 8 READONLY IO for a cache opened read-write
(permissions changed; READONLY_DBMOVED = file unlinked or renamed), P otherwise; 9 INTERRUPT N/C (only if
we add sqlite3_interrupt for cancellation); 10 IOERR IO (all extended, including SHMMAP/SHMSIZE/SHMOPEN);
11 CORRUPT IO, and reopening will not fix it; 12 NOTFOUND P; 13 FULL IO (also max_page_count reached);
14 CANTOPEN IO; 15 PROTOCOL T per docs, see recommendation; 16 EMPTY N (unused); 17 SCHEMA T (prepare_v3
reprepares; surfaces only after repeated changes); 18 TOOBIG C; 19 CONSTRAINT C; 20 MISMATCH C;
21 MISUSE P; 22 NOLFS IO; 23 AUTH P (no authorizer); 24 FORMAT N (unused); 25 RANGE P; 26 NOTADB IO
(cache file is not a database); 27 NOTICE, 28 WARNING N (log only; 283 NOTICE_RECOVER_WAL seen in the log).
