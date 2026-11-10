# S3 fault injection for a Go engine on Tigris: survey (2026-11-10)

Scope: tools that can inject faults into the S3 traffic of a Go (`aws-sdk-go-v2`) client, preferably in front of the real Tigris endpoint, plus local S3 implementations to use as a fallback. This builds on `research-testing.md` section 5 and does not repeat it.

Tags: **[V]** means verified from a primary source (code I read, official docs, repo metadata). **[I]** means inferred.

Fault types used throughout:
1. error responses (5xx, SlowDown, 412, 409, 404, throttling, malformed or truncated bodies)
2. latency and timeouts
3. **request applied by the store, response lost or replaced** (ambiguous outcome)
4. **delayed delivery** (request lands after the client gave up and maybe retried)
5. connection reset mid-request or mid-response, partial reads (including range GETs)
6. stale reads and stale LIST
7. client process crash at a chosen point

Shallow clones used for source reading are in a local directory (not published).

---

## 0. Bottom line

- **No existing project covers fault types 3 and 4 against a real HTTPS S3 endpoint from `go test` without you writing the injection logic.** [V for each tool below; I for "none exists", based on several searches]
  - The closest are:
    - **mitmproxy** with a Python addon (sidecar). It can do 3 (`flow.kill()` or rewrite in the `response` hook) and 4 (client-replay of a copied flow later), but you still write the addon. [I]
    - **chaotic** (`RomanAgaltsev/chaotic`), a Go `RoundTripper` and aws-sdk-go-v2 middleware. It does 3 only as "rewrite the successful response" and has no 4. It is very young. [V]
- The established systems that test this well (SlateDB, Graft, WarpStream, Tigris itself) all use **their own** wrapper or a simulator (Antithesis). None ships a reusable S3 proxy. [V]
- **Smallest own code:** an `http.RoundTripper` wrapper, about 400–600 lines, plugged in through `s3.Options.HTTPClient`. Plus a tiny crash-point package and a subprocess re-exec helper for 7. Details are in section 5.

---

## 1. Group A: inject in front of a real endpoint

### 1.1 Client-side Go (in-process, best fit for `go test`)

#### Own `http.RoundTripper` (recommended; see section 5)
- aws-sdk-go-v2 accepts any `HTTPClient` (an interface with `Do(*http.Request)`) through `s3.Options.HTTPClient`. The SDK builds a fresh signed request for each retry attempt and sends each one through this client. So a RoundTripper sees every **attempt**, after SigV4 signing, below the retry loop. [V: SDK design; retry middleware in `aws/retry/middleware.go`]
- TLS is a non-issue. The wrapper delegates to a normal `*http.Transport` that talks HTTPS to `t3.storage.dev`. [V]

#### aws-sdk-go-v2 retry facts that shape the injector [V: `aws/retry/standard.go`, `retryable_error.go`, cloned 2026-11]
- Retried by default:
  - HTTP status 500, 502, 503 and 504
  - error codes `RequestTimeout` and `RequestTimeoutException`, plus throttle codes
  - "connection errors": anything implementing `ConnectionError()`, any error whose text contains `connection reset` or `use of closed network connection`, connection refused, and temporary DNS errors
- **Consequence for 3:** after an injected "applied but reset", the SDK **retries the conditional PUT by itself**. The retry gets 412 (`If-None-Match: *`) or 412/404 (`If-Match`). The engine must treat "412 on a retried attempt" as **ambiguous, not lost**. This is the single most important behaviour to test. Count attempts in the injector and expose them to the test. [V for retry behaviour; I for the consequence]
- Injected errors must have the same type and text as real ones, or the SDK classifies them differently. Use `&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}` and a `net.Error` with `Timeout() == true`. [I]
- The SDK has an opt-in new retry behaviour ("retries 2026", throttle-aware backoff) in `aws/retry/jitter_backoff.go`. Pin it explicitly in tests so behaviour does not drift. [V exists; I advice]

#### chaotic, `github.com/RomanAgaltsev/chaotic` (MIT, created 2026-06, 0 stars, single maintainer, active to 2026-11) [V: repo metadata, source read]
- Adapters:
  - `adapter/http.WrapTransport(rt, engine)` (a RoundTripper)
  - `adapter/aws` (smithy middleware at the Finalize step by default, or the Build step, so injected errors go through SDK retries)
  - `net`, `io`, `sql`, and others
- Fault primitives:
  - `Latency`, `Jittered`, `Error`, `Panic`
  - `ConnDrop` (hard reset), `Disconnect` (FIN)
  - HTTP status synthesis, header faults
  - stream `SlowReader`, `SlowWriter`, `Truncate(n)` (through the io adapter)
  - `MutateResponse(fn)`
- Rules match on op kind, method, path regex and time windows. There are sequence/staged/sticky rules and rate limits. Probability rules take a seed (PCG). [V: `engine/rule.go`]
- **Fault 3:** all faults except header faults and `MutateResponse` run in `Before` and **abort without forwarding**. `MutateResponse` runs after a successful real round-trip, so you *can* turn an applied 200 into a 503 or a broken body. You *cannot* turn it into a transport error or timeout after forwarding. [V: `adapter/http/transport.go`]
- **Fault 4:** not supported. [V]
- Fault 5: `ConnDrop` happens before forwarding only. A truncated body would need the io adapter or a mutate wrapper. [V/I]
- Verdict: usable as a rule engine, but too young to depend on for the core risk. Borrow ideas from it, or vendor it. [I]

#### jhonsferg/relay `ext/chaos`, falzm/chaos [V: search, repo metadata]
- relay chaos is middleware for the relay HTTP client only.
- falzm/chaos is *server-side* middleware and is **archived**.
- Neither fits.

#### netchaos, `github.com/jpgomesr/netchaos` (MIT, v0.4.0 RC, active) [V: README]
- A **simulated** in-memory `net.Conn`/`Listener` network with seeded latency, loss, corruption, partition and mid-stream reset, designed for `testing/synctest`.
- It is not a proxy onto real sockets, so it cannot reach Tigris. It is useful only against an in-process fake, such as Versity embedgw on a netchaos listener. [I]

#### Go failpoints for 7 (client crash points)
- **etcd-io/gofail** (Apache-2.0, active 2026-11) [V: README]
  - Failpoints are comments (`// gofail: var X struct{}`) that `gofail enable` rewrites into code. Revert with `gofail disable`.
  - Control comes from the `GOFAIL_FAILPOINTS` env var (`name=panic`, `sleep(10)`, `return(...)`, counts) or the HTTP endpoint `GOFAIL_HTTP`.
  - etcd's robustness tests use it to crash members at chosen points.
- **pingcap/failpoint** (Apache-2.0, active 2026-10) [V: README]
  - Uses `failpoint.Inject("name", func(){...})` markers, which need `failpoint-ctl enable` code rewriting or `-toolexec failpoint-toolexec` with a separate GOCACHE.
  - Terms: `[p%][n*]return|sleep|panic|pause|print|off`. Enable in tests with `failpoint.Enable` or `GO_FAILPOINTS`.
- Both are storage-agnostic. Both make you choose between code generation and build-cache juggling. For a new codebase, a 30-line package of your own works as well [I]:
  - `crashpoint.Hit("commit/after-manifest-put")`, a no-op unless built with a `crashpoints` tag or enabled through env
  - an action of `os.Exit(137)` or `syscall.Kill(os.Getpid(), SIGKILL)`, so no deferred cleanup runs
- A crash must kill a **process**. Run the engine in a child process by re-exec'ing the test binary (`exec.Command(os.Args[0], "-test.run=^TestChildWorkload$")` plus an env var), then reopen from the bucket in the parent and run the fsck. [I]
- **Graft does exactly this** (Rust): `precept::sometimes_fault!("RemoteCommit: before commit" / "after commit", fault_crash())`, where `fault_crash` calls `process::exit(0)` or panics. "After commit" is the crash-version of fault 3. [V: `crates/graft/src/rt/action/remote_commit.rs`, `fault.rs`]

### 1.2 Proxies (sidecar or embedded)

#### Toxiproxy (Shopify, MIT, v2.13.0 on 2026-10-14, active) [V: repo, README, CHANGELOG]
- Toxics:
  - `latency`, `bandwidth`, `slow_close`
  - `timeout` (stop data, close after N ms; 0 means black-hole)
  - `reset_peer` (RST, optionally after a timeout)
  - `slicer`, `limit_data` (close after N bytes), `packet_loss`
- Each toxic is directional (upstream or downstream) and has a `toxicity` probability. There are **no HTTP-level toxics**: no status codes, no per-request matching. [V]
- **TLS re-check:** this works fine from Go without cert hacks. Keep the real URL (`https://<bucket>.t3.storage.dev`) and give the `http.Transport` a `DialContext` that dials the toxiproxy listen address. TLS (SNI and cert check) then runs end to end with Tigris, and toxiproxy relays ciphertext. Use one proxy per upstream host, or path-style addressing. [I; standard Go technique]
- **Embeddable in-process:** module `github.com/Shopify/toxiproxy/v2` exports `NewServer(NewMetricsContainer(nil), logger)`, `NewProxy(server, name, listen, upstream)`, `(*Proxy).Start()` and `ToxicCollection.AddToxicJson`. [V: `api.go`, `proxy.go`, `metrics.go`]
- Fault coverage:
  - 2: yes.
  - 3: approximately. Use a downstream `limit_data` or `reset_peer` on an already warm connection: once response bytes flow, the server has processed the request. This is timing-dependent and connection-scoped, not request-scoped, because the stream is encrypted and connections are pooled. [I]
  - 5: yes, but you cannot aim it at a byte offset inside the plaintext body. [I]
  - 1, 4 and 6: no. Not seeded. [I: uses `math/rand`]
- Its value: it produces *real* kernel and `crypto/tls` errors, which checks that your error classification works on genuine failures. Use it as a complement to the RoundTripper injector, not a substitute. [I]

#### Envoy HTTP fault filter (Apache-2.0) [V: envoy docs]
- Faults: `abort` (HTTP or gRPC status), `delay`, and `response_rate_limit` (KiB/s on the response body). Each has a percentage, and you can match on upstream cluster and headers.
- Header-controlled faults:
  - `x-envoy-fault-abort-request`, `-abort-request-percentage`
  - `x-envoy-fault-delay-request`, `-delay-request-percentage`
  - `x-envoy-fault-throughput-response`, `-throughput-response-percentage`
- Per-request scripting from Go: add these headers in a RoundTripper. They are not in SigV4 `SignedHeaders`, so adding them after signing is safe. [I]
- Abort returns a local reply and **never forwards**. Delay happens before forwarding. If the client gives up during the delay, Envoy resets the stream and does not deliver late. So **no 3 and no 4** from the fault filter. [V for the filter's fault list; I for the timing]
- A Lua filter (`envoy_on_response`) could rewrite `:status` after the upstream answered, which gives the "replaced with an error" form of 3. That means you write Lua. [I]
- TLS: run the client in plain HTTP with `Host: <bucket>.t3.storage.dev` (via a custom dialer) and let an Envoy cluster originate TLS to Tigris. SigV4 then stays valid, because the Host header is unchanged. [I]
- Cost: a container or `func-e` binary per CI job, plus YAML. This is heavy for what it adds. [I]

#### mitmproxy (MIT, very active) [V: modes doc; I for addon specifics]
- Reverse mode: `--mode reverse:https://t3.storage.dev` accepts plain HTTP *or* HTTPS from the client and talks HTTPS upstream. [V]
- It rewrites the `Host` header by default. You need `--set keep_host_header=true`, or SigV4 breaks. [V: docs say Host is rewritten unless `keep_host_header`]
- Python addon hooks: `request`, `responseheaders`, `response`, plus async hooks. [V for hook list]
  - 1: set `flow.response` in `request`. [I]
  - 2: `await asyncio.sleep`. [I]
  - **3:** in `response` (after the upstream has fully answered), `flow.kill()` or replace the status/body. [I]
  - **4:** copy the flow, kill the client side, and later run `replay.client` on the copy. [I]
  - 5: truncate `flow.response.content` or kill mid-stream with streaming. [I]
  - 6: replay a cached older response. [I]
- Determinism: seed `random` in the addon. Drive rules from Go through a small control endpoint in the addon, or through request headers. [I]
- Catch: a Python runtime in CI and on macOS, a sidecar process, and an addon of about 150 lines you still write. It is the most capable *off-the-shelf proxy* for 3 and 4. It is not less code than doing the same in Go. [I]

#### gaul/s3proxy (Apache-2.0, 4.1.1 on 2026-09-10, active) [V: README, source]
- Middlewares: alias, prefix, bucket-locator, **eventual-consistency** (writes to one store, reads from another, copies asynchronously with configurable delay and probability), large-object mocking, **latency** (per-op latency and bandwidth), read-only, regex, sharded, storage-class, user-metadata, no-cache, encrypted. **No error or failure injection middleware.** [V: `middleware/` listing, `EventualBlobStore.java`, `LatencyBlobStore.java`]
- It is a gateway (it re-parses and re-issues requests), not a transparent proxy. Fronting Tigris through its `aws-s3-sdk` backend changes the request semantics. [V]
- Conditional PUT [V: `S3ProxyHandler.java`]:
  - native for azureblob, aws-s3 and GCS
  - `If-None-Match` native on `filesystem` and `transient`
  - other backends either refuse with NotImplemented or emulate in-process
  - CompleteMultipartUpload conditionals are per backend
- Covers 2 and 6 only, as a local fake (group B), not in front of Tigris. JVM or Docker.

#### Chaos Mesh HTTPChaos [V: docs]
- `abort`, `delay`, `replace` and `patch`, on target `Request` **or `Response`**. Response `replace.code` is a genuine "applied, then replaced" fault (3). TLS needs v2.8.5 or later and a CA.
- **Kubernetes only**, so it does not fit `go test`. [V]

#### AWS Fault Injection Service [V: FIS actions reference]
- `aws:fis:inject-api-internal-error`, `-throttle-error` and `-unavailable-error` support only the **`ec2` and `kinesis`** namespaces.
- The only S3 action is `aws:s3:bucket-pause-replication`.
- `aws:network:disrupt-connectivity` with scope `s3` blocks a VPC subnet's traffic to the regional S3 endpoint.
- AWS-only, IAM-role/VPC scoped, useless for Tigris and for `go test`.

#### Other "chaos S3" projects found [V: search, repo metadata]
- `rustfs/s3chaos` (Apache-2.0, 8 stars): a Kubernetes and Chaos Mesh framework for testing **RustFS clusters** (disk, partition, pod kill). It is not a client-side S3 proxy.
- `paperclipai/paperclip` `storage-proxy.ts` (PR #20288, merged 2026-11): an in-app TypeScript S3 fault proxy.
  - It keeps the signed Host header and matches rules by method, path and remaining count.
  - Faults: outage 503, latency with jitter, 5xx/SlowDown rate, reset, ExpiredToken.
  - **No applied-but-lost and no delayed delivery.** It is another example of a team writing its own.
- `jaswanthrkumar4906/go-chaos-proxy`: a toy reverse proxy with random 503s and latency.

### 1.3 What object-store-backed databases actually do

| System | Mechanism | 3? | 4? | Reusable from Go? |
|---|---|---|---|---|
| **SlateDB** (Rust, Apache-2.0) | `slatedb-dst::FailingObjectStore` wraps any `object_store::ObjectStore`. Seeded (`DbRand`) toxics: `Latency{latency,jitter}`, `Bandwidth`, `ResetPeer`, `SlowClose`. Direction is `Upstream` (before the inner call) or `Downstream` (**after the inner call returned**). Filters by `Operation` enum and `path_prefix`, with a probability. `HttpFailBefore{percentage,status_code}` gives synthetic HTTP errors before dispatch. Mock clock. Seeded current-thread Tokio. `publish = false`. [V: `slatedb-dst/src/failing_object_store.rs`, README] | **Yes**: a Downstream `ResetPeer` on `PutOpts` runs after `inner.put_opts` succeeded | No | No (Rust, unpublished). **Copy the design**: the toxic struct maps almost 1:1 to a Go rule |
| **Graft** (Rust, Apache-2.0) | `precept` (Antithesis-compatible fault catalog) `sometimes_fault!` crash points before and after remote commit. Storage through OpenDAL. [V] | crash after commit | — | Pattern only |
| **Apache OpenDAL** `ChaosLayer` | `ChaosLayer::new(error_ratio)`. "For now, ChaosLayer only injects read operations": an EOF error on reads. RNG is seeded from the OS, so it is not reproducible. [V: `core/layers/chaos/src/lib.rs`] | No | No | No |
| **Rust `object_store`** | `ThrottledStore` (per-call, per-byte and per-entry sleeps) and `LimitStore` (concurrency). **No chaos or error wrapper.** [V: `src/` listing, `throttle.rs`] | No | No | No |
| **Neon** `remote_storage::UnreliableWrapper` | Fails the first N attempts of each (op, path), optionally with a probability, **before** calling inner. Unseeded `rand::rng()`. Crash points use the `fail` crate. [V: `libs/remote_storage/src/simulate_failures.rs`] | No | No | Pattern only |
| **Grafana Mimir** `ErrorInjectedBucketClient` | Wraps the Thanos `objstore.Bucket`. An injector func `(op mask, name) -> error` runs **before** the call. AGPL-3.0. [V: `pkg/storage/bucket/error_injected_bucket_client.go`] | No | No | Go, but AGPL and a different interface |
| **Thanos objstore** | No fault helpers (`inmem` bucket only). [V: grep of the clone] | — | — | — |
| **Litestream** | Build-tagged `chaos_test.go` with a hand-written `chaosReplicaClient` that fails before the call. Uses `httptest` servers for S3 client unit tests. [V] | No | No | Pattern only |
| **WarpStream** | Whole system in Antithesis, with LocalStack as S3. Network faults and pauses. [V: prior notes] | yes (network) | yes (network) | Commercial, no internet, so no Tigris |
| **Tigris** | Tigris itself was tested in Antithesis. [V: prior notes] | — | — | — |
| turbopuffer, Delta-rs, Iceberg | No public fault-injection tooling found. Delta and Iceberg test suites use MinIO or LocalStack containers. [I: search only] | — | — | — |

---

## 2. Group B: local S3 implementations (fallback, fast runs, Antithesis)

| Server | License, maintenance | Conditional fidelity | Faults built in | Run under `go test` |
|---|---|---|---|---|
| **Versity Gateway** (posix backend) | Apache-2.0, v1.11.0 on 2026-11-06, very active [V] | `checkPutPreconditions` is re-checked **under a per-object publish lock** (flock by default, or fcntl/local) for PutObject, **CompleteMultipartUpload**, and CopyObject. Conditional DeleteObject and DeleteObjects are supported (If-Match, also with size and last-modified). There are tests for conditional writes over delete markers. **No 409 ConditionalRequestConflict** (it serializes instead). [V: `backend/posix/posix.go` lines 2736, 3029, 4783, 6894; `objlock.go`; `s3err.go`] | None | **In-process**: package `embedgw` runs the gateway as a library ("only a single gateway instance per process"), so start it once in `TestMain`. Also a single static binary or container. Go module, `go 1.26`. macOS needs xattrs (APFS has them) [I] |
| **SeaweedFS** | Apache-2.0, 4.53 on 2026-11-08, very active [V] | Wiki: `If-Match` and `If-None-Match` on PUT, POST (CompleteMultipartUpload) and COPY. DELETE takes only `If-Match`. "The conditional check and the write are atomic — cluster-wide" (per-path lock on the owner filer). 412 on failure, no 409 mentioned. [V: wiki S3-Conditional-Operations] | None | `weed mini` (single process; `weed/command/mini.go` exists [V]) or a container. Seconds to start [I] |
| **RustFS** | Apache-2.0, 1.0.4 on 2026-10-25, very active [V] | Supported, but **recent bug churn** [V: issue titles]: #6798 "preserve atomic 1 MiB conditional writes" (Aug 2026), #8625 "return 412 for conditional multipart uploads on existing keys" (Oct 2026), #9081 (open) "If-Match succeeds on a delete marker; S3 returns 404" | None (s3chaos is a separate k8s harness) | Binary or container |
| **Adobe S3Mock** | Apache-2.0, 5.2.3 on 2026-09-19, active [V] | If-Match and If-None-Match on PUT, CompleteMultipartUpload (MultipartController) and copy-source. DeleteObject If-Match, including `x-amz-if-match-size` and last-modified. [V: controller source] Validation runs in the controller before the store call, so it is probably **not atomic under concurrency**. [I] | None | JVM or container (slow start) |
| **moto** (server mode) | Apache-2.0, very active [V] | PutObject If-Match and If-None-Match; DeleteObject If-Match; CompleteMultipartUpload **only `If-None-Match: *`** (no If-Match). Check-then-act in Python. [V: `moto/s3/responses.py`] | None | Python process or container |
| **gaul/s3proxy** (transient/filesystem) | Apache-2.0 [V] | `If-None-Match` native; If-Match emulated in-process [V] | **latency, eventual consistency** (2, 6) [V] | JVM or container |
| **gofakes3** | MIT [prior notes] | PutObject only; no CompleteMultipartUpload or Delete conditionals; no 409. Forks: only `rclone/gofakes3` (27 stars) is maintained, and it adds no conditionals [V: fork list, code search] | None | In-process `http.Handler` |
| **Ceph RGW** | LGPL | Implements conditional PUT, multipart and delete, judging by s3-tests coverage [I] | None | Heavy (a cluster via `vstart` or containers). Not worth it for CI [I] |
| **Garage** | AGPL-3.0 | **No conditional writes**: "structurally impossible to implement in Garage due to the lack of a consensus algorithm" [V: known-issues page] | — | **Exclude** |
| **MinIO CE**, **LocalStack** | archived / needs a token [prior notes] | — | — | Exclude |

**Tigris local emulator:** none exists. Searches and the docs index show no emulator, dev server or test mode. [V: docs `llms.txt` index; search]

**Ranking for the fallback store:**
1. **Versity embedgw**: Go, in-process, atomic conditionals including multipart, and it ships its own conformance suite.
2. **SeaweedFS `weed mini`**: also the best candidate for the container inside Antithesis.
3. Then RustFS or S3Mock. [I]

---

## 3. Tigris specifics [V: docs.tigrisdata.com, fetched 2026-11-10]

- **Conditionals page:**
  - `If-Match`, `If-None-Match` (`"*"` means create-only), `If-Modified-Since`, `If-Unmodified-Since`, and the copy-source variants.
  - The error table lists "412 Precondition Failed … (PUT, DELETE, or GET requests)".
  - It says **nothing about CompleteMultipartUpload conditionals, conditional-delete headers, or 409**. Measure these yourself.
- **Consistency page:** "Compare-and-set (If-Match, If-None-Match) and other conditional operations always evaluate against the latest state."
  - Global and Dual-region buckets are eventually consistent cross-region.
  - Single-region and Multi-region buckets are strong everywhere.
  - The `X-Tigris-Consistent` header no longer appears on either page. Treat it as legacy.
- **S3 compatibility page:** generated from `tigrisdata-community/s3-api-compat-tests` (Go, 67 ops, AWS JS SDK numbers quoted: Tigris 61/68). The page shows only **GET** conditionals. The suite itself has only `GetObject_IfMatch` and `GetObject_IfNoneMatch`, with no conditional-write tests, and the repo declares no license. [V: repo contents]
- **No fault-injection headers, test endpoints, documented error-code list, or documented rate limits.**
  - A third-party profile claims 503 SlowDown on hot prefixes.
  - A Tigris-side fly.io forum answer says there is no request limit and that 5xx means a server issue.
  - Neither is authoritative, so treat SlowDown as possible but unspecified. [V: absence in docs index; V for the two secondary claims and their conflict]
- **Useful for tests:**
  - **Snapshots and forks.** Create a snapshot-enabled bucket with the header `X-Tigris-Enable-Snapshot: true` on CreateBucket. Take a snapshot with `X-Tigris-Snapshot: true; name=…`; the version comes back in `X-Tigris-Snapshot-Version`. Forks are zero-copy, from the current state or a snapshot. The Go examples use `s3.Options.APIOptions` with `http.AddHeaderValue`. [V: `buckets/snapshots-and-forks.md`, `forks.md`]
  - Per-test or per-CI-job forks of a seeded fixture bucket give isolation cheaply.
  - Snapshotting the bucket when a test fails preserves the exact state for the fsck and for repro. [I]
- **Delayed delivery (4) against Tigris** depends on how long a SigV4-signed request stays valid. AWS rejects requests skewed by more than 15 minutes (`RequestTimeTooSkewed`). Tigris's window is undocumented. Keep injected delivery delays in seconds, which is enough to outlast client timeouts and retries. [V for AWS from general knowledge; I for Tigris]

---

## 4. Conformance suites to diff Tigris against fakes

| Suite | Lang | Conditional coverage | Notes |
|---|---|---|---|
| **ceph s3-tests** | Python, boto3, pytest | `test_put_object_if_match`, `test_put_current_object_if_none_match`, `test_multipart_put_object_if_match`, `test_multipart_put_current_object_if_none_match`, `test_delete_object_if_match*` (incl. `_size`, `_last_modified_time`, versioned), `test_delete_objects_*if_match*`, `test_atomic_conditional_write_*`, `test_atomic_dual_conditional_write_*`; pytest marker `conditional_write` [V: grep of the clone] | **The `ceph/s3-tests` repo was archived on 2026-10-12. The tests moved into `ceph/ceph` at `src/test/rgw/s3-tests`** [V: README notice, contents API]. Needs a config with two users. Many tests are RGW-specific, so filter by marker |
| **versitygw test** (`versitygw test --endpoint … --access … --secret … full-flow`) | Go CLI | `PutObject_conditional_writes`, `CompleteMultipartUpload_conditional_writes`, `CopyObject_conditional_writes(_precedence)`, `DeleteObject_conditional_*`, `DeleteObjects_conditional*`, versioning variants, bucket-policy `s3:if-none-match`/`s3:if-match` conditions [V: `tests/integration`, `cmd/versitygw/test.go`] | Takes any endpoint. Whether every test passes against a non-Versity server is untested, so expect to skip some [I] |
| **versitygw `tests/conditionalrace`** | Go test | 32-way concurrent `If-Match` and `If-None-Match: *` races, expecting exactly 1×2xx and 31×412, three rounds. Also round-trip etag checks and a 20 MiB multipart SHA-256 check [V: file header] | Env `VERSITY_ENDPOINT`, `VERSITY_ACCESS_KEY`, `VERSITY_SECRET_KEY`, `VERSITY_BUCKET`. Works against any endpoint, including Tigris [I]. **Run this against Tigris early** |
| **tigrisdata-community/s3-api-compat-tests** | Go | GET conditionals only [V] | Baseline compatibility only |

Recommended: a small **own contract test** (`internal/s3contract`), built on the conditional-race idea above plus the AWS semantics in `research-testing.md`.
- Cover: 412 vs 404 for `If-Match` on a deleted key; 409 on concurrent delete; CompleteMultipartUpload with `If-None-Match` and `If-Match`; conditional delete; ETag quoting.
- Run it against Tigris (nightly), Versity embedgw and the own in-memory fake (every PR).
- Diff the results. A divergence is a bug in the fake or a documented Tigris quirk. [I]

---

## 5. What to build (smallest own code) [I unless noted]

### 5.1 `s3fault`: an `http.RoundTripper` injector (about 400–600 lines including tests)
Wraps a normal `*http.Transport`. Installed with `s3.New(..., func(o *s3.Options){ o.HTTPClient = &http.Client{Transport: s3fault.New(base, plan)} })`. It works the same against Tigris, Versity embedgw and gofakes3.

**Classifier** (about 60 lines). It derives `Op` from method, query and headers:
- query keys `uploads`, `uploadId`, `partNumber`, `list-type=2`, `delete`, `tagging`
- headers `If-Match`, `If-None-Match`, `Range`, `x-amz-copy-source`
- the key comes from the path (path-style) or the host prefix (virtual-hosted)

**Rule:** `{Op set, key glob, Nth (per-(op,key) counter), probability, Action}`. Draws come from a `math/rand/v2` PCG seeded from `-s3fault.seed` (print the seed on failure). Use per-(op,key) counters, not a global counter, so goroutine scheduling does not change which request a rule hits.

**Actions:**

| Action | Fault | Mechanism |
|---|---|---|
| `FailBefore{Status, Code, Body}` | 1 | Synthesize an S3 XML error response (`<Error><Code>SlowDown</Code>…`) without forwarding. The body can be malformed or truncated |
| `Delay{Before or After, d}` | 2 | Sleep, respecting `req.Context()`. A timeout is the client's own deadline expiring |
| **`ForwardThenFail{Kind}`** | **3** | Call inner, **read and close the whole real response** (so the store has committed and replied), record the true outcome in the trace, then return a synthetic `Status` (503/500), a reset `*net.OpError{ECONNRESET}`, a timeout `net.Error`, or a 200 whose body errors after N bytes |
| **`DeliverLate{d}`** | **4** | Buffer the request body (bytes, with the signed `x-amz-content-sha256` intact), return a timeout or reset to the caller at once, and in a goroutine **re-send the identical signed request** with `context.Background()` after `d`. Record its real outcome. The SDK's retry, a fresh signed attempt, races it. The test waits for late deliveries to drain (`plan.Wait()`) before asserting |
| `CutRequest{afterBytes}` | 5 (upload) | Wrap `req.Body` so it fails after N bytes. The Go transport aborts the upload mid-stream for real |
| `CutResponse{afterBytes}` / `CorruptResponse` | 5 (download, range GET) | Wrap `resp.Body`. Return `ECONNRESET` or `io.ErrUnexpectedEOF` after N bytes, or flip bytes. This exercises checksum validation |
| `Stale{versionsBack}` | 6 | Cache recent GET/HEAD/LIST responses per key or prefix, and serve an older one |
| `Crash{point}` | 3+7 | Call `crashpoint.Trigger` after forwarding and before returning, in a child process |

**Trace:** an append-only log of `{seq, op, key, attempt, rule, injected, real_status, real_etag}`. Tests and the fsck use it to decide what "should" be in the bucket. It is also the replay artifact.

Real-store determinism caveat: the seed fixes *fault decisions*, not Tigris latency or interleaving. For exact replay, run the same plan against the in-process fake.

### 5.2 `crashpoint` (about 30 lines) plus a re-exec helper (about 60 lines)
Covers fault 7. Use gofail instead only if you want its env and HTTP control and are fine with codegen.

### 5.3 Optional: embedded Toxiproxy for TCP realism
Dial through it with the custom `DialContext` trick (section 1.2) to get real RST, black-hole and truncation errors from the kernel and TLS stack.

### 5.4 What to reuse
- the stdlib `net/http` RoundTripper
- SlateDB's toxic model (Upstream/Downstream × op filter × path prefix × probability) as the rule schema
- chaotic's rule-engine ideas (sequence, sticky, per-rule budgets), vendored or as inspiration
- Versity `embedgw` and `tests/conditionalrace`
- ceph s3-tests and `versitygw test` for differential conformance
- Tigris forks and snapshots for isolation and forensics

---

## Sources
- SlateDB DST: https://github.com/slatedb/slatedb/tree/main/slatedb-dst (`src/failing_object_store.rs`, `README.md`)
- OpenDAL ChaosLayer: https://github.com/apache/opendal/blob/main/core/layers/chaos/src/lib.rs
- Rust object_store throttle: https://github.com/apache/arrow-rs-object-store/blob/main/src/throttle.rs
- Neon UnreliableWrapper: https://github.com/neondatabase/neon/blob/main/libs/remote_storage/src/simulate_failures.rs
- Mimir ErrorInjectedBucketClient: https://github.com/grafana/mimir/blob/main/pkg/storage/bucket/error_injected_bucket_client.go
- Graft crash points: https://github.com/orbitinghail/graft/blob/main/crates/graft/src/rt/action/remote_commit.rs, precept: https://github.com/orbitinghail/precept
- Litestream chaos test: https://github.com/benbjohnson/litestream/blob/main/cmd/litestream-vfs/chaos_test.go
- chaotic: https://github.com/RomanAgaltsev/chaotic, https://pkg.go.dev/github.com/RomanAgaltsev/chaotic/adapter/aws
- netchaos: https://github.com/jpgomesr/netchaos
- Toxiproxy: https://github.com/Shopify/toxiproxy (README toxics, CHANGELOG 2.13.0)
- Envoy fault filter: https://www.envoyproxy.io/docs/envoy/latest/configuration/http/http_filters/fault_filter
- mitmproxy modes: https://docs.mitmproxy.org/stable/concepts/modes/, events: https://docs.mitmproxy.org/stable/api/events.html
- s3proxy middlewares: https://github.com/gaul/s3proxy (README "Middlewares", `src/main/java/org/gaul/s3proxy/middleware/`)
- Chaos Mesh HTTPChaos: https://chaos-mesh.org/docs/simulate-http-chaos-on-kubernetes/
- AWS FIS actions: https://docs.aws.amazon.com/fis/latest/userguide/fis-actions-reference.html
- gofail: https://github.com/etcd-io/gofail, pingcap/failpoint: https://github.com/pingcap/failpoint
- aws-sdk-go-v2 retry: https://github.com/aws/aws-sdk-go-v2/tree/main/aws/retry
- rustfs/s3chaos: https://github.com/rustfs/s3chaos; paperclip S3 fault proxy: https://github.com/paperclipai/paperclip/pull/20288
- Versity gateway: https://github.com/versity/versitygw (`embedgw/`, `backend/posix/posix.go`, `tests/integration/`, `tests/conditionalrace/`)
- SeaweedFS conditional ops: https://github.com/seaweedfs/seaweedfs/wiki/S3-Conditional-Operations
- RustFS issues: https://github.com/rustfs/rustfs/issues/9081, /8625, /6798
- Adobe S3Mock: https://github.com/adobe/S3Mock (`MultipartController.kt`, `ObjectController.kt`)
- moto: https://github.com/getmoto/moto/blob/master/moto/s3/responses.py
- Garage known issues: https://garagehq.deuxfleurs.fr/documentation/reference-manual/known-issues/
- ceph s3-tests (archived, moved): https://github.com/ceph/s3-tests, https://github.com/ceph/ceph/tree/main/src/test/rgw/s3-tests
- Tigris: https://www.tigrisdata.com/docs/objects/conditionals/, https://www.tigrisdata.com/docs/concepts/consistency/, https://www.tigrisdata.com/docs/api/s3/, https://www.tigrisdata.com/docs/forks/, https://www.tigrisdata.com/docs/buckets/snapshots-and-forks/, https://github.com/tigrisdata-community/s3-api-compat-tests
- Tigris rate-limit claims (secondary, conflicting): https://apis.io/rate-limits/tigris-data/tigris-data-rate-limits/, https://community.fly.io/t/tigris-performance-quotas-and-limits/21943
