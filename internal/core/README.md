# Storage core

`internal/core` is the storage core of s4ql. A `DB` is one instance of one database: a cache in SQLite, a lease in the manifest, and commits through a manifest swap. `Open` does not wait for the lease, and `DB.Wait` does. `DB.Begin` gives a write transaction, a `Tx`, and `DB.Read` gives a read transaction, a `ReadTx`. The package starts one goroutine, `DB.leaseLoop`.

What the package does in each case is in the code:

- the package comment and the doc comments of `Options` and `DB` in `core.go`
- the doc comments of `Tx.Commit` and of `Tx.spoolCommit`, with the net effect of a transaction, in `tx.go`
- the doc comment of `DB.Read` in `read.go`, with the check of the lease before a read
- the doc comments of `DB.acquire`, `DB.resumeLocked`, `DB.renewLocked`, `DB.observe`, and `flight` in `lease.go`, and of `margin` in `core.go`, for the lease and for a swap whose outcome is unknown
- the doc comments of `DB.catchUp` and `DB.fetch` in `catchup.go`, and of `replay` in `replay.go`
- the errors and the doc comment of `classify` in `errors.go`
- the names of the events and of their attributes in `telemetry.go`
- the planted bugs in `bugs.go`

This file holds the reasons behind the design, what was chosen and what was rejected, what was measured, and what the package cannot do. The rules that the package follows are in [docs/spec.md](../../docs/spec.md) under "Concurrency" and "Constraints", and this file does not repeat them. Until the core was built, the reasoning behind those rules was in four items of [docs/todo.md](../../docs/todo.md). That reasoning is now here, with Markus's words and their dates. What is still open or not built is in the todo, under [The storage core](../../docs/todo.md#the-storage-core) and [Tests of the storage core](../../docs/todo.md#tests-of-the-storage-core).

The core was built on 2026-11-12 and 2026-11-13, and merged in pull request #12. It is the first code that uses the simulation harness ([internal/sim/README.md](../sim/README.md)), the storage formats ([internal/format/README.md](../format/README.md)), and the object store in `internal/store` together. The diary is [2026-11-12-storage-core.md](../../docs/diary/2026-11-12-storage-core.md).

## The interface

The future SQL engine is the only user of the package, and the interface is made for it.

- **Every call that can fail takes a context.** A call that waits ends when its context ends, and a call logs its events with its context.
- **A table is an `s4qlpb.CreateTable`, and a value is an `s4qlpb.Value`.** They are the messages of the commit object, so nothing is converted between the engine and the log.
- **A scan is an `iter.Seq2`.** It runs on the goroutine of the caller, so a scan starts no goroutine.
- **The names of tables are compared as SQLite compares them:** ASCII letters without regard to case, and every other character exactly (`fold`). The cache is SQLite, and the oracle is SQLite. A table keeps the spelling of its create.
- **A primary key has at most 250 columns.** A row is found by a condition with a term for each column of its key, and SQLite refuses a condition that is nested deeper than a thousand levels, which about 500 terms are. SQLite itself allows a key of 2000 columns, so this is a restriction of the build. maragubot accepted it on 2026-11-13.
- **The core checks the storage class of every value itself,** in a write and in replay (`table.checkValue`). A `STRICT` table does not refuse every wrong value: for an `INT` column, SQLite converts the text `'1'` and the real `1.0` to an integer without an error. This is a choice of the build, and maragubot accepted it on 2026-11-13.

## Concurrency

- **There is no state machine.** The package is plain blocking Go. Each caller's goroutine does its own work from top to bottom, and the one goroutine of the package renews the lease. The rest of the concurrency comes from the callers. The spec once asked for a state machine that does no I/O, and that sentence was deleted with Markus's agreement on 2026-11-10 and 2026-11-11. The reasons: a state machine does not solve the hard part, which is the order goroutines run in; it fits interactive transactions poorly; and FoundationDB, whose testing this project follows, runs plain coroutine code on a deterministic scheduler and has no such state machine. The design was to be judged by building on it. Markus on 2026-11-11: "I think we should just try it out."
- **No read starts on a cache that lacks an acknowledged commit.** If the cache fails after the manifest swap, `Tx.Commit` stops the instance before it returns, and no read starts after the stop. A read can still start while SQLite fails and before the core sees the error. That read sees the state before the commit, which is allowed, because the commit is not acknowledged until `Tx.Commit` returns. Before the build, the todo asked that no read start between the failure and the return of `Commit`, which cannot be met as worded. The rule that the build keeps is the spec's: a read that starts after a commit is acknowledged sees it. The workload with everything has readers that race these stops.

## The cache

- **SQLite's own schema is the catalogue.** The core keeps no table of tables. It reads the names from `sqlite_schema` and the columns from `pragma_table_info`. There is then one source of truth, and a person with the `sqlite3` shell sees the tables of the database.
- **The cache has one table of its own, `s4ql_cache`.** It holds the version of the cache's layout and the position, which is the name of the last commit object that the cache has applied. The position is text, 64 hexadecimal digits, so that a person can compare it with the manifest.
- **The names that start with `s4ql_` are the core's.** The core refuses a table whose name starts with `s4ql_` or `sqlite_`, in any case of the letters (`ErrReservedName`). SQLite reserves `sqlite_` itself. maragubot chose the prefix before the build and told Markus on 2026-11-12. The roadmap lists the restriction under [Restrictions](../../docs/roadmap.md#restrictions).
- **`synchronous` is `normal`, not `full`.** A cache that loses its last commits in a power loss is behind, which catch-up handles, and it is never corrupt. A commit is acknowledged from S3, so the sync at every commit that `full` adds buys nothing. The doc comment of `conn.setUp` names the source.
- **The SQLite binding got four methods for this package:** `Stmt.BindText`, `Stmt.BindNull`, `Stmt.ColumnText`, and `Stmt.Reset`, in `internal/sqlite`. The position and the names of tables and columns are text, a value can be NULL, and replay runs one statement for many rows.

How the cache handles SQLite's "busy" was chosen by maragubot on 2026-11-11, after an investigation, and told to Markus. The investigation is [research/sqlite-busy.md](../../docs/research/sqlite-busy.md).

- **A lock in Go does not keep SQLite from reporting "busy".** A reader that starts while the writer commits can take the WAL write lock for a moment, and the writer's next `begin immediate` then fails at once. It happens with a single reader, and only with real parallelism.
- **Every connection has a busy timeout,** one second by default (`Options.BusyTimeout`). SQLite then waits out the window itself. In the investigation a timeout of one millisecond already removed every failure, and the wait was about 1.5 ms on fewer than 1% of begins.
- **The write connection opens first, runs one statement, and stays open while the instance runs.** Recovery of the WAL after a crash, and the cleanup when the last connection closes, also report "busy", and this keeps both away from the read connections.
- **"Busy" after the timeout is returned to the caller and logged as a warning** (`ErrBusy`). It is not a local I/O failure, and the instance goes on. In this design it means that another process has the cache open, or that s4ql has a bug.

## A write transaction

- **Which rows a transaction changed is kept in the cache, not in memory.** For each table that existed when the transaction began, a temporary table holds the primary key of every row that the transaction upserted or deleted, and whether the row existed then. A key goes in with `insert or ignore`, so the first record of a key stays. `Tx.Commit` writes the commit object from joins of those tables with the real ones. A transaction can therefore change any number of rows.
- **A table that the transaction created needs no temporary table,** because all its rows are new.
- **The commit object and the annotations go through spool files.** The files are next to the cache. Each is unlinked as soon as it is open, so a crash leaves none behind. A failure of a spool file is a local I/O failure, and it stops the instance.
- **A transaction that changed nothing writes no commit object,** also when it has annotations, which are then stored nowhere. It commits if the lease is valid by the clocks, as a read does. A commit object without changes would only lengthen the log, and without a manifest swap nothing but the clocks can judge the commit. This is a choice of the build, not an agreed rule, and maragubot accepted it on 2026-11-13.
- **An error of the cache in a call of a transaction leaves the transaction fit only for a rollback.** One way to get there is to drop a table while a scan of the transaction is running, which SQLite refuses.
- **`DB.Begin` looks at the clocks, and `Tx.Commit` does not.** `DB.Begin` refuses at once when the lease has run out by a clock, as the spec says. In `Tx.Commit` the manifest swap decides, and a swap that is applied also renews the lease. This is a choice of the build too.
- **A write transaction has no maximum duration, for now.** A writer that waits for the writer lock gives up when its own context ends.

## The commit

- **The keys of a database are `<prefix>/manifest.json` and `<prefix>/commits/<shard>/<name>`,** where the name is the commit object's SHA-256 as 64 hexadecimal digits, and the shard is the first two digits of the name, as in `commits/9f/9f2c…e41a`. With an empty prefix, the keys have none. maragubot chose the keys before the build and told Markus on 2026-11-12, and the shard was agreed with Markus on 2026-11-13.
- **The commit objects are sharded for a copy on a disk, not for S3.** S3 does not care how many keys share a prefix. A generic tool that mirrors a bucket to a disk keeps the layout, though, and a database writes one commit object for every transaction, so one flat folder would reach millions of files. Markus on 2026-11-13: "Sharding by the first few characters might be a good thing? In case we ever need to replicate commits to a disk or something." One level of 256 folders holds about 39,000 files each at ten million commits. Two levels would give a small database nearly one folder per commit. Unlike git, the file keeps the full name, so that a file that is copied alone still names itself. The format is a draft at version 0, so nothing reads the old layout, and nothing migrates.
- **The commit object is uploaded before the manifest lock is taken.** The lease task can then renew the lease while a large commit object is on its way.
- **A commit object that the store can never take fails the commit at once.** When the store refuses the upload with `store.ErrTooLarge`, `Tx.Commit` returns an error that wraps `ErrTooLarge` and does not try again. Nothing is committed, and the instance keeps the lease and goes on serving. On S3 the limit is 5 GiB, the most that one PutObject takes. Markus said of this case before it was built: "Don't worry too much about commit objects above 5GB for now."
- **A write of the manifest that the store refuses as too large is taken as of unknown outcome,** as any error of the store that does not mean "not applied". A manifest is a few hundred bytes, so only a store that is set up wrong refuses one, and an unknown outcome is never wrong, only slower. The doc comment of `refusal` in `lease.go` says so too.
- **The position is written into the cache's transaction before the swap.** After the swap, all that the cache has to do is to commit.
- **The two locks are taken in one order:** the writer lock, then the manifest lock.
- **`serving` becomes true only under the writer lock.** A write transaction that saw the instance serve therefore cannot miss a catch-up.
- **One ordinary mutex, `DB.mu`, guards the flags of the instance.** It is never held across a wait. That is the rule for library code that neither the lint nor the scheduler checks; see [Conditions for keeping the design](../sim/README.md#conditions-for-keeping-the-design).

## The lease

The design was agreed with Markus on 2026-11-11 and changed on 2026-11-12. Markus on the frame for all of it, on 2026-11-11: s4ql is "designed mostly for always-on server-side usage", and "I don't want to design around theoretical failures."

- **The cost of the design is a wait after a crash.** An instance that starts after a crash waits the holder's duration and the margin before it serves, even if the crash was long ago: 33 seconds with the defaults. A clean shutdown releases the lease, so a normal restart does not wait. Markus asked on 2026-11-12: "we agree that regular shutdowns still don't have to wait, right?" A spare that is already running takes over sooner, because it has watched the manifest since about the holder's last renewal.
- **An instance without the lease reads the manifest every `Options.Watch`,** which is 5 seconds by default. It then sees a release within that time. When a holder dies, the instance has already watched the manifest since about the holder's last renewal.
- **The wait counts from after the read,** because a read shows the manifest as it was at some moment before the answer came.
- **The margin is the larger of two seconds and a tenth of the holder's duration.** It is for a monotonic clock that runs fast on the instance that waits. The doc comment of `margin` gives the numbers.
- **The holder counts its lease from when the first attempt of a renewal was sent,** not from a retry and not from the answer. Section 6 of [research/s3-clock.md](../../docs/research/s3-clock.md) says why.
- **A new holder writes a new lease entry.** The fields of the old holder's entry that this version does not know are not kept. One holder keeps them from renewal to renewal. The todo lists this as an open question under [Storage formats](../../docs/todo.md#storage-formats), and this is what the build chose.
- **The retries of the lease task back off,** from one second to `Options.Watch`, while the store gives no answer.
- **Both clocks of the holder stay in the check.** If both fail, reads can be stale, and Markus accepts that, on 2026-11-11: "if the server is in such bad a state that both clocks fail, stale reads are probably okay too."
- **The lease is checked before the state of a read is fixed, and that check is the one that counts.** At its moment no other instance holds the lease, so the cache holds the newest state, and the state that the read fixes later is that state or a newer one. Before the build, the todo said that one check was enough and that its order did not matter, because "The cache never holds another instance's commits". The research note on S3's clock advises the check after the read. Neither is so: catch-up puts the commits of other instances into the cache, so an instance that fixed the state of a read while it was a spare could take the lease and catch up before a check that came only afterwards. That check would then pass on stale data. The review of the build found this. `DB.Read` checks a second time after the state is fixed, which only keeps a read from being served late.
- **A call that waits for its lock waits no longer than the lease has left** (`DB.acquireServing`). Nothing wakes a caller when the lease runs out by a clock, so the wait ends then and looks again. `DB.Wait` returns only when the lease is valid by the clocks too, and is woken when a renewal makes it valid again.
- **A lease lasts a year at the most,** so that no sum of a time and a lease leaves the range of a duration. The duration of another holder comes from the manifest and can be any, so the wait for it is computed without such a sum. This is a choice of the build, and maragubot accepted it on 2026-11-13.
- **An instance without a valid lease is a normal state.** It covers an instance that cannot reach S3 and whose lease runs out, an instance whose lease another one has taken, and an instance that has just started. Markus on 2026-11-11, on an instance that cannot reach S3: "this is where we'd return a HTTP 502 and let the clients retry." On a spare: "we can have a spare instance running, and it'll never serve anything until it gets the lease."
- **Opening does not wait for the lease.** `DB.Wait` waits, as `Ping` does next to `Open` in `database/sql`. Wrong S3 settings therefore do not fail `Open`. They show in `DB.Wait` and in the error of every call.

### How the design of the lease came about

On 2026-11-12, unless a date is given:

- **On 2026-11-11 the manifest was to record an absolute expiry time.** Another instance was to take the lease when its own wall clock had passed that time, which is only safe if the wall clocks of the machines agree. The alternative was a duration and a value that another instance watches stand still, and it was passed over. Markus: "A is fine for now."
- **The simulation harness showed that one wrong wall clock is enough for stale reads.** The holder's wall clock is behind. The holder renews, and writes an expiry that is already past for every other instance. A spare takes the lease, correctly by its own clock, and commits. The holder's two checks both pass, so it serves reads from its cache until its next renewal fails. A spare whose clock is ahead does the same. The design of 2026-11-11 said "A wall clock that is set backward is caught by the monotonic clock", which held only while the holder wrote no expiry from the wrong clock.
- **maragubot proposed to take the time from S3:** compare the manifest's `Last-Modified` with the `Date` of S3's response. Markus: "Ooh, of course. S3 should be our clock, I can't believe I didn't think of that."
- **The research found S3's clock better than the wall clocks, but not safe enough to rest the lease on.** On Tigris a client can set `Last-Modified` with a request header, and MinIO has the same hole through a header of its own. The two timestamps usually come from different machines. The only store that documents a bound on the skew between its own clocks is MinIO, and its bound is 15 minutes. No system was found that compares the two timestamps. The idea swaps "our machines' clocks agree" for "the store's clocks agree", which cannot be checked either. The note is [research/s3-clock.md](../../docs/research/s3-clock.md).
- **The research pointed to the rule that Kubernetes and the DynamoDB Lock Client use, which compares no clocks.** It is the alternative of 2026-11-11. Markus: "didn't we discard this design already? But I think it sounds good."

Why the manifest has a salt, and not a counter, is in [internal/format/README.md](../format/README.md#the-manifest).

## A swap whose outcome is unknown

The rule is in the spec, under "An unknown commit outcome is resolved from S3". It was agreed with Markus on 2026-11-11. Markus on the state after "outcome unknown": "it's kind of the same as when the lease expired? We return a temporary error to readers and writers." It is the same state: the instance does not know whether its cache is current, so it refuses and keeps trying.

One mechanism serves the rule, for commits and for renewals:

- **Every swap that the holder sends is a swap from the one version that it knows.** A swap that ends without an answer is kept as a `flight`.
- **At most one flight is ever applied,** because all flights are swaps from the same version. Any swap from that version that is applied rules all the others out for good. A version that a holder wrote never comes back, because of the salt.
- **`DB.observe` reads the manifest and tells three cases.** The manifest is unchanged. Or it is the manifest of one of the flights, which the instance then adopts. Or it is another manifest, and the lease is lost.

What follows from the mechanism:

- **A commit can be sent while the outcome of a renewal is unknown.** If the renewal turns out to be applied, the commit is sent again as a new swap from the new version.
- **After a refusal with an unchanged manifest, the same swap is sent again.** This is the 409 of S3: S3 can refuse a swap from the right version while another write to the same key is on its way, and that other write can be the first attempt. A refusal therefore does not prove that the first attempt arrived. The builder of the simulated store found this in AWS's documentation on 2026-11-12, and the simulated store does it on purpose (`internal/store`).
- **After the lease is lost, a commit with an unknown attempt is looked for in the log** (`Tx.inLog`). The walk goes back from the new head to the parent of the transaction. Another instance may have committed on top in the meantime, so the walk does not look at the head alone.
- **While `Tx.Commit` still waits, it pushes the commit:** it sends the same swap again.
- **While `Tx.Commit` still waits, reads go on** for as long as the lease is valid by the clocks. They see the state before the transaction, which is allowed, because the commit is not acknowledged.
- **After the caller gave up, the lease task does not push the commit.** It reads the manifest. If the manifest is unchanged, the lease task renews the lease without the commit, and that renewal is the swap that rules the stuck request out. A read alone never settles it, because the stuck request can arrive after the read.
- **After "outcome unknown", the instance serves nothing until it knows.** It refuses reads and writes with `ErrNoLease`, as an instance without a valid lease does. Otherwise the application could check its data on a cache that lacks the transaction, conclude that the transaction was not committed, and repeat it, while the stuck request commits the first one.
- **An application that gets "outcome unknown" must check its own data,** or make its transactions safe to repeat. FoundationDB does the same with `commit_unknown_result` ([research/foundationdb.md](../../docs/research/foundationdb.md)).
- **`Tx.Commit` goes on finding out when the instance stops meanwhile.** The caller is owed an answer for as long as it waits, and the answer comes from the store. The error for an unknown outcome wraps nothing but `ErrOutcomeUnknown`, so that a caller that tests for `ErrStopped` first is not told "not committed" for a commit that may be in the log.
- **One more case gives "outcome unknown" without the caller giving up:** the lease is lost, the commit must be looked for in the log, and the log holds a commit object that can never be read. Nothing can tell then. The spec first said "only if the caller gives up first". Markus agreed on 2026-11-13 to name this case in the spec too: "Also sounds quite theoretical, but yes, sounds like the right response."
- **The transaction of the cache is rolled back when `Tx.Commit` returns "outcome unknown".** If the commit object turns out to be in the log, catch-up applies it from the store. There is then one path, and it is the path of every start.

### A take that arrives late

The version of a manifest without a lease entry can come back, because such a manifest has the same bytes whenever the head is the same, and a version comes from the bytes, as S3's ETag does. A request to take a free lease that is stuck in the network can therefore be applied after a later release. The manifest then names a holder that does not know it, and the next instance waits for that lease to time out. maragubot proposed a salt at the top level of the manifest, new at every write, so that no version ever comes back. Markus decided against it on 2026-11-13: "Isn't that a bit theoretical? And the cost is only a delay in lease? I'd say no. As long as we don't gamble with correctness."

It is no gamble with correctness, for this reason:

- **A request that is stuck in the network can be applied late only if the version that it is from comes back,** and a version comes back only if the same bytes are written again.
- **The only manifest that is written again with the same bytes is the one without a lease entry.** Every write of a lease entry carries a new salt.
- **The only swaps sent from a manifest without a lease entry are takes of the lease** (`DB.acquire`), and every attempt of a take draws a new salt, so no two takes carry the same bytes. A take that arrives late can therefore install a lease and nothing else.
- **Every commit and every renewal is sent from a version that a holder wrote,** which holds a salt and never comes back: the holder's first swap is from the version that its take wrote, and each later one from the version of the swap before it. A release is sent from such a version too.

The reasoning rests on one more thing: nothing sends a take twice with the same bytes. The core never does, and the client of the store must not either. The client for real S3 must already never let the SDK retry a conditional write by itself ([docs/todo.md](../../docs/todo.md#the-s3-client-and-s3s-checksums)), which also covers this.

`TestLease_unsure` shows both: that every attempt of a take carries a salt of its own, and the case itself, in which a take that is stuck is applied after the holder released the lease, a commit of the holder that is stuck is refused, the head is unchanged, and the next instance takes the lease after the holder's duration and the margin. The workloads of the simulation meet the case too, which `run.verifyLease` allows for. The workload of writes that land late meets it in most seeds, and the planted bug `resendTake` shows that the last condition matters: an instance that sends a take that got no answer again with the same bytes gives the manifest a version that it had before, when the first attempt lands after the next release at the same head.

## Catching up and replay

- **Catch-up downloads every missing commit object twice.** It walks the log back from the head to the cache's position, and it reads each commit object on the way to its end, so that it trusts a parent only when the bytes match the name. It then applies the commit objects, oldest first, and downloads each again. A spool of the walk could remove the second download later.
- **The holder renews its lease during a long catch-up** (`DB.keepLease`): between commit objects, and between the changes of one.
- **A download that fails is tried again where it failed** (`DB.fetchAgain`). The first build started the walk from the head again after any failure, so a cache that was N commit objects behind needed N downloads in a row to succeed, and with a store that fails one download in twenty, a log of 30 commit objects was never caught up. The review of the build found this.
- **Replay is strict.** A commit object that the reader accepts and that does not fit the cache stops the instance with `ErrLogBroken`. Examples are a table that is there already, a row to delete that is not there, and a value of the wrong storage class. The cache is right up to the parent, so the commit object is wrong.
- **Before replay refuses a commit object, it reads the commit object to its end** (`authentic`). Bytes that were damaged on their way must give "does not match its name" and a second download, not a stop.
- **A commit object that the store does not have is `ErrLogBroken` too.**

## Stopping

The rules for a local I/O failure were agreed with Markus on 2026-11-10, and they are in the spec. The failure that started them: the manifest swap succeeded, so the transaction is committed in S3, and the commit to the cache then fails. The commit is real, but the cache cannot show it, so the instance must not go on serving reads from the cache at the old state. Markus asked for a story that covers every local I/O failure, not only a full disk, because local disk I/O can fail in many ways. What comparable systems tell the client, and what the node does, in that failure is in [research/commit-acknowledgement.md](../../docs/research/commit-acknowledgement.md).

- **One rule covers every local I/O failure.** Any I/O failure on the cache stops the instance, also where nothing was committed: a full disk during a write before the swap, a failed read, or a failure during catch-up. There is then one rule and one recovery path to test, at the cost of availability on transient failures. Markus: "We can relax later if needed."
- **Which SQLite codes are a local I/O failure** was chosen by maragubot on 2026-11-11 and told to Markus. The list is in the doc comment of `classify`, and the full classification is in [research/sqlite-busy.md](../../docs/research/sqlite-busy.md). SQLite's `INTERNAL` is not in the list, and the core does not treat it as one.
- **Nothing is tried again in place.** After a failed fsync, the local state cannot be trusted until it is opened again. PostgreSQL makes the same choice for the same reason.
- **Stopped means a dead handle.** s4ql is a library and cannot crash the process, so the `DB` dies instead. It also signals the stop directly, in the style of a context: `DB.Done` closes, and `DB.Err` gives the cause. The application can then react at once, and not only at its next call.
- **`Tx.Commit` returns nil when the swap succeeded,** also when the cache then fails. Its result keeps one meaning: nil if and only if the transaction is known to be committed. An error that means "committed, but stopped" was considered and rejected. Almost all Go code treats any error from `Commit` as "not committed", so a job queue would deliver the job again and a user would submit again after the restart, and the transaction would be written twice.
- **Running reads fail.** This is a lean, not a hard rule. Markus: "Sounds theoretical. Don't overfocus on it. Leaning towards abort." Letting them finish would break no rule, because they hold a state from before the commit.
- **The library does not open itself again, for now.** The application closes the database and opens it again, so recovery from a stop is the same path as recovery from a crash of the application.
- **Recovery follows the cache's recorded position.** If the failed local commit did not land, the start applies the missing commit object. If it landed despite the error, there is nothing to apply. An I/O error from SQLite's `COMMIT` does not say whether the commit happened, so recovery never assumes either state.
- **`DB.stop` is for three things:** a local I/O failure, something in the store that the instance cannot read, and `DB.Close`. Before the build, the item on the lease in the todo said that a permanent stop is for local I/O failures only. maragubot chose before the build, and told Markus on 2026-11-12, that what cannot be read stops the instance too: a manifest or a commit object that needs a newer version of s4ql, one that is malformed, a commit object whose bytes do not match its name after a second download, and a log that is broken. Nothing that cannot be read is skipped.
- **The lease task makes the one attempt to release the lease** (`DB.releaseOnce`), after a stop for a failure.
- **`DB.Close` releases the lease itself** (`DB.release`), and ends the lease task, with its attempt. It reads the manifest and releases the lease while the manifest names the instance, for at most as long as the lease lasts. A clean shutdown therefore releases the lease also after a failed first try, and also a lease that the instance holds without knowing it.

### Closing the cache

- **`DB.Close` closes the cache whatever the store does.** Before 2026-11-13, `Close` spent its whole context on the release of the lease and only then closed the cache. With a short deadline and a store that could not be reached, the caller got an error and a cache that stayed open, again at every retry. A test fell into exactly this, in the step of `leak-builder` in the diary. maragubot had it changed, and the todo held it until then.
- **When `Close` returns, every connection that no call uses is closed,** whether the store answered or not. The first thing that `Close` does after the stop is to close them. The release of the lease comes after, with half of the time that the context has left, if it has a deadline, so that the rest is left for the calls that still run.
- **A connection that a call uses is never closed under it.** Each call that touches a connection counts itself on it while it runs (`DB.use` and `DB.unuse`), and so does the lease task while it takes the lease or catches up. The last call to leave a connection of an instance that is closing closes it. `Close` waits for those calls until its context is done, and then returns an error that says that a call still uses the cache. After the stop, every call fails at its next check, so the wait is short. Two things take longer: a `Tx.Commit` whose swap may be applied, which goes on finding out and then commits or rolls back the transaction of the cache, and a request to the store that is on its way, which ends at its answer or its timeout.
- **A transaction that is open and idle does not keep the cache open.** Its connection is closed, which rolls the write transaction back, and every later call of the transaction fails with `ErrStopped`. `Tx.Rollback` and `ReadTx.Close` then return nil. Waiting for such a transaction would gain nothing, because no call of it can succeed after the stop. This is a choice of the build, and maragubot made it on 2026-11-13.

## Waiting

- **A wait for a condition needs nothing new from the host.** The wait is `host.Host.Sleep`, or the `Acquire` of a semaphore, with a context that is derived from the caller's. Whoever changes `stopped`, `holder`, or `serving` cancels the contexts that are registered (`DB.waitLocked`, `DB.wakeLocked`).
- **Three things wait this way.** `DB.Wait` waits until the instance serves. A writer or a reader that waits for its lock fails at once when the instance stops serving. The lease task is woken from its pause.
- **Polling was rejected,** because a call that waits must fail "at once". The toy library that the harness once had polled with `Sleep`. The host has no method to wait for a condition; see [The host interface](../sim/README.md#the-host-interface). The build was free to add one, and none was needed.

## Planted bugs

The reason for planted bugs is in [Conditions for keeping the design](../sim/README.md#conditions-for-keeping-the-design): a check can pass for the wrong reason.

- **The tests plant wrong decisions in the real code.** `Options` has an unexported field, `bugs`. Each field of it switches on one wrong decision, at the one place where the right decision is made. Twenty fields are for the simulation, and one is a data race for the race detector.
- **A program that uses the package cannot switch a bug on.** Only the package's own test files can set the field, through `export_test.go`.
- **The cost is that the wrong branches are compiled into the package,** each behind a test of one boolean.

## Events

- **The instance logs one wide event for each unit of work,** through `Options.Logger`, which is a `*slog.Logger`. The names of the events and of the attributes are in one place, `telemetry.go`. maragubot chose this before the build and told Markus on 2026-11-12.
- **The level of an event says how much an operator needs it.** Routine work is Debug: a commit that succeeded, also one that changed nothing, a renewal of the lease that succeeded, and a call that was refused for lack of a valid lease. A change of state is Info: the lease taken, lost, or released, a catch-up, and a close. What went wrong and did not stop the instance is Warn, and a stop for good is Error. The list is in the doc comment of the events in `telemetry.go`. With a logger at the default level, Info, a healthy instance logs nothing after it has started. The levels were agreed with Markus on 2026-11-13, who asked: "So we log on each commit? Isn't that a bit much? But maybe on a debug or trace level it's fine".
- **A commit whose cache fails after the manifest swap is Warn,** though `Tx.Commit` returns nil for it, because something went wrong. The stop that follows is Error.
- **An event at Debug costs almost nothing while Debug is off.** The core asks the logger whether it takes the level, with the context, before it builds attributes that cost anything to build: the event of a commit keeps plain values until then (`commitReport`), and the events of a renewal and of a refusal are not built at all.
- **Every event is logged with a context, through `slog.Logger.LogAttrs`,** so that tracing can later add a span to it through the context. Markus on 2026-11-13: "make sure to use their context-aware log function counterparts, so we can add tracing context (span ID etc.) later through the context". An event of a call carries the context of its caller. The lease task has no caller: its events carry the context that `Open` was given, without its cancellation, because the task outlives the call of `Open`. The events of `DB.Close` carry the context of `Close`. `TestEvents` checks the contexts of a commit, of a read and a `Begin` that are refused, of the lease task, and of a close.
- **There is no OpenTelemetry, no trace, and no metric yet.** The todo holds them, to be decided with the public driver.

## What the package does not do, and what to know

- **A catch-up downloads every missing commit object twice.** See [Catching up and replay](#catching-up-and-replay).
- **Replay holds one batch of rows decoded.** That is about twenty mebibytes for a batch of one mebibyte, whatever the size of the commit object.
- **No test shows that the margin is enough.** The tests check that an instance waits for the holder's duration and the margin (`run.verifyHistory`). The harness cannot make a clock run fast, so the fault that the margin is for stays out of the workloads; see the comment of `clockFaults` in `simulation_test.go`.
- **A take of the lease that arrives late can make the next instance wait for a lease to time out.** It adds no commit. See [A take that arrives late](#a-take-that-arrives-late).
- **The simulation does not reach SQLite's "busy".** The write connection opens first, and every connection has a busy timeout; see [The cache](#the-cache). Both guard against "busy" under real parallelism, which the scheduler never produces; see [What the scheduled mode cannot do](../sim/README.md#what-the-scheduled-mode-cannot-do).
- **The manifest has no guard against bytes that are damaged on their way.** A commit object is checked against its name and downloaded a second time. A manifest that is damaged and no longer parses stops the instance at the first download. One that is damaged and still parses would be believed, and the swap that takes the lease would write it back, because the swap is from the version that the store gave, not from the bytes. The store's contract promises content that is not changed, and the design has no checksum for the manifest.
- **There is no layer under SQLite that injects faults, for now.** SQLite tests its own handling of I/O errors, and using the official C code is how s4ql inherits that.
- **There are no snapshots and no garbage collection.** Nothing deletes a commit object of the log, or one that was uploaded and never committed.
- **There is no branch other than `main`.**

## Tests

`model_test.go` holds a second, simpler implementation, in maps. `mstate.apply` replays a commit object strictly: a key that appears twice is an error, and so is a delete of a row that is not there. `mtx` computes the net effect of a transaction. `model` reads the log through the direct view of the simulated store.

`simulation_test.go` holds the harness. Callers create and drop tables, upsert, delete, get, and scan, and every result is compared with the model. The tables have three names in two spellings each, with characters that must be quoted, and four shapes. The doc comment of `workload.run` lists the invariants, and the doc comment of `run.verifyHistory` lists the checks of the store's history.

`workloads_test.go` holds the three workloads, `singleWorkload`, `fullWorkload`, and `hammerWorkload`. It also holds the three aimed workloads: `competing` for the rules of the lease, `unknownOutcomes` for a swap whose outcome is unknown, and `lateWrites` for writes that land long after they were sent. The list `plantedBugs` gives each bug its workload, and a control where the bug needs one ability of the harness or one fault.

`lateWrites` is for a write that lands long after it was sent. A real store can apply a write many minutes after its caller gave up or crashed, and no store documents a bound. The simulated store holds such a write for up to `Faults.LateBy`, a minute in the other workloads, and with `Faults.Long` for up to `Faults.LongBy`, 15 minutes here. The lease lasts 30 seconds and a takeover takes about 33, so only the long hold makes it common for a write of one holder to land after others took the lease and committed. The callers run for about 15 simulated minutes, with crashes, restarts, closes, and cut-offs, and `run.verifyLandings` reads the history of the manifest after each run to count what became of every write that was held back, and to check that a swap of the manifest that lands after the lease changed hands is applied only as a take of a free lease. The design says that such a write is harmless, and the runs show it: a late swap of the manifest is from a version that a holder wrote, which never comes back, so it is refused, and a late create of a commit object finds the object there already, or creates an object that no manifest names. `TestSimulation` requires that both kinds land after another node took the lease in a quarter of its seeds at least. The planted bug `resendTake` needs a write that lands late, and its workload, a variant of this one, finds it 12 times as often with the long hold as with holds of up to a minute; see [A take that arrives late](#a-take-that-arrives-late) and the table below.

Over 1000 seeds of `lateWrites` on 2026-11-13, each run twice, no seed violated an invariant and no pair of traces differed. 28596 commits were acknowledged, and 1396 had an unknown outcome. Of 17691 swaps of the manifest that landed late, 17570 were refused, and the 121 that were applied all landed before another holder had written. In 887 seeds such a swap landed after another node had taken the lease, and in 643 after other nodes had taken it twice. Of 4283 creates of commit objects that landed late, 2050 were refused because the object was there, and 2233 created an object that no manifest names. In 628 seeds such a create landed after another node had taken the lease, and in 411 after two. The run took 14 minutes on the machine of the build, beside other runs. When the store was changed to apply a late swap without its condition once it was 70 seconds old, the checks found it in each of the first five seeds, and `run.verifyLandings` was the one that found it in two of them.

`free_test.go` runs the workloads on real goroutines, for the race detector. `TestFreeRunning_plantedDataRace` runs the test binary again, and requires the race detector to report a planted data race with the scheduler off.

A buggify point sits between the manifest swap and the commit to the cache (`Tx.commitCache`), with two variants: the commit to the cache is rolled back and reports an error, or it goes through and still reports an error. The second variant is not distrust of SQLite, which can legitimately return an I/O error from `COMMIT` after the transaction became durable. It catches a bug of ours: recovery that assumes that an error means that the cache did not get the transaction, and so applies the commit object twice.

A crash in the workload with everything copies the cache and its write-ahead log at the moment of the crash, without the shared-memory index, and a new instance starts on the copy. Nothing opens the copy before the instance does, so `Open` recovers the cache from the log, as after a real crash. The harness then checks that the cache holds what it held at the crash, and what the log says up to its position. It also reads a copy of the file of the cache alone, without the log, and counts the restarts where the file lacked commits that the cache had, which only the recovery from the log can bring back. Copying the cache without its log makes the check fail in most seeds. Over 1000 seeds on 2026-11-13, 761 of the 1000 restarts recovered commits that only the log held, from logs of 5064 frames in all and 36 in the largest. Before that day, the harness read the copy before the instance opened it, and its connection, as the last one to close, checkpointed the copy and deleted the log, so `Open` never recovered anything.

Every test that opens a cache checks at its end that no connection to it is left open (`leftOpen`): the scenario tests through `cacheDir`, and each run of a workload as a violation of the run. SQLite deletes the write-ahead log when the last connection to a database closes, so a log that is left shows a connection that is left. A connection that has not read the cache keeps no log, so the check misses that one. The C address sanitizer finds a connection that is left too, but only once nothing on the Go heap points to its memory any more, which depends on when the collector ran, and only when the whole test binary passes.

The hunt for a leak under the sanitizer found a real bug in the core on 2026-11-13, which is fixed. A scan that ended by a panic left its statement open. On a read, the connection then went back to the pool with its old snapshot, so a later read on it could serve the state from before an acknowledged commit. In a write transaction, the next commit failed with "database table is locked". No simulation had found it, because no workload ends a scan by a panic. `TestScan` in `cache_test.go` now shows both cases. The check for a write-ahead log that is left also catches the bug, because a statement that stays open keeps its connection from closing.

`script_test.go` holds a store that does to chosen requests what a test spells out. The scenario tests in `lease_test.go`, `tx_test.go`, `replay_test.go`, `read_test.go`, and `core_test.go` use it, and they run on the fake clock of `testing/synctest`. The tests of each clock of the lease check (`TestLease_clocks`) and of the cache that fails after the manifest swap (`TestDB_stop`) run one instance under the scheduler, because only it can move one clock and fire one buggify point.

By default, `TestSimulation` runs 40 seeds of the workload with one instance, 30 of the workload with everything, 10 of the workload whose callers never pause, and 10 of the workload with writes that land late. Every seed runs twice, and the traces are compared. The events of the instances are part of the trace. `TestSimulation_plantedBugs` runs the workload of each bug until four seeds have found the bug, from at most 200 seeds. It runs the control and the same workload without the bug for 10 seeds each. `TestFreeRunning` runs each of the first three workloads 30 times, in parallel with the simulations. Under the race detector and the address sanitizer, `TestTx_Commit_large` runs a tenth of its rows and does not check the heap; CI checks it in a run of its own without either.

The environment variables:

- `S4QL_SIM_SEEDS` sets the number of seeds for every workload and every bug, and the number of free runs. The workload of each bug then runs all of them, to count how many find the bug.
- `S4QL_SIM_SEED` makes `TestSimulationSeed` run one seed and log its whole trace. `S4QL_SIM_WORKLOAD` chooses the workload: `single`, `full`, `hammer`, or `late`.
- `S4QL_SIM_BUG` makes `TestSimulationSeed` run the workload of the planted bug that it names. `S4QL_SIM_CONTROL` then runs the control of that bug, and `S4QL_SIM_CLEAN` runs the workload without the bug.
- `S4QL_CORE_ROWS` sets the number of rows of `TestTx_Commit_large`.

The large runs, as they were made for the numbers below:

```sh
S4QL_SIM_SEEDS=10000 go test -timeout 6h -run '^TestSimulation$' ./internal/core/
S4QL_SIM_SEEDS=1000 go test -timeout 3h -run '^TestSimulation_plantedBugs$' ./internal/core/
```

On a Mac with 10 cores and a load of 13 to 18 from other work, the first took 23 minutes for the workload with one instance, 30 minutes for the workload with everything, and 39 minutes for the workload whose callers never pause, and the second took 33 minutes while the first ran beside it. The planted bugs over 10000 seeds were not run. The time grows with the number of seeds, so they need about ten times that, and `-timeout 12h` leaves room.

The default run of the package took 46 seconds on that machine, 98 seconds under the race detector, 133 seconds under the race detector with `GOMAXPROCS=4`, and 126 seconds under the address sanitizer in a Docker container with four CPUs, at a load of 13 to 19. A profile shows three quarters of the time in handing the baton between the scheduler and the task that it runs, so the time goes with the number of steps, and parallel subtests gain a factor of three to four, not ten.

The workload of writes that land late and its planted bug added little, because their callers think for long and a seed takes about 8000 steps. On 2026-11-13, at a load of 12 to 17, the default run took 45 s against 46 s before, measured 20 minutes apart. With each test binary run in turns with one built from the code before, it took 104 s twice against 88 s and 97 s under the race detector, and 121 s and 128 s against 124 s and 118 s under the address sanitizer in Docker.

`S4QL_CORE_ROWS=2000000 go test -run TestTx_Commit_large ./internal/core/` runs a transaction of four million rows. Its commit object was 157 MB. The heap of the Go runtime grew by 3.4 MB while the transaction was written and committed, and by 20 MB while it was replayed, which is one batch of rows as the messages that it decodes to. With the default of 600,000 rows and a commit object of 23 MB, the numbers were 3.2 MB and 20 MB, so neither grows with the transaction.

### What was measured

Each workload ran with `S4QL_SIM_SEEDS=10000`. No seed violated an invariant, and no pair of traces differed:

| | One instance | Everything | Callers that never pause |
|---|---|---|---|
| Seeds, each run twice | 10000 | 10000 | 10000 |
| Commits acknowledged | 143626 | 110225 | 635909 |
| Commits of nothing | 24898 | 19422 | 109932 |
| Commits that failed | 423 | 2622 | 4468 |
| Commits of unknown outcome | 308 | 473 | 2812 |
| Of those, settled as committed | 114 | 121 | 1064 |
| Settled as not committed, by a renewal | 194 | 156 | 1748 |
| Settled by the loss of the lease | 0 | 151 | 0 |
| Commits cut off by a crash | 0 | 294 | 0 |
| Reads served | 294934 | 245988 | 1331510 |
| Reads overtaken by a commit | 6345 | 2626 | 264404 |
| Reads and writes refused for lack of a valid lease | 6898 | 828216 | 219957 |
| Reads refused because a clock said that the lease had run out | 805 | 5691 | 6264 |
| Leases taken | 20000 | 38763 | 20000 |
| Leases lost to another instance | 0 | 2706 | 0 |
| Renewals whose outcome was unknown, and then settled | 931 | 13222 | 1329 |
| Crashes, each with a restart on a copy of the cache | 0 | 10000 | 0 |
| Instances closed and opened again while callers ran | 0 | 17702 | 0 |
| Stops for a cache that failed after the manifest swap | 0 | 7082 | 0 |
| Stops for a commit object damaged in two downloads in a row | 0 | 38 | 0 |
| Moves of a clock | 0 | 167174 | 0 |
| Buggify points that fired | 0 | 37794 | 0 |
| Longest time until an instance served after the workload | 20 s | 2 m 8 s | 32 s |
| The bound on that time | 4 m 56 s | 5 m 29 s | 4 m 56 s |

Half of the leases taken in each column are those of the instance that replays the whole log into an empty cache at the end of every run. The faults of the store in the same runs:

| Fault | One instance | Everything | Callers that never pause |
|---|---|---|---|
| Requests lost on their way | 7364 | 45819 | 30167 |
| Answers lost on their way | 6790 | 43815 | 28896 |
| Calls that got no answer at all | 7390 | 36817 | 32116 |
| Swaps refused for a conflict with another write | 3434 | 6830 | 39309 |
| Slow ways | 14531 | 87789 | 61472 |
| Downloads whose content broke off | 206 | 32033 | 1352 |
| Clients cut off | 1941 | 11217 | 8040 |
| Outages | 762 | 4649 | 3264 |
| Writes stuck in the network, to arrive later | 14175 | 18580 | 61533 |
| Writes applied after their caller gave up or crashed | 2600 | 4273 | 9611 |
| Callers that gave up on a request | 19744 | 113983 | 84164 |
| Downloads of commit objects damaged on their way | 0 | 1925 | 0 |

The planted bugs, each over 1000 seeds. The control is the same workload without what the bug needs. Each bug's workload also ran without the bug:

| Planted bug | Seeds that found it | Seeds of the control that found it | Seeds that found anything without the bug |
|---|---|---|---|
| A renewal that swaps the manifest outside the lock | 884 | no control | 0 |
| A commit that reaches the cache before the manifest | 166 | no control | 0 |
| A read that checks the wall clock only | 115 | 0 | 0 |
| A read that checks the monotonic clock only | 123 | 0 | 0 |
| An instance that takes over a lease only after a release | 1000 | 0 | 0 |
| A renewal that writes no new salt | 968 | 0 | 0 |
| An instance that waits for its own duration, not the holder's | 347 | 0 | 0 |
| A swap against the version read last, not the one watched | 997 | 0 | 0 |
| A holder that counts its lease from the answer to a swap | 53 | 0 | 0 |
| A commit that ignores a failure of the cache | 223 | 0 | 0 |
| A commit acknowledged before the outcome of its swap is known | 984 | 0 | 0 |
| "Not committed" from a read of an unchanged manifest | 131 | 0 | 0 |
| An instance that serves on after "outcome unknown" | 884 | 0 | 0 |
| "Not committed" because the lease was lost, without the log | 73 | 0 | 0 |
| A replay that commits before the reader reported the end | 219 | 0 | 0 |
| A commit object that lists a key twice | 1000 | no control | 0 |
| A commit object that forgets a delete | 918 | no control | 0 |
| A catch-up that skips a commit object that it cannot read | 562 | 0 | 0 |
| A renewal that waits for the writer lock | 870 | 0 | 0 |
| A take of the lease that is sent again with the same bytes | 387 | 0 | 0 |

- **Two controls found their bug in the first count, and were wrong, not the core.** The control of the lease counted from the answer had no slow answers, and found the bug in 1 of 1000 seeds: an instance also learns late of a swap whose answer a cut-off lost. It has one instance now. The control of the replay that commits early had no downloads that break off by themselves, and found the bug in 3 of 1000: content also breaks off when its client is cut off while replay renews the lease. It has no cut-offs and no outages now. Both were counted again.
- **The read model finds the bugs of the clocks in about one seed in nine, and the lease counted from the answer in one in nineteen.** The stale window runs from the new holder's first acknowledged commit to the old holder's next contact with the store. The history of the store finds the bugs of the watcher in nearly every seed, because it needs no read at the right moment.
- **The default run finds every bug four times within 81 seeds.**
- **The take that is sent again needs the long hold to be found often.** The same workload with holds of up to a minute, `Faults.Long` off, found it in 33 of 1000 seeds, against 387, and its control, with no write held back, in none. The workload `lateWrites` itself, whose callers write, found it in none of 200: a commit moves the head, and the manifest without a lease entry at the old head never comes back.

Mutation testing, with one mutant for each decision of the core, each a copy of one file put in place with `go test -overlay`:

- **On the final code:** 245 mutants over `core.go`, `lease.go`, `tx.go`, `catchup.go`, `replay.go`, `read.go`, `cache.go`, `spool.go`, and `errors.go`, run one after another. With the tests as the build left them, 188 were caught and 57 were not. 23 of the 188 made a test hang, and were caught by its timeout or by a limit of 2 GB on its memory. A control without a change passed every test.
- **After tests for the survivors that deserved one,** 213 are caught and 32 are not.
- **The mutants that no test catches:**
  - 14 remove a check whose case something else still refuses: four checks of replay whose commit objects SQLite or the binding refuses (one of them by accident, through a parameter `?0`), the check of a commit object's parent, which the walk through the log makes true, and nine guards that an earlier or a later check covers.
  - 11 change only a cost, a time, or an event: the temporary table that a created table does not need, the back-off after failures, one extra renewal or refused swap, the count of attempts, the order of a scan that SQLite gives anyway, and the like.
  - 1 is equivalent: the fields of a lease entry that this version does not know, which the holder's own entry never has.
  - 6 change what matters, and no test could produce their case: the order in which the connections open, which guards against "busy" after a crash under real parallelism; the sync of the cache, which only a power loss shows; and four local failures in the middle of an operation (a read of the spool file, the rollback of a read, a change during replay, a length in the spool), which need a file system that fails.

  The diary has the reason for each.
- **After local files that fail on demand** (2026-11-13, [diary](../../docs/diary/2026-11-13-failing-file-system.md)), three of those four are caught. Each test arms its fault before the operations that it fails, and chooses them by their kind and count, not by time:
  - a read of the spool file during the upload (`upload-spool-error-retried`), by "should stop for a failure to read the spool file during the upload, and not try the upload again" in `TestDB_stop`;
  - a read of the length of an annotation from its spool file (`spool-length-error-ends`), by "should stop for a failure to read the length of an annotation from its spool file, and not commit without it" in `TestDB_stop`;
  - a write of the cache that fails while replay applies a change (`replay-cache-error-as-broken`), by "should stop for good with a local failure, and not a broken log, when the file system fails while replay applies a change, and keep the cache as it was" in `TestDB_stop_cache`.

  The fourth, the rollback at the end of a read (`read-end-swallows-local`), is still not caught, because no file system can make it fail. SQLite 3.54.1 ends a read transaction through `sqlite3RollbackAll`, `pager_unlock`, and `sqlite3WalEndReadTransaction`, which return nothing, so a `ROLLBACK` after only reads returns `SQLITE_OK` whatever the files do, or `SQLITE_ERROR` ("no transaction is active") after a failed read that SQLite rolled back itself, which is not a local failure. A test in `internal/sqlite/faultvfs` shows that such a rollback meets no operation that can fail. Out of memory is the one local failure that could still reach the check, in the prepare of the `ROLLBACK`. The check stays, because it costs nothing and holds if SQLite changes.

  The test of replay depends on SQLite's page cache: its commit object takes about twice the pages that the default cache of 2 MB holds, so that replay writes pages before its commit. It also checks, through the event of the catch-up, that replay failed before it read the commit object to its end, so it fails, and does not pass without reaching the check, if a change of the cache's settings ends the spill.

The spool files fail through the field `local` of `Options`, which wraps each spool file and which only the tests set. The cache fails through `internal/sqlite/faultvfs`, an SQLite VFS that wraps the default VFS and fails the operations that a test arms. A rule of `depguard` in `.golangci.yml` lets only test files import it, so no other build carries it, and a connection uses it only when it is opened through its name.