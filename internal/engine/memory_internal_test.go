package engine

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// memory is a [database] in memory, with tables of one integer column in their primary key, for the
// fuzz test, which runs many more statements with it than it could on the storage core. A write
// transaction changes a copy of the tables, which its commit puts in place.
type memory struct {
	tables map[string]*memTable
}

type memTable struct {
	ct   *s4qlpb.CreateTable
	rows map[int64][]*s4qlpb.Value
}

// clone of the tables, with rows that are not shared.
func (m *memory) clone() map[string]*memTable {
	tables := make(map[string]*memTable, len(m.tables))
	for k, t := range m.tables {
		tables[k] = &memTable{ct: t.ct, rows: maps.Clone(t.rows)}
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
}

func (x *memTx) table(_ context.Context, name string) (*s4qlpb.CreateTable, error) {
	if t, ok := x.tables[fold(name)]; ok {
		return t.ct, nil
	}
	return nil, nil
}

func (x *memTx) scan(_ context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		t := x.tables[fold(name)]
		for _, k := range slices.Sorted(maps.Keys(t.rows)) {
			if !yield(t.rows[k], nil) {
				return
			}
		}
	}
}

func (x *memTx) get(_ context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	return x.tables[fold(name)].rows[key[0].GetInteger()], nil
}

func (x *memTx) createTable(_ context.Context, ct *s4qlpb.CreateTable) error {
	x.tables[fold(ct.GetTable())] = &memTable{ct: ct, rows: map[int64][]*s4qlpb.Value{}}
	return nil
}

func (x *memTx) dropTable(_ context.Context, name string) error {
	delete(x.tables, fold(name))
	return nil
}

func (x *memTx) upsert(_ context.Context, name string, row []*s4qlpb.Value) error {
	t := x.tables[fold(name)]
	t.rows[row[t.ct.GetPrimaryKey()[0]].GetInteger()] = row
	return nil
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

// state of the memory: each table, in the order of the names as bytes, with its columns and its
// rows in the order of the key.
func (m *memory) state() string {
	tables := slices.Collect(maps.Values(m.tables))
	slices.SortFunc(tables, func(a, b *memTable) int { return cmp.Compare(a.ct.GetTable(), b.ct.GetTable()) })
	var b strings.Builder
	for _, t := range tables {
		var columns []string
		for _, c := range t.ct.GetColumns() {
			columns = append(columns, c.GetName()+" "+typeName(c.GetType()))
		}
		var rows []string
		for _, k := range slices.Sorted(maps.Keys(t.rows)) {
			values := make([]string, len(t.rows[k]))
			for i, v := range t.rows[k] {
				values[i] = fmt.Sprint(v.GetInteger())
			}
			rows = append(rows, strings.Join(values, ","))
		}
		fmt.Fprintf(&b, "%v (%v): %v\n", t.ct.GetTable(), strings.Join(columns, ", "), strings.Join(rows, " "))
	}
	return b.String()
}

// NewMemory engine, on a database in memory, and a function that gives the state of the database as
// text.
func NewMemory(h host.Host) (*Engine, func() string) {
	m := &memory{tables: map[string]*memTable{}}
	return newEngine(m, h, Options{}), m.state
}
