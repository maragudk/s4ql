package stmttest

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The rules of the arithmetic of the piece of work arithmetic, for a statement that [HasExpression]
// reports, as s4ql's documents give them: the refusals of its types, which come in an order that is
// the engine's own, so that a rule gives each refusal that may come, and the justification of an
// evaluation error by SQLite's own result of the operator that failed. They are independent of the
// engine and of the evaluator: they read the typed form of the statement, the types of the columns,
// and the arguments, and leave what SQLite computes to a query that the caller runs.

// ColumnClass of a column of the declared type typ, by the rule of exact types: "integer" for INT and
// INTEGER, and "real", "text", or "blob" for the others.
func ColumnClass(typ string) string {
	switch typ {
	case "INT", "INTEGER":
		return "integer"
	}
	return strings.ToLower(typ)
}

// ArgumentClass of a value as SQLite binds it: "integer", "real", "text", "blob", or "null".
func ArgumentClass(v any) string {
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

// meets reports whether values of the classes a and b meet by the rule of exact types: where they
// are one class, or either is that of NULL.
func meets(a, b string) bool {
	return a == b || a == "null" || b == "null"
}

// numeric reports whether no class of the classes is text or a blob, which arithmetic never meets.
func numeric(classes ...string) bool {
	return !slices.Contains(classes, "text") && !slices.Contains(classes, "blob")
}

// columnNamed of the column of the table of the name, as the engine names it in a message.
func columnNamed(table string, c TableColumn) string {
	return fmt.Sprintf("the column %v.%v of type %v", table, c.Name, c.Type)
}

// plainName of the literal or the parameter t, with the arguments args, as the engine names it in a
// message, and "NULL" for NULL, which no message names.
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
	case stmt.Null:
		return "NULL"
	}
	kind := map[string]string{"integer": "an integer", "real": "a real", "text": "text", "blob": "a blob", "null": "NULL"}[ArgumentClass(args[t.Parameter-1])]
	return fmt.Sprintf("argument %v, which is %v,", t.Parameter, kind)
}

// ArithmeticRefusals of the select, the update, or the delete s with arithmetic, of the table of the
// name, as the table stores it, and of the columns, in the order of the table, which SQLite prepares,
// with the arguments args as SQLite binds them, one for each parameter: every "not supported" that
// s4ql's rules of types give for it, whatever their order, which is the engine's own. They are the
// refusals of the values of its conditions and of those of its subqueries that are of another class
// than what they are compared with, by the rule of exact types, of an operand of arithmetic that is a
// column of type TEXT or BLOB or an argument that is text or a blob, and, but in a read-only
// transaction, which refuses the write first, of the values of SET of another class than their
// columns. columnsOf gives the name and the columns of the table of a subquery, as for the table of s,
// or nil columns for a table that SQLite does not have. A condition with a column that its table does
// not have, which is correlated, is left out, and so are the refusals of a correlated subquery and of
// an argument of LIMIT, which the caller's rules of the subqueries and of LIMIT give.
func ArithmeticRefusals(table string, columns []TableColumn, s stmt.Statement, args []any, readOnly bool, columnsOf func(name string) (string, []TableColumn)) []string {
	where, subs := whereOf(s)
	refusals := whereRefusals(table, columns, where, subs, args, columnsOf)
	for _, q := range subs {
		inner, innerColumns := columnsOf(q.Table.Value)
		refusals = append(refusals, whereRefusals(inner, innerColumns, q.Where, nil, args, columnsOf)...)
	}
	if u, ok := s.(*stmt.Update); ok && !readOnly {
		refusals = append(refusals, setRefusals(table, columns, u.Set, args)...)
	}
	return refusals
}

// whereOf the select, the update, or the delete s: its conditions and its subqueries.
func whereOf(s stmt.Statement) ([]stmt.Condition, []stmt.Select) {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Where, s.Subqueries
	case *stmt.Update:
		return s.Where, s.Subqueries
	case *stmt.Delete:
		return s.Where, s.Subqueries
	}
	return nil, nil
}

// whereRefusals of the conditions where, of the table of the name and of the columns, whose
// subqueries are subs, with the arguments args, as [ArithmeticRefusals] gives them.
func whereRefusals(name string, columns []TableColumn, where []stmt.Condition, subs []stmt.Select, args []any, columnsOf func(string) (string, []TableColumn)) []string {
	var refusals []string
	for _, c := range where {
		if slices.ContainsFunc(conditionNames(c), func(n stmt.Name) bool { return ColumnOf(columns, n) < 0 }) {
			continue
		}
		var tested, testedName string
		if c.Expression.Present() {
			var refused []string
			tested, refused = expressionClass(c.Expression, columns, args)
			refusals = append(refusals, refused...)
			testedName = "an expression of type " + tested
		} else {
			position := ColumnOf(columns, c.Column)
			tested, testedName = ColumnClass(columns[position].Type), columnNamed(name, columns[position])
		}
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			continue
		}
		var value, valueName string
		switch v := c.Value; {
		case v.Expression.Kind == stmt.ExprColumn && !c.Expression.Present():
			position := ColumnOf(columns, v.Expression.Column)
			value, valueName = ColumnClass(columns[position].Type), columnNamed(name, columns[position])
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
			inner, innerColumns := columnsOf(q.Table.Value)
			position := 0
			if q.Columns != nil {
				position = ColumnOf(innerColumns, q.Columns[0])
			}
			if position < 0 || len(innerColumns) == 0 {
				continue
			}
			value, valueName = ColumnClass(innerColumns[position].Type), fmt.Sprint("subquery ", v.Subquery, ", of ", columnNamed(inner, innerColumns[position]), ",")
		default:
			value, valueName = ArgumentClass(BoundValue(v, args)), plainName(v, args)
		}
		arithmetic := c.Expression.Present() || c.Value.Expression.Present()
		if !meets(value, tested) || arithmetic && !numeric(value, tested) {
			refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v compared with %v", valueName, testedName))
		}
	}
	return refusals
}

// setRefusals of the assignments set of an update of the table of the name and of the columns, with
// the arguments args, as [ArithmeticRefusals] gives them.
func setRefusals(name string, columns []TableColumn, set []stmt.Assignment, args []any) []string {
	var refusals []string
	for _, a := range set {
		position := ColumnOf(columns, a.Column)
		if position < 0 {
			continue
		}
		into := columnNamed(name, columns[position])
		var value, valueName string
		switch v := a.Value; {
		case v.Expression.Kind == stmt.ExprColumn:
			from := ColumnOf(columns, v.Expression.Column)
			value, valueName = ColumnClass(columns[from].Type), columnNamed(name, columns[from])
			if value != ColumnClass(columns[position].Type) {
				refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v for %v", valueName, into))
			}
			continue
		case v.Expression.Present():
			var refused []string
			value, refused = expressionClass(v.Expression, columns, args)
			refusals = append(refusals, refused...)
			valueName = "an expression of type " + value
		default:
			value, valueName = ArgumentClass(BoundValue(v, args)), plainName(v, args)
		}
		if !meets(value, ColumnClass(columns[position].Type)) || a.Value.Expression.Present() && !numeric(ColumnClass(columns[position].Type)) {
			refusals = append(refusals, fmt.Sprintf("s4ql: not supported: %v for %v", valueName, into))
		}
	}
	return refusals
}

// expressionClass of the arithmetic expression e over the columns, with the arguments args: the
// class of its values, "integer", "real", or "null" where an argument that is NULL makes every value
// NULL, as the documents of the evaluator type it, and the refusals of its operands: each column of
// type TEXT or BLOB, and each argument that is text or a blob, at its offset.
func expressionClass(e stmt.Expression, columns []TableColumn, args []any) (string, []string) {
	switch e.Kind {
	case stmt.ExprValue:
		return ArgumentClass(BoundValue(stmt.Term{Value: e.Value}, args)), nil
	case stmt.ExprParameter:
		switch args[e.Parameter-1].(type) {
		case string:
			return "", []string{fmt.Sprintf("s4ql: not supported: argument %v in arithmetic, which is text (at offset %v)", e.Parameter, e.Span.Start)}
		case []byte:
			return "", []string{fmt.Sprintf("s4ql: not supported: argument %v in arithmetic, which is a blob (at offset %v)", e.Parameter, e.Span.Start)}
		}
		return ArgumentClass(args[e.Parameter-1]), nil
	case stmt.ExprColumn:
		typ := columns[ColumnOf(columns, e.Column)].Type
		if c := ColumnClass(typ); c == "text" || c == "blob" {
			return "", []string{fmt.Sprintf("s4ql: not supported: column of type %v in arithmetic (at offset %v)", typ, e.Span.Start)}
		}
		return ColumnClass(typ), nil
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

// conditionNames of the condition c: the column that it tests, or the columns of the expression that
// it tests, and the columns of its value, in the order of the text.
func conditionNames(c stmt.Condition) []stmt.Name {
	var names []stmt.Name
	if !c.Expression.Present() {
		names = append(names, c.Column)
	}
	names = append(names, expressionNames(c.Expression)...)
	return append(names, expressionNames(c.Value.Expression)...)
}

// expressionNames of the columns of the expression e, in the order of the text.
func expressionNames(e stmt.Expression) []stmt.Name {
	if e.Kind == stmt.ExprColumn {
		return []stmt.Name{e.Column}
	}
	var names []stmt.Name
	for _, o := range e.Operands {
		names = append(names, expressionNames(o)...)
	}
	return names
}

// ArithmeticRefusal reports whether message is a refusal that [ArithmeticRefusals] may give.
func ArithmeticRefusal(message string) bool {
	for _, part := range []string{" in arithmetic (at offset ", " in arithmetic, which is ", " compared with an expression of type ", "s4ql: not supported: an expression of type ",
		" compared with the column ", " for the column "} {
		if strings.Contains(message, part) {
			return true
		}
	}
	return false
}

// ArithmeticSite of an arithmetic expression that the evaluator computes, in a statement: its tree,
// Root, and where it is: the assignment of SET at the index Set, or the condition at the index Where
// of the WHERE of the statement, if Sub is 0, or of its subquery of the number Sub; Set or Where is
// -1 for the other.
type ArithmeticSite struct {
	Set, Where, Sub int
	Root            stmt.Expression
}

// ArithmeticSites of the select, the update, or the delete s, in the order of the text: SET, the
// WHERE, and the WHERE of each subquery, but a column alone, which is copied or compared without the
// evaluator.
func ArithmeticSites(s stmt.Statement) []ArithmeticSite {
	var sites []ArithmeticSite
	add := func(e stmt.Expression, site ArithmeticSite) {
		if e.Present() && e.Kind != stmt.ExprColumn {
			site.Root = e
			sites = append(sites, site)
		}
	}
	if u, ok := s.(*stmt.Update); ok {
		for i, a := range u.Set {
			add(a.Value.Expression, ArithmeticSite{Set: i, Where: -1})
		}
	}
	where, subs := whereOf(s)
	for j, c := range where {
		add(c.Expression, ArithmeticSite{Set: -1, Where: j})
		add(c.Value.Expression, ArithmeticSite{Set: -1, Where: j})
	}
	for k, q := range subs {
		for j, c := range q.Where {
			add(c.Expression, ArithmeticSite{Set: -1, Where: j, Sub: k + 1})
			add(c.Value.Expression, ArithmeticSite{Set: -1, Where: j, Sub: k + 1})
		}
	}
	return sites
}

// Operators of the expression e: its nodes that compute, the negations and the binary operators, from
// the root, in the order of a walk that visits a node before its operands.
func Operators(e stmt.Expression) []stmt.Expression {
	var ops []stmt.Expression
	if len(e.Operands) > 0 {
		ops = append(ops, e)
	}
	for _, o := range e.Operands {
		ops = append(ops, Operators(o)...)
	}
	return ops
}

// EvaluationKinds are the kinds of an evaluation error of the evaluator.
var EvaluationKinds = []error{expr.ErrOverflow, expr.ErrDivisionByZero, expr.ErrNotFinite}

// Quirk of the operator n of arithmetic, of the kind of evaluation error, one of [EvaluationKinds],
// with the arguments args: a condition that SQLite makes true where its own result of n alone, on its
// operands, is the quirk that s4ql refuses with the error: a real from two integers for an overflow,
// NULL from two operands that are not NULL for a division by zero, and an infinity for a real result
// that is not finite; and the arguments of its parameters. SQLite's negation has no quirk but the
// overflow.
func Quirk(n stmt.Expression, kind error, args []any) (string, []any) {
	left, leftArgs := ExpressionText(n.Operands[0], args)
	if n.Kind == stmt.ExprNegate {
		if kind != expr.ErrOverflow {
			return "0", nil
		}
		return "typeof(" + left + ") = 'integer' and typeof(-(" + left + ")) = 'real'", append(slices.Clone(leftArgs), leftArgs...)
	}
	right, rightArgs := ExpressionText(n.Operands[1], args)
	value, valueArgs := "("+left+" "+n.Kind.String()+" "+right+")", append(slices.Clone(leftArgs), rightArgs...)
	switch kind {
	case expr.ErrOverflow:
		return "typeof(" + left + ") = 'integer' and typeof(" + right + ") = 'integer' and typeof(" + value + ") = 'real'", slices.Concat(leftArgs, rightArgs, valueArgs)
	case expr.ErrDivisionByZero:
		return left + " is not null and " + right + " is not null and " + value + " is null", slices.Concat(leftArgs, rightArgs, valueArgs)
	}
	return value + " in (9e999, -9e999)", valueArgs
}

// QuirkQuery of the operator n of the site of the statement s, of the kind, with the arguments args: a
// select that gives a row where SQLite shows the quirk of n, as [Quirk] says, for a row of the table
// of the site that every condition of guard holds for, or, where the root of the site has no column,
// for its one value, which needs no row. The conditions of guard are of the table of the site, those
// of the statement or of the subquery of the site, and a subquery among their values is written as
// the subquery of the statement is, unless subqueries gives the value that the engine computed for it,
// by its number from 1.
func QuirkQuery(s stmt.Statement, site ArithmeticSite, n stmt.Expression, kind error, guard []stmt.Condition, subqueries []stmt.Term, args []any) (string, []any) {
	condition, bound := Quirk(n, kind, args)
	if !site.Root.HasColumn() {
		return "select 1 where " + condition, bound
	}
	table := tableOf(s)
	_, subs := whereOf(s)
	if site.Sub > 0 {
		table, subs = subs[site.Sub-1].Table, nil
	}
	if subqueries != nil && site.Sub == 0 {
		guard = slices.Clone(guard)
		for i, c := range guard {
			if c.Value.Subquery > 0 {
				guard[i].Value = subqueries[c.Value.Subquery-1]
			}
		}
	}
	found, foundArgs := FoundQuery(table, guard, subs, args)
	found = strings.TrimSuffix(found, " limit 1")
	if len(guard) == 0 {
		found += " where "
	} else {
		found += " and "
	}
	return found + condition + " limit 1", append(foundArgs, bound...)
}

// tableOf the select, the update, or the delete s.
func tableOf(s stmt.Statement) stmt.Name {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Table
	case *stmt.Update:
		return s.Table
	case *stmt.Delete:
		return s.Table
	}
	return stmt.Name{}
}

// SiteGuard of the site of the statement s: the conditions that a row holds for where the documented
// rule may evaluate the expression of the site on it, of the table of the site: those before the
// condition of the site in the order of the text, of the WHERE of the statement or of its subquery,
// or, for SET, every condition of the WHERE. A row that a condition after it rules out, as a bound of
// the access may, is such a row too: the rule does not decide it.
func SiteGuard(s stmt.Statement, site ArithmeticSite) []stmt.Condition {
	where, subs := whereOf(s)
	if site.Sub > 0 {
		where = subs[site.Sub-1].Where
	}
	if site.Set >= 0 {
		return where
	}
	return where[:site.Where]
}

// Justify the evaluation error of the kind at the operator op whose node starts at the offset, of
// the statement s with the arguments args: what is wrong with it, or "". Its operator must be one of
// the statement, and SQLite's result of that operator alone must show the quirk of its kind, as
// [QuirkQuery] gives the query, for a row that the documented rule may evaluate, as [SiteGuard] gives
// its conditions. rows reports whether SQLite gives a row for a query with its arguments, on the
// database that the statement saw, and subqueries are as for [QuirkQuery]. Of two operators of one
// spelling whose nodes start at one offset, as the two + of `a + b + c`, or the negation and the
// subtraction of `-a - b`, which the message of the error does not tell apart, either justifies it.
func Justify(s stmt.Statement, args []any, kind error, op stmt.ExpressionKind, offset int, subqueries []stmt.Term, rows func(src string, args []any) bool) string {
	at := false
	for _, site := range ArithmeticSites(s) {
		for _, n := range Operators(site.Root) {
			if n.Span.Start != offset || n.Kind.String() != op.String() {
				continue
			}
			at = true
			if rows(QuirkQuery(s, site, n, kind, SiteGuard(s, site), subqueries, args)) {
				return ""
			}
		}
	}
	if at {
		return "SQLite's result of its operator justifies it for no row that the rule may evaluate"
	}
	return "is at no operator of the statement"
}

// evaluationPattern of the message of an evaluation error, as [expr.Error] writes it.
var evaluationPattern = regexp.MustCompile(`^s4ql: (integer overflow|division by zero|real result that is not finite) in ([-+*/]) \(at offset (\d+)\)$`)

// EvaluationOf the message of an evaluation error, as [expr.Error] writes it, such as "s4ql: integer
// overflow in + (at offset 17)": its kind, of [EvaluationKinds], its operator, and the offset of the
// node of its operator, and true; or false for another message. A - is a subtraction or a negation,
// which the message does not tell apart, and is given as [stmt.ExprSubtract].
func EvaluationOf(message string) (kind error, op stmt.ExpressionKind, offset int, ok bool) {
	m := evaluationPattern.FindStringSubmatch(message)
	if m == nil {
		return nil, 0, 0, false
	}
	for _, k := range EvaluationKinds {
		if strings.HasSuffix(k.Error(), m[1]) {
			kind = k
		}
	}
	op = map[string]stmt.ExpressionKind{"+": stmt.ExprAdd, "-": stmt.ExprSubtract, "*": stmt.ExprMultiply, "/": stmt.ExprDivide}[m[2]]
	offset, err := strconv.Atoi(m[3])
	return kind, op, offset, err == nil && kind != nil
}
