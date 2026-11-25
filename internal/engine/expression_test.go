package engine_test

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// expressed reports whether s is a statement with an arithmetic expression: in the SET of an update,
// or in a condition of its WHERE or of the WHERE of a subquery.
func expressed(s stmt.Statement) bool {
	var where []stmt.Condition
	var subqueries []stmt.Select
	switch s := s.(type) {
	case *stmt.Select:
		where, subqueries = s.Where, s.Subqueries
	case *stmt.Update:
		where, subqueries = s.Where, s.Subqueries
		for _, a := range s.Set {
			if a.Value.Expression.Present() {
				return true
			}
		}
	case *stmt.Delete:
		where, subqueries = s.Where, s.Subqueries
	}
	for _, q := range subqueries {
		where = append(where[:len(where):len(where)], q.Where...)
	}
	for _, c := range where {
		if c.Expression.Present() || c.Value.Expression.Present() {
			return true
		}
	}
	return false
}

// conditionTerms of the conditions where: the value of each, and the arithmetic expression that it
// tests, as a term, where it tests one, in the order of the text.
func conditionTerms(where []stmt.Condition) []stmt.Term {
	var terms []stmt.Term
	for _, c := range where {
		tested := stmt.Term{Expression: c.Expression, Span: c.Expression.Span}
		switch {
		case !c.Expression.Present():
			terms = append(terms, c.Value)
		case c.ValueFirst:
			terms = append(terms, c.Value, tested)
		default:
			terms = append(terms, tested, c.Value)
		}
	}
	return terms
}

// conditionParameters of the condition c: the numbers of its parameters, in the order of the text,
// but those of a subquery.
func conditionParameters(c stmt.Condition) []int {
	var ps []int
	for _, t := range conditionTerms([]stmt.Condition{c}) {
		ps = append(ps, termParameters(t)...)
	}
	return ps
}

// termParameters of the term t: the number of its parameter, or those of its arithmetic expression in
// the order of the text, but those of a subquery.
func termParameters(t stmt.Term) []int {
	if t.Parameter > 0 {
		return []int{t.Parameter}
	}
	return expressionParameters(t.Expression)
}

// expressionParameters of the expression e, in the order of the text.
func expressionParameters(e stmt.Expression) []int {
	if e.Kind == stmt.ExprParameter {
		return []int{e.Parameter}
	}
	var ps []int
	for _, o := range e.Operands {
		ps = append(ps, expressionParameters(o)...)
	}
	return ps
}

// conditionColumns of the condition c: the column that it tests, or the columns of the expression that
// it tests, and the columns of its value, in the order of the text.
func conditionColumns(c stmt.Condition) []stmt.Name {
	var names []stmt.Name
	if !c.Expression.Present() {
		names = append(names, c.Column)
	}
	for _, t := range conditionTerms([]stmt.Condition{c}) {
		names = append(names, expressionColumns(t.Expression)...)
	}
	return names
}

// expressionColumns of the expression e, in the order of the text.
func expressionColumns(e stmt.Expression) []stmt.Name {
	if e.Kind == stmt.ExprColumn {
		return []stmt.Name{e.Column}
	}
	var names []stmt.Name
	for _, o := range e.Operands {
		names = append(names, expressionColumns(o)...)
	}
	return names
}

// The kinds of the calls with arithmetic that [pair.checkArithmetic] decides without SQLite.
const (
	excusedArithmeticType = "arithmetic refused by the rule of exact types"
	excusedEvaluation     = "evaluation error that SQLite's result of its operator justifies"
	// modeledDue is the kind of a select with arithmetic whose due errors [pair.due] decides by the
	// range of the model's access.
	modeledDue = "arithmetic of a select whose due errors the model's access decides"
)

// checkArithmetic of the call c of the statement s with arithmetic, with the arguments args as SQLite
// binds them, whose answer of the engine is got, against s4ql's rules for arithmetic, independently
// of the engine. A statement that SQLite does not prepare gets SQLite's error, which the caller
// compares. Otherwise:
//
//   - "Not supported" must be a refusal of [arithmeticRefusals], and one of them must come if there are
//     any: the engine may give any of them, since their order is the engine's own, which its own tests
//     pin.
//   - An evaluation error must be justified by SQLite: SQLite's result of the failing operator alone,
//     on its operands, must show the quirk that s4ql refuses, for a row that the documented rule may
//     evaluate, as [pair.justify] says. The call is not run in SQLite, and nothing changed. An update
//     or a delete in a read-only transaction computes nothing, so it never has one.
//   - Where the engine gives no error, no evaluation error was due, as [pair.due] says.
//
// It gives the kind of excuse if the rule decides the call, or "", and what differs, or "".
func (p *pair) checkArithmetic(c stmttest.Call, s stmt.Statement, args []any, got answer) (excuse, failure string) {
	t, o := p.t, p.o
	t.Helper()
	if len(args) != s.Parameters() || o.prepare(t, c.Src) != nil {
		return "", ""
	}
	refusals := arithmeticRefusals(t, o, s, args, p.readOnly)
	switch got.err {
	case "not supported":
		if !slices.Contains(refusals, got.message) {
			return excusedArithmeticType, fmt.Sprintf("%q with the arguments %#v: the engine gives %q, and the rules %q", c.Src, args, got.message, refusals)
		}
		return excusedArithmeticType, ""
	case evaluationError:
		if _, isSelect := s.(*stmt.Select); p.readOnly && !isSelect {
			// An update or a delete in a read-only transaction computes nothing, and gives SQLite's
			// error for the write.
			return excusedEvaluation, fmt.Sprintf("%q with the arguments %#v: the engine gives %q in a read-only transaction", c.Src, args, got.message)
		}
		if why := p.justify(s, args, got.eval); why != "" {
			return excusedEvaluation, fmt.Sprintf("%q with the arguments %#v: the engine gives %q, which %v", c.Src, args, got.message, why)
		}
		return excusedEvaluation, ""
	case "":
		if len(refusals) > 0 {
			return "", fmt.Sprintf("%q with the arguments %#v: the engine gives %v, and the rules refuse it with one of %q", c.Src, args, got, refusals)
		}
		if due := p.due(s, args); due != "" {
			return "", fmt.Sprintf("%q with the arguments %#v: the engine gives %v, and %v", c.Src, args, got, due)
		}
	}
	return "", ""
}

// arithmeticRefusals of the select, the update, or the delete s with arithmetic, which SQLite
// prepares, with the arguments args as SQLite binds them, on the database of o: every "not supported"
// that s4ql's rules give for it, whatever their order, as [stmttest.ArithmeticRefusals] gives them.
func arithmeticRefusals(t testing.TB, o *oracle, s stmt.Statement, args []any, readOnly bool) []string {
	t.Helper()
	table, _ := tableWhere(s)
	name, columns := o.columns(t, table.Value)
	return stmttest.ArithmeticRefusals(name, columns, s, args, readOnly, func(name string) (string, []stmttest.TableColumn) { return o.columns(t, name) })
}

// computedTerms of the values that the engine computed for the subqueries of the statement of the
// pair, as literals, or nil where it computed none.
func (p *pair) computedTerms() []stmt.Term {
	if p.computed == nil {
		return nil
	}
	var terms []stmt.Term
	for _, v := range p.computed {
		terms = append(terms, valueTerm(v))
	}
	return terms
}

// rows reports whether SQLite gives a row for the query src with the arguments args, which must not
// fail.
func (p *pair) rows(src string, args []any) bool {
	p.t.Helper()
	a := p.o.run(p.t, src, args...)
	if a.err != "" {
		p.t.Fatalf("%q: SQLite gives %v", src, a.err)
	}
	return len(a.rows) > 0
}

// quirkRows of the operator n of the site of the statement s, with the arguments args, of the kind:
// whether SQLite shows the quirk of n, as [stmttest.QuirkQuery] says, for a row that the conditions
// of [stmttest.SiteGuard] hold for, and, if access.must, that the access reads: the range of the
// model's access, access.conditions, where access.modeled, and otherwise every condition that may
// serve the access. A subquery of the statement has the value that the engine computed, where it
// computed them.
func (p *pair) quirkRows(s stmt.Statement, site stmttest.ArithmeticSite, n stmt.Expression, kind error, args []any, access accessGuard) bool {
	p.t.Helper()
	_, where := tableWhere(s)
	if site.Sub > 0 {
		where = subqueriesIn(s)[site.Sub-1].Where
	}
	var guard []stmt.Condition
	if site.Set < 0 && access.must {
		if access.modeled && site.Sub == 0 {
			guard = slices.Clone(access.conditions)
		} else {
			for _, c := range where {
				if !c.Expression.Present() && !c.Value.Expression.HasColumn() {
					guard = append(guard, c)
				}
			}
		}
	}
	guard = append(guard, stmttest.SiteGuard(s, site)...)
	return p.rows(stmttest.QuirkQuery(s, site, n, kind, guard, p.computedTerms(), args))
}

// accessGuard of [pair.quirkRows]: whether a row must be one that the access reads, and, where
// modeled, the conditions of the range of the model's access, as [pair.modelGuard] gives them.
type accessGuard struct {
	must, modeled bool
	conditions    []stmt.Condition
}

// modelGuard of the select s with arithmetic, with the arguments args, which the engine ran without
// an error: the conditions that hold for a row that the model's access reads, as
// [stmttest.ChosenAccess] chooses it, and true; or nil and false where the pair has no model's
// access for s, as for a statement that is no select, one with a subquery or a LIMIT, or one that
// the model does not take. The model chooses from the conditions that may serve the access, as the
// rule of the engine says: those that are not computed for each row, each value of arithmetic as the
// value that SQLite gives it.
func (p *pair) modelGuard(s stmt.Statement, args []any) ([]stmt.Condition, bool) {
	sel, ok := s.(*stmt.Select)
	if !ok || sel.Subqueries != nil || sel.Limited() {
		return nil, false
	}
	return p.accessRange(sel, args)
}

// writeGuard of the update or the delete s with arithmetic, with the arguments args, which the
// instance of the pair ran by the rules: the conditions that hold for a row that the model's access
// reads, as [pair.accessRange] gives them for the select of its WHERE, since a write finds its rows
// as a select without ORDER BY and LIMIT does, and true; or nil and false for another statement, one
// with a subquery, whose access depends on its value, and an instance that reads every row.
func (p *pair) writeGuard(s stmt.Statement, args []any) ([]stmt.Condition, bool) {
	switch s.(type) {
	case *stmt.Update, *stmt.Delete:
	default:
		return nil, false
	}
	if subqueried(s) || p.i.e.ScansAll() {
		return nil, false
	}
	sel, selArgs := stmttest.SelectOf(s, args)
	if sel == nil {
		return nil, false
	}
	return p.accessRange(sel, selArgs)
}

// accessRange of the select s with arithmetic and without subqueries, with the arguments args: the
// conditions that hold for a row that the model's access reads, as [stmttest.ChosenAccess] chooses it
// from the conditions that may serve the access, as [pair.reducedSelect] gives them, and true; or
// nil and false where the model does not take it.
func (p *pair) accessRange(sel *stmt.Select, args []any) ([]stmt.Condition, bool) {
	t, o := p.t, p.o
	t.Helper()
	reduced, ok := p.reducedSelect(sel, args, true)
	if !ok {
		return nil, false
	}
	table, columns := o.columns(t, sel.Table.Value)
	_, indexes := o.tableIndexes(t, table, columns)
	a, ok := stmttest.ChosenAccess(columns, indexes, reduced, nil)
	if !ok {
		return nil, false
	}
	var primary []int
	for i, c := range columns {
		if c.PrimaryKey {
			primary = append(primary, i)
		}
	}
	key := primary
	if a.Index != "" {
		i := slices.IndexFunc(indexes, func(i stmttest.TableIndex) bool { return i.Name == a.Index })
		if i < 0 {
			return nil, false
		}
		key = nil
		for _, c := range indexes[i].Columns {
			key = append(key, c.Position)
		}
		for _, position := range primary {
			if !slices.Contains(key, position) {
				key = append(key, position)
			}
		}
	}
	name := func(position int) stmt.Name { return stmt.Name{Value: columns[position].Name} }
	var guard []stmt.Condition
	switch {
	case a.None:
		// The key is never NULL.
		return []stmt.Condition{{Column: name(primary[0]), Operator: stmt.OpIsNull}}, true
	case a.Full:
		return nil, true
	}
	for i, pin := range a.Pins {
		if pin == nil {
			guard = append(guard, stmt.Condition{Column: name(key[i]), Operator: stmt.OpIsNull})
			continue
		}
		guard = append(guard, stmt.Condition{Column: name(key[i]), Operator: stmt.OpEqual, Value: literalTerm(pin)})
	}
	if a.Lower != nil {
		op := map[bool]stmt.Operator{true: stmt.OpGreater, false: stmt.OpGreaterOrEqual}[a.LowerOpen]
		guard = append(guard, stmt.Condition{Column: name(key[len(a.Pins)]), Operator: op, Value: literalTerm(a.Lower)})
	}
	if a.Upper != nil {
		op := map[bool]stmt.Operator{true: stmt.OpLess, false: stmt.OpLessOrEqual}[a.UpperOpen]
		guard = append(guard, stmt.Condition{Column: name(key[len(a.Pins)]), Operator: op, Value: literalTerm(a.Upper)})
	}
	return guard, true
}

// reducedSelect of the select s with arithmetic and without subqueries, with the arguments args: the
// select of the conditions of s that may serve the access, as the rule of the engine says, those that
// are not computed for each row, in their order, and its ORDER BY and LIMIT, with no parameter: each
// value a literal, and, if constants, each expression without a column the literal of the value that
// SQLite gives it, or without those conditions otherwise. It reports false where SQLite gives an
// expression no value of one row.
func (p *pair) reducedSelect(s *stmt.Select, args []any, constants bool) (*stmt.Select, bool) {
	t, o := p.t, p.o
	t.Helper()
	reduced := *s
	reduced.Where = nil
	if s.Limit.Parameter > 0 {
		reduced.Limit = literalTerm(args[s.Limit.Parameter-1])
	}
	for _, c := range s.Where {
		if c.Expression.Present() || c.Value.Expression.HasColumn() || !constants && c.Value.Expression.Present() {
			continue
		}
		switch {
		case c.Value.Expression.Present():
			text, bound := stmttest.ExpressionText(c.Value.Expression, args)
			a := o.run(t, "select "+text, bound...)
			if a.err != "" || len(a.rows) != 1 {
				return nil, false
			}
			v, ok := literalOf(a.rows[0])
			if !ok {
				return nil, false
			}
			c.Value = v
		case c.Operator != stmt.OpIsNull && c.Operator != stmt.OpIsNotNull:
			c.Value = literalTerm(stmttest.BoundValue(c.Value, args))
		}
		reduced.Where = append(reduced.Where, c)
	}
	return &reduced, true
}

// literalTerm of the value v as SQLite binds it, as a literal of the statement pass.
func literalTerm(v any) stmt.Term {
	switch v := v.(type) {
	case int64:
		return stmt.Term{Value: stmt.Integer(v)}
	case float64:
		return stmt.Term{Value: stmt.Real(v)}
	case string:
		return stmt.Term{Value: stmt.Text(v)}
	case []byte:
		return stmt.Term{Value: stmt.Blob(v)}
	}
	return stmt.Term{Value: stmt.Null{}}
}

// literalOf the text of a value of a row of an answer of SQLite: the literal of its integer, its
// real, or NULL, and true, or false for another text.
func literalOf(text string) (stmt.Term, bool) {
	if text == "null" {
		return stmt.Term{Value: stmt.Null{}}, true
	}
	if f, ok := strings.CutPrefix(text, "real "); ok {
		r, err := strconv.ParseFloat(f, 64)
		return stmt.Term{Value: stmt.Real(r)}, err == nil
	}
	i, err := strconv.ParseInt(text, 10, 64)
	return stmt.Term{Value: stmt.Integer(i)}, err == nil
}

// valueTerm of the value v, as a literal of the statement pass.
func valueTerm(v *s4qlpb.Value) stmt.Term {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return stmt.Term{Value: stmt.Integer(v.GetInteger())}
	case s4qlpb.Value_Real_case:
		return stmt.Term{Value: stmt.Real(v.GetReal())}
	case s4qlpb.Value_Text_case:
		return stmt.Term{Value: stmt.Text(v.GetText())}
	case s4qlpb.Value_Blob_case:
		return stmt.Term{Value: stmt.Blob(v.GetBlob())}
	}
	return stmt.Term{Value: stmt.Null{}}
}

// justify the evaluation error ee of the engine for the statement s with the arguments args: what is
// wrong with it, or "", as [stmttest.Justify] says, with the values that the engine computed for the
// subqueries of s. For an update or a delete that the instance ran, a row of its WHERE must be one
// that the model's access reads too, as [pair.writeGuard] gives it, where it gives one, so that a
// write that reads rows outside its access fails here. A select needs no such guard, since
// [pair.checkAccess] compares its access with the model's.
func (p *pair) justify(s stmt.Statement, args []any, ee *expr.Error) string {
	if ee == nil {
		return "is no *expr.Error"
	}
	// An access that reads every row gives no condition, and so no guard.
	access, ok := p.writeGuard(s, args)
	if ok {
		p.count("arithmetic of a write whose evaluation error the model's access judges")
	}
	return stmttest.JustifyWithin(s, args, ee.Kind, ee.Op, ee.Span.Start, p.computedTerms(), access, p.rows)
}

// changedBefore reports whether the update u, with the arguments args, which failed with the
// evaluation error ee at an operator of its SET, changed a row before the row that failed, by
// SQLite's rows: whether its WHERE holds for a row, and the first such row in the order of the key,
// which the update changes first, shows the quirk of no operator of SET. An update with a subquery,
// and one of a table whose key has more than one column, are left out.
func (p *pair) changedBefore(u *stmt.Update, args []any, ee *expr.Error) bool {
	t, o := p.t, p.o
	t.Helper()
	if ee == nil || u.Subqueries != nil {
		return false
	}
	var key []string
	_, columns := o.columns(t, u.Table.Value)
	for _, c := range columns {
		if c.PrimaryKey {
			key = append(key, stmttest.Quote(c.Name))
		}
	}
	if len(key) != 1 {
		return false
	}
	first, firstArgs := stmttest.SelectText(&stmt.Select{Table: u.Table, Where: u.Where}, args, " order by "+key[0]+" limit 1")
	if !p.rows(first, firstArgs) {
		return false
	}
	inSet := false
	for _, site := range stmttest.ArithmeticSites(u) {
		if site.Set < 0 {
			continue
		}
		for _, n := range stmttest.Operators(site.Root) {
			if n.Span.Start == ee.Span.Start && n.Kind.String() == ee.Op.String() {
				inSet = true
			}
			for _, kind := range stmttest.EvaluationKinds {
				quirk, bound := stmttest.Quirk(n, kind, args)
				if p.rows("select 1 from ("+first+") where "+quirk, append(slices.Clone(firstArgs), bound...)) {
					return false
				}
			}
		}
	}
	return inSet
}

// due is what evaluation error was due for the statement s, with the arguments args, which the engine
// ran without one, or "": each expression without a column of the statement, and of a subquery that
// it computed, is computed, and so is each expression of a condition for a row that the access reads
// and that every condition before it holds for, and each of SET for a row that the WHERE holds for,
// so SQLite's result of none of their operators may show a quirk there, as [pair.quirkRows] says.
// The rows that the access reads are those of the model's access for a select, as [pair.modelGuard]
// gives it, and otherwise those that every condition that may serve the access holds for. A subquery
// reads only up to its first row, and a select with LIMIT only up to its last, which their order
// decides, so neither has a due error for a row. A subquery with LIMIT 0 gives NULL and computes
// nothing, also no expression without a column.
func (p *pair) due(s stmt.Statement, args []any) string {
	sel, isSelect := s.(*stmt.Select)
	computed := !subqueried(s) || p.computed != nil
	subs := subqueriesIn(s)
	access := accessGuard{must: true}
	if access.conditions, access.modeled = p.modelGuard(s, args); access.modeled {
		p.count(modeledDue)
	}
	for _, site := range stmttest.ArithmeticSites(s) {
		switch {
		case !site.Root.HasColumn() && site.Sub > 0 && (!computed || limitZero(subs[site.Sub-1].Limit, args)):
			continue
		case site.Root.HasColumn() && (site.Sub > 0 || !computed || isSelect && sel.Limited()):
			continue
		}
		for _, n := range stmttest.Operators(site.Root) {
			for _, kind := range stmttest.EvaluationKinds {
				if p.quirkRows(s, site, n, kind, args, access) {
					return fmt.Sprintf("SQLite shows the quirk of %v of %v at offset %v for a row that the rule evaluates", kind, n.Kind, n.Span.Start)
				}
			}
		}
	}
	return ""
}

// limitZero reports whether the term of a LIMIT, with the arguments args, is the integer 0; a LIMIT
// that the select does not have is not.
func limitZero(limit stmt.Term, args []any) bool {
	return (limit.Value != nil || limit.Parameter > 0) && stmttest.BoundValue(limit, args) == any(int64(0))
}

// arithmeticRows of the table n of [arithmeticSetUp], with the edges of each type and NULL.
var arithmeticRows = []stmttest.Call{
	{Src: "insert into n values (1, 0, 10, 0.5, 'a', x'01', 1)", Supported: true},
	{Src: "insert into n values (2, 1, 20, -2.5, 'b', x'02', 2)", Supported: true},
	{Src: "insert into n values (3, -1, null, 1e308, 'c', null, 3)", Supported: true},
	{Src: "insert into n values (4, 9223372036854775807, 30, null, null, x'04', 4)", Supported: true},
	{Src: "insert into n values (5, -9223372036854775808, 40, 2.0, 'e', x'05', 5)", Supported: true},
	{Src: "insert into n values (6, null, 50, -0.0, 'f', x'06', 6)", Supported: true},
}

// arithmeticSetUp is the table n of the differential tests of arithmetic: a column of each type, an
// index on an INT and one on a REAL column, and [arithmeticRows]; and the tables of [keyChanges].
var arithmeticSetUp = slices.Concat([]stmttest.Call{
	{Src: "create table n (k int primary key, i int, j integer, r real, s text, b blob, u int not null) strict, without rowid", Supported: true},
	{Src: "create index n_i on n (i)", Supported: true},
	{Src: "create index n_r on n (r desc)", Supported: true},
}, arithmeticRows, []stmttest.Call{
	// The tables of [keyChanges]: an integer key and a real one, each with an index that runs with the
	// key and one that runs against it.
	{Src: "create table m (k int primary key, a int, b int) strict, without rowid", Supported: true},
	{Src: "create index m_a on m (a)", Supported: true},
	{Src: "create index m_b on m (b)", Supported: true},
	{Src: "insert into m values (1, 10, -1)", Supported: true},
	{Src: "insert into m values (2, 20, -2)", Supported: true},
	{Src: "insert into m values (3, 30, -3)", Supported: true},
	{Src: "insert into m values (4, 40, -4)", Supported: true},
	{Src: "create table q (k real primary key, a real, b real) strict, without rowid", Supported: true},
	{Src: "create index q_a on q (a)", Supported: true},
	{Src: "create index q_b on q (b)", Supported: true},
	{Src: "insert into q values (1.0, 10.0, -1.0)", Supported: true},
	{Src: "insert into q values (2.0, 20.0, -2.0)", Supported: true},
	{Src: "insert into q values (3.0, 30.0, -3.0)", Supported: true},
	{Src: "insert into q values (4.0, 40.0, -4.0)", Supported: true},
})

// keyChanges are calls of [arithmeticCases] that change the key with arithmetic or a copy of a
// column, on the tables m, of an integer key, and q, of a real one, of [arithmeticSetUp]: each SET
// without a WHERE, with one on the key, and with one on each index, with and without RETURNING, each
// in a transaction that is rolled back, so that each starts from the same rows. SQLite changes the
// rows of an update that changes the key in the order of the key, as s4ql does, whatever the access,
// so the two give the same `UNIQUE constraint failed` and the same rows.
func keyChanges() []stmttest.Call {
	var calls []stmttest.Call
	for _, table := range []string{"m", "q"} {
		for _, set := range []string{"k = k + 1", "k = k - 1", "k = -k", "k = 6 - k", "k = k * 2", "k = k + 10", "k = k - 10",
			"k = a", "k = b", "a = k, k = a", "k = k + 1, a = k", "a = k + 1, k = a / 10"} {
			for _, where := range []string{"", " where k > 1", " where a >= 20", " where b <= -2", " where b >= -3 and a > 10"} {
				for _, returning := range []string{"", " returning *"} {
					calls = append(calls, stmttest.Call{Src: "begin"}, stmttest.Call{Src: "update " + table + " set " + set + where + returning}, stmttest.Call{Src: "rollback"})
				}
			}
		}
	}
	return append(calls, stmttest.Call{Src: "update m set k = k + 10 returning *"}, stmttest.Call{Src: "update q set k = -k"})
}

// arithmeticCases of the differential tests of arithmetic, run in their order on the rows of
// [arithmeticSetUp], each through Exec and through Query, and then those of [keyChanges]. A call whose
// text is "begin", "begin read only", "commit", or "rollback" begins or ends a transaction of the pair.
var arithmeticCases = slices.Concat([]stmttest.Call{
	// Each operator, in SET and on both sides of a comparison, with constants and per row.
	{Src: "select k from n where i + 1 = 2"},
	{Src: "select k from n where 2 = i + 1"},
	{Src: "select k from n where j - 5 > 10"},
	{Src: "select k from n where 10 >= j - 5"},
	{Src: "select k from n where j * 2 <= 40"},
	{Src: "select k from n where j / 3 < 10"},
	{Src: "select k from n where r * 2 > 0.0"},
	{Src: "select k from n where r / 2 != 0.25"},
	{Src: "select k from n where -j < -20"},
	{Src: "select k from n where -r <= 0.0"},
	{Src: "select k from n where k = 1 + 1"},
	{Src: "select k from n where k > ? * 2", Args: []any{int64(1)}},
	{Src: "select k from n where k < 10 / ?", Args: []any{int64(3)}},
	{Src: "select k from n where k = ? - 1", Args: []any{nil}},
	{Src: "select k from n where r = 1.5 - 1", Args: nil},
	{Src: "select k from n where i = j / 10 - 1"},
	{Src: "select k from n where j / 10 = k"},
	{Src: "select k from n where j * 2 < u * 15"},
	{Src: "select k from n where r * 2 < j * 1.0"},
	{Src: "select k from n where k = u"},
	{Src: "select k from n where j > i"},
	{Src: "select k from n where s = s and b <= b"},
	{Src: "update n set j = j + 1 where k = 1"},
	{Src: "update n set j = j - k, u = u * 2 where k > 1 returning *"},
	{Src: "update n set r = r / 2 returning k, r"},
	{Src: "update n set r = -r where k = 2 returning r"},
	{Src: "update n set j = -j * ? where k < 3", Args: []any{int64(3)}},
	{Src: "update n set r = j * 0.5, j = k + 100 where k = 2 returning *"},
	{Src: "update n set i = j where k = 6"},
	{Src: "update n set s = s, b = b, r = r where k = 1 returning *"},
	{Src: "delete from n where j - 100 = k returning k"},
	{Src: "insert into n values (2, 1, 20, -2.5, 'b', x'02', 2)"},

	// Every pair of types: an expression of each type compared with and set into a column of each.
	{Src: "select k from n where i = 1.0 * 1"},
	{Src: "select k from n where r = 1 + 1"},
	{Src: "select k from n where s = 1 + 1"},
	{Src: "select k from n where b = 1.5 * 2"},
	{Src: "select k from n where s = k + 1"},
	{Src: "select k from n where r = ? + 1", Args: []any{nil}},
	{Src: "select k from n where k + 0 = 'a'"},
	{Src: "select k from n where k + 0 = ?", Args: []any{"a"}},
	{Src: "select k from n where k + 0 = 2.0"},
	{Src: "select k from n where k + 0.0 = 2.0"},
	{Src: "select k from n where k + 0 = (select r from n where k = 1)"},
	{Src: "select k from n where k + 0 = (select j from n where k = 1)"},
	{Src: "select k from n where k * 1.0 = r * 1"},
	{Src: "select k from n where s + 1 = 1"},
	{Src: "select k from n where k + ? = 1", Args: []any{[]byte("x")}},
	{Src: "select k from n where k = r"},
	{Src: "select k from n where s = b"},
	{Src: "update n set i = r * 2"},
	{Src: "update n set r = i + 1"},
	{Src: "update n set s = i + 1"},
	{Src: "update n set b = 1 + 1"},
	{Src: "update n set i = ? + 1", Args: []any{nil}},
	{Src: "update n set u = ? + 1 where k = 1", Args: []any{nil}},
	{Src: "update n set u = i + 1"},
	{Src: "update n set i = s"},
	{Src: "update n set j = i where k = 1 returning j"},
	{Src: "update n set r = j"},

	// The rows again, as the updates before set i to NULL on every row.
	{Src: "delete from n"},
}, arithmeticRows, []stmttest.Call{
	// Overflow, division by zero, and a result that is not finite: in SET, in a constant, and in WHERE,
	// guarded and unguarded, with and without an index, with a LIMIT.
	{Src: "update n set i = i + 1"},
	{Src: "update n set i = i - 1 where k = 5"},
	{Src: "update n set j = j / (k - 2)"},
	{Src: "update n set r = r * 10"},
	{Src: "update n set j = j + 1, i = i * 2 where k < 5"},
	{Src: "update n set i = 1 where k = ? + 1", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set i = ? * 2 where k = 99", Args: []any{int64(math.MaxInt64)}},
	{Src: "select k from n where k = 1 / ?", Args: []any{int64(0)}},
	{Src: "select k from n where k = 1 / ? and k = 99", Args: []any{0.0}},
	{Src: "select k from n where k = ? * 1e308", Args: []any{10.0}},
	{Src: "select k from n where i + 1 > 0"},
	{Src: "select k from n where k <> 4 and i + 1 > 0"},
	{Src: "select k from n where i + 1 > 0 and k <> 4"},
	{Src: "select k from n where k < 4 and i + 1 > 0"},
	{Src: "select k from n where i + 1 > 0 and k < 4"},
	{Src: "select k from n where i < 2 and i - 1 < 0"},
	{Src: "select k from n where i - 1 < 0 and i > -5"},
	{Src: "select k from n where j / (k - 3) > 0"},
	{Src: "select k from n where k <> 3 and j / (k - 3) > 0"},
	{Src: "select k from n where r * 10 > 0"},
	{Src: "select k from n where r < 1e300 and r * 10 > 0"},
	{Src: "select k from n where j / (k - 3) > 0 order by k limit 1"},
	{Src: "select k from n where j / (k - 3) > 0 order by k desc limit 1"},
	{Src: "select k from n where r * 10 > 0 order by r limit 2"},
	{Src: "delete from n where i * 2 = 0"},
	{Src: "delete from n where k = 4 and i * 2 = 0"},

	// A subquery: with arithmetic inside its WHERE, beside it, and failing in it.
	{Src: "select k from n where k = (select k from n where j + 0 = 20)"},
	{Src: "select k from n where k + 0 = (select k from n where u * 2 = ? order by k limit 1)", Args: []any{int64(4)}},
	{Src: "update n set u = u + 1 where k = (select k from n where j - 1 >= ? order by j limit 1) returning k, u", Args: []any{int64(25)}},
	{Src: "select k from n where k = (select k from n where i + 1 > 0 order by k limit 1)"},
	{Src: "select k from n where k = (select k from n where k = 1 + ?)", Args: []any{int64(math.MaxInt64)}},
	{Src: "delete from n where k = (select k from n where r * 10 > 0 order by k desc limit 1)"},

	// In a transaction, rolled back and committed, and usable after a failure.
	{Src: "begin"},
	{Src: "update n set j = j * 2 where k < 3 returning *"},
	{Src: "update n set i = i + 1"},
	{Src: "select k, j from n where j / 2 > 0"},
	{Src: "rollback"},
	{Src: "begin"},
	{Src: "update n set u = u + 10 returning u"},
	{Src: "update n set r = r * 10"},
	{Src: "delete from n where u - 10 = 1"},
	{Src: "commit"},
	{Src: "select * from n"},

	// In a read-only transaction, an update or a delete computes no constant and reads no row, and
	// gives SQLite's error for the write; a select computes as outside one.
	{Src: "begin read only"},
	{Src: "delete from n where k = 1 + ?", Args: []any{int64(math.MaxInt64)}},
	{Src: "delete from n where k = 99 and i = ? * 2", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set i = ? * 2", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set i = ? * 2 where k = 1", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set j = j + 1 where k = 1 + ?", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set i = i + 1"},
	{Src: "update n set k = k + 1"},
	{Src: "delete from n where j / (k - 3) > 0"},
	{Src: "delete from n where k = (select k from n where j = 1 + ?)", Args: []any{int64(math.MaxInt64)}},
	{Src: "update n set i = r * 2"},
	{Src: "update n set j = 1 where s = k + 1"},
	{Src: "select k from n where k = 1 + ?", Args: []any{int64(math.MaxInt64)}},
	{Src: "select k from n where i + 1 > 0"},
	{Src: "select k from n where j / 10 = k"},
	{Src: "rollback"},

	// The same text again with other arguments, as a prepared statement runs.
	{Src: "update n set u = u + ? where k = ? returning u", Args: []any{int64(1), int64(2)}},
	{Src: "update n set u = u + ? where k = ? returning u", Args: []any{int64(math.MaxInt64), int64(2)}},
	{Src: "update n set u = u + ? where k = ? returning u", Args: []any{nil, int64(2)}},
	{Src: "update n set u = u + ? where k = ? returning u", Args: []any{2.5, int64(2)}},
	{Src: "update n set u = u + ? where k = ? returning u", Args: []any{int64(-1), int64(2)}},

	// Negative zero stored and given.
	{Src: "update n set r = -0.0 * 1 where k = 2 returning r"},
	{Src: "update n set r = r * -1 returning r"},
	{Src: "select k from n where r = 0.0 * -1"},
}, keyChanges())

func TestPair_arithmetic(t *testing.T) {
	run := func(t *testing.T, p *pair, i *instance, calls []stmttest.Call) {
		t.Helper()
		for _, c := range arithmeticSetUp {
			is.Equal(t, "", p.step(c), c.Src)
		}
		var log []string
		for _, c := range calls {
			switch c.Src {
			case "begin", "begin read only":
				readOnly := c.Src != "begin"
				for _, conn := range []*engine.Conn{i.c, p.other.c} {
					is.NotError(t, map[bool]func(context.Context) error{false: conn.Begin, true: conn.BeginReadOnly}[readOnly](t.Context()))
				}
				for _, s := range oracleBegin(readOnly) {
					is.Equal(t, "", p.o.run(t, s).err, s)
				}
				p.inTx, p.readOnly = true, readOnly
				log = append(log, "-- "+c.Src)
				continue
			case "commit", "rollback":
				for _, conn := range []*engine.Conn{i.c, p.other.c} {
					if c.Src == "commit" {
						is.NotError(t, conn.Commit(t.Context()))
					} else {
						is.NotError(t, conn.Rollback(t.Context()))
					}
				}
				for _, s := range oracleEnd(p.readOnly, c.Src == "commit") {
					is.Equal(t, "", p.o.run(t, s).err, s)
				}
				p.inTx, p.readOnly = false, false
				log = append(log, "-- "+c.Src)
				continue
			}
			// A read runs through Exec and through Query. A write runs once, as each call is one, through
			// Query where it has RETURNING, so that its rows are compared, and through Exec otherwise.
			ways := []bool{false, true}
			switch s, _ := stmt.Check(c.Src); s := s.(type) {
			case *stmt.Update:
				ways = []bool{s.Returning.Present()}
			case *stmt.Delete:
				ways = []bool{s.Returning.Present()}
			case *stmt.Insert:
				ways = []bool{s.Returning.Present()}
			}
			for _, query := range ways {
				c.Query = query
				log = append(log, fmt.Sprintf("%v -- with %#v, as a query: %v", c.Src, c.Args, query))
				if failure := p.step(c); failure != "" {
					t.Fatalf("after\n%v\n%v", strings.Join(log[:len(log)-1], "\n"), failure)
				}
			}
		}
		want := p.o.state(t)
		is.Equal(t, want, i.state(), strings.Join(log, "\n"))
		is.Equal(t, want, p.other.state(), strings.Join(log, "\n"))
		if i.memory == nil {
			i.restart(true, engine.Options{})
			is.Equal(t, want, i.state(), "after a replay into an empty cache")
		}
	}

	t.Run("should give SQLite's answer or an error that SQLite's own result justifies, for each case, on the core and in memory", func(t *testing.T) {
		for _, memory := range []bool{false, true} {
			i := newInstance(t, engine.Options{})
			if memory {
				i = newMemoryInstance(t)
			}
			y := tally{}
			run(t, newPair(t, i, y), i, arithmeticCases)
			for _, kind := range []string{"excused: " + excusedArithmeticType, "excused: " + excusedEvaluation, comparedWithSQLite, "reached: " + modeledDue} {
				is.True(t, y[kind] > 0, kind)
			}
		}
	})

	t.Run("should give SQLite's answer or a justified error for generated statements with arithmetic", func(t *testing.T) {
		n := 100
		if slow() {
			n = 8
		}
		y := tally{}
		for seed := range uint64(n) {
			i := newMemoryInstance(t)
			if seed%4 == 0 {
				i = newInstance(t, engine.Options{})
			}
			run(t, newPair(t, i, y), i, arithmeticCalls(rand.New(rand.NewPCG(seed, 0xa1)), 40))
		}
		t.Log(y)
		for _, kind := range []string{"excused: " + excusedArithmeticType, "excused: " + excusedEvaluation, comparedWithSQLite, "reached: " + modeledDue} {
			is.True(t, y[kind] > 0, kind)
		}
	})
}

// arithmeticCalls draws n calls of arithmetic on the table n of [arithmeticSetUp] from r: selects,
// updates, which set the key too, and deletes, with a subquery one time in ten, in a transaction one
// time in ten, read-only one time in two of those, with expressions over the columns, literals at the
// edges of each type, and arguments. Each expression
// draws its operands from one class, integer or real, and one operand in twelve from the other, or
// from a TEXT or BLOB column or a text argument, so that most statements run and every pair of types
// meets.
func arithmeticCalls(r *rand.Rand, n int) []stmttest.Call {
	type pool struct {
		columns, literals []string
		arguments         []any
	}
	pools := []pool{
		{[]string{"k", "i", "j", "u"}, []string{"0", "1", "2", "-1", "3", "9223372036854775807", "-9223372036854775808", "4611686018427387904"},
			[]any{int64(0), int64(1), int64(-2), int64(math.MaxInt64), int64(math.MinInt64), nil}},
		{[]string{"r"}, []string{"0.5", "-2.5", "1e308", "0.0", "-0.0", "2.0", "1e-300"}, []any{0.5, math.Copysign(0, -1), 1e308, -1e308, 2.0, nil}},
	}
	var args []any
	var expression func(class, depth int, column bool) string
	leaf := func(class int, column bool) string {
		p := pools[class]
		if r.IntN(12) == 0 {
			if r.IntN(3) == 0 {
				return []string{"s", "b"}[r.IntN(2)]
			}
			p = pools[1-class]
		}
		switch k := r.IntN(10); {
		case column || k < 5:
			return p.columns[r.IntN(len(p.columns))]
		case k < 8:
			return p.literals[r.IntN(len(p.literals))]
		}
		a := p.arguments[r.IntN(len(p.arguments))]
		if r.IntN(40) == 0 {
			a = "1"
		}
		args = append(args, a)
		return "?"
	}
	expression = func(class, depth int, column bool) string {
		if depth == 0 || r.IntN(3) == 0 {
			return leaf(class, column)
		}
		if r.IntN(8) == 0 {
			operand := expression(class, depth-1, column)
			if strings.ContainsAny(operand[:1], "-0123456789") {
				// A literal takes no negation of its own, and a negation no second sign.
				return operand
			}
			return "-" + operand
		}
		left := expression(class, depth-1, column && r.IntN(2) == 0)
		op := []string{"+", "-", "*", "/"}[r.IntN(4)]
		right := expression(class, depth-1, column && !strings.ContainsAny(left, "kijrusb"))
		if op == "/" && strings.Trim(right, "-0.") == "" {
			right = "2"
		}
		return "(" + left + " " + op + " " + right + ")"
	}
	condition := func() string {
		op := []string{"=", "!=", "<", "<=", ">", ">="}[r.IntN(6)]
		class := r.IntN(2)
		column := pools[class].columns[r.IntN(len(pools[class].columns))]
		switch r.IntN(5) {
		case 0:
			return column + " " + op + " " + expression(class, 2, false)
		case 1:
			return expression(class, 2, true) + " " + op + " " + leaf(class, false)
		case 2:
			return expression(class, 2, false) + " " + op + " " + column
		case 3:
			return column + " " + op + " " + pools[class].columns[r.IntN(len(pools[class].columns))]
		}
		return expression(class, 2, true) + " " + op + " " + expression(class, 2, r.IntN(2) == 0)
	}
	where := func() string {
		w := " where " + condition()
		for r.IntN(3) == 0 {
			w += " and " + condition()
		}
		return w
	}
	assignment := func(column string) string {
		class := 0
		if column == "r" {
			class = 1
		}
		if r.IntN(6) == 0 {
			return column + " = " + pools[class].columns[r.IntN(len(pools[class].columns))]
		}
		return column + " = " + expression(class, 2, r.IntN(4) != 0)
	}
	var calls []stmttest.Call
	for len(calls) < n {
		args = nil
		var src string
		switch k := r.IntN(10); {
		case k < 4:
			src = "select k from n" + where()
			if r.IntN(4) == 0 {
				src += " order by k" + []string{"", " desc"}[r.IntN(2)] + " limit " + fmt.Sprint(r.IntN(3))
			}
		case k < 5:
			src = "select k from n where k = (select k from n" + where() + " order by j limit 1)"
		case k < 8:
			set := []string{"i", "j", "r", "u", "k"}
			first := r.IntN(len(set))
			src = "update n set " + assignment(set[first])
			if r.IntN(3) == 0 {
				src += ", " + assignment(set[(first+1+r.IntN(len(set)-1))%len(set)])
			}
			if r.IntN(3) != 0 {
				src += where()
			}
			if r.IntN(2) == 0 {
				src += " returning *"
			}
		default:
			src = "delete from n" + where()
			if r.IntN(2) == 0 {
				src += " returning k"
			}
		}
		if r.IntN(10) == 0 {
			calls = append(calls, stmttest.Call{Src: []string{"begin", "begin read only"}[r.IntN(2)]}, stmttest.Call{Src: src, Args: args}, stmttest.Call{Src: []string{"commit", "rollback"}[r.IntN(2)]})
			continue
		}
		calls = append(calls, stmttest.Call{Src: src, Args: args})
	}
	return calls
}

// reachArithmetic counts with count what the statement s with arithmetic, whose answer of the engine
// is got, reaches: each of its expressions by whether it has a column, where it is, and the kind of
// the statement, by the outcome, which names the kind of an evaluation error.
func reachArithmetic(s stmt.Statement, got answer, count func(string)) {
	outcome := "ok"
	switch {
	case got.eval != nil:
		outcome = "an evaluation error: " + got.eval.Kind.Error()
	case got.err != "":
		outcome = messageKind(got.err)
	}
	kind := strings.ToLower(strings.TrimPrefix(fmt.Sprintf("%T", s), "*stmt."))
	for _, site := range stmttest.ArithmeticSites(s) {
		where := "WHERE"
		switch {
		case site.Set >= 0:
			where = "SET"
		case site.Sub > 0:
			where = "a subquery"
		}
		constant := map[bool]string{true: "with a column", false: "without a column"}[site.Root.HasColumn()]
		count(fmt.Sprint("arithmetic ", constant, " in ", where, " of a ", kind, ", ", outcome))
	}
}

// reachOfArithmetic counts what the call c of the statement s with arithmetic, whose answer of the
// engine is got, reaches, on the database of SQLite before the call: what [reachArithmetic] counts,
// and the cases of the generated checks of arithmetic, each under "arithmetic: ". An evaluation error
// by its kind, where it is, in SET, in WHERE, or in a subquery, whether its expression has a column,
// and in a write transaction, which [pair.reach] follows. A select with LIMIT that failed on a row, and
// one that gave its rows where a row that it did not read would fail, as SQLite's quirk shows. A
// condition computed for each row that rules a row out that the others of its select hold for. An
// update that sets a column from itself, one whose SET reads a column that it sets before, and one
// that computes its key from its row, each by whether it changed rows or broke the key. And what
// [reachCompared] counts.
func (p *pair) reachOfArithmetic(c stmttest.Call, s stmt.Statement, got answer) {
	t, o := p.t, p.o
	t.Helper()
	reachArithmetic(s, got, p.count)
	args := oracleArgs(c.Args)
	if len(args) != s.Parameters() || o.prepare(t, c.Src) != nil {
		return
	}
	reachCompared(s, got, p.count)
	if ee := got.eval; ee != nil {
		for _, site := range stmttest.ArithmeticSites(s) {
			if !slices.ContainsFunc(stmttest.Operators(site.Root), func(n stmt.Expression) bool { return n.Span == ee.Span && n.Kind == ee.Op }) {
				continue
			}
			where := "WHERE"
			switch {
			case site.Set >= 0:
				where = "SET"
			case site.Sub > 0:
				where = "a subquery"
			}
			constant := map[bool]string{true: "with a column", false: "without a column"}[site.Root.HasColumn()]
			p.count(fmt.Sprint("arithmetic: ", ee.Kind.Error(), " in ", where))
			p.count(fmt.Sprint("arithmetic: ", ee.Kind.Error(), " in ", where, " of an expression ", constant))
			if sel, ok := s.(*stmt.Select); ok && sel.Limited() && site.Root.HasColumn() && site.Sub == 0 {
				p.count("arithmetic: select with LIMIT that failed on a row")
			}
			break
		}
		if p.inTx && !p.readOnly {
			p.count("arithmetic: evaluation error in a write transaction")
			p.evaluatedInTx = true
		}
		return
	}
	if got.err != "" {
		return
	}
	switch s := s.(type) {
	case *stmt.Select:
		if s.Subqueries != nil || !c.Query {
			return
		}
		if s.Limited() && len(got.rows) > 0 {
			for _, site := range stmttest.ArithmeticSites(s) {
				if !site.Root.HasColumn() {
					continue
				}
				if slices.ContainsFunc(stmttest.Operators(site.Root), func(n stmt.Expression) bool {
					return slices.ContainsFunc(stmttest.EvaluationKinds, func(kind error) bool { return p.quirkRows(s, site, n, kind, args, accessGuard{}) })
				}) {
					p.count("arithmetic: select with LIMIT that gave rows where a row that it did not read fails")
					break
				}
			}
		}
		for i, c := range s.Where {
			if !c.Expression.Present() && !c.Value.Expression.HasColumn() {
				continue
			}
			others := slices.Concat(s.Where[:i], s.Where[i+1:])
			all, allArgs := stmttest.FoundQuery(s.Table, others, nil, args)
			one, oneArgs := stmttest.FoundQuery(s.Table, slices.Concat(others, []stmt.Condition{c}), nil, args)
			if all := o.run(t, all, allArgs...); len(all.rows) > 0 {
				what := "rules out every row that the others hold for"
				if len(o.run(t, one, oneArgs...).rows) > 0 {
					what = "holds for a row that the others hold for"
				}
				p.count("arithmetic: condition computed for each row that " + what)
			}
		}
	case *stmt.Update:
		outcome := "that changed rows"
		switch {
		case got.err == "" && p.i.changes == 0:
			return
		case strings.Contains(got.err, "UNIQUE constraint failed"):
			outcome = "that broke the key"
		}
		_, columns := o.columns(t, s.Table.Value)
		for i, a := range s.Set {
			names := expressionColumns(a.Value.Expression)
			if a.Value.Expression.Kind == stmt.ExprColumn && a.Value.Expression.Column.Fold() != a.Column.Fold() {
				p.count("arithmetic: update that copies another column alone " + outcome)
			}
			if slices.ContainsFunc(names, func(n stmt.Name) bool { return n.Fold() == a.Column.Fold() }) && a.Value.Expression.Kind != stmt.ExprColumn {
				p.count("arithmetic: update that sets a column from itself " + outcome)
				if position := stmttest.ColumnOf(columns, a.Column); position >= 0 && columns[position].PrimaryKey {
					p.count("arithmetic: update that computes its key from its row " + outcome)
				}
			}
			if slices.ContainsFunc(s.Set[:i], func(b stmt.Assignment) bool {
				return slices.ContainsFunc(names, func(n stmt.Name) bool { return n.Fold() == b.Column.Fold() })
			}) {
				p.count("arithmetic: update whose SET reads a column that it sets before " + outcome)
			}
		}
	}
}

// classRefusals are the refusals of the classes of arithmetic that [reachCompared] counts, by the
// kind that it counts each as.
var classRefusals = []struct {
	pattern *regexp.Regexp
	kind    string
}{
	{regexp.MustCompile(`^s4ql: not supported: (an? [a-z]+ literal|argument \d+, which is [a-z ]+,) compared with an expression of type [a-z]+$`), "a plain value compared with an expression"},
	{regexp.MustCompile(`^s4ql: not supported: subquery \d+, of the column .+, compared with an expression of type [a-z]+$`), "a subquery compared with an expression"},
	{regexp.MustCompile(`^s4ql: not supported: an expression of type null compared with the column .+ of type (TEXT|BLOB)$`), "an expression of type null compared with a TEXT or BLOB column"},
	{regexp.MustCompile(`^s4ql: not supported: an expression of type null for the column .+ of type (TEXT|BLOB)$`), "an expression of type null for a TEXT or BLOB column"},
	{regexp.MustCompile(`^s4ql: not supported: an expression of type integer for the column .+ of type REAL$`), "an expression of type integer for a REAL column"},
}

// reachCompared counts with count, under "arithmetic: ", the comparisons of the statement s with
// arithmetic, which SQLite prepares, whose answer of the engine is got, of an expression that a
// condition tests, of the WHERE of the statement or of a subquery, with a plain value, a literal or a
// ?, and with a subquery, by whether the engine gave no error, and the refusals of the classes of
// [classRefusals] that the engine gave.
func reachCompared(s stmt.Statement, got answer, count func(string)) {
	if got.err == "not supported" {
		for _, r := range classRefusals {
			if r.pattern.MatchString(got.message) {
				count("arithmetic: refused " + r.kind)
			}
		}
	}
	if got.err != "" {
		return
	}
	_, where := tableWhere(s)
	for _, q := range subqueriesIn(s) {
		where = append(slices.Clone(where), q.Where...)
	}
	for _, c := range where {
		switch {
		case !c.Expression.Present() || c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
		case c.Value.Subquery > 0:
			count("arithmetic: expression compared with a subquery, ok")
		case !c.Value.Expression.Present():
			count("arithmetic: expression compared with a plain value, ok")
		}
	}
}

// reachServed counts how the constants of the select s with arithmetic serve its access, which is
// want, as [pair.checkAccess] checked it, on the table of the columns and of the indexes: where the
// model chooses another access without the conditions whose value is an expression without a column,
// by whether want gets one key, pins columns of an index, or bounds the key or an index.
func (p *pair) reachServed(s *stmt.Select, args []any, columns []stmttest.TableColumn, indexes []stmttest.TableIndex, want stmttest.Access) {
	without, ok := p.reducedSelect(s, args, false)
	if !ok {
		return
	}
	if other, ok := stmttest.ChosenAccess(columns, indexes, without, nil); !ok || other.Same(want) {
		return
	}
	of := "the key"
	if want.Index != "" {
		of = "an index"
	}
	switch {
	case want.None:
		p.count("arithmetic: constant expression that makes the access read no row")
	case want.Get:
		p.count("arithmetic: constant expression that serves a get of one key")
	case want.Lower != nil || want.Upper != nil:
		p.count("arithmetic: constant expression that serves a bound of " + of)
	case len(want.Pins) > 0:
		p.count("arithmetic: constant expression that serves the pins of " + of)
	}
}

func TestNewPlan_arithmetic(t *testing.T) {
	t.Run("should plan arithmetic as an Arithmetic, with each binary operator in parentheses, and a condition that tests an expression by it", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"update t set v = v + 1, w = -(? * 2.5) where k = 1 - ? returning *", "update t set v = (v + 1), w = -(?1 * 2.5) returning *\n  filter k = (1 - ?2)\n    scan t\n"},
			{"select * from t where a * 2 < b * 3 and 4 >= a - -1", "query\n  project *\n    filter (a * 2) < (b * 3) and (a - -1) <= 4\n      scan t\n"},
			{"delete from t where a = b and k + 1 = (select k from u where x / 2 = ?)", "delete from t\n  filter a = b and (k + 1) = (subquery 1)\n    scan t\n  subquery 1\n    project k\n      filter (x / 2) = ?1\n        scan u\n"},
			{"update t set a = b", "update t set a = b\n  scan t\n"},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.Equal(t, c.want, engine.Format(engine.NewPlan(s)), c.src)
		}
	})
}

func TestEngine_arithmetic(t *testing.T) {
	setUp := func(t *testing.T, i *instance) {
		t.Helper()
		for _, src := range []string{
			"create table t (k int primary key, a int, b int, r real, s text, n int not null) strict, without rowid",
			"create index t_a on t (a)",
			"insert into t values (1, 10, 0, 1.5, 'x', 1)",
			"insert into t values (2, 20, 2, 2.5, 'y', 2)",
			"insert into t values (3, 30, 3, null, null, 3)",
			"insert into t values (4, null, 4, 4.5, 'z', 4)",
		} {
			is.Equal(t, "", i.exec(src).err, src)
		}
	}
	// read runs the query src with args, and gives the access of its event, the rows that it read,
	// and its rows, or its error.
	read := func(t *testing.T, i *instance, h *events, src string, args ...any) string {
		t.Helper()
		got := i.queryValues(src, values(args))
		if got.err != "" {
			return got.message
		}
		e, _ := h.last(t)
		access := e[engine.AttrAccess]
		if e[engine.AttrIndex] != "" {
			access += " " + e[engine.AttrIndex]
		}
		return fmt.Sprint(access, " reads ", e[engine.AttrRowsRead], " gives ", strings.Join(slices.Sorted(slices.Values(got.rows)), " "))
	}

	t.Run("should serve the key and the bounds of an index with an expression without a column, as with an argument, and read by neither with one with a column", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		setUp(t, i)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{"select k from t where k = 1 + 1", nil, "primary_key reads 1 gives 2"},
			{"select k from t where k = ? * 2", []any{int64(2)}, "primary_key reads 1 gives 4"},
			{"select k from t where 3 - 1 = k", nil, "primary_key reads 1 gives 2"},
			{"select k from t where k = 1 + ?", []any{nil}, "none reads 0 gives "},
			{"select k from t where k > 4 / 2", nil, "primary_key reads 2 gives 3 4"},
			{"select k from t where a = 5 * 4", nil, "index t_a reads 1 gives 2"},
			{"select k from t where a >= 10 + 5 and a < 2 * 20", nil, "index t_a reads 2 gives 2 3"},
			{"select k from t where k + 0 = 2", nil, "full_scan reads 4 gives 2"},
			{"select k from t where a = k * 10", nil, "full_scan reads 4 gives 1 2 3"},
			{"select k from t where k = a / 10", nil, "full_scan reads 4 gives 1 2 3"},
			{"select k from t where k = b", nil, "full_scan reads 4 gives 2 3 4"},
			{"select k from t where k = 3 and a + 0 = 30", nil, "primary_key reads 1 gives 3"},
			{"select k from t where a + 0 = 30 and k > 1 + 1", nil, "primary_key reads 2 gives 3"},
		} {
			is.Equal(t, c.want, read(t, i, h, c.src, c.args...), c.src)
		}
	})

	t.Run("should evaluate the conditions of a row in the order of the text and stop at the first that is not true, and a bound guards every condition", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		setUp(t, i)
		for _, c := range []struct{ src, want string }{
			// b is 0 in the row of k = 1.
			{"select k from t where b <> 0 and a / b > 1", "full_scan reads 4 gives 2 3"},
			{"select k from t where a / b > 1 and b <> 0", "s4ql: division by zero in / (at offset 22)"},
			// The key's bound reads no row with b = 0, wherever it stands.
			{"select k from t where a / b > 1 and k > 1", "primary_key reads 3 gives 2 3"},
			{"select k from t where a / b > 1 and k = 2", "primary_key reads 1 gives 2"},
			// An index's bound too.
			{"select k from t where a / b > 1 and a >= 20", "index t_a reads 2 gives 2 3"},
			// A NULL does not stop the other side of its comparison.
			{"select k from t where a + 1 > b / (b - b)", "s4ql: division by zero in / (at offset 30)"},
		} {
			is.Equal(t, c.want, read(t, i, h, c.src), c.src)
		}
		// By a path that reads every row, the bound does not guard.
		i.e.SetPath(engine.Path{Full: true})
		is.Equal(t, "s4ql: division by zero in / (at offset 22)", read(t, i, h, "select k from t where a / b > 1 and k > 1"))
		i.e.SetPath(engine.Path{})
	})

	t.Run("should read no row after the last that a LIMIT gives, so the direction decides whether a row that fails is reached", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		setUp(t, i)
		const src = "select k from t where 10 / (k - 1) > 0 limit 1"
		i.e.SetPath(engine.Path{Backward: true})
		is.Equal(t, "full_scan reads 1 gives 4", read(t, i, h, src))
		i.e.SetPath(engine.Path{Forward: true})
		is.Equal(t, "s4ql: division by zero in / (at offset 22)", read(t, i, h, src))
		i.e.SetPath(engine.Path{})
	})

	t.Run("should compute an expression without a column once, after the checks, also where no row is read, but in a subquery only where the subquery is computed", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		setUp(t, i)
		maxInt := int64(math.MaxInt64)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{"select k from t where k = 99 and a = 1 + ?", []any{maxInt}, "s4ql: integer overflow in + (at offset 37)"},
			{"select k from t where k = 1 + ? limit 0", []any{maxInt}, "s4ql: integer overflow in + (at offset 26)"},
			{"select k from t where k is null and a = ? * 2", []any{maxInt}, "s4ql: integer overflow in * (at offset 40)"},
			// The key is never NULL, so the subquery is not computed, nor its expression.
			{"select k from t where k is null and k = (select k from t where a = ? * 2)", []any{maxInt}, "none reads 0 gives "},
			{"select k from t where k = (select k from t where a = ? * 2)", []any{maxInt}, "s4ql: integer overflow in * (at offset 53)"},
			// A comparison with NULL keeps the subquery from being computed only where it is a constant, or
			// serves the access, and not where it is computed for each row.
			{"select k from t where k = ? + 0 and k = (select k from t where k = 1 + ?)", []any{nil, maxInt}, "none reads 0 gives "},
			{"select k from t where k = ? and k = (select k from t where k = 1 + ?)", []any{nil, maxInt}, "none reads 0 gives "},
			{"select k from t where b = ? and k = (select k from t where k = 1 + ?)", []any{nil, maxInt}, "none reads 0 gives "},
			{"select k from t where k + 0 = ? and k = (select k from t where k = 1 + ?)", []any{nil, maxInt}, "s4ql: integer overflow in + (at offset 67)"},
			// The names, the lengths, and the types come before.
			{"select k from t where k = 1 + ? and x = 1", []any{maxInt}, "1/1: no such column: x"},
			{"select k from t where k = 1 + ? and s = 1", []any{maxInt}, "s4ql: not supported: an integer literal compared with the column t.s of type TEXT"},
		} {
			got := i.queryValues(c.src, values(c.args))
			text := got.message
			if got.err == "" {
				text = read(t, i, h, c.src, c.args...)
			} else if !strings.HasPrefix(text, "s4ql:") {
				text = got.err
			}
			is.Equal(t, c.want, text, c.src)
		}
		got := i.execValues("update t set a = 2 * ? where k = 99", values([]any{maxInt}))
		is.Equal(t, "s4ql: integer overflow in * (at offset 17)", got.message)
		got = i.execValues("delete from t where k = 99 and a = ? - 1", values([]any{int64(math.MinInt64)}))
		is.Equal(t, "s4ql: integer overflow in - (at offset 35)", got.message)
	})

	t.Run("should give SQLite's errors of the names of arithmetic where SQLite gives them, and the refusals of its types after them", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		setUp(t, i)
		for _, c := range []struct{ src, want string }{
			{"update t set z = x", "1/1: no such column: x"},
			{"update t set a = x, z = 1", "1/1: no such column: x"},
			{"update t set z = 1, a = x", "1/1: no such column: z"},
			{`update t set a = a + "x"`, `1/1: no such column: "x" - should this be a string literal in single-quotes?`},
			{"update t set a = x + 1 where y = 1", "1/1: no such column: x"},
			{"update t set a = 1 where a + x = 1", "1/1: no such column: x"},
			{"select k from t where x is null and a + y = 1", "1/1: no such column: x"},
			{"select k from t where k is null and a + y = 1", "1/1: no such column: y"},
			{"select k from t where y = a + x", "1/1: no such column: y"},
			{"select k from t where a + x = y", "1/1: no such column: x"},
			{"select k from t where s + 1 = x", "1/1: no such column: x"},
			{"select k from t where s + 1 = 1", "not supported: column of type TEXT in arithmetic (at offset 22)"},
			{"select k from t where k = ? + s", "not supported: column of type TEXT in arithmetic (at offset 30)"},
			{"select k from t where s = k + 1", "not supported: an expression of type integer compared with the column t.s of type TEXT"},
			{"select k from t where s = k + ?", "not supported: an expression of type null compared with the column t.s of type TEXT"},
			{"select k from t where r = k + 1", "not supported: an expression of type integer compared with the column t.r of type REAL"},
			{"select k from t where k = r * 1", "not supported: an expression of type real compared with the column t.k of type INT"},
			{"select k from t where k + 0 = 1.5", "not supported: a real literal compared with an expression of type integer"},
			{"select k from t where k + 0 = 'a'", "not supported: a string literal compared with an expression of type integer"},
			{"select k from t where k * 1 = r * 1", "not supported: an expression of type real compared with an expression of type integer"},
			{"select k from t where k = s", "not supported: the column t.s of type TEXT compared with the column t.k of type INT"},
			{"select k from t where k + 0 = (select s from t)", "not supported: subquery 1, of the column t.s of type TEXT, compared with an expression of type integer"},
			{"update t set s = k + 1", "not supported: an expression of type integer for the column t.s of type TEXT"},
			{"update t set a = r + 1", "not supported: an expression of type real for the column t.a of type INT"},
			{"update t set a = s", "not supported: the column t.s of type TEXT for the column t.a of type INT"},
			{"update t set a = 1.5, b = b + 1.5", "not supported: a real literal for the column t.a of type INT"},
			{"update t set b = b + 1.5, a = 1.5", "not supported: a real literal for the column t.a of type INT"},
		} {
			args := []any{}
			if strings.Contains(c.src, "?") {
				args = []any{nil}
			}
			got := i.execValues(c.src, values(args))
			text := got.err
			if got.err == "not supported" {
				text = strings.TrimPrefix(got.message, "s4ql: ")
			}
			is.Equal(t, c.want, text, c.src)
		}
		// SQLite gives row value misused at the first leaf of the expression, after the sign of a
		// literal, as the shell shows it.
		is.Equal(t, "", i.exec("create table u (x int primary key, y int) strict, without rowid").err)
		for src, offset := range map[string]int{"select k from t where a + 1 = (select * from u)": 22, "select k from t where (a + 1) * 2 = (select * from u)": 23,
			"select k from t where -a = (select * from u)": 23, "select k from t where -1.5 * a = (select * from u)": 23} {
			got := i.queryValues(src, nil)
			is.Equal(t, "1/1: row value misused", got.err, src)
			is.Equal(t, offset, got.offset, src)
		}
		got := i.queryValues("select k from t where k + ? = 1", values([]any{"1"}))
		is.Equal(t, "s4ql: not supported: argument 1 in arithmetic, which is text (at offset 26)", got.message)
		got = i.queryValues("select k from t where k = 2 + ?", values([]any{nil}))
		is.Equal(t, "", got.err)
	})

	t.Run("should give SQLite's string or blob too big for an argument of arithmetic that is too long, after the names and before the refusal of its type", func(t *testing.T) {
		d := newDuo(t, 1000,
			"create table t (k int primary key, a int, s text) strict, without rowid",
			"insert into t values (1, 1, 'x')")
		long, longBlob := strings.Repeat("a", 2000), []byte(strings.Repeat("a", 2000))
		for _, arg := range []any{long, longBlob} {
			d.run("select k from t where k + ? = 1", arg).is("18/18: string or blob too big")
			d.run("select k from t where 1 = k + ?", arg).is("18/18: string or blob too big")
			d.run("select k from t where k = 1 + ?", arg).is("18/18: string or blob too big")
			d.run("select k from t where k = (select k from t where a * ? = 1)", arg).is("18/18: string or blob too big")
			d.run("update t set a = a + ? where k = 1", arg).is("18/18: string or blob too big")
			d.run("update t set a = 2 * ?", arg).is("18/18: string or blob too big")
			d.run("update t set s = 1, a = a + ?", arg).is("18/18: string or blob too big")
			d.run("delete from t where a - ? = 0", arg).is("18/18: string or blob too big")
			// The names come first.
			d.run("select k from t where k + ? = x", arg).is("1/1: no such column: x")
		}
		got := d.run("select k from t where k + ? = 1", strings.Repeat("a", 1000))
		got.is("not supported")
		is.Equal(t, "s4ql: not supported: argument 1 in arithmetic, which is text (at offset 26)", got.got.message)
		d.same()
	})

	t.Run("should name the first column in the order of the table that gets NULL and does not take it, as SQLite does, whatever the order of SET", func(t *testing.T) {
		d := newDuo(t, 0,
			"create table t (k int primary key, a int not null, b int not null) strict, without rowid",
			"insert into t values (1, 1, 1)")
		d.run("update t set b = a + ?, a = b + ?", nil, nil).is("19/1299: NOT NULL constraint failed: t.a")
		d.run("update t set a = b + ?, b = a + ?", nil, nil).is("19/1299: NOT NULL constraint failed: t.a")
		d.run("update t set b = ?, a = b + ?", nil, nil).is("19/1299: NOT NULL constraint failed: t.a")
		d.run("update t set b = a + ?, a = b", nil).is("19/1299: NOT NULL constraint failed: t.b")
		d.run("update t set b = a * 2, a = b + ?", nil).is("19/1299: NOT NULL constraint failed: t.a")
		d.run("update t set b = a + 1, a = b + 1 returning *").ok()
		d.same()
	})

	t.Run("should change the rows of an update that changes the key in the order of the key, as SQLite does, whatever the index that reads them", func(t *testing.T) {
		d := newDuo(t, 0,
			"create table m (k int primary key, a int, b int) strict, without rowid",
			"create index m_a on m (a)",
			"create index m_b on m (b)",
			"insert into m values (1, 10, -1)",
			"insert into m values (2, 20, -2)",
			"insert into m values (3, 30, -3)")
		const unique = "19/1555: UNIQUE constraint failed: m.k"
		// In the order of the key, 1 becomes 2 while 2 is there; in the order of the index on b, 3
		// would become 4 first, and every row would move.
		d.run("update m set k = k + 1").is(unique)
		d.run("update m set k = k + 1 where b <= -1").is(unique)
		d.run("update m set k = k + 1 where a >= 10 returning *").is(unique)
		d.run("update m set k = 4 - k where b < 0").is(unique)
		// In the order of the key, 1 becomes 0 first, and every row moves; in the order of the index on
		// b, 3 would become 2 while 2 is there.
		d.run("update m set k = k - 1 where b <= -1").ok()
		d.run("update m set k = k + 1 where b <= -1 returning *").is(unique)
		d.run("update m set k = k + 10 where b <= -1 returning *").ok()
		d.run("update m set k = a, a = k").ok()
		d.run("update m set k = b where a < 20").ok()
		d.same()
	})

	t.Run("should compute the constants of SET, then of WHERE, then of each subquery, each in the order of the text, and the two sides of a comparison left to right", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		setUp(t, i)
		maxInt := int64(math.MaxInt64)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			// SET before WHERE, and the text's order within each.
			{"update t set a = ? + 1 where k = ? * 2", []any{maxInt, maxInt}, "s4ql: integer overflow in + (at offset 17)"},
			{"update t set a = 1, b = ? * 2 where k = ? + 1", []any{maxInt, maxInt}, "s4ql: integer overflow in * (at offset 24)"},
			{"update t set b = ? + 1, a = ? * 2", []any{maxInt, maxInt}, "s4ql: integer overflow in + (at offset 17)"},
			{"select k from t where k = ? + 1 and a = ? * 2", []any{maxInt, maxInt}, "s4ql: integer overflow in + (at offset 26)"},
			{"select k from t where a = ? * 2 and k = ? + 1", []any{maxInt, maxInt}, "s4ql: integer overflow in * (at offset 26)"},
			{"delete from t where k = 1 / ? and a = ? * 2", []any{int64(0), maxInt}, "s4ql: division by zero in / (at offset 24)"},
			// The constants of the WHERE of the statement before those of its subquery, wherever it stands.
			{"select k from t where k = (select k from t where k = ? + 1) and a = ? * 2", []any{maxInt, maxInt}, "s4ql: integer overflow in * (at offset 68)"},
			// The two sides of a comparison that are computed for each row, left to right, on every row.
			{"select k from t where k + 9223372036854775807 = k / (k - k)", nil, "s4ql: integer overflow in + (at offset 22)"},
			{"select k from t where k / (k - k) = k + 9223372036854775807", nil, "s4ql: division by zero in / (at offset 22)"},
			{"select k from t where k + 9223372036854775807 = (k - k) / k", nil, "s4ql: integer overflow in + (at offset 22)"},
		} {
			got := i.queryValues(c.src, values(c.args))
			if !strings.HasPrefix(c.src, "select") {
				got = i.execValues(c.src, values(c.args))
			}
			is.Equal(t, c.want, got.message, c.src)
		}
	})

	t.Run("should give SQLite's attempt to write a readonly database for an update or a delete in a read-only transaction, and compute no constant there", func(t *testing.T) {
		d := newDuo(t, 0,
			"create table t (k int primary key, a int, s text) strict, without rowid",
			"insert into t values (1, 1, 'x')")
		is.NotError(t, d.i.c.BeginReadOnly(t.Context()))
		for _, src := range oracleBegin(true) {
			is.Equal(t, "", d.o.run(t, src).err, src)
		}
		maxInt := int64(math.MaxInt64)
		const readOnly = "8/8: attempt to write a readonly database"
		d.run("delete from t where k = 1 + ?", maxInt).is(readOnly)
		d.run("delete from t where k = 99 and a = 1 + ?", maxInt).is(readOnly)
		d.run("update t set a = ? * 2", maxInt).is(readOnly)
		d.run("update t set a = ? * 2 where k = 1", maxInt).is(readOnly)
		d.run("update t set a = a + 1 where k = 1 + ?", maxInt).is(readOnly)
		d.run("update t set a = a + 1").is(readOnly)
		d.run("update t set a = a * 9223372036854775807 where k = 1").is(readOnly)
		d.run("delete from t where a / (a - a) = 1").is(readOnly)
		d.run("delete from t where k = (select k from t where a = 1 + ?)", maxInt).is(readOnly)
		// The write is refused before the values of SET are checked, as for a plain value.
		d.run("update t set a = s + 1").is(readOnly)
		d.run("update t set a = a + ?", "1").is(readOnly)
		d.run("update t set a = 'x'").is(readOnly)
		d.run("update t set s = a + 1").is(readOnly)
		// The names come first, the types of WHERE as in a select, and a select computes its constants
		// as outside a transaction.
		d.run("update t set a = x + 1").is("1/1: no such column: x")
		got := d.run("update t set a = s + 1 where s = k + 1")
		got.is("not supported")
		is.Equal(t, "s4ql: not supported: an expression of type integer compared with the column t.s of type TEXT", got.got.message)
		is.Equal(t, "s4ql: integer overflow in + (at offset 26)", d.i.queryValues("select k from t where k = 1 + ?", values([]any{maxInt})).message)
		is.NotError(t, d.i.c.Rollback(t.Context()))
		for _, src := range oracleEnd(true, false) {
			is.Equal(t, "", d.o.run(t, src).err, src)
		}
		d.same()
	})

	t.Run("should set every column from the row as it was, check NOT NULL on each row with a value per row, change nothing where one row fails, and leave the count of changes then", func(t *testing.T) {
		h := &events{}
		i := newInstance(t, engine.Options{Logger: slog.New(h)})
		setUp(t, i)
		is.Equal(t, `columns "k INT, a INT, b INT", rows ["1,0,10" "2,2,20" "3,3,30" "4,4,null"]`, i.queryValues("update t set a = b, b = a returning k, a, b", nil).String())
		is.Equal(t, `columns "k INT, n INT", rows ["1,10" "2,20" "3,30"]`, i.queryValues("update t set n = b where b is not null returning k, n", nil).String())
		before := i.state()
		changes := i.changes
		got := i.execValues("update t set n = b", nil)
		is.Equal(t, "19/1299: NOT NULL constraint failed: t.n", got.err)
		is.Equal(t, int64(0), i.changes)
		is.Equal(t, before, i.state())
		i.changes = changes
		got = i.execValues("update t set a = a * 4611686018427387904 returning a", nil)
		is.Equal(t, "s4ql: integer overflow in * (at offset 17)", got.message)
		is.Equal(t, changes, i.changes)
		is.Equal(t, before, i.state())
		e, _ := h.last(t)
		is.Equal(t, "evaluation_error", e[engine.AttrOutcome])
		is.Equal(t, "s4ql: integer overflow in * (at offset 17)", e[engine.AttrError])
		// A transaction goes on after the failure, with what it did before.
		is.NotError(t, i.c.Begin(t.Context()))
		is.Equal(t, "", i.execValues("update t set n = n + 1 where k = 1", nil).err)
		is.Equal(t, evaluationError, i.execValues("update t set a = a / (k - 1)", nil).err)
		is.Equal(t, `columns "n INT", rows ["11"]`, i.queryValues("select n from t where k = 1", nil).String())
		is.NotError(t, i.c.Commit(t.Context()))
		is.Equal(t, `columns "n INT", rows ["11"]`, i.queryValues("select n from t where k = 1", nil).String())
	})
}
