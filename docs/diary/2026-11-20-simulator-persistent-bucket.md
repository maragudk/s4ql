# Diary: a bucket that persists between reloads of the simulator

The simulator on the site, `internal/simulator` served by `site/simulator`, starts over on every reload. Markus: "I'd like the data to persist between refreshes (just like S3 would), with a reset button available." This diary is of the builder that keeps the simulated bucket in the browser.

## Step 1: The brief

**Author:** builder (simulator persistent bucket)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> What persists is what S3 would hold: the bucket's objects (the manifest and the commit objects). What does not persist is everything a machine would lose: the instances' caches, their processes, the faults that are switched on, the clocks' shifts. After a reload, `a` starts on the existing bucket and has to get its data from it, as a real app on a new machine would, and `b` stands by. Do not hide or shortcut what the real code does on an existing bucket.
>
> - No changes to the Go code outside the simulator. One simulator, one code path. No planted bugs. The page stays as it is designed. Few words.
> - The page keeps the bucket in IndexedDB. On start, the page gives the stored objects to the engine with `start`, and the engine puts them in the simulated store before any instance starts. The engine reports every change to the bucket with the answers, also when the answer to the instance is lost or hangs. Each answer's changes go into one IndexedDB transaction, in order.
> - The checker starts from the bucket it is given. The keys of the counter must not repeat keys from before the reload. Fake time continues across a reload; the stop after 24 fake hours counts the session's own age. Check whether a wall clock that jumps back changes behaviour.
> - Reset in the playback bar, styled like the others, with the neutral blip. No confirmation. Works while playing, paused, and after the stop. The stop message says what is now true.
> - Store a version string that the engine reports, from the format versions and a version of the simulator's stored shape. On a mismatch, a refusal by the engine, or a stored log that the checker finds wrong, start fresh and say so in one log line.
> - Growth: measure, and choose a cap by numbers. At the cap, the least surprising thing.
> - Step 0: how the simulated store can be seeded and observed without changing it; what an instance does on an existing bucket with an empty cache, for 100 to 15,000 commit objects, natively and in Chromium; the size of a commit object and the manifest; the time to load N objects from IndexedDB.
>
> A later message from the lead, from Markus: "Just use one bucket but different database prefixes per tab?" One stored bucket, one database prefix per tab; `start` takes the prefix. A tab takes the prefix in `sessionStorage` if its Web Lock is free, else the most recently used stored prefix whose lock is free, else a new one. It deletes older stored prefixes whose locks are free. Reset empties only its own prefix. Without Web Locks: one fixed prefix. Without IndexedDB: in memory.

**Interpretation:** the engine takes a stored bucket (objects, the counter, and the time of the history) and a prefix with `start`, seeds the simulated store through its public API, and reports every applied write; the page keeps the bucket in IndexedDB, per prefix, under Web Locks.

**Inferred intent:** the simulator behaves like an application on S3: the data outlives the machine, and what the code does to get it back is visible.

### What I did

Read `AGENTS.md`, the README of `internal/simulator`, the diaries of the live engine, the SQL, and the page, and the code of `run.go`, `live.go`, `check.go`, `trace.go`, `request.go`, and `simstore`.

### Why

What the reading gave:

- `simstore` has no way to put an object in place, and its faults are fixed at `New`. The only public way in is a write through a `Client`. A client with a nil sender never crashes. `Store.Object` reads an object directly, without faults, so the engine can confirm each seeded object and write it again until it is there.
- The store traces `applied` after it releases its mutex, and the engine already calls `Store.Object` at that line to decode the object. That is the point at which a change is in the store, whatever happens to the answer afterwards.
- `format`'s version constants are not exported. The bytes of a fixed manifest and a fixed commit object carry them, so a hash of those bytes changes with the versions and the encodings.
- The core writes `Lease.Renewed` and never reads it back for a decision (`internal/core/lease.go`: two writes, no read).

## Step 2: The engine on a stored bucket

**Author:** builder (simulator persistent bucket)

### What I did

- `bucket.go`: `StoredBucket` (`version`, `objects` of `{key, content, at}`, `values`, `from`), `StorageVersion` (`1-` and 16 hex digits of a SHA-256 of a fixed manifest and commit object as `format` writes them), `parseBucket`, which refuses what no run could have left (another version, a key outside the prefix, an object that does not read, a log that is not whole or loops, a key in the log twice, a key that the counter has not given, found by undoing the Feistel of `barcode` in `given`), and `seedStore`.
- `request.go`: `prefix` (default `db`) and `bucket` on a request and on the session settings. `run.go` opens the core on `r.req.Prefix`.
- The seed: a client `seed` of `simstore` with a nil sender writes the commit objects, 32 workers side by side, and then the manifest, each until `Store.Object` holds it, with a timeout of 10 s per attempt. `storeEvent` drops request events without a pending request of an instance, and cut-offs of `seed`. Then one event `store`/`seeded` with the manifest's version and `by`, the time of the history at the time 0 of the run.
- The checker gets the log through `checker.seed`; the fold through `fold.pending`, which the event `seeded` puts in, so `StateAt` folds a batch run with a bucket the same way.
- Each answer of a session has `changes` (recorded in `storeEvent` at `applied`, which the store traces after it has applied the write and released its mutex) and `values`; the answer to `start` has `version`.
- Every process shifts its wall clock by `by` when it starts, so wall times, the manifest's `renewed`, and the commit times read as one history.

### Why

- Seeding through the store's own client is the only way in without changing `simstore`; `Store.Object` makes it sound under faults. Versions come out right because the store computes them.
- `by` and not `from`: the seed takes fake time (2 s for 1,000 objects), and the history must go on from `from` when the instances start, not jump by the seed.

### What didn't work

- The first test with every fault expected a commit within 2 minutes of the reload and failed: the holder catches up one download after another, and a download that gets no answer waits for its timeout of 5 min. That is the core, so the test now gives it 10 fake minutes after `calm`.
- A command `cat > file` with no input in a background shell hung, and the edit after it never ran; a test then failed for a reason that was not in the code.

### How to review

`go test -tags s4ql_browser -run 'TestEngine_bucket|TestGiven|TestChecker' ./internal/simulator/`. Read `bucket.go`, then the filter at the top of `storeEvent`.

## Step 3: Step 0, the numbers

**Author:** builder (simulator persistent bucket)

A throwaway test (not committed) grew a bucket with fast writes and started a session on it at each size, with the page's settings; it ran natively and through `make test-browser`. Fake times were the same in both builds.

| Commit objects | Seed, fake | Seed, real native / Chromium | Lease taken | First call served | Catch-up real native / Chromium |
| --- | --- | --- | --- | --- | --- |
| 108 | 0.3 s | 18 ms / 44 ms | 33.7 s | 47.9 s | 29 ms / 103 ms |
| 1,011 | 2.2 s | 326 ms / 355 ms | 35.3 s | 165.1 s | 504 ms / 649 ms |
| 5,002 | 9.9 s | 1.55 s / 1.74 s | 43.1 s | 674.9 s | 2.13 s / 3.32 s |
| 15,025 | 29.4 s | 3.81 s / 6.16 s | 62.6 s | 1,945.7 s | 8.93 s / 14.75 s |

- A commit object is 135 bytes, the manifest 312. The page writes 548 objects, 74 KB, per fake hour.
- `a` waits for the old lease (30 s by its own clocks), then catches up sequentially, two downloads per commit object, about 0.13 s of fake time each. The core has no snapshot, compaction, or deletion.
- sessionStorage, measured with Playwright: Chromium takes 5,242,880 UTF-16 code units, whatever the characters; WebKit takes 5,242,880 characters below 256 but 2,621,440 others (it counts bytes, with Latin-1 strings as one byte a character). Every code unit, also a lone surrogate, round-trips in both. Firefox could not be installed through playwright-cli; MDN says 5 MiB of session storage per origin.
- Loading N objects of 135 bytes from sessionStorage with `Keeper.load`: 0.7 ms for 100, 3.8 ms for 1,000, 4.6 ms for 2,000, 12 ms for 5,000, 36 ms for 15,000 (182 characters each); 2,000 setItem calls take about 4 ms.

## Step 4: Reads only of known keys

**Author:** builder (simulator persistent bucket)

A later message from the lead, from Markus: a select asks only for a barcode that the application knows is in the store. `checker.pick` takes the key among the acknowledged writes, half the newest and the rest one of the 50 newest; a call that would read while none is known is an insert. `checker.seed` adds the stored log to the acknowledged writes, which also closed a hole: before, a read of a stored key at the empty log would have passed every check. The SQL test now requires every read to find its row, the client test expects the first call to be an insert, and the test of writes held back after a crash moved from seed 4 to seed 1, whose run still has one.

## Step 5: The page

**Author:** builder (simulator persistent bucket)

### What I did

- `site/simulator/bucket.js`: `Keeper` of the tab's database in sessionStorage, under `s4ql:name` (`db-` and six characters without look-alikes), `s4ql:meta` (version, counter, time of the history, number of objects), `s4ql:m`, and `s4ql:c:<32 bytes of the name>`. A value is the time, a comma, and the bytes as `atob` gives them. One answer's writes happen in one task with the meta last; a failing write or a pass of the budget drops the stored database and the page goes on in memory.
- `page.js`: `begin` loads the bucket and starts the session with it; a refusal drops it and starts empty, with a line. The clock, the log, and the commit objects show `base + t`. `reset` empties the database, keeps the name, drops any answer still on its way (`gen`), and starts paused at 0, also after a stop, with a new Worker if the old one failed. The page stops at `CAP` commit objects. The stop messages say "Reload to go on with what S3 holds, or Reset to start over." The name is under the letter S3.
- `sound.js` exports `blip`. `sitecheck` loads `bucket.js`, serves a page whose sessionStorage throws (`no-storage`), and checks a reload, damage, another version, a failing write, Reset, the cap, a tab without storage, and an empty sessionStorage.

### Why

- One character a byte: Latin-1 costs one unit in Chromium and one byte in WebKit, against 4/3 for base64. Packing 15 bits into a character would halve it in Chromium but double it in WebKit.
- The cap is 2,000 commit objects, by the time to serve, not by storage: at 2,000 the new `a` serves about 4.5 fake minutes after a reload; 2,000 objects are about 370,000 characters, 7% of the quota. That is 3.6 fake hours of the page's workload.
- A second tab cannot be made inside sitecheck's one tab, whose frames share sessionStorage; the check clears it, as a new tab finds it, and Playwright showed a second tab with another name and no commit objects.

### What didn't work

- The quota page hung in Chrome: my fill loop overwrote the same key forever once a small write succeeded.
- The first sitecheck run took a's role "serves" for the end of the catch-up; a takes the lease about 35 s before it has caught up. The check now waits for the line of the catch-up.
