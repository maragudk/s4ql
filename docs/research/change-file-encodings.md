# Binary encodings for the change file

Date: 2026-11-11. Tags: **V** = verified in this run (tested, or read in a specification, the official documentation, source code, a release page, or an advisory database, with the reference). **I** = inferred, or from memory.

Question: which binary encoding should the change file use? The file is immutable, kept forever, and named by the SHA-256 checksum of its stored bytes. It must gain fields without breaking older or newer readers. It holds one commit, which can be one row or millions of rows.

Short answer: write the Protocol Buffers wire format by hand, as a stream of length-delimited records, with no generated code. Use `protowire` or about two hundred lines of our own code on top of `encoding/binary`. CBOR is a close second, and is first if a dump that shows every value's type without a schema matters more than a small and fixed wire surface. Every other candidate fails on at least one criterion that matters here. Section 10 has the ranking and what would change it, and section 11 has a sketch of the layout.

Method: specifications and documentation read on the web. Library facts from the GitHub API (`gh api`), the raw `go.mod` files, and the OSV advisory database (`api.osv.dev`). Transitive dependencies measured with `go mod tidy` and `go list -deps` in throwaway modules in the scratchpad, with Go 1.27.3. Value round-trips tested with small Go programs for `protowire` v1.36.12 and `fxamacker/cbor` v2.10.0, and with the `sqlite3` 3.54.1 command-line shell. No benchmarks were run. A helper agent checked the formats of the comparable systems in section 8 in this run, in primary sources.

## 1. What the format must carry

From `docs/spec.md` and the "Storage formats" item in `docs/todo.md`:

- A format version, the name of the previous change file, the row changes (full rows inserted or changed, keys of deleted rows), schema changes, and, as evidence only, the SQL statements with their non-deterministic inputs.
- Values of STRICT tables: 64-bit signed integers, 64-bit floats, text, blobs, and NULL. An `ANY` column can hold any of these per row, so the type belongs to each value, not only to the column. [I]
- New fields must not break older or newer readers.
- Change files are never rewritten. So the rule for the manifest, "keep the fields you do not know when you rewrite", does not apply. An old reader only has to skip what it does not know. [I]

What SQLite itself does with edge values, measured with the `sqlite3` 3.54.1 shell (the same version as `internal/sqlite/c/source.json`) [V]:

| Input | Stored in a STRICT column | Read back |
|---|---|---|
| `-0.0` into `REAL` | yes | `0.0`. The sign is lost on storage. In an expression, `atan2(0.0, -0.0)` gives π, so the sign exists in memory. |
| `1e999`, `-1e999` into `REAL` | yes | `Inf`, `-Inf`, type `real` |
| `9223372036854775807` into `REAL` | yes | `9.2233720368547758e+18`, type `real` |
| `x''` into `BLOB`, `''` into `TEXT` | yes | an empty blob and an empty text, distinct from NULL |
| `cast(x'ff00fe' as text)` into `TEXT` | yes | type `text`, three bytes. SQLite does not check UTF-8, and the text holds a NUL byte. |

- NaN cannot be stored. `sqlite3VdbeMemSetDouble` sets the value to NULL when it is NaN (`internal/sqlite/c/sqlite3.c` line 86279). [V]
- Consequences for the format [I]:
  - Integers need the full signed 64-bit range, and floats need their exact bit pattern, including the infinities.
  - The format does not need to carry NaN or `-0.0` for SQLite's sake, but it should not lose them if the engine ever produces them. Storing the raw IEEE 754 bits is the simplest way.
  - Text must be stored as raw bytes. A format that requires valid UTF-8 in its string type would reject a value SQLite accepts. Whether s4ql's engine will accept invalid UTF-8 is a separate decision.
  - NULL, empty text, and empty blob must be three different encodings.

## 2. Comparison table

"Deps" counts non-standard modules in a consumer's `go.sum` after `go mod tidy` with one blank import [V, measured]. "Canonical" means the specification defines one encoding per value.

| Candidate | Evolution mechanism | Exact values | Streaming | Go implementation | Spec stability | Self-describing | Hostile input | Canonical |
|---|---|---|---|---|---|---|---|---|
| Protobuf, generated code | field numbers and wire types, unknown fields skipped | yes, but proto3 `string` must be UTF-8 and implicit presence merges empty with absent | only as length-delimited messages, each under 2 GiB | `google.golang.org/protobuf` v1.36.12 (2026-08-10); 2 modules; needs `protoc` and `protoc-gen-go`; reflection-based runtime | wire format unchanged except deprecated groups; Google's documentation | partly: `protoc --decode_raw` | fuzzed; 2 advisories, both in text and JSON parsers | no, by design |
| Protobuf wire by hand (`protowire`) | same | yes, when text is `bytes` and presence is ours | same, with our own framing | `protowire` (one 571-line file); 1 module; or zero with `encoding/binary` | same | same | our own decoder: we fuzz it | we can define one |
| CBOR (RFC 8949) | maps with integer keys; unknown keys ignored | yes, but text strings must be valid UTF-8, so text needs a tag or a byte string | RFC 8742 sequences, or indefinite-length items | `fxamacker/cbor` v2.10.0 (2026-10-28); 2 modules; reflection | IETF Internet Standard (STD 94), wire-compatible with RFC 7049 (2013) | yes, fully typed | decoder limits; billions of fuzz execs per release; no advisories | yes, section 4.2 |
| DAG-CBOR, DRISL | as CBOR | **no**: forbids NaN, ±Infinity, `-0.0`; DRISL forbids floats | **no**: concatenation forbidden | `go-ipld-prime` v0.24.0; 3 advisories, 2 in 2026 | IPLD and DASL specs | yes | see advisories | yes |
| Own TLV | ours | ours | ours | ours; 0 modules | ours | only with our tool | ours | ours |
| SQLite changeset | **none**: no version, no extension point | yes: 8-byte big-endian integers and doubles, an "undefined" type distinct from NULL | via the `_strm` API | C code in SQLite; session extension not in our build | described only in source comments | partly: table name, column count, PK flags | SQLite's code | no |
| FlatBuffers | vtables; append fields or set `id` | yes | **no**: one in-memory buffer, 32-bit offsets | v25.12.19 (2026-02-06); 1 module; needs `flatc`; **no verifier in Go** | stable, Google project | no | no Go verifier | no |
| Cap'n Proto | ordinal fields, data and pointer sections | yes | segments, whole message | `capnproto.org/go/capnp/v3` v3.1.0-alpha.2 (2025-10-25), "beta software"; 3 modules | spec "not finalized" per the Go README | no | traversal and depth limits | a canonical form exists |
| Apache Avro | schema resolution, needs the writer's schema | yes | object container files with blocks | `hamba/avro` archived and unmaintained; `linkedin/goavro` v2.15.0 (2026-01-21) | Apache 1.12.x | container embeds the schema | `hamba/avro`: 4 advisories | Parsing Canonical Form for schemas only |
| MessagePack | maps with string keys, or arrays by position | yes | concatenated objects | `tinylib/msgp` v1.6.5 (2026-09-30) codegen; `vmihailenco/msgpack` last release 2023-10-26 | spec unchanged since 2021, one breaking split of "raw" in 2013 | yes | no advisories | no |
| Amazon Ion | struct fields by symbol | yes, typed nulls | value streams | `ion-go` v1.5.0 (2024-06-06), no commits since | Ion 1.0 | yes | unknown | no |
| Thrift compact | 16-bit field ids, skip by type | yes | framed transport | `apache/thrift` v0.25.0 (2026-09-30); 2 advisories in 2026 | Apache | partly | see advisories | no |
| ASN.1 DER | extension markers | Go's `encoding/asn1` has no REAL | definite lengths | standard library, no float support | ITU-T X.690 | partly | many CVEs over decades [I] | yes |
| JSON Lines | unknown keys ignored | **no**: no Infinity, NaN, or raw bytes in JSON [I] | one value per line | standard library | RFC 8259 | yes, as text | standard library | no |

The rows below the first five are ruled out in section 7.

## 3. Protocol Buffers with generated code

Sources: https://protobuf.dev/programming-guides/encoding/, https://protobuf.dev/programming-guides/proto3/, https://protobuf.dev/programming-guides/serialization-not-canonical/, https://protobuf.dev/programming-guides/techniques/, https://protobuf.dev/editions/overview/, https://github.com/protocolbuffers/protobuf-go.

**Compatibility mechanism** [V, encoding and proto3 guides]:

- Every field is a varint tag, `field_number << 3 | wire_type`. Wire types 0 (VARINT), 1 (I64), 2 (LEN) and 5 (I32) are in use; 3 and 4 (groups) are deprecated.
- An old reader skips an unknown field because the wire type alone gives its length.
- A new reader that misses a field sees it as absent, and the generated code returns the default.
- "Proto3 messages preserve unknown fields and include them during parsing and in the serialized output", but "Some actions can cause unknown fields to be lost."
- Editions "don't change a message's binary, text, or JSON serialization format."

**Traps** [V unless marked]:

- "Field numbers should never be reused." The fix is a `reserved` list.
- Changing a field's type is mostly unsafe. Only `int32`, `uint32`, `int64`, `uint64` and `bool` are mutually compatible.
- With implicit presence (proto3 without `optional`), a field set to its default "will not be serialized to the wire", and "there's no way of telling whether that field was explicitly set to the default value". An empty text and a missing text look the same. `optional` or a `oneof` fixes this.
- For a non-repeated field that appears twice, "the parser accepts the last value it sees." A corrupted or crafted file can override a value silently.
- `protobuf-go` rejects a proto3 `string` field that holds invalid UTF-8 ("string field contains invalid UTF-8", `internal/impl/codec_field.go`). SQLite text must therefore be a `bytes` field.
- Changing a field from `int64` to `sint64` changes its encoding, and old data then decodes as wrong numbers without an error. [I]

**Exact values** [V]:

- `double` and `fixed64` are 8-byte little-endian. A `-0.0` written with `protowire.AppendFixed64(math.Float64bits(x))` round-trips with its sign (tested).
- `int64` holds the full range. A negative value as a plain varint always takes 10 bytes; `sint64` uses ZigZag. `math.MinInt64` takes 10 bytes either way (tested).

**Streaming** [V]: "Protos must be smaller than 2 GiB when serialized", and "Protobuf is not designed to handle large messages… larger than a megabyte". The documented pattern is to "write the size of each message before you write the message itself". `encoding/protodelim` does exactly this, with a `MaxSize` option.

**Go implementation** [V]:

- Maintained by the Go team. v1.36.12 released 2026-08-10. v1.37.0-rc.1 tagged 2026-10-20, with a new Go version support policy.
- Pure Go. A consumer importing `proto` gets 2 modules in `go.sum` and builds 13 non-standard packages.
- Generated code needs `protoc`, a C++ program, plus the `protoc-gen-go` plugin.
- The runtime uses reflection and `unsafe`-based fast paths. [I]

**Stability** [I]: the wire format has been public since 2008 and has not changed, apart from deprecating groups. It is described in Google's documentation, not in a standards document. A file will very likely be readable in twenty years: the format is everywhere, and it is simple enough to decode by hand.

**Self-describing**: partly. `protoc --decode_raw` prints field numbers and values without a schema (`src/google/protobuf/compiler/command_line_interface.cc`) [V]. Without the schema it cannot tell `sint64` from `int64`, or `bytes` from an embedded message. [I]

**Hostile input** [V, OSV]:

- `protobuf-go` has its own fuzzers (`internal/fuzz/wirefuzz`, `oss-fuzz-build.sh`).
- Its two advisories are both outside the binary decoder: GO-2023-1631 (a panic in the text-format parser) and GO-2024-2611 (an infinite loop in `protojson`).
- `DefaultRecursionLimit` is 10000.

**Canonical** [V]: "Protobuf serialization is not (and cannot be) canonical". Deterministic mode does not hold across library versions or builds. Here this does not matter, because the checksum is taken over the stored bytes.

## 4. Protocol Buffers wire format by hand

**With `protowire`** [V, https://pkg.go.dev/google.golang.org/protobuf/encoding/protowire]:

- `protowire` "parses and formats the raw wire encoding". It offers `AppendTag`, `AppendVarint`, `AppendFixed64`, `AppendBytes`, `ConsumeTag`, `ConsumeVarint`, `ConsumeFieldValue` (which skips an unknown field), `EncodeZigZag`, and the size functions.
- It is one file of 571 lines (`encoding/protowire/wire.go` at v1.36.12). It imports `io`, `math`, `math/bits`, and the module's `internal/errors`.
- A consumer gets 1 module in `go.sum` and 3 non-standard packages (measured).
- No code generation, no reflection, no `protoc`.
- Tested: a reader loop with `ConsumeTag` and `ConsumeFieldValue` skipped an unknown field 99 and then read field 1.
- Presence is ours, because we write each field by hand. Text written as LEN bytes is never checked for UTF-8.

**Without any dependency** [V, `$GOROOT/src/encoding/binary/varint.go`]:

- The standard library's `binary.AppendUvarint` and `binary.Uvarint` use the same base-128 varint as protobuf: 7 bits at a time, least significant first, at most 10 bytes. `binary.AppendVarint` uses ZigZag. Fixed 64-bit values are `binary.LittleEndian`.
- A complete implementation is the tag, four wire types, and a skip function. [I] Roughly 150 to 250 lines with checks. Its correctness can be tested against `protowire` and `protoc --decode_raw` as oracles, and with Go's native fuzzing.
- One strictness point: both `binary.Uvarint` and `protowire.ConsumeVarint` accept an overlong varint, so `80 80 00` decodes as 0 in 3 bytes (tested). Our own decoder can reject non-minimal varints, duplicate non-repeated fields, and out-of-order fields, and so give each value one encoding.

**Assessment** [I]: hand-writing removes every weakness of section 3 except the missing self-description of types. The format stays readable by any protobuf implementation in any language, given a `.proto` file that we publish as documentation. That file would be a description, not a build input.

## 5. CBOR

Sources: https://www.rfc-editor.org/rfc/rfc8949.html, https://www.rfc-editor.org/rfc/rfc8742.html, https://github.com/fxamacker/cbor (v2.10.0 source: `decode.go`, `encode.go`), https://ipld.io/specs/codecs/dag-cbor/spec/, https://atproto.com/specs/data-model.

**Compatibility mechanism**:

- A record is a map, and its keys are small integers (fxamacker's `keyasint` struct tag). An old reader ignores a key it does not know.
- In fxamacker this is the default: `ExtraReturnErrors` is `ExtraDecErrorNone`, and `ExtraDecErrorUnknownField` turns unknown keys into errors. A new reader sees a missing key as the zero value.
- Tested: a struct with keys 1 and 2, encoded as `a2 01 01 02 63 6e 65 77`, decoded into a struct with only key 1 without error. [V]
- The RFC's design goals include "extensibility without the need for version negotiation", and "the extended data must be decodable by earlier decoders" (section 1.1). [V]

**Traps** [V unless marked]:

- "A map that has duplicate keys may be well-formed, but it is not valid", and each protocol "MUST define what to do" (sections 3.1 and 5.6). fxamacker can reject duplicates (`DupMapKeyEnforcedAPF`).
- Text strings (major type 3) must be valid UTF-8. fxamacker rejects invalid text by default ("cbor: invalid UTF-8 string", tested). SQLite text must therefore be a byte string (major type 2) with a type marker, or a tagged item.
- Integers range from −2^64 to 2^64−1, wider than `int64`. A decoder must reject values outside `int64`. fxamacker does: "-18446744073709551616 overflows Go's int64" (tested).
- fxamacker's default limits are `MaxArrayElements` 131072, `MaxMapPairs` 131072, and `MaxNestedLevels` 32 (`decode.go`). A commit of millions of rows must be a sequence of items, not one array, or the limit must be raised.
- Decoding into `interface{}` gives `uint64` for positive and `int64` for negative integers. Typed decoding avoids this. [I]

**Exact values** [V, tested with `CoreDetEncOptions`]:

- `-0.0` encodes as `f9 80 00`, and decodes with its sign.
- `+Inf` and `-Inf` encode as `f9 7c 00` and `f9 fc 00`.
- `0.1` stays an 8-byte double, and `1.5` shrinks to a half float, `f9 3e 00`, without loss. A reader must accept all three float widths.
- `math.MaxInt64` is `1b 7f…`, and `math.MinInt64` is `3b 7f…`.
- An empty byte string is `40`, distinct from null (`f6`).
- NaN becomes `f9 7e 00` by default, so a NaN payload is lost unless `NaNConvertNone` is set.
- The float shrinking is the RFC's "preferred serialization": "the shortest floating-point encoding that preserves its value" (section 4.1).

**Streaming** [V]:

- RFC 8742 CBOR Sequences are items "simply concatenated", and "can be consumed (and produced) incrementally without requiring a streaming CBOR parser".
- A malformed item stops the rest: "it is not possible to reliably decode the rest of the sequence".
- Indefinite-length arrays with a "break" code also stream, but deterministic encoding forbids them.
- fxamacker supports sequences (`UnmarshalFirst`, `NewDecoder`).

**Go implementation** [V]:

- v2.10.0 released 2026-10-28, maintained by Faye Amacker, active.
- Pure Go, "doesn't use Go's unsafe package". Its one dependency is `x448/float16`, so a consumer gets 2 modules.
- Uses reflection. Code generation is optional and not needed.
- Users named in the README include Arm, Cisco, Kubernetes, Microsoft, Red Hat and Tailscale.
- Release policy: "Coverage-guided fuzzing must pass billions of execs before tagging a release".
- OSV lists no advisories for `fxamacker/cbor` or `fxamacker/cbor/v2`.

**Stability** [V]: RFC 8949 is on the standards track as STD 94. It keeps "full compatibility with the interchange format of RFC 7049" and "does not create a new version of the format". The wire format is therefore unchanged since 2013.

**Self-describing** [V]: every item carries its major type, so a dump shows integers, floats, byte strings, text, arrays, maps and null without a schema. fxamacker has `Diagnose` and `DiagnoseFirst`, which print RFC 8949 diagnostic notation. Field meanings still need the schema, because keys are integers. [I]

**Canonical** [V]: section 4.2 defines Core Deterministic Encoding: "Preferred serialization MUST be used", map keys "MUST be sorted in the bytewise lexicographic order", and "Indefinite-length items MUST NOT appear". NaN needs one chosen representation, "typically 0xf97e00". fxamacker implements this as `CoreDetEncOptions`.

**DAG-CBOR and DRISL** [V]:

- DAG-CBOR is the strict subset for content-addressed data in IPLD. Floats "must always" be 64-bit, map keys must be strings, and the only tag is 42.
- It forbids values SQLite stores. "NaN, Infinity and -Infinity must not be accepted", and "-0.0 should not appear or be accepted".
- It forbids concatenation: "Back-to-back concatenated objects are not allowed or supported."
- AT Protocol's successor, DRISL (https://dasl.ing/drisl.html), disallows floats in atproto data "for now".
- Both suit small signed records, not a log of SQLite values. Ruled out.
- `go-ipld-prime` has had two 2026 advisories for its DAG-CBOR decoder: GO-2026-5073 (unbounded allocation from CBOR headers) and GO-2026-5684 (unbounded recursion). This is a reminder that "CBOR plus strict rules" still needs a hardened decoder.

## 6. A format of our own, and SQLite's changeset

**Own tag-length-value, as a baseline** [I]:

- It can do everything the hand-written protobuf wire does: a tag with a type code, a length, and skipping of unknown tags.
- It gains nothing over protobuf's wire format, except freedom from one rule: protobuf allows a non-repeated field to repeat.
- It loses external readers and external tools such as `protoc --decode_raw`.
- The difference is small, because the protobuf wire format is itself a TLV with four wire types. Keep this option only if the protobuf framing ever gets in the way.

**SQLite changeset format** (session extension) [V, `internal/sqlite/c/sqlite3.c` lines 235210–235330 and 235477, https://sqlite.org/sessionintro.html last updated 2025-05-31; capture and apply semantics are in `sqlite-cache-and-oracle.md` §2.1]:

- **Layout:**
  - A changeset is a sequence of table groups. Each group starts with `0x54` ('T'), a varint column count, one PK-flag byte per column, and the table name, NUL-terminated.
  - Each change is an op byte (INSERT `0x12`, UPDATE `0x17`, DELETE `0x09`), an "indirect" flag byte, and then an old and/or a new record.
  - A patchset uses `0x50` ('P') and keeps only the PK of a delete, and the PK plus the changed columns of an update.
- **Values:**
  - Each value is a type byte, then the payload: `0x00` undefined, `0x01` integer (8-byte big-endian), `0x02` real (8-byte big-endian IEEE 754), `0x03` text and `0x04` blob (varint length, then bytes), `0x05` NULL.
  - So it represents the full `int64` range, exact doubles including `-0.0` and the infinities, empty versus NULL, and "unchanged" versus NULL. Text is copied as bytes, without a UTF-8 check.
- **What it cannot represent:**
  - Schema changes. The session page does not mention DDL.
  - Column names and types: only the column count and PK flags. Apply matches columns by position.
  - Tables without a declared primary key, and rows with NULL in a primary key column ("No changes … are recorded").
  - Metadata such as the previous file, statements, or a timestamp.
- **Compatibility story:** none.
  - There is no magic number, no format version, and no field that an old reader could skip.
  - The format is described only in comments in the source. The session page says nothing about its stability or versioning.
  - A new op code or value type would be a hard error for every older reader. [I]
- **Other facts:**
  - The in-memory buffer is sized with `int` (`struct SessionBuffer`), so one buffer is under 2 GiB. The `_strm` functions stream in chunks.
  - The session extension is not in our build: `internal/sqlite/options.h` does not define `SQLITE_ENABLE_SESSION`.
- **Verdict** [I]: it is not a container for s4ql. Its value encoding is a good model: a type byte, then a fixed 8-byte integer or double, or a length and bytes. So is its "undefined" value, which separates "not changed" from NULL, the same split as pgoutput's `'u'` and `'n'` (section 8).

## 7. Ruled out quickly

- **FlatBuffers** [V, https://flatbuffers.dev/evolution/, `docs/source/internals.md`, `docs/source/support.md`, `go/` directory]:
  - Evolution is good: append fields or give every field an `id`, deprecate instead of removing, check with `flatc --conform`. SlateDB uses it for its manifest.
  - Offsets are `uint32`, so a buffer is under 4 GiB, and in practice under 2 GiB for signed offsets [I].
  - The whole buffer is built in memory, so a commit of millions of rows would need many small buffers with our own framing. That gives up its one strength, zero-copy random access. [I]
  - The support table lists no buffer verifier for Go, and the `go/` directory has none. A hostile or corrupted file can therefore make reads go out of range. [I]
- **Cap'n Proto** [V, https://capnproto.org/encoding.html, go-capnp README and tags]:
  - Evolution works like FlatBuffers', with ordinals and default values stored XOR-ed.
  - The Go library's latest tag is v3.1.0-alpha.2 (2025-10-25), and its README calls it "beta software" until "the official Cap'n Proto spec is finalized".
  - A consumer gets 3 modules. Schemas need the C++ `capnp` compiler. [I]
- **Apache Avro** [V, https://avro.apache.org/docs/1.12.0/specification/, latest release 1.12.2 on 2026-08-23]:
  - The binary encoding "does not include field names", and "readers are wholly reliant on the schema used when the data was encoded". Every file must therefore carry or name its writer schema, and every reader must implement Avro's schema resolution.
  - The best Go library, `hamba/avro`, is archived: "This project is no longer maintained". OSV lists 4 advisories for it, 3 from 2026-07. `linkedin/goavro` v2.15.0 (2026-01-21) is maintained but slow in published benchmarks (section 9).
  - Iceberg's manifests show the trap: bounds whose type is not stored must be inferred after a type promotion (section 8).
- **MessagePack** [V, https://github.com/msgpack/msgpack/blob/master/spec.md]:
  - It is CBOR's predecessor in spirit, with no standard deterministic encoding.
  - The spec's last change was in 2021, and in 2013 it split "raw" into str and bin, a change old deserializers had to be upgraded for.
  - It has no field-number mechanism: evolution means maps with keys of our choice, as in CBOR.
  - The main reflection library, `vmihailenco/msgpack`, last released on 2023-10-26. CBOR is the same idea with an IETF standard behind it.
- **Amazon Ion** [V, https://amazon-ion.github.io/ion-docs/docs/binary.html, ion-go commits]:
  - It is rich (typed nulls, `-0.0` floats, annotations) but needs symbol tables.
  - `ion-go` has had no commit since its v1.5.0 release on 2024-06-06.
- **Thrift compact protocol** [V, `doc/specs/thrift-compact-protocol.md`, OSV]:
  - Field ids are 16-bit, and unknown fields are skipped by type. That is the protobuf model with a smaller field space.
  - The Go library is part of an RPC framework, and has two 2026 advisories: CVE-2026-43871 (an infinite loop) and CVE-2026-41602 (integer overflow in `TFramedTransport`).
- **ASN.1 DER**:
  - DER is canonical and has extension markers [I].
  - Go's `encoding/asn1` has no REAL type at all (no float code in `$GOROOT/src/encoding/asn1`) [V].
  - The encoding of REAL in X.690 is complex, and ASN.1 parsers have a long history of CVEs [I].
- **Bebop** [V]: no official Go support in its README. The third-party `200sc/bebop` last released v0.6.2 on 2024-07-05.
- **SBE (Simple Binary Encoding)** [V, real-logic wiki "Message Versioning", release 1.42.0 on 2026-11-06]:
  - Fields can only be appended at the end of a block, with `sinceVersion`, and decoders are generated per schema version.
  - It is built for fixed-size trading messages, not variable rows and blobs.
- **Borsh** [V, near/borsh README]:
  - It has no tags, so no field can be added. Floats are `err_if_nan`.
  - `borsh-go` last released v0.3.1 on 2022-01-12.
- **Apache Fory** [V: v1.7.7 on 2026-10-12]:
  - Graft considers it (`graft.md`).
  - The project and its cross-language specification are young [I], so its longevity is unproven.
- **Small Go-only formats** (`mus`, `benc`, `colfer`, `gotiny`) [I]: fast in benchmarks, but each has a single maintainer, no specification process, and little or no evolution story.

## 8. What comparable systems use

The first block was checked by a helper agent in this run, in primary sources. The rest is from the existing notes.

- **Delta Lake** [V, https://github.com/delta-io/delta/blob/master/PROTOCOL.md]:
  - Commits are newline-delimited JSON, one action per line. Checkpoints are Parquet.
  - Readers "must ignore such unrecognized fields". This is safe because breaking changes must raise `minReaderVersion`/`minWriterVersion` or add an entry to `readerFeatures`/`writerFeatures`.
- **Apache Iceberg** [V, https://github.com/apache/iceberg/blob/main/format/spec.md]:
  - Table metadata is JSON. Manifests and manifest lists are Avro in v1 to v3. v4, which moves to Parquet, "is under active development and has not been formally adopted".
  - Readers "must throw an exception if a table's version is higher than the supported version".
  - Trap: bounds stored without their type must be inferred after a float-to-double promotion.
- **Kafka** [V, `docs/implementation/message-format.md`, `DefaultRecord.java` in apache/kafka]:
  - A record batch has a fixed big-endian header with a `magic` byte (currently 2) and a CRC-32C. Records inside use protobuf-style varints with ZigZag.
  - NULL is length −1.
  - The CRC follows `magic`, so readers "must parse the magic byte before deciding how to interpret the bytes".
- **FoundationDB** [V, `flow/README.md`, `design/tlog-forward-compatibility.md.html`, `design/backup-dataFormat.md`]:
  - Network messages use its own FlatBuffers-like `ObjectSerializer`. A new field must be one that "correctness must not depend on", and simulation tests this by default-initialising fields at random.
  - The tlog's on-disk format is versioned by an opaque `log_version`.
  - The backup mutation log is hand-rolled big-endian `type|kLen|vLen|key|value` blocks.
- **CockroachDB** [V, `pkg/kv/kvserver/raftlog/encoding.go`, `pkg/backup/backuppb/backup.proto`]:
  - Raft entries are protobuf, behind a 1-byte encoding prefix that gained new values over time.
  - Backups are SSTs plus a protobuf manifest. From 23.1, the large `Files` lists moved out of the protobuf manifest into SSTs, because of their allocation cost.
- **PostgreSQL logical replication** (pgoutput) [V, https://www.postgresql.org/docs/current/protocol-logicalrep-message-formats.html]:
  - Custom binary messages with a 1-byte type.
  - Each column is a kind byte: `'n'` NULL, `'u'` unchanged TOAST value, `'t'` text, `'b'` binary. Only `'t'` and `'b'` carry a 4-byte length and bytes.
  - The protocol version (1 to 4) is negotiated per session and not stored.
- **MySQL row-based binlog** [V, MySQL source `libs/mysql/binlog/event/rows_event.cpp` and the Format_description_event docs]:
  - The format description event carries a post-header length per event type, so an old reader can skip new header bytes.
  - TABLE_MAP optional metadata is TLV.
  - Trap: on an unknown TLV type the parser asserts instead of skipping the `len` bytes.
- **etcd WAL** [V, `server/storage/wal/encoder.go`, `walpb/record.proto`]:
  - Proto2 `Record{type, crc, data}`, framed with an 8-byte length padded to 8 bytes.
  - Each CRC is chained from the previous one.
- **SlateDB** [V, `slatedb/src/flatbuffer_types.rs`, `scripts/check_flatbuffers_evolution.sh`; `slatedb.md`]:
  - The manifest is FlatBuffers behind a 2-byte big-endian `MANIFEST_FORMAT_VERSION`, currently 2. CI runs `flatc --conform` against the base branch.
  - The WAL is written as SSTs. An RFC for a dedicated WAL format (`rfcs/0014`) was rejected.
- **Litestream LTX** [V, https://github.com/superfly/ltx/blob/main/README.md; `litestream-vfs.md` §2]:
  - Custom big-endian binary: a header, a page block, a page index, and a trailer.
  - Trap: "LTX files carry no on-disk version field". Versions 2 and 3 share the `LTX1` magic, so "A reader cannot determine the format version from the file alone".
- **Graft** [V, from `graft.md`]: commits use bilrost, a protobuf-like Rust format. Issue #296 plans a switch to Protobuf or Fory "so third-party tools can read the format".
- **FoundationDB Record Layer** [V, from `foundationdb.md`]: records and the schema are protobuf.
- **turbopuffer** [V, https://turbopuffer.com/docs/architecture]: every write adds a WAL file, with a CAS commit point. No WAL or manifest format is public.

Lessons [I]:

- Hand-rolled binary with a version (Kafka, FoundationDB, LTX) and protobuf (CockroachDB, etcd, Graft's plan) dominate logs. No surveyed log uses CBOR, MessagePack, Avro (except Iceberg's manifests) or Cap'n Proto.
- Store a version that the file alone reveals (LTX is the counterexample).
- Gate "ignore unknown fields" with a version or feature list, so a change that old readers must not ignore can still be expressed (Delta).
- Check format evolution mechanically in CI (SlateDB).
- Keep "unchanged" separate from NULL (pgoutput, the SQLite changeset).
- Keep large lists out of a single message (CockroachDB's backup manifest).

## 9. Size and speed

Published data [V, https://alecthomas.github.io/go_serialization_benchmarks/ `data.js`; repository last commit 2025-07-04]:

- The benchmark struct is small: `Name`, `BirthDay`, `Phone`, `Siblings`, `Spouse`, `Money`.
- The published data does not include `google.golang.org/protobuf` or `fxamacker/cbor`. It includes an old Cap'n Proto library (`glycerine/go-capnproto`), not v3.

| Library | Marshal ns/op | Unmarshal ns/op | Bytes |
|---|---|---|---|
| `gogo/protobuf` (codegen) | 191 | 192 | 53 |
| `tinylib/msgp` (codegen) | 191 | 239 | 97 |
| `vmihailenco/msgpack` (reflect) | 1078 | 705 | 92 |
| `google/flatbuffers` | 1080 | 203 | 95 |
| `linkedin/goavro` binary | 1186 | 1167 | 47 |
| `glycerine/go-capnproto` | 2103 | 612 | 96 |
| `encoding/json` | 1679 | 2612 | 151 |
| `encoding/gob` | 6683 | 33482 | 172 |

Reading [I]:

- Formats with numeric tags or no tags are about a third of the size of JSON. Formats with string keys (MessagePack maps, about 92 bytes) pay for the names.
- Speed is mostly the implementation: generated or hand-written code is several times faster than reflection.
- For s4ql, the row values dominate a change file's size, and a hand-written encoder is in the fast class.
- A per-value overhead of one or two bytes (tag, type, length) separates the good candidates by only a few percent.
- Compression, if added later, is a separate decision. The checksum would still cover the stored bytes.

## 10. Ranking

**1. Protobuf wire format, written by hand.** Use `protowire`, or our own small implementation on `encoding/binary` with `protowire` as a test oracle.

- Unknown fields are skipped by a rule that has not changed since 2008 and is implemented in every language.
- The wire surface is tiny: four wire types. That makes a strict decoder small enough to review completely and fuzz to exhaustion. [I]
- Exact values: ZigZag or fixed integers, raw IEEE bits for floats, and text as bytes with no UTF-8 check.
- Presence and duplicate handling are ours, so the classic proto3 traps do not apply.
- Streaming is a sequence of length-delimited records.
- No code generation, no `protoc`, no reflection, and zero or one dependency.
- `protoc --decode_raw` and a published `.proto` give outside readers a path.

**2. CBOR, a strict subset with integer map keys.** Use a hand-written subset, or `fxamacker/cbor`.

- It is an Internet Standard, unchanged since 2013.
- It is fully self-describing, so a dump shows each value's type, which matters for `ANY` columns and for debugging.
- It has a defined deterministic encoding and a standard for sequences (RFC 8742).
- It ranks second for three reasons:
  - A larger surface to restrict: tags, half floats, bignum-range integers, indefinite lengths, and simple values.
  - Text must be valid UTF-8, so SQLite text needs a workaround.
  - No surveyed log system uses it.

**3. Own TLV.** Keep it as the fallback only.

What would change the ranking [I]:

- **Self-description outweighs a small surface.** If Markus wants a dump of an arbitrary change file, without our schema, to show integers, floats, text, blobs and NULL unambiguously, CBOR moves to first.
- **Canonical content matters.** If two writers must produce identical bytes for identical commits, for example to deduplicate or to compare a rebuilt log, CBOR's standard deterministic encoding moves it up. With protobuf, we would have to define and enforce our own canonical rules.
- **Random access within a file matters.** If readers must range-read single rows of a huge change file, add an index trailer in either format, as LTX does. Only if zero-copy access matters would FlatBuffers come back, and then with our own verifier.
- **Generated readers in other languages become a goal.** This strengthens rank 1: the published `.proto` becomes a build input for those readers, while Go keeps its hand-written code.
- **A dependency-free core.** This favours rank 1 with our own varint code, since `encoding/binary` already has the varint and ZigZag functions.

## 11. Sketch: a change file in the protobuf wire format

The whole file is one valid protobuf message, whose top-level fields are records in write order. A reader streams it field by field: tag, length, payload. An old reader skips a record kind it does not know, and `protoc --decode_raw` can dump a file under 2 GiB. [I]

```
ChangeFile (the whole object; top-level fields in this order)
  1  header     LEN Header      exactly once, first
  2  table      LEN TableRef    sets the current table for the changes that follow
  3  upsert     LEN Row         a full row, inserted or changed
  4  delete     LEN Key         the primary-key values of a deleted row
  5  schema     LEN SchemaChange
  6  statement  LEN Statement   evidence only
 15  trailer    LEN Trailer     exactly once, last

Header
  1  format_version  varint      1; a reader refuses a version it does not know
  2  previous        bytes(32)   SHA-256 name of the previous change file; absent in the first file
  3  created         fixed64     wall-clock time in Unix nanoseconds, informational
  …  new fields are optional and skippable; a change that old readers must not
     ignore raises format_version (Delta's lesson)

Row / Key
  1  value  LEN Value   repeated, in column order

Value (exactly one field set)
  1  integer  sint64 (ZigZag varint)
  2  real     fixed64 (IEEE 754 bits, unchanged)
  3  text     bytes (raw, no UTF-8 check)
  4  blob     bytes
  5  null     varint 0
  (no field set = error)

Statement
  1  sql     bytes
  2  arg     LEN Value, repeated
  3  input   LEN Input, repeated (time, random seed, …)

Trailer
  1  records  varint   number of records before the trailer, as a cross-check
```

Notes on the sketch [I]:

- The file starts with the header's tag byte, `0x0a`, so there is no separate magic number. If tools need one, the header can carry a fixed `magic` value as its first field, and the file stays a valid protobuf message.
- The reader applies records into a local SQLite transaction and commits only after the SHA-256 of the bytes read equals the file's name. A truncated or corrupted file then never reaches the cache. The trailer's count is a second check, not the first.
- The writer cannot name the file before the last byte is hashed. A large commit is therefore written to a local temporary file, or hashed while it is uploaded to a temporary key, before the create-if-absent upload under its name.
- Strict decoding rules: minimal varints only, every non-repeated field at most once, header first and trailer last, a `Value` with exactly one field, and integer and length bounds checked before allocation.
- Open design questions, not decided here:
  - whether columns are identified by position, which the log's schema changes make well defined, or by name or ID;
  - whether a long run of rows for one table is grouped in one `LEN` record for size;
  - whether `format_version` also appears in a fixed-size prefix for readers that refuse a file without parsing it.

## Not verified

- The defaults of `protobuf-go`'s binary decoder for size limits beyond the recursion limit.
- The exact content of the fuzzing in `protobuf-go`'s OSS-Fuzz integration. Whether `fxamacker/cbor`'s private fuzz corpus covers the options we would use.
- That SBE's generator and Cap'n Proto's Go schema compiler need Java and C++ tools respectively (from memory).
- That JSON cannot carry Infinity or NaN (RFC 8259, from memory).
- Published size and speed numbers for `google.golang.org/protobuf` and `fxamacker/cbor` in the same benchmark as the others: the benchmark's published data leaves them out.
- Neon's formats were not checked.
