package s3store

import (
	"log/slog"
	"time"
)

// eventRequest is the event of one request to the store, when it ends: for a write, when the
// answer has come, and for a read, when its content is closed.
const eventRequest = "store_request"

// The operations of the client, as the attribute attrOperation names them.
const (
	opGet    = "get"
	opCreate = "create"
	opSwap   = "swap"
)

// The outcomes of a request, as the attribute attrOutcome names them. A write whose outcome is
// unknown is outcomeUnknown or outcomeUnexpected, and a write with any other outcome but outcomeOK
// was not applied.
const (
	outcomeOK       = "ok"
	outcomeNotFound = "not_found"
	outcomeExists   = "exists"
	outcomeChanged  = "changed"
	outcomeChecksum = "checksum"
	// outcomeUnknown is a write that ended without an answer that shows what happened.
	outcomeUnknown = "unknown"
	// outcomeUnexpected is an answer that the client did not expect, such as one without an ETag.
	outcomeUnexpected = "unexpected"
	// outcomeFailed is a read that failed, before or while its content was read.
	outcomeFailed = "failed"
	// outcomeIncomplete is a read whose content was closed before its end, without an error.
	outcomeIncomplete = "incomplete"
)

// The names of the attributes. Those that are also in the storage core have the same names there.
const (
	attrOperation = "operation"
	attrOutcome   = "outcome"
	attrError     = "error"
	// attrAttempts is the number of times that the HTTP transport wrote the request.
	attrAttempts = "attempts"
	attrDuration = "duration.total"

	attrKey       = "store.key"
	attrBytes     = "store.bytes"
	attrStatus    = "http.status_code"
	attrRequestID = "store.request_id"
)

// end r with the given outcome and error, which it returns, and log its event. A request that
// succeeded, or was refused as part of normal work, is logged at Debug. A checksum that does not
// match, an unknown outcome, an unexpected answer, and a failed read are logged at Warn.
func (r *request) end(outcome string, err error) error {
	level := slog.LevelDebug
	switch outcome {
	case outcomeChecksum, outcomeUnknown, outcomeUnexpected, outcomeFailed:
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String(attrOperation, r.op),
		slog.String(attrKey, r.key),
		slog.String(attrOutcome, outcome),
		slog.Int64(attrBytes, r.bytes),
		slog.Duration(attrDuration, time.Since(r.start)),
		slog.Int(attrStatus, r.status),
		slog.String(attrRequestID, r.requestID),
		slog.Int(attrAttempts, int(r.attempts.Load())),
	}
	if err != nil {
		attrs = append(attrs, slog.String(attrError, err.Error()))
	}
	r.c.log.LogAttrs(r.ctx, level, eventRequest, attrs...)
	return err
}
