package ast_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
)

// parse src, which must parse, and return its statement and the tokens read for it.
func parse(t *testing.T, src string) (*ast.Statement, []ast.Token) {
	t.Helper()
	stmt, _, err := parser.Parse(src)
	is.NotError(t, err)
	return stmt, parser.Tokens(src, stmt)
}

// TestCheckOwnership proves the check of ownership with trees that are broken in known ways: each
// must be found, by the part of the check that is there for it, with its message.
func TestCheckOwnership(t *testing.T) {
	const src = "select a + 1 as x from t where b;"

	t.Run("should pass a tree from the parser", func(t *testing.T) {
		stmt, read := parse(t, src)
		is.NotError(t, ast.CheckOwnership(stmt, read))
	})

	t.Run("should find a token that no node owns", func(t *testing.T) {
		stmt, read := parse(t, src)
		read = append(read, ast.Token{Kind: token.ID, Offset: 40, Text: "z"})
		is.Equal(t, `token "z" at 40 is owned by no node`, errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a token that is missing from the tree", func(t *testing.T) {
		stmt, read := parse(t, src)
		col := firstColumn(stmt)
		col.As = ast.Token{}
		col.Span.Start = col.Expr.Bounds().Start
		is.Equal(t, `token 4 of the tree is "x" at 16 of kind ID, but the token read is "as" at 13 of kind AS`,
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a token that two nodes own", func(t *testing.T) {
		stmt, read := parse(t, src)
		where := stmt.Cmd.(*ast.SelectCmd).Select.First.(*ast.SimpleSelect).Where
		dup := &ast.Literal{Value: where.Where}
		dup.Span = ast.Span{Start: where.Where.Offset, End: where.Where.End()}
		where.X = &ast.Binary{X: dup, Op: where.Where, Y: where.X}
		ast.SetSpan(where.X, ast.Span{Start: where.Where.Offset, End: where.X.(*ast.Binary).Y.Bounds().End})
		is.Equal(t, `in *ast.Binary, *ast.Literal at {25 30} overlaps token "where" at {25 30}`,
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a token that was not read", func(t *testing.T) {
		stmt, read := parse(t, src)
		is.Equal(t, `token ";" at 32 is owned by a node, but was not read`, errString(ast.CheckOwnership(stmt, read[:len(read)-1])))
	})

	t.Run("should find a span that is too wide", func(t *testing.T) {
		stmt, read := parse(t, src)
		firstColumn(stmt).Span.End++
		is.Equal(t, "*ast.ResultColumn has the span {7 18}, but its tokens and children cover {7 17}",
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a span that is too narrow", func(t *testing.T) {
		stmt, read := parse(t, src)
		stmt.Span.Start++
		is.Equal(t, "*ast.Statement has the span {1 33}, but its tokens and children cover {0 33}",
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a span that ends before it starts", func(t *testing.T) {
		stmt, read := parse(t, src)
		firstColumn(stmt).Span = ast.Span{Start: 17, End: 7}
		is.Equal(t, "*ast.ResultColumn has the span {17 7}", errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a child whose span covers a token of its parent", func(t *testing.T) {
		stmt, read := parse(t, src)
		col := firstColumn(stmt)
		col.Expr.(*ast.Binary).Span.End = col.As.End()
		col.Expr.(*ast.Binary).Y.(*ast.Literal).Span.End = col.As.End()
		is.Equal(t, "*ast.Literal has the span {11 15}, but its tokens and children cover {11 12}",
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a token whose kind differs from the one read", func(t *testing.T) {
		stmt, read := parse(t, src)
		firstColumn(stmt).As.Kind = token.ID
		is.Equal(t, `token 4 of the tree is "as" at 13 of kind ID, but the token read is "as" at 13 of kind AS`,
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find a token without text", func(t *testing.T) {
		stmt, read := parse(t, src)
		firstColumn(stmt).As.Text = ""
		is.Equal(t, "*ast.ResultColumn owns an empty token at 13", errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find an empty node outside its parent", func(t *testing.T) {
		stmt, read := parse(t, "create virtual table v using m(a, , b)")
		args := stmt.Cmd.(*ast.CreateVirtualTable).Args
		is.NotError(t, ast.CheckOwnership(stmt, read))
		args[1].Span = ast.Span{Start: 1000, End: 1000}
		is.Equal(t, "in *ast.CreateVirtualTable with the span {0 38}, the empty *ast.ModuleArg is at {1000 1000}",
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should find an empty node before its parent", func(t *testing.T) {
		stmt, read := parse(t, "create virtual table v using m(a, , b)")
		stmt.Cmd.(*ast.CreateVirtualTable).Args[1].Span = ast.Span{Start: -1, End: -1}
		is.Equal(t, "in *ast.CreateVirtualTable with the span {0 38}, the empty *ast.ModuleArg is at {-1 -1}",
			errString(ast.CheckOwnership(stmt, read)))
	})

	t.Run("should accept an empty node at either end of its parent", func(t *testing.T) {
		for _, at := range []int{0, 38} {
			stmt, read := parse(t, "create virtual table v using m(a, , b)")
			stmt.Cmd.(*ast.CreateVirtualTable).Args[1].Span = ast.Span{Start: at, End: at}
			is.NotError(t, ast.CheckOwnership(stmt, read))
		}
	})

	t.Run("should find a node without tokens whose span is not empty", func(t *testing.T) {
		stmt, read := parse(t, "create virtual table v using m(a, , b)")
		stmt.Cmd.(*ast.CreateVirtualTable).Args[1].Span = ast.Span{Start: 34, End: 35}
		is.Equal(t, "*ast.ModuleArg has no tokens, but its span is {34 35}", errString(ast.CheckOwnership(stmt, read)))
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
