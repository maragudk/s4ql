# S3 store

`internal/store/s3store` is the [`store.Store`](../store.go) on S3 and on stores that are compatible with it. A `Client` is the client of one bucket, on the AWS SDK for Go, version 2.

What the client does is in the code:

- the package comment in `s3store.go`, on a write that is sent once and on the checksums of a write and of a read
- the doc comments of `Options`, `New`, and `MaxSize` in `s3store.go`
- the doc comments of `guard` and `body` in `content.go`
- the event of a request, and the names of its attributes, in `telemetry.go`

This file holds how to run the tests against a real bucket, what was measured on Tigris, why a write cannot go out twice, and what rests on AWS's documentation alone. The project has no AWS credentials, so nothing here was checked against AWS. What is still open is in the todo, under [The S3 client and S3's checksums](../../../docs/todo/s3-client.md).

## The integration tests

`TestClient_conformance` runs the conformance suite, `storetest.Test`, against a real bucket. `TestClient_realBucket` runs checks of its own against the same bucket. `TestClient_core` runs the storage core of `internal/core` on the client against the same bucket: it commits rows and replays them into an empty cache, hands the lease from an instance that closes to one that waits, and has a commit fail whose manifest swap the store refuses because another instance committed first. The core depends on the client only in this test. All three skip under `-short`, and fail with what is missing without their settings.

- **The settings come from `.env.test` at the root of the repository,** which names the project's Tigris bucket, its endpoint, and its region. A variable set in the environment wins. The credentials, `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, come from the environment or from the untracked `.env` next to `.env.test`. The details are in the documentation of [`internal/store/s3test`](../s3test/s3test.go).
- **`go test ./...` runs them** with the other tests, and so does CI's job `test`, with the credentials from the repository's secrets. `make test` passes `-short`, so it skips them. `make test-s3` runs this package with the others that hold tests against a real bucket.
- **Each test writes under a new prefix,** `s4ql-test/s3store/<UTC time>-<8 random characters>/`, which `s3test.NewPrefix` gives, and deletes everything under it at its end. The cleanup calls `ListObjectsV2` and `DeleteObject` of the SDK directly, because `store.Store` has no delete.
- **The credentials need four permissions:** `s3:GetObject`, `s3:PutObject`, and `s3:ListBucket` for the client, and `s3:DeleteObject` for the cleanup.
- **A run of this package's tests takes about 50 seconds** against Tigris from Denmark under the race detector; measured on 2026-11-19.

## The checksum of a write

A write can carry its SHA-256 to the store in three ways. Each was tried against the project's Tigris bucket on 2026-11-13, as a PutObject with `If-None-Match: *`:

- **As the header `x-amz-checksum-sha256`, Tigris does not check it.** A wrong checksum got 200, and Tigris stored the object and the wrong checksum. [tigris-probe-results.md](../../../docs/research/tigris-probe-results.md) found the same before.
- **In an aws-chunked trailer, Tigris checks it.** The request has `x-amz-trailer: x-amz-checksum-sha256` and `x-amz-content-sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`. A wrong checksum got `400 BadDigest`, "Aws-chunked trailer checksum does not match", and nothing was stored, also for empty content. A right one got 200, and Tigris stored the checksum.
- **As the payload hash of the signature, Tigris checks it.** The request is signed with our SHA-256 as its `x-amz-content-sha256`, the payload hash of Signature Version 4. A wrong checksum got `400 XAmzContentSHA256Mismatch`, "The provided 'x-amz-content-sha256' header does not match what was computed.", and nothing was stored, also for empty content. A right one got 200, but Tigris stored no checksum with the object.

The client uses the third way, and sends the checksum as `x-amz-checksum-sha256` too, so that the object carries it. Tigris stores that header without a check, but its value is the one that the payload hash made Tigris check.

The payload hash was chosen over the trailer for three reasons:

- **It needs no encoding of our own.**
- **It works over plain HTTP too.** The SDK sends trailers over HTTPS only.
- **The store must compute it anyway,** to check the signature.

The client reports both `BadDigest` and `XAmzContentSHA256Mismatch` as `store.ErrChecksum`. Over HTTPS, a write without a checksum is sent with `UNSIGNED-PAYLOAD` as its payload hash, so its content is read only once. Over plain HTTP, the SDK reads the content once more first, to compute the hash.

AWS documents a check of both the header and the payload hash. Neither was checked against AWS.

- **The header:** "For single part uploads, you can provide checksum values as headers. You can either provide a pre-calculated value or let the AWS SDK calculate one during upload. If the checksum value calculated by S3 matches your provided value, the request is accepted. If the values don't match, the request is rejected." (https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html)
- **The payload hash:** "If the upload request is signed with Signature Version 4, then Amazon Web Services S3 uses the `x-amz-content-sha256` header as a checksum instead of `Content-MD5`." (https://docs.aws.amazon.com/cli/latest/reference/s3api/upload-part.html) The quote is from the reference of the AWS CLI, because the pages of the S3 API render only with JavaScript.

## A write is sent once

The refusal of a second attempt can be caused by the first, so a conditional write must go out once. Each of these keeps it from going out twice:

- **The SDK retries nothing.** Its retryer is `aws.NopRetryer`, and `RetryMaxAttempts` is 0. Both are set after the shared configuration is loaded, so `AWS_MAX_ATTEMPTS` or `AWS_RETRY_MODE` in the environment cannot turn retries on. A test sets both and sees one request.
- **net/http does not resend a PUT that it started to write.** On a reused connection, it resends a request only if it wrote nothing of it, or if the request is replayable. A PUT is replayable only with an `Idempotency-Key` header and a body that is nil or that `GetBody` gives again, and the client's PUT has neither. The code is `persistConn.shouldRetryRequest` in `net/http/transport.go`, and `Request.isReplayable`, in Go 1.27.3.
- **The tests of the writes would see a resent PUT.** They cut a reused connection without an answer, and count one PUT. A test of a read cuts the connection in the same way, and there net/http does send the GET again.
- **The transport speaks HTTP/1.1 only,** so the retries of HTTP/2 cannot happen.
- **No proxy is used,** because an HTTP proxy could resend a request.
- **No redirect is followed.** `CheckRedirect` returns `http.ErrUseLastResponse`, because Go follows a 307 of a PUT with empty content by sending the PUT again.
- **A last guard counts the attempts.** `httptrace.ClientTrace.WroteHeaders` counts every time the transport writes the headers of the request. The trace is put on the request in a middleware after signing, so that a request of the SDK for credentials, which runs under the context of the call, is not counted. The client takes an answer as a refusal only if the count is exactly one, and reports any other as an unknown outcome, as `store.Store` requires.

## What was not checked on AWS

Each of these was seen on Tigris, or is in AWS's documentation. None was checked against AWS.

- **That the store checks the payload hash and the header.** Tigris checks the payload hash only. For AWS there are only the two quotes above.
- **The codes of a refusal:** 412 `PreconditionFailed`, 404 `NoSuchKey`, and 409 `ConditionalRequestConflict`. Tigris was seen to send the first two. AWS documents the 409 for `If-Match` "in the case of concurrent requests", and for `If-None-Match` only when a delete races the write (https://docs.aws.amazon.com/AmazonS3/latest/userguide/conditional-writes.html).
- **What a 409 means.** The client reports a 409 on a swap as `store.ErrChanged`. It reports a 409 on a create as an unknown outcome, because none of the five errors that mean "not applied" means "refused, and no object is known to be at the key".
- **A read of a missing object gives 404 only with `s3:ListBucket`,** and 403 without it (https://docs.aws.amazon.com/AmazonS3/latest/API/API_GetObject.html). The client never takes a 403 as `store.ErrNotFound`.
- **One PutObject takes up to 5 GB.** "You can send a PUT request to upload an object of up to 5 GB in a single operation" (https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html). The client refuses more than 5 GiB with `store.ErrTooLarge` before it sends anything. It makes no multipart upload. It also reports `400 EntityTooLarge` as `store.ErrTooLarge`, for a store whose limit is lower; AWS documents the code as "Your proposed upload exceeds the maximum allowed object size" (https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html), and no store was seen to send it.
- **Keys with characters that mean something in a URL or a path are kept apart and read back.** `TestClient_realBucket` shows it on Tigris for `?`, `#`, `%20`, `+`, `//`, a leading or a trailing slash, a tab, a newline, `...`, é in its NFC and NFD forms, and keys of 1024 bytes. The conformance suite shows on Tigris that keys that differ only in the case of a letter, in a doubled slash, or in the last byte of a key of the longest length are kept apart. These keys were not tried on AWS.

## What no test shows

- **That the guard of a write's content does anything today.** net/http in Go 1.27.3 does read a request's body after `RoundTrip` has returned: with a plain `http.Client`, a store that answers before it has read the body sees reads go on after the answer. What stops them in the client is first the SDK: smithy-go wraps the body in a reader that it closes right after `http.Client.Do`, with the comment "HTTP RoundTripper *should* close the request body. But this may not happen in a timely manner" (`transport/http/client.go`). That is not part of its API, so the `guard` in `content.go` does the same, and waits for a read in progress. The end-to-end test in `TestClient_content` passes with either of the two, and the guard is tested on its own in `content_internal_test.go`.
- **That `ChecksumMode: ENABLED` on a read does anything.** It is redundant with `ResponseChecksumValidation: WhenSupported`, which makes the SDK ask for the checksum too. Both are set.
