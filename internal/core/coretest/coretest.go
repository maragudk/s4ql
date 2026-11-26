// Package coretest has what the tests of other packages need of the storage core in
// [maragu.dev/s4ql/internal/core] beyond its API. Only tests may import it, which the linter's rule
// coretest checks.
package coretest

import (
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/core/internal/testonly"
)

// SyncOff is o with SQLite's syncs of the cache to disk off, as [core.SyncOff] makes it. A test that
// neither fails a sync nor crashes the operating system cannot tell syncs from none, and syncs cost
// time at every commit.
func SyncOff(o core.Options) core.Options {
	return core.SyncOff(o, testonly.Key{})
}
