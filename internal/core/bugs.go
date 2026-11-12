package core

// bugs that the tests of this package plant in it, to show that their checks find them. Each field
// switches on one wrong decision, at the one place where the right one is made, and the comment at
// that place says "the planted bug".
//
// The zero value plants nothing. It is the field of [Options] that no code outside this package can
// set, and nothing in this package sets it but its tests, so a program that uses the package has no
// way to switch a bug on. The cost is that the wrong branches are compiled into the package, each
// behind a test of one boolean.
type bugs struct {
	// renewOutsideLock makes the renewal read the manifest's version under the manifest lock and
	// swap after releasing it, so that a renewal and a commit can be in flight from one version.
	renewOutsideLock bool
	// cacheFirst makes a commit reach the cache before the manifest swap.
	cacheFirst bool
	// wallOnly makes the lease check before a read look at the wall clock only.
	wallOnly bool
	// monotonicOnly makes the lease check before a read look at the monotonic clock only.
	monotonicOnly bool
	// takeoverAfterRelease makes an instance take the lease only when the manifest has no lease
	// entry, as if every holder released its lease.
	takeoverAfterRelease bool
	// keepSalt makes a renewal write the salt of the manifest that it replaces.
	keepSalt bool
	// ownDuration makes an instance that watches the lease wait for its own lease duration, and not
	// for the holder's.
	ownDuration bool
	// lateVersion makes an instance that watches the lease go on with its wait when the manifest
	// changes, and swap against the version that it read last.
	lateVersion bool
	// leaseFromAnswer makes a holder count its lease from the answer to a swap, and not from when it
	// first sent the swap.
	leaseFromAnswer bool
	// ignoreCacheError makes a commit whose manifest swap succeeded ignore a failure of the cache.
	ignoreCacheError bool
	// ackUnknown makes a commit whose manifest swap has an unknown outcome count as committed.
	ackUnknown bool
	// settleByRead makes an instance conclude from a read of an unchanged manifest that a commit
	// with an unknown outcome is not committed, without the swap that rules the stuck request out.
	settleByRead bool
	// serveUnknown makes an instance go on serving after a commit returned with an unknown outcome.
	serveUnknown bool
	// lostMeansFailed makes a commit whose swap had an unknown outcome count as not committed when
	// another instance has taken the lease, without a look at the log.
	lostMeansFailed bool
	// replayCommitsEarly makes replay keep what it applied of a commit object that breaks off.
	replayCommitsEarly bool
	// keyTwice makes a commit object list each changed row of a table that the transaction did not
	// create twice.
	keyTwice bool
	// forgetDelete makes a commit object leave out the rows that the transaction deleted.
	forgetDelete bool
	// skipUnreadable makes a catch-up skip a commit object whose bytes do not match its name.
	skipUnreadable bool
	// renewWaitsForWriter makes the renewal wait for the writer lock, so that the lease is not
	// renewed while a write transaction is open.
	renewWaitsForWriter bool
	// unlockedCounter makes every read add to a counter without synchronisation, which is a data
	// race.
	unlockedCounter bool
}
