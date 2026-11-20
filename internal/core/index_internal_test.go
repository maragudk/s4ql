package core

import (
	"context"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// ScanPlan is the statement that [ReadTx.ScanIndex] runs for the scan, and SQLite's plan for it on
// the connection of the transaction: the details of the rows of "explain query plan", in their
// order. It fails as ScanIndex does.
func (r *ReadTx) ScanPlan(ctx context.Context, name string, scan IndexScan) (string, []string, error) {
	const operation = "scan"
	if err := r.check(); err != nil {
		return "", nil, err
	}
	defer r.leave()
	t, err := r.table(ctx, operation, name)
	if err != nil {
		return "", nil, err
	}
	ix, err := scanned(t, scan, r.c.limit, r.c.bugs)
	if err != nil {
		return "", nil, err
	}
	sql, _, err := t.scanSQL(ix, scan, r.c.bugs)
	if err != nil {
		return "", nil, err
	}
	var plan []string
	err = r.c.query("explain query plan "+sql, func(s *sqlite.Stmt) error {
		plan = append(plan, s.ColumnText(3))
		return nil
	})
	return sql, plan, r.d.cacheError(ctx, operation, err)
}

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
				for _, err := range c.scan(tb, Bound{}, Bound{}) {
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
