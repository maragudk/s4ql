# The engine of the simulator

`internal/simulator` runs the storage core of s4ql, with its SQL engine on top, under the simulation, and returns what happened as a structured trace. It runs the code in two ways:

- A batch run takes a request, which is a seed, settings, and a schedule of actions and faults at fake times, runs it to its end, and returns the whole trace.
- A live session takes a seed and settings, and has no end. The page advances its fake time, acts at the time of the session, and asks for the state now, one command at a time. Each answer has the events since the answer before.

It exists for the simulator on the project's site, which is to show how the current code reacts to faults of its environment. The page must therefore run the code, and `site/simulator/` runs this engine in place of the JavaScript model of the design that it ran before. The page drives one live session, and `make site-check` checks it. This file is the specification for the page: the messages, the request, the response, the live session, the events, the state at a time, the invariants, what the code does under each action, and how a page loads the engine.

- The package builds only with the tag `s4ql_browser`. With the tag it builds natively and for a browser, with `GOOS=js GOARCH=wasm`, where it runs on the SQLite binding of [internal/sqlite/browser](../sqlite/browser/README.md).
- `internal/core`, `internal/engine`, `internal/sim`, and `internal/store/simstore` are used unchanged, through their exported API. The calls of the workload are SQL, which `internal/engine` runs on the database of the core, as the `database/sql` driver does.
- `engine/` holds the program for a browser, its Worker, the module that a page imports, and a demo page. `serve/` builds and serves them.

## How a run works

`Engine.Handle` takes a message, which is a kind and a body as JSON, and returns the answer as JSON. The kinds `run` and `state` are those of a batch run. The other five are the commands of a live session.

| Kind | Body | Answer |
| --- | --- | --- |
| `run` | A request: see [The request](#the-request). | The response: see [The response](#the-response). The run goes to its end before the engine answers. |
| `state` | `{"t": T}` | The state of the last batch run at the time T: see [The state at a time](#the-state-at-a-time). |
| `start` | The settings of a session: see [The settings](#the-settings). | Starts a session, and answers with its first events. A session that runs is ended first. |
| `advance` | `{"by": D}` or `{"to": T}` | Moves the fake time of the session on by D, or to the time T, and answers with the events of the stretch. |
| `act` | An action as in a schedule, without `at`. | Does the action at the time of the session. |
| `now` | `{}` or `{"commits": N}` | The state of the session now. |
| `end` | `{}` | Ends the session, with the last checks of the invariants, the summary, and the hash. |

The five commands of a session answer in one form: see [The answer](#the-answer). A message of another kind gets an answer with only `error`, which lists the kinds. A batch run and a session do not touch each other: `run` does not end a session, and `state` is always that of the last batch run.

A batch run and a session run the same code in the same way:

1. The engine starts a scheduler of `internal/sim` with the seed, in a `testing/synctest` bubble of its own, and a simulated store of `simstore`. If the request has a stored bucket, the engine puts its objects into the store before any instance starts: see [A stored bucket](#a-stored-bucket).
2. It starts the instances, named `a`, `b`, `c`, and so on. A process runs each instance: a simulated machine with its own clocks, its cache in a file, its callers, and its client of the store. The first process of an instance has the name of the instance, and a restart starts the next: `a2`, `a3`, and so on. With `standby`, only `a` starts here, and the others once it serves. If the request has a client, it starts too: see [Client](#client).
3. In a batch run, the first task of the run does the actions of the schedule at their times, and then sleeps until the end. In a session, the first task does the commands of the page as they come: see [A live session](#a-live-session).
4. At the end it runs the last checks of the invariants and stops recording. The end of a session is its command `end`.

The trace holds the events before `end`, and those at `end` that happen before the recording stops. After that the engine lets the run finish, and none of it is in the trace: every process that has not crashed closes its database, every monotonic clock that stands still starts again, because the sleeps of a process stand still with its clock, and the requests that are on their way end.

### Determinism

- The same request gives the same response, byte for byte, every time and on every engine.
- Two requests that differ only in their schedules give the same events before a time T, if the schedules agree before T: the actions with `at` below T are the same, at the same indexes. "Before" is strict. Events at exactly T can differ.
- Two requests that differ only in `end` give the same events before the earlier end.
- A session that gets the actions of a request's schedule at their times gives the same events and the same hash as the batch run: see [A session and a batch run](#a-session-and-a-batch-run).

The request is the whole input of a batch run, so a page can share a batch run as its request.

### A fault while the run plays

A page has two ways to let a visitor act while a run plays.

The first is the batch run again. The page plays a trace back at its own speed. When the visitor acts at the fake time T, the page adds the action to the schedule at T and asks for the run again. The events before T are the same, so the visitor sees the run go on from where it was. Events at exactly T can change. A page that must keep everything it has shown gives the new action a time after the last event shown.

The second is the live session, in which the run itself waits for the visitor. The page sends `act` when the visitor acts, and the action happens at the time of the session.

An earlier version of this file said that a run cannot wait for the visitor in a bubble: a goroutine of the bubble that waits is durably blocked, so fake time would run on without the visitor. That holds for a wait on something made inside the bubble. The session waits on a channel made outside it, and such a wait is not durable, so fake time stands still: see [A live session](#a-live-session).

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
| `client` | `{"instance": ""}` | an instance of the run, or empty | The client, which calls one instance from outside the instances: see [Client](#client). Empty is no client. |
| `standby` | false | | Starts the instances after `a` only once `a` serves: see [Client](#client). |
| `schedule` | `[]` | at most 1000 actions | The actions, each at its time: see [Schedule](#schedule). |
| `lines` | false | | Asks for the text trace of the scheduler in the response. |
| `prefix` | `db` | 1 to 32 characters of `a` to `z`, `0` to `9`, and `-`, not starting with `-` | The prefix of the database in the store. The key of each of its objects starts with the prefix and a slash. |
| `bucket` | none | see [A stored bucket](#a-stored-bucket) | The objects that the store holds under the prefix when the run starts. Without it the store starts empty. |

The other options of the core keep their defaults. An instance renews its lease after two thirds of its duration. A request for the manifest times out after 5 s, and an upload or a download of a commit object after 5 min. An instance that does not hold the lease reads the manifest every 5 s. SQLite waits 1 s for a lock on the cache.

The scheduler delays half of all task starts, yields, and releases by up to 50 ms, and fails a batch run after 5,000,000 steps. A session has no limit of steps.

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
| `deletes` | 25 | 0 to 100 | The share of writes that are deletes, in percent. The others are inserts. |
| `timeout` | 30000 | above 0, at most 1800000 | The timeout of each call, on the monotonic clock of its process. |

A caller repeats two steps. It pauses for 1 ms and a time from 0 to `think`, from the seed. Then it makes one call, a write or a read. While the workload is off it makes no call, and looks again after a second.

A write is an insert or a delete. One number u from the seed decides the call. The call is a write if `u mod 100` is below `writes`, and a read otherwise. A write is a delete if `(u / 100) mod 100`, with the division rounded down, is below `deletes`, and an insert otherwise.

Each call is SQL, on a connection of its own, `engine.Conn`, of the engine of its process. Each process has an engine, `engine.New` on its database and its host, which it makes when it opens its database. The engine draws the directions in which its queries read from a seed of its own, `engine.Options.Directions`, which the seed of the run, the process, and the number of its engine fix, so the draws of the scheduler are those of the callers and the faults alone. The table is `barcodes`:

```sql
create table barcodes (code integer primary key) strict, without rowid
```

- An insert is one write transaction: `Conn.Begin`, the insert `insert into barcodes (code) values (?)`, and `Conn.Commit`. The argument is the key of the row, the barcode of the next value of a counter of the run. The counter starts at 1, or after the `values` of the [stored bucket](#a-stored-bucket) that the run started on. Every insert takes a value when it starts, also one that is then refused, so a key belongs to one insert. If the insert fails with `no such table: barcodes`, the transaction runs the `create table` above and the insert again, so the first insert of a run commits both statements. A statement that fails in a transaction has no effect there, and the transaction stays open.
- A delete is one write transaction: `Conn.Begin`, the delete `delete from barcodes where code = ?`, and `Conn.Commit`. The argument is a key that the application knows is in the store and that no delete has picked before. The delete picks it when it starts, by a number from the seed, each such key with the same chance. The key leaves the known keys at that moment, so no read selects it while the delete runs. So a key is inserted by one transaction and deleted by at most one, and a key and whether its write deletes tell the transactions apart.
- A read is one select of one key, `select code from barcodes where code = ?`, which the engine runs as a lookup of the key. The read picks its key when it starts, by a number from the seed, so the key is the same in every build. One read in eight selects the key of one of the 10 newest acknowledged deletes, if there is one, and expects no row. The others select a key that the application knows is in the store, and expect its row: half of them the newest of these keys, and the others one of the 50 newest. The events of the read say which in `expect`, as `none` or `row`. A read that a state that is not stale serves gives what it expects, with one exception: a read that expects its row can find none when a delete of its key started while the read ran. So `expect` decides no violation. The checker decides, by the log: see [The invariants](#the-invariants). The read reports the key that it saw, if it found the row, and its position: the name of the last commit object in the state that it saw, which the engine gives as `statement.position` in its event of the statement. A select on a database without the table is served with no rows and the error `no such table: barcodes`, because it saw a state too. Only a state before the first commit lacks the table, and that state is stale, because the insert of the read's key was acknowledged before the read started.
- While a call has no key to pick, it is an insert instead, also the call of the action `read`. Its events have `op` as `write`. A read has no key while the application knows none. A delete has no key while the application knows fewer than two keys, so that the reads always have a key, and while a delete has picked every known key before. So the first call of a run on an empty store is an insert.

The application knows that the row of a key is in the store from the log of the stored bucket that the run started on, from an acknowledged insert, and from a delete that ended without deleting. It knows the keys in the order in which it learned them, and the last is the newest. How a delete ends decides what the application knows of its key:

| The delete ends with | Its key |
| --- | --- |
| `committed` | Is among the acknowledged deletes, of which a read can select the 10 newest. |
| `unknown` or `lost` | Is in limbo: no read selects it. |
| Any other end | Is known again, as the newest key. |
| No end, because its process crashed | Is in limbo too. |

No delete picks a key that a delete has picked before, however that delete ended.

The barcode of the value n is a number of 13 digits in the form of an EAN-13 barcode: the prefix `57`, ten digits that a bijection of n modulo 10^10 gives, and the check digit of EAN-13, which weighs the twelve digits before it by 1 and 3 in turn from the left. The bijection is four rounds of a Feistel network on the two halves of five digits each, which maps (hi, lo) to (lo, hi + f(lo)) modulo 10^5 with `f(lo) = (31337 lo² + 7919 lo + 104729 r + 12345) mod 10^5` in the round r from 0. So the keys of a run are distinct and do not rise with n, and each fits an integer of 64 bits, also in JavaScript.

With the default of `deletes`, three writes in four are inserts, so the table grows for the whole run. The action `write` makes an insert and the action `read` a read, as above, with the caller `visitor`. No action makes a delete.

How a call ends comes from the calls of the engine, which passes every error of the core through unchanged. A write, an insert or a delete, that `Conn.Begin` refuses ends as a refusal, by the error of the core: `refused`, `stopped`, `busy`, or `timed_out`. One whose insert, create, or delete fails ends with `abandoned`, and one whose `Conn.Commit` fails with `unknown` for an error that matches `core.ErrOutcomeUnknown`, or with `not_committed`. A read that fails ends as a refusal in the same way. `error` is the text of the engine's error.

### Client

The client is one source of calls that stands outside the instances, as the user of an application does. `client.instance` is the instance that it calls first, its target, and the action `client` points it at another. It is a task of the run, not of a process, so no crash, stop, or fault of an instance ends it. A batch run and a session have no client unless the request names one.

It repeats two steps, as a caller does: a pause of 1 ms and a time from 0 to `workload.think`, from the seed, on the time of the run, and then one call on its target, an insert, a delete, or a read by `workload.writes` and `workload.deletes`. Its calls are those of [Workload](#workload), with the caller `client`, the timeout `workload.timeout` on the monotonic clock of the target's process. As there, a call that has no key to pick is an insert. `workload.on` and the action `workload` do not switch it.

Each call goes to the process that runs the target when the call starts:

- If the target cannot take it, the call ends at once with `down`: the instance has not started, its process has crashed, or its database is not open, because it is starting, could not be opened, is closed, or has stopped. `error` says which.
- Otherwise a task of the process makes the call, as the application in the process serves a request that comes over its network, and the client waits for it. The call ends as any call does: an instance that does not hold the lease refuses it with `refused`.
- If the process crashes while the call is on it, the call ends with `lost`, at the crash: the client gets no answer. For a write, the outcome is unknown, as after `unknown`: what the process sent before the crash is still on its way, and the write can be in the log.

The connection between the client and its target has no faults. `hangNetwork` and `disconnectNetwork` are faults of the network between an instance and the store.

A `client` action takes effect at once. A call that is on its way when the target changes ends on the instance it went to, and the next call goes to the new target. The action is ignored in a run without a client.

With `standby`, `a` starts alone, and the client waits. A task of the run waits for the database of `a` to serve, by `Wait` of `core`, and then starts the other instances, which find the lease taken and stand by, and lets the client make its first call. It also starts them if `a` cannot serve: its process has crashed, its database is not open or stops, or the duration of its lease has passed on the time of the run. This is how s4ql is meant to run: one long-running instance per database, and a second that stands by to take over. Until an instance starts, it is in the state `waiting`, and an action on it is ignored, except `hangNetwork`, `disconnectNetwork`, `failDisk`, `loseAnswer`, `failRequest`, and `client`.

A page that shows an application and one database, as the simulator page does, starts a session with the client on `a`, `standby`, and no callers in the processes, `workload.callers` at 0. The instance that the client does not call then gets no call.

### Schedule

An action has the time `at`, from 0 to before `end`, the name `do`, and the parameters of its kind. Actions at the same time happen in the order of the schedule. `instance` is the name of an instance of the run. Every action also works in a session, as the body of the command `act`, without `at`.

| `do` | Parameters | Effect |
| --- | --- | --- |
| `write` | `instance` | One insert on the instance, by the caller `visitor`, whether the workload is on or off. The call starts in a task of the process, up to 50 ms after `at`. |
| `read` | `instance` | One read, in the same way. While the application knows no key, the call is an insert: see [Workload](#workload). |
| `crash` | `instance` | The process crashes. None of its tasks runs again. What it has sent is still on its way, and `afterCrash` decides whether a write that arrives later is applied. |
| `restart` | `instance` | A new process of the instance starts, on a copy of the cache that the crashed process left: the database file and its write-ahead log. Its wall clock reads the true time, which is the time of the history in a run on a [stored bucket](#a-stored-bucket), and its monotonic clock zero. It has a client of its own, so a cut-off of the crashed process does not hold for it. |
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
| `pauseClocks` | `instance` | Both clocks of the process stand still, as after `stopWall` and `stopMonotonic`. |
| `resumeClocks` | `instance` | Both clocks go on from where they stood, as after `startWall` and `startMonotonic`. |
| `hangNetwork` | `instance`, `on` | Switches a hang of the network of the instance on or off. While it hangs, no request of the instance is sent, no answer reaches it, and each caller waits until it gives up. |
| `disconnectNetwork` | `instance`, `on` | Switches a disconnect of the network of the instance on or off. While it is disconnected, each request of the instance fails at once and is not sent. |
| `failDisk` | `instance`, `on` | Switches a failure of the disk of the instance on or off. While the disk fails, every file operation on the cache of the instance fails. |
| `loseAnswer` | `request`, optional `instance` | Arms a fault. The next matching request is sent, the store handles it, and its answer is lost on the way to the caller, which gets an error. |
| `failRequest` | `request`, optional `instance` | Arms a fault. The next matching request fails before it is sent. |
| `workload` | `on` | Switches the callers on or off. |
| `client` | `instance` | Points the client at the instance: see [Client](#client). |

A missing `on` is false, so an action without it switches off.

An action that cannot be done is ignored. Its event says so, with the reason:

- Every action for an instance that has not started yet, with `standby`, except `hangNetwork`, `disconnectNetwork`, `failDisk`, `loseAnswer`, `failRequest`, and `client`.
- `write`, `read`, `crash`, `close`, `open`, `cutOff`, and the seven clock actions are ignored when the process of the instance has crashed.
- `restart` is ignored when the process has not crashed.
- `close` is ignored when the database is not open.
- `open` is ignored while the database is open, also while a database that has stopped waits for its process to open it again.
- `client` is ignored in a run without a client.

`hangNetwork`, `disconnectNetwork`, and `failDisk` are done whether the process has crashed or not, because the fault belongs to the instance, whichever process runs it.

Where the actions come from:

- `crash` and the clock actions are those of `sim`. `pauseClocks` and `resumeClocks` are its two stops and its two starts together. Buggify is `sim`'s too.
- `restart` starts a new instance of `sim` on the copied files. `close` and `open` call the core as an application does.
- `cutOff`, `outage`, and `calm` are those of `simstore`.
- `loseAnswer`, `failRequest`, `hangNetwork`, `disconnectNetwork`, and the random `damaged` are the engine's own, in the client that wraps the client of `simstore`. They are faults of the network that the contract of the store allows.
- `failDisk` is a fault of `internal/sqlite/faultvfs`, an SQLite VFS that makes file operations fail. The core opens the cache through it by the field `VFS` of `core.Options`.

`loseAnswer` and `failRequest` are one-shot. An armed fault waits, without a time limit, for the first request that matches, and is then used up. A request matches if its purpose is `request`, or `request` is `any`, and if it comes from `instance`, where one is given. `request` is one of `any`, `read`, `take`, `commit`, `renew`, `release`, `upload`, and `download`: see [the purposes](#request). One request trips at most one fault, the one armed first. The caller gets an error that does not say whether the store applied the request, as after a real loss.

### The network and the disk of an instance

`hangNetwork`, `disconnectNetwork`, and `failDisk` each switch one fault of one instance. The fault belongs to the machine of the instance. It holds across a crash and a restart, until an action switches it off. The state shows the three: see [The state at a time](#the-state-at-a-time).

While the network of an instance hangs:

- A request of the instance is not sent. Its caller waits until its own context ends, and then gets an error. The context ends by a timeout of the core, which is 5 s for the manifest and 5 min for a commit object, or by the timeout of the call, whichever comes first. The events are `request` with `hung`, without the number of a request, and `request` with `returned` and the error, when the caller gives up.
- A request that was on its way when the hang started still reaches the store, as `simstore` carries it. If its answer comes back while the network hangs, the answer is lost, and the caller waits until it gives up. The events are `hung`, with the number of the request, and then `returned` with the error.
- A request that started while the network hung gets no answer when the hang is switched off. Its caller still waits until it gives up.

While the network of an instance is disconnected:

- A request of the instance fails at once and is not sent. Its one event is `request` with `disconnected`, without the number of a request.
- A request that is on its way when the disconnect is switched on loses its caller at once, which the store shows as `gave_up`. The caller gets the error of the network. The event is `disconnected`, with the number of the request, in place of `returned`. The request goes on as one whose caller gave up, so the store can still apply a write: see [the life of a request](#request).
- The content of a download that the instance is reading when the disconnect is switched on breaks off with the same error.

If both are on, the disconnect wins. Neither error says whether the store applied the request, so the core counts a renewal of the lease that fails this way as one of unknown outcome. A request that is not sent does not use up an armed `loseAnswer` or `failRequest`.

While the disk of an instance fails, every operation on the files of its cache fails with an I/O error: open, read, write, truncate, sync, size, and lock. Every process of the instance opens its cache through one VFS of `faultvfs`, which makes the operations fail. With `on` as false, no operation fails. The spool files of the commits do not go through the VFS, so they do not fail. The engine keeps the VFSes in a pool across runs: a crashed process leaves its files open, and a VFS cannot be closed while a file that was opened through it is open.

No action makes a disk hang: see [A disk that hangs](#a-disk-that-hangs).

### A stored bucket

`bucket` is what a page keeps of a database between two runs, as S3 would keep it. A run that gets one starts on a store that holds its objects, as an application that starts on a new machine finds its bucket. The simulator page keeps the bucket of its session this way, so that a reload goes on with the same database.

| Field | Meaning |
| --- | --- |
| `version` | The version of the storage that wrote the objects. It must be the version of the engine: see below. |
| `objects` | The objects under the prefix, in the order in which they were last written, each with `key`, `content`, and `at`. `content` is the bytes of the object, as base64 in JSON. `at` is when the object was last written, in milliseconds of the history, as S3 keeps the time of the last modification. |
| `values` | The counter of the workload: the number of values that the inserts before took. The first insert of the run takes the next. |
| `from` | The time of the history at which the run starts, in milliseconds. |

The history is the life of the database across its runs. Its time counts from the start of the first run, and a run on a stored bucket goes on at `from`.

The version of the engine is what `StorageVersion` gives, and the answer to `start` has it: see [The answer](#the-answer). Its form is `<shape>-<16 hexadecimal digits>`. The shape is that of a stored bucket, which is 2, so the version starts with `2-`. The digits are the first 8 bytes of a SHA-256 of a fixed manifest and a fixed commit object as `internal/format` writes them. So the version changes with the versions and the encodings of the formats.

The engine refuses a bucket that no run of this engine could have left. The error is `the bucket is refused: ` and the reason. The engine checks the bucket once the rest of the request has passed, and gives the first reason that it finds. A `start` that is refused this way leaves the session that runs as it is, as any refused `start` does. The engine refuses:

- Another `version` than that of the engine.
- More than 50,000 objects.
- `values` outside 0 to 10^10, and `from` outside 0 to 10^13.
- A key that is there twice, and an `at` that is not finite.
- A key that is neither `<prefix>/manifest.json` nor that of a commit object of the prefix.
- A commit object that cannot be read, also one whose bytes do not match its name.
- A manifest that cannot be read.
- A log from the head of the manifest that is not whole, because the bucket lacks one of its commit objects, or that loops.
- A key that the log inserts twice.
- A key that the log inserts and that is not the barcode of a value from 1 to `values`. The engine undoes the barcode to find the value.
- A delete in the log of a key that the log does not hold there: a key that it never inserts, a key that it inserts only later, and a key that it has deleted before.

A bucket without a manifest is allowed, and so are commit objects outside the log.

The engine seeds the store before any instance starts, through a client of `simstore` with the name `seed`. The client has no sender, so it never crashes.

1. It creates the commit objects, 32 side by side.
2. Once the store holds all of them, it creates the manifest. So the store never holds a manifest whose log it lacks.
3. It records one event `store` with `seeded`: see [store](#store).

The engine creates each object again until `Store.Object` shows that the store holds it, each attempt with a timeout of 10 s of fake time, up to 100 attempts. So a fault of the store delays the seed and does not change it. If the store has not taken an object after 100 attempts, as under faults that make every write fail, such as `lostRequest` at 1, the run ends there: a batch run answers with only `error`, and `start` with only `error`, `the bucket is refused: the store did not take ...`. The session that ran before is then ended, because `start` ends it before the run starts. The seed takes fake time: about 2 s for 1,000 objects with the default latency.

The requests of the seed are not in the trace. The engine drops every event `request` that no request of an instance stands behind, and every cut-off of the client `seed`. A request of the seed can still arrive late, after the seed. The store refuses it then, and its events are dropped too. An outage that a request of the seed starts is in the trace. The numbers `write` of the applied writes count the writes of the seed too.

The instances start at the end of the seed, at the time `from` of the history. `by` of the event `seeded` is the time of the history at the time 0 of the run: `from` less the time of the run at the end of the seed, or 0 if that is less, so the history never goes back before its start. The engine shifts the wall clock of every process of the run by `by` when the process starts, also after a restart, so the clock reads the time of the history. The `wall` of the events and the `renewed` of a lease are then times of the history. `t` still counts from the start of the run. The core reads the `renewed` of a manifest for no decision: it only writes it, in [internal/core/lease.go](../core/lease.go). So the shift changes no decision. It keeps the times that the commit objects and the manifest hold in one history.

The checker takes the writes of the log of the bucket, its inserts and its deletes, as acknowledged before the run, in the order of the log. So every read of the run must see them. The application knows the rows of the state at the head, which are the keys that the log inserts less those that it deletes, in the order of their inserts. It knows no acknowledged delete at the start, so no call picks a key that the log deletes. The fold of the state puts the objects of the bucket into its bucket at the event `seeded`: see [The state at a time](#the-state-at-a-time).

Without a bucket none of this happens: the store starts empty, no event `seeded` comes, and no clock is shifted.

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
| `calls` | The calls that ended, counted by `op` and `what`, such as `"write committed"`, `"delete committed"`, and `"read served"`. |
| `requests` | The events of the kind `request`, counted by `what`. A request counts once under each of its events: `sent`, `applied`, `answered`, and so on. |
| `log` | The number of commit objects in the log at `end`, the same as the log of the state at `end`. |
| `violations` | The events of the kind `check`. |
| `excused` | Those of them that are excused. |

## A live session

A session is one run without an end, which the page drives one command at a time:

1. `start` starts it, with a seed and settings.
2. `advance` moves its fake time on, and answers with the events of the stretch.
3. `act` does an action at its fake time.
4. `now` answers with its state.
5. `end` ends it.

Between two commands fake time stands still, however long the page waits. No task runs, and the scheduler takes no step.

The first task of the run is the controller of the session. It does one command, answers it, and waits for the next. It waits on a channel that is made outside the `testing/synctest` bubble of the run, and a wait on such a channel is not durable. The loop of the scheduler calls `synctest.Wait` before each step, which returns only once every goroutine of the bubble is durably blocked. So while the controller waits for the page, the scheduler starts no task and does not move fake time. For an advance, the controller sleeps through the scheduler, as the first task of a batch run sleeps until its next action, and the other tasks run until it wakes.

The engine does not keep the trace of a session. It folds each event into the state of the session as it comes, and forgets an event once an answer has brought it. A page that needs an event later keeps it.

An engine has one session at most. A command without a session gets an answer with the error `there is no session: start one`.

### The settings

The body of `start` is an object with the fields of a request that hold for a whole run: `seed`, `instances`, `leases`, `latency`, `faults`, `buggify`, `workload`, `client`, `standby`, `prefix`, and `bucket`. Each has the meaning and the limits that [The request](#the-request) gives it. A session has no `end`, no `schedule`, and no `lines`: each of them is an error, as any other member is.

A field that is missing has the default of a request, with one exception: `instances` is 2 in a session, and 3 in a request.

The default of `seed` is 1. A page that wants another run each time picks the seed itself.

`start` ends the session that runs, if there is one, and drops its last answer. A page that wants the last checks and the hash of a session sends `end` first. A `start` whose settings are refused, also for their bucket, leaves the session that runs as it is.

### The commands

`advance` takes exactly one of `by` and `to`, in milliseconds of fake time:

| Field | Limits | Meaning |
| --- | --- | --- |
| `by` | 0 to 1800000 | How far fake time moves on. |
| `to` | 0 or above, and at most 1800000 after the time of the session | The time that fake time moves on to. A time that is not after the time of the session does nothing: the answer has the same `t` and no events. |

The answer to an advance to the time T has the events up to the moment at which the controller wakes at T. Other tasks can have events at exactly T that come after that moment. They are in the answer to the next advance that moves fake time on.

`act` takes an action as [Schedule](#schedule) describes it, without `at`. The action happens at the time of the session. The answer has what the action does at once: its event `action`, and for some actions an effect, such as `crashed` before the event of a `crash`, and `started` and `opened` after that of a `restart`. What other tasks then do, such as the call of a `write`, comes with the next advance. The index `action` of the event counts the actions of the session from 0.

`now` takes `{}`, or `{"commits": N}` with N at 0 or above. Its answer has the state of the session in `state`: see [The state at a time](#the-state-at-a-time). It runs no task, so `t` and `steps` are those of the answer before, and `events` is empty.

`end` takes `{}`. The engine runs the last checks of the invariants, stops recording, and lets the run finish, as at the end of a batch run. Its answer is the last of the session.

A command that the engine refuses gets an answer with `error`, and the session goes on as if the command had not come. The engine refuses a body that is not JSON of the command, a member that the command does not have, a value outside its limits, and an action that a schedule could not hold, such as one for an instance that the session does not have.

### The answer

Every command of a session answers with an object of these fields:

| Field | Meaning |
| --- | --- |
| `error` | Why the command was refused. It is the only field then. |
| `t` | The fake time of the session when the command was done. |
| `events` | The events since the answer before, in the order in which they happened. |
| `steps` | The steps of the scheduler so far. |
| `state` | The state of the session, in the answer to `now`. |
| `changes` | The objects that writes put into the store since the answer before, in the order of the writes: see below. |
| `values` | The counter of the workload now: the number of values that writes have taken, with those of the stored bucket. |
| `version` | The version of the engine's storage, on the answer to `start`: see [A stored bucket](#a-stored-bucket). |
| `ended` | True on the last answer of the session: the answer to `end`, or to the command during which the scheduler failed. |
| `summary`, `hash` | Those of [The response](#the-response), on the last answer. |
| `schedulerError` | The failure of the scheduler itself, on the last answer, if it failed. `t` is then the time at which it failed. |

The events of all answers of a session, one after the other, are the whole trace of the session, each event once and in order. On the last answer, `events` has the violations that the last checks found, if any, and `steps` counts the whole run, also the steps after the end.

Each of `changes` has `key`, `content`, and `at`, as an object of a [stored bucket](#a-stored-bucket) has them. `content` is the object as the store held it right after the write. `at` is the time of the write in the history: the time of the run, plus the `by` of the event `seeded` after a start on a stored bucket.

- The engine records a write at the moment the store applies it, also when its answer is then lost, hangs, or finds no caller, as S3 would hold it.
- The writes of the seed are not among the changes.
- The core never deletes an object, so no change deletes one.
- On the last answer, `changes` has the writes until the run has finished, also those after `end`, such as the releases of the lease by the closes.

A page that keeps `changes` and `values` has the bucket of the session as a stored bucket. `version` is that of the answer to `start`. `objects` are those of the bucket that the session started on, each replaced by its last change, and the objects that the changes added. `values` is that of the last answer. `from` is the `t` of the last answer, plus the `from` of the bucket that the session started on, if it started on one.

The answer to `start` has the first events of the session: the process of each instance has started and opened its database. Its `t` can be above 0, because the scheduler delays the start of the first task of a run, as it delays half of all task starts. With the seed 1 a session starts at 1.537405, and so does a batch run. An advance by 1000 from there ends at 1001.537405, and an advance to 1000 at 1000. After a start on a stored bucket, the event `seeded` comes before the first event of an instance, and `t` is after the seed.

### A session and a batch run

Take a batch request whose schedule lists its actions in the order of their times, and a session with the same seed and the same settings. The session advances with `to` to the time of each action of the schedule, does the action, advances to the `end` of the request, and ends. It then gives the same events, steps, summary, and hash as the batch run. `live_test.go` tests this for three seeds, with every action, every fault, and buggify. For a schedule in another order, only the index `action` of the action events differs.

They are the same because the controller does what the first task of a batch run does, in the same order. The scheduler therefore takes the same steps and draws the same numbers from the seed.

An advance that ends at a time without an action wakes the controller once more than a batch run wakes its first task. That is one more step of the scheduler and one more number drawn from the seed. From that time on, the session takes another turn than the batch run with the same seed, and its turn is as valid.

The state that `now` gives is the fold of the events that the answers have brought so far, at the time of the session. A page that folds the events itself, by the rules of [The state at a time](#the-state-at-a-time), has the same state. The same test checks this after each action.

## The events

Every event has `t`, its fake time, and `kind`. Most have `what`, which says what happened in a word. A field that does not apply is left out, and so is a number that is 0, a boolean that is false, and an empty text. `wall`, `monotonic`, `action`, and `on` are the exception: they are there when they apply, also at 0 and at false.

| Field | Meaning |
| --- | --- |
| `instance` | The name of the instance, which stays the same across a restart. |
| `process` | The name of the process that runs it, such as `a2`. |
| `error` | The error, as text. |
| `text` | What happened, in a sentence of the store or the engine. |

The names of commit objects are 64 hexadecimal digits. The name of the empty log, the head of a new database, is 64 zeros. The key of every object starts with the prefix of the request, which is `db` by default. The manifest has the key `<prefix>/manifest.json`, and a commit object the key `<prefix>/commits/`, the first two digits of its name, a slash, and its name.

### process

The life of a process and of its database.

| `what` | Fields | Meaning |
| --- | --- | --- |
| `waiting` | | With `standby`, at the start of the run: the instance starts once `a` serves. It has no `process`. |
| `started` | `wall`, `monotonic` | A process starts: at the start of the run, at a `restart`, and with `standby` when the instance starts. |
| `opened` | `holder` | It opened its database. `holder` is the ID under which this database holds the lease. Every open gives a new one. |
| `open_failed` | `error` | It could not open its database. The core opens only local files here, so this is a local failure. The process opens the database again every 2 s on its monotonic clock, until an open succeeds or an action closes the database. |
| `stopped` | `error` | The core has stopped for good, with the cause. Each process checks this every 2 s on its monotonic clock, as an application would, then closes the database and opens it again. |
| `closed` | `error`, if the close failed | The close of the database has ended, after a `close`, or after `stopped`. |
| `crashed` | | The process crashed. |

### clock

One event for each clock action that is done, with `what` the name of the action: `shiftWall`, `stopWall`, `startWall`, `stopMonotonic`, `startMonotonic`, `pauseClocks`, or `resumeClocks`.

| Field | Meaning |
| --- | --- |
| `by` | The shift of `shiftWall`. |
| `wall` | The wall clock of the process after the action, in milliseconds from the start of the run, or of the history in a run on a [stored bucket](#a-stored-bucket). It is negative after a shift far enough backward. |
| `monotonic` | Its monotonic clock after the action, in milliseconds since the process started. |
| `wallStopped`, `monotonicStopped` | Whether each clock stands still. |

### core

An event that the storage core logs, taken from its logger as it is. It has no `what`. `event` is its name, `level` is `DEBUG`, `INFO`, `WARN`, or `ERROR`, and `attrs` holds its attributes. A duration in `attrs` is in milliseconds, measured on the monotonic clock of the process. Every event has `db.prefix`, the prefix of the request, and `instance.id`, the holder ID. A failure has `error`.

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
| `caller` | `caller-0`, `caller-1`, and so on for the workload, `visitor` for an action, and `client` for the client. |
| `op` | `write` for an insert, `delete`, or `read`. |
| `value` | The key that an insert inserts, a barcode of 13 digits. A delete has none. |
| `sql`, `args` | The statement of the call, `insert into barcodes (code) values (?)`, `delete from barcodes where code = ?`, or `select code from barcodes where code = ?`, and its argument in a list: the key that the call inserts, deletes, or selects. They are on every event of the call. |
| `expect` | What a read expects, `row` or `none`: see [Workload](#workload). It is on every event of a read. |
| `started` | When the call started. It is on the event that ends the call. |
| `keys`, `position` | What a read that was served saw: its key if it found the row, and its position. |

Every call has the event `started`, and one event when it ends:

| `what` | Meaning |
| --- | --- |
| `committed` | The write is committed, and the caller knows it. |
| `unknown` | The commit ended with an unknown outcome. |
| `not_committed` | The commit failed, and the transaction is not committed. |
| `abandoned` | The write failed before its commit and was rolled back. |
| `served` | The read was served. On a database without the table, with no keys and the error `no such table: barcodes`. |
| `refused` | The instance has no valid lease. `error` ends with the reason of the core. |
| `stopped` | The instance has stopped, or its database is closed. |
| `busy` | The cache was busy. |
| `timed_out` | The timeout of the call ended before the transaction started. |
| `failed` | Any other error, also that the process has no database open. |
| `down` | A call of the client that its target could not take. `error` says why: see [Client](#client). |
| `lost` | A call of the client that was on its target when the process crashed. For a write the outcome is unknown. |

`instance` and `process` of every event of a call are where the call went. The event that starts a call of the client is at the time it was sent. Its call is then made in a task of the process, which can start up to 50 ms later. A call of the client to an instance that has not started has no `process`.

A call of a caller or of a visitor that is on its way when its process crashes has no event that ends it. One of the client ends with `lost`.

### request

A request to the store. Every event of one request has the same `request`, the number that the store gave it, from 1 in the order sent.

| Field | Meaning |
| --- | --- |
| `method` | `get`, `create`, or `swap`. |
| `key` | The key of the object. |
| `purpose` | What the request is for: see below. |
| `bytes` | The size of what a write sends, on `sent` and `failed`, and on `hung` and `disconnected` of a write that is not sent. |
| `version` | The version of the object after a write, on `applied`. The version that a read got, on `read`. The version that the caller gets, on `returned`. |
| `write` | The number of the write among those that the store applied, from 1, on `applied`. The writes of the seed of a stored bucket count too. |
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
- `hung`: the network of the instance hangs. Without `request`, the request is not sent. With `request`, the answer of a request that was on its way is lost. Either way the caller waits until it gives up, and `returned` follows then, with the error.
- `disconnected`: the network of the instance is disconnected. Without `request`, the request fails at once and is not sent, and this is its only event. With `request`, the disconnect came while the request was on its way: the caller gets an error, and the event comes in place of `returned`, after `gave_up`.
- `too_large`: a write above the size limit of the store is not sent. The engine sets no limit, so this does not happen.

A request of a process that crashes gets no `returned`.

`object` on `applied` has one of three members:

- `manifest`: `head`, the name of the newest commit object, and `lease`, which is null or has `holder`, `duration`, `renewed`, and `salt`. `renewed` is the wall clock of the holder at the renewal, in milliseconds from the start of the run, or of the history in a run on a stored bucket. `salt` is in hexadecimal.
- `commit`: `name`, `parent`, `keys`, the keys of the rows that it inserts into `barcodes`, `deletes`, the keys of the rows that it deletes from `barcodes`, `tables`, the tables that it creates, `changes`, the number of its changes, and `statements`, the annotations that the engine wrote into it: one for each statement of the transaction that wrote, in order, each with `sql`, its text, and `args`, its arguments by position. An argument is an integer, a real, or text as itself, a blob as hexadecimal text, or null. `keys`, `deletes`, `tables`, `statements`, and `args` are left out when they are empty.
- `error`: why the object could not be read.

### store

What happens to the store as a whole.

| `what` | Fields | Meaning |
| --- | --- | --- |
| `cut_off` | `instance`, `process`, `for` | The process is cut off from the store for `for`, by the action or by the fault. |
| `outage` | `for` | The store is unreachable for every process for `for`. |
| `calm` | | The store makes no new fault from now on. |
| `seeded` | `version`, `by`, `text` | The store holds the objects of the [stored bucket](#a-stored-bucket) of the request. `version` is the version of the manifest in the store, if the bucket has one. `by` is the time of the history at the time 0 of the run. `text` says how many objects the store holds from before. |

A line of the store that the engine does not know becomes an event of this kind with only `text`. A test fails if one occurs.

### action

One event for each action of the schedule, at its time, and one for each `act` of a session, at the time of the session.

| Field | Meaning |
| --- | --- |
| `action` | The index of the action in the schedule, from 0. In a session, the number of the action among those of the session, from 0. |
| `do` | The name of the action. |
| `what` | `done`, `ignored`, or `failed` for a `restart` whose cache could not be copied. |
| `text` | `done`, or why the action was ignored or failed. |
| `instance`, `process`, `for`, `by` | Those of the action. `process` is the process of the instance when the action came, so the crashed one for a `restart`. It is missing on an action that was ignored for a crashed process. |
| `on` | What the action switched to, true or false, on `workload`, `hangNetwork`, `disconnectNetwork`, and `failDisk`. |

`process` is also missing on an action for an instance that has not started.

The effect has events of its own: `process` with `crashed` just before the action event of a `crash`, `process` with `started` just after that of a `restart`, and `clock` just before that of a clock action. A `close` and an `open` run in a task, and their `process` events come when they end. `hangNetwork`, `disconnectNetwork`, and `failDisk` have no event besides the action event. Their effect shows in the requests and in the core's events that follow.

### client

The client of the run starts: `what` is `started`, at the start of the run, and `instance` is its first target. The action `client` gives the later targets, in its event `action`.

### check

A violation of an invariant, when the checker finds it. `what` is `violated`, `invariant` is the name, `excused` says whether the design accepts it, and `text` says what was found. A violation by a read has `instance` and `process`. See [The invariants](#the-invariants).

### buggify

A point of buggify fired. `what` is `fired`, and `site` is the name of the point. The core has two points today: `core: the lease is renewed early` and `core: the cache fails to commit after the manifest swap`. Whether a point is on for the run, and each time that it does not fire, is in `lines` only.

## The state at a time

The message `state`, with the body `{"t": T}`, answers with the state of the last batch run at the time T. The state is a fold of the events up to and including T, by `StateAt` in `state.go`. It reads nothing else but the stored bucket of the request, if it has one, so a page can fold the events itself. The engine takes no snapshot during a run, because reading a cache or waking a task for one would change the run.

The command `now` of a session answers with the state of the session at its time, in the field `state` of the answer. It is the same fold, of every event of the session so far, which the engine keeps as the session goes. With `{"commits": N}` and N above 0, `bucket.commits` lists only the last N commit objects and `bucket.log` only the last N names, and `bucket.commitCount` and `bucket.logLength` give the numbers of all of them. Without `commits`, or with 0, the two lists are whole and the two counts are missing.

| Field | Meaning |
| --- | --- |
| `error` | Why there is no state: no run yet, or a body that is not `{"t": T}`. The other fields are zero or empty then. |
| `t` | The time. |
| `bucket` | The manifest and the commit objects. |
| `instances` | The instances, in the order of their first events. |
| `requests` | The requests in flight, in the order sent. |
| `calls` | The calls that have started and not ended, by their numbers. |
| `client` | `{"instance": T}`, with T the instance that the client calls now, from the event `client` and the events `action` of `client` that are `done`. Missing in a run without a client. |
| `outageUntil` | The latest end of an outage: the largest `t` + `for` of the events `store` with `outage`. It can lie in the past. |
| `calm` | Whether an event `store` with `calm` has come. |
| `violations`, `excused` | The events `check` so far, and those of them that are excused. |

As in the events, a field that is zero can be missing: `outageUntil`, `calm`, `holder`, `cutOffUntil`, the two flags of the clocks, the three flags of the faults of an instance, the two counts of the bucket, and the `value` of a call.

`bucket`, from the events `request` with `applied`:

| Field | Folded from |
| --- | --- |
| `manifest` | The `object.manifest` of the last one, or null. |
| `manifestVersion` | Its `version`. |
| `manifestWrites` | The number of them. |
| `commits` | Each `object.commit`, once, in the order written, with its `name`, `parent`, `keys`, `deletes`, `tables`, `changes`, and `statements`, the `process` that wrote it, the time `at`, and `inLog`. |
| `log` | The names of the commit objects of the log, from the first to the head: from `manifest.head`, follow `parent` through `commits` to the empty log. `inLog` says whether a commit object is among them. |
| `commitCount`, `logLength` | The numbers of all commit objects and of all names of the log, in a state of `now` that lists only the last of each. |

At the event `store` with `seeded`, the fold puts the objects of the [stored bucket](#a-stored-bucket) into `bucket`, as if the store had applied them then, in the order of the bucket with the manifest last. A commit object from the bucket has an empty `process`. Its `at` is its `at` in the bucket less the `by` of the event, which is its time on the time of the run, where a time before the run is negative. `manifestVersion` is the `version` of the event, and `manifestWrites` counts the seed's write of the manifest.

An instance:

| Field | Folded from |
| --- | --- |
| `instance`, `process` | The last event `process` with `started`. |
| `state` | `waiting` from `waiting`. `starting` from `started`. `running` from `opened`. `failed` from `open_failed`. `closing` from the event `action` of a `close` that is `done`. `stopped` from `stopped`, and from an event `core` with `stop` whose outcome is not `closed`. `closed` from `closed`. `crashed` from `crashed`. |
| `holder` | The `holder` of the last `opened`. A new process starts without one. |
| `lease` | `held` from the event `core` with `lease`, the transition `taken`, and the outcome `taken`. `none` from the transition `lost`, from `released` with the outcome `released`, from the event `core` with `stop`, from `closed`, `stopped`, and `crashed`, and for a new process. |
| `position` | The name of the last commit object in the cache: see below. |
| `wall`, `monotonic` | The clocks at T: see below. |
| `wallStopped`, `monotonicStopped` | Those of the last event `clock`. False for a new process. |
| `cutOffUntil` | The latest end of a cut-off of the process: the largest `t` + `for` of its events `store` with `cut_off`. It can lie in the past. A new process starts without one. |
| `networkHung`, `networkDisconnected`, `diskFailing` | The `on` of the last event `action` that is `done`, of `hangNetwork`, `disconnectNetwork`, and `failDisk` for the instance. A new process keeps them, because the faults belong to the instance. |

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
| `waiting` | True from `sent`. False from `returned`, `answer_lost`, and `disconnected`. |
| `happened` | The `what` of its events after `sent`, in order. |

A request that is not sent, by `failRequest`, a hang, or a disconnect, has no number and is never listed.

A request is listed while `waiting` is true or `phase` is not `done`. So a write that the store holds back stays listed after its caller gave up, and a request without an answer stays listed, as `done`, while its caller waits.

A call in flight has `call`, `instance`, `process`, `caller`, `op`, `value`, `sql`, and `args` from its event `started`, and `started`, the `t` of that event.

A crash ends the calls of its process in the fold, and every wait of the process for a request: its calls leave `calls`, and its requests get `waiting` false. A request of a crashed process stays in `requests` while the network still carries it, until its `phase` is `done`.

## The invariants

The engine checks six invariants during the run and at its end. The end of a session is its command `end`, so a session that is not ended gets only the checks during the run. A violation is an event of the kind `check`. The checker sees what an application and the store see: how each call ends, what each read returns, and the objects that the store applies. A write is an insert or a delete. A key is inserted by one transaction and deleted by at most one, so a key and whether its write deletes tell the transactions apart.

"The log" is the chain of commit objects from the head of the manifest back to the empty log. The rows of the log up to a position are the keys that its commit objects up to there insert, less those that they delete. A read selects the row of one key; its position says which writes its state has besides. After a start on a [stored bucket](#a-stored-bucket), the writes of its log, inserts and deletes, count as acknowledged before the run, in the order of the log.

| `invariant` | Checked | Meaning |
| --- | --- | --- |
| `chain` | At every write of the manifest that the store applies. | The manifest moves its head only to a commit object that is in the store and whose parent is the head before. So the log is one chain. |
| `acknowledged` | When a write ends with `committed`, and at the end. | A commit that was acknowledged is in the log: once when it is acknowledged, and still at the end. |
| `not_committed` | At the end. | A write that ended with `not_committed` is not in the log, and neither is a write of the client that ended with `down`, which was never made. |
| `once` | At the end. | No insert and no delete is in the log twice, and the log deletes a row only at a position where it holds the row. |
| `state` | When a read is served. | The read saw a state that the log had: its position is the empty log or a commit object of the log, and it gives the row of its key, once, if the rows of the log up to that position have it, and no row otherwise. |
| `fresh` | When a read is served, and at the end. | The read saw every write that was acknowledged before the read started. It also saw every write that returned with an unknown outcome before the read started and that the log holds at the end, which includes a write of the client that ended with `lost`. It saw a write, an insert or a delete, of its own key or of another, if the state at its position holds the commit object of the write: the commit object of the position, or one of its ancestors in the store. A read gets at most one violation in each of the two checks. |

`acknowledged` and `not_committed` hold for a delete as for an insert. `fresh` checks only the position of a read, and `state` checks its row at that position. So a read that gives the wrong row, or none, at a position that holds every write that it must see is a violation of `state` alone. A delete that has not ended asks nothing of a read: a read at a position before the delete gives the row, a read at a position after it gives none, and both are legal. A delete of unknown outcome that the log does not hold at the end asks nothing of a read either.

The checker checks a read in a time that does not grow with the log while the log is one chain: it keeps the index in the log of the commit object that inserts each key and of the one that deletes it, and of the latest acknowledged write. A read that this fast check does not pass is checked again against the chain from the head, which decides. A second insert of a key in the log, and a delete of a row that the log does not hold there, end the fast check: the checker then checks every read against the chain from the head. `check_internal_test.go` checks every read of the keys 0 to 4, giving every set of four keys, at every position both ways: both find the same violations, and the fast check passes every read without one. It does so on a log of three inserts, and on that log with a delete of the key 1, once acknowledged and once of unknown outcome, and an acknowledged insert of the key 4 after it.

Only `fresh` can be excused. The design accepts a read that misses a commit on a process whose two clocks both went wrong in the direction that keeps a lease alive too long. A violation of `fresh` is excused if, by the time of the check, the monotonic clock of the read's process has been stopped, and its wall clock has been stopped or shifted backward. `pauseClocks` stops both.

What the checker cannot see, and how far it is tested:

- A read shows one row of its state. `state` checks that row against the log at the read's position, and `fresh` checks the writes by the position. A read that gives the right row of its key and the right position, from a state whose other rows are wrong, is not caught; a read of a range showed many rows of its state at once.

- It sees nothing inside the core: no cache except through the reads, and nothing of what an instance believes about its lease.
- It checks reads that were served. For a write that ended in another way than `committed`, `unknown`, `lost`, `not_committed`, or `down`, such as `abandoned`, it does not check that the log lacks it.
- It can be tested only on input made for it, in `check_internal_test.go`. The core cannot be made to break an invariant from outside its package, and planted bugs are left out of the simulator on purpose. The tests of the runs expect no violation that is not excused.

## What the code does under each action

These are observations of the code as it is on 2026-11-19. Each is from a batch run with the seed 1, two instances, the other defaults of a request, and an `end` of 150000. `a` takes the lease at the start, and `b` is the spare, whose calls the core refuses with `not_holder`. The action comes at 30 s, on `a` unless the line says otherwise, and the action that undoes it at 90 s. Another seed gives other times, and a change to the code can change the course. `run_test.go` checks the course under `close`, `hangNetwork`, `disconnectNetwork`, `failDisk` on the holder, and `pauseClocks`, without the times.

Where a line says that a run has the same events as one without the action, the other run has an action without effect at the same time: `workload` with `on` as true. Any action wakes the first task of the run at its time, which gives the run another turn from there, whatever the action does.

- **`crash`, then `restart`.** Nothing releases the lease. `b` takes it at 64.1 s, once the lease of 30 s has run out by its clocks. The new process `a2` opens its database at once, at 90 s, and is the spare.
- **`close`.** The core stops at 30 s: the event `stop` has the outcome `closed`. It releases the lease at 30.1 s, in an event `lease` whose transition and outcome are `released`, and the process is `closed`. `b` takes the lease at 31.1 s. The calls of `a` end with `stopped` from then on. So a page tells a stop from a crash by the release, and by how soon the other instance takes over.
- **`hangNetwork`.** The upload of a write that starts at 31.9 s is not sent, and neither is the renewal of the lease at 47.7 s. The renewal ends at 52.7 s with the outcome `unknown`, at its timeout of 5 s, and so does each renewal after it. At 57.7 s the lease of `a` ends by its own monotonic clock, and the core refuses its calls with `lease_expired_monotonic`. The write ends with `not_committed` at 61.9 s, at the timeout of its call. `b` takes the lease at 64.1 s. After the hang ends at 90 s, the renewal that started under it still times out, at 94.7 s. At 99.9 s `a` learns that it lost the lease, and is the spare from then on. No invariant is violated.
- **`disconnectNetwork`.** The course is the same, with an error at once where the hang has a wait. The upload of the write fails at once, and the core tries it again until the write ends with `not_committed` at 61.9 s, at the timeout of its call. Each renewal, from 47.7 s on, fails at once with the outcome `unknown`, and the core tries again after 1 s, 2 s, 4 s, and then every 5 s. The lease of `a` ends by its monotonic clock at 57.7 s. `b` takes it at 64.2 s. After the reconnect at 90 s, `a` learns at 94.8 s that it lost the lease. No invariant is violated.
- **`failDisk` on the holder.** Nothing shows until the next commit. At 32.0 s a commit has swapped the manifest, and its step in the cache fails. The core stops with the outcome `local_failure` and the error `local I/O failure: the commit of the cache after the manifest swap: sqlite: disk I/O error (code 778)`. The commit has the outcome `committed_and_stopped`, and its write ends with `committed`: it is in the log. The core releases the lease at 32.1 s. At 34.0 s the process finds the database stopped, closes it, and opens it again, which fails with `open the cache: local I/O failure: sqlite: unable to open database file (code 14)`, and fails again every 2 s. The state of `a` is `failed`, and its calls end with `stopped`. `b` takes the lease at 36.2 s. The disk works again at 90 s, the next open succeeds at 90.3 s, and `a` is the spare.
- **`failDisk` on the spare.** Nothing shows: the run has the same events as one without the action. A spare reads only the manifest. It does not touch its cache until it takes the lease or catches up for a read, and the core refuses its calls before that, because it does not hold the lease.
- **`pauseClocks`, then `resumeClocks`.** `a` has no event at all while its clocks stand still, no call and no request, because every sleep and timeout of the process stands still with its monotonic clock. `b` takes the lease at 64.1 s. After `resumeClocks` at 90 s, both clocks of `a` are 60 s behind, and `a` still believes in its lease. It serves three reads, at 92.0 s, 92.6 s, and 94.8 s, that miss a write that was acknowledged while `a` stood still. Then a commit of `a` has its swap of the manifest refused, `a` reads the manifest, and learns at 95.3 s that it lost the lease. The three reads are violations of `fresh`, and all three are excused: the design accepts them for a process whose two clocks both stood still.
- **`shiftWall` by an hour.** Forward on the holder, the core refuses the calls of `a` with `lease_expired_wall`, 18 of them from 31.9 s on, until `a` renews its lease at 47.8 s. Then `a` goes on as the holder, and `b` never takes the lease. Backward on the holder, the run has the same events at the same times as one without the action. Only the names of the commit objects, the versions, and the `renewed` of the lease differ, because a commit object and the manifest hold the wall clock of the holder. Forward or backward on the spare, the run has the same events as one without the action. So a jump of the wall clock shows only on a holder, and only forward, which is what the builder of the page saw earlier.

### On a stored bucket

These are observations of the code as it is on 2026-11-20. Each is from a session with the page's settings, which are the client on `a`, `standby`, and no callers, and the seed 8, on the bucket that an earlier session left.

- `a` starts with an empty cache, as on a new machine.
- It finds the lease of the earlier holder in the manifest, and takes it only once that lease has run out by its own clocks: about 33 s after it starts, with a lease of 30 s.
- Then it catches up from the empty log. The core downloads the commit objects one after the other, each twice, which takes about 0.13 s of fake time for each commit object at the default latency. Until then it refuses the calls with `not_caught_up`.
- The core has no snapshot, no compaction, and no deletion of old commit objects, so the time of the catch-up grows with the log.

The times, by the number of commit objects in the bucket:

| Commit objects | Seed, fake | Seed, real, natively / Chromium | `a` takes the lease | `a` serves its first call | Catch-up, real, natively / Chromium |
| --- | --- | --- | --- | --- | --- |
| 108 | 0.3 s | 18 ms / 44 ms | 33.7 s | 47.9 s | 29 ms / 103 ms |
| 1,011 | 2.2 s | 326 ms / 355 ms | 35.3 s | 165.1 s | 504 ms / 649 ms |
| 5,002 | 9.9 s | 1.55 s / 1.74 s | 43.1 s | 674.9 s | 2.13 s / 3.32 s |
| 15,025 | 29.4 s | 3.81 s / 6.16 s | 62.6 s | 1,945.7 s | 8.93 s / 14.75 s |

The lease and the first call are at fake times from the start of the run, which includes the seed. The fake times were the same natively and in Chromium. The real times were measured on an Apple M4 with Go 1.27.4 and headless Chrome 158.

A commit object of this workload has 135 bytes, and the manifest 312 bytes. The page's workload writes about 548 objects in a fake hour, which are 74 KB.

## A disk that hangs

No action makes a file operation of a cache hang, in either build. It is not built, for these reasons.

In a browser, SQLite runs on the one thread of its Worker, and a call into it is synchronous. For a file operation to hang while the rest of the simulation goes on, the task inside the call would have to park in the scheduler from inside the VFS, through a call back into Go, as `xSleep` of the VFS calls `s4qlYield`. The frame of SQLite then stays on the JavaScript stack. While it does, no file operation of Go's `os` package can finish, because `fs.js` calls back only from the microtask queue, once the JavaScript stack is empty: see the README of [internal/sqlite/browser](../sqlite/browser/README.md). Every commit of the core creates and removes a spool file with `os.CreateTemp` and `os.Remove`, in [internal/core/spool.go](../core/spool.go). So the first commit of any instance during the hang would block outside the scheduler, which fails the run. The official SQLite build also has `SQLITE_THREADSAFE=0`, and SQLite does not support a call into it by one task while another task is suspended in the middle of a call.

Natively, with cgo, it could be built. The C VFS of `faultvfs` would call an exported Go function that parks the calling task through `sim`, which finds the task by its goroutine. SQLite is thread-safe there. A connection that meets a lock that the hung connection holds would then wait in SQLite's busy handler, in real time.

In a browser it would take one of two things:

- Spool files that do not go through `os`, and calls into SQLite that can be suspended. The first needs an option of `core`, which has one only for its tests, `WrapSpools`. The official build does not offer the second.
- A hang in the Go binding, before a call enters SQLite. That is another fault than a file operation that hangs.

A page must not show a hang of a disk that the code never saw.

## The old page and the engine

The page in `site/simulator/` ran a JavaScript model of the design before it ran this engine. What the model offered that the real code cannot, or can only in another form:

- **Stall and resume.** The model pauses a process while its clocks run. `sim` cannot pause the tasks of a process. `pauseClocks` comes close: it stops both clocks, so every sleep and timeout of the process stands still with them, which is what a stopped process looks like to its own code. The difference is that the clocks of the process stay behind after `resumeClocks`, by the time that they stood still.
- **Clock rates.** The model has a monotonic clock that runs at twice the rate, and both clocks at half the rate. A clock of `sim` is shifted, stopped, and started, and runs at the true rate.
- **A clock set true again.** No action does it. A page can shift a wall clock by the difference that the state shows. A monotonic clock that stood still stays behind for the life of its process.
- **Planted bugs.** They are left out on purpose. The page shows the code as it is.
- **Heal all, as one action.** A page composes it: `calm`, a `restart` of each crashed instance, `open` for each closed database, `resumeClocks` or the two start actions for the clocks, a `shiftWall` back, and `hangNetwork`, `disconnectNetwork`, and `failDisk` with `on` as false. A cut-off or an outage that runs still goes on to its end.
- **Random faults of processes.** The model's chaos crashes, shuts down, and stalls instances at random, and moves their clocks. The engine draws only the faults of the store. A page draws the others from the seed and puts them into the schedule, or sends them to a session.
- **Reconnect.** A cut-off or an outage has its duration from the start, and no action ends it early. A hang and a disconnect of the network of an instance have no duration: `hangNetwork` and `disconnectNetwork` with `on` as false end them when the page says so.
- **Slow S3, switched at a time.** The store takes its faults when the run starts. `slow` holds for the whole run, until a `calm`.
- **Buggify, switched at a time.** It is on or off for the run.
- **The rows of a cache and the state of its disk.** The state has the position of a cache, and whether an action makes its disk fail. A read that is served shows the keys.
- **Unknown outcomes in flight.** The state has no field for them. The events have them: `call` with `unknown`, `core` with `unknown_outcome`, and refusals with the reason `outcome_unknown`.
- **Checks inside the caches.** The model checks that no cache is ahead of the manifest and that replaying the log gives the rows of every cache. The engine's checker sees a cache only through reads, with `state`.

An earlier version of this list had a failing disk in it. The engine has one now, as the action `failDisk`.

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

For the project's site, `make site` writes the files into `site/simulator/engine/`, which `.gitignore` covers, without the demo page, and with `build.json`, `{"commit":"..."}`, which names the commit that the engine is built from. The Pages workflow runs it before it uploads `site/`, with `SITEVERSION` set to the commit, which writes the version into the page in place on its checkout: the page then loads `page.js` and `run.js` with the query `?v=<commit>`, and passes the query of its own URL on to `simulator.js`, which passes it on to the Worker, which puts it on every file that it loads, also `sqlite3.wasm`. So the HTML of one deploy never runs files of another, whatever a cache holds. Without `SITEVERSION` the page stays as it is in git and loads every file without a query. `make site-serve` serves the site on `SITEADDR` without the two headers below, as GitHub Pages serves it, and `make site-check` checks the page in headless Chromium with `sitecheck`: see its package comment.

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

A live session on the same engine:

```js
await engine.start({ seed: 7 });
const { t, events } = await engine.advance(100);
await engine.act({ do: 'crash', instance: 'a' });
const { state } = await engine.now(5);
const { summary, hash } = await engine.end();
```

| Member | Meaning |
| --- | --- |
| `Engine.start(base)` | Starts a Worker and resolves with an engine once it takes messages. `base` is the URL of the directory with the files, by default the directory of `simulator.js`. |
| `run(request)` | Runs the request, an object, and resolves with the response as an object. It rejects with an `Error` that has the response's `error` if the engine refuses the request. |
| `state(t)` | Resolves with the state of the last batch run at `t`. It rejects if there is no run yet. |
| `start(session)` | Starts a session with the settings, an object, by default `{}`, and resolves with the answer to `start`. It ends the session that runs. |
| `advance(ms)` | Sends `advance` with `by`: moves the fake time of the session on by `ms`. |
| `advanceTo(t)` | Sends `advance` with `to`: moves the fake time of the session on to `t`. |
| `act(action)` | Does the action, an object without `at`, at the time of the session. |
| `now(commits)` | Resolves with the answer to `now`, whose `state` is the state of the session. With `commits` above 0, the bucket lists only that many of the last commit objects. |
| `end()` | Ends the session, and resolves with its last answer. |
| `timings` | `ready`: the milliseconds from the start of the Worker until the engine took messages. `run`: the milliseconds that the engine took for the last message, without the transfer and the parsing of the answer. |
| `memory` | The size of SQLite's WebAssembly memory after the last answer, in bytes. |
| `terminate()` | Ends the Worker. |
| `Engine.recycleBytes` | The memory above which `run` and `start` start a fresh Worker first. The default is 64 MiB. |

The static `Engine.start(base)` makes the engine, and `start(session)` on the engine starts a session. Each of the five calls of a session resolves with the answer of [The answer](#the-answer), as an object. It rejects with an `Error` that has the answer's `error` if the engine refuses the command, and the session goes on.

Calls wait for one another: the engine takes one message at a time. If the Worker fails or the program exits, every call that waits rejects. What the program writes to standard output and standard error goes to the console.

SQLite's memory grows from run to run, and in a session with every crash. A process that crashes leaves its SQLite connections open, as a crashed process does, and nothing closes them. The Worker reports the size of SQLite's WebAssembly memory, which never shrinks, because the official build does not export `sqlite3_memory_used`. Before a batch run and before the start of a session, the module ends the Worker and starts a new one if the memory is above `recycleBytes`. That costs one start of the engine, and it ends the session that runs, also when the call is `run`. The other calls of a session never start a new Worker.

### The Worker

A page that does not use the module talks to `worker.js` itself. The Worker loads the scripts in the order that the README of `internal/sqlite/browser` gives, and runs `engine.wasm` with the argument `-test.run=^TestEngine$`. It puts the query of its own URL, if any, on the URL of every file that it loads.

The page posts `{id, kind, body}`. `kind` is `run` or `state` for a batch run, or `start`, `advance`, `act`, `now`, or `end` for a session, and `body` is the message as JSON text or as an object. The Worker posts:

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

Below that the page has a live session:

- "Start" starts a session with the settings in a text field, which are `{"seed": 7}` at first.
- "Advance" moves fake time on by the step, which is in a field in milliseconds and is 1000 at first.
- "Play" advances by the step every 100 ms of real time, until "Pause". It skips a turn while the answer before has not come.
- "Act" does the action in a field, which is `{"do": "hangNetwork", "instance": "a", "on": true}` at first.
- "End" ends the session.

After each answer the page asks for `now` with `commits` at 5. It shows the fake time, the steps, the time that the last answer took, SQLite's memory, the instances now, and the last 200 events of the session.

## Tests

The tests of `internal/simulator` run natively and in Chromium. They show:

- **The request** (`request_test.go`): the default of every field, an object given in part, each refusal with its message, and that one error lists every problem.
- **Determinism** (`simulator_test.go`): the same bytes for the same request twice, and on a second engine. The same hash and events twice for ten seeds with every fault and buggify, with no violation that is not excused. The same events before the time from which two schedules differ, at 40000 and at 61234.5. The same events before the earlier of two ends.
- **Messages** (`simulator_test.go`): a malformed request and an unknown kind get an error, and the engine goes on. A state before any run, and a malformed state, get an error.
- **Actions** (`run_test.go`): the effect of each action, and which actions are ignored.
- **Actions on the holder of the lease** (`run_test.go`), in runs of two instances. Under a hang and under a disconnect of its network, no request of the holder is sent, its renewals end with an unknown outcome, and the other instance takes the lease. A disconnect ends the wait for a request that is on its way. A failing disk stops the core, the opens fail again and again, and the first open after the disk works succeeds. Paused clocks stop the calls and the requests of the process, and every violation after the resume is excused. A close releases the lease, and the other instance takes it within 5 s. A fault of an instance is switched also when its process has crashed, and `pauseClocks` is ignored then.
- **A live session** (`live_test.go`): the same events, steps, summary, and hash as a batch run with the same actions at the same times, for three seeds, with every action, every fault, and buggify, and the state of `now` after each action against the fold of the events so far. The same for three seeds with the client, `standby`, and switches of the client, also before the other instances have started. Fake time does not move between commands: `t` and `steps` stay the same over 50 commands `now`, with `runtime.Gosched` between them. The events of an advance lie in its stretch. A malformed command is refused, and the session goes on. A command without a session gets an error, also after `end`. A new `start` ends the session that runs. `now` with `commits` lists only the last commit objects, with the two counts. The steps, the events, and the bytes of the events of a fake minute, as the mean of minutes 50 to 60 of a session, are less than 15% above those of minutes 10 to 20, with the defaults and with the client, `standby`, and no callers.
- **The client** (`client_test.go`), with `standby` and no callers. `a` takes the lease before `b` starts, and the first call of the client comes after that. The client keeps calling `a` through a crash and a restart, a close and an open, and a failing disk: `down` while `a` cannot take its calls, and `refused` once `a` stands by. A switch moves its calls to `b`, and a call that was on `a` at the switch ends on `a`. The instance that the client does not call gets no call. A write that a hang holds on `a` when `a` crashes ends with `lost` at the crash. Actions on an instance that has not started are ignored, except the faults of the instance. Over 24 seeds with every fault, crashes, closes, and switches, every call ends once on the instance and process it started on, and no violation is not excused. Writes are lost at crashes in those runs, and some of them are in the log at the end: with `lost` counted as `not_committed`, the same runs have violations of `not_committed`.
- **Faults** (`run_test.go`): each fault of the store, damaged downloads, and buggify show their events, with no violation. Under every fault at once, the engine knows every line of the store.
- **The state** (`state_test.go`): the position of each cache, folded from the events of the core alone, against what each read saw. The log. A request and a call in flight, from their start to their end.
- **The checker** (`check_internal_test.go`): each invariant on input made to break it, also by a write of the client that ended with `down` or `lost`, by a read that misses its row, gives another or gives it twice, by one that misses the write of its key, and by one at a state without the write of another key, and the excuse of `fresh`. After a start on a stored log, a read at the empty log violates `fresh`, and a read at the head that misses its row of the stored log violates `state`, both by the fast check and by the slow one. For deletes: an acknowledged delete that the log lacks, by both checks; a delete that failed and that the log holds; a delete that the log holds twice, a delete of a row that the log never held, and a delete before its insert; a read that gives a deleted row at its position, by both checks; a read at a state without an acknowledged delete of its key, or of another key, and its excuse on a process whose two clocks went wrong; a read at a state without a delete of unknown outcome that the log holds at the end; a read that misses a row that the log never deleted; and a stored log that deletes, with reads that agree with it, a read that misses a row of it, and a read before its delete. The fast check and the slow one find the same violations for every read of five keys, also on a log with a delete. The picks of the calls: a delete picks only a known key that no delete has picked, and leaves the last known key alone. No read selects a key while its delete runs. A key in limbo and a key that is known again are deleted no more. One read in eight selects one of the 10 newest acknowledged deletes.
- **The SQL** (`run_test.go`): the first commit object holds the `create table` and the insert. Each later one inserts one key, or deletes one that a commit object before it inserted and none deleted, with the key as argument. The events of a delete have its statement and no `value`, and a delete that is committed is in a commit object. The keys are barcodes of EAN-13, distinct and not rising. Every read gives what it expects, the row of its key or no row for a key that a commit object deleted, or no row where a delete of its key committed while it ran. A run whose first inserts all fail makes only inserts.
- **A stored bucket** (`bucket_test.go`, `bucket_internal_test.go`). A session that ends and starts again on the bucket that it kept goes on, with the page's settings: `a` opens once, reads give rows from before, new inserts take new keys, an instance takes the lease from the earlier holder, and no invariant is violated. The first session deletes, and no call of the second picks a key that the first deleted. The same holds for three seeds with three instances and every fault, with a crash, a hang, a disconnect, a restart, and a lost answer before the second start, and a crash and a `calm` after it. The changes of every answer are the objects of its events `applied`, also for a write whose answer is lost. The same bucket, counter, time, and seed give the same events and hash. The wall clocks and the commit objects start at the time of the history, and the fold has the version of the manifest from `seeded`. A damaged bucket is refused, and the session that runs goes on: another version, the head missing, a commit object in the middle of the log missing, a manifest that is none, a commit object with a byte changed, a key outside the prefix, a counter behind the log, and a key twice. `TestSeed_readLog` takes a log that deletes rows that it holds, and refuses a delete of a row that the log never held, a delete before its insert, and a delete twice. The objects of a database stay under its prefix. Undoing the barcode finds every value up to the counter, and no other key.

`TestEngine` in `internal/simulator/engine` serves a page when one drives it: it looks for the object `s4qlEngine`, which `worker.js` sets. Without the object, as in `make test-browser`, it plays the page with an object of its own: it shows that `submit` refuses a second message while the first waits and a message without its three arguments, and that the answers that `done` gets to a run and to a state of that run are those of an engine called directly, so that one engine serves every message. The whole path through `worker.js` is what `make site-check` checks.

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

Before it moved to a live session, the page of the site ran two instances, the default workload, and no random faults, and asked for a longer run every 2 min of fake time, up to 10 min. Its runs, the mean of five with a crash and a restart, in headless Chrome driven by `playwright-cli` on the same machine: 2 min 70 ms, 4 min 146 ms, 6 min 222 ms, 8 min 300 ms, 10 min 375 ms, 12 min 457 ms, and 15 min 632 ms, of which the transfer and parse of the answer took 1 to 4 ms. A fault that the visitor added held the playback for one run, so the page stopped at 10 min. It now advances a session by the real time that passed, or five times that with turbo, one step for each animation frame, so a step is a few hundred milliseconds of fake time at most.

In the demo page, the request of the demo, 2 min with the preset and six actions, took 166 to 186 ms the first time and 88 to 110 ms after that, of which the transfer and parse of the answer took 1 to 8 ms. Natively the same run takes about 40 ms, so the browser is about 2.3 times slower.

The time grows with the steps of the scheduler, which grow with fake time, instances, and callers. A native profile of a 10 min run put half of the time in handing the baton between the scheduler and its tasks, as the README of `internal/core` measured for its tests, and a tenth in creating the spool files of commits. Encoding the response took 3%. A page that must run faster has these levers: a shorter `end`, fewer instances or callers, and a longer `think`. Calling SQLite through `//go:wasmimport` in place of `syscall/js` would make each call cheaper, at 6 ns against 807 ns by the measurement of step 1; it was not needed for these times.

The size of SQLite's WebAssembly memory, after 60 runs of 1 min with two crashes each: 8 MiB before, 30 MiB after 20 runs, 53 MiB after 40, 76 MiB after 60, so about 0.55 MB for each crashed process. Sixty runs without crashes after them grew it once, to 91 MiB, and then no more. The Go heap after a collection stayed under 1 MB between runs.

The hashes of two requests, the demo's and one with a crash and a restart, are the same natively, in the runner, and in the demo page: `95308c33cd30a785...` and `b82b8ab6e0336832...`. No test compares them across the two builds.

The times of a start on a stored bucket, by the length of its log, are in [On a stored bucket](#on-a-stored-bucket).

### A long session

Measured on 2026-11-20 on the same machine, with Go 1.27.4, natively and in headless Chrome through the runner of `internal/sqlite/browser`, with the workload as SQL and a read of one key. A session with the seed 7 advanced in steps of 30 fake minutes, and at each age five advances of one fake minute were timed. The page's settings are `{"workload": {"callers": 0}, "client": {"instance": "a"}, "standby": true}`; the defaults are two instances with two callers each.

The mean time of one fake minute, as one `advance`, at the age of the session:

| Session | Where | 0 h | 24 h |
| --- | --- | --- | --- |
| The page's settings | natively | 15 ms | 14 ms |
| The page's settings | Chromium | 61 ms | 24 ms |
| The defaults | natively | 34 ms | 39 ms |
| The defaults | Chromium | 55 ms | 96 ms |

A fake minute of the page's settings takes 423 steps at the start and 472 after 24 hours, and the steps were the same natively and in Chromium, to the step. A read is a lookup of one key, so it costs the same however many rows the table has. The engine yields to the host between two rows of a query, a step each, so a read of a range of 100 rows cost about 100 steps, and a fake minute about 1,440.

With deletes, measured natively on 2026-11-20 on a loaded Apple M4 with Go 1.27.4, with the page's settings and the seed 7, a fake minute takes 431 steps at 0.5 h and 433 at 4.2 h, when the log has 2,324 commit objects, against 425 and 431 without deletes. The times were the same within the noise of the machine: the medians were 27.6 ms and 27.5 ms, against 24.6 ms and 34.7 ms without deletes, from three runs each of 30 fake minutes.

`TestEngine_live` also checks that the steps, the events, and the bytes of a fake minute, as the mean of minutes 50 to 60, are less than 15% above those of minutes 10 to 20.

So a fake minute costs the same after an hour as at the start. Four things still grow with a session:

- **The Go heap**, natively by about 6.5 MB for each fake hour with the page's settings: 155.5 MB after 24 hours, and 250.4 MB with the defaults. Most of it is the text trace of `sim`, which has a line for each step, also for each row of a query, and keeps every line for the hash, which cannot be switched off without a change to `internal/sim`. The rest is what `simstore` keeps, which is every applied write with its content and a handle of every task that carried a request, the commit objects of the bucket in the engine's fold and in its checker, the checker's record of every read that was served and of every write that ended, and a map of the manifests by their versions.
- **SQLite's WebAssembly memory**, with every crash. Without crashes it does not grow: it is 8 MiB after an hour. With crashes it grows by about 0.47 MB for each crashed process: from 8.4 MB to 12.1, 25.3, and 36.5 MB after 20, 40, and 60 crashes and restarts in one session. Nothing closes the connections of a crashed process. Closing them needs `CloseCache` of `core` as a normal method, where only its tests have it now, and that is a change to `core`.
- **The time of a step**, with every crash. A crashed process leaves its tasks parked in the scheduler until the run ends, and the scheduler sorts the names of all parked tasks at every step. After 60 crashes a fake minute took 89 ms in Chromium, against about 45 ms without them.
- **The state that `now` returns**, which lists every commit object unless `commits` limits it.

What a page should do about them:

- Limit `now` with `commits`.
- Fold the events itself, or keep only the events that it shows.
- Start a new session, or a new Worker, when the memory or the number of crashes has grown large, for example after a few fake hours. A new session starts with an empty trace and no parked tasks. Only a new Worker gives SQLite's memory back. `simulator.js` starts a new Worker before a `start` once SQLite's memory is above `recycleBytes`, and does not look at the Go heap.
