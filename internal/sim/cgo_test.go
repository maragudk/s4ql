package sim_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

// openWAL connection to the database file at path, in WAL mode and with the given busy timeout in
// milliseconds, closed when the test ends.
func openWAL(t *testing.T, path string, busyTimeout int) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.Close()) })
	is.NotError(t, c.Exec(fmt.Sprint("pragma busy_timeout = ", busyTimeout)))
	is.NotError(t, c.Exec("pragma journal_mode = wal"))
	return c
}

// slow counts to n on c with a recursive query, which keeps one step of the statement busy in C, and
// returns the count.
func slow(c *sqlite.Conn, n int64) (int64, error) {
	s, err := c.Prepare("with recursive c (x) as (select 1 union all select x + 1 from c where x < ?) select count(*) from c")
	if err != nil {
		return 0, err
	}
	if err := s.BindInt64(1, n); err != nil {
		_ = s.Finalize()
		return 0, err
	}
	if _, err := s.Step(); err != nil {
		_ = s.Finalize()
		return 0, err
	}
	count := s.ColumnInt64(0)
	return count, s.Finalize()
}

// busy reports whether err is SQLITE_BUSY from SQLite, also with an extended result code.
func busy(err error) bool {
	var sqliteErr *sqlite.Error
	// 5 is SQLITE_BUSY on https://sqlite.org/rescode.html.
	return errors.As(err, &sqliteErr) && sqliteErr.Code.Primary() == 5
}

func TestSynctestWithCgo(t *testing.T) {
	t.Run("should not return from Wait while a SQLite call is running in another goroutine", func(t *testing.T) {
		for i := range 5 {
			realStart := time.Now()
			synctest.Test(t, func(t *testing.T) {
				c := openWAL(t, filepath.Join(t.TempDir(), "test.db"), 0)
				fakeStart := time.Now()

				var done atomic.Bool
				var fakeElapsed time.Duration
				started := make(chan struct{})
				go func() {
					close(started)
					n, err := slow(c, 3_000_000)
					is.NotError(t, err)
					is.Equal(t, int64(3_000_000), n)
					fakeElapsed = time.Since(fakeStart)
					done.Store(true)
				}()
				<-started
				synctest.Wait()
				is.True(t, done.Load(), "Wait returned before the SQLite call did")
				is.Equal(t, 0, fakeElapsed)
			})
			t.Logf("run %v: Wait returned after the call; the bubble took %v of real time", i, time.Since(realStart).Round(time.Millisecond))
		}
	})

	t.Run("should not advance fake time while a SQLite call is running", func(t *testing.T) {
		realStart := time.Now()
		synctest.Test(t, func(t *testing.T) {
			c := openWAL(t, filepath.Join(t.TempDir(), "test.db"), 0)
			fakeStart := time.Now()

			// The value is atomic because the race detector does not treat the order that fake time gives
			// as synchronisation: with a plain variable it reports a data race here.
			var fakeAtReturn atomic.Int64
			fakeAtReturn.Store(-1)
			started := make(chan struct{})
			go func() {
				close(started)
				_, err := slow(c, 3_000_000)
				is.NotError(t, err)
				fakeAtReturn.Store(int64(time.Since(fakeStart)))
			}()
			<-started
			// Every goroutine but the one in C is now durably blocked, on a timer that is due in an hour.
			time.Sleep(time.Hour)
			is.Equal(t, 0, fakeAtReturn.Load())
			is.Equal(t, time.Hour, time.Since(fakeStart))
		})
		t.Logf("an hour of fake time passed only after the call returned; the bubble took %v of real time", time.Since(realStart).Round(time.Millisecond))
	})

	t.Run("should let a SQLite call that waits for the write lock with a busy timeout sleep in real time and then fail, without a deadlock report", func(t *testing.T) {
		realStart := time.Now()
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			holder := openWAL(t, path, 0)
			waiter := openWAL(t, path, 1500)
			fakeStart := time.Now()

			is.NotError(t, holder.Exec("begin immediate"))

			// The holder's goroutine (this one) is durably blocked while the waiter is in C.
			result := make(chan error)
			go func() {
				result <- waiter.Exec("begin immediate")
			}()
			is.True(t, busy(<-result))
			is.Equal(t, 0, time.Since(fakeStart))
			is.NotError(t, holder.Exec("commit"))
		})
		real := time.Since(realStart)
		is.True(t, real >= 1500*time.Millisecond, "the busy timeout did not run in real time")
		t.Logf("the waiter got SQLITE_BUSY after %v of real time and no fake time", real.Round(time.Millisecond))
	})

	t.Run("should never give the lock to a SQLite call that waits for it while its holder sleeps on fake time", func(t *testing.T) {
		realStart := time.Now()
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			holder := openWAL(t, path, 0)
			waiter := openWAL(t, path, 1500)
			fakeStart := time.Now()

			is.NotError(t, holder.Exec("begin immediate"))

			var mu sync.Mutex
			var events []string
			log := func(format string, args ...any) {
				mu.Lock()
				defer mu.Unlock()
				events = append(events, fmt.Sprintf(format, args...))
			}

			var wg sync.WaitGroup
			wg.Go(func() {
				// Outside a bubble, a millisecond is far less than the waiter's busy timeout, and the waiter
				// would get the lock.
				time.Sleep(time.Millisecond)
				log("holder commits at fake %v", time.Since(fakeStart))
				is.NotError(t, holder.Exec("commit"))
			})
			wg.Go(func() {
				err := waiter.Exec("begin immediate")
				log("waiter returns busy %v at fake %v", busy(err), time.Since(fakeStart))
			})
			wg.Wait()
			is.EqualSlice(t, []string{"waiter returns busy true at fake 0s", "holder commits at fake 1ms"}, events)
		})
		real := time.Since(realStart)
		is.True(t, real >= 1500*time.Millisecond)
		t.Logf("the waiter gave up after %v of real time; fake time could not advance to wake the holder", real.Round(time.Millisecond))
	})

	t.Run("should fail a SQLite call at once that wants a held write lock with no busy timeout", func(t *testing.T) {
		realStart := time.Now()
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			holder := openWAL(t, path, 0)
			waiter := openWAL(t, path, 0)

			is.NotError(t, holder.Exec("begin immediate"))
			result := make(chan error)
			go func() {
				result <- waiter.Exec("begin immediate")
			}()
			is.True(t, busy(<-result))
			is.NotError(t, holder.Exec("commit"))
		})
		t.Logf("the waiter got SQLITE_BUSY at once; the bubble took %v of real time", time.Since(realStart).Round(time.Millisecond))
	})

	t.Run("should give the lock to a SQLite call that waits for it while its holder is running, as outside a bubble", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			holder := openWAL(t, path, 0)
			// The busy timeout runs in real time. It is far above the time that the holder's work can
			// take on any machine, so that the speed of the machine does not decide the test.
			waiter := openWAL(t, path, 60_000)

			is.NotError(t, holder.Exec("begin immediate"))

			var wg sync.WaitGroup
			started := make(chan struct{})
			wg.Go(func() {
				close(started)
				// The holder is never durably blocked: it works in C and then commits.
				_, err := slow(holder, 1_000_000)
				is.NotError(t, err)
				is.NotError(t, holder.Exec("commit"))
			})
			wg.Go(func() {
				<-started
				is.NotError(t, waiter.Exec("begin immediate"))
				is.NotError(t, waiter.Exec("commit"))
			})
			wg.Wait()
		})
	})

	t.Run("should still report a deadlock when goroutines block for good after SQLite calls", func(t *testing.T) {
		// A bubble that deadlocks never ends its test, so nothing in it is cleaned up. The file and the
		// connections therefore belong to this test, and the bubble's two goroutines stay blocked for
		// as long as the process runs.
		path := filepath.Join(t.TempDir(), "test.db")
		holder := openWAL(t, path, 0)
		waiter := openWAL(t, path, 0)

		var panicked any
		func() {
			defer func() { panicked = recover() }()
			synctest.Test(t, func(*testing.T) {
				is.NotError(t, holder.Exec("begin immediate"))
				never := make(chan struct{})
				go func() {
					is.True(t, busy(waiter.Exec("begin immediate")))
					<-never
				}()
				<-never
			})
		}()
		is.Equal(t, "deadlock: all goroutines in bubble are blocked", fmt.Sprint(panicked))
	})

	// In WAL mode, a writer can get SQLITE_BUSY when it begins although no other connection writes,
	// once readers run in parallel with it. So a lock in Go around the write connection does not keep
	// SQLite from wanting a lock that is taken.
	for _, test := range []struct {
		name        string
		busyTimeout int
	}{
		{name: "should log how often one writer among parallel readers gets SQLITE_BUSY when it begins with no busy timeout", busyTimeout: 0},
		{name: "should not give SQLITE_BUSY to one writer among parallel readers with a busy timeout", busyTimeout: 60_000},
	} {
		t.Run(test.name, func(t *testing.T) {
			var busyOpen, busyRead, busyBegin, busyCommit, reads, writes atomic.Int64
			realStart := time.Now()
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "test.db")
				setup := openWAL(t, path, 0)
				is.NotError(t, setup.Exec("create table t1 (k integer primary key, v integer not null) strict"))
				is.NotError(t, setup.Exec("insert into t1 (k, v) values (1, 0)"))

				count := func(n *atomic.Int64, err error) bool {
					if busy(err) {
						n.Add(1)
						return true
					}
					is.NotError(t, err)
					return false
				}

				var wg sync.WaitGroup
				stop := make(chan struct{})
				for range 8 {
					wg.Go(func() {
						// Each goroutine opens its own connection while the others are already at work.
						c, err := sqlite.Open(path)
						is.NotError(t, err)
						defer func() { is.NotError(t, c.Close()) }()
						is.NotError(t, c.Exec("pragma busy_timeout = 0"))
						for count(&busyOpen, c.Exec("pragma journal_mode = wal")) {
						}
						for {
							select {
							case <-stop:
								return
							default:
							}
							if count(&busyRead, c.Exec("begin")) {
								continue
							}
							if count(&busyRead, c.Exec("select v from t1 where k = 1")) {
								is.NotError(t, c.Exec("rollback"))
								continue
							}
							is.NotError(t, c.Exec("commit"))
							reads.Add(1)
						}
					})
				}
				wg.Go(func() {
					defer close(stop)
					c, err := sqlite.Open(path)
					is.NotError(t, err)
					defer func() { is.NotError(t, c.Close()) }()
					is.NotError(t, c.Exec(fmt.Sprint("pragma busy_timeout = ", test.busyTimeout)))
					for count(&busyOpen, c.Exec("pragma journal_mode = wal")) {
					}
					for writes.Load() < 3000 {
						if count(&busyBegin, c.Exec("begin immediate")) {
							continue
						}
						is.NotError(t, c.Exec("update t1 set v = v + 1 where k = 1"))
						if count(&busyCommit, c.Exec("commit")) {
							is.NotError(t, c.Exec("rollback"))
							continue
						}
						writes.Add(1)
					}
				})
				wg.Wait()
			})
			t.Logf("%v writes and %v reads in %v of real time; SQLITE_BUSY on setting WAL mode: %v, in reads: %v, on begin immediate: %v, on commit: %v",
				writes.Load(), reads.Load(), time.Since(realStart).Round(time.Millisecond),
				busyOpen.Load(), busyRead.Load(), busyBegin.Load(), busyCommit.Load())
			is.Equal(t, 0, busyRead.Load())
			is.Equal(t, 0, busyCommit.Load())
			if test.busyTimeout > 0 {
				is.Equal(t, 0, busyBegin.Load())
			}
		})
	}
}
