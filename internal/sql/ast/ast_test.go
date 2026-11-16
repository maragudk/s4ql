package ast_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
)

func TestToken(t *testing.T) {
	t.Run("should be absent as the zero value", func(t *testing.T) {
		is.True(t, !ast.Token{}.Present())
		is.Equal(t, "<absent>", ast.Token{}.String())
	})

	t.Run("should end after its text", func(t *testing.T) {
		tok := ast.Token{Kind: token.ID, Offset: 7, Text: `"a b"`}
		is.True(t, tok.Present())
		is.Equal(t, 12, tok.End())
		is.Equal(t, `"a b"`, tok.String())
	})
}

func TestWalk(t *testing.T) {
	t.Run("should visit every node in depth-first order, and skip the children when told", func(t *testing.T) {
		stmt, _ := parse(t, "select a + 1, b from t")
		var kinds []string
		ast.Walk(stmt, func(n ast.Node) bool {
			kinds = append(kinds, typeName(n))
			_, isBinary := n.(*ast.Binary)
			return !isBinary
		})
		is.EqualSlice(t, []string{"Statement", "SelectCmd", "Select", "SimpleSelect", "ResultColumn", "Binary",
			"ResultColumn", "ColumnRef", "From", "FromItem", "TableSource"}, kinds)
	})

	t.Run("should give the tokens of the whole tree in order", func(t *testing.T) {
		stmt, read := parse(t, "select a + 1 from t;")
		is.EqualSlice(t, read, ast.AllTokens(stmt))
		is.Equal(t, 1, len(ast.Tokens(stmt)))
		is.Equal(t, ";", ast.Tokens(stmt)[0].Text)
		children := ast.Children(stmt.Cmd.(*ast.SelectCmd).Select.First)
		is.Equal(t, 2, len(children))
		is.Equal(t, "ResultColumn", typeName(children[0]))
		is.Equal(t, "From", typeName(children[1]))
	})
}

func TestDump(t *testing.T) {
	t.Run("should write the fields that are present, structs that are not nodes in braces", func(t *testing.T) {
		stmt, _ := parse(t, "drop table if exists s.t")
		is.Equal(t, "Statement{Cmd: Drop{Drop: drop, Object: table, IfExists: {If: if, Exists: exists}, Name: {Schema: s, Dot: ., Name: t}}}", ast.Dump(stmt))
	})
}

func typeName(n ast.Node) string {
	d := ast.Dump(n)
	for i, c := range d {
		if c == '{' {
			return d[:i]
		}
	}
	return d
}
