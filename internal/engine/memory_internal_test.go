package engine

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// memory is a [database] in memory, with tables of one column in their primary key, of any type, for
// the fuzz test, which runs many more statements with it than it could on the storage core. It scans
// by an index as [memTx.scan] says. A write
// transaction changes a copy of the tables, which its commit puts in place. A real that is -0.0 is
// stored as 0.0, as the core stores it, and the keys are compared as SQLite compares them.
type memory struct {
	tables map[string]*memTable
}

type memTable struct {
	ct *s4qlpb.CreateTable
	// rows by the key of their primary-key value, as [memKey] gives it.
	rows map[string][]*s4qlpb.Value
	// indexes of the table, in the order in which they were created. A memory holds the definition
	// of an index and nothing else: a scan by one sorts the rows of the table.
	indexes []*s4qlpb.CreateIndex
}

// memKey of a value of a primary key: values that SQLite takes for one key, such as 0.0 and -0.0,
// have one memKey.
func memKey(v *s4qlpb.Value) string {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return fmt.Sprint("i", v.GetInteger())
	case s4qlpb.Value_Real_case:
		r := v.GetReal()
		if r == 0 {
			r = 0
		}
		return fmt.Sprint("r", math.Float64bits(r))
	case s4qlpb.Value_Text_case:
		return "t" + v.GetText()
	case s4qlpb.Value_Blob_case:
		return "b" + string(v.GetBlob())
	}
	panic(fmt.Sprintf("engine: a key of kind %v", v.WhichKind()))
}

// compareValues of one type, as SQLite compares them: integers and reals by their number, and text
// and blobs byte by byte.
func compareValues(a, b *s4qlpb.Value) int {
	switch a.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return cmp.Compare(a.GetInteger(), b.GetInteger())
	case s4qlpb.Value_Real_case:
		return cmp.Compare(a.GetReal(), b.GetReal())
	case s4qlpb.Value_Text_case:
		return strings.Compare(a.GetText(), b.GetText())
	case s4qlpb.Value_Blob_case:
		return bytes.Compare(a.GetBlob(), b.GetBlob())
	}
	panic(fmt.Sprintf("engine: a key of kind %v", a.WhichKind()))
}

// sorted rows of the table, in the order of their keys.
func (t *memTable) sorted() [][]*s4qlpb.Value {
	k := t.ct.GetPrimaryKey()[0]
	rows := slices.Collect(maps.Values(t.rows))
	slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int { return compareValues(a[k], b[k]) })
	return rows
}

// clone of the tables, with rows that are not shared.
func (m *memory) clone() map[string]*memTable {
	tables := make(map[string]*memTable, len(m.tables))
	for k, t := range m.tables {
		tables[k] = &memTable{ct: t.ct, rows: maps.Clone(t.rows), indexes: slices.Clone(t.indexes)}
	}
	return tables
}

func (m *memory) begin(context.Context) (writeTx, error) {
	return &memTx{m: m, tables: m.clone()}, nil
}

// reader of the memory, which is the memory itself: it runs any number of read transactions at
// once.
func (m *memory) reader(context.Context) (readConn, error) {
	return m, nil
}

// lengthLimit of the memory is SQLite's default.
func (m *memory) lengthLimit() int {
	return 1_000_000_000
}

func (m *memory) close(context.Context) error {
	return nil
}

func (m *memory) read(context.Context) (readTx, error) {
	return &memTx{m: m, tables: m.tables}, nil
}

// memTx is a transaction of a memory.
type memTx struct {
	m      *memory
	tables map[string]*memTable
	// saved is the state of tables at the savepoint that is open.
	saved map[string]*memTable
	// read is the number of rows that the scans and the gets of the transaction gave.
	read int64
}

func (x *memTx) table(_ context.Context, name string) (*s4qlpb.CreateTable, error) {
	if t, ok := x.tables[fold(name)]; ok {
		return t.ct, nil
	}
	return nil, nil
}

func (x *memTx) indexes(_ context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	return x.tables[fold(name)].indexes, nil
}

func (x *memTx) index(_ context.Context, name string) (*s4qlpb.CreateIndex, error) {
	for _, t := range x.tables {
		for _, ix := range t.indexes {
			if fold(ix.GetIndex()) == fold(name) {
				return ix, nil
			}
		}
	}
	return nil, nil
}

// compareNullable is [compareValues] of two values of a column that takes NULL, of which NULL is
// the least, as SQLite sorts it.
func compareNullable(a, b *s4qlpb.Value) int {
	switch {
	case a.HasNull() && b.HasNull():
		return 0
	case a.HasNull():
		return -1
	case b.HasNull():
		return 1
	}
	return compareValues(a, b)
}

// scan the rows of the table as the storage core does, by a way of its own: it sorts every row by
// the key of the index, each column in its direction, and then keeps the rows whose leading columns
// are equal to the pinned values, NULL to NULL, and whose next column is not NULL and within the
// bounds.
func (x *memTx) scan(_ context.Context, name string, scan core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		t := x.tables[fold(name)]
		var key []int
		descending := map[int]bool{}
		if scan.Index != "" {
			var found *s4qlpb.CreateIndex
			for _, ix := range t.indexes {
				if fold(ix.GetIndex()) == fold(scan.Index) {
					found = ix
				}
			}
			if found == nil {
				yield(nil, internalError("a scan of the index %v, which the table %v does not have", scan.Index, name))
				return
			}
			for _, c := range found.GetColumns() {
				key = append(key, int(c.GetColumn()))
				descending[int(c.GetColumn())] = c.GetDescending()
			}
		}
		for _, position := range t.ct.GetPrimaryKey() {
			if !slices.Contains(key, int(position)) {
				key = append(key, int(position))
			}
		}
		rows := slices.Collect(maps.Values(t.rows))
		slices.SortFunc(rows, func(a, b []*s4qlpb.Value) int {
			for _, position := range key {
				c := compareNullable(a[position], b[position])
				if descending[position] {
					c = -c
				}
				if c != 0 {
					return c
				}
			}
			return 0
		})
		if scan.Backward {
			slices.Reverse(rows)
		}
	rows:
		for _, row := range rows {
			for i, v := range scan.Equal {
				if compareNullable(row[key[i]], v) != 0 {
					continue rows
				}
			}
			if scan.Lower.Value != nil || scan.Upper.Value != nil {
				v := row[key[len(scan.Equal)]]
				if v.HasNull() {
					continue
				}
				if scan.Lower.Value != nil {
					if c := compareValues(v, scan.Lower.Value); c < 0 || c == 0 && scan.Lower.Open {
						continue
					}
				}
				if scan.Upper.Value != nil {
					if c := compareValues(v, scan.Upper.Value); c > 0 || c == 0 && scan.Upper.Open {
						continue
					}
				}
			}
			x.read++
			if !yield(row, nil) {
				return
			}
		}
	}
}

func (x *memTx) get(_ context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row := x.tables[fold(name)].rows[memKey(key[0])]
	if row != nil {
		x.read++
	}
	return row, nil
}

func (x *memTx) rowsRead() int64 {
	return x.read
}

func (x *memTx) createIndex(_ context.Context, ci *s4qlpb.CreateIndex) error {
	t := x.tables[fold(ci.GetTable())]
	t.indexes = append(t.indexes, ci)
	return nil
}

func (x *memTx) dropIndex(_ context.Context, name string) error {
	for _, t := range x.tables {
		t.indexes = slices.DeleteFunc(t.indexes, func(ix *s4qlpb.CreateIndex) bool { return fold(ix.GetIndex()) == fold(name) })
	}
	return nil
}

func (x *memTx) createTable(_ context.Context, ct *s4qlpb.CreateTable) error {
	x.tables[fold(ct.GetTable())] = &memTable{ct: ct, rows: map[string][]*s4qlpb.Value{}}
	return nil
}

func (x *memTx) dropTable(_ context.Context, name string) error {
	delete(x.tables, fold(name))
	return nil
}

func (x *memTx) upsert(_ context.Context, name string, row []*s4qlpb.Value) error {
	t := x.tables[fold(name)]
	stored := make([]*s4qlpb.Value, len(row))
	for i, v := range row {
		if v.HasReal() && v.GetReal() == 0 {
			v = s4qlpb.Value_builder{Real: new(0.0)}.Build()
		}
		stored[i] = v
	}
	t.rows[memKey(stored[t.ct.GetPrimaryKey()[0]])] = stored
	return nil
}

// insertNew does not measure the record of the row, as a memory holds rows of any length.
func (x *memTx) insertNew(ctx context.Context, name string, row []*s4qlpb.Value) (bool, error) {
	t := x.tables[fold(name)]
	if _, ok := t.rows[memKey(row[t.ct.GetPrimaryKey()[0]])]; ok {
		return false, nil
	}
	return true, x.upsert(ctx, name, row)
}

func (x *memTx) delete(_ context.Context, name string, key []*s4qlpb.Value) (bool, error) {
	t := x.tables[fold(name)]
	k := memKey(key[0])
	_, ok := t.rows[k]
	delete(t.rows, k)
	return ok, nil
}

// keys runs f with a set of keys in memory, whose all gives the least key above the one before, each
// time it looks.
func (x *memTx) keys(_ context.Context, _ string, f func(keySet) error) error {
	return f(&memKeys{keys: map[string]*s4qlpb.Value{}})
}

type memKeys struct {
	keys map[string]*s4qlpb.Value
}

func (k *memKeys) add(_ context.Context, key []*s4qlpb.Value) error {
	k.keys[memKey(key[0])] = key[0]
	return nil
}

func (k *memKeys) all(context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		var last *s4qlpb.Value
		for {
			var next *s4qlpb.Value
			for _, key := range k.keys {
				if (last == nil || compareValues(key, last) > 0) && (next == nil || compareValues(key, next) < 0) {
					next = key
				}
			}
			if next == nil || !yield([]*s4qlpb.Value{next}, nil) {
				return
			}
			last = next
		}
	}
}

func (x *memTx) position() format.Name                                   { return format.EmptyLog }
func (x *memTx) annotate(context.Context, string, []*s4qlpb.Value) error { return nil }
func (x *memTx) rollback(context.Context) error                          { return nil }
func (x *memTx) end(context.Context) error                               { return nil }
func (x *memTx) serving(context.Context) error                           { return nil }
func (x *memTx) lost() error                                             { return nil }
func (x *memTx) release(context.Context) error                           { x.saved = nil; return nil }

func (x *memTx) savepoint(context.Context) error {
	x.saved = (&memory{tables: x.tables}).clone()
	return nil
}

func (x *memTx) rollbackTo(context.Context) error {
	x.tables, x.saved = x.saved, nil
	return nil
}

func (x *memTx) commit(context.Context) error {
	x.m.tables = x.tables
	return nil
}

// state of the memory: each table, in the order of the names as bytes, with its columns, each with
// NULL after its type if it takes NULL, and its rows in the order of the key, and each value as text
// gives it.
func (m *memory) state(text func(*s4qlpb.Value) string) string {
	tables := slices.Collect(maps.Values(m.tables))
	slices.SortFunc(tables, func(a, b *memTable) int { return cmp.Compare(a.ct.GetTable(), b.ct.GetTable()) })
	var b strings.Builder
	for _, t := range tables {
		var columns []string
		for _, c := range t.ct.GetColumns() {
			column := c.GetName() + " " + typeName(c.GetType())
			if c.GetNullable() {
				column += " NULL"
			}
			columns = append(columns, column)
		}
		var rows []string
		for _, row := range t.sorted() {
			values := make([]string, len(row))
			for i, v := range row {
				values[i] = text(v)
			}
			rows = append(rows, strings.Join(values, ","))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", t.ct.GetTable(), strings.Join(columns, ", "), strings.Join(rows, " "))
	}
	// The indexes of the tables, as the tests of the engine on the core give them.
	for _, t := range tables {
		var indexes []string
		for _, ci := range t.indexes {
			var cs []string
			for _, c := range ci.GetColumns() {
				column := strconv.Quote(t.ct.GetColumns()[c.GetColumn()].GetName())
				if c.GetDescending() {
					column += " desc"
				}
				cs = append(cs, column)
			}
			indexes = append(indexes, strconv.Quote(ci.GetIndex())+" ("+strings.Join(cs, ", ")+")")
		}
		if len(indexes) > 0 {
			fmt.Fprintf(&b, "%v: %v\n", strconv.Quote(t.ct.GetTable()), strings.Join(indexes, " "))
		}
	}
	return b.String()
}

// NewMemory engine with the options, on a database in memory, and a function that gives the state of
// the database as text, with each value as its argument gives it.
func NewMemory(h host.Host, options Options) (*Engine, func(text func(*s4qlpb.Value) string) string) {
	m := &memory{tables: map[string]*memTable{}}
	return newEngine(m, h, options), m.state
}
