package core

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

func TestTable_checkIndexed(t *testing.T) {
	t.Run("should refuse to read the indexes of a table, to scan it, and to write a row of it before its indexes are read", func(t *testing.T) {
		// A table as a look-up without its indexes gives it. Each call refuses it before it uses its
		// connection or its transaction, which are empty here.
		tb := newTable(s4qlpb.CreateTable_builder{
			Table:      new("t1"),
			Columns:    []*s4qlpb.Column{s4qlpb.Column_builder{Name: new("k"), Type: new(s4qlpb.Type_TYPE_INT)}.Build()},
			PrimaryKey: []uint32{0},
		}.Build())
		key := []*s4qlpb.Value{s4qlpb.Value_builder{Integer: new(int64(1))}.Build()}
		c := &conn{}

		for name, call := range map[string]func() error{
			"index": func() error {
				_, err := tb.index("i")
				return err
			},
			"create indexes": func() error {
				_, err := tb.createIndexes()
				return err
			},
			"forget indexes": func() error {
				return (&Tx{}).forgetIndexes(tb)
			},
			"scan statement": func() error {
				_, _, err := tb.scanSQL(nil, IndexScan{}, bugs{})
				return err
			},
			"scan": func() error {
				for _, err := range c.scan(t.Context(), tb) {
					return err
				}
				return nil
			},
			"upsert": func() error {
				return c.upsert(tb, key)
			},
			"insert": func() error {
				_, err := c.insert(tb, key)
				return err
			},
			"delete": func() error {
				_, err := c.delete(tb, key)
				return err
			},
		} {
			err := call()
			is.Error(t, ErrCacheVersion, err, name)
			is.Equal(t, `the cache is not one that this version of s4ql reads: the indexes of its table "t1" are used before they are read`, err.Error(), name)
		}

		// The same table with its indexes read, of which it has none.
		tb.primary, tb.indexed = "sqlite_autoindex_t1_1", true
		ix, err := tb.index("i")
		is.NotError(t, err)
		is.True(t, ix == nil)
		cis, err := tb.createIndexes()
		is.NotError(t, err)
		is.Equal(t, 0, len(cis))
		sql, _, err := tb.scanSQL(nil, IndexScan{}, bugs{})
		is.NotError(t, err)
		is.Equal(t, `select * from main."t1" indexed by "sqlite_autoindex_t1_1" order by "k"`, sql)
	})
}

// stepSort opens a connection of the cache through a VFS of faultvfs, with a length limit of 1000
// bytes and a table of 5,000 rows, and gives a function that runs the step of a sort of the table with
// ctx while the VFS fails as fault, checks that the limit of the connection is back after it, and
// gives the error of the step.
func stepSort(t *testing.T) func(ctx context.Context, fault faultvfs.Fault) error {
	t.Helper()
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, vfs.Close()) })
	c, err := openConn(sqlite.OpenVFS, filepath.Join(t.TempDir(), "cache.db"), Options{VFS: vfs.Name(), BusyTimeout: time.Second, LengthLimit: 1000})
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, c.close()) })
	// 5,000 rows of 900 bytes each are 4.5 MB, and SQLite sorts up to 2 MB, the size of its page
	// cache, in memory, and the rest in temporary files.
	is.NotError(t, c.exec("create table t (k int primary key, v int, b blob) strict, without rowid"))
	is.NotError(t, c.exec("with recursive c (x) as (select 0 union all select x + 1 from c where x < 4999) insert into t select x, (x * 7919) % 5000, randomblob(900) from c"))
	// tooLong makes a blob that is longer than the limit of the connection, and shorter than
	// SQLite's largest limit.
	tooLong := func() error {
		return c.query("select zeroblob(2000)", func(*sqlite.Stmt) error { return nil })
	}
	is.True(t, isTooBig(tooLong()))

	return func(ctx context.Context, fault faultvfs.Fault) error {
		t.Helper()
		s, err := c.prepare("select k, +v, b from t order by +v")
		is.NotError(t, err)
		vfs.Fail(fault)
		_, err = c.step(ctx, s, true)
		vfs.Fail(faultvfs.Fault{})
		_ = s.Finalize()
		is.True(t, isTooBig(tooLong()), "the limit of the connection is back")
		return err
	}
}

func TestConn_step(t *testing.T) {
	t.Run("should put the length limit of the connection back after the step of a sort, when it gives a row, and when SQLite cannot write the files of the sort", func(t *testing.T) {
		step := stepSort(t)
		is.NotError(t, step(t.Context(), faultvfs.Fault{}))
		err := step(t.Context(), faultvfs.Fault{Ops: faultvfs.Write, Files: faultvfs.Temp})
		is.True(t, classify(err) == kindLocal, fmt.Sprint(err))
	})
}
