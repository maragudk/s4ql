// Package format reads and writes the two storage formats of s4ql: the manifest, which is JSON, and
// the commit object, which is Protocol Buffers with the schema in proto/s4ql/v1/commit.proto and the
// generated types in [s4qlpb].
//
// Both formats are drafts, and what this package writes carries version 0. A reader of either
// format refuses what it cannot be sure of reading as the writer meant it, and every refusal is an
// error that wraps one of three sentinel errors, so the caller can tell the cases apart:
//
//   - [ErrNewerVersion]: a newer version of s4ql wrote it, and this version cannot read it.
//   - [ErrMalformed]: no version of s4ql writes it like this.
//   - [ErrNameMismatch]: the bytes of a commit object do not match its name, so it is corrupt.
//
// An error from the underlying reader or writer is passed on and wraps none of the three.
//
// The package does no I/O of its own and reads no clock: the caller passes in the readers, the
// writers, and the commit time.
package format

import (
	"errors"
)

var (
	// ErrNewerVersion is wrapped by the error for something that a newer version of s4ql wrote: a
	// version above the one this package reads, or a field it does not know where it must not skip
	// one.
	ErrNewerVersion = errors.New("needs a newer version of s4ql")

	// ErrMalformed is wrapped by the error for something that no version of s4ql writes.
	ErrMalformed = errors.New("malformed")

	// ErrNameMismatch is wrapped by the error for a commit object whose bytes do not have the
	// SHA-256 checksum that is its name.
	ErrNameMismatch = errors.New("bytes do not match the name")
)
