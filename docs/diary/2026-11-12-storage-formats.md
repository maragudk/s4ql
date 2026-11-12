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
