package core_test

import (
	"context"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store/simstore"
)

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

func TestDB_catchUp(t *testing.T) {
	// Two instances of one database run on one cache, which TestOpen_cacheDir in the root package
	// supports. Instance a's manifest swap of a commit is applied, and a then stalls, before the
	// commit of the cache, for longer than its lease, and its renewals do not reach the store. b takes
	// the lease, reads the position of the cache, which is the parent of a's commit, and plans to
	// replay a's commit object. a then commits the cache. The adversarial review of 2026-11-26 found
	// that b's replay then found the cache at the commit object itself, not at its parent, and stopped
	// b for good with ErrLogBroken, although the log and the cache were both right.
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

				bErr, bState := make(chan error, 1), make(chan string, 1)
				bTask := ib.Go(ctx, "b", func(ctx context.Context) {
					is.NotError(t, ib.Sleep(ctx, time.Second))
					b, err := core.Open(ctx, ib, st.Client("b", ib), path, "db", o)
					is.NotError(t, err)
					wctx, cancel := ib.WithTimeout(ctx, 3*time.Minute)
					err = b.Wait(wctx)
					cancel()
					bErr <- err
					if err == nil {
						rd, err := b.Reader(ctx)
						is.NotError(t, err)
						r, err := rd.Read(ctx)
						is.NotError(t, err)
						state, err := dump(ctx, r)
						is.NotError(t, err)
						is.NotError(t, rd.Close(ctx))
						bState <- show(state)
					}
					cctx, cancel := ib.WithTimeout(ctx, 5*time.Minute)
					defer cancel()
					_ = b.Close(cctx)
				})
				is.NotError(t, aTask.Wait(ctx))
				is.NotError(t, bTask.Wait(ctx))

				// a's commit is in the log, and a acknowledged it.
				is.NotError(t, <-aErr)
				// b took over, and serves at a's commit.
				is.NotError(t, <-bErr)
				is.Equal(t, "t(k*0,v): 1,1", <-bState)
			}))
		})
	})
}
