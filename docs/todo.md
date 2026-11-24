# To do

A builder records the decisions and notes of its step in the file of its slice, `docs/todo/slice-NN.md`, under a heading for its step; the first step of a slice creates the file. An open item or a design question goes in the file of its area. An item is deleted when it is built. So two branches that work on different slices never write to the same file.

A working list of things that are agreed but not built, and of design questions that are still open. The agreed rules are in [spec.md](spec.md), and the SQL slices are in [roadmap.md](roadmap.md).

Each item is marked agreed or open. An agreed item carries the details that a builder needs who was not in the conversation. When an item is built, it is deleted from this list, and its reasoning moves next to the code as tests and doc comments. The code is then the source of truth for how it was done.

The list is in the directory [todo](todo), one file per area and one per slice:

- [sql-front-end.md](todo/sql-front-end.md): the tokenizer, the parser, and the pass after it, what is open in them, and the known differences from SQLite.
- [driver.md](todo/driver.md): the public package and the `database/sql` driver.
- [storage-core.md](todo/storage-core.md): what is open or not built in the storage core.
- [storage-core-tests.md](todo/storage-core-tests.md): what the tests of the storage core and the simulation harness do not show yet.
- [ci.md](todo/ci.md): faults seen in CI's jobs that are not fixed.
- [performance.md](todo/performance.md): performance measurement.
- [wal-index.md](todo/wal-index.md): the memory-mapped wal-index.
- [storage-formats.md](todo/storage-formats.md): the manifest and the commit object.
- [s3-client.md](todo/s3-client.md): the S3 client and S3's checksums.
- [vendoring.md](todo/vendoring.md): a module that vendors s4ql.
- [slice-02.md](todo/slice-02.md): slice 2 of the roadmap, parameters.
- [slice-03.md](todo/slice-03.md): slice 3 of the roadmap, transactions.
- [slice-04.md](todo/slice-04.md): slice 4 of the roadmap, the other types.
- [slice-05.md](todo/slice-05.md): slice 5 of the roadmap, several columns.
- [slice-06.md](todo/slice-06.md): slice 6 of the roadmap, `null`.
- [slice-07.md](todo/slice-07.md): slice 7 of the roadmap, `where` on `select`.
- [slice-08.md](todo/slice-08.md): slice 8 of the roadmap, `update` and `delete`.
- [slice-09.md](todo/slice-09.md): slice 9 of the roadmap, indexes.
- [slice-10.md](todo/slice-10.md): slice 10 of the roadmap, `if not exists` and `if exists`.
- [slice-11.md](todo/slice-11.md): slice 11 of the roadmap, `default` with a literal.
- [slice-12.md](todo/slice-12.md): slice 12 of the roadmap, a value on the left of a comparison.
- [slice-13.md](todo/slice-13.md): slice 13 of the roadmap, `order by` and `limit` on `select`.
- [slice-14.md](todo/slice-14.md): slice 14 of the roadmap, `returning` on `insert`, `update`, and `delete`.
- [slice-15.md](todo/slice-15.md): slice 15 of the roadmap, a scalar subquery in a `where`.
- [generated-workload.md](todo/generated-workload.md): the profiles and the random streams of the generated checks.
