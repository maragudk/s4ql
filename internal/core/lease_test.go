package core_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
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

// readErr is the error of starting a read transaction on d, which is closed again if it starts.
func readErr(t *testing.T, d *core.DB) error {
	t.Helper()
	r, err := d.Read(t.Context())
	if err != nil {
		return err
	}
	return r.Close(t.Context())
}

// beginErr is the error of beginning a write transaction on d, which is rolled back if it begins.
func beginErr(t *testing.T, d *core.DB) error {
	t.Helper()
	tx, err := d.Begin(t.Context())
	if err != nil {
		return err
	}
	return tx.Rollback(t.Context())
}

// serves reports whether d serves now, without waiting.
func serves(t *testing.T, d *core.DB) bool {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return d.Wait(ctx) == nil
}

func TestLease(t *testing.T) {
	t.Run("should not wait for the lease when opening, and refuse reads and writes at once until it serves", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			s.on("get", "manifest", hang, -1)
			d := w.openOn(t, "a", s, core.Options{})

			start := time.Now()
			is.Error(t, core.ErrNoLease, readErr(t, d))
			is.Error(t, core.ErrNoLease, beginErr(t, d))
			is.Equal(t, 0, time.Since(start))

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			is.Error(t, context.DeadlineExceeded, d.Wait(ctx))
			is.Equal(t, time.Minute, time.Since(start))

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			is.NotError(t, readErr(t, d))
			is.NotError(t, beginErr(t, d))
		})
	})

	t.Run("should give every request to the store a timeout", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			// A request without a timeout would wait for ever, and the bubble would report a deadlock.
			s.on("get", "manifest", hang, 2)
			s.on("create", "manifest", hang, 2)
			d := w.openOn(t, "a", s, core.Options{StoreTimeout: 3 * time.Second})
			start := time.Now()
			is.NotError(t, d.Wait(t.Context()))
			is.True(t, time.Since(start) >= 12*time.Second, fmt.Sprint(time.Since(start)))

			s.on("swap", "manifest", hang, 2)
			s.on("create", "commits/", hang, 2)
			s.on("get", "manifest", hang, 2)
			start = time.Now()
			write(t, d, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))
			})
			is.True(t, time.Since(start) >= 6*time.Second, fmt.Sprint(time.Since(start)))
		})
	})

	t.Run("should make a second instance a spare that refuses, and let it serve soon after the holder is closed", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			a := w.serve(t, "a", core.Options{})
			write(t, a, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))
			})
			b := w.open(t, "b", core.Options{})

			// The spare reads the manifest every five seconds, so the moment is one between two reads.
			time.Sleep(5*time.Minute + time.Second)
			is.True(t, serves(t, a))
			is.True(t, !serves(t, b))
			is.Error(t, core.ErrNoLease, readErr(t, b))
			is.Error(t, core.ErrNoLease, beginErr(t, b))
			is.Equal(t, a.ID(), w.manifest(t).Lease.Holder)

			is.NotError(t, a.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the manifest has a lease entry after the holder was closed")
			start := time.Now()
			is.NotError(t, b.Wait(t.Context()))
			// The spare reads the manifest every five seconds, and waits for nothing else.
			is.True(t, time.Since(start) <= 5*time.Second, fmt.Sprint(time.Since(start)))
			is.Equal(t, b.ID(), w.manifest(t).Lease.Holder)
			is.Equal(t, "t1(k*0): ", read(t, b))
		})
	})

	t.Run("should take the lease of a holder that died only after the manifest stood still for the holder's duration and the margin", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			a := w.openOn(t, "a", s, core.Options{Lease: 40 * time.Second})
			is.NotError(t, a.Wait(t.Context()))
			// The holder is cut off for good, which is what a watcher sees of a holder that died.
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			// The spare has a shorter lease of its own, and must wait for the holder's.
			b := w.open(t, "b", core.Options{Lease: 10 * time.Second})
			start := time.Now()
			wait := 40*time.Second + core.Margin(40*time.Second)
			time.Sleep(wait - time.Second)
			is.True(t, !serves(t, b), "the spare took the lease before the holder's duration and the margin had passed")
			is.Equal(t, a.ID(), w.manifest(t).Lease.Holder)

			is.NotError(t, b.Wait(t.Context()))
			is.True(t, time.Since(start) >= wait, fmt.Sprint(time.Since(start)))
			is.True(t, time.Since(start) <= wait+time.Second, fmt.Sprint(time.Since(start)))
			is.Equal(t, b.ID(), w.manifest(t).Lease.Holder)
			is.Equal(t, 10*time.Second, w.manifest(t).Lease.Duration)
		})
	})

	t.Run("should start its wait again when the holder writes the manifest", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			a := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, a.Wait(t.Context()))
			b := w.open(t, "b", core.Options{})

			// The holder renews after 20 seconds, and then dies.
			time.Sleep(25 * time.Second)
			renewed := time.Now().Add(-5 * time.Second)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			is.NotError(t, b.Wait(t.Context()))
			// The spare saw the renewal within five seconds of it, and waited from then.
			since := time.Since(renewed)
			wait := 30*time.Second + core.Margin(30*time.Second)
			is.True(t, since >= wait && since <= wait+5*time.Second+time.Second, fmt.Sprint(since))
		})
	})

	t.Run("should renew the lease after RenewAfter with a new salt and the time, and count a commit as a renewal", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			first := w.manifest(t).Lease
			is.Equal(t, d.ID(), first.Holder)
			is.Equal(t, 30*time.Second, first.Duration)
			is.True(t, first.Renewed.Equal(time.Now()), "the lease does not record the time at which it was taken")

			time.Sleep(19 * time.Second)
			is.Equal(t, first.Salt, w.manifest(t).Lease.Salt)
			time.Sleep(2 * time.Second)
			second := w.manifest(t).Lease
			is.True(t, second.Salt != first.Salt, "the renewal wrote no new salt")
			is.Equal(t, 20*time.Second, second.Renewed.Sub(first.Renewed))

			// A commit ten seconds after the renewal moves the next renewal to twenty seconds after it.
			time.Sleep(9 * time.Second)
			write(t, d, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))
			})
			committed := w.manifest(t).Lease
			is.True(t, committed.Salt != second.Salt, "the commit wrote no new salt")
			is.Equal(t, 30*time.Second, committed.Renewed.Sub(first.Renewed))
			time.Sleep(19 * time.Second)
			is.Equal(t, committed.Salt, w.manifest(t).Lease.Salt)
			time.Sleep(2 * time.Second)
			is.True(t, w.manifest(t).Lease.Salt != committed.Salt, "no renewal twenty seconds after the commit")
		})
	})

	t.Run("should refuse reads and writes when the lease has run out by the clocks, and serve again when a renewal gets through", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			time.Sleep(29 * time.Second)
			is.NotError(t, readErr(t, d))
			time.Sleep(2 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
			is.Error(t, core.ErrNoLease, beginErr(t, d))

			time.Sleep(time.Minute)
			s.clear()
			// The instance asks the store again every five seconds by now.
			time.Sleep(6 * time.Second)
			is.NotError(t, readErr(t, d))
			is.NotError(t, beginErr(t, d))
			is.Equal(t, d.ID(), w.manifest(t).Lease.Holder)
		})
	})

	t.Run("should count the lease from when a renewal was first sent, and not from its answer", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{StoreTimeout: time.Minute})
			is.NotError(t, d.Wait(t.Context()))
			// Every swap is applied at once, and its answer comes 25 seconds later.
			s.on("swap", "manifest", hold, -1, 25*time.Second)

			// The renewal is sent after 20 seconds and answered after 45. Until then, the lease is the
			// one that the instance took at the start.
			time.Sleep(44 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
			// From the answer on, the lease is the one that the renewal gave when it was sent, which
			// lasts until 50 seconds.
			time.Sleep(2 * time.Second)
			is.NotError(t, readErr(t, d))
			// The next renewal was sent at 45 seconds and is not answered before 70.
			time.Sleep(14 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
		})
	})

	t.Run("should go on renewing the lease while a write transaction is open", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))

			for range 10 {
				time.Sleep(20 * time.Second)
				is.NotError(t, readErr(t, d))
			}
			is.NotError(t, tx.Commit(t.Context()))
			is.Equal(t, "t1(k*0): ", read(t, d))
		})
	})

	t.Run("should lose the lease to an instance that took it, refuse, and get it back after that one is closed", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			a := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, a.Wait(t.Context()))
			write(t, a, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))
			})
			tx, err := a.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.Upsert(t.Context(), "t1", row(1)))

			// The holder is cut off until the spare has taken the lease and committed.
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			b := w.serve(t, "b", core.Options{})
			write(t, b, func(tx *core.Tx) {
				is.NotError(t, tx.Upsert(t.Context(), "t1", row(2)))
			})
			s.clear()
			time.Sleep(6 * time.Second)

			// The write transaction that was open is not committed.
			is.Error(t, core.ErrNoLease, tx.Upsert(t.Context(), "t1", row(3)))
			err = tx.Commit(t.Context())
			is.Error(t, core.ErrNoLease, err)
			is.True(t, !errors.Is(err, core.ErrOutcomeUnknown))
			is.Error(t, core.ErrNoLease, readErr(t, a))

			is.NotError(t, b.Close(t.Context()))
			is.NotError(t, a.Wait(t.Context()))
			is.Equal(t, "t1(k*0): 2", read(t, a))
		})
	})
}

func TestLease_window(t *testing.T) {
	t.Run("should count the lease that it took from when it sent the swap, and not from its answer", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			// The write that takes the lease is applied at once, and its answer comes 15 seconds later,
			// as that of every write after it.
			s.on("create", "manifest", hold, -1, 15*time.Second)
			s.on("swap", "manifest", hold, -1, 15*time.Second)
			d := w.openOn(t, "a", s, core.Options{StoreTimeout: time.Minute})
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, 15*time.Second, time.Since(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))

			// The lease lasts until 30 seconds, and the renewal that is sent at 20 is not answered
			// before 35.
			time.Sleep(time.Second)
			is.NotError(t, readErr(t, d))
			time.Sleep(16 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
		})
	})

	t.Run("should count the lease from the first attempt of a renewal, when a later attempt finds it applied", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			// The renewal at 20 seconds is applied, and its answer is lost. The store is then out of
			// reach until 30 seconds, so that a later attempt is what finds the renewal applied.
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			time.Sleep(30 * time.Second)
			s.clear()
			// From then on the instance hears nothing from the store again, once it has found out.
			s.on("swap", "manifest", pass, 1)
			s.on("get", "manifest", pass, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			// The renewal gave a lease from 20 to 50 seconds, whenever the instance found out.
			time.Sleep(15 * time.Second)
			is.NotError(t, readErr(t, d))
			time.Sleep(6 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
		})
	})

	t.Run("should refuse while it catches up after taking the lease again, and serve only then", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			a := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, a.Wait(t.Context()))
			transact(t, a, create("t1", 1, "k"))

			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			b := w.serve(t, "b", core.Options{})
			transact(t, b, upsert("t1", 2))
			s.clear()
			time.Sleep(6 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, a))

			// The download of what the other instance committed takes 20 seconds each time.
			s.on("get", "commits/", hold, -1, 20*time.Second)
			is.NotError(t, b.Close(t.Context()))
			time.Sleep(10 * time.Second)
			is.Equal(t, a.ID(), w.manifest(t).Lease.Holder)
			is.True(t, !serves(t, a), "the instance serves before it has caught up")
			is.Error(t, core.ErrNoLease, readErr(t, a))
			is.Error(t, core.ErrNoLease, beginErr(t, a))

			is.NotError(t, a.Wait(t.Context()))
			is.Equal(t, "t1(k*0): 2", read(t, a))
		})
	})

	t.Run("should keep the lease while a catch-up takes longer than the lease lasts", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			transact(t, first, create("t1", 1, "k"))
			for i := range 5 {
				transact(t, first, upsert("t1", i), upsert("t1", 10+i), upsert("t1", 20+i))
			}
			is.NotError(t, first.Close(t.Context()))

			// Each download takes 9 seconds: six to walk back through the log, and six to apply.
			s := newScriptStore(w.client())
			s.on("get", "commits/", hold, -1, 9*time.Second)
			a := w.openOn(t, "a", s, core.Options{})
			time.Sleep(time.Second)
			spare := w.open(t, "spare", core.Options{})

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			defer cancel()
			is.NotError(t, a.Wait(ctx))
			is.True(t, time.Since(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) > 100*time.Second)
			is.Equal(t, a.ID(), w.manifest(t).Lease.Holder)
			is.True(t, !serves(t, spare), "the spare took the lease from an instance that was catching up")
			is.NotError(t, readErr(t, a))
		})
	})

	t.Run("should keep the lease while it tries a download again for longer than the lease lasts", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			transact(t, first, create("t1", 1, "k"))
			is.NotError(t, first.Close(t.Context()))

			// The download of the one commit object fails 40 times, which takes minutes of waiting.
			s := newScriptStore(w.client())
			s.on("get", "commits/", drop, 40)
			start := time.Now()
			a := w.openOn(t, "a", s, core.Options{})
			time.Sleep(time.Second)
			spare := w.open(t, "spare", core.Options{})

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			defer cancel()
			is.NotError(t, a.Wait(ctx))
			is.True(t, time.Since(start) > 2*time.Minute, fmt.Sprint(time.Since(start)))
			is.Equal(t, a.ID(), w.manifest(t).Lease.Holder)
			is.True(t, !serves(t, spare), "the spare took the lease from an instance that was catching up")
			is.Equal(t, "t1(k*0): ", read(t, a))
		})
	})
}

func TestLease_retries(t *testing.T) {
	t.Run("should send the same renewal again while it has no answer, and a new one after it knows", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			sent := func() []string {
				s.mu.Lock()
				defer s.mu.Unlock()
				return slices.Clone(s.swaps)
			}
			s.on("swap", "", drop, -1)
			time.Sleep(40 * time.Second)
			// The renewal at 20 seconds, and its attempts after 1, 2, 4, 5, and 5 seconds more: the
			// instance asks less and less often, up to every five seconds.
			lost := sent()
			is.Equal(t, 6, len(lost))
			for _, swap := range lost {
				is.True(t, swap == lost[0], "an attempt of the renewal without an answer wrote another manifest")
			}

			s.clear()
			time.Sleep(10 * time.Second)
			all := sent()
			is.Equal(t, len(lost)+2, len(all))
			is.True(t, all[len(lost)] == lost[0], "the attempt that was answered wrote another manifest")
			is.True(t, all[len(lost)+1] != lost[0], "the next renewal wrote the same manifest")
			is.NotError(t, readErr(t, d))
		})
	})
}

// clocked runs f as a task of one instance under the scheduler, with a calm store, after the
// instance serves and has been cut off from the store for an hour, so that it can renew nothing.
func clocked(t *testing.T, f func(ctx context.Context, s *sim.Scheduler, inst *sim.Instance, d *core.DB)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		s := sim.New(1, sim.Options{})
		dir := t.TempDir()
		is.NotError(t, s.Run(func(ctx context.Context) {
			st := simstore.New(s, simstore.Options{Latency: 10 * time.Millisecond})
			inst := s.NewInstance("a")
			_ = inst.Go(ctx, "test", func(ctx context.Context) {
				client := st.Client("a", inst)
				d, err := core.Open(ctx, inst, client, filepath.Join(dir, "a.db"), "db", options())
				is.NotError(t, err)
				is.NotError(t, d.Wait(ctx))
				client.CutOff(ctx, time.Hour)
				f(ctx, s, inst, d)
				cctx, cancel := inst.WithTimeout(ctx, time.Second)
				defer cancel()
				_ = d.Close(cctx)
			}).Wait(ctx)
		}))
	})
}

func TestLease_clocks(t *testing.T) {
	// started reports the error of starting a read on d.
	started := func(ctx context.Context, d *core.DB) error {
		r, err := d.Read(ctx)
		if err != nil {
			return err
		}
		return r.Close(ctx)
	}

	t.Run("should refuse a read by the monotonic clock when the wall clock stands still or was set backward", func(t *testing.T) {
		for name, move := range map[string]func(inst *sim.Instance){
			"stands still": func(inst *sim.Instance) { inst.StopWall() },
			"set backward": func(inst *sim.Instance) { inst.ShiftWall(-time.Hour) },
		} {
			clocked(t, func(ctx context.Context, s *sim.Scheduler, inst *sim.Instance, d *core.DB) {
				move(inst)
				is.NotError(t, s.Sleep(ctx, 25*time.Second))
				is.NotError(t, started(ctx, d), name)
				is.NotError(t, s.Sleep(ctx, 6*time.Second))
				err := started(ctx, d)
				is.Error(t, core.ErrNoLease, err, name)
				is.True(t, strings.Contains(fmt.Sprint(err), "lease_expired_monotonic"), fmt.Sprint(name, ": ", err))
			})
		}
	})

	t.Run("should refuse a read by the wall clock when the monotonic clock stands still", func(t *testing.T) {
		clocked(t, func(ctx context.Context, s *sim.Scheduler, inst *sim.Instance, d *core.DB) {
			inst.StopMonotonic()
			is.NotError(t, s.Sleep(ctx, 25*time.Second))
			is.NotError(t, started(ctx, d))
			is.NotError(t, s.Sleep(ctx, 6*time.Second))
			err := started(ctx, d)
			is.Error(t, core.ErrNoLease, err)
			is.True(t, strings.Contains(fmt.Sprint(err), "lease_expired_wall"), fmt.Sprint(err))
			inst.StartMonotonic()
		})
	})

	t.Run("should fail a call that waits for a lock when the first of the two clocks says that the lease has run out", func(t *testing.T) {
		clocked(t, func(ctx context.Context, s *sim.Scheduler, inst *sim.Instance, d *core.DB) {
			tx, err := d.Begin(ctx)
			is.NotError(t, err)
			// The wall clock says that the lease runs out ten seconds before the monotonic clock does.
			inst.ShiftWall(10 * time.Second)
			done := make(chan error, 1)
			waiter := inst.Go(ctx, "waiter", func(ctx context.Context) {
				_, err := d.Begin(ctx)
				done <- err
			})
			is.NotError(t, s.Sleep(ctx, 21*time.Second))
			is.Equal(t, 1, len(done))
			err = <-done
			is.Error(t, core.ErrNoLease, err)
			is.True(t, strings.Contains(fmt.Sprint(err), "lease_expired_wall"), fmt.Sprint(err))
			is.NotError(t, tx.Rollback(ctx))
			is.NotError(t, waiter.Wait(ctx))
		})
	})

	t.Run("should not refuse a read because the wall clock was set forward by less than the lease has left", func(t *testing.T) {
		clocked(t, func(ctx context.Context, s *sim.Scheduler, inst *sim.Instance, d *core.DB) {
			inst.ShiftWall(10 * time.Second)
			is.NotError(t, s.Sleep(ctx, 15*time.Second))
			is.NotError(t, started(ctx, d))
			is.NotError(t, s.Sleep(ctx, 6*time.Second))
			is.Error(t, core.ErrNoLease, started(ctx, d))
		})
	})
}

func TestLease_gone(t *testing.T) {
	t.Run("should lose the lease when a renewal finds no manifest to replace", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			// The manifest is gone, as far as the instance can tell.
			s.on("swap", "manifest", missing, -1)
			s.on("get", "manifest", missing, -1)

			time.Sleep(25 * time.Second)
			err := readErr(t, d)
			is.Error(t, core.ErrNoLease, err)
			is.True(t, strings.Contains(fmt.Sprint(err), "not_holder"), fmt.Sprint(err))
		})
	})
}

func TestLease_end(t *testing.T) {
	t.Run("should serve until the moment that the duration of the lease has passed since it was sent, and not at that moment", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			start := time.Now()
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			time.Sleep(time.Until(start.Add(30*time.Second - time.Nanosecond)))
			is.NotError(t, readErr(t, d))
			time.Sleep(time.Nanosecond)
			is.Error(t, core.ErrNoLease, readErr(t, d))
		})
	})
}

func TestLease_unsure(t *testing.T) {
	t.Run("should take at once a lease that names it, after the answer to its taking was lost", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			is.NotError(t, first.Close(t.Context()))

			s := newScriptStore(w.client())
			s.on("swap", "manifest", lose, 1)
			d := w.openOn(t, "a", s, core.Options{})
			start := time.Now()
			is.NotError(t, d.Wait(t.Context()))
			is.True(t, time.Since(start) < 5*time.Second, fmt.Sprint(time.Since(start)))
			is.Equal(t, d.ID(), w.manifest(t).Lease.Holder)

			// Each of the two writes that took the lease carries a salt of its own.
			s.mu.Lock()
			swaps := slices.Clone(s.swaps)
			s.mu.Unlock()
			is.Equal(t, 2, len(swaps))
			lost, err := format.ParseManifest([]byte(swaps[0]))
			is.NotError(t, err)
			answered, err := format.ParseManifest([]byte(swaps[1]))
			is.NotError(t, err)
			is.True(t, lost.Lease.Salt != answered.Lease.Salt, "two writes of the manifest carry the same salt")
		})
	})

	t.Run("should release at its close a lease that it holds without knowing it", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			is.NotError(t, first.Close(t.Context()))

			// The swap that takes the lease is applied, its answer is lost, and the instance hears
			// nothing from the store until it is closed.
			s := newScriptStore(w.client())
			s.on("swap", "manifest", lose, 1)
			s.on("get", "manifest", pass, 1)
			s.on("get", "", drop, -1)
			d := w.openOn(t, "a", s, core.Options{})
			time.Sleep(3 * time.Second)
			is.True(t, !serves(t, d))
			is.Equal(t, d.ID(), w.manifest(t).Lease.Holder)

			s.clear()
			is.NotError(t, d.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
		})
	})

	t.Run("should not release at its close the lease of another instance, when it was unsure whether it held it", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			is.NotError(t, first.Close(t.Context()))

			// The swap that would take the lease is lost on its way, and the instance hears nothing
			// from the store while another instance takes the lease.
			s := newScriptStore(w.client())
			s.on("get", "manifest", pass, 1)
			s.on("swap", "manifest", drop, 1)
			s.on("get", "", drop, -1)
			d := w.openOn(t, "a", s, core.Options{})
			time.Sleep(3 * time.Second)
			b := w.serve(t, "b", core.Options{})

			s.clear()
			is.NotError(t, d.Close(t.Context()))
			lease := w.manifest(t).Lease
			is.True(t, lease != nil && lease.Holder == b.ID(), "the lease of the other instance was released")
			is.True(t, serves(t, b))
		})
	})
}

func TestLease_waiting(t *testing.T) {
	// waiting starts a write transaction on d and a call that waits for it.
	waiting := func(t *testing.T, d *core.DB) (*core.Tx, chan error) {
		t.Helper()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		waiter := make(chan error, 1)
		go func() { waiter <- beginErr(t, d) }()
		time.Sleep(time.Second)
		is.Equal(t, 0, len(waiter))
		return tx, waiter
	}

	t.Run("should fail a call that waits when the lease runs out by the clocks, and not let Wait return until it is valid again", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			tx, waiter := waiting(t, d)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			time.Sleep(28 * time.Second)
			is.Equal(t, 0, len(waiter))
			time.Sleep(2 * time.Second)
			is.Error(t, core.ErrNoLease, <-waiter)

			// The instance still holds the lease, as far as it knows, and does not serve.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			is.Error(t, context.DeadlineExceeded, d.Wait(ctx))
			cancel()
			waited := make(chan error, 1)
			go func() { waited <- d.Wait(t.Context()) }()
			time.Sleep(time.Second)
			is.Equal(t, 0, len(waited))
			s.clear()
			is.NotError(t, <-waited)
			is.NotError(t, readErr(t, d))
			is.NotError(t, tx.Rollback(t.Context()))
		})
	})

	t.Run("should fail a call whose lock comes free after the lease ran out by the clocks, before its wait ended", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			start := time.Now()
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			// The lease has 50 milliseconds left, so the call waits for the lock for the least time
			// that a wait lasts, which is longer than that.
			time.Sleep(time.Until(start.Add(30*time.Second - 50*time.Millisecond)))
			is.True(t, serves(t, d))
			waiter := make(chan error, 1)
			go func() { waiter <- beginErr(t, d) }()
			time.Sleep(75 * time.Millisecond)
			is.True(t, !serves(t, d))
			is.Equal(t, 0, len(waiter))

			// The lock comes free while the call still waits, and the lease has run out.
			is.NotError(t, tx.Rollback(t.Context()))
			err = <-waiter
			is.Error(t, core.ErrNoLease, err)
			is.True(t, strings.Contains(fmt.Sprint(err), "lease_expired"), fmt.Sprint(err))
		})
	})

	t.Run("should fail a call that waits when the instance finds that it has lost the lease", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			a := w.openOn(t, "a", s, core.Options{Lease: 10 * time.Minute, RenewAfter: 10 * time.Second})
			is.NotError(t, a.Wait(t.Context()))
			// The lease lasts ten minutes by the clocks, so only the news of its loss ends the wait.
			tx, waiter := waiting(t, a)

			// Another instance writes the manifest, as one does that has taken the lease.
			l := newHandLog(t, w)
			l.publish()
			time.Sleep(15 * time.Second)
			is.Equal(t, 1, len(waiter))
			is.Error(t, core.ErrNoLease, <-waiter)
			is.Error(t, core.ErrNoLease, tx.Commit(t.Context()))
		})
	})
}

func TestLease_duration(t *testing.T) {
	t.Run("should wait for a holder whose lease lasts longer than a duration can be added to", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			data, err := format.Manifest{Lease: &format.Lease{Holder: "another", Duration: 1<<63 - 1, Renewed: time.Now(), Salt: 1}}.Marshal()
			is.NotError(t, err)
			l.publishBytes(data)

			d := w.open(t, "a", core.Options{})
			time.Sleep(24 * time.Hour)
			is.True(t, !serves(t, d), "the instance took a lease that lasts for ever")
			is.Equal(t, "another", w.manifest(t).Lease.Holder)
		})
	})

	t.Run("should refuse a lease of more than a year", func(t *testing.T) {
		w := newWorld(t)
		_, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, "a.db"), "db", core.Options{Lease: 366 * 24 * time.Hour, RenewAfter: time.Hour})
		is.True(t, err != nil)
	})
}
