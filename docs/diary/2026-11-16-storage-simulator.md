# Diary: A simulator of the storage design on the site

Markus asked for a page on the project site where people can play with the storage design and inject faults. The page is `/site/simulator/index.html`, served at https://www.s4ql.com/simulator/, and the model behind it is `/site/simulator/model.js`, an ES module that the page imports and that Node runs headless in `/site/simulator/model.test.mjs`.

## Step 1: Requirements

**Author:** simulator-builder

### Prompt Context

**Verbatim prompt:** Build an interactive simulator of s4ql's storage design, in JavaScript, as a new page of the project site. Markus asked: "is it possible to add a simulator written in Javascript to the page? So people can play with the storage design, inject faults etc. Maybe build it on a separate page." [...] `site/simulator/index.html` (served at https://www.s4ql.com/simulator/), plus the model as a separate ES module `site/simulator/model.js` that the page imports and that Node can run headless. Vanilla JS, no dependencies, no build step [...]. The model, deterministic and driven by one seed: a simulated S3 with strong consistency, compare-and-swap on `manifest.json` (ETag-style), and create-if-absent for commit objects [...]. Two or three instances [...]. Virtual time only. [...] Faults the user can inject at any moment [...]. Also a buggify switch [...]. An invariant checker that runs after every step [...]. Add a "planted bug" switch [...]. The page: same world as `site/index.html` [...]. Verification: `node site/simulator/model.test.mjs` [...] over at least 2000 seeds with random workloads, random faults, and buggify on, no invariant is violated; with the planted bug on, the checker finds a violation within a bounded number of seeds; scenario tests for the stall case [...], the dropped-response case [...], and catch-up after a crash. [...] Open the page in a browser [...] at 1440x900 and 390x844 [...]. Commit on your branch with clear messages. Do not push and do not merge.

(The brief is abridged at the "[...]" marks; the full text is in the lead's conversation.)

**Interpretation:** Write a discrete-event model of the lease, the log, the cache, and the simulated store in plain JavaScript, faithful to `/docs/spec.md`, `/internal/core/README.md`, and the Go code in `/internal/core` where the documents are silent. Put an invariant checker and planted bugs in it, test it headless with Node's built-in runner over thousands of seeds, and build a page in the visual language of `/site/index.html` that drives it.

**Inferred intent:** Give visitors a way to see why the design is safe: what a stall, a lost answer, a partition, or a wrong clock does, and that the checker catches a real bug when one is planted. The page must not claim more than the design does.

### What I did

Read `/AGENTS.md`, `/docs/spec.md`, `/internal/core/README.md`, `/internal/format/README.md`, `/internal/format/testdata/manifest-v0-lease.json`, `/site/index.html`, and in `/internal/core` the files `bugs.go`, `core.go` (options, `margin`, `refusalLocked`), `lease.go` (all of it), `read.go`, `tx.go` (`Commit`, `upload`, `publish`, `inLog`, `commitCache`), and `catchup.go`. Read the list of phrases at https://claudisms.ai/claudisms.md.

### Why

The model has to follow the Go code's decisions where the spec is silent: when a spare starts its wait, what a refused swap leads to, when a commit is pushed and when the lease task resolves it, and which checks a read and a commit make.

### What worked

The doc comments in `lease.go` and `tx.go` give every decision of the lease and the commit in a form that maps one to one to generator functions.

### What didn't work

Nothing yet.

### What I learned

- An instance whose lease ran out by its clocks still believes it is the holder (`holder` stays true). Reads and `Begin` refuse, but an open transaction goes on to its swap, which S3 refuses. That is the stall case.
- A request that fails before it is sent is not a refusal to the core. Only "changed", "not found", "exists", and a checksum error are, so a failed swap is of unknown outcome too.
- A spare's wait counts from after its read, and the holder's lease counts from when its renewal was first sent. A watcher waits the holder's duration plus a margin of `max(2 s, duration/10)`.

### What was tricky

Several words of the design are on the list of phrases to avoid, such as "hold", "carry", and "settled". Literal holding of a lease is fine by the list's own note. "Settled" is banned in every form, so the page says "resolved".

### What warrants review

Nothing built yet.

### Future work

None yet.
