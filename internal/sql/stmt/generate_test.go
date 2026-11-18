package stmt_test

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"testing"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sqlite"
)

// position of a name in the statements of slice 1.
type position int

const (
	createTable position = iota
	createColumn
	insertTable
	insertColumn
	selectTable
	dropTable
)

// positionTexts give, for each position, a statement of slice 1 with %v in that position.
var positionTexts = []string{
	createTable:  "create table %v (v int primary key) strict, without rowid",
	createColumn: "create table t (%v int primary key) strict, without rowid",
	insertTable:  "insert into %v (v) values (1)",
	insertColumn: "insert into t (%v) values (1)",
	selectTable:  "select * from %v",
	dropTable:    "drop table %v",
}

// basePool of names for the generator: plain names, names in double quotes with odd content, names
// that differ only in case, and names close to the reserved ones.
var basePool = []string{
	"t1", "T1", "a", "A", "_x", "x1$", "été", "ÉTÉ", "Ω", "😀", "x_", "a1b2", "rowid", "oid", "main", "temp",
	"sqlite", "sqlitex", "s4ql", "s4qlx", "pragma_x", "json", "json_eachx", "xsqlite_", "pragma_table_listx",
	`"t1"`, `"T1"`, `"a b"`, `"a""b"`, `""""`, `""`, `"select"`, `"from"`, `"x.y"`, `"1"`, `"  "`,
	`"ÆØÅ"`, `"æøå"`, `"t1 "`, `"-- not a comment"`, `"/* x */"`, `"'"`, `"[x]"`, "\"`x`\"", `"a\tb"`,
	`"a;b"`, `"sqlite"`, `"s4ql"`, `"Ｔ１"`, `"` + strings.Repeat("x", 200) + `"`, strings.Repeat("y", 100),
}

// keywords of SQLite, in lower case.
func keywords(t testing.TB) []string {
	t.Helper()
	var kws []string
	for _, kw := range sqlite.Keywords() {
		kws = append(kws, strings.ToLower(kw))
	}
	return kws
}

var (
	poolsOnce sync.Once
	pools     [][]string
)

// namePools for each position: the names of [basePool] and the keywords of SQLite that SQLite reads
// as a name there.
func namePools(t testing.TB) [][]string {
	t.Helper()
	poolsOnce.Do(func() {
		pools = make([][]string, len(positionTexts))
		for p, text := range positionTexts {
			for _, n := range append(append([]string{}, basePool...), keywords(t)...) {
				if oracle.Judge(fmt.Sprintf(text, n)).Kind == oracle.Parses {
					pools[p] = append(pools[p], n)
				}
			}
		}
	})
	return pools
}

// generator of random statements of slice 1, with names from [namePools], keywords in any case, and
// spaces and comments in every gap.
type generator struct {
	r     *rand.Rand
	pools [][]string
	b     strings.Builder
}

func newGenerator(t testing.TB, seed uint64) *generator {
	return &generator{r: rand.New(rand.NewPCG(seed, 0x511ce1)), pools: namePools(t)}
}

// gaps between tokens. None starts with "-", so that a gap after a minus sign does not make it a
// comment.
var gaps = []string{" ", "  ", "\n", "\t", "\r\n", "\f", "/* c */", "/**/", "/* a\nb */", " /*-*/ ", " -- c\n", "\n--\n"}

// gap writes the text between two tokens: required if the tokens would run together without it.
func (g *generator) gap(required bool) {
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

// keyword written in a random case, and its offset.
func (g *generator) keyword(w string) int {
	at := g.b.Len()
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
	return at
}

// punct writes a token that is not a word, and gives its offset.
func (g *generator) punct(p string) int {
	at := g.b.Len()
	g.b.WriteString(p)
	return at
}

// name written for the position p, with its value.
func (g *generator) name(p position) stmt.Name {
	pool := g.pools[p]
	raw := pool[g.r.IntN(len(pool))]
	if raw[0] != '"' && g.r.IntN(3) == 0 {
		raw = strings.ToUpper(raw)
		if oracle.Judge(fmt.Sprintf(positionTexts[p], raw)).Kind != oracle.Parses {
			raw = strings.ToLower(raw)
		}
	}
	value := raw
	if raw[0] == '"' {
		value = strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	n := stmt.Name{Raw: raw, Value: value, Offset: g.b.Len()}
	g.b.WriteString(raw)
	return n
}

// value written as an integer of slice 1, with its span.
func (g *generator) value() (int64, stmt.Span) {
	var v int64
	switch g.r.IntN(10) {
	case 0:
		v = math.MaxInt64
	case 1:
		v = math.MinInt64
	case 2:
		v = math.MaxInt64 - int64(g.r.IntN(3))
	case 3:
		v = math.MinInt64 + int64(g.r.IntN(3))
	case 4:
		v = 0
	case 5:
		v = int64(g.r.Uint64())
	case 6:
		v = int64(math.Pow10(g.r.IntN(19))) - int64(g.r.IntN(2))
	default:
		v = int64(g.r.IntN(2001)) - 1000
	}
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

// statement of slice 1 at random: its text and the statement that it holds.
func (g *generator) statement() (string, stmt.Statement) {
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
	var s stmt.Statement
	var span *stmt.Span
	switch g.r.IntN(4) {
	case 0:
		c := &stmt.CreateTable{}
		g.keyword("create")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.Table = g.name(createTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		c.Column = g.name(createColumn)
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
	case 1:
		c := &stmt.Insert{}
		g.keyword("insert")
		g.gap(true)
		g.keyword("into")
		g.gap(true)
		c.Table = g.name(insertTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		c.Column = g.name(insertColumn)
		g.gap(false)
		g.punct(")")
		g.gap(false)
		g.keyword("values")
		g.gap(false)
		g.punct("(")
		g.gap(false)
		c.Value, c.ValueSpan = g.value()
		g.gap(false)
		g.punct(")")
		s, span = c, &c.Span
	case 2:
		c := &stmt.Select{}
		g.keyword("select")
		g.gap(false)
		g.punct("*")
		g.gap(false)
		g.keyword("from")
		g.gap(true)
		c.Table = g.name(selectTable)
		s, span = c, &c.Span
	default:
		c := &stmt.DropTable{}
		g.keyword("drop")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.Table = g.name(dropTable)
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
	return g.b.String(), s
}

var generatedCount = flag.Int("generated", 20000, "statements of the generator of slice 1")

// TestGenerate checks that every statement of the generator of slice 1 is supported, as the statement
// that the generator wrote, and does that in SQLite.
func TestGenerate(t *testing.T) {
	n := *generatedCount
	if slow() {
		n /= 10
	}
	g := newGenerator(t, *seed)
	for range n {
		src, want := g.statement()
		s, err := stmt.Check(src)
		if err != nil || fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
			t.Fatalf("%q: gives %#v %v, want %#v", src, s, err, want)
		}
		verify(t, src)
	}
	for p, pool := range namePools(t) {
		t.Logf("%v names in position %q", len(pool), positionTexts[p])
	}
}

var mutantsEach = flag.Int("mutants", 5, "mutants of each statement in TestGenerate_mutants")

// TestGenerate_mutants checks the pass against SQLite on one-token mutants of statements of the
// generator of slice 1: each is a syntax error as in SQLite, not supported, or supported and does in
// SQLite what it says. It logs the mutants that are still supported, by how they differ from the
// statement they come from.
func TestGenerate_mutants(t *testing.T) {
	n := *generatedCount
	if slow() {
		n /= 10
	}
	g := newGenerator(t, *seed+1)
	c := counts{}
	kinds := map[string][]string{}
	for i := range n {
		src, want := g.statement()
		for _, m := range parsetest.Mutants(src, *seed+uint64(i), *mutantsEach) {
			o, s := verify(t, m)
			c[o]++
			if o == supported {
				kind := difference(want, s)
				kinds[kind] = append(kinds[kind], fmt.Sprintf("%q from %q", m, src))
			}
		}
	}
	t.Logf("%v mutants: %v", n**mutantsEach, c)
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t.Logf("supported, %v: %v, for example:", k, len(kinds[k]))
		for _, e := range kinds[k][:min(3, len(kinds[k]))] {
			t.Logf("  %v", e)
		}
	}
}

// difference between the statement a, which a text of the generator holds, and the statement b,
// which a mutant of it holds.
func difference(a, b stmt.Statement) string {
	if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
		return fmt.Sprintf("another kind of statement, %T", b)
	}
	var diffs []string
	switch a := a.(type) {
	case *stmt.CreateTable:
		b := b.(*stmt.CreateTable)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if a.Column.Value != b.Column.Value {
			diffs = append(diffs, "column name")
		}
	case *stmt.Insert:
		b := b.(*stmt.Insert)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if a.Column.Value != b.Column.Value {
			diffs = append(diffs, "column name")
		}
		if a.Value != b.Value {
			diffs = append(diffs, "value")
		}
	case *stmt.Select:
		if a.Table.Value != b.(*stmt.Select).Table.Value {
			diffs = append(diffs, "table name")
		}
	case *stmt.DropTable:
		if a.Table.Value != b.(*stmt.DropTable).Table.Value {
			diffs = append(diffs, "table name")
		}
	}
	if diffs == nil {
		return fmt.Sprintf("the same %T", a)
	}
	return fmt.Sprintf("%T with another %v", a, strings.Join(diffs, " and "))
}

// FuzzGenerate checks the pass against SQLite on a statement of the generator of slice 1 and a
// mutant of it, from seeds that the fuzzer chooses.
func FuzzGenerate(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Fuzz(func(t *testing.T, seed, mutant uint64) {
		src, want := newGenerator(t, seed).statement()
		s, err := stmt.Check(src)
		if err != nil || fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
			t.Fatalf("%q: gives %#v %v, want %#v", src, s, err, want)
		}
		verify(t, src)
		for _, m := range parsetest.Mutants(src, mutant, 1) {
			verify(t, m)
		}
	})
}
