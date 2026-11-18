package stmt

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

// TestRecognize gives the recognizer trees that the parser never gives, for the paths that SQLite's
// grammar does not reach.
func TestRecognize(t *testing.T) {
	tok := func(kind token.Kind, offset int, text string) ast.Token {
		return ast.Token{Kind: kind, Offset: offset, Text: text}
	}
	span := func(n ast.Node, start, end int) {
		ast.SetSpan(n, ast.Span{Start: start, End: end})
	}

	t.Run("should not support a statement that ends where the recognizer expects another token", func(t *testing.T) {
		// CREATE TABLE t, without the parenthesis that the grammar needs.
		src := "create table t"
		tree := &ast.Statement{Cmd: &ast.CreateTable{Create: tok(token.Create, 0, "create"), Table: tok(token.Table, 7, "table"),
			Name: ast.QualifiedName{Name: tok(token.ID, 13, "t")}}}
		span(tree.Cmd, 0, 14)
		span(tree, 0, 14)
		s, err := recognize(src, tree)
		is.True(t, s == nil)
		is.Equal(t, "s4ql: not supported: statement (at offset 14)", err.Error())
	})

	t.Run("should not support a statement with a token that no node holds", func(t *testing.T) {
		// DROP TABLE t x, where the x is in the span of the statement and in no node.
		src := "drop table t x"
		tree := &ast.Statement{Cmd: &ast.Drop{Drop: tok(token.Drop, 0, "drop"), Object: tok(token.Table, 5, "table"),
			Name: ast.QualifiedName{Name: tok(token.ID, 11, "t")}}}
		span(tree.Cmd, 0, 12)
		span(tree, 0, 14)
		s, err := recognize(src, tree)
		is.True(t, s == nil)
		is.Equal(t, "s4ql: not supported: statement (at offset 13)", err.Error())
	})

	t.Run("should not support a statement with a token that the text does not have", func(t *testing.T) {
		// DROP TABLE t, where the tree holds the name t at an offset where the text has a space.
		src := "drop table  t"
		tree := &ast.Statement{Cmd: &ast.Drop{Drop: tok(token.Drop, 0, "drop"), Object: tok(token.Table, 5, "table"),
			Name: ast.QualifiedName{Name: tok(token.ID, 11, "t")}}}
		span(tree.Cmd, 0, 13)
		span(tree, 0, 13)
		s, err := recognize(src, tree)
		is.True(t, s == nil)
		is.Equal(t, "s4ql: not supported: DROP (at offset 12)", err.Error())
	})

	t.Run("should pass on a panic that is not its own", func(t *testing.T) {
		// CREATE TABLE t (, with no column, which the grammar does not allow.
		src := "create table t ("
		tree := &ast.Statement{Cmd: &ast.CreateTable{Create: tok(token.Create, 0, "create"), Table: tok(token.Table, 7, "table"),
			Name: ast.QualifiedName{Name: tok(token.ID, 13, "t")}, LParen: tok(token.LP, 15, "(")}}
		span(tree.Cmd, 0, 16)
		span(tree, 0, 16)
		defer func() {
			is.True(t, recover() != nil)
		}()
		_, _ = recognize(src, tree)
		t.Fatal("no panic")
	})
}
