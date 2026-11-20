package core_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/proto"
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

// commits of the log of the world's database, oldest first, each as the lines of [changes].
func (w *world) commits(t *testing.T) [][]string {
	t.Helper()
	var log [][]string
	for name := w.manifest(t).Head; name != format.EmptyLog; {
		data, _, ok := w.store.Object(commitKey("db", name))
		is.True(t, ok, "the log names a commit object that the store does not have")
		parent, lines, err := changes(name, data)
		is.NotError(t, err)
		log = append([][]string{lines}, log...)
		name = parent
	}
	return log
}

// changes of the commit object with the given name and bytes, one line for each change and each
// annotation, such as "create t1(k*0,v)", "drop t1", "upsert t1 [0 1] 1,2", "delete t1 1", and
// "note insert... [1,2]", and the name of its parent.
func changes(name format.Name, data []byte) (format.Name, []string, error) {
	r := format.NewCommitReader(bytes.NewReader(data), name)
	header, err := r.Header()
	if err != nil {
		return format.Name{}, nil, err
	}
	parent, err := format.NameFromBytes(header.GetParent())
	if err != nil {
		return format.Name{}, nil, err
	}
	var lines []string
	for {
		change, annotation, err := r.Next()
		if errors.Is(err, io.EOF) {
			return parent, lines, nil
		}
		if err != nil {
			return format.Name{}, nil, err
		}
		switch {
		case annotation != nil:
			s := annotation.GetStatement()
			lines = append(lines, fmt.Sprint("note ", s.GetSql(), " [", text(s.GetParameters()), "]"))
		case change.HasCreateTable():
			lines = append(lines, "create "+describe(change.GetCreateTable()))
		case change.HasDropTable():
			lines = append(lines, "drop "+change.GetDropTable().GetTable())
		case change.HasUpsertRows():
			for _, row := range change.GetUpsertRows().GetRows() {
				lines = append(lines, fmt.Sprint("upsert ", change.GetUpsertRows().GetTable(), " ", change.GetUpsertRows().GetColumns(), " ", text(row.GetValues())))
			}
		case change.HasDeleteRows():
			for _, key := range change.GetDeleteRows().GetPrimaryKeys() {
				lines = append(lines, fmt.Sprint("delete ", change.GetDeleteRows().GetTable(), " ", text(key.GetValues())))
			}
		}
	}
}

// step of a transaction in a test of the net effect.
type step func(t *testing.T, tx *core.Tx)

func create(name string, keys int, columns ...string) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.CreateTable(t.Context(), tableOf(name, keys, columns...)))
	}
}

// createWith the table ct.
func createWith(ct *s4qlpb.CreateTable) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.CreateTable(t.Context(), ct))
	}
}

func dropTable(name string) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.DropTable(t.Context(), name))
	}
}

func upsert(name string, vs ...any) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.Upsert(t.Context(), name, row(vs...)))
	}
}

func remove(name string, found bool, vs ...any) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		got, err := tx.Delete(t.Context(), name, row(vs...))
		is.NotError(t, err)
		is.Equal(t, found, got)
	}
}

func note(sql string, vs ...any) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.Annotate(t.Context(), sql, row(vs...)))
	}
}

// transact runs the steps in one write transaction of d, and commit it.
func transact(t *testing.T, d *core.DB, steps ...step) {
	t.Helper()
	write(t, d, func(tx *core.Tx) {
		for _, s := range steps {
			s(t, tx)
		}
	})
}

func TestTx_Commit(t *testing.T) {
	// Each test starts from the same two tables: t1 with the rows 1,10 and 2,20, and t2 with the
	// row 1,1,100.
	start := []step{
		create("t1", 1, "k", "v"), upsert("t1", 1, 10), upsert("t1", 2, 20),
		create("t2", 2, "a", "b", "c"), upsert("t2", 1, 1, 100),
	}

	for _, test := range []struct {
		name  string
		steps []step
		// commit is the commit object of the steps, and nil if they leave nothing to commit.
		commit []string
		// state of the database after the steps.
		state string
	}{
		{
			name:   "should list a row once that was upserted twice, with its last values",
			steps:  []step{upsert("t1", 3, 30), upsert("t1", 3, 31), upsert("t1", 1, 11), upsert("t1", 1, 12)},
			commit: []string{"upsert t1 [0 1] 1,12", "upsert t1 [0 1] 3,31"},
			state:  "t1(k*0,v): 1,12 | 2,20 | 3,31\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should leave nothing of a row that was inserted and deleted again",
			steps:  []step{upsert("t1", 3, 30), remove("t1", true, 3), upsert("t1", 4, 40)},
			commit: []string{"upsert t1 [0 1] 4,40"},
			state:  "t1(k*0,v): 1,10 | 2,20 | 4,40\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:  "should commit nothing when every change is undone",
			steps: []step{upsert("t1", 3, 30), remove("t1", true, 3), remove("t1", false, 9), create("t3", 1, "k"), dropTable("t3"), note("insert into t1 values (3, 30)")},
			state: "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list the delete of a row that was there, once, and not the delete of one that was not",
			steps:  []step{remove("t1", true, 1), remove("t1", false, 1), remove("t1", false, 7), remove("t2", true, 1, 1)},
			commit: []string{"delete t1 1", "delete t2 1,1"},
			state:  "t1(k*0,v): 2,20\nt2(a*0,b*1,c): ",
		},
		{
			name:   "should list a row as upserted that was deleted and inserted again",
			steps:  []step{remove("t1", true, 1), upsert("t1", 1, 15)},
			commit: []string{"upsert t1 [0 1] 1,15"},
			state:  "t1(k*0,v): 1,15 | 2,20\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list a row as deleted that was upserted and then deleted",
			steps:  []step{upsert("t1", 1, 15), remove("t1", true, 1)},
			commit: []string{"delete t1 1"},
			state:  "t1(k*0,v): 2,20\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list upserts before deletes for a table, each in the order of the primary key",
			steps:  []step{upsert("t1", 9, 90), remove("t1", true, 2), upsert("t1", 5, 50), remove("t1", true, 1)},
			commit: []string{"upsert t1 [0 1] 5,50", "upsert t1 [0 1] 9,90", "delete t1 1", "delete t1 2"},
			state:  "t1(k*0,v): 5,50 | 9,90\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list a table that was created with all its rows, and nothing of rows that came and went",
			steps:  []step{createWith(nullableTable(tableOf("t3", 1, "k", "v"), 1)), upsert("t3", 2, nil), upsert("t3", 1, 1), upsert("t3", 3, 3), remove("t3", true, 3), upsert("t3", 1, 2)},
			commit: []string{"create t3(k*0,v?)", "upsert t3 [0 1] 1,2", "upsert t3 [0 1] 2,null"},
			state:  "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100\nt3(k*0,v?): 1,2 | 2,null",
		},
		{
			name:   "should list only the drop of a table that was changed and then dropped",
			steps:  []step{upsert("t1", 3, 30), remove("t1", true, 1), dropTable("t1")},
			commit: []string{"drop t1"},
			state:  "t2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list a drop, a create, and the rows for a table that was dropped and created again",
			steps:  []step{upsert("t1", 3, 30), dropTable("t1"), create("t1", 1, "v", "k"), upsert("t1", 7, 70), upsert("t1", 1, 5)},
			commit: []string{"drop t1", "create t1(v*0,k)", "upsert t1 [0 1] 1,5", "upsert t1 [0 1] 7,70"},
			state:  "t1(v*0,k): 1,5 | 7,70\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list only the drop for a table that was dropped, created again, and dropped again",
			steps:  []step{dropTable("t1"), create("t1", 1, "x"), upsert("t1", 1), dropTable("t1")},
			commit: []string{"drop t1"},
			state:  "t2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list only the last table of a name that was created, dropped, and created again",
			steps:  []step{create("t3", 1, "x"), upsert("t3", 1), dropTable("t3"), create("t3", 1, "y"), upsert("t3", 2)},
			commit: []string{"create t3(y*0)", "upsert t3 [0] 2"},
			state:  "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100\nt3(y*0): 2",
		},
		{
			name:   "should treat names that differ only in the case of ASCII letters as one table, and keep each as it was written",
			steps:  []step{upsert("T1", 3, 30), dropTable("T1"), create("T1", 1, "K"), upsert("t1", 4)},
			commit: []string{"drop t1", "create T1(K*0)", "upsert T1 [0] 4"},
			state:  "T1(K*0): 4\nt2(a*0,b*1,c): 1,1,100",
		},
		{
			name:   "should list the tables in the order in which the transaction first changed them",
			steps:  []step{upsert("t2", 2, 2, 200), create("t0", 1, "k"), upsert("t1", 3, 30), upsert("t2", 0, 0, 0)},
			commit: []string{"upsert t2 [0 1 2] 0,0,0", "upsert t2 [0 1 2] 2,2,200", "create t0(k*0)", "upsert t1 [0 1] 3,30"},
			state:  "t0(k*0): \nt1(k*0,v): 1,10 | 2,20 | 3,30\nt2(a*0,b*1,c): 0,0,0 | 1,1,100 | 2,2,200",
		},
		{
			name: "should list the annotations after the changes, in the order in which they were made, with their parameters",
			steps: []step{
				note("insert into t1 (k, v) values (?, ?)", 3, 30), upsert("t1", 3, 30),
				note("delete from t1 where k = ?", 1), remove("t1", true, 1),
				note("select ?, ?, ?, ?, ?", "text", 1.5, nil, []byte{1}, math.Copysign(0, -1)),
			},
			commit: []string{
				"upsert t1 [0 1] 3,30", "delete t1 1",
				"note insert into t1 (k, v) values (?, ?) [3,30]", "note delete from t1 where k = ? [1]",
				`note select ?, ?, ?, ?, ? ["text",r1.5,null,x'01',r-0]`,
			},
			state: "t1(k*0,v): 2,20 | 3,30\nt2(a*0,b*1,c): 1,1,100",
		},
	} {
		t.Run(test.name+", and replay it to the same state", func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, start...)
			before := w.manifest(t).Head

			transact(t, d, test.steps...)
			log := w.commits(t)
			if test.commit == nil {
				is.Equal(t, before, w.manifest(t).Head)
				is.Equal(t, 1, len(log))
			} else {
				is.Equal(t, 2, len(log))
				is.EqualSlice(t, test.commit, log[1])
			}
			is.Equal(t, test.state, read(t, d))
			is.NotError(t, d.Close(t.Context()))

			// An empty cache replays the log to the same state as the cache that the transaction
			// changed.
			replayed := w.serve(t, "b", core.Options{})
			is.Equal(t, test.state, read(t, replayed))
		})
	}

	t.Run("should send every write to the store with the checksum of its content, and name a commit object by it", func(t *testing.T) {
		w := newWorld(t)
		s := newScriptStore(w.client())
		d := w.openOn(t, "a", s, core.Options{})
		is.NotError(t, d.Wait(t.Context()))
		transact(t, d, start...)
		transact(t, d, upsert("t1", 3, 30))
		is.NotError(t, d.Close(t.Context()))

		is.EqualSlice(t, nil, s.unchecked)
		is.Equal(t, 2, s.count("create", "db/commits/"))
		for _, key := range w.store.Keys() {
			if !strings.HasPrefix(key, "db/commits/") {
				is.Equal(t, "db/manifest.json", key)
				continue
			}
			data, _, _ := w.store.Object(key)
			sum := sha256.Sum256(data)
			// The key is in a folder named by the first two digits of the name, and ends with the name.
			name := hex.EncodeToString(sum[:])
			is.Equal(t, "db/commits/"+name[:2]+"/"+name, key)
		}
	})

	t.Run("should let the transaction read its own writes, which no read transaction sees before the commit", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 3, 30)(t, tx)
		remove("t1", true, 1)(t, tx)
		create("t3", 1, "k")(t, tx)
		dropTable("t2")(t, tx)

		got, err := tx.Get(t.Context(), "t1", row(3))
		is.NotError(t, err)
		is.Equal(t, "3,30", text(got))
		got, err = tx.Get(t.Context(), "t1", row(1))
		is.NotError(t, err)
		is.True(t, got == nil)
		state, err := dump(t.Context(), tx)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0,v): 2,20 | 3,30\nt3(k*0): ", show(state))
		table, err := tx.Table(t.Context(), "T3")
		is.NotError(t, err)
		is.Equal(t, "t3(k*0)", describe(table))
		_, err = tx.Table(t.Context(), "t2")
		is.Error(t, core.ErrNoTable, err)

		is.Equal(t, "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100", read(t, d))
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, "t1(k*0,v): 2,20 | 3,30\nt3(k*0): ", read(t, d))
	})

	t.Run("should keep nothing of a transaction that is rolled back", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		head := w.manifest(t).Head

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 3, 30)(t, tx)
		dropTable("t2")(t, tx)
		create("t3", 1, "k")(t, tx)
		note("drop table t2")(t, tx)
		is.NotError(t, tx.Rollback(t.Context()))

		is.Equal(t, head, w.manifest(t).Head)
		is.Equal(t, "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100", read(t, d))
		// The next transaction starts clean.
		transact(t, d, upsert("t1", 4, 40))
		is.EqualSlice(t, []string{"upsert t1 [0 1] 4,40"}, w.commits(t)[1])
	})

	t.Run("should refuse every call after the transaction has ended", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		for _, end := range []func(tx *core.Tx) error{
			func(tx *core.Tx) error { return tx.Commit(t.Context()) },
			func(tx *core.Tx) error { return tx.Rollback(t.Context()) },
		} {
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, end(tx))

			is.Error(t, core.ErrTxDone, tx.Commit(t.Context()))
			is.Error(t, core.ErrTxDone, tx.Rollback(t.Context()))
			is.Error(t, core.ErrTxDone, tx.CreateTable(t.Context(), tableOf("t1", 1, "k")))
			is.Error(t, core.ErrTxDone, tx.DropTable(t.Context(), "t1"))
			is.Error(t, core.ErrTxDone, tx.Upsert(t.Context(), "t1", row(1)))
			_, err = tx.Delete(t.Context(), "t1", row(1))
			is.Error(t, core.ErrTxDone, err)
			is.Error(t, core.ErrTxDone, tx.Annotate(t.Context(), "select 1", nil))
			_, err = tx.Get(t.Context(), "t1", row(1))
			is.Error(t, core.ErrTxDone, err)
			_, err = tx.Table(t.Context(), "t1")
			is.Error(t, core.ErrTxDone, err)
			_, err = tx.Tables(t.Context())
			is.Error(t, core.ErrTxDone, err)
			for _, err := range tx.Scan(t.Context(), "t1") {
				is.Error(t, core.ErrTxDone, err)
			}
		}
	})

	t.Run("should refuse what the database cannot hold, each with its own error, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		ctx := t.Context()

		is.Error(t, core.ErrTableExists, tx.CreateTable(ctx, tableOf("t1", 1, "k")))
		is.Error(t, core.ErrTableExists, tx.CreateTable(ctx, tableOf("T1", 1, "k")))
		is.Error(t, core.ErrNoTable, tx.DropTable(ctx, "t9"))
		is.Error(t, core.ErrNoTable, tx.Upsert(ctx, "t9", row(1)))
		_, err = tx.Delete(ctx, "t9", row(1))
		is.Error(t, core.ErrNoTable, err)
		_, err = tx.Get(ctx, "t9", row(1))
		is.Error(t, core.ErrNoTable, err)
		for _, err := range tx.Scan(ctx, "t9") {
			is.Error(t, core.ErrNoTable, err)
		}
		// The cache's own table is no table of the database.
		_, err = tx.Table(ctx, "s4ql_cache")
		is.Error(t, core.ErrNoTable, err)
		is.Error(t, core.ErrNoTable, tx.DropTable(ctx, "s4ql_cache"))
		is.Error(t, core.ErrNoTable, tx.Upsert(ctx, "s4ql_cache", row(1, 0, "x")))

		for _, name := range []string{"s4ql_cache", "s4ql_", "S4QL_x", "s4Ql_touched_1", "sqlite_schema", "SQLITE_x", "sqlite_"} {
			is.Error(t, core.ErrReservedName, tx.CreateTable(ctx, tableOf(name, 1, "k")))
		}

		for name, ct := range map[string]*s4qlpb.CreateTable{
			"no name":                     tableOf("", 1, "k"),
			"no columns":                  tableOf("t3", 0),
			"no primary key":              tableOf("t3", 0, "k"),
			"a column without a name":     tableOf("t3", 1, "k", ""),
			"two columns of one name":     tableOf("t3", 1, "k", "K"),
			"NUL in the name":             tableOf("t\x003", 1, "k"),
			"NUL in the name of a column": tableOf("t3", 1, "k\x00"),
			"a name that is not UTF-8":    tableOf("t\xff", 1, "k"),
			"a column without a type":     s4qlpb.CreateTable_builder{Table: new("t3"), Columns: []*s4qlpb.Column{s4qlpb.Column_builder{Name: new("k")}.Build()}, PrimaryKey: []uint32{0}}.Build(),
			"a column of an unknown type": s4qlpb.CreateTable_builder{Table: new("t3"), Columns: []*s4qlpb.Column{s4qlpb.Column_builder{Name: new("k"), Type: new(s4qlpb.Type(99))}.Build()}, PrimaryKey: []uint32{0}}.Build(),
			"a key column twice":          s4qlpb.CreateTable_builder{Table: new("t3"), Columns: tableOf("t3", 1, "k").GetColumns(), PrimaryKey: []uint32{0, 0}}.Build(),
			"a key column outside":        s4qlpb.CreateTable_builder{Table: new("t3"), Columns: tableOf("t3", 1, "k").GetColumns(), PrimaryKey: []uint32{1}}.Build(),
			"too many columns":            tableOf("t3", 1, manyColumns(2001)...),
		} {
			err := tx.CreateTable(ctx, ct)
			is.Error(t, core.ErrInvalid, err, name)
		}

		for name, values := range map[string][]*s4qlpb.Value{
			"too few values":              row(1),
			"too many values":             row(1, 2, 3),
			"NULL in the primary key":     row(nil, 2),
			"text in an integer column":   row(1, "2"),
			"a real in an integer column": row(1, 2.0),
			"a blob in an integer column": row(1, []byte{2}),
			"text in the primary key":     row("1", 2),
			"a value without a kind":      {row(1)[0], &s4qlpb.Value{}},
		} {
			is.Error(t, core.ErrInvalid, tx.Upsert(ctx, "t1", values), name)
		}
		for name, key := range map[string][]*s4qlpb.Value{
			"no values":          row(),
			"too many values":    row(1, 2),
			"NULL":               row(nil),
			"text":               row("1"),
			"a real":             row(1.0),
			"a value of no kind": {&s4qlpb.Value{}},
		} {
			_, err := tx.Delete(ctx, "t1", key)
			is.Error(t, core.ErrInvalid, err, name)
			_, err = tx.Get(ctx, "t1", key)
			is.Error(t, core.ErrInvalid, err, name)
		}
		is.Error(t, core.ErrInvalid, tx.Annotate(ctx, "select '\xff'", nil))
		is.Error(t, core.ErrInvalid, tx.Annotate(ctx, "select ?", []*s4qlpb.Value{{}}))
		is.Error(t, core.ErrInvalid, tx.Annotate(ctx, "select ?", row("\xff")))

		// None of it changed anything or broke the transaction.
		upsert("t1", 3, 30)(t, tx)
		is.NotError(t, tx.Commit(ctx))
		is.EqualSlice(t, []string{"upsert t1 [0 1] 3,30"}, w.commits(t)[1])
	})

	t.Run("should let one write transaction in at a time, and let the next one see what the first committed", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			first, err := d.Begin(t.Context())
			is.NotError(t, err)
			create("t1", 1, "k")(t, first)

			began := make(chan *core.Tx)
			go func() {
				second, err := d.Begin(t.Context())
				is.NotError(t, err)
				began <- second
			}()
			time.Sleep(time.Hour)
			select {
			case <-began:
				t.Fatal("a second write transaction began while the first was open")
			default:
			}

			is.NotError(t, first.Commit(t.Context()))
			second := <-began
			upsert("t1", 1)(t, second)
			is.NotError(t, second.Commit(t.Context()))
			is.Equal(t, "t1(k*0): 1", read(t, d))
		})
	})

	t.Run("should give up waiting for the transaction before it when the context ends", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			d := w.serve(t, "a", core.Options{})
			first, err := d.Begin(t.Context())
			is.NotError(t, err)

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			_, err = d.Begin(ctx)
			is.Error(t, context.DeadlineExceeded, err)
			is.NotError(t, first.Rollback(t.Context()))
		})
	})
}

// committing is a transaction that creates the table t2, ready to commit, on an instance that
// reaches the store through a scripted store, and that has the table t1.
func committing(t *testing.T, w *world, options core.Options) (*scriptStore, *core.DB, *core.Tx) {
	t.Helper()
	s := newScriptStore(w.client())
	d := w.openOn(t, "a", s, options)
	is.NotError(t, d.Wait(t.Context()))
	transact(t, d, create("t1", 1, "k"))
	tx, err := d.Begin(t.Context())
	is.NotError(t, err)
	create("t2", 1, "k")(t, tx)
	return s, d, tx
}

func TestTx_Commit_unknownOutcome(t *testing.T) {
	const without, with = "t1(k*0): ", "t1(k*0): \nt2(k*0): "

	for _, test := range []struct {
		name string
		op   string
		key  string
		what what
	}{
		{name: "should commit when the answer to the manifest swap is lost, by asking again", op: "swap", key: "manifest", what: lose},
		{name: "should commit when the manifest swap is lost on its way, by sending it again", op: "swap", key: "manifest", what: drop},
		{name: "should commit when the store refuses the manifest swap for a conflict and the manifest is unchanged", op: "swap", key: "manifest", what: conflict},
		{name: "should commit when the manifest swap gets no answer until its timeout", op: "swap", key: "manifest", what: hang},
		{name: "should commit when the manifest swap is applied and its answer comes after its timeout", op: "swap", key: "manifest", what: hold},
		{name: "should commit when the answer to the upload is lost, because the object that is there is success", op: "create", key: "commits/", what: lose},
		{name: "should commit when the upload is lost on its way, by sending it again", op: "create", key: "commits/", what: drop},
		{name: "should commit when the upload gets no answer until its timeout", op: "create", key: "commits/", what: hang},
	} {
		t.Run(test.name, func(t *testing.T) {
			bubble(t, func(t *testing.T, w *world) {
				s, d, tx := committing(t, w, core.Options{ObjectTimeout: 10 * time.Second})
				s.on(test.op, test.key, test.what, 1, time.Minute)

				is.NotError(t, tx.Commit(t.Context()))
				is.Equal(t, with, read(t, d))
				is.Equal(t, "[[create t1(k*0)] [create t2(k*0)]]", fmt.Sprint(w.commits(t)))
				is.True(t, s.count(test.op, test.key) >= 2, "the request was not sent again")
			})
		})
	}

	t.Run("should give up an upload that gets no answer after five minutes by default, and send it again", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("create", "commits/", hang, 1)

			start := time.Now()
			is.NotError(t, tx.Commit(t.Context()))
			elapsed := time.Since(start)
			is.True(t, elapsed >= 5*time.Minute && elapsed < 6*time.Minute, fmt.Sprint(elapsed))
			is.Equal(t, with, read(t, d))
		})
	})

	t.Run("should commit with a new swap when a renewal whose outcome was unknown turns out to be applied", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			// The renewal after 20 seconds is applied, and its answer is lost.
			s.on("swap", "manifest", lose, 1)
			time.Sleep(20*time.Second + 500*time.Millisecond)

			is.NotError(t, tx.Commit(t.Context()))
			is.Equal(t, with, read(t, d))
			is.Equal(t, 2, len(w.commits(t)))
		})
	})

	t.Run("should return that the outcome is unknown when the caller gives up, serve nothing, and then find the commit applied", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := tx.Commit(ctx)
			is.Error(t, core.ErrOutcomeUnknown, err)
			// The error wraps no other error, as the doc of Commit says: every other one means "not committed".
			is.True(t, !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errScripted), fmt.Sprint(err))
			is.Error(t, core.ErrNoLease, readErr(t, d))
			is.Error(t, core.ErrNoLease, beginErr(t, d))
			time.Sleep(10 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, with, read(t, d))
			is.Equal(t, 2, len(w.commits(t)))
		})
	})

	t.Run("should rule out a commit whose swap is stuck with a renewal, and only then serve the state without it", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", stick, 1)
			s.on("swap", "", drop, -1)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			is.Error(t, core.ErrOutcomeUnknown, tx.Commit(ctx))
			// The instance can read the manifest, which is unchanged, and that settles nothing,
			// because the stuck swap can still arrive.
			time.Sleep(10 * time.Second)
			is.Error(t, core.ErrNoLease, readErr(t, d))
			salt := w.manifest(t).Lease.Salt

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, without, read(t, d))
			is.True(t, w.manifest(t).Lease.Salt != salt, "the instance serves again without a renewal that rules the stuck swap out")

			// The stuck swap arrives, and is refused.
			errs := s.deliver()
			is.Equal(t, 1, len(errs))
			is.True(t, errs[0] != nil, "the stuck swap was applied after the instance had settled that it is not committed")
			is.Equal(t, without, read(t, d))
			is.Equal(t, 1, len(w.commits(t)))
			transact(t, d, create("t3", 1, "k"))
			is.Equal(t, "t1(k*0): \nt3(k*0): ", read(t, d))
		})
	})

	t.Run("should find a commit applied whose stuck swap arrived while the instance could not reach the store", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", stick, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			is.Error(t, core.ErrOutcomeUnknown, tx.Commit(ctx))
			is.EqualSlice(t, []error{nil}, s.deliver())

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, with, read(t, d))
		})
	})

	t.Run("should report a commit as not committed, and go on serving, when the caller gives up before a swap was sent", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("create", "commits/", hang, -1)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := tx.Commit(ctx)
			is.True(t, err != nil && !errors.Is(err, core.ErrOutcomeUnknown), fmt.Sprint(err))
			is.Error(t, context.DeadlineExceeded, err)
			is.Equal(t, without, read(t, d))
		})
	})

	t.Run("should report a commit as not committed, and go on serving, when every swap was refused before the caller gave up", func(t *testing.T) {
		// The store refuses each swap for a conflict, or because its bytes were damaged on their way.
		for _, refusal := range []what{conflict, mismatch} {
			bubble(t, func(t *testing.T, w *world) {
				s, d, tx := committing(t, w, core.Options{})
				s.on("swap", "manifest", refusal, -1)

				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				err := tx.Commit(ctx)
				is.True(t, err != nil && !errors.Is(err, core.ErrOutcomeUnknown), fmt.Sprint(refusal, ": ", err))
				s.clear()
				is.Equal(t, without, read(t, d), fmt.Sprint(refusal))
				is.Equal(t, 1, len(w.commits(t)), fmt.Sprint(refusal))
			})
		}
	})

	t.Run("should find a commit in the log on which another instance has committed after taking the lease", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, a, tx := committing(t, w, core.Options{})
			// The swap of the commit is applied, and the holder hears nothing more from the store,
			// until another instance has taken the lease and committed on top.
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			var commitErr error
			committed := make(chan struct{})
			go func() {
				commitErr = tx.Commit(t.Context())
				close(committed)
			}()
			b := w.serve(t, "b", core.Options{})
			is.Equal(t, with, read(t, b))
			transact(t, b, create("t3", 1, "k"))
			s.clear()

			<-committed
			is.NotError(t, commitErr)
			is.Error(t, core.ErrNoLease, readErr(t, a))
			is.NotError(t, b.Close(t.Context()))
			is.NotError(t, a.Wait(t.Context()))
			is.Equal(t, "t1(k*0): \nt2(k*0): \nt3(k*0): ", read(t, a))
		})
	})

	t.Run("should report an unknown outcome without waiting for the caller when the log in which it looks for the commit holds a commit object that can never be read", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, a, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			var commitErr error
			committed := make(chan struct{})
			go func() {
				commitErr = tx.Commit(t.Context())
				close(committed)
			}()
			// Another instance takes the lease, and the log then gets a malformed commit object on top,
			// under a lease of another holder, so that the first instance stays a spare.
			b := w.serve(t, "b", core.Options{})
			is.NotError(t, b.Close(t.Context()))
			l := newHandLog(t, w)
			l.head = w.manifest(t).Head
			l.raw(nil, []*s4qlpb.Change{{}}, nil)
			data, err := format.Manifest{Head: l.head, Lease: &format.Lease{Holder: "another", Duration: time.Hour, Renewed: time.Now(), Salt: 1}}.Marshal()
			is.NotError(t, err)
			l.publishBytes(data)
			start := time.Now()
			s.clear()

			select {
			case <-committed:
			case <-time.After(time.Hour):
				t.Fatal("the commit is still looking in the log after an hour")
			}
			is.True(t, time.Since(start) < 10*time.Second, fmt.Sprint(time.Since(start)))
			is.Error(t, core.ErrOutcomeUnknown, commitErr)
			is.Error(t, core.ErrNoLease, readErr(t, a))
		})
	})

	t.Run("should report a commit as not committed when another instance took the lease and the commit is not in the log", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, a, tx := committing(t, w, core.Options{})
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			var commitErr error
			committed := make(chan struct{})
			go func() {
				commitErr = tx.Commit(t.Context())
				close(committed)
			}()
			b := w.serve(t, "b", core.Options{})
			transact(t, b, create("t3", 1, "k"))
			s.clear()

			<-committed
			is.Error(t, core.ErrNoLease, commitErr)
			is.True(t, !errors.Is(commitErr, core.ErrOutcomeUnknown))
			// The transaction was committing when the lease was lost, so the lease task left it alone,
			// and the log decided.
			is.True(t, !errors.Is(commitErr, core.ErrTxLeaseLost), fmt.Sprint(commitErr))
			is.NotError(t, b.Close(t.Context()))
			is.NotError(t, a.Wait(t.Context()))
			is.Equal(t, "t1(k*0): \nt3(k*0): ", read(t, a))
		})
	})
}

// fileStore is a store that keeps its objects in files and never holds one in memory, so that the
// memory of a test with a large commit object is the memory of the core. It is calm, and it is for
// one instance.
type fileStore struct {
	dir string
	mu  sync.Mutex
}

func (s *fileStore) path(key string) string {
	return filepath.Join(s.dir, hex.EncodeToString([]byte(key)))
}

// version of the object in the file: the checksum of its content, read in pieces.
func (s *fileStore) version(path string) (store.Version, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return store.Version(hex.EncodeToString(h.Sum(nil))), nil
}

func (s *fileStore) Get(_ context.Context, key string) (io.ReadCloser, store.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, err := s.version(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", store.ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	f, err := os.Open(s.path(key))
	return f, version, err
}

// put the content in the file of key.
func (s *fileStore) put(key string, content io.Reader) (store.Version, error) {
	f, err := os.Create(s.path(key))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, content); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return s.version(s.path(key))
}

func (s *fileStore) Create(_ context.Context, key string, content io.ReadSeeker, _ *store.Checksum) (store.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(s.path(key)); err == nil {
		return "", store.ErrExists
	}
	return s.put(key, content)
}

func (s *fileStore) Swap(_ context.Context, key string, from store.Version, content io.ReadSeeker, _ *store.Checksum) (store.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version, err := s.version(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", store.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if version != from {
		return "", store.ErrChanged
	}
	return s.put(key, content)
}

// manifest of the database "db" in the store.
func (s *fileStore) manifest(t *testing.T) format.Manifest {
	t.Helper()
	data, err := os.ReadFile(s.path("db/manifest.json"))
	is.NotError(t, err)
	m, err := format.ParseManifest(data)
	is.NotError(t, err)
	return m
}

// memoryWatch samples the heap of the Go runtime while a test runs, and reads the high-water mark of
// the memory that SQLite allocates, and reports how far each grew.
type memoryWatch struct {
	base   uint64
	peak   atomic.Uint64
	sqlite int64
	stop   chan struct{}
	done   chan struct{}
}

// watchMemory from now on. SQLite's mark counts every connection in the process, so a test that
// watches it must not run beside another test that uses SQLite.
func watchMemory() *memoryWatch {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := &memoryWatch{base: m.HeapAlloc, stop: make(chan struct{}), done: make(chan struct{})}
	w.sqlite, _ = sqlite.MemoryUsed(true)
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > w.peak.Load() {
					w.peak.Store(m.HeapAlloc)
				}
			}
		}
	}()
	return w
}

// grown is how many bytes the heap and SQLite held at most above what they held at the start.
func (w *memoryWatch) grown() (heap, sqliteHeap int64) {
	close(w.stop)
	<-w.done
	_, highwater := sqlite.MemoryUsed(false)
	return int64(w.peak.Load()) - int64(w.base), highwater - w.sqlite
}

func TestTx_Commit_large(t *testing.T) {
	// The number of rows in each of two tables is 300,000 unless the environment variable
	// S4QL_CORE_ROWS sets another, such as some millions. The heap may grow by a fixed number of
	// bytes, whatever the number of rows. While the transaction is written, the writer of a commit
	// object holds one batch of rows, of a mebibyte, and the collector lags behind. While it is
	// replayed, the reader holds one batch as the messages that it decodes to, which are some twenty
	// times its bytes, so only a run with millions of rows tells that from the whole commit object.
	//
	// Under the race detector the test is some twenty times slower and the heap is another, so it
	// runs with a tenth of the rows there, and checks only that the transaction commits and replays.
	// The same holds under the address sanitizer, where the test would take a minute alone.
	//
	// The memory that SQLite itself allocates, for its page caches and for the temporary files that
	// it keeps in memory, may grow by a fixed number of bytes too. The cache of a connection is the
	// default of 2000 KiB, and temporary files are on disk, so it grows by some 5 MB whatever the
	// number of rows. A cache that holds every page, or temporary files in memory, take as much as
	// the database, some 20 MB with 300,000 rows.
	instrumented := raceEnabled || asanEnabled
	rows := 300_000
	if instrumented {
		rows = 30_000
	}
	const limit, replayLimit, sqliteLimit = 8 << 20, 48 << 20, 8 << 20
	if v := os.Getenv("S4QL_CORE_ROWS"); v != "" {
		var err error
		rows, err = strconv.Atoi(v)
		is.NotError(t, err)
	}

	t.Run("should never hold a large transaction in memory: not its rows, not its commit object, and not which rows it changed, and neither when it is replayed", func(t *testing.T) {
		// The collector runs often, so that the heap is close to what is in use.
		defer debug.SetGCPercent(debug.SetGCPercent(10))
		dir := cacheDir(t)
		s := &fileStore{dir: t.TempDir()}
		h := host.New()
		d, err := core.Open(t.Context(), h, s, filepath.Join(dir, "a.db"), "db", core.Options{})
		is.NotError(t, err)
		defer func() { _ = d.Close(context.WithoutCancel(t.Context())) }()
		is.NotError(t, d.Wait(t.Context()))
		// A table that the transaction creates, and one that is there before, so that the transaction
		// keeps track of which of its rows it changed.
		transact(t, d, createWith(nullableTable(tableOf("old", 2, "a", "b", "c", "d"), 3)))

		large := int64(1) << 40
		start := time.Now()
		memory := watchMemory()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.CreateTable(t.Context(), tableOf("new", 1, "k", "v", "w")))
		for i := range int64(rows) {
			is.NotError(t, tx.Upsert(t.Context(), "old", row(large+i, i%7, large-i, nil)))
			is.NotError(t, tx.Upsert(t.Context(), "new", row(large+i, large-i, i)))
			if i%3 == 0 {
				is.NotError(t, tx.Annotate(t.Context(), "insert into new (k, v, w) values (?, ?, ?)", row(large+i, large-i, i)))
			}
		}
		is.NotError(t, tx.Commit(t.Context()))
		grown, sqliteGrown := memory.grown()

		info, err := os.Stat(s.path(commitKey("db", s.manifest(t).Head)))
		is.NotError(t, err)
		t.Logf("a transaction of %v rows gave a commit object of %v bytes in %v, the heap grew by at most %v bytes, and the memory of SQLite by %v", 2*rows, info.Size(), time.Since(start).Round(time.Millisecond), grown, sqliteGrown)
		is.True(t, sqliteGrown < sqliteLimit, fmt.Sprintf("the memory of SQLite grew by %v bytes for a commit object of %v bytes", sqliteGrown, info.Size()))
		if !instrumented {
			is.True(t, info.Size() > 2*limit, "the commit object is too small to tell anything")
			is.True(t, grown < limit, fmt.Sprintf("the heap grew by %v bytes for a commit object of %v bytes", grown, info.Size()))
		}
		is.NotError(t, d.Close(t.Context()))

		// An empty cache replays the commit object, and holds no more of it in memory.
		start = time.Now()
		memory = watchMemory()
		replayed, err := core.Open(t.Context(), h, s, filepath.Join(dir, "b.db"), "db", core.Options{})
		is.NotError(t, err)
		defer func() { _ = replayed.Close(context.WithoutCancel(t.Context())) }()
		is.NotError(t, replayed.Wait(t.Context()))
		grown, sqliteGrown = memory.grown()
		t.Logf("the replay took %v, the heap grew by at most %v bytes, and the memory of SQLite by %v", time.Since(start).Round(time.Millisecond), grown, sqliteGrown)
		is.True(t, sqliteGrown < sqliteLimit, fmt.Sprintf("the memory of SQLite grew by %v bytes for a commit object of %v bytes", sqliteGrown, info.Size()))
		if !instrumented {
			is.True(t, grown < replayLimit, fmt.Sprintf("the heap grew by %v bytes for a commit object of %v bytes", grown, info.Size()))
		}

		r, err := startRead(t, replayed)
		is.NotError(t, err)
		for _, table := range []string{"old", "new"} {
			var n int64
			for values, err := range r.Scan(t.Context(), table) {
				is.NotError(t, err)
				is.Equal(t, large+n, values[0].GetInteger())
				n++
			}
			is.Equal(t, int64(rows), n)
		}
		is.NotError(t, r.Close(t.Context()))
	})
}

func TestTx_Commit_settling(t *testing.T) {
	const without, with = "t1(k*0): ", "t1(k*0): \nt2(k*0): "

	t.Run("should count a commit as not committed when the renewal that rules it out is applied without an answer", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", stick, 1)
			s.on("swap", "", drop, -1)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			is.Error(t, core.ErrOutcomeUnknown, tx.Commit(ctx))
			head := w.manifest(t).Head

			// The renewal without the commit is applied, and its answer is lost: two swaps are in
			// flight now, and the manifest is the one of the second.
			s.clear()
			s.on("swap", "manifest", lose, 1)
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, without, read(t, d))
			is.Equal(t, head, w.manifest(t).Head)
			is.True(t, s.deliver()[0] != nil, "the stuck swap was applied")
			transact(t, d, create("t3", 1, "k"))
			is.Equal(t, "t1(k*0): \nt3(k*0): ", read(t, d))
			is.Equal(t, 2, len(w.commits(t)))
		})
	})

	t.Run("should settle a commit of unknown outcome as soon as the caller has its answer, and not at the next renewal", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			// The swap is applied, its answer is lost, and the attempt after it is lost too.
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, 1)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			is.Error(t, core.ErrOutcomeUnknown, tx.Commit(ctx))

			start := time.Now()
			is.NotError(t, d.Wait(t.Context()))
			is.True(t, time.Since(start) < time.Second, fmt.Sprint(time.Since(start)))
			is.Equal(t, with, read(t, d))
		})
	})

	t.Run("should refuse a commit of nothing when the lease has run out by the clocks", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			upsert("t1", 2)(t, tx)
			remove("t1", true, 2)(t, tx)
			got, err := tx.Get(t.Context(), "t1", row(1))
			is.NotError(t, err)
			is.Equal(t, "1", text(got))
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			time.Sleep(31 * time.Second)

			// The transaction read the cache, and nothing says any more that the cache is current.
			err = tx.Commit(t.Context())
			is.Error(t, core.ErrNoLease, err)
			is.Equal(t, 1, len(w.commits(t)))
		})
	})

	t.Run("should not commit a transaction after a call of it failed in the cache", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 3)(t, tx)
		// SQLite refuses to drop a table that a statement of the same connection is reading.
		var dropErr error
		for _, err := range tx.Scan(t.Context(), "t1") {
			is.NotError(t, err)
			dropErr = tx.DropTable(t.Context(), "t1")
			break
		}
		is.True(t, dropErr != nil, "the table was dropped under its scan")
		is.True(t, !errors.Is(dropErr, core.ErrStopped))

		is.True(t, tx.Upsert(t.Context(), "t1", row(4)) != nil, "the transaction goes on after a call of it failed")
		is.True(t, tx.Commit(t.Context()) != nil, "the transaction was committed after a call of it failed")
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))
		is.Equal(t, 1, len(w.commits(t)))
		// The instance goes on.
		transact(t, d, upsert("t1", 5))
		is.Equal(t, "t1(k*0): 1 | 2 | 5", read(t, d))
	})
}

func TestTx_Commit_stopped(t *testing.T) {
	const without, with = "t1(k*0): ", "t1(k*0): \nt2(k*0): "

	t.Run("should go on finding out when the instance is closed while the outcome is unknown, and report the commit", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			// The swap is applied, its answer is lost, and the store is out of reach for a while.
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			committed := make(chan error, 1)
			go func() { committed <- tx.Commit(t.Context()) }()
			time.Sleep(5 * time.Second)
			// The close waits for the commit, and gives up. The commit still uses the write connection.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			is.Error(t, context.DeadlineExceeded, d.Close(ctx))
			cancel()
			is.Equal(t, 0, len(committed))
			open, err := leftOpen(w.dir)
			is.NotError(t, err)
			is.EqualSlice(t, []string{"a.db"}, open)

			// The commit closes the connection when it ends.
			s.clear()
			is.NotError(t, <-committed)
			cacheClosed(t, w.dir)
			is.Equal(t, 2, len(w.commits(t)))
			is.NotError(t, d.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
			again := w.serve(t, "a", core.Options{})
			is.Equal(t, with, read(t, again))
		})
	})

	t.Run("should report an unknown outcome, and not a stop, when the caller gives up after the instance was closed", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", lose, 1)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)

			committed := make(chan error, 1)
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			go func() { committed <- tx.Commit(ctx) }()
			time.Sleep(5 * time.Second)
			closed := make(chan error, 1)
			go func() { closed <- d.Close(t.Context()) }()

			err := <-committed
			is.Error(t, core.ErrOutcomeUnknown, err)
			is.True(t, !errors.Is(err, core.ErrStopped) && !errors.Is(err, core.ErrClosed), fmt.Sprint(err))
			is.Equal(t, time.Minute, time.Since(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
			is.True(t, <-closed != nil, "the close reports that it released a lease that it could not release")
			is.Equal(t, 2, len(w.commits(t)))
		})
	})

	t.Run("should report a commit as not committed when the instance stops before a swap was sent", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("create", "commits/", hang, -1)

			committed := make(chan error, 1)
			go func() { committed <- tx.Commit(t.Context()) }()
			time.Sleep(time.Second)
			closed := make(chan error, 1)
			go func() { closed <- d.Close(t.Context()) }()

			err := <-committed
			is.Error(t, core.ErrStopped, err)
			is.True(t, !errors.Is(err, core.ErrOutcomeUnknown), fmt.Sprint(err))
			is.NotError(t, <-closed)
			is.Equal(t, 1, len(w.commits(t)))
			again := w.serve(t, "a", core.Options{})
			is.Equal(t, without, read(t, again))
		})
	})
}

// sleepCounter opens the cache of an instance with the options through a VFS of faultvfs that fails
// nothing, and counts the sleeps of SQLite's busy handler. It is closed when the test ends, after
// the instance, whose cleanup is registered later.
func sleepCounter(t *testing.T, o core.Options) (*faultvfs.VFS, core.Options) {
	t.Helper()
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, vfs.Close()) })
	o.VFS = vfs.Name()
	return vfs, o
}

func TestDB_Begin(t *testing.T) {
	t.Run("should wait for a lock on the cache that another connection holds for a while, as long as the busy timeout allows", func(t *testing.T) {
		w := newWorld(t)
		vfs, o := sleepCounter(t, core.Options{BusyTimeout: time.Minute})
		d := w.serve(t, "a", o)
		transact(t, d, create("t1", 1, "k"))

		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))
		sleeps, _ := vfs.Slept()
		is.Equal(t, 0, sleeps)

		committed := make(chan error, 1)
		go func() {
			committed <- func() error {
				tx, err := d.Begin(t.Context())
				if err != nil {
					return err
				}
				if err := tx.Upsert(t.Context(), "t1", row(1)); err != nil {
					return err
				}
				return tx.Commit(t.Context())
			}()
		}()
		// The lock is released only after SQLite's busy handler has slept, which it does only when it
		// found the lock held, so the transaction waits for the lock on every machine. A transaction
		// that ends while the lock is held ends the test.
		for sleeps == 0 {
			select {
			case err := <-committed:
				t.Fatalf("the transaction ended without waiting for the lock: %v", err)
			case <-time.After(time.Millisecond):
			}
			sleeps, _ = vfs.Slept()
		}
		is.NotError(t, other.Exec("rollback"))
		is.NotError(t, <-committed)
		is.Equal(t, "t1(k*0): 1", read(t, d))
	})

	t.Run("should wait for a lock on the cache for a second by default", func(t *testing.T) {
		w := newWorld(t)
		vfs, o := sleepCounter(t, core.Options{})
		d := w.serve(t, "a", o)
		transact(t, d, create("t1", 1, "k"))

		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))
		_, slept := vfs.Slept()
		is.Equal(t, time.Duration(0), slept)

		// SQLite's busy handler asks to sleep for the busy timeout in all, and sleeps at least as long
		// as it asks. Only a bound from below on the time that passes holds on a slow machine.
		start := time.Now()
		err = beginErr(t, d)
		elapsed := time.Since(start)
		is.Error(t, core.ErrBusy, err)
		_, slept = vfs.Slept()
		is.Equal(t, time.Second, slept)
		is.True(t, elapsed >= time.Second, fmt.Sprint(elapsed))
		is.NotError(t, other.Exec("rollback"))
	})

	t.Run("should return and log busy, and go on, when another connection holds the cache's write lock past the busy timeout", func(t *testing.T) {
		w := newWorld(t)
		events := &recorder{}
		d := w.serve(t, "a", core.Options{BusyTimeout: 50 * time.Millisecond, Logger: events.logger()})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		// Another process has the cache open and holds its write lock.
		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))

		_, err = d.Begin(t.Context())
		is.Error(t, core.ErrBusy, err)
		is.True(t, !errors.Is(err, core.ErrLocal) && !errors.Is(err, core.ErrStopped))
		is.Equal(t, 1, len(events.named(core.EventBusy)))
		is.Equal(t, "busy", at(events.named(core.EventBusy)[0], core.AttrOutcome))
		is.Equal(t, "begin", at(events.named(core.EventBusy)[0], core.AttrOperation))
		is.Equal(t, slog.LevelWarn, levelOf(events.named(core.EventBusy)[0]))

		// It is no local I/O failure: the instance goes on, and reads do not wait for the lock.
		is.NotError(t, d.Err())
		is.Equal(t, "t1(k*0): 1", read(t, d))
		is.NotError(t, other.Exec("rollback"))
		transact(t, d, upsert("t1", 2))
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))
	})
}

func TestTx_CreateTable(t *testing.T) {
	t.Run("should keep tables and columns with hostile names apart, in the cache and in a replay of the log", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})

		// Every name is the name of a table, and every table has all the names as its columns, with
		// the name of the table as the primary key, so that mixing two of them up would show.
		var want []string
		for i, name := range hostile {
			columns := append([]string{name}, hostile[:i]...)
			columns = append(columns, hostile[i+1:]...)
			ct := tableOf(name, 1, columns...)
			values := make([]any, len(columns))
			for j := range values {
				values[j] = 1000*i + j
			}
			// A table at a time, so that a failure names it.
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.CreateTable(t.Context(), ct), name)
			is.NotError(t, tx.Upsert(t.Context(), name, row(values...)), name)
			got, err := tx.Get(t.Context(), name, row(1000*i))
			is.NotError(t, err, name)
			is.Equal(t, text(row(values...)), text(got), name)
			is.NotError(t, tx.Commit(t.Context()), name)
			want = append(want, fmt.Sprint(describe(ct), ": ", text(row(values...))))
		}
		got := strings.Split(read(t, d), "\n")
		// Some names hold line breaks, so the comparison is of the whole text.
		is.Equal(t, len(strings.Split(strings.Join(want, "\n"), "\n")), len(got))
		for _, table := range want {
			is.True(t, strings.Contains(read(t, d), table), table)
		}
		is.NotError(t, d.Close(t.Context()))

		// The cache as a person sees it with sqlite3: the tables have the names of the database.
		_, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, len(hostile), len(state))

		replayed := w.serve(t, "b", core.Options{})
		for _, table := range want {
			is.True(t, strings.Contains(read(t, replayed), table), table)
		}

		// And they can be changed and dropped by their names.
		for i, name := range hostile {
			tx, err := replayed.Begin(t.Context())
			is.NotError(t, err)
			found, err := tx.Delete(t.Context(), name, row(1000*i))
			is.NotError(t, err, name)
			is.True(t, found, name)
			is.NotError(t, tx.DropTable(t.Context(), name), name)
			is.NotError(t, tx.Commit(t.Context()), name)
		}
		is.Equal(t, "", read(t, replayed))
	})

	t.Run("should refuse a table whose name starts with s4ql_ or sqlite_, in any case of the letters, and allow names that only look like it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)

		for _, name := range []string{"s4ql_", "s4ql_cache", "s4ql_touched_1", "S4QL_x", "s4Ql_x", "S4qL_", "sqlite_", "sqlite_schema", "sqlite_master", "SQLITE_x", "SqLiTe_stat1", "sqlite_autoindex_t1_1"} {
			err := tx.CreateTable(t.Context(), tableOf(name, 1, "k"))
			is.Error(t, core.ErrReservedName, err, name)
			is.True(t, !errors.Is(err, core.ErrInvalid), name)
		}
		for _, name := range []string{"s4ql", "s4qlx", "_s4ql_", " s4ql_", "sqlite", "sqlitex", "xsqlite_", "s4ql-", "ſ4ql_x"} {
			is.NotError(t, tx.CreateTable(t.Context(), tableOf(name, 1, "k")), name)
		}
		// A column may have such a name.
		is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "s4ql_cache", "sqlite_schema")))
		is.NotError(t, tx.Commit(t.Context()))
	})
}

func TestTx_Commit_checksum(t *testing.T) {
	t.Run("should upload again when the store says once that the content does not have its checksum, and stop when it says so twice", func(t *testing.T) {
		for _, test := range []struct {
			times int
			stops bool
		}{{times: 1}, {times: 2, stops: true}} {
			bubble(t, func(t *testing.T, w *world) {
				s, d, tx := committing(t, w, core.Options{})
				s.on("create", "commits/", mismatch, test.times)
				err := tx.Commit(t.Context())
				if !test.stops {
					is.NotError(t, err)
					is.Equal(t, 2, len(w.commits(t)))
					return
				}
				// Twice, it is the spool file that does not hold what was written to it.
				is.Error(t, core.ErrStopped, err)
				is.Error(t, core.ErrLocal, err)
				is.True(t, isDone(d))
				is.Equal(t, 1, len(w.commits(t)))
			})
		}
	})
}

func TestTx_Commit_tooLarge(t *testing.T) {
	t.Run("should fail at once with ErrTooLarge for a commit object that the store can never take, commit nothing, and commit the next transaction", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := sim.NewFree(sim.Buggify{})
			// The limit is far above a manifest, and below the commit object of a thousand rows.
			w := &world{host: h, store: simstore.New(h, simstore.Options{MaxSize: 2 << 10}), dir: cacheDir(t)}
			events := &recorder{}
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{Logger: events.logger()})
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, create("t1", 1, "k", "v"))
			before, head := read(t, d), w.manifest(t).Head

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			for i := range 1000 {
				upsert("t1", i, i)(t, tx)
			}
			// A core that sent the upload again would give up only when this context ends.
			ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
			defer cancel()
			uploads := s.count("create", "commits/")
			err = tx.Commit(ctx)
			is.Error(t, core.ErrTooLarge, err)
			is.True(t, ctx.Err() == nil, "the commit waited until its context ended")
			is.Equal(t, 1, s.count("create", "commits/")-uploads, "the upload was sent again")
			for _, other := range []error{core.ErrOutcomeUnknown, core.ErrNoLease, core.ErrStopped, context.DeadlineExceeded} {
				is.True(t, !errors.Is(err, other), fmt.Sprint(err))
			}
			is.Error(t, core.ErrTxDone, tx.Rollback(t.Context()))

			failed := events.with(core.EventCommit, core.AttrOutcome, "not_committed")
			is.Equal(t, 1, len(failed))
			is.Equal(t, slog.LevelWarn, levelOf(failed[0]))
			is.Equal(t, "1", at(failed[0], "attempts"))

			// Nothing changed, and the instance serves on.
			is.Equal(t, head, w.manifest(t).Head)
			is.Equal(t, before, read(t, d))
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, upsert("t1", 1, 2))
			is.Equal(t, "[[create t1(k*0,v)] [upsert t1 [0 1] 1,2]]", fmt.Sprint(w.commits(t)))
		})
	})
}

func TestTx_CreateTable_names(t *testing.T) {
	t.Run("should keep apart names that differ in the case of letters outside ASCII, as SQLite does", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("été", 1, "k"), create("ÉTÉ", 1, "k"), upsert("été", 1), upsert("ÉTÉ", 2))
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		// The ASCII letter folds, and the others do not.
		got, err := tx.Get(t.Context(), "éTé", row(1))
		is.NotError(t, err)
		is.Equal(t, "1", text(got))
		_, err = tx.Get(t.Context(), "Été", row(1))
		is.Error(t, core.ErrNoTable, err)
		is.NotError(t, tx.Rollback(t.Context()))
	})

	t.Run("should refuse a primary key of more columns than a row can be found by, and take the widest that it allows", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.Error(t, core.ErrInvalid, tx.CreateTable(t.Context(), tableOf("wide", 251, manyColumns(251)...)))

		is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 250, manyColumns(251)...)))
		values := make([]any, 251)
		for i := range values {
			values[i] = i
		}
		is.NotError(t, tx.Upsert(t.Context(), "t1", row(values...)))
		is.NotError(t, tx.Commit(t.Context()))

		// The row is found by its key in a table that was there before the transaction, which is
		// where the transaction keeps track of the keys that it changes.
		tx, err = d.Begin(t.Context())
		is.NotError(t, err)
		got, err := tx.Get(t.Context(), "t1", row(values[:250]...))
		is.NotError(t, err)
		is.Equal(t, text(row(values...)), text(got))
		values[250] = 7
		is.NotError(t, tx.Upsert(t.Context(), "t1", row(values...)))
		found, err := tx.Delete(t.Context(), "t1", row(values[:250]...))
		is.NotError(t, err)
		is.True(t, found)
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, "", strings.TrimPrefix(read(t, d), describe(tableOf("t1", 250, manyColumns(251)...))+": "))
	})
}

func TestTx_Savepoint(t *testing.T) {
	// own is the state that the transaction tx sees.
	own := func(t *testing.T, tx *core.Tx) string {
		t.Helper()
		state, err := dump(t.Context(), tx)
		is.NotError(t, err)
		return show(state)
	}

	t.Run("should undo everything since the savepoint, in the cache and in the commit object, and keep what came before", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10), upsert("t1", 2, 20), create("t2", 1, "k"))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t1", 3, 30)(t, tx)
		note("insert 3")(t, tx)
		is.NotError(t, tx.Savepoint(t.Context()))
		upsert("t1", 4, 40)(t, tx)
		upsert("t1", 1, 11)(t, tx)
		remove("t1", true, 2)(t, tx)
		create("t3", 1, "k")(t, tx)
		upsert("t3", 1)(t, tx)
		dropTable("t2")(t, tx)
		is.NotError(t, tx.RollbackTo(t.Context()))
		is.Equal(t, "t1(k*0,v): 1,10 | 2,20 | 3,30\nt2(k*0): ", own(t, tx))

		// The transaction goes on from the savepoint: the table that it created there is gone, and the
		// one that it dropped there is back.
		create("t3", 1, "k")(t, tx)
		upsert("t2", 5)(t, tx)
		is.NotError(t, tx.Commit(t.Context()))
		want := "t1(k*0,v): 1,10 | 2,20 | 3,30\nt2(k*0): 5\nt3(k*0): "
		is.Equal(t, want, read(t, d))
		is.EqualSlice(t, []string{"upsert t1 [0 1] 3,30", "create t3(k*0)", "upsert t2 [0] 5", "note insert 3 []"}, w.commits(t)[1])

		// A replay of the log into an empty cache gives the same state.
		is.NotError(t, d.Close(t.Context()))
		is.Equal(t, want, read(t, w.serve(t, "b", core.Options{})))
	})

	t.Run("should keep everything since the savepoint at a release", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Savepoint(t.Context()))
		upsert("t1", 2)(t, tx)
		remove("t1", true, 1)(t, tx)
		note("insert 2")(t, tx)
		is.NotError(t, tx.Release(t.Context()))
		is.NotError(t, tx.Savepoint(t.Context()))
		create("t2", 1, "k")(t, tx)
		is.NotError(t, tx.Release(t.Context()))
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, "t1(k*0): 2\nt2(k*0): ", read(t, d))
		is.EqualSlice(t, []string{"upsert t1 [0] 2", "delete t1 1", "create t2(k*0)", "note insert 2 []"}, w.commits(t)[1])
	})

	t.Run("should keep the record of a row that was changed before the savepoint, and make the record anew for a table changed only after it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10), create("t2", 1, "k", "v"), upsert("t2", 1, 10))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		// t1 has its record of changed rows before the savepoint, and t2 gets one only after it.
		upsert("t1", 2, 20)(t, tx)
		is.NotError(t, tx.Savepoint(t.Context()))
		remove("t1", true, 1)(t, tx)
		remove("t2", true, 1)(t, tx)
		is.NotError(t, tx.RollbackTo(t.Context()))
		upsert("t2", 2, 20)(t, tx)
		// A drop removes the record of the table, and the rollback to the savepoint brings it back.
		is.NotError(t, tx.Savepoint(t.Context()))
		dropTable("t1")(t, tx)
		is.NotError(t, tx.RollbackTo(t.Context()))
		upsert("t1", 3, 30)(t, tx)
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, "t1(k*0,v): 1,10 | 2,20 | 3,30\nt2(k*0,v): 1,10 | 2,20", read(t, d))
		is.EqualSlice(t, []string{"upsert t1 [0 1] 2,20", "upsert t1 [0 1] 3,30", "upsert t2 [0 1] 2,20"}, w.commits(t)[1])
	})

	t.Run("should refuse a second savepoint, a release and a rollback to without one, and a commit while one is open", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.Error(t, core.ErrInvalid, tx.Release(t.Context()))
		is.Error(t, core.ErrInvalid, tx.RollbackTo(t.Context()))
		is.NotError(t, tx.Savepoint(t.Context()))
		is.Error(t, core.ErrInvalid, tx.Savepoint(t.Context()))
		upsert("t1", 1)(t, tx)
		is.Error(t, core.ErrInvalid, tx.Commit(t.Context()))
		is.Equal(t, 1, len(w.commits(t)))
		is.Equal(t, "t1(k*0): ", read(t, d))
		is.Error(t, core.ErrTxDone, tx.Savepoint(t.Context()))
		is.Error(t, core.ErrTxDone, tx.Release(t.Context()))
		is.Error(t, core.ErrTxDone, tx.RollbackTo(t.Context()))
		is.Error(t, core.ErrTxDone, tx.Serving(t.Context()))
		transact(t, d, upsert("t1", 2))
	})

	t.Run("should fail the transaction at a rollback to a savepoint before an annotation", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Savepoint(t.Context()))
		upsert("t1", 1)(t, tx)
		note("insert 1")(t, tx)
		is.Error(t, core.ErrInvalid, tx.RollbackTo(t.Context()))
		is.Error(t, core.ErrInvalid, tx.Upsert(t.Context(), "t1", row(2)))
		is.Error(t, core.ErrInvalid, tx.Serving(t.Context()))
		is.Error(t, core.ErrInvalid, tx.Commit(t.Context()))
		is.Equal(t, 1, len(w.commits(t)))
		is.Equal(t, "t1(k*0): ", read(t, d))
	})

	t.Run("should take, release, and roll back to a savepoint while the lease is not valid by the clocks, which Serving refuses", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			s := newScriptStore(w.client())
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, create("t1", 1, "k"))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.Serving(t.Context()))
			is.NotError(t, tx.Savepoint(t.Context()))
			upsert("t1", 1)(t, tx)
			s.on("swap", "", drop, -1)
			s.on("get", "", drop, -1)
			time.Sleep(31 * time.Second)

			is.Error(t, core.ErrNoLease, tx.Serving(t.Context()))
			is.NotError(t, tx.RollbackTo(t.Context()))
			is.NotError(t, tx.Savepoint(t.Context()))
			is.NotError(t, tx.Release(t.Context()))
			is.NotError(t, tx.Rollback(t.Context()))
		})
	})

	t.Run("should refuse a savepoint, and give the stop at a release and a rollback to, on an instance that has stopped", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Savepoint(t.Context()))
		is.NotError(t, d.Close(t.Context()))
		is.Error(t, core.ErrStopped, tx.Serving(t.Context()))
		is.Error(t, core.ErrStopped, tx.RollbackTo(t.Context()))
		is.Error(t, core.ErrStopped, tx.Release(t.Context()))
		is.Error(t, core.ErrStopped, tx.Savepoint(t.Context()))
		is.NotError(t, tx.Rollback(t.Context()))
	})
}

// savepointCounts are what the random transactions of [TestTx_Savepoint_random] reached.
type savepointCounts struct {
	transactions, commits, rollbacks, savepoints, releases, rollbacksTo, openAtEnd int
	// dropThenCreate counts the creates of a name after a drop of it in the same transaction, with
	// a rollback to a savepoint between the two.
	dropThenCreate int
	// firstTouchUndone counts the rollbacks to a savepoint in which the transaction touched a key
	// of a table that existed when it began for the first time.
	firstTouchUndone int
	// dropInSavepoint counts the drops, inside a savepoint, of a table that existed when the
	// savepoint was taken.
	dropInSavepoint int
	// emptyRollbackTo counts the rollbacks to a savepoint in which nothing changed.
	emptyRollbackTo int
	// refused counts the upserts of a row that the table cannot hold.
	refused int
	// negativeZero counts the upserts and deletes given -0.0 as the key, and textKeys and blobKeys
	// those of a text key and of a blob key in a table that existed when the transaction began.
	negativeZero, textKeys, blobKeys int
	// nulls counts the upserts of a row with NULL in its column that takes NULL, and refusedNulls
	// the upserts of NULL into a column that does not take it, among those that refused counts.
	nulls, refusedNulls int
}

// savepointLimit is the length limit of the cache in [TestTx_Savepoint_random].
const savepointLimit = 1000

func (c *savepointCounts) add(o savepointCounts) {
	c.transactions += o.transactions
	c.commits += o.commits
	c.rollbacks += o.rollbacks
	c.savepoints += o.savepoints
	c.releases += o.releases
	c.rollbacksTo += o.rollbacksTo
	c.openAtEnd += o.openAtEnd
	c.dropThenCreate += o.dropThenCreate
	c.firstTouchUndone += o.firstTouchUndone
	c.dropInSavepoint += o.dropInSavepoint
	c.emptyRollbackTo += o.emptyRollbackTo
	c.refused += o.refused
	c.negativeZero += o.negativeZero
	c.textKeys += o.textKeys
	c.blobKeys += o.blobKeys
	c.nulls += o.nulls
	c.refusedNulls += o.refusedNulls
}

// TestTx_Savepoint_random runs random write transactions of random steps on one instance: creates
// and drops of a few tables in two spellings each, with a key and a value of random types, upserts
// and deletes of a few keys of the type of the table's key, drawn from [edges], and of NULL in a
// value that takes it, upserts of a value of another kind, of one that is too big, or of NULL where
// the table takes none, annotations, and savepoints, releases, and rollbacks to a
// savepoint in random places, also where they are refused. A savepoint that is open at the end is
// rolled back to. Then the transaction commits or rolls back. After each step, what the
// transaction reads must be what the model of [newTx] gives for the same steps; after each end,
// the commit object must be the model's net effect, and what the instance serves the model's
// state. At the end of each seed, a strict replay of the log in the model and a replay into an
// empty cache must give the same state, and the cache of the replay must hold the same values as
// the instance's, bit for bit. The flag -replay runs that one seed, and logs its steps.
func TestTx_Savepoint_random(t *testing.T) {
	first, n := uint64(0), uint64(400)
	if testing.Short() || raceEnabled || asanEnabled {
		n = 40
	}
	if *replay != "" {
		seed, err := strconv.ParseUint(*replay, 10, 64)
		is.NotError(t, err)
		first, n = seed, 1
	}
	var all savepointCounts
	for seed := first; seed < first+n; seed++ {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			defer func() {
				if t.Failed() {
					var mode string
					if raceEnabled {
						mode = "-race "
					}
					if asanEnabled {
						mode = "-asan "
					}
					t.Logf("replay it with: go test %v-run '^TestTx_Savepoint_random$' -v ./internal/core/ -replay %v", mode, seed)
				}
			}()
			all.add(randomSavepoints(t, seed, *replay != ""))
		})
	}
	t.Logf("%v seeds: %+v", n, all)
	if n < 400 {
		t.Logf("NOT CHECKED: what the transactions reached, because %v seeds are fewer than the 400 of the default run", n)
		return
	}
	// A quarter of what the 400 seeds reached on 2026-11-19, so that the test fails when a case
	// stops happening.
	for _, c := range []struct {
		name         string
		got, atLeast int
	}{
		{"commits", all.commits, 294},
		{"rollbacks", all.rollbacks, 104},
		{"rollbacks to a savepoint", all.rollbacksTo, 286},
		{"savepoints open at the end", all.openAtEnd, 138},
		{"creates after a drop of the name, with a rollback to a savepoint between", all.dropThenCreate, 7},
		{"first touches of a key undone by a rollback to a savepoint", all.firstTouchUndone, 42},
		{"drops in a savepoint of a table that existed before it", all.dropInSavepoint, 65},
		{"rollbacks to a savepoint in which nothing changed", all.emptyRollbackTo, 145},
		// A quarter of what the 400 seeds reached on 2026-11-19 with the types of slice 4.
		{"upserts of a row that the table cannot hold", all.refused, 68},
		{"upserts and deletes given -0.0 as the key", all.negativeZero, 18},
		{"upserts and deletes of a text key in a table that existed before", all.textKeys, 125},
		{"upserts and deletes of a blob key in a table that existed before", all.blobKeys, 91},
		// A quarter of what the 400 seeds reached on 2026-11-20 with the NULL of slice 6.
		{"upserts of a row with NULL", all.nulls, 50},
		{"upserts of NULL where the table takes none", all.refusedNulls, 14},
	} {
		if c.got < c.atLeast {
			t.Errorf("the transactions reached %q %v times, and must reach it %v times at least", c.name, c.got, c.atLeast)
		}
	}
}

// randomSavepoints runs the transactions of one seed of [TestTx_Savepoint_random], and returns what
// they reached.
func randomSavepoints(t *testing.T, seed uint64, verbose bool) savepointCounts {
	t.Helper()
	ctx := t.Context()
	r := rand.New(rand.NewPCG(seed, 0x5a7e9014))
	logf := func(format string, args ...any) {
		if verbose {
			t.Logf(format, args...)
		}
	}
	var c savepointCounts
	w := newWorld(t)
	options := core.Options{LengthLimit: savepointLimit}
	d := w.serve(t, "a", options)
	// Three tables, each in two spellings that SQLite takes for the same name, and three shapes, with
	// a key of any type, and a value of any type that takes NULL or does not.
	names := []string{"t1", "T1", "t2", "T2", "tX", "Tx"}
	committed := newState()
	// keys of each type for this seed: four of its edges, so that a transaction touches a key more
	// than once.
	keys := map[s4qlpb.Type][]*s4qlpb.Value{}
	for _, typ := range types {
		pool := slices.Clone(edges[typ])
		r.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })
		keys[typ] = pool[:4]
	}
	// keyOf the table with the folded name, as the transaction sees it, at index i of the keys of its
	// type: of the type of its key, or an integer if there is no such table.
	keyOf := func(m *mtx, folded string, i int) []*s4qlpb.Value {
		if tb := m.now.tables[folded]; tb != nil {
			return []*s4qlpb.Value{keys[tb.create.GetColumns()[0].GetType()][i]}
		}
		return row(1 + i)
	}

	for i := range 2 + r.IntN(5) {
		c.transactions++
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		m := newTx(committed)
		// saved is the model at the savepoint that is open, or nil.
		var saved *mtx
		// changedSince and touchedFirst say whether anything changed, and whether a key of a table
		// that existed when the transaction began was touched for the first time, since the savepoint.
		var changedSince, touchedFirst bool
		// dropped are the folded names that the transaction dropped, true once a rollback to a
		// savepoint came after the drop.
		dropped := map[string]bool{}
		var notes []string
		logf("transaction %v", i)

		// first is whether a change of the key of the table is the transaction's first touch of it, in
		// a table that existed when the transaction began.
		first := func(folded string, key []*s4qlpb.Value) bool {
			if m.now.tables[folded] == nil {
				return false
			}
			ch, ok := m.changed[folded]
			return !ok || (!ch.created && ch.touched[text(stored(key))] == nil)
		}
		rollbackTo := func() {
			is.NotError(t, tx.RollbackTo(ctx))
			c.rollbacksTo++
			if !changedSince {
				c.emptyRollbackTo++
			}
			if touchedFirst {
				c.firstTouchUndone++
			}
			for folded := range dropped {
				dropped[folded] = true
			}
			m, saved = saved, nil
		}
		// count the upsert or delete of the key in the table with the folded name, which is there.
		count := func(folded string, key []*s4qlpb.Value) {
			if v := key[0]; v.WhichKind() == s4qlpb.Value_Real_case && v.GetReal() == 0 && math.Signbit(v.GetReal()) {
				c.negativeZero++
			}
			if ch := m.changed[folded]; ch == nil || ch.created {
				return
			}
			switch key[0].WhichKind() {
			case s4qlpb.Value_Text_case:
				c.textKeys++
			case s4qlpb.Value_Blob_case:
				c.blobKeys++
			}
		}

		for range 1 + r.IntN(30) {
			name := names[r.IntN(len(names))]
			folded := foldName(name)
			key := keyOf(m, folded, r.IntN(4))
			switch k := r.IntN(100); {
			case k < 12:
				ct := typedTable(name, types[r.IntN(len(types))], types[r.IntN(len(types))])
				switch r.IntN(4) {
				case 0, 1:
					ct = typedTable(name, types[r.IntN(len(types))])
				case 2:
					ct = nullableTable(ct, 1)
				}
				logf("create %v", describe(ct))
				err := tx.CreateTable(ctx, ct)
				if m.create(ct) {
					is.NotError(t, err)
					changedSince = true
					if dropped[folded] {
						c.dropThenCreate++
					}
				} else {
					is.Error(t, core.ErrTableExists, err)
				}

			case k < 22:
				logf("drop %v", name)
				existed := saved != nil && saved.now.tables[folded] != nil
				err := tx.DropTable(ctx, name)
				if m.drop(name) {
					is.NotError(t, err)
					changedSince = true
					if _, ok := dropped[folded]; !ok {
						dropped[folded] = false
					}
					if existed {
						c.dropInSavepoint++
					}
				} else {
					is.Error(t, core.ErrNoTable, err)
				}

			case k < 49:
				values := slices.Clone(key)
				if tb := m.now.tables[folded]; tb != nil && len(tb.create.GetColumns()) == 2 {
					column := tb.create.GetColumns()[1]
					if pool := edges[column.GetType()]; !column.GetNullable() || r.IntN(3) > 0 {
						values = append(values, pool[r.IntN(len(pool))])
					} else {
						values = append(values, row(nil)[0])
					}
				}
				logf("upsert %v %v", name, text(values))
				firstTouch := first(folded, key)
				err := tx.Upsert(ctx, name, values)
				if m.upsert(name, values) {
					count(folded, key)
					if slices.ContainsFunc(values, isNull) {
						c.nulls++
					}
					is.NotError(t, err)
					changedSince = true
					touchedFirst = touchedFirst || (saved != nil && firstTouch)
				} else {
					is.Error(t, core.ErrNoTable, err)
				}

			case k < 52:
				// A row that the table cannot hold, which changes nothing.
				tb := m.now.tables[folded]
				if tb == nil {
					break
				}
				values := slices.Clone(key)
				if len(tb.create.GetColumns()) == 2 {
					values = append(values, edges[tb.create.GetColumns()[1].GetType()][0])
				}
				position := r.IntN(len(values))
				want := core.ErrInvalid
				column := tb.create.GetColumns()[position]
				switch typ := column.GetType(); {
				case r.IntN(4) == 0 && !column.GetNullable():
					// NULL in the key, or in a value that does not take it.
					values[position] = row(nil)[0]
					c.refusedNulls++
				case r.IntN(2) == 0 && typ == s4qlpb.Type_TYPE_TEXT:
					values[position], want = row(strings.Repeat("x", savepointLimit+1))[0], core.ErrTooBig
				case r.IntN(2) == 0 && typ == s4qlpb.Type_TYPE_BLOB:
					values[position], want = row(make([]byte, savepointLimit+1))[0], core.ErrTooBig
				default:
					// A value of the type two places on, which is always of another kind.
					next := types[(slices.Index(types, typ)+2)%len(types)]
					values[position] = edges[next][0]
				}
				logf("upsert %v of a value that it refuses at %v", name, position)
				c.refused++
				is.Error(t, want, tx.Upsert(ctx, name, values))

			case k < 70:
				logf("delete %v %v", name, text(key))
				firstTouch := first(folded, key)
				found, err := tx.Delete(ctx, name, key)
				table, want := m.remove(name, key)
				if table {
					count(folded, key)
					is.NotError(t, err)
					is.Equal(t, want, found)
					changedSince = changedSince || want
					touchedFirst = touchedFirst || (saved != nil && firstTouch)
				} else {
					is.Error(t, core.ErrNoTable, err)
				}

			case k < 80:
				logf("savepoint")
				err := tx.Savepoint(ctx)
				if saved != nil {
					is.Error(t, core.ErrInvalid, err)
					break
				}
				is.NotError(t, err)
				c.savepoints++
				saved, changedSince, touchedFirst = m.clone(), false, false

			case k < 88:
				logf("release")
				err := tx.Release(ctx)
				if saved == nil {
					is.Error(t, core.ErrInvalid, err)
					break
				}
				is.NotError(t, err)
				c.releases++
				saved = nil

			case k < 96:
				logf("rollback to")
				if saved == nil {
					is.Error(t, core.ErrInvalid, tx.RollbackTo(ctx))
					break
				}
				rollbackTo()

			default:
				// An annotation inside a savepoint fails the transaction at the rollback to it, which
				// the engine never makes, so the steps annotate only outside one.
				if saved != nil {
					break
				}
				sql := fmt.Sprint("statement ", len(notes))
				logf("annotate %q", sql)
				is.NotError(t, tx.Annotate(ctx, sql, nil))
				notes = append(notes, "note "+sql+" []")
			}
			own, err := dump(ctx, tx)
			is.NotError(t, err)
			is.Equal(t, m.now.text(), show(own), "what the transaction reads")
		}
		if saved != nil {
			logf("rollback to, at the end")
			c.openAtEnd++
			rollbackTo()
			own, err := dump(ctx, tx)
			is.NotError(t, err)
			is.Equal(t, m.now.text(), show(own), "what the transaction reads")
		}
		// Nothing of the transaction is seen outside before its end.
		is.Equal(t, committed.text(), read(t, d), "what the instance serves while a transaction is open")

		head := w.manifest(t).Head
		if r.IntN(4) == 0 {
			logf("rollback")
			c.rollbacks++
			is.NotError(t, tx.Rollback(ctx))
			is.Equal(t, head, w.manifest(t).Head)
		} else {
			logf("commit")
			c.commits++
			is.NotError(t, tx.Commit(ctx))
			effect := m.effect()
			if len(effect) == 0 {
				is.Equal(t, head, w.manifest(t).Head, "a transaction that changed nothing writes no commit object")
			} else {
				name := w.manifest(t).Head
				data, _, ok := w.store.Object(commitKey("db", name))
				is.True(t, ok, "the log names a commit object that the store does not have")
				parent, lines, err := changes(name, data)
				is.NotError(t, err)
				is.Equal(t, head, parent)
				is.Equal(t, strings.Join(append(effect, notes...), "\n"), strings.Join(lines, "\n"), "the commit object")
			}
			committed = m.now
		}
		is.Equal(t, committed.text(), read(t, d), "what the instance serves after the end of a transaction")
	}

	// A strict replay of the log in the model, and a replay into an empty cache, give the state of
	// the instance that committed, and the two caches hold the same values, bit for bit.
	mdl := newModel(w.store, "db")
	is.NotError(t, mdl.sync())
	is.Equal(t, committed.text(), mdl.states[len(mdl.states)-1].text(), "a strict replay of the log")
	is.NotError(t, d.Close(ctx))
	replayed := w.serve(t, "b", options)
	is.Equal(t, committed.text(), read(t, replayed), "a replay of the log into an empty cache")
	is.NotError(t, replayed.Close(ctx))
	_, before, err := cacheDump(filepath.Join(w.dir, "a.db"))
	is.NotError(t, err)
	_, after, err := cacheDump(filepath.Join(w.dir, "b.db"))
	is.NotError(t, err)
	is.Equal(t, committed.text(), show(before), "the cache of the instance that committed")
	is.Equal(t, show(before), show(after), "the cache that the log was replayed into")
	return c
}

func TestTx_types(t *testing.T) {
	t.Run("should create, upsert, get, scan, and delete rows of every type, with and without NULL, commit what the cache holds, and replay it into an empty cache with the same bytes", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ctx := t.Context()
		committed := newState()

		// A table for each type of its key, with a column of every type after the key that does not
		// take NULL, and then one of every type that does.
		var nullable []int
		for i := range types {
			nullable = append(nullable, 1+len(types)+i)
		}
		for _, typ := range types {
			ct := nullableTable(typedTable("t_"+typeName(typ), slices.Concat([]s4qlpb.Type{typ}, types, types)...), nullable...)
			keys := edges[typ]
			m := newTx(committed)
			tx, err := d.Begin(ctx)
			is.NotError(t, err)
			is.NotError(t, tx.CreateTable(ctx, ct))
			is.True(t, m.create(ct))
			got, err := tx.Table(ctx, ct.GetTable())
			is.NotError(t, err)
			is.Equal(t, describe(ct), describe(got))
			for i, k := range keys {
				values := []*s4qlpb.Value{k}
				for _, other := range types {
					values = append(values, edges[other][i%len(edges[other])])
				}
				for j, other := range types {
					if (i+j)%2 == 0 {
						values = append(values, row(nil)[0])
					} else {
						values = append(values, edges[other][(i+1)%len(edges[other])])
					}
				}
				is.NotError(t, tx.Upsert(ctx, ct.GetTable(), values), text(values))
				m.upsert(ct.GetTable(), values)
			}
			is.NotError(t, tx.Commit(ctx))
			committed = m.now
			is.Equal(t, committed.text(), read(t, d))

			// A second transaction gets every key, also as it was given, deletes every other one,
			// upserts a key that existed and then deletes it, and upserts again one that it deleted,
			// in a table that existed before it.
			m = newTx(committed)
			tx, err = d.Begin(ctx)
			is.NotError(t, err)
			for i, k := range keys {
				// -0.0 is the key 0.0, which may be deleted already.
				row, err := tx.Get(ctx, ct.GetTable(), []*s4qlpb.Value{k})
				is.NotError(t, err)
				is.Equal(t, text(m.now.tables[foldName(ct.GetTable())].rows[text(stored([]*s4qlpb.Value{k}))]), text(row), text([]*s4qlpb.Value{k}))
				if i%2 == 0 {
					_, want := m.remove(ct.GetTable(), []*s4qlpb.Value{k})
					found, err := tx.Delete(ctx, ct.GetTable(), []*s4qlpb.Value{k})
					is.NotError(t, err)
					is.Equal(t, want, found, text([]*s4qlpb.Value{k}))
				}
			}
			// The row of the key keys[1] existed, and is not deleted above: the commit object deletes
			// it, and has no upsert of it.
			existed := []*s4qlpb.Value{keys[1]}
			for _, other := range types {
				existed = append(existed, edges[other][1])
			}
			for range types {
				existed = append(existed, row(nil)[0])
			}
			is.NotError(t, tx.Upsert(ctx, ct.GetTable(), existed))
			m.upsert(ct.GetTable(), existed)
			_, want := m.remove(ct.GetTable(), existed[:1])
			is.True(t, want, "the row of "+text(existed[:1])+" is there")
			found, err := tx.Delete(ctx, ct.GetTable(), existed[:1])
			is.NotError(t, err)
			is.True(t, found, text(existed[:1]))
			again := []*s4qlpb.Value{keys[0]}
			for _, other := range types {
				again = append(again, edges[other][len(edges[other])-1])
			}
			for range types {
				again = append(again, row(nil)[0])
			}
			is.NotError(t, tx.Upsert(ctx, ct.GetTable(), again))
			m.upsert(ct.GetTable(), again)
			own, err := dump(ctx, tx)
			is.NotError(t, err)
			is.Equal(t, m.now.text(), show(own))
			head := w.manifest(t).Head
			is.NotError(t, tx.Commit(ctx))
			name := w.manifest(t).Head
			data, _, ok := w.store.Object(commitKey("db", name))
			is.True(t, ok)
			parent, lines, err := changes(name, data)
			is.NotError(t, err)
			is.Equal(t, head, parent)
			is.Equal(t, strings.Join(m.effect(), "\n"), strings.Join(lines, "\n"), "the commit object of "+typeName(typ))
			committed = m.now
		}
		is.Equal(t, committed.text(), read(t, d))
		is.NotError(t, d.Close(ctx))

		// The cache holds what the model holds, bit for bit, and so does a cache that the log was
		// replayed into, with the types as they were declared.
		_, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, committed.text(), show(state))
		replayed := w.serve(t, "b", core.Options{})
		is.Equal(t, committed.text(), read(t, replayed))
		is.NotError(t, replayed.Close(ctx))
		_, again, err := cacheDump(filepath.Join(w.dir, "b.db"))
		is.NotError(t, err)
		is.Equal(t, show(state), show(again))
		mdl := newModel(w.store, "db")
		is.NotError(t, mdl.sync())
		is.Equal(t, committed.text(), mdl.states[len(mdl.states)-1].text(), "a strict replay of the log")
	})

	t.Run("should take -0.0 as the key 0.0, and store, commit, and replay 0.0 for it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		negative := math.Copysign(0, -1)
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), typedTable("t", s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_REAL)))
			is.NotError(t, tx.Upsert(t.Context(), "t", row(negative, negative)))
			got, err := tx.Get(t.Context(), "t", row(0.0))
			is.NotError(t, err)
			is.Equal(t, "r0,r0", text(got))
			is.NotError(t, tx.Upsert(t.Context(), "t", row(0.0, 1.0)))
			is.NotError(t, tx.Annotate(t.Context(), "insert into t values (?, ?)", row(negative, negative)))
		})
		is.EqualSlice(t, []string{"create t(c0*0:real,c1:real)", "upsert t [0 1] r0,r1", "note insert into t values (?, ?) [r-0,r-0]"}, w.commits(t)[0])

		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.Upsert(t.Context(), "t", row(negative, negative)))
		})
		is.EqualSlice(t, []string{"upsert t [0 1] r0,r0"}, w.commits(t)[1])
		transact(t, d, remove("t", true, negative))
		is.EqualSlice(t, []string{"delete t r0"}, w.commits(t)[2])
		transact(t, d, upsert("t", negative, 2.0))
		is.Equal(t, "t(c0*0:real,c1:real): r0,r2", read(t, d))
		is.NotError(t, d.Close(t.Context()))

		_, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, "t(c0*0:real,c1:real): r0,r2", show(state))
		replayed := w.serve(t, "b", core.Options{})
		is.Equal(t, "t(c0*0:real,c1:real): r0,r2", read(t, replayed))
	})

	t.Run("should refuse a value of another type than its column, a real that is not a number or infinite, and text that is not UTF-8 or holds NUL, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ctx := t.Context()
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		// One value of each kind, with those that SQLite converts in a strict table: the text '1'
		// to an integer, the real 1.0 to an integer, and the integer 1 to a real.
		values := row(1, 1.0, 1.5, "1", "1.0", []byte("1"), []byte{})
		bad := map[s4qlpb.Type][]*s4qlpb.Value{
			s4qlpb.Type_TYPE_REAL: row(math.NaN(), math.Inf(1), math.Inf(-1)),
			s4qlpb.Type_TYPE_TEXT: row("\xff", "a\x00", "\x00", "\xc0\x80"),
		}
		for _, typ := range types {
			name := "t_" + typeName(typ)
			is.NotError(t, tx.CreateTable(ctx, typedTable(name, typ, typ)))
			good := edges[typ][0]
			refused := slices.Clone(bad[typ])
			for _, v := range values {
				// The edges of a type are of the kind that it takes.
				if v.WhichKind() != good.WhichKind() {
					refused = append(refused, v)
				}
			}
			for _, v := range refused {
				what := typeName(typ) + " " + text([]*s4qlpb.Value{v})
				// The error names the column and the table of the value.
				for position, row := range [][]*s4qlpb.Value{{v, good}, {good, v}} {
					err := tx.Upsert(ctx, name, row)
					is.Error(t, core.ErrInvalid, err, what)
					where := fmt.Sprintf("in the column %q of the table %q", fmt.Sprint("c", position), name)
					is.True(t, strings.Contains(err.Error(), where), what+": "+err.Error())
				}
				found, err := tx.Delete(ctx, name, []*s4qlpb.Value{v})
				is.Error(t, core.ErrInvalid, err, what)
				is.True(t, !found, what)
				_, err = tx.Get(ctx, name, []*s4qlpb.Value{v})
				is.Error(t, core.ErrInvalid, err, what)
			}
			is.NotError(t, tx.Upsert(ctx, name, []*s4qlpb.Value{good, good}))
		}
		is.NotError(t, tx.Commit(ctx))
		// Only the creates and the good rows.
		is.Equal(t, 2*len(types), len(w.commits(t)[0]))
	})

	t.Run("should refuse a row of another number of values than columns, and a key whose later value is not of its column", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ctx := t.Context()
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		// A table whose primary key is its two columns, a text and a real.
		ct := typedTable("two", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL)
		ct.SetPrimaryKey([]uint32{0, 1})
		is.NotError(t, tx.CreateTable(ctx, ct))

		err = tx.Upsert(ctx, "two", row("a"))
		is.Error(t, core.ErrInvalid, err)
		is.True(t, strings.Contains(err.Error(), `a row of 1 values for the table "two", which has 2 columns`), err.Error())
		err = tx.Upsert(ctx, "two", row("a", 1.5, 2.5))
		is.True(t, strings.Contains(err.Error(), `a row of 3 values for the table "two", which has 2 columns`), err.Error())

		found, err := tx.Delete(ctx, "nosuch", row(1))
		is.Error(t, core.ErrNoTable, err)
		is.True(t, !found)
		for _, key := range [][]*s4qlpb.Value{row("a", "b"), row("a", math.NaN())} {
			found, err = tx.Delete(ctx, "two", key)
			is.Error(t, core.ErrInvalid, err, text(key))
			is.True(t, !found, text(key))
			is.True(t, strings.Contains(err.Error(), `in the column "c1" of the table "two"`), err.Error())
			_, err = tx.Get(ctx, "two", key)
			is.Error(t, core.ErrInvalid, err, text(key))
		}
		is.NotError(t, tx.Rollback(ctx))
	})
}

func TestTx_readFails(t *testing.T) {
	// key of the row i, of 900 bytes, so that the rows take more pages than SQLite's page cache
	// holds and SQLite reads some of them from the file again.
	key := func(i int) string { return fmt.Sprintf("%0900d", i) }
	const rows = 4000
	setUp := func(t *testing.T) (*faultvfs.VFS, *world, *core.DB) {
		t.Helper()
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		w := newWorld(t)
		o := core.Options{}
		o.VFS = vfs.Name()
		d := w.serve(t, "a", o)
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), typedTable("t", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_INT)))
			for i := range rows {
				is.NotError(t, tx.Upsert(t.Context(), "t", row(key(i), i)))
			}
		})
		return vfs, w, d
	}
	reads := faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB | faultvfs.WAL}

	t.Run("should fail the transaction for an upsert into a table that existed, when SQLite cannot read whether the row is there", func(t *testing.T) {
		vfs, _, d := setUp(t)
		ctx := t.Context()
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		// Only the first read fails, which is that of the row before the upsert writes it.
		once := reads
		once.Times = 1
		vfs.Fail(once)
		for i := range rows {
			if err = tx.Upsert(ctx, "t", row(key(i), -i)); err != nil {
				break
			}
		}
		is.True(t, vfs.Failures() > 0, "no read failed")
		is.Error(t, core.ErrLocal, err)
		vfs.Fail(faultvfs.Fault{})
		_ = tx.Rollback(ctx)
		_ = d.Close(ctx)
		is.NotError(t, vfs.Close())
	})

	t.Run("should fail the commit, and write no commit object, when SQLite cannot read the rows that the transaction touched", func(t *testing.T) {
		vfs, w, d := setUp(t)
		ctx := t.Context()
		head := w.manifest(t).Head
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		for i := 0; i < rows; i += 2 {
			is.NotError(t, tx.Upsert(ctx, "t", row(key(i), -i)))
			_, err := tx.Delete(ctx, "t", row(key(i+1)))
			is.NotError(t, err)
		}
		// Only the first read fails, which is that of the rows that the transaction upserted, so that
		// every later step of the commit would work.
		once := reads
		once.Times = 1
		vfs.Fail(once)
		err = tx.Commit(ctx)
		is.True(t, vfs.Failures() > 0, "no read failed")
		is.True(t, err != nil, "the commit succeeded")
		is.Equal(t, head, w.manifest(t).Head, "the log has a new commit object")
		vfs.Fail(faultvfs.Fault{})
		_ = d.Close(ctx)
		is.NotError(t, vfs.Close())
	})
}

func TestTx_null(t *testing.T) {
	t.Run("should take NULL in a column of any type that takes it, refuse it in any other column and in the key before SQLite sees the row, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ctx := t.Context()
		null := row(nil)[0]
		m := newTx(newState())
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		for _, typ := range types {
			name := "t_" + typeName(typ)
			// A key, a column that does not take NULL, and one that does, all of the type.
			ct := nullableTable(typedTable(name, typ, typ, typ), 2)
			is.NotError(t, tx.CreateTable(ctx, ct))
			m.create(ct)
			good := edges[typ][0]
			for _, refused := range [][]*s4qlpb.Value{{null, good, good}, {null, good, null}, {good, null, good}, {good, null, null}} {
				what := typeName(typ) + " " + text(refused)
				// The message says which rule the NULL breaks: the key's, or that of a column that does
				// not take NULL.
				why := `NULL in the column "c1" of the table "` + name + `", which does not take NULL`
				if refused[0].HasNull() {
					why = `NULL in the primary-key column "c0" of the table "` + name + `"`
				}
				err := tx.Upsert(ctx, name, refused)
				is.Error(t, core.ErrInvalid, err, what)
				is.True(t, strings.Contains(err.Error(), why), what+": "+err.Error())
				inserted, err := tx.Insert(ctx, name, refused)
				is.Error(t, core.ErrInvalid, err, what)
				is.True(t, strings.Contains(err.Error(), why), what+": "+err.Error())
				is.True(t, !inserted, what)
			}
			_, err := tx.Delete(ctx, name, []*s4qlpb.Value{null})
			is.Error(t, core.ErrInvalid, err)
			_, err = tx.Get(ctx, name, []*s4qlpb.Value{null})
			is.Error(t, core.ErrInvalid, err)

			taken := []*s4qlpb.Value{good, good, null}
			is.NotError(t, tx.Upsert(ctx, name, taken))
			m.upsert(name, taken)
			got, err := tx.Get(ctx, name, []*s4qlpb.Value{good})
			is.NotError(t, err)
			is.Equal(t, text(taken), text(got))
			inserted, err := tx.Insert(ctx, name, []*s4qlpb.Value{edges[typ][1], good, null})
			is.NotError(t, err)
			is.True(t, inserted)
			m.upsert(name, []*s4qlpb.Value{edges[typ][1], good, null})
		}
		own, err := dump(ctx, tx)
		is.NotError(t, err)
		is.Equal(t, m.now.text(), show(own))
		is.NotError(t, tx.Commit(ctx))
		is.Equal(t, strings.Join(m.effect(), "\n"), strings.Join(w.commits(t)[0], "\n"))
		is.NotError(t, d.Close(ctx))

		replayed := w.serve(t, "b", core.Options{})
		is.Equal(t, m.now.text(), read(t, replayed))
		r, err := startRead(t, replayed)
		is.NotError(t, err)
		for _, typ := range types {
			got, err := r.Get(ctx, "t_"+typeName(typ), []*s4qlpb.Value{edges[typ][0]})
			is.NotError(t, err)
			is.Equal(t, text([]*s4qlpb.Value{edges[typ][0], edges[typ][0], null}), text(got))
		}
		is.NotError(t, r.Close(ctx))
	})

	t.Run("should declare a column that does not take NULL as NOT NULL in the cache, so that SQLite refuses a NULL in it too, in the cache of the transaction and in a cache that replays the log", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, createWith(nullableTable(typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT), 2)))
		is.NotError(t, d.Close(t.Context()))
		is.NotError(t, w.serve(t, "b", core.Options{}).Close(t.Context()))
		for _, cache := range []string{"a", "b"} {
			c, err := sqlite.Open(filepath.Join(w.dir, cache+".db"))
			is.NotError(t, err)
			err = c.Exec(`insert into "t" ("c0", "c1", "c2") values (1, null, 1)`)
			is.True(t, err != nil && strings.Contains(err.Error(), "NOT NULL constraint failed: t.c1"), fmt.Sprint(cache, ": ", err))
			is.NotError(t, c.Exec(`insert into "t" ("c0", "c1", "c2") values (1, 1, null)`), cache)
			is.NotError(t, c.Close())
		}
	})

	t.Run("should commit a table whose columns say that they do not take NULL as the table that the cache gives back, which leaves the field out", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ct := nullableTable(tableOf("t", 1, "k", "v", "w"), 2)
		ct.GetColumns()[0].SetNullable(false)
		ct.GetColumns()[1].SetNullable(false)
		var got *s4qlpb.CreateTable
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), ct))
			var err error
			got, err = tx.Table(t.Context(), "t")
			is.NotError(t, err)
		})
		is.True(t, !got.GetColumns()[1].HasNullable(), "the cache gives back a nullable of false")
		data, _, ok := w.store.Object(commitKey("db", w.manifest(t).Head))
		is.True(t, ok)
		r := format.NewCommitReader(bytes.NewReader(data), w.manifest(t).Head)
		change, _, err := r.Next()
		is.NotError(t, err)
		is.True(t, proto.Equal(got, change.GetCreateTable()), "the commit object holds another table than the cache gives back")
	})
}

func TestTx_tooBig(t *testing.T) {
	// limit is the length limit of the cache in these tests, which SQLite does not raise.
	const limit = 1000
	options := core.Options{LengthLimit: limit}

	t.Run("should fail the transaction for a write that fails locally, which is not one that is too big", func(t *testing.T) {
		// The connections to the cache are closed by the cleanups that are registered later, which
		// run first.
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		t.Cleanup(func() { is.NotError(t, vfs.Close()) })
		w := newWorld(t)
		o := options
		o.VFS = vfs.Name()
		d := w.serve(t, "a", o)
		ctx := t.Context()
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		is.NotError(t, tx.CreateTable(ctx, typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_BLOB)))
		// The rows take more pages than SQLite's page cache holds, so SQLite writes pages to the
		// write-ahead log before the commit, and every such write fails.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.WAL})
		for i := range 100000 {
			err = tx.Upsert(ctx, "t", row(i, bytes.Repeat([]byte{1}, 900)))
			if err != nil {
				break
			}
		}
		is.True(t, vfs.Failures() > 0, "no write failed")
		is.Error(t, core.ErrLocal, err)
		is.True(t, !errors.Is(err, core.ErrTooBig), "a local failure is too big")
		is.True(t, tx.Upsert(ctx, "t", row(-1, []byte{})) != nil, "the transaction goes on")
		found, err := tx.Delete(ctx, "t", row(0))
		is.True(t, err != nil && !found, "the transaction goes on")
		vfs.Fail(faultvfs.Fault{})
		_ = tx.Rollback(ctx)
		_ = d.Close(ctx)
	})

	t.Run("should fail the transaction for an upsert or a delete that fails locally, also where it records the keys that it touched", func(t *testing.T) {
		// The keys take more pages than SQLite's page caches hold, so SQLite writes pages of the
		// table to the write-ahead log, and pages of the temporary tables of the keys that the
		// transaction touched to a temporary file, before the commit.
		key := func(i int) string { return fmt.Sprintf("%0900d", i) }
		for _, c := range []struct {
			files  faultvfs.File
			delete bool
		}{{faultvfs.WAL, true}, {faultvfs.Temp, true}, {faultvfs.Temp, false}} {
			what := fmt.Sprint("files ", c.files, ", delete ", c.delete)
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			w := newWorld(t)
			o := options
			o.VFS = vfs.Name()
			d := w.serve(t, "a", o)
			ctx := t.Context()
			const rows = 4000
			transact(t, d, func(t *testing.T, tx *core.Tx) {
				is.NotError(t, tx.CreateTable(ctx, typedTable("t", s4qlpb.Type_TYPE_TEXT)))
				for i := range rows {
					is.NotError(t, tx.Upsert(ctx, "t", row(key(i))))
				}
			})
			tx, err := d.Begin(ctx)
			is.NotError(t, err)
			vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: c.files})
			found := true
			for i := range rows {
				if c.delete {
					found, err = tx.Delete(ctx, "t", row(key(i)))
					if err == nil {
						is.True(t, found, what)
					}
				} else {
					err = tx.Upsert(ctx, "t", row(key(i)))
				}
				if err != nil {
					break
				}
			}
			is.True(t, vfs.Failures() > 0, "no write failed: "+what)
			is.Error(t, core.ErrLocal, err, what)
			is.True(t, !errors.Is(err, core.ErrTooBig), what)
			is.True(t, !c.delete || !found, "a delete that failed found its row: "+what)
			vfs.Fail(faultvfs.Fault{})
			_ = tx.Rollback(ctx)
			_ = d.Close(ctx)
			is.NotError(t, vfs.Close(), what)
		}
	})

	t.Run("should refuse a create table whose declaration in the cache is longer than the limit, and commit nothing of it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", options)
		ctx := t.Context()
		head := w.manifest(t).Head
		// SQLite stores the text of the declaration in sqlite_schema, where it is longer than the
		// limit.
		long := tableOf("t", 1, "k", strings.Repeat("v", limit))
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		err = tx.CreateTable(ctx, long)
		is.True(t, err != nil, "the create table succeeded")
		_, err = tx.Table(ctx, "t")
		is.True(t, err != nil, "the transaction goes on")
		is.True(t, tx.Commit(ctx) != nil, "the commit succeeded")
		is.Equal(t, head, w.manifest(t).Head, "the log has a new commit object")
		_ = d.Close(ctx)
	})

	t.Run("should fail the transaction, and write no commit object, where the temporary tables of the keys that it touched cannot be created or dropped", func(t *testing.T) {
		// The keys of t take more pages than SQLite's page cache of the temporary tables holds, so
		// SQLite writes those pages to the temporary file and reads them back, and a create or a drop
		// of a temporary table needs the file: there is no room for a page without it.
		key := func(i int) string { return fmt.Sprintf("%0900d", i) }
		const rows = 4000
		for _, step := range []string{"create", "drop table", "commit"} {
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			w := newWorld(t)
			o := options
			o.VFS = vfs.Name()
			temp := faultvfs.Fault{Ops: faultvfs.Read | faultvfs.Write, Files: faultvfs.Temp}
			// The commit object of the step "commit" is one row, which the spool writes to its file at
			// once at the end, after the commit has read the temporary tables and before it drops them.
			var armAtSpool atomic.Bool
			o.WrapSpools(func(kind string, f *os.File) core.SpoolFile {
				return &armingSpool{f: f, arm: func() {
					if armAtSpool.Load() {
						vfs.Fail(temp)
					}
				}}
			})
			d := w.serve(t, "a", o)
			ctx := t.Context()
			transact(t, d, func(t *testing.T, tx *core.Tx) {
				is.NotError(t, tx.CreateTable(ctx, typedTable("t", s4qlpb.Type_TYPE_TEXT)))
				is.NotError(t, tx.CreateTable(ctx, typedTable("u", s4qlpb.Type_TYPE_INT)))
				if step != "commit" {
					for i := range rows {
						is.NotError(t, tx.Upsert(ctx, "t", row(key(i))))
					}
				}
				is.NotError(t, tx.Upsert(ctx, "u", row(1)))
			})
			head := w.manifest(t).Head
			tx, err := d.Begin(ctx)
			is.NotError(t, err)
			for i := range rows {
				is.NotError(t, tx.Upsert(ctx, "t", row(key(i))))
			}
			before := vfs.Failures()
			switch step {
			case "create":
				// The upsert into u creates its temporary tables.
				vfs.Fail(temp)
				err = tx.Upsert(ctx, "u", row(1))
			case "drop table":
				vfs.Fail(temp)
				err = tx.DropTable(ctx, "t")
			case "commit":
				// The keys of t are of rows that the transaction inserts and deletes, so the commit object
				// holds only the new row of u.
				for i := range rows {
					_, err := tx.Delete(ctx, "t", row(key(i)))
					is.NotError(t, err)
				}
				is.NotError(t, tx.Upsert(ctx, "u", row(2)))
				armAtSpool.Store(true)
				err = tx.Commit(ctx)
			}
			is.True(t, vfs.Failures() > before, "nothing failed: "+step)
			is.Error(t, core.ErrLocal, err, step)
			is.Equal(t, head, w.manifest(t).Head, "the log has a new commit object: "+step)
			vfs.Fail(faultvfs.Fault{})
			_ = tx.Rollback(ctx)
			_ = d.Close(ctx)
			is.NotError(t, vfs.Close(), step)
		}
	})

	// sqliteTakes reports whether SQLite's own table of the same shape, under the same limit,
	// takes the row, and fails the test for any other error than "string or blob too big".
	sqliteTakes := func(t *testing.T, create string, row []*s4qlpb.Value) bool {
		t.Helper()
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		is.NotError(t, c.SetLengthLimit(limit))
		is.NotError(t, c.Exec(create))
		s, err := c.Prepare(fmt.Sprint("insert into t values (", strings.Repeat("?, ", len(row)-1), "?)"))
		is.NotError(t, err)
		defer func() { _ = s.Finalize() }()
		for i, v := range row {
			if err := bindValue(s, i+1, v); err != nil {
				return false
			}
		}
		_, err = s.Step()
		if err != nil {
			is.True(t, strings.Contains(err.Error(), "string or blob too big"), err.Error())
			return false
		}
		return true
	}

	t.Run("should refuse a text or a blob, and a row, exactly where SQLite refuses it as too big, and go on with nothing changed", func(t *testing.T) {
		for _, shape := range []struct {
			name, create string
			table        *s4qlpb.CreateTable
			// row of a value of n bytes.
			row func(n int) []*s4qlpb.Value
		}{
			{"a text key", "create table t (c0 text primary key) strict, without rowid", typedTable("t", s4qlpb.Type_TYPE_TEXT),
				func(n int) []*s4qlpb.Value { return row(strings.Repeat("x", n)) }},
			{"a blob key", "create table t (c0 blob primary key) strict, without rowid", typedTable("t", s4qlpb.Type_TYPE_BLOB),
				func(n int) []*s4qlpb.Value { return row(bytes.Repeat([]byte{0}, n)) }},
			{"a blob beside an integer key", "create table t (c0 int primary key, c1 blob) strict, without rowid", typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_BLOB),
				func(n int) []*s4qlpb.Value { return row(1, bytes.Repeat([]byte{1}, n)) }},
			{"a text beside a text key", "create table t (c0 text primary key, c1 text) strict, without rowid", typedTable("t", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_TEXT),
				func(n int) []*s4qlpb.Value { return row(strings.Repeat("k", n/2), strings.Repeat("v", n-n/2)) }},
			// A NULL takes no bytes in the body of SQLite's record, only its byte in the header.
			{"a text key beside NULL", "create table t (c0 text primary key, c1 int) strict, without rowid", nullableTable(typedTable("t", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_INT), 1),
				func(n int) []*s4qlpb.Value { return row(strings.Repeat("k", n), nil) }},
			{"a blob between NULLs beside an integer key", "create table t (c0 int primary key, c1 real, c2 blob, c3 text) strict, without rowid",
				nullableTable(typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_BLOB, s4qlpb.Type_TYPE_TEXT), 1, 3),
				func(n int) []*s4qlpb.Value { return row(1, nil, bytes.Repeat([]byte{1}, n), nil) }},
		} {
			// In a table that the transaction created, and in one that existed before it, where the
			// transaction records the keys that it touches.
			for _, before := range []bool{false, true} {
				what := fmt.Sprint(shape.name, ", in a table that existed before: ", before)
				w := newWorld(t)
				d := w.serve(t, "a", options)
				ctx := t.Context()
				small := shape.row(10)
				committed := newState()
				if before {
					transact(t, d, func(t *testing.T, tx *core.Tx) {
						is.NotError(t, tx.CreateTable(ctx, shape.table))
						is.NotError(t, tx.Upsert(ctx, "t", small))
					})
					m := newTx(committed)
					m.create(shape.table)
					m.upsert("t", small)
					committed = m.now
				}

				m := newTx(committed)
				tx, err := d.Begin(ctx)
				is.NotError(t, err)
				if !before {
					is.NotError(t, tx.CreateTable(ctx, shape.table))
					is.NotError(t, tx.Upsert(ctx, "t", small))
					m.create(shape.table)
					m.upsert("t", small)
				}
				var took, refused int
				for n := limit - 20; n <= limit+2; n++ {
					err := tx.Upsert(ctx, "t", shape.row(n))
					if sqliteTakes(t, shape.create, shape.row(n)) {
						is.NotError(t, err, fmt.Sprint(what, ", ", n, " bytes"))
						m.upsert("t", shape.row(n))
						took++
					} else {
						is.Error(t, core.ErrTooBig, err, fmt.Sprint(what, ", ", n, " bytes"))
						is.True(t, !errors.Is(err, core.ErrInvalid), "too big is not invalid")
						refused++
					}
				}
				// The boundary is inside the lengths that were tried.
				is.True(t, took > 0 && refused > 2, fmt.Sprint(what, ": took ", took, ", refused ", refused))

				// A key that is longer than the limit is refused in a get and a delete, too.
				var long []*s4qlpb.Value
				switch small[0].WhichKind() {
				case s4qlpb.Value_Text_case:
					long = row(strings.Repeat("x", limit+1))
				case s4qlpb.Value_Blob_case:
					long = row(bytes.Repeat([]byte{0}, limit+1))
				}
				if long != nil {
					_, err := tx.Get(ctx, "t", long)
					is.Error(t, core.ErrTooBig, err)
					want := fmt.Sprintf(`a value of %v bytes in the column "c0" of the table "t", where the limit is %v`, limit+1, limit)
					is.True(t, strings.Contains(err.Error(), want), err.Error())
					_, err = tx.Delete(ctx, "t", long)
					is.Error(t, core.ErrTooBig, err)
				}

				own, err := dump(ctx, tx)
				is.NotError(t, err)
				is.Equal(t, m.now.text(), show(own), what)
				is.NotError(t, tx.Commit(ctx), what)
				is.Equal(t, m.now.text(), read(t, d), what)
				log := w.commits(t)
				is.Equal(t, strings.Join(m.effect(), "\n"), strings.Join(log[len(log)-1], "\n"), what)

				is.NotError(t, d.Close(ctx))
				is.Equal(t, m.now.text(), read(t, w.serve(t, "b", options)), what)
			}
		}
	})

	t.Run("should insert a row of a new key, insert nothing for a key that exists, and refuse a row exactly where SQLite refuses it as too big, also for a key that exists", func(t *testing.T) {
		create := "create table t (c0 int primary key, c1 text, c2 blob) strict, without rowid"
		table := typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB)
		big := func(key int64, n int) []*s4qlpb.Value {
			return row(key, strings.Repeat("x", n/2), bytes.Repeat([]byte{1}, n-n/2))
		}
		w := newWorld(t)
		d := w.serve(t, "a", options)
		ctx := t.Context()
		m := newTx(newState())
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(ctx, table))
			inserted, err := tx.Insert(ctx, "t", row(1, "a", []byte{1}))
			is.NotError(t, err)
			is.True(t, inserted)
		})
		m.create(table)
		m.upsert("t", row(1, "a", []byte{1}))
		committed := m.now

		// In a table that existed before the transaction, where the transaction records the keys that
		// it touches.
		m = newTx(committed)
		tx, err := d.Begin(ctx)
		is.NotError(t, err)
		var took, refused int
		for n := limit - 20; n <= limit+2; n++ {
			inserted, err := tx.Insert(ctx, "t", big(1, n))
			if sqliteTakes(t, create, big(1, n)) {
				is.NotError(t, err, fmt.Sprint(n, " bytes, a key that exists"))
				is.True(t, !inserted, fmt.Sprint(n, " bytes, a key that exists"))
				took++
			} else {
				is.Error(t, core.ErrTooBig, err, fmt.Sprint(n, " bytes, a key that exists"))
				is.True(t, !inserted)
				refused++
			}
			key := int64(n + 10)
			inserted, err = tx.Insert(ctx, "t", big(key, n))
			if sqliteTakes(t, create, big(key, n)) {
				is.NotError(t, err, fmt.Sprint(n, " bytes, a new key"))
				is.True(t, inserted, fmt.Sprint(n, " bytes, a new key"))
				m.upsert("t", big(key, n))
			} else {
				is.Error(t, core.ErrTooBig, err, fmt.Sprint(n, " bytes, a new key"))
				is.True(t, !inserted)
			}
		}
		is.True(t, took > 0 && refused > 2, fmt.Sprint("took ", took, ", refused ", refused))
		own, err := dump(ctx, tx)
		is.NotError(t, err)
		is.Equal(t, m.now.text(), show(own))
		is.NotError(t, tx.Commit(ctx))
		is.Equal(t, m.now.text(), read(t, d))
		log := w.commits(t)
		is.Equal(t, strings.Join(m.effect(), "\n"), strings.Join(log[len(log)-1], "\n"))
	})

	t.Run("should refuse a key longer than the limit in a read", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", options)
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), typedTable("t", s4qlpb.Type_TYPE_TEXT)))
		})
		r, err := startRead(t, d)
		is.NotError(t, err)
		_, err = r.Get(t.Context(), "t", row(strings.Repeat("x", limit+1)))
		is.Error(t, core.ErrTooBig, err)
		got, err := r.Get(t.Context(), "t", row(strings.Repeat("x", limit)))
		is.NotError(t, err)
		is.True(t, got == nil)
		is.NotError(t, r.Close(t.Context()))
	})
}

// armingSpool is a spool file that calls arm before each write. It does not embed the file, so that
// no method of the file, such as ReadFrom, goes around arm.
type armingSpool struct {
	f   *os.File
	arm func()
}

func (s *armingSpool) Read(p []byte) (int, error) {
	return s.f.Read(p)
}

func (s *armingSpool) Write(p []byte) (int, error) {
	s.arm()
	return s.f.Write(p)
}

func (s *armingSpool) Seek(offset int64, whence int) (int64, error) {
	return s.f.Seek(offset, whence)
}

func (s *armingSpool) Close() error {
	return s.f.Close()
}
