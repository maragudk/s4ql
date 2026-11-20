package engine

import (
	"context"
	"iter"
	"math"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Plant the bug with the given name in o, which is a field of [bugs]. It panics for a name that is
// none.
func (o *Options) Plant(name string) {
	set, ok := plantable[name]
	if !ok {
		panic("engine: no planted bug is named " + name)
	}
	set(&o.bugs)
}

// ScanAll makes every query of an engine of the options o read every row of its table, and not the
// rows by the key, as [Options.scanAll] says.
func (o *Options) ScanAll() {
	o.scanAll = true
}

// SetScanAll makes every query of the engine e from now on read every row of its table if on, as
// [Options.scanAll] says, and the rows by the key otherwise. A query reads the setting when it
// starts, so a test sets it between the queries of a connection.
func (e *Engine) SetScanAll(on bool) {
	e.scanAll = on
}

// ScansAll reports whether every query of the engine e reads every row of its table, as
// [Engine.SetScanAll] and [Options.ScanAll] make it.
func (e *Engine) ScansAll() bool {
	return e.scanAll
}

// AccessOf the select, the update, or the delete of src, with args for its parameters, on the table
// ct: how its query, or the scan that finds the rows that the statement changes, reads the rows of ct
// by the key, as [keyAccess] gives it to the scan: "no row", "a get of one key", "a scan from a lower
// bound", "a scan to an upper bound", "a scan between two bounds", or "a scan of every row". It is ""
// if src holds no select, update, or delete with WHERE, or if the build of the plan fails, as it does
// for a column that ct does not have and for a value of a condition of another type than its column.
// Each argument must be a value that a column holds.
func AccessOf(src string, ct *s4qlpb.CreateTable, args []*s4qlpb.Value) string {
	s, err := stmt.Check(src)
	if err != nil || s == nil || len(args) != s.Parameters() {
		return ""
	}
	var n source
	switch s := s.(type) {
	case *stmt.Select:
		if s.Where == nil {
			return ""
		}
		n, err = buildQuery(context.Background(), tableOf{ct}, NewPlan(s).(*Query), args, math.MaxInt, false)
	case *stmt.Update, *stmt.Delete:
		if u, ok := s.(*stmt.Update); ok && u.Where == nil || !ok && s.(*stmt.Delete).Where == nil {
			return ""
		}
		var k sink
		k, err = buildSink(context.Background(), tableOf{ct}, NewPlan(s), args, bugs{}, math.MaxInt, false)
		switch k := k.(type) {
		case *update:
			n = k.src
		case *remove:
			n = k.src
		}
	default:
		return ""
	}
	for err == nil {
		switch s := n.(type) {
		case *project:
			n = s.src
		case *filter:
			n = s.src
		case *scan:
			a := s.access
			switch {
			case a.none:
				return "no row"
			case a.key != nil:
				return "a get of one key"
			case a.lower.value != nil && a.upper.value != nil:
				return "a scan between two bounds"
			case a.lower.value != nil:
				return "a scan from a lower bound"
			case a.upper.value != nil:
				return "a scan to an upper bound"
			}
			return "a scan of every row"
		default:
			return ""
		}
	}
	return ""
}

// tableOf is a [writer] of the one table ct, under any name, which reads and writes no rows.
type tableOf struct {
	ct *s4qlpb.CreateTable
}

func (t tableOf) table(context.Context, string) (*s4qlpb.CreateTable, error) { return t.ct, nil }

func (t tableOf) scan(context.Context, string) iter.Seq2[[]*s4qlpb.Value, error] {
	panic("engine: a scan of a table of AccessOf")
}

func (t tableOf) scanRange(context.Context, string, bound, bound) iter.Seq2[[]*s4qlpb.Value, error] {
	panic("engine: a scan of a table of AccessOf")
}

func (t tableOf) get(context.Context, string, []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	panic("engine: a get of a table of AccessOf")
}

func (t tableOf) createTable(context.Context, *s4qlpb.CreateTable) error {
	panic("engine: a create table of AccessOf")
}

func (t tableOf) dropTable(context.Context, string) error { panic("engine: a drop table of AccessOf") }

func (t tableOf) upsert(context.Context, string, []*s4qlpb.Value) error {
	panic("engine: a write of a table of AccessOf")
}

func (t tableOf) insertNew(context.Context, string, []*s4qlpb.Value) (bool, error) {
	panic("engine: a write of a table of AccessOf")
}

func (t tableOf) delete(context.Context, string, []*s4qlpb.Value) (bool, error) {
	panic("engine: a write of a table of AccessOf")
}

func (t tableOf) keys(context.Context, string, func(keySet) error) error {
	panic("engine: a set of keys of a table of AccessOf")
}

// plantable bugs, by the names of the fields of [bugs].
var plantable = map[string]func(b *bugs){
	"skipUnique":    func(b *bugs) { b.skipUnique = true },
	"ackUnknown":    func(b *bugs) { b.ackUnknown = true },
	"checkOutside":  func(b *bugs) { b.checkOutside = true },
	"keepOpen":      func(b *bugs) { b.keepOpen = true },
	"keepRead":      func(b *bugs) { b.keepRead = true },
	"annotateNext":  func(b *bugs) { b.annotateNext = true },
	"keepFailed":    func(b *bugs) { b.keepFailed = true },
	"readNewest":    func(b *bugs) { b.readNewest = true },
	"rollbackKeeps": func(b *bugs) { b.rollbackKeeps = true },
	"convertReal":   func(b *bugs) { b.convertReal = true },
	"listOrder":     func(b *bugs) { b.listOrder = true },
	"zeroLeftOut":   func(b *bugs) { b.zeroLeftOut = true },
	"openBound":     func(b *bugs) { b.openBound = true },
	"changeNothing": func(b *bugs) { b.changeNothing = true },
	"keepOldKey":    func(b *bugs) { b.keepOldKey = true },
}

// The names of the event and of its attributes, for the tests of the telemetry.
const (
	EventStatement   = eventStatement
	AttrKind         = attrKind
	AttrTable        = attrTable
	AttrSQL          = attrSQL
	AttrRows         = attrRows
	AttrPosition     = attrPosition
	AttrParent       = attrParent
	AttrOutcome      = attrOutcome
	AttrError        = attrError
	AttrCode         = attrCode
	AttrExtendedCode = attrExtendedCode
	AttrDuration     = attrDuration
)

// FromCore is [fromCore].
var FromCore = fromCore
