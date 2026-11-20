package stmt

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
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

	t.Run("should not support a parameter whose name is not the next token, before it looks at the name", func(t *testing.T) {
		// INSERT INTO t (v) VALUES ( :a), where the tree holds the name :a at an offset where the text
		// has a space, so that the parameter that the text has is a token that the recognizer did not
		// take, and not a named parameter.
		src := "insert into t (v) values ( :a)"
		tree, _, err := parser.Parse(src)
		is.NotError(t, err)
		var find func(n ast.Node) *ast.Variable
		find = func(n ast.Node) *ast.Variable {
			if v, ok := n.(*ast.Variable); ok {
				return v
			}
			for _, c := range ast.Children(n) {
				if v := find(c); v != nil {
					return v
				}
			}
			return nil
		}
		v := find(tree)
		is.True(t, v != nil)
		v.Name.Offset--
		s, nse := recognize(src, tree)
		is.True(t, s == nil)
		is.Equal(t, "s4ql: not supported: parameter (at offset 27)", nse.Error())
	})

	t.Run("should panic for a blob literal of an odd number of hex digits, which the tokenizer never gives", func(t *testing.T) {
		// INSERT INTO t (v) VALUES (x'00'), where the tree holds the blob x'0'.
		src := "insert into t (v) values (x'00')"
		tree, _, err := parser.Parse(src)
		is.NotError(t, err)
		var find func(n ast.Node) *ast.Literal
		find = func(n ast.Node) *ast.Literal {
			if l, ok := n.(*ast.Literal); ok {
				return l
			}
			for _, c := range ast.Children(n) {
				if l := find(c); l != nil {
					return l
				}
			}
			return nil
		}
		l := find(tree)
		is.True(t, l != nil)
		l.Value.Text = "x'0'"
		defer func() {
			is.Equal(t, `stmt: the blob literal "x'0'": encoding/hex: odd length hex string`, fmt.Sprint(recover()))
		}()
		_, _ = recognize(src, tree)
		t.Fatal("no panic")
	})

	t.Run("should pass on a panic that is not its own", func(t *testing.T) {
		// INSERT INTO t (a b, with no comma between the columns, which the grammar does not allow.
		src := "insert into t (a b"
		tree := &ast.Statement{Cmd: &ast.Insert{Insert: tok(token.Insert, 0, "insert"), Into: tok(token.Into, 7, "into"),
			Table: &ast.TargetTable{Name: ast.QualifiedName{Name: tok(token.ID, 12, "t")}}, LParen: tok(token.LP, 14, "("),
			Columns: &ast.IDList{Names: []ast.Token{tok(token.ID, 15, "a"), tok(token.ID, 17, "b")}}}}
		span(tree.Cmd, 0, 18)
		span(tree, 0, 18)
		defer func() {
			is.True(t, recover() != nil)
		}()
		_, _ = recognize(src, tree)
		t.Fatal("no panic")
	})
}
