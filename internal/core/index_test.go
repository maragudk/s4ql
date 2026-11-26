package core_test

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

// desc is the position of a column that an index of [indexOf] holds in descending order.
type desc int

// indexOf the given name on the table, on the columns at the given positions in the columns of the
// table: an int for a column in ascending order, and a [desc] for one in descending order.
func indexOf(name, table string, columns ...any) *s4qlpb.CreateIndex {
	ci := s4qlpb.CreateIndex_builder{Index: new(name), Table: new(table)}
	for _, c := range columns {
		switch c := c.(type) {
		case int:
			ci.Columns = append(ci.Columns, s4qlpb.IndexColumn_builder{Column: new(uint32(c))}.Build())
		case desc:
			ci.Columns = append(ci.Columns, s4qlpb.IndexColumn_builder{Column: new(uint32(c)), Descending: new(true)}.Build())
		default:
			panic(fmt.Sprintf("no column of an index for a %T", c))
		}
	}
	return ci.Build()
}

func createIndex(name, table string, columns ...any) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.CreateIndex(t.Context(), indexOf(name, table, columns...)))
	}
}

func dropIndex(name string) step {
	return func(t *testing.T, tx *core.Tx) {
		t.Helper()
		is.NotError(t, tx.DropIndex(t.Context(), name))
	}
}

// createIndexChange and dropIndexChange are changes of a commit object that is written by hand.
func createIndexChange(ci *s4qlpb.CreateIndex) *s4qlpb.Change {
	return s4qlpb.Change_builder{CreateIndex: ci}.Build()
}

func dropIndexChange(name string) *s4qlpb.Change {
	return s4qlpb.Change_builder{DropIndex: s4qlpb.DropIndex_builder{Index: new(name)}.Build()}.Build()
}

// cacheState is the dump of the cache at path, as [show] shows it, after SQLite's integrity check
// of the cache, which fails the test if an index does not hold what its table holds.
func cacheState(t *testing.T, path string) string {
	t.Helper()
	_, state, err := cacheDump(path)
	is.NotError(t, err)
	return show(state)
}

// modelOf the log of the world's database: the state after its commit objects, as the model applies
// them, which refuses a commit object that is no net effect.
func modelOf(t *testing.T, w *world) *mstate {
	t.Helper()
	var names []format.Name
	var all [][]byte
	for name := w.manifest(t).Head; name != format.EmptyLog; {
		data, _, ok := w.store.Object(commitKey("db", name))
		is.True(t, ok, "the log names a commit object that the store does not have")
		parent, _, err := changes(name, data)
		is.NotError(t, err)
		names, all = append([]format.Name{name}, names...), append([][]byte{data}, all...)
		name = parent
	}
	m := newState()
	for i, name := range names {
		_, err := m.apply(name, all[i])
		is.NotError(t, err)
	}
	return m
}

// scanBy the index of the table, or by its primary key for no name, and return the rows as text.
func scanBy(t *testing.T, r interface {
	ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
}, table string, scan core.IndexScan) []string {
	t.Helper()
	rows := []string{}
	for row, err := range r.ScanIndex(t.Context(), table, scan) {
		is.NotError(t, err)
		rows = append(rows, text(row))
	}
	return rows
}

func TestIndexKey(t *testing.T) {
	// keyed is a table of the integer columns a, b, c, and d, with the primary key on the columns at
	// the given positions, in that order.
	keyed := func(primaryKey ...uint32) *s4qlpb.CreateTable {
		ct := s4qlpb.CreateTable_builder{Table: new("t"), PrimaryKey: primaryKey}
		for _, c := range []string{"a", "b", "c", "d"} {
			ct.Columns = append(ct.Columns, s4qlpb.Column_builder{Name: new(c), Type: new(s4qlpb.Type_TYPE_INT)}.Build())
		}
		return ct.Build()
	}
	asc := func(position int) core.KeyColumn { return core.KeyColumn{Position: position} }
	dsc := func(position int) core.KeyColumn { return core.KeyColumn{Position: position, Descending: true} }

	tests := []struct {
		name     string
		table    *s4qlpb.CreateTable
		index    *s4qlpb.CreateIndex
		expected []core.KeyColumn
	}{
		{name: "should be the primary key for no index", table: keyed(0), index: nil, expected: []core.KeyColumn{asc(0)}},
		{name: "should be the primary key of several columns in key order for no index", table: keyed(2, 0), index: nil, expected: []core.KeyColumn{asc(2), asc(0)}},
		{name: "should be the column of the index and then the primary key first in the table", table: keyed(0), index: indexOf("i", "t", 2), expected: []core.KeyColumn{asc(2), asc(0)}},
		{name: "should be the column of the index and then the primary key last in the table", table: keyed(3), index: indexOf("i", "t", 1), expected: []core.KeyColumn{asc(1), asc(3)}},
		{name: "should keep the order and the directions of several columns of the index", table: keyed(0), index: indexOf("i", "t", desc(3), 1, desc(2)), expected: []core.KeyColumn{dsc(3), asc(1), dsc(2), asc(0)}},
		{name: "should not repeat the primary key that the index holds", table: keyed(0), index: indexOf("i", "t", 2, 0), expected: []core.KeyColumn{asc(2), asc(0)}},
		{name: "should keep the direction of the primary key that the index holds in descending order", table: keyed(1), index: indexOf("i", "t", desc(1), 3), expected: []core.KeyColumn{dsc(1), asc(3)}},
		{name: "should add only the columns of a primary key of several that the index does not hold, in key order and ascending", table: keyed(3, 0, 1), index: indexOf("i", "t", desc(0), 2), expected: []core.KeyColumn{dsc(0), asc(2), asc(3), asc(1)}},
		{name: "should add nothing to an index that holds every column of the primary key", table: keyed(1, 2), index: indexOf("i", "t", 2, desc(1)), expected: []core.KeyColumn{asc(2), dsc(1)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.EqualSlice(t, test.expected, core.IndexKey(test.table, test.index))
		})
	}
}

func TestTx_CreateIndex(t *testing.T) {
	// Each test starts from two tables: t1 with the rows 1,10 and 2,20 and 3,10, and t2 with the row
	// 1,1,100, and from the index i1 on the column v of t1.
	start := []step{
		create("t1", 1, "k", "v"), upsert("t1", 1, 10), upsert("t1", 2, 20), upsert("t1", 3, 10),
		create("t2", 2, "a", "b", "c"), upsert("t2", 1, 1, 100),
		createIndex("i1", "t1", 1),
	}

	t.Run("should build an index from the rows of its table, list it after the indexes that were created before it, and give it to a read after the commit", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.CreateIndex(t.Context(), indexOf("I2", "T1", desc(1), 0)))
		is.NotError(t, tx.CreateIndex(t.Context(), indexOf("i3", "t2", 2)))
		is.NotError(t, tx.CreateIndex(t.Context(), indexOf("i0", "t1", 0)))

		indexes, err := tx.Indexes(t.Context(), "T1")
		is.NotError(t, err)
		// The table has the name of its create, and a column in ascending order has no direction.
		is.Equal(t, " +i1(1) +I2(1 desc,0) +i0(0)", describeIndexes(indexes))
		is.Equal(t, "t1", indexes[1].GetTable())
		is.True(t, !indexes[0].GetColumns()[0].HasDescending(), "a column in ascending order has the field for its direction")
		index, err := tx.Index(t.Context(), "i2")
		is.NotError(t, err)
		is.Equal(t, "I2 on t1(1 desc,0)", describeIndex(index))
		is.EqualSlice(t, []string{"2,20", "1,10", "3,10"}, scanBy(t, tx, "t1", core.IndexScan{Index: "i2"}))
		is.NotError(t, tx.Commit(t.Context()))

		r, err := startRead(t, d)
		is.NotError(t, err)
		indexes, err = r.Indexes(t.Context(), "t1")
		is.NotError(t, err)
		is.Equal(t, " +i1(1) +I2(1 desc,0) +i0(0)", describeIndexes(indexes))
		index, err = r.Index(t.Context(), "I3")
		is.NotError(t, err)
		is.Equal(t, "i3 on t2(2)", describeIndex(index))
		_, err = r.Index(t.Context(), "i9")
		is.Error(t, core.ErrNoIndex, err)
		_, err = r.Index(t.Context(), "t1")
		is.Error(t, core.ErrNoIndex, err)
		_, err = r.Indexes(t.Context(), "t9")
		is.Error(t, core.ErrNoTable, err)
		is.EqualSlice(t, []string{"2,20", "1,10", "3,10"}, scanBy(t, r, "t1", core.IndexScan{Index: "I2"}))
		is.NotError(t, r.Close(t.Context()))
	})

	t.Run("should keep an index up to date with every write to its table", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		transact(t, d, upsert("t1", 4, 5), upsert("t1", 1, 30), remove("t1", true, 2))

		r, err := startRead(t, d)
		is.NotError(t, err)
		is.EqualSlice(t, []string{"4,5", "3,10", "1,30"}, scanBy(t, r, "t1", core.IndexScan{Index: "i1"}))
		is.NotError(t, r.Close(t.Context()))
		is.Equal(t, "t1(k*0,v) +i1(1): 1,30 | 3,10 | 4,5\nt2(a*0,b*1,c): 1,1,100", cacheState(t, filepath.Join(w.dir, "a.db")))
	})

	many := make([]any, 251)
	for i := range many {
		many[i] = i
	}
	for _, test := range []struct {
		name  string
		index *s4qlpb.CreateIndex
		cause error
		why   string
	}{
		{name: "on a table that does not exist", index: indexOf("i2", "t9", 0), cause: core.ErrNoTable, why: `"t9"`},
		{name: "on a table that does not exist, before it looks at the name of the index, which is reserved", index: indexOf("sqlite_i", "t9", 0), cause: core.ErrNoTable},
		{name: "on a table that does not exist, before it looks at the name of the index, which a table has", index: indexOf("t1", "t9", 0), cause: core.ErrNoTable},
		{name: "on a table without a name, before it looks at the index, which has no columns", index: indexOf("i2", ""), cause: core.ErrNoTable},
		{name: "whose name starts with sqlite_", index: indexOf("sqlite_i", "t1", 0), cause: core.ErrReservedName, why: `"sqlite_i"`},
		{name: "whose name starts with S4QL_", index: indexOf("S4QL_i", "t1", 0), cause: core.ErrReservedName},
		{name: "whose name is reserved, before it looks at its columns, which the table does not have", index: indexOf("s4ql_i", "t1", 7), cause: core.ErrReservedName},
		{name: "whose name another table has", index: indexOf("T2", "t1", 0), cause: core.ErrTableExists, why: `"t2"`},
		{name: "whose name its own table has", index: indexOf("t1", "t1", 0), cause: core.ErrTableExists, why: `"t1"`},
		{name: "whose name a table has, before it looks at its columns, of which it has none", index: indexOf("t2", "t1"), cause: core.ErrTableExists},
		{name: "whose name an index has", index: indexOf("i1", "t2", 0), cause: core.ErrIndexExists, why: `"i1"`},
		{name: "whose name an index has in another case of the letters", index: indexOf("I1", "t1", 0), cause: core.ErrIndexExists, why: `"i1"`},
		{name: "whose name an index has, before it looks at its columns, one of which it names twice", index: indexOf("i1", "t1", 0, 0), cause: core.ErrIndexExists},
		{name: "without a name", index: indexOf("", "t1", 0), cause: core.ErrInvalid, why: "invalid: malformed: change.create_index.index is empty"},
		{name: "whose name holds the character NUL", index: indexOf("i\x00", "t1", 0), cause: core.ErrInvalid, why: "NUL"},
		{name: "without columns", index: indexOf("i2", "t1"), cause: core.ErrInvalid, why: "has no columns"},
		{name: "that names a column twice", index: indexOf("i2", "t1", 1, 0, 1), cause: core.ErrInvalid, why: "columns[2] lists column 1 twice"},
		{name: "that names a column twice, once in each direction", index: indexOf("i2", "t1", 1, desc(1)), cause: core.ErrInvalid, why: "lists column 1 twice"},
		{name: "on a column that the table does not have", index: indexOf("i2", "t1", 0, 2), cause: core.ErrInvalid, why: `column 2 of the table "t1", which has 2 columns`},
		{name: "of more columns than a scan can pin", index: indexOf("i2", "t1", many...), cause: core.ErrInvalid, why: "an index of 251 columns, where the limit is 250"},
	} {
		t.Run("should refuse an index "+test.name+", and go on", func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, start...)

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			err = tx.CreateIndex(t.Context(), test.index)
			is.Error(t, test.cause, err)
			is.True(t, strings.Contains(err.Error(), test.why), err.Error())
			for _, other := range []error{core.ErrNoTable, core.ErrReservedName, core.ErrTableExists, core.ErrIndexExists, core.ErrInvalid} {
				is.True(t, other == test.cause || !errors.Is(err, other), "the error also wraps "+other.Error())
			}
			// The refusal changed nothing, and the transaction goes on and commits.
			upsert("t1", 4, 40)(t, tx)
			is.NotError(t, tx.Commit(t.Context()))
			log := w.commits(t)
			is.EqualSlice(t, []string{"upsert t1 [0 1] 4,40"}, log[len(log)-1])
			is.Equal(t, "t1(k*0,v) +i1(1): 1,10 | 2,20 | 3,10 | 4,40\nt2(a*0,b*1,c): 1,1,100", read(t, d))
		})
	}

	t.Run("should refuse an index whose statement in the cache is longer than the length limit, with its length and the limit, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: 1000})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		// The statement in the cache is `CREATE INDEX "<name>" ON "t1" ("k")`, 29 bytes and the name.
		err = tx.CreateIndex(t.Context(), indexOf(strings.Repeat("i", 1000), "t1", 0))
		is.Error(t, core.ErrInvalid, err)
		is.True(t, strings.Contains(err.Error(), "an index whose statement in the cache is 1029 bytes, where the limit is 1000"), err.Error())
		upsert("t1", 4, 40)(t, tx)
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("should take an index of 250 columns, and scan by it with every column of its key pinned", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		// 250 columns in the primary key and 250 outside it, so the key of the index has 500.
		names := manyColumns(500)
		ct := tableOf("wide", 250, names...)
		values := make([]any, 500)
		positions := make([]any, 250)
		for i := range values {
			values[i] = i
		}
		for i := range positions {
			positions[i] = 250 + i
		}
		transact(t, d, createWith(ct), upsert("wide", values...), createIndex("wide_i", "wide", positions...))

		r, err := startRead(t, d)
		is.NotError(t, err)
		pinned := append(row(values[250:]...), row(values[:250]...)...)
		is.Equal(t, 1, len(scanBy(t, r, "wide", core.IndexScan{Index: "wide_i", Equal: pinned})))
		last := len(pinned) - 1
		is.Equal(t, 1, len(scanBy(t, r, "wide", core.IndexScan{Index: "wide_i", Equal: pinned[:last], Lower: core.Bound{Value: pinned[last]}, Upper: core.Bound{Value: pinned[last]}, Backward: true})))
		is.Equal(t, 1, len(scanBy(t, r, "wide", core.IndexScan{Index: "wide_i", Backward: true})))
		is.NotError(t, r.Close(t.Context()))
	})

	t.Run("should refuse a table whose name an index has, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		err = tx.CreateTable(t.Context(), tableOf("I1", 1, "k"))
		is.Error(t, core.ErrIndexExists, err)
		is.True(t, !errors.Is(err, core.ErrTableExists), "the error says that a table exists")
		is.True(t, strings.Contains(err.Error(), `"i1"`), err.Error())
		// The name is free once the index is dropped.
		dropIndex("i1")(t, tx)
		create("I1", 1, "k")(t, tx)
		is.NotError(t, tx.Commit(t.Context()))
		is.Equal(t, "I1(k*0): \nt1(k*0,v): 1,10 | 2,20 | 3,10\nt2(a*0,b*1,c): 1,1,100", read(t, d))
	})

	t.Run("should leave nothing of an index of a transaction that is rolled back, in the cache or in the log", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		path := filepath.Join(w.dir, "a.db")
		before, head := cacheState(t, path), w.manifest(t).Head

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		createIndex("i2", "t2", 2)(t, tx)
		dropIndex("i1")(t, tx)
		createIndex("i1", "t1", 0)(t, tx)
		upsert("t1", 9, 90)(t, tx)
		is.NotError(t, tx.Rollback(t.Context()))

		is.Equal(t, before, cacheState(t, path))
		is.Equal(t, "t1(k*0,v) +i1(1): 1,10 | 2,20 | 3,10\nt2(a*0,b*1,c): 1,1,100", read(t, d))
		is.Equal(t, head, w.manifest(t).Head)
		// The next transaction creates the same index.
		transact(t, d, createIndex("i2", "t2", 2))
		is.Equal(t, "t1(k*0,v) +i1(1): 1,10 | 2,20 | 3,10\nt2(a*0,b*1,c) +i2(2): 1,1,100", cacheState(t, path))
	})

	t.Run("should take back the indexes that a statement created and dropped when the transaction rolls back to its savepoint, and keep them when it releases it", func(t *testing.T) {
		for _, release := range []bool{false, true} {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, start...)

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			createIndex("i2", "t2", 2)(t, tx)
			is.NotError(t, tx.Savepoint(t.Context()))
			dropIndex("i2")(t, tx)
			dropIndex("i1")(t, tx)
			createIndex("i3", "t1", 0)(t, tx)
			createIndex("i1", "t2", 0)(t, tx)
			if release {
				is.NotError(t, tx.Release(t.Context()))
			} else {
				is.NotError(t, tx.RollbackTo(t.Context()))
			}
			state, err := dump(t.Context(), tx)
			is.NotError(t, err)
			is.NotError(t, tx.Commit(t.Context()))

			log := w.commits(t)
			if release {
				is.Equal(t, "t1(k*0,v) +i3(0): 1,10 | 2,20 | 3,10\nt2(a*0,b*1,c) +i1(0): 1,1,100", show(state))
				is.EqualSlice(t, []string{"drop index i1", "create index i3 on t1(0)", "create index i1 on t2(0)"}, log[len(log)-1])
			} else {
				is.Equal(t, "t1(k*0,v) +i1(1): 1,10 | 2,20 | 3,10\nt2(a*0,b*1,c) +i2(2): 1,1,100", show(state))
				is.EqualSlice(t, []string{"create index i2 on t2(2)"}, log[len(log)-1])
			}
			is.Equal(t, show(state), read(t, d))
			is.Equal(t, show(state), cacheState(t, filepath.Join(w.dir, "a.db")))
			is.NotError(t, d.Close(t.Context()))
			is.Equal(t, show(state), read(t, w.serve(t, "b", core.Options{})))
		}
	})
}

func TestTx_DropIndex(t *testing.T) {
	start := []step{create("t1", 1, "k", "v"), upsert("t1", 1, 10), createIndex("i1", "t1", 1), createIndex("i2", "t1", 0)}

	t.Run("should drop an index by its name in any case of the letters, and leave its table, the rows, and the other indexes", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		transact(t, d, dropIndex("I1"))
		is.Equal(t, "t1(k*0,v) +i2(0): 1,10", read(t, d))
		is.Equal(t, "t1(k*0,v) +i2(0): 1,10", cacheState(t, filepath.Join(w.dir, "a.db")))
	})

	for name, index := range map[string]string{
		"that does not exist":                           "i9",
		"under the name of a table":                     "t1",
		"under the name of the index of a primary key":  "sqlite_autoindex_t1_1",
		"under a reserved name":                         "s4ql_cache",
		"without a name":                                "",
		"under a name that holds the character NUL":     "i1\x00",
		"under a name that an index starts with":        "i",
		"under a name with other letters than an index": "í1",
	} {
		t.Run("should refuse to drop an index "+name+", and go on", func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, start...)

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.Error(t, core.ErrNoIndex, tx.DropIndex(t.Context(), index))
			upsert("t1", 2, 20)(t, tx)
			is.NotError(t, tx.Commit(t.Context()))
			is.Equal(t, "t1(k*0,v) +i1(1) +i2(0): 1,10 | 2,20", read(t, d))
			is.Equal(t, "t1(k*0,v) +i1(1) +i2(0): 1,10 | 2,20", cacheState(t, filepath.Join(w.dir, "a.db")))
		})
	}
}

func TestTx_Commit_indexes(t *testing.T) {
	// Each test starts from the table t1 with the rows 1,10 and 2,20 and its indexes i1 on v and i2
	// on k, in that order, and from the table t2 with the row 1,1,100 and its index i3 on c
	// descending and a.
	start := []step{
		create("t1", 1, "k", "v"), upsert("t1", 1, 10), upsert("t1", 2, 20),
		create("t2", 2, "a", "b", "c"), upsert("t2", 1, 1, 100),
		createIndex("i1", "t1", 1), createIndex("i2", "t1", 0), createIndex("i3", "t2", desc(2), 0),
	}
	const t1, t2 = "t1(k*0,v) +i1(1) +i2(0): 1,10 | 2,20", "t2(a*0,b*1,c) +i3(2 desc,0): 1,1,100"

	for _, test := range []struct {
		name  string
		steps []step
		// commit is the commit object of the steps, and nil if they leave nothing to commit.
		commit []string
		// state of the database after the steps.
		state string
	}{
		{
			name:   "should list an index that was created, after the indexes that its table had",
			steps:  []step{createIndex("i4", "t1", desc(1), 0)},
			commit: []string{"create index i4 on t1(1 desc,0)"},
			state:  "t1(k*0,v) +i1(1) +i2(0) +i4(1 desc,0): 1,10 | 2,20\n" + t2,
		},
		{
			name:   "should list a table that was created, then its rows, and then the index that was created on it",
			steps:  []step{create("t3", 1, "k", "v"), createIndex("i4", "t3", 1), upsert("t3", 2, 5), upsert("t3", 1, 7)},
			commit: []string{"create t3(k*0,v)", "upsert t3 [0 1] 1,7", "upsert t3 [0 1] 2,5", "create index i4 on t3(1)"},
			state:  t1 + "\n" + t2 + "\nt3(k*0,v) +i4(1): 1,7 | 2,5",
		},
		{
			name:  "should leave nothing of an index that was created and dropped",
			steps: []step{createIndex("i4", "t1", 0), dropIndex("i4"), createIndex("i4", "t2", 1), dropIndex("I4")},
			state: t1 + "\n" + t2,
		},
		{
			name:   "should list the drop of an index",
			steps:  []step{dropIndex("I1")},
			commit: []string{"drop index i1"},
			state:  "t1(k*0,v) +i2(0): 1,10 | 2,20\n" + t2,
		},
		{
			name:   "should list a drop and a create for an index that was dropped and created again as it was, which is then the last index of its table",
			steps:  []step{dropIndex("i1"), createIndex("i1", "t1", 1)},
			commit: []string{"drop index i1", "create index i1 on t1(1)"},
			state:  "t1(k*0,v) +i2(0) +i1(1): 1,10 | 2,20\n" + t2,
		},
		{
			name:   "should list only the last index of a name that was dropped, created, dropped, and created again, on another table",
			steps:  []step{dropIndex("i1"), createIndex("i1", "t1", 0), dropIndex("i1"), createIndex("I1", "t2", 1)},
			commit: []string{"drop index i1", "create index I1 on t2(1)"},
			state:  "t1(k*0,v) +i2(0): 1,10 | 2,20\nt2(a*0,b*1,c) +i3(2 desc,0) +I1(1): 1,1,100",
		},
		{
			name:   "should list only the drop of a table for the indexes that went with it",
			steps:  []step{dropTable("t1")},
			commit: []string{"drop t1"},
			state:  t2,
		},
		{
			name:   "should list the drop of an index before the drop of its table, when the transaction dropped both",
			steps:  []step{upsert("t1", 3, 30), dropIndex("i2"), dropTable("t1")},
			commit: []string{"drop index i2", "drop t1"},
			state:  t2,
		},
		{
			name:   "should leave nothing of an index that was created on a table that was then dropped",
			steps:  []step{createIndex("i4", "t2", 1), dropIndex("i3"), createIndex("i3", "t2", 0), dropTable("t2")},
			commit: []string{"drop index i3", "drop t2"},
			state:  t1,
		},
		{
			name:   "should list a drop, a create, the rows, and the new index for a table that was dropped and created again with another index, and nothing of the indexes that the table had",
			steps:  []step{dropTable("t1"), create("t1", 1, "v", "k"), upsert("t1", 7, 70), createIndex("i9", "t1", 1), createIndex("i1", "t1", desc(0))},
			commit: []string{"drop t1", "create t1(v*0,k)", "upsert t1 [0 1] 7,70", "create index i9 on t1(1)", "create index i1 on t1(0 desc)"},
			state:  "t1(v*0,k) +i9(1) +i1(0 desc): 7,70\n" + t2,
		},
		{
			name:   "should list the drops of indexes, then the changes of the tables, and then the new indexes, in the order in which the transaction created them",
			steps:  []step{upsert("t2", 2, 2, 200), createIndex("i5", "t2", 1), dropIndex("i2"), remove("t1", true, 1), createIndex("i4", "t1", 1, 0), dropIndex("i3")},
			commit: []string{"drop index i2", "drop index i3", "upsert t2 [0 1 2] 2,2,200", "delete t1 1", "create index i5 on t2(1)", "create index i4 on t1(1,0)"},
			state:  "t1(k*0,v) +i1(1) +i4(1,0): 2,20\nt2(a*0,b*1,c) +i5(1): 1,1,100 | 2,2,200",
		},
		{
			name:   "should give the name of a table that was dropped to an index",
			steps:  []step{dropTable("t2"), createIndex("T2", "t1", 0)},
			commit: []string{"drop t2", "create index T2 on t1(0)"},
			state:  "t1(k*0,v) +i1(1) +i2(0) +T2(0): 1,10 | 2,20",
		},
		{
			name:   "should give the name of an index that was dropped to a table",
			steps:  []step{dropIndex("i1"), create("I1", 1, "k"), upsert("i1", 5)},
			commit: []string{"drop index i1", "create I1(k*0)", "upsert I1 [0] 5"},
			state:  "I1(k*0): 5\nt1(k*0,v) +i2(0): 1,10 | 2,20\n" + t2,
		},
		{
			name:   "should give the name of an index that went with its table to a table, and the name of that table to an index",
			steps:  []step{dropTable("t2"), create("i3", 1, "k"), createIndex("t2", "i3", 0)},
			commit: []string{"drop t2", "create i3(k*0)", "create index t2 on i3(0)"},
			state:  "i3(k*0) +t2(0): \n" + t1,
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
			is.Equal(t, test.state, cacheState(t, filepath.Join(w.dir, "a.db")))
			is.NotError(t, d.Close(t.Context()))

			// An empty cache replays the log to the same state as the cache that the transaction
			// changed, with the same indexes in the same order.
			replayed := w.serve(t, "b", core.Options{})
			is.Equal(t, test.state, read(t, replayed))
			is.Equal(t, test.state, cacheState(t, filepath.Join(w.dir, "b.db")))
			// The model reads the commit object as a net effect, and comes to the same state.
			is.Equal(t, test.state, modelOf(t, w).text())
		})
	}
}

func TestIndexes_order(t *testing.T) {
	t.Run("should keep the indexes of a table in the order in which they were created, through drops, closes, and a replay, also when SQLite gives an index the place in its schema of one that was dropped", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		path := filepath.Join(w.dir, "a.db")
		transact(t, d, create("t1", 1, "k", "v", "w"), create("t2", 1, "k", "v"))
		// The model of the order: the names of the indexes of each table.
		order := map[string][]string{}
		check := func(d *core.DB) {
			t.Helper()
			r, err := startRead(t, d)
			is.NotError(t, err)
			for _, table := range []string{"t1", "t2"} {
				indexes, err := r.Indexes(t.Context(), table)
				is.NotError(t, err)
				names := []string{}
				for _, ci := range indexes {
					names = append(names, ci.GetIndex())
				}
				is.Equal(t, fmt.Sprint(order[table]), fmt.Sprint(names), table)
			}
			is.NotError(t, r.Close(t.Context()))
		}
		make := func(table, name string) step {
			order[table] = append(order[table], name)
			return createIndex(name, table, 1)
		}
		drop := func(table, name string) step {
			for i, other := range order[table] {
				if other == name {
					order[table] = append(order[table][:i:i], order[table][i+1:]...)
				}
			}
			return dropIndex(name)
		}

		transact(t, d, make("t1", "c"), make("t2", "x"), make("t1", "a"), make("t1", "b"))
		check(d)
		// The last row of the schema is dropped, and the next index takes its rowid.
		transact(t, d, drop("t1", "b"), make("t1", "0"))
		check(d)
		// Rows in the middle of the schema are dropped, in one transaction with creates.
		transact(t, d, drop("t1", "c"), make("t2", "c"), make("t1", "z"), drop("t2", "x"), make("t1", "x"))
		check(d)
		// A table between the indexes in the schema, and an index that is dropped and created again.
		transact(t, d, create("t3", 1, "k"), drop("t1", "a"), make("t1", "a"), make("t2", "b"), dropTable("t3"))
		check(d)
		is.Equal(t, "[0 z x a]", fmt.Sprint(order["t1"]))
		is.Equal(t, "[c b]", fmt.Sprint(order["t2"]))
		is.NotError(t, d.Close(t.Context()))

		// The same cache, opened again, and an empty cache that replays the log.
		again := w.serve(t, "a", core.Options{})
		check(again)
		state := cacheState(t, path)
		is.Equal(t, "t1(k*0,v,w) +0(1) +z(1) +x(1) +a(1): \nt2(k*0,v) +c(1) +b(1): ", state)
		is.NotError(t, again.Close(t.Context()))
		replayed := w.serve(t, "b", core.Options{})
		check(replayed)
		is.Equal(t, state, cacheState(t, filepath.Join(w.dir, "b.db")))
	})
}

func TestReplay_indexes(t *testing.T) {
	t.Run("should apply the changes of a commit object in their order: a table with its index, an index that is created and dropped, and a table that is dropped and created again with another index", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		l.base()
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.CreateIndex(indexOf("i1", "t1", 1)))
			is.NotError(t, cw.CreateIndex(indexOf("i2", "T2", desc(2), 0)))
		})
		l.commit(func(cw *format.CommitWriter) {
			// A table, an index on it before its rows, and its rows.
			is.NotError(t, cw.CreateTable(nullableTable(tableOf("t3", 1, "k", "v"), 1)))
			is.NotError(t, cw.CreateIndex(indexOf("i3", "t3", 1)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{0, 1}, row(1, nil)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{0, 1}, row(2, 5)))
			// An index that is created and dropped, which the core never writes, and another under
			// its name.
			is.NotError(t, cw.CreateIndex(indexOf("i4", "t1", 0)))
			is.NotError(t, cw.DropIndex("I4"))
			is.NotError(t, cw.CreateIndex(indexOf("i4", "t2", 1)))
			// An index that is dropped and created again is the last of its table.
			is.NotError(t, cw.CreateIndex(indexOf("i5", "t1", 0)))
			is.NotError(t, cw.DropIndex("i1"))
			is.NotError(t, cw.CreateIndex(indexOf("I1", "t1", desc(1))))
		})
		l.commit(func(cw *format.CommitWriter) {
			// A table that is dropped, with its indexes, and created again with another index, and the
			// name of one of its indexes for a table.
			is.NotError(t, cw.DropTable("t1"))
			is.NotError(t, cw.CreateTable(tableOf("t1", 1, "v", "k")))
			is.NotError(t, cw.UpsertRow("t1", []uint32{0, 1}, row(7, 70)))
			is.NotError(t, cw.CreateIndex(indexOf("i9", "t1", 1)))
			is.NotError(t, cw.CreateTable(tableOf("i5", 1, "k")))
			// The name of a table that is dropped for an index.
			is.NotError(t, cw.DropTable("t2"))
			is.NotError(t, cw.CreateIndex(indexOf("t2", "i5", 0)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{0, 1}, row(3, nil)))
		})
		l.publish()

		const want = "i5(k*0) +t2(0): \nt1(v*0,k) +i9(1): 7,70\nt3(k*0,v?) +i3(1): 1,null | 2,5 | 3,null"
		d := w.serve(t, "a", core.Options{})
		is.Equal(t, want, read(t, d))
		is.Equal(t, want, cacheState(t, filepath.Join(w.dir, "a.db")))
		r, err := startRead(t, d)
		is.NotError(t, err)
		is.EqualSlice(t, []string{"1,null", "3,null", "2,5"}, scanBy(t, r, "t3", core.IndexScan{Index: "i3"}))
		is.NotError(t, r.Close(t.Context()))

	})

	t.Run("should apply an index of as many columns as this version takes", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		l.base()
		columns := make([]any, core.MaxIndexColumns)
		for i := range columns {
			columns[i] = i + 1
		}
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.CreateTable(tableOf("wide", 1, manyColumns(core.MaxIndexColumns+1)...)))
			is.NotError(t, cw.CreateIndex(indexOf("iw", "wide", columns...)))
		})
		l.publish()

		d := w.serve(t, "a", core.Options{})
		r, err := startRead(t, d)
		is.NotError(t, err)
		ci, err := r.Index(t.Context(), "iw")
		is.NotError(t, err)
		is.Equal(t, core.MaxIndexColumns, len(ci.GetColumns()))
		is.NotError(t, r.Close(t.Context()))
	})

	many := make([]any, 251)
	for i := range many {
		many[i] = i
	}
	wide := manyColumns(251)
	for _, test := range []struct {
		name string
		// changes of the commit object, on top of the base and of the index i1 on the column v of t1.
		changes []*s4qlpb.Change
		cause   error
		why     string
	}{
		{name: "that creates an index on a table that does not exist", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t9", 0))}, cause: core.ErrLogBroken, why: `it creates an index on the table "t9", which does not exist`},
		{name: "that creates an index on a table that it dropped", changes: []*s4qlpb.Change{dropTableChange("t2"), createIndexChange(indexOf("i2", "t2", 0))}, cause: core.ErrLogBroken, why: `"t2", which does not exist`},
		{name: "that creates an index on a table that it creates later", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t3", 0)), createTable(tableOf("t3", 1, "k"))}, cause: core.ErrLogBroken, why: `"t3", which does not exist`},
		{name: "that creates an index on a column that the table does not have", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t1", 1, 2))}, cause: core.ErrLogBroken, why: `it creates the index "i2" on column 2 of the table "t1", which has 2 columns`},
		{name: "that creates an index without columns", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t1"))}, cause: format.ErrMalformed, why: "has no columns"},
		{name: "that creates an index with a column twice", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t1", 0, desc(0)))}, cause: format.ErrMalformed, why: "lists column 0 twice"},
		{name: "that creates an index without a name", changes: []*s4qlpb.Change{createIndexChange(indexOf("", "t1", 0))}, cause: format.ErrMalformed, why: "index is empty"},
		{name: "that creates an index under the name of its table", changes: []*s4qlpb.Change{createIndexChange(indexOf("T1", "t1", 0))}, cause: format.ErrMalformed, why: "is the name of its table"},
		{name: "that creates an index under the name of another table", changes: []*s4qlpb.Change{createIndexChange(indexOf("T2", "t1", 0))}, cause: core.ErrLogBroken, why: `it creates the index "T2", and the table "t2" exists`},
		{name: "that creates an index under the name of an index", changes: []*s4qlpb.Change{createIndexChange(indexOf("I1", "t2", 0))}, cause: core.ErrLogBroken, why: `it creates the index "I1", and the index "i1" exists`},
		{name: "that creates an index under the name of an index that it creates", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "t2", 0)), createIndexChange(indexOf("i2", "t1", 0))}, cause: core.ErrLogBroken, why: `and the index "i2" exists`},
		{name: "that creates an index under a reserved name", changes: []*s4qlpb.Change{createIndexChange(indexOf("sqlite_i", "t1", 0))}, cause: core.ErrLogBroken, why: `it creates the index "sqlite_i", whose name is reserved`},
		{name: "that creates an index under a name of the cache's own", changes: []*s4qlpb.Change{createIndexChange(indexOf("S4QL_i", "t1", 0))}, cause: core.ErrLogBroken, why: `whose name is reserved`},
		{name: "that creates an index on the cache's own table", changes: []*s4qlpb.Change{createIndexChange(indexOf("i2", "s4ql_cache", 0))}, cause: core.ErrLogBroken, why: `"s4ql_cache", which does not exist`},
		{name: "that creates an index of more columns than this version takes", changes: []*s4qlpb.Change{createTable(tableOf("wide", 1, wide...)), createIndexChange(indexOf("i2", "wide", many...))}, cause: core.ErrLogBroken, why: `it creates the index "i2" of 251 columns, where the limit is 250`},
		{name: "that creates a table under the name of an index", changes: []*s4qlpb.Change{createTable(tableOf("I1", 1, "k"))}, cause: core.ErrLogBroken, why: `it creates the table "I1", and the index "i1" exists`},
		{name: "that drops an index that does not exist", changes: []*s4qlpb.Change{dropIndexChange("i9")}, cause: core.ErrLogBroken, why: `it drops the index "i9", which does not exist`},
		{name: "that drops an index twice", changes: []*s4qlpb.Change{dropIndexChange("i1"), dropIndexChange("i1")}, cause: core.ErrLogBroken, why: `it drops the index "i1", which does not exist`},
		{name: "that drops an index after the table that it was on", changes: []*s4qlpb.Change{dropTableChange("t1"), dropIndexChange("i1")}, cause: core.ErrLogBroken, why: `it drops the index "i1", which does not exist`},
		{name: "that drops a table as an index", changes: []*s4qlpb.Change{dropIndexChange("t1")}, cause: core.ErrLogBroken, why: `it drops the index "t1", which does not exist`},
		{name: "that drops the index of a primary key", changes: []*s4qlpb.Change{dropIndexChange("sqlite_autoindex_t1_1")}, cause: core.ErrLogBroken, why: `which does not exist`},
		{name: "that drops an index as a table", changes: []*s4qlpb.Change{dropTableChange("i1")}, cause: core.ErrLogBroken, why: `it drops the table "i1", which does not exist`},
		{name: "that drops an index without a name", changes: []*s4qlpb.Change{dropIndexChange("")}, cause: format.ErrMalformed, why: "drop_index.index is empty"},
		{name: "that upserts rows of an index", changes: []*s4qlpb.Change{upsertRows("i1", []uint32{0, 1}, row(1, 2))}, cause: core.ErrLogBroken, why: `it upserts rows of the table "i1", which does not exist`},
		{name: "that holds a create index with a field that this version does not know", changes: []*s4qlpb.Change{func() *s4qlpb.Change {
			change := createIndexChange(indexOf("i2", "t1", 0))
			change.GetCreateIndex().GetColumns()[0].ProtoReflect().SetUnknown([]byte{0x98, 0x06, 0x01})
			return change
		}()}, cause: format.ErrNewerVersion},
	} {
		t.Run("should stop, apply nothing, and release the lease for a commit object "+test.name, func(t *testing.T) {
			w := newWorld(t)
			l := newHandLog(t, w)
			l.base()
			l.commit(func(cw *format.CommitWriter) {
				is.NotError(t, cw.CreateIndex(indexOf("i1", "t1", 1)))
			})
			l.publish()
			const state = "t1(k*0,v) +i1(1): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100"
			first := w.serve(t, "a", core.Options{})
			is.Equal(t, state, read(t, first))
			is.NotError(t, first.Close(t.Context()))
			base := l.head

			// A change that the cache takes comes first, and must not stay.
			l.raw(nil, append([]*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(5, 50)), createIndexChange(indexOf("i8", "t1", 0))}, test.changes...), nil)
			l.commit(func(cw *format.CommitWriter) {
				is.NotError(t, cw.CreateTable(tableOf("t9", 1, "k")))
			})
			l.publish()

			d := w.open(t, "a", core.Options{})
			err := d.Wait(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.Error(t, test.cause, err)
			is.True(t, strings.Contains(err.Error(), test.why), err.Error())
			is.Error(t, core.ErrStopped, readErr(t, d))
			is.Error(t, core.ErrStopped, beginErr(t, d))

			position, got, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, base, position)
			is.Equal(t, state, show(got))
			is.NotError(t, d.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
		})
	}

	t.Run("should keep nothing of a commit object that breaks off after it created an index, and apply it when it comes whole", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			l.commit(func(cw *format.CommitWriter) {
				is.NotError(t, cw.CreateTable(tableOf("t1", 1, "k", "v")))
				// The first index is there when the rows come, and the second is built from them.
				is.NotError(t, cw.CreateIndex(indexOf("i1", "t1", desc(1))))
				for i := range 1000 {
					is.NotError(t, cw.UpsertRow("t1", []uint32{0, 1}, row(i, i%7)))
				}
				is.NotError(t, cw.CreateIndex(indexOf("i2", "t1", 1, desc(0))))
			})
			l.publish()
			s := newScriptStore(w.client())
			// The walk back through the log reads the commit object once, whole. Every download to
			// apply it breaks off in the middle of its rows, after the table and its first index.
			s.on("get", "commits/", pass, 1)
			s.on("get", "commits/", breakOff, -1)
			d := w.openOn(t, "a", s, core.Options{})

			time.Sleep(time.Minute)
			is.True(t, !serves(t, d))
			is.NotError(t, d.Err())
			is.True(t, s.count("get", "commits/") > 3, "the download was not tried again")
			position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, format.EmptyLog, position)
			is.Equal(t, "", show(state))

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			r, err := startRead(t, d)
			is.NotError(t, err)
			indexes, err := r.Indexes(t.Context(), "t1")
			is.NotError(t, err)
			is.Equal(t, " +i1(1 desc) +i2(1,0 desc)", describeIndexes(indexes))
			for _, index := range []string{"i1", "i2"} {
				is.Equal(t, 1000, len(scanBy(t, r, "t1", core.IndexScan{Index: index})))
				is.Equal(t, 143, len(scanBy(t, r, "t1", core.IndexScan{Index: index, Equal: row(0)})))
			}
			is.NotError(t, r.Close(t.Context()))
			// The integrity check of the cache passes, with both indexes.
			position, state, err = cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, l.head, position)
			is.Equal(t, 1, len(state))
		})
	})
}

func TestOpen_cache_indexes(t *testing.T) {
	t.Run("should refuse a cache of version 2, made before indexes, and leave it as it is", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10))
		is.NotError(t, d.Close(t.Context()))
		path := filepath.Join(w.dir, "a.db")
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		is.NotError(t, c.Exec("update s4ql_cache set version = 2"))
		is.NotError(t, c.Close())
		before, err := os.ReadFile(path)
		is.NotError(t, err)

		_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
		is.Error(t, core.ErrCacheVersion, err)
		is.True(t, strings.Contains(err.Error(), fmt.Sprint("it has version 2, and this version of s4ql reads version ", core.CacheVersion)), err.Error())
		is.Equal(t, int64(2), cacheVersion(t, path))
		after, err := os.ReadFile(path)
		is.NotError(t, err)
		is.True(t, string(before) == string(after), "the open changed the main file of the cache")
	})

	t.Run("should answer a get and a table, and stop for good at the first call that needs the indexes, when another process adds to the cache an index that s4ql does not make", func(t *testing.T) {
		for _, test := range []struct {
			// statement that the other process runs on the cache, which has the table t1 with the
			// columns k, v, and w, and the row 1,1,1.
			statement string
			// table that the statement puts the index on, and index that it makes.
			table, index string
			// columns of the table, and the row that it holds under the key 1, or empty for none.
			columns int
			row     string
			why     string
			// reserved is the name of an index that the statement makes with a reserved name, which a
			// call of an index by its name does not find, and which stops nothing.
			reserved string
		}{
			{statement: "create unique index u on t1 (v)", table: "t1", index: "u", columns: 3, row: "1,1,1", why: `its table "t1" has the index "u", which is unique, partial, of a constraint, or of a reserved name`},
			{statement: "create index p on t1 (v) where v > 1", table: "t1", index: "p", columns: 3, row: "1,1,1", why: `its table "t1" has the index "p", which is unique, partial`},
			{statement: "create index e on t1 (v + 1)", table: "t1", index: "e", columns: 3, row: "1,1,1", why: `the index "e" of its table "t1" is on an expression`},
			{statement: "create index e on t1 (k, abs(v) desc)", table: "t1", index: "e", columns: 3, row: "1,1,1", why: `the index "e" of its table "t1" is on an expression`},
			{statement: "create index n on t1 (w collate nocase)", table: "t1", index: "n", columns: 3, row: "1,1,1", why: `the index "n" of its table "t1" has the collation "nocase"`},
			{statement: "create index n on t1 (v, w collate rtrim desc)", table: "t1", index: "n", columns: 3, row: "1,1,1", why: `the index "n" of its table "t1" has the collation "rtrim"`},
			{statement: "create index d on t1 (v, k, v)", table: "t1", index: "d", columns: 3, row: "1,1,1", why: `the index "d" of its table "t1" holds the column "v" twice`},
			{statement: "create index s4ql_i on t1 (v)", table: "t1", columns: 3, row: "1,1,1", why: `its table "t1" has the index "s4ql_i", which is unique, partial, of a constraint, or of a reserved name`, reserved: "S4QL_I"},
		} {
			for _, write := range []bool{false, true} {
				// A get, a table, and the tables read no index, so they answer. Every other call on the
				// table reads its indexes first, a write of a row among them, and so do, by the name of
				// the index, which is no reserved one, an index, a drop index, and a create table.
				calls := []string{"indexes", "scan", "scan index", "index"}
				if write {
					calls = append(calls, "upsert", "insert", "delete", "keys", "drop table", "create index", "drop index", "create table")
				}
				for _, call := range calls {
					if test.index == "" && (call == "index" || call == "drop index" || call == "create table") {
						continue
					}
					w := newWorld(t)
					d := w.serve(t, "a", core.Options{})
					transact(t, d, create("t1", 1, "k", "v", "w"), upsert("t1", 1, 1, 1))

					c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
					is.NotError(t, err)
					is.NotError(t, c.Exec(test.statement), test.statement)
					is.NotError(t, c.Close())

					what := fmt.Sprint(test.statement, ", ", call, ", in a write: ", write)
					var r schemaReader
					var tx *core.Tx
					var end func()
					if write {
						tx, err = d.Begin(t.Context())
						is.NotError(t, err)
						r, end = tx, func() { _ = tx.Rollback(t.Context()) }
					} else {
						rtx, err := startRead(t, d)
						is.NotError(t, err)
						r, end = rtx, func() { _ = rtx.Close(t.Context()) }
					}

					for range 2 {
						got, err := r.Get(t.Context(), test.table, row(1))
						is.NotError(t, err, what)
						is.Equal(t, test.row, text(got), what)
						ct, err := r.Table(t.Context(), test.table)
						is.NotError(t, err, what)
						is.Equal(t, test.table, ct.GetTable(), what)
						is.Equal(t, test.columns, len(ct.GetColumns()), what)
						tables, err := r.Tables(t.Context())
						is.NotError(t, err, what)
						is.True(t, slices.ContainsFunc(tables, func(ct *s4qlpb.CreateTable) bool { return ct.GetTable() == test.table }), what)
					}
					if test.reserved != "" {
						_, err := r.Index(t.Context(), test.reserved)
						is.Error(t, core.ErrNoIndex, err, what)
					}
					is.True(t, !isDone(d), what)

					scanErr := func(rows iter.Seq2[[]*s4qlpb.Value, error]) error {
						for _, err := range rows {
							if err != nil {
								return err
							}
						}
						return nil
					}
					switch call {
					case "indexes":
						_, err = r.Indexes(t.Context(), test.table)
					case "scan":
						err = scanErr(r.Scan(t.Context(), test.table))
					case "scan index":
						err = scanErr(r.ScanIndex(t.Context(), test.table, core.IndexScan{}))
					case "index":
						_, err = r.Index(t.Context(), test.index)
					case "upsert":
						err = tx.Upsert(t.Context(), test.table, row(2, 2, 2)[:test.columns])
					case "insert":
						_, err = tx.Insert(t.Context(), test.table, row(2, 2, 2)[:test.columns])
					case "delete":
						_, err = tx.Delete(t.Context(), test.table, row(1))
					case "keys":
						err = tx.Keys(t.Context(), test.table, func(*core.KeySet) error { return nil })
					case "drop table":
						err = tx.DropTable(t.Context(), test.table)
					case "create index":
						err = tx.CreateIndex(t.Context(), indexOf("i1", test.table, 0))
					case "drop index":
						err = tx.DropIndex(t.Context(), test.index)
					case "create table":
						err = tx.CreateTable(t.Context(), tableOf(test.index, 1, "k"))
					}
					is.Error(t, core.ErrLocal, err, what)
					is.Error(t, core.ErrCacheVersion, err, what)
					is.True(t, strings.Contains(err.Error(), test.why), err.Error())
					end()
					is.True(t, isDone(d), what)
				}
			}
		}
	})

	t.Run("should stop for good at an index by its name when another process adds to the cache an index on a table of a reserved name", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"))
		c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.NotError(t, c.Exec("create table s4ql_x (k int primary key) strict, without rowid"))
		is.NotError(t, c.Exec("create index ix on s4ql_x (k)"))
		is.NotError(t, c.Close())

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		_, err = tx.Index(t.Context(), "IX")
		is.Error(t, core.ErrCacheVersion, err)
		is.True(t, strings.Contains(err.Error(), `its index "IX" is on the table "s4ql_x", which cannot be found`), err.Error())
		_ = tx.Rollback(t.Context())
		is.True(t, isDone(d))
	})
}

// schemaReader is what a write transaction and a read transaction both read with, with the count of
// the statements with which their connection has read tables and indexes from the schema of the
// cache.
type schemaReader interface {
	reader
	Table(ctx context.Context, name string) (*s4qlpb.CreateTable, error)
	Index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error)
	ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
	SchemaReads() int
}

func TestSchemaReads(t *testing.T) {
	// The table t5 has five indexes, and the table t0 has none. A table is read from the schema of
	// the cache with two statements, and its indexes with one more and one for each of them: six for
	// those of t5, and one for those of t0.
	start := []step{
		create("t5", 1, "k", "a", "b", "c", "d", "e"), upsert("t5", 1, 1, 1, 1, 1, 1),
		createIndex("i1", "t5", 1), createIndex("i2", "t5", 2), createIndex("i3", "t5", 3), createIndex("i4", "t5", 4), createIndex("i5", "t5", 5),
		create("t0", 1, "k", "a"), upsert("t0", 1, 1),
	}

	type call struct {
		// what the call is, as [schemaCall] makes it.
		what string
		// t5 and t0 are the statements with which the call reads the schema, on each table.
		t5, t0 int
	}
	for _, test := range []struct {
		name string
		// calls of one transaction, in their order.
		calls []call
		// write is whether only a write transaction makes the calls.
		write bool
	}{
		{name: "two statements for a get, and none for the next", calls: []call{{"get", 2, 2}, {"get", 0, 0}}},
		{name: "two statements for a table, and none for a get after it", calls: []call{{"table", 2, 2}, {"get", 0, 0}}},
		{name: "two statements for each table for the tables, and none for a table and a get after them", calls: []call{{"tables", 4, 4}, {"table", 0, 0}, {"get", 0, 0}}},
		{name: "the statements for the indexes at the first scan after a get, and none for a get and a scan after it", calls: []call{{"get", 2, 2}, {"scan", 6, 1}, {"get", 0, 0}, {"scan", 0, 0}, {"scan index", 0, 0}}},
		{name: "the statements for the table and its indexes for a scan", calls: []call{{"scan", 8, 3}, {"table", 0, 0}}},
		{name: "the statements for the table and its indexes for a scan by an index", calls: []call{{"scan index", 8, 3}, {"indexes", 0, 0}}},
		{name: "the statements for the table and its indexes for the indexes", calls: []call{{"indexes", 8, 3}, {"indexes", 0, 0}, {"get", 0, 0}}},
		{name: "the statements for the indexes for the indexes after the tables", calls: []call{{"tables", 4, 4}, {"indexes", 6, 1}}},
		{name: "the statements for the table and its indexes for an upsert", write: true, calls: []call{{"upsert", 8, 3}, {"get", 0, 0}, {"upsert", 0, 0}}},
		{name: "the statements for the indexes for an upsert after a get", write: true, calls: []call{{"get", 2, 2}, {"upsert", 6, 1}}},
		{name: "the statements for the table and its indexes for an insert", write: true, calls: []call{{"insert", 8, 3}, {"insert", 0, 0}}},
		{name: "the statements for the table and its indexes for a delete", write: true, calls: []call{{"delete", 8, 3}, {"delete", 0, 0}}},
	} {
		for _, write := range []bool{false, true} {
			if test.write && !write {
				continue
			}
			kind := "read"
			if write {
				kind = "write"
			}
			t.Run(fmt.Sprint("should read the schema with ", test.name, ", in a ", kind, " transaction"), func(t *testing.T) {
				w := newWorld(t)
				d := w.serve(t, "a", core.Options{})
				transact(t, d, start...)

				// Each table in a transaction of its own, since a connection forgets its tables when a
				// transaction ends.
				for _, table := range []string{"t5", "t0"} {
					var r schemaReader
					var tx *core.Tx
					var end func(ctx context.Context) error
					if write {
						var err error
						tx, err = d.Begin(t.Context())
						is.NotError(t, err)
						r, end = tx, tx.Rollback
					} else {
						rtx, err := startRead(t, d)
						is.NotError(t, err)
						r, end = rtx, rtx.Close
					}

					before := r.SchemaReads()
					for i, c := range test.calls {
						schemaCall(t, r, tx, table, c.what)
						want := c.t5
						if table == "t0" {
							want = c.t0
						}
						after := r.SchemaReads()
						is.Equal(t, want, after-before, fmt.Sprint("call ", i, ", ", c.what, ", on ", table))
						before = after
					}
					is.NotError(t, end(t.Context()))
				}
			})
		}
	}
}

// schemaCall makes the call that what names on the table t5 or t0 of [TestSchemaReads], which holds
// one row: a "get" of the row, a "table", the "tables", a "scan" of the table, a "scan index" by the
// third index of t5 or by the primary key of t0, the "indexes", or, with tx, the write transaction
// that r is, an "upsert" or an "insert" of a second row or a "delete" of it.
func schemaCall(t *testing.T, r schemaReader, tx *core.Tx, table, what string) {
	t.Helper()
	indexes, second := 5, row(2, 2, 2, 2, 2, 2)
	if table == "t0" {
		indexes, second = 0, row(2, 2)
	}
	switch what {
	case "get":
		got, err := r.Get(t.Context(), table, row(1))
		is.NotError(t, err)
		is.Equal(t, strings.TrimSuffix(strings.Repeat("1,", len(second)), ","), text(got))
	case "table":
		ct, err := r.Table(t.Context(), table)
		is.NotError(t, err)
		is.Equal(t, table, ct.GetTable())
	case "tables":
		tables, err := r.Tables(t.Context())
		is.NotError(t, err)
		is.Equal(t, 2, len(tables))
	case "scan":
		var rows int
		for _, err := range r.Scan(t.Context(), table) {
			is.NotError(t, err)
			rows++
		}
		is.True(t, rows > 0)
	case "scan index":
		scan := core.IndexScan{}
		if table == "t5" {
			scan.Index = "i3"
		}
		is.True(t, len(scanBy(t, r, table, scan)) > 0)
	case "indexes":
		cis, err := r.Indexes(t.Context(), table)
		is.NotError(t, err)
		is.Equal(t, indexes, len(cis))
	case "upsert":
		is.NotError(t, tx.Upsert(t.Context(), table, second))
	case "insert":
		_, err := tx.Insert(t.Context(), table, second)
		is.NotError(t, err)
	case "delete":
		_, err := tx.Delete(t.Context(), table, second[:1])
		is.NotError(t, err)
	default:
		t.Fatal("no call is named", what)
	}
}

// keyPart is one column of the key of an index or a primary key, as the tests work it out for
// themselves from the definitions.
type keyPart struct {
	position   uint32
	descending bool
}

// keyParts of the index ci of the table, or of its primary key if ci is nil: the columns of the
// index, and then the primary-key columns that the index does not hold, in ascending order.
func keyParts(ct *s4qlpb.CreateTable, ci *s4qlpb.CreateIndex) []keyPart {
	var key []keyPart
	for _, c := range ci.GetColumns() {
		key = append(key, keyPart{c.GetColumn(), c.GetDescending()})
	}
	for _, position := range ct.GetPrimaryKey() {
		if !slices.ContainsFunc(key, func(p keyPart) bool { return p.position == position }) {
			key = append(key, keyPart{position: position})
		}
	}
	return key
}

// wantPlan is the one row of SQLite's plan that a scan by the index ci of the table, or by its
// primary key if ci is nil, must have: a search of exactly that index, by a constraint for each
// pinned column and each bound, in the order of the key, or a scan of the whole of it for a scan
// without bounds. SQLite names an index that holds every column of the table a covering index.
func wantPlan(ct *s4qlpb.CreateTable, ci *s4qlpb.CreateIndex, scan core.IndexScan) string {
	key := keyParts(ct, ci)
	name := func(i int) string { return ct.GetColumns()[key[i].position].GetName() }
	var constraints []string
	for i := range scan.Equal {
		constraints = append(constraints, name(i)+"=?")
	}
	if scan.Lower.Value != nil {
		constraints = append(constraints, name(len(scan.Equal))+">?")
	}
	if scan.Upper.Value != nil {
		constraints = append(constraints, name(len(scan.Equal))+"<?")
	}
	by := "PRIMARY KEY"
	if ci != nil {
		by = "INDEX " + ci.GetIndex()
		if len(key) == len(ct.GetColumns()) {
			by = "COVERING " + by
		}
	}
	switch {
	case len(constraints) > 0:
		return fmt.Sprint("SEARCH main.", ct.GetTable(), " USING ", by, " (", strings.Join(constraints, " AND "), ")")
	case ci != nil:
		return fmt.Sprint("SCAN main.", ct.GetTable(), " USING ", by)
	}
	return "SCAN main." + ct.GetTable()
}

// sqlName quotes a name as an SQL identifier.
func sqlName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// newPlain makes a database of SQLite alone, in memory, with the table, its indexes, and the rows.
func newPlain(t *testing.T, ct *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, rows [][]*s4qlpb.Value) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	var columns, key, marks []string
	for _, column := range ct.GetColumns() {
		columns = append(columns, sqlName(column.GetName())+" "+typeName(column.GetType()))
		marks = append(marks, "?")
	}
	for _, position := range ct.GetPrimaryKey() {
		key = append(key, sqlName(ct.GetColumns()[position].GetName()))
	}
	is.NotError(t, c.Exec(fmt.Sprint("create table ", sqlName(ct.GetTable()), " (", strings.Join(columns, ", "), ", primary key (", strings.Join(key, ", "), ")) strict, without rowid")))
	for _, ci := range indexes {
		var terms []string
		for _, column := range ci.GetColumns() {
			term := sqlName(ct.GetColumns()[column.GetColumn()].GetName())
			if column.GetDescending() {
				term += " desc"
			}
			terms = append(terms, term)
		}
		is.NotError(t, c.Exec(fmt.Sprint("create index ", sqlName(ci.GetIndex()), " on ", sqlName(ct.GetTable()), " (", strings.Join(terms, ", "), ")")))
	}
	s, err := c.Prepare(fmt.Sprint("insert or replace into ", sqlName(ct.GetTable()), " values (", strings.Join(marks, ", "), ")"))
	is.NotError(t, err)
	for _, row := range rows {
		for i, v := range row {
			is.NotError(t, bindValue(s, i+1, v))
		}
		_, err := s.Step()
		is.NotError(t, err)
		is.NotError(t, s.Reset())
	}
	is.NotError(t, s.Finalize())
	return c
}

// plainScan gives the rows of a scan as SQLite gives them from the plain database c for a
// statement that the test writes itself: every pinned column "is" its value, which is how SQL
// writes an equality that NULL takes part in, the bounds are comparisons, the order is the whole
// key of the index, each column in its direction or all in the other, or, for a scan with Order, the
// columns of Order and then the primary key, and SQLite picks its plan.
func plainScan(t *testing.T, c *sqlite.Conn, ct *s4qlpb.CreateTable, ci *s4qlpb.CreateIndex, scan core.IndexScan) []string {
	t.Helper()
	key := keyParts(ct, ci)
	name := func(i int) string { return sqlName(ct.GetColumns()[key[i].position].GetName()) }
	var conditions, order []string
	var args []*s4qlpb.Value
	for i, v := range scan.Equal {
		args = append(args, v)
		conditions = append(conditions, fmt.Sprint(name(i), " is ?", len(args)))
	}
	if scan.Lower.Value != nil {
		args = append(args, scan.Lower.Value)
		conditions = append(conditions, fmt.Sprint(name(len(scan.Equal)), map[bool]string{false: " >= ?", true: " > ?"}[scan.Lower.Open], len(args)))
	}
	if scan.Upper.Value != nil {
		args = append(args, scan.Upper.Value)
		conditions = append(conditions, fmt.Sprint(name(len(scan.Equal)), map[bool]string{false: " <= ?", true: " < ?"}[scan.Upper.Open], len(args)))
	}
	for i, part := range key {
		order = append(order, name(i)+map[bool]string{false: " asc", true: " desc"}[part.descending != scan.Backward])
	}
	if len(scan.Order) > 0 {
		// The columns of the order, each in its direction, and then the whole primary key, ascending or
		// all descending.
		order = nil
		for _, c := range scan.Order {
			order = append(order, sqlName(ct.GetColumns()[c.Position].GetName())+map[bool]string{false: " asc", true: " desc"}[c.Descending])
		}
		for _, position := range ct.GetPrimaryKey() {
			order = append(order, sqlName(ct.GetColumns()[position].GetName())+map[bool]string{false: " asc", true: " desc"}[scan.Backward])
		}
	}
	sql := "select * from " + sqlName(ct.GetTable())
	if len(conditions) > 0 {
		sql += " where " + strings.Join(conditions, " and ")
	}
	sql += " order by " + strings.Join(order, ", ")

	s, err := c.Prepare(sql)
	is.NotError(t, err, sql)
	for i, v := range args {
		is.NotError(t, bindValue(s, i+1, v))
	}
	rows := []string{}
	for {
		ok, err := s.Step()
		is.NotError(t, err, sql)
		if !ok {
			break
		}
		values := make([]*s4qlpb.Value, s.ColumnCount())
		for i := range values {
			values[i] = columnValue(s, i)
		}
		rows = append(rows, text(values))
	}
	is.NotError(t, s.Finalize())
	return rows
}

// describeScan for the message of a failed check.
func describeScan(scan core.IndexScan) string {
	bound := func(b core.Bound) string {
		if b.Value == nil {
			return "none"
		}
		return fmt.Sprint(text([]*s4qlpb.Value{b.Value}), map[bool]string{false: " closed", true: " open"}[b.Open])
	}
	return fmt.Sprintf("index %q equal [%v] lower %v upper %v backward %v", scan.Index, text(scan.Equal), bound(scan.Lower), bound(scan.Upper), scan.Backward)
}

func TestScanIndex(t *testing.T) {
	// The table t has the key k, the column a that takes NULL, the text b that takes NULL, and the
	// real c. Its index i is on a and b descending, so its key is a, b descending, k. Its index j
	// is on c descending and k descending, which is its whole key. The table u has a key of two
	// columns.
	ct := nullableTable(typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL), 1, 2)
	start := []step{
		createWith(ct),
		upsert("t", 1, nil, nil, 1.5), upsert("t", 2, nil, "x", 2.5), upsert("t", 3, 1, "a", 0.5), upsert("t", 4, 1, "b", 3.5),
		upsert("t", 5, 1, nil, 1.5), upsert("t", 6, 2, "a", 1.5), upsert("t", 7, 1, "b", 2.5),
		createIndex("i", "t", 1, desc(2)), createIndex("j", "t", desc(3), desc(0)),
		create("u", 2, "a", "b", "v"), upsert("u", 1, 1, 0), upsert("u", 1, 2, 0), upsert("u", 1, 3, 0), upsert("u", 2, 1, 0),
	}
	closed := func(v any) core.Bound { return core.Bound{Value: row(v)[0]} }
	open := func(v any) core.Bound { return core.Bound{Value: row(v)[0], Open: true} }

	for _, test := range []struct {
		name  string
		table string
		scan  core.IndexScan
		// want are the keys of the rows of the scan, in its order: k of t, and a and b of u as "a.b".
		want string
	}{
		{name: "the whole index, with NULL first in an ascending column and last in a descending one", scan: core.IndexScan{Index: "i"}, want: "2 1 4 7 3 5 6"},
		{name: "the whole index backward, in the exact reverse", scan: core.IndexScan{Index: "i", Backward: true}, want: "6 5 3 7 4 1 2"},
		{name: "the index by a name in another case of the letters", scan: core.IndexScan{Index: "I"}, want: "2 1 4 7 3 5 6"},
		{name: "the rows with one value in the first column", scan: core.IndexScan{Index: "i", Equal: row(1)}, want: "4 7 3 5"},
		{name: "the rows with one value in the first column, backward", scan: core.IndexScan{Index: "i", Equal: row(1), Backward: true}, want: "5 3 7 4"},
		{name: "the rows with NULL in the first column", scan: core.IndexScan{Index: "i", Equal: row(nil)}, want: "2 1"},
		{name: "the rows with NULL in the first column, backward", scan: core.IndexScan{Index: "i", Equal: row(nil), Backward: true}, want: "1 2"},
		{name: "the rows with a value in each of two columns, in the order of the primary key", scan: core.IndexScan{Index: "i", Equal: row(1, "b")}, want: "4 7"},
		{name: "the rows with a value in the first column and NULL in the second", scan: core.IndexScan{Index: "i", Equal: row(1, nil)}, want: "5"},
		{name: "the rows with NULL in both columns", scan: core.IndexScan{Index: "i", Equal: row(nil, nil)}, want: "1"},
		{name: "no row for a value that no row has", scan: core.IndexScan{Index: "i", Equal: row(9)}, want: ""},
		{name: "a descending column from a value on, which is the start of its range in the index", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: closed("a")}, want: "4 7 3"},
		{name: "a descending column above a value", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: open("a")}, want: "4 7"},
		{name: "a descending column up to a value, without the rows whose value is NULL", scan: core.IndexScan{Index: "i", Equal: row(1), Upper: closed("a")}, want: "3"},
		{name: "a descending column below a value", scan: core.IndexScan{Index: "i", Equal: row(1), Upper: open("b")}, want: "3"},
		{name: "a descending column between two values", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: open("a"), Upper: closed("b")}, want: "4 7"},
		{name: "a descending column between two values, backward", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: closed("a"), Upper: closed("b"), Backward: true}, want: "3 7 4"},
		{name: "a descending column between two values that are the wrong way around", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: closed("b"), Upper: closed("a")}, want: ""},
		{name: "the first column from a value on", scan: core.IndexScan{Index: "i", Lower: closed(1)}, want: "4 7 3 5 6"},
		{name: "the first column up to a value, without the rows whose value is NULL", scan: core.IndexScan{Index: "i", Upper: closed(1)}, want: "4 7 3 5"},
		{name: "the first column below the smallest value, without the rows whose value is NULL", scan: core.IndexScan{Index: "i", Upper: open(1)}, want: ""},
		{name: "the first column above a value, backward", scan: core.IndexScan{Index: "i", Lower: open(1), Backward: true}, want: "6"},
		{name: "the primary-key column at the end of the key from a value on", scan: core.IndexScan{Index: "i", Equal: row(1, "b"), Lower: closed(5)}, want: "7"},
		{name: "the primary-key column at the end of the key below a value", scan: core.IndexScan{Index: "i", Equal: row(1, "b"), Upper: open(7)}, want: "4"},
		{name: "the primary-key column at the end of the key after a NULL", scan: core.IndexScan{Index: "i", Equal: row(nil, "x"), Upper: closed(2)}, want: "2"},
		{name: "one row with every column of the key pinned", scan: core.IndexScan{Index: "i", Equal: row(1, "b", 7)}, want: "7"},
		{name: "no row with every column of the key pinned to values of two rows", scan: core.IndexScan{Index: "i", Equal: row(1, "a", 7)}, want: ""},
		{name: "an index of two descending columns", scan: core.IndexScan{Index: "j"}, want: "4 7 2 6 5 1 3"},
		{name: "an index of two descending columns, backward", scan: core.IndexScan{Index: "j", Backward: true}, want: "3 1 5 6 2 7 4"},
		{name: "a descending real above a value", scan: core.IndexScan{Index: "j", Lower: open(1.5)}, want: "4 7 2"},
		{name: "a descending real up to a value", scan: core.IndexScan{Index: "j", Upper: closed(1.5)}, want: "6 5 1 3"},
		{name: "a descending primary-key column in the index from a value on", scan: core.IndexScan{Index: "j", Equal: row(1.5), Lower: closed(5)}, want: "6 5"},
		{name: "a descending primary-key column in the index below a value, backward", scan: core.IndexScan{Index: "j", Equal: row(1.5), Upper: open(6), Backward: true}, want: "1 5"},
		{name: "the primary key", scan: core.IndexScan{}, want: "1 2 3 4 5 6 7"},
		{name: "the primary key backward", scan: core.IndexScan{Backward: true}, want: "7 6 5 4 3 2 1"},
		{name: "the primary key between two values", scan: core.IndexScan{Lower: open(3), Upper: closed(6)}, want: "4 5 6"},
		{name: "the primary key between two values, backward", scan: core.IndexScan{Lower: open(3), Upper: closed(6), Backward: true}, want: "6 5 4"},
		{name: "one row by its primary key", scan: core.IndexScan{Equal: row(4)}, want: "4"},
		{name: "the second column of a primary key from a value on", table: "u", scan: core.IndexScan{Equal: row(1), Lower: closed(2)}, want: "1.2 1.3"},
		{name: "the second column of a primary key below a value, backward", table: "u", scan: core.IndexScan{Equal: row(1), Upper: open(3), Backward: true}, want: "1.2 1.1"},
		{name: "the first column of a primary key of two columns from a value on, backward", table: "u", scan: core.IndexScan{Lower: closed(1), Backward: true}, want: "2.1 1.3 1.2 1.1"},
		{name: "one row by both columns of its primary key", table: "u", scan: core.IndexScan{Equal: row(1, 3)}, want: "1.3"},
	} {
		t.Run("should scan "+test.name+", in a read and in a write, and count the rows that it read", func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, start...)
			table := test.table
			if table == "" {
				table = "t"
			}
			keys := func(rows [][]*s4qlpb.Value) string {
				var keys []string
				for _, row := range rows {
					key := fmt.Sprint(row[0].GetInteger())
					if table == "u" {
						key += fmt.Sprint(".", row[1].GetInteger())
					}
					keys = append(keys, key)
				}
				return strings.Join(keys, " ")
			}
			count := len(strings.Fields(test.want))

			r, err := startRead(t, d)
			is.NotError(t, err)
			var rows [][]*s4qlpb.Value
			for row, err := range r.ScanIndex(t.Context(), table, test.scan) {
				is.NotError(t, err)
				rows = append(rows, row)
			}
			is.Equal(t, test.want, keys(rows))
			is.Equal(t, int64(count), r.RowsRead())
			// The same scan once more adds as many.
			for _, err := range r.ScanIndex(t.Context(), table, test.scan) {
				is.NotError(t, err)
			}
			is.Equal(t, int64(2*count), r.RowsRead())
			is.NotError(t, r.Close(t.Context()))

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			rows = nil
			for row, err := range tx.ScanIndex(t.Context(), table, test.scan) {
				is.NotError(t, err)
				rows = append(rows, row)
			}
			is.Equal(t, test.want, keys(rows))
			is.Equal(t, int64(count), tx.RowsRead())
			is.NotError(t, tx.Rollback(t.Context()))
		})
	}

	t.Run("should scan what the transaction wrote, by an index that it created", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		upsert("t", 8, 1, "b", 9.5)(t, tx)
		remove("t", true, 4)(t, tx)
		upsert("t", 3, 1, "c", 0.5)(t, tx)
		createIndex("n", "t", desc(2), 1)(t, tx)
		is.EqualSlice(t, []string{`3,1,"c",r0.5`, `7,1,"b",r2.5`, `8,1,"b",r9.5`}, scanBy(t, tx, "t", core.IndexScan{Index: "i", Equal: row(1), Lower: core.Bound{Value: row("a")[0], Open: true}}))
		is.EqualSlice(t, []string{`2,null,"x",r2.5`, `3,1,"c",r0.5`, `7,1,"b",r2.5`, `8,1,"b",r9.5`}, scanBy(t, tx, "t", core.IndexScan{Index: "n", Lower: core.Bound{Value: row("b")[0]}}))
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("should count the rows that a scan stepped to and the rows that a get found, and nothing else", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		for _, read := range []bool{true, false} {
			var r interface {
				reader
				ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
				ScanRange(ctx context.Context, table string, lower, upper core.Bound) iter.Seq2[[]*s4qlpb.Value, error]
				RowsRead() int64
			}
			if read {
				rtx, err := startRead(t, d)
				is.NotError(t, err)
				defer func() { is.NotError(t, rtx.Close(t.Context())) }()
				r = rtx
			} else {
				tx, err := d.Begin(t.Context())
				is.NotError(t, err)
				defer func() { is.NotError(t, tx.Rollback(t.Context())) }()
				// The writes of a transaction, and the reads that they make, are not counted.
				upsert("u", 9, 9, 9)(t, tx)
				remove("u", true, 9, 9)(t, tx)
				r = tx
			}
			is.Equal(t, int64(0), r.RowsRead())

			// A loop that ends at the third of the seven rows of the index has stepped to three.
			var seen int
			for _, err := range r.ScanIndex(t.Context(), "t", core.IndexScan{Index: "i"}) {
				is.NotError(t, err)
				if seen++; seen == 3 {
					break
				}
			}
			is.Equal(t, int64(3), r.RowsRead())
			// A scan of nothing reads nothing, and one of two rows reads two, of the seven.
			is.Equal(t, 0, len(scanBy(t, r, "t", core.IndexScan{Index: "i", Equal: row(7)})))
			is.Equal(t, int64(3), r.RowsRead())
			is.Equal(t, 2, len(scanBy(t, r, "t", core.IndexScan{Index: "i", Equal: row(1, "b")})))
			is.Equal(t, int64(5), r.RowsRead())
			// A get counts the row that it finds.
			got, err := r.Get(t.Context(), "t", row(4))
			is.NotError(t, err)
			is.True(t, got != nil)
			is.Equal(t, int64(6), r.RowsRead())
			got, err = r.Get(t.Context(), "t", row(40))
			is.NotError(t, err)
			is.True(t, got == nil)
			is.Equal(t, int64(6), r.RowsRead())
			// The scans by the primary key count as well: four rows of u, and three of t.
			for _, err := range r.Scan(t.Context(), "u") {
				is.NotError(t, err)
			}
			is.Equal(t, int64(10), r.RowsRead())
			for _, err := range r.ScanRange(t.Context(), "t", core.Bound{Value: row(2)[0], Open: true}, core.Bound{Value: row(5)[0]}) {
				is.NotError(t, err)
			}
			is.Equal(t, int64(13), r.RowsRead())
			// What fails before it reads counts nothing, and neither does the schema.
			for _, err := range r.ScanIndex(t.Context(), "t", core.IndexScan{Index: "nope"}) {
				is.Error(t, core.ErrNoIndex, err)
			}
			_, err = r.Tables(t.Context())
			is.NotError(t, err)
			_, err = r.Indexes(t.Context(), "t")
			is.NotError(t, err)
			is.Equal(t, int64(13), r.RowsRead())
		}
	})

	for _, test := range []struct {
		name  string
		table string
		scan  core.IndexScan
		cause error
		why   string
	}{
		{name: "of a table that does not exist", table: "t9", scan: core.IndexScan{}, cause: core.ErrNoTable},
		{name: "by an index that does not exist", table: "t", scan: core.IndexScan{Index: "i9"}, cause: core.ErrNoIndex, why: `"i9" on the table "t"`},
		{name: "by an index of another table", table: "u", scan: core.IndexScan{Index: "i"}, cause: core.ErrNoIndex, why: `"i" on the table "u"`},
		{name: "by the name of the table", table: "t", scan: core.IndexScan{Index: "t"}, cause: core.ErrNoIndex},
		{name: "by the name that SQLite gives the index of the primary key", table: "t", scan: core.IndexScan{Index: "sqlite_autoindex_t_1"}, cause: core.ErrNoIndex},
		{name: "that pins more columns than the key of the index has", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1, "b", 7, 1)}, cause: core.ErrInvalid, why: `a scan that pins 4 columns of a key of the table "t", which has 3`},
		{name: "that pins more columns than the primary key has", table: "t", scan: core.IndexScan{Equal: row(1, 2)}, cause: core.ErrInvalid, why: "which has 1"},
		{name: "with a lower bound after the last column of the key", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1, "b", 7), Lower: closed(1)}, cause: core.ErrInvalid, why: "a bound after the last of the 3 columns"},
		{name: "with an upper bound after the last column of the primary key", table: "u", scan: core.IndexScan{Equal: row(1, 2), Upper: open(1)}, cause: core.ErrInvalid, why: "a bound after the last of the 2 columns"},
		{name: "with a lower bound that is NULL", table: "t", scan: core.IndexScan{Index: "i", Lower: closed(nil)}, cause: core.ErrInvalid, why: `a bound that is NULL, on the column "c1"`},
		{name: "with an upper bound that is NULL", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Upper: open(nil)}, cause: core.ErrInvalid, why: `a bound that is NULL, on the column "c2"`},
		{name: "that pins a column that does not take NULL to NULL", table: "t", scan: core.IndexScan{Index: "j", Equal: row(nil)}, cause: core.ErrInvalid, why: `NULL in the column "c3"`},
		{name: "that pins a primary-key column at the end of the key to NULL", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1, "b", nil)}, cause: core.ErrInvalid, why: `NULL in the primary-key column "c0"`},
		{name: "that pins a column to a value of another storage class", table: "t", scan: core.IndexScan{Index: "i", Equal: row("1")}, cause: core.ErrInvalid, why: `in the column "c1"`},
		{name: "that pins a real column to an integer", table: "t", scan: core.IndexScan{Index: "j", Equal: row(1)}, cause: core.ErrInvalid, why: `in the column "c3"`},
		{name: "that pins a column to no value", table: "t", scan: core.IndexScan{Index: "i", Equal: []*s4qlpb.Value{nil}}, cause: core.ErrInvalid},
		{name: "that pins a column to a value of no kind", table: "t", scan: core.IndexScan{Index: "i", Equal: []*s4qlpb.Value{{}}}, cause: core.ErrInvalid},
		{name: "with a bound of another storage class", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: closed(1)}, cause: core.ErrInvalid, why: `in the column "c2"`},
		{name: "with a valid lower bound and an upper bound of another storage class", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: closed("a"), Upper: closed(1)}, cause: core.ErrInvalid, why: `in the column "c2"`},
		{name: "with a bound that is text with the character NUL", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Upper: closed("a\x00")}, cause: core.ErrInvalid, why: "NUL"},
		{name: "with a bound of no kind", table: "t", scan: core.IndexScan{Index: "i", Upper: core.Bound{Value: &s4qlpb.Value{}}}, cause: core.ErrInvalid},
		{name: "that pins a column to text that is longer than SQLite takes", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1, strings.Repeat("x", 1001))}, cause: core.ErrTooBig},
		{name: "with a bound that is longer than SQLite takes", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Lower: open(strings.Repeat("x", 1001))}, cause: core.ErrTooBig},
	} {
		t.Run("should refuse a scan "+test.name+", read nothing, and go on", func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{LengthLimit: 1000})
			transact(t, d, start...)

			r, err := startRead(t, d)
			is.NotError(t, err)
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			for _, scan := range []iter.Seq2[[]*s4qlpb.Value, error]{r.ScanIndex(t.Context(), test.table, test.scan), tx.ScanIndex(t.Context(), test.table, test.scan)} {
				var n int
				for row, err := range scan {
					n++
					is.True(t, row == nil)
					is.Error(t, test.cause, err)
					is.True(t, strings.Contains(err.Error(), test.why), err.Error())
				}
				is.Equal(t, 1, n)
			}
			is.Equal(t, int64(0), r.RowsRead())
			is.Equal(t, 7, len(scanBy(t, r, "t", core.IndexScan{Index: "j"})))
			is.NotError(t, r.Close(t.Context()))
			// The write transaction is not failed by the refusal.
			upsert("u", 5, 5, 5)(t, tx)
			is.NotError(t, tx.Commit(t.Context()))
		})
	}

	t.Run("should take a bound of exactly the longest text that SQLite takes", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: 1000})
		transact(t, d, start...)
		r, err := startRead(t, d)
		is.NotError(t, err)
		is.Equal(t, 0, len(scanBy(t, r, "t", core.IndexScan{Index: "i", Equal: row(1), Lower: core.Bound{Value: row(strings.Repeat("x", 1000))[0]}})))
		is.NotError(t, r.Close(t.Context()))
	})

	t.Run("should end a scan by an index that is running when the instance stops, and give back its statement when a panic ends it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		rd := newReader(t, d)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)
		func() {
			defer func() { _ = recover() }()
			for _, err := range r.ScanIndex(t.Context(), "t", core.IndexScan{Index: "i"}) {
				is.NotError(t, err)
				panic("in the loop over a scan")
			}
		}()
		is.NotError(t, r.Close(t.Context()))
		// The next read of the same reader, on the same connection, sees a later commit.
		transact(t, d, upsert("t", 8, 0, "z", 0.0))
		r, err = rd.Read(t.Context())
		is.NotError(t, err)
		is.Equal(t, 8, len(scanBy(t, r, "t", core.IndexScan{Index: "i"})))

		closed := make(chan error, 1)
		var rows int
		var scanErr error
		for _, err := range r.ScanIndex(t.Context(), "t", core.IndexScan{Index: "i", Backward: true}) {
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

	t.Run("should scan the longest row that a table takes by a key of more than 63 columns, which SQLite sorts", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: 3000})
		// A key of 70 columns, 6 in the primary key and 64 in the index, and a text that fills the
		// row up to the limit.
		wide := tableOf("wide", 6, manyColumns(71)...)
		wide.GetColumns()[70].SetType(s4qlpb.Type_TYPE_TEXT)
		values := make([]any, 71)
		positions := make([]any, 64)
		for i := range values {
			values[i] = 1000 + i
		}
		for i := range positions {
			positions[i] = 6 + i
		}
		transact(t, d, createWith(wide), createIndex("wide_i", "wide", positions...))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		// The longest text with which SQLite takes the row.
		longest := -1
		for n := 3000; n > 2000; n-- {
			values[70] = strings.Repeat("x", n)
			err := tx.Upsert(t.Context(), "wide", row(values...))
			if err == nil {
				longest = n
				break
			}
			is.Error(t, core.ErrTooBig, err)
		}
		is.True(t, longest > 2000 && longest < 3000, fmt.Sprint(longest))
		// SQLite sorts by the 70 columns, under the largest length limit.
		for _, scan := range []core.IndexScan{{Index: "wide_i"}, {Index: "wide_i", Backward: true}, {Index: "wide_i", Lower: core.Bound{Value: row(1006)[0]}}} {
			is.Equal(t, 1, len(scanBy(t, tx, "wide", scan)), describeScan(scan))
		}
		is.NotError(t, tx.Commit(t.Context()))
	})

	t.Run("should scan the longest row that a table takes by a key of more than 63 columns of reals that hold integers, which SQLite sorts", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: 3000})
		// A key of 70 columns, 6 in the primary key and 64 reals in the index, and a text that fills
		// the row up to the limit. The table keeps a real that holds an integer in fewer bytes than
		// the 8 of what SQLite sorts, so the record of the sort is longer than the row.
		wide := tableOf("wide", 6, manyColumns(71)...)
		values := make([]any, 71)
		positions := make([]any, 64)
		for i := range values {
			values[i] = 1000 + i
		}
		for i := range positions {
			positions[i] = 6 + i
			wide.GetColumns()[6+i].SetType(s4qlpb.Type_TYPE_REAL)
			values[6+i] = float64(i)
		}
		wide.GetColumns()[70].SetType(s4qlpb.Type_TYPE_TEXT)
		transact(t, d, createWith(wide), createIndex("wide_i", "wide", positions...))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		// The longest text with which SQLite takes the row.
		longest := -1
		for n := 3000; n > 2000; n-- {
			values[70] = strings.Repeat("x", n)
			err := tx.Upsert(t.Context(), "wide", row(values...))
			if err == nil {
				longest = n
				break
			}
			is.Error(t, core.ErrTooBig, err)
		}
		is.True(t, longest > 2000 && longest < 3000, fmt.Sprint(longest))
		// The last scan pins 6 columns, and leaves 64, the fewest that SQLite sorts by.
		for _, scan := range []core.IndexScan{{Index: "wide_i"}, {Index: "wide_i", Backward: true}, {Index: "wide_i", Lower: core.Bound{Value: row(0.0)[0]}}, {Index: "wide_i", Equal: row(0.0, 1.0, 2.0, 3.0, 4.0, 5.0)}} {
			is.Equal(t, 1, len(scanBy(t, tx, "wide", scan)), describeScan(scan))
		}
		is.NotError(t, tx.Commit(t.Context()))
		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()
		is.Equal(t, 1, len(scanBy(t, r, "wide", core.IndexScan{Index: "wide_i"})))
	})

	t.Run("should read the same rows in the same order, sorted by SQLite, by a key of more columns than SQLite compares with the order of an index", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		// A key of 70 columns: 6 in the primary key, and 64 in the index.
		wide := tableOf("wide", 6, manyColumns(70)...)
		steps := []step{createWith(wide)}
		rnd := rand.New(rand.NewPCG(9, 9))
		var rows [][]*s4qlpb.Value
		for range 12 {
			values := make([]any, 70)
			for i := range values {
				// The columns of the index hold few values, so that the later columns decide the order.
				values[i] = rnd.IntN(2)
			}
			rows = append(rows, row(values...))
			steps = append(steps, upsert("wide", values...))
		}
		positions := make([]any, 64)
		for i := range positions {
			positions[i] = 6 + i
			if i%3 == 0 {
				positions[i] = desc(6 + i)
			}
		}
		ci := indexOf("wide_i", "wide", positions...)
		transact(t, d, append(steps, createIndex("wide_i", "wide", positions...))...)
		plain := newPlain(t, wide, []*s4qlpb.CreateIndex{ci}, rows)

		r, err := startRead(t, d)
		is.NotError(t, err)
		for _, scan := range []core.IndexScan{
			{Index: "wide_i"}, {Index: "wide_i", Backward: true},
			{Index: "wide_i", Equal: row(0, 1), Lower: core.Bound{Value: row(1)[0]}},
			// With seven columns pinned, 63 are left to order by, which SQLite reads in order.
			{Index: "wide_i", Equal: rows[0][6:13], Backward: true},
		} {
			before := r.RowsRead()
			got := scanBy(t, r, "wide", scan)
			is.EqualSlice(t, plainScan(t, plain, wide, ci, scan), got, describeScan(scan))
			is.Equal(t, int64(len(got)), r.RowsRead()-before)
			_, plan, err := r.ScanPlan(t.Context(), "wide", scan)
			is.NotError(t, err)
			if len(scan.Equal) == 7 {
				is.EqualSlice(t, []string{wantPlan(wide, ci, scan)}, plan)
				continue
			}
			// SQLite still reads the index that was asked for, by the bounds that were asked for, and
			// then sorts what it read.
			is.EqualSlice(t, []string{wantPlan(wide, ci, scan), "USE TEMP B-TREE FOR ORDER BY"}, plan)
		}
		is.NotError(t, r.Close(t.Context()))
	})
}

// choices of a generated test of scans: each takes the next byte of a fuzz input, and then the
// numbers of a random source, or zero if there is none.
type choices struct {
	b   []byte
	rnd *rand.Rand
}

// intn is a choice among n.
func (c *choices) intn(n int) int {
	switch {
	case len(c.b) > 0:
		v := int(c.b[0])
		c.b = c.b[1:]
		return v % n
	case c.rnd != nil:
		return c.rnd.IntN(n)
	}
	return 0
}

// scanStats count the scans of a generated test, by what they exercise.
type scanStats struct {
	scans, some, pinnedNull, descendingBound, trailing, backward, covering int
}

// checkIndexScans makes, by the choices of in, a table of two to five columns of any types, with a
// primary key of one or two of them, columns that take NULL, defaults of their types and of NULL,
// up to twelve rows, and one to three indexes, in the core and in a database of SQLite alone. The
// table must read back with its defaults. It then runs scans by the primary key and
// by each index: every shape, if every, of the number of pinned columns, of a lower and an upper
// bound that are missing, closed, or open, and of the direction, and else eight that in chooses.
// Each scan must give the rows that SQLite gives for a statement of the test's own, in the same
// order; SQLite's plan for the statement of the core must be a search of the index that was asked
// for by the bounds that were asked for, and nothing else, so no sort; and the scan must count as
// many rows as it gave, which are the rows within its bounds.
func checkIndexScans(t *testing.T, in *choices, every bool) scanStats {
	t.Helper()
	names := []string{"a", "B", `c "q"`, "select", "é"}
	ct := s4qlpb.CreateTable_builder{Table: new(`t ';"x`)}
	n := 2 + in.intn(4)
	first := in.intn(n)
	ct.PrimaryKey = []uint32{uint32(first)}
	if second := in.intn(n); second != first {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(second))
	}
	for i := range n {
		column := s4qlpb.Column_builder{Name: new(names[i]), Type: new(types[in.intn(len(types))])}
		if !slices.Contains(ct.PrimaryKey, uint32(i)) && in.intn(2) == 0 {
			column.Nullable = new(true)
		}
		ct.Columns = append(ct.Columns, column.Build())
	}
	table := ct.Build()
	// A column has a default one time in three, NULL one time in four of those, and else a key of its
	// type; the table must read back with them.
	for _, c := range table.GetColumns() {
		if in.intn(3) > 0 {
			continue
		}
		if pool := simKeys[c.GetType()]; in.intn(4) > 0 {
			c.SetDefault(pool[in.intn(len(pool))])
		} else {
			c.SetDefault(row(nil)[0])
		}
	}
	value := func(position uint32, null int) *s4qlpb.Value {
		c := table.GetColumns()[position]
		if c.GetNullable() && in.intn(null) == 0 {
			return row(nil)[0]
		}
		pool := simKeys[c.GetType()]
		return pool[in.intn(len(pool))]
	}
	var rows [][]*s4qlpb.Value
	for range in.intn(13) {
		values := make([]*s4qlpb.Value, n)
		for i := range values {
			values[i] = value(uint32(i), 4)
		}
		rows = append(rows, values)
	}
	var indexes []*s4qlpb.CreateIndex
	// before are the indexes that are created before the rows are written, which SQLite then keeps
	// up to date, and the others it builds from the rows.
	var before []bool
	for i := range 1 + in.intn(3) {
		positions := make([]uint32, n)
		for j := range positions {
			positions[j] = uint32(j)
		}
		for j := n - 1; j > 0; j-- {
			k := in.intn(j + 1)
			positions[j], positions[k] = positions[k], positions[j]
		}
		ci := s4qlpb.CreateIndex_builder{Index: new([]string{"i0", `I "1`, "select"}[i]), Table: new(table.GetTable())}
		for _, position := range positions[:1+in.intn(min(n, 3))] {
			column := s4qlpb.IndexColumn_builder{Column: new(position)}
			if in.intn(2) == 0 {
				column.Descending = new(true)
			}
			ci.Columns = append(ci.Columns, column.Build())
		}
		indexes = append(indexes, ci.Build())
		before = append(before, in.intn(2) == 0)
	}

	w := newWorld(t)
	d := w.serve(t, "a", core.Options{})
	write(t, d, func(tx *core.Tx) {
		is.NotError(t, tx.CreateTable(t.Context(), table))
		for i, ci := range indexes {
			if before[i] {
				is.NotError(t, tx.CreateIndex(t.Context(), ci))
			}
		}
		for _, values := range rows {
			is.NotError(t, tx.Upsert(t.Context(), table.GetTable(), values), text(values))
		}
		for i, ci := range indexes {
			if !before[i] {
				is.NotError(t, tx.CreateIndex(t.Context(), ci))
			}
		}
	})
	plain := newPlain(t, table, indexes, rows)
	r, err := startRead(t, d)
	is.NotError(t, err)
	defer func() {
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
	}()
	got, err := r.Table(t.Context(), table.GetTable())
	is.NotError(t, err)
	is.Equal(t, describe(storedTable(table)), describe(got))

	var stats scanStats
	// all is the number of rows of the table.
	all := len(plainScan(t, plain, table, nil, core.IndexScan{}))
	// check one scan: by the index ci, or the primary key, with pinned columns, and bounds of the
	// kinds lower and upper, which are 0 for none, 1 for closed, and 2 for open.
	check := func(ci *s4qlpb.CreateIndex, pinned, lower, upper int, backward bool) {
		key := keyParts(table, ci)
		scan := core.IndexScan{Index: ci.GetIndex(), Backward: backward}
		// The pinned values are those of a row, so that the scan finds something, or any values.
		var from []*s4qlpb.Value
		if len(rows) > 0 && in.intn(4) > 0 {
			from = stored(rows[in.intn(len(rows))])
		}
		for i := range pinned {
			if from != nil && in.intn(8) > 0 {
				scan.Equal = append(scan.Equal, from[key[i].position])
				continue
			}
			scan.Equal = append(scan.Equal, value(key[i].position, 2))
		}
		bound := func(kind int) core.Bound {
			if kind == 0 {
				return core.Bound{}
			}
			pool := simKeys[table.GetColumns()[key[pinned].position].GetType()]
			return core.Bound{Value: pool[in.intn(len(pool))], Open: kind == 2}
		}
		scan.Lower, scan.Upper = bound(lower), bound(upper)
		what := fmt.Sprint(describe(table), describeIndexes(indexes), " ", describeScan(scan))

		read := r.RowsRead()
		got := []string{}
		for values, err := range r.ScanIndex(t.Context(), table.GetTable(), scan) {
			is.NotError(t, err, what)
			got = append(got, text(values))
		}
		want := plainScan(t, plain, table, ci, scan)
		is.EqualSlice(t, want, got, what)
		is.Equal(t, int64(len(want)), r.RowsRead()-read, what)
		sql, plan, err := r.ScanPlan(t.Context(), table.GetTable(), scan)
		is.NotError(t, err, what)
		is.EqualSlice(t, []string{wantPlan(table, ci, scan)}, plan, what+" "+sql)

		stats.scans++
		if len(got) == 0 || len(got) == all {
			return
		}
		stats.some++
		if slices.ContainsFunc(scan.Equal, isNull) {
			stats.pinnedNull++
		}
		if (lower > 0 || upper > 0) && key[pinned].descending {
			stats.descendingBound++
		}
		if len(scan.Equal)+min(lower+upper, 1) > len(ci.GetColumns()) && ci != nil {
			stats.trailing++
		}
		if backward && len(got) > 1 {
			stats.backward++
		}
		if ci != nil && len(key) == n {
			stats.covering++
		}
	}

	accesses := append([]*s4qlpb.CreateIndex{nil}, indexes...)
	if !every {
		for range 8 {
			ci := accesses[in.intn(len(accesses))]
			pinned := in.intn(len(keyParts(table, ci)) + 1)
			lower, upper := in.intn(3), in.intn(3)
			if pinned == len(keyParts(table, ci)) {
				lower, upper = 0, 0
			}
			check(ci, pinned, lower, upper, in.intn(2) == 0)
		}
		return stats
	}
	for _, ci := range accesses {
		key := keyParts(table, ci)
		for pinned := range len(key) + 1 {
			for lower := range 3 {
				for upper := range 3 {
					if pinned == len(key) && lower+upper > 0 {
						continue
					}
					for _, backward := range []bool{false, true} {
						check(ci, pinned, lower, upper, backward)
					}
				}
			}
		}
	}
	return stats
}

func TestScanIndex_generated(t *testing.T) {
	t.Run("should give, for every index, every shape of bounds, and both directions, the rows that SQLite gives from a database of its own, in the same order, by a search of exactly that index and those bounds, and count them", func(t *testing.T) {
		seeds := 400
		if testing.Short() {
			seeds = 60
		}
		var total scanStats
		for seed := range seeds {
			stats := checkIndexScans(t, &choices{rnd: rand.New(rand.NewPCG(uint64(seed), 20261120))}, true)
			total.scans += stats.scans
			total.some += stats.some
			total.pinnedNull += stats.pinnedNull
			total.descendingBound += stats.descendingBound
			total.trailing += stats.trailing
			total.backward += stats.backward
			total.covering += stats.covering
		}
		t.Logf("%+v", total)
		// The scans that tell a fault from none give some rows of the table and not all of them.
		is.True(t, total.some > total.scans/10, "too few scans give some rows and not all")
		is.True(t, total.pinnedNull > 0, "no scan pins a column to NULL and finds rows")
		is.True(t, total.descendingBound > 0, "no scan bounds a descending column and finds rows")
		is.True(t, total.trailing > 0, "no scan reaches into the primary-key columns at the end of a key and finds rows")
		is.True(t, total.backward > 0, "no scan reads more than one row backward")
		is.True(t, total.covering > 0, "no scan reads an index that holds every column of its table")
	})
}

// FuzzScanIndex runs eight scans that the input chooses, on a table, rows, and indexes that the
// input chooses, as [checkIndexScans] checks them against SQLite.
func FuzzScanIndex(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 0, 0, 1, 1, 1, 3, 1, 2, 0, 1, 2, 3, 0, 1, 1, 0, 1, 2, 2, 1, 0, 3, 1, 1, 0, 2, 1})
	f.Add([]byte{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5, 8, 9, 7, 9, 3, 2, 3, 8, 4, 6, 2, 6, 4, 3, 3, 8, 3, 2, 7, 9, 5, 0, 2, 8, 8, 4, 1, 9, 7, 1, 6, 9, 3, 9, 9, 3, 7, 5, 1, 0, 5, 8, 2, 0, 9, 7, 4, 9, 4, 4, 5, 9, 2, 3, 0, 7, 8, 1, 6, 4, 0, 6, 2, 8, 6, 2, 0, 8, 9, 9, 8, 6, 2, 8, 0, 3, 4, 8, 2, 5, 3, 4, 2, 1, 1, 7, 0, 6, 7, 9})
	f.Add([]byte{2, 0, 0, 2, 0, 4, 0, 4, 0, 12, 0, 1, 0, 2, 1, 1, 0, 3, 1, 5, 2, 0, 0, 1, 1, 2, 3, 0, 1, 1, 2, 1, 0, 1, 0, 2, 2, 1, 0, 0, 0, 0, 2, 2, 0, 1, 0, 1, 1, 0, 3, 2, 1, 1, 1, 0, 2, 0, 1, 2, 2, 1, 1, 0})
	for seed := range 6 {
		rnd := rand.New(rand.NewPCG(uint64(seed), 7))
		b := make([]byte, 160)
		for i := range b {
			b[i] = byte(rnd.IntN(256))
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		checkIndexScans(t, &choices{b: input}, false)
	})
}

// orderOf the columns at the given positions of a table, for [core.IndexScan.Order]: an int for a
// column in ascending order, and a [desc] for one in descending order.
func orderOf(columns ...any) []core.KeyColumn {
	var order []core.KeyColumn
	for _, c := range columns {
		switch c := c.(type) {
		case int:
			order = append(order, core.KeyColumn{Position: c})
		case desc:
			order = append(order, core.KeyColumn{Position: int(c), Descending: true})
		default:
			panic(fmt.Sprintf("no column of an order for a %T", c))
		}
	}
	return order
}

func TestScanIndex_order(t *testing.T) {
	// The table t has the key c0, the integer c1 and the text c2, which take NULL, and the real c3.
	// Its index i is on c1 and c2 descending, so its key is c1, c2 descending, c0, and its index j is
	// on c3 descending and c0 descending. Rows tie in each of c1, c2, and c3.
	ct := nullableTable(typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL), 1, 2)
	i, j := indexOf("i", "t", 1, desc(2)), indexOf("j", "t", desc(3), desc(0))
	tRows := [][]*s4qlpb.Value{
		row(1, nil, nil, 1.5), row(2, nil, "x", 2.5), row(3, 1, "a", 0.5), row(4, 1, "b", 3.5),
		row(5, 1, nil, 1.5), row(6, 2, "a", 1.5), row(7, 1, "b", 2.5),
	}
	// The table v has a column of each type, each of which takes NULL, with the values whose order
	// SQLite's sort decides: NULL, the ends of the integers, 0.0 and -0.0, which are equal, and texts
	// and blobs that differ in their bytes and in their length.
	cv := nullableTable(typedTable("v", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB), 1, 2, 3, 4)
	vRows := [][]*s4qlpb.Value{
		row(1, nil, nil, nil, nil),
		row(2, int64(math.MinInt64), -1e300, "", []byte{}),
		row(3, int64(math.MaxInt64), 1e300, "😀", []byte{0xff}),
		row(4, 0, 0.0, "a", []byte{0x00}),
		row(5, 0, math.Copysign(0, -1), "a", []byte{0x00}),
		row(6, -1, -0.5, "A", []byte{0x01}),
		row(7, 1, 0.5, "a b", []byte{0x01, 0x00}),
		row(8, 2, 1.5, "ab", []byte{0x7f}),
		row(9, nil, nil, nil, nil),
		row(10, 3, 2.5, "É", []byte{0x80}),
		row(11, 4, 3.5, "é", []byte{0x80, 0x00}),
	}
	// The table u has a key of two columns, and one value in its third.
	cu := tableOf("u", 2, "a", "b", "v")
	uRows := [][]*s4qlpb.Value{row(1, 1, 0), row(1, 2, 0), row(1, 3, 0), row(2, 1, 0)}

	start := []step{createWith(ct), createWith(cv), createWith(cu)}
	for _, r := range tRows {
		start = append(start, func(t *testing.T, tx *core.Tx) { is.NotError(t, tx.Upsert(t.Context(), "t", r)) })
	}
	for _, r := range vRows {
		start = append(start, func(t *testing.T, tx *core.Tx) { is.NotError(t, tx.Upsert(t.Context(), "v", r)) })
	}
	for _, r := range uRows {
		start = append(start, func(t *testing.T, tx *core.Tx) { is.NotError(t, tx.Upsert(t.Context(), "u", r)) })
	}
	start = append(start, func(t *testing.T, tx *core.Tx) {
		is.NotError(t, tx.CreateIndex(t.Context(), i))
		is.NotError(t, tx.CreateIndex(t.Context(), j))
	})
	closed := func(v any) core.Bound { return core.Bound{Value: row(v)[0]} }
	keys := func(rows []string) string {
		var ks []string
		for _, r := range rows {
			k, _, _ := strings.Cut(r, ",")
			ks = append(ks, k)
		}
		return strings.Join(ks, " ")
	}

	t.Run("should give the rows in the order of the columns, with NULL first ascending and last descending, and the rows that tie by the primary key or its reverse", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()

		for _, test := range []struct {
			name  string
			table string
			scan  core.IndexScan
			// want are the keys of the rows, in their order: c0 of t and of v.
			want string
		}{
			{name: "an integer column ascending, with NULL first, and the rows that tie by the key", table: "t", scan: core.IndexScan{Order: orderOf(1)}, want: "1 2 3 4 5 7 6"},
			{name: "an integer column ascending, and the rows that tie by the reverse of the key", table: "t", scan: core.IndexScan{Order: orderOf(1), Backward: true}, want: "2 1 7 5 4 3 6"},
			{name: "an integer column descending, with NULL last", table: "t", scan: core.IndexScan{Order: orderOf(desc(1))}, want: "6 3 4 5 7 1 2"},
			{name: "an integer column descending, and the rows that tie by the reverse of the key", table: "t", scan: core.IndexScan{Order: orderOf(desc(1)), Backward: true}, want: "6 7 5 4 3 2 1"},
			{name: "two columns, each in its direction", table: "t", scan: core.IndexScan{Order: orderOf(desc(3), 2)}, want: "4 7 2 1 5 6 3"},
			{name: "the order that the index of the scan has", table: "t", scan: core.IndexScan{Index: "i", Order: orderOf(1, desc(2))}, want: "2 1 4 7 3 5 6"},
			{name: "the reverse of the order that the index of the scan has", table: "t", scan: core.IndexScan{Index: "i", Order: orderOf(desc(1), 2), Backward: true}, want: "6 5 3 7 4 1 2"},
			{name: "the rows of a pinned column of an index, by another column", table: "t", scan: core.IndexScan{Index: "i", Equal: row(1), Order: orderOf(3)}, want: "3 5 7 4"},
			{name: "the rows within the bounds of an index, by another column", table: "t", scan: core.IndexScan{Index: "j", Lower: closed(1.5), Upper: closed(2.5), Order: orderOf(2, 1)}, want: "1 5 6 7 2"},
			{name: "the rows above a key, by the key descending", table: "t", scan: core.IndexScan{Lower: closed(3), Order: orderOf(desc(0))}, want: "7 6 5 4 3"},
			{name: "the integers, with the rows of NULL first", table: "v", scan: core.IndexScan{Order: orderOf(1)}, want: "1 9 2 6 4 5 7 8 10 11 3"},
			{name: "the reals descending, with 0.0 and -0.0 equal, and NULL last", table: "v", scan: core.IndexScan{Order: orderOf(desc(2))}, want: "3 11 10 8 7 4 5 6 2 1 9"},
			{name: "the texts by their bytes", table: "v", scan: core.IndexScan{Order: orderOf(3)}, want: "1 9 2 6 4 5 7 8 10 11 3"},
			{name: "the blobs by their bytes and then their length, descending", table: "v", scan: core.IndexScan{Order: orderOf(desc(4))}, want: "3 11 10 8 7 6 4 5 2 1 9"},
		} {
			t.Run(test.name, func(t *testing.T) {
				is.Equal(t, test.want, keys(scanBy(t, r, test.table, test.scan)))
			})
		}

		t.Run("the rows that tie in a table with a key of two columns, by the columns of the key that the order does not name", func(t *testing.T) {
			pairs := func(scan core.IndexScan) string {
				var ps []string
				for _, r := range scanBy(t, r, "u", scan) {
					parts := strings.Split(r, ",")
					ps = append(ps, parts[0]+"."+parts[1])
				}
				return strings.Join(ps, " ")
			}
			is.Equal(t, "1.1 1.2 1.3 2.1", pairs(core.IndexScan{Order: orderOf(2)}))
			is.Equal(t, "2.1 1.3 1.2 1.1", pairs(core.IndexScan{Order: orderOf(2), Backward: true}))
			is.Equal(t, "1.1 2.1 1.2 1.3", pairs(core.IndexScan{Order: orderOf(1)}))
			is.Equal(t, "2.1 1.1 1.2 1.3", pairs(core.IndexScan{Order: orderOf(1), Backward: true}))
			is.Equal(t, "1.3 1.2 1.1 2.1", pairs(core.IndexScan{Order: orderOf(2, desc(1), 0), Backward: true}))
		})
	})

	t.Run("should give the rows that SQLite gives for the same order, by every scan, forward and backward, in a read and in a write transaction, with exactly one sort in the plan of the cache where the scan has an order and none where it has not", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		type table struct {
			ct      *s4qlpb.CreateTable
			indexes []*s4qlpb.CreateIndex
			rows    [][]*s4qlpb.Value
			scans   []core.IndexScan
			orders  [][]core.KeyColumn
		}
		tables := []table{
			{
				ct: ct, indexes: []*s4qlpb.CreateIndex{i, j}, rows: tRows,
				scans: []core.IndexScan{
					{}, {Lower: closed(3)}, {Index: "i"}, {Index: "i", Equal: row(1)}, {Index: "i", Equal: row(nil)},
					{Index: "i", Equal: row(1), Lower: closed("a"), Upper: closed("b")}, {Index: "i", Equal: row(1, "b")},
					{Index: "j"}, {Index: "j", Lower: closed(1.5)}, {Index: "j", Equal: row(9.5)},
				},
				orders: [][]core.KeyColumn{
					orderOf(1), orderOf(desc(1)), orderOf(2), orderOf(desc(2)), orderOf(3), orderOf(desc(3), 1),
					orderOf(1, desc(2)), orderOf(desc(1), 2), orderOf(1, desc(2), 0), orderOf(desc(3), desc(0)),
					orderOf(0), orderOf(desc(0)), orderOf(2, desc(0)), orderOf(1, 2, 3, 0), orderOf(desc(3), desc(2), desc(1), desc(0)),
				},
			},
			{
				ct: cv, rows: vRows, scans: []core.IndexScan{{}, {Upper: closed(9)}},
				orders: [][]core.KeyColumn{
					orderOf(1), orderOf(desc(1)), orderOf(2), orderOf(desc(2)), orderOf(3), orderOf(desc(3)), orderOf(4), orderOf(desc(4)),
					orderOf(2, desc(3)), orderOf(desc(4), 1, 2, 3),
				},
			},
			{
				ct: cu, rows: uRows, scans: []core.IndexScan{{}, {Equal: row(1)}, {Equal: row(1), Lower: closed(2)}},
				orders: [][]core.KeyColumn{orderOf(2), orderOf(1), orderOf(desc(1)), orderOf(desc(0)), orderOf(1, 0), orderOf(desc(1), desc(0)), orderOf(2, 1, 0)},
			},
		}

		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, tx.Rollback(t.Context())) }()

		checked := 0
		for _, table := range tables {
			plain := newPlain(t, table.ct, table.indexes, table.rows)
			name := table.ct.GetTable()
			for _, scan := range table.scans {
				var ci *s4qlpb.CreateIndex
				for _, ix := range table.indexes {
					if ix.GetIndex() == scan.Index {
						ci = ix
					}
				}
				for _, order := range table.orders {
					for _, backward := range []bool{false, true} {
						scan.Order, scan.Backward = order, backward
						what := fmt.Sprintf("%v order %v", describeScan(scan), order)
						want := plainScan(t, plain, table.ct, ci, scan)
						is.EqualSlice(t, want, scanBy(t, r, name, scan), what)
						is.EqualSlice(t, want, scanBy(t, tx, name, scan), what)

						sql, plan, err := r.ScanPlan(t.Context(), name, scan)
						is.NotError(t, err)
						// SQLite's plan has one row for a sort of all the rows of the statement by its ORDER BY.
						is.EqualSlice(t, []string{wantPlan(table.ct, ci, scan), "USE TEMP B-TREE FOR ORDER BY"}, plan, sql)
						// The same scan without the order reads the same index and sorts nothing.
						scan.Order = nil
						sql, plan, err = r.ScanPlan(t.Context(), name, scan)
						is.NotError(t, err)
						is.EqualSlice(t, []string{wantPlan(table.ct, ci, scan)}, plan, sql)
						checked++
					}
				}
			}
		}
		is.Equal(t, 2*(10*15+2*10+3*7), checked)
	})

	t.Run("should select each column of the order as the term that it orders by, and sort nothing for a scan that pins the whole primary key", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()

		// SQLite keeps the value of a selected column that is also a term of the ORDER BY once in what
		// it sorts, so the statement selects the term, and the key after the terms breaks the ties.
		sql, plan, err := r.ScanPlan(t.Context(), "t", core.IndexScan{Index: "i", Equal: row(1), Order: orderOf(desc(3), 2), Backward: true})
		is.NotError(t, err)
		is.Equal(t, `select "c0", "c1", +"c2", +"c3" from main."t" indexed by "i" where "c1" = ?1 order by +"c3" desc, +"c2", "c0" desc`, sql)
		is.EqualSlice(t, []string{"SEARCH main.t USING INDEX i (c1=?)", "USE TEMP B-TREE FOR ORDER BY"}, plan)

		// One row at most has a whole primary key, and SQLite does not sort it.
		for _, test := range []struct {
			table string
			scan  core.IndexScan
			want  string
		}{
			{table: "t", scan: core.IndexScan{Equal: row(3), Order: orderOf(1)}, want: "SEARCH main.t USING PRIMARY KEY (c0=?)"},
			{table: "u", scan: core.IndexScan{Equal: row(1, 2), Order: orderOf(desc(2))}, want: "SEARCH main.u USING PRIMARY KEY (a=? AND b=?)"},
		} {
			sql, plan, err := r.ScanPlan(t.Context(), test.table, test.scan)
			is.NotError(t, err)
			is.EqualSlice(t, []string{test.want}, plan, sql)
			before := r.RowsRead()
			is.Equal(t, 1, len(scanBy(t, r, test.table, test.scan)))
			is.Equal(t, int64(1), r.RowsRead()-before)
		}
	})

	t.Run("should count every row within the bounds of a scan with an order, however early the loop over it stops", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)

		for _, read := range []bool{true, false} {
			var r interface {
				ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
				RowsRead() int64
			}
			// rows is the number of rows of t as the transaction sees it.
			rows := int64(7)
			if read {
				rtx, err := startRead(t, d)
				is.NotError(t, err)
				defer func() { is.NotError(t, rtx.Close(t.Context())) }()
				r = rtx
			} else {
				tx, err := d.Begin(t.Context())
				is.NotError(t, err)
				defer func() { is.NotError(t, tx.Rollback(t.Context())) }()
				// The transaction reads its own row.
				upsert("t", 8, 1, "c", 4.5)(t, tx)
				r, rows = tx, 8
			}
			// stop the loop over the scan after it has given the number of rows, and give the rows that the
			// transaction read for it.
			stop := func(scan core.IndexScan, after int) int64 {
				before, seen := r.RowsRead(), 0
				for _, err := range r.ScanIndex(t.Context(), "t", scan) {
					is.NotError(t, err)
					if seen++; seen == after {
						break
					}
				}
				return r.RowsRead() - before
			}
			sorted := core.IndexScan{Order: orderOf(desc(3))}
			// A loop to the end counts each row once, and so does one that stops at the last row.
			is.Equal(t, rows, stop(sorted, 0))
			is.Equal(t, rows, stop(sorted, int(rows)))
			// A loop that stops at the first row has made the cache read every row, to sort them.
			is.Equal(t, rows, stop(sorted, 1))
			is.Equal(t, rows, stop(sorted, 3))
			sorted.Backward = true
			is.Equal(t, rows, stop(sorted, 2))
			// The same loop over the scan without the order reads the row that it stops at.
			is.Equal(t, int64(1), stop(core.IndexScan{}, 1))
			// The rows within the bounds are counted, and no others: those with 1 in the first column of
			// the index, of which the table has four and the write transaction a fifth, those of them
			// between two bounds, and none.
			is.Equal(t, rows-3, stop(core.IndexScan{Index: "i", Equal: row(1), Order: orderOf(3)}, 1))
			is.Equal(t, int64(3), stop(core.IndexScan{Index: "i", Equal: row(1), Lower: closed("a"), Upper: closed("b"), Order: orderOf(3)}, 1))
			is.Equal(t, int64(2), stop(core.IndexScan{Index: "i", Equal: row(nil), Order: orderOf(desc(0))}, 1))
			is.Equal(t, int64(0), stop(core.IndexScan{Index: "i", Equal: row(9), Order: orderOf(3)}, 1))
			is.Equal(t, int64(0), stop(core.IndexScan{Index: "i", Equal: row(9), Order: orderOf(3)}, 0))
		}
	})

	t.Run("should sort a table of the most columns that the cache holds by every column, its key among them", func(t *testing.T) {
		// SQLite takes no more terms in an ORDER BY than a table can have columns, 2000, so the
		// statement of the sort names no column twice.
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		wide := tableOf("w", 1, manyColumns(2000)...)
		first, second := make([]any, 2000), make([]any, 2000)
		for i := range first {
			first[i], second[i] = 0, 0
		}
		first[0], first[1], second[0], second[1] = 1, 2, 2, 1
		transact(t, d, createWith(wide), func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.Upsert(t.Context(), "w", row(first...)))
			is.NotError(t, tx.Upsert(t.Context(), "w", row(second...)))
		})
		var order []any
		for i := 1; i < 2000; i++ {
			order = append(order, i)
		}
		order = append(order, desc(0))
		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()
		var got []int64
		for row, err := range r.ScanIndex(t.Context(), "w", core.IndexScan{Order: orderOf(order...)}) {
			is.NotError(t, err)
			got = append(got, row[0].GetInteger())
		}
		is.EqualSlice(t, []int64{2, 1}, got)
	})

	t.Run("should refuse an order of a column that the table does not have, and of more columns than it has", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, start...)
		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, tx.Rollback(t.Context())) }()

		for _, test := range []struct {
			order []core.KeyColumn
			why   string
		}{
			{order: orderOf(4), why: `a scan in the order of the column at 4 of the table "t", which has 4 columns`},
			{order: orderOf(1, -1), why: `a scan in the order of the column at -1 of the table "t", which has 4 columns`},
			{order: orderOf(0, 1, 2, 3, 0), why: `a scan in the order of 5 columns of the table "t", which has 4`},
		} {
			for _, s := range []interface {
				ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
			}{r, tx} {
				n := 0
				for row, err := range s.ScanIndex(t.Context(), "t", core.IndexScan{Order: test.order}) {
					n++
					is.True(t, row == nil)
					is.Error(t, core.ErrInvalid, err)
					is.True(t, strings.HasSuffix(err.Error(), test.why), err.Error())
				}
				is.Equal(t, 1, n)
			}
			_, _, err := r.ScanPlan(t.Context(), "t", core.IndexScan{Order: test.order})
			is.Error(t, core.ErrInvalid, err)
			is.True(t, strings.HasSuffix(err.Error(), test.why), err.Error())
		}
		is.Equal(t, int64(0), r.RowsRead())
		is.Equal(t, int64(0), tx.RowsRead())
	})

	t.Run("should sort every row that the table holds under a length limit, also the longest, by any of its columns", func(t *testing.T) {
		const limit = 1000
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: limit})
		long := typedTable("long", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_REAL)
		transact(t, d, createWith(long), upsert("long", 1, strings.Repeat("a", 300), strings.Repeat("b", 600), 2.0, 3.0))
		// The longest row that the table takes, with two reals that the table stores as integers of one
		// byte and a sort as reals of eight.
		longest := 0
		for n := limit; n > 0 && longest == 0; n-- {
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			if err := tx.Upsert(t.Context(), "long", row(2, "", strings.Repeat("c", n), 4.0, 5.0)); err != nil {
				is.Error(t, core.ErrTooBig, err)
				is.NotError(t, tx.Rollback(t.Context()))
				continue
			}
			is.NotError(t, tx.Commit(t.Context()))
			longest = n
		}
		is.True(t, longest > limit-20, fmt.Sprint("the longest text of a row is ", longest, " bytes"))

		r, err := startRead(t, d)
		is.NotError(t, err)
		defer func() { is.NotError(t, r.Close(t.Context())) }()
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		defer func() { is.NotError(t, tx.Rollback(t.Context())) }()
		for _, order := range [][]core.KeyColumn{
			orderOf(1), orderOf(2), orderOf(desc(2)), orderOf(3), orderOf(desc(4), 3), orderOf(3, 2, 1), orderOf(1, 2, 3, 4, 0), orderOf(0),
		} {
			for _, backward := range []bool{false, true} {
				scan := core.IndexScan{Order: order, Backward: backward}
				is.Equal(t, 2, len(scanBy(t, r, "long", scan)), fmt.Sprint(order))
				is.Equal(t, 2, len(scanBy(t, tx, "long", scan)), fmt.Sprint(order))
			}
		}
	})

	t.Run("should sort more rows than SQLite holds in memory in temporary files of the cache, and fail as a local failure when it cannot write them", func(t *testing.T) {
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		w := newWorld(t)
		o := core.Options{}
		o.VFS = vfs.Name()
		d := w.serve(t, "a", o)
		// 2,000 rows of 4,000 bytes each are 8 MB, and SQLite sorts up to 2 MB, the size of its page
		// cache, in memory. The column c1 orders them otherwise than their key.
		const rows = 2000
		big := nullableTable(typedTable("big", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_BLOB))
		body := make([]byte, 4000)
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), big))
			is.NotError(t, tx.CreateTable(t.Context(), typedTable("small", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT)))
			for k := range rows {
				is.NotError(t, tx.Upsert(t.Context(), "big", row(k, (k*7919)%rows, body)))
			}
			for k := range 10 {
				is.NotError(t, tx.Upsert(t.Context(), "small", row(k, -k)))
			}
		})
		r, err := startRead(t, d)
		is.NotError(t, err)

		// 7919 is a prime that does not divide 2000, so each value of c1 from 0 to 1999 is in one row.
		n := 0
		for row, err := range r.ScanIndex(t.Context(), "big", core.IndexScan{Order: orderOf(desc(1))}) {
			is.NotError(t, err)
			is.Equal(t, int64(rows-1-n), row[1].GetInteger())
			is.Equal(t, len(body), len(row[2].GetBlob()))
			n++
		}
		is.Equal(t, rows, n)
		is.Equal(t, int64(rows), r.RowsRead())

		// With every write of a temporary file failing, a sort of few rows, which SQLite holds in
		// memory, still succeeds, and the sort of the many fails.
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.Temp})
		is.Equal(t, 10, len(scanBy(t, r, "small", core.IndexScan{Order: orderOf(1)})))
		is.Equal(t, 0, vfs.Failures())
		n = 0
		for row, err := range r.ScanIndex(t.Context(), "big", core.IndexScan{Order: orderOf(desc(1))}) {
			n++
			is.True(t, row == nil)
			is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
		}
		is.Equal(t, 1, n)
		is.True(t, vfs.Failures() > 0, "no write of a temporary file failed")
		vfs.Fail(faultvfs.Fault{})
		_ = r.Close(t.Context())
		_ = d.Close(t.Context())
		is.NotError(t, vfs.Close())
	})
}

// bigRows is the number of rows of the table of [fillBig].
const bigRows = 10_000

// bigSorted is a scan of the table of [fillBig] that SQLite sorts in its first step.
var bigSorted = core.IndexScan{Order: orderOf(desc(1))}

// fillBig creates the table big in d, with [bigRows] rows whose third column is body. SQLite sorts
// its 10,000 rows in about 100,000 instructions and counts them in about 40,000, and its column c1
// orders them otherwise than their key: 7919 is a prime that does not divide 10,000, so each value
// from 0 to 9,999 is in one row.
func fillBig(t *testing.T, d *core.DB, body string) {
	t.Helper()
	big := typedTable("big", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_TEXT)
	write(t, d, func(tx *core.Tx) {
		is.NotError(t, tx.CreateTable(t.Context(), big))
		for k := range bigRows {
			is.NotError(t, tx.Upsert(t.Context(), "big", row(k, (k*7919)%bigRows, body, "")))
		}
	})
}

// bigScanner is a read transaction or a write transaction that scans the table of [fillBig].
type bigScanner interface {
	ScanIndex(ctx context.Context, table string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
	RowsRead() int64
}

// beginBig begins a read transaction, or a write transaction that has written a row of its own to
// the table of [fillBig], and gives it with the number of rows of the table that it sees.
func beginBig(t *testing.T, d *core.DB, read bool) (bigScanner, *core.Tx, int) {
	t.Helper()
	if read {
		r, err := startRead(t, d)
		is.NotError(t, err)
		t.Cleanup(func() { _ = r.Close(t.Context()) })
		return r, nil, bigRows
	}
	tx, err := d.Begin(t.Context())
	is.NotError(t, err)
	t.Cleanup(func() { _ = tx.Rollback(t.Context()) })
	is.NotError(t, tx.Upsert(t.Context(), "big", row(bigRows, -1, "tx", "")))
	return tx, tx, bigRows + 1
}

// TestScanIndex_context ends the context of a scan with an order between its rows, which holds in
// every build of SQLite's binding. The tests of a context that ends within a step of SQLite are in
// index_cgo_test.go, for the binding whose [sqlite.Stmt.StepContext] stops a step.
func TestScanIndex_context(t *testing.T) {
	for _, read := range []bool{true, false} {
		t.Run(fmt.Sprint("should give every row of a scan, sorted or not, whose context ends after its first row, in a read transaction ", read), func(t *testing.T) {
			// SQLite counts the instructions of a statement over all its steps, so the steps after the
			// first, which run to their end, run past the instructions at which it would look at the
			// context.
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			fillBig(t, d, "x")
			s, _, total := beginBig(t, d, read)
			for _, scan := range []core.IndexScan{{}, bigSorted} {
				ctx, cancel := context.WithCancel(t.Context())
				n := 0
				for _, err := range s.ScanIndex(ctx, "big", scan) {
					is.NotError(t, err)
					n++
					cancel()
				}
				cancel()
				is.Equal(t, total, n, describeScan(scan))
			}
		})

		t.Run(fmt.Sprint("should not count the rows that a sort read and did not give when the context has ended before the loop over it stops, in a read transaction ", read), func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			fillBig(t, d, "x")
			s, tx, _ := beginBig(t, d, read)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			before := s.RowsRead()
			for _, err := range s.ScanIndex(ctx, "big", bigSorted) {
				is.NotError(t, err)
				cancel()
				break
			}
			is.Equal(t, int64(1), s.RowsRead()-before)
			if tx != nil {
				is.NotError(t, tx.Upsert(t.Context(), "big", row(bigRows+1, -2, "tx", "")))
				is.NotError(t, tx.Commit(t.Context()))
			}
		})

		t.Run(fmt.Sprint("should not count the rows of a sort of a few rows, which SQLite would count before it looks at the context, when the context has ended before the loop over it stops, in a read transaction ", read), func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, createWith(tableOf("few", 1, "k", "v")), upsert("few", 1, 3), upsert("few", 2, 2), upsert("few", 3, 1))
			var s bigScanner
			if read {
				r, err := startRead(t, d)
				is.NotError(t, err)
				defer func() { _ = r.Close(t.Context()) }()
				s = r
			} else {
				tx, err := d.Begin(t.Context())
				is.NotError(t, err)
				defer func() { _ = tx.Rollback(t.Context()) }()
				s = tx
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			before := s.RowsRead()
			for _, err := range s.ScanIndex(ctx, "few", core.IndexScan{Order: orderOf(1)}) {
				is.NotError(t, err)
				cancel()
				break
			}
			is.Equal(t, int64(1), s.RowsRead()-before)
		})

		t.Run(fmt.Sprint("should stop the instance as for a local failure, and keep the cache what the log holds, when a read of the cache fails in a step whose context ends at the last look of SQLite in it, in a read transaction ", read), func(t *testing.T) {
			// SQLite looks at the context once more on its way out of a step that failed, and a stop
			// there would turn the I/O error, which has rolled back the whole transaction of the cache,
			// into the error of the context (adversarial review of 2026-11-26, agy 13.1). A progress
			// period of 1 makes SQLite look there in every step.
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			w := newWorld(t)
			o := core.Options{VFS: vfs.Name()}
			o.ProgressPeriod(1)
			d := w.serve(t, "a", o)
			transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 10), create("t2", 1, "k", "v"), upsert("t2", 1, 1))
			// A new instance has new connections, which have read no page of t2 yet.
			is.NotError(t, d.Close(t.Context()))
			d = w.serve(t, "a", o)
			var s interface {
				Table(ctx context.Context, name string) (*s4qlpb.CreateTable, error)
				ScanRange(ctx context.Context, name string, lower, upper core.Bound) iter.Seq2[[]*s4qlpb.Value, error]
			}
			var tx *core.Tx
			if read {
				r, err := startRead(t, d)
				is.NotError(t, err)
				defer func() { _ = r.Close(t.Context()) }()
				s = r
			} else {
				tx, err = d.Begin(t.Context())
				is.NotError(t, err)
				defer func() { _ = tx.Rollback(t.Context()) }()
				is.NotError(t, tx.Upsert(t.Context(), "t1", row(2, 20)))
				s = tx
			}
			_, err = s.Table(t.Context(), "t2")
			is.NotError(t, err)

			vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB | faultvfs.WAL})
			n := 0
			for row, err := range s.ScanRange(newFailedContext(t.Context(), vfs), "t2", core.Bound{}, core.Bound{}) {
				n++
				is.True(t, row == nil)
				is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
				is.True(t, !errors.Is(err, context.Canceled), fmt.Sprint(err))
			}
			vfs.Fail(faultvfs.Fault{})
			is.Equal(t, 1, n)
			is.True(t, vfs.Failures() > 0, "no read of the cache failed")
			is.True(t, isDone(d), "the instance stopped")
			if tx != nil {
				// No later write of the transaction reaches the cache, and nothing commits.
				err := tx.Upsert(t.Context(), "t1", row(3, 30))
				is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
				is.True(t, tx.Commit(t.Context()) != nil)
			}
			_ = d.Close(t.Context())

			// The cache, opened again, holds what the log holds, as a new cache that replays it does.
			committed := "t1(k*0,v): 1,10\nt2(k*0,v): 1,1"
			d = w.serve(t, "a", core.Options{})
			is.Equal(t, committed, served(t, d))
			is.NotError(t, d.Close(t.Context()))
			is.Equal(t, committed, served(t, w.serve(t, "b", core.Options{})))
			is.NotError(t, vfs.Close())
		})

		t.Run(fmt.Sprint("should stop the instance as for a local failure, count none of the rows, and fail a write transaction, when the count of the rows that a sort read and did not give cannot read the cache, in a read transaction ", read), func(t *testing.T) {
			vfs, err := faultvfs.New()
			is.NotError(t, err)
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			// The rows are larger than the page cache, so the count reads pages of the cache again.
			fillBig(t, d, strings.Repeat("x", 400))
			is.NotError(t, d.Close(t.Context()))
			o := core.Options{}
			o.VFS = vfs.Name()
			d = w.serve(t, "a", o)
			s, tx, _ := beginBig(t, d, read)

			before := s.RowsRead()
			for _, err := range s.ScanIndex(t.Context(), "big", bigSorted) {
				is.NotError(t, err)
				vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB | faultvfs.WAL})
				break
			}
			vfs.Fail(faultvfs.Fault{})
			is.True(t, vfs.Failures() > 0, "no read of the cache failed")
			is.Equal(t, int64(1), s.RowsRead()-before)
			is.True(t, isDone(d), "the instance stopped")
			if tx != nil {
				err := tx.Upsert(t.Context(), "big", row(bigRows+1, -2, "tx", ""))
				is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
				is.True(t, strings.Contains(err.Error(), "can only be rolled back"), err.Error())
				_ = tx.Rollback(t.Context())
			}
			_ = d.Close(t.Context())
			is.NotError(t, vfs.Close())
		})
	}
}

// failedContext ends at the first call of its Err after a file operation of its VFS has failed,
// which with a progress period of 1 is the look of SQLite at the context on its way out of the step
// whose read failed.
type failedContext struct {
	context.Context
	vfs  *faultvfs.VFS
	once sync.Once
	done chan struct{}
}

func newFailedContext(parent context.Context, vfs *faultvfs.VFS) *failedContext {
	return &failedContext{Context: parent, vfs: vfs, done: make(chan struct{})}
}

func (c *failedContext) Done() <-chan struct{} {
	return c.done
}

func (c *failedContext) Err() error {
	if c.vfs.Failures() == 0 {
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

// served is [read], for a test whose variable read hides it.
func served(t *testing.T, d *core.DB) string {
	t.Helper()
	return read(t, d)
}

// indexFault is a transaction of the fault tests of indexes, on top of [indexFaultBase].
type indexFault struct {
	name  string
	steps []step
}

// indexFaults are the transactions of the fault tests of indexes: one that creates an index, one
// that drops one, one that does both, one that creates a table and an index on it, and one that
// changes rows beside them, with a row long enough that the spool file of the commit object is
// written before its end, and an annotation, which has a spool file of its own.
func indexFaults() []indexFault {
	// The spool file writes through a buffer of 64 KiB.
	long := strings.Repeat("x", 70<<10)
	return []indexFault{
		{name: "create index", steps: []step{createIndex("i1", "t", 2, desc(1))}},
		{name: "drop index", steps: []step{dropIndex("i0")}},
		{name: "drop and create index", steps: []step{dropIndex("i0"), createIndex("i1", "t", 2), createIndex("i0", "t", desc(1))}},
		{name: "create table and index", steps: []step{create("u", 1, "k", "v"), upsert("u", 1, 10), upsert("u", 2, 20), createIndex("iu", "u", 1)}},
		{name: "rows beside indexes", steps: []step{
			upsert("t", 100, 7, long), upsert("t", 3, 30, "y"), remove("t", true, 4), dropIndex("i0"), createIndex("i1", "t", desc(1)), note("insert"),
		}},
	}
}

// indexFaultBase is the state that the fault tests of indexes start from: the table t with rows
// and its index i0, and tables beside it whose definitions make the schema of the cache take more
// than its first page, so that a lookup by name reads pages of the schema from the file.
func indexFaultBase() []step {
	ct := s4qlpb.CreateTable_builder{Table: new("t"), PrimaryKey: []uint32{0}, Columns: []*s4qlpb.Column{
		s4qlpb.Column_builder{Name: new("k"), Type: new(s4qlpb.Type_TYPE_INT)}.Build(),
		s4qlpb.Column_builder{Name: new("v"), Type: new(s4qlpb.Type_TYPE_INT)}.Build(),
		s4qlpb.Column_builder{Name: new("w"), Type: new(s4qlpb.Type_TYPE_TEXT)}.Build(),
	}}.Build()
	steps := []step{createWith(ct)}
	for i := range 10 {
		steps = append(steps, upsert("t", i, i%3, fmt.Sprint("w", i)))
	}
	steps = append(steps, createIndex("i0", "t", 1))
	for i := range 8 {
		steps = append(steps, create(fmt.Sprint("pad", i), 1, manyColumns(30)...))
	}
	return steps
}

// shrinkPageCache of the cache at path to the fewest pages that SQLite takes, as the default of
// every connection that opens it later, so that SQLite reads most pages from the file each time it
// needs them, and a fault of a read can land in nearly every read of the cache. The official build
// of SQLite in a browser does not have the PRAGMA, and its connections keep their page cache, so a
// commit there reads nothing from the file. It reports whether the cache keeps the size.
func shrinkPageCache(t *testing.T, path string) bool {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	is.NotError(t, c.Exec("pragma default_cache_size = 1"))
	s, err := c.Prepare("pragma default_cache_size")
	is.NotError(t, err)
	ok, err := s.Step()
	is.NotError(t, err)
	kept := ok && s.ColumnInt64(0) == 1
	is.NotError(t, s.Finalize())
	is.NotError(t, c.Close())
	return kept
}

// faultStep is the distance between two positions of a fault in the fault tests of indexes: each
// position, and under -short every fourth from the first on. A test goes on until a fault lands
// nowhere, so either way it ends past the last read or write.
func faultStep() int {
	if testing.Short() {
		return 4
	}
	return 1
}

// TestTx_Commit_indexFaults fails each read of the cache, and each write of a spool file, in turn,
// at the commit of transactions that create and drop indexes, one fault in each run. A commit then
// fails with a local error, not as a cache of another version, and publishes nothing, or it is
// published as it is without a fault, and a read that failed stops the instance. Either way the
// cache, opened again, and a new cache that replays the log hold what the log holds.
func TestTx_Commit_indexFaults(t *testing.T) {
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	defer func() { is.NotError(t, vfs.Close()) }()

	for _, test := range indexFaults() {
		t.Run(test.name, func(t *testing.T) {
			// The transaction without a fault gives the commit object and the state that a commit
			// that succeeds must give.
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, indexFaultBase()...)
			before := read(t, d)
			baseLog := w.commits(t)
			transact(t, d, test.steps...)
			after := read(t, d)
			want := w.commits(t)[len(baseLog)]
			is.NotError(t, d.Close(t.Context()))

			for _, kind := range []string{"read", "spool write"} {
				runs, failed, shrunk := 0, 0, false
				for at := 0; ; at += faultStep() {
					what := fmt.Sprint(kind, " ", at)
					if at == 100000 {
						t.Fatal("every commit fails: " + what)
					}
					w := newWorld(t)
					d := w.serve(t, "a", core.Options{})
					transact(t, d, indexFaultBase()...)
					is.NotError(t, d.Close(t.Context()))
					path := filepath.Join(w.dir, "a.db")
					shrunk = shrinkPageCache(t, path)
					head := w.manifest(t).Head

					o := core.Options{}
					o.VFS = vfs.Name()
					spools := failSpools(&o)
					d = w.serve(t, "a", o)
					tx, err := d.Begin(t.Context())
					is.NotError(t, err)
					for _, s := range test.steps {
						s(t, tx)
					}
					faultsBefore := vfs.Failures() + spools.failures()
					if kind == "read" {
						vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.AnyFile, After: at, Times: 1})
					} else {
						spools.failWrite(at)
					}
					err = tx.Commit(t.Context())
					vfs.Fail(faultvfs.Fault{})
					spools.failWrite(-1)
					faulted := vfs.Failures()+spools.failures() > faultsBefore
					stopped := isDone(d)
					_ = d.Close(t.Context())
					runs++

					published := w.manifest(t).Head != head
					if err != nil {
						failed++
						is.True(t, faulted, "the commit failed, and nothing failed: "+what+": "+err.Error())
						is.True(t, !published, "the commit failed, and is in the log: "+what)
						is.Error(t, core.ErrLocal, err, what)
						is.True(t, !errors.Is(err, core.ErrCacheVersion), "a failed read or write read as a cache of another version: "+what+": "+err.Error())
					} else {
						is.True(t, published, "the commit succeeded, and is not in the log: "+what)
						is.True(t, !faulted || stopped, "the commit succeeded, and the instance goes on after a failure: "+what)
						log := w.commits(t)
						is.Equal(t, len(baseLog)+1, len(log), what)
						is.EqualSlice(t, want, log[len(baseLog)], what)
					}
					state := before
					if published {
						state = after
					}
					// The cache, opened again, and a new cache, which replays the whole log, hold what the
					// log holds.
					again := w.serve(t, "a", core.Options{})
					is.Equal(t, state, read(t, again), what)
					is.NotError(t, again.Close(t.Context()))
					is.Equal(t, state, cacheState(t, path), what)
					fresh := w.serve(t, "fresh", core.Options{})
					is.Equal(t, state, read(t, fresh), what)
					is.NotError(t, fresh.Close(t.Context()))

					if !faulted {
						break
					}
				}
				t.Log(kind, ": ", runs, " runs, ", failed, " failed")
				// Without the small page cache, as in a browser, a commit reads nothing from the file.
				is.True(t, failed > 0 || (kind == "read" && !shrunk), "no commit failed at a "+kind)
			}
		})
	}
}

// TestReplay_indexFaults fails each read of the cache in turn while an instance opens and replays a
// commit object that creates and drops indexes, one fault in each run. The replay then fails with a
// local error, not as a broken log or a cache of another version, or it succeeds without a failed
// read, and the cache, opened again, holds the log.
func TestReplay_indexFaults(t *testing.T) {
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	defer func() { is.NotError(t, vfs.Close()) }()

	for _, test := range indexFaults() {
		t.Run(test.name, func(t *testing.T) {
			// The instance b is caught up with the base, and the instance a then commits the
			// transaction, which b replays when it opens again.
			w := newWorld(t)
			a := w.serve(t, "a", core.Options{})
			transact(t, a, indexFaultBase()...)
			is.NotError(t, a.Close(t.Context()))
			b := w.serve(t, "b", core.Options{})
			is.NotError(t, b.Close(t.Context()))
			a = w.serve(t, "a", core.Options{})
			transact(t, a, test.steps...)
			want := read(t, a)
			is.NotError(t, a.Close(t.Context()))
			head := w.manifest(t).Head
			_ = shrinkPageCache(t, filepath.Join(w.dir, "b.db"))
			data, err := os.ReadFile(filepath.Join(w.dir, "b.db"))
			is.NotError(t, err)
			o := core.Options{}
			o.VFS = vfs.Name()

			// Each open gets a copy of the cache of b, and the read that fails is the one after the
			// reads that succeed, until no read of the open and of the replay fails.
			runs, failed := 0, 0
			for at := 0; ; at += faultStep() {
				what := fmt.Sprint("read ", at)
				if at == 100000 {
					t.Fatal("every replay fails: " + what)
				}
				path := filepath.Join(w.dir, fmt.Sprint("b-", at, ".db"))
				is.NotError(t, os.WriteFile(path, data, 0o600))
				before := vfs.Failures()
				vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.AnyFile, After: at, Times: 1})
				d, err := core.Open(t.Context(), w.host, w.client(), path, "db", o)
				if err == nil {
					err = d.Wait(t.Context())
				}
				vfs.Fail(faultvfs.Fault{})
				faulted := vfs.Failures() > before
				if d != nil {
					_ = d.Close(t.Context())
				}
				runs++

				if err != nil {
					failed++
					is.True(t, faulted, "the replay failed, and no read failed: "+what+": "+err.Error())
					is.Error(t, core.ErrLocal, err, what)
					for _, other := range []error{core.ErrLogBroken, core.ErrCacheVersion, core.ErrCacheNotInLog} {
						is.True(t, !errors.Is(err, other), "a failed read read as a fault of the log or of the cache: "+what+": "+err.Error())
					}
				} else {
					is.True(t, !faulted, "the replay succeeded, and a read failed: "+what)
				}
				// The log is as it was, and the cache, opened again, holds it.
				is.Equal(t, head, w.manifest(t).Head, what)
				d, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
				is.NotError(t, err, what)
				is.NotError(t, d.Wait(t.Context()), what)
				is.Equal(t, want, read(t, d), what)
				is.NotError(t, d.Close(t.Context()), what)
				is.Equal(t, want, cacheState(t, path), what)
				is.NotError(t, os.Remove(path))

				if !faulted {
					break
				}
			}
			t.Log(runs, " runs, ", failed, " failed")
			is.True(t, failed > 0, "no replay failed")
		})
	}
}
