// Package store is the interface through which s4ql talks to its object store, [Store]: read an
// object, create one where none is, and replace one that is unchanged since it was read. That is all
// that s4ql needs of S3, and nothing else is in the interface.
//
// The errors of a write tell four cases apart, each of which means that the write was not applied:
//
//   - [ErrNotFound]: no object is at the key.
//   - [ErrExists]: an object is already at the key.
//   - [ErrChanged]: the object changed since its version was read.
//   - [ErrChecksum]: the content does not match the checksum that came with it.
//
// Every other error of a write means that its outcome is unknown: the write may have been applied,
// it may not have been, and it may still be applied later, after the call has returned. A request
// can be stuck in the network, and a store can apply it after the caller has given up. Only a later
// read, or a conditional write that the stuck request would make fail, tells what happened. An
// error that wraps [context.Canceled] or [context.DeadlineExceeded] is no exception.
package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
)

var (
	// ErrNotFound is wrapped by the error of a read or a swap for which no object is at the key.
	// The swap was not applied.
	ErrNotFound = errors.New("no object at the key")

	// ErrExists is wrapped by the error of a create for which an object is already at the key. The
	// create was not applied.
	ErrExists = errors.New("an object exists at the key")

	// ErrChanged is wrapped by the error of a swap for which the object at the key does not have the
	// version that the swap is from. The swap was not applied.
	ErrChanged = errors.New("the object changed since its version was read")

	// ErrChecksum is wrapped by the error of a write whose content does not have the SHA-256
	// checksum that came with it. The write was not applied.
	ErrChecksum = errors.New("the content does not match its checksum")
)

// Version of an object, as a read or a write returns it, which a swap is checked against. It is
// opaque, and never empty.
//
// Two equal versions of the same key mean the same content. The same content does not always give
// the same version, and it can: on S3, the version is the ETag, which for a simple upload without
// encryption by a key of the customer's is the MD5 checksum of the content. So a swap that writes
// the same bytes as are there already can leave the version as it was, and a swap from that version
// still succeeds after it.
type Version string

// Checksum of content: its SHA-256.
type Checksum = [sha256.Size]byte

// Store of objects by key. It is strongly consistent: a read reflects every write that returned
// success before the read started, and a write is applied at one instant, completely or not at all.
// A read that overlaps a write reflects it or not. Its methods are safe for concurrent use.
//
// A key is a string of valid UTF-8 of 1 to 1024 bytes, without a path segment that is "." or "..".
type Store interface {
	// Get the object at key: its content, which the caller reads to its end or until it fails, and
	// then closes, and its version. The content is the object as it was at one instant during the
	// call, also if it is replaced while the caller reads it. Reading it can fail at any point with
	// an error, and reports [io.EOF] only after the whole content. It can be read only until ctx is
	// done. Get fails with [ErrNotFound] if no object is at key.
	Get(ctx context.Context, key string) (io.ReadCloser, Version, error)

	// Create an object at key with content, if no object is there, and return its version. The
	// content is what content reads from its current offset to its end, and the caller does not use
	// content again until Create has returned. If checksum is not nil, the store refuses content
	// that does not have it. Create fails with [ErrExists] if an object is at key, and with
	// [ErrChecksum] for a checksum that does not match. Any other error means that the outcome is
	// unknown.
	Create(ctx context.Context, key string, content io.ReadSeeker, checksum *Checksum) (Version, error)

	// Swap the object at key for one with content, if the object there has the version from, and
	// return the new version. The content and checksum are as for [Store.Create]. Swap fails with
	// [ErrNotFound] if no object is at key, with [ErrChanged] if the object there has another
	// version, and with [ErrChecksum] for a checksum that does not match. Any other error means that
	// the outcome is unknown.
	Swap(ctx context.Context, key string, from Version, content io.ReadSeeker, checksum *Checksum) (Version, error)
}
