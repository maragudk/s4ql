# Diary: A fuzz finding of `FuzzSequence`, and a test of the S3 client that depends on timing

A measurement of two days of CI runs turned up two failures that nobody had looked at: `FuzzSequence` of `internal/sqlite/crosscheck/drivercheck` failed once in the job `asan-fuzz` on `main`, and a subtest of `TestClient_connections` in `internal/store/s3store` failed in 4 of 142 runs of the job `test`. Each is a branch and a pull request of its own: `fix-fuzz-sequence` from `origin/main` at `093b646`, and `fix-s3store-flake` on top of it, so that it carries this diary.

## Step 1: The input of `FuzzSequence` that failed after #73

**Author:** fuzz-sequence and s3store-flake builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "`FuzzSequence` in the driver check (`internal/sqlite/crosscheck/drivercheck`) failed in the `asan-fuzz` job on `main` on 2026-11-20 at 00:58 (run 44650069801, job 136002504138, after pull request #73): `insert into t3 values (2, 'b', 'c')` got `not supported: a string literal for the column t3.v3 of type REAL` where the test's rule expected no error (""). [...] Rebuild the input from the log and run it against today's `origin/main`. Say whether it still fails. Find out which side is wrong [...] If it no longer fails, find the commit that fixed it (bisect over `main` if cheap) and still add the input to the corpus."
**Interpretation:** Rebuild the corpus file byte for byte, run it on `main` and on the commit of the run, and decide from SQLite and the rule of exact types whether s4ql or the test's rule was wrong.
**Inferred intent:** Know that the finding is closed, and keep the input in the seed corpus so that it stays covered.

### What I did

The job's log prints every file under `testdata/fuzz` after a failure. `gh run view 44650069801 --log --job 136002504138` gave the three lines of `c7060d373c508f7a`. Its SHA-256 starts with `c7060d373c508f7a`, which is the name that Go gives a corpus file, so the bytes are exact. The input creates one table, `t3 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid`, and runs inserts and selects on it; no table is dropped, and no transaction is rolled back. `insert into t3 values (2, 'b', 'c')` gives a text for the REAL column `v3`. SQLite 3.54.1 refuses it too: "cannot store TEXT value in REAL column t3.v3". By the rule of exact types, s4ql refuses it with its "not supported", and the test excuses that only if its rule gives the same refusal.

- At `ac45bbf`, the head of the run (#73), the input fails as in the log. `pair.typeRefusal` looked at the type of the first value only, against the column with `pk = 1`: the key, an INT, with the value 2. So it gave "" for every insert whose key had the right type.
- At `8514960^` it fails, and at `8514960` ("Check every value of an insert of several columns in `drivercheck`") it passes. That commit is in #74 (`a832e68`), merged right after #73, and not in #73.
- On `origin/main` at `093b646` it passes, pinned and not pinned. The rule is `stmttest.TypeRule` now, which looks at every column in the order of the table.

The fault was in the test, not in s4ql. I added the file as it was to `internal/sqlite/crosscheck/drivercheck/testdata/fuzz/FuzzSequence/`, and a case to `TestTypeRule` in `internal/sql/stmt/stmttest/rule_test.go`: `insert into t values ('x', 2, 'c', null)`, a text for the REAL column after a key of the right type, which gives the refusal for `t.b`.

`go test -run='^$' -fuzz='^FuzzSequence$' -fuzztime=5m ./internal/sqlite/crosscheck/drivercheck` passed with 9,184 executions and 48 new interesting inputs, on a machine that other builders' tests loaded at the same time. Under the address sanitizer, in the `golang:1.27.3` container of `make test-asan`, for this package only: `go test -asan -short -shuffle on ./drivercheck` in `internal/sqlite/crosscheck` passed in 159 s, and `make fuzz FUZZDIRS=internal/sqlite/crosscheck/drivercheck FUZZFLAGS=-asan FUZZTIME=30s` passed with 263 executions after a baseline of 64 inputs, the new one among them.

The rate was low, often 0 per second, and while it ran another test failed with `connect: can't assign requested address`. `netstat` showed 32,604 established connections on 127.0.0.1, and `lsof` gave from 3,400 to 4,200 of them to each worker of the fuzz run. Each input opens an instance of s4ql against the one fake store of the worker, each instance has an `http.Transport` of its own, and nothing closes its idle connections when the instance closes, so they stay for the `IdleConnTimeout` of 90 seconds. That is a leak in the product, small for an application that opens one instance, and I recorded it in `docs/todo.md` under the S3 client and did not fix it.

### Why

The input was found by a fuzz run of 30 seconds, and nothing in the seed corpus had a value of another type after the key in a table of several columns with every column given; the case in `TestTypeRule` says the same thing at the level of the rule.

### What worked

The cleanup step of the job, which prints every corpus file, made the input exact without an artifact.

### What didn't work

My first copy of the input from the log stopped one line early, without `bool(true)`, and `go test` refused it: `wrong number of values in corpus entry: 2, want 3`. The target has had three arguments since before #73.

### What I learned

A finding of a fuzz run on `main` can be fixed by the next pull request before anyone looks at it. Here the run of `asan-fuzz` on the merge of #73 found what #74, which was already open, fixed an hour later.

### What was tricky

Telling the leak of connections from a slow machine: the fuzz run's rate of 0 per second looked like load until `lsof` counted the connections per process.

### What warrants review

- The claim about the leak in `docs/todo.md`, which I read from the code of `s4ql.go` and `internal/store/s3store` and from the counts of `lsof`, and did not prove with a test.
- Whether the case in `TestTypeRule` belongs there or is one too many: the rule already had a case of a value of another type in another column, with an argument for a TEXT column.

### Future work

The idle connections of a closed instance, in `docs/todo.md`. They may also slow the fuzz jobs of CI, where Linux has about 28,000 local ports.
