package s3store

import (
	"log/slog"
	"time"
)

// The operations of the client, as an event names them.
const (
	opGet    = "get"
	opCreate = "create"
	opSwap   = "swap"
)

// The outcomes of a request, as an event names them. A write whose outcome is unknown is
// outcomeUnknown or outcomeUnexpected, and a write with any other outcome but outcomeOK was not
// applied.
const (
	outcomeOK       = "ok"
	outcomeNotFound = "not_found"
	outcomeExists   = "exists"
	outcomeChanged  = "changed"
	outcomeChecksum = "checksum"
	// outcomeTooLarge is a write whose content is larger than one write takes: larger than [MaxSize],
	// which the client refuses before it sends anything, or larger than the store takes.
	outcomeTooLarge = "too_large"
	// outcomeNotSent is a call that the client refused before it sent anything, such as one with a
	// key that the store does not take, for another reason than its size.
	outcomeNotSent = "not_sent"
	// outcomeUnknown is a write that ended without an answer that shows what happened.
	outcomeUnknown = "unknown"
	// outcomeUnexpected is an answer that the client did not expect, such as one without an ETag.
	outcomeUnexpected = "unexpected"
	// outcomeFailed is a read that failed, before or while its content was read.
	outcomeFailed = "failed"
	// outcomeIncomplete is a read whose content was closed before its end, without an error.
	outcomeIncomplete = "incomplete"
)

// end r with the given outcome and error, which it returns, and log its event, store_request. A
// request that succeeded, or was refused as part of normal work, is logged at Debug. A checksum
// that does not match, content that is too large, a call that was not sent, an unknown outcome, an unexpected answer, and a
// failed read are logged at Warn. The attempts are the number of times that the HTTP transport
// wrote the request.
func (r *request) end(outcome string, err error) error {
	level := slog.LevelDebug
	switch outcome {
	case outcomeChecksum, outcomeTooLarge, outcomeNotSent, outcomeUnknown, outcomeUnexpected, outcomeFailed:
		level = slog.LevelWarn
	}
	attrs := []slog.Attr{
		slog.String("operation", r.op),
		slog.String("store.key", r.key),
		slog.String("outcome", outcome),
		slog.Int64("store.bytes", r.bytes),
		slog.Duration("duration.total", time.Since(r.start)),
		slog.Int("http.status_code", r.status),
		slog.String("store.request_id", r.requestID),
		slog.Int("attempts", int(r.attempts.Load())),
	}
	if err != nil {
		attrs = append(attrs, slog.String("error", err.Error()))
	}
	r.c.log.LogAttrs(r.ctx, level, "store_request", attrs...)
	return err
}
