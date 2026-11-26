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
	// forward for an update and a delete. For a query whose rows are sorted, it is the direction of
	// the rows that tie, and a query whose access delivers the order of its ORDER BY reads in the
	// direction that delivers it, whatever these say.
	Forward, Backward bool
	// Sort makes the storage sort the rows of every query with ORDER BY, also where the key of its
	// access delivers the order. Without it, the access that Full, Primary, or Index forces delivers
	// the order where its key does, and is sorted otherwise: [Access.Delivered] of [PathAccessOf] says
	// which. With none of the three, Sort gives the access that the rules choose for the statement
	// without its ORDER BY.
	Sort bool
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
	q.limited, q.pins, q.sort = p.Limited, p.Pins, p.Sort
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

// ObserveSubqueries gives f the context of each statement of the engine e from now on and the values
// of its scalar subqueries, in the order of the text, once the engine has computed them, and the
// statement uses the values that f gives in their place; nil observes nothing. A test sets it between
// the statements, as for [Engine.SetPath], or once before the first.
func (e *Engine) ObserveSubqueries(f func(context.Context, []*s4qlpb.Value) []*s4qlpb.Value) {
	e.observe = f
}

// Forced reports whether the engine e forces its statements to read by a path, as [Engine.SetPath]
// and [Options.ScanAll] force one, other than the one that the rules choose, or to sort. A path that
// forces only the direction of the scan reads as the rules choose.
func (e *Engine) Forced() bool {
	return e.path.kind != pathRules || e.path.sort
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
	// Backward reports whether the scan gives its rows in the reverse of the order of its key, and,
	// for an access that is Sorted, its rows that tie in the reverse of the order of the primary key.
	Backward bool
	// Delivered reports whether the statement has an ORDER BY whose order the key of the access
	// delivers, in the direction of Backward, so that nothing sorts, and Sorted whether it has one
	// whose rows the storage sorts, by the columns By. A statement without ORDER BY has neither.
	Delivered, Sorted bool
	By                []core.KeyColumn
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
// two bounds". A scan of a statement with ORDER BY has ", in order" after that if its key delivers
// the order, and ", sorted" if its rows are sorted. The direction is not in it.
func (a Access) String() string {
	switch {
	case a.None:
		return "no row"
	case a.Point != nil:
		return "a get of one key"
	}
	return a.scanString() + map[bool]string{true: ", in order"}[a.Delivered] + map[bool]string{true: ", sorted"}[a.Sorted]
}

// scanString is [Access.String] of a scan, without its order.
func (a Access) scanString() string {
	if a.Full {
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
		Upper: core.Bound{Value: a.Upper, Open: a.UpperOpen}, Backward: a.Backward, Order: a.By}
}

func exported(a access) Access {
	return Access{None: a.none, Point: a.point, Full: a.full(), Index: a.index, Equal: a.equal,
		Lower: a.lower.value, Upper: a.upper.value, LowerOpen: a.lower.open, UpperOpen: a.upper.open, Backward: a.backward,
		Delivered: a.order == delivered, Sorted: a.order == sorted, By: a.by}
}

// ChooseAccess is [chooseAccess] for conditions on the columns at the positions, each with its
// operator and its value, which is nil for a test for NULL: the rules of the choice of an access, as
// a function of the table, its indexes, and the conditions alone.
func ChooseAccess(t *s4qlpb.CreateTable, indexes []*s4qlpb.CreateIndex, positions []int, ops []stmt.Operator, values []*s4qlpb.Value) (Access, error) {
	conditions := make([]condition, len(positions))
	for i := range positions {
		conditions[i] = condition{position: positions[i], op: ops[i], value: values[i]}
	}
	a, err := chooseAccess(t, indexes, conditions, orderBy{}, bugs{})
	return exported(a), err
}

// AccessOf the select, the update, or the delete of src, with args for its parameters, on the table
// ct with the indexes, in the order in which they were created: how the statement reads the rows of
// ct, as [chooseAccess] gives it to its scan, without a direction, but for an access that delivers
// the order of an ORDER BY, which has the direction that delivers it. It reports false if src holds
// no such statement with WHERE, and no select with ORDER BY, or if the statement fails before it has
// an access, as it does for a column that ct does not have, for a value of another type than its
// column, and for an argument of a LIMIT that is no integer from 0. Each argument must be a value
// that a column holds.
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
	// The access of an update or a delete does not depend on its RETURNING, so a column of RETURNING
	// that ct does not have does not keep it from one.
	switch s := s.(type) {
	case *stmt.Select:
		if s.Where == nil && s.OrderBy == nil {
			return Access{}, false
		}
		built, err = buildQuery(context.Background(), tableOf{ct, indexes}, NewPlan(s).(*Query), args, math.MaxInt, how)
	case *stmt.Update:
		if s.Where == nil {
			return Access{}, false
		}
		without := *s
		without.Returning = stmt.Returning{}
		built, err = buildSink(context.Background(), readOnly{tableOf{ct, indexes}}, NewPlan(&without), args, bugs{}, math.MaxInt, how)
	case *stmt.Delete:
		if s.Where == nil {
			return Access{}, false
		}
		without := *s
		without.Returning = stmt.Returning{}
		built, err = buildSink(context.Background(), readOnly{tableOf{ct, indexes}}, NewPlan(&without), args, bugs{}, math.MaxInt, how)
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

// Backward reports whether the rows of an insert, an update, or a delete with RETURNING come in the
// reverse of the order in which the statement changed them, as the engine drew it, and false for
// rows of another statement.
func (r *Rows) Backward() (backward, ok bool) {
	ret, ok := r.src.(*returned)
	if !ok {
		return false, false
	}
	return ret.backward, true
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
	"skipUnique":            func(b *bugs) { b.skipUnique = true },
	"ackUnknown":            func(b *bugs) { b.ackUnknown = true },
	"checkOutside":          func(b *bugs) { b.checkOutside = true },
	"keepOpen":              func(b *bugs) { b.keepOpen = true },
	"keepRead":              func(b *bugs) { b.keepRead = true },
	"argsNext":              func(b *bugs) { b.argsNext = true },
	"keepFailed":            func(b *bugs) { b.keepFailed = true },
	"readNewest":            func(b *bugs) { b.readNewest = true },
	"rollbackKeeps":         func(b *bugs) { b.rollbackKeeps = true },
	"convertReal":           func(b *bugs) { b.convertReal = true },
	"listOrder":             func(b *bugs) { b.listOrder = true },
	"zeroLeftOut":           func(b *bugs) { b.zeroLeftOut = true },
	"nullDefault":           func(b *bugs) { b.nullDefault = true },
	"openBound":             func(b *bugs) { b.openBound = true },
	"changeNothing":         func(b *bugs) { b.changeNothing = true },
	"keepOldKey":            func(b *bugs) { b.keepOldKey = true },
	"pinByPlace":            func(b *bugs) { b.pinByPlace = true },
	"replaceExisting":       func(b *bugs) { b.replaceExisting = true },
	"unturnedBound":         func(b *bugs) { b.unturnedBound = true },
	"turnMixed":             func(b *bugs) { b.turnMixed = true },
	"limitFirst":            func(b *bugs) { b.limitFirst = true },
	"ascendLast":            func(b *bugs) { b.ascendLast = true },
	"skipGap":               func(b *bugs) { b.skipGap = true },
	"lastIndexWins":         func(b *bugs) { b.lastIndexWins = true },
	"rowsOfFailed":          func(b *bugs) { b.rowsOfFailed = true },
	"oldValues":             func(b *bugs) { b.oldValues = true },
	"deleteNone":            func(b *bugs) { b.deleteNone = true },
	"dropLast":              func(b *bugs) { b.dropLast = true },
	"starKeyFirst":          func(b *bugs) { b.starKeyFirst = true },
	"lastRow":               func(b *bugs) { b.lastRow = true },
	"firstNotNull":          func(b *bugs) { b.firstNotNull = true },
	"ownChanges":            func(b *bugs) { b.ownChanges = true },
	"keepValue":             func(b *bugs) { b.keepValue = true },
	"noRowMatches":          func(b *bugs) { b.noRowMatches = true },
	"outsideTx":             func(b *bugs) { b.outsideTx = true },
	"swapSubqueries":        func(b *bugs) { b.swapSubqueries = true },
	"limitZeroRow":          func(b *bugs) { b.limitZeroRow = true },
	"orderIgnored":          func(b *bugs) { b.orderIgnored = true },
	"setFromNew":            func(b *bugs) { b.setFromNew = true },
	"nullCompares":          func(b *bugs) { b.nullCompares = true },
	"evalStops":             func(b *bugs) { b.evalStops = true },
	"errorSkipsRow":         func(b *bugs) { b.errorSkipsRow = true },
	"copyAcross":            func(b *bugs) { b.copyAcross = true },
	"constantsServeNothing": func(b *bugs) { b.constantsServeNothing = true },
	"copySelf":              func(b *bugs) { b.copySelf = true },
	"oneCommit":             func(b *bugs) { b.oneCommit = true },
	"goOn":                  func(b *bugs) { b.goOn = true },
	"commitFailed":          func(b *bugs) { b.commitFailed = true },
	"commitLater":           func(b *bugs) { b.commitLater = true },
	"annotateText":          func(b *bugs) { b.annotateText = true },
	"undoInTx":              func(b *bugs) { b.undoInTx = true },
	"firstResult":           func(b *bugs) { b.firstResult = true },
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
	AttrDirection    = attrDirection
	AttrOrder        = attrOrder
	AttrRowsRead     = attrRowsRead
	AttrPosition     = attrPosition
	AttrParent       = attrParent
	AttrOutcome      = attrOutcome
	AttrError        = attrError
	AttrCode         = attrCode
	AttrExtendedCode = attrExtendedCode
	AttrDuration     = attrDuration
	AttrNothing      = attrNothing
)

// FromCore is [fromCore].
var FromCore = fromCore
