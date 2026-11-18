package parser_test

import (
	"flag"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/token"
)

var (
	grammarOnce sync.Once
	theGrammar  *parsetest.Grammar
	grammarErr  error
)

// readGrammar of testdata/parse.y, once.
func readGrammar(t testing.TB) *parsetest.Grammar {
	t.Helper()
	grammarOnce.Do(func() {
		b, err := os.ReadFile("testdata/parse.y")
		if err != nil {
			grammarErr = err
			return
		}
		theGrammar, grammarErr = parsetest.ParseGrammar(string(b))
	})
	if grammarErr != nil {
		t.Fatal(grammarErr)
	}
	return theGrammar
}

// lemonRules of the parser that is compiled into the binding: the rules in the comments of the
// table yyRuleInfoLhs in the vendored amalgamation, which Lemon writes as `/* (N) rule */`.
func lemonRules(t testing.TB) []string {
	t.Helper()
	b, err := os.ReadFile("../../sqlite/c/sqlite3.c")
	is.NotError(t, err)
	src := string(b)
	start := strings.Index(src, "static const YYCODETYPE yyRuleInfoLhs[] = {")
	is.True(t, start >= 0)
	end := start + strings.Index(src[start:], "};")
	var rules []string
	for _, line := range strings.Split(src[start:end], "\n") {
		_, after, ok := strings.Cut(line, "/* (")
		if !ok {
			continue
		}
		_, r, _ := strings.Cut(after, ") ")
		rules = append(rules, strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(r), "*/")))
	}
	return rules
}

// TestGrammar checks that the rules read from parse.y are those of the parser that SQLite was built
// with, one by one, and that the keywords of its %fallback are those of the parser. Lemon numbers
// the rules in an order of its own, with the rules that have code first, so the lists are compared
// sorted.
func TestGrammar(t *testing.T) {
	g := readGrammar(t)
	lemon := lemonRules(t)
	is.Equal(t, 412, len(lemon))
	var ours []string
	for _, r := range g.Rules {
		ours = append(ours, r.String())
	}
	slices.Sort(ours)
	lemon = slices.Clone(lemon)
	slices.Sort(lemon)
	is.EqualSlice(t, lemon, ours)

	is.Equal(t, 73, len(g.Fallback))
	fallback := map[string]bool{}
	for _, name := range g.Fallback {
		fallback[name] = true
	}
	for k := range token.Kind(255) {
		if k.IsKeyword() {
			is.Equal(t, fallback[k.String()], parser.IsFallback(k), k.String())
		}
	}
}

var generatedCount = flag.Int("generated", 20000, "sentences of the generator in TestGenerate")

// TestGenerate compares the parser with SQLite on sentences of the grammar and their mutants, and
// checks the ownership of the tokens of those that parse.
func TestGenerate(t *testing.T) {
	n := *generatedCount
	if slow() {
		n /= 10
	}
	sqls := generated(t, *seed, n, 2)
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	parsed := 0
	for _, sql := range sqls {
		if parse(sql).OK {
			parsed++
		}
		checkOwnership(t, sql)
	}
	t.Logf("%v texts, %v parse, %v agree, SQLite does not say on %v", r.Total, parsed, r.Agreed, r.NotSaid)
	if !slow() && atDefault("seed", "generated") {
		pinned(t, "texts of the generator on which SQLite does not say", 5357, r.NotSaid)
		pinned(t, "texts of the generator that parse", 31731, parsed)
	}
}

// FuzzGenerate compares the parser with SQLite on a sentence of the grammar and a mutant of it,
// from a seed that the fuzzer chooses.
func FuzzGenerate(f *testing.F) {
	g := readGrammar(f)
	f.Add(uint64(1), uint8(10), uint64(2))
	f.Fuzz(func(t *testing.T, seed uint64, budget uint8, mutant uint64) {
		gen := parsetest.NewGenerator(g, seed)
		s := gen.Sentence("input", int(budget%100))
		for _, sql := range append([]string{s}, parsetest.Mutants(s, mutant, 1)...) {
			if d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), sql); found {
				t.Fatal(d)
			}
			checkOwnership(t, sql)
		}
	})
}

// generated sentences of the grammar, n of them from seed, from its start symbol input, with
// mutantsEach mutants of each.
func generated(t testing.TB, seed uint64, n, mutantsEach int) []string {
	t.Helper()
	gen := parsetest.NewGenerator(readGrammar(t), seed)
	var sqls []string
	for i := range n {
		s := gen.Sentence("input", 4+gen.IntN(40))
		sqls = append(sqls, s)
		sqls = append(sqls, parsetest.Mutants(s, seed+uint64(i), mutantsEach)...)
	}
	return sqls
}
