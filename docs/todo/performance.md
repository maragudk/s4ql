# Performance measurement

- **Agreed on 2026-11-20, not built: performance measurement.** It has two kinds.
  - Counts, which are exact: the rows read from the cache per query, the requests and bytes to S3 per commit, and allocations. They are pinned in tests and fail on a regression, and they never depend on timing. The first are built with slice 9 of the [roadmap](../roadmap.md): `TestEngine_rowsRead` pins the rows read from the cache for a point lookup, an equality, an equality with a range, and goqite's receive without its `order by`, with and without an index, and the README of the engine has the table. The requests and bytes to S3 and the allocations are not built.
  - Timings, which are noisy: Go benchmarks of statements, commits, and replay. Each runs beside the same work on plain SQLite on the same machine, and is reported as the ratio to SQLite. CI records them on every merge to `main`, and a chart shows them over time. This is a piece of work of its own, after slice 9.
  - Not decided: where the results are kept, and where the chart lives.
