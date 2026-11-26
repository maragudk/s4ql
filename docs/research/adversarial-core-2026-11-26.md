# Adversarial review of the storage core, 2026-11-26

Two outside models reviewed `internal/core`, `internal/store`, and `internal/format` at commit 474d81fb. Each worked alone and did not see the other's answer. maragubot checked every claim against the code, and reproduced what it could with the simulated store and scheduler.

## Summary

- **codex** (OpenAI, `gpt-6.2-sol`, effort medium, the defaults of its configuration) gave 4 findings: 2 confirmed faults, 0 plausible and not reproduced, 2 true on paper and without effect in practice, 0 already handled, 0 wrong.
- **agy** (Google, Gemini 4 Carbon, effort high) gave 10 findings. Its tenth has three independent parts, which are counted here as three, so 12 claims: 2 confirmed faults, 0 plausible and not reproduced, 8 true on paper and without effect in practice, 1 already handled or recorded, 1 wrong.
- **The two models found no claim in common.** codex looked at the paths between instances: the log, the lease, and recovery. agy looked mostly at what the cache trusts on the local disk.
- **No confirmed fault loses or corrupts what the log in S3 holds.** One confirmed fault acknowledges a transaction whose own swap was never applied, which breaks serializability (a lost update), but it needs two instances whose wall clocks read the same nanosecond. One serves an empty cache as fresh after an operator deletes the cache of a running instance and starts another on its path. One stops a spare for good during a takeover on a shared cache, with the log and the cache both right. One takes a live holder's lease at once, but only with options and a manifest that no instance of s4ql writes.
- **The four confirmed faults have failing tests** in `internal/core/adversarial_test.go`, named `TestAdversarial_*`. They fail on purpose, deterministically, and are to pass once each fault is fixed.

The prompt gave each model the rules of the spec as the properties to break, and asked for at most fifteen findings, each with its sequence of events and its lines of code. codex refused the first two runs at the end as a "possible cybersecurity risk", after about 140,000 tokens of reading each time. The words "adversarial" and "attack" were taken out of the second prompt, and it was refused all the same. A resumed session that asked only for the final answer, called a review of the project's own code for its maintainers, gave it. agy ran for about 45 minutes and changed no file.

## The findings of codex

### codex 1: a cache whose rows are damaged on the disk serves them

- **Class: true on paper, without effect in practice.** The sequence holds: a bit that changes in a row of a checkpointed cache, where SQLite still reads the record, is served as it is, since `Open` checks the schema and the identity and catch-up trusts the position. This is the fault model of the design and not a fault of the code. The cache trusts a local disk that reports its failures, and SQLite has no checksum of its pages either. `internal/core/README.md` (the bullet on statements of the cache) already says that a process that writes the rows of the cache itself goes unseen. Which silent disk failures the simulation injects is open in `docs/todo/storage-core-tests.md` (Nightmare mode).

### codex 2: a takeover on a shared cache stops the new holder

- **Class: confirmed fault, reproduced** by `TestAdversarial_sharedCacheTakeover`.
- **Sequence:** instances a and b of one database run on one cache, which `TestOpen_cacheDir` in the root package supports. a's manifest swap of commit C is applied, and a then stalls before the commit of the cache for longer than its lease, with no renewal reaching the store. b takes the lease, reads the position of the cache, which is C's parent P, and plans to replay C (`catchup.go:30`, `catchup.go:79`). a then commits the cache at C. b's replay takes the write lock and finds the cache at C, not at P (`replay.go:90`), and stops b for good with `ErrLogBroken`.
- **The test** wraps a's host so that the yield `core: a commit between the manifest swap and the cache` (`tx.go:1424`) sleeps a minute, and drops a's swaps from then on. b stops with "the commit object 9d2f… has the parent d9fb…, and the cache is at 9d2f…".
- **Effect:** availability only. a's commit is in the log and in the cache, and a acknowledged it rightly. A pause of the whole process that long is rare, but a VM migration or a stopped container makes one.

### codex 3: two transactions with the same bytes are both acknowledged

- **Class: confirmed fault, reproduced** by `TestAdversarial_identicalCommits`.
- **Sequence:** a and b begin at the same parent P, make the same change (`t(k=1, v=1)`), have no annotations, and read the same wall clock, so their commit objects have the same bytes and the same name C (`format/writer.go:105`, `tx.go:1109`). a uploads C, and its swaps get no answer. b takes the lease after a's has run out, finds C there already (`ErrExists`, which `tx.go:1524` takes as success), and swaps the manifest to C. a reaches the store again, its swap is refused, the manifest names b, and `Tx.inLog` finds C in the log (`tx.go:1638`, `tx.go:1663`) and returns published. Both `Commit` calls return nil, from one swap.
- **The test** stops both wall clocks at the same instant with `sim.Instance.StopWall`. The store holds two commit objects at the end, the create of the table and C, which shows that the names are equal.
- **Effect:** a lost update. If both transactions read 0 and wrote 1 as an increment, the counter rises once for two acknowledged commits. The spec's rule "The transaction is committed if and only if its commit object is in the log" assumes that a name identifies one transaction, and the bytes hold nothing that is unique to one. The case needs the two wall clocks to read the same nanosecond, while b's clock is behind a's by the lease and the margin. That does not happen with clocks that run, but it does with a clock that stands still or has a coarse resolution, and nothing in the format rules it out. The simulation's workloads put a random id into every annotation, so they never write two commit objects with the same bytes.

### codex 4: an object at the key of a commit is taken for the commit object

- **Class: true on paper, without effect in practice.** `Tx.upload` takes `store.ErrExists` as success without reading the object (`tx.go:1524`). An object with other bytes at that key needs a SHA-256 collision, or a tool outside s4ql that wrote under the commit prefix. The core writes every commit object with its checksum, and the store refuses bytes that do not match it (`s3store.go:336`). If it happens, catch-up stops loudly with `ErrNameMismatch` and does not apply wrong data. A check of the existing object's checksum on `ErrExists` would close it cheaply.

## The findings of agy

### agy 1: a reader opens a new file at the path of the cache

- **Class: confirmed fault, reproduced** by `TestAdversarial_replacedCache`.
- **Sequence:** the files of a running holder's cache are deleted, and a second instance of the same database is opened on the same path. It makes a new, empty cache there and waits as a spare. The holder's next `DB.Reader` opens the file at the path (`read.go:51`), and `Reader.Read` fixes its state there and checks only the lease (`read.go:154` to `read.go:170`). The read serves the empty cache, at `EmptyLog`, as fresh, without the holder's acknowledged commits.
- **Effect:** a stale read, after an operator error. `internal/core/README.md` (the bullet "Only the write connection makes the cache") deals with a deleted cache, which `CANTOPEN` stops, but not with a cache that another instance makes at the path afterwards. A read could compare its position with the position that the write connection last committed, which the instance knows in memory. agy also named a cache of another database at the path. That is the same path through the code, and the test covers the first case only.

### agy 2: a manifest damaged on its way is written back

- **Class: true on paper, without effect in practice,** and already recorded as open in `docs/todo/storage-core.md` ("a manifest that is damaged on its way"). Every manifest that s4ql writes carries its checksum (`lease.go:76`, `s3store.go:336`), and a read asks for it and has the SDK compare it (`s3store.go:264`). Damage then needs a link without TLS, and a manifest that a tool outside s4ql wrote without a checksum. The shortened duration (`"30s"` to `"3s"`) that agy describes parses, as `parseDuration` would take it.

### agy 3: a store that checks no checksum on upload acknowledges a damaged commit object

- **Class: true on paper, without effect in practice.** It needs a store that checks neither the SHA-256 that signs the payload nor `x-amz-checksum-sha256`, and a bit that changes between the spool file and the store. Then the commit is acknowledged, and every catch-up stops on `ErrNameMismatch`, with nothing wrong applied. agy is right that `docs/todo/s3-client.md` says too little where it says that a store which does not check the payload's hash "still works, without the second guard": on such a store a damaged upload is acknowledged and then cannot be read.

### agy 4: a scan of a write transaction goes on after a stop

- **Class: true on paper, without effect in practice.** `Tx.scan` checks `tx.marked` between rows and not `stoppedError` (`index.go:1166`), where `ReadTx.scan` checks the stop (`index.go:1071`). So the rows of a running scan keep coming after the instance stops and releases its lease. They are the transaction's own snapshot from a state that was current when it began, and the transaction can no longer commit (`Tx.Commit` checks `usable` at `tx.go:1368`), so no read is stale in the sense of the spec. The cost is that `DB.Close` waits for that scan until its context ends, and that the lean "Running reads fail" (`internal/core/README.md`, Stopping) does not hold for it. agy's claim that the rows are stale is wrong.

### agy 5: an overflow in `DB.acquire` takes a live lease at once

- **Class: confirmed fault, reproduced** by `TestAdversarial_watchOverflow`.
- **Sequence:** the manifest holds a lease of 9e18 nanoseconds (about 285 years), and an instance is opened with `Options.Watch` at the largest duration. `withDefaults` bounds the lease at a year and `Watch` only from below (`core.go:124` to `core.go:129`), and `ParseManifest` takes any positive duration. In `DB.acquire`, the time left is below `Watch`, and `left + margin(duration)` (`lease.go:364`) overflows to a negative duration, so the instance takes the lease at once.
- **Effect:** none in practice. No instance of s4ql writes a lease of more than a year, and nobody sets `Watch` to centuries. The comment at `lease.go:360` says that no sum leaves the range, which is false for such a `Watch`. An upper bound on `Watch` fixes it.

### agy 6: a cache at `EmptyLog` with tables is accepted

- **Class: true on paper, without effect in practice.** Only `conn.make` refuses tables in a cache without a position (`cache.go:422`). A cache whose record says `EmptyLog` and that holds a table passes `conn.checkObjects` (`cache.go:337`), and catch-up replays the whole log over it. A commit that then touches the table puts it into the log, and replay elsewhere fails with `ErrLogBroken`. The premise is a program other than s4ql that writes the cache, which the design does not defend against beyond the checks at `Open`. A check at `Open` that a cache at `EmptyLog` holds no table would close this case.

### agy 7: a cache file in UTF-16 is accepted

- **Class: true on paper, without effect in practice.** Nothing checks `pragma encoding` (`cache.go:267` to `cache.go:311`), so an empty SQLite file made in UTF-16 at the path of the cache gets the table `s4ql_cache` and is used, and SQLite's `BINARY` order of text then differs from UTF-8's. The premise is again a file that another program made. A check of the encoding at `Open` is one line.

### agy 8: a foreign table with a foreign key makes replay report a broken log

- **Class: true on paper, without effect in practice.** The build turns foreign keys on (`internal/sqlite/options.h:27`), and `conn.checkObjects` takes any table (`cache.go:337`), which `conn.load` checks only at its first look-up. A table added by another program, with a row that references a table of the database, makes a delete or an `insert or replace` in that table fail with `SQLITE_CONSTRAINT_FOREIGNKEY`. Replay wraps it in `ErrLogBroken` (`replay.go:117`), and a write transaction fails without a stop. The cause is a foreign writer, the error then names the wrong culprit, and no wrong data is applied.

### agy 9: an I/O failure on the memory-mapped `-shm` kills the process

- **Class: already recorded** as open in `docs/todo/wal-index.md`, with the parked answer of the `unix-excl` VFS. It is counted under "already handled" here, although nothing handles it yet.

### agy 10a: a smaller `Options.LengthLimit` makes replay report a broken log

- **Class: true on paper, without effect in practice.** The doc comment of `LengthLimit` (`core.go:73` to `core.go:79`) says that nothing but tests sets another limit, so no two instances in use differ in it.

### agy 10b: replay takes a table with more than 250 key columns

- **Class: true on paper, without effect in practice.** `Tx.CreateTable` refuses a key of more than 250 columns (`tx.go:457`), and replay does not check it, as `applyCreateIndex` does for an index. The premise is a commit object that no version of s4ql writes. The table would then fail at its first get with an error of SQLite, which is loud.

### agy 10c: the statements of a transaction do not look at the clocks

- **Class: wrong.** This is the design, recorded in `internal/core/README.md` ("`Tx.Serving` and `ReadTx.Serving` look at the clocks"): the engine calls `Serving` before each statement. A read transaction fixes its state while the lease is valid (`read.go:163`), so what it reads later is a state that was current at its start, which the spec allows.

## What was not covered

- **The SQL engine and the driver,** which were out of scope.
- **Real S3.** Nothing ran against a bucket. The claims about Tigris and AWS rest on `internal/store/s3store/README.md`.
- **Real parallelism.** The reproductions run on the simulation's scheduler or in a bubble of `testing/synctest`. SQLite's busy wait holds the scheduler, so the test of the shared cache sets the busy timeout to a millisecond and lets catch-up try again.
- **The claims that rest on a foreign writer** (agy 6, 7, and 8) were checked by reading the code, not by building such a file.
- **The second opinions' own breadth.** codex ran at effort medium, its configured default, and each of its runs read about 140,000 tokens before it answered. A run at a higher effort, or one on a narrower scope such as `lease.go` and `Tx.publish` alone, may find more.
