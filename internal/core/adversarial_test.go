package core_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store/simstore"
)

// TestAdversarial_identicalCommits reproduces a finding of the adversarial review of 2026-11-26
// (docs/research/adversarial-core-2026-11-26.md). It FAILS on purpose: it shows a fault, and is
// to pass once the fault is fixed.
//
// Two instances commit transactions with the same parent, the same changes, no annotations, and
// the same commit time by their wall clocks. Their commit objects then have the same bytes, and so
// the same name. Instance a's swap gets no answer, b takes the lease and commits, and a then finds
// "its" commit object in the log with Tx.inLog and acknowledges it, although none of a's swaps was
// applied. Both commits return nil from one manifest swap.
func TestAdversarial_identicalCommits(t *testing.T) {
	t.Run("should not acknowledge two commits that only one manifest swap made", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(1, sim.Options{})
			dir := cacheDir(t)
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: 10 * time.Millisecond})
				ia, ib := s.NewInstance("a"), s.NewInstance("b")
				sa := newScriptStore(st.Client("a", ia))

				aDone := make(chan error, 1)
				var a *core.DB
				aTask := ia.Go(ctx, "a", func(ctx context.Context) {
					var err error
					a, err = core.Open(ctx, ia, sa, filepath.Join(dir, "a.db"), "db", options())
					is.NotError(t, err)
					is.NotError(t, a.Wait(ctx))
					tx, err := a.Begin(ctx)
					is.NotError(t, err)
					is.NotError(t, tx.CreateTable(ctx, tableOf("t", 1, "k", "v")))
					is.NotError(t, tx.Commit(ctx))

					// The wall clocks of both instances stand at the same instant from here on.
					ia.StopWall()
					ib.StopWall()

					tx, err = a.Begin(ctx)
					is.NotError(t, err)
					is.NotError(t, tx.Upsert(ctx, "t", row(1, 1)))
					// The upload of the commit object goes through, and every swap of the manifest is lost
					// on its way until the test clears the rule.
					sa.on("swap", "manifest", drop, -1)
					aDone <- tx.Commit(ctx)
					cctx, cancel := ia.WithTimeout(ctx, 5*time.Minute)
					defer cancel()
					_ = a.Close(cctx)
				})

				// b takes the lease after a's has run out, begins at the same parent, and makes the
				// same change.
				bErr := make(chan error, 1)
				bTask := ib.Go(ctx, "b", func(ctx context.Context) {
					is.NotError(t, s.Sleep(ctx, time.Second))
					b, err := core.Open(ctx, ib, st.Client("b", ib), filepath.Join(dir, "b.db"), "db", options())
					is.NotError(t, err)
					is.NotError(t, b.Wait(ctx))
					tx, err := b.Begin(ctx)
					is.NotError(t, err)
					is.NotError(t, tx.Upsert(ctx, "t", row(1, 1)))
					bErr <- tx.Commit(ctx)
					cctx, cancel := ib.WithTimeout(ctx, 5*time.Minute)
					defer cancel()
					_ = b.Close(cctx)
				})
				is.NotError(t, bTask.Wait(ctx))
				is.NotError(t, <-bErr)

				// a reaches the store again.
				sa.clear()
				is.NotError(t, aTask.Wait(ctx))
				aErr := <-aDone

				// The two transactions wrote one commit object: the log holds the create of the table
				// and one more.
				var commits int
				for _, key := range st.Keys() {
					if strings.Contains(key, "/commits/") {
						commits++
					}
				}
				is.Equal(t, 2, commits)

				// One swap was applied, b's, so a's commit must not be acknowledged.
				is.True(t, aErr != nil, "both commits were acknowledged, from one manifest swap")
			}))
		})
	})
}

// TestAdversarial_watchOverflow reproduces a finding of the adversarial review of 2026-11-26
// (docs/research/adversarial-core-2026-11-26.md). It FAILS on purpose: it shows a fault, and is
// to pass once the fault is fixed.
//
// Options.Watch has no upper bound, and a manifest can hold any positive duration. With a holder's
// duration of 9e18 nanoseconds (about 285 years) and a Watch longer than that, DB.acquire finds
// the time left below Watch, adds the margin to it, and the sum overflows to a negative duration,
// so the instance takes the lease at once from a holder that renewed a moment ago.
func TestAdversarial_watchOverflow(t *testing.T) {
	t.Run("should not take at once the lease of a holder that renewed a moment ago", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			data, err := format.Manifest{Lease: &format.Lease{Holder: "another", Duration: 9_000_000_000_000_000_000, Renewed: time.Now(), Salt: 1}}.Marshal()
			is.NotError(t, err)
			l.publishBytes(data)

			d := w.open(t, "a", core.Options{Watch: 1<<63 - 1})
			time.Sleep(time.Minute)
			is.True(t, !serves(t, d), "the instance took a lease that lasts for centuries a minute after its renewal")
			is.Equal(t, "another", w.manifest(t).Lease.Holder)
		})
	})
}

// TestAdversarial_replacedCache reproduces a finding of the adversarial review of 2026-11-26
// (docs/research/adversarial-core-2026-11-26.md). It FAILS on purpose: it shows a fault, and is
// to pass once the fault is fixed.
//
// The files of the cache of a running holder are deleted, and a second instance of the same
// database is opened on the same path, which makes a new, empty cache there and waits as a spare.
// The holder's next reader opens the file at the path, which is the new cache, and its read
// serves the empty cache as fresh, without the holder's acknowledged commits.
func TestAdversarial_replacedCache(t *testing.T) {
	t.Run("should not serve a read from a cache that another instance made at the path of the holder's cache", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			a := w.serve(t, "a", core.Options{})
			transact(t, a, create("t1", 1, "k"))
			head := w.manifest(t).Head

			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(filepath.Join(w.dir, "a.db"+suffix))
			}
			_ = w.open(t, "a", core.Options{})

			r, err := startRead(t, a)
			if err != nil {
				return
			}
			position := r.Position()
			is.NotError(t, r.Close(t.Context()))
			is.Equal(t, head, position)
		})
	})
}

// stallHost is the host of an instance that, once armed, stalls for d at the first yield at site,
// as a process does that the operating system pauses, and calls before as the stall begins.
type stallHost struct {
	*sim.Instance
	site   string
	d      time.Duration
	before func()
	armed  bool
}

func (h *stallHost) Yield(ctx context.Context, site string) {
	if h.armed && site == h.site {
		h.armed = false
		h.before()
		_ = h.Instance.Sleep(ctx, h.d)
		return
	}
	h.Instance.Yield(ctx, site)
}

// TestAdversarial_sharedCacheTakeover reproduces a finding of the adversarial review of 2026-11-26
// (docs/research/adversarial-core-2026-11-26.md). It FAILS on purpose: it shows a fault, and is
// to pass once the fault is fixed.
//
// Two instances of one database run on one cache, which TestOpen_cacheDir in the root package
// supports. Instance a's manifest swap of a commit is applied, and a then stalls, before the
// commit of the cache, for longer than its lease, and its renewals do not reach the store. b takes
// the lease, reads the position of the cache, which is the parent of a's commit, and plans to
// replay a's commit object. a then commits the cache. b's replay finds the cache at the commit
// object itself, not at its parent, and stops b for good with ErrLogBroken, although the log and
// the cache are both right.
func TestAdversarial_sharedCacheTakeover(t *testing.T) {
	t.Run("should let a spare on the same cache serve after it takes over during the commit of the holder", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			s := sim.New(1, sim.Options{})
			dir := cacheDir(t)
			path := filepath.Join(dir, "shared.db")
			o := options()
			// SQLite's busy wait would hold the scheduler, so a wait for the write lock of the cache
			// fails at once, and catch-up tries again.
			o.BusyTimeout = time.Millisecond
			is.NotError(t, s.Run(func(ctx context.Context) {
				st := simstore.New(s, simstore.Options{Latency: 10 * time.Millisecond})
				ia, ib := s.NewInstance("a"), s.NewInstance("b")
				sa := newScriptStore(st.Client("a", ia))
				ha := &stallHost{Instance: ia, site: "core: a commit between the manifest swap and the cache", d: time.Minute,
					before: func() { sa.on("swap", "manifest", drop, -1) }}

				aErr := make(chan error, 1)
				aTask := ia.Go(ctx, "a", func(ctx context.Context) {
					a, err := core.Open(ctx, ha, sa, path, "db", o)
					is.NotError(t, err)
					is.NotError(t, a.Wait(ctx))
					tx, err := a.Begin(ctx)
					is.NotError(t, err)
					is.NotError(t, tx.CreateTable(ctx, tableOf("t", 1, "k", "v")))
					is.NotError(t, tx.Commit(ctx))

					is.NotError(t, ia.Sleep(ctx, 2*time.Second))
					tx, err = a.Begin(ctx)
					is.NotError(t, err)
					is.NotError(t, tx.Upsert(ctx, "t", row(1, 1)))
					ha.armed = true
					aErr <- tx.Commit(ctx)
					cctx, cancel := ia.WithTimeout(ctx, 5*time.Minute)
					defer cancel()
					_ = a.Close(cctx)
				})

				bErr := make(chan error, 1)
				bTask := ib.Go(ctx, "b", func(ctx context.Context) {
					is.NotError(t, ib.Sleep(ctx, time.Second))
					b, err := core.Open(ctx, ib, st.Client("b", ib), path, "db", o)
					is.NotError(t, err)
					wctx, cancel := ib.WithTimeout(ctx, 3*time.Minute)
					bErr <- b.Wait(wctx)
					cancel()
					cctx, cancel := ib.WithTimeout(ctx, 5*time.Minute)
					defer cancel()
					_ = b.Close(cctx)
				})
				is.NotError(t, aTask.Wait(ctx))
				is.NotError(t, bTask.Wait(ctx))

				// a's commit is in the log, and a acknowledged it.
				is.NotError(t, <-aErr)
				// b took over, and must serve.
				is.NotError(t, <-bErr)
			}))
		})
	})
}
