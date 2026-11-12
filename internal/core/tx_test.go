package core_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// commits of the log of the world's database, oldest first, each as the lines of [changes].
func (w *world) commits(t *testing.T) [][]string {
	t.Helper()
	var log [][]string
	for name := w.manifest(t).Head; name != format.EmptyLog; {
		data, _, ok := w.store.Object("db/commits/" + name.String())
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
			"too many columns":            tableOf("t3", 1, strings.Split(strings.TrimSuffix(strings.Repeat("c,", 2001), ","), ",")...),
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
		bubble(t, func(t *testing.T, w *world) {
			s, d, tx := committing(t, w, core.Options{})
			s.on("swap", "manifest", conflict, -1)

			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := tx.Commit(ctx)
			is.True(t, err != nil && !errors.Is(err, core.ErrOutcomeUnknown), fmt.Sprint(err))
			s.clear()
			is.Equal(t, without, read(t, d))
			is.Equal(t, 1, len(w.commits(t)))
		})
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
