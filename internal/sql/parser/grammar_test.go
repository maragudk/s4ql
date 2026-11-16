package parser_test

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/token"
)

// rule of SQLite's grammar, as read from testdata/parse.y: a nonterminal and the symbols it expands
// to. A symbol is a nonterminal, a terminal, or a set of terminals: a token class such as ids, or
// terminals joined by |, such as COMMIT|END.
type rule struct {
	lhs string
	rhs []symbol
	// line of the rule in parse.y.
	line int
}

// symbol on the right side of a rule: a nonterminal, or one or more terminals.
type symbol struct {
	nonterminal string
	terminals   []string
}

func (s symbol) String() string {
	if s.nonterminal != "" {
		return s.nonterminal
	}
	return strings.Join(s.terminals, "|")
}

// String of the rule in the form that Lemon writes in comments of the generated parser, with token
// classes written as their terminals.
func (r rule) String() string {
	var b strings.Builder
	b.WriteString(r.lhs)
	b.WriteString(" ::=")
	for _, s := range r.rhs {
		b.WriteByte(' ')
		b.WriteString(s.String())
	}
	return b.String()
}

// grammar of SQLite as read from parse.y, with the %if sections resolved for the compile options of
// the binding, which defines none of the macros that parse.y tests.
type grammar struct {
	rules    []rule
	byLHS    map[string][]int
	fallback []string
	// classes of tokens: %token_class name A|B.
	classes map[string][]string
}

var (
	grammarOnce sync.Once
	theGrammar  *grammar
	grammarErr  error
)

// readGrammar of testdata/parse.y, once.
func readGrammar(t testing.TB) *grammar {
	t.Helper()
	grammarOnce.Do(func() {
		b, err := os.ReadFile("testdata/parse.y")
		if err != nil {
			grammarErr = err
			return
		}
		theGrammar, grammarErr = parseGrammar(string(b))
	})
	if grammarErr != nil {
		t.Fatal(grammarErr)
	}
	return theGrammar
}

// grammarLexer reads the parts of a Lemon grammar file that matter for its rules: words,
// punctuation, and blocks of C code in braces, which it skips as one item.
type grammarLexer struct {
	src  string
	i    int
	line int
}

// item of a Lemon grammar file.
type item struct {
	text string
	line int
	// block is true for a block of C code in braces.
	block bool
	// lineStart is true if the item is the first on its line.
	lineStart bool
}

func (l *grammarLexer) next() (item, bool) {
	lineStart := false
	for l.i < len(l.src) {
		c := l.src[l.i]
		switch {
		case c == '\n':
			l.line++
			l.i++
			lineStart = true
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case strings.HasPrefix(l.src[l.i:], "//"):
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
		case strings.HasPrefix(l.src[l.i:], "/*"):
			end := strings.Index(l.src[l.i+2:], "*/")
			l.line += strings.Count(l.src[l.i:l.i+2+end+2], "\n")
			l.i += 2 + end + 2
		default:
			goto token
		}
	}
	return item{}, false
token:
	start, line := l.i, l.line+1
	c := l.src[l.i]
	switch {
	case c == '{':
		depth := 0
		for l.i < len(l.src) {
			if strings.HasPrefix(l.src[l.i:], "/*") {
				end := l.i + 2 + strings.Index(l.src[l.i+2:], "*/") + 2
				l.line += strings.Count(l.src[l.i:end], "\n")
				l.i = end
				continue
			}
			if strings.HasPrefix(l.src[l.i:], "//") {
				for l.i < len(l.src) && l.src[l.i] != '\n' {
					l.i++
				}
				continue
			}
			switch d := l.src[l.i]; d {
			case '{':
				depth++
			case '}':
				depth--
			case '\n':
				l.line++
			case '"', '\'':
				// A string or character literal of C: skip it, with its escapes.
				for l.i++; l.i < len(l.src) && l.src[l.i] != d; l.i++ {
					if l.src[l.i] == '\\' {
						l.i++
					}
				}
			}
			l.i++
			if depth == 0 {
				break
			}
		}
		return item{text: l.src[start:l.i], line: line, block: true, lineStart: lineStart}, true
	case c == '%' || c == '_' || unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)):
		l.i++
		for l.i < len(l.src) && (l.src[l.i] == '_' || unicode.IsLetter(rune(l.src[l.i])) || unicode.IsDigit(rune(l.src[l.i]))) {
			l.i++
		}
	case strings.HasPrefix(l.src[l.i:], "::="):
		l.i += 3
	default:
		l.i++
	}
	return item{text: l.src[start:l.i], line: line, lineStart: lineStart}, true
}

// parseGrammar reads the rules, token classes, and fallbacks of a Lemon grammar file.
func parseGrammar(src string) (*grammar, error) {
	g := &grammar{byLHS: map[string][]int{}, classes: map[string][]string{}}
	l := &grammarLexer{src: src}
	var items []item
	for {
		it, ok := l.next()
		if !ok {
			break
		}
		items = append(items, it)
	}

	// Resolve the %if sections: none of the macros that parse.y tests is defined.
	var kept []item
	var active []bool // per open section: whether its items are kept
	on := func() bool { return !slices.Contains(active, false) }
	for i := 0; i < len(items); i++ {
		it := items[i]
		// The condition of a directive is the rest of its line.
		cond := func() string {
			var words []string
			for i+1 < len(items) && !items[i+1].lineStart {
				i++
				words = append(words, items[i].text)
			}
			return strings.Join(words, " ")
		}
		switch it.text {
		case "%ifdef":
			cond()
			active = append(active, false)
		case "%ifndef":
			cond()
			active = append(active, true)
		case "%if":
			active = append(active, evalCondition(cond()))
		case "%else":
			cond()
			active[len(active)-1] = !active[len(active)-1]
		case "%endif":
			cond()
			active = active[:len(active)-1]
		default:
			if on() {
				kept = append(kept, it)
			}
		}
	}
	if len(active) != 0 {
		return nil, fmt.Errorf("unbalanced %%if sections")
	}

	// oneArgument directives take a word or a block; typed directives a word and a block; the rest
	// end with a period.
	oneArgument := map[string]bool{"%include": true, "%stack_size": true, "%stack_size_limit": true,
		"%realloc": true, "%free": true, "%token_prefix": true, "%token_type": true,
		"%default_type": true, "%extra_context": true, "%syntax_error": true,
		"%stack_overflow": true, "%name": true}
	typed := map[string]bool{"%type": true, "%destructor": true}
	for i := 0; i < len(kept); i++ {
		it := kept[i]
		switch {
		case oneArgument[it.text]:
			i++
		case typed[it.text]:
			i += 2
		case strings.HasPrefix(it.text, "%"):
			var args []string
			for i++; kept[i].text != "."; i++ {
				args = append(args, kept[i].text)
			}
			switch it.text {
			case "%fallback":
				g.fallback = append(g.fallback, args[1:]...)
			case "%token_class":
				for _, a := range args[1:] {
					if a != "|" {
						g.classes[args[0]] = append(g.classes[args[0]], a)
					}
				}
			}
		case it.block:
			// The code of the rule before it.
		default:
			// A rule: lhs [(alias)] ::= symbols . [[PREC]]
			r := rule{lhs: it.text, line: it.line}
			i++
			if kept[i].text == "(" {
				i += 3
			}
			if kept[i].text != "::=" {
				return nil, fmt.Errorf("line %v: expected ::= after %v, got %q", it.line, it.text, kept[i].text)
			}
			for i++; kept[i].text != "."; i++ {
				s := kept[i].text
				if s == "(" {
					i += 2
					continue
				}
				if s == "|" {
					i++
					last := &r.rhs[len(r.rhs)-1]
					last.terminals = append(last.terminals, kept[i].text)
					continue
				}
				if unicode.IsUpper(rune(s[0])) {
					r.rhs = append(r.rhs, symbol{terminals: []string{s}})
				} else if class, ok := g.classes[s]; ok {
					r.rhs = append(r.rhs, symbol{terminals: class})
				} else {
					r.rhs = append(r.rhs, symbol{nonterminal: s})
				}
			}
			if i+1 < len(kept) && kept[i+1].text == "[" {
				i += 3
			}
			g.byLHS[r.lhs] = append(g.byLHS[r.lhs], len(g.rules))
			g.rules = append(g.rules, r)
		}
	}
	return g, nil
}

// evalCondition of %if with no macro defined: identifiers are false, and !, &&, and || combine them.
func evalCondition(cond string) bool {
	or := false
	for _, alt := range strings.Split(cond, "| |") {
		and := true
		for _, term := range strings.Split(alt, "& &") {
			term = strings.TrimSpace(term)
			value := false
			for strings.HasPrefix(term, "!") {
				value = !value
				term = strings.TrimSpace(term[1:])
			}
			and = and && value
		}
		or = or || and
	}
	return or
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
	for _, r := range g.rules {
		ours = append(ours, r.String())
	}
	slices.Sort(ours)
	lemon = slices.Clone(lemon)
	slices.Sort(lemon)
	is.EqualSlice(t, lemon, ours)

	is.Equal(t, 73, len(g.fallback))
	fallback := map[string]bool{}
	for _, name := range g.fallback {
		fallback[name] = true
	}
	for k := range token.Kind(255) {
		if k.IsKeyword() {
			is.Equal(t, fallback[k.String()], parser.IsFallback(k), k.String())
		}
	}
}

// namePool for the names that the generator writes for ID: plain names, names in every quote style,
// keywords that fall back to ID, and keywords that do not, which the grammar rejects there.
var namePool = func() []string {
	p := []string{"t1", "a", "b", "x", "rowid", "main", "temp", "_x", "x1$", "été",
		`"t1"`, `"a""b"`, "[a b]", "`t1`", "'t1'",
		"abort", "action", "after", "analyze", "asc", "attach", "before", "begin", "by", "cascade",
		"cast", "column", "conflict", "database", "deferred", "desc", "detach", "do", "each", "end",
		"exclusive", "explain", "fail", "for", "ignore", "immediate", "initially", "instead", "like",
		"glob", "regexp", "match", "no", "plan", "query", "key", "of", "offset", "pragma", "raise",
		"recursive", "release", "replace", "restrict", "row", "rows", "rollback", "savepoint",
		"temporary", "trigger", "vacuum", "view", "virtual", "with", "without", "nulls", "first", "last",
		"current", "following", "partition", "preceding", "range", "unbounded", "exclude", "groups",
		"others", "ties", "generated", "always", "materialized", "reindex", "rename", "current_date",
		"current_time", "current_timestamp", "if", "window", "over", "filter",
		"indexed", "left", "outer", "natural", "cross", "inner", "full", "right",
		"select", "from", "where", "table", "and", "not", "null", "union", "primary",
	}
	return p
}()

// terminalTexts gives texts for each terminal of the grammar that is not a keyword.
var terminalTexts = map[string][]string{
	"SEMI": {";"}, "LP": {"("}, "RP": {")"}, "COMMA": {","}, "DOT": {"."}, "PLUS": {"+"},
	"MINUS": {"-"}, "STAR": {"*"}, "SLASH": {"/"}, "REM": {"%"}, "CONCAT": {"||"},
	"PTR": {"->", "->>"}, "BITAND": {"&"}, "BITOR": {"|"}, "BITNOT": {"~"}, "LSHIFT": {"<<"},
	"RSHIFT": {">>"}, "EQ": {"=", "=="}, "NE": {"!=", "<>"}, "LT": {"<"}, "LE": {"<="}, "GT": {">"},
	"GE":     {">="},
	"STRING": {"'s'", "''", "'it''s'"}, "INTEGER": {"1", "0", "42", "0x1F", "9223372036854775808"},
	"FLOAT": {"1.5", ".5", "1e3", "2.5E-3"}, "BLOB": {"x'00'", "X'CAFE'", "x''"},
	"QNUMBER":  {"1_000", "0x1_F", "1_0.5", "1_", "1__2"},
	"VARIABLE": {"?", "?1", ":a", "@a", "$a", "$a::b(c)", "#a", "#1"},
	"CTIME_KW": {"current_date", "current_time", "current_timestamp"},
	"LIKE_KW":  {"like", "glob", "regexp"},
	"JOIN_KW":  {"left", "right", "full", "inner", "cross", "natural", "outer"},
	"TEMP":     {"temp", "temporary"}, "AUTOINCR": {"autoincrement"}, "COLUMNKW": {"column"},
	"ISNULL": {"isnull"}, "NOTNULL": {"notnull"},
}

// generator of random sentences of the grammar.
type generator struct {
	g *grammar
	r *rand.Rand
	// height of each nonterminal: the least depth of a derivation of it into terminals.
	height map[string]int
	// covered rules, by index, of the sentences generated.
	covered []int
	// used rules, by index, in the derivation of the last sentence.
	used []int
}

func newGenerator(g *grammar, seed uint64) *generator {
	gen := &generator{g: g, r: rand.New(rand.NewPCG(seed, 0x5eed)), height: map[string]int{}, covered: make([]int, len(g.rules))}
	// Iterate to a fixed point: the height of a rule is one more than the greatest height of the
	// nonterminals on its right.
	for changed := true; changed; {
		changed = false
		for _, r := range g.rules {
			h, ok := 1, true
			for _, s := range r.rhs {
				if s.nonterminal != "" {
					sh, known := gen.height[s.nonterminal]
					if !known {
						ok = false
						break
					}
					h = max(h, sh+1)
				}
			}
			if old, known := gen.height[r.lhs]; ok && (!known || h < old) {
				gen.height[r.lhs] = h
				changed = true
			}
		}
	}
	return gen
}

// ruleHeight of the rule with index i.
func (gen *generator) ruleHeight(i int) int {
	h := 1
	for _, s := range gen.g.rules[i].rhs {
		if s.nonterminal != "" {
			h = max(h, gen.height[s.nonterminal]+1)
		}
	}
	return h
}

// sentence derived from the nonterminal start, with at most about budget nonterminals expanded
// freely; past that, the rules of least height are taken, so that every sentence is finite.
func (gen *generator) sentence(start string, budget int) string {
	gen.used = gen.used[:0]
	var words []string
	gen.expand(start, &budget, &words)
	var b strings.Builder
	for i, w := range words {
		if i > 0 {
			switch gen.r.IntN(20) {
			case 0:
				b.WriteString("\n")
			case 1:
				b.WriteString(" /* c */ ")
			default:
				b.WriteByte(' ')
			}
		}
		b.WriteString(w)
	}
	return b.String()
}

func (gen *generator) expand(nt string, budget *int, words *[]string) {
	rules := gen.g.byLHS[nt]
	var choice int
	if *budget > 0 {
		*budget--
		// Half the time, take the rule of the nonterminal that has been used least, so that every rule
		// gets its turn; otherwise any rule.
		choice = rules[gen.r.IntN(len(rules))]
		if gen.r.IntN(2) == 0 {
			for _, i := range rules {
				if gen.covered[i] < gen.covered[choice] {
					choice = i
				}
			}
		}
	} else {
		least := -1
		for _, i := range rules {
			if least < 0 || gen.ruleHeight(i) < gen.ruleHeight(least) {
				least = i
			}
		}
		choice = least
	}
	gen.covered[choice]++
	gen.used = append(gen.used, choice)
	for _, s := range gen.g.rules[choice].rhs {
		if s.nonterminal != "" {
			gen.expand(s.nonterminal, budget, words)
			continue
		}
		*words = append(*words, gen.terminal(s.terminals[gen.r.IntN(len(s.terminals))]))
	}
}

// terminal text for the terminal name.
func (gen *generator) terminal(name string) string {
	switch name {
	case "ID":
		return namePool[gen.r.IntN(len(namePool))]
	case "ANY":
		pool := append(slices.Clone(namePool), "1", "'s'", "+", ";", ",", "(", ")")
		return pool[gen.r.IntN(len(pool))]
	}
	if texts, ok := terminalTexts[name]; ok {
		return texts[gen.r.IntN(len(texts))]
	}
	// A keyword, in a random case.
	w := strings.ToLower(name)
	if gen.r.IntN(4) == 0 {
		w = strings.ToUpper(w)
	}
	return w
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
}

// FuzzGenerate compares the parser with SQLite on a sentence of the grammar and a mutant of it,
// from a seed that the fuzzer chooses.
func FuzzGenerate(f *testing.F) {
	g := readGrammar(f)
	f.Add(uint64(1), uint8(10), uint64(2))
	f.Fuzz(func(t *testing.T, seed uint64, budget uint8, mutant uint64) {
		gen := newGenerator(g, seed)
		s := gen.sentence("input", int(budget%100))
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
	gen := newGenerator(readGrammar(t), seed)
	var sqls []string
	for i := range n {
		s := gen.sentence("input", 4+gen.r.IntN(40))
		sqls = append(sqls, s)
		sqls = append(sqls, parsetest.Mutants(s, seed+uint64(i), mutantsEach)...)
	}
	return sqls
}
