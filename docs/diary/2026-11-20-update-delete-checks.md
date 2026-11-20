# Diary: Slice 8 of the roadmap in the generated checks, `update` and `delete`

The engine and the driver run the `update` and `delete` of slice 8 since the branch `update-delete-engine` (pull request #91, in review), with hand-written comparisons with SQLite, both ways of reading rows. `stmttest.Sequence`, which the engine's differential tests, the simulation, and `drivercheck`'s generated comparison with mattn's driver draw from, wrote no update or delete beyond the one text of `stmttest.Others`. This task makes it write both, checks every one with a WHERE both ways, counts what the workload reaches, plants a bug of the new code in the simulation, and adds a fuzz target. The branch is `update-delete-checks`, from `origin/update-delete-engine` at `655bca7`.

## Step 1: The requirements

**Author:** update-delete-checks builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "Turn on `Generator.Changes` or the hooks `Generator.Update`/`Generator.Delete` in `stmttest.Sequence`, so that the generated sequences of the differential tests, the simulation, and the driver check contain updates and deletes, valid and invalid, with and without `where`, with literals, `null`, and arguments, with key changes that succeed and that collide, in and outside transactions, with rollbacks. [...] Every update and delete with a `where` must also be checked both ways [...] Choose the device [...] and give the reason. [...] `checkLength` and `longCondition` [...] cover the `set` literals [...] `reach` and the tallies [...] fail if a kind is never reached. [...] drivercheck compares `RowsAffected` through the pool only for inserts: do it for updates and deletes, and read the tables back afterwards. [...] at least one planted bug in the new code [...] A fuzz target for the new statements through the engine and through core [...] If a generated check finds a real difference from SQLite or a lost or wrong write, that is the point of this step: fix it if the fix is small and clearly right [...] Never weaken a comparison to make a test pass."
**Interpretation:** Hooks `Sequence.update` and `Sequence.delete` that draw from what the inserts wrote; a second engine in the pair of the engine's tests that reads the other way and runs every call; the access of an update and a delete checked against the rule; the length rules for SET; counted cases of slice 8 with least numbers in each test; `RowsAffected` and the tables in `drivercheck`; a planted bug of a change of the key; `FuzzChanges` and `FuzzChanges_core`.
**Inferred intent:** The generated checks, which found most bugs of the earlier slices, cover slice 8 with the same trust before mutation testing of its code.

### What I did

Read the roadmap's slice 8, the diaries of the engine step of slice 8 and of the generated checks of slice 7, and the decisions in `docs/todo.md`.

### Why

The pattern of slice 7 is the pattern here.

### What worked

Nothing to run yet.

### What didn't work

Nothing yet.

### What I learned

`Sequence` does not set the pools of `UpdateTable`, `UpdateColumn`, and `DeleteTable`, so a hook must give the names, or the pools must be set.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

- The sequence's hooks, the checks, the planted bug, and the fuzz targets: the next steps.
