// Package stmttest is a generator of random statements of slice 1, for tests: each statement is a
// text with the typed form of [maragu.dev/s4ql/internal/sql/stmt] that it holds, with names from
// pools, keywords in any case, and spaces and comments in every gap.
//
// The generator draws its random numbers from a [Rand] that the test gives it, so that a test
// chooses its seed and its generator of numbers.
package stmttest

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sqlite"
)

// Rand is the source of the generator's random numbers, such as a *rand.Rand of math/rand/v2.
type Rand interface {
	// IntN is a number in [0, n).
	IntN(n int) int
	// Uint64 is any number.
	Uint64() uint64
}

// Position of a name in the statements of slice 1.
type Position int

const (
	CreateTable Position = iota
	CreateColumn
	InsertTable
	InsertColumn
	SelectTable
	DropTable
)

// PositionTexts give, for each position, a statement of slice 1 with %v in that position.
var PositionTexts = []string{
	CreateTable:  "create table %v (v int primary key) strict, without rowid",
	CreateColumn: "create table t (%v int primary key) strict, without rowid",
	InsertTable:  "insert into %v (v) values (1)",
	InsertColumn: "insert into t (%v) values (1)",
	SelectTable:  "select * from %v",
	DropTable:    "drop table %v",
}

// BasePool of names: plain names, names in double quotes with odd content, names that differ only in
// case, and names close to the reserved ones. SQLite reads each as a name in every position, and the
// pass supports each.
var BasePool = []string{
	"t1", "T1", "a", "A", "_x", "x1$", "été", "ÉTÉ", "Ω", "😀", "x_", "a1b2", "rowid", "oid", "main", "temp",
	"sqlite", "sqlitex", "s4ql", "s4qlx", "pragma_x", "json", "json_eachx", "xsqlite_", "pragma_table_listx",
	`"t1"`, `"T1"`, `"a b"`, `"a""b"`, `""""`, `"select"`, `"from"`, `"x.y"`, `"1"`, `"  "`,
	`"ÆØÅ"`, `"æøå"`, `"t1 "`, `"-- not a comment"`, `"/* x */"`, `"'"`, `"[x]"`, "\"`x`\"", `"a\tb"`,
	`"a;b"`, `"sqlite"`, `"s4ql"`, `"Ｔ１"`, `"` + strings.Repeat("x", 200) + `"`, strings.Repeat("y", 100),
}

// Keywords of SQLite, in lower case.
func Keywords() []string {
	var kws []string
	for _, kw := range sqlite.Keywords() {
		kws = append(kws, strings.ToLower(kw))
	}
	return kws
}

// NamePools gives, for each position, the names of [BasePool] and the keywords of SQLite that SQLite
// reads as a name there. It fails if SQLite does not read a name of BasePool as a name somewhere.
var NamePools = sync.OnceValues(func() ([][]string, error) {
	pools := make([][]string, len(PositionTexts))
	for p, text := range PositionTexts {
		pools[p] = BasePool
		for _, n := range BasePool {
			if v := oracle.Judge(fmt.Sprintf(text, n)); v.Kind != oracle.Parses {
				return nil, fmt.Errorf("SQLite does not read %v as a name in %q: %+v", n, text, v)
			}
		}
		for _, kw := range Keywords() {
			if oracle.Judge(fmt.Sprintf(text, kw)).Kind == oracle.Parses {
				pools[p] = append(slices.Clip(pools[p]), kw)
			}
		}
	}
	return pools, nil
})

// Quote the value of a name as a name in double quotes.
func Quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// Kind of a statement of slice 1.
type Kind int

const (
	KindCreateTable Kind = iota
	KindInsert
	KindSelect
	KindDropTable
)

// Generator of random statements of slice 1.
type Generator struct {
	r Rand
	// pools of names, for each [Position].
	pools [][]string
	// Value gives the value of an insert, or nil for a value at random, at and around the limits.
	Value func() int64
	// Parameters, if not 0, makes one insert in Parameters have a plain ? in place of its literal,
	// with the value as its argument.
	Parameters int
	b          strings.Builder
}

// NewGenerator with the random numbers of r and a pool of names for each [Position]. Each name of a
// pool must be one that SQLite reads as a name in its position, and that the pass supports.
func NewGenerator(r Rand, pools [][]string) *Generator {
	return &Generator{r: r, pools: pools}
}

// gaps between tokens. None starts with "-", so that a gap after a minus sign does not make it a
// comment.
var gaps = []string{" ", "  ", "\n", "\t", "\r\n", "\f", "/* c */", "/**/", "/* a\nb */", " /*-*/ ", " -- c\n", "\n--\n"}

// gap writes the text between two tokens: required if the tokens would run together without it.
func (g *Generator) gap(required bool) {
	if !required && g.r.IntN(3) == 0 {
		return
	}
	for {
		g.b.WriteString(gaps[g.r.IntN(len(gaps))])
		if g.r.IntN(4) != 0 {
			return
		}
	}
}

// keyword written in a random case.
func (g *Generator) keyword(w string) {
	switch g.r.IntN(3) {
	case 0:
		g.b.WriteString(w)
	case 1:
		g.b.WriteString(strings.ToUpper(w))
	default:
		for _, c := range w {
			if g.r.IntN(2) == 0 {
				c = []rune(strings.ToUpper(string(c)))[0]
			}
			g.b.WriteRune(c)
		}
	}
}

// punct writes a token that is not a word.
func (g *Generator) punct(p string) {
	g.b.WriteString(p)
}

// name written for the position p, with its value. A bare name is written in upper case one time in
// three, which is another name for a name with letters outside ASCII.
func (g *Generator) name(p Position) stmt.Name {
	pool := g.pools[p]
	raw := pool[g.r.IntN(len(pool))]
	if raw[0] != '"' && g.r.IntN(3) == 0 {
		raw = strings.ToUpper(raw)
	}
	value := raw
	if raw[0] == '"' {
		value = strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	n := stmt.Name{Raw: raw, Value: value, Offset: g.b.Len()}
	g.b.WriteString(raw)
	return n
}

// integer for a value: from [Generator.Value], or at random.
func (g *Generator) integer() int64 {
	if g.Value != nil {
		return g.Value()
	}
	return Integer(g.r)
}

// value written as an integer of slice 1, with its span.
func (g *Generator) value() (int64, stmt.Span) {
	v := g.integer()
	start := g.b.Len()
	digits := fmt.Sprint(v)
	if v < 0 || (v == 0 && g.r.IntN(4) == 0) {
		g.b.WriteString("-")
		g.gap(false)
		digits = strings.TrimPrefix(digits, "-")
	}
	if g.r.IntN(4) == 0 {
		digits = strings.Repeat("0", 1+g.r.IntN(30)) + digits
	}
	g.b.WriteString(digits)
	return v, stmt.Span{Start: start, End: g.b.Len()}
}

// Integer at random from r: at and around the limits of 64 bits, zero, powers of ten, any, and
// small ones.
func Integer(r Rand) int64 {
	switch r.IntN(10) {
	case 0:
		return math.MaxInt64
	case 1:
		return math.MinInt64
	case 2:
		return math.MaxInt64 - int64(r.IntN(3))
	case 3:
		return math.MinInt64 + int64(r.IntN(3))
	case 4:
		return 0
	case 5:
		return int64(r.Uint64())
	case 6:
		return int64(math.Pow10(r.IntN(19))) - int64(r.IntN(2))
	default:
		return int64(r.IntN(2001)) - 1000
	}
}

// Statement of slice 1 at random: its text and the statement that it holds. An insert with a
// parameter, which [Generator.Parameters] asks for, is written without its argument.
func (g *Generator) Statement() (string, stmt.Statement) {
	src, s, _ := g.statement(-1)
	return src, s
}

// StatementOf the kind: its text and the statement that it holds, as [Generator.Statement] gives it.
func (g *Generator) StatementOf(k Kind) (string, stmt.Statement) {
	src, s, _ := g.statement(k)
	return src, s
}

// CallOf the kind: its text, the statement that it holds, and an int64 argument for each of its
// parameters.
func (g *Generator) CallOf(k Kind) (string, stmt.Statement, []any) {
	return g.statement(k)
}

// statement of the kind k, or of a kind at random if k is negative, with an argument for each of its
// parameters.
func (g *Generator) statement(k Kind) (string, stmt.Statement, []any) {
	g.b.Reset()
	switch g.r.IntN(4) {
	case 1:
		g.b.WriteString(";")
	case 2:
		g.gap(true)
		g.b.WriteString("; ;")
		g.gap(false)
	}
	g.gap(false)
	start := g.b.Len()
	if k < 0 {
		k = Kind(g.r.IntN(4))
	}
	var s stmt.Statement
	var span *stmt.Span
	var args []any
	switch k {
	case KindCreateTable:
		c := &stmt.CreateTable{}
		g.keyword("create")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.Table = g.name(CreateTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		c.Column = g.name(CreateColumn)
		g.gap(true)
		g.keyword("int")
		g.gap(true)
		g.keyword("primary")
		g.gap(true)
		g.keyword("key")
		g.gap(false)
		g.punct(")")
		options := []func(){
			func() { g.keyword("strict") },
			func() { g.keyword("without"); g.gap(true); g.keyword("rowid") },
		}
		if g.r.IntN(2) == 0 {
			options[0], options[1] = options[1], options[0]
		}
		g.gap(false)
		options[0]()
		g.gap(false)
		g.punct(",")
		g.gap(false)
		options[1]()
		s, span = c, &c.Span
	case KindInsert:
		c := &stmt.Insert{}
		g.keyword("insert")
		g.gap(true)
		g.keyword("into")
		g.gap(true)
		c.Table = g.name(InsertTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		c.Column = g.name(InsertColumn)
		g.gap(false)
		g.punct(")")
		g.gap(false)
		g.keyword("values")
		g.gap(false)
		g.punct("(")
		g.gap(false)
		if g.Parameters > 0 && g.r.IntN(g.Parameters) == 0 {
			c.Parameter, c.ValueSpan = 1, stmt.Span{Start: g.b.Len(), End: g.b.Len() + 1}
			g.punct("?")
			args = []any{g.integer()}
		} else {
			c.Value, c.ValueSpan = g.value()
		}
		g.gap(false)
		g.punct(")")
		s, span = c, &c.Span
	case KindSelect:
		c := &stmt.Select{}
		g.keyword("select")
		g.gap(false)
		g.punct("*")
		g.gap(false)
		g.keyword("from")
		g.gap(true)
		c.Table = g.name(SelectTable)
		s, span = c, &c.Span
	default:
		c := &stmt.DropTable{}
		g.keyword("drop")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.Table = g.name(DropTable)
		s, span = c, &c.Span
	}
	*span = stmt.Span{Start: start, End: g.b.Len()}
	if g.r.IntN(2) == 0 {
		g.gap(false)
		g.punct(";")
		span.End = g.b.Len()
		switch g.r.IntN(3) {
		case 0:
			g.gap(false)
			g.b.WriteString(";")
		case 1:
			g.b.WriteString(" -- end")
		}
	} else if g.r.IntN(2) == 0 {
		g.gap(true)
	}
	return g.b.String(), s, args
}
