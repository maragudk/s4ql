# What a change file holds: how other logs represent their contents

Date: 2026-11-11. Tags: **V** = verified in this run in a primary source (official documentation, a protocol specification, or source code), with the reference. **I** = inferred, or from memory.

Question: how do other databases and change streams represent the *contents* of a log of changes, so that s4ql does not have to invent everything? We are designing what a change file holds: row changes, schema changes, and the SQL statements kept as evidence. The byte encoding is settled (Protocol Buffers, see [change-file-encodings.md](change-file-encodings.md)), and this note does not repeat that survey.

Short answer: every system that ships row changes to a reader with its own copy of the schema says which table *and which columns* the values belong to, and the systems that send less than names have a documented way to diverge silently. s4ql's heading before a table's rows should hold the table name, the column names in order, their types, and the primary-key flags, and replay should refuse a file whose heading does not equal the schema it has replayed. Section 10 has a summary table, section 11 the recommendations, and section 12 the open decisions.

Method: specifications, documentation, and source code read in this run. PostgreSQL at `master` commit `7a5559a` and the version 18 and 19 documentation. MySQL at `trunk` commit `06bcb0e` and the 8.4 manual. Delta Lake `PROTOCOL.md` at `master` commit `812bcbc`. SQLite 3.54.1 from `internal/sqlite/c/`. A helper agent checked Debezium, Spanner, CockroachDB, Dolt, and the SQLite replication systems in primary sources in this run. File paths such as `src/backend/...` are relative to each project's repository.

## 1. What s4ql has agreed so far

From `docs/spec.md` and the "Storage formats" item in `docs/todo.md`:

- A change file holds the net effect of one commit. A primary key appears at most once per table.
- The changes are a list, applied in order. The first kinds are: create a table (name and columns as data, not SQL text), put a full row, and delete a row by its primary key.
- An unknown kind of change is refused, never skipped.
- The SQL statements are kept as evidence, with their parameters and non-deterministic inputs. They are not linked to the changes.
- Every table is `STRICT` and `WITHOUT ROWID`, so every table has a primary key and no integer keys are generated (`docs/roadmap.md`).

Open right now: whether the heading before a table's rows holds the table name only, or also the column names and perhaps types.

## 2. PostgreSQL logical replication (`pgoutput`)

Sources: the message formats, https://www.postgresql.org/docs/current/protocol-logicalrep-message-formats.html; the message flow, https://www.postgresql.org/docs/current/protocol-logical-replication.html; restrictions, https://www.postgresql.org/docs/current/logical-replication-restrictions.html; subscriptions, https://www.postgresql.org/docs/current/logical-replication-subscription.html. All read in this run (PostgreSQL 18 documentation).

**Table and column identity.**

- Every Insert, Update, and Delete message carries a numeric relation OID, not a name. [V formats]
- "Before the first DML message for a given relation OID, a Relation message will be sent, describing the schema of that relation. Subsequently, a new Relation message will be sent if the relation's definition has changed since the last Relation message was sent for it." [V message flow]
- The Relation message holds the namespace, the relation name, the replica identity setting, and, per column, a key flag, the column name, the type OID, and the type modifier. A Type message precedes it for a type that is not built in. [V formats]
- The subscriber matches tables "using the fully qualified table name", and "Columns of a table are also matched by name. The order of columns in the subscriber table does not need to match". The target may have extra columns, filled with their defaults. [V subscriptions]
- A target that lacks a published column is an error: `logical replication target relation "%s.%s" is missing replicated column: %s`. [V `src/backend/replication/logical/relation.c` line 274]
- The Relation message is state that lives across messages, and keeping it right was subtle. `pgoutput.c` tracks, per relation, whether the schema was sent, and for streamed transactions it tracks this per transaction, because "the commit order may be different from the order the transactions are sent in" and "the (sub) transactions might get aborted so we need to send the schema for each (sub) transaction so that we don't lose the schema information on abort". [V `src/backend/replication/pgoutput/pgoutput.c` lines 107–124, 740–770]

**Row images.**

- Each column in a tuple is one of `'n'` NULL, `'u'` "unchanged TOASTed value (the actual value is not sent)", `'t'` text, or `'b'` binary. NULL and unchanged are therefore different. [V formats]
- Insert carries the new tuple. Delete carries the key (`'K'`) by default, or the whole old row (`'O'`) with `REPLICA IDENTITY FULL`. Update carries the new tuple, plus the old key only "if the update changed data in any of the column(s) that are part of the REPLICA IDENTITY index", or the old row with `FULL`. [V formats]
- So an update of the primary key is one Update message with the old key, not a delete and an insert. [V formats]
- The new tuple of an update is full, except that large out-of-line (TOAST) values that did not change are sent as `'u'`. [V formats]

**Schema changes.** "The database schema and DDL commands are not replicated." A mismatch makes replication "error until the schema is updated", and the advice is to apply additive changes to the subscriber first. [V restrictions] PostgreSQL 19 (RC 1 on 2026-10-15) adds replication of sequence values, and its release notes do not mention DDL. [V https://www.postgresql.org/docs/19/release-19.html] Logical decoding also skips the rows that a table rewrite produces: "Ignore temporary heaps created during DDL unless the plugin has asked for them". [V `src/backend/replication/logical/reorderbuffer.c` lines 2371–2376] So `ALTER TABLE ... ADD COLUMN ... DEFAULT` never re-emits existing rows. [I from V]

**Original statements.** None. The only free-form record is the logical decoding message (`'M'`), with a prefix and opaque content, written by `pg_logical_emit_message` and sent only when the subscriber asks for it with the `messages` option. [V formats, message flow] Extensions use it to ship things that are not rows. [I]

**Non-deterministic inputs.** Not applicable: logical decoding reads results from the WAL. [I]

**Transaction shape.** Statement order, not net effect: "all messages between a pair of Begin and Commit messages belong to the same transaction", and each row change is its own message, so a key can appear many times. [V message flow; I for repetition] Begin and Commit carry the commit LSN and timestamp. Large transactions can be streamed before commit between Stream Start and Stream Stop, from protocol version 2. [V formats]

**Unknown kinds.** The protocol version (1 to 4) is negotiated at `START_REPLICATION`, and each version adds messages, so a reader never sees a kind it did not ask for. [V message flow] The apply worker raises `invalid logical replication message type` on anything else. [V `src/backend/replication/logical/worker.c` line 3944]

**Pitfalls.**

- DDL is not replicated, so every schema change is a manual, ordered procedure on two servers, and a mistake stops replication. [V restrictions]
- Sequences were not replicated until version 19, so a promoted subscriber hands out ids that already exist. [V restrictions, release-19]
- The stateful Relation cache needed special handling for streamed and aborted transactions (above).

## 3. MySQL binary log

Sources: the 8.4 manual pages on binary logging options (https://dev.mysql.com/doc/refman/8.4/en/replication-options-binary-log.html), differing table definitions (`replication-features-differing-tables.html`), safe and unsafe statements (`replication-rbr-safe-unsafe.html`), and functions (`replication-features-functions.html`); and the source at `trunk`: `libs/mysql/binlog/event/rows_event.h`, `statement_events.h`, `control_events.h`, `binlog_event.h`, `sql/binlog_reader.cc`, and `sql/binlog.cc`. All read in this run.

**Table and column identity.**

- "In row-based mode, every row operation event is preceded by a Table_map_event which maps a table definition to a number." It carries a 6-byte table id, the database and table names, the column count, one type byte per column, per-column type metadata, a nullability bitmap, and optional metadata as type-length-value fields. [V `rows_event.h` lines 68–200]
- The rows events refer to the table only by that number. [V `rows_event.h`, `Rows_event` post-header]
- Column *names* are optional metadata: "This is only included if binlog_row_metadata=FULL". The default of `binlog_row_metadata` is `MINIMAL`. [V `rows_event.h` line 454; options page] The manual gives the purpose of `FULL`: replicas "transfer data when its table structure is different from the source's", and external software can "decode row events". [V options page]
- The replica matches columns by position: "Columns common to both versions of the table must be defined in the same order on the source and the replica", and they must come before any extra columns. [V differing-tables page]
- Without names, a CDC reader must know the schema at each point of the stream by other means. [I] Debezium's DDL parser and schema history exist for this reason (section 6).

**Row images.**

- Each rows event has a bitmap of the columns present in the before image and in the after image, and each row has a NULL bitmap over the present columns. So "absent" (not in the bitmap) and NULL are different. [V `rows_event.h` lines 717–880]
- `binlog_row_image` is `full` by default: all columns in both images. `minimal` logs only the columns needed to identify the row in the before image, and only the changed or generated columns in the after image. A delete logs only the before image, an insert only the after image. [V options page]
- An update of the primary key is one update row with a before and an after image. [I from the format]
- The trap: with `minimal` or `noblob`, if the tables differ in column order, types, or primary key, "no warning or error is issued; the source and replica silently diverge". [V options page]
- Downstream tools depend on full images. gh-ost: "Right now `FULL` row image is supported." [V https://github.com/github/gh-ost/blob/master/doc/requirements-and-limitations.md]

**Schema changes.** "DDL statements such as CREATE TABLE and ALTER TABLE are always logged as statements, without regard to the logging format in effect". [V options page] The DDL text travels in a `QUERY_EVENT`, and the replica runs it again. So an `ADD COLUMN ... DEFAULT` is one statement, and the existing rows are not in the log. [I from V] `CREATE TABLE ... SELECT` is logged as a generated `CREATE` statement plus row events. [V `replication-sbr-rbr.html`]

**Original statements.** With `binlog_rows_query_log_events=ON` (default `OFF`), the server writes a `Rows_query_log_event` with the statement text. It is written "into binlog before the table map", so its position links it to the row events of its statement. [V options page; `sql/binlog.cc` lines 8263–8268] It is "informational", and readers normally ignore it. [V options page] Whether the text has the parameter values of a prepared statement substituted was not checked. [I]

**Non-deterministic inputs (statement-based format).**

- `Query_event` carries the statement and status variables such as `sql_mode` and the time zone. [V `statement_events.h` lines 64–110, 485–551]
- `Intvar_event` holds `LAST_INSERT_ID` or `INSERT_ID` for the next query, and `Rand_event` holds the two 64-bit seeds of `RAND()`. [V `statement_events.h` lines 910–1043]
- "For NOW(), the binary log includes the timestamp", so the replica returns the source's time. `SYSDATE()` "is not affected by SET TIMESTAMP statements in the binary log and is nondeterministic". [V functions page]
- The list of unsafe statements kept growing: `FOUND_ROWS()`, `GET_LOCK()`, `LOAD_FILE()`, `RAND()`, `ROW_COUNT()`, `SYSDATE()`, `UUID()`, `USER()`, system variables, loadable functions, the full-text plugin, `AUTO_INCREMENT` from triggers, `INSERT ... ON DUPLICATE KEY UPDATE` on a table with several unique keys (Bug #11765650), updates with `LIMIT`, and more. [V safe-unsafe page]
- `INSERT INTO t2 SELECT UUID(), * FROM t1` cannot be made safe in statement format at all. [V functions page]
- The outcome: "binlog_format is deprecated ... only row-based logging should be employed for any new MySQL Replication setups." [V options page]

**Transaction shape.** Statement order. A statement's rows can span several rows events, and the last one has `STMT_END_F`. [V `rows_event.h` line 893] `binlog_row_event_max_size` (default 8192 bytes) is a soft limit on one rows event. [V options page] "A transaction is written in one chunk to the binary log, so it is never split between several binary logs." [V options page]

**Unknown kinds.** An event type the reader does not know is refused, unless the writer set `LOG_EVENT_IGNORABLE_F` in its header. Then the reader turns it into an `Ignorable_log_event` "So that SLAVE SQL THREAD will only update the position and continue". [V `sql/binlog_reader.cc` lines 309–321; `binlog_event.h` line 165] The base class says new ignorable events "shall not harm the old slaves". [V `control_events.h` lines 654–668] `Rows_query_event` derives from it. [V `rows_event.h` line 1185] Event type numbers "should never change", and the obsolete version 1 rows events are "rejected by the applier since 8.4.0". [V `binlog_event.h` lines 312–372]

**Pitfalls.**

- Silent divergence with minimal images and differing tables (above).
- Statement-based replication's open-ended list of unsafe statements, ending in deprecation (above).
- Column names were optional and off by default, so every external reader had to track DDL itself. [I]
- The TABLE_MAP optional metadata parser asserts on an unknown TLV type instead of skipping it ([change-file-encodings.md](change-file-encodings.md) section 8). [V there]

## 4. SQLite session extension (changesets and patchsets)

Sources: the format comment in `internal/sqlite/c/sqlite3.c` lines 235251–235330, and the API comments in `internal/sqlite/c/sqlite3.h` lines 12530–12760. Read in this run. The capture and apply semantics, with measurements, are in [sqlite-cache-and-oracle.md](sqlite-cache-and-oracle.md) section 2.1.

**Table and column identity.** Each group of changes starts with a table header: `'T'` (`'P'` for a patchset), the column count, one primary-key flag per column, and the unqualified table name. There are no column names or types. "The i'th field of each record is associated with the i'th column of the table". [V `sqlite3.c`]

**When the reader's schema differs.** A table is compatible if it has the same name, "at least as many columns as recorded in the changeset", and the primary-key columns in the same positions. Otherwise "it is not an error, but none of the changes associated with the table are applied", and only a `sqlite3_log()` warning is issued. [V `sqlite3.h` lines 12738–12748] A changegroup fills in missing trailing columns "using the default column values from the database schema". [V `sqlite3.h` lines 12543–12549] The existing note measured the effect: after an unshipped `ADD COLUMN`, the rows were "silently dropped" and apply returned success. [V there, E5]

**Row images.**

- Changeset: an insert has the full new row, and a delete the full old row. An update has the old values of the primary key and of the changed columns, and the new values of the changed columns. All other fields are "undefined" (type byte `0x00`), which is distinct from NULL (`0x05`). [V `sqlite3.c`]
- Patchset: a delete keeps only the primary key, and an update only the primary key and the new values of changed columns. [V `sqlite3.c`]
- An update of the primary key is recorded as a delete plus an insert. [V there, §2.1.1]

**Schema changes.** Not represented. [V there] **Original statements and non-deterministic inputs:** not represented. **Transaction shape:** net effect, with "data ... only recorded once" per primary key, combined with the final contents of the table. Changes are grouped per table, and their order within a table "is undefined". [V there, §2.1.1] **Unknown kinds:** no version and no extension point ([change-file-encodings.md](change-file-encodings.md) section 6). [V there]

**Pitfalls.** Silent skipping of an incompatible table, matching by position, and no DDL are the three design choices that the earlier measurements turned into silent divergence. [V there]

## 5. Delta Lake transaction log

Source: `PROTOCOL.md` at `master` commit `812bcbc` (https://github.com/delta-io/delta/blob/master/PROTOCOL.md), and the Delta documentation on change data feed (https://docs.delta.io/latest/delta-change-data-feed.html) and column mapping (https://docs.delta.io/latest/delta-column-mapping.html). Read in this run.

Delta's log records *files*, not rows: `add` and `remove` actions name Parquet data files. It is the closest system in shape to s4ql (a log of immutable commits in object storage), and the furthest in granularity. [V Actions]

**Table and column identity.**

- The `metaData` action holds the whole schema (`schemaString`), and "Subsequent metaData actions completely overwrite the current metadata of the table". There is "at most one metadata action in a given version". [V Change Metadata]
- The data files are Parquet and carry their own column names. With column mapping, each column also has a stable 32-bit id and a physical name that "is static and can be different than the display name of the column, which is changeable". Readers in `id` mode resolve columns by the id, and in `name` mode by the physical name. This is what makes rename and drop cheap. [V Column Mapping]

**Row images (change data feed).** When enabled, a commit can add `cdc` files whose rows have `_change_type` = `insert`, `update_preimage`, `update_postimage`, or `delete`. So an update carries both the full before and the full after row. [V Change Data Files, Add CDC File]

**Schema changes.** A new `metaData` action with the complete new schema. `ALTER TABLE ... ADD COLUMN` with a default in the same command is forbidden: "Table operations that add new columns ... MUST not specify a default value for any column in the same command that the column is created". A default may be set afterwards, and "this metadata only applies for write operations, not read operations". So no existing row ever needs rewriting. [V Default Columns]

**Renames hurt readers of the change stream.** Change data feed "has limitations after performing non-additive schema changes such as renaming or dropping a column". Support grew release by release: none in 2.0, batch reads without such changes in 2.1, streaming in 2.2, batch reads across such changes in 2.3 ("Queries still fail if the version range specified spans a non-additive schema change"), and streaming across them only with "schema tracking" in 3.0. [V change data feed page]

**Original statements.** `commitInfo` is optional provenance, and "Implementations are free to store any valid JSON-formatted data" in it. The example has `operation` and `operationParameters` but no SQL text. Checkpoints do not keep it. [V Commit Provenance Information, Action Reconciliation] With in-commit timestamps, `commitInfo` must come first and hold `inCommitTimestamp`. Before that feature, the commit time came from the file's modification time. [V In-Commit Timestamps, Add CDC File]

**Transaction shape.** One commit is one log file, and replay reconciles actions: "The latest metaData action seen wins", and for files "keep only the newest reference for each logical file". [V Action Reconciliation]

**Unknown kinds.** Two levels. Readers "must ignore" unrecognized actions and fields, which is safe because "breaking changes must be accompanied by an increase in the protocol version recorded in a table or by the addition of a table feature". A reader that does not support every name in `readerFeatures` must not read the table. [V Protocol Evolution, Table Features]

**Pitfalls.**

- A writer that upgrades to table features must assume that every feature of the old protocol version was used somewhere in history, unless it can prove otherwise. The spec's example is change data feed, switched on and off before the upgrade. [V Table Features for New and Existing Tables]
- Renames and drops broke change data readers for several releases (above).

## 6. Change-data-capture formats

The helper agent read these in this run, at debezium `16a6c5c`, cockroachdb/docs `27e64c5` (v26.3), and googleapis `547f8fd`. Paths are relative to each repository. I re-checked the Spanner `ColumnMetadata` message and the CockroachDB backfill text myself.

### Debezium

**Table and column identity.** Each event value has `before`, `after`, `source`, `op`, and `ts_ms`. `source` names the database, schema, and table. Each event can carry its own Kafka Connect schema, with field names, types, and optionality, next to the payload. [V `documentation/modules/ROOT/pages/connectors/postgresql.adoc` lines 940–990, 1180–1285]

**Row images.** `op` is `c`, `u`, `d`, `t` (truncate), `m` (message), or `r` (snapshot read). A delete has `after` null, and with PostgreSQL's default replica identity its `before` holds only the key. A tombstone with the same key follows for Kafka compaction. [V `postgresql.adoc` lines 1067, 1188–1283] An update of the key becomes a delete, a tombstone, and a create, linked by the headers `__debezium.oldkey` and `__debezium.newkey`. [V lines 1162–1180] PostgreSQL's unchanged TOAST values arrive as the placeholder `__debezium_unavailable_value`, because reading the value later "would potentially lead to race conditions". A consumer cannot tell that sentinel from real data without knowing it. [V lines 2381–2392, 4516–4519] Debezium requires `binlog_format=ROW` and `binlog_row_image=FULL` on MySQL. [V `partials/modules/all-connectors/shared-mariadb-mysql.adoc` lines 2927–2931]

**Schema changes.** For MySQL, Debezium parses the DDL text in the binlog and keeps a separate schema history topic of `HistoryRecord{source, position, databaseName, schemaName, ddl, tableChanges}`. [V `debezium-connector-common/src/main/java/io/debezium/relational/history/HistoryRecord.java` lines 19–25] On recovery it prefers the structured `tableChanges` (create, alter with `previousId`, drop), with the full table definition after the change, and parses the DDL text only when that is missing or `prefer.ddl` is set. [V `AbstractSchemaHistory.java` `recover()` lines 98–160; `shared-mariadb-mysql.adoc` lines 209–457] The history topic must never be partitioned, because it needs one global order. [V same, line 265] Debezium's new SQLite connector (incubating) captures full old and new rows with triggers, detects DDL from `schema_version`, and emits only `tableChanges`, so a table rename shows up as a drop and a create. [V `pages/connectors/sqlite.adoc` lines 30–52, 206, 351–372]

**Original statements.** The MySQL option `include.query` puts the statement text into each row event, from the binlog's query annotations. Parameters are not separate, and snapshot events never have it. [V `partials/.../ref-mariadb-mysql-rqd-connector-cfg-props.adoc` lines 423–437]

**Transaction shape.** Statement order from the source log. [I] An optional transaction topic has BEGIN and END events, and END has `event_count` and a count per table. Each data event gets `transaction{id, total_order, data_collection_order}`. [V `postgresql.adoc` lines 476–540]

**Unknown kinds.** No "safe to ignore" mechanism was found. Unparseable DDL stops the connector unless `schema.history.internal.skip.unparseable.ddl=true`, which "can lead to data loss or mangling". [V `partials/.../ref-connector-configuration-database-history-properties.adoc` lines 33–37]

**Pitfalls.** Most come from keeping the schema apart from the data and replaying it from DDL text. [V, issue titles read through the Jira REST API]

- The history topic or its content is fully or partly missing: DBZ-6142, DBZ-7846, DBZ-1230. Data loss in it: DBZ-7809. Duplicates corrupt the internal schema: DBZ-5342.
- A rename lost its `previousId`: DBZ-5386.
- DDL the parser cannot read: DBZ-8849, DBZ-6736, DBZ-6316. The skip option was broken for MariaDB: DBZ-8745.
- DDL run with `sql_log_bin=OFF` is missing from the history, and the in-memory schema goes stale. [V `shared-mariadb-mysql.adoc` around line 160]

### Google Spanner change streams

Source: `google/spanner/v1/change_stream.proto`, https://cloud.google.com/spanner/docs/change-streams/details, and https://cloud.google.com/spanner/docs/change-streams.

- **A heading in every record.** A `DataChangeRecord` covers one table, one modification type, one commit timestamp, one partition, and one transaction. It carries `column_metadata`: per column, the name, the type, `is_primary_key`, and the ordinal position "based on the original table definition". Each value points into that list by `column_metadata_index`. [V `change_stream.proto` lines 43–75]
- **Row images.** A `Mod` has `keys`, `old_values`, and `new_values`, which "only contain the non-key columns". The capture type is `OLD_AND_NEW_VALUES` (the default, changed columns only), `NEW_VALUES`, `NEW_ROW` (all watched columns), or `NEW_ROW_AND_OLD_VALUES`. [V proto lines 120–134; details page]
- **Schema changes.** Change streams "don't watch schema changes or any data changes that directly result from schema changes, other than backfills for default values". A dropped table emits no deletes. [V change-streams page] The backfill comes as records with `is_system_transaction` set ("TTL, column backfill, etc."). [V proto lines 150–206]
- **Original statements.** Only a `transaction_tag`. [V proto]
- **Transaction shape.** A transaction can span many records and partitions. Each record has `record_sequence`, `number_of_records_in_transaction`, `number_of_partitions_in_transaction`, and `is_last_record_in_transaction_in_partition`, so a reader can count the records to know it has the whole transaction. [V proto lines 150–206] The sequence "might not always match the original ordering". [V details page] Whether one key can repeat in a transaction was not found.
- **Unknown kinds.** The record is a `oneof` of five kinds. Apache Beam's reader throws `Unknown change stream record type`, and maps unknown modification and capture types to `UNKNOWN`. [V apache/beam `.../spanner/changestreams/mapper/ChangeStreamRecordMapper.java` lines 107, 273–275, 427–489]

### CockroachDB changefeeds

Source: `src/current/v26.3/create-changefeed.md`, `changefeed-messages.md`, and `changefeed-message-envelopes.md` in cockroachdb/docs.

- **Identity and images.** Rows are JSON or Avro with column names. The default `wrapped` envelope has the key and the full `after` row, and does not tell an insert from an update. `diff` adds `before`, and `enriched` adds `op`, `source`, and `schema`. [V `create-changefeed.md` lines 119–148; envelopes lines 477–755; I for insert versus update]
- **Deletes.** The key with `"after": null`. A delete can arrive for a row never seen inserted. A change to a key column gives a delete and an insert. [V `changefeed-messages.md` lines 176–184; `_includes/v26.3/cdc/composite-key-delete-insert.md`]
- **Schema changes.** No DDL events. Adding a column with a default, a stored computed column, or a `NOT NULL` column, or dropping a column, makes the changefeed "emit a copy of the table using the new schema": every row again. `schema_change_policy` is `backfill` (default), `nobackfill`, or `stop`. [V `changefeed-messages.md` lines 266–344; `create-changefeed.md` lines 143–144]
- **Transaction shape.** At-least-once delivery, ordered per key only on first emission, with "no total message ordering or transactional ordering". Resolved timestamps are the barrier. [V `changefeed-messages.md` lines 30–49, 128–262]
- **Pitfalls.** Duplicates after restarts and schema changes. JSON `null` and SQL NULL look the same unless `encode_json_value_null_as_object` is set. [V lines 227–262, 530–553]

## 7. Dolt: row history without a change log

Source (helper agent): dolt `ca92b7c`, https://docs.dolthub.com/architecture/storage-engine/commit-graph, and the `dolt_diff_<table>` documentation.

- A commit is a content-addressed snapshot of all tables in prolly trees. No change log is stored, and a diff is computed between two snapshots. [V docs]
- `dolt_diff_<table>` uses the *current* schema for its columns. The `DOLT_DIFF()` function is the tool for ranges that span schema changes. [V docs]
- **Column identity.** Each column has a `uint64` tag that survives renames. It is derived from a seeded random generator over the table name, column name, and column kinds, with the comment "DO NOT ALTER THIS METHOD". Merge matches columns by tag, then by name. [V `go/libraries/doltcore/schema/column.go` lines 58–59; `schema/tag.go` lines 78–110; `merge/merge_schema.go` lines 553–575, 720–745]
- **Pitfalls.** Issue #7772 (open), "Get rid of column tags": two `ALTER` sequences that give the same schema give different hashes, and after the new storage format "the only product reason to keep tags was ... column rename". Identical alterations on two branches give a "two different columns with same tag" merge error (#2335). Tag collisions dropped tables or indexes (#11007, #10005, #10003). [V GitHub issues]
- Lesson [I]: a hidden numeric identity derived from history is fragile. An explicit rename record in the log keeps track of renames without one.

## 8. Logical logs for SQLite

Source (helper agent): rqlite `d904ad9`, cr-sqlite `43ab94c`, marmot `2a8acbe`, turso `0c9fe0d`, corrosion `f7d8c91`, and the documentation pages named below.

### rqlite: statements as the truth

- A Raft entry holds `Statement{sql, parameters[]}`, where a parameter is a typed `oneof` with an optional name. There is no row data. [V `command/proto/command.proto` lines 24–49, 172–186]
- Before a statement enters the log, rqlite rewrites `RANDOM()`, `RANDOMBLOB(N)`, `'now'` in the date and time functions, and `CURRENT_TIMESTAMP`, `CURRENT_DATE`, and `CURRENT_TIME` into literals, except inside `ORDER BY`. [V `command/sql/processor.go` lines 37–81, 166–258]
- Time becomes a Julian day printed with `%f`, six decimals of a day. [V lines 278–279] That is a resolution of about 86 ms. [I, arithmetic]
- A statement that fails to parse or rewrite goes into the log unrewritten. [V lines 61–72] Defaults, `CHECK`, generated columns, indexes, triggers, and views are deliberately not rewritten. [V lines 170–177] Its documentation says such defaults and triggers may differ "from node to node". [V https://rqlite.io/docs/api/non-deterministic/]
- Bugs: the original problem, #536 (2018). Rewriting froze `DEFAULT (datetime('now'))` in a `CREATE TABLE`, #2592 (2026-03). `CURRENT_TIMESTAMP` inside `strftime()` was rewritten to a quoted identifier, so the literal string was stored, #2965 (2026-10). [V issues]
- Issue #1338, open since 2023, proposes shipping session changesets instead, to end the "concerns about RANDOM, TIMESTAMP, and AUTOINCREMENT". [V]
- rqlite now also emits row-level change events, `CDCEvent{op, table, column_names, old/new_row}`, captured with the pre-update hook as each node applies the log. They are an output only. [V `command.proto` `CDCEvent`; `cdc/DESIGN.md` lines 5–44]

### cr-sqlite and Corrosion: one change per cell

- `crsql_changes` has the columns `table`, `pk` (packed), `cid`, `val`, `col_version`, `db_version`, `site_id`, `cl`, and `seq`. [V `core/src/changes-vtab.c` lines 26–31] `cid` is the column *name*. [V corrosion `doc/crdts.md` lines 78–84]
- Row existence uses the sentinel column `"-1"`, and an even causal length `cl` means deleted. [V `core/rs/core/src/c.rs` lines 10–11; `changes_vtab_write.rs` lines 514–527]
- Schema changes are not in the changes. Each peer wraps its own `ALTER` in `crsql_begin_alter` and `crsql_commit_alter`. [V `README.md` lines 73–79; `core/rs/core/src/alter.rs` lines 42–90]
- An unknown table is an error on merge. [V `changes_vtab_write.rs` lines 491–499] In Corrosion, a peer with an older schema fails with "SQL logic error" (#261), and buffered changes are retried after a schema change (#504). Corrosion forbids destructive schema changes. [V issues; `doc/schema.md` lines 3–16]

### Marmot: rows by name, DDL as SQL

- A replicated statement is a `oneof`: a row change, a DDL change (`sql`), and others. A captured row has `OldValues` and `NewValues` as maps from column *name* to bytes, from the pre-update hook. A transaction carries `required_schema_version`, and a participant refuses a prepare whose required version is above its own. [V `grpc/marmot.proto` lines 149–276; `common/cdc_entry.go` lines 23–33; `docs/src/pages/replication.mdx` lines 1108–1114]
- Its documentation gives the reason for rows: SQL replay is "Non-deterministic (UUID, RANDOM, NOW)", while row capture gives "Exact byte-for-byte values". [V `replication.mdx` lines 240–247]
- Forward compatibility by design: a new flag was put on an existing type so that old binaries fail cleanly, because a new enum value "would instead reach a conversion that does not know it". [V `marmot.proto` lines 198–207]

### Turso's `turso_cdc` (early preview)

- A change row has `change_type` (1 insert, 0 update, −1 delete, 2 commit), the table name, the rowid, and `before` and `after` records. DDL is recorded as row changes to `sqlite_schema`. [V turso `docs/manual.md` lines 1100–1180]
- The records hold no column names. The decoding function uses the *current* schema, which "can lead to incorrect results when decoding older CDC records", so users must track schema versions themselves. [V line 1221] `WITHOUT ROWID` tables are not supported. [V line 1155]

### Others

- **PowerSync:** a per-bucket oplog of PUT (insert or replace, full row as JSON) and REMOVE. "The protocol itself is schemaless", and unknown tables land in `ps_untyped`. [V docs.powersync.com `architecture/powersync-protocol.md` lines 29–77]
- **sqlite-sync:** every device must have the same schema, and a schema hash travels with each payload. [V sqliteai/sqlite-sync `README.md` line 193]

## 9. Ruled out: systems that ship pages or frames

These replicate physical pages, not logical row changes, so they have nothing to say about row, schema, or statement records. [V, helper agent]

- **Litestream:** WAL pages in LTX files (https://litestream.io/how-it-works; see also [litestream-vfs.md](litestream-vfs.md)).
- **LiteFS:** LTX files of pages (superfly/litefs `docs/ARCHITECTURE.md` lines 11–23).
- **dqlite:** Raft commands of WAL frames (canonical/dqlite `src/command.h` lines 14–17).
- **libSQL:** replication streams `Frame` messages of WAL frames (`libsql-replication/proto/replication_log.proto` lines 34–48).

## 10. Summary table

Questions: (1) table and column identity, (2) row images, (3) schema changes, (4) statements as evidence, (5) non-deterministic inputs, (6) transaction shape, (7) unknown record kinds.

| System | 1 Identity | 2 Images | 3 Schema | 4 Statements | 5 Non-determinism | 6 Shape | 7 Unknown kinds |
|---|---|---|---|---|---|---|---|
| PostgreSQL `pgoutput` | Relation message (names, type OIDs, key flags) before first use and after a change; rows by OID; match by name, loud error | full new row; old key only if key changed; `'u'` unchanged ≠ `'n'` NULL; key update is one Update | not replicated | none (`'M'` messages are free-form) | n/a (results from WAL) | statement order; streaming of large transactions | version negotiated per session; unknown type is an error |
| MySQL row binlog | TABLE_MAP: names of table, types; column names only with `FULL` metadata (default `MINIMAL`); match by position | `full` (default) or `minimal`; bitmaps tell absent from NULL; `minimal` can silently diverge | DDL text as `QUERY_EVENT`, re-run on replica | `Rows_query_log_event`, off by default, before its table map | n/a | statement order; `STMT_END_F`; never split across files | refused unless `LOG_EVENT_IGNORABLE_F` |
| MySQL statement binlog | n/a | n/a | DDL text | the statement is the truth | timestamp, `Rand_event` seeds, `Intvar_event` ids; long unsafe list; format deprecated | statement order | as above |
| SQLite session | header: name, column count, key flags; no names or types; match by position; mismatch silently skipped | changeset: full insert and delete, changed columns of update; "undefined" ≠ NULL; key update = delete + insert | none | none | n/a | net effect per key; order within a table undefined | no versioning |
| Delta Lake | full schema in `metaData`; Parquet names; optional stable column ids | files, not rows; CDF has pre- and post-images | `metaData` with full schema replaces the old; `ADD COLUMN ... DEFAULT` forbidden | `commitInfo`, free-form, no SQL in the spec | in-commit timestamp | one file per commit; latest action wins | ignore unknown fields; breaking change needs protocol version or table feature |
| Debezium | names and types in each event's schema | full `before` and `after`; key-only delete; placeholder for unchanged TOAST; key update = delete + create | separate history: DDL text plus structured `tableChanges` | `include.query` (MySQL), no parameters | n/a | statement order; optional BEGIN/END with counts | none found; bad DDL stops the connector |
| Spanner | per-record `column_metadata` (name, type, key flag, position) | changed columns by default, or full new row; old values optional | not watched; default backfills as system transactions | `transaction_tag` only | n/a | records per partition, with counts and sequence | `oneof`; Beam throws on unknown record type |
| CockroachDB | column names in each row | full `after`; `before` with `diff`; delete = key + null | no DDL events; backfill re-emits the whole table | none [I] | n/a | at-least-once, per-key order only | not checked |
| Dolt | snapshots; column tags survive renames | diff of snapshots | in the snapshot; tags | commit message only [I] | n/a | net effect of snapshots | not checked |
| rqlite | statement text | none (statements) | DDL text | the statement is the truth, with typed parameters | rewrites `RANDOM()` and time; silent fallbacks; recurring bugs | statement order | not checked |
| cr-sqlite | table and column name per cell | one cell per change; sentinel for row existence | not in the changes | none | n/a | per cell with versions | unknown table is an error |
| Marmot | column names per value | old and new as name maps | DDL as SQL text; required schema version | DDL text only | rejects SQL replay for this reason | per transaction | designed to fail cleanly |
| Turso `turso_cdc` | table name, no column names; documented wrong decoding of old records | before and after records | rows of `sqlite_schema` | none | n/a | log order, with commit rows | not checked |

## 11. Recommendations for s4ql

Each recommendation names the evidence. The last column of the table above shows how often the alternative went wrong.

1. **The heading before a table's rows holds the table name, the column names in order, each column's type, and the primary-key flags.** Values in the rows that follow are positional against that heading.
   - Every system whose reader keeps its own copy of the schema and gets less than names has a documented silent failure: MySQL with `minimal` images ("silently diverge", section 3), the SQLite changeset ("not an error, but none of the changes ... are applied", section 4), and Turso ("incorrect results when decoding older CDC records", section 8).
   - The systems built most recently for outside readers put names and types next to the values: Spanner in every record, Debezium in every event, CockroachDB, Marmot, and cr-sqlite by name. PostgreSQL sends names, types, and key flags in its Relation message. MySQL added column names as optional metadata so that replicas and external software can read rows when the table differs.
   - The cost is a few bytes per table per file, because one heading covers all the rows of that table that follow it. [I]
2. **Replay checks the heading against the schema it has replayed, and refuses on any difference.** The names are then not only a description but a check that the writer and the reader agree, at every file. This turns the silent failures above into the loud error that PostgreSQL gives ("missing replicated column", section 2) and that Marmot and sqlite-sync give with a schema version or hash (section 8). Replay should not match by name and tolerate differences, as PostgreSQL does, because s4ql's reader must reproduce the writer's state exactly. [I]
3. **Each change file is self-contained.** No heading or other state carries over from an earlier file. Within a file, a heading is repeated after any schema change to its table, as PostgreSQL re-sends a Relation message when the definition changes. PostgreSQL's cross-message schema state needed special care for streamed and aborted transactions (section 2), and Debezium's separate schema history is the source of most of its listed bugs (section 6). [V for the evidence, I for the rule]
4. **A put carries the full row, and there is no "unchanged" marker.** Full images are what made MySQL's default safe and what gh-ost and Debezium require. The "unchanged" value is where PostgreSQL and Debezium needed a special case and a sentinel. NULL is an ordinary value. [V sections 2, 3, 6]
5. **An update of the primary key is a delete of the old key and a put of the new row.** The SQLite session extension, Debezium, and CockroachDB do this. It keeps two kinds of row change, as agreed. [V sections 4, 6]
6. **Schema changes are structured data, never DDL text that replay parses.** Debezium prefers its structured `tableChanges` over the DDL text when it recovers, and still lists DDL parser failures as bugs; rqlite's DDL rewriting is a source of bugs; Delta stores the whole schema as data. [V sections 5, 6, 8] Each schema change should also carry the complete definition of the table after the change, as Debezium's `tableChanges` and Delta's `metaData` do. Replay can then check its own result against it, and a tool can read the definition without replaying from the start. This is an option; see decision 3 in section 12.
7. **`alter table add column` with a default is one schema change, and existing rows are not repeated in the log.** SQLite allows only constant defaults in `ADD COLUMN` (no `CURRENT_TIME`, `CURRENT_DATE`, `CURRENT_TIMESTAMP`, or an expression in parentheses) and does not change the stored rows (https://sqlite.org/lang_altertable.html, section 4). [V] So replay of the schema change gives the same result as SQLite. PostgreSQL and MySQL also leave the rows out of their logs. CockroachDB re-emits every row, which turns one statement into a change file as large as the table. Delta forbids the combination entirely. [V sections 2, 3, 5, 6] The same holds for `drop table`: one record, no row deletes, as in Spanner. [V section 6]
8. **Replay applies results and does not run triggers, cascades, or constraint checks while it applies.** The file already holds every row a cascade or trigger changed. The SQLite session extension re-fired triggers and cascades on apply, which produced conflicts ([sqlite-cache-and-oracle.md](sqlite-cache-and-oracle.md) §2.1.3, E6 and E7). With the net effect, the order of rows across tables is not the order of the statements, so a foreign key checked row by row could fail on a valid commit. [V there, I for s4ql]
9. **An unknown change kind is refused, and the decoder must make that true.** Protocol Buffers says of a `oneof`: if it reads as not set, "it could mean that the oneof has not been set or it has been set to a field in a different version of the oneof. There is no way to tell the difference". The generated code keeps the unknown kind as an unknown field and shows the change as empty. So replay must treat a change with no known kind set as an error, and must not skip it. The same page says that with several members of one `oneof` on the wire, "only the last member seen is used"; a strict reader should refuse that too. [V https://protobuf.dev/programming-guides/proto3/] Beam's Spanner reader and MySQL's applier both refuse unknown kinds by default. [V sections 3, 6]
10. **Records that a reader may ignore are marked as such by their place in the format, not left to guess.** MySQL's single flag bit lets a writer add a new informational event that old readers skip, while every other unknown event is refused. Delta splits the same way between ignorable fields and features that a reader must support. [V sections 3, 5] For s4ql, the statements and their inputs are the ignorable part. Keep them in records apart from the changes, so that "may skip" and "must refuse" are two different places in the file. [I]
11. **Statement evidence records each value that the engine actually used, not a rewritten statement.** For a statement: the SQL text, the parameters, the wall-clock time it read, and the random values or seed it used. MySQL records the time and the `RAND()` seeds next to the statement for this purpose. rqlite's rewriting of the SQL text missed defaults, triggers, and `ORDER BY`, and had bugs into 2026. [V sections 3, 8] For s4ql the risk is lower because the evidence is never replayed as truth, but evidence that cannot reproduce the commit is less useful. [I]
12. **Optional: a count of changes per file, as a cross-check.** Spanner's `number_of_records_in_transaction` and Debezium's `event_count` let a reader know that it has the whole transaction. The encoding note's trailer already proposes this. [V sections 6; change-file-encodings.md section 11]

## 12. Decisions still open

1. **What the heading holds.** Options seen: the table name only (no system that ships rows to an independent reader does this); name, column count, and key flags (SQLite changeset); name and types, with names optional (MySQL); name, column names, types, and key flags (PostgreSQL, Spanner, Debezium). **Answer: the table name, the column names in order, their types, and the primary-key flags**, checked against the replayed schema (recommendations 1 and 2). Types should be the declared `STRICT` type names. Whether to also add `NOT NULL`, defaults, and other constraints can wait: they belong to the table definition in the schema change records, not to every heading. [I]
2. **Column identity across renames.** Options: names, with an explicit rename record (PostgreSQL, Debezium's `previousId`); stable numeric ids (Delta's column mapping, Dolt's tags). Dolt is considering removing its tags because they broke history independence and merges, and Delta's renames broke change data readers for several releases. With a single writer and a log replayed in order, names plus an explicit rename record are enough. [V sections 5, 7; I for s4ql]
3. **What a schema change record holds.** Options: the operation only (for example "add column c, type, default"); the operation plus the complete table definition after it (Debezium's `tableChanges`); the complete definition only (Delta's `metaData`). The second gives replay a check and gives tools a readable definition at every change.
4. **Before images.** Options: none (s4ql as agreed, the SQLite patchset); the key only (PostgreSQL's default delete); full old rows (Delta's change data feed, Debezium, Spanner's `OLD_AND_NEW_VALUES`, CockroachDB's `diff`). Replay does not need them. A future change feed for applications, or an audit tool, would. Adding them later is a new optional field, so this can wait. [I]
5. **Net effect when one commit creates, drops, or alters a table and also changes its rows.** Options: order the file as schema changes first, then rows (a commit that drops and recreates a table with the same name then needs care); or keep the net effect per table between schema changes, in commit order, with a heading after each schema change. The second matches recommendation 3. Delta allows at most one `metaData` action per commit, which avoids the question. [V section 5]
6. **Grouping of rows.** Options: one heading, then many rows (SQLite, MySQL); a heading inside every record (Spanner, Debezium). Grouping is smaller. A heading per record makes each record readable alone. With small records in a stream, grouping fits better. [I]
7. **Linking statements to changes.** As agreed, they are not linked. Others: MySQL links by position (the query event comes right before its table map), and Debezium copies the query into each row event. Nothing found here argues for reopening this. [V sections 3, 6]
8. **How "ignorable" is marked.** Options: a flag on each record (MySQL); a version and a list of required features (Delta); a separate field for evidence that the format declares skippable (recommendation 10). [V sections 3, 5]

## Not verified

- Whether MySQL's `Rows_query_log_event` text has the parameter values of a prepared statement substituted.
- Whether one key can appear twice in one transaction in Spanner, Debezium, or CockroachDB.
- Whether CockroachDB keeps statement text anywhere in a changefeed.
- Whether SQLite's `'now'` keeps one value for a whole statement. This matters for recommendation 11. https://sqlite.org/deterministic.html was not re-read in this run.
- Dolt's merge behaviour beyond the code cited.
- The Debezium, Spanner, CockroachDB, Dolt, and SQLite-system claims were read by the helper agent. I re-checked only Spanner's `ColumnMetadata` message and CockroachDB's backfill text.
