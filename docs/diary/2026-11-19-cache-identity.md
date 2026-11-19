# Diary: The cache records which database it belongs to

Nothing tied a cache to its database: two instances of two databases that start on one empty cache directory both served, and each read the tables that the other wrote. Markus agreed that the cache records the identity of its database and that an instance refuses a cache of another one; the details are the lead's and the builder's. The work is on the branch `cache-identity`, which starts from `main` at `47ecc21`.

## Step 1: The brief, and a plan

**Author:** cache-identity builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Nothing ties a cache to its database. Two instances of two different databases that start on one empty cache directory both serve, and each reads the tables the other writes: wrong data without an error. [...] The cache records which database it belongs to, and an instance refuses a cache that belongs to another database. - The identity of a database is where it lives: the store's endpoint, the bucket, and the prefix, as the public options give them. The public package builds one identity value from them and hands it to the core as an option; the core treats it as an opaque string and does not know about S3. [...] - A cache that this open creates records the identity in the same local transaction that creates its schema, so that two instances that start on one empty directory at the same time cannot both believe that the cache is theirs [...] test exactly this race (two instances of two databases on one empty directory, started together, many times): never do both serve, and the one that is refused says why. - An instance whose cache records another identity refuses loudly, with an error that names both identities and says what to do [...] Prefer that `Open` fails with it as a local problem, if the core opens the cache at open [...] - A cache that exists and holds no record of an identity (written before this change) is refused in the same way [...] If the cache has a format or schema version, bump it [...] - Changing the endpoint string for the same bucket therefore asks for a new cache directory. Document that at the options."
**Interpretation:** A new option of `core.Options` carries an opaque identity. `conn.setUp` in `/internal/core/cache.go` creates the table `s4ql_cache` and its one row, now with the identity, inside one `begin immediate` transaction, and refuses a cache whose row holds another identity. The public package builds the identity from `Options.Endpoint`, `Options.Bucket`, and `Options.Prefix`. The layout of the cache changes, so `cacheVersion` goes from 0 to 1.
**Inferred intent:** Wrong data without an error becomes a loud refusal at `Open`, with no window in which two databases share one cache.

### What I did

Reset the worktree to `origin/main` at `47ecc21` and created the branch `cache-identity`. Read `/AGENTS.md`, the section Constraints of `/docs/spec.md`, `/internal/core/core.go` (`Open`), `/internal/core/cache.go` (`setUp`, `position`), `/internal/core/errors.go`, the open tests in `/internal/core/core_test.go`, `/s4ql.go`, `/conn.go` (`newConnector`), `/export_test.go`, `/errors.go`, `TestOpen` and `TestOpen_cacheDir` in `/s4ql_test.go`, and the open point in `/docs/todo.md`.

What I found: `core.Open` opens the write connection and runs `setUp` before it returns, so the refusal can come at `Open`, and the public `Open` fails with it as a local problem. Today `setUp` runs `create table if not exists`, a count, and an insert, each as a transaction of its own, so two instances can both find the table empty; the second insert would then fail on the primary key, which is luck and not a design. The engine, the simulator, and the core's own tests call `core.Open` directly with no identity, which is the empty identity, and open one cache with one database each, so only the root package's tests that open another prefix on one directory change.

The plan:

1. Core: `Options.Identity`, a column `database` in `s4ql_cache`, `cacheVersion` 1, and `setUp` in one `begin immediate` transaction that either creates the table with the row or reads the row and compares. A new error `ErrCacheDatabase` names both identities and the remedy. Tests in the core, also of the race.
2. Public: an identity built from the endpoint, the bucket, and the prefix, the documentation of `Open` and of the options, the identity in the event `open`, and tests through `database/sql`.
3. `/docs/todo.md`.

### Why

The core opens the cache at `Open`, so a check there answers before anything is served, and a transaction that takes SQLite's write lock before it looks closes the race.

### What worked

The order of `Open` already makes the write connection the first to touch the cache, so the setup is the natural place.

### What didn't work

Nothing tried yet.

### What I learned

The identity is only compared, never parsed, so the core can keep it opaque, and the public package only has to make it injective.

### What was tricky

How to write the identity so that two different places never give one string, and it still reads well in an error.

### What warrants review

The plan's choice to refuse at `Open` instead of as a stop.

### Future work

The steps of the plan.

## Step 2: The core records the identity and refuses a cache of another database

**Author:** cache-identity builder

### Prompt Context

**Verbatim prompt:** (from the brief, as in step 1) "A cache that this open creates records the identity in the same local transaction that creates its schema [...] Think this through with SQLite's locking as the core uses it, and test exactly this race (two instances of two databases on one empty directory, started together, many times): never do both serve, and the one that is refused says why."
**Interpretation:** The core's half: the option, the column, the transaction, the error, and the tests of the record and of the race.
**Inferred intent:** The core alone guarantees that one cache holds one database, whatever the caller passes as the identity.

### What I did

Added `core.Options.Identity` and `core.ErrCacheDatabase` (`/internal/core/core.go`, `/internal/core/errors.go`). `openError` keeps `ErrCacheDatabase` out of `ErrLocal`, as it does `ErrCacheVersion`, because neither is an I/O failure. In `/internal/core/cache.go`, `cacheVersion` is 1, and `s4ql_cache` has a fourth column, `database text not null`. `conn.setUp` now:

1. puts the cache into wal, through a new `conn.wal`;
2. reads whether `s4ql_cache` exists, with no lock beyond a read, so that a spare of the same database opens the cache while the serving instance holds a write transaction on it (`Tx` holds `begin immediate` for its whole life);
3. only if it does not: `begin immediate`, looks again inside the transaction (`conn.make`), creates the table and inserts the row with the identity if it is still missing, and commits;
4. reads the position, which refuses another version as before, and then the identity, which must be the instance's.

The error, as `core.Open` returns it: `open the cache: the cache belongs to another database: it records the database "one", and this instance opens the database "two"; open each database with a cache of its own, or delete the cache, with its files -wal and -shm, if the database has moved`.

Tests in `TestOpen_identity` in `/internal/core/core_test.go`: a new cache records the identity, also the empty one; a reopen with the same identity serves; four other identities (another, the empty one, one with a trailing space, one in another case) are refused with the exact text, not as `ErrLocal`, and the cache is unchanged and opens again for its own database; a cache of version 0, written by hand in the old layout, is refused with `ErrCacheVersion` and left as it is; 50 rounds of two instances of two identities that open one new cache at once, of which exactly one opens, the other's error names both identities, and the cache records the winner's; 20 rounds of two instances of one identity at once, of which both open, one serves, and the other serves after the first closes.

Committed as "Record the identity of the database in the cache of `core`, and refuse a cache of another database at `core.Open`".

### Why

The check has to be at the place that makes the cache, and the transaction that makes it has to hold the write lock before it looks, or two instances can both see an empty cache.

### What worked

Reading before taking the write lock keeps the same database twice on one cache as it was: the second opens without waiting for the first's write transactions.

### What didn't work

The first run of the race test failed, and not where I expected:

```
--- FAIL: TestOpen_identity/should_let_exactly_one_of_two_instances_of_two_databases_that_open_one_new_cache_at_once_have_it,_and_refuse_the_other,_many_times (0.01s)
    core_test.go:1306: Expected "the cache belongs to another database" (type *errors.errorString), but got "open the cache: local I/O failure: sqlite: database is locked (code 5)" (type *fmt.wrapErrors)
--- FAIL: TestOpen_identity/should_let_two_instances_of_one_database_that_open_one_new_cache_at_once_both_have_it,_one_serving_and_the_other_a_spare (0.14s)
    core_test.go:1339: Expected nil error, but got "open the cache: local I/O failure: sqlite: database is locked (code 5)" (type *errors.joinError)
```

with a busy timeout of an hour. A temporary label on each statement of `setUp` showed that every "locked" came from `pragma journal_mode = wal`. This is on `main` too: two instances of one database that start at once on an empty directory can fail `Open` with "busy". In `OP_JournalMode` of `/internal/sqlite/c/sqlite3.c`, the change to wal calls `sqlite3BtreeSetVersion`, which opens a read transaction and then upgrades it to a write; `sqlite3BtreeBeginTrans` calls the busy handler only from no transaction, so the upgrade reports "busy" at once when another connection makes the same change. `conn.wal` therefore answers "busy" with `begin immediate` and `rollback`, which waits with the busy handler for the other connection's change to end, and asks again; the cache is then in wal, and the pragma writes nothing. A temporary print showed the retry in 37 of 250 opens over five runs of the test, and never a second retry. The loop gives up after five tries, for a connection that changes the mode back and forth.

### What I learned

SQLite's busy timeout does not cover every lock: an upgrade from a read to a write gives "busy" at once, to avoid a deadlock, and the change of the journal mode is such an upgrade.

### What was tricky

Keeping the spare: a `begin immediate` at every open would make a second instance of the same database wait for the first's write transaction and fail with "busy" after the timeout. Only the making of the table takes the lock.

### What warrants review

`conn.wal` and its comment, which rests on my reading of SQLite's code; and that `conn.make` runs `c.forget` after the create, as `conn.create` does.

### Future work

The public half.

## Step 3: The public package builds the identity, and `Open` refuses a cache directory of another database

**Author:** cache-identity builder

### Prompt Context

**Verbatim prompt:** (from the brief, as in step 1) "The identity of a database is where it lives: the store's endpoint, the bucket, and the prefix, as the public options give them. The public package builds one identity value from them and hands it to the core as an option [...] Changing the endpoint string for the same bucket therefore asks for a new cache directory. Document that at the options. [...] Add the recorded identity to that event if it fits the present style."
**Interpretation:** `Options.identity` in `/s4ql.go`, set into `core.Options.Identity` in `newConnector` in `/conn.go`, the documentation of `Open`, `Options.Endpoint`, and `Options.CacheDir`, the attribute in the event `open`, the tests through `database/sql`, and `/docs/todo.md`.
**Inferred intent:** An operator who points two databases at one directory gets an `Open` that fails and says which two, and one who moves a database is told what to do.

### What I did

The identity is `s3://` and the bucket, a slash, and the prefix, each of its segments escaped with `url.PathEscape`, followed by ` at ` and the endpoint if `Options.Endpoint` is not empty: `s3://bucket/app at https://t3.storage.dev`. It is injective: `url.PathEscape` escapes the slash, the space, the question mark, and the percent sign, so the bucket ends at the first slash, the prefix at the first space, and what follows ` at ` is the endpoint as given. I chose that form over JSON or Go's `%q` for each part because the core prints both identities with `%q`, and a form without quotes reads well there.

`newConnector` sets `co.Identity`, which replaces whatever a test passed in `core.Options`; the comment of `OpenStore` in `/export_test.go` says so. The event `open` gets `db.identity`. The documentation of `Open` says that a cache directory belongs to one database, what the error starts with, what to do, and that of two databases that open one new directory at once one opens; that of `Options.Endpoint` says that another text for the same store is another database to the cache, and that the environment is not seen; that of `Options.CacheDir` says that it belongs to one database.

`TestOpen_cacheDir` in `/s4ql_test.go`: the record for four sets of options, among them an empty prefix, a prefix with a space, a non-ASCII letter, and a question mark; a refusal at `Open` for another prefix, bucket, endpoint, and no endpoint, while the first instance runs and after it has closed, each with the exact text and not `ErrStopped`, after which the cache records its own database and serves its row; and 20 rounds of two prefixes that open one new directory at once, of which exactly one opens and the other's error names both. The test that an instance of another database stops with `ErrCacheNotInLog` is gone: that case is now refused at `Open`. `TestOptions_Logger` has the new attribute. In `/docs/todo.md` the open point is replaced by the decision and its details, and the comment of `core.ErrCacheNotInLog` no longer says that the cache belongs to another database.

No other test opens two databases on one directory: the engine, the simulator, and the core's own tests call `core.Open` with the empty identity, and `internal/sqlite/crosscheck/drivercheck` is not in this tree.

Committed as "Build the identity of the database in `s4ql` from the endpoint, the bucket, and the prefix, so that `Open` refuses a cache directory of another database".

### Why

The core knows nothing of S3, and the public options are the one place where the endpoint, the bucket, and the prefix are together.

### What worked

The tests were red for the right reasons first: no record, both opens succeeded, `Open` gave no error.

### What didn't work

The first run of the race test in the root package failed with `table t1 already exists` at `s4ql_test.go:399`: each round used the prefixes "one" and "two" in one store, so the second round's winner found the first round's table. Each round now has prefixes of its own.

### What I learned

`OpenStore` takes the options that name the store and used none of them; the identity is the first use of `Bucket` and `Endpoint` in the tests on the in-memory store.

### What was tricky

The text for the operator: the core does not know the cache directory, so its advice is to delete "the cache, with its files -wal and -shm"; a cache file deleted without its write-ahead log could bring old pages back.

### What warrants review

The form of the identity, and the documentation of `Open` and of `Options.Endpoint` in `/s4ql.go`.

### Future work

None from this step.
