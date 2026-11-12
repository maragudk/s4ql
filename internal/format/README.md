# Storage formats

`internal/format` reads and writes the two storage formats of s4ql: the manifest, which is JSON, and the commit object, which is Protocol Buffers. The schema of the commit object is [proto/s4ql/v1/commit.proto](../../proto/s4ql/v1/commit.proto), and `internal/s4qlpb` holds the Go code that `make generate` makes from it.

What the formats hold, and what a reader refuses, is in the code:

- the comments of the schema
- the package comment in `format.go`, with the three kinds of refusal
- the doc comments of `Manifest`, `Name`, `CommitReader`, and `CommitWriter`

This file holds the reasons behind the design. Markus agreed the design on 2026-11-11 and 2026-11-12, and he reviewed the schema line by line in pull request #4 on 2026-11-12.

The research behind it is in three notes: [change-file-encodings.md](../../docs/research/change-file-encodings.md) compares the encodings, [change-file-contents.md](../../docs/research/change-file-contents.md) shows what comparable systems put in their logs, and [protobuf-practices.md](../../docs/research/protobuf-practices.md) reviews the schema against the practices of Protocol Buffers. The notes use older terms: "change file" for a commit object, "record" for what is now a change, and "evidence" for an annotation.

## Terms

- **"Commit" is the action, and "commit object" is what it stores,** the immutable object in S3, as in git. The pointer to the commit object before it is `parent`. "Commit object" replaces "change file", which was our own invention.
- **The names follow SQLite where they can.** Markus: "I'd just like to be consistent and aligned with SQLite if possible." The table name is the field `table` in every kind of change, and a column has a `name` and a `type`. The longer names from SQLite's grammar, `table_name`, `column_name`, and `type_name`, were tried and dropped; Markus: "maybe that's a bit much."
- **`UpsertRows` is Markus's name.** `PutRows`, `InsertRows`, and `ReplaceRows` were considered. An updated row is stored with a key that is already there, so "insert" alone would be wrong.

## Drafts and version 0

Both formats are drafts until we settle on version 1. Markus: "while we're developing, this is all still a draft, so breaking changes are fine until we're settling on a v1." A draft commit object and a draft manifest carry version 0, so that a later release can refuse them cleanly. The rule that field numbers and types never change holds from version 1 on, and so does the check of it with `buf breaking`, which is not set up yet.

## What the format is optimised for

Markus: "we don't optimize this for printing, but for robustness, consistency, speed, and other things. But printing to screen is not one of them."

## Protocol Buffers

Markus asked for "a binary format that is forwards and backwards compatible". The commit object uses Protocol Buffers with the standard tooling: a `.proto` file as the schema, generated Go code, and `google.golang.org/protobuf`.

- **Not Cap'n Proto,** which Markus leaned towards first. Its strengths are reading single fields without decoding, and RPC. A log that is read once from front to back uses neither. Its byte format is a memory layout with pointers and segments, which is more to trust in a file that must stay readable forever.
- **Not a hand-written codec,** which maragubot recommended first. The tooling means less code of ours, a decoder that is fuzzed continuously, the `.proto` file as the real schema, and, from version 1 on, a check in CI that a change to the schema does not break old files. Both ways write the same bytes, so the choice can change later without touching a stored file.
- **The schema uses `edition = "2024"`, not `proto3`.** Every field then records whether it was set, and the generated Go code is the kind that the documentation recommends. The bytes are the same, so this can change.

The reader and the writer handle the top level of the stream themselves with `protowire`, and use the generated code for each record inside it: the header, a change, or an annotation. They do not use `protodelim`, whose default limit of 4 MiB is below SQLite's limit for one value. The limit is `MaxRecordSize`.

## The manifest

- **A branch entry is an object,** so that it can gain fields later.
- **An empty log has a name: 32 zero bytes,** `EmptyLog`. The `head` of a new database is that name, and so is the `parent` of the first commit object. Markus proposed this on 2026-11-11. Both fields are then required, so a forgotten `parent` is an error, and not a commit object that looks like the first one and silently cuts off the log before it. The first commit is also no special case: every commit sets `parent` to the `head` it read.
- **The manifest keeps the fields it does not know** when an instance rewrites it, as the doc comment of `Manifest` says. The lease is renewed often, so without this rule an older version would delete the fields of a newer one at every renewal.
- **There is no count of commits.** Markus: "Drop the "commits", let's keep the essentials."
- **The JSON is handled with `encoding/json/v2`.** Markus, on 2026-11-12: "Make sure to use the new json/v2 from Go stdlib". By default it matches keys exactly and refuses a key given twice, invalid UTF-8, and lone surrogates, and an embedded fallback keeps the fields that the manifest does not know, so none of that needs code of our own. A `depguard` rule in [.golangci.yml](../../.golangci.yml) refuses an import of `encoding/json` in `make lint` and in CI.

## The commit object

### Its shape

- **A header, a list of changes, and a list of annotations,** as the three fields of `CommitObject`, written and read as a stream so that a commit of millions of rows is never in memory at once.
- **The header is a field of `CommitObject`, not a kind of change,** because it appears exactly once. Protocol Buffers cannot mark a field as required, and it accepts bytes where the header is missing, late, or doubled, so the reader checks this itself.
- **Each commit object stands alone.** No state carries over from an earlier one.
- **A commit object holds the net effect of its commit,** and the changes are applied in order. A primary key therefore appears at most once per table.
- **The statements and the changes are not linked.** The commit object does not say which statement caused which change.
- **There are no before-images and no count of changes, for now.** Both can be added later as new fields.

### The kinds of change

- **The kinds follow the roadmap.** A kind is defined when the slice that needs it is built, so `alter table` and indexes each become a new kind. Every `insert`, `update`, and `delete` ends up as upserted and deleted rows, so row changes need nothing new. Adding a column with a default will be one change, with no rows repeated.
- **Each kind has its own message, which holds only what it needs.** Markus asked for this in the review: "DropTable doesn't need columns nor primary key. That's my point."
- **`CreateTable` holds the table as data, not as SQL text,** so that replay needs no SQL parser.
- **`DropTable` does not list the rows as deletes.**
- **A key is its own message, `PrimaryKey`, and not a `Row`,** because it is not a row. Markus asked for this in the review.
- **An update of a primary key is a delete of the old key and an upsert of the new row.**
- **An unknown kind of change is refused, never skipped.** Skipping a `drop table` would silently leave the cache different from the log.
- **A new kind is a new member of the `oneof`, with a new number.** Old commit objects stay valid, and `buf breaking` accepts the addition. Moving a field that is already there into a `oneof` is unsafe.

### Rows carry no column names or types

Replay does not compare a row with the cache's tables. Markus: "The precondition is that the cache is correct up to the parent, and with a deterministic change, the commit is safe and the postcondition is the same." Replay already checks that precondition through the cache's recorded position, so a check of the columns could never fail. The silent failures that the research found come from systems where the reader's tables are managed apart from the writer's, which is not our case. This reverses the "heading" with names and types that was agreed on 2026-11-11.

Two checks remain without extra data, both at replay: a row with the wrong number of values for its table is refused, and the cache's strict tables reject a value of the wrong type.

### Only the changed columns

Markus asked on 2026-11-12: "Wouldn't we rather want only the columns that need changing?" `UpsertRows` therefore lists the columns that its rows hold values for. Markus: "I lean towards the column position first, then the values after."

- **The reason is size.** If a row holds a large text or blob and a commit changes a small value in it, a full row would store the large value again in every such commit, and with no snapshots the log keeps all of them.
- **The positions are on the batch, not on each row.** The rows that one statement changes nearly always change the same columns, so the list is stored once per batch, and rows with different columns go into different batches.
- **No marker for "unchanged" is needed,** because a column that is not listed is not touched. SQLite's changesets and Spanner send only the changed columns too, and both need such a marker apart from NULL. PostgreSQL, MySQL by default, Debezium, and CockroachDB send the full row.
- **The case with only some columns is safe to apply,** because the cache is correct up to the parent.

### Batches

An `UpsertRows` or a `DeleteRows` must be complete in memory before it is written, so it is limited to about one MiB, and a commit that changes many rows of one table writes several. The limit is on bytes, and the format knows the bytes, so `CommitWriter` makes the batches and the engine hands it one row at a time.

### Keys and values

- **The primary key is a list of column positions** in `CreateTable`, in key order. A position cannot disagree with a column name the way a second copy of the name could.
- **A value is in one of SQLite's five storage classes,** under SQLite's names. Null is stored explicitly, because SQLite treats it as a storage class, and because a value with no kind must stay distinct from it: that is either a kind from a newer version or a malformed value, and both are refused.
- **All text is valid UTF-8:** table names, column names, text values, and SQL text. They are `string` fields, which Protocol Buffers refuses to write or read otherwise, and s4ql rejects what would produce other text; see the restrictions in [roadmap.md](../../docs/roadmap.md#restrictions). Markus: "I have a hunch that's going to make life easier down the line."
- **An unknown value of `Type` is refused.** Go keeps it as a plain number without an error, so the reader checks every enum value itself.

### The commit time

The header holds the wall-clock time of the commit, from the lease holder's clock. Replay does not use it. It is there because a later feature, such as restoring to a point in time, needs it, and it cannot be added to old commit objects. The time that a statement read is a different thing: SQLite fixes the time per statement, not per transaction, so it will be a field of `Statement` when a slice needs it.

### Annotations

Annotations are what replay may skip. An annotation is today always one statement: its SQL text and its parameters. Markus chose the name "annotation" over "evidence".

- **Where something sits says whether it may be skipped.** Everything in `changes` must be understood, or the commit object is refused. Everything in `annotations` may be skipped, including a kind of annotation that the reader does not know.
- **The parameters make a statement complete,** so a tool can run a commit's statements through the engine again and compare the outcome with the recorded changes.
- **The cost is size:** the data of an insert is stored roughly twice. Leaving the annotations out can become a setting later.

### Unknown fields

Agreed with Markus on 2026-11-12: a reader refuses a commit object that holds a field it does not know, anywhere outside the annotations.

- **Without the rule, an old reader would silently differ from the log.** It would skip a later field such as "not null" on a column and create the column without the constraint.
- **The rule is mechanical and needs no discipline from us.** The alternative was to raise the header's version for every such field, which depends on remembering it. The version stays for a change of meaning that adds no field.
- **Anything that an old reader may ignore goes into an annotation,** where an unknown field is skipped.
- **The reader never discards unknown fields,** because they are how it finds what a newer version wrote. The research tested the refusal with a small walker over the decoded message, which is what `checkKnown` does.

### The check against the name

The agreement was that a commit object is checked against its SHA-256 name before it is decoded. A stream that is never in memory at once cannot be checked before it is decoded, so the reader checks the bytes as it reads them and reports a mismatch at the end. The purpose stays: nothing that the reader hands out is used before the end, and a corrupt commit object is never reported as needing a newer version. The doc comment of `CommitReader` gives the details.

## Tests

`testdata/` holds sample commit objects and manifests of version 0, and the tests read them. `TestCommitSamples` fails if no sample sets some field of the schema. While the format is a draft, a change to it writes the samples again with `go test ./internal/format -run 'TestCommitSamples|TestManifestSamples' -update`. `manifest-v0-unknown.json` is written by hand, with fields of a pretended newer version at every level.

`make fuzz` runs `FuzzParseManifest`, `FuzzCommitReader`, and `FuzzCommitWriter`. CI runs `make lint-proto`, which is `buf lint` with the `STANDARD` and `COMMENTS` rules and `buf format`, and checks that the committed generated code is what `make generate` makes.
