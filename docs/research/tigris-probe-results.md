# Tigris conditional-write probe: results (2026-11-10)

Bucket: multi-region Tigris bucket with strong consistency, endpoint host `t3.storage.dev`, `AWS_REGION=auto`.
Client: a Mac Mini on residential fibre in Denmark. Every response that carried a region header said `X-Tigris-Served-From: fra` and `X-Tigris-Regions: fra` (2,861 of 2,861 responses with the header).
SDK: `aws-sdk-go-v2` `service/s3` v1.114.9 on Go 1.27.3. Retries were off (`aws.NopRetryer`) for every experiment unless stated. All traffic was HTTP/1.1 (12,967 of 12,967 exchanges in the main run).

Source: the probe source code and raw logs (not published).
Raw output: `.../tigris-probe/out/results.txt` (main run), `out/results-a.txt`, `out/results-b.txt`, `out/results-c.txt` (earlier partial runs), `out/results-q14.txt` (extra race), `out/exchanges-*.jsonl` (every HTTP exchange, scrubbed), `out/analyze-full.txt` (status and header tally of the main run).

"Observed N/N" means measured. "Inferred" means my reading of the measurements.

## 1. Findings that matter for a compare-and-swap commit protocol

1. **Conditional writes were atomic in every trial.** `If-None-Match: *` races with 8, 32 and 64 writers on 50 keys each (5,200 PUTs) had exactly one winner per key (150/150 keys). The stored object always carried the reported winner's token. `If-Match` counter CAS with 8 and 32 writers lost no updates: final value = reported successes (69 = 69, 36 = 36). Losers always got a clean `412 PreconditionFailed`.
2. **No 409, no 429, no 5xx under contention.** Across 14,448 requests in all runs, the only status codes were 200, 204, 206, 304, 404, 412, 416, 400 (deliberate), and 502 (only from the metadata-size bug below). No `ConditionalRequestConflict` appeared, so a client cannot rely on seeing 409 to detect contention. 412 is the only contention signal.
3. **ETag = quoted hex MD5 of the body (10/10). Identical content gives an identical ETag, so there is a real ABA risk.** After A -> B -> A, `If-Match: etag(A)` taken before B succeeded. Retrying an `If-Match` PUT whose content equals the current content also succeeds (200). The protocol must put a unique token in the manifest body, such as a monotonically increasing version or writer nonce, so no two manifest versions are byte-identical.
4. **A retry of your own successful conditional PUT gets 412 (observed 20/20 for `If-None-Match: *`, 20/20 for `If-Match`).** After a timeout, the client cannot tell "my write won" from "someone else won" by the status code. It must HEAD or GET and compare a writer token. User metadata round-trips reliably for this (10/10 via HEAD, 10/10 via GET, 20/20 after a rejected retry). A token in the body works too.
5. **User metadata of about 4 KiB or more poisons the object.** A PUT with a 4,096-byte or 8,000-byte `x-amz-meta-*` value returned 200, but every later HEAD and GET of that object returned `502 Bad Gateway` (an nginx HTML page). 3,500 bytes worked. 16,000 bytes was rejected at the edge with `400 Request Header Or Cookie Too Large` (HTML, not S3 XML). Keep metadata small, well under 2 KiB.
6. **Header-mode checksums are not validated.** A PUT with a wrong `x-amz-checksum-crc32` or `x-amz-checksum-sha256` header got 200, and Tigris stored and echoed the wrong value. A payload corrupted in flight under a correct CRC32 header was also stored. Later SDK GETs with checksum validation then fail on that object. The SDK's default aws-chunked **trailer** checksums (CRC32, CRC32C) and `Content-MD5` *were* validated: a flipped payload byte gave `400 BadDigest` and nothing was stored. Keep the SDK default (`WhenSupported`, trailer), or send `Content-MD5`. Never send a precomputed `x-amz-checksum-*` header and assume Tigris checks it.
7. **`DeleteObjects` ignores per-object `ETag`, and `DeleteObject` ignores `x-amz-if-match-size`.** Both deleted the object despite a wrong condition. Single-object `DeleteObject` with `If-Match` *is* enforced (412 on mismatch, 10/10).
8. **Tigris extends `If-None-Match` on PUT beyond AWS.** `If-None-Match: <current etag>` gave 412, and `If-None-Match: <other etag>` gave 200 and overwrote the object. AWS supports only `*`. This is harmless, but do not rely on it for portability.

## 2. Per-question results

### Q1. PutObject `If-None-Match: *`
- Absent key: `200` (20/20). Present key: `412`, S3 code `PreconditionFailed` (20/20). Body:
  `<Error><Code>PreconditionFailed</Code><Message>At least one of the pre-conditions you specified did not hold</Message><Resource>/<key></Resource><RequestId>…</RequestId><Key>…</Key><BucketName>…</BucketName></Error>`
- DELETE `204`, then `If-None-Match: *` immediately: `200` (20/20). A tight PUT/DELETE cycle on one key: 30/30 PUT 200, 30/30 DELETE 204. A fresh-connection GET after re-create returned the new body (20/20).
- `If-None-Match: "*"` (quoted) behaves like `*`.
- Unlike AWS: `If-None-Match: <current etag>` gives 412, and `<other etag>` gives 200 and overwrites (1 trial each).

### Q2. PutObject `If-Match`
- Match: `200` (10/10). Stale ETag: `412 PreconditionFailed` (10/10). Absent key: `404 NoSuchKey` (10/10). An unquoted ETag is accepted (10/10).
- ETag: quoted, lowercase hex MD5 of the body (10/10). Identical content on different keys, or on an overwrite, gives the same ETag. **ABA confirmed** (1 trial; follows from MD5 ETags).
- `If-Match: *`: 200 on an existing key, `404 NoSuchKey` on an absent key. `If-Match: ""` (legacy Tigris create-only form): 200 on an absent key, 412 on an existing one. `If-Match: W/<etag>`: 412.
- Multipart ETags have the form `"<hex>-<parts>"`.

### Q3. Atomicity under contention
| test | requests | outcome |
|---|---|---|
| INM:* race, 8 writers × 50 keys | 400 | 200 ×50, 412 ×350; one winner on 50/50 keys; stored token = winner 50/50 |
| INM:* race, 32 writers × 50 keys | 1,600 | 200 ×50, 412 ×1,550; one winner on 50/50 keys |
| INM:* race, 64 writers × 50 keys | 3,200 | 200 ×50, 412 ×3,150; one winner on 50/50 keys |
| If-Match counter, 8 writers × 40 GET+PUT | 320 pairs, 3.3 s | 69 successes, final 69, 0 lost updates, 0 ambiguous; 412 ×251 |
| If-Match counter, 32 writers × 15 GET+PUT | 480 pairs, 1.7 s | 36 successes, final 36, 0 lost updates; 412 ×444 |

Goroutine start spread was ≤1.8 ms, and each writer had its own pre-warmed TLS connection. 409, 429 and 5xx rates were all 0. Under contention, about 21 successful CAS per second landed on one key with either 8 or 32 writers. That is the same as sequential throughput, so contention cost goodput only through wasted attempts.

### Q4. Hunting for 409
No 409 in any test. Observed:
- (a) DELETE racing `If-Match` PUT (50): both succeeded 34×, and the final state was always 404 (PUT then DELETE). DELETE first, then PUT `404 NoSuchKey`, 16×. Every outcome was serializable.
- (b) Unconditional PUT racing `If-Match` PUT (50 in the main run, plus 100 in q14 with readback): both 200 49× (final body always "uncond", meaning If-Match ordered first), If-Match 412 51×. **0/100 non-serializable outcomes.**
- (c) An 8 MiB `If-None-Match: *` PUT, then a small `If-None-Match: *` PUT 150 ms later on the same key (6): the small one won (200) every time, and the big one got 412 after uploading for 0.6–1.3 s. (d) The same with `If-Match` on one ETag (6): small won 6/6. (e) An 8 MiB `If-Match` PUT, then a DELETE 150 ms later (6): DELETE 204, big PUT `404 NoSuchKey` 6/6.
- Inferred: Tigris evaluates the precondition when the upload *finishes*, not when it starts. A slow writer loses to a fast one that started later, and pays for the full upload first.

### Q5. Consistency, read from a fresh TCP+TLS connection right after each write (100 trials each)
Read-after-write 100/100, HEAD-after-write 100/100, read-after-overwrite 100/100, read-after-delete (404) 100/100, LIST-after-PUT 50/50, LIST-after-DELETE 25/25. **0 stale results.** Every fresh GET reported `X-Tigris-Served-From: fra`, `X-Tigris-Regions: fra`, `X-Tigris-Read-Source: inlined`. A single-region test client cannot show cross-region staleness (inferred).

### Q6. `X-Tigris-Consistent: true`
100 interleaved pairs each of GET, HEAD and INM:* PUT, all 200. Latency was the same with and without the header (GET p50 27.5 vs 27.2 ms, PUT p50 63.1 vs 63.1 ms). The response header set was the same. Read-after-write with the header on both sides: 30/30. `X-Tigris-CAS: true` on GET: 200, no visible effect. Nothing changed that I could observe on this bucket type from this location.

### Q7. Conditional GET/HEAD and ranges (5 trials each)
- GET/HEAD `If-None-Match: <current>`: `304 NotModified`. `If-None-Match: <other>`: 200. `If-None-Match: *` on an existing key: 304. On a missing key: `404 NoSuchKey`.
- GET/HEAD `If-Match: <current>`: 200. `<other>`: `412 PreconditionFailed`. With a Range header: 206, or 412. On a missing key: 404.
- Ranges returned the right bytes and `Content-Range` 5/5 each: `bytes=0-99` gave `bytes 0-99/1048576`; `bytes=-500` (suffix) gave `bytes 1048076-1048575/1048576`; `bytes=N-` (open-ended) was correct; an end past EOF was clamped. A start past EOF gave `416 InvalidRange` "Invalid range". A multi-range request gave `416 InvalidRange` "Only support one range". Quirk: `bytes=-0` gave **206 with an empty body** and `Content-Range: bytes 1048576-1048575/1048576`.
- 200 random 64 KiB range GETs of a 16 MiB object: 0 byte mismatches.

### Q8. User metadata on a conditional PUT
- Stored and returned by HEAD 10/10 and GET 10/10. Keys come back lower-cased (`MixedCase` became `mixedcase`). Values with spaces and UTF-8 (`æøå-ü`) round-trip. 50 keys × 30 bytes: all stored.
- An unconditional overwrite without metadata clears it (replace, not merge).
- The 412 response carries no `ETag` and no `x-amz-meta-*`, so the loser learns nothing about the winner.
- Size: values of 1,000–3,500 bytes work. **4,096 and 8,000 bytes: the PUT gives 200, then HEAD and GET give `502 Bad Gateway` (nginx HTML).** 16,000 bytes: `400 Request Header Or Cookie Too Large` (HTML; the SDK reports a deserialization error). Tigris does not enforce AWS's 2 KiB limit, but objects above about 4 KiB of metadata become unreadable.

### Q9. Multipart
- `CompleteMultipartUpload` with `If-None-Match: *` on an absent key: 200 (5/5). On an existing key: `412 PreconditionFailed` (5/5), and the upload stays alive (ListParts 200, 5/5).
- `If-Match: <wrong>`: 412 (5/5). Then `If-Match: <right>` on the same upload: 200 (5/5). `If-Match` on an absent key: `404 NoSuchKey` (5/5).
- Metadata from `CreateMultipartUpload` is preserved (5/5).
- Two different uploads racing complete with `If-None-Match: *` (20): exactly one winner 20/20, the other 412. The stored token matched the winner 20/20. The losing uploads stay open until aborted (cleanup aborted 20).
- **A complete retried after success, on the same uploadId, returns 200** (5/5 with `If-None-Match: *`, 5/5 unconditional), not 412. **If another writer overwrote the key in between, the retry returns `404 NoSuchUpload`** (5/5 for `If-None-Match: *`, 5/5 for `If-Match: <own etag>`), and the other writer's object survives (5/5). So a retried complete never clobbered a newer object.
- The same uploadId completed twice concurrently (10): both 200 8×, one 200 plus one `404 NoSuchUpload` 2×.
- Part-size minimum is not enforced: two 5-byte parts completed (200).
- SDK default CRC32 part checksums worked unmodified with Complete.

### Q10. Conditional DELETE
- `DeleteObject If-Match: <wrong>`: `412 PreconditionFailed`, and the object survives (10/10). `If-Match: <current>`: 204, then HEAD 404 (10/10). `If-Match` on a missing key: `404 NoSuchKey` (10/10). `If-Match: *`: 204 if present, `404 NoSuchKey` if absent.
- An unconditional DELETE of a missing key: 204.
- **Ignored conditions:** `x-amz-if-match-size: 99` (wrong) still deleted the object. `DeleteObjects` with a wrong per-object `ETag` deleted it anyway (reported in `Deleted`, no `Errors`).

### Q11. Retrying our own success
- Identical retry of a successful `If-None-Match: *` PUT: `412` (20/20). The stored object still had our token (20/20).
- Identical retry of a successful `If-Match` PUT with the old ETag: `412` (20/20).
- Exception: if the new content equals the old content, the ETag is unchanged and the retry gets 200 (1 trial).
- For multipart, see Q9: a retry of a completed upload gives 200 while the object is unchanged.

### Q12. Latency (sequential, kept-alive connection, 200 requests each, served from `fra`)
| op | n | errors | p50 ms | p90 ms | p99 ms | max ms | mean ms |
|---|---|---|---|---|---|---|---|
| PUT 1 KiB unconditional (overwrite) | 200 | 0 | 61.8 | 78.7 | 122.7 | 680.9 | 68.6 |
| PUT 1 KiB `If-None-Match: *` (new keys) | 200 | 0 | 63.7 | 82.0 | 125.8 | 181.1 | 68.2 |
| PUT 1 KiB `If-Match` (CAS chain) | 200 | 0 | 63.0 | 89.9 | 184.1 | 345.6 | 72.1 |
| PUT 1 MiB unconditional | 200 | 0 | 290.7 | 344.3 | 415.1 | 753.3 | 296.8 |
| HEAD | 200 | 0 | 28.5 | 41.0 | 147.6 | 542.6 | 35.9 |
| GET 1 KiB | 200 | 0 | 29.7 | 40.4 | 137.2 | 526.6 | 35.9 |
| GET `If-None-Match` → 304 | 200 | 0 | 29.2 | 34.7 | 72.3 | 172.7 | 31.5 |
| GET 64 KiB range of 16 MiB | 200 | 0 | 49.2 | 74.1 | 359.3 | 722.8 | 62.6 |
| LIST prefix (100 keys) | 200 | 0 | 35.0 | 42.5 | 143.6 | 154.7 | 38.8 |
| PUT 1 KiB `If-None-Match: *`, 16-way parallel | 800 | 0 | 62.7 | 91.5 | 211.7 | 599.6 | 72.2 |

Throughput:
- Sequential GET + `If-Match` PUT on one key: 100/100 in 10.32 s = **9.7 CAS/s**.
- Sequential `If-Match` PUT chain (ETag from the previous PUT response, no GET): 100/100 in 8.43 s = **11.9 CAS/s**.
- 16 parallel `If-None-Match: *` PUTs on distinct keys: 800/800 in 3.94 s = **203 PUT/s**, no 429.
- Conditional PUTs cost no more than unconditional ones (p50 about 62–64 ms). `Server-Timing` suggests about 5–80 ms of that is server write time. The rest is network RTT to Frankfurt (inferred).

Also from Q6 (100 each, interleaved): GET p50 27.5 ms, HEAD 28.1 ms, 1-byte INM PUT 63.1 ms.

### Q13. Surprises and SDK compatibility
- **SDK default checksums work unmodified.** The default PUT uses `Content-Encoding: aws-chunked`, `X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`, `X-Amz-Trailer: x-amz-checksum-crc32`, and gets 200. The response echoes `X-Amz-Checksum-Crc32` and `X-Amz-Checksum-Type: FULL_OBJECT`. CRC32C, SHA1, SHA256 and CRC64NVME trailers all work. GET with `ChecksumMode=ENABLED` validates.
- **Integrity checks (1 trial each):**

  | upload mode | payload byte flipped in flight | result |
  |---|---|---|
  | default client, CRC32 trailer | yes | `400 BadDigest` "Aws-chunked trailer checksum does not match …", nothing stored |
  | default client, CRC32C trailer | yes | `400 BadDigest`, nothing stored |
  | `Content-MD5` header | yes | `400 BadDigest`, nothing stored |
  | correct `x-amz-checksum-crc32` header (WhenRequired client) | yes | **200, corrupt object stored**; later validated GET fails |
  | no checksum (WhenRequired client) | yes | **200, corrupt object stored** |
  | wrong `x-amz-checksum-crc32` / `-sha256` header, intact body | no | **200, wrong checksum stored and echoed** |
  | wrong `Content-MD5`, intact body | no | `400 BadDigest` |

- **Error body format** is AWS-style XML with `Code`, `Message`, `Resource`, `RequestId`, `Key`, `BucketName`. There is no `HostId`. `RequestId` is a decimal integer. Edge errors (400 header too large, 502) are nginx HTML, and the SDK surfaces them as deserialization errors with an empty RequestID.
- **The default SDK retryer does not retry 412** (1 attempt).
- Headers on every response: `Server: nginx`, `Server-Timing: total;dur=N[, server;desc=write;dur=M]`, `Strict-Transport-Security`. Reads also carry `X-Tigris-Regions`, `X-Tigris-Served-From`, `X-Tigris-Read-Source: inlined`. GET responses carry `X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER` (an odd echo of how the object was uploaded).
- `Last-Modified` has 1-second resolution. Don't use it for ordering.
- The SDK printed `Response has no supported checksum. Not validating response payload.` at DEBUG for range GETs and 304s. This is harmless noise.

## 3. Method notes

- Each run used a unique prefix `probe/<UTC timestamp>-<8 hex>/`. Every key passes a guard that aborts the program if the key is outside the prefix. The transport refuses any host other than the configured S3 endpoint.
- Two clients: `c` (keep-alive, no retries) for writes and timing, and `fresh` (keep-alives off, so a new TCP+TLS connection per request) for consistency reads.
- Racing goroutines block on a shared channel and then fire together. Connections are pre-warmed by n concurrent HEADs.
- Corruption tests flip one payload byte in the transport after the SDK computed its checksum (the first data byte after the aws-chunked chunk header).
- Request volume: 14,448 requests across all runs (12,967 in the main run). About 400 MiB uploaded, mostly 200 × 1 MiB and 18 × 8 MiB.
- Fixes to the previous agent's code before running: a compile error (`keysOf` made generic), a vet error (non-constant format string), a possible nil dereference in q4(e), SDK non-API errors reported as `SDKError` instead of a bare status, tracking of every multipart uploadId for cleanup, and a `smoke` experiment. I also added the metadata-size readback via GET, the corruption tests, the stale multipart-complete retry, region tallies in q5, and q14 (an unconditional-vs-conditional race with readback).
- Invocations: `go run . -run q1,…`, `go run . -analyze out/exchanges-<ts>.jsonl`, and `go run . -cleanup probe/<ts>-<rand>/`, run from the probe directory. All of these work as the previous agent described, with one caveat: `mkdir -p out` must run first when stdout is redirected into `out/`. The program creates `out/` itself, but only after the shell has tried to open the redirect target. The run list also accepts `smoke` and `q14`.

## 4. Redaction and cleanup

- The logger drops the `Authorization`, `X-Amz-Security-Token`, `User-Agent` and `amz-sdk-*` request headers. It replaces the access key ID, secret key and bucket name with `<redacted>` in every logged header value, path, error body and printed line. The bucket name appeared only inside `<BucketName>` in error XML and was redacted.
- A scan of every source file and output file for the literal access key ID, secret and bucket name found 0 matches. Credentials were loaded only into the process environment of each command.
- After every run, the cleanup listed the prefix, deleted all objects and aborted open uploads (20 aborted in each multipart run). A final pass over all six run prefixes reported `remaining_objects=0 remaining_mpu=0`.
