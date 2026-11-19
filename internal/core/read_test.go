package core_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
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

		r, err := startRead(t, d)
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

		r, err := startRead(t, d)
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

		r, err := startRead(t, d)
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

func TestReader_Read(t *testing.T) {
	t.Run("should see one state, whatever is committed while it is open", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10))

		r, err := startRead(t, d)
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

	t.Run("should run a read transaction on each of twenty readers at once, each on the state of its start, while writes commit", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))

		// Each reader starts its read transaction after one more commit than the reader before it.
		const n = 20
		var readers []*core.Reader
		var reads []*core.ReadTx
		var want []string
		for i := range n {
			transact(t, d, upsert("t1", i))
			want = append(want, fmt.Sprint(i))
			rd, err := d.Reader(t.Context())
			is.NotError(t, err)
			readers = append(readers, rd)
			r, err := rd.Read(t.Context())
			is.NotError(t, err)
			reads = append(reads, r)
		}
		is.Equal(t, n, d.Readers())
		transact(t, d, upsert("t1", n))

		for i, r := range reads {
			state, err := dump(t.Context(), r)
			is.NotError(t, err)
			is.Equal(t, "t1(k*0): "+strings.Join(want[:i+1], " | "), show(state))
		}
		for i, r := range reads {
			is.NotError(t, r.Close(t.Context()))
			is.NotError(t, readers[i].Close(t.Context()))
		}
		is.Equal(t, 0, d.Readers())
	})

	t.Run("should refuse a second read on a reader at once while its first is open, and start it once the first has ended", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		rd := newReader(t, d)
		first, err := rd.Read(t.Context())
		is.NotError(t, err)

		_, err = rd.Read(t.Context())
		is.Error(t, core.ErrReaderBusy, err)
		// The refusal leaves the first as it is, and another reader does not meet it.
		transact(t, d, upsert("t1", 2))
		state, err := dump(t.Context(), first)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0): 1", show(state))
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))

		is.NotError(t, first.Close(t.Context()))
		second, err := rd.Read(t.Context())
		is.NotError(t, err)
		state, err = dump(t.Context(), second)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0): 1 | 2", show(state))
		is.NotError(t, second.Close(t.Context()))
	})

	t.Run("should refuse a read whose context is done", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := newReader(t, d).Read(ctx)
		is.Error(t, context.Canceled, err)
	})

	t.Run("should refuse a read on an instance that does not serve, and serve it once the instance has the lease and has caught up", func(t *testing.T) {
		w := newWorld(t)
		holder := w.serve(t, "a", core.Options{})
		transact(t, holder, create("t1", 1, "k"), upsert("t1", 1))

		// A reader opens on a spare, whose cache is empty, and reads nothing there.
		spare := w.open(t, "b", core.Options{Watch: 10 * time.Millisecond})
		rd := newReader(t, spare)
		_, err := rd.Read(t.Context())
		is.Error(t, core.ErrNoLease, err)

		transact(t, holder, upsert("t1", 2))
		is.NotError(t, holder.Close(t.Context()))
		is.NotError(t, spare.Wait(t.Context()))
		r, err := rd.Read(t.Context())
		is.NotError(t, err)
		is.Equal(t, w.manifest(t).Head, r.Position())
		state, err := dump(t.Context(), r)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0): 1 | 2", show(state))
		is.NotError(t, r.Close(t.Context()))
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

func TestDB_Reader(t *testing.T) {
	t.Run("should refuse to open a reader on an instance that is closed, with the stop and its cause", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		is.NotError(t, d.Close(t.Context()))
		_, err := d.Reader(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrClosed, err)
		is.Equal(t, 0, d.Readers())
	})

	t.Run("should make the close of the instance wait for a reader that is opening, which then leaves nothing open", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			// The connections to the cache are closed by the cleanups that are registered later,
			// which run first.
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			t.Cleanup(func() { is.NotError(t, vfs.Close()) })
			h := &yieldHook{Host: w.host, site: "core: a reader before its open"}
			d := w.openWith(t, "a", h, w.client(), core.Options{VFS: vfs.Name()})
			is.NotError(t, d.Wait(t.Context()))

			// The instance is closed while the reader is about to open its connection. The close has
			// stopped the instance and closed the write connection, and waits for the reader.
			closed := make(chan error, 1)
			closing := func() {
				go func() {
					ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
					defer cancel()
					closed <- d.Close(ctx)
				}()
				time.Sleep(time.Minute)
				is.True(t, isDone(d))
				is.Equal(t, 0, vfs.OpenFiles())
				is.Equal(t, 0, len(closed))
			}
			h.f.Store(&closing)
			_, err = d.Reader(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrClosed, err)

			// The reader has woken the close, which returns without waiting for its context.
			start := time.Now()
			is.NotError(t, <-closed)
			is.Equal(t, 0, time.Since(start))
			is.Equal(t, 0, vfs.OpenFiles())
			is.Equal(t, 0, d.Readers())
		})
	})

	t.Run("should stop the instance for good when the files of the cache are deleted under it, without making an empty cache, and a start after it rebuilds the cache from the store", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		rd, err := d.Reader(t.Context())
		is.NotError(t, err)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)
		_, err = r.Get(t.Context(), "t1", row(1))
		is.NotError(t, err)
		is.NotError(t, r.Close(t.Context()))

		// The cache, its write-ahead log, and its wal-index, which the VFS of a browser keeps in memory.
		path := filepath.Join(w.dir, "a.db")
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(path + suffix); suffix != "-shm" || !errors.Is(err, os.ErrNotExist) {
				is.NotError(t, err)
			}
		}

		// A new reader opens the cache that is there, and does not make one.
		_, err = d.Reader(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrLocal, err)
		is.True(t, strings.Contains(err.Error(), "open a reader: sqlite: unable to open database file (code 14)"), err.Error())
		is.True(t, isDone(d))
		is.Error(t, core.ErrLocal, d.Err())
		is.Equal(t, 1, d.Readers())
		_, err = rd.Read(t.Context())
		is.Error(t, core.ErrLocal, err)
		is.NotError(t, rd.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
		entries, err := os.ReadDir(w.dir)
		is.NotError(t, err)
		for _, e := range entries {
			t.Errorf("the file %v is left", e.Name())
		}

		// The start of a new instance makes a cache and replays the log into it.
		again := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0): 1", read(t, again))
	})

	t.Run("should stop the instance for good when the cache cannot be opened for a reader, as for any local failure of the cache", func(t *testing.T) {
		w := newWorld(t)
		// The connections to the cache are closed by the cleanups that are registered later, which
		// run first.
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		d := w.serve(t, "a", core.Options{VFS: vfs.Name()})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		rd := newReader(t, d)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)

		// The next open of the file of the cache fails, as it does for a process that has no file
		// descriptor left.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Open, Files: faultvfs.MainDB, Times: 1})
		_, err = d.Reader(t.Context())
		is.Equal(t, 1, vfs.Failures())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrLocal, err)
		is.True(t, isDone(d))
		is.Error(t, core.ErrLocal, d.Err())
		is.Equal(t, 1, d.Readers())

		// The stop ends the read transaction of the reader that is open, and no reader opens after it.
		_, err = r.Get(t.Context(), "t1", row(1))
		is.Error(t, core.ErrLocal, err)
		_, err = d.Reader(t.Context())
		is.Error(t, core.ErrLocal, err)
		is.Equal(t, 1, vfs.Failures())
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
	})
}

func TestReader_Close(t *testing.T) {
	t.Run("should end the read transaction that is open and close the connection, and do nothing a second time", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		rd, err := d.Reader(t.Context())
		is.NotError(t, err)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)
		_, err = r.Get(t.Context(), "t1", row(1))
		is.NotError(t, err)
		is.Equal(t, 1, d.Readers())

		is.NotError(t, rd.Close(t.Context()))
		is.Equal(t, 0, d.Readers())
		_, err = r.Get(t.Context(), "t1", row(1))
		is.Error(t, core.ErrTxDone, err)
		is.Error(t, core.ErrTxDone, r.Close(t.Context()))
		_, err = rd.Read(t.Context())
		is.Error(t, core.ErrReaderClosed, err)
		is.NotError(t, rd.Close(t.Context()))

		// The instance goes on, with other readers.
		is.Equal(t, "t1(k*0): 1", read(t, d))
	})

	t.Run("should close nothing while a scan of its read transaction runs, and close the reader after it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2))
		rd, err := d.Reader(t.Context())
		is.NotError(t, err)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)

		var rows []string
		for row, err := range r.Scan(t.Context(), "t1") {
			is.NotError(t, err)
			rows = append(rows, text(row))
			is.Error(t, core.ErrReaderInUse, rd.Close(t.Context()))
			is.Equal(t, 1, d.Readers())
		}
		is.EqualSlice(t, []string{"1", "2"}, rows)
		is.NotError(t, rd.Close(t.Context()))
		is.Equal(t, 0, d.Readers())
	})

	t.Run("should have nothing left to close after the close of the instance, which closes the connection of every reader, also under a read transaction that is open", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		var readers []*core.Reader
		var reads []*core.ReadTx
		for i := range 8 {
			rd, err := d.Reader(t.Context())
			is.NotError(t, err)
			readers = append(readers, rd)
			r, err := rd.Read(t.Context())
			is.NotError(t, err)
			// Half of the readers are idle, and half have a read transaction open.
			if i%2 == 0 {
				is.NotError(t, r.Close(t.Context()))
				continue
			}
			_, err = r.Tables(t.Context())
			is.NotError(t, err)
			reads = append(reads, r)
		}

		is.NotError(t, d.Close(t.Context()))
		cacheClosed(t, w.dir)
		for _, r := range reads {
			_, err := r.Tables(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrClosed, err)
		}
		for _, rd := range readers {
			_, err := rd.Read(t.Context())
			is.Error(t, core.ErrClosed, err)
			is.NotError(t, rd.Close(t.Context()))
			is.NotError(t, rd.Close(t.Context()))
		}
		for _, r := range reads {
			is.Error(t, core.ErrTxDone, r.Close(t.Context()))
		}
		is.Equal(t, 0, d.Readers())
	})

	t.Run("should close the readers of an instance that has stopped, whose stop has ended the read transaction of each", func(t *testing.T) {
		w := newWorld(t)
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		d := w.serve(t, "a", core.Options{VFS: vfs.Name()})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		var readers []*core.Reader
		var reads []*core.ReadTx
		for range 6 {
			rd, err := d.Reader(t.Context())
			is.NotError(t, err)
			readers = append(readers, rd)
			r, err := rd.Read(t.Context())
			is.NotError(t, err)
			reads = append(reads, r)
		}

		// The cache fails after the manifest swap of a commit, which stops the instance.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		transact(t, d, upsert("t1", 2))
		is.True(t, vfs.Failures() > 0, "no write failed")
		is.True(t, isDone(d))
		vfs.Fail(faultvfs.Fault{})

		for i, r := range reads {
			_, err := r.Get(t.Context(), "t1", row(1))
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrLocal, err)
			var scanned int
			for _, err := range r.Scan(t.Context(), "t1") {
				scanned++
				is.Error(t, core.ErrLocal, err)
			}
			is.Equal(t, 1, scanned)
			// The read transaction of every second reader is left to the close of its reader.
			if i%2 == 0 {
				is.NotError(t, r.Close(t.Context()))
			}
		}
		for _, rd := range readers[:3] {
			is.NotError(t, rd.Close(t.Context()))
			_, err := rd.Read(t.Context())
			is.Error(t, core.ErrLocal, err)
		}
		is.Equal(t, 3, d.Readers())

		// The close of the instance closes the others.
		is.NotError(t, d.Close(t.Context()))
		cacheClosed(t, w.dir)
		for _, rd := range readers[3:] {
			is.NotError(t, rd.Close(t.Context()))
		}
		is.Equal(t, 0, d.Readers())
	})
}
