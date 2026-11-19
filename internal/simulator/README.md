# The engine of the simulator

`internal/simulator` runs the storage core of s4ql under the simulation, one request at a time, and returns what happened as a structured trace. A request is a seed, settings, and a schedule of actions and faults at fake times.

It exists for the simulator on the project's site, which is to show how the current code reacts to faults of its environment. The page must therefore run the code, and `site/simulator/` runs this engine in place of the JavaScript model of the design that it ran before. This file is the specification for the page: the request, the response, the events, the state at a time, the invariants, and how a page loads the engine.

- The package builds only with the tag `s4ql_browser`. With the tag it builds natively and for a browser, with `GOOS=js GOARCH=wasm`, where it runs on the SQLite binding of [internal/sqlite/browser](../sqlite/browser/README.md).
- `internal/core`, `internal/sim`, and `internal/store/simstore` are used unchanged, through their exported API.
- `engine/` holds the program for a browser, its Worker, the module that a page imports, and a demo page. `serve/` builds and serves them.

## How a run works

`Engine.Handle` takes a request as JSON and returns the response as JSON. One request is one run, and the run goes to its end before the engine answers:

1. The engine starts a scheduler of `internal/sim` with the seed, in a `testing/synctest` bubble of its own, and a simulated store of `simstore`.
2. It starts the instances, named `a`, `b`, `c`, and so on. A process runs each instance: a simulated machine with its own clocks, its cache in a file, its callers, and its client of the store. The first process of an instance has the name of the instance, and a restart starts the next: `a2`, `a3`, and so on.
3. The first task of the run does the actions of the schedule at their times, and then sleeps until the end.
4. At the end it runs the last checks of the invariants and stops recording.

The trace holds the events before `end`, and those at `end` that happen before the recording stops. After that the engine lets the run finish, and none of it is in the trace: every process that has not crashed closes its database, every monotonic clock that stands still starts again, because the sleeps of a process stand still with its clock, and the requests that are on their way end.

### Determinism

- The same request gives the same response, byte for byte, every time and on every engine.
- Two requests that differ only in their schedules give the same events before a time T, if the schedules agree before T: the actions with `at` below T are the same, at the same indexes. "Before" is strict. Events at exactly T can differ.
- Two requests that differ only in `end` give the same events before the earlier end.

The request is the whole input of a run, so a page can share a run as its request.

### A fault while the run plays

The engine has no live run. A page plays a trace back at its own speed. When the visitor acts at the fake time T, the page adds the action to the schedule at T and asks for the run again. The events before T are the same, so the visitor sees the run go on from where it was.

Events at exactly T can change. A page that must keep everything it has shown gives the new action a time after the last event shown.

A live loop, in which the run waits for the visitor, does not work in a bubble. A goroutine of the bubble that waits for the visitor is durably blocked, so fake time would run on without the visitor. The JavaScript callback that brings the visitor's message runs outside the bubble.

## The request

Every time and duration is a number of milliseconds of fake time, which may have a fraction. Every time counts from the start of the run. A field that is missing has its default, also inside `faults` and `workload`. A member that is no field is an error, and so is a value outside its limits. The error lists every problem of the request.

| Field | Default | Limits | Meaning |
| --- | --- | --- | --- |
| `seed` | 1 | an unsigned 64-bit integer | The seed of the scheduler. Every random choice of the run comes from it. |
| `end` | 120000 | above 0, at most 1800000 | The time at which the run ends. |
| `instances` | 3 | 1 to 5 | The number of instances, which stays the same for the run. |
| `leases` | `[30000]` | at least one, each 1000 to 600000 | The durations of the leases. Instance number i, from 0, gets `leases[i mod length]`. A restarted instance keeps its duration. A list that is given replaces the default. |
| `latency` | 50 | 0 to 60000 | The longest time that a request takes to the store, and again back. Each way takes a time from 0 to `latency`, from the seed. |
| `faults` | all 0 | see [Faults](#faults) | The faults that happen at random. |
| `buggify` | false | | Switches buggify of the storage core on for the run. Each point is then on with the probability 0.25, and a point that is on fires with the probability 0.25 each time the code reaches it. |
| `workload` | see [Workload](#workload) | | The callers that each instance runs. |
| `schedule` | `[]` | at most 1000 actions | The actions, each at its time: see [Schedule](#schedule). |
| `lines` | false | | Asks for the text trace of the scheduler in the response. |

The other options of the core keep their defaults. An instance renews its lease after two thirds of its duration. A request for the manifest times out after 5 s, and an upload or a download of a commit object after 5 min. An instance that does not hold the lease reads the manifest every 5 s. SQLite waits 1 s for a lock on the cache.

The scheduler delays half of all task starts, yields, and releases by up to 50 ms, and fails a run after 5,000,000 steps.

A page that goes through `simulator.js` sends the seed as a JavaScript number, so a seed above 2^53 - 1 loses precision.

### Faults

`faults` holds the faults that happen at random, from the seed. A probability is from 0 to 1 and is drawn for each request; 0 switches the fault off. A duration is from 0 to 1800000 and is an upper bound: the fault takes a time from 0 to it.

All but `damaged` are the faults of `simstore`, and the doc comment of `Faults` in [simstore.go](../store/simstore/simstore.go) describes each in full. The column "All" has the values of `simstore.AllFaults`, which the tests of the core run with, as a preset for a page.

| Field | All | Meaning |
| --- | --- | --- |
| `lostRequest` | 0.02 | A request is lost on its way to the store, and the caller gets an error. |
| `lostResponse` | 0.02 | The response is lost on its way back. The store has handled the request, and the caller gets an error. |
| `hang` | 0.2 | The caller of a request that a fault made fail gets no answer at all, and waits until its timeout. |
| `slow` | 0.02 | One way of a request takes up to `slowBy` longer. |
| `slowBy` | 45000 | |
| `brokenRead` | 0.02 | The content of a read breaks off: at its start, in the middle, or after its last byte. |
| `cutOff` | 0.005 | A request cuts its process off from the store, for up to `cutOffFor`. No request of the process arrives, and no response reaches it. |
| `cutOffFor` | 40000 | |
| `outage` | 0.002 | A request makes the store unreachable for every process, for up to `outageFor`. |
| `outageFor` | 20000 | |
| `conflict` | 0.1 | The store refuses a swap from the right version while another write to the same key is in flight, as S3 may. |
| `late` | 0.5 | A write whose caller no longer waits for it is still applied, up to `lateBy` later: a write whose caller gave up before it arrived, and a write that was lost, which is then stuck in the network. Otherwise such a write is never applied. |
| `afterCrash` | 0.5 | A write whose sender crashed before it arrived is still applied, up to `lateBy` later. Otherwise it is never applied. |
| `lateBy` | 60000 | |
| `long` | 0 | A write that `late` or `afterCrash` holds back waits up to `longBy`, in place of `lateBy`. |
| `longBy` | 0 | |
| `damaged` | 0 | A download of a commit object reaches the instance with one byte changed. The engine adds this fault, in its client of the store. |

The preset as JSON:

```json
{"lostRequest": 0.02, "lostResponse": 0.02, "hang": 0.2, "conflict": 0.1, "slow": 0.02, "slowBy": 45000,
 "brokenRead": 0.02, "cutOff": 0.005, "cutOffFor": 40000, "outage": 0.002, "outageFor": 20000,
 "late": 0.5, "afterCrash": 0.5, "lateBy": 60000}
```

The store takes its faults when the run starts. The action `calm` switches them off for the rest of the run, and nothing switches them on again.

### Workload

Each process runs callers, as an application would.

| Field | Default | Limits | Meaning |
| --- | --- | --- | --- |
| `on` | true | | Whether the callers make calls from the start. The action `workload` switches them. |
| `callers` | 2 | 0 to 8 | The callers of each process. |
| `think` | 5000 | 0 to 1800000 | The longest pause of a caller before a call. |
| `writes` | 40 | 0 to 100 | The share of calls that are writes, in percent. The others are reads. |
| `timeout` | 30000 | above 0, at most 1800000 | The timeout of each call, on the monotonic clock of its process. |

A caller repeats two steps. It pauses for 1 ms and a time from 0 to `think`, from the seed. Then it makes one call, a write or a read by `writes`. While the workload is off it makes no call, and looks again after a second.

- A write is one write transaction. It creates the table `t`, with the one integer column `k` as its primary key, if the database does not have it. It inserts one row and commits. The key of the row is the next value of a counter of the run, which starts at 1. Every write takes a value when it starts, also one that is then refused, so a key belongs to one call, and the keys tell the transactions apart.
- A read is one read transaction that scans `t`. It reports the keys that it saw and its position: the name of the last commit object in the state that it saw.

The actions `write` and `read` make the same calls, with the caller `visitor`.

### Schedule

An action has the time `at`, from 0 to before `end`, the name `do`, and the parameters of its kind. Actions at the same time happen in the order of the schedule. `instance` is the name of an instance of the run.

| `do` | Parameters | Effect |
| --- | --- | --- |
| `write` | `instance` | One write on the instance, by the caller `visitor`, whether the workload is on or off. The call starts in a task of the process, up to 50 ms after `at`. |
| `read` | `instance` | One read, in the same way. |
| `crash` | `instance` | The process crashes. None of its tasks runs again. What it has sent is still on its way, and `afterCrash` decides whether a write that arrives later is applied. |
| `restart` | `instance` | A new process of the instance starts, on a copy of the cache that the crashed process left: the database file and its write-ahead log. Its wall clock reads the true time, and its monotonic clock zero. It has a client of its own, so a cut-off of the crashed process does not hold for it. |
| `close` | `instance` | The process closes its database, as an application that shuts down does. The core stops, waits for the calls that still run, and releases the lease, within 5 min on the clock of the process. Calls end with `stopped` from then on. |
| `open` | `instance` | The process opens its database again: after a `close`, or when it could not be opened, or when its process closed it after a stop and could not open it again. The database gets a new holder ID. |
| `cutOff` | `instance`, `for` | The process is cut off from the store for `for`, which is above 0 and at most 1800000. No request of it arrives, and no response reaches it. |
| `outage` | `for` | The store is unreachable for every process for `for`, with the same limits. |
| `calm` | | The store makes no new random fault from now on. What a fault has started goes on to its end: a cut-off, an outage, a slow request, and a write that is stuck or held back. The latency stays. Damaged downloads end too. Armed faults, buggify, and later actions are not calmed. |
| `shiftWall` | `instance`, `by` | The wall clock of the process shifts by `by`, forward or, if negative, backward. `by` is not 0, and at most 86400000 either way. |
| `stopWall` | `instance` | The wall clock stands still. |
| `startWall` | `instance` | The wall clock goes on from where it stood, so it stays behind by the time it stood still. |
| `stopMonotonic` | `instance` | The monotonic clock stands still, and with it every sleep and timeout of the process: the pauses of its callers, the renewals of its lease, and the timeouts of its calls and requests. |
| `startMonotonic` | `instance` | The monotonic clock goes on from where it stood. |
| `loseAnswer` | `request`, optional `instance` | Arms a fault. The next matching request is sent, the store handles it, and its answer is lost on the way to the caller, which gets an error. |
| `failRequest` | `request`, optional `instance` | Arms a fault. The next matching request fails before it is sent. |
| `workload` | `on` | Switches the callers on or off. A missing `on` is false. |

An action that cannot be done is ignored. Its event says so, with the reason:

- `write`, `read`, `crash`, `close`, `open`, `cutOff`, and the five clock actions are ignored when the process of the instance has crashed.
- `restart` is ignored when the process has not crashed.
- `close` is ignored when the database is not open.
- `open` is ignored while the database is open, also while a database that has stopped waits for its process to open it again.

Where the actions come from:

- `crash` and the clock actions are those of `sim`. Buggify is `sim`'s too.
- `restart` starts a new instance of `sim` on the copied files. `close` and `open` call the core as an application does.
- `cutOff`, `outage`, and `calm` are those of `simstore`.
- `loseAnswer`, `failRequest`, and the random `damaged` are the engine's own, in the client that wraps the client of `simstore`. They are faults of the network that the contract of the store allows.

`loseAnswer` and `failRequest` are one-shot. An armed fault waits, without a time limit, for the first request that matches, and is then used up. A request matches if its purpose is `request`, or `request` is `any`, and if it comes from `instance`, where one is given. `request` is one of `any`, `read`, `take`, `commit`, `renew`, `release`, `upload`, and `download`: see [the purposes](#request). One request trips at most one fault, the one armed first. The caller gets an error that does not say whether the store applied the request, as after a real loss.

## The response

| Field | Meaning |
| --- | --- |
| `error` | Why the request was refused. It is the only field then. |
| `request` | The request, with the default of every field that it left out. |
| `events` | The trace: the events in the order in which they happened, so `t` never decreases. |
| `summary` | Counts of what happened: see below. |
| `hash` | The SHA-256, in hexadecimal, of the text trace of the scheduler and its next random number, as the tests of the core compute it. Two runs with the same hash ran the same way, step for step. It covers the whole run, also what happens after `end`. |
| `steps` | The steps of the scheduler in the whole run. Each ran one task from one wait to its next. |
| `lines` | The text trace of the scheduler, if the request asked for it: a line for each step, and the lines that the instances and the store add. |
| `schedulerError` | The failure of the scheduler itself, such as a run that exceeded its steps. |

The summary:

| Field | Meaning |
| --- | --- |
| `calls` | The calls that ended, counted by `op` and `what`, such as `"write committed"` and `"read served"`. |
| `requests` | The events of the kind `request`, counted by `what`. A request counts once under each of its events: `sent`, `applied`, `answered`, and so on. |
| `log` | The number of commit objects in the log at `end`, the same as the log of the state at `end`. |
| `violations` | The events of the kind `check`. |
| `excused` | Those of them that are excused. |

## The events

Every event has `t`, its fake time, and `kind`. Most have `what`, which says what happened in a word. A field that does not apply is left out, and so is a number that is 0, a boolean that is false, and an empty text. `wall`, `monotonic`, and `action` are the exception: they are there when they apply, also at 0.

| Field | Meaning |
| --- | --- |
| `instance` | The name of the instance, which stays the same across a restart. |
| `process` | The name of the process that runs it, such as `a2`. |
| `error` | The error, as text. |
| `text` | What happened, in a sentence of the store or the engine. |

The names of commit objects are 64 hexadecimal digits. The name of the empty log, the head of a new database, is 64 zeros. The manifest has the key `db/manifest.json`, and a commit object the key `db/commits/`, the first two digits of its name, a slash, and its name.

### process

The life of a process and of its database.

| `what` | Fields | Meaning |
| --- | --- | --- |
| `started` | `wall`, `monotonic` | A process starts: at the start of the run, and at a `restart`. |
| `opened` | `holder` | It opened its database. `holder` is the ID under which this database holds the lease. Every open gives a new one. |
| `open_failed` | `error` | It could not open its database. The core opens only local files here, so this is a local failure. |
| `stopped` | `error` | The core has stopped for good, with the cause. Each process checks this every 2 s on its monotonic clock, as an application would, then closes the database and opens it again. |
| `closed` | `error`, if the close failed | The close of the database has ended, after a `close`, or after `stopped`. |
| `crashed` | | The process crashed. |

### clock

One event for each clock action that is done, with `what` the name of the action: `shiftWall`, `stopWall`, `startWall`, `stopMonotonic`, or `startMonotonic`.

| Field | Meaning |
| --- | --- |
| `by` | The shift of `shiftWall`. |
| `wall` | The wall clock of the process after the action, in milliseconds from the start of the run. It is negative after a shift far enough backward. |
| `monotonic` | Its monotonic clock after the action, in milliseconds since the process started. |
| `wallStopped`, `monotonicStopped` | Whether each clock stands still. |

### core

An event that the storage core logs, taken from its logger as it is. It has no `what`. `event` is its name, `level` is `DEBUG`, `INFO`, `WARN`, or `ERROR`, and `attrs` holds its attributes. A duration in `attrs` is in milliseconds, measured on the monotonic clock of the process. Every event has `db.prefix`, which is `db`, and `instance.id`, the holder ID. A failure has `error`.

The names and attributes are those of [internal/core/telemetry.go](../core/telemetry.go), and that file is the reference. The ones that matter to a page:

| `event` | Attributes | `outcome` |
| --- | --- | --- |
| `lease` | `lease.transition`: `taken`, `renewed`, `lost`, or `released`. `lease.holder` on all but `released`. For `taken` also `lease.previous_holder`, `lease.duration`, and `commit.name`, the head. `attempts` and `duration.total` for a request that was sent. | `taken`: `taken`, or `failed`. `renewed`: `renewed`, `unknown` for no answer, `conflict` for a refusal that left the manifest unchanged, or `failed`. `lost`: `lost`, with the new holder in `lease.holder`. `released`: `released`, or `failed`. |
| `commit` | `commit.parent`. Once the commit object is written: `commit.name`, `commit.changes`, `commit.annotations`, and `commit.bytes`. `attempts`, `duration.total`, and `duration.spool`, `duration.upload`, `duration.swap`, and `duration.cache` for the phases that it reached. | `committed`, `empty` for a transaction that changed nothing, `committed_and_stopped` for a commit after which the cache failed, `unknown`, or `not_committed`. |
| `catch_up` | `catch_up.from` and `catch_up.to`, the position of the cache before and the head that it goes to. `catch_up.commits`, the number of commit objects applied. `commit.bytes`, `duration.total`. | `caught_up`, or `failed`. |
| `refusal` | `operation`, such as `begin`, `read`, or `commit`. `reason`: `not_holder`, `outcome_unknown`, `not_caught_up`, `lease_expired_monotonic`, or `lease_expired_wall`. | `refused` |
| `unknown_outcome` | `unknown.kind`: `commit` or `renewal`. `attempts`, `duration.total`, and `commit.name` for a commit. Logged when a swap of the manifest whose outcome was unknown is settled. | `committed`, `not_committed`, `renewed`, or `lease_lost`. |
| `busy` | `operation`. The cache was busy after the busy timeout. | `busy` |
| `stop` | The instance has stopped for good. | `closed` for a close. Otherwise the kind of the cause: `local_failure`, `cache_not_in_log`, `newer_version`, `name_mismatch`, `malformed`, `log_broken`, or `other`. |

A commit renews the lease too, so an instance that commits often logs few events `lease` with the transition `renewed`. The core logs no event when a lease runs out by the clocks. The refusals that follow carry the reason.

### call

A call of the application: one write or read transaction.

| Field | Meaning |
| --- | --- |
| `call` | The number of the call, from 1, in the order in which the calls started. |
| `caller` | `caller-0`, `caller-1`, and so on for the workload, and `visitor` for an action. |
| `op` | `write` or `read`. |
| `value` | The key that a write inserts. |
| `started` | When the call started. It is on the event that ends the call. |
| `keys`, `position` | What a read that was served saw. |

Every call has the event `started`, and one event when it ends:

| `what` | Meaning |
| --- | --- |
| `committed` | The write is committed, and the caller knows it. |
| `unknown` | The commit ended with an unknown outcome. |
| `not_committed` | The commit failed, and the transaction is not committed. |
| `abandoned` | The write failed before its commit and was rolled back. |
| `served` | The read was served. |
| `refused` | The instance has no valid lease. `error` ends with the reason of the core. |
| `stopped` | The instance has stopped, or its database is closed. |
| `busy` | The cache was busy. |
| `timed_out` | The timeout of the call ended before the transaction started. |
| `failed` | Any other error, also that the process has no database open. |

### request

A request to the store. Every event of one request has the same `request`, the number that the store gave it, from 1 in the order sent.

| Field | Meaning |
| --- | --- |
| `method` | `get`, `create`, or `swap`. |
| `key` | The key of the object. |
| `purpose` | What the request is for: see below. |
| `bytes` | The size of what a write sends, on `sent` and `failed`. |
| `version` | The version of the object after a write, on `applied`. The version that a read got, on `read`. The version that the caller gets, on `returned`. |
| `write` | The number of the write among those that the store applied, from 1, on `applied`. |
| `object` | What an applied write wrote: see below. |
| `way` | `there` or `back`, on `slow`. |
| `by` | The delay, on `slow`, `held`, and `stuck`. |

The purposes:

| `purpose` | The request |
| --- | --- |
| `read` | A get of the manifest. |
| `download` | A get of a commit object. |
| `upload` | The create of a commit object. |
| `take` | A write of the manifest that gives the lease to a new holder: the create of the manifest, and a swap after which the holder differs. |
| `commit` | A swap of the manifest that moves the head. It renews the lease too. |
| `renew` | A swap of the manifest that changes neither the holder nor the head. |
| `release` | A swap after which the manifest has no lease. |
| `write` | A write of the manifest that the engine cannot place: content that is no manifest, or a swap from a version that the engine has not seen applied. |

The life of a request, in the order of its events:

1. `sent`: the process sends it.
2. `slow`, with `way` as `there`: it takes longer on its way.
3. When it arrives after its caller gave up or its sender crashed: `never_applied`, or `held`, after which the store applies it `by` later. A get is never held.
4. When the store is unreachable for the process, or by `lostRequest`: `lost`, which ends it, or for a write by `late`, `stuck`. A stuck write gets `arrived` after `by`, or `never_applied` if the store is unreachable then.
5. The store handles it. For a write: `applied`, or `refused` with the reason in `error`, such as `the version is ...`, `an object exists`, or `a conflict with another write in flight`. For a get: `read`, `read_broken` for content that breaks off, or `not_found`.
6. `slow`, with `way` as `back`.
7. The response: `answered`, `no_caller` if the caller gave up or crashed, or `response_lost`. A write that was stuck gets no response.

The caller's side runs beside it:

- `gave_up`: the context of the caller ended before an answer came, by a timeout of the core or the end of a call. The request goes on.
- `no_answer`: by `hang`, the caller of a request that was lost on its way there or back gets no answer, and waits until it gives up.
- `returned`: the call returns to the core, with `version`, or with `error`. It has no `request` if the call ended before anything was sent, as when its context had ended already.
- `damaged`: one byte of a download changed on its way. It comes before `returned`.
- `answer_lost`: an armed `loseAnswer` took the answer. It comes in place of `returned`.
- `failed`: an armed `failRequest` ended the request before it was sent. It has no `request`, and is the only event.
- `too_large`: a write above the size limit of the store is not sent. The engine sets no limit, so this does not happen.

A request of a process that crashes gets no `returned`.

`object` on `applied` has one of three members:

- `manifest`: `head`, the name of the newest commit object, and `lease`, which is null or has `holder`, `duration`, `renewed`, and `salt`. `renewed` is the wall clock of the holder at the renewal, in milliseconds from the start of the run. `salt` is in hexadecimal.
- `commit`: `name`, `parent`, `keys`, the keys of the rows that it inserts into `t`, `tables`, the tables that it creates, and `changes`, the number of its changes.
- `error`: why the object could not be read.

### store

What happens to the store as a whole.

| `what` | Fields | Meaning |
| --- | --- | --- |
| `cut_off` | `instance`, `process`, `for` | The process is cut off from the store for `for`, by the action or by the fault. |
| `outage` | `for` | The store is unreachable for every process for `for`. |
| `calm` | | The store makes no new fault from now on. |

A line of the store that the engine does not know becomes an event of this kind with only `text`. A test fails if one occurs.

### action

One event for each action of the schedule, at its time.

| Field | Meaning |
| --- | --- |
| `action` | The index of the action in the schedule, from 0. |
| `do` | The name of the action. |
| `what` | `done`, `ignored`, or `failed` for a `restart` whose cache could not be copied. |
| `text` | `done`, or why the action was ignored or failed. |
| `instance`, `process`, `for`, `by` | Those of the action. `process` is the process of the instance when the action came, so the crashed one for a `restart`. It is missing on an action that was ignored for a crashed process. |

The effect has events of its own: `process` with `crashed` just before the action event of a `crash`, `process` with `started` just after that of a `restart`, and `clock` just before that of a clock action. A `close` and an `open` run in a task, and their `process` events come when they end.

### check

A violation of an invariant, when the checker finds it. `what` is `violated`, `invariant` is the name, `excused` says whether the design accepts it, and `text` says what was found. A violation by a read has `instance` and `process`. See [The invariants](#the-invariants).

### buggify

A point of buggify fired. `what` is `fired`, and `site` is the name of the point. The core has two points today: `core: the lease is renewed early` and `core: the cache fails to commit after the manifest swap`. Whether a point is on for the run, and each time that it does not fire, is in `lines` only.

## The state at a time

The message `state`, with the body `{"t": T}`, answers with the state of the last run at the time T. The state is a fold of the events up to and including T, by `StateAt` in `state.go`. It reads nothing else, so a page can fold the events itself. The engine takes no snapshot during a run, because reading a cache or waking a task for one would change the run.

| Field | Meaning |
| --- | --- |
| `error` | Why there is no state: no run yet, or a body that is not `{"t": T}`. It is the only field then. |
| `t` | The time. |
| `bucket` | The manifest and the commit objects. |
| `instances` | The instances, in the order of their first events. |
| `requests` | The requests in flight, in the order sent. |
| `calls` | The calls that have started and not ended, by their numbers. |
| `outageUntil` | The latest end of an outage: the largest `t` + `for` of the events `store` with `outage`. It can lie in the past. |
| `calm` | Whether an event `store` with `calm` has come. |
| `violations`, `excused` | The events `check` so far, and those of them that are excused. |

As in the events, a field that is zero can be missing: `outageUntil`, `calm`, `holder`, `cutOffUntil`, the two flags of the clocks, and the `value` of a call.

`bucket`, from the events `request` with `applied`:

| Field | Folded from |
| --- | --- |
| `manifest` | The `object.manifest` of the last one, or null. |
| `manifestVersion` | Its `version`. |
| `manifestWrites` | The number of them. |
| `commits` | Each `object.commit`, once, in the order written, with its `name`, `parent`, `keys`, `tables`, and `changes`, the `process` that wrote it, the time `at`, and `inLog`. |
| `log` | The names of the commit objects of the log, from the first to the head: from `manifest.head`, follow `parent` through `commits` to the empty log. `inLog` says whether a commit object is among them. |

An instance:

| Field | Folded from |
| --- | --- |
| `instance`, `process` | The last event `process` with `started`. |
| `state` | `starting` from `started`. `running` from `opened`. `failed` from `open_failed`. `closing` from the event `action` of a `close` that is `done`. `stopped` from `stopped`, and from an event `core` with `stop` whose outcome is not `closed`. `closed` from `closed`. `crashed` from `crashed`. |
| `holder` | The `holder` of the last `opened`. A new process starts without one. |
| `lease` | `held` from the event `core` with `lease`, the transition `taken`, and the outcome `taken`. `none` from the transition `lost`, from `released` with the outcome `released`, from the event `core` with `stop`, from `closed`, `stopped`, and `crashed`, and for a new process. |
| `position` | The name of the last commit object in the cache: see below. |
| `wall`, `monotonic` | The clocks at T: see below. |
| `wallStopped`, `monotonicStopped` | Those of the last event `clock`. False for a new process. |
| `cutOffUntil` | The latest end of a cut-off of the process: the largest `t` + `for` of its events `store` with `cut_off`. It can lie in the past. A new process starts without one. |

Events `core` of a process that is no longer the process of its instance are left out of the fold.

Three points about the state of an instance:

- `closed` comes when the close has ended. A close waits for the calls that still run, so the state is `closing` from the action until then. The event `core` with `stop` and the outcome `closed` marks the moment that the core stops serving.
- `held` says that the core took the lease and has not learned that it lost it. The lease can have run out by the clocks. The manifest in `bucket` and the clocks of the instance say whether it has.
- A restart keeps `position`, because the new process starts on a copy of the cache.

The clocks. The last event `process` with `started`, or `clock`, of the instance gives a base: its `t`, `wall`, and `monotonic`. At T, `wall` is the base's `wall` plus T minus the base's `t`, unless the wall clock is stopped, in which case it is the base's `wall`. `monotonic` follows the same rule.

The position starts at the empty log and follows these events of the process:

- `core` with `commit` and the outcome `committed` or `committed_and_stopped`: `commit.name`.
- `core` with `catch_up` and the outcome `caught_up`: `catch_up.to`.
- `core` with `catch_up`, another outcome, and `catch_up.commits` above 0: the commit object that is that many after `catch_up.from` on the way to `catch_up.to`, found through the `parent` of the commit objects in `bucket`. If the way cannot be followed, `catch_up.to`.
- `call` with `served`: its `position`.

A test folds the positions from the events of the core alone, and compares them with what each read saw.

A request in flight has `request`, `instance`, `process`, `method`, `key`, and `purpose` from its event `sent`, and:

| Field | Folded from |
| --- | --- |
| `sent` | The `t` of `sent`. |
| `phase` | `to_store` from `sent` and from `arrived`. `held` from `held`. `stuck` from `stuck`. `to_caller` from `applied`, `refused`, `read`, `read_broken`, and `not_found`, unless the request was stuck: then `done`. `done` from `answered`, `no_caller`, `response_lost`, `lost`, and `never_applied`. |
| `waiting` | True from `sent`. False from `returned` and `answer_lost`. |
| `happened` | The `what` of its events after `sent`, in order. |

A request is listed while `waiting` is true or `phase` is not `done`. So a write that the store holds back stays listed after its caller gave up, and a request without an answer stays listed, as `done`, while its caller waits.

A call in flight has `call`, `instance`, `process`, `caller`, `op`, and `value` from its event `started`, and `started`, the `t` of that event.

A crash ends the calls of its process in the fold, and every wait of the process for a request: its calls leave `calls`, and its requests get `waiting` false. A request of a crashed process stays in `requests` while the network still carries it, until its `phase` is `done`.

## The invariants

The engine checks six invariants during the run and at its end. A violation is an event of the kind `check`. The checker sees what an application and the store see: how each call ends, what each read returns, and the objects that the store applies. Each write has a key of its own, so the keys tell the transactions apart.

"The log" is the chain of commit objects from the head of the manifest back to the empty log.

| `invariant` | Checked | Meaning |
| --- | --- | --- |
| `chain` | At every write of the manifest that the store applies. | The manifest moves its head only to a commit object that is in the store and whose parent is the head before. So the log is one chain. |
| `acknowledged` | When a write ends with `committed`, and at the end. | A commit that was acknowledged is in the log: once when it is acknowledged, and still at the end. |
| `not_committed` | At the end. | A write that ended with `not_committed` is not in the log. |
| `once` | At the end. | No key is in the log twice. |
| `state` | When a read is served. | The read saw a state that the log had: its position is the empty log or a commit object of the log, and its keys are those of the log up to that position. |
| `fresh` | When a read is served, and at the end. | The read saw every write that was acknowledged before the read started. It also saw every write that returned with an unknown outcome before the read started and that the log holds at the end. A read gets at most one violation in each of the two checks. |

Only `fresh` can be excused. The design accepts a read that misses a commit on a process whose two clocks both went wrong in the direction that keeps a lease alive too long. A violation of `fresh` is excused if, by the time of the check, the monotonic clock of the read's process has been stopped, and its wall clock has been stopped or shifted backward.

What the checker cannot see, and how far it is tested:

- It sees nothing inside the core: no cache except through the reads, and nothing of what an instance believes about its lease.
- It checks reads that were served. For a write that ended in another way than `committed`, `unknown`, or `not_committed`, such as `abandoned`, it does not check that the log lacks it.
- It can be tested only on input made for it, in `check_internal_test.go`. The core cannot be made to break an invariant from outside its package, and planted bugs are left out of the simulator on purpose. The tests of the runs expect no violation that is not excused.

## The old page and the engine

The page in `site/simulator/` ran a JavaScript model of the design before it ran this engine. What the model offered that the real code cannot:

- **Stall and resume.** The model pauses a process while its clocks run. `sim` cannot pause the tasks of a process. `stopMonotonic` holds its sleeps and timeouts, and the clock stands still with them, which is a different fault.
- **Clock rates.** The model has a monotonic clock that runs at twice the rate, and both clocks at half the rate. A clock of `sim` is shifted, stopped, and started, and runs at the true rate.
- **A clock set true again.** No action does it. A page can shift a wall clock by the difference that the state shows. A monotonic clock that stood still stays behind for the life of its process.
- **A failing disk.** `faultvfs` cannot be reached. `core.Options` has no exported option for a VFS, and `core.Open` refuses a path that starts with `file:`.
- **Planted bugs.** They are left out on purpose. The page shows the code as it is.
- **Heal all, as one action.** A page composes it: `calm`, a `restart` of each crashed instance, `open` for each closed database, the two start actions for the clocks, and a `shiftWall` back. A cut-off or an outage that runs still goes on to its end.
- **Random faults of processes.** The model's chaos crashes, shuts down, and stalls instances at random, and moves their clocks. The engine draws only the faults of the store. A page draws the others from the seed and puts them into the schedule.
- **Reconnect.** A cut-off or an outage has its duration from the start, and no action ends it early.
- **Slow S3, switched at a time.** The store takes its faults when the run starts. `slow` holds for the whole run, until a `calm`.
- **Buggify, switched at a time.** It is on or off for the run.
- **The rows of a cache and the state of its disk.** The state has the position of a cache. A read that is served shows the keys.
- **Unknown outcomes in flight.** The state has no field for them. The events have them: `call` with `unknown`, `core` with `unknown_outcome`, and refusals with the reason `outcome_unknown`.
- **Checks inside the caches.** The model checks that no cache is ahead of the manifest and that replaying the log gives the rows of every cache. The engine's checker sees a cache only through reads, with `state`.

What the engine offers that the model did not:

- The storage core itself, unchanged, on the official SQLite build. What the model simplified is as the code does it: a read checks the lease before and after it fixes its state, and a catch-up downloads each commit object twice.
- Commit objects in Protocol Buffers, as the format defines them.
- The timeouts of the core: 5 s for the manifest and 5 min for a commit object, where the model has 5 s for every request.
- The core's own events, with their attributes and durations.
- The core's own points of buggify.
- Every fault of the store with a probability of its own, set in the request. Outages and broken reads are new. Conflicts, requests held in the network, and damaged downloads are in the model only as points of its buggify, at fixed rates. Here a conflict comes only while another write is in flight, as S3 documents it. A write whose caller gave up, or whose sender crashed, can land up to 30 minutes late.
- 1 to 5 instances, where the model has 2 or 3, with a lease duration for each.
- Settings for the latency and the workload.
- Runs that match the native build. The response has the hash that the tests of the core compute, and the same request gives the same hash natively and in Chromium.

## Using it from a page

### The files

A static site serves these files from one directory. The directory is on the origin of the page, because a page can start a Worker only from its own origin.

| File | From |
| --- | --- |
| `engine.wasm` | The program: the test binary of `internal/simulator/engine`. |
| `worker.js` | `internal/simulator/engine`. The Worker that runs the program. |
| `simulator.js` | `internal/simulator/engine`. The module that a page imports. |
| `sqlite3.js`, `sqlite3.wasm`, `vfs.js`, `bridge.js`, `fs.js` | `internal/sqlite/browser`: see [its README](../sqlite/browser/README.md). |
| `wasm_exec.js` | `lib/wasm/` in the GOROOT of the Go that built `engine.wasm`. It must come from that Go. |

`make simulator SIMULATORDIR=dir` writes them into `dir`, with the demo page `index.html`. Without `SIMULATORDIR` the directory is `s4ql-simulator` in `$TMPDIR`, or in `/tmp`. The target runs:

```sh
go run -tags s4ql_browser ./internal/simulator/serve -out dir
```

`serve` builds the program with the Go on the PATH, and copies the other files:

```sh
GOOS=js GOARCH=wasm go test -c -tags s4ql_browser -o dir/engine.wasm ./internal/simulator/engine
```

The program is a test binary because a run needs a `testing.T` for its `testing/synctest` bubble, and a test binary is the supported way to get one. Its one test, `TestEngine`, serves the messages of the page for as long as the Worker lives.

For the project's site, `make site` writes the files into `site/simulator/engine/`, which `.gitignore` covers, without the demo page, and with `build.json`, `{"commit":"..."}`, which names the commit that the engine is built from. The Pages workflow runs it before it uploads `site/`. `make site-serve` serves the site on `SITEADDR` without the two headers below, as GitHub Pages serves it, and `make site-check` checks the page in headless Chromium with `sitecheck`: see its package comment.

The Worker instantiates `engine.wasm` while it streams, so the server must send it with the content type `application/wasm`.

The headers `Cross-Origin-Opener-Policy: same-origin` and `Cross-Origin-Embedder-Policy: require-corp` are optional. With them the VFS sleeps with `Atomics.wait`, and without them it spins.

The official SQLite build logs errors about OPFS in the console, because its OPFS proxy worker is not served. They are harmless: the engine keeps its files in memory.

### The module

```js
import { Engine } from './simulator.js';

const engine = await Engine.start();
const response = await engine.run({ seed: 1, end: 120000, schedule: [] });
const state = await engine.state(60000);
```

| Member | Meaning |
| --- | --- |
| `Engine.start(base)` | Starts a Worker and resolves with an engine once it takes messages. `base` is the URL of the directory with the files, by default the directory of `simulator.js`. |
| `run(request)` | Runs the request, an object, and resolves with the response as an object. It rejects with an `Error` that has the response's `error` if the engine refuses the request. |
| `state(t)` | Resolves with the state of the last run at `t`. It rejects if there is no run yet. |
| `timings` | `ready`: the milliseconds from the start of the Worker until the engine took messages. `run`: the milliseconds that the engine took for the last message, without the transfer and the parsing of the answer. |
| `memory` | The size of SQLite's WebAssembly memory after the last answer, in bytes. |
| `terminate()` | Ends the Worker. |
| `Engine.recycleBytes` | The memory above which `run` starts a fresh Worker first. The default is 64 MiB. |

Calls wait for one another: the engine takes one message at a time. If the Worker fails or the program exits, every call that waits rejects. What the program writes to standard output and standard error goes to the console.

SQLite's memory grows from run to run. A run whose processes crash leaves their SQLite connections open, as a crashed process does, and nothing closes them. The Worker reports the size of SQLite's WebAssembly memory, which never shrinks, because the official build does not export `sqlite3_memory_used`. Before a run, `run` ends the Worker and starts a new one if the memory is above `recycleBytes` and no call waits. That costs one start of the engine.

### The Worker

A page that does not use the module talks to `worker.js` itself. The Worker loads the scripts in the order that the README of `internal/sqlite/browser` gives, and runs `engine.wasm` with the argument `-test.run=^TestEngine$`.

The page posts `{id, kind, body}`. `kind` is `run` or `state`, and `body` is the message as JSON text or as an object. The Worker posts:

| Message | Meaning |
| --- | --- |
| `{type: "ready", ms}` | The engine takes messages. `ms` is the time since the Worker started. |
| `{type: "answer", id, answer, ms, memory}` | The answer to the message `id`, as JSON text, the milliseconds that the engine took, and the memory as above. An answer to a message that the engine refuses has only `error`. |
| `{type: "output", fd, text}` | What the program wrote to standard output, `fd` 1, or standard error, `fd` 2. |
| `{type: "exit", code}` | The program ended. |
| `{type: "error", message}` | The Worker failed. |

Messages wait in a queue in the Worker, and the engine takes one at a time. A message of an unknown kind and a request that cannot be parsed get an answer with an error, and the engine goes on.

### The demo

```sh
make simulator-demo
```

Then open http://127.0.0.1:8080/. The target writes the files, as `make simulator` does, and serves them with the two headers above and without caching, on `SIMULATORADDR`. The page runs one request three times when it loads, and again on "Run". It shows the time of each run, the summary, the state at the end, and the first 300 events. The request is in a text field and can be changed.

## Tests

The tests of `internal/simulator` run natively and in Chromium. They show:

- **The request** (`request_test.go`): the default of every field, an object given in part, each refusal with its message, and that one error lists every problem.
- **Determinism** (`simulator_test.go`): the same bytes for the same request twice, and on a second engine. The same hash and events twice for ten seeds with every fault and buggify, with no violation that is not excused. The same events before the time from which two schedules differ, at 40000 and at 61234.5. The same events before the earlier of two ends.
- **Messages** (`simulator_test.go`): a malformed request and an unknown kind get an error, and the engine goes on. A state before any run, and a malformed state, get an error.
- **Actions** (`run_test.go`): the effect of each action, and which actions are ignored.
- **Faults** (`run_test.go`): each fault of the store, damaged downloads, and buggify show their events, with no violation. Under every fault at once, the engine knows every line of the store.
- **The state** (`state_test.go`): the position of each cache, folded from the events of the core alone, against what each read saw. The log. A request and a call in flight, from their start to their end.
- **The checker** (`check_internal_test.go`): each invariant on input made to break it, and the excuse of `fresh`.

`TestEngine` in `internal/simulator/engine` skips unless a page drives it: it looks for the object `s4qlEngine`, which `worker.js` sets.

Natively, which `make test` does too:

```sh
go test -tags s4ql_browser ./internal/simulator/...
```

In Chromium, through the runner that the README of `internal/sqlite/browser` describes:

```sh
make test-browser BROWSERPKGS=./internal/simulator
```

`BROWSERPKGS` holds `./internal/simulator` and `./internal/simulator/engine` by default, so `make check-browser` and `make test-browser` cover them. `SIMULATORPKGS` is `./internal/simulator/...`: `make lint` runs `golangci-lint` and the lint for code under the simulation over it, natively with the tag. CI's job `browser` does all of this.

## Measurements

Measured on an Apple M4 with headless Chrome 158 and Go 1.27.4, on 2026-11-18, with the bundle that `make simulator` writes, served from 127.0.0.1.

Sizes, with `gzip -9` in parentheses:

- `engine.wasm`: 12,123,036 bytes (2,945,531), and 2,098,722 with Brotli at level 11. Built with `-ldflags='-s -w'` it is 11,825,424 bytes (2,887,392), which saves 2% and loses the names of functions in a panic, so the build keeps them.
- `sqlite3.wasm` 879,430 (406,762), `sqlite3.js` 439,732 (93,665), and the JavaScript of the engine, the bridge, the VFS, the file system, and `wasm_exec.js` together 50,621 (16,818).

A page downloads about 3.5 MB with gzip. On 127.0.0.1, which takes no time for the download, the engine was ready 100 ms after the demo page loaded its module in a fresh browser profile, 84 ms after its Worker started, and 75 to 89 ms on later loads.

The time of one run, the mean of five after two to warm up, in the runner of `internal/sqlite/browser`, with three instances and the default workload unless the line says otherwise:

| Run | Time | JSON of the response |
| --- | --- | --- |
| 2 min, no faults | 93 ms | 306,681 bytes |
| 2 min, the preset of faults | 97 ms | 332,343 bytes |
| 5 min, the preset, a crash and a restart | 195 ms | 681,297 bytes |
| 10 min, the preset | 425 ms | 1,559,583 bytes |
| 30 min, 5 instances, the preset, buggify | 1,467 ms | 6,628,991 bytes |

The page of the site runs two instances, the default workload, and no random faults, and asks for a longer run every 2 min of fake time, up to 10 min. Its runs, the mean of five with a crash and a restart, in headless Chrome driven by `playwright-cli` on the same machine: 2 min 70 ms, 4 min 146 ms, 6 min 222 ms, 8 min 300 ms, 10 min 375 ms, 12 min 457 ms, and 15 min 632 ms, of which the transfer and parse of the answer took 1 to 4 ms. A fault that the visitor adds holds the playback for one run, so the page stops at 10 min.

In the demo page, the request of the demo, 2 min with the preset and six actions, took 166 to 186 ms the first time and 88 to 110 ms after that, of which the transfer and parse of the answer took 1 to 8 ms. Natively the same run takes about 40 ms, so the browser is about 2.3 times slower.

The time grows with the steps of the scheduler, which grow with fake time, instances, and callers. A native profile of a 10 min run put half of the time in handing the baton between the scheduler and its tasks, as the README of `internal/core` measured for its tests, and a tenth in creating the spool files of commits. Encoding the response took 3%. A page that must run faster has these levers: a shorter `end`, fewer instances or callers, and a longer `think`. Calling SQLite through `//go:wasmimport` in place of `syscall/js` would make each call cheaper, at 6 ns against 807 ns by the measurement of step 1; it was not needed for these times.

The size of SQLite's WebAssembly memory, after 60 runs of 1 min with two crashes each: 8 MiB before, 30 MiB after 20 runs, 53 MiB after 40, 76 MiB after 60, so about 0.55 MB for each crashed process. Sixty runs without crashes after them grew it once, to 91 MiB, and then no more. The Go heap after a collection stayed under 1 MB between runs.

The hashes of two requests, the demo's and one with a crash and a restart, are the same natively, in the runner, and in the demo page: `95308c33cd30a785...` and `b82b8ab6e0336832...`. No test compares them across the two builds.
