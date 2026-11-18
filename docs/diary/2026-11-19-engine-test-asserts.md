# Diary: Make the engine's tests assert what they count

A reviewer of the last round of changes to the SQL engine of slice 1 found it sound to merge, and found places where its tests assert less than they could, each shown by a planted fault. This work closes them, in tests only, on the branch `engine-test-asserts`.

## Step 1: The brief, and a plan

**Author:** engine-test-asserts builder

### Prompt Context

**Verbatim prompt:** (excerpt from the lead's brief) "A reviewer of the engine's last round of changes found it sound to merge, and found places where the tests assert less than they could. Each was shown by planting a fault. Close them. This is a small task in tests only; do not change production code, and do not widen it. [...] 1. **Nothing asserts that most statements of the simulation reach a transaction.** [...] Add a ceiling on the share of statements that are refused for want of the lease, per workload, with room above what is seen. Show that the planted change now fails, then revert it. 2. **The minimums barely notice the store's faults being switched off.** [...] Count the faults that the simulated store actually injects, by kind, and assert a minimum for each kind per workload [...] 3. **`keepOpen` and `keepRead` share a signature.** [...] Assert that `keepOpen` is found by its own violation in at least some minimum number of the seeds [...] 4. **Excused texts in the engine's tests are counted and not asserted.** [...] In the full run with default flags [...] assert the counts exactly [...] 5. **The `excused` check in `internal/sql/stmt` asserts only a sum.** [...] Pin each outcome on its own. 6. **The simulation does not check a refused text against SQLite.** [...] Add the same check to the simulation's verification if it fits there simply [...] 7. **Two texts drifted from the code.** [...]"
**Interpretation:** Seven small changes to tests and two texts. The simulation's tally gains a ceiling on "failed: no lease" and minimums for the faults of the store by kind, counted from the markers in the trace; the planted-bug test counts the seeds that find `keepOpen` by "no instance runs a write"; the differential test pins its counts of excused texts in a full run; the `excused` check of `stmt` pins each of its two outcomes; the simulation's verification prepares each text that the engine refuses as not supported in SQLite on an empty database; and the README of `stmt`, the README of the engine, and `docs/todo.md` say what the code does.
**Inferred intent:** Each check fails for the fault that it is meant to catch, with a planted fault as proof where the brief asks for one.

### What I did

Read `AGENTS.md`, `/internal/engine/simulation_test.go`, `/internal/engine/differential_test.go`, the pins of `/internal/sql/parser/parser_test.go`, the `excused` check of `/internal/sql/stmt/check_test.go`, the markers of the store's faults in `/internal/core/workloads_test.go` and `/internal/store/simstore/client_test.go`, and the texts of the trace in `/internal/store/simstore/client.go`.

### Why

The markers that the core's workload test counts are the texts that `simstore` writes into the trace when it injects a fault, and the engine's simulation keeps the same trace in `simulation.lines`, so the faults can be counted there without any change to `simstore`.

### What worked

Nothing built yet.

### What didn't work

Nothing tried yet.

### What I learned

`simstore.AllFaults` switches on ten faults: lost requests and responses, hangs, conflicts, slow ways, broken reads, cut-offs, outages, late writes, and writes applied after a crash. It does not switch on `Long`. A write applied after its sender crashed needs a crash, which the hammer workload and the runs under the address sanitizer do not have.

### What was tricky

Nothing yet.

### What warrants review

Nothing yet.

### Future work

None yet.
