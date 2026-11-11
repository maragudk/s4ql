# Protocol Buffers practice for the commit object

Date: 2026-11-11. Tags: **V** = verified in this run in a primary source (the official documentation, the language or encoding guide, source code, or a release page), or tested, with the reference. **I** = inferred, or from memory.

Question: what are the best practices of Protocol Buffers for data that is stored forever, and what do they say about the draft schema of the commit object in `docs/todo.md`? The product owner asked in particular whether new kinds can be added to a `oneof` later. The choice of Protocol Buffers and the contents of a commit object are covered in [change-file-encodings.md](change-file-encodings.md) and [change-file-contents.md](change-file-contents.md), which say "change file" for what is now the commit object. This note does not repeat them.

Short answer:

- **Yes, a new kind can be added to a `oneof` later.** Old files stay readable by new readers. An old reader that meets the new kind does not fail: it sees "no kind set" and keeps the bytes as an unknown field. In Go, our reader can tell this from an empty record, because a `Record` holds nothing but its `oneof`. So the agreed rule "an unknown kind of change is refused" can be made true, and the breaking-change checker does not flag the addition. Never move an existing field into the `oneof`.
- **Only field numbers and field types are forever.** Names can change, because the binary format does not hold them.
- **Use `edition = "2024"`, not `proto3`.** Edition 2024 gives every field explicit presence and selects the Go API that the Go team recommends. It writes the same bytes, so the choice can be undone. Edition 2026 is not yet supported by a released Go generator.
- **Three must-fix points in the draft:** a `go_package` option, `bytes` for the SQL text, and a reader that refuses unknown fields wherever replay depends on them.
- **The streaming plan works.** A record on the wire is the byte `0x0a`, a length, and the record. Go writes and reads it one record at a time with `protowire` and `encoding/protodelim`. The result is byte for byte the same as marshalling the whole `Commit`.

Section 13 lists the changes to the draft, and section 14 has the revised `.proto` file.

Method: the protobuf.dev pages were read from their source repository, `protocolbuffers/protocolbuffers.github.io` at commit `51aa6af0521b` (2026-11-10). They are cited by their protobuf.dev URL. The Go library was read at tags `v1.36.12` (2026-08-10, the latest release) and `v1.37.0-rc.1` (2026-10-20, a release candidate). Buf v1.75.0 (2026-10-22) and `protoc-gen-go` v1.36.12 were built into the scratchpad with `go install` and `GOBIN`, so nothing was installed system-wide. Small Go programs in the scratchpad tested each behaviour marked "tested". `protoc` was not installed; `buf` compiles `.proto` files itself. Versions of `protoc` were read from its source at each release tag.

## 0. Terms

These terms are used in the rest of the note.

- **Message:** a record type in the schema, such as `Header`. **Field:** one named value in a message. Each field has a **field number**, such as the `1` in `uint32 version = 1`.
- **Wire format:** the bytes that Protocol Buffers writes. Each field on the wire is a **tag** (the field number and a **wire type**) followed by the value. The wire type tells a reader how long the value is, so a reader can skip a field it does not know. A **varint** is an integer written in 1 to 10 bytes, 7 bits per byte. A **length-delimited** value (wire type 2, `LEN`) is a varint length and then that many bytes; strings, bytes, nested messages, and repeated messages use it.
- **Schema:** the `.proto` file. **Generated code:** the Go types that `protoc-gen-go` writes from the schema. **Descriptor:** the compiled form of the schema that the library holds at run time.
- **Unknown field:** a field on the wire whose number the reader's schema does not have. Usually it was written by a newer version.
- **`oneof`:** a group of fields of which at most one is set. Each field in it is a **member**. It is the Protocol Buffers form of a tagged union.
- **`enum`:** a named list of integer constants.
- **Field presence:** whether the reader can tell "the field was not set" from "the field was set to its zero value". **Implicit presence** cannot tell; **explicit presence** can.
- **`reserved`:** a statement that forbids a field number or name in the schema, so that a deleted field's number is never used again.
- **`proto3`** and **editions:** two dialects of the schema language. `syntax = "proto3"` is the older one. `edition = "2024"` is its successor. Both write the same wire format.
- **Well-known types:** messages that ship with Protocol Buffers, such as `google.protobuf.Timestamp`.
- **Package:** the namespace of a schema, such as `s4ql.v1`. It is not stored in the binary data.

## 1. Schema evolution rules

Source: "Updating A Message Type" in https://protobuf.dev/programming-guides/proto3/#updating, which the editions guide repeats in the same words (https://protobuf.dev/programming-guides/editions/#updating). [V]

The guide sorts changes into three groups.

**Wire-unsafe** ("will break if you use parse data that was serialized using the old schema with a parser that is using the new schema"):

- "Changing field numbers for any existing field is not safe." Changing a number "is equivalent to deleting the field and adding a new field with the same type."
- "Moving fields into an existing `oneof` is not safe."

**Wire-safe** ("fully safe to evolve the schema in this way without risk of data loss or new parse failures"):

- "Adding new fields is safe." Old code "simply ignore[s] the new field when parsing."
- "Removing fields is safe." But "The same field number must not used again", so reserve it or rename the field with an `OBSOLETE_` prefix.
- "Adding additional values to an enum is safe."
- "Changing a single explicit presence field or extension into a member of a **new** `oneof` is safe."
- "Changing a `oneof` which contains only one field to an explicit presence field is safe."

**Wire-compatible, conditionally safe** (old data parses, but the parse "may be lossy"):

- "`int32`, `uint32`, `int64`, `uint64`, and `bool` are all compatible." A value that does not fit is truncated, as in a C++ cast.
- "`sint32` and `sint64` are compatible with each other but are *not* compatible with the other integer types."
- "`string` and `bytes` are compatible as long as the bytes are valid UTF-8."
- "Embedded messages are compatible with `bytes`."
- "For `string`, `bytes`, and message fields, singular is compatible with `repeated`." Not for numbers, because repeated numbers are packed.
- `enum` is compatible with the 32-bit and 64-bit integer types.

The guide warns: "Note that any wire-safe changes may be a breaking change to application code", for example a `switch` on an enum.

**Field numbers** [V, https://protobuf.dev/programming-guides/proto3/#assigning]: a number "**cannot be changed once your message type is in use**" and "**should never be reused**". "Never take a field number out of the reserved list for reuse." Reuse can lead to "Data corruption". Numbers 1 to 15 take one byte on the wire, 16 to 2047 take two, and 19000 to 19999 are reserved for the implementation.

**`reserved`** [V, https://protobuf.dev/programming-guides/proto3/#deleting]: when a field is deleted, "you **must** reserve the deleted field number". Reserving the name too is advised for JSON and text format. Example: `reserved 2, 15, 9 to 11;` and, in proto3, `reserved "foo", "bar";`. In editions, names are written without quotes: `reserved foo, bar;` (https://protobuf.dev/best-practices/dos-donts/#reserve-tag-numbers). Enum values are reserved the same way, and `40 to max` reserves to the end of the range.

**Renaming** [V]: "Reusing an old field name later is generally safe, except when using TextProto or JSON encodings where the field name is serialized" (https://protobuf.dev/programming-guides/proto3/#reserved-field-names). The binary format holds only numbers. Tested with `buf breaking`: a rename passes the `WIRE` rules and fails the `FILE` rules (section 10).

**Changing a type** [V, https://protobuf.dev/best-practices/dos-donts/#change-type]: "Almost never change the type of a field; it'll mess up deserialization, same as re-using a tag number."

**Presence changes** [V, https://protobuf.dev/programming-guides/field_presence/#change-compatibility]: "Changing a field between *explicit presence* and *implicit presence* is a binary-compatible change", but what a writer emits changes, so "This change may or may not be safe, depending on the application's semantics."

For s4ql [I]: we never rewrite a commit object, and every writer is a newer or older s4ql. So the question is always the same: can every later reader read every earlier file, and does an earlier reader refuse, rather than misread, a later file? The rules above answer the first part. Sections 2 and 11 answer the second.

## 2. `oneof`

### What an old reader sees

The documentation [V, https://protobuf.dev/programming-guides/proto3/#backward]:

> Be careful when adding or removing oneof fields. If checking the value of a oneof returns `None`/`NOT_SET`, it could mean that the oneof has not been set or it has been set to a field in a different version of the oneof. There is no way to tell the difference, since there's no way to know if an unknown field on the wire is a member of the oneof.

On the wire, a `oneof` is nothing special: "`Oneof` fields are encoded the same as if the fields were not in a `oneof`" (https://protobuf.dev/programming-guides/encoding/#oneofs) [V]. So a new member is a new field, and "Adding new fields is safe" applies. The guide does not list "add a member to an existing `oneof`" by name.

Tested with Go v1.36.12 [V, tested]. A newer schema added `Table drop_table = 7` to `Record`'s `oneof`. A record with that member is the bytes `3a 03 0a 01 74`. The older generated code read it as follows:

- `GetKind()` returned `nil` (open struct API). With the opaque API, `WhichKind()` returned `Record_Kind_not_set_case` and `HasKind()` returned false.
- `ProtoReflect().GetUnknown()` returned the 5 bytes. `protowire.ConsumeTag` on them gave field number 7, wire type 2.
- An empty record also gave `nil`, but with no unknown bytes.
- With `proto.UnmarshalOptions{DiscardUnknown: true}`, the unknown bytes were gone, so the two cases looked the same.

### Telling "no member" from "unknown member" in Go

The documentation is right in general: the library cannot know which `oneof` an unknown field belonged to. It does not matter for us if a message holds nothing but its `oneof`. Then any unknown field in it is an unknown member. The API [V, `reflect/protoreflect/value.go` at v1.36.12]:

- `m.ProtoReflect().GetUnknown()` returns `protoreflect.RawFields`, the raw bytes of all unknown fields. "GetUnknown retrieves the entire list of unknown fields."
- `protowire.ConsumeTag(b)` returns the field number, the wire type, and the length of the tag. `protowire.ConsumeFieldValue(num, typ, b)` returns the length of the value, so a loop can list every unknown field.
- `m.ProtoReflect().WhichOneof(od)` "reports which field within the oneof is populated, returning nil if none are populated."

The rule for replay [I]: when a `Record` has no known kind, refuse the commit object. If `GetUnknown()` is not empty, say that a newer version of s4ql is needed. If it is empty, say that the record is malformed. Do the same for `Value`. For `Evidence`, skip. Never set `DiscardUnknown`. Write a comment in the schema that `Record` and `Value` never get a field outside their `oneof`, because a later field outside it would break this rule.

### Two members on the wire

The documentation [V, https://protobuf.dev/programming-guides/proto3/#oneof-features]: "If the parser encounters multiple members of the same oneof on the wire, only the last member seen is used in the parsed message." A different member clears the one set before. The same member again replaces a number, or merges a message.

Tested [V, tested]: a `Record` with a header and then a put parses as a put; a put and then a header parses as a header; two headers merge (the first's `version` and the second's `parent` both survive). The generated code gives no sign that this happened. Two whole commit objects joined end to end also parse as one `Commit` with all their records, because "parsing the concatenation of two encoded messages produces exactly the same result as if you had parsed the two messages separately and merged" (https://protobuf.dev/programming-guides/encoding/#last-one-wins) [V, tested: two 3-record objects gave 6 records].

For s4ql [I]: the checksum check excludes damage after writing, and our writer cannot produce two members from generated code. The reader must still check the order that the format promises: exactly one header, and first. That check also catches a joined file. A raw check that each record holds exactly one field is cheap with `protowire`, but it only guards against a broken writer.

### Is a `oneof` the right switch for kinds?

The guide presents `oneof` for the case "where at most one field will be set at the same time" (https://protobuf.dev/programming-guides/proto3/#oneof) [V]. It is the standard tagged union of Protocol Buffers [I]. The alternatives:

- **A kind enum plus optional fields.** The reader must check that the enum and the set field agree, which is a second source of truth. An unknown enum value is visible in Go (section 5), but nothing stops two fields being set. Worse than a `oneof` for us [I].
- **Separate repeated fields on `Commit`,** such as `repeated Row puts = 4; repeated Row deletes = 5;`. The wire keeps the order of all records, but "only the order of records for the same field with respect to each other is preserved" in the parsed result (https://protobuf.dev/programming-guides/encoding/#repeated) [V]. Generated code gives one list per field, so the order of headings, puts, and deletes across kinds would be lost. A streaming reader of our own would see the order, but standard tools would not. Not suitable, because the changes are applied in order [I].
- **Several message fields with "at most one set" by convention.** etcd does this: `InternalRaftRequest` is "the union of all requests which can be sent via raft", with fields `put = 4`, `delete_range = 5`, and so on, and no `oneof` (`api/etcdserverpb/raft_internal.proto` on `main`) [V]. The generated code does not enforce "one", and two set fields would both be kept. A `oneof` is the stricter form of the same idea [I].
- **`google.protobuf.Any`:** a message that holds the bytes of any message and a type URL string. The best-practice page says "**Do** prefer extensions over `Any` where possible", because `Any` "has a number of design flaws", and its main use is "horizontal infrastructure" that must "propagate completely arbitrary messages" (https://protobuf.dev/best-practices/dos-donts/#avoid-any) [V]. It would also store a type URL in every record, and the type URL contains the package name. Not suitable [I].

## 3. `proto3` or editions

The state on 2026-11-11 [V]:

- "Protobuf Editions replace the proto2 and proto3 designations", and "**The latest released edition is 2024.**" (https://protobuf.dev/editions/overview/).
- "Editions won't break existing binaries and don't change a message's binary, text, or JSON serialization format." (same page). So the choice of dialect does not touch stored bytes.
- `protoc` supports edition 2024 from v32.0 (2025-08-14). Its release notes say "Enable edition 2024 support in protoc and language generators for cpp, ..., python". `ProtocMaximumEdition()` is `EDITION_2024` in v32.0 and v33.0 and `EDITION_2026` in v36.2 and v37.0-rc2 (`src/google/protobuf/compiler/code_generator.h` at each tag).
- `protoc-gen-go` supports up to edition 2024 from v1.36.9. The commit "internal/editionssupport: declare support for edition 2024" is dated 2025-08-26. In v1.36.12, `internal/editionssupport/editions.go` has `Maximum = descriptorpb.Edition_EDITION_2024`. In v1.37.0-rc.1, `Maximum` is `EDITION_2026`.
- Tested: Buf v1.75.0 compiles an `edition = "2026"` file, and `protoc-gen-go` v1.36.12 refuses it with "use of edition EDITION_2026 not yet supported by the Go Protobuf runtime". Edition 2024 compiled, generated, and passed lint.

The official recommendation [V]:

- proto3 guide: "`optional`: (recommended)" and "*implicit*: (not recommended)", and `optional` "is recommended over *implicit* fields for maximum compatibility with Protobuf editions" (https://protobuf.dev/programming-guides/proto3/#field-labels).
- "Implicit presence ... is a design decision the Protobuf team regrets. We do not recommend using implicit presence for new fields." and "Explicit presence is the silent default in Protobuf Editions going forward." (https://protobuf.dev/design-decisions/implicit-presence/).
- For Go: "We recommend you select the Opaque API for new development. Protobuf Edition 2024 ... made the Opaque API the default." (https://protobuf.dev/reference/go/opaque-faq/#which).

What differs for us between `proto3` and edition 2024 [V, https://protobuf.dev/editions/features/]:

| Feature | proto3 | edition 2024 | edition 2026 |
|---|---|---|---|
| `field_presence` | `IMPLICIT`, or explicit with `optional` | `EXPLICIT`; the `optional` keyword is gone | `EXPLICIT` |
| `enum_type` | `OPEN` | `OPEN` | `OPEN` |
| `utf8_validation` | `VERIFY` | `VERIFY`, can be set to `NONE` per field | `VERIFY` |
| Go `api_level` | open struct (opaque only by a `protoc` flag) | `API_OPAQUE` | `API_OPAQUE` |
| `default_symbol_visibility` | `EXPORT_ALL` | `EXPORT_TOP_LEVEL` | `STRICT` |
| `enforce_naming_style` | none | `STYLE2024` | `STYLE2026` |
| `enforce_proto_limits` | none | none | 1500 fields per message, 1700 values per enum |

- **UTF-8.** Edition 2024 can set `features.utf8_validation = NONE` on a `string` field. Go honours it: `internal/strs/strings.go` returns the field's own setting for editions files and "is proto3" otherwise [V]. The feature page warns that with `NONE`, "Parsers may handle this type of field in an unpredictable way, such as replacing invalid characters." So `bytes` remains the safe type for SQLite text, also in editions [I].
- **Migration later.** A tool named Prototiller converts files between editions (https://protobuf.dev/editions/overview/#lifecycles) [V]. Editions are "planned to be released roughly once a year", and a removed feature comes with "a major version bump" and generous timelines [V, same page].

Recommendation [I]: `edition = "2024"`. It makes the agreed rule "a field that can be absent is declared so" the default, and it gives the recommended Go API without command-line flags. Move to 2026 when a released `protoc-gen-go` supports it. The bytes do not change, so this is not a forever choice. If any tool we want to use cannot read editions, `proto3` with `optional` on every scalar field is the equivalent, and it can be converted later.

## 4. Field presence

Source: https://protobuf.dev/programming-guides/field_presence/ [V].

- With implicit presence, a field "set to the default (zero) value ... will not be serialized to the wire. In fact, you cannot determine whether the default (zero) value was set or parsed from the wire or not provided at all."
- "Message fields and `oneof` members are always explicit presence, and repeated fields and maps are always implicit presence." (https://protobuf.dev/design-decisions/implicit-presence/#what).
- "If you set a oneof field to the default value (such as setting an int32 oneof field to 0), the 'case' of that oneof field will be set, and the value will be serialized on the wire." (https://protobuf.dev/programming-guides/proto3/#oneof-features).
- In Go's open struct API, `optional` scalars become pointers (`m.Foo != nil`). In the opaque API, they get `Has`, `Set`, and `Clear` methods.

What the draft's fields do, tested in `proto3` with Go v1.36.12 [V, tested]:

| Field | Value set | Bytes written | Read back |
|---|---|---|---|
| `Column` with `name ""`, `type 0`, `primary_key 0` | all zero | none (0 bytes) | all zero, no way to see they were set |
| `Value.integer` in the `oneof` | 0 | `08 00` | member `integer`, value 0 |
| `Value.real` in the `oneof` | 0.0 | `11` and 8 zero bytes | member `real` |
| `Value.text` in the `oneof` | `nil` or `[]byte{}` | `1a 00` | member `text`, empty |
| `Value.blob` in the `oneof` | `nil` | `22 00` | member `blob`, empty |
| `Value.null` in the `oneof` | `&Null{}` or `nil` | `2a 00` | member `null` |

So inside the `oneof`, NULL, empty text, empty blob, integer 0, and real 0.0 are all distinct and all present. The draft's `Value` is correct as it is. Edge case [V]: "If a float or double value is set to +0 it will not be serialized, but -0 is considered distinct and will be serialized" (proto3 guide, default values). Inside a `oneof`, +0 was written (tested), and so is -0 [I].

Outside a `oneof`, in `proto3` [I from the table]:

- `uint32 primary_key = 0` is not written, and reads as 0. That is the right meaning, "not part of the key", so implicit presence is harmless here.
- `string name = ""` is not written, and reads as "". An empty name is invalid in any case, so the reader must refuse it either way.
- `uint32 version = 0` and an empty `parent` read the same as missing. The reader refuses both, because 0 is not a known version and `parent` must be 32 bytes. The 32 zero bytes of the first commit are not the default (the default is empty bytes), so they are always written.
- `int64 time = 0` would read as 1970. Missing time and time zero would look the same.

With edition 2024, every one of these fields gets explicit presence [V, tested in the revised schema]: `HasParent()` was false for a header without a parent, and a `primary_key` set to 0 was written as `18 00`. The cost is 2 bytes for each key-less column in each heading [I].

## 5. Enums

Sources: https://protobuf.dev/programming-guides/proto3/#enum, https://protobuf.dev/programming-guides/enum/, https://protobuf.dev/programming-guides/style/#enums [V].

- **The zero value.** "the first value defined in an enum definition **must** have the value zero and should have the name `ENUM_TYPE_NAME_UNSPECIFIED` or `ENUM_TYPE_NAME_UNKNOWN`", and "It is also recommended that this first, default value have no semantic meaning other than 'this value was unspecified'." Buf's default lint wants `_UNSPECIFIED` (section 10).
- **Open and closed enums.** "**Open** enums will parse the value `2` and store it directly in the field." "**Closed** enums will parse the value `2` and store it in the message's unknown field set" and report the field as unset. proto3 and editions enums are open. "All known Go releases are out of conformance. Go treats all enums as **open**."
- **What a Go reader sees for an unknown value.** Tested [V, tested]: a newer writer set `TYPE_TEXT = 2`. The older reader got `Type(2)`, which prints as `2`, and no unknown field. So the reader must compare the value with the values it knows and refuse the rest. The library does not do it.
- **Prefixes.** Enum values are not scoped by their enum, "so the same name in two sibling enums is not allowed". The style guide strongly recommends a prefix of the enum name, and "the remainder of the name with the prefix stripped should still be a legal and style-conformant enum name". So `TYPE_INT` is right. In Go the constant is `Type_TYPE_INT`. Edition 2024 has a Go feature `strip_enum_prefix` that would give `Type_INT`. Its default is to keep the prefix.
- **Removing values.** Reserve the number and name: `reserved 2, 15, 9 to 11, 40 to max;`.
- **Avoid C macro names.** "Avoid using macro constants such as "`NULL`," "`NAN`," and "`DOMAIN`" as enum values." (https://protobuf.dev/best-practices/dos-donts/#macro-constants). This matters if a `Type` value is ever named `TYPE_NULL`; the prefix avoids the exact name [I].

## 6. Naming, packages, and files

Source: https://protobuf.dev/programming-guides/style/ unless noted [V].

- **Files:** `lower_snake_case.proto`. Order: license, overview, syntax or edition, package, imports, file options, everything else. Line length 80, indent 2.
- **Names:** messages and enums in TitleCase, fields and `oneof`s in `lower_snake_case`, enum values in `UPPER_SNAKE_CASE`. "Use pluralized names for repeated fields." Underscores only before a letter. Avoid the prefixes `has_`, `get_`, `set_`, `clear_`, the suffix `_value`, the name `descriptor`, and language keywords.
- **Keywords:** "If the name of a message, field, enum, or enum value is a keyword in the language that reads from/writes to that field, then Protobuf may change the field name" (https://protobuf.dev/best-practices/dos-donts/#avoid-keywords). The draft's field `delete` is a keyword in C++ and JavaScript [I]. Go is not affected: the generated names are `GetDelete` and `Record_Delete`.
- **Packages:** "a dot-delimited sequence of lower_snake_case names", "a short but unique name based on the project name", and "should not be coupled with the directory path". The style guide itself does not require a version suffix. Buf's default lint does (`PACKAGE_VERSION_SUFFIX`): "When you do need an incompatible revision, the standard pattern is to publish a new versioned package alongside the old one" (https://buf.build/docs/lint/rules/). Buf also wants the directory to match the package (`PACKAGE_DIRECTORY_MATCH`), so `s4ql.v1` goes in `s4ql/v1/`.
- **The package name is not in the stored bytes.** A tag holds only the field number and wire type (https://protobuf.dev/programming-guides/encoding/#structure) [V]. The Go FAQ warns that changing it "is not backwards compatible for types used as extension fields, stored in `google.protobuf.Any`, or for gRPC Service definitions" (https://protobuf.dev/reference/go/faq/#fix-namespace-conflict) [V]. We use none of them, so the package can be renamed later [I].
- **Generic names.** All declarations linked into a Go binary go into one global registry by full name, and two equal names "will either panic at init-time or silently drop the conflict". The FAQ recommends "a package name that is deliberately chosen to be universally unique" (https://protobuf.dev/reference/go/faq/#namespace-conflict) [V]. Our full names are `s4ql.v1.Value`, `s4ql.v1.Type`, and so on, which do not clash with `google.protobuf.Value` or `google.protobuf.NullValue`. The short names are only a problem inside one package [I]. A program that embeds s4ql and also links another schema with package `s4ql.v1` would clash, which is unlikely.
- **`go_package`:** "For open source projects, you **must** provide either a `go_package` option or set the Bazel `-M` flag" (https://protobuf.dev/programming-guides/proto3/#packages) [V]. The Go guide recommends the option in the file, with "the full import path of the Go package". It discourages the `path;name` form. "There is no correlation between the Go import path and the `package` specifier in the `.proto` file." (https://protobuf.dev/reference/go/go-generated-opaque/#package) [V].
- **Where the files go:** "Prefer not to put `.proto` files in the same directory as other language sources. Consider creating a subpackage `proto`" (https://protobuf.dev/programming-guides/proto3/#location) [V].
- **One file or many.** The "1-1-1" practice is one top-level entry point per file. But it is "not ideal (extremely conceptually coupled messages which have readability benefits by being co-located)", and its costs do not apply "when a `.proto` file has no imports" (https://protobuf.dev/best-practices/1-1-1/) [V]. One file for the commit object fits the exception [I].
- **Storage and API messages.** "**Do** Use Different Messages For RPC APIs and Storage", because "the needs of long-term storage and live RPC services tend to later diverge" (https://protobuf.dev/best-practices/dos-donts/#separate-types-for-storage) [V]. If s4ql ever gets a network protocol, it should not reuse these messages [I].

## 7. Well-known types

**Commit time** [V]:

- "**Do** Use Well-Known Types and Common Types ... do not use `int32 timestamp_seconds_since_epoch` or `int64 timeout_millis` in your code when a perfectly suitable common type already exists!" (https://protobuf.dev/best-practices/dos-donts/#well-known-common).
- `google.protobuf.Timestamp` is `int64 seconds = 1; int32 nanos = 2;`. Seconds "Must be between -62135596800 and 253402300799 inclusive (which corresponds to 0001-01-01T00:00:00Z to 9999-12-31T23:59:59Z)", nanos between 0 and 999,999,999. Leap seconds are smeared (`src/google/protobuf/timestamp.proto` at v36.2).
- Go has `timestamppb.New(time.Time)`, `AsTime()`, and `CheckValid()` (`types/known/timestamppb`). It is part of `google.golang.org/protobuf`, so no new module.
- An `int64` of nanoseconds since 1970 covers the years 1677 to 2262 [V, tested with `time.Unix`].
- Tested: a header with a version and a `Timestamp`, and no parent, was 12 bytes. `buf convert` printed the time as `"2026-11-11T12:00:00Z"` in JSON, and as `seconds: 1794398400` in text format [V, tested].

Recommendation [I]: use `google.protobuf.Timestamp`. It follows the official advice, has a documented meaning, and dump tools print it as a date. The only cost is an import and a few bytes.

**NULL** [V, `src/google/protobuf/struct.proto` at v36.2]: `NullValue` is "a sentinel, using an enum with only one value to represent the null value for the `Value` type union", which is the JSON value model. "A field of type `NullValue` with any value other than `0` is considered invalid." It would work as a `oneof` member, since a `oneof` member set to 0 is written. But it is an enum, so an old reader would accept any integer there, and it brings in `struct.proto`, the JSON model. Recommendation [I]: keep the empty message `Null`. It is written as `2a 00` (section 4), it cannot hold a wrong value, and its meaning is ours.

## 8. Large data and streaming

**Official advice** [V]:

- "Protos must be smaller than 2 GiB when serialized. Many proto implementations will refuse to serialize or parse messages that exceed this limit." (https://protobuf.dev/programming-guides/encoding/#size-limit). The limits page adds: "It's recommended to bound request and response sizes." (https://protobuf.dev/programming-guides/proto-limits/#total).
- "Protobuf is not designed to handle large messages. As a general rule of thumb, if you are dealing in messages larger than a megabyte each, it may be time to consider an alternate strategy." But "Protobuf is great for handling individual messages *within* a large data set." (https://protobuf.dev/programming-guides/techniques/#large-data).
- The documented pattern for many messages: "write the size of each message before you write the message itself. When you read the messages back in, you read the size, then read the bytes into a separate buffer, then parse from that buffer." (https://protobuf.dev/programming-guides/techniques/#streaming).
- Repeated message fields are "not packable", so "an entry for each individual value is encoded" (https://protobuf.dev/programming-guides/encoding/#repeated).

**Our plan on the wire.** `repeated Record records = 1` writes each record as the tag `0x0a` (field 1, wire type 2), a varint length, and the record's bytes. That is the documented pattern with one extra byte in front of each record. The file is then also a valid `Commit` message. Tested [V, tested]: three records written this way gave exactly the bytes of `proto.Marshal(&Commit{Records: recs})`, and the reader below read them back one at a time.

**Writing in Go** [V, `encoding/protodelim/protodelim.go` and `encoding/protowire` at v1.36.12, tested]:

```go
tag := protowire.AppendTag(nil, 1, protowire.BytesType) // the byte 0x0a
for each record r {
	w.Write(tag)
	protodelim.MarshalTo(w, r) // writes the varint length, then proto.Marshal(r)
}
```

`protodelim.MarshalOptions` embeds `proto.MarshalOptions`, so `Deterministic` can be set. `w` can be an `io.MultiWriter` of the file and a SHA-256 hash [I].

**Reading in Go** [V, same files, tested]:

```go
br := bufio.NewReader(r)
opts := protodelim.UnmarshalOptions{MaxSize: limit}
for {
	t, err := binary.ReadUvarint(br) // the tag; io.EOF here means the end
	num, typ := protowire.DecodeTag(t)
	// refuse anything but field 1, wire type 2
	var rec s4qlpb.Record
	err = opts.UnmarshalFrom(br, &rec)
}
```

- `protodelim.UnmarshalFrom` reads a varint size and then the message. "A zero MaxSize will default to 4 MiB. Setting MaxSize to -1 disables the limit." A larger message gives a `*protodelim.SizeTooLargeError`. The 4 MiB default "corresponds to the default gRPC max request/response size".
- The 4 MiB default is too small for us: SQLite's build allows values up to `SQLITE_MAX_LENGTH`, 1,000,000,000 bytes (`internal/sqlite/c/sqlite3.c` line 14812) [V]. `MaxSize` must be set to at least the largest record s4ql can write [I].
- With a `*bufio.Reader`, `UnmarshalFrom` parses from the reader's buffer with `Peek` when the record fits, and otherwise allocates the record's size. Parsing copies `bytes` fields (`*p.Bytes() = append(emptyBuf[:], v...)` in `internal/impl/codec_gen.go`), so the records do not alias the buffer [V].
- An alternative without `protodelim`: read the length with `binary.ReadUvarint`, read that many bytes, and call `proto.UnmarshalOptions.Unmarshal`. This also allows the raw check of section 2 on the record's bytes [I].

**Size limits in Go** [V, source search]: a search of `proto/` and the decoder in `internal/impl` found no 2 GiB check when parsing; the encoder only stops caching sizes above `math.MaxInt32`. One `Record` stays far below 2 GiB, because a SQLite value is at most 1 GB [I]. A whole commit object can be larger than 2 GiB. Our streaming reader does not care, but `proto.Unmarshal` of the whole file and other tools would then need the whole file in memory, and C++ tools such as `protoc --decode` refuse it [I]. So the promise "a standard tool can print one" holds only below 2 GiB.

## 9. The Go API, generated code, and pinning

**Which API** [V]:

- Three APIs: "`API_OPEN`: The Open Struct API generates struct types that are open to direct access." "`API_HYBRID`: ... also includes accessor methods ... but still exports the struct fields." "`API_OPAQUE`: ... the struct fields are hidden and can no longer be directly accessed." (https://protobuf.dev/editions/features/#go-api_level).
- "We recommend you select the Opaque API for new development." and "the (existing) Open Struct API will not be removed." (https://go.dev/blog/protobuf-opaque, 2024-12-16). The hybrid API is advised for "packages that publish generated code (`.pb.go` files)", so their users can move at their own pace (same post). Our generated code would be internal, so this does not apply [I].
- The opaque API works with any dialect, but to select it inside a `proto3` file "you must migrate said file to editions first"; otherwise use `--go_opt=default_api_level=API_OPAQUE` (https://protobuf.dev/reference/go/opaque-faq/).

**`oneof` in each API** [V, https://protobuf.dev/reference/go/go-generated/#oneof and https://protobuf.dev/reference/go/go-generated-opaque/#oneof, and the generated code in the test]:

- Open struct and hybrid: a field `Kind isRecord_Kind` that holds one of the wrapper types `*Record_Header`, `*Record_Put`, and so on. A type switch with `case nil:` for "not set" and a `default:` for anything else.
- Opaque: `WhichKind()` returns a constant such as `Record_Put_case` or `Record_Kind_not_set_case`, with `HasKind`, `ClearKind`, `GetPut`, `SetPut`, `HasPut`. Messages are built with setters or with `Record_builder{...}.Build()`. The documented `switch` has a `default:` that returns an error "has an unexpected new oneof field".

**Commit the generated code** [V]: "Go generate is never run automatically by go build, go test, and so on." (`go help generate`). A user of `maragu.dev/s4ql` gets only what is in the module, so the `.pb.go` file must be committed [I]. The generated header records the generator version: `// protoc-gen-go v1.36.12` and `// protoc (unknown)` when Buf compiles [V, tested].

**Pinning the generator** [V, tested in the scratchpad]:

- `go get -tool google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12` adds a `tool` line to `go.mod`, and nothing else, because the module already needs `google.golang.org/protobuf`. It then runs as `go tool protoc-gen-go`.
- `go get -tool github.com/bufbuild/buf/cmd/buf@v1.75.0` added 91 `require` lines to a test `go.mod`. To keep s4ql's `go.mod` small, run Buf with `go run github.com/bufbuild/buf/cmd/buf@v1.75.0`, or from a separate tools module [I].
- Buf compiles the schema itself, so `protoc` is not needed. A `buf.gen.yaml` with `local: ["go", "tool", "protoc-gen-go"]` ran the pinned plugin, and its output was identical to that of the binary.
- Buf can also use "remote plugins" from its registry [I]. A local plugin keeps the build off the network.

**Checking in CI that the committed code matches** [I]: run the generator, then `git diff --exit-code`. Because the header holds the plugin version, a run with another version also shows as a difference.

## 10. Checking evolution mechanically

**`buf breaking`** compares the schema with an earlier one, for example `buf breaking --against '.git#branch=main'` [V, `buf breaking --help`]. Its categories, from https://buf.build/docs/breaking/rules/ [V]:

- `FILE`, the default: "Detects changes that move generated code between files, breaking generated source code on a per-file basis."
- `PACKAGE`: generated code per package.
- `WIRE_JSON`: "Detects changes that break wire (binary) or JSON encoding."
- `WIRE`: "Detects changes that break wire (binary) encoding." The page names the case "You're your own client. For example, you're trying to detect issues reading Protobuf encoded messages from older versions of your program that were persisted to disk or other non-volatile storage." It adds: "Use the less strict `WIRE` when you can guarantee that only binary encoded messages are decoded."
- "Pick one and stick with it."

The `WIRE` category holds 15 rules in v1.75.0 [V, `buf config ls-breaking-rules --format json`], among them `FIELD_NO_DELETE_UNLESS_NUMBER_RESERVED`, `ENUM_VALUE_NO_DELETE_UNLESS_NUMBER_RESERVED`, `RESERVED_MESSAGE_NO_DELETE`, `RESERVED_ENUM_NO_DELETE`, `FIELD_WIRE_COMPATIBLE_TYPE`, `FIELD_WIRE_COMPATIBLE_CARDINALITY`, and `FIELD_SAME_ONEOF`.

Tested with v1.75.0 [V, tested]:

| Change | `WIRE` | `WIRE_JSON` | `FILE` |
|---|---|---|---|
| Add a member to an existing `oneof` | passes | passes | passes |
| Move a field into an existing `oneof` | fails | fails | fails |
| Move a field into a new `oneof` | fails | not run | fails |
| Delete a field without reserving it | fails | fails (number and name) | fails |
| Delete an enum value without reserving it | fails | not run | fails |
| `int64` to `sint64` | fails | fails | fails |
| `int32` to `int64` | passes | fails | fails |
| `string` to `bytes` | passes | not run | fails |
| `bytes` to a message | fails | not run | fails |
| Change a message field's type to another message | fails | not run | fails |
| Rename a field | passes | fails | fails |
| Add a field to `Column`, an enum value, and a `oneof` member | passes | not run | not run |

Notes [I]:

- Buf is stricter than the documentation in two cases: it refuses `bytes` to a message, and moving a field into a new `oneof`, which the guide calls compatible or safe for an explicit-presence field.
- `WIRE` passes `int32` to `int64` because it is wire-compatible, though lossy for old readers. For a format with old readers that must refuse rather than misread, that is a case to catch in review or in tests.
- No checker knows our meaning: that a new `oneof` member must make old readers refuse, or that a new field in `Column` changes replay. Tests with committed sample files written by each format version are the second line: every later reader must read them all.

Recommendation [I]: `WIRE` for the commit-object schema. We never store JSON, and `WIRE` leaves names free to improve. Choose `WIRE_JSON` instead if a JSON dump of a commit object is ever meant to be read back.

**`buf lint` on the draft** [V, tested]. With the default `STANDARD` rules, Buf v1.75.0 printed:

```
s4ql.proto:3:1:Files with package "s4ql" must be within a directory "s4ql" relative to root but were in directory ".".
s4ql.proto:3:1:Package name "s4ql" should be suffixed with a correctly formed version, such as "s4ql.v1".
s4ql.proto:50:3:Enum zero value name "TYPE_UNKNOWN" should be suffixed with "_UNSPECIFIED".
```

The optional `COMMENTS` rules added 30 more: every field, `oneof`, and message without a comment of its own. Lint does not check for a missing `go_package`; the Go generator needs it. The revised schema in section 14 passes `STANDARD` and `COMMENTS` with no output, and `buf format -d` shows no difference [V, tested].

## 11. Robustness

Options of `proto.UnmarshalOptions` at v1.36.12 [V, `proto/decode.go`]: `Merge`, `AllowPartial`, `DiscardUnknown`, `Resolver`, `RecursionLimit`, and `NoLazyDecoding`. There is no option to refuse unknown fields in the binary format.

- **`DiscardUnknown`:** "If DiscardUnknown is set, unknown fields are ignored." It would erase the only evidence that a newer writer added a kind (section 2). Never set it [V, tested].
- **Unknown fields are kept** by default: "Proto3 messages preserve unknown fields and include them during parsing and in the serialized output." They are lost when a message is converted to JSON or copied field by field (https://protobuf.dev/programming-guides/proto3/#retaining) [V]. Tested: an old reader that marshalled the record again wrote the same bytes, with the unknown field [V, tested]. We do not rewrite commit objects, so this matters only for tools.
- **Refusing unknown fields outside `Evidence`** [I, tested as a sketch]: a short function can walk a decoded record with `protoreflect.Message.Range` and refuse any message whose `GetUnknown()` is not empty, except inside `Evidence`. Tested on a heading whose `Column` held an unknown field 4: it returned "s4ql.v1.Column has unknown field 4". Without it, an old reader would replay a column with a new property, such as a collation, as if the property did not exist. The agreed alternative is to raise `Header.version` whenever such a field is added. etcd does the mechanical version of that: it annotates fields with `[(versionpb.etcd_version_field) = "3.5"]` and computes "minimal etcd version able to interpret WAL log" by visiting the fields that are set (`api/versionpb/version.proto` and `server/storage/wal/version.go` on `main`) [V].
- **Recursion:** `RecursionLimit` defaults to `protowire.DefaultRecursionLimit`, which is 10000 [V, `encoding/protowire/wire.go`]. The limits page says Go is at 10000 and "there is a plan to reduce this to 100" (https://protobuf.dev/programming-guides/proto-limits/#depth) [V]. Our schema has no recursive message and is at most six levels deep, so a limit such as 32 would refuse only crafted input, such as deeply nested unknown groups [I].
- **Size:** see section 8. `protodelim`'s `MaxSize` is the only size limit; a `[]byte` input is bounded by its own length [V].
- **UTF-8:** a `string` field with invalid UTF-8 fails both ways in `proto3` and in edition 2024 by default. `proto.Marshal` returned "string field contains invalid UTF-8", and so did `proto.Unmarshal` [V, tested]. A commit with such SQL text would fail at the last step, after the statements ran. A `bytes` field accepts it [V, tested].
- **Deterministic marshalling:** "the deterministic serialization is NOT canonical across languages. It is not guaranteed to remain stable over time." and "Users who need canonical serialization ... must define their own canonicalization specification" (`proto/encode.go`) [V]. The best-practice page says "**Never** Rely on Serialization Stability Across Builds" [V]. We hash the stored bytes and never compare a re-encoding, so this does not affect us [I].
- **Field order:** "Protobuf parsers must be able to parse fields in any order." (https://protobuf.dev/programming-guides/encoding/#order) [V]. The order of records is safe, because they are items of one repeated field (section 2).

## 12. Known mistakes

From https://protobuf.dev/best-practices/dos-donts/ [V], those that apply to stored data:

- "**Don't** Re-use a Tag Number ... If the change was live ever, there could be serialized versions of your proto in a log somewhere."
- "**Do** Reserve Tag Numbers for Deleted Fields" and "**Do** Reserve Numbers for Deleted Enum Values".
- "**Don't** Change the Type of a Field".
- "**Don't** Add a Required Field." proto3 has none; editions has `LEGACY_REQUIRED` only for migrated proto2 files.
- "**Do** Include an Unspecified Value in an Enum".
- "**Don't** Change the Default Value of a Field".
- "**Don't** Go from Repeated to Scalar": "you'll lose data".
- "**Don't** use Text Format Messages for Interchange": "If you use protos converted to JSON in your API or for storing data, you may not be able to safely rename fields or enums at all."
- "**Never** Rely on Serialization Stability Across Builds".
- "**Do** Use Different Messages For RPC APIs and Storage".
- "**Don't** Use Booleans for Something That Has Two States Now, but Might Have More Later".
- The page opens with: "Don't assume that you can make a breaking change and it'll be okay because the client and server are in sync." For us, the reader of 2036 and the writer of 2026 are never in sync [I].

From projects that keep protobuf data for years:

- **etcd** [V, on `main`]: its WAL request message keeps `reserved 2; reserved "v2";` for the removed v2 API, and annotates every field with the etcd version that introduced it, so that it can tell which version may read a log (section 11).
- **CockroachDB:** the earlier note records that it moved large file lists out of the protobuf backup manifest because of allocation cost ([change-file-encodings.md](change-file-encodings.md), section 8). I did not verify other CockroachDB pitfalls in this run.

## 13. Review of the draft schema

Each change is marked **must**, **should**, or **optional**.

1. **Must: add `option go_package`.** The Go generator needs it: "For open source projects, you **must** provide either a `go_package` option or set the Bazel `-M` flag" (section 6). The revised file uses `maragu.dev/s4ql/internal/s4qlpb`, so the generated types are not part of s4ql's public API. The exact path is an implementation detail.
2. **Must: make `Statement.sql` a `bytes` field.** A `string` with invalid UTF-8 makes `proto.Marshal` fail, which would fail a commit after its statements ran (section 11, tested). SQL text can hold such text in a literal, and the agreed rule already stores SQLite text as bytes. The alternative is to refuse such SQL before it runs.
3. **Must: decide what a reader does with unknown fields outside `Evidence`.** Section 11. Either refuse them, as tested, or rely on raising `Header.version` for every such field. Without one of the two, an old reader silently replays a new property of a table or column as absent. The revised schema documents the first. This is a design question for Markus.
4. **Must (reader, not schema): refuse a `Record` or `Value` with no known kind, and tell an unknown kind from a malformed one through `GetUnknown()`.** Never set `DiscardUnknown`. Section 2, tested. The schema says in comments that these two messages never get a field outside their `oneof`.
5. **Must (reader): refuse a `Type` value it does not know.** Go keeps an unknown enum value as a plain number and reports no error (section 5, tested).
6. **Must (reader): set `protodelim.UnmarshalOptions.MaxSize`** to at least the largest record s4ql writes. The default of 4 MiB is below SQLite's 1 GB value limit (section 8).
7. **Should: use `edition = "2024"`.** Explicit presence for every field, the recommended Go API by default, and the same bytes (section 3). `Column.primary_key` and the `Header` fields then have presence without the `optional` keyword, which editions do not have.
8. **Should: name the package `s4ql.v1` and put the file at `s4ql/v1/commit.proto`.** Buf's default lint requires both (section 10). The package is not in the stored bytes, so this is about tooling and a unique name in Go's registry (section 6).
9. **Should: rename `TYPE_UNKNOWN` to `TYPE_UNSPECIFIED`.** The documentation allows both, and Buf's default lint wants `_UNSPECIFIED` (section 5). It also avoids confusion with an unknown value from a newer writer, which Go shows as a number and not as the zero value.
10. **Should: make `Header.time` a `google.protobuf.Timestamp`.** The official advice, a range that does not end in 2262, and dates in dumps (section 7).
11. **Should: check the schema in CI** with `buf lint` (`STANDARD`) and `buf breaking --against '.git#branch=main'` (`WIRE`), and check that the committed `.pb.go` matches its schema (sections 9 and 10).
12. **Should: keep sample commit objects from each format version in the repository and read them all in tests.** No checker knows what a field means (section 10).
13. **Optional: write a comment on every element** (Buf's `COMMENTS` rules). Done in the revised file. The comments are the specification of a forever format.
14. **Optional: rename `put` and `delete` to `put_row` and `delete_row`.** `delete` is a keyword in C++ and JavaScript, which the best practices say to avoid (section 6). Go is not affected, and names can be changed later under `WIRE`. Not applied.
15. **Optional: rename the enum `Type` to `ColumnType`.** It reads better in Go (`ColumnType_COLUMN_TYPE_INT`). Not applied.
16. **Optional: set `RecursionLimit`** to a small number such as 32 (section 11). Reader code, not schema.
17. **No change: the `oneof`s, the empty `Null` message, `sint64` for integers, `double` for reals, `bytes` for text and blobs, and the field numbers.** Each kind and value member has a one-byte tag. Each member is written even when its value is zero or empty (section 4).
18. **No change: keep one file.** The messages are tightly coupled, and the file has only one import (section 6).

## 14. The revised schema

With changes 1 to 4 and 7 to 13 applied. It passes `buf lint` with `STANDARD` and `COMMENTS`, and `buf format`. It generates opaque Go code with `protoc-gen-go` v1.36.12 [V, tested].

```proto
// The format of an s4ql commit object.
//
// Commit objects are kept forever, and every version of s4ql must read
// every one ever written. Field numbers and field types never change, and
// a number that is no longer used is reserved. Names can change.
edition = "2024";

package s4ql.v1;

import "google/protobuf/timestamp.proto";

option go_package = "maragu.dev/s4ql/internal/s4qlpb";

// A commit object: everything one commit did.
//
// It is written and read as a stream, one record at a time: each record is
// the byte 0x0a, the length of the record as a varint, and the record.
message Commit {
  // The records, in the order they were written.
  repeated Record records = 1;
}

// One record of a commit object.
//
// A Record holds no field outside `kind`, now or later. A reader that finds
// no known kind refuses the commit object: if the record holds an unknown
// field, a newer version of s4ql wrote it; if it holds nothing, it is
// malformed.
message Record {
  // What the record is.
  oneof kind {
    // Exactly once, and first.
    Header header = 1;
    // A change: creates this table.
    Table create_table = 2;
    // The puts and deletes that follow belong to this table.
    Table heading = 3;
    // A change: one full row, in the heading's column order.
    Row put = 4;
    // A change: the primary-key values of the row to remove.
    Row delete = 5;
    // Not needed for replay. A reader may skip it.
    Evidence evidence = 6;
  }
}

// The first record of every commit object.
message Header {
  // The version of this format. A reader refuses a version it does not know.
  // It is raised when something is added that a reader must not ignore.
  uint32 version = 1;
  // The name of the commit object before this one: 32 bytes, all zero in
  // the first one.
  bytes parent = 2;
  // The wall-clock time of the commit, from the clock of the lease holder.
  // Evidence only.
  google.protobuf.Timestamp time = 3;
}

// A table: what "create table" creates, and what a heading names.
message Table {
  // The name of the table. s4ql refuses an identifier that is not valid UTF-8.
  string name = 1;
  // The columns, in their declared order.
  repeated Column columns = 2;
}

// One column of a table.
message Column {
  // The name of the column. s4ql refuses an identifier that is not valid UTF-8.
  string name = 1;
  // The declared type of the column.
  Type type = 2;
  // The column's position in the primary key, starting at 1,
  // or 0 if it is not part of the key.
  uint32 primary_key = 3;
}

// The declared type of a column. More are added as the roadmap grows.
// A reader refuses a value it does not know.
enum Type {
  // Never written. A reader refuses it.
  TYPE_UNSPECIFIED = 0;
  // The STRICT type INT.
  TYPE_INT = 1;
}

// A row: its values in the heading's column order.
message Row {
  // The values.
  repeated Value values = 1;
}

// One SQLite value.
//
// A Value holds no field outside `kind`, now or later. A reader refuses a
// value with no known kind, as it does a record.
message Value {
  // The storage class of the value, and the value.
  oneof kind {
    // A 64-bit signed integer.
    sint64 integer = 1;
    // A 64-bit IEEE 754 floating-point number.
    double real = 2;
    // Text, as bytes, because SQLite text need not be valid UTF-8.
    bytes text = 3;
    // A blob.
    bytes blob = 4;
    // NULL.
    Null null = 5;
  }
}

// The SQL NULL. It has no fields.
message Null {}

// Evidence of what ran. A reader skips a kind of evidence it does not know.
message Evidence {
  // What the evidence is.
  oneof kind {
    // One statement, as it ran.
    Statement statement = 1;
  }
}

// One statement, as it ran.
message Statement {
  // The SQL text, as bytes, because it can hold text that is not valid UTF-8.
  bytes sql = 1;
  // The parameters, by position, starting with parameter 1.
  repeated Value parameters = 2;
  // Non-deterministic inputs, such as the time, become fields here
  // when a slice of the roadmap first needs them.
}
```

Notes on the revised file [I]:

- The comments on `Table.name` and `Column.name` state a rule that the engine must enforce: a `string` field refuses invalid UTF-8 (change 2). If s4ql should accept any identifier SQLite accepts, these become `bytes` too.
- The comment on `Header.version` keeps the agreed rule. If change 3 is decided as "refuse unknown fields outside `Evidence`", the version is still needed for changes of meaning that add no field.

## Not verified

- Which tools other than Buf and `protoc-gen-go` read edition 2024 files today, for example the generators for other languages that a third-party reader might use.
- That `protoc --decode` refuses a commit object larger than 2 GiB. This is from the documentation's "Many proto implementations will refuse", not tested.
- CockroachDB's documented protobuf pitfalls, beyond the item in the earlier note.
- Whether `delete` causes any problem in generators other than C++ and JavaScript. From memory.
