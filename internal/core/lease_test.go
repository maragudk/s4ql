package core_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
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
			time.Sleep(2 * time.Second)
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
			time.Sleep(2 * time.Second)

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
