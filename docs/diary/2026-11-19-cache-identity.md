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
