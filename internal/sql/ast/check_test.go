package ast_test

import (
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
)

// parse src, which must parse, and return its statement and the tokens read for it.
func parse(t *testing.T, src string) (*ast.Statement, []ast.Token) {
	t.Helper()
	stmt, _, err := parser.Parse(src)
	is.NotError(t, err)
	return stmt, parser.Tokens(src, stmt)
}

// TestCheckOwnership proves the check of ownership with trees that are broken in known ways: each
// must be found.
func TestCheckOwnership(t *testing.T) {
	const src = "select a + 1 as x from t where b;"

	t.Run("should pass a tree from the parser", func(t *testing.T) {
		stmt, read := parse(t, src)
		is.NotError(t, ast.CheckOwnership(stmt, read))
	})

	t.Run("should find a token that no node owns", func(t *testing.T) {
		stmt, read := parse(t, src)
		col := firstColumn(stmt)
		col.As = ast.Token{}
		col.Span.Start = col.Expr.Bounds().Start
		err := ast.CheckOwnership(stmt, read)
		is.True(t, err != nil && strings.Contains(err.Error(), `"as"`), errString(err))
	})

	t.Run("should find a token that two nodes own", func(t *testing.T) {
		stmt, read := parse(t, src)
		where := stmt.Cmd.(*ast.SelectCmd).Select.First.(*ast.SimpleSelect).Where
		dup := &ast.Literal{Value: where.Where}
		dup.Span = ast.Span{Start: where.Where.Offset, End: where.Where.End()}
		where.X = &ast.Binary{X: dup, Op: where.Where, Y: where.X}
		ast.SetSpan(where.X, ast.Span{Start: where.Where.Offset, End: where.X.(*ast.Binary).Y.Bounds().End})
		is.True(t, ast.CheckOwnership(stmt, read) != nil)
	})

	t.Run("should find a token that was not read", func(t *testing.T) {
		stmt, read := parse(t, src)
		is.True(t, ast.CheckOwnership(stmt, read[:len(read)-1]) != nil)
	})

	t.Run("should find a span that is too wide", func(t *testing.T) {
		stmt, read := parse(t, src)
		col := firstColumn(stmt)
		col.Span.End++
		err := ast.CheckOwnership(stmt, read)
		is.True(t, err != nil && strings.Contains(err.Error(), "ResultColumn has the span"), errString(err))
	})

	t.Run("should find a span that is too narrow", func(t *testing.T) {
		stmt, read := parse(t, src)
		stmt.Span.Start++
		is.True(t, ast.CheckOwnership(stmt, read) != nil)
	})

	t.Run("should find a child that overlaps a token of its parent", func(t *testing.T) {
		stmt, read := parse(t, src)
		col := firstColumn(stmt)
		col.Expr.(*ast.Binary).Span.End = col.As.End()
		col.Expr.(*ast.Binary).Y.(*ast.Literal).Span.End = col.As.End()
		err := ast.CheckOwnership(stmt, read)
		is.True(t, err != nil, errString(err))
	})

	t.Run("should find a token whose kind differs from the one read", func(t *testing.T) {
		stmt, read := parse(t, src)
		firstColumn(stmt).As.Kind = ast.Token{}.Kind + 3
		is.True(t, ast.CheckOwnership(stmt, read) != nil)
	})

	t.Run("should find an empty node outside its parent", func(t *testing.T) {
		stmt, read := parse(t, "create virtual table v using m(a, , b)")
		args := stmt.Cmd.(*ast.CreateVirtualTable).Args
		is.NotError(t, ast.CheckOwnership(stmt, read))
		args[1].Span = ast.Span{Start: 1000, End: 1000}
		is.True(t, ast.CheckOwnership(stmt, read) != nil)
	})
}

func firstColumn(stmt *ast.Statement) *ast.ResultColumn {
	return stmt.Cmd.(*ast.SelectCmd).Select.First.(*ast.SimpleSelect).Columns[0]
}

func errString(err error) string {
	if err == nil {
		return "no error"
	}
	return err.Error()
}
