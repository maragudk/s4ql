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

## Step 6: Self-review and verification

**Author:** formats-builder

### Prompt Context

**Verbatim prompt:** When the implementation is done, self-review it as your standing instructions say, fix what is real, and run `make test`, `make lint`, `buf lint`, and the fuzz tests again.
**Interpretation:** Run the code-review skill over the branch, fix the real findings, and verify the goal of Step 1 on the final code.
**Inferred intent:** Hand back work whose claims have been checked by someone other than its author.

### What I did

I ran the `code-review` skill at level `high` on `main...worktree-storage-formats`. It returned nine findings, and confirmed the first three with probes. I fixed five:

- **The writer reported a record above the limit from a later call.** A row only went into the batch, and the size was checked at the flush, so the next `DropTable` or `Close` failed with an error about another change. Now `add` computes, before anything is written, the size of the change with the row added, and the size of a change of the row alone. It starts a new batch when the first is too large, and refuses the row in the same call when the second is. Three new tests in `/internal/format/writer_internal_test.go` cover this.
- **`time.Parse` with `time.RFC3339Nano` accepts what RFC 3339 does not**: a comma before the fraction, and offsets of `+24:00` and `+23:60`. Probe output: `2026-11-11T12:00:30,123Z 2026-11-11 12:00:30.123 +0000 UTC <nil>` and `2026-11-11T12:00:30+24:00 2026-11-11 12:00:30 +2400 +2400 <nil>`. The expiry is now matched against the syntax of RFC 3339, section 5.6, before it is parsed. More than nine digits of fraction are still read, to the nanosecond, and the doc comment says so.
- **The writer copied a large row several times.** A batch now keeps headroom of 30 bytes in front of its buffer. The rows are marshalled straight into the buffer with `MarshalAppend` after their size is known from `MarshalOptions.Size`. At the flush, the tags and lengths of the record and the change go into the headroom, and the buffer is written as it is. A non-batch record is marshalled once into its final buffer. Both check that `MarshalAppend` wrote exactly `Size` bytes.
- **The checks built a path string for every row and value even when nothing failed.** They now return a `checkError` without a path. Each level adds its own name only when a check below it refuses, so a check that passes allocates no strings. A new test pins two messages, for example `malformed: record 2.upsert_rows.columns[2] lists column 0 twice`.
- **Field numbers were constants written by hand.** They now come from the generated descriptors through `fieldNumber`, which panics at start if a field is renamed.

I did not change four things:

- **The reader accepts a change after an annotation.** Requirement 7 says that it accepts changes and annotations in any order after the header. Only the writer keeps the order, for requirement 4.
- **The UTF-8 checks run again on reading** after `proto.Unmarshal` has checked strings. The reader and the writer share one set of checks, which is how requirement 10 holds by construction. A UTF-8 check costs little next to the unmarshalling.
- **`/docs/todo.md` still says the formats are not built** and still holds a copy of the schema. I may not edit it, so it goes in my report.
- **The reader hands out changes before the name is checked,** where `/docs/todo.md` says a commit object "is checked against its SHA-256 name before it is decoded". This is the departure of point 8 that Step 1 already records, and it is for the lead to take to Markus.

The rework itself produced a bug that the error messages showed: `checkColumnPositions` and its caller both added `columns`, giving `record 2.upsert_rows.columns.columns[2]`. Each now adds its own name once.

I ran the mutation script again on the reworked code. A new survivor showed that the copy of the caller's column positions was no longer covered: the bytes of a batch are now written when it starts, so only the comparison with the next row reads the stored positions. The test "should copy the values it is handed" now hands in a second row with the changed slice. With `slices.Clone` removed it fails, and with it there it passes. The four survivors that remain are the ones of Step 5.

### Why

The goal of Step 1 is verified on the code that is handed back, not on the code before the review.

### What worked

The results on the final code, commit `6732cea`:

- `make test`: all packages pass; `/internal/format` has 95.3% coverage. `go test -race -shuffle on ./internal/format` passes.
- `make lint`: `golangci-lint` reports 0 issues in both modules, and `buf lint` and `buf format --diff --exit-code` print nothing.
- `make generate` changes nothing, and `go mod tidy` changes neither `/go.mod` nor `/go.sum`.
- Each fuzz test for five minutes with `-fuzztime 5m`, with no finding: `FuzzParseManifest` 21,395,874 runs, `FuzzCommitReader` 20,594,326, `FuzzCommitWriter` 1,870,485. A run before the review fixes gave 21,025,036 and 29,347,017 for the first two.

### What didn't work

My first round of five-minute fuzz runs ran in the background while I edited the checks, so the third test was built from code in the middle of a change:

```
== FuzzCommitWriter
	have (*s4qlpb.Annotation, string)
	want (*s4qlpb.Annotation)
./writer.go:248:31: too many errors
FAIL	maragu.dev/s4ql/internal/format [build failed]
```

I ran all three again after the last commit and left the code alone until they finished.

### What I learned

`MarshalOptions.Size` with the same options gives the length that `MarshalAppend` writes, so a writer can lay out a record before it marshals into the record. A `oneof` or other field whose wire type differs from the schema is kept by Go as an unknown field, so the reader reports it as needing a newer version, not as malformed. The checksum is compared first, so this happens only for bytes that match their name.

### What was tricky

Keeping the bytes of a batch identical to marshalling the whole message while writing them in place. The test of requirement 20 and `FuzzCommitWriter` both compare the stream with `proto.Marshal` of the decoded message, and they kept passing through the rework.

### What warrants review

- `/internal/format/writer.go`, `add` and `flush`: the arithmetic of the headroom and of `changeSize`.
- `/internal/format/check.go`, `checkError` and `in`: `in` changes the path of an error it is handed, which is safe because every `checkError` is new and used once.

### Future work

- `/docs/todo.md` loses the item "Storage formats", and the reasoning in it moves next to the code where it is not there yet; the lead decides with Markus.
- The departure of point 8 from "checked against its SHA-256 name before it is decoded" needs Markus's agreement.

## Step 7: The manifest on `encoding/json/v2`, and a guard against version 1

**Author:** json-builder

### Prompt Context

**Verbatim prompt:** Markus, the product owner, said: "Make sure to use the new json/v2 from Go stdlib". The package `internal/format` was built today, and its manifest code in `internal/format/manifest.go` uses `encoding/json` (version 1) with a good deal of code of its own around it. Move it to `encoding/json/v2` and `encoding/json/jsontext`. [...] No `encoding/json` version 1 in the module [...] Use what version 2 gives, and delete what it makes unnecessary. [...] The behaviour of the manifest stays as it is. [...] The sample files stay byte for byte the same [...] A guard against version 1 coming back. [...] No other change of behaviour, and no change to the commit object's reader, writer, or schema.

A second message arrived mid-run: "One small addition to your task, as its own commit: fix a stale comment in `proto/s4ql/v1/commit.proto`. In message `Change`, the comment on the field `upsert_rows` says "Inserts rows into a table, or replaces the rows with the same keys." Change it to "Inserts rows into a table, or changes rows that are there." [...] Then run `make generate` and commit the schema together with the regenerated `internal/s4qlpb/commit.pb.go`, and check that `make lint-proto` passes."

**Interpretation:** Replace the hand-written JSON object parser and writer of the manifest with struct tags and embedded fallbacks of version 2. Keep every pinned behaviour and the sample bytes. Move every other import of `encoding/json` in the repository to version 2. Add a lint rule that keeps version 1 out. Separately, fix one comment in the schema.

**Inferred intent:** Less code of our own to trust in the manifest, with the stricter and safer defaults of version 2, and no way for version 1 to come back unnoticed.

### What I did

The schema comment came first, as commit `fc0d02f`: the one comment line in `/proto/s4ql/v1/commit.proto` and the two lines that `make generate` changed in `/internal/s4qlpb/commit.pb.go`. `make lint-proto` passed.

In `/internal/format/manifest.go`, `ParseManifest` and `Manifest.Marshal` now go through four unexported structs, `manifestJSON`, `branchesJSON`, `branchJSON`, and `leaseJSON`. Each has a field `Unknown map[string]jsontext.Value` with the tag `json:",embed"`, the embedded fallback of version 2, which collects the members that the struct does not know. The head is a `*Name`, so `Name.UnmarshalText` checks the digits. `decodeObject`, `decodeString`, `checkSurrogates`, `appendObject`, `appendString`, and `member` are gone, and so is the explicit UTF-8 check on the holder: `json.Marshal` refuses invalid UTF-8, and its error is wrapped in `ErrMalformed`. The code without comments and blank lines went from 328 lines to 161.

These parts of our own stay. Each one does something that version 2 does not:

- **A first pass that reads only `version` and `lease` as `jsontext.Value`.** A newer version must be told apart even when the rest does not fit the structs, such as `{"version":1,"branches":[]}`. A null `lease` must be refused, and a `*leaseJSON` reads null as nil, the same as a missing lease.
- **`checkManifestVersion` on the raw text of the version.** Version 2 cannot tell `0.0`, `0e0`, and a version too large for `uint64` apart the way the tests want.
- **The nil checks for `branches`, `main`, and `head`,** which version 2 leaves nil when they are missing or null.
- **The parsing and formatting of the expiry,** with `rfc3339`, `checkExpiry`, and `formatExpiry`, as the brief asked.

`Marshal` calls `json.Marshal` with `jsontext.WithIndent("  ")`, `json.Deterministic(true)`, `jsontext.PreserveRawStrings(true)`, and `jsontext.EscapeForJS(true)`. The first two give the layout and the sorted unknown keys. With `PreserveRawStrings`, unknown values keep their escapes, as `json.Compact` and `json.Indent` kept them before. With `EscapeForJS`, U+2028 and U+2029 in our own strings and in keys are written as `\u2028` and `\u2029`, as the encoder of version 1 always wrote them.

The other users of version 1 were `/internal/format/name_test.go`, `/internal/sim/simulation_test.go`, `/internal/sim/toy_test.go`, `/internal/sqlite/sqlite_test.go`, and `/internal/sqlite/update/main.go`. All except the update tool only needed the import changed. The update tool's `json.MarshalIndent(m, "", "\t")` became `json.Marshal(m, jsontext.WithIndent("\t"), json.Deterministic(true))`. A throwaway program read `/internal/sqlite/c/source.json` back and marshalled it 20 times with the new call, and got the same bytes each time. In the test helper `decodeJSON` of `FuzzParseManifest`, an `UnmarshalFromFunc` for `*any` keeps each number as its text, as `UseNumber` did before. A throwaway test checked that nested numbers come out as text and strings stay strings.

The guard is the new `/.golangci.yml`: version 2 of the configuration, the standard linters, which are the ones that ran before, and `depguard` with a rule that denies `encoding/json$`. The `$` makes the match exact, so `encoding/json/v2` and `encoding/json/jsontext` pass. Before the migration, `golangci-lint run` reported `internal/format/manifest.go:5:2: import 'encoding/json' is not allowed from list 'json': use encoding/json/v2 and encoding/json/jsontext (depguard)`. In `/internal/sqlite/crosscheck`, `golangci-lint config path` prints `../../../.golangci.yml`, and a temporary test file there that imported `encoding/json` was reported. CI's reusable workflow, read with `gh api repos/maragudk/workflows/contents/.github/workflows/lint.yml`, runs `golangci/golangci-lint-action@v9` with `version: latest` at the repository root, so it reads the same file. The crosscheck job runs the action with `working-directory: internal/sqlite/crosscheck`, where the configuration is found in the parent directories. Generated files are excluded by golangci-lint's default, so `/internal/s4qlpb/commit.pb.go` is not checked. It does not import `encoding/json` anyway.

I checked the new code against the old with a differential test that I did not commit. It was the old `manifest.go` with every identifier prefixed by `old`, in a temporary internal test file, and two fuzz targets. One compared the error class and the `Marshal` bytes of both parsers on any input, for 3 minutes and then 1 more minute: 7,728,981 and 4,960,195 runs. The other compared `Marshal` of a lease with any holder and expiry, for 1 minute: 2,784,090 runs. They found no difference other than the two below. Each was skipped by name in the target.

`/internal/format/README.md` has a new point under "The manifest" with Markus's request and the guard. The doc comments of `ParseManifest` and `Marshal` say what version 2 now decides.

For the self-review I ran the `fabrik:code-review` skill on commit `e2a314c`. Two reviewers each ran their own differential fuzz against the old code, and both found nothing serious. Both found the same set of minor points, and I fixed them in commit `6f5e824`:

- No test pinned that the version is checked before the lease. Swapping the two checks passed every test.
- No test pinned the order of unknown keys at every level. The one ordering test had two unknown keys at the top level, so removing `Deterministic` failed it only about 7 runs in 8.
- No test covered the escaping of U+2028 in unknown values.
- The aliasing test covered one level only.
- The comment "The version comes first" said more than the code does. The `Marshal` doc said that unknown fields keep their escapes, which holds for their values but not for their names.
- A missing expiry was reported as `manifest lease expiry "" is not an RFC 3339 time`. It now says `has no expiry`.
- The README point claimed "anywhere in the repository", which files behind a build tag disprove.
- `jsonNumber` was a package-level type used in one function.

I left two points from the review alone. The other error messages are less precise than before, and some name the unexported types, as in `cannot unmarshal JSON number into Go format.leaseJSON`. Every manifest is also decoded twice. One pass with `branches` and `lease` as raw values would still not keep the version ahead of the duplicate check, and it would need more code.

New tests:

- the escapes and numbers of unknown values are written as read
- the order of unknown keys at all four levels, with five keys each
- U+2028 and U+2029 are escaped in a holder and in unknown values
- a parsed manifest does not change when its input bytes change, at all four levels
- version 1 is newer even with a null lease, or a lease that is not an object
- a key given twice inside an unknown value is refused, also in a newer version

### Why

Markus asked for version 2. Its defaults are the rules that the old parser was written to enforce: exact keys, no duplicate names, no invalid UTF-8, no lone surrogates. With version 2 they come from the standard library, which is tested far more than our parser was.

### What worked

- All tests of `/internal/format/manifest_test.go` passed on the first run of the new code. The three sample files in `/internal/format/testdata/` are byte for byte the same, and `TestManifestSamples` checks them without `-update`.
- The checks on the final commit `6f5e824`: `make test` passes in both modules, and `/internal/format` has 96.9% coverage, with every block of `manifest.go` covered. `go test -race -shuffle on ./internal/format/` passes. `make lint` reports 0 issues in both modules, and `buf lint` and `buf format` print nothing. `FuzzParseManifest` ran 5 minutes with `-fuzztime 5m` with no finding, both on `e2a314c` (24,987,689 runs) and on `6f5e824` (27,298,043 runs). I did not touch the code while either run was going.
- Red checks for the options. Without `PreserveRawStrings` the escape test fails. Without `EscapeForJS` the U+2028 test fails. Without `Deterministic` the new order test failed in 5 of 5 runs.

### What didn't work

- My first experiment, a module in the scratchpad directory, was refused: "This agent is isolated in the worktree [...], but this command is too complex to verify that it stays inside the worktree." I moved the experiments to `/tmpexp/` in the worktree and deleted it afterwards. A directory named `source` there could not be renamed or deleted, because any command with the word `source` in it was refused as "runs a string through source". I deleted it with the parent directory.
- The editing tools turn the text `\u2028` or `\u0041` in what I write into the raw character. My first U+2028 test held a raw U+2028 in a raw string, and it failed with `manifest_test.go:288: Not true`, although the output was right. I write such escapes with a placeholder and replace it with `perl -pi -e 's/@@/\x5c/g'`, and I check the bytes with `sed 's/\\/BS/g'`.
- The same mangling had already weakened three things in the tests of Step 3. "should refuse a key given twice in another spelling" gave `"x"` twice and no escape. "should read an escaped surrogate pair" read a raw emoji. The seed of `FuzzParseManifest` had a raw emoji. The first two now use `\u0078` and `\ud83d\ude00` again. The seed is kept, and a second one with the escaped pair was added.
- The first `make lint` with the new rule showed only 3 of the 7 imports. golangci-lint's default `max-same-issues` is 3, so this was not a gap in the rule.

### What I learned

- In Go 1.27 the tag for an embedded fallback is `json:",embed"`, not `inline` or `unknown`. `go doc encoding/json/v2` lists only `omitzero`, `omitempty`, `string`, `case`, and `embed`.
- The fallback is written after the declared fields. Under `Deterministic`, its keys are sorted with `slices.Sort`, which is byte order, as `slices.Sort` sorted them before. The documentation promises neither, so the tests pin both. See "What warrants review".
- `PreserveRawStrings` keeps the escapes of raw values, but `EscapeForJS` still applies to them.
- Unmarshalling into `jsontext.Value` copies, so the manifest does not alias the caller's bytes.
- A JSON null in a pointer field gives nil, the same as a missing field. Where the two must differ, the value has to be read raw.

### What was tricky

There are two intended changes of behaviour, both from version 2:

- **A key given twice is refused at any depth,** also inside an unknown value. The old parser checked only the objects it parsed itself: the manifest, `branches`, `main`, and `lease`. `{"version":0,...,"x":{"a":1,"a":2}}` was accepted and written back with the duplicate. The doc comment always said "a key given twice in the same object". Because the whole document is checked before the version, `{"version":1,"x":{"a":1,"a":2}}` and `{"version":1,"branches":{"main":1,"main":2}}` are now `ErrMalformed`, where they were `ErrNewerVersion`. Keeping the old split would mean `jsontext.AllowDuplicateNames(true)` in the first pass, and then `{"version":0,"version":1}` would no longer be refused.
- **A raw U+2028 or U+2029 inside an unknown value is written as `\u2028` or `\u2029`.** Before, it was written raw. It is the same JSON value. The alternative, without `EscapeForJS`, would write our own holder and keys with raw U+2028 where version 1 escaped them. I chose to keep what we write ourselves the same.

### What warrants review

- **The output order rests on how version 2 behaves, not on a promise.** `go doc encoding/json/v2.Deterministic` guarantees determinism only "across different instances of identical binaries, but not across different builds of a program (such as different source or toolchain version [...])". Where the fallback goes is not documented either. A Go upgrade that changed either would fail the new order test and `TestManifestSamples`. It would not go unnoticed, but it would need code of our own again.
- `/internal/format/manifest.go`, `ParseManifest`: the two passes, and the comment on why the lease is read raw.
- `/.golangci.yml`: the rule and its comment. Files behind a build tag that is off on the linting platform, such as `/internal/sqlite/asan.go` with `//go:build asan`, are not checked. A reviewer found this with a probe file.
- `/internal/sim/toy_test.go`: a change file with no writes is now written with `"sets":[]` instead of `"sets":null`, so its name changes. Nothing pins names, and the oracle compares with `slices.Equal`.

### Future work

- If the order of the output must hold across Go versions, and not only be caught by the tests, the writer needs its own ordering again, for example through a `MarshalerTo` on the structs. That is for the lead and Markus to decide.

## Step 8: The lease entry without clocks compared between machines

**Author:** lease-builder

### Prompt Context

**Verbatim prompt:** One change to a storage format of s4ql [...] The manifest, `manifest.json`, is read and written by `internal/format/manifest.go`. Its `lease` entry holds a holder and an absolute expiry time today. The product owner, Markus, agreed on 2026-11-12 to a new design of the lease that compares no clocks between machines: another instance takes the lease only after it has watched the manifest stay unchanged for the lease's duration, on its own monotonic clock. The lease entry changes to this: `"lease": {"holder": "5b0e…77c3", "duration": 30, "renewed": "2026-11-12T12:00:30.123456789Z", "salt": "c41d07a9e2b35f68"}` [...] `expires` is gone. [...] 1. `format.Lease` holds the four values, with Go types that make a wrong value hard to write [...] 2. The reader refuses, as malformed: a lease without any of the four [...] 3. `Manifest.Marshal` refuses a lease that the reader would refuse [...] 4. The package does not draw the salt and does not read the clock [...] 5. Everything else about the manifest stays as it is [...] 6. Tests first [...] 7. `internal/format/README.md` and the doc comments describe the lease entry as it now is [...] 8. Nothing outside `internal/format` uses `format.Lease` yet; check that [...]

A change arrived mid-run from the lead, from Markus: "the lease's `duration` is written in Go's `time.Duration` format, as a JSON string, not as a number of seconds. He said: "Maybe even use the Go time.Duration format?" So the entry reads `"duration": "30s"`. [...] The reader accepts only that canonical spelling: parse it, and refuse it as malformed unless `String` of the parsed value equals the text that was read. [...] A duration of zero or below is refused. A fraction of a second is allowed (`500ms`) [...] `Manifest.Marshal` refuses a duration of zero or below. [...] Check in `go doc encoding/json/v2` how version 2 handles `time.Duration` [...]"

**Interpretation:** Replace `Lease.Expires` with `Duration`, `Renewed`, and `Salt`, read and written with exact spellings: the duration only as `time.Duration.String` writes it, the renewal time under the rules the expiry had, and the salt as exactly 16 lowercase hexadecimal digits. Break old manifests at version 0. Keep every other behaviour of the manifest.

**Inferred intent:** A lease that watches the manifest's bytes needs the holder's duration, and a manifest whose bytes change at every renewal. The format must make both hard to get wrong.

### What I did

- Merged `origin/main` into the branch first. It fast-forwarded to `e116bdc`, which contains "Return the lease expiry of `format.ParseManifest` in UTC".
- Checked requirement 8: `grep` finds `format.Lease` only in `/internal/format/`. `/internal/sim/toy_test.go` has an `Expires` of its own on the toy's manifest type, which is not `format.Lease`, so nothing outside moved.
- Read `go doc -all encoding/json/v2`. It says: "A Go time.Duration currently has no default representation and results in a SemanticError, unless the encoding/json.FormatDurationAsNano option is specified". It documents no `format:` tag option for durations. So `leaseJSON` holds the duration, the renewal time, and the salt as strings, and `parseLease` checks each spelling.
- Tests first in `/internal/format/manifest_test.go`. I rewrote the lease JSON of the existing tests with `perl` substitutions instead of the editing tools, because several of those lines also hold `\u` escapes, and checked the bytes with `sed 's/\\/BS/g'` afterwards. New tests read and write each field, and there is one refusal row per case below. `go vet ./internal/format/` was red: `m.Lease.Duration undefined (type *format.Lease has no field or method Duration)`.
- `/internal/format/manifest.go`: `Lease{Holder string; Duration time.Duration; Renewed time.Time; Salt uint64}`. `parseLease` refuses a missing or null field by name, then calls `parseDuration`, `parseRenewed`, and `parseSalt`. `parseDuration` is `time.ParseDuration`, then `d.String() == s`, then `d > 0`. `parseRenewed` is the old expiry code, renamed. `parseSalt` checks 16 lowercase hexadecimal digits, as `format.ParseName` does, and then calls `strconv.ParseUint`. `Marshal` refuses a duration of zero or below, and writes `Duration.String()` and `fmt.Sprintf("%016x", Salt)`. The package comment in `/internal/format/format.go` now says that the caller passes in the renewal time and the salt.
- Regenerated the samples with `go test ./internal/format -run 'TestCommitSamples|TestManifestSamples' -update`, and edited `/internal/format/testdata/manifest-v0-unknown.json` by hand. `git diff internal/format/testdata` shows changes only inside `lease`, in both manifest files. The commit samples are unchanged.
- A writer sub-agent added six bullets to "The manifest" in `/internal/format/README.md`. I checked them against the brief and kept them. I also added `Lease` to the README's list of doc comments.
- Commits: `3c5c2fd` (the change) and `8d19edc` (the review fixes).

The refusals added to `ParseManifest`, all `ErrMalformed`:

- a lease without a duration, without a renewal time, or without a salt, and each of these given as null
- a lease with `expires` and no `renewed`, and the old form with only `holder` and `expires`
- a duration that is a JSON number or an empty string, or that is `0s`, `0`, `-30s`, `+30s`, `0.5m`, `30000ms`, `30s0ms`, `1m`, `90s`, `1.50s`, `030s`, `1us`, `1μs` (Greek mu), ` 30s`, `30S`, `30`, `3e1s`, or `2562047h47m16.854775808s` (overflow)
- a salt that is a JSON number or an empty string, of 15 or 17 digits, with uppercase digits, with a `g`, with `0x`, with `+`, with `_`, or with a space
- every refusal of the old expiry, now of `renewed`

`Marshal` now refuses a duration of zero, of -1s, and of `math.MinInt64`, besides what it refused before.

### Why

The brief asks for Go types that make a wrong value hard to write. `time.Duration` and `uint64` cannot hold a fraction of a nanosecond or a salt that is not hexadecimal. The wrong values left are a duration of zero or below, which both sides refuse, and a renewal time outside the years 0 to 9999, which both sides refused before. The canonical spelling gives one duration one spelling, so `FuzzParseManifest` compares the duration and the salt of the input and of the output as text.

### What worked

- The refusal tests passed on the first run of the new code. A mutation check showed that they test what they say. With `|| d.String() != s` removed, ten of the duration rows and the fuzz seed with `90s` failed. With the duration check in `Marshal` disabled, its three new rows failed. Both were reverted, and `git diff` was checked afterwards.
- On `8d19edc`: `make test` passes in all modules, with 96.9% coverage in `/internal/format`. `go test -race ./internal/format/` passes. `make lint` reports 0 issues in all three modules, and simlint and `buf` print nothing. `FuzzParseManifest` ran 5 minutes with `-fuzztime 5m` and found nothing, in 29,468,643 runs. I did not touch the code while it ran.
- `time.Duration(math.MaxInt64).String()` is `2562047h47m16.854775807s`, and `time.ParseDuration` reads it back. So no upper limit of our own is needed: `ParseDuration` refuses anything beyond the range of `time.Duration`, and every positive `time.Duration` round-trips. The tests pin both ends.

### What didn't work

- My first mutation of the `Marshal` check, a `perl` substitution that split one `if` into two, left `err` undefined, and `go test ./` printed `FAIL maragu.dev/s4ql/internal/format [build failed]`. A second substitution, `err != nil && false`, worked, and I reverted it the same way.
- The first commit had a gofmt issue in the test file: `Head:  format.Name(...)` was still aligned for a field that I had made multi-line. `make lint` did not report it. `gofmt -l internal/format` did, and `8d19edc` fixes it.
- Appending this step with a `cat >> ... <<'EOF'` here-document was refused: "This agent is isolated in the worktree [...], but this command is too complex to verify that it stays inside the worktree." I appended it with the editing tool instead.
- I first started the five-minute fuzz run on `3c5c2fd`. I stopped it when the review led to a code change, and ran it again on `8d19edc`.

### What I learned

- `encoding/json/v2` in Go 1.27 has no default representation for `time.Duration`. Without `FormatDurationAsNano` it gives a `SemanticError`, so a `time.Duration` field in `leaseJSON` would not work.
- `time.Duration.String` writes microseconds with U+00B5 (micro sign), and `time.ParseDuration` also reads `us` and U+03BC (Greek mu). The canonical check refuses the last two. The test file holds each sign as raw UTF-8. I checked them with `xxd`: `c2 b5` in the accepted row, and `ce bc` in the refused row.
- `Duration.String` writes a whole hour as `1h0m0s` and a whole minute as `1m0s`, so `1m` and `1h` are refused.

### What was tricky

- Requirement 3 and the fuzz property push against each other on zero values. The reader accepts `0000000000000000` as a salt, because it is a valid draw, and `0001-01-01T00:00:00Z` as a renewal time, because the old rules accepted it. If `Marshal` refused a zero `Salt` or a zero `Renewed`, a manifest that was read could not always be written again, and `FuzzParseManifest` would fail. So `Marshal` accepts both, and a holder that forgets to set them is not caught here.
- A JSON escape such as `"1µs"` decodes to the canonical text and is accepted, and the writer then writes the raw character. That is the same as for the holder: the known fields are written in their shortest form.

### What warrants review

Self-review with the `code-review` skill at medium effort over `e116bdc..HEAD` gave seven findings. I acted on two:

- The hand-written hex parser in `parseSalt` is now the digit check of `format.ParseName` plus `strconv.ParseUint`. The error branch after `ParseUint` cannot be reached after the digit check, and it is kept as a guard.
- The doc comment of `ParseManifest` said that a lease with `expires` and no `renewed` "lacks its renewal time". The error for the old form says `has no duration`, because the duration is checked first. The comment now says that `expires` is a field it does not know, and that such a lease is refused like any lease that lacks a field.

I left the other five alone:

- `/docs/spec.md` line 39 and `/docs/todo.md` still describe the absolute expiry. The brief forbids editing either, and says that a writer is rewriting the todo item. The spec's wording needs Markus's agreement, so I report it to the lead.
- `Marshal` accepts a zero salt. The reason is under "What was tricky", and it is a question for the lead.
- `Marshal` accepts a zero renewal time, for the same reason.
- An `expires` next to `renewed` is kept and written back at every renewal. Requirement 2 asks for exactly this.
- The checks for an empty field repeat what the parsers refuse. I kept them for their error text, such as `has no salt`, as Step 7 did for the expiry.

To validate: `go test -shuffle on ./internal/format/`, then `git diff e116bdc -- internal/format/testdata` (only `lease` changes), then `cd internal/format && go test -run '^$' -fuzz '^FuzzParseManifest$' -fuzztime 5m .`.

### Future work

- The lease code that uses `format.Lease` must draw a new salt at every write, and set `Renewed` from the host's wall clock through the host interface that simlint allows. Nothing in the format can check that the salt is new.
- `/docs/spec.md` needs the new lease design in its "Constraints", in wording that Markus agrees to.
