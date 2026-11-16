package parser_test

import (
	"errors"
	"flag"
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

// every nth of sqls.
func every(sqls []string, n int) []string {
	var some []string
	for i := 0; i < len(sqls); i += n {
		some = append(some, sqls[i])
	}
	return some
}

var mutants = flag.Int("mutants", 3, "mutants of each corpus entry in TestParse_mutants")
var seed = flag.Uint64("seed", 0, "seed of the mutants in TestParse_mutants")

func TestParse_mutants(t *testing.T) {
	var sqls []string
	for i, sql := range corpusSQL(t) {
		sqls = append(sqls, parsetest.Mutants(sql, *seed+uint64(i), *mutants)...)
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

// FuzzParse compares the parser with SQLite on any text, and checks the ownership of the tokens of
// every statement that ParseAll reads from it.
func FuzzParse(f *testing.F) {
	for i, sql := range corpusSQL(f) {
		if i%25 == 0 {
			f.Add(sql)
		}
	}
	own, err := corpus.Read("own")
	if err != nil {
		f.Fatal(err)
	}
	for _, sql := range own {
		f.Add(sql)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), src); found {
			t.Fatal(d)
		}
		stmts, _ := parser.ParseAll(src)
		for _, stmt := range stmts {
			if err := ast.CheckOwnership(stmt, parser.Tokens(src, stmt)); err != nil {
				t.Fatalf("%q: %v\n%v", src, err, ast.Dump(stmt))
			}
		}
	})
}
