# Diary: a bucket that persists between reloads of the simulator

The simulator on the site, `internal/simulator` served by `site/simulator`, starts over on every reload. Markus: "I'd like the data to persist between refreshes (just like S3 would), with a reset button available." This diary is of the builder that keeps the simulated bucket in the browser.

## Step 1: The brief

**Author:** builder (simulator persistent bucket)

### Prompt Context

**Verbatim prompt:** (the brief from the lead, abridged to its requirements)

> What persists is what S3 would hold: the bucket's objects (the manifest and the commit objects). What does not persist is everything a machine would lose: the instances' caches, their processes, the faults that are switched on, the clocks' shifts. After a reload, `a` starts on the existing bucket and has to get its data from it, as a real app on a new machine would, and `b` stands by. Do not hide or shortcut what the real code does on an existing bucket.
>
> - No changes to the Go code outside the simulator. One simulator, one code path. No planted bugs. The page stays as it is designed. Few words.
> - The page keeps the bucket in IndexedDB. On start, the page gives the stored objects to the engine with `start`, and the engine puts them in the simulated store before any instance starts. The engine reports every change to the bucket with the answers, also when the answer to the instance is lost or hangs. Each answer's changes go into one IndexedDB transaction, in order.
> - The checker starts from the bucket it is given. The keys of the counter must not repeat keys from before the reload. Fake time continues across a reload; the stop after 24 fake hours counts the session's own age. Check whether a wall clock that jumps back changes behaviour.
> - Reset in the playback bar, styled like the others, with the neutral blip. No confirmation. Works while playing, paused, and after the stop. The stop message says what is now true.
> - Store a version string that the engine reports, from the format versions and a version of the simulator's stored shape. On a mismatch, a refusal by the engine, or a stored log that the checker finds wrong, start fresh and say so in one log line.
> - Growth: measure, and choose a cap by numbers. At the cap, the least surprising thing.
> - Step 0: how the simulated store can be seeded and observed without changing it; what an instance does on an existing bucket with an empty cache, for 100 to 15,000 commit objects, natively and in Chromium; the size of a commit object and the manifest; the time to load N objects from IndexedDB.
>
> A later message from the lead, from Markus: "Just use one bucket but different database prefixes per tab?" One stored bucket, one database prefix per tab; `start` takes the prefix. A tab takes the prefix in `sessionStorage` if its Web Lock is free, else the most recently used stored prefix whose lock is free, else a new one. It deletes older stored prefixes whose locks are free. Reset empties only its own prefix. Without Web Locks: one fixed prefix. Without IndexedDB: in memory.

**Interpretation:** the engine takes a stored bucket (objects, the counter, and the time of the history) and a prefix with `start`, seeds the simulated store through its public API, and reports every applied write; the page keeps the bucket in IndexedDB, per prefix, under Web Locks.

**Inferred intent:** the simulator behaves like an application on S3: the data outlives the machine, and what the code does to get it back is visible.

### What I did

Read `AGENTS.md`, the README of `internal/simulator`, the diaries of the live engine, the SQL, and the page, and the code of `run.go`, `live.go`, `check.go`, `trace.go`, `request.go`, and `simstore`.

### Why

What the reading gave:

- `simstore` has no way to put an object in place, and its faults are fixed at `New`. The only public way in is a write through a `Client`. A client with a nil sender never crashes. `Store.Object` reads an object directly, without faults, so the engine can confirm each seeded object and write it again until it is there.
- The store traces `applied` after it releases its mutex, and the engine already calls `Store.Object` at that line to decode the object. That is the point at which a change is in the store, whatever happens to the answer afterwards.
- `format`'s version constants are not exported. The bytes of a fixed manifest and a fixed commit object carry them, so a hash of those bytes changes with the versions and the encodings.
- The core writes `Lease.Renewed` and never reads it back for a decision (`internal/core/lease.go`: two writes, no read).
