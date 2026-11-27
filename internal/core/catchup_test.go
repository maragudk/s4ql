package core_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store"
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
		_ = h.Sleep(ctx, h.d)
		return
	}
	h.Instance.Yield(ctx, site)
}

func TestDB_catchUp(t *testing.T) {
	// Two instances of one database run on one cache, which the package supports. Instance a's
	// manifest swap of a commit is applied, and a then stalls, before the commit of the cache, for
	// longer than its lease, and its renewals do not reach the store. b takes the lease, reads the
	// position of the cache, which is the parent of a's commit, and plans to replay a's commit
	// object. a then commits the cache. The adversarial review of 2026-11-26
	// (docs/research/adversarial-core-2026-11-26.md) found that b's replay then found the cache at
	// the commit object itself, not at its parent, and stopped b for good with ErrLogBroken, although
	// the log and the cache were both right.
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

// moveStore is a store that calls move at the at-th download of an object whose key holds key.
type moveStore struct {
	store.Store
	key  string
	at   int
	move func()

	mu sync.Mutex
	n  int
}

func (s *moveStore) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	if strings.Contains(key, s.key) {
		s.mu.Lock()
		s.n++
		move := s.n == s.at
		s.mu.Unlock()
		if move {
			s.move()
		}
	}
	return s.Store.Get(ctx, key)
}

// setCachePosition records the position in the cache at path, as another instance on the same
// cache that commits to it does, without the rows of what it applied.
func setCachePosition(t *testing.T, path, position string) {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	is.NotError(t, c.Exec("update s4ql_cache set head = '"+position+"'"))
	is.NotError(t, c.Close())
}

func TestDB_catchUp_moved(t *testing.T) {
	// The log is the base, c1, and c2. The cache moves while the catch-up is about to apply c1: at
	// its second download, after the walk.
	setUp := func(t *testing.T, w *world) (*handLog, format.Name) {
		t.Helper()
		l := newHandLog(t, w)
		l.base()
		c1 := l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t1", []uint32{0, 1}, row(3, 30)))
		})
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t1", []uint32{0, 1}, row(4, 40)))
		})
		l.publish()
		return l, c1
	}

	// The catch-up that finds the cache moved fails, and the lease task starts the next one soon,
	// from the position that the cache then records. The test bounds its wait, so that a catch-up
	// that tried the same commit object again, and never got past it, fails the test.
	wait := func(t *testing.T, d *core.DB) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		return d.Wait(ctx)
	}

	t.Run("should catch up from the commit object that the cache moved to, in the middle of the log", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l, c1 := setUp(t, w)
			path := filepath.Join(w.dir, "a.db")
			s := &moveStore{Store: w.client(), key: c1.String(), at: 2, move: func() { setCachePosition(t, path, c1.String()) }}
			events := &recorder{}
			start := time.Now()
			d := w.openOn(t, "a", s, core.Options{Logger: events.logger()})
			is.NotError(t, wait(t, d))
			// Another instance on the same cache commits to it in the course of things, so the next
			// catch-up is no back-off away, and the one that found the cache moved is no warning.
			is.True(t, time.Since(start) < time.Second, fmt.Sprint(time.Since(start)))
			failed := events.with("catch_up", "outcome", "failed")
			is.Equal(t, 1, len(failed))
			is.Equal[any](t, slog.LevelInfo, failed[0]["level"])

			// What the cache holds for c1 is what the other instance committed with it, here nothing.
			is.Equal(t, "t1(k*0,v): 1,10 | 2,20 | 4,40\nt2(a*0,b*1,c): 1,1,100", read(t, d))
			position, _, err := cacheDump(path)
			is.NotError(t, err)
			is.Equal(t, l.head, position)
		})
	})

	t.Run("should stop with ErrCacheNotInLog when the cache moved to a commit object that is not in the log", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			_, c1 := setUp(t, w)
			path := filepath.Join(w.dir, "a.db")
			elsewhere := strings.Repeat("ab", 32)
			s := &moveStore{Store: w.client(), key: c1.String(), at: 2, move: func() { setCachePosition(t, path, elsewhere) }}
			d := w.openOn(t, "a", s, core.Options{})
			err := wait(t, d)
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrCacheNotInLog, err)
			is.True(t, strings.Contains(err.Error(), "the cache is at "+elsewhere), err.Error())
		})
	})
}

// BenchmarkDB_catchUp measures how long an instance with an empty cache, with syncs on as an
// instance has them by default, takes to catch up on a log of 3000 commits of 10 rows each, from its
// open until it serves. Catch-up applies each commit object in a transaction of the cache of its own.
func BenchmarkDB_catchUp(b *testing.B) {
	h := sim.NewFree(sim.Buggify{})
	st := simstore.New(h, simstore.Options{})
	dir := b.TempDir()
	var fast core.Options
	fast.SyncOff()
	writer, err := core.Open(b.Context(), h, st.Client("writer", nil), filepath.Join(dir, "writer.db"), "db", fast)
	is.NotError(b, err)
	is.NotError(b, writer.Wait(b.Context()))
	tx, err := writer.Begin(b.Context())
	is.NotError(b, err)
	is.NotError(b, tx.CreateTable(b.Context(), tableOf("t1", 1, "k", "v")))
	is.NotError(b, tx.Commit(b.Context()))
	const commits = 3000
	for i := range commits {
		tx, err := writer.Begin(b.Context())
		is.NotError(b, err)
		for j := range 10 {
			is.NotError(b, tx.Upsert(b.Context(), "t1", row(i*10+j, j)))
		}
		is.NotError(b, tx.Commit(b.Context()))
	}
	is.NotError(b, writer.Close(b.Context()))

	n := 0
	for b.Loop() {
		d, err := core.Open(b.Context(), h, st.Client(fmt.Sprint("reader-", n), nil), filepath.Join(dir, fmt.Sprint("cache-", n, ".db")), "db", core.Options{})
		is.NotError(b, err)
		is.NotError(b, d.Wait(b.Context()))
		b.StopTimer()
		is.NotError(b, d.Close(b.Context()))
		n++
		b.StartTimer()
	}
	b.ReportMetric(float64(commits)*float64(b.N)/b.Elapsed().Seconds(), "commits/s")
}
