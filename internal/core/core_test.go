package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
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
	return &world{host: h, store: simstore.New(h, simstore.Options{}), dir: cacheDir(t)}
}

// cacheDir is a temporary directory for caches. When the test ends, after the cleanups that were
// registered later, such as those that close instances, it fails the test for each cache in it whose
// write-ahead log is left: see [leftOpen]. A test that has failed already is not checked, because
// a failure can skip the close of an instance.
func cacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		open, err := leftOpen(dir)
		is.NotError(t, err)
		for _, name := range open {
			t.Errorf("a connection to the cache %v is still open", name)
		}
	})
	return dir
}

// leftOpen are the caches in dir whose write-ahead log is there, which a connection that has read
// them keeps: "The WAL file exists for as long as any database connection has the database open.
// Usually, the WAL file is deleted automatically when the last connection to the database closes."
// (https://sqlite.org/wal.html) A connection that has not read a cache yet keeps no log, so not every
// connection that is left shows.
func leftOpen(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), "-wal"); ok {
			names = append(names, name)
		}
	}
	return names, nil
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
	return w.openWith(t, cache, w.host, s, options)
}

// openWith opens an instance on the host h that reaches the store through s.
func (w *world) openWith(t *testing.T, cache string, h host.Host, s store.Store, options core.Options) *core.DB {
	t.Helper()
	d, err := core.Open(t.Context(), h, s, filepath.Join(w.dir, cache+".db"), "db", options)
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

// text of a row: its values joined by commas, each exactly, so that two values have the same text
// only if they are the same, bit for bit: an integer in decimal, a real as "r" and the shortest
// decimal that reads back to its bits, with the sign of -0, text quoted as Go quotes it, a blob as
// x” with its bytes in hexadecimal, and null.
func text(values []*s4qlpb.Value) string {
	parts := make([]string, len(values))
	for i, v := range values {
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			parts[i] = fmt.Sprint(v.GetInteger())
		case s4qlpb.Value_Real_case:
			parts[i] = fmt.Sprint("r", v.GetReal())
		case s4qlpb.Value_Text_case:
			parts[i] = fmt.Sprintf("%q", v.GetText())
		case s4qlpb.Value_Blob_case:
			parts[i] = fmt.Sprintf("x'%x'", v.GetBlob())
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

// typeNames are SQLite's names of the types of a column, in lowercase, by their type in a commit
// object.
var typeNames = map[s4qlpb.Type]string{
	s4qlpb.Type_TYPE_INT:     "int",
	s4qlpb.Type_TYPE_INTEGER: "integer",
	s4qlpb.Type_TYPE_REAL:    "real",
	s4qlpb.Type_TYPE_TEXT:    "text",
	s4qlpb.Type_TYPE_BLOB:    "blob",
}

// typeName of the type of a column, as SQLite names it, in lowercase.
func typeName(typ s4qlpb.Type) string {
	if name, ok := typeNames[typ]; ok {
		return name
	}
	return fmt.Sprint("?", typ)
}

// typedTable of the given name, with one column of each of the given types, named c0, c1, and so
// on, of which the first is the primary key.
func typedTable(name string, types ...s4qlpb.Type) *s4qlpb.CreateTable {
	ct := s4qlpb.CreateTable_builder{Table: new(name), PrimaryKey: []uint32{0}}
	for i, typ := range types {
		ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(fmt.Sprint("c", i)), Type: new(typ)}.Build())
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
// position there, and with its type after a colon unless it is int, as in "t1(a*0,b:text)".
func describe(ct *s4qlpb.CreateTable) string {
	columns := make([]string, len(ct.GetColumns()))
	for i, c := range ct.GetColumns() {
		columns[i] = c.GetName()
		for position, key := range ct.GetPrimaryKey() {
			if int(key) == i {
				columns[i] += fmt.Sprint("*", position)
			}
		}
		if c.GetType() != s4qlpb.Type_TYPE_INT {
			columns[i] += ":" + typeName(c.GetType())
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

// newReader of d, which is closed when the test ends, before the instance is.
func newReader(t *testing.T, d *core.DB) *core.Reader {
	t.Helper()
	rd, err := d.Reader(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = rd.Close(context.WithoutCancel(t.Context())) })
	return rd
}

// startRead starts a read transaction on a reader of its own, as [newReader] gives it.
func startRead(t *testing.T, d *core.DB) (*core.ReadTx, error) {
	t.Helper()
	return newReader(t, d).Read(t.Context())
}

// startErr is the error of opening a reader of d and starting a read transaction on it. The
// transaction is closed again if it starts, and so is the reader.
func startErr(ctx context.Context, d *core.DB) error {
	rd, err := d.Reader(ctx)
	if err != nil {
		return err
	}
	r, err := rd.Read(ctx)
	if err == nil {
		err = r.Close(ctx)
	}
	if closeErr := rd.Close(ctx); err == nil {
		err = closeErr
	}
	return err
}

// read what the instance serves now.
func read(t *testing.T, d *core.DB) string {
	t.Helper()
	r, err := startRead(t, d)
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

	t.Run("should make a call that waits fail, close the cache under the transactions that are open, and fail their every call", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			upsert("t1", 2)(t, tx)
			rd := newReader(t, d)
			r, err := rd.Read(t.Context())
			is.NotError(t, err)

			// A writer waits for the write transaction. A read never waits.
			waiting := make(chan error, 1)
			go func() { waiting <- beginErr(t, d) }()
			time.Sleep(time.Second)
			is.Equal(t, 0, len(waiting))

			// No call of the two transactions runs, so Close closes the cache at once.
			start := time.Now()
			is.NotError(t, d.Close(t.Context()))
			is.Equal(t, 0, time.Since(start))
			is.True(t, isDone(d))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
			cacheClosed(t, w.dir)

			// The call that waited has failed, and the transactions that are open fail from now on.
			is.Error(t, core.ErrStopped, <-waiting)
			_, err = rd.Read(t.Context())
			is.Error(t, core.ErrStopped, err)
			_, err = r.Get(t.Context(), "t1", row(1))
			is.Error(t, core.ErrStopped, err)
			for _, err := range r.Scan(t.Context(), "t1") {
				is.Error(t, core.ErrStopped, err)
			}
			is.Error(t, core.ErrStopped, tx.Upsert(t.Context(), "t1", row(3)))
			is.Error(t, core.ErrStopped, tx.Commit(t.Context()))
			is.Error(t, core.ErrTxDone, tx.Rollback(t.Context()))
			is.NotError(t, r.Close(t.Context()))

			is.NotError(t, d.Close(t.Context()))
			again := w.serve(t, "a", core.Options{})
			is.Equal(t, "t1(k*0): 1", read(t, again))
		})
	})

	t.Run("should close the cache whatever the store does, and say that the lease is not released", func(t *testing.T) {
		for _, fault := range []what{drop, hang} {
			bubble(t, func(t *testing.T, w *world) {
				events := &recorder{}
				s := newScriptStore(w.client())
				d := w.openOn(t, "a", s, core.Options{Logger: events.logger()})
				is.NotError(t, d.Wait(t.Context()))
				transact(t, d, create("t1", 1, "k"))
				s.on("swap", "", fault, -1)
				s.on("get", "", fault, -1)

				start := time.Now()
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
				err := d.Close(ctx)
				cancel()
				is.True(t, err != nil && strings.Contains(err.Error(), "the lease is not released"), fmt.Sprint(err))
				// The release that failed is logged, once: Close ends the lease task before its try.
				released := events.with(core.EventLease, core.AttrTransition, "released")
				is.Equal(t, 1, len(released))
				is.Equal(t, "failed", at(released[0], core.AttrOutcome))
				is.Equal(t, slog.LevelWarn, levelOf(released[0]))
				// The release had half of the time.
				is.True(t, time.Since(start) <= time.Second, fmt.Sprint(time.Since(start)))
				cacheClosed(t, w.dir)
				is.Equal(t, d.ID(), w.manifest(t).Lease.Holder)

				// Closing it again does nothing, and the next instance waits for the lease to time out.
				s.clear()
				is.NotError(t, d.Close(t.Context()))
				b := w.open(t, "b", core.Options{})
				start = time.Now()
				is.NotError(t, b.Wait(t.Context()))
				is.True(t, time.Since(start) >= 30*time.Second+core.Margin(30*time.Second), fmt.Sprint(time.Since(start)))
				is.Equal(t, "t1(k*0): ", read(t, b))
			})
		}
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

	t.Run("should give up releasing at once when it cannot read the manifest", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			newHandLog(t, w).publishBytes([]byte("not a manifest"))

			start := time.Now()
			err := d.Close(t.Context())
			is.Error(t, format.ErrMalformed, err)
			is.Equal(t, 0, time.Since(start))
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
		dir := cacheDir(t)
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
				is.Error(t, core.ErrLocal, startErr(ctx, d))
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
				rd, err := again.Reader(ctx)
				is.NotError(t, err)
				r, err := rd.Read(ctx)
				is.NotError(t, err)
				state, err = dump(ctx, r)
				is.NotError(t, err)
				is.Equal(t, "t1(k*0): 1", show(state))
				is.Equal(t, man.Head, r.Position())
				is.NotError(t, r.Close(ctx))
				is.NotError(t, rd.Close(ctx))
				// Under buggify the next commit stops the instance again, so it is closed as it is.
				is.NotError(t, again.Close(ctx))
			}).Wait(ctx)
		}))
	})
	return f
}

// moveAway the directory, so that no new file can be made in it by its path, until the returned
// function or the end of the test moves it back. The files that are open in it stay open. It is a
// rename and not a change of permissions, which do not bind root, as in the container of the
// address sanitizer.
func moveAway(t *testing.T, dir string) (back func()) {
	t.Helper()
	away := dir + "-away"
	is.NotError(t, os.Rename(dir, away))
	_, err := os.CreateTemp(dir, "probe-*")
	is.True(t, err != nil, "a file was made in the directory that was moved away")
	moved := true
	back = func() {
		if moved {
			moved = false
			is.NotError(t, os.Rename(away, dir))
		}
	}
	t.Cleanup(back)
	return back
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
			is.Equal(t, slog.LevelError, levelOf(stops[0]))
			stopped := f.events.with(core.EventCommit, core.AttrOutcome, "committed_and_stopped")
			is.Equal(t, 1, len(stopped))
			is.Equal(t, slog.LevelWarn, levelOf(stopped[0]))
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
		back := moveAway(t, w.dir)
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
		back()
		is.NotError(t, d.Close(t.Context()))
		is.True(t, w.manifest(t).Lease == nil, "the lease is not released")

		again := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0): ", read(t, again))
	})

	t.Run("should stop for a failure to write the spool file of the annotations", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		moveAway(t, w.dir)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		err = tx.Annotate(t.Context(), "select 1", nil)
		is.Error(t, core.ErrLocal, err)
		is.Error(t, core.ErrStopped, tx.Commit(t.Context()))
		is.True(t, isDone(d))
	})

	// stoppedBySpool checks that a commit failed because a spool file failed, that the instance
	// stopped with a local failure and released the lease, and that nothing was committed: the log
	// is still at head, and the instance that is opened again serves state.
	stoppedBySpool := func(t *testing.T, w *world, d *core.DB, err error, head format.Name, state string) {
		t.Helper()
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrLocal, err)
		is.True(t, !errors.Is(err, core.ErrOutcomeUnknown))
		is.True(t, isDone(d))
		is.Error(t, core.ErrLocal, d.Err())
		is.Error(t, core.ErrLocal, readErr(t, d))
		is.Equal(t, head, w.manifest(t).Head)
		is.NotError(t, d.Close(t.Context()))
		is.True(t, w.manifest(t).Lease == nil, "the lease is not released")

		again := w.serve(t, "a", core.Options{})
		is.Equal(t, state, read(t, again))
	}

	t.Run("should stop for a failure to read the spool file during the upload, and not try the upload again", func(t *testing.T) {
		w := newWorld(t)
		o := core.Options{}
		faults := failSpools(&o)
		d := w.serve(t, "a", o)
		transact(t, d, create("t1", 1, "k"))
		head := w.manifest(t).Head

		// The upload reads the spool file of the commit object, and its first read fails. A second
		// upload would read it whole.
		faults.failReads("commit", 1)
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 1)(t, tx)
		err = tx.Commit(t.Context())
		is.Equal(t, 1, faults.failures())
		stoppedBySpool(t, w, d, err, head, "t1(k*0): ")
	})

	t.Run("should stop for a failure to read the length of an annotation from its spool file, and not commit without it", func(t *testing.T) {
		w := newWorld(t)
		o := core.Options{}
		faults := failSpools(&o)
		d := w.serve(t, "a", o)
		transact(t, d, create("t1", 1, "k"))
		head := w.manifest(t).Head

		// The first read of the spool file of the annotations is the one of the length of the first
		// annotation.
		faults.failReads("annotations", 1)
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 1)(t, tx)
		note("insert into t1 values (?)", 1)(t, tx)
		err = tx.Commit(t.Context())
		is.Equal(t, 1, faults.failures())
		stoppedBySpool(t, w, d, err, head, "t1(k*0): ")
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
		d := w.serve(t, "a", core.Options{})
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
		r, err := startRead(t, d)
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

	t.Run("should stop for good when another process makes the cache one of another version, or takes its position, while it runs", func(t *testing.T) {
		for _, tamper := range []string{
			fmt.Sprint("update s4ql_cache set version = ", core.CacheVersion+1),
			"delete from s4ql_cache",
		} {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"))

			c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.NotError(t, c.Exec(tamper))
			is.NotError(t, c.Close())

			err = beginErr(t, d)
			is.Error(t, core.ErrStopped, err, tamper)
			is.Error(t, core.ErrLocal, err, tamper)
			is.Error(t, core.ErrCacheVersion, err, tamper)
			is.True(t, isDone(d), tamper)
		}
	})

	t.Run("should stop for good when another process adds to the cache a table that s4ql did not make", func(t *testing.T) {
		for table, why := range map[string]string{
			"t9 (k any primary key) strict, without rowid":        `the column "k" of its table "t9" has the type "ANY"`,
			"t9 (k int primary key, v any) strict, without rowid": `the column "v" of its table "t9" has the type "ANY"`,
			"t9 (k int) strict": `its table "t9" has 1 columns and a primary key of 0`,
		} {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})

			c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.NotError(t, c.Exec("create table "+table))
			is.NotError(t, c.Close())

			r, err := startRead(t, d)
			is.NotError(t, err)
			_, err = r.Table(t.Context(), "t9")
			is.Error(t, core.ErrLocal, err, table)
			is.Error(t, core.ErrCacheVersion, err, table)
			is.True(t, strings.Contains(err.Error(), why), err.Error())
			_ = r.Close(t.Context())
			is.True(t, isDone(d), table)
		}
	})

	t.Run("should stop for good when the cache fails while it applies a commit object in a catch-up", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			first := w.serve(t, "first", core.Options{})
			transact(t, first, create("t1", 1, "k"))
			is.NotError(t, first.Close(t.Context()))

			// The first download of the commit object takes a while, and meanwhile another process
			// makes the cache one of another version.
			s := newScriptStore(w.client())
			s.on("get", "commits/", hold, 1, 5*time.Second)
			d := w.openOn(t, "a", s, core.Options{})
			time.Sleep(time.Second)
			c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.NotError(t, c.Exec(fmt.Sprint("update s4ql_cache set version = ", core.CacheVersion+1)))
			is.NotError(t, c.Close())

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			defer cancel()
			err = d.Wait(ctx)
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrLocal, err)
			is.Error(t, core.ErrCacheVersion, err)
		})
	})

	t.Run("should stop for good with a local failure, and not a broken log, when the file system fails while replay applies a change, and keep the cache as it was", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			// The connections to the cache are closed by the cleanups that are registered later, which
			// run first.
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			t.Cleanup(func() { is.NotError(t, vfs.Close()) })

			// The cache is made, so that the instance that opens it again writes nothing to it before
			// the catch-up.
			is.NotError(t, w.serve(t, "a", core.Options{}).Close(t.Context()))

			// The rows of the commit object take more pages than SQLite's page cache holds, about twice
			// as many, so replay writes pages to the write-ahead log while it applies the changes,
			// before its commit. The check of the bytes that replay read, below, fails the test if it
			// does not.
			first := w.serve(t, "first", core.Options{})
			columns := make([]string, 100)
			for i := range columns {
				columns[i] = fmt.Sprint("c", i)
			}
			write(t, first, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, columns...)))
				for i := range 4000 {
					values := make([]any, len(columns))
					for j := range values {
						// A value of eight bytes in SQLite's records.
						values[j] = int64(1)<<50 + int64(i*len(columns)+j)
					}
					is.NotError(t, tx.Upsert(t.Context(), "t1", row(values...)))
				}
			})
			head := w.manifest(t).Head
			object, _, ok := w.store.Object(commitKey("db", head))
			is.True(t, ok, "the store has no commit object")
			is.NotError(t, first.Close(t.Context()))

			// Every write to the write-ahead log fails.
			vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
			events := &recorder{}
			o := core.Options{Logger: events.logger()}
			o.VFS = vfs.Name()
			d := w.open(t, "a", o)

			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
			defer cancel()
			err = d.Wait(ctx)
			is.True(t, vfs.Failures() > 0, "no write failed")
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrLocal, err)
			is.True(t, !errors.Is(err, core.ErrLogBroken), "the stop blames the log")
			is.True(t, isDone(d))
			is.Error(t, core.ErrLocal, d.Err())
			is.NotError(t, d.Close(ctx))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")

			// Replay failed before it read the commit object to its end, so in a change, and not in
			// its commit. The event of the catch-up is logged by the lease task after the stop, and
			// the close waits for that task.
			failed := events.with(core.EventCatchUp, core.AttrOutcome, "failed")
			is.Equal(t, 1, len(failed))
			read, ok := failed[0][core.AttrBytes].(int64)
			is.True(t, ok, "the event has no count of bytes")
			is.True(t, read < int64(len(object)), fmt.Sprint("replay read ", read, " of ", len(object), " bytes"))

			// Nothing of the commit object stayed in the cache, which catches up when it is opened
			// again.
			position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, format.EmptyLog, position)
			is.Equal(t, 0, len(state))
			again := w.serve(t, "a", core.Options{})
			r, err := startRead(t, again)
			is.NotError(t, err)
			is.Equal(t, head, r.Position())
			rows, err := r.Get(t.Context(), "t1", row(int64(1)<<50))
			is.NotError(t, err)
			is.Equal(t, len(columns), len(rows))
			is.NotError(t, r.Close(t.Context()))
		})
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

	t.Run("should close the cache under a write transaction that is open, which is then rolled back", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"))
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			upsert("t1", 1)(t, tx)

			is.NotError(t, d.Close(t.Context()))
			cacheClosed(t, w.dir)
			is.Error(t, core.ErrStopped, tx.Upsert(t.Context(), "t1", row(2)))
			is.NotError(t, tx.Rollback(t.Context()))
			is.NotError(t, d.Close(t.Context()))

			again := w.serve(t, "a", core.Options{})
			is.Equal(t, "t1(k*0): ", read(t, again))
		})
	})
}

// cacheClosed fails the test if a connection to a cache in dir is still open, as far as [leftOpen]
// can tell.
func cacheClosed(t *testing.T, dir string) {
	t.Helper()
	open, err := leftOpen(dir)
	is.NotError(t, err)
	is.EqualSlice(t, nil, open)
}

func TestDB_Close_waiting(t *testing.T) {
	t.Run("should close the cache under a read transaction that is open, whose calls then fail", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"))
			r, err := startRead(t, d)
			is.NotError(t, err)
			_, err = r.Tables(t.Context())
			is.NotError(t, err)

			is.NotError(t, d.Close(t.Context()))
			cacheClosed(t, w.dir)
			_, err = r.Tables(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.NotError(t, r.Close(t.Context()))
		})
	})

	t.Run("should not close a connection under a call that runs on it, and close it when the call ends", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2))
			r, err := startRead(t, d)
			is.NotError(t, err)

			closed := make(chan error, 1)
			var rows []string
			for row, err := range r.Scan(t.Context(), "t1") {
				if err != nil {
					// The scan fails at its next row after the stop, and so ends.
					is.Error(t, core.ErrStopped, err)
					break
				}
				rows = append(rows, text(row))
				if len(rows) > 1 {
					continue
				}
				// Close waits for the scan, which is a call that runs on its connection.
				go func() { closed <- d.Close(t.Context()) }()
				time.Sleep(time.Minute)
				is.Equal(t, 0, len(closed))
				open, err := leftOpen(w.dir)
				is.NotError(t, err)
				is.EqualSlice(t, []string{"a.db"}, open)
			}
			is.EqualSlice(t, []string{"1"}, rows)
			is.NotError(t, <-closed)
			cacheClosed(t, w.dir)
			is.NotError(t, r.Close(t.Context()))
		})
	})

	t.Run("should return when its context is done while a call still runs, and close the connection when the call ends", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
			r, err := startRead(t, d)
			is.NotError(t, err)

			next, stop := iter.Pull2(r.Scan(t.Context(), "t1"))
			_, err, ok := next()
			is.True(t, ok)
			is.NotError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			err = d.Close(ctx)
			cancel()
			is.Error(t, context.DeadlineExceeded, err)
			is.True(t, strings.Contains(err.Error(), "a call still uses the cache"), err.Error())
			open, err := leftOpen(w.dir)
			is.NotError(t, err)
			is.EqualSlice(t, []string{"a.db"}, open)

			stop()
			cacheClosed(t, w.dir)
			is.NotError(t, r.Close(t.Context()))
			is.NotError(t, d.Close(t.Context()))
		})
	})

	t.Run("should end what the lease task is waiting for, and not wait for it", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			// The lease task waits for an answer that does not come before the timeout of a minute.
			s.on("get", "manifest", hang, -1)
			d := w.openOn(t, "a", s, core.Options{StoreTimeout: time.Minute})
			time.Sleep(time.Second)

			start := time.Now()
			is.NotError(t, d.Close(t.Context()))
			is.Equal(t, 0, time.Since(start))
		})
	})
}

func TestOpen_cache(t *testing.T) {
	t.Run("should record the name of the last commit object as the position of the cache, where a person can read it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		path := filepath.Join(w.dir, "a.db")

		position, _, err := cacheDump(path)
		is.NotError(t, err)
		is.Equal(t, format.EmptyLog, position)

		for i := range 3 {
			transact(t, d, create(fmt.Sprint("t", i), 1, "k"))
			position, _, err := cacheDump(path)
			is.NotError(t, err)
			is.Equal(t, w.manifest(t).Head, position)

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.Equal(t, position, tx.Position())
			is.NotError(t, tx.Rollback(t.Context()))
			r, err := startRead(t, d)
			is.NotError(t, err)
			is.Equal(t, position, r.Position())
			is.NotError(t, r.Close(t.Context()))
		}
	})

	t.Run("should open a cache made before the other types, with its tables of int columns, and read and change them", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10), upsert("t1", 2, nil))
		head := w.manifest(t).Head
		is.NotError(t, d.Close(t.Context()))

		// The cache as the version before wrote it, statement by statement, at the position of the log.
		c, err := sqlite.Open(filepath.Join(w.dir, "old.db"))
		is.NotError(t, err)
		for _, sql := range []string{
			"pragma journal_mode = wal",
			"create table main.s4ql_cache (id integer primary key check (id = 1), version integer not null, head text not null, database text not null) strict",
			"insert into main.s4ql_cache (id, version, head, database) values (1, 1, '" + head.String() + "', '')",
			`create table main."t1" ("k" int, "v" int, primary key ("k")) strict, without rowid`,
			`insert or replace into main."t1" ("k", "v") values (1, 10)`,
			`insert or replace into main."t1" ("k", "v") values (2, null)`,
		} {
			is.NotError(t, c.Exec(sql), sql)
		}
		is.NotError(t, c.Close())

		old := w.serve(t, "old", core.Options{})
		is.Equal(t, "t1(k*0,v): 1,10 | 2,null", read(t, old))
		transact(t, old, upsert("t1", 3, 30), create("t2", 1, "k"), upsert("t2", 1))
		is.Equal(t, "t1(k*0,v): 1,10 | 2,null | 3,30\nt2(k*0): 1", read(t, old))
		is.NotError(t, old.Close(t.Context()))
		is.Equal(t, "t1(k*0,v): 1,10 | 2,null | 3,30\nt2(k*0): 1", read(t, w.serve(t, "b", core.Options{})))
	})

	t.Run("should fail for a file that is not a database, and leave it as it is", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		is.NotError(t, os.WriteFile(path, []byte(strings.Repeat("this is not a database\n", 100)), 0o600))

		_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
		is.Error(t, core.ErrLocal, err)
		data, err := os.ReadFile(path)
		is.NotError(t, err)
		is.True(t, strings.HasPrefix(string(data), "this is not a database"), "the file was rebuilt")
	})

	t.Run("should fail for a cache in a directory that does not exist", func(t *testing.T) {
		w := newWorld(t)
		_, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, "missing", "a.db"), "db", core.Options{})
		is.Error(t, core.ErrLocal, err)
	})

	t.Run("should fail for a cache of another version, and for one whose position is no name, and leave it as it is", func(t *testing.T) {
		for _, tamper := range []string{
			fmt.Sprint("update s4ql_cache set version = ", core.CacheVersion+1),
			"update s4ql_cache set head = 'not a name'",
			"delete from s4ql_cache",
		} {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
			is.NotError(t, d.Close(t.Context()))

			path := filepath.Join(w.dir, "a.db")
			c, err := sqlite.Open(path)
			is.NotError(t, err)
			is.NotError(t, c.Exec(tamper))
			is.NotError(t, c.Close())

			_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.Error(t, core.ErrCacheVersion, err, tamper)

			// Nothing rebuilt it: the table and its row are still there.
			c, err = sqlite.Open(path)
			is.NotError(t, err)
			is.NotError(t, c.Exec("insert into t1 (k) values (2)"))
			is.True(t, c.Exec("insert into t1 (k) values (1)") != nil, "the row is gone")
			is.NotError(t, c.Close())
		}
	})

	t.Run("should refuse options, prefixes, and paths that make no sense", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		for name, options := range map[string]core.Options{
			"a renewal after the lease is over": {Lease: 10 * time.Second, RenewAfter: 10 * time.Second},
			"a lease below zero":                {Lease: -time.Second},
			"a timeout below zero":              {StoreTimeout: -time.Second},
			"a busy timeout below zero":         {BusyTimeout: -time.Second},
			"an object timeout below zero":      {ObjectTimeout: -time.Second},
			"a watch interval below zero":       {Watch: -time.Second},
			"a length limit below 1000":         {LengthLimit: 999},
			"a length limit above SQLite's":     {LengthLimit: 1_000_000_001},
		} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", options)
			is.True(t, err != nil, name)
		}
		_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{LengthLimit: 999})
		is.True(t, strings.Contains(fmt.Sprint(err), "the length limit 999 is not from 1000 to 1000000000"), fmt.Sprint(err))
		for _, prefix := range []string{"/db", "db/", "a//b", ".", "a/../b", "a/./b", "\xff", strings.Repeat("x", 901)} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, prefix, core.Options{})
			is.True(t, err != nil, prefix)
		}
		for _, path := range []string{"", ":memory:", "file:a.db"} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.True(t, err != nil, path)
		}
		_, err = os.Stat(path)
		is.True(t, os.IsNotExist(err), "a cache was made")
	})

	t.Run("should keep the objects of a database under its prefix, and those of a database without a prefix at the top", func(t *testing.T) {
		w := newWorld(t)
		for _, prefix := range []string{"", "one", "one/two", "smørrebrød/🥪"} {
			d, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, fmt.Sprint(len(prefix), ".db")), prefix, core.Options{})
			is.NotError(t, err)
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, create("t1", 1, "k"))
			is.NotError(t, d.Close(t.Context()))
		}
		var manifests, commits []string
		for _, key := range w.store.Keys() {
			if name, ok := strings.CutSuffix(key, "manifest.json"); ok {
				manifests = append(manifests, name)
				continue
			}
			i := strings.LastIndex(key, "commits/")
			rest := key[i+len("commits/"):]
			is.True(t, i >= 0 && len(rest) == 67 && rest[2] == '/' && rest[:2] == rest[3:5], key)
			commits = append(commits, key[:i])
		}
		is.EqualSlice(t, []string{"", "one/", "one/two/", "smørrebrød/🥪/"}, manifests)
		is.EqualSlice(t, []string{"", "one/", "one/two/", "smørrebrød/🥪/"}, commits)
	})
}

// cacheIdentity is the identity of the database that the cache at path records.
func cacheIdentity(t *testing.T, path string) string {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	defer func() { is.NotError(t, c.Close()) }()
	s, err := c.Prepare("select database from s4ql_cache")
	is.NotError(t, err)
	ok, err := s.Step()
	is.NotError(t, err)
	is.True(t, ok, "the cache records no identity")
	identity := s.ColumnText(0)
	is.NotError(t, s.Finalize())
	return identity
}

func TestOpen_identity(t *testing.T) {
	// hasRow1 reports whether the table t1 of the cache at path has the row with the key 1, without
	// changing the cache.
	hasRow1 := func(t *testing.T, path string) bool {
		t.Helper()
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		s, err := c.Prepare("select 1 from t1 where k = 1")
		is.NotError(t, err)
		ok, err := s.Step()
		is.NotError(t, err)
		is.NotError(t, s.Finalize())
		return ok
	}

	t.Run("should record the identity of its database in a new cache, also the empty one", func(t *testing.T) {
		w := newWorld(t)
		for cache, identity := range map[string]string{"a": "the database of a", "b": ""} {
			is.NotError(t, w.serve(t, cache, core.Options{Identity: identity}).Close(t.Context()))
			is.Equal(t, identity, cacheIdentity(t, filepath.Join(w.dir, cache+".db")))
		}
	})

	t.Run("should serve again from a cache that records the identity of its database", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{Identity: "one"})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		is.NotError(t, d.Close(t.Context()))

		d = w.serve(t, "a", core.Options{Identity: "one"})
		is.Equal(t, "t1(k*0): 1", read(t, d))
		transact(t, d, upsert("t1", 2))
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))
	})

	t.Run("should refuse a cache that records another identity, name both and what to do, and leave the cache as it is", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		d := w.serve(t, "a", core.Options{Identity: "one"})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
		is.NotError(t, d.Close(t.Context()))

		for _, other := range []string{"two", "", "one ", "One"} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{Identity: other})
			is.Error(t, core.ErrCacheDatabase, err, other)
			is.True(t, !errors.Is(err, core.ErrLocal), "a local I/O failure")
			is.Equal(t, fmt.Sprintf(`open the cache: the cache belongs to another database: it records the database "one", and this instance opens the database %q; open each database with a cache of its own, or delete the cache, with its files -wal and -shm, if the database has moved`, other), err.Error())
		}

		is.Equal(t, "one", cacheIdentity(t, path))
		is.True(t, hasRow1(t, path), "the row is gone")
		d = w.serve(t, "a", core.Options{Identity: "one"})
		is.Equal(t, "t1(k*0): 1", read(t, d))
	})

	t.Run("should refuse a cache of version 0, which records no identity, and leave it as it is", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		for _, s := range []string{
			"pragma journal_mode = wal",
			"create table s4ql_cache (id integer primary key check (id = 1), version integer not null, head text not null) strict",
			"insert into s4ql_cache (id, version, head) values (1, 0, '" + format.EmptyLog.String() + "')",
			"create table t1 (k int, primary key (k)) strict, without rowid",
			"insert into t1 (k) values (1)",
		} {
			is.NotError(t, c.Exec(s), s)
		}
		is.NotError(t, c.Close())

		_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{Identity: "one"})
		is.Error(t, core.ErrCacheVersion, err)
		is.Equal(t, "open the cache: the cache is not one that this version of s4ql reads: it has version 0, and this version of s4ql reads version 1; open the database with a new cache, or delete this one, with its files -wal and -shm", err.Error())
		is.True(t, hasRow1(t, path), "the row is gone")
		position, _, err := cacheDump(path)
		is.NotError(t, err)
		is.Equal(t, format.EmptyLog, position)
	})

	t.Run("should let exactly one of two instances of two databases that open one new cache at once have it, and refuse the other, many times", func(t *testing.T) {
		w := newWorld(t)
		for i := range 50 {
			path := filepath.Join(w.dir, fmt.Sprint(i, ".db"))
			// A slow machine must not make the cache busy.
			options := core.Options{BusyTimeout: time.Hour}
			identities := []string{"one", "two"}
			dbs := make([]*core.DB, len(identities))
			errs := make([]error, len(identities))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for j, identity := range identities {
				o := options
				o.Identity = identity
				s := w.client()
				wg.Go(func() {
					<-start
					dbs[j], errs[j] = core.Open(t.Context(), w.host, s, path, "db", o)
				})
			}
			close(start)
			wg.Wait()

			winner := -1
			for j := range identities {
				if errs[j] == nil {
					is.True(t, winner < 0, "both instances opened the cache")
					winner = j
					continue
				}
				is.Error(t, core.ErrCacheDatabase, errs[j], fmt.Sprint(i))
			}
			is.True(t, winner >= 0, fmt.Sprint("neither instance opened the cache: ", errs))
			loser := 1 - winner
			is.True(t, strings.Contains(errs[loser].Error(), fmt.Sprintf("it records the database %q, and this instance opens the database %q", identities[winner], identities[loser])), errs[loser].Error())
			is.Equal(t, identities[winner], cacheIdentity(t, path))

			d := dbs[winner]
			is.NotError(t, d.Wait(t.Context()))
			is.NotError(t, d.Close(t.Context()))
		}
	})

	t.Run("should let two instances of one database that open one new cache at once both have it, one serving and the other a spare", func(t *testing.T) {
		w := newWorld(t)
		for i := range 20 {
			path := filepath.Join(w.dir, fmt.Sprint(i, ".db"))
			// A slow machine must not make the cache busy, nor the lease run out, and the spare looks
			// often for a lease that was released.
			options := core.Options{BusyTimeout: time.Hour, Lease: time.Hour, Watch: 10 * time.Millisecond, Identity: "one"}
			dbs := make([]*core.DB, 2)
			errs := make([]error, 2)
			start := make(chan struct{})
			var wg sync.WaitGroup
			for j := range dbs {
				s := w.client()
				wg.Go(func() {
					<-start
					dbs[j], errs[j] = core.Open(t.Context(), w.host, s, path, fmt.Sprint("db", i), options)
				})
			}
			close(start)
			wg.Wait()
			is.NotError(t, errors.Join(errs...))

			// One of them gets the lease, and the other serves once the first has closed.
			type wait struct {
				j   int
				err error
			}
			served := make(chan wait, len(dbs))
			for j, d := range dbs {
				go func() { served <- wait{j: j, err: d.Wait(t.Context())} }()
			}
			first := <-served
			is.NotError(t, first.err)
			transact(t, dbs[first.j], create("t1", 1, "k"), upsert("t1", 1))
			is.NotError(t, dbs[first.j].Close(t.Context()))
			second := <-served
			is.NotError(t, second.err)
			is.True(t, first.j != second.j)
			is.Equal(t, "t1(k*0): 1", read(t, dbs[second.j]))
			is.NotError(t, dbs[second.j].Close(t.Context()))
		}
	})
}

func TestDB_pause(t *testing.T) {
	t.Run("should not pause when the state has changed since it was read, and pause otherwise", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			start := time.Now()
			is.NotError(t, d.PauseUnlessChanged(t.Context(), time.Hour, true))
			is.Equal(t, 0, time.Since(start))
			is.NotError(t, d.PauseUnlessChanged(t.Context(), time.Second, false))
			is.Equal(t, time.Second, time.Since(start))
		})
	})
}

func TestOpen_manifest(t *testing.T) {
	t.Run("should stop for a manifest of more than a mebibyte, without reading all of it", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		l.publishBytes([]byte(`{"version": 0, "branches": {"main": {"head": "0000000000000000000000000000000000000000000000000000000000000000"}}, "more": "` + strings.Repeat("x", 1<<20) + `"}`))
		d := w.open(t, "a", core.Options{})
		err := d.Wait(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, format.ErrMalformed, err)
	})

	t.Run("should stop for a manifest of more than a mebibyte whose first mebibyte is a manifest", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		data, err := format.Manifest{Head: format.EmptyLog}.Marshal()
		is.NotError(t, err)
		l.publishBytes(append(data, bytes.Repeat([]byte(" "), 1<<20)...))
		d := w.open(t, "a", core.Options{})
		err = d.Wait(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, format.ErrMalformed, err)
	})
}
