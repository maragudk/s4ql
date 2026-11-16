package parser_test

import (
	"errors"
	"testing"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
)

// parse src with the parser, as a result for the harness.
func parse(src string) parsetest.Result {
	_, tail, err := parser.Parse(src)
	if err != nil {
		var e *parser.Error
		if !errors.As(err, &e) {
			panic(err)
		}
		return parsetest.Result{Message: e.Message, Offset: e.Offset}
	}
	return parsetest.Result{OK: true, Offset: -1, Tail: tail}
}

// checkOwnership of the tokens of the first statement of src, if it parses.
func checkOwnership(t testing.TB, src string) {
	t.Helper()
	stmt, _, err := parser.Parse(src)
	if err != nil || stmt == nil {
		return
	}
	if err := ast.CheckOwnership(stmt, parser.Tokens(src, stmt)); err != nil {
		t.Errorf("%q: %v\n%v", src, err, ast.Dump(stmt))
	}
}

func corpusSQL(t testing.TB) []string {
	t.Helper()
	var sqls []string
	for _, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
	}
	return sqls
}

func TestParse_mutants(t *testing.T) {
	var sqls []string
	for i, sql := range corpusSQL(t) {
		sqls = append(sqls, parsetest.Mutants(sql, uint64(i), 3)...)
	}
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v agree, SQLite does not say on %v", r.Total, r.Agreed, r.NotSaid)
	for _, sql := range sqls {
		checkOwnership(t, sql)
	}
}

func TestParse_corpus(t *testing.T) {
	sqls := corpusSQL(t)
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v agree, SQLite does not say on %v", r.Total, r.Agreed, r.NotSaid)
	for _, sql := range sqls {
		checkOwnership(t, sql)
	}
}
