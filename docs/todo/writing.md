# Writing

What is agreed and not built about the statements that write, and what is open. The short statement is in [the roadmap](../roadmap.md#writing). The options behind the decisions are in [the feature map](../research/feature-map.md), core gap 4.

- **Agreed with Markus on 2026-11-24, not built: upserts.** Markus chose option (b) of the feature map, `on conflict ... do update` and `do nothing` only: "b sounds like the least surprising right choice here."
  - An `insert` without a conflict clause fails on a key that exists and changes nothing, as it does today. That is SQLite's `abort`.
  - `insert ... on conflict (key) do nothing` and `insert ... on conflict (key) do update set ...` are built when they are next.
  - `replace into` and the spellings `insert or abort`, `insert or fail`, `insert or rollback`, `insert or ignore`, and `insert or replace` stay rejected. A `replace` deletes the old row and inserts a new one, so a column that the statement does not name falls back to its default. A `do update` changes only the columns that its `set` names.
  - For the builder: the storage core already has an upsert, which inserts a row or replaces the row of its key, and `update` uses it (`conn.upsert` in `internal/core/cache.go`, `Tx.Upsert`).
  - glue's session store, scs, writes with `REPLACE INTO`. That is a known conflict of the milestone, listed in [the roadmap](../roadmap.md#milestones).
  - The piece of work has a file of its own, [upserts.md](upserts.md), with the decisions of each step. The statement pass reads both forms since 2026-11-26, and the engine refuses them until its step.
- **Decided by the lead on 2026-11-24: several rows per `insert`, and `insert ... select`, are details,** built when they are next. Today an insert writes exactly one row, and the operators refuse a plan of an insert of several rows or of none with an internal error ([the engine's README](../../internal/engine/README.md#the-operators)).
