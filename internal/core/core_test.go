package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/store"
	"maragu.dev/s4ql/internal/store/simstore"
)

// world is a calm store and a host with real goroutines, for the tests that need no scheduler.
type world struct {
	host  *sim.Free
	store *simstore.Store
	dir   string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	h := sim.NewFree(sim.Buggify{})
	return &world{host: h, store: simstore.New(h, simstore.Options{}), dir: t.TempDir()}
}

// client of the world's store, with a name of its own.
func (w *world) client() store.Store {
	return w.store.Client(fmt.Sprint("client-", w.host.Uint64()), nil)
}

// open an instance with the cache of the given name, closed when the test ends.
func (w *world) open(t *testing.T, cache string, options core.Options) *core.DB {
	t.Helper()
	return w.openOn(t, cache, w.client(), options)
}

// openOn opens an instance that reaches the store through s.
func (w *world) openOn(t *testing.T, cache string, s store.Store, options core.Options) *core.DB {
	t.Helper()
	d, err := core.Open(t.Context(), w.host, s, filepath.Join(w.dir, cache+".db"), "db", options)
	is.NotError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		_ = d.Close(ctx)
	})
	return d
}

// serve opens an instance and waits until it serves.
func (w *world) serve(t *testing.T, cache string, options core.Options) *core.DB {
	t.Helper()
	d := w.open(t, cache, options)
	is.NotError(t, d.Wait(t.Context()))
	return d
}

// row of values: an int for an integer, and nil for NULL.
func row(vs ...any) []*s4qlpb.Value {
	values := make([]*s4qlpb.Value, len(vs))
	for i, v := range vs {
		switch v := v.(type) {
		case nil:
			values[i] = s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
		case int:
			values[i] = s4qlpb.Value_builder{Integer: new(int64(v))}.Build()
		case int64:
			values[i] = s4qlpb.Value_builder{Integer: new(v)}.Build()
		case string:
			values[i] = s4qlpb.Value_builder{Text: new(v)}.Build()
		case float64:
			values[i] = s4qlpb.Value_builder{Real: new(v)}.Build()
		case []byte:
			values[i] = s4qlpb.Value_builder{Blob: v}.Build()
		default:
			panic(fmt.Sprintf("no value for a %T", v))
		}
	}
	return values
}

// text of a row: its values joined by commas.
func text(values []*s4qlpb.Value) string {
	parts := make([]string, len(values))
	for i, v := range values {
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			parts[i] = fmt.Sprint(v.GetInteger())
		case s4qlpb.Value_Null_case:
			parts[i] = "null"
		default:
			parts[i] = fmt.Sprint("?", v.WhichKind())
		}
	}
	return strings.Join(parts, ",")
}

// tableOf the given name, with integer columns, of which the first keys are the primary key.
func tableOf(name string, keys int, columns ...string) *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new(name)}
	for _, c := range columns {
		ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(c), Type: new(s4qlpb.Type_TYPE_INT)}.Build())
	}
	for i := range keys {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(i))
	}
	return ct.Build()
}

// reader is what a write transaction and a read transaction both read with.
type reader interface {
	Tables(ctx context.Context) ([]*s4qlpb.CreateTable, error)
	Get(ctx context.Context, table string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error)
	Scan(ctx context.Context, table string) iter.Seq2[[]*s4qlpb.Value, error]
}

// describe a table: its name, its columns, each with a star if it is in the primary key and the
// position there, as in "t1(a*0,b)".
func describe(ct *s4qlpb.CreateTable) string {
	columns := make([]string, len(ct.GetColumns()))
	for i, c := range ct.GetColumns() {
		columns[i] = c.GetName()
		for position, key := range ct.GetPrimaryKey() {
			if int(key) == i {
				columns[i] += fmt.Sprint("*", position)
			}
		}
	}
	return fmt.Sprint(ct.GetTable(), "(", strings.Join(columns, ","), ")")
}

// dump the tables that r sees, each as its description and its rows in the order of a scan.
func dump(ctx context.Context, r reader) (map[string][]string, error) {
	tables, err := r.Tables(ctx)
	if err != nil {
		return nil, err
	}
	state := map[string][]string{}
	for _, ct := range tables {
		rows := []string{}
		for row, err := range r.Scan(ctx, ct.GetTable()) {
			if err != nil {
				return nil, err
			}
			rows = append(rows, text(row))
		}
		state[describe(ct)] = rows
	}
	return state, nil
}

// show a dump in a fixed order, for comparing.
func show(state map[string][]string) string {
	var tables []string
	for table, rows := range state {
		tables = append(tables, fmt.Sprint(table, ": ", strings.Join(rows, " | ")))
	}
	slices.Sort(tables)
	return strings.Join(tables, "\n")
}

// read what the instance serves now.
func read(t *testing.T, d *core.DB) string {
	t.Helper()
	r, err := d.Read(t.Context())
	is.NotError(t, err)
	state, err := dump(t.Context(), r)
	is.NotError(t, err)
	is.NotError(t, r.Close(t.Context()))
	return show(state)
}

// write runs f in a write transaction of d and commits it.
func write(t *testing.T, d *core.DB, f func(tx *core.Tx)) {
	t.Helper()
	tx, err := d.Begin(t.Context())
	is.NotError(t, err)
	f(tx)
	is.NotError(t, tx.Commit(t.Context()))
}

func TestOpen(t *testing.T) {
	t.Run("should serve a new database, keep what is committed, and replay it into an empty cache", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})

		is.Equal(t, "", read(t, d))
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "k", "v")))
			is.NotError(t, tx.Upsert(t.Context(), "t1", row(2, 20)))
			is.NotError(t, tx.Upsert(t.Context(), "t1", row(1, nil)))
		})
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.Upsert(t.Context(), "t1", row(3, 30)))
			found, err := tx.Delete(t.Context(), "t1", row(2))
			is.NotError(t, err)
			is.True(t, found)
		})
		is.Equal(t, "t1(k*0,v): 1,null | 3,30", read(t, d))
		is.NotError(t, d.Close(t.Context()))

		again := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0,v): 1,null | 3,30", read(t, again))
		is.NotError(t, again.Close(t.Context()))

		empty := w.serve(t, "b", core.Options{})
		is.Equal(t, "t1(k*0,v): 1,null | 3,30", read(t, empty))
	})
}

// isDone reports whether the instance has signalled that it has stopped.
func isDone(d *core.DB) bool {
	select {
	case <-d.Done():
		return true
	default:
		return false
	}
}

func TestDB_Close(t *testing.T) {
	t.Run("should release the lease, close the cache, signal the stop, and refuse every call after it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))
		is.True(t, !isDone(d))
		is.NotError(t, d.Err())

		is.NotError(t, d.Close(t.Context()))
		is.True(t, isDone(d))
		is.Error(t, core.ErrStopped, d.Err())
		is.Error(t, core.ErrClosed, d.Err())
		is.True(t, w.manifest(t).Lease == nil, "the manifest has a lease entry")

		for name, err := range map[string]error{
			"read":  readErr(t, d),
			"begin": beginErr(t, d),
			"wait":  d.Wait(t.Context()),
		} {
			is.Error(t, core.ErrStopped, err, name)
			is.Error(t, core.ErrClosed, err, name)
		}
		// Closing it again does nothing.
		is.NotError(t, d.Close(t.Context()))

		// The cache is closed: another connection can take every lock on it.
		c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.NotError(t, c.Exec("pragma locking_mode = exclusive"))
		is.NotError(t, c.Exec("begin exclusive"))
		is.NotError(t, c.Exec("rollback"))
		is.NotError(t, c.Close())
	})

	t.Run("should make calls that wait fail, abort a read that is open, and wait for the transactions that are open", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{Readers: 1})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			upsert("t1", 2)(t, tx)
			r, err := d.Read(t.Context())
			is.NotError(t, err)

			// A writer waits for the write transaction, and a reader for the one read connection.
			waiting := make(chan error, 2)
			go func() { waiting <- beginErr(t, d) }()
			go func() { waiting <- readErr(t, d) }()
			time.Sleep(time.Second)
			is.Equal(t, 0, len(waiting))

			// Close does not return while the two transactions are open.
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			err = d.Close(ctx)
			cancel()
			is.Error(t, context.DeadlineExceeded, err)
			is.True(t, isDone(d))
			is.True(t, w.manifest(t).Lease == nil, "the lease is released before the transactions have ended")

			// The calls that waited have failed, and the transactions that are open fail from now on.
			is.Error(t, core.ErrStopped, <-waiting)
			is.Error(t, core.ErrStopped, <-waiting)
			_, err = r.Get(t.Context(), "t1", row(1))
			is.Error(t, core.ErrStopped, err)
			for _, err := range r.Scan(t.Context(), "t1") {
				is.Error(t, core.ErrStopped, err)
			}
			is.Error(t, core.ErrStopped, tx.Upsert(t.Context(), "t1", row(3)))
			is.Error(t, core.ErrStopped, tx.Commit(t.Context()))
			is.NotError(t, r.Close(t.Context()))

			is.NotError(t, d.Close(t.Context()))
			again := w.serve(t, "a", core.Options{})
			is.Equal(t, "t1(k*0): 1", read(t, again))
		})
	})

	t.Run("should give up releasing a lease that it cannot release, and leave it to time out", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			start := time.Now()
			err := d.Close(t.Context())
			is.True(t, err != nil, "the close reports no error")
			is.True(t, time.Since(start) > 30*time.Second && time.Since(start) < time.Minute, fmt.Sprint(time.Since(start)))
			is.Equal(t, d.ID(), w.manifest(t).Lease.Holder)

			// The cache is closed all the same, and the next instance waits for the lease to time out.
			b := w.open(t, "b", core.Options{})
			start = time.Now()
			is.NotError(t, b.Wait(t.Context()))
			is.True(t, time.Since(start) >= 30*time.Second+core.Margin(30*time.Second), fmt.Sprint(time.Since(start)))
		})
	})
}

func TestDB_Wait(t *testing.T) {
	t.Run("should return when the instance serves, and fail when the instance stops or the context ends first", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			a := w.serve(t, "a", core.Options{})
			b := w.open(t, "b", core.Options{})

			// The spare waits for as long as the holder lives.
			waited := make(chan error, 1)
			go func() { waited <- b.Wait(t.Context()) }()
			time.Sleep(10 * time.Minute)
			is.Equal(t, 0, len(waited))
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			is.Error(t, context.DeadlineExceeded, b.Wait(ctx))
			cancel()

			is.NotError(t, a.Close(t.Context()))
			is.NotError(t, <-waited)

			c := w.open(t, "c", core.Options{})
			go func() { waited <- c.Wait(t.Context()) }()
			time.Sleep(time.Second)
			is.NotError(t, c.Close(t.Context()))
			is.Error(t, core.ErrClosed, <-waited)
		})
	})
}

// failing is the outcome of one run of [fail].
type failing struct {
	// variant of the failure: whether the cache had the transaction when the instance stopped.
	landed bool
	events *recorder
}

// fail runs one instance under the scheduler with seed, with buggify on at every point, so that the
// cache fails to commit after the first manifest swap, and checks what the instance does then.
func fail(t *testing.T, seed uint64) failing {
	t.Helper()
	f := failing{events: &recorder{}}
	synctest.Test(t, func(t *testing.T) {
		s := sim.New(seed, sim.Options{Buggify: sim.Buggify{On: true, Activation: 1, Fire: 1}})
		dir := t.TempDir()
		is.NotError(t, s.Run(func(ctx context.Context) {
			st := simstore.New(s, simstore.Options{Latency: 10 * time.Millisecond})
			inst := s.NewInstance("a")
			_ = inst.Go(ctx, "test", func(ctx context.Context) {
				path := filepath.Join(dir, "a.db")
				o := options()
				o.Logger = f.events.logger()
				d, err := core.Open(ctx, inst, st.Client("a", inst), path, "db", o)
				is.NotError(t, err)
				is.NotError(t, d.Wait(ctx))

				tx, err := d.Begin(ctx)
				is.NotError(t, err)
				is.NotError(t, tx.CreateTable(ctx, tableOf("t1", 1, "k")))
				is.NotError(t, tx.Upsert(ctx, "t1", row(1)))
				// The manifest swap is applied, so the commit is acknowledged, whatever the cache does.
				is.NotError(t, tx.Commit(ctx))

				// By then the instance has stopped: no read starts on a cache that may lack the commit.
				is.True(t, isDone(d), "the instance has not signalled a stop")
				for name, err := range map[string]error{"err": d.Err(), "wait": d.Wait(ctx)} {
					is.Error(t, core.ErrStopped, err, name)
					is.Error(t, core.ErrLocal, err, name)
				}
				_, err = d.Read(ctx)
				is.Error(t, core.ErrLocal, err)
				_, err = d.Begin(ctx)
				is.Error(t, core.ErrLocal, err)

				// The commit is in the log, and the cache has it or not.
				data, _, _ := st.Object("db/manifest.json")
				man, err := format.ParseManifest(data)
				is.NotError(t, err)
				is.True(t, man.Head != format.EmptyLog, "the log does not have the commit")
				position, state, err := cacheDump(path)
				is.NotError(t, err)
				switch position {
				case man.Head:
					f.landed = true
					is.Equal(t, "t1(k*0): 1", show(state))
				case format.EmptyLog:
					is.Equal(t, "", show(state))
				default:
					t.Errorf("the cache is at %v", position)
				}

				// The instance releases the lease by itself, and does not open itself again.
				is.NotError(t, inst.Sleep(ctx, 10*time.Second))
				data, _, _ = st.Object("db/manifest.json")
				man, err = format.ParseManifest(data)
				is.NotError(t, err)
				is.True(t, man.Lease == nil, "the lease is not released")
				is.Error(t, core.ErrLocal, d.Wait(ctx))
				is.NotError(t, d.Close(ctx))

				// Opening it again shows the transaction, once, whichever way the cache failed.
				again, err := core.Open(ctx, inst, st.Client("again", inst), path, "db", options())
				is.NotError(t, err)
				is.NotError(t, again.Wait(ctx))
				r, err := again.Read(ctx)
				is.NotError(t, err)
				state, err = dump(ctx, r)
				is.NotError(t, err)
				is.Equal(t, "t1(k*0): 1", show(state))
				is.Equal(t, man.Head, r.Position())
				is.NotError(t, r.Close(ctx))
				// Under buggify the next commit stops the instance again, so it is closed as it is.
				is.NotError(t, again.Close(ctx))
			}).Wait(ctx)
		}))
	})
	return f
}

func TestDB_stop(t *testing.T) {
	t.Run("should acknowledge the commit, stop for good, release the lease once, and show the transaction when opened again, whether the cache rolled back or committed when it failed", func(t *testing.T) {
		variants := map[bool]int{}
		for seed := uint64(0); seed < 40 && (variants[true] == 0 || variants[false] == 0); seed++ {
			f := fail(t, seed)
			variants[f.landed]++

			stops := f.events.named(core.EventStop)
			is.Equal(t, 1, len(stops))
			is.Equal(t, "local_failure", at(stops[0], core.AttrOutcome))
			is.Equal(t, 1, len(f.events.with(core.EventCommit, core.AttrOutcome, "committed_and_stopped")))
			// One try to release the lease, which is the one of the stop: closing the instance finds
			// the lease released.
			is.Equal(t, 1, len(f.events.with(core.EventLease, core.AttrTransition, "released")))
		}
		is.True(t, variants[true] > 0 && variants[false] > 0, fmt.Sprint("the cache failed in one way only: ", variants))
	})

	t.Run("should stop for a failure to write the spool file, and commit nothing", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))
		head := w.manifest(t).Head

		// The directory of the cache takes no new files from now on.
		is.NotError(t, os.Chmod(w.dir, 0o500))
		defer func() { is.NotError(t, os.Chmod(w.dir, 0o700)) }()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 1)(t, tx)
		err = tx.Commit(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrLocal, err)
		is.True(t, !errors.Is(err, core.ErrOutcomeUnknown))

		is.True(t, isDone(d))
		is.Error(t, core.ErrLocal, d.Err())
		is.Error(t, core.ErrLocal, readErr(t, d))
		is.Equal(t, head, w.manifest(t).Head)
		is.NotError(t, os.Chmod(w.dir, 0o700))
		is.NotError(t, d.Close(t.Context()))
		is.True(t, w.manifest(t).Lease == nil, "the lease is not released")

		again := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0): ", read(t, again))
	})

	t.Run("should stop for a failure to write the spool file of the annotations", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		is.NotError(t, os.Chmod(w.dir, 0o500))
		defer func() { is.NotError(t, os.Chmod(w.dir, 0o700)) }()

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		err = tx.Annotate(t.Context(), "select 1", nil)
		is.Error(t, core.ErrLocal, err)
		is.Error(t, core.ErrStopped, tx.Commit(t.Context()))
		is.True(t, isDone(d))
	})
}

func TestMargin(t *testing.T) {
	t.Run("should be a tenth of the holder's duration, and two seconds at the least", func(t *testing.T) {
		for duration, margin := range map[time.Duration]time.Duration{
			time.Second:      2 * time.Second,
			10 * time.Second: 2 * time.Second,
			20 * time.Second: 2 * time.Second,
			30 * time.Second: 3 * time.Second,
			45 * time.Second: 4500 * time.Millisecond,
			10 * time.Minute: time.Minute,
		} {
			is.Equal(t, margin, core.Margin(duration), fmt.Sprint(duration))
		}
	})
}

func TestDB_stop_cache(t *testing.T) {
	t.Run("should stop for good when SQLite reports that the cache is corrupt, and fail the read that met it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{Readers: 2})
		transact(t, d, create("t1", 1, "k", "v"))
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		for i := range 20_000 {
			is.NotError(t, tx.Upsert(t.Context(), "t1", row(i, i)))
		}
		is.NotError(t, tx.Commit(t.Context()))

		// Another connection moves everything from the write-ahead log into the file of the cache,
		// and the file is then damaged from its second page on.
		path := filepath.Join(w.dir, "a.db")
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		is.NotError(t, c.Exec("pragma wal_checkpoint(truncate)"))
		is.NotError(t, c.Close())
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		is.NotError(t, err)
		info, err := f.Stat()
		is.NotError(t, err)
		_, err = f.WriteAt(bytes.Repeat([]byte{0xa5}, int(info.Size())-8192), 8192)
		is.NotError(t, err)
		is.NotError(t, f.Close())

		// A read connection that has read none of the table's pages meets the damage.
		r, err := d.Read(t.Context())
		is.NotError(t, err)
		var failed error
		for _, err := range r.Scan(t.Context(), "t1") {
			if err != nil {
				failed = err
			}
		}
		is.Error(t, core.ErrStopped, failed)
		is.Error(t, core.ErrLocal, failed)
		_ = r.Close(t.Context())

		is.True(t, isDone(d))
		is.Error(t, core.ErrLocal, d.Err())
		is.Error(t, core.ErrLocal, beginErr(t, d))
		is.Error(t, core.ErrLocal, readErr(t, d))
		is.NotError(t, d.Close(t.Context()))
		is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
	})
}

func TestDB_Close_hanging(t *testing.T) {
	t.Run("should return when the store gives no answer at all, after trying for as long as the lease lasts", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			s.on("swap", "", hang, -1)
			s.on("get", "", hang, -1)

			// Without a timeout on each request, this would wait for ever, and the bubble would report
			// a deadlock.
			start := time.Now()
			is.True(t, d.Close(t.Context()) != nil, "the close reports no error")
			is.True(t, time.Since(start) < time.Minute, fmt.Sprint(time.Since(start)))
		})
	})

	t.Run("should not close the cache under a write transaction that is open", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			is.Error(t, context.DeadlineExceeded, d.Close(ctx))
			is.NotError(t, tx.Rollback(t.Context()))
			is.NotError(t, d.Close(t.Context()))
		})
	})
}
