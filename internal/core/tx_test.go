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
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
			steps:  []step{create("t3", 1, "k", "v"), upsert("t3", 2, nil), upsert("t3", 1, 1), upsert("t3", 3, 3), remove("t3", true, 3), upsert("t3", 1, 2)},
			commit: []string{"create t3(k*0,v)", "upsert t3 [0 1] 1,2", "upsert t3 [0 1] 2,null"},
			state:  "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100\nt3(k*0,v): 1,2 | 2,null",
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
				note("select ?, ?, ?, ?", "text", 1.5, nil, []byte{1}),
			},
			commit: []string{
				"upsert t1 [0 1] 3,30", "delete t1 1",
				"note insert into t1 (k, v) values (?, ?) [3,30]", "note delete from t1 where k = ? [1]",
				fmt.Sprint("note select ?, ?, ?, ? [?", s4qlpb.Value_Text_case, ",?", s4qlpb.Value_Real_case, ",null,?", s4qlpb.Value_Blob_case, "]"),
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

// heapWatch samples the heap of the Go runtime while a test runs, and reports how far it grew.
type heapWatch struct {
	base uint64
	peak atomic.Uint64
	stop chan struct{}
	done chan struct{}
}

func watchHeap() *heapWatch {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w := &heapWatch{base: m.HeapAlloc, stop: make(chan struct{}), done: make(chan struct{})}
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

// grown is how many bytes the heap held at most above what it held at the start.
func (w *heapWatch) grown() int64 {
	close(w.stop)
	<-w.done
	return int64(w.peak.Load()) - int64(w.base)
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
	instrumented := raceEnabled || asanEnabled
	rows := 300_000
	if instrumented {
		rows = 30_000
	}
	const limit, replayLimit = 8 << 20, 48 << 20
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
		transact(t, d, create("old", 2, "a", "b", "c", "d"))

		large := int64(1) << 40
		start := time.Now()
		heap := watchHeap()
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
		grown := heap.grown()

		info, err := os.Stat(s.path(commitKey("db", s.manifest(t).Head)))
		is.NotError(t, err)
		t.Logf("a transaction of %v rows gave a commit object of %v bytes in %v, and the heap grew by at most %v bytes", 2*rows, info.Size(), time.Since(start).Round(time.Millisecond), grown)
		if !instrumented {
			is.True(t, info.Size() > 2*limit, "the commit object is too small to tell anything")
			is.True(t, grown < limit, fmt.Sprintf("the heap grew by %v bytes for a commit object of %v bytes", grown, info.Size()))
		}
		is.NotError(t, d.Close(t.Context()))

		// An empty cache replays the commit object, and holds no more of it in memory.
		start = time.Now()
		heap = watchHeap()
		replayed, err := core.Open(t.Context(), h, s, filepath.Join(dir, "b.db"), "db", core.Options{})
		is.NotError(t, err)
		defer func() { _ = replayed.Close(context.WithoutCancel(t.Context())) }()
		is.NotError(t, replayed.Wait(t.Context()))
		grown = heap.grown()
		t.Logf("the replay took %v, and the heap grew by at most %v bytes", time.Since(start).Round(time.Millisecond), grown)
		if !instrumented {
			is.True(t, grown < replayLimit, fmt.Sprintf("the heap grew by %v bytes for a commit object of %v bytes", grown, info.Size()))
		}

		r, err := replayed.Read(t.Context())
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

func TestDB_Begin(t *testing.T) {
	t.Run("should wait for a lock on the cache that another connection holds for a while, as long as the busy timeout allows", func(t *testing.T) {
		w := newWorld(t)
		// The timeout is far longer than the lock is held, so that the speed of the machine decides
		// nothing.
		d := w.serve(t, "a", core.Options{BusyTimeout: time.Minute})
		transact(t, d, create("t1", 1, "k"))

		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))
		released := make(chan error, 1)
		go func() {
			time.Sleep(200 * time.Millisecond)
			released <- other.Exec("rollback")
		}()

		transact(t, d, upsert("t1", 1))
		is.NotError(t, <-released)
		is.Equal(t, "t1(k*0): 1", read(t, d))
	})

	t.Run("should wait for a lock on the cache for a second by default", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"))

		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))

		// SQLite waits at least the busy timeout. The bound above it is far, so that the speed of the
		// machine decides nothing.
		start := time.Now()
		err = beginErr(t, d)
		elapsed := time.Since(start)
		is.Error(t, core.ErrBusy, err)
		is.True(t, elapsed >= time.Second && elapsed < 8*time.Second, fmt.Sprint(elapsed))
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
