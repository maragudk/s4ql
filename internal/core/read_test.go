package core_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// yieldHook is a host that calls the function in f, if there is one, at each yield of the site.
type yieldHook struct {
	host.Host
	site string
	f    atomic.Pointer[func()]
}

func (h *yieldHook) Yield(ctx context.Context, site string) {
	if f := h.f.Load(); f != nil && site == h.site {
		(*f)()
	}
	h.Host.Yield(ctx, site)
}

func TestReadTx_Scan(t *testing.T) {
	t.Run("should end a scan that is running when the instance stops", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2), upsert("t1", 3))

		r, err := d.Read(t.Context())
		is.NotError(t, err)
		closed := make(chan error, 1)
		var rows int
		var scanErr error
		for _, err := range r.Scan(t.Context(), "t1") {
			if err != nil {
				scanErr = err
				break
			}
			rows++
			if rows == 1 {
				// Close stops the instance at once, and then waits for the read to end.
				go func() { closed <- d.Close(t.Context()) }()
				<-d.Done()
			}
		}
		is.Equal(t, 1, rows)
		is.Error(t, core.ErrStopped, scanErr)
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, <-closed)
	})
}

func TestReadTx_Get(t *testing.T) {
	t.Run("should refuse a key that is not one of the table", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		r, err := d.Read(t.Context())
		is.NotError(t, err)
		for name, key := range map[string][]*s4qlpb.Value{
			"no values":       row(),
			"too many values": row(1, 2),
			"NULL":            row(nil),
			"text":            row("1"),
		} {
			_, err := r.Get(t.Context(), "t1", key)
			is.Error(t, core.ErrInvalid, err, name)
		}
		is.NotError(t, r.Close(t.Context()))
	})
}

func TestReadTx_Close(t *testing.T) {
	t.Run("should refuse every call after the read transaction has ended", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		r, err := d.Read(t.Context())
		is.NotError(t, err)
		is.NotError(t, r.Close(t.Context()))

		is.Error(t, core.ErrTxDone, r.Close(t.Context()))
		_, err = r.Table(t.Context(), "t1")
		is.Error(t, core.ErrTxDone, err)
		_, err = r.Tables(t.Context())
		is.Error(t, core.ErrTxDone, err)
		_, err = r.Get(t.Context(), "t1", row(1))
		is.Error(t, core.ErrTxDone, err)
		var scanned int
		for _, err := range r.Scan(t.Context(), "t1") {
			scanned++
			is.Error(t, core.ErrTxDone, err)
		}
		is.Equal(t, 1, scanned)
	})
}

func TestDB_Read(t *testing.T) {
	t.Run("should see one state, whatever is committed while it is open", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10))

		r, err := d.Read(t.Context())
		is.NotError(t, err)
		transact(t, d, upsert("t1", 1, 11), upsert("t1", 2, 20))
		got, err := r.Get(t.Context(), "t1", row(1))
		is.NotError(t, err)
		is.Equal(t, "1,10", text(got))
		state, err := dump(t.Context(), r)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0,v): 1,10", show(state))
		is.NotError(t, r.Close(t.Context()))
		is.Equal(t, "t1(k*0,v): 1,11 | 2,20", read(t, d))
	})

	t.Run("should run four read transactions at once by default, and make a fifth wait", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			var reads []*core.ReadTx
			for range 4 {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				r, err := d.Read(ctx)
				cancel()
				is.NotError(t, err)
				reads = append(reads, r)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			_, err := d.Read(ctx)
			cancel()
			is.Error(t, context.DeadlineExceeded, err)
			for _, r := range reads {
				is.NotError(t, r.Close(t.Context()))
			}
		})
	})

	t.Run("should refuse a read whose lease ran out by the clocks after the state that it sees was fixed", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			h := &yieldHook{Host: w.host, site: "core: a read before the check of the lease"}
			s := newScriptStore(w.client())
			d := w.openWith(t, "a", h, s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			is.NotError(t, readErr(t, d))

			// No renewal gets through, and the lease runs out while the read has fixed its state and
			// not yet been checked again.
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			late := func() { time.Sleep(31 * time.Second) }
			h.f.Store(&late)
			is.Error(t, core.ErrNoLease, readErr(t, d))
		})
	})
}
