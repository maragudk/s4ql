package core_test

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sim"
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
