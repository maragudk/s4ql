# Diary: storage formats

Build the two storage formats of s4ql as code: the manifest, which is JSON, and the commit object, which is Protocol Buffers. Both were agreed with Markus on 2026-11-11 and 2026-11-12 and are recorded in `/docs/todo.md` under "Storage formats", which also holds the only copy of the commit object's schema. The formats are the second of four pieces of the storage core without SQL: the simulation harness, the storage formats, the object store, and the core itself.

## Step 1: Requirements and scope

**Author:** main

### Prompt Context

**Verbatim prompt:** Okay, what's next?
**Interpretation:** The three design questions that came before the storage core are settled (the lease and its clocks, a manifest swap with an unknown outcome, and the storage formats). Markus asks what to build now. I proposed the four pieces, and after a short exchange about the weekly usage limit I started the harness and the formats in parallel, each in its own worktree.
**Inferred intent:** Start building the storage core on what was agreed, without waiting for him on anything that is not a question of design.

### What I did

I read `/docs/todo.md`, the part on streaming in `/docs/research/protobuf-practices.md`, `/.github/workflows/ci.yml`, and the `Makefile`. I created the worktree `.claude/worktrees/storage-formats` on branch `worktree-storage-formats`, wrote this entry, and briefed one builder on Opus.

The requirements handed to the builder. What was agreed with Markus is in `/docs/todo.md` and is not repeated here; this list adds what the code must do.

The schema and its tooling:

1. **The schema is the one in `/docs/todo.md`, unchanged,** in the file `/proto/s4ql/v1/commit.proto`. A change to it is a design question for Markus.
2. **The Go code is generated with the standard tooling** into `/internal/s4qlpb` and committed. The versions of the tools are pinned, and one `make` target generates the code.
3. **CI checks the schema** with `buf lint` under the `STANDARD` and `COMMENTS` rules, and checks that the committed generated code is what the schema generates. `buf breaking` is not set up yet, because the format is a draft and may break.

The commit object:

4. **A stream writer.** It writes the header first, then changes and annotations one at a time, and never holds the whole commit object. The bytes it writes are a valid encoding of one `CommitObject` message, the same as marshalling the whole message. It computes the name, the SHA-256 checksum of all bytes, while it writes.
5. **The writer makes the batches.** The caller hands it rows and primary keys one at a time. The writer groups consecutive ones of the same table, and for rows of the same columns, into `UpsertRows` and `DeleteRows` of about 1 MiB. The order of the changes is kept.
6. **A strict stream reader.** It takes the bytes and the expected name, gives the header and then one change or annotation at a time, and never holds the whole commit object. It refuses, each with an error that says what was wrong:
   - a header that is missing, not first, or doubled; a version other than 0; a `parent` that is not 32 bytes; a missing commit time
   - a field it does not know anywhere outside the annotations, including in `CommitObject` itself
   - a `Change` or a `Value` with no known kind
   - a `Type` it does not know, and `TYPE_UNSPECIFIED`
   - a row whose number of values differs from the number of columns of its batch; an `UpsertRows` with no columns or with a column listed twice; a `CreateTable` whose primary key is empty, lists a column twice, or points outside its columns; an empty table or column name
   - text that is not valid UTF-8
7. **The reader skips** a kind of annotation it does not know, and a field it does not know inside an annotation. It accepts changes and annotations in any order after the header.
8. **The name is checked.** The reader computes the checksum as it reads and reports a mismatch at the end. If decoding fails earlier, it reads the rest, and if the checksum does not match the name, it reports that and not the decoding error. A corrupt commit object is then never reported as "needs a newer version". The caller must use nothing it has read until the reader has reported the end without an error.
9. **The errors tell three cases apart:** the commit object needs a newer version of s4ql; it is malformed; its bytes do not match its name.
10. **The writer refuses what the reader refuses,** so that s4ql can never write a commit object that it cannot read.
11. **A record can be large.** The reader's limit for one record is above SQLite's limit of 1 GB for one value, and is documented.
12. **No second set of types.** The reader and the writer hand out the generated types, unless the builder finds a concrete need for types of our own and says so.

The manifest:

13. **A type for the manifest with reading and writing,** for the JSON in `/docs/todo.md`: `version`, `branches.main.head`, and the optional `lease` with `holder` and `expires`.
14. **The reader refuses** a version other than 0, a missing `main` branch, a `head` that is not 64 lowercase hexadecimal digits, and a `lease` without a holder or without a valid expiry time. A newer version is told apart from a malformed manifest, as for the commit object.
15. **Fields it does not know are kept** when a manifest is read, changed, and written again, at every level: in the manifest itself, in `branches`, in a branch, and in `lease`.
16. **The expiry time** is written in UTC in RFC 3339 format with fractions of a second, and any RFC 3339 time is read.
17. **A name is one type,** for the manifest's `head` and the commit object's `parent` and own name: 32 bytes, written as 64 lowercase hexadecimal digits, and all zero for an empty log.

Testing:

18. **Fuzz tests** show that the two readers never panic on any bytes, that everything the writer writes is read back equal, and that a manifest read and written again keeps every field. They run in `make fuzz`.
19. **Sample commit objects and manifests of version 0 are committed as files,** and a test reads them. They cover every message and field of the schema, including the case of `UpsertRows` with only some columns. While the format is a draft, a change to it regenerates them.
20. **A test shows that the stream and the whole message are the same bytes:** a commit object from the stream writer decodes with `proto.Unmarshal` into one `CommitObject`, and that message marshalled again gives the same bytes.
21. **A test for each thing the readers refuse or skip,** with bytes written by hand or by a schema of a pretended newer version.

Rules that come from the simulation harness, which is built at the same time and will lint this code: no `go` statements, no reading of the clock, no random numbers, and no context timeouts outside tests. The caller passes in the commit time and everything else that varies.

Out of scope: replay into the cache, checks against a table's columns, the object store, the lease logic, and any API of the database. The builder does not edit `/docs/spec.md`, `/docs/todo.md`, `/README.md`, or `/AGENTS.md`, and reports instead what in them is now out of date.

No telemetry: these are pure encoders and decoders with no I/O of their own.

How the goal is verified: `make test`, `make lint`, and `buf lint` pass; each fuzz test runs for at least five minutes without a finding; the tests of points 19 to 21 pass; and a reviewer who changes the reader to accept one of the refused cases sees a test fail.

### Why

The formats rest on nothing else and everything about them is decided, so they can be built while the harness is built. The core needs both: every commit writes a commit object and swaps the manifest, and every start reads them.

### What worked

The research note had already tested the streaming plan: a record on the wire is the byte `0x0a`, a length, and the record, and the result is byte for byte the same as marshalling the whole message.

### What didn't work

Nothing has failed yet. No code has been written.

### What I learned

"A commit object is checked against its SHA-256 name before it is decoded", as `/docs/todo.md` says, cannot hold literally for a stream that is never in memory at once. Point 8 keeps its purpose: nothing is used before the name is checked, and corruption is never mistaken for a newer version.

### What was tricky

Where the batches are made. The engine knows rows and the format knows bytes, and the limit of about 1 MiB is a limit on bytes, so the writer makes the batches.

### What warrants review

Point 8 departs from the wording agreed with Markus, and he has to hear of it. Point 6 lists checks that the schema's comments imply but do not all state, such as an empty primary key.

### Future work

`buf breaking` under the `WIRE` rules is set up when version 1 is settled. `/docs/todo.md` loses what this piece builds.

## Step 2: The schema, its generated code, and the tooling

**Author:** formats-builder

### Prompt Context

**Verbatim prompt:** You are building the storage formats of s4ql, a Go SQL database whose truth is a log in S3, with a local SQLite cache. [...] Build the two storage formats as code: the manifest (`manifest.json`, JSON) and the commit object (Protocol Buffers, written and read as a stream). Both designs are agreed with the product owner, Markus, down to the schema, which he reviewed line by line. You build encoders, decoders, their tests, and the tooling. You do not build anything that uses them.
**Interpretation:** Requirements 1 to 21 of Step 1, plus the lead's additions: decide the packages, the API, the pinning of the tools, and the reader's record limit; never change the schema; refuse every case where a reader could apply a commit object differently from its writer.
**Inferred intent:** Give the builder of the core two formats it can trust without reading their internals, with the refusals proven by tests.

### What I did

I copied the schema from the fenced block in `/docs/todo.md` into `/proto/s4ql/v1/commit.proto` with `awk`, so it is byte for byte the agreed text, comments included. I pinned `protoc-gen-go` v1.36.12 with `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12`, which added one `tool` line and the `google.golang.org/protobuf` requirement to `/go.mod`. Buf v1.75.0 runs as `go run github.com/bufbuild/buf/cmd/buf@v1.75.0`, kept in one `BUF` variable in `/Makefile`, so its 90-odd dependencies stay out of `/go.mod`. `/buf.yaml` sets the module to `proto` and lint to `STANDARD` and `COMMENTS`. `/buf.gen.yaml` runs the local plugin `go tool protoc-gen-go` with `module=maragu.dev/s4ql`, which writes `/internal/s4qlpb/commit.pb.go`. I added `/internal/s4qlpb/doc.go` for a package comment.

New `make` targets: `generate`, and `lint-proto` (Buf's lint and `buf format --diff --exit-code`), which `make lint` now also runs. A new CI job `proto` in `/.github/workflows/ci.yml` runs `make lint-proto`, then `make generate` and fails if `git status --porcelain` shows anything. `make fuzz` now also loops over `/internal/format`.

### Why

Requirements 1 to 3. One place pins each tool: `/go.mod` for the generator and the `Makefile` for Buf, and CI calls the `make` targets so it cannot drift from them.

### What worked

`buf lint` passed with no output on the first run, as the research said it would, and `make generate` twice gave no difference, so the check in CI is stable. The edition 2024 file generated the opaque API without flags.

### What didn't work

Nothing failed in this step.

### What I learned

`go run pkg@version` ignores the current module, so it pins Buf without touching `/go.mod`. The first run costs a build of Buf, which took about 9 seconds here with a warm module cache.

### What was tricky

Where the generated file lands: with `module=maragu.dev/s4ql`, protoc-gen-go strips the module from the `go_package` path and writes to `internal/s4qlpb/commit.pb.go`, and not under `s4ql/v1/`.

### What warrants review

That the `proto` CI job is the right shape. It reruns the generator with the pinned versions, and the generated header names `protoc-gen-go v1.36.12`, so a different generator version also shows as a difference.

### Future work

`buf breaking` under `WIRE`, from version 1 on.

## Step 3: The name and the manifest

**Author:** formats-builder

### Prompt Context

**Verbatim prompt:** (as in Step 2)
**Interpretation:** Requirements 13 to 17, and the manifest's part of 18 and 19.
**Inferred intent:** A manifest that old and new versions can rewrite without losing each other's fields, and that refuses what it cannot read.

### What I did

I wrote one package, `/internal/format`, for both formats, because they share the type of a name and the three error cases. `/internal/format/format.go` holds the package comment and the sentinel errors `ErrNewerVersion`, `ErrMalformed`, and `ErrNameMismatch`. `/internal/format/name.go` holds `Name`, a `[32]byte` with `EmptyLog`, `ParseName` (exactly 64 lowercase hexadecimal digits), `NameFromBytes` (exactly 32 bytes), and the text methods.

`/internal/format/manifest.go` holds `Manifest` (`Head Name`, `Lease *Lease`) and `Lease` (`Holder string`, `Expires time.Time`), with `ParseManifest([]byte) (Manifest, error)` and `Manifest.Marshal() ([]byte, error)`. The zero `Manifest` is a new database. The fields that the manifest does not know are kept in unexported maps of raw JSON at each of the four levels, and are written back after the known fields, in the order of their keys. Other branches than `main` are kept the same way. The output is indented with two spaces like the example in `/docs/todo.md`, with a final newline.

I parse JSON objects myself with `json.Decoder.Token`, not with struct tags, for three reasons found while writing the tests: `encoding/json` matches keys without regard to case, so `"Version"` would be read as the version; it takes the last of two equal keys, where other parsers take the first; and it silently replaces invalid UTF-8, and an escaped lone surrogate such as `"\ud800"`, with U+FFFD, so a rewrite would change the manifest. The parser refuses all of these: invalid UTF-8 in the whole document, lone surrogates in escapes, keys given twice in one object (also when spelled with an escape), and data after the object.

The version is checked first, before anything else is read, because a newer version may change everything else: `0` is read, an integer without a sign, fraction, or exponent above 0 is `ErrNewerVersion`, and anything else is `ErrMalformed`. The expiry is written in UTC with three, six, or nine digits of fraction, whichever is needed to keep every nanosecond, so that a read and a write keep the time exactly. Any RFC 3339 time is read, with a lowercase `t` and `z` made uppercase because Go's parser refuses them. An expiry whose year in UTC is outside 0 to 9999 is refused on both sides, because RFC 3339 has four-digit years.

The samples are `/internal/format/testdata/manifest-v0-new.json` and `manifest-v0-lease.json`, written by `go test ./internal/format -run TestManifestSamples -update`, and `manifest-v0-unknown.json`, written by hand with fields of a pretended newer version at every level. `FuzzParseManifest` checks that the parser never panics, that a manifest it reads it can write, and that the written JSON is the same as the input except for the spelling of the expiry.

### Why

Requirements 13 to 17. A manifest that loses another version's fields would break that version at every renewal, and a manifest that changes on a rewrite would do the same in a smaller way.

### What worked

The table of refusals in `TestParseManifest` drove most of the parser's shape. A minute of `FuzzParseManifest` found nothing.

### What didn't work

My first run of the sample test put the flag before the package:

```
$ go test -shuffle on -update -run TestManifestSamples ./internal/format
# .
no Go files in /Users/maragubot/Developer/s4ql/.claude/worktrees/storage-formats
FAIL	. [setup failed]
```

A test binary's own flag must come after the package: `go test ./internal/format -run TestManifestSamples -update`.

The harness refused a compound command that changed directory twice (`cd .. && cd ..`) with "this command is too complex to verify that it stays inside the worktree". I split such commands into plain ones with absolute paths after that.

### What I learned

Go's `time.Parse` with `time.RFC3339Nano` reads times with and without a fraction, but not a lowercase `T` or `Z`, and not a leap second (`:60`), which RFC 3339 allows. The parser documents that it cannot read a leap second.

### What was tricky

Keeping every field while normalising the known ones. The fuzz test compares the input and the output as generic JSON with the expiry removed, and compares the expiries as times.

### What warrants review

Whether the holder should be validated beyond "a non-empty string". Nothing fixes its format yet. Also what a new holder does with the unknown fields of the old holder's lease: `Lease` keeps them only while the same value is changed in place, and a new `&Lease{}` drops them.

### Future work

None from this step.

## Step 4: The commit object reader and writer

**Author:** formats-builder

### Prompt Context

**Verbatim prompt:** (as in Step 2)
**Interpretation:** Requirements 4 to 12, 20, and 21.
**Inferred intent:** A reader that turns every case where it could replay a commit object differently from its writer into an error, and a writer that cannot produce what the reader refuses.

### What I did

`/internal/format/check.go` holds the checks that the reader and the writer share. `checkKnown` walks a message with `protoreflect` and refuses, as `ErrNewerVersion`, any unknown field (`GetUnknown()` not empty) and any enum value that the descriptor does not list, at every depth. It is run on the header and on every change, and never on annotations, which are records of their own. Then `checkHeader` and `checkChange` do the checks of requirement 6.

`/internal/format/reader.go` holds `CommitReader`: `NewCommitReader(r io.Reader, name Name)`, `Header()`, and `Next() (*s4qlpb.Change, *s4qlpb.Annotation, error)`, which returns `io.EOF` only after the name matched. It reads below the generated code, one record of `CommitObject` at a time: a tag varint, a length varint, and the bytes. The source is wrapped in a reader that records its first error and a `TeeReader` into SHA-256, under a 64 KiB `bufio.Reader`. On any refusal, `fail` reads the rest of the stream, and reports `ErrNameMismatch` instead if the checksum does not match. An error of the source is passed on as it is and wraps none of the three. The buffer of a record grows as its bytes arrive, doubling up to the claimed length, so a corrupt length of 2 GiB allocates no more than about twice what follows.

`/internal/format/writer.go` holds `CommitWriter`: `NewCommitWriter(w io.Writer, parent Name, commitTime time.Time)`, which writes the header, then `CreateTable`, `DropTable`, `UpsertRow`, `DeleteRow`, `Annotate`, and `Close() (Name, error)`. A row or key is marshalled and checked as it is handed in, so the writer copies the caller's values. A batch is kept as the encoded rows with their tags. At a flush, the writer marshals the `UpsertRows` or `DeleteRows` without its rows and appends the rows, which is the next field number. A batch is written when the next change does not belong to it, when adding a row would take it above 1 MiB, and at `Annotate` and `Close`. Any error latches.

Two choices that follow from the requirements:

- The writer refuses a change after an annotation. Requirement 4 asks for the bytes of marshalling the whole message, which puts all changes before all annotations. The reader still accepts any order (requirement 7).
- `MaxRecordSize` is 2 GiB less one byte, the limit of Protocol Buffers for one message, and both sides enforce it. SQLite's `SQLITE_MAX_LENGTH` of 1,000,000,000 bytes, in `/internal/sqlite/c/sqlite3.c`, limits one value and one whole row, so any row fits.

Refusals I added beyond requirement 6, each because a wrong reader would otherwise replay differently or because SQLite cannot hold the value:

- the name of a table or a column that holds NUL, at which SQLite's C interface ends a string, so `t\x00u` would create `t`
- two columns of a create table whose names differ only in the case of ASCII letters, which SQLite treats as the same name
- an upsert rows with no rows, and a delete rows with no keys, which our writer never writes
- a primary key with no values, and keys with different numbers of values in one delete rows
- a real that is NaN, which SQLite stores as NULL
- an invalid commit time (`Timestamp.CheckValid`), and a header without a version, which explicit presence makes visible
- a record whose wire type is not "bytes", a field number of 0, a tag of more than 10 bytes, and a record above the limit

### Why

Requirements 4 to 12. The checks are one set of functions used by both sides, which is how requirement 10 holds by construction.

### What worked

`TestCommitWriter` "should write the bytes of one commit object message" passed on the first run: the stream decodes with `proto.Unmarshal`, and both `proto.Marshal` and the deterministic marshalling give the same bytes again. So the trick of appending encoded rows to a marshalled batch without rows matches what the library writes.

I printed the error of every refusal case once with a temporary `t.Log`, and each fails for its own reason, not an earlier one.

### What didn't work

The first build of the tests failed:

```
internal/format/reader_test.go:208:88: cannot use withUnknown(&s4qlpb.Change{}, 5, protowire.BytesType) (value of interface type proto.Message) as *s4qlpb.Change value in argument to changeRecord: need type assertion
```

The helper that adds an unknown field became generic, `withUnknown[M proto.Message](m M, ...) M`.

### What I learned

Unknown fields set with `ProtoReflect().SetUnknown` are marshalled, so a test can build any newer-version bytes from generated types, at any depth, without a second schema. `protowire.DecodeTag` returns field number -1 for a tag that overflows, and `Number.IsValid` catches it.

### What was tricky

Requirement 8 against requirement 9 when the source fails: if the source returns an error, the reader cannot read the rest, so it cannot tell corruption from a newer version. It then returns the source's error and none of the three cases. A source that fails once and then reports the end would otherwise look like a commit object that was cut short, and give a mismatch; the `sourceReader` that records the first error prevents that.

### What warrants review

- `/internal/format/check.go`: whether any check is too strict for what a later version might legitimately write.
- The reader skips an annotation with no known kind, including an empty one, and hands out a known annotation without checking it, even with a NaN parameter or a value of an unknown kind. The writer checks annotations fully.

### Future work

None from this step.

## Step 5: Samples, fuzz tests, and a mutation pass

**Author:** formats-builder

### Prompt Context

**Verbatim prompt:** (as in Step 2)
**Interpretation:** Requirements 18, 19, and 21, and the last criterion of Step 1: a reviewer who changes the reader to accept a refused case sees a test fail.
**Inferred intent:** Evidence that the tests guard the refusals, not just that they pass.

### What I did

`/internal/format/testdata/commit-v0-first.bin` and `commit-v0-all.bin` are written by the writer in `TestCommitSamples` with `-update`. The test checks that the writer still writes the same bytes, and that the reader reads what an independently built list of messages says. A subtest walks every field of every message in `s4qlpb.File_s4ql_v1_commit_proto` and fails if no sample sets it, so the samples cover the schema, including an `UpsertRows` with only some columns. `buf convert` reads `commit-v0-all.bin` as a `CommitObject` and prints it as JSON.

`FuzzCommitReader` reads any bytes with their own name and with another. With their own name, the result is a commit object that `proto.Unmarshal` also reads with the same number of changes, or `ErrMalformed` or `ErrNewerVersion`, never a mismatch. With another name, it is always `ErrNameMismatch` and nothing else. `FuzzCommitWriter` turns the input into up to 200 operations on three tables, writes them, reads them back, splits the batches into single rows, and compares, and checks the whole-message bytes again. It includes values of 300 KiB so batches fill.

Then I wrote a scratch script, not committed, which applies each of 92 mutations to the checks alone, such as `if false {` in place of a condition, and runs the tests. The first run left nine survivors. Five were gaps, now closed by tests in `/internal/format/reader_test.go`, `/internal/format/manifest_test.go`, and the new `/internal/format/reader_internal_test.go`:

- a change record with the wire type of a fixed 32-bit number, followed by valid bytes, was accepted when the wire-type check was removed
- the record limit could not be tested at 2 GiB, so the reader and the writer now hold it in a field that the internal test lowers
- a source that fails once and then ends gave a mismatch when the check of the source's error was removed
- a lone low surrogate `"\udc00"` in the manifest was untested, and a check for a high surrogate at the end of the input was unreachable and is gone

The four that survive are expected: `!h.HasParent()` and `!h.HasCommitTime()` overlap with the length check of the parent and with `CheckValid`, and "create table with no columns" overlaps with the primary key's checks; they stay for their clearer messages. Deterministic marshalling changes nothing while the schema has no map.

### Why

A passing test of a refusal says little if the refusal can be removed without a failure. The mutation pass is the check that the lead's last criterion asks a reviewer to do.

### What worked

Every other mutation, including `DiscardUnknown: true` in the reader's options and dropping the copy of the caller's columns, fails a test.

### What didn't work

To check the coverage subtest, I edited a sample's statement with `sed` and ran with `-update`, then ran `git checkout internal/format/testdata/` to undo it. The sample files were not committed yet, so `git checkout` restored nothing ("Updated 0 paths from the index") and the samples held the edited content. I put the statement back and wrote the samples again, and the test passed against them. Only then did I commit them.

Short fuzz runs printed long stretches of `execs: ... (0/sec)`, for example `fuzz: elapsed: 42s, execs: 2765726 (0/sec)` for the manifest, while new interesting inputs were found just before. I did not find a slow input, and the runs ended normally.

### What I learned

`-shuffle on` orders top-level tests, but the go skill says not to rely on the order of subtests either, so the coverage subtest reads the sample files itself and does not use what other subtests saw.

### What was tricky

Building the fuzz writer so that every generated operation is valid, except a change after an annotation, which must be refused: keys of a table always have the same number of values, and a NaN real becomes NULL.

### What warrants review

Whether the samples should also include a commit object that a pretended newer version wrote, kept as a file. The tests build such bytes in code.

### Future work

None from this step.
