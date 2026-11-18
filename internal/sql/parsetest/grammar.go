package parsetest

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"unicode"
)

// Rule of a Lemon grammar: a nonterminal and the symbols it expands to. A symbol is a nonterminal, a
// terminal, or a set of terminals: a token class such as ids, or terminals joined by |, such as
// COMMIT|END.
type Rule struct {
	LHS string
	RHS []Symbol
	// Line of the rule in the grammar file.
	Line int
}

// Symbol on the right side of a rule: a nonterminal, or one or more terminals.
type Symbol struct {
	Nonterminal string
	Terminals   []string
}

func (s Symbol) String() string {
	if s.Nonterminal != "" {
		return s.Nonterminal
	}
	return strings.Join(s.Terminals, "|")
}

// String of the rule in the form that Lemon writes in comments of the generated parser, with token
// classes written as their terminals.
func (r Rule) String() string {
	var b strings.Builder
	b.WriteString(r.LHS)
	b.WriteString(" ::=")
	for _, s := range r.RHS {
		b.WriteByte(' ')
		b.WriteString(s.String())
	}
	return b.String()
}

// Grammar of SQLite as read from its parse.y, with the %if sections resolved for the compile options
// of the binding in internal/sqlite, which defines none of the macros that parse.y tests.
type Grammar struct {
	Rules []Rule
	// ByLHS gives the indexes in Rules of the rules of each nonterminal.
	ByLHS map[string][]int
	// Fallback keywords of %fallback, which fall back to ID.
	Fallback []string
	// Classes of tokens: %token_class name A|B.
	Classes map[string][]string
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

// ParseGrammar reads the rules, token classes, and fallbacks of a Lemon grammar file, such as
// SQLite's parse.y.
func ParseGrammar(src string) (*Grammar, error) {
	g := &Grammar{ByLHS: map[string][]int{}, Classes: map[string][]string{}}
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
				g.Fallback = append(g.Fallback, args[1:]...)
			case "%token_class":
				for _, a := range args[1:] {
					if a != "|" {
						g.Classes[args[0]] = append(g.Classes[args[0]], a)
					}
				}
			}
		case it.block:
			// The code of the rule before it.
		default:
			// A rule: lhs [(alias)] ::= symbols . [[PREC]]
			r := Rule{LHS: it.text, Line: it.line}
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
					last := &r.RHS[len(r.RHS)-1]
					last.Terminals = append(last.Terminals, kept[i].text)
					continue
				}
				if unicode.IsUpper(rune(s[0])) {
					r.RHS = append(r.RHS, Symbol{Terminals: []string{s}})
				} else if class, ok := g.Classes[s]; ok {
					r.RHS = append(r.RHS, Symbol{Terminals: class})
				} else {
					r.RHS = append(r.RHS, Symbol{Nonterminal: s})
				}
			}
			if i+1 < len(kept) && kept[i+1].text == "[" {
				i += 3
			}
			g.ByLHS[r.LHS] = append(g.ByLHS[r.LHS], len(g.Rules))
			g.Rules = append(g.Rules, r)
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

// namePool for the names that the generator writes for ID: plain names, names in every quote style,
// keywords that fall back to ID, and keywords that do not, which the grammar rejects there.
var namePool = []string{"t1", "a", "b", "x", "rowid", "main", "temp", "_x", "x1$", "été",
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

// Generator of random sentences of a grammar.
type Generator struct {
	g *Grammar
	r *rand.Rand
	// height of each nonterminal: the least depth of a derivation of it into terminals.
	height map[string]int
	// covered rules, by index, of the sentences generated.
	covered []int
	// used rules, by index, in the derivation of the last sentence.
	used []int
}

// NewGenerator of sentences of g, from seed.
func NewGenerator(g *Grammar, seed uint64) *Generator {
	gen := &Generator{g: g, r: rand.New(rand.NewPCG(seed, 0x5eed)), height: map[string]int{}, covered: make([]int, len(g.Rules))}
	// Iterate to a fixed point: the height of a rule is one more than the greatest height of the
	// nonterminals on its right.
	for changed := true; changed; {
		changed = false
		for _, r := range g.Rules {
			h, ok := 1, true
			for _, s := range r.RHS {
				if s.Nonterminal != "" {
					sh, known := gen.height[s.Nonterminal]
					if !known {
						ok = false
						break
					}
					h = max(h, sh+1)
				}
			}
			if old, known := gen.height[r.LHS]; ok && (!known || h < old) {
				gen.height[r.LHS] = h
				changed = true
			}
		}
	}
	return gen
}

// IntN gives a random number in [0, n) from the generator's source.
func (gen *Generator) IntN(n int) int {
	return gen.r.IntN(n)
}

// Used rules, by their index in the grammar, in the derivation of the last sentence.
func (gen *Generator) Used() []int {
	return gen.used
}

// ruleHeight of the rule with index i.
func (gen *Generator) ruleHeight(i int) int {
	h := 1
	for _, s := range gen.g.Rules[i].RHS {
		if s.Nonterminal != "" {
			h = max(h, gen.height[s.Nonterminal]+1)
		}
	}
	return h
}

// Sentence derived from the nonterminal start, with at most about budget nonterminals expanded
// freely; past that, the rules of least height are taken, so that every sentence is finite. Half the
// time a nonterminal takes the rule that has been used least, so that every rule gets its turn.
// Names come from a pool of plain names, every quote style, every fallback keyword, the join
// keywords, and some keywords that are not names.
func (gen *Generator) Sentence(start string, budget int) string {
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

func (gen *Generator) expand(nt string, budget *int, words *[]string) {
	rules := gen.g.ByLHS[nt]
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
	for _, s := range gen.g.Rules[choice].RHS {
		if s.Nonterminal != "" {
			gen.expand(s.Nonterminal, budget, words)
			continue
		}
		*words = append(*words, gen.terminal(s.Terminals[gen.r.IntN(len(s.Terminals))]))
	}
}

// terminal text for the terminal name.
func (gen *Generator) terminal(name string) string {
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
