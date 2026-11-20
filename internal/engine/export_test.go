package engine

import (
	"context"
	"fmt"
	"iter"
	"math"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
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

// Path that the tests force every statement of an engine to read its rows by, as [path] says. The
// zero value forces nothing.
type Path struct {
	// Full reads every row of the table in the order of the primary key.
	Full bool
	// Primary scans by the primary key, also where the rules read one row by a get.
	Primary bool
	// Index is the name of the index to scan by. A statement on a table that has no index of the name
	// reads as the rules say.
	Index string
	// Limited makes the access pin at most Pins leading columns of the key, and bound nothing unless
	// the conditions pin no more than that.
	Limited bool
	Pins    int
	// Forward and Backward fix the direction of the scan, which is otherwise drawn for a query, and
	// forward for an update and a delete.
	Forward, Backward bool
}

func (p Path) path() path {
	var q path
	switch {
	case p.Full:
		q.kind = pathFull
	case p.Primary:
		q.kind = pathPrimary
	case p.Index != "":
		q.kind, q.index = pathIndex, p.Index
	}
	q.limited, q.pins = p.Limited, p.Pins
	switch {
	case p.Forward:
		q.direction = directionForward
	case p.Backward:
		q.direction = directionBackward
	}
	return q
}

// Path makes every statement of an engine of the options o read its rows by p.
func (o *Options) Path(p Path) {
	o.path = p.path()
}

// SetPath makes every statement of the engine e from now on read its rows by p. A statement reads
// the setting when it starts, so a test sets it between the statements of a connection, and never
// while another goroutine runs a statement of the engine.
func (e *Engine) SetPath(p Path) {
	e.path = p.path()
}

// Seed makes an engine of the options o draw the directions of the scans of its queries from the
// sequence of the seed, and not from its host.
func (o *Options) Seed(seed uint64) {
	o.Directions = &seed
}

// Seeded reports whether the options o have a seed, as [Options.Seed] gives them one.
func (o Options) Seeded() bool {
	return o.Directions != nil
}

// SetSeed makes the engine e draw the directions of the scans of its queries from the start of the
// sequence of the seed from now on, as [Options.Seed] does.
func (e *Engine) SetSeed(seed uint64) {
	e.seeded = true
	e.draws.Store(seed)
}

// ScanAll makes every query of an engine of the options o read every row of its table, and not the
// rows that the rules choose, as a [Path] that is Full does.
func (o *Options) ScanAll() {
	o.path.kind = pathFull
}

// SetScanAll makes every query of the engine e from now on read every row of its table if on, and
// the rows that the rules choose otherwise, as [Engine.SetPath] does with a [Path] that is Full or
// with none. It keeps the direction that the path of the engine forces.
func (e *Engine) SetScanAll(on bool) {
	e.path.kind = pathRules
	if on {
		e.path.kind = pathFull
	}
}

// ScansAll reports whether every query of the engine e reads every row of its table, as
// [Engine.SetScanAll] and [Options.ScanAll] make it.
func (e *Engine) ScansAll() bool {
	return e.path.kind == pathFull
}

// Access of a statement to the rows of its table, as [access] holds it, for the tests.
type Access struct {
	// None reads no row.
	None bool
	// Point is the primary key of the one row that a get reads, or nil.
	Point []*s4qlpb.Value
	// Full reads every row in the order of the primary key.
	Full bool
	// Index is the name of the index of a scan, or empty for the primary key.
	Index string
	// Equal holds the values that the scan pins the leading columns of its key to.
	Equal []*s4qlpb.Value
	// Lower and Upper bound the column after the pinned ones, if they are not nil, and leave the
	// bound itself out if open.
	Lower, Upper         *s4qlpb.Value
	LowerOpen, UpperOpen bool
	// Backward reports whether the scan gives its rows in the reverse of the order of its key.
	Backward bool
}

// Pinned is the number of leading columns of its key that the access pins.
func (a Access) Pinned() int {
	if a.Point != nil {
		return len(a.Point)
	}
	return len(a.Equal)
}

// String of the access in words: "no row", "a get of one key", "a scan of every row", or a scan by
// the primary key or by an index, with the number of columns that it pins, if any, and its bounds,
// if any, such as "a scan of the index i with 2 pinned from a lower bound". A scan by the primary key
// that pins nothing is "a scan from a lower bound", "a scan to an upper bound", or "a scan between
// two bounds". The direction is not in it.
func (a Access) String() string {
	switch {
	case a.None:
		return "no row"
	case a.Point != nil:
		return "a get of one key"
	case a.Full:
		return "a scan of every row"
	}
	s := "a scan"
	if a.Index != "" {
		s += " of the index " + a.Index
	}
	if len(a.Equal) > 0 {
		s += fmt.Sprint(" with ", len(a.Equal), " pinned")
	}
	switch {
	case a.Lower != nil && a.Upper != nil:
		s += " between two bounds"
	case a.Lower != nil:
		s += " from a lower bound"
	case a.Upper != nil:
		s += " to an upper bound"
	}
	return s
}

// Scan of the access, a scan by the primary key or by an index, as the engine gives it to the
// storage.
func (a Access) Scan() core.IndexScan {
	return core.IndexScan{Index: a.Index, Equal: a.Equal, Lower: core.Bound{Value: a.Lower, Open: a.LowerOpen},
		Upper: core.Bound{Value: a.Upper, Open: a.UpperOpen}, Backward: a.Backward}
}

func exported(a access) Access {
	return Access{None: a.none, Point: a.point, Full: a.full(), Index: a.index, Equal: a.equal,
		Lower: a.lower.value, Upper: a.upper.value, LowerOpen: a.lower.open, UpperOpen: a.upper.open, Backward: a.backward}
}

// ChooseAccess is [chooseAccess] for conditions on the columns at the positions, each with its
// operator and its value, which is nil for a test for NULL: the rules of the choice of an access, as
// a function of the table, its indexes, and the conditions alone.
func ChooseAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, positions []int, ops []stmt.Operator, values []*s4qlpb.Value) (Access, error) {
	conditions := make([]condition, len(positions))
	for i := range positions {
		conditions[i] = condition{position: positions[i], op: ops[i], value: values[i]}
	}
	a, err := chooseAccess(t, indexes, conditions, bugs{})
	return exported(a), err
}

// AccessOf the select, the update, or the delete of src, with args for its parameters, on the table
// ct with the indexes, in the order in which they were created: how the statement reads the rows of
// ct, as [chooseAccess] gives it to its scan, without a direction. It reports false if src holds no
// such statement with WHERE, or if the statement fails before it has an access, as it does for a
// column that ct does not have and for a value of another type than its column. Each argument must
// be a value that a column holds.
func AccessOf(src string, ct *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, args []*s4qlpb.Value) (Access, bool) {
	return PathAccessOf(src, ct, indexes, args, Path{})
}

// PathAccessOf is [AccessOf] for an engine whose statements read by the path p, with the direction
// that p fixes.
func PathAccessOf(src string, ct *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, args []*s4qlpb.Value, p Path) (Access, bool) {
	s, err := stmt.Check(src)
	if err != nil || s == nil || len(args) != s.Parameters() {
		return Access{}, false
	}
	how := reading{path: p.path()}
	how.backward = how.path.direction == directionBackward
	var built any
	switch s := s.(type) {
	case *stmt.Select:
		if s.Where == nil {
			return Access{}, false
		}
		built, err = buildQuery(context.Background(), tableOf{ct, indexes}, NewPlan(s).(*Query), args, math.MaxInt, how)
	case *stmt.Update:
		if s.Where == nil {
			return Access{}, false
		}
		built, err = buildSink(context.Background(), readOnly{tableOf{ct, indexes}}, NewPlan(s), args, bugs{}, math.MaxInt, how)
	case *stmt.Delete:
		if s.Where == nil {
			return Access{}, false
		}
		built, err = buildSink(context.Background(), readOnly{tableOf{ct, indexes}}, NewPlan(s), args, bugs{}, math.MaxInt, how)
	default:
		return Access{}, false
	}
	if err != nil {
		return Access{}, false
	}
	scan := scanOf(built)
	if scan == nil {
		return Access{}, false
	}
	return exported(scan.access), true
}

// Access of the rows of a query: how its scan reads the rows of its table, with the direction that
// the engine drew for it, and false for rows of a statement that is no query.
func (r *Rows) Access() (Access, bool) {
	scan := scanOf(r.src)
	if scan == nil {
		return Access{}, false
	}
	return exported(scan.access), true
}

// tableOf is a [readTx] of the one table ct, under any name, with the indexes, which reads no rows.
type tableOf struct {
	ct  *s4qlpb.CreateTable
	cis []*s4qlpb.CreateIndex
}

func (t tableOf) table(context.Context, string) (*s4qlpb.CreateTable, error) { return t.ct, nil }

func (t tableOf) indexes(context.Context, string) ([]*s4qlpb.CreateIndex, error) {
	return t.cis, nil
}

func (t tableOf) index(context.Context, string) (*s4qlpb.CreateIndex, error) {
	return nil, nil
}

func (t tableOf) scan(context.Context, string, core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	panic("engine: a scan of a table of AccessOf")
}

func (t tableOf) get(context.Context, string, []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	panic("engine: a get of a table of AccessOf")
}

func (t tableOf) rowsRead() int64 { return 0 }

func (t tableOf) position() format.Name { return format.Name{} }

func (t tableOf) serving(context.Context) error { return nil }

func (t tableOf) end(context.Context) error { return nil }

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
	"pinByPlace":    func(b *bugs) { b.pinByPlace = true },
}

// The names of the event and of its attributes, for the tests of the telemetry.
const (
	EventStatement   = eventStatement
	AttrKind         = attrKind
	AttrTable        = attrTable
	AttrSQL          = attrSQL
	AttrRows         = attrRows
	AttrAccess       = attrAccess
	AttrIndex        = attrIndex
	AttrRowsRead     = attrRowsRead
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
