package parser

import (
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// This file decides one thing only: whether SQLite reads the schema while it parses a multi-row
// VALUES, which decides the offset of the error of a variable #N at the end of the statement (see
// [parser.result]). sqlite3MultiValues in insert.c compiles the rows of a VALUES into a co-routine
// while it parses, and reads the schema first, when the statement has no WITH before it, the new
// row is constant, and either the co-routine has started or the row before has no affinity. Being
// constant is SQLite's exprIsConst in expr.c, on the expression as the grammar's actions build it,
// which is what the functions below follow on the tree.

// valuesRow notes a row of a VALUES after the first, which SQLite's parser reduces when the token at
// index at arrives; prev is the row before it, and started tells whether the co-routine of the
// VALUES has started. It returns whether it has started after this row.
func (p *parser) valuesRow(row, prev *ast.ValuesRow, at int, started bool) bool {
	if p.withAt >= 0 && p.withAt < at {
		return false
	}
	if !constantList(row.Exprs, true) || (!started && !noAffinity(prev.Exprs)) {
		return false
	}
	if !started && p.schemaReadAt < 0 {
		p.schemaReadAt = at
	}
	return true
}

// constantList reports whether every expression is constant: sqlite3ExprListIsConstant, where noIs
// makes IS and IS NOT not constant.
func constantList(exprs []ast.Expr, noIs bool) bool {
	for _, e := range exprs {
		if !constant(e, noIs) {
			return false
		}
	}
	return true
}

// noAffinity reports whether the row is constant and none of its values has an affinity:
// exprListIsNoAffinity in insert.c. At parse time only a CAST has one, through COLLATE and the
// first value of a row value.
func noAffinity(exprs []ast.Expr) bool {
	if !constantList(exprs, false) {
		return false
	}
	for _, e := range exprs {
		if hasAffinity(e) {
			return false
		}
	}
	return true
}

// hasAffinity of e: sqlite3ExprAffinity is not 0.
func hasAffinity(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Paren:
		return hasAffinity(e.X)
	case *ast.Collate:
		return hasAffinity(e.X)
	case *ast.Row:
		return hasAffinity(e.Exprs[0])
	case *ast.Cast:
		return true
	}
	return false
}

// constant reports whether e is constant: exprIsConst in expr.c with the walker's eCode 1, or 6 with
// noIs, on the expression that the grammar's actions build from e.
func constant(e ast.Expr, noIs bool) bool {
	switch e := e.(type) {
	case *ast.Literal:
		if e.Value.Kind == token.CTimeKw {
			// term ::= CTIME_KW builds a call of the function of that name without arguments.
			return constantFunction(e.Value.Text, 0)
		}
		return true
	case *ast.Variable:
		return true
	case *ast.ColumnRef:
		// A single unquoted true or false is TK_TRUEFALSE; any other name is not constant.
		if e.Table.Present() || isQuoted(e.Column.Text) {
			return false
		}
		name := asciiLower(e.Column.Text)
		return name == "true" || name == "false"
	case *ast.Paren:
		return constant(e.X, noIs)
	case *ast.Row:
		return constantList(e.Exprs, noIs)
	case *ast.Unary:
		return constant(e.X, noIs)
	case *ast.Binary:
		if e.Op.Kind == token.Ptr {
			// expr ::= expr PTR expr builds a call of the function -> or ->>.
			return constant(e.X, noIs) && constant(e.Y, noIs) && constantFunction(e.Op.Text, 2)
		}
		return constant(e.X, noIs) && constant(e.Y, noIs)
	case *ast.Is:
		if isNull(e.Y) {
			// sqlite3PExprIs makes X IS NULL into ISNULL, which is constant if X is.
			return constant(e.X, noIs)
		}
		return !noIs && constant(e.X, noIs) && constant(e.Y, noIs)
	case *ast.NullTest:
		return constant(e.X, noIs)
	case *ast.Like:
		args := []ast.Expr{e.Y, e.X}
		if e.EscapeX != nil {
			args = append(args, e.EscapeX)
		}
		return constantList(args, noIs) && constantFunction(e.Op.Text, len(args))
	case *ast.Between:
		return constant(e.X, noIs) && constant(e.Low, noIs) && constant(e.High, noIs)
	case *ast.InList:
		if len(e.List) == 0 {
			// x IN () is false, and x NOT IN () true; with a function in x, false AND x or true OR x.
			return !hasFunction(e.X) || constant(e.X, noIs)
		}
		if isRow(e.X) {
			// A row value IN a list becomes IN a VALUES, a SELECT.
			return false
		}
		return constant(e.X, noIs) && constantList(e.List, noIs)
	case *ast.Collate:
		return constant(e.X, noIs)
	case *ast.Cast:
		return constant(e.X, noIs)
	case *ast.FuncCall:
		if e.Filter != nil || e.Over != nil {
			return false
		}
		return constantList(e.Args, noIs) && constantFunction(dequote(e.Name.Text), len(e.Args))
	case *ast.Case:
		if e.Operand != nil && !constant(e.Operand, noIs) {
			return false
		}
		for _, w := range e.Whens {
			if !constant(w.Condition, noIs) || !constant(w.Result, noIs) {
				return false
			}
		}
		return e.ElseX == nil || constant(e.ElseX, noIs)
	}
	// A SELECT in any form, and RAISE, are not constant.
	return false
}

// isNull reports whether e is NULL, in any number of parentheses, which leave no node in SQLite's
// tree.
func isNull(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.Paren:
			e = x.X
		case *ast.Literal:
			return x.Value.Kind == token.Null
		default:
			return false
		}
	}
}

// isRow reports whether e is a row value, in any number of parentheses.
func isRow(e ast.Expr) bool {
	for {
		switch x := e.(type) {
		case *ast.Paren:
			e = x.X
		case *ast.Row:
			return true
		default:
			return false
		}
	}
}

// hasFunction reports whether e holds a call of a function outside a SELECT, as SQLite's EP_HasFunc
// does: a call by name, LIKE and its kind, -> and ->>, and CURRENT_TIME and its kind.
func hasFunction(e ast.Expr) bool {
	found := false
	ast.Walk(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncCall, *ast.Like:
			found = true
		case *ast.Binary:
			found = found || n.Op.Kind == token.Ptr
		case *ast.Literal:
			found = found || n.Value.Kind == token.CTimeKw
		case *ast.Select:
			return false
		}
		return !found
	})
	return found
}

// constantFunction reports whether a call of the function name with n arguments is constant when
// its arguments are: the function that sqlite3FindFunction finds for it has SQLITE_FUNC_CONSTANT
// or SQLITE_FUNC_SLOCHNG and is not an aggregate. It finds the overload of the best match, as
// matchQuality in callback.c rates them: the exact number of arguments over any number.
func constantFunction(name string, n int) bool {
	name = asciiLower(name)
	best, quality := false, 0
	for _, f := range builtinFunctions {
		if f.name != name {
			continue
		}
		q := 0
		switch {
		case f.nArg == n:
			q = 2
		case f.nArg == -1, f.nArg == -3 && n >= 1, f.nArg == -4 && n >= 2:
			q = 1
		}
		if q > quality {
			best, quality = f.constant, q
		}
	}
	return best
}

// isQuoted reports whether a name is quoted: sqlite3Isquote of its first byte.
func isQuoted(text string) bool {
	return text != "" && (text[0] == '"' || text[0] == '\'' || text[0] == '`' || text[0] == '[')
}

// dequote a name as sqlite3Dequote does: in double quotes, single quotes, or backticks, with its
// doubled quote characters made single, and in square brackets as it is inside.
func dequote(text string) string {
	if !isQuoted(text) {
		return text
	}
	q := text[0]
	if q == '[' {
		q = ']'
	}
	var b strings.Builder
	for i := 1; i < len(text); i++ {
		c := text[i]
		if c == q {
			if q != ']' && i+1 < len(text) && text[i+1] == q {
				b.WriteByte(c)
				i++
				continue
			}
			break
		}
		b.WriteByte(c)
	}
	return b.String()
}

// asciiLower of s, as SQLite compares names: only ASCII letters fold.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
