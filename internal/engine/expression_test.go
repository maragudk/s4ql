package engine_test

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
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
//     evaluate, as [pair.justify] says. The call is not run in SQLite, and nothing changed.
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
// that s4ql's rules give for it, whatever their order. They are the refusals of the values of its
// conditions and of those of its subqueries that are of another class than what they are compared
// with, by the rule of exact types, an operand of arithmetic that is a column of type TEXT or BLOB
// or an argument that is text or a blob, a correlated subquery, an argument of LIMIT that is not an
// integer from 0, and, but in a read-only transaction, which refuses the write first, the values of
// SET of another class than their columns.
func arithmeticRefusals(t testing.TB, o *oracle, s stmt.Statement, args []any, readOnly bool) []string {
	t.Helper()
	table, where := tableWhere(s)
	subs := subqueriesIn(s)
	name, columns := o.columns(t, table.Value)
	refusals := whereRefusals(t, o, name, columns, where, subs, args)
	for _, q := range subs {
		inner, innerColumns := o.columns(t, q.Table.Value)
		refusals = append(refusals, whereRefusals(t, o, inner, innerColumns, q.Where, nil, args)...)
		if rule := limitRule(&q, args); rule != "" {
			refusals = append(refusals, rule)
		}
	}
	if rule := correlatedRule(t, o, where, subs); rule != "" {
		refusals = append(refusals, rule)
	}
	switch s := s.(type) {
	case *stmt.Select:
		if rule := limitRule(s, args); rule != "" {
			refusals = append(refusals, rule)
		}
	case *stmt.Update:
		if !readOnly {
			refusals = append(refusals, setRefusals(name, columns, s.Set, args)...)
		}
	}
	return refusals
}

// whereRefusals of the conditions where, of the table of the name, as the table stores it, and of the
// columns, whose subqueries are subs, with the arguments args, on the database of o, as
// [arithmeticRefusals] gives them. A condition with a column that the table does not have, a
// correlated one, is left to [correlatedRule].
func whereRefusals(t testing.TB, o *oracle, name string, columns []stmttest.TableColumn, where []stmt.Condition, subs []stmt.Select, args []any) []string {
	t.Helper()
	var refusals []string
	for _, c := range where {
		if slices.ContainsFunc(conditionColumns(c), func(n stmt.Name) bool { return stmttest.ColumnOf(columns, n) < 0 }) {
			continue
		}
		var tested, testedName string
		if c.Expression.Present() {
			var refused []string
			tested, refused = expressionClass(c.Expression, columns, args)
			refusals = append(refusals, refused...)
			testedName = "an expression of type " + tested
		} else {
			position := stmttest.ColumnOf(columns, c.Column)
			tested, testedName = columnClass(columns[position].Type), columnNamed(name, columns[position])
		}
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			continue
		}
		var value, valueName string
		switch v := c.Value; {
		case v.Expression.Kind == stmt.ExprColumn && !c.Expression.Present():
			position := stmttest.ColumnOf(columns, v.Expression.Column)
			value, valueName = columnClass(columns[position].Type), columnNamed(name, columns[position])
			if value != tested {
				refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v compared with %v", valueName, testedName))
			}
			continue
		case v.Expression.Present():
			var refused []string
			value, refused = expressionClass(v.Expression, columns, args)
			refusals = append(refusals, refused...)
			valueName = "an expression of type " + value
		case v.Subquery > 0:
			q := subs[v.Subquery-1]
			inner, innerColumns := o.columns(t, q.Table.Value)
			position := 0
			if q.Columns != nil {
				position = stmttest.ColumnOf(innerColumns, q.Columns[0])
			}
			if position < 0 || len(innerColumns) == 0 {
				continue
			}
			value, valueName = columnClass(innerColumns[position].Type), fmt.Sprint("subquery ", v.Subquery, ", of ", columnNamed(inner, innerColumns[position]), ",")
		default:
			value, valueName = argumentClass(stmttest.BoundValue(v, args)), plainName(v, args)
		}
		arithmetic := c.Expression.Present() || c.Value.Expression.Present()
		if !matches(value, tested) || arithmetic && !numeric(value, tested) {
			refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v compared with %v", valueName, testedName))
		}
	}
	return refusals
}

// numeric reports whether no class of the classes is text or a blob, which arithmetic never meets.
func numeric(classes ...string) bool {
	return !slices.Contains(classes, "text") && !slices.Contains(classes, "blob")
}

// setRefusals of the assignments set of an update of the table of the name and of the columns, with
// the arguments args, as [arithmeticRefusals] gives them.
func setRefusals(name string, columns []stmttest.TableColumn, set []stmt.Assignment, args []any) []string {
	var refusals []string
	for _, a := range set {
		position := stmttest.ColumnOf(columns, a.Column)
		if position < 0 {
			continue
		}
		into := columnNamed(name, columns[position])
		var value, valueName string
		switch v := a.Value; {
		case v.Expression.Kind == stmt.ExprColumn:
			from := stmttest.ColumnOf(columns, v.Expression.Column)
			value, valueName = columnClass(columns[from].Type), columnNamed(name, columns[from])
			if value != columnClass(columns[position].Type) {
				refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v for %v", valueName, into))
			}
			continue
		case v.Expression.Present():
			var refused []string
			value, refused = expressionClass(v.Expression, columns, args)
			refusals = append(refusals, refused...)
			valueName = "an expression of type " + value
		default:
			value, valueName = argumentClass(stmttest.BoundValue(v, args)), plainName(v, args)
		}
		if !matches(value, columnClass(columns[position].Type)) || a.Value.Expression.Present() && !numeric(columnClass(columns[position].Type)) {
			refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v for %v", valueName, into))
		}
	}
	return refusals
}

// expressionClass of the arithmetic expression e over the columns, with the arguments args: the
// class of its values, "integer", "real", or "null" where an argument that is NULL makes every value
// NULL, as the evaluator types it, and the refusals of its operands: each column of type TEXT or
// BLOB, and each argument that is text or a blob, at its offset.
func expressionClass(e stmt.Expression, columns []stmttest.TableColumn, args []any) (string, []string) {
	switch e.Kind {
	case stmt.ExprValue:
		return argumentClass(stmttest.BoundValue(stmt.Term{Value: e.Value}, args)), nil
	case stmt.ExprParameter:
		switch args[e.Parameter-1].(type) {
		case string:
			return "", []string{fmt.Sprintf("s4ql: not supported: argument %v in arithmetic, which is text (at offset %v)", e.Parameter, e.Span.Start)}
		case []byte:
			return "", []string{fmt.Sprintf("s4ql: not supported: argument %v in arithmetic, which is a blob (at offset %v)", e.Parameter, e.Span.Start)}
		}
		return argumentClass(args[e.Parameter-1]), nil
	case stmt.ExprColumn:
		typ := columns[stmttest.ColumnOf(columns, e.Column)].Type
		if c := columnClass(typ); c == "text" || c == "blob" {
			return "", []string{fmt.Sprintf("s4ql: not supported: column of type %v in arithmetic (at offset %v)", typ, e.Span.Start)}
		}
		return columnClass(typ), nil
	case stmt.ExprNegate:
		return expressionClass(e.Operands[0], columns, args)
	}
	left, refusals := expressionClass(e.Operands[0], columns, args)
	right, more := expressionClass(e.Operands[1], columns, args)
	switch {
	case left == "null" || right == "null":
		return "null", append(refusals, more...)
	case left == "real" || right == "real":
		return "real", append(refusals, more...)
	}
	return "integer", append(refusals, more...)
}

// columnClass of a column of the declared type typ: one for INT and INTEGER.
func columnClass(typ string) string {
	switch typ {
	case "INT", "INTEGER":
		return "integer"
	}
	return strings.ToLower(typ)
}

// argumentClass of a value as SQLite binds it.
func argumentClass(v any) string {
	switch v.(type) {
	case int64:
		return "integer"
	case float64:
		return "real"
	case string:
		return "text"
	case []byte:
		return "blob"
	}
	return "null"
}

// matches reports whether values of the classes a and b meet by the rule of exact types: where they
// are one class, or either is that of NULL.
func matches(a, b string) bool {
	return a == b || a == "null" || b == "null"
}

// columnNamed of the column of the table of the name, as the engine names it in a message.
func columnNamed(table string, c stmttest.TableColumn) string {
	return fmt.Sprintf("the column %v.%v of type %v", table, c.Name, c.Type)
}

// plainName of the literal or the parameter t, with the arguments args, as the engine names it in a
// message.
func plainName(t stmt.Term, args []any) string {
	switch t.Value.(type) {
	case stmt.Integer:
		return "an integer literal"
	case stmt.Real:
		return "a real literal"
	case stmt.Text:
		return "a string literal"
	case stmt.Blob:
		return "a blob literal"
	}
	kind := map[string]string{"integer": "an integer", "real": "a real", "text": "text", "blob": "a blob"}[argumentClass(args[t.Parameter-1])]
	return fmt.Sprintf("argument %v, which is %v,", t.Parameter, kind)
}

// arithmeticSite of an arithmetic expression that the evaluator computes, in a statement: its tree,
// root, and where it is: the assignment of SET at the index set, or the condition at the index where
// of the WHERE of the statement, if sub is 0, or of its subquery of the number sub.
type arithmeticSite struct {
	set, where, sub int
	root            stmt.Expression
}

// arithmeticSites of the statement s, in the order of the text, but a column alone, which is copied or
// compared without the evaluator.
func arithmeticSites(s stmt.Statement) []arithmeticSite {
	var sites []arithmeticSite
	add := func(e stmt.Expression, site arithmeticSite) {
		if e.Present() && e.Kind != stmt.ExprColumn {
			site.root = e
			sites = append(sites, site)
		}
	}
	if u, ok := s.(*stmt.Update); ok {
		for i, a := range u.Set {
			add(a.Value.Expression, arithmeticSite{set: i, where: -1})
		}
	}
	_, where := tableWhere(s)
	for j, c := range where {
		add(c.Expression, arithmeticSite{set: -1, where: j})
		add(c.Value.Expression, arithmeticSite{set: -1, where: j})
	}
	for k, q := range subqueriesIn(s) {
		for j, c := range q.Where {
			add(c.Expression, arithmeticSite{set: -1, where: j, sub: k + 1})
			add(c.Value.Expression, arithmeticSite{set: -1, where: j, sub: k + 1})
		}
	}
	return sites
}

// operators of the expression e: its nodes that compute, the negations and the binary operators.
func operators(e stmt.Expression) []stmt.Expression {
	var ops []stmt.Expression
	if len(e.Operands) > 0 {
		ops = append(ops, e)
	}
	for _, o := range e.Operands {
		ops = append(ops, operators(o)...)
	}
	return ops
}

// evaluationKinds are the kinds of an evaluation error, each as [quirk] tests it.
var evaluationKinds = []error{expr.ErrOverflow, expr.ErrDivisionByZero, expr.ErrNotFinite}

// quirk of the operator n of arithmetic, with the arguments args: a condition that SQLite makes true
// where its own result of n alone, on its operands, is the quirk of the kind that s4ql refuses: a real
// from two integers for an overflow, NULL from two operands that are not NULL for a division by zero,
// and an infinity for a real result that is not finite; and the arguments of its parameters.
func quirk(n stmt.Expression, kind error, args []any) (string, []any) {
	left, leftArgs := stmttest.ExpressionText(n.Operands[0], args)
	if n.Kind == stmt.ExprNegate {
		if kind != expr.ErrOverflow {
			return "0", nil
		}
		return "typeof(" + left + ") = 'integer' and typeof(-(" + left + ")) = 'real'", append(slices.Clone(leftArgs), leftArgs...)
	}
	right, rightArgs := stmttest.ExpressionText(n.Operands[1], args)
	value, valueArgs := "("+left+" "+n.Kind.String()+" "+right+")", append(slices.Clone(leftArgs), rightArgs...)
	switch kind {
	case expr.ErrOverflow:
		return "typeof(" + left + ") = 'integer' and typeof(" + right + ") = 'integer' and typeof(" + value + ") = 'real'", slices.Concat(leftArgs, rightArgs, valueArgs)
	case expr.ErrDivisionByZero:
		return left + " is not null and " + right + " is not null and " + value + " is null", slices.Concat(leftArgs, rightArgs, valueArgs)
	}
	return value + " in (9e999, -9e999)", valueArgs
}

// quirkRows of the operator n of the site of the statement s, with the arguments args, of the kind:
// whether SQLite shows the quirk of n, as [quirk] says, for a row that the conditions before the site
// in the order of the text hold for, where the site is a condition, or that every condition holds
// for, where it is an assignment of SET; also those that may serve the access, if must. An expression
// without a column has one value, which needs no row. A subquery of the statement has the value of
// computed, where the engine computed them.
func (p *pair) quirkRows(s stmt.Statement, site arithmeticSite, n stmt.Expression, kind error, args []any, must bool) bool {
	t, o := p.t, p.o
	t.Helper()
	condition, bound := quirk(n, kind, args)
	if !site.root.HasColumn() {
		return len(o.run(t, "select 1 where "+condition, bound...).rows) > 0
	}
	table, where := tableWhere(s)
	subs := subqueriesIn(s)
	if site.sub > 0 {
		table, where, subs = subs[site.sub-1].Table, subs[site.sub-1].Where, nil
	}
	var guard []stmt.Condition
	if site.set < 0 {
		if must {
			for _, c := range where {
				if !c.Expression.Present() && !c.Value.Expression.HasColumn() {
					guard = append(guard, c)
				}
			}
		}
		guard = append(guard, where[:site.where]...)
	} else {
		guard = where
	}
	if p.computed != nil && site.sub == 0 {
		guard = slices.Clone(guard)
		for i, c := range guard {
			if c.Value.Subquery > 0 {
				guard[i].Value = valueTerm(p.computed[c.Value.Subquery-1])
			}
		}
	}
	found, foundArgs := stmttest.FoundQuery(table, guard, subs, args)
	found = strings.TrimSuffix(found, " limit 1")
	if len(guard) == 0 {
		found += " where "
	} else {
		found += " and "
	}
	a := o.run(t, found+condition+" limit 1", append(foundArgs, bound...)...)
	if a.err != "" {
		t.Fatalf("%q: SQLite gives %v", found+condition, a.err)
	}
	return len(a.rows) > 0
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
// wrong with it, or "". Its operator must be one of the statement, at its span, and SQLite's result of
// that operator alone must show the quirk of its kind for a row that the documented rule may evaluate:
// one that the conditions before it in the order of the text hold for, or, for SET, one that the
// WHERE holds for, as [pair.quirkRows] says. A row that a condition after it rules out, as a bound of
// the access may, is such a row too: the rule does not decide it.
func (p *pair) justify(s stmt.Statement, args []any, ee *expr.Error) string {
	if ee == nil {
		return "is no *expr.Error"
	}
	for _, site := range arithmeticSites(s) {
		for _, n := range operators(site.root) {
			if n.Span != ee.Span || n.Kind != ee.Op {
				continue
			}
			if p.quirkRows(s, site, n, ee.Kind, args, false) {
				return ""
			}
			return "SQLite's result of its operator justifies for no row that the rule may evaluate"
		}
	}
	return "is at no operator of the statement"
}

// due is what evaluation error was due for the statement s, with the arguments args, which the engine
// ran without one, or "": each expression without a column of the statement, and of a subquery that
// it computed, is computed, and so is each expression of a condition for a row that every condition
// that may serve the access and every condition before it hold for, and each of SET for a row that
// the WHERE holds for, so SQLite's result of none of their operators may show a quirk there, as
// [pair.quirkRows] says. A subquery reads only up to its first row, and a select with LIMIT only up to
// its last, which their order decides, so neither has a due error for a row.
func (p *pair) due(s stmt.Statement, args []any) string {
	sel, isSelect := s.(*stmt.Select)
	computed := !subqueried(s) || p.computed != nil
	for _, site := range arithmeticSites(s) {
		switch {
		case !site.root.HasColumn() && site.sub > 0 && !computed:
			continue
		case site.root.HasColumn() && (site.sub > 0 || !computed || isSelect && sel.Limited()):
			continue
		}
		for _, n := range operators(site.root) {
			for _, kind := range evaluationKinds {
				if p.quirkRows(s, site, n, kind, args, true) {
					return fmt.Sprintf("SQLite shows the quirk of %v of %v at offset %v for a row that the rule evaluates", kind, n.Kind, n.Span.Start)
				}
			}
		}
	}
	return ""
}

// arithmeticSetUp is the table of the differential tests of arithmetic: a column of each type, an
// index on an INT and one on a REAL column, and rows with the edges of each type and NULL.
var arithmeticSetUp = []stmttest.Call{
	{Src: "create table n (k int primary key, i int, j integer, r real, s text, b blob, u int not null) strict, without rowid", Supported: true},
	{Src: "create index n_i on n (i)", Supported: true},
	{Src: "create index n_r on n (r desc)", Supported: true},
	{Src: "insert into n values (1, 0, 10, 0.5, 'a', x'01', 1)", Supported: true},
	{Src: "insert into n values (2, 1, 20, -2.5, 'b', x'02', 2)", Supported: true},
	{Src: "insert into n values (3, -1, null, 1e308, 'c', null, 3)", Supported: true},
	{Src: "insert into n values (4, 9223372036854775807, 30, null, null, x'04', 4)", Supported: true},
	{Src: "insert into n values (5, -9223372036854775808, 40, 2.0, 'e', x'05', 5)", Supported: true},
	{Src: "insert into n values (6, null, 50, -0.0, 'f', x'06', 6)", Supported: true},
}

// arithmeticCases of the differential tests of arithmetic, run in their order on the rows of
// [arithmeticSetUp], each through Exec and through Query. A call whose text is "begin", "commit", or
// "rollback" begins or ends a transaction of the pair.
var arithmeticCases = []stmttest.Call{
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
}

func TestPair_arithmetic(t *testing.T) {
	run := func(t *testing.T, p *pair, i *instance, calls []stmttest.Call) {
		t.Helper()
		for _, c := range arithmeticSetUp {
			is.Equal(t, "", p.step(c), c.Src)
		}
		var log []string
		for _, c := range calls {
			switch c.Src {
			case "begin":
				is.NotError(t, i.c.Begin(t.Context()))
				is.NotError(t, p.other.c.Begin(t.Context()))
				for _, s := range oracleBegin(false) {
					is.Equal(t, "", p.o.run(t, s).err, s)
				}
				p.inTx = true
				log = append(log, "-- begin")
				continue
			case "commit", "rollback":
				for _, conn := range []*engine.Conn{i.c, p.other.c} {
					if c.Src == "commit" {
						is.NotError(t, conn.Commit(t.Context()))
					} else {
						is.NotError(t, conn.Rollback(t.Context()))
					}
				}
				for _, s := range oracleEnd(false, c.Src == "commit") {
					is.Equal(t, "", p.o.run(t, s).err, s)
				}
				p.inTx = false
				log = append(log, "-- "+c.Src)
				continue
			}
			for _, query := range []bool{false, true} {
				c.Query = query
				log = append(log, fmt.Sprintf("%v -- with %#v, as a query: %v", c.Src, c.Args, query))
				if failure := p.step(c); failure != "" {
					t.Fatalf("after\n%v\n%v", strings.Join(log[:len(log)-1], "\n"), failure)
				}
				// A write that succeeded through Exec writes again through Query, so each is a call of its
				// own, but a read, which may be run twice.
				if s, _ := stmt.Check(c.Src); s != nil {
					if _, ok := s.(*stmt.Select); !ok {
						break
					}
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
			for _, kind := range []string{"excused: " + excusedArithmeticType, "excused: " + excusedEvaluation, comparedWithSQLite} {
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
		for _, kind := range []string{"excused: " + excusedArithmeticType, "excused: " + excusedEvaluation, comparedWithSQLite} {
			is.True(t, y[kind] > 0, kind)
		}
	})
}

// arithmeticCalls draws n calls of arithmetic on the table of [arithmeticSetUp] from r: selects,
// updates, and deletes, with a subquery one time in ten, in a transaction one time in ten, with
// expressions over the columns, literals at the edges of each type, and arguments. Each expression
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
		{[]string{"r"}, []string{"0.5", "-2.5", "1e308", "0.0", "-0.0", "2.0", "1e-300"}, []any{0.5, -0.0, 1e308, -1e308, 2.0, nil}},
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
			set := []string{"i", "j", "r", "u"}
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
			calls = append(calls, stmttest.Call{Src: "begin"}, stmttest.Call{Src: src, Args: args}, stmttest.Call{Src: []string{"commit", "rollback"}[r.IntN(2)]})
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
	for _, site := range arithmeticSites(s) {
		where := "WHERE"
		switch {
		case site.set >= 0:
			where = "SET"
		case site.sub > 0:
			where = "a subquery"
		}
		constant := map[bool]string{true: "with a column", false: "without a column"}[site.root.HasColumn()]
		count(fmt.Sprint("arithmetic ", constant, " in ", where, " of a ", kind, ", ", outcome))
	}
}
