package stmt_test

import (
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
)

// keywords of SQLite, in lower case.
func keywords(testing.TB) []string {
	return stmttest.Keywords()
}

// namePools from [stmttest.NamePools].
func namePools(t testing.TB) [][]string {
	t.Helper()
	pools, err := stmttest.NamePools()
	if err != nil {
		t.Fatal(err)
	}
	return pools
}

// positionTexts are those of [stmttest.PositionTexts].
var positionTexts = stmttest.PositionTexts

// generator of random statements of slices 1, 2, and 4: those of [stmttest], which writes the column
// type INT and integer literals only, with the type of the column and the literal of an insert
// replaced at random.
type generator struct {
	*stmttest.Generator
	// r gives the random numbers of the replacements, apart from those of the statements.
	r *rand.Rand
	// types, if not 0, makes one CREATE TABLE in types have another type than INT, and one insert of a
	// literal in types have a real, a string, or a blob.
	types int
}

// newGenerator from seed, which writes a plain ? in place of the value of one insert in four, and
// another type or literal in one statement of the others in two.
func newGenerator(t testing.TB, seed uint64) generator {
	g := stmttest.NewGenerator(rand.New(rand.NewPCG(seed, 0x511ce1)), namePools(t))
	g.Parameters = 4
	return generator{Generator: g, r: rand.New(rand.NewPCG(seed, 0x7e5)), types: 2}
}

// statement at random: its text and the statement that it holds.
func (g generator) statement() (string, stmt.Statement) {
	src, s := g.Statement()
	if g.types == 0 || g.r.IntN(g.types) != 0 {
		return src, s
	}
	switch s := s.(type) {
	case *stmt.CreateTable:
		// The type is the first token after the name of the column.
		at := s.Column.Offset + len(s.Column.Raw)
		for _, tk := range token.Tokens(src[at:]) {
			if tk.Kind == token.Space || tk.Kind == token.Comment {
				continue
			}
			types := []stmt.Type{stmt.TypeInteger, stmt.TypeReal, stmt.TypeText, stmt.TypeBlob}
			s.Type = types[g.r.IntN(len(types))]
			word := g.inCase(strings.ToLower(s.Type.String()))
			start, end := at+tk.Offset, at+tk.Offset+tk.Len
			src = src[:start] + word + src[end:]
			s.Span.End += len(word) - (end - start)
			break
		}
	case *stmt.Insert:
		if s.Parameter > 0 {
			break
		}
		text, v := g.literal()
		src = src[:s.ValueSpan.Start] + text + src[s.ValueSpan.End:]
		s.Span.End += len(text) - (s.ValueSpan.End - s.ValueSpan.Start)
		s.Value, s.ValueSpan.End = v, s.ValueSpan.Start+len(text)
	}
	return src, s
}

// inCase gives the word w in lower case, upper case, or a mix, at random.
func (g generator) inCase(w string) string {
	switch g.r.IntN(3) {
	case 0:
		return w
	case 1:
		return strings.ToUpper(w)
	}
	b := []byte(w)
	for i := range b {
		if g.r.IntN(2) == 0 {
			b[i] = strings.ToUpper(string(b[i]))[0]
		}
	}
	return string(b)
}

// literal at random of a real, a string, or a blob: its text and its value.
func (g generator) literal() (string, stmt.Value) {
	switch g.r.IntN(5) {
	case 0, 1:
		return realLiteral(g.r)
	case 2, 3:
		return stringLiteral(g.r)
	}
	return blobLiteral(g.r)
}

// realLiteral at random: the text of a float64, which may have a sign, in one of the forms of a real
// literal of SQLite, and the float64. The text has the digits of the float64 that strconv gives, which
// round to it, so its value is the float64 if SQLite rounds those digits to the nearest float, which
// the tests check.
func realLiteral(r *rand.Rand) (string, stmt.Value) {
	f := realValue(r)
	digits := strconv.FormatFloat(math.Abs(f), 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(digits, "e")
	var text string
	switch r.IntN(6) {
	case 0:
		// As strconv writes it, such as 1.5e+00, or 5e-324.
		text = digits
	case 1:
		// Without a fraction or a sign of the exponent, in upper case, such as 15E-1.
		whole := strings.Replace(mantissa, ".", "", 1)
		e, _ := strconv.Atoi(exp)
		e -= len(whole) - 1
		text = whole + "E" + strconv.Itoa(e)
	case 2:
		// Without an exponent, such as 0.0001 or 1e+300 written out.
		text = strconv.FormatFloat(math.Abs(f), 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += "."
		}
	case 3:
		// Without an exponent and without a zero before the point, such as .5, if it is below 1.
		text = strconv.FormatFloat(math.Abs(f), 'f', -1, 64)
		text = strings.TrimPrefix(text, "0")
		if !strings.Contains(text, ".") {
			text += ".0"
		}
	case 4:
		// With leading and trailing zeros, which do not change the value.
		text = strings.Repeat("0", r.IntN(5)) + strconv.FormatFloat(math.Abs(f), 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += "."
		}
		text += strings.Repeat("0", r.IntN(5))
	default:
		// In upper case, with the exponent's sign, such as 1.5E+03.
		text = mantissa + "E" + exp
	}
	if math.Signbit(f) {
		text = "-" + []string{"", " ", "/* c */", "\n"}[r.IntN(4)] + text
	}
	return text, stmt.Real(f)
}

// realValue at random, finite: small numbers, numbers with few digits, the limits of float64 and the
// subnormal ones, zero and negative zero, and any.
func realValue(r *rand.Rand) float64 {
	sign := 1.0
	if r.IntN(3) == 0 {
		sign = -1
	}
	switch r.IntN(9) {
	case 0:
		return sign * 0
	case 1:
		return sign * float64(r.IntN(2001)) / 8
	case 2:
		return sign * float64(r.IntN(100000)) / math.Pow10(r.IntN(10))
	case 3:
		return sign * []float64{math.MaxFloat64, math.SmallestNonzeroFloat64, 0x1p-1022, 0x1p-1022 - 0x1p-1074,
			math.Nextafter(math.MaxFloat64, 0), 0.1, 0.2, 0.3, 1e23, 9007199254740993, 2.2250738585072011e-308}[r.IntN(11)]
	case 4:
		// A subnormal one.
		return sign * float64(1+r.IntN(1<<20)) * math.SmallestNonzeroFloat64
	case 5:
		return sign * math.Pow10(r.IntN(617)-308)
	default:
		for {
			f := math.Float64frombits(r.Uint64())
			if !math.IsInf(f, 0) && !math.IsNaN(f) {
				return f
			}
		}
	}
}

// stringPool holds the content of strings: empty, with quotes, with what looks like SQL, and with
// letters of more than one byte.
var stringPool = []string{"", "a", "'", "''", "a'b", "it's", `"`, "--", "/*", "*/", " ", "\t\n", "x'01'", "1",
	"1.5", "null", "été", "ÆØÅ", "😀", "a b c", "\\", "?", ";", "select 1;", strings.Repeat("x", 300)}

// stringLiteral at random: its text, and the string as its value.
func stringLiteral(r *rand.Rand) (string, stmt.Value) {
	var v string
	if r.IntN(2) == 0 {
		v = stringPool[r.IntN(len(stringPool))]
	} else {
		var b strings.Builder
		for range r.IntN(12) {
			// Any rune but NUL and the surrogates, which are not valid UTF-8, mostly from ASCII.
			var c rune
			if r.IntN(4) == 0 {
				c = rune(1 + r.IntN(0x10ffff))
			} else {
				c = rune(1 + r.IntN(0x7f))
			}
			if 0xd800 <= c && c <= 0xdfff {
				c = 'x'
			}
			b.WriteRune(c)
		}
		v = b.String()
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'", stmt.Text(v)
}

// blobLiteral at random: its text, with an x in either case and hex digits in either case, and the
// bytes as its value.
func blobLiteral(r *rand.Rand) (string, stmt.Value) {
	b := make([]byte, r.IntN(20))
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	digits := fmt.Sprintf("%x", b)
	if r.IntN(2) == 0 {
		digits = strings.ToUpper(digits)
	}
	return []string{"x", "X"}[r.IntN(2)] + "'" + digits + "'", stmt.Blob(b)
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
	excused(t, c, 1, 367)
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
		if a.Type != b.Type {
			diffs = append(diffs, "type")
		}
	case *stmt.Insert:
		b := b.(*stmt.Insert)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if a.Column.Value != b.Column.Value {
			diffs = append(diffs, "column name")
		}
		switch {
		case a.Parameter != b.Parameter:
			diffs = append(diffs, "parameter")
		case fmt.Sprintf("%T", a.Value) != fmt.Sprintf("%T", b.Value):
			diffs = append(diffs, "kind of value")
		case show(a.Value) != show(b.Value):
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

// nearMisses of the four forms: each is the form with one clause, keyword, or item added or changed,
// which a one-token mutant cannot make, or a literal that is not supported or is a syntax error. %[1]v
// is the table, %[2]v the column, %[3]v the text of the value, and %[4]v the type of the column.
var nearMisses = map[string][]string{
	"*stmt.CreateTable": {
		"explain create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"explain query plan create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temp table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temporary table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not exists %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table main.%[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table temp.%[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v unsigned primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v(10) primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v %[4]v primary key) strict, without rowid",
		`create table %[1]v (%[2]v "%[4]v" primary key) strict, without rowid`,
		"create table %[1]v (%[2]v '%[4]v' primary key) strict, without rowid",
		"create table %[1]v (%[2]v [%[4]v] primary key) strict, without rowid",
		"create table %[1]v (%[2]v any primary key) strict, without rowid",
		"create table %[1]v (%[2]v varchar primary key) strict, without rowid",
		"create table %[1]v (%[2]v double primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]vs primary key) strict, without rowid",
		"create table %[1]v (%[2]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key unique) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key collate binary) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key references u (x)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key references u deferrable initially deferred) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key asc) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key desc) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key on conflict fail) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key autoincrement) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key generated always as (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key constraint c) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v constraint c primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v not null primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int) strict, without rowid",
		"create table %[1]v (w int, %[2]v %[4]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, unique (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v, primary key (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key) strict",
		"create table %[1]v (%[2]v %[4]v primary key) without rowid",
		"create table %[1]v (%[2]v %[4]v primary key)",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid, strict",
		"create table %[1]v (%[2]v %[4]v primary key) strict, strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key), strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid, foo",
		"create table %[1]v as select 1",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid; select 1",
	},
	"*stmt.Insert": {
		"explain insert into %[1]v (%[2]v) values (%[3]v)",
		"with x as (select 1) insert into %[1]v (%[2]v) values (%[3]v)",
		"replace into %[1]v (%[2]v) values (%[3]v)",
		"insert or replace into %[1]v (%[2]v) values (%[3]v)",
		"insert or ignore into %[1]v (%[2]v) values (%[3]v)",
		"insert or abort into %[1]v (%[2]v) values (%[3]v)",
		"insert into main.%[1]v (%[2]v) values (%[3]v)",
		"insert into %[1]v as x (%[2]v) values (%[3]v)",
		"insert into %[1]v values (%[3]v)",
		"insert into %[1]v (%[2]v, w) values (%[3]v, 1)",
		"insert into %[1]v (%[2]v, %[2]v) values (%[3]v, %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v, 1)",
		"insert into %[1]v (%[2]v) values ((%[3]v))",
		"insert into %[1]v (%[2]v) values (+%[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v + 0)",
		"insert into %[1]v (%[2]v) values (- (%[3]v))",
		"insert into %[1]v (%[2]v) values (%[3]v collate binary)",
		"insert into %[1]v (%[2]v) values (cast(%[3]v as int))",
		"insert into %[1]v (%[2]v) values (?1)",
		"insert into %[1]v (%[2]v) values (:a)",
		"insert into %[1]v (%[2]v) values (-?)",
		"insert into %[1]v (%[2]v) values (?, ?)",
		"insert into %[1]v (%[2]v) select %[3]v",
		"insert into %[1]v (%[2]v) values (%[3]v) union select 1",
		"insert into %[1]v (%[2]v) values (%[3]v) returning *",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict do nothing",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict (%[2]v) do update set %[2]v = 1",
		"insert into %[1]v default values",
		"insert into %[1]v (%[2]v) values (%[3]v); select 1",
		"insert into %[1]v (%[2]v) values (%[3]v || %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]vx)",
		"insert into %[1]v (%[2]v) values (%[3]v')",
		"insert into %[1]v (%[2]v) values ('a)",
		"insert into %[1]v (%[2]v) values ('a'')",
		"insert into %[1]v (%[2]v) values ('a' 'b')",
		"insert into %[1]v (%[2]v) values (- 'a')",
		"insert into %[1]v (%[2]v) values (-x'01')",
		"insert into %[1]v (%[2]v) values (x'6')",
		"insert into %[1]v (%[2]v) values (x'6g')",
		"insert into %[1]v (%[2]v) values (x '61')",
		"insert into %[1]v (%[2]v) values (1e)",
		"insert into %[1]v (%[2]v) values (1e+)",
		"insert into %[1]v (%[2]v) values (1.5.5)",
		"insert into %[1]v (%[2]v) values (.)",
		"insert into %[1]v (%[2]v) values (1e999)",
		"insert into %[1]v (%[2]v) values (-1e999)",
		"insert into %[1]v (%[2]v) values (1_000.5)",
		"insert into %[1]v (%[2]v) values (0x1p3)",
		"insert into %[1]v (%[2]v) values (null)",
		"insert into %[1]v (%[2]v) values (true)",
		"insert into %[1]v (%[2]v) values (- false)",
		`insert into %[1]v (%[2]v) values ("a")`,
		"insert into %[1]v (%[2]v) values (cast('a' as text))",
	},
	"*stmt.Select": {
		"explain select * from %[1]v",
		"with x as (select 1) select * from %[1]v",
		"select distinct * from %[1]v",
		"select all * from %[1]v",
		"select *, * from %[1]v",
		"select %[1]v.* from %[1]v",
		"select 1 from %[1]v",
		"select * from main.%[1]v",
		"select * from %[1]v as x",
		"select * from %[1]v x",
		"select * from %[1]v indexed by i",
		"select * from %[1]v not indexed",
		"select * from (%[1]v)",
		"select * from (select * from %[1]v)",
		"select * from %[1]v, u",
		"select * from %[1]v join u",
		"select * from %[1]v natural join u",
		"select * from %[1]v left join u on 1",
		"select * from %[1]v where 1",
		"select * from %[1]v group by 1",
		"select * from %[1]v group by 1 having 1",
		"select * from %[1]v window w as ()",
		"select * from %[1]v order by 1",
		"select * from %[1]v limit 1",
		"select * from %[1]v limit 1 offset 1",
		"select * from %[1]v union select * from %[1]v",
		"select * from %[1]v union all select * from %[1]v",
		"select * from %[1]v except select * from %[1]v",
		"select * from %[1]v; select 1",
	},
	"*stmt.DropTable": {
		"explain drop table %[1]v",
		"drop table if exists %[1]v",
		"drop table main.%[1]v",
		"drop table temp.%[1]v",
		"drop view %[1]v",
		"drop index %[1]v",
		"drop trigger %[1]v",
		"drop table %[1]v; select 1",
	},
}

// TestGenerate_nearMisses checks that no near miss of a statement of the generator of slice 1 is
// supported: each is a syntax error as in SQLite, or not supported.
func TestGenerate_nearMisses(t *testing.T) {
	n := *generatedCount / 10
	if slow() {
		n /= 10
	}
	// The forms of the near misses write the value of an insert themselves, so the generator writes no
	// parameter.
	g := newGenerator(t, *seed+3)
	g.Parameters = 0
	c := counts{}
	for range n {
		src, s := g.statement()
		var table, column, value, typ string
		switch s := s.(type) {
		case *stmt.CreateTable:
			table, column, typ = s.Table.Raw, s.Column.Raw, s.Type.String()
		case *stmt.Insert:
			table, column, value = s.Table.Raw, s.Column.Raw, src[s.ValueSpan.Start:s.ValueSpan.End]
		case *stmt.Select:
			table = s.Table.Raw
		case *stmt.DropTable:
			table = s.Table.Raw
		}
		for _, form := range nearMisses[fmt.Sprintf("%T", s)] {
			src := fmt.Sprintf(form, table, column, value, typ)
			o, _ := verify(t, src)
			if o == supported {
				t.Fatalf("%q, a near miss of slice 1, is supported", src)
			}
			c[o]++
		}
	}
	t.Logf("%v near misses: %v", c[syntaxError]+c[notSupported]+c[syntaxNotSaid]+c[notSupportedNotSaid], c)
	excused(t, c, 0, 985)
}
