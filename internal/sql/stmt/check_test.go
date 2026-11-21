package stmt_test

import (
	"errors"
	"flag"
	"fmt"
	"maps"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests on many texts then use a tenth of them.
func slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
}

// every nth of sqls.
func every(sqls []string, n int) []string {
	var some []string
	for i := 0; i < len(sqls); i += n {
		some = append(some, sqls[i])
	}
	return some
}

var seed = flag.Uint64("seed", 0, "seed of the mutants and of the generators")

// outcome of [verify].
type outcome int

const (
	preparse outcome = iota
	syntaxError
	notSupported
	supported
	noStatement
	syntaxNotSaid
	notSupportedNotSaid
)

func (o outcome) String() string {
	return [...]string{"rejected before parsing", "syntax error", "not supported", "supported", "no statement",
		"syntax error where SQLite does not say", "not supported where SQLite does not say"}[o]
}

// verify the answer of [stmt.Check] on src against SQLite: text that is not UTF-8 or that has a NUL
// byte is not supported, at its first such byte; a syntax error of SQLite is given exactly, with its
// result codes; a text that SQLite parses is never a syntax error; a "not supported" error has the
// form that the README gives, with a name from its vocabulary; a text without a statement holds only
// spaces, comments, and semicolons; and a supported statement does in SQLite what it says.
func verify(t testing.TB, src string) (outcome, stmt.Statement) {
	t.Helper()
	s, err := stmt.Check(src)
	if s2, err2 := stmt.Check(src); fmt.Sprint(s2, err2) != fmt.Sprint(s, err) {
		t.Fatalf("%q: two calls give %v %v and %v %v", src, s, err, s2, err2)
	}
	var se *sqlerr.Error
	var ne *stmt.NotSupportedError
	if errors.As(err, &ne) {
		checkNotSupported(t, src, err, ne)
	}

	if at, what := badText(src); at >= 0 {
		if ne == nil || ne.What != what || ne.Offset != at {
			t.Fatalf("%q: gives %v, want not supported %v at %v", src, err, what, at)
		}
		return preparse, nil
	}
	if ne != nil && (ne.What == "NUL byte" || ne.What == "invalid UTF-8") {
		t.Fatalf("%q: gives %v, but the text is valid UTF-8 without a NUL byte", src, err)
	}

	v := oracle.Judge(src)
	switch {
	case errors.As(err, &se):
		if se.Code != 1 || se.ExtendedCode != 1 || !oracle.IsSyntaxMessage(se.Message) {
			t.Fatalf("%q: gives the syntax error %+v", src, se)
		}
		if v.Kind == oracle.NotSaid && v.Reason != oracle.Unsettled && v.Reason != oracle.NotParsed {
			// An error that one of SQLite's grammar actions or a limit of its parser raised came before
			// the syntax error, or may have replaced it: the known gap of the README.
			return syntaxNotSaid, nil
		}
		if v.Kind != oracle.SyntaxError || v.Message != se.Message || v.Code != 1 {
			t.Fatalf("%q: gives the syntax error %+v, SQLite gives %+v", src, se, v)
		}
		return syntaxError, nil
	case ne != nil:
		if v.Kind == oracle.SyntaxError {
			t.Fatalf("%q: gives %v, SQLite gives the syntax error %q", src, err, v.Message)
		}
		if v.Kind == oracle.NotSaid {
			// SQLite does not say whether the text parses, so nothing holds the answer to SQLite's.
			return notSupportedNotSaid, nil
		}
		return notSupported, nil
	case err != nil:
		t.Fatalf("%q: gives the error %v of type %T", src, err, err)
	case s == nil:
		if v.Kind == oracle.SyntaxError || !onlySpace(src) {
			t.Fatalf("%q: gives no statement, SQLite gives %+v", src, v)
		}
		return noStatement, nil
	}
	// SQLite refuses a table with an error of its columns while it parses, which stops it early.
	if c, ok := s.(*stmt.CreateTable); ok && c.ColumnsError() != nil {
		if v.Kind != oracle.NotSaid || v.Reason != oracle.StoppedEarly || v.Message != c.ColumnsError().Message {
			t.Fatalf("%q: is supported with the error %q of its columns, SQLite gives %+v", src, c.ColumnsError(), v)
		}
	} else if v.Kind != oracle.Parses {
		t.Fatalf("%q: is supported, SQLite gives %+v", src, v)
	}
	checkMeaning(t, src, s)
	return supported, s
}

// badText gives the offset of the first NUL byte or byte that is not valid UTF-8 in src, and which it
// is, or -1.
func badText(src string) (int, string) {
	nul := strings.IndexByte(src, 0)
	if nul < 0 {
		nul = len(src)
	}
	if !utf8.ValidString(src[:nul]) {
		for i, r := range src[:nul] {
			if r == utf8.RuneError {
				if _, n := utf8.DecodeRuneInString(src[i:]); n == 1 {
					return i, "invalid UTF-8"
				}
			}
		}
	}
	if nul < len(src) {
		return nul, "NUL byte"
	}
	return -1, ""
}

// onlySpace reports whether src holds nothing but spaces, comments, and semicolons.
func onlySpace(src string) bool {
	for _, tk := range token.Tokens(src) {
		if tk.Kind != token.Space && tk.Kind != token.Comment && tk.Kind != token.Semi {
			return false
		}
	}
	return true
}

var messageForm = regexp.MustCompile(`^s4ql: not supported: (.+) \(at offset (\d+)\)$`)

// checkNotSupported: the error matches [stmt.ErrNotSupported], its message has the form of the
// README, its name is in the README's vocabulary, and its offset is where a token starts or ends.
func checkNotSupported(t testing.TB, src string, err error, ne *stmt.NotSupportedError) {
	t.Helper()
	m := messageForm.FindStringSubmatch(err.Error())
	if !errors.Is(err, stmt.ErrNotSupported) || m == nil || m[1] != ne.What || m[2] != fmt.Sprint(ne.Offset) {
		t.Fatalf("%q: the error %q does not have the form of not supported", src, err)
	}
	if !vocabulary(t)[ne.What] {
		t.Fatalf("%q: %q is not in the vocabulary of the README", src, ne.What)
	}
	if _, bad := badText(src); bad != "" {
		return
	}
	for _, tk := range token.Tokens(src) {
		if tk.Kind != token.Space && tk.Kind != token.Comment && (tk.Offset == ne.Offset || tk.Offset+tk.Len == ne.Offset) {
			return
		}
	}
	t.Fatalf("%q: the offset %v is neither the start nor the end of a token", src, ne.Offset)
}

// readVocabulary of the "not supported" errors: the names in the first column of the tables of the
// section "Not supported" of the README.
var readVocabulary = sync.OnceValues(func() (map[string]bool, error) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		return nil, err
	}
	_, section, _ := strings.Cut(string(b), "\n## Not supported\n")
	section, _, _ = strings.Cut(section, "\n## ")
	names := map[string]bool{}
	row := regexp.MustCompile("^\\| `([^`]+)` \\|")
	for _, line := range strings.Split(section, "\n") {
		if m := row.FindStringSubmatch(line); m != nil {
			names[m[1]] = true
		}
	}
	return names, nil
})

// vocabulary of the "not supported" errors, from [readVocabulary].
func vocabulary(t testing.TB) map[string]bool {
	t.Helper()
	names, err := readVocabulary()
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// counts of outcomes, for the log of a test.
type counts map[outcome]int

func (c counts) String() string {
	var parts []string
	for o := preparse; o <= notSupportedNotSaid; o++ {
		parts = append(parts, fmt.Sprintf("%v %v", c[o], o))
	}
	return strings.Join(parts, ", ")
}

// excused requires, in a run of all the texts with the default flags, that of the counts c exactly
// syntax are syntax errors and exactly notSupported are texts that are not supported, where SQLite
// does not say, which [oracle.Judge] excuses from the comparison with SQLite. A change of the oracle
// or of the pass that excuses more texts, or that moves a text from one outcome to the other, would
// otherwise pass unseen. The run is the same on every machine, so the numbers are exact.
func excused(t *testing.T, c counts, syntax, notSupported int) {
	t.Helper()
	if slow() {
		return
	}
	for _, name := range []string{"seed", "grammar", "generated", "mutants"} {
		if f := flag.Lookup(name); f.Value.String() != f.DefValue {
			return
		}
	}
	for _, e := range []struct {
		what string
		o    outcome
		want int
	}{
		{"syntax errors", syntaxNotSaid, syntax},
		{"not supported", notSupportedNotSaid, notSupported},
	} {
		if got := c[e.o]; got != e.want {
			t.Errorf("%v texts are %v where SQLite does not say, which the tests excuse from the comparison with SQLite, but the test pins %v. The number may be changed only on purpose: find out why it changed, and change it here and in the README of the package, with the reason.", got, e.what, e.want)
		}
	}
}

// verifyAll texts, and log the counts of the outcomes and the supported texts by their kind of
// statement, with a few of each, and give the counts. Of a text that is rejected before it is
// parsed, the part before its first NUL byte or byte that is not UTF-8 is verified too.
func verifyAll(t *testing.T, sqls []string) counts {
	t.Helper()
	c := counts{}
	byKind := map[string][]string{}
	n := 0
	var one func(string)
	one = func(sql string) {
		n++
		o, s := verify(t, sql)
		c[o]++
		if o == supported {
			kind := fmt.Sprintf("%T", s)
			byKind[kind] = append(byKind[kind], sql)
		}
		if at, _ := badText(sql); o == preparse {
			one(sql[:at])
		}
	}
	for _, sql := range sqls {
		one(sql)
	}
	t.Logf("%v texts, and %v with the parts before a byte rejected before parsing: %v", len(sqls), n, c)
	kinds := slices.Sorted(maps.Keys(byKind))
	for _, k := range kinds {
		t.Logf("supported, %v: %v, for example:", k, len(byKind[k]))
		for _, sql := range byKind[k][:min(5, len(byKind[k]))] {
			t.Logf("  %q", sql)
		}
	}
	return c
}

func corpusSQL() []string {
	var sqls []string
	for _, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
	}
	return sqls
}

// TestCheck_corpus checks the pass against SQLite on the corpus.
func TestCheck_corpus(t *testing.T) {
	sqls := corpusSQL()
	if slow() {
		sqls = every(sqls, 10)
	}
	excused(t, verifyAll(t, sqls), 62, 237)
}

// TestCheck_corpusMutants checks the pass against SQLite on one-token mutants of the corpus.
func TestCheck_corpusMutants(t *testing.T) {
	entries := corpusSQL()
	if slow() {
		entries = every(entries, 10)
	}
	var sqls []string
	for i, sql := range entries {
		sqls = append(sqls, parsetest.Mutants(sql, *seed+uint64(i), 1)...)
	}
	excused(t, verifyAll(t, sqls), 78, 178)
}

var grammarCount = flag.Int("grammar", 20000, "sentences of the grammar in TestCheck_grammar")

// readGrammar of SQLite from the parser's copy of parse.y.
func readGrammar(t testing.TB) *parsetest.Grammar {
	t.Helper()
	b, err := os.ReadFile("../parser/testdata/parse.y")
	is.NotError(t, err)
	g, err := parsetest.ParseGrammar(string(b))
	is.NotError(t, err)
	return g
}

// TestCheck_grammar checks the pass against SQLite on random sentences of SQLite's grammar, and a
// mutant of each.
func TestCheck_grammar(t *testing.T) {
	n := *grammarCount
	if slow() {
		n /= 10
	}
	gen := parsetest.NewGenerator(readGrammar(t), *seed)
	var sqls []string
	for i := range n {
		s := gen.Sentence("input", 4+gen.IntN(40))
		sqls = append(sqls, s)
		sqls = append(sqls, parsetest.Mutants(s, *seed+uint64(i), 1)...)
	}
	excused(t, verifyAll(t, sqls), 2356, 1479)
}

// FuzzCheck checks the pass against SQLite on any text.
func FuzzCheck(f *testing.F) {
	for i, sql := range corpusSQL() {
		if i%25 == 0 {
			f.Add(sql)
		}
	}
	for _, c := range checkCases {
		f.Add(c.src)
	}
	g := newGenerator(f, 1)
	for range 200 {
		src, _ := g.Statement()
		f.Add(src)
	}
	f.Fuzz(func(t *testing.T, src string) {
		verify(t, src)
	})
}

// checkCase is a text and what [stmt.Check] gives for it.
type checkCase struct {
	name string
	src  string
	// want is the statement, or nil.
	want stmt.Statement
	// err is the message of the error, or "".
	err string
}

func name(raw, value string, offset int) stmt.Name {
	return stmt.Name{Raw: raw, Value: value, Offset: offset}
}

// checkCases pin the definition of slice 1 in the README: each supported form, and the near misses
// with their exact errors.
var checkCases = []checkCase{
	// The supported forms.
	{name: "create table", src: "create table t1 (v1 int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 58}, Table: name("t1", "t1", 13), Columns: []stmt.Column{{Name: name("v1", "v1", 17), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "create table with the options in the other order, keywords in upper case, and a semicolon",
		src:  `CREATE TABLE "T""1" (V1 INT PRIMARY KEY) WITHOUT ROWID, STRICT;`,
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 63}, Table: name(`"T""1"`, `T"1`, 13), Columns: []stmt.Column{{Name: name("V1", "V1", 21), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "create table with comments in every gap", src: "/*a*/create/*b*/table--c\nt(v/**/Int primary\tkey)strict,without rowid--d",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 5, End: 68}, Table: name("t", "t", 25), Columns: []stmt.Column{{Name: name("v", "v", 27), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "create table named by a keyword", src: "create table abort (key int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 62}, Table: name("abort", "abort", 13), Columns: []stmt.Column{{Name: name("key", "key", 20), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "create table with a name of non-ASCII letters", src: `create table "é" (été int primary key) strict, without rowid`,
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 63}, Table: name(`"é"`, "é", 13), Columns: []stmt.Column{{Name: name("été", "été", 19), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "insert", src: "insert into t1 (v1) values (1)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t1", "t1", 12), Columns: []stmt.Name{name("v1", "v1", 16)}, Values: []stmt.Term{{Value: stmt.Integer(1), Span: stmt.Span{Start: 28, End: 29}}}}},
	{name: "insert of the largest integer", src: "insert into t (v) values (9223372036854775807)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 46}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Integer(9223372036854775807), Span: stmt.Span{Start: 26, End: 45}}}}},
	{name: "insert of the smallest integer", src: "insert into t (v) values (-9223372036854775808)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 47}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Integer(-9223372036854775808), Span: stmt.Span{Start: 26, End: 46}}}}},
	{name: "insert of a negative integer with a comment after the sign", src: "insert into t (v) values (- /* x */ 5)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 38}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Integer(-5), Span: stmt.Span{Start: 26, End: 37}}}}},
	{name: "insert of an integer with leading zeros", src: `insert into "t" ("v") values (-000000000000000000000000009223372036854775808);`,
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 78}, Table: name(`"t"`, "t", 12), Columns: []stmt.Name{name(`"v"`, "v", 17)}, Values: []stmt.Term{{Value: stmt.Integer(-9223372036854775808), Span: stmt.Span{Start: 30, End: 76}}}}},
	{name: "insert of zero with a sign", src: "insert into t (v) values (-0)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 29}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Integer(0), Span: stmt.Span{Start: 26, End: 28}}}}},
	{name: "insert of a parameter", src: "insert into t1 (v1) values (?)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t1", "t1", 12), Columns: []stmt.Name{name("v1", "v1", 16)}, Values: []stmt.Term{{Parameter: 1, Span: stmt.Span{Start: 28, End: 29}}}}},
	{name: "insert of a parameter with comments around it and a semicolon", src: "INSERT INTO t (v) VALUES (/* a */?--b\n);",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 40}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Parameter: 1, Span: stmt.Span{Start: 33, End: 34}}}}},
	{name: "select", src: "select * from t1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 16}, Table: name("t1", "t1", 14)}},
	{name: "select with semicolons and a comment after it", src: "SELECT*FROM\"a b\";;; -- end",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 17}, Table: name(`"a b"`, "a b", 11)}},
	{name: "drop table", src: "drop table t1",
		want: &stmt.DropTable{Span: stmt.Span{Start: 0, End: 13}, Table: name("t1", "t1", 11)}},
	{name: "drop table after empty statements", src: " ;; drop table \"sqlite\"",
		want: &stmt.DropTable{Span: stmt.Span{Start: 4, End: 23}, Table: name(`"sqlite"`, "sqlite", 15)}},

	// No statement.
	{name: "empty text", src: ""},
	{name: "spaces, comments, and semicolons", src: " ; /* x */ ;; -- y\n\t"},
	{name: "an unterminated comment", src: "/* x"},

	// Before parsing.
	{name: "NUL byte", src: "select * from t\x00", err: "s4ql: not supported: NUL byte (at offset 15)"},
	{name: "NUL byte before a syntax error", src: "\x00select", err: "s4ql: not supported: NUL byte (at offset 0)"},
	{name: "invalid UTF-8", src: "select * from \"\xff\"", err: "s4ql: not supported: invalid UTF-8 (at offset 15)"},
	{name: "a surrogate in UTF-8", src: "select * from \"\xed\xa0\x80\"", err: "s4ql: not supported: invalid UTF-8 (at offset 15)"},
	{name: "invalid UTF-8 before a NUL byte", src: "selec\xc0\x00 t", err: "s4ql: not supported: invalid UTF-8 (at offset 5)"},
	{name: "invalid UTF-8 in a comment", src: "-- \xff\nselect * from t", err: "s4ql: not supported: invalid UTF-8 (at offset 3)"},

	// Syntax errors.
	{name: "syntax error", src: "selec * from t", err: `near "selec": syntax error`},
	{name: "incomplete input", src: "create table t1 (v1 int primary key) strict, without", err: "incomplete input"},
	{name: "syntax error in a statement that is not supported", src: "create view v as select 1 + where", err: `near "where": syntax error`},
	{name: "syntax error after a statement that is not supported", src: "create table t (v1 int primary key) strict without rowid", err: `near "without": syntax error`},
	{name: "unrecognized token", src: "insert into t (v) values (1_)", err: `unrecognized token: "1_"`},

	// More than one statement.
	{name: "second statement", src: "select * from t; select * from t", err: "s4ql: not supported: second statement (at offset 17)"},
	{name: "second statement with a syntax error", src: "drop table t;selec", err: "s4ql: not supported: second statement (at offset 13)"},
	{name: "second statement after comments", src: "drop table t; -- x\n/* y */ ; begin", err: "s4ql: not supported: second statement (at offset 29)"},
	{name: "first statement not supported before a second one", src: "drop table main.t; drop table t", err: "s4ql: not supported: schema name (at offset 11)"},
	{name: "first statement a syntax error before a second one", src: "drop tabl t; drop table t", err: `near "tabl": syntax error`},
	{name: "the two tokens of /* at the end", src: "drop table t; /*", err: "s4ql: not supported: second statement (at offset 14)"},

	// Commands that are not supported.
	{name: "begin", src: "begin", err: "s4ql: not supported: BEGIN (at offset 0)"},
	{name: "create view", src: "create view v as select 1", err: "s4ql: not supported: CREATE VIEW (at offset 0)"},
	{name: "explain", src: "explain select * from t", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain query plan", src: "explain query plan select * from t", err: "s4ql: not supported: EXPLAIN QUERY PLAN (at offset 0)"},
	{name: "a statement after spaces nested deeper than the parser's limit", src: " ;\nselect " + strings.Repeat("(", 2500) + "1" + strings.Repeat(")", 2500),
		err: "s4ql: not supported: statement beyond a limit of the parser (at offset 3)"},
	{name: "a statement nested deeper than the parser's limit", src: "select " + strings.Repeat("(", 2500) + "1" + strings.Repeat(")", 2500),
		err: "s4ql: not supported: statement beyond a limit of the parser (at offset 0)"},

	// Names.
	{name: "a name in brackets", src: "select * from [t]", err: "s4ql: not supported: name in brackets (at offset 14)"},
	{name: "a name in backticks", src: "drop table `t`", err: "s4ql: not supported: name in backticks (at offset 11)"},
	{name: "a single-quoted name", src: "create table 't' (v int primary key) strict, without rowid", err: "s4ql: not supported: single-quoted name (at offset 13)"},
	{name: "a single-quoted column name", src: "create table t ('v' int primary key) strict, without rowid", err: "s4ql: not supported: single-quoted name (at offset 16)"},
	{name: "a single-quoted column name in insert", src: "insert into t ('v') values (1)", err: "s4ql: not supported: single-quoted name (at offset 15)"},
	{name: "an empty table name", src: `create table "" (v int primary key) strict, without rowid`, err: "s4ql: not supported: empty name (at offset 13)"},
	{name: "an empty column name", src: `create table t ("" int primary key) strict, without rowid`, err: "s4ql: not supported: empty name (at offset 16)"},
	{name: "an empty table name in insert", src: `insert into "" (v) values (1)`, err: "s4ql: not supported: empty name (at offset 12)"},
	{name: "an empty column name in insert", src: `insert into t ("") values (1)`, err: "s4ql: not supported: empty name (at offset 15)"},
	{name: "an empty table name in select", src: `select * from ""`, err: "s4ql: not supported: empty name (at offset 14)"},
	{name: "an empty table name in drop table", src: `drop table ""`, err: "s4ql: not supported: empty name (at offset 11)"},
	{name: "an empty table name after a schema name", src: `insert into main."" (v) values (1)`, err: "s4ql: not supported: schema name (at offset 12)"},
	{name: "a table name that starts with sqlite_", src: "select * from sqlite_schema", err: "s4ql: not supported: reserved table name (at offset 14)"},
	{name: "a table name that starts with sqlite_ in another case", src: `drop table "SQLite_x"`, err: "s4ql: not supported: reserved table name (at offset 11)"},
	{name: "a table name that starts with s4ql_", src: "insert into S4QL_t (v) values (1)", err: "s4ql: not supported: reserved table name (at offset 12)"},
	{name: "a table name that starts with s4ql_ in create table", src: "create table s4ql_t (v int primary key) strict, without rowid", err: "s4ql: not supported: reserved table name (at offset 13)"},
	{name: "a column name that starts with sqlite_", src: "create table t (sqlite_v int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 63}, Table: name("t", "t", 13), Columns: []stmt.Column{{Name: name("sqlite_v", "sqlite_v", 16), Type: stmt.TypeInt, PrimaryKey: true}}}},
	{name: "the name of a virtual table of SQLite", src: "select * from json_each", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 14)"},
	{name: "the name of a PRAGMA's virtual table", src: `drop table "PRAGMA_table_list"`, err: "s4ql: not supported: name of a virtual table of SQLite (at offset 11)"},
	{name: "the name of a virtual table of JSONB", src: "insert into JSONB_TREE (v) values (1)", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 12)"},
	{name: "a name with pragma_ that is no PRAGMA", src: "select * from pragma_x", want: &stmt.Select{Span: stmt.Span{Start: 0, End: 22}, Table: name("pragma_x", "pragma_x", 14)}},
	{name: "a schema name", src: "select * from main.t", err: "s4ql: not supported: schema name (at offset 14)"},

	// CREATE TABLE.
	{name: "temp", src: "create temp table t (v int primary key) strict, without rowid", err: "s4ql: not supported: TEMP (at offset 7)"},
	{name: "as select", src: "create table t as select 1", err: "s4ql: not supported: CREATE TABLE AS (at offset 15)"},
	{name: "no options", src: "create table t (v int primary key)", err: "s4ql: not supported: table without STRICT and WITHOUT ROWID (at offset 34)"},
	{name: "strict alone", src: "create table t (v int primary key) strict;", err: "s4ql: not supported: table without WITHOUT ROWID (at offset 41)"},
	{name: "without rowid alone", src: "create table t (v int primary key) without rowid", err: "s4ql: not supported: table without STRICT (at offset 48)"},
	{name: "strict twice", src: "create table t (v int primary key) strict, strict, without rowid", err: "s4ql: not supported: table option (at offset 43)"},
	{name: "without rowid twice", src: "create table t (v int primary key) without rowid, without rowid", err: "s4ql: not supported: table option (at offset 50)"},
	{name: "a third option", src: "create table t (v int primary key) strict, without rowid, strict", err: "s4ql: not supported: table option (at offset 56)"},
	{name: "an empty first option and an unknown option", src: "create table t (v int primary key), foo", err: "s4ql: not supported: empty table option (at offset 34)"},
	{name: "an empty first option", src: "create table t (v int primary key), strict, without rowid", err: "s4ql: not supported: empty table option (at offset 34)"},
	{name: "an unknown option", src: "create table t (v int primary key) strict, without rowid, foo", err: "s4ql: not supported: table option (at offset 56)"},
	{name: "rowid in quotes", src: `create table t (v int primary key) strict, without "rowid"`, err: "s4ql: not supported: table option (at offset 51)"},
	{name: "strict in quotes", src: `create table t (v int primary key) "strict", without rowid`, err: "s4ql: not supported: table option (at offset 35)"},
	{name: "the type integer", src: "create table t (v integer primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 60}, Table: name("t", "t", 13), Columns: []stmt.Column{{Name: name("v", "v", 16), Type: stmt.TypeInteger, PrimaryKey: true}}}},
	{name: "the type real in upper case", src: "CREATE TABLE t (v REAL PRIMARY KEY) STRICT, WITHOUT ROWID",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 57}, Table: name("t", "t", 13), Columns: []stmt.Column{{Name: name("v", "v", 16), Type: stmt.TypeReal, PrimaryKey: true}}}},
	{name: "the type text in mixed case", src: "create table t (v Text primary key) without rowid, strict",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 57}, Table: name("t", "t", 13), Columns: []stmt.Column{{Name: name("v", "v", 16), Type: stmt.TypeText, PrimaryKey: true}}}},
	{name: "the type blob in mixed case", src: "create table t (v bLoB primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 57}, Table: name("t", "t", 13), Columns: []stmt.Column{{Name: name("v", "v", 16), Type: stmt.TypeBlob, PrimaryKey: true}}}},
	{name: "text in quotes", src: `create table t (v "text" primary key) strict, without rowid`, err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "blob in single quotes", src: `create table t (v 'blob' primary key) strict, without rowid`, err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "a text with a size", src: "create table t (v text(10) primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 22)"},
	{name: "integer of two words", src: "create table t (v integer unsigned primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 26)"},
	{name: "a type that SQLite's STRICT table does not know", src: "create table t (v varchar primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "double", src: "create table t (v double primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "int in quotes", src: `create table t (v "int" primary key) strict, without rowid`, err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "a type of two words", src: "create table t (v int generated always primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 22)"},
	{name: "a type with a size", src: "create table t (v int(10) primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 21)"},
	{name: "a type of two words that starts with int", src: "create table t (v int unsigned primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 22)"},
	{name: "a type of two words that does not start with int", src: "create table t (v big int primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "any", src: "create table t (v any primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 18)"},
	{name: "no type", src: "create table t (v primary key) strict, without rowid", err: "s4ql: not supported: column without a type (at offset 18)"},
	{name: "a type with a size and no primary key", src: "create table t (v int(10)) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 21)"},
	{name: "a type of two words and no primary key", src: "create table t (v int unsigned) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 22)"},
	{name: "a constraint after primary key and a second column", src: "create table t (a int primary key autoincrement, b int) strict, without rowid", err: "s4ql: not supported: AUTOINCREMENT (at offset 34)"},
	{name: "a constraint after primary key and a table constraint", src: "create table t (a int primary key desc, check (a > 0)) strict, without rowid", err: "s4ql: not supported: ASC or DESC (at offset 34)"},
	{name: "no primary key", src: "create table t (v int) strict, without rowid", err: "s4ql: not supported: table without PRIMARY KEY (at offset 21)"},
	{name: "a constraint after primary key", src: "create table t (v int primary key unique) strict, without rowid", err: "s4ql: not supported: UNIQUE (at offset 34)"},
	{name: "a constraint name", src: "create table t (v int constraint pk primary key) strict, without rowid", err: "s4ql: not supported: CONSTRAINT name (at offset 22)"},
	{name: "asc", src: "create table t (v int primary key asc) strict, without rowid", err: "s4ql: not supported: ASC or DESC (at offset 34)"},
	{name: "on conflict", src: "create table t (v int primary key on conflict fail) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
	{name: "autoincrement", src: "create table t (v int primary key autoincrement) strict, without rowid", err: "s4ql: not supported: AUTOINCREMENT (at offset 34)"},
	{name: "a second column without NOT NULL, which takes NULL", src: "create table t (v int primary key, w int) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 63}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column("v", "v", 16, stmt.TypeInt, true, false), column("w", "w", 35, stmt.TypeInt, false, false)}}},
	{name: "a table constraint", src: "create table t (v int, primary key (v)) strict, without rowid", err: "s4ql: not supported: table constraint (at offset 21)"},
	{name: "a table constraint after the column", src: "create table t (v int primary key, check (v > 0)) strict, without rowid", err: "s4ql: not supported: table constraint (at offset 33)"},

	// INSERT.
	{name: "replace", src: "replace into t (v) values (1)", err: "s4ql: not supported: REPLACE (at offset 0)"},
	{name: "insert or replace", src: "insert or replace into t (v) values (1)", err: "s4ql: not supported: INSERT OR (at offset 7)"},
	{name: "with and replace", src: "with c as (select 1) replace into t (v) values (1)", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "a select with a with", src: "insert into t (v) with c as (select 1) select * from c", err: "s4ql: not supported: WITH clause (at offset 18)"},
	{name: "with", src: "with c as (select 1) insert into t (v) values (1)", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "an alias", src: "insert into t as u (v) values (1)", err: "s4ql: not supported: alias (at offset 14)"},
	{name: "an alias and no column list", src: "insert into t as u values (1)", err: "s4ql: not supported: alias (at offset 14)"},
	{name: "default values", src: "insert into t default values", err: "s4ql: not supported: DEFAULT VALUES (at offset 14)"},
	{name: "default values with a column list", src: "insert into t (v) default values", err: "s4ql: not supported: DEFAULT VALUES (at offset 18)"},
	{name: "two rows", src: "insert into t (v) values (1), (2)", err: "s4ql: not supported: second row (at offset 28)"},
	{name: "a select", src: "insert into t (v) select 1", err: "s4ql: not supported: INSERT with SELECT (at offset 18)"},
	{name: "values with a compound", src: "insert into t (v) values (1) union select 2", err: "s4ql: not supported: compound SELECT (at offset 29)"},
	{name: "an upsert", src: "insert into t (v) values (1) on conflict do nothing", err: "s4ql: not supported: ON CONFLICT clause (at offset 29)"},
	{name: "returning", src: "insert into t (v) values (1) returning *", err: "s4ql: not supported: RETURNING clause (at offset 29)"},
	{name: "a plus sign", src: "insert into t (v) values (+1)", err: "s4ql: not supported: + sign (at offset 26)"},
	{name: "two signs", src: "insert into t (v) values (- -1)", err: "s4ql: not supported: second sign (at offset 28)"},
	{name: "a minus and a plus", src: "insert into t (v) values (-+1)", err: "s4ql: not supported: second sign (at offset 27)"},
	{name: "a minus and a bitwise not", src: "insert into t (v) values (-~1)", err: "s4ql: not supported: ~ operator (at offset 27)"},
	{name: "a minus and not", src: "insert into t (v) values (- not 1)", err: "s4ql: not supported: NOT operator (at offset 28)"},
	{name: "a bitwise not", src: "insert into t (v) values (~1)", err: "s4ql: not supported: ~ operator (at offset 26)"},
	{name: "not", src: "insert into t (v) values (not 1)", err: "s4ql: not supported: NOT operator (at offset 26)"},
	{name: "a hex literal", src: "insert into t (v) values (0x10)", err: "s4ql: not supported: hex literal (at offset 26)"},
	{name: "a negative hex literal", src: "insert into t (v) values (-0X10)", err: "s4ql: not supported: hex literal (at offset 27)"},
	{name: "a digit separator", src: "insert into t (v) values (1_000)", err: "s4ql: not supported: number with a digit separator (at offset 26)"},
	{name: "a number outside 64 bits", src: "insert into t (v) values (9223372036854775808)", err: "s4ql: not supported: integer outside 64 bits (at offset 26)"},
	{name: "a negative number outside 64 bits", src: "insert into t (v) values (-9223372036854775809)", err: "s4ql: not supported: integer outside 64 bits (at offset 27)"},
	{name: "a real", src: "insert into t (v) values (1.5)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(1.5), Span: stmt.Span{Start: 26, End: 29}}}}},
	{name: "a real without a digit before the point", src: "insert into t (v) values (.5)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 29}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(0.5), Span: stmt.Span{Start: 26, End: 28}}}}},
	{name: "a real without a digit after the point", src: "insert into t (v) values (1.)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 29}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(1), Span: stmt.Span{Start: 26, End: 28}}}}},
	{name: "a real with an exponent", src: "insert into t (v) values (1e3)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(1000), Span: stmt.Span{Start: 26, End: 29}}}}},
	{name: "a real with an exponent in upper case and its sign", src: "insert into t (v) values (1E+3)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(1000), Span: stmt.Span{Start: 26, End: 30}}}}},
	{name: "a real with a fraction and a negative exponent", src: "insert into t (v) values (1.5e-3)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 33}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(0.0015), Span: stmt.Span{Start: 26, End: 32}}}}},
	{name: "a negative real", src: "insert into t (v) values (-1.5)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(-1.5), Span: stmt.Span{Start: 26, End: 30}}}}},
	{name: "negative zero", src: "insert into t (v) values (-0.0)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(math.Copysign(0, -1)), Span: stmt.Span{Start: 26, End: 30}}}}},
	{name: "a real zero", src: "insert into t (v) values (0.0)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(0), Span: stmt.Span{Start: 26, End: 29}}}}},
	{name: "a negative real with a comment after the sign", src: "insert into t (v) values (- /* x */ 2.5e10)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 43}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(-2.5e10), Span: stmt.Span{Start: 26, End: 42}}}}},
	{name: "the largest real", src: "insert into t (v) values (1.7976931348623157e308)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 49}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(math.MaxFloat64), Span: stmt.Span{Start: 26, End: 48}}}}},
	{name: "the smallest real above zero", src: "insert into t (v) values (4.9e-324)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(math.SmallestNonzeroFloat64), Span: stmt.Span{Start: 26, End: 34}}}}},
	{name: "a real too small for a float, which is zero", src: "insert into t (v) values (1e-400)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 33}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(0), Span: stmt.Span{Start: 26, End: 32}}}}},
	{name: "a real with an exponent of more digits than SQLite reads", src: "insert into t (v) values (0.5e-1000000)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 39}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(0), Span: stmt.Span{Start: 26, End: 38}}}}},
	{name: "a real of more digits than SQLite reads, which it rounds otherwise than to the nearest float", src: "insert into t (v) values (3500000000000000.2500001)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 51}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(3500000000000000), Span: stmt.Span{Start: 26, End: 50}}}}},
	{name: "a real with leading and trailing zeros", src: "insert into t (v) values (00012.50)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Real(12.5), Span: stmt.Span{Start: 26, End: 34}}}}},
	{name: "an empty string", src: "insert into t (v) values ('')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 29}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Text(""), Span: stmt.Span{Start: 26, End: 28}}}}},
	{name: "a string with a quote", src: "insert into t (v) values ('a''b')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 33}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Text("a'b"), Span: stmt.Span{Start: 26, End: 32}}}}},
	{name: "a string of letters of more than one byte", src: "insert into t (v) values ('été')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 34}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Text("été"), Span: stmt.Span{Start: 26, End: 33}}}}},
	{name: "a string that looks like a comment", src: "insert into t (v) values ('-- not a comment')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 45}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Text("-- not a comment"), Span: stmt.Span{Start: 26, End: 44}}}}},
	{name: "a blob", src: "insert into t (v) values (x'6162')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 34}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Blob("ab"), Span: stmt.Span{Start: 26, End: 33}}}}},
	{name: "a blob in upper and mixed case", src: "insert into t (v) values (X'aBcD')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 34}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Blob{0xab, 0xcd}, Span: stmt.Span{Start: 26, End: 33}}}}},
	{name: "an empty blob", src: "insert into t (v) values (x'')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{{Value: stmt.Blob{}, Span: stmt.Span{Start: 26, End: 29}}}}},
	{name: "a string after a minus", src: "insert into t (v) values (-'a')", err: "s4ql: not supported: string after a sign (at offset 27)"},
	{name: "a blob after a minus", src: "insert into t (v) values (- x'01')", err: "s4ql: not supported: blob after a sign (at offset 28)"},
	{name: "a real that SQLite reads as an infinity", src: "insert into t (v) values (1e999)", err: "s4ql: not supported: infinite real number (at offset 26)"},
	{name: "a negative real that SQLite reads as an infinity", src: "insert into t (v) values (-1e999)", err: "s4ql: not supported: infinite real number (at offset 27)"},
	{name: "true", src: "insert into t (v) values (true)", err: "s4ql: not supported: TRUE or FALSE (at offset 26)"},
	{name: "false after a minus", src: "insert into t (v) values (-FALSE)", err: "s4ql: not supported: TRUE or FALSE (at offset 27)"},
	{name: "true in double quotes", src: `insert into t (v) values ("true")`, err: "s4ql: not supported: column reference (at offset 26)"},
	{name: "true after a table name", src: "insert into t (v) values (t.true)", err: "s4ql: not supported: column reference (at offset 26)"},
	{name: "false after a minus and a table name", src: "insert into t (v) values (-t.false)", err: "s4ql: not supported: column reference (at offset 27)"},
	{name: "a concatenation", src: "insert into t (v) values ('a' || 'b')", err: "s4ql: not supported: binary operator (at offset 26)"},
	{name: "a cast of a string", src: "insert into t (v) values (cast('1' as int))", err: "s4ql: not supported: CAST (at offset 26)"},
	{name: "a real after two minus signs", src: "insert into t (v) values (- -1.5)", err: "s4ql: not supported: second sign (at offset 28)"},
	{name: "a real after a plus", src: "insert into t (v) values (+1.5)", err: "s4ql: not supported: + sign (at offset 26)"},
	{name: "a string with a collation", src: "insert into t (v) values ('a' collate nocase)", err: "s4ql: not supported: COLLATE (at offset 26)"},
	{name: "a real with a digit separator", src: "insert into t (v) values (1_000.5)", err: "s4ql: not supported: number with a digit separator (at offset 26)"},
	{name: "an unterminated string", src: "insert into t (v) values ('a)", err: `unrecognized token: "'a)"`},
	{name: "a blob of an odd number of digits", src: "insert into t (v) values (x'6')", err: `unrecognized token: "x'6'"`},
	{name: "a blob with a digit that is not hex", src: "insert into t (v) values (x'6g')", err: `unrecognized token: "x'6g'"`},
	{name: "an exponent without digits", src: "insert into t (v) values (1e)", err: `unrecognized token: "1e"`},
	{name: "an exponent with a sign and no digits", src: "insert into t (v) values (1e+)", err: `unrecognized token: "1e"`},
	{name: "a real with two points", src: "insert into t (v) values (1.5.5)", err: `near ".5": syntax error`},
	{name: "a point alone", src: "insert into t (v) values (.)", err: `near ".": syntax error`},
	{name: "null", src: "insert into t (v) values (null)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{lit(stmt.Null{}, 26, 30)}}},
	{name: "a negative null", src: "insert into t (v) values (-null)", err: "s4ql: not supported: NULL after a sign (at offset 27)"},
	{name: "current_time", src: "insert into t (v) values (current_time)", err: "s4ql: not supported: current date or time (at offset 26)"},
	{name: "parentheses", src: "insert into t (v) values ((1))", err: "s4ql: not supported: parentheses (at offset 26)"},
	{name: "a sign before parentheses", src: "insert into t (v) values (-(1))", err: "s4ql: not supported: parentheses (at offset 27)"},
	{name: "a numbered parameter", src: "insert into t (v) values (?1)", err: "s4ql: not supported: numbered parameter (at offset 26)"},
	{name: "the parameter ?0, which SQLite refuses", src: "insert into t (v) values (?0)", err: "s4ql: not supported: numbered parameter (at offset 26)"},
	{name: "a parameter with a number beyond SQLite's limit", src: "insert into t (v) values (?32767)", err: "s4ql: not supported: numbered parameter (at offset 26)"},
	{name: "a parameter with a colon", src: "insert into t (v) values (:a)", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter with an at sign", src: "insert into t (v) values (@a)", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter with a dollar sign", src: "insert into t (v) values ($a)", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter with a dollar sign and a suffix of Tcl", src: "insert into t (v) values ($a::b(c))", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter with a hash and a letter", src: "insert into t (v) values (#a)", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter with a hash and a digit", src: "insert into t (v) values (#1)", err: `near "#1": syntax error`},
	{name: "two parameters without a comma", src: "insert into t (v) values (? ?)", err: `near "?": syntax error`},
	{name: "a parameter after a minus", src: "insert into t (v) values (-?)", err: "s4ql: not supported: parameter after a sign (at offset 27)"},
	{name: "a numbered parameter after a minus", src: "insert into t (v) values (- ?1)", err: "s4ql: not supported: parameter after a sign (at offset 28)"},
	{name: "a parameter after a plus", src: "insert into t (v) values (+?)", err: "s4ql: not supported: + sign (at offset 26)"},
	{name: "a parameter after two minus signs", src: "insert into t (v) values (- - ?)", err: "s4ql: not supported: second sign (at offset 28)"},
	{name: "a parameter in parentheses", src: "insert into t (v) values ((?))", err: "s4ql: not supported: parentheses (at offset 26)"},
	{name: "a parameter in an expression", src: "insert into t (v) values (? + 1)", err: "s4ql: not supported: binary operator (at offset 26)"},
	{name: "a parameter in a cast", src: "insert into t (v) values (cast(? as int))", err: "s4ql: not supported: CAST (at offset 26)"},
	{name: "a parameter and a second row", src: "insert into t (v) values (?), (?)", err: "s4ql: not supported: second row (at offset 28)"},
	{name: "a parameter in a second statement", src: "insert into t (v) values (?); insert into t (v) values (?)", err: "s4ql: not supported: second statement (at offset 30)"},
	{name: "a parameter in RETURNING", src: "insert into t (v) values (?) returning ?", err: "s4ql: not supported: RETURNING clause (at offset 29)"},
	{name: "a parameter in an upsert", src: "insert into t (v) values (?) on conflict (v) do update set v = ?", err: "s4ql: not supported: ON CONFLICT clause (at offset 29)"},
	{name: "a parameter as a table", src: "insert into ? (v) values (1)", err: `near "?": syntax error`},
	{name: "a parameter in a select", src: "select ?", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "a parameter in WHERE", src: "select * from t where v = ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 27}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, param(1, 26))}}},
	{name: "a parameter in LIMIT", src: "select * from t limit ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 23}, Table: name("t", "t", 14), Limit: param(1, 22)}},
	{name: "a parameter in a default", src: "create table t (v int primary key default ?) strict, without rowid", err: `near "?": syntax error`},

	// DEFAULT, of slice 11.
	{name: "a default of 0 on a NOT NULL column, as goqite declares it", src: "create table t (k int primary key, v int not null default 0) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 82}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, true), stmt.Integer(0), 58, 59)}}},
	{name: "a default before NOT NULL", src: "create table t (k int primary key, v int default 0 not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 82}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, true), stmt.Integer(0), 49, 50)}}},
	{name: "a default in upper case on a column that takes NULL", src: "create table t (k int primary key, v INTEGER DEFAULT 5) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 77}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInteger, false, false), stmt.Integer(5), 53, 54)}}},
	{name: "a default with comments around it", src: "create table t (k int primary key, v int/* a */default/* b */7--c\n) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 89}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(7), 61, 62)}}},
	{name: "a default of a negative integer", src: "create table t (k int primary key, v int default -1) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 74}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(-1), 49, 51)}}},
	{name: "a default of a negative integer with a space after the sign", src: "create table t (k int primary key, v int default - 1) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 75}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(-1), 49, 52)}}},
	{name: "a default of a negative integer with a comment after the sign", src: "create table t (k int primary key, v int default - /* c */ 1) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 83}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(-1), 49, 60)}}},
	{name: "a default of the smallest integer", src: "create table t (k int primary key, v int default -9223372036854775808) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 92}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(math.MinInt64), 49, 69)}}},
	{name: "a default of the largest integer", src: "create table t (k int primary key, v int default 9223372036854775807) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 91}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(math.MaxInt64), 49, 68)}}},
	{name: "a default of an integer with leading zeros", src: "create table t (k int primary key, v int default 00012) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 77}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(12), 49, 54)}}},
	{name: "a default of zero with a sign", src: "create table t (k int primary key, v int default -0) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 74}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Integer(0), 49, 51)}}},
	{name: "a default of negative zero", src: "create table t (k int primary key, v real default -0.0) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 77}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(math.Copysign(0, -1)), 50, 54)}}},
	{name: "a default of a real", src: "create table t (k int primary key, v real default 1.0) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 76}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(1), 50, 53)}}},
	{name: "a default of a real without a whole part", src: "create table t (k int primary key, v real default .5) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 75}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(0.5), 50, 52)}}},
	{name: "a default of a real without a fraction", src: "create table t (k int primary key, v real default 1.) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 75}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(1), 50, 52)}}},
	{name: "a default of a real with an exponent", src: "create table t (k int primary key, v real default 1e3) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 76}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(1000), 50, 53)}}},
	{name: "a default of a real with a signed exponent in upper case", src: "create table t (k int primary key, v real not null default 1E+3) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 86}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, true), stmt.Real(1000), 59, 63)}}},
	{name: "a default of a negative real with a negative exponent", src: "create table t (k int primary key, v real default -1.5e-3) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 80}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(-0.0015), 50, 57)}}},
	{name: "a default of a real below the smallest float", src: "create table t (k int primary key, v real default 1e-400) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 79}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeReal, false, false), stmt.Real(0), 50, 56)}}},
	{name: "a default of a string", src: "create table t (k int primary key, v text default 'a') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 76}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeText, false, false), stmt.Text("a"), 50, 53)}}},
	{name: "a default of a string with a quote", src: "create table t (k int primary key, v text not null default 'it''s') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 89}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeText, false, true), stmt.Text("it's"), 59, 66)}}},
	{name: "a default of the empty string", src: "create table t (k int primary key, v text default '') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 75}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeText, false, false), stmt.Text(""), 50, 52)}}},
	{name: "a default of a string that looks like SQL", src: "create table t (k int primary key, v text default 'x'' default 1 --') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 91}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeText, false, false), stmt.Text("x' default 1 --"), 50, 68)}}},
	{name: "a default of a blob", src: "create table t (k int primary key, v blob default x'00') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 78}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeBlob, false, false), stmt.Blob{0}, 50, 55)}}},
	{name: "a default of a blob in mixed case", src: "create table t (k int primary key, v blob default X'aBcD') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 80}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeBlob, false, false), stmt.Blob{0xab, 0xcd}, 50, 57)}}},
	{name: "a default of the empty blob", src: "create table t (k int primary key, v blob not null default x'') strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 85}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeBlob, false, true), stmt.Blob{}, 59, 62)}}},
	{name: "a default of NULL", src: "create table t (k int primary key, v text default null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 77}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeText, false, false), stmt.Null{}, 50, 54)}}},
	{name: "a default of NULL in upper case", src: "create table t (k int primary key, v int default NULL) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 76}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, false), stmt.Null{}, 49, 53)}}},
	{name: "a default of NULL on a NOT NULL column", src: "create table t (k int primary key, v int not null default null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 85}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeInt, false, true), stmt.Null{}, 58, 62)}}},
	{name: "a default of NULL before NOT NULL", src: "create table t (k int primary key, v blob default Null not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 86}, Table: name("t", "t", 13), Columns: []stmt.Column{column("k", "k", 16, stmt.TypeInt, true, false), withDefault(column("v", "v", 35, stmt.TypeBlob, false, true), stmt.Null{}, 50, 54)}}},
	{name: "a default on the primary key", src: "create table t (k int primary key default 1) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 66}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeInt, true, false), stmt.Integer(1), 42, 43)}}},
	{name: "a default before PRIMARY KEY", src: "create table t (k text default 'a' primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 69}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeText, true, false), stmt.Text("a"), 31, 34)}}},
	{name: "a default between PRIMARY KEY and NOT NULL", src: "create table t (k real primary key default 2.5 not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 78}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeReal, true, true), stmt.Real(2.5), 43, 46)}}},
	{name: "a default between NOT NULL and PRIMARY KEY", src: "create table t (k blob not null default x'01' primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 80}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeBlob, true, true), stmt.Blob{1}, 40, 45)}}},
	{name: "a default of NULL on the primary key", src: "create table t (k int primary key default null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 69}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeInt, true, false), stmt.Null{}, 42, 46)}}},
	{name: "a default of NULL before PRIMARY KEY", src: "create table t (k int default null primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 69}, Table: name("t", "t", 13), Columns: []stmt.Column{withDefault(column("k", "k", 16, stmt.TypeInt, true, false), stmt.Null{}, 30, 34)}}},
	{name: "a default with a + sign", src: "create table t (k int primary key, v int default +1) strict, without rowid", err: "s4ql: not supported: + sign (at offset 49)"},
	{name: "a default of NULL after a + sign", src: "create table t (k int primary key, v int default +null) strict, without rowid", err: "s4ql: not supported: NULL after a sign (at offset 49)"},
	{name: "a default of NULL after a - sign", src: "create table t (k int primary key, v int default -null) strict, without rowid", err: "s4ql: not supported: NULL after a sign (at offset 50)"},
	{name: "a default of TRUE", src: "create table t (k int primary key, v int default true) strict, without rowid", err: "s4ql: not supported: TRUE or FALSE (at offset 49)"},
	{name: "a default of FALSE in upper case", src: "create table t (k int primary key, v int default FALSE) strict, without rowid", err: "s4ql: not supported: TRUE or FALSE (at offset 49)"},
	{name: "a default of TRUE in double quotes", src: "create table t (k int primary key, v int default \"true\") strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 49)"},
	{name: "a default of a hex integer", src: "create table t (k int primary key, v int default 0x10) strict, without rowid", err: "s4ql: not supported: hex literal (at offset 49)"},
	{name: "a default of a negative hex integer", src: "create table t (k int primary key, v int default -0x10) strict, without rowid", err: "s4ql: not supported: hex literal (at offset 50)"},
	{name: "a default with a digit separator", src: "create table t (k int primary key, v int default 1_000) strict, without rowid", err: "s4ql: not supported: number with a digit separator (at offset 49)"},
	{name: "a default of an integer above 64 bits", src: "create table t (k int primary key, v int default 9223372036854775808) strict, without rowid", err: "s4ql: not supported: integer outside 64 bits (at offset 49)"},
	{name: "a default of an integer below 64 bits", src: "create table t (k int primary key, v int default -9223372036854775809) strict, without rowid", err: "s4ql: not supported: integer outside 64 bits (at offset 50)"},
	{name: "a default of an infinite real", src: "create table t (k int primary key, v real default 1e999) strict, without rowid", err: "s4ql: not supported: infinite real number (at offset 50)"},
	{name: "a default of a negative infinite real", src: "create table t (k int primary key, v real default -1e999) strict, without rowid", err: "s4ql: not supported: infinite real number (at offset 51)"},
	{name: "a default of a string after a sign", src: "create table t (k int primary key, v text default -'a') strict, without rowid", err: "s4ql: not supported: string after a sign (at offset 51)"},
	{name: "a default of a blob after a sign", src: "create table t (k int primary key, v blob default -x'01') strict, without rowid", err: "s4ql: not supported: blob after a sign (at offset 51)"},
	{name: "a default of a double-quoted string", src: "create table t (k int primary key, v text default \"abc\") strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 50)"},
	{name: "a default of a bare word", src: "create table t (k int primary key, v text default abc) strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 50)"},
	{name: "a default of a keyword that is a name", src: "create table t (k int primary key, v text default abort) strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 50)"},
	{name: "a default of a name in brackets", src: "create table t (k int primary key, v text default [abc]) strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 50)"},
	{name: "a default of a name in backticks", src: "create table t (k int primary key, v text default `abc`) strict, without rowid", err: "s4ql: not supported: name after DEFAULT (at offset 50)"},
	{name: "a default in parentheses", src: "create table t (k int primary key, v int default (1)) strict, without rowid", err: "s4ql: not supported: DEFAULT of an expression (at offset 49)"},
	{name: "a default of an expression", src: "create table t (k int primary key, v int default (1+1)) strict, without rowid", err: "s4ql: not supported: DEFAULT of an expression (at offset 49)"},
	{name: "a default of NULL in parentheses", src: "create table t (k int primary key, v int default (null)) strict, without rowid", err: "s4ql: not supported: DEFAULT of an expression (at offset 49)"},
	{name: "a default of a parameter in parentheses", src: "create table t (k int primary key, v int default (?)) strict, without rowid", err: "s4ql: not supported: DEFAULT of an expression (at offset 49)"},
	{name: "a default of current_timestamp", src: "create table t (k int primary key, v text default current_timestamp) strict, without rowid", err: "s4ql: not supported: current date or time (at offset 50)"},
	{name: "a default of current_date", src: "create table t (k int primary key, v text default CURRENT_DATE) strict, without rowid", err: "s4ql: not supported: current date or time (at offset 50)"},
	{name: "a default of current_time after a sign", src: "create table t (k int primary key, v text default -current_time) strict, without rowid", err: "s4ql: not supported: current date or time (at offset 51)"},
	{name: "a second default", src: "create table t (k int primary key, v int default 1 default 2) strict, without rowid", err: "s4ql: not supported: second DEFAULT (at offset 51)"},
	{name: "a second default of the same value", src: "create table t (k int primary key, v int default 1 not null default 1) strict, without rowid", err: "s4ql: not supported: second DEFAULT (at offset 60)"},
	{name: "a second default after NULL", src: "create table t (k int primary key, v text default null default 'a') strict, without rowid", err: "s4ql: not supported: second DEFAULT (at offset 55)"},
	{name: "ON CONFLICT before a second default", src: "create table t (k int primary key, v int default 1 not null on conflict abort default 2) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 60)"},
	{name: "a default with a collation", src: "create table t (k int primary key, v text default 'a' collate binary) strict, without rowid", err: "s4ql: not supported: COLLATE (at offset 54)"},
	{name: "a default and NULL", src: "create table t (k int primary key, v int default null null) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 54)"},
	{name: "a default and UNIQUE", src: "create table t (k int primary key, v int default 1 unique) strict, without rowid", err: "s4ql: not supported: UNIQUE (at offset 51)"},
	{name: "a default and CHECK", src: "create table t (k int primary key, v int not null default 1 check (v > 0)) strict, without rowid", err: "s4ql: not supported: CHECK (at offset 60)"},
	{name: "an integer default of a REAL column", src: "create table t (k int primary key, v real default 1) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a negative integer default of a REAL column", src: "create table t (k int primary key, v real default -1) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "an integer default of a TEXT column", src: "create table t (k int primary key, v text default 1) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "an integer default of a BLOB column", src: "create table t (k int primary key, v blob not null default 0) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 59)"},
	{name: "a real default of an INT column", src: "create table t (k int primary key, v int default 1.0) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 49)"},
	{name: "a real default of an INTEGER column", src: "create table t (k int primary key, v integer default -1.5) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 53)"},
	{name: "a real default of a TEXT column", src: "create table t (k int primary key, v text default 1.5) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a real default of a BLOB column", src: "create table t (k int primary key, v blob default 0.0) strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a text default of an INT column", src: "create table t (k int primary key, v int default '1') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 49)"},
	{name: "a text default of a REAL column", src: "create table t (k int primary key, v real default '1.5') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a text default of a BLOB column", src: "create table t (k int primary key, v blob default '') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a blob default of an INT column", src: "create table t (k int primary key, v int default x'01') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 49)"},
	{name: "a blob default of a REAL column", src: "create table t (k int primary key, v real default X'') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a blob default of a TEXT column", src: "create table t (k int primary key, v text default x'31') strict, without rowid", err: "s4ql: not supported: DEFAULT of another type than its column (at offset 50)"},
	{name: "a default without a value", src: "create table t (k int primary key, v int default) strict, without rowid", err: "near \")\": syntax error"},
	{name: "a default of two signs", src: "create table t (k int primary key, v int default - -1) strict, without rowid", err: "near \"-\": syntax error"},
	{name: "a default of + and -", src: "create table t (k int primary key, v int default +-1) strict, without rowid", err: "near \"-\": syntax error"},
	{name: "a default of ~", src: "create table t (k int primary key, v int default ~1) strict, without rowid", err: "near \"~\": syntax error"},
	{name: "a default of NOT NULL", src: "create table t (k int primary key, v int default not null) strict, without rowid", err: "near \"not\": syntax error"},
	{name: "a default of a parameter after a sign", src: "create table t (k int primary key, v int default -?) strict, without rowid", err: "near \"?\": syntax error"},
	{name: "a default of a sign before parentheses", src: "create table t (k int primary key, v int default -(1)) strict, without rowid", err: "near \"(\": syntax error"},
	{name: "a default of empty parentheses", src: "create table t (k int primary key, v int default ()) strict, without rowid", err: "near \")\": syntax error"},
	{name: "a default of two strings", src: "create table t (k int primary key, v text default 'x' 'y') strict, without rowid", err: "near \"'y'\": syntax error"},
	{name: "a default of a keyword", src: "create table t (k int primary key, v int default on) strict, without rowid", err: "near \"on\": syntax error"},
	{name: "a default of primary key", src: "create table t (k int primary key, v int default primary key) strict, without rowid", err: "near \"primary\": syntax error"},
	{name: "a default of a real without exponent digits", src: "create table t (k int primary key, v real default 1.5e) strict, without rowid", err: "unrecognized token: \"1.5e\""},
	{name: "a default of a blob of odd digits", src: "create table t (k int primary key, v blob default x'0') strict, without rowid", err: "unrecognized token: \"x'0'\""},
	{name: "an expression", src: "insert into t (v) values (1 + 2)", err: "s4ql: not supported: binary operator (at offset 26)"},
	{name: "a double-quoted string", src: `insert into t (v) values ("1")`, err: "s4ql: not supported: column reference (at offset 26)"},
	{name: "a function", src: "insert into t (v) values (abs(1))", err: "s4ql: not supported: function call (at offset 26)"},

	// SELECT.
	{name: "table.*", src: "select t.* from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "* twice", src: "select *, * from t", err: "s4ql: not supported: * and another result column (at offset 7)"},
	{name: "distinct and a column", src: "select distinct v from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "no from and a semicolon", src: "select * ;", err: "s4ql: not supported: SELECT without FROM (at offset 9)"},
	{name: "a schema before a reserved name", src: "select * from main.sqlite_schema", err: "s4ql: not supported: schema name (at offset 14)"},
	{name: "a schema before a name in brackets", src: "select * from main.[t]", err: "s4ql: not supported: schema name (at offset 14)"},
	{name: "distinct", src: "select distinct * from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "all", src: "select all * from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "no from", src: "select *", err: "s4ql: not supported: SELECT without FROM (at offset 8)"},
	{name: "no from but a where", src: "select * where 1", err: "s4ql: not supported: WHERE clause (at offset 9)"},
	{name: "where", src: "select * from t where v = 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 27}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Integer(1), 26, 27))}}},
	{name: "order by", src: "select * from t order by v",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 26}, Table: name("t", "t", 14), OrderBy: []stmt.OrderColumn{oc(name("v", "v", 25), false)}}},
	{name: "limit", src: "select * from t limit 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 23}, Table: name("t", "t", 14), Limit: lit(stmt.Integer(1), 22, 23)}},
	{name: "a join", src: "select * from t, u", err: "s4ql: not supported: join (at offset 15)"},
	{name: "an alias with as", src: "select * from t as u", err: "s4ql: not supported: alias (at offset 16)"},
	{name: "an alias", src: "select * from t u", err: "s4ql: not supported: alias (at offset 16)"},
	{name: "indexed by", src: "select * from t indexed by i", err: "s4ql: not supported: INDEXED BY (at offset 16)"},
	{name: "not indexed", src: "select * from t not indexed", err: "s4ql: not supported: NOT INDEXED (at offset 16)"},
	{name: "a table in parentheses", src: "select * from (t)", err: "s4ql: not supported: join in parentheses (at offset 14)"},
	{name: "a subquery", src: "select * from (select 1)", err: "s4ql: not supported: subquery in FROM (at offset 14)"},
	{name: "a table-valued function", src: "select * from json_each('[]')", err: "s4ql: not supported: table-valued function (at offset 14)"},
	{name: "a compound", src: "select * from t union select * from t", err: "s4ql: not supported: compound SELECT (at offset 16)"},
	{name: "values", src: "values (1)", err: "s4ql: not supported: VALUES (at offset 0)"},
	{name: "with", src: "with c as (select 1) select * from c", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "on after the table", src: "select * from t on 1", err: "s4ql: not supported: ON clause (at offset 16)"},

	// DROP.
	{name: "drop view", src: "drop view t", err: "s4ql: not supported: DROP VIEW (at offset 5)"},
	{name: "drop index", src: "DROP Index t", want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 12}, Index: name("t", "t", 11)}},
	{name: "drop trigger", src: "drop trigger t", err: "s4ql: not supported: DROP TRIGGER (at offset 5)"},
	{name: "a schema", src: "drop table temp.t", err: "s4ql: not supported: schema name (at offset 11)"},

	// Slice 5: CREATE TABLE of several columns.
	{name: "create table of three columns", src: "create table t1 (v1 int primary key, v2 text not null, v3 real not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 94}, Table: name("t1", "t1", 13), Columns: []stmt.Column{
			column("v1", "v1", 17, stmt.TypeInt, true, false), column("v2", "v2", 37, stmt.TypeText, false, true), column("v3", "v3", 55, stmt.TypeReal, false, true)}}},
	{name: "a primary key that says NOT NULL before", src: "create table t (v int not null primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 65}, Table: name("t", "t", 13), Columns: []stmt.Column{column("v", "v", 16, stmt.TypeInt, true, true)}}},
	{name: "a primary key that says NOT NULL after", src: "create table t (v int primary key not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 65}, Table: name("t", "t", 13), Columns: []stmt.Column{column("v", "v", 16, stmt.TypeInt, true, true)}}},
	{name: "the primary key last, and the options in the other order", src: "create table t (a text not null, b blob not null, k integer primary key) without rowid, strict",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 94}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column("a", "a", 16, stmt.TypeText, false, true), column("b", "b", 33, stmt.TypeBlob, false, true), column("k", "k", 50, stmt.TypeInteger, true, false)}}},
	{name: "a column named twice in another case, which SQLite refuses", src: "create table t (v int primary key, V text not null) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 73}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column("v", "v", 16, stmt.TypeInt, true, false), column("V", "V", 35, stmt.TypeText, false, true)}}},
	{name: "a column named twice, once in quotes, which SQLite refuses", src: `create table t ("a" int primary key, b int not null, A int not null) strict, without rowid`,
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 90}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column(`"a"`, "a", 16, stmt.TypeInt, true, false), column("b", "b", 37, stmt.TypeInt, false, true), column("A", "A", 53, stmt.TypeInt, false, true)}}},
	manyColumns("the most columns that SQLite allows", 2000, ""),
	manyColumns("more columns than SQLite allows, which it refuses", 2001, ""),
	manyColumns("a 2001st column named as an earlier one, which SQLite refuses for the number", 2001, "c0"),
	manyColumns("a 2000th column named as an earlier one, which SQLite refuses for the name", 2000, "C1"),
	{name: "a column that is not the primary key and not NOT NULL, before the primary key", src: "create table t (a int, b int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 63}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column("a", "a", 16, stmt.TypeInt, false, false), column("b", "b", 23, stmt.TypeInt, true, false)}}},
	{name: "two primary keys", src: "create table t (a int primary key, b int primary key) strict, without rowid", err: "s4ql: not supported: second PRIMARY KEY (at offset 41)"},
	{name: "primary key twice on a column", src: "create table t (a int primary key primary key) strict, without rowid", err: "s4ql: not supported: second PRIMARY KEY (at offset 34)"},
	{name: "a constraint before a second primary key", src: "create table t (a int primary key not null on conflict abort, b int primary key) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 43)"},
	{name: "no primary key in columns that say NOT NULL", src: "create table t (a int not null, b int not null) strict, without rowid", err: "s4ql: not supported: table without PRIMARY KEY (at offset 46)"},
	{name: "NOT NULL twice", src: "create table t (a int not null not null primary key) strict, without rowid", err: "s4ql: not supported: NULL or NOT NULL constraint (at offset 31)"},
	{name: "NULL on the primary key", src: "create table t (a int primary key null) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 34)"},
	{name: "NULL on a column", src: "create table t (a int primary key, b int null) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 41)"},
	{name: "NOT NULL with ON CONFLICT", src: "create table t (a int primary key, b int not null on conflict ignore) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 50)"},
	{name: "NOT NULL with ON CONFLICT in a table without a primary key", src: "create table t (a int, b int not null on conflict abort) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 38)"},
	{name: "ASC before a second primary key on a column", src: "create table t (a int primary key asc primary key) strict, without rowid", err: "s4ql: not supported: ASC or DESC (at offset 34)"},
	{name: "ON CONFLICT before a second primary key on a column", src: "create table t (k int primary key, a int not null on conflict abort primary key) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 50)"},
	{name: "a primary key as a table constraint after the columns", src: "create table t (a int primary key, b int not null, primary key (b)) strict, without rowid", err: "s4ql: not supported: table constraint (at offset 49)"},
	{name: "a unique table constraint", src: "create table t (a int primary key, b int not null, unique (b)) strict, without rowid", err: "s4ql: not supported: table constraint (at offset 49)"},
	{name: "a primary key of two columns", src: "create table t (a int not null, b int not null, primary key (a, b)) strict, without rowid", err: "s4ql: not supported: table constraint (at offset 46)"},
	{name: "a second column without a type", src: "create table t (a int primary key, b) strict, without rowid", err: "s4ql: not supported: column without a type (at offset 36)"},

	// Slice 5: INSERT of several values.
	{name: "insert without a column list", src: "insert into t1 values (1, 'a', 1.5)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 35}, Table: name("t1", "t1", 12), Values: []stmt.Term{
			lit(stmt.Integer(1), 23, 24), lit(stmt.Text("a"), 26, 29), lit(stmt.Real(1.5), 31, 34)}}},
	{name: "insert of a list of columns in another order", src: "insert into t1 (v3, v1, v2) values (2.5, 2, 'b')",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 48}, Table: name("t1", "t1", 12), Columns: []stmt.Name{name("v3", "v3", 16), name("v1", "v1", 20), name("v2", "v2", 24)},
			Values: []stmt.Term{lit(stmt.Real(2.5), 36, 39), lit(stmt.Integer(2), 41, 42), lit(stmt.Text("b"), 44, 47)}}},
	{name: "insert of three parameters", src: "insert into t1 (v1, v2, v3) values (?, ?, ?)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 44}, Table: name("t1", "t1", 12), Columns: []stmt.Name{name("v1", "v1", 16), name("v2", "v2", 20), name("v3", "v3", 24)},
			Values: []stmt.Term{param(1, 36), param(2, 39), param(3, 42)}}},
	{name: "insert of a literal and a parameter", src: "insert into t (v, w) values (-1, ?)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15), name("w", "w", 18)},
			Values: []stmt.Term{lit(stmt.Integer(-1), 29, 31), param(1, 33)}}},
	{name: "insert of more values than columns, which SQLite refuses", src: "insert into t (v) values (1, 2)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)},
			Values: []stmt.Term{lit(stmt.Integer(1), 26, 27), lit(stmt.Integer(2), 29, 30)}}},
	{name: "insert of fewer values than columns, which SQLite refuses", src: "insert into t (v, w) values (1)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15), name("w", "w", 18)},
			Values: []stmt.Term{lit(stmt.Integer(1), 29, 30)}}},
	{name: "insert of a parameter without a column list", src: "insert into t values (?)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 24}, Table: name("t", "t", 12), Values: []stmt.Term{param(1, 22)}}},
	{name: "a column named twice in a list", src: "insert into t1 (v1, v2, v2) values (1, 'a', 'b')", err: "s4ql: not supported: column named twice (at offset 24)"},
	{name: "a column named twice in another case", src: "insert into t (a, A) values (1, 2)", err: "s4ql: not supported: column named twice (at offset 18)"},
	{name: "a column named twice, once in quotes", src: `insert into t (a, "a") values (1, 2)`, err: "s4ql: not supported: column named twice (at offset 18)"},
	{name: "two rows without a column list", src: "insert into t values (1), (2)", err: "s4ql: not supported: second row (at offset 24)"},
	{name: "null as a second value", src: "insert into t values (1, null)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 12), Values: []stmt.Term{lit(stmt.Integer(1), 22, 23), lit(stmt.Null{}, 25, 29)}}},
	{name: "a numbered parameter after a plain one", src: "insert into t (a, b) values (?, ?1)", err: "s4ql: not supported: numbered parameter (at offset 32)"},
	{name: "a second column in brackets", src: "insert into t (a, [b]) values (1, 2)", err: "s4ql: not supported: name in brackets (at offset 18)"},
	{name: "a second column in single quotes", src: "insert into t (a, '') values (1, 2)", err: "s4ql: not supported: single-quoted name (at offset 18)"},

	// Slice 5: SELECT of columns.
	{name: "select of a column", src: "select v from t",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 15}, Table: name("t", "t", 14), Columns: []stmt.Name{name("v", "v", 7)}}},
	{name: "select of columns in any order, one twice", src: "select v2, v1, v2 from t1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 25}, Table: name("t1", "t1", 23), Columns: []stmt.Name{name("v2", "v2", 7), name("v1", "v1", 11), name("v2", "v2", 15)}}},
	{name: "select of a column in quotes and in two cases", src: `select "a b", A, a from t`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 25}, Table: name("t", "t", 24), Columns: []stmt.Name{name(`"a b"`, "a b", 7), name("A", "A", 14), name("a", "a", 17)}}},
	{name: "select of a column named by a keyword", src: "select abort from t",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 19}, Table: name("t", "t", 18), Columns: []stmt.Name{name("abort", "abort", 7)}}},
	{name: "select of a column named true in quotes", src: `select "true" from t`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 20}, Table: name("t", "t", 19), Columns: []stmt.Name{name(`"true"`, "true", 7)}}},
	{name: "a column and *", src: "select v, * from t", err: "s4ql: not supported: * and another result column (at offset 10)"},
	{name: "* and a column", src: "select *, v from t", err: "s4ql: not supported: * and another result column (at offset 7)"},
	{name: "DISTINCT before * and a column", src: "select distinct *, v from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "DISTINCT before TRUE", src: "select distinct true from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "DISTINCT before a literal", src: "select distinct 1 from t", err: "s4ql: not supported: DISTINCT or ALL (at offset 7)"},
	{name: "an alias of a column with as", src: "select v as a from t", err: "s4ql: not supported: alias (at offset 9)"},
	{name: "an alias of a column", src: "select v a from t", err: "s4ql: not supported: alias (at offset 9)"},
	{name: "a column with its table", src: "select t.v from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "a column with its schema and table", src: "select main.t.v from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "an expression of a column", src: "select v + 1 from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "a string in the select list", src: "select 'v' from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "true in the select list", src: "select true from t", err: "s4ql: not supported: TRUE or FALSE (at offset 7)"},
	{name: "false after a column", src: "select v, FALSE from t", err: "s4ql: not supported: TRUE or FALSE (at offset 10)"},
	{name: "a column in brackets", src: "select [v] from t", err: "s4ql: not supported: name in brackets (at offset 7)"},
	{name: "a column in backticks", src: "select `v` from t", err: "s4ql: not supported: name in backticks (at offset 7)"},
	{name: "an empty column name in select", src: `select "" from t`, err: "s4ql: not supported: empty name (at offset 7)"},
	{name: "a comma before from", src: "select v, from t", err: `near "from": syntax error`},
	{name: "a column without from", src: "select v", err: "s4ql: not supported: SELECT without FROM (at offset 8)"},
	{name: "a column and where", src: "select v from t where v = ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 27}, Table: name("t", "t", 14), Columns: []stmt.Name{name("v", "v", 7)}, Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, param(1, 26))}}},

	// Slice 6: NULL.
	{name: "a column that takes NULL, as the roadmap writes it", src: "create table t1 (v1 int primary key, v2 text) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 67}, Table: name("t1", "t1", 13), Columns: []stmt.Column{
			column("v1", "v1", 17, stmt.TypeInt, true, false), column("v2", "v2", 37, stmt.TypeText, false, false)}}},
	{name: "columns of every type that take NULL, around the primary key", src: "create table t (a text, b blob, c real, k int primary key, d integer) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 91}, Table: name("t", "t", 13), Columns: []stmt.Column{
			column("a", "a", 16, stmt.TypeText, false, false), column("b", "b", 24, stmt.TypeBlob, false, false), column("c", "c", 32, stmt.TypeReal, false, false),
			column("k", "k", 40, stmt.TypeInt, true, false), column("d", "d", 59, stmt.TypeInteger, false, false)}}},
	{name: "NULL after NOT NULL", src: "create table t (a int primary key, b int not null null) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 50)"},
	{name: "NULL before NOT NULL", src: "create table t (a int primary key, b int null not null) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 41)"},
	{name: "NULL with ON CONFLICT", src: "create table t (a int primary key, b int null on conflict fail) strict, without rowid", err: "s4ql: not supported: NULL constraint (at offset 41)"},
	{name: "NULL after ASC", src: "create table t (a int primary key asc null) strict, without rowid", err: "s4ql: not supported: ASC or DESC (at offset 34)"},
	{name: "NULL after ON CONFLICT", src: "create table t (a int primary key on conflict abort null) strict, without rowid", err: "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
	{name: "a column without a type that takes NULL", src: "create table t (a int primary key, b) strict, without rowid", err: "s4ql: not supported: column without a type (at offset 36)"},
	{name: "no primary key in columns that take NULL", src: "create table t (a int, b text) strict, without rowid", err: "s4ql: not supported: table without PRIMARY KEY (at offset 29)"},
	{name: "null in upper case", src: "insert into t1 values (1, NULL)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 31}, Table: name("t1", "t1", 12), Values: []stmt.Term{lit(stmt.Integer(1), 23, 24), lit(stmt.Null{}, 26, 30)}}},
	{name: "null in mixed case after a parameter", src: "insert into t (a, b) values (?, nULl)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 37}, Table: name("t", "t", 12), Columns: []stmt.Name{name("a", "a", 15), name("b", "b", 18)},
			Values: []stmt.Term{param(1, 29), lit(stmt.Null{}, 32, 36)}}},
	{name: "null and a comment", src: "insert into t (v) values (/* x */ null -- y\n)",
		want: &stmt.Insert{Span: stmt.Span{Start: 0, End: 45}, Table: name("t", "t", 12), Columns: []stmt.Name{name("v", "v", 15)}, Values: []stmt.Term{lit(stmt.Null{}, 34, 38)}}},
	{name: "null after a plus", src: "insert into t (v) values (+null)", err: "s4ql: not supported: NULL after a sign (at offset 26)"},
	{name: "null after a minus and a comment", src: "insert into t (v) values (- /* x */ NULL)", err: "s4ql: not supported: NULL after a sign (at offset 36)"},
	{name: "null after two signs", src: "insert into t (v) values (- -null)", err: "s4ql: not supported: second sign (at offset 28)"},
	{name: "null after a plus and a minus", src: "insert into t (v) values (+-null)", err: "s4ql: not supported: + sign (at offset 26)"},
	{name: "null after a bitwise not", src: "insert into t (v) values (~null)", err: "s4ql: not supported: ~ operator (at offset 26)"},
	{name: "not null as a value", src: "insert into t (v) values (not null)", err: "s4ql: not supported: NOT operator (at offset 26)"},
	{name: "null in double quotes", src: `insert into t (v) values ("null")`, err: "s4ql: not supported: column reference (at offset 26)"},
	{name: "null in parentheses", src: "insert into t (v) values ((null))", err: "s4ql: not supported: parentheses (at offset 26)"},
	{name: "null with a collation", src: "insert into t (v) values (null collate binary)", err: "s4ql: not supported: COLLATE (at offset 26)"},
	{name: "is null", src: "insert into t (v) values (null is null)", err: "s4ql: not supported: IS (at offset 26)"},
	{name: "is not null", src: "insert into t (v) values (1 is not null)", err: "s4ql: not supported: IS (at offset 26)"},
	{name: "isnull", src: "insert into t (v) values (null isnull)", err: "s4ql: not supported: NULL test (at offset 26)"},
	{name: "notnull", src: "insert into t (v) values (1 notnull)", err: "s4ql: not supported: NULL test (at offset 26)"},
	{name: "not null after a value", src: "insert into t (v) values (1 not null)", err: "s4ql: not supported: NULL test (at offset 26)"},
	{name: "a cast of null", src: "insert into t (v) values (cast(null as text))", err: "s4ql: not supported: CAST (at offset 26)"},
	{name: "nullif", src: "insert into t (v) values (nullif(1, 1))", err: "s4ql: not supported: function call (at offset 26)"},
	{name: "null and a second row", src: "insert into t (v) values (null), (null)", err: "s4ql: not supported: second row (at offset 31)"},
	{name: "null in a select", src: "insert into t (v) select null", err: "s4ql: not supported: INSERT with SELECT (at offset 18)"},
	{name: "null as a result column", src: "select null from t", err: "s4ql: not supported: result column other than a column or * (at offset 7)"},
	{name: "null after a column in the select list", src: "select v, NULL from t", err: "s4ql: not supported: result column other than a column or * (at offset 10)"},
	{name: "is null in where", src: "select * from t where v is null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpIsNull, stmt.Term{})}}},
	{name: "is not null in where", src: "select v from t where v is not null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 14), Columns: []stmt.Name{name("v", "v", 7)}, Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpIsNotNull, stmt.Term{})}}},
	{name: "null in limit", src: "select * from t limit null", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "null in an upsert", src: "insert into t (v) values (1) on conflict (v) do update set v = null", err: "s4ql: not supported: ON CONFLICT clause (at offset 29)"},

	// Slice 7: WHERE on SELECT.
	{name: "where, as the roadmap writes it", src: "select * from t1 where v1 = 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 29}, Table: name("t1", "t1", 14), Where: []stmt.Condition{cond(name("v1", "v1", 23), stmt.OpEqual, lit(stmt.Integer(1), 28, 29))}}},
	{name: "three conditions, as the roadmap writes them", src: "select v2 from t1 where v1 >= 2 and v1 < 10 and v2 is not null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 62}, Table: name("t1", "t1", 15), Columns: []stmt.Name{name("v2", "v2", 7)}, Where: []stmt.Condition{
			cond(name("v1", "v1", 24), stmt.OpGreaterOrEqual, lit(stmt.Integer(2), 30, 31)), cond(name("v1", "v1", 36), stmt.OpLess, lit(stmt.Integer(10), 41, 43)),
			cond(name("v2", "v2", 48), stmt.OpIsNotNull, stmt.Term{})}}},
	{name: "a parameter, as the roadmap writes it", src: "select * from t1 where v2 = ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 29}, Table: name("t1", "t1", 14), Where: []stmt.Condition{cond(name("v2", "v2", 23), stmt.OpEqual, param(1, 28))}}},
	{name: "the other operators, and values of every type", src: "select * from t where a == 'x' and b <> x'00' and c != -1.5 and d <= - 7 and e > .5",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 83}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("a", "a", 22), stmt.OpEqual, lit(stmt.Text("x"), 27, 30)), cond(name("b", "b", 35), stmt.OpNotEqual, lit(stmt.Blob{0}, 40, 45)),
			cond(name("c", "c", 50), stmt.OpNotEqual, lit(stmt.Real(-1.5), 55, 59)), cond(name("d", "d", 64), stmt.OpLessOrEqual, lit(stmt.Integer(-7), 69, 72)),
			cond(name("e", "e", 77), stmt.OpGreater, lit(stmt.Real(0.5), 81, 83))}}},
	{name: "parameters around a test for NULL, numbered in the order of the text", src: "select * from t where a = ? and b < ? and c is null and d = ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 61}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("a", "a", 22), stmt.OpEqual, param(1, 26)), cond(name("b", "b", 32), stmt.OpLess, param(2, 36)),
			cond(name("c", "c", 42), stmt.OpIsNull, stmt.Term{}), cond(name("d", "d", 56), stmt.OpEqual, param(3, 60))}}},
	{name: "keywords in upper case and a semicolon", src: "SELECT * FROM t WHERE v IS NOT NULL AND w == X'01';",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 51}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("v", "v", 22), stmt.OpIsNotNull, stmt.Term{}), cond(name("w", "w", 40), stmt.OpEqual, lit(stmt.Blob{1}, 45, 50))}}},
	{name: "a comparison with null, and is null", src: "select * from t where v = null and w is null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 44}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Null{}, 26, 30)), cond(name("w", "w", 35), stmt.OpIsNull, stmt.Term{})}}},
	{name: "a column named by a keyword", src: "select * from t where abort = 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("abort", "abort", 22), stmt.OpEqual, lit(stmt.Integer(1), 30, 31))}}},
	{name: "a column in double quotes twice", src: `select * from t where "a b" = 'x' and "a b" > 'a'`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 49}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name(`"a b"`, "a b", 22), stmt.OpEqual, lit(stmt.Text("x"), 30, 33)), cond(name(`"a b"`, "a b", 38), stmt.OpGreater, lit(stmt.Text("a"), 46, 49))}}},
	{name: "a column three times, once in another case", src: "select * from t where v > 1 and v < 5 and V <> 3",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 48}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("v", "v", 22), stmt.OpGreater, lit(stmt.Integer(1), 26, 27)), cond(name("v", "v", 32), stmt.OpLess, lit(stmt.Integer(5), 36, 37)),
			cond(name("V", "V", 42), stmt.OpNotEqual, lit(stmt.Integer(3), 47, 48))}}},
	{name: "comments in every gap of a where", src: "select/*a*/*/*b*/from/*c*/t/*d*/where/*e*/v/*f*/=/*g*/1/*h*/and--i\nw/*j*/is/*k*/not/*l*/null/*m*/;",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 98}, Table: name("t", "t", 26), Where: []stmt.Condition{
			cond(name("v", "v", 42), stmt.OpEqual, lit(stmt.Integer(1), 54, 55)), cond(name("w", "w", 67), stmt.OpIsNotNull, stmt.Term{})}}},
	{name: "the limits of 64 bits", src: "select * from t where v = 9223372036854775807 and v = -9223372036854775808",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 74}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Integer(9223372036854775807), 26, 45)), cond(name("v", "v", 50), stmt.OpEqual, lit(stmt.Integer(-9223372036854775808), 54, 74))}}},
	{name: "a real below the smallest", src: "select * from t where v = 1e-400",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 32}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Real(0), 26, 32))}}},
	{name: "a column named true in quotes", src: `select * from t where "true" = 1`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 32}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name(`"true"`, "true", 22), stmt.OpEqual, lit(stmt.Integer(1), 31, 32))}}},
	// SQLite goes on after a test for NULL of a column that the table does not have, and checkWhere
	// checks which name its error gives.
	{name: "tests for NULL of one column before a comparison", src: "select * from t where v is null and V is not null and v = 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 59}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("v", "v", 22), stmt.OpIsNull, stmt.Term{}), cond(name("V", "V", 36), stmt.OpIsNotNull, stmt.Term{}),
			cond(name("v", "v", 54), stmt.OpEqual, lit(stmt.Integer(1), 58, 59))}}},
	{name: "tests for NULL of four columns around a comparison", src: "select a from t where b is null and c is null and d = 1 and e is null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 69}, Table: name("t", "t", 14), Columns: []stmt.Name{name("a", "a", 7)}, Where: []stmt.Condition{
			cond(name("b", "b", 22), stmt.OpIsNull, stmt.Term{}), cond(name("c", "c", 36), stmt.OpIsNull, stmt.Term{}),
			cond(name("d", "d", 50), stmt.OpEqual, lit(stmt.Integer(1), 54, 55)), cond(name("e", "e", 60), stmt.OpIsNull, stmt.Term{})}}},
	{name: "a value and AND without a gap", src: "select * from t where v=1and w<-1", err: `unrecognized token: "1and"`},
	// Only a sign makes a value of a literal: ~ is an operator of its own.
	{name: "a bitwise not of a number on the left", src: "select * from t where ~1 < v", err: "s4ql: not supported: unary operator (at offset 22)"},
	{name: "a column on the right", src: "select * from t where v = w", err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "a column in double quotes on the right", src: `select * from t where v = "a"`, err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "a column with its table on the right", src: "select * from t where v = t.w", err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "a column with its table in where", src: "select * from t where t.v = 1", err: "s4ql: not supported: column with the name of a table (at offset 22)"},
	{name: "a column with its schema and table in where", src: "select * from t where main.t.v = 1", err: "s4ql: not supported: column with the name of a table (at offset 22)"},
	{name: "or", src: "select * from t where v = 1 or w = 2", err: "s4ql: not supported: OR (at offset 28)"},
	{name: "or after and", src: "select * from t where v = 1 and w = 2 or x = 3", err: "s4ql: not supported: OR (at offset 38)"},
	{name: "or before and", src: "select * from t where v = 1 or w = 2 and x = 3", err: "s4ql: not supported: OR (at offset 28)"},
	{name: "or twice", src: "select * from t where v = 1 or w = 2 or x = 3", err: "s4ql: not supported: OR (at offset 28)"},
	{name: "a function before or", src: "select * from t where lower(v) = 'a' or w = 2", err: "s4ql: not supported: function call (at offset 22)"},
	{name: "a function after and before or", src: "select * from t where v = 1 and lower(v) = 'a' or w = 2", err: "s4ql: not supported: function call (at offset 32)"},
	{name: "not", src: "select * from t where not v = 1", err: "s4ql: not supported: NOT operator (at offset 22)"},
	{name: "not after and", src: "select * from t where v = 1 and not w = 2", err: "s4ql: not supported: NOT operator (at offset 32)"},
	{name: "a condition in parentheses", src: "select * from t where (v = 1)", err: "s4ql: not supported: parentheses (at offset 22)"},
	{name: "a value in parentheses", src: "select * from t where v = (1)", err: "s4ql: not supported: parentheses (at offset 26)"},
	{name: "a second condition in parentheses", src: "select * from t where v = 1 and (w = 2)", err: "s4ql: not supported: parentheses (at offset 32)"},
	{name: "an expression on the left", src: "select * from t where v + 1 = 2", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "an expression on the right", src: "select * from t where v = 1 + 1", err: "s4ql: not supported: binary operator (at offset 26)"},
	{name: "a column after a sign", src: "select * from t where -v = 1", err: "s4ql: not supported: unary operator (at offset 22)"},
	{name: "a function on the left", src: "select * from t where lower(v) = 'a'", err: "s4ql: not supported: function call (at offset 22)"},
	{name: "a function on the right", src: "select * from t where v = abs(1)", err: "s4ql: not supported: function call (at offset 26)"},
	{name: "in", src: "select * from t where v in (1, 2)", err: "s4ql: not supported: IN (at offset 22)"},
	{name: "not in", src: "select * from t where v not in (1)", err: "s4ql: not supported: IN (at offset 22)"},
	{name: "between", src: "select * from t where v between 1 and 2", err: "s4ql: not supported: BETWEEN (at offset 22)"},
	{name: "like", src: "select * from t where v like 'a'", err: "s4ql: not supported: LIKE, GLOB, REGEXP, or MATCH (at offset 22)"},
	{name: "not like", src: "select * from t where v not like 'a'", err: "s4ql: not supported: LIKE, GLOB, REGEXP, or MATCH (at offset 22)"},
	{name: "glob", src: "select * from t where v glob 'a'", err: "s4ql: not supported: LIKE, GLOB, REGEXP, or MATCH (at offset 22)"},
	{name: "is and a number", src: "select * from t where v is 1", err: "s4ql: not supported: IS with a value other than NULL (at offset 27)"},
	{name: "is true", src: "select * from t where v is true", err: "s4ql: not supported: IS with a value other than NULL (at offset 27)"},
	{name: "is not and a number", src: "select * from t where v is not 1", err: "s4ql: not supported: IS with a value other than NULL (at offset 31)"},
	{name: "is and a parameter", src: "select * from t where v is ?", err: "s4ql: not supported: IS with a value other than NULL (at offset 27)"},
	{name: "is and a column", src: "select * from t where v is w", err: "s4ql: not supported: IS with a value other than NULL (at offset 27)"},
	{name: "is and a negative number", src: "select * from t where v is -1", err: "s4ql: not supported: IS with a value other than NULL (at offset 27)"},
	{name: "is and null in parentheses", src: "select * from t where v is (null)", err: "s4ql: not supported: parentheses (at offset 27)"},
	{name: "is null with a collation", src: "select * from t where v is null collate binary", err: "s4ql: not supported: COLLATE (at offset 27)"},
	{name: "is distinct from", src: "select * from t where v is distinct from null", err: "s4ql: not supported: IS DISTINCT FROM (at offset 27)"},
	{name: "is not distinct from", src: "select * from t where v is not distinct from 1", err: "s4ql: not supported: IS DISTINCT FROM (at offset 31)"},
	{name: "isnull", src: "select * from t where v isnull", err: "s4ql: not supported: NULL test (at offset 22)"},
	{name: "notnull", src: "select * from t where v notnull", err: "s4ql: not supported: NULL test (at offset 22)"},
	{name: "not null after a column", src: "select * from t where v not null", err: "s4ql: not supported: NULL test (at offset 22)"},
	{name: "a collation on the left", src: "select * from t where v collate nocase = 'a'", err: "s4ql: not supported: COLLATE (at offset 22)"},
	{name: "a collation on the right", src: "select * from t where v = 'a' collate nocase", err: "s4ql: not supported: COLLATE (at offset 26)"},
	{name: "a column alone", src: "select * from t where v", err: "s4ql: not supported: column reference (at offset 22)"},
	{name: "a literal alone", src: "select * from t where 1", err: "s4ql: not supported: literal (at offset 22)"},
	{name: "a parameter alone", src: "select * from t where ?", err: "s4ql: not supported: parameter (at offset 22)"},
	{name: "a column after a sign alone", src: "select * from t where -v", err: "s4ql: not supported: unary operator (at offset 22)"},
	{name: "a bitwise not of a column alone", src: "select * from t where ~v", err: "s4ql: not supported: unary operator (at offset 22)"},
	{name: "true alone", src: "select * from t where true", err: "s4ql: not supported: TRUE or FALSE (at offset 22)"},
	{name: "true on the left", src: "select * from t where true = v", err: "s4ql: not supported: TRUE or FALSE (at offset 22)"},
	{name: "true on the right", src: "select * from t where v = true", err: "s4ql: not supported: TRUE or FALSE (at offset 26)"},
	// With the name of a table, true and false are names of columns, as in SQLite.
	{name: "a column named true with its table alone", src: "select * from t where t.true", err: "s4ql: not supported: column reference (at offset 22)"},
	{name: "a column named false with its table on the right", src: "select * from t where v = t.false", err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "false after and", src: "select * from t where v = 1 and false", err: "s4ql: not supported: TRUE or FALSE (at offset 32)"},
	{name: "a literal after and", src: "select * from t where v = 1 and 1", err: "s4ql: not supported: literal (at offset 32)"},
	{name: "a column after and", src: "select * from t where v = 1 and w", err: "s4ql: not supported: column reference (at offset 32)"},
	{name: "order by after where", src: "select * from t where v = 1 order by v",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 38}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Integer(1), 26, 27))},
			OrderBy: []stmt.OrderColumn{oc(name("v", "v", 37), false)}}},
	{name: "limit after where", src: "select * from t where v = 1 limit 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 14), Where: []stmt.Condition{cond(name("v", "v", 22), stmt.OpEqual, lit(stmt.Integer(1), 26, 27))},
			Limit: lit(stmt.Integer(1), 34, 35)}},
	{name: "group by after where", src: "select * from t where v = 1 group by v", err: "s4ql: not supported: GROUP BY clause (at offset 28)"},
	{name: "a window after where", src: "select * from t where v = 1 window w as ()", err: "s4ql: not supported: WINDOW clause (at offset 28)"},
	{name: "a compound after where", src: "select * from t where v = 1 union select * from t", err: "s4ql: not supported: compound SELECT (at offset 28)"},
	{name: "a plus sign in where", src: "select * from t where v = +1", err: "s4ql: not supported: + sign (at offset 26)"},
	{name: "a numbered parameter in where", src: "select * from t where v = ?1", err: "s4ql: not supported: numbered parameter (at offset 26)"},
	{name: "a named parameter in where", src: "select * from t where v = :a", err: "s4ql: not supported: named parameter (at offset 26)"},
	{name: "a parameter after a sign in where", src: "select * from t where v = -?", err: "s4ql: not supported: parameter after a sign (at offset 27)"},
	{name: "two signs in where", src: "select * from t where v = - -1", err: "s4ql: not supported: second sign (at offset 28)"},
	{name: "a hex literal in where", src: "select * from t where v = 0x10", err: "s4ql: not supported: hex literal (at offset 26)"},
	{name: "an infinite real in where", src: "select * from t where v = 1e999", err: "s4ql: not supported: infinite real number (at offset 26)"},
	{name: "an integer outside 64 bits in where", src: "select * from t where v = 9223372036854775808", err: "s4ql: not supported: integer outside 64 bits (at offset 26)"},
	{name: "a string after a sign in where", src: "select * from t where v = -'a'", err: "s4ql: not supported: string after a sign (at offset 27)"},
	{name: "a blob after a sign in where", src: "select * from t where v = -x'01'", err: "s4ql: not supported: blob after a sign (at offset 27)"},
	{name: "null after a minus in where", src: "select * from t where v = -null", err: "s4ql: not supported: NULL after a sign (at offset 27)"},
	{name: "null after a plus in where", src: "select * from t where v = +null", err: "s4ql: not supported: NULL after a sign (at offset 26)"},
	{name: "a bitwise not in where", src: "select * from t where v = ~1", err: "s4ql: not supported: ~ operator (at offset 26)"},
	{name: "not as a value in where", src: "select * from t where v = not 1", err: "s4ql: not supported: NOT operator (at offset 26)"},
	{name: "the current time in where", src: "select * from t where v = current_time", err: "s4ql: not supported: current date or time (at offset 26)"},
	{name: "two comparisons in a row", src: "select * from t where v = 1 = 1", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "is null and a comparison", src: "select * from t where v is null = 1", err: "s4ql: not supported: IS (at offset 22)"},
	{name: "a comparison and is not null", src: "select * from t where v = 1 is not null", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "a JSON operator", src: "select * from t where v -> 'a' = 1", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "a column in brackets in where", src: "select * from t where [v] = 1", err: "s4ql: not supported: name in brackets (at offset 22)"},
	{name: "a column in backticks in where", src: "select * from t where `v` = 1", err: "s4ql: not supported: name in backticks (at offset 22)"},
	{name: "an empty name in where", src: `select * from t where "" = 1`, err: "s4ql: not supported: empty name (at offset 22)"},
	{name: "a subquery in where", src: "select * from t where v = (select 1)", err: "s4ql: not supported: subquery (at offset 26)"},
	{name: "exists", src: "select * from t where exists (select 1)", err: "s4ql: not supported: EXISTS (at offset 22)"},
	{name: "case in where", src: "select * from t where v = case when 1 then 1 end", err: "s4ql: not supported: CASE (at offset 26)"},
	{name: "a cast in where", src: "select * from t where v = cast(1 as int)", err: "s4ql: not supported: CAST (at offset 26)"},
	{name: "an in with a parameter after and", src: "select * from t where v = 1 and w in (select v from u where v = ?)", err: "s4ql: not supported: IN (at offset 32)"},
	{name: "a comma between conditions", src: "select * from t where v = 1, w = 2", err: `near ",": syntax error`},
	{name: "and at the end", src: "select * from t where v = 1 and", err: "incomplete input"},
	{name: "two operators", src: "select * from t where v == == 1", err: `near "==": syntax error`},
	{name: "a join before where", src: "select * from t, u where v = 1", err: "s4ql: not supported: join (at offset 15)"},
	{name: "an alias before where", src: "select * from t as u where u.v = 1", err: "s4ql: not supported: alias (at offset 16)"},
	{name: "indexed by before where", src: "select * from t indexed by i where v = 1", err: "s4ql: not supported: INDEXED BY (at offset 16)"},
	{name: "a where in a subquery in FROM", src: "select * from (select * from u where v = 1)", err: "s4ql: not supported: subquery in FROM (at offset 14)"},
	{name: "a where in an insert", src: "insert into t select * from u where v = 1", err: "s4ql: not supported: INSERT with SELECT (at offset 14)"},
	{name: "a where in create table as", src: "create table t as select * from u where v = 1", err: "s4ql: not supported: CREATE TABLE AS (at offset 15)"},

	// Slice 8: UPDATE and DELETE.
	{name: "update, as the roadmap writes it", src: "update t1 set v2 = 'b', v3 = 2.5 where v1 = 1",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 45}, Table: name("t1", "t1", 7),
			Set:   []stmt.Assignment{set(name("v2", "v2", 14), lit(stmt.Text("b"), 19, 22)), set(name("v3", "v3", 24), lit(stmt.Real(2.5), 29, 32))},
			Where: []stmt.Condition{cond(name("v1", "v1", 39), stmt.OpEqual, lit(stmt.Integer(1), 44, 45))}}},
	{name: "update of null without where, as the roadmap writes it", src: "update t1 set v2 = null",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 23}, Table: name("t1", "t1", 7), Set: []stmt.Assignment{set(name("v2", "v2", 14), lit(stmt.Null{}, 19, 23))}}},
	{name: "delete, as the roadmap writes it", src: "delete from t1 where v1 >= 10",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 29}, Table: name("t1", "t1", 12), Where: []stmt.Condition{cond(name("v1", "v1", 21), stmt.OpGreaterOrEqual, lit(stmt.Integer(10), 27, 29))}}},
	{name: "delete without where, as the roadmap writes it", src: "delete from t1",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 14}, Table: name("t1", "t1", 12)}},
	{name: "update with parameters, as the roadmap writes it", src: "update t1 set v2 = ? where v1 = ?",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 33}, Table: name("t1", "t1", 7), Set: []stmt.Assignment{set(name("v2", "v2", 14), param(1, 19))},
			Where: []stmt.Condition{cond(name("v1", "v1", 27), stmt.OpEqual, param(2, 32))}}},
	{name: "parameters of an update, numbered in the order of the text, those of set first", src: "update t set a = ?, b = 1, c = ? where d = ? and e is null and f < ?",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 68}, Table: name("t", "t", 7),
			Set: []stmt.Assignment{set(name("a", "a", 13), param(1, 17)), set(name("b", "b", 20), lit(stmt.Integer(1), 24, 25)), set(name("c", "c", 27), param(2, 31))},
			Where: []stmt.Condition{cond(name("d", "d", 39), stmt.OpEqual, param(3, 43)), cond(name("e", "e", 49), stmt.OpIsNull, stmt.Term{}),
				cond(name("f", "f", 63), stmt.OpLess, param(4, 67))}}},
	{name: "update with keywords in upper case and a semicolon", src: "UPDATE T SET V = X'01' WHERE V IS NOT NULL;",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 43}, Table: name("T", "T", 7), Set: []stmt.Assignment{set(name("V", "V", 13), lit(stmt.Blob{1}, 17, 22))},
			Where: []stmt.Condition{cond(name("V", "V", 29), stmt.OpIsNotNull, stmt.Term{})}}},
	{name: "delete with parameters", src: "delete from t where a = ? and b <> ?",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 36}, Table: name("t", "t", 12),
			Where: []stmt.Condition{cond(name("a", "a", 20), stmt.OpEqual, param(1, 24)), cond(name("b", "b", 30), stmt.OpNotEqual, param(2, 35))}}},
	{name: "update with literals of every form", src: "update t set a = -1, b = - 7, c = .5e1, d = 9223372036854775807, e = -9223372036854775808, f = ''''",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 99}, Table: name("t", "t", 7), Set: []stmt.Assignment{
			set(name("a", "a", 13), lit(stmt.Integer(-1), 17, 19)), set(name("b", "b", 21), lit(stmt.Integer(-7), 25, 28)),
			set(name("c", "c", 30), lit(stmt.Real(5), 34, 38)), set(name("d", "d", 40), lit(stmt.Integer(math.MaxInt64), 44, 63)),
			set(name("e", "e", 65), lit(stmt.Integer(math.MinInt64), 69, 89)), set(name("f", "f", 91), lit(stmt.Text("'"), 95, 99))}}},
	{name: "update with comments in every gap", src: "/*a*/update/*b*/t/*c*/set/*d*/v/*e*/=/*f*/1/*g*/,--h\nw=2/*i*/where/*j*/v=1/*k*/;--l",
		want: &stmt.Update{Span: stmt.Span{Start: 5, End: 80}, Table: name("t", "t", 16),
			Set:   []stmt.Assignment{set(name("v", "v", 30), lit(stmt.Integer(1), 42, 43)), set(name("w", "w", 53), lit(stmt.Integer(2), 55, 56))},
			Where: []stmt.Condition{cond(name("v", "v", 71), stmt.OpEqual, lit(stmt.Integer(1), 73, 74))}}},
	{name: "delete with comments in every gap", src: "delete/**/from/**/t/**/where/**/v/**/is/**/null--x",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 47}, Table: name("t", "t", 18), Where: []stmt.Condition{cond(name("v", "v", 32), stmt.OpIsNull, stmt.Term{})}}},
	{name: "update of a table and columns named by keywords", src: "update abort set key = 1 where abort = 1",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 40}, Table: name("abort", "abort", 7), Set: []stmt.Assignment{set(name("key", "key", 17), lit(stmt.Integer(1), 23, 24))},
			Where: []stmt.Condition{cond(name("abort", "abort", 31), stmt.OpEqual, lit(stmt.Integer(1), 39, 40))}}},
	{name: "delete of a table named by a keyword", src: "delete from abort where key = 1",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 31}, Table: name("abort", "abort", 12), Where: []stmt.Condition{cond(name("key", "key", 24), stmt.OpEqual, lit(stmt.Integer(1), 30, 31))}}},
	{name: "update with names in double quotes", src: `update "a b" set "x""y" = 1 where "x""y" = 2`,
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 44}, Table: name(`"a b"`, "a b", 7), Set: []stmt.Assignment{set(name(`"x""y"`, `x"y`, 17), lit(stmt.Integer(1), 26, 27))},
			Where: []stmt.Condition{cond(name(`"x""y"`, `x"y`, 34), stmt.OpEqual, lit(stmt.Integer(2), 43, 44))}}},
	{name: "update of the column of its where", src: "update t set v = 1 where v = 2",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 30}, Table: name("t", "t", 7), Set: []stmt.Assignment{set(name("v", "v", 13), lit(stmt.Integer(1), 17, 18))},
			Where: []stmt.Condition{cond(name("v", "v", 25), stmt.OpEqual, lit(stmt.Integer(2), 29, 30))}}},
	{name: "update of columns named rowid, oid, and _rowid_", src: "update t set rowid = 1, oid = 2, _rowid_ = 3",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 44}, Table: name("t", "t", 7), Set: []stmt.Assignment{
			set(name("rowid", "rowid", 13), lit(stmt.Integer(1), 21, 22)), set(name("oid", "oid", 24), lit(stmt.Integer(2), 30, 31)),
			set(name("_rowid_", "_rowid_", 33), lit(stmt.Integer(3), 43, 44))}}},
	{name: "update of a column named true", src: "update t set true = 1",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 21}, Table: name("t", "t", 7), Set: []stmt.Assignment{set(name("true", "true", 13), lit(stmt.Integer(1), 20, 21))}}},
	{name: "update after empty statements", src: ";; update t set v = 1;",
		want: &stmt.Update{Span: stmt.Span{Start: 3, End: 22}, Table: name("t", "t", 10), Set: []stmt.Assignment{set(name("v", "v", 16), lit(stmt.Integer(1), 20, 21))}}},
	{name: "delete with a comparison with null", src: "delete from t where v = null and w is not null",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 46}, Table: name("t", "t", 12),
			Where: []stmt.Condition{cond(name("v", "v", 20), stmt.OpEqual, lit(stmt.Null{}, 24, 28)), cond(name("w", "w", 33), stmt.OpIsNotNull, stmt.Term{})}}},
	{name: "an assignment with ==, which SQLite reads as =", src: "update t set v == 1",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 19}, Table: name("t", "t", 7), Set: []stmt.Assignment{set(name("v", "v", 13), lit(stmt.Integer(1), 18, 19))}}},
	{name: "an assignment with <>", src: "update t set v <> 1", err: `near "<>": syntax error`},
	{name: "an assignment of default", src: "update t set v = default", err: `near "default": syntax error`},
	{name: "a column twice in set", src: "update t set v = 1, v = 2", err: "s4ql: not supported: column named twice (at offset 20)"},
	{name: "a column twice in set, in another case", src: "update t set v = 1, w = 2, V = 3", err: "s4ql: not supported: column named twice (at offset 27)"},
	{name: "a column twice in set, once in double quotes", src: `update t set "v" = 1, v = 2`, err: "s4ql: not supported: column named twice (at offset 22)"},
	{name: "an expression in set", src: "update t set v = v + 1", err: "s4ql: not supported: binary operator (at offset 17)"},
	{name: "a column as the value in set", src: "update t set v = w", err: "s4ql: not supported: column reference (at offset 17)"},
	{name: "a name in double quotes as the value in set", src: `update t set v = "a"`, err: "s4ql: not supported: column reference (at offset 17)"},
	{name: "a value in parentheses in set", src: "update t set v = (1)", err: "s4ql: not supported: parentheses (at offset 17)"},
	{name: "a function in set", src: "update t set v = abs(1)", err: "s4ql: not supported: function call (at offset 17)"},
	{name: "a subquery in set", src: "update t set v = (select 1)", err: "s4ql: not supported: subquery (at offset 17)"},
	{name: "a plus sign in set", src: "update t set v = +1", err: "s4ql: not supported: + sign (at offset 17)"},
	{name: "a numbered parameter in set", src: "update t set v = ?1", err: "s4ql: not supported: numbered parameter (at offset 17)"},
	{name: "a named parameter in set", src: "update t set v = :a", err: "s4ql: not supported: named parameter (at offset 17)"},
	{name: "a parameter after a sign in set", src: "update t set v = -?", err: "s4ql: not supported: parameter after a sign (at offset 18)"},
	{name: "true in set", src: "update t set v = true", err: "s4ql: not supported: TRUE or FALSE (at offset 17)"},
	{name: "a hex literal in set", src: "update t set v = 0x10", err: "s4ql: not supported: hex literal (at offset 17)"},
	{name: "an infinite real in set", src: "update t set v = -1e999", err: "s4ql: not supported: infinite real number (at offset 18)"},
	{name: "the current time in set", src: "update t set v = current_time", err: "s4ql: not supported: current date or time (at offset 17)"},
	{name: "null after a sign in set", src: "update t set v = -null", err: "s4ql: not supported: NULL after a sign (at offset 18)"},
	{name: "returning after set", src: "update t set v = 1 returning *", err: "s4ql: not supported: RETURNING clause (at offset 19)"},
	{name: "returning after the where of an update", src: "update t set v = 1 where v = 1 returning v", err: "s4ql: not supported: RETURNING clause (at offset 31)"},
	{name: "returning after a delete", src: "delete from t returning *", err: "s4ql: not supported: RETURNING clause (at offset 14)"},
	{name: "returning after the where of a delete", src: "delete from t where v = 1 returning *", err: "s4ql: not supported: RETURNING clause (at offset 26)"},
	{name: "update from", src: "update t set v = 1 from u", err: "s4ql: not supported: FROM clause (at offset 19)"},
	{name: "update from with a column of the other table", src: "update t set v = u.v from u where t.w = u.w", err: "s4ql: not supported: column reference (at offset 17)"},
	{name: "update from with a literal and where", src: "update t set v = 1 from u where w = 1", err: "s4ql: not supported: FROM clause (at offset 19)"},
	{name: "set of a list of columns", src: "update t set (v, w) = (1, 2)", err: "s4ql: not supported: SET of a list of columns (at offset 13)"},
	{name: "set of a list of one column", src: "update t set (v) = 1", err: "s4ql: not supported: SET of a list of columns (at offset 13)"},
	{name: "set of a list of columns after a column", src: "update t set v = 1, (w, x) = (2, 3)", err: "s4ql: not supported: SET of a list of columns (at offset 20)"},
	{name: "update or replace", src: "update or replace t set v = 1", err: "s4ql: not supported: UPDATE OR (at offset 7)"},
	{name: "update or ignore", src: "update or ignore t set v = 1", err: "s4ql: not supported: UPDATE OR (at offset 7)"},
	{name: "update or rollback", src: "update or rollback t set v = 1", err: "s4ql: not supported: UPDATE OR (at offset 7)"},
	{name: "update or abort", src: "update or abort t set v = 1", err: "s4ql: not supported: UPDATE OR (at offset 7)"},
	{name: "update or fail", src: "update or fail t set v = 1", err: "s4ql: not supported: UPDATE OR (at offset 7)"},
	{name: "an alias in an update", src: "update t as u set v = 1", err: "s4ql: not supported: alias (at offset 9)"},
	{name: "an alias in a delete", src: "delete from t as u where u.v = 1", err: "s4ql: not supported: alias (at offset 14)"},
	{name: "indexed by in an update", src: "update t indexed by i set v = 1", err: "s4ql: not supported: INDEXED BY (at offset 9)"},
	{name: "not indexed in an update", src: "update t not indexed set v = 1", err: "s4ql: not supported: NOT INDEXED (at offset 9)"},
	{name: "indexed by in a delete", src: "delete from t indexed by i where v = 1", err: "s4ql: not supported: INDEXED BY (at offset 14)"},
	{name: "not indexed in a delete", src: "delete from t not indexed where v = 1", err: "s4ql: not supported: NOT INDEXED (at offset 14)"},
	{name: "with before an update", src: "with x as (select 1) update t set v = 1", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "with before a delete", src: "with x as (select 1) delete from t", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "explain update", src: "explain update t set v = 1", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain delete", src: "explain query plan delete from t", err: "s4ql: not supported: EXPLAIN QUERY PLAN (at offset 0)"},
	{name: "a schema in an update", src: "update main.t set v = 1", err: "s4ql: not supported: schema name (at offset 7)"},
	{name: "a schema in a delete", src: "delete from main.t", err: "s4ql: not supported: schema name (at offset 12)"},
	{name: "the temp schema in a delete", src: "delete from temp.t where v = 1", err: "s4ql: not supported: schema name (at offset 12)"},
	{name: "a column in brackets in set", src: "update t set [v] = 1", err: "s4ql: not supported: name in brackets (at offset 13)"},
	{name: "a column in backticks in set", src: "update t set `v` = 1", err: "s4ql: not supported: name in backticks (at offset 13)"},
	{name: "a single-quoted column in set", src: "update t set 'v' = 1", err: "s4ql: not supported: single-quoted name (at offset 13)"},
	{name: "an empty column name in set", src: `update t set "" = 1`, err: "s4ql: not supported: empty name (at offset 13)"},
	{name: "an empty table name in an update", src: `update "" set v = 1`, err: "s4ql: not supported: empty name (at offset 7)"},
	{name: "a table in brackets in a delete", src: "delete from [t]", err: "s4ql: not supported: name in brackets (at offset 12)"},
	{name: "a reserved table in an update", src: "update sqlite_schema set v = 1", err: "s4ql: not supported: reserved table name (at offset 7)"},
	{name: "a reserved table in a delete", src: "delete from S4QL_x", err: "s4ql: not supported: reserved table name (at offset 12)"},
	{name: "a virtual table of SQLite in a delete", src: "delete from json_each", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 12)"},
	{name: "or in the where of an update", src: "update t set v = 1 where v = 1 or w = 2", err: "s4ql: not supported: OR (at offset 31)"},
	{name: "in in the where of an update", src: "update t set v = 1 where v = 1 and w in (1)", err: "s4ql: not supported: IN (at offset 35)"},
	{name: "a column with its table in the where of a delete", src: "delete from t where t.v = 1", err: "s4ql: not supported: column with the name of a table (at offset 20)"},
	{name: "a numbered parameter in the where of an update", src: "update t set v = ? where w = ?2", err: "s4ql: not supported: numbered parameter (at offset 29)"},
	{name: "a second statement after an update", src: "update t set v = 1; select 1", err: "s4ql: not supported: second statement (at offset 20)"},
	{name: "a second statement after a delete", src: "delete from t;delete from t", err: "s4ql: not supported: second statement (at offset 14)"},
	{name: "limit on an update", src: "update t set v = 1 limit 1", err: `near "limit": syntax error`},
	{name: "order by on an update", src: "update t set v = 1 where v = 1 order by v", err: `near "order": syntax error`},
	{name: "limit on a delete", src: "delete from t limit 1", err: `near "limit": syntax error`},
	{name: "order by and limit on a delete", src: "delete from t where v = 1 order by v limit 1", err: `near "order": syntax error`},
	{name: "update with where and nothing after it", src: "update t set v = 1 where", err: "incomplete input"},
	{name: "update with where and a semicolon after it", src: "update t set v = 1 where;", err: `near ";": syntax error`},
	{name: "delete with where and nothing after it", src: "delete from t where", err: "incomplete input"},
	{name: "delete with and at the end", src: "delete from t where v = 1 and", err: "incomplete input"},
	{name: "update without set", src: "update t", err: "incomplete input"},
	{name: "update with set and no assignment", src: "update t set;", err: `near ";": syntax error`},
	{name: "update with an assignment without a value", src: "update t set v = ;", err: `near ";": syntax error`},
	{name: "update with a comma at the end of set", src: "update t set v = 1, where v = 1", err: `near "where": syntax error`},
	{name: "update of a column with its table", src: "update t set t.v = 1", err: `near ".": syntax error`},
	{name: "delete without from", src: "delete t", err: `near "t": syntax error`},
	{name: "delete of two tables", src: "delete from t, u", err: `near ",": syntax error`},

	// Slice 9: CREATE INDEX and DROP INDEX.
	{name: "create index, as the roadmap writes it", src: "create index i on t (a, b desc)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 31}, Index: name("i", "i", 13), Table: name("t", "t", 18),
			Columns: []stmt.IndexColumn{ixc(name("a", "a", 21), false), ixc(name("b", "b", 24), true)}}},
	{name: "drop index, as the roadmap writes it", src: "drop index i",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 12}, Index: name("i", "i", 11)}},
	{name: "create index with asc, desc, and neither, in upper case, and a semicolon", src: "CREATE INDEX I ON T (A ASC, B DESC, C);",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 39}, Index: name("I", "I", 13), Table: name("T", "T", 18),
			Columns: []stmt.IndexColumn{ixc(name("A", "A", 21), false), ixc(name("B", "B", 28), true), ixc(name("C", "C", 36), false)}}},
	{name: "create index with comments in every gap", src: "/*a*/create/*b*/index/*c*/i/*d*/on/*e*/t/*f*/(/*g*/a/*h*/desc/*i*/,/*j*/b/*k*/)/*l*/;--m",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 5, End: 85}, Index: name("i", "i", 26), Table: name("t", "t", 39),
			Columns: []stmt.IndexColumn{ixc(name("a", "a", 51), true), ixc(name("b", "b", 72), false)}}},
	{name: "create index of an index, a table, and a column named by keywords", src: "create index abort on action (key)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 34}, Index: name("abort", "abort", 13), Table: name("action", "action", 22),
			Columns: []stmt.IndexColumn{ixc(name("key", "key", 30), false)}}},
	{name: "create index with names in double quotes", src: `create index "i""x" on "a b" ("x""y" desc)`,
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 42}, Index: name(`"i""x"`, `i"x`, 13), Table: name(`"a b"`, "a b", 23),
			Columns: []stmt.IndexColumn{ixc(name(`"x""y"`, `x"y`, 30), true)}}},
	{name: "create index of the name of its table, which SQLite refuses for the database", src: "create index t on t (a)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 23}, Index: name("t", "t", 13), Table: name("t", "t", 18),
			Columns: []stmt.IndexColumn{ixc(name("a", "a", 21), false)}}},
	{name: "create index of columns named rowid, oid, and _rowid_", src: "create index i on t (rowid, oid, _rowid_)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 41}, Index: name("i", "i", 13), Table: name("t", "t", 18),
			Columns: []stmt.IndexColumn{ixc(name("rowid", "rowid", 21), false), ixc(name("oid", "oid", 28), false), ixc(name("_rowid_", "_rowid_", 33), false)}}},
	{name: "create index of the name of a virtual table of SQLite", src: "create index json_each on t (a)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 31}, Index: name("json_each", "json_each", 13), Table: name("t", "t", 26),
			Columns: []stmt.IndexColumn{ixc(name("a", "a", 29), false)}}},
	{name: "drop index of a name in double quotes after empty statements", src: ` ;; drop index "x""y";`,
		want: &stmt.DropIndex{Span: stmt.Span{Start: 4, End: 22}, Index: name(`"x""y"`, `x"y`, 15)}},
	{name: "drop index of a keyword with comments", src: "drop/**/index/*c*/abort",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 23}, Index: name("abort", "abort", 18)}},
	{name: "drop index of the name of a virtual table of SQLite", src: "drop index pragma_table_list",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 28}, Index: name("pragma_table_list", "pragma_table_list", 11)}},
	{name: "create unique index", src: "create unique index i on t (a)", err: "s4ql: not supported: UNIQUE INDEX (at offset 7)"},
	{name: "create index of an index in main", src: "create index main.i on t (a)", err: "s4ql: not supported: schema name (at offset 13)"},
	{name: "create index of an index in temp, which SQLite refuses for a table that is not TEMP", src: "create index temp.i on t (a)", err: "s4ql: not supported: schema name (at offset 13)"},
	{name: "a partial index", src: "create index i on t (a) where a > 0", err: "s4ql: not supported: partial index (at offset 24)"},
	{name: "an index on a sum", src: "create index i on t (a + 1)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on a function", src: "create index i on t (lower(a))", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on a negated column", src: "create index i on t (-a)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on null", src: "create index i on t (null)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on a number", src: "create index i on t (1)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on a parameter", src: "create index i on t (?)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on the current time", src: "create index i on t (current_time)", err: "s4ql: not supported: index on an expression (at offset 21)"},
	{name: "an index on a column in parentheses, which SQLite reads as the column", src: "create index i on t ((a))", err: "s4ql: not supported: parentheses (at offset 21)"},
	{name: "an index with a collation", src: "create index i on t (a collate nocase)", err: "s4ql: not supported: COLLATE (at offset 21)"},
	{name: "an index on a single-quoted name, which SQLite reads as a name", src: "create index i on t ('a')", err: "s4ql: not supported: single-quoted name (at offset 21)"},
	{name: "an index on a name in brackets", src: "create index i on t ([a])", err: "s4ql: not supported: name in brackets (at offset 21)"},
	{name: "an index on a name in backticks", src: "create index i on t (`a`)", err: "s4ql: not supported: name in backticks (at offset 21)"},
	{name: "an index on an empty name", src: `create index i on t ("")`, err: "s4ql: not supported: empty name (at offset 21)"},
	{name: "an index of an empty name", src: `create index "" on t (a)`, err: "s4ql: not supported: empty name (at offset 13)"},
	{name: "an index on a column with its table", src: "create index i on t (t.a)", err: "s4ql: not supported: column with the name of a table (at offset 21)"},
	{name: "an index on a column with its schema and table", src: "create index i on t (main.t.a)", err: "s4ql: not supported: column with the name of a table (at offset 21)"},
	{name: "an index on true", src: "create index i on t (true)", err: "s4ql: not supported: TRUE or FALSE (at offset 21)"},
	{name: "an index on false after a column", src: "create index i on t (a, false)", err: "s4ql: not supported: TRUE or FALSE (at offset 24)"},
	{name: "an index on a column twice", src: "create index i on t (a, a)", err: "s4ql: not supported: column named twice (at offset 24)"},
	{name: "an index on a column twice, in another case and order", src: "create index i on t (a, b, A desc)", err: "s4ql: not supported: column named twice (at offset 27)"},
	{name: "an index on a column twice, once in double quotes", src: `create index i on t ("a", a)`, err: "s4ql: not supported: column named twice (at offset 26)"},
	{name: "an index on a column twice, once named by a keyword", src: `create index i on t (text desc, "Text")`, err: "s4ql: not supported: column named twice (at offset 32)"},
	{name: "an index with nulls first", src: "create index i on t (a nulls first)", err: "s4ql: not supported: NULLS FIRST or NULLS LAST (at offset 23)"},
	{name: "an index with desc and nulls last", src: "create index i on t (a desc nulls last)", err: "s4ql: not supported: NULLS FIRST or NULLS LAST (at offset 28)"},
	{name: "an index of a name that starts with sqlite_", src: "create index sqlite_i on t (a)", err: "s4ql: not supported: reserved index name (at offset 13)"},
	{name: "an index of a name that starts with s4ql_", src: "create index S4QL_i on t (a)", err: "s4ql: not supported: reserved index name (at offset 13)"},
	{name: "an index of the name of the index of a primary key", src: `create index "sqlite_autoindex_t_1" on t (a)`, err: "s4ql: not supported: reserved index name (at offset 13)"},
	{name: "an index on a reserved table", src: "create index i on sqlite_schema (a)", err: "s4ql: not supported: reserved table name (at offset 18)"},
	{name: "an index on a virtual table of SQLite", src: "create index i on json_each (a)", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 18)"},
	{name: "an index on a table in brackets", src: "create index i on [t] (a)", err: "s4ql: not supported: name in brackets (at offset 18)"},
	{name: "an index on a single-quoted table", src: "create index i on 't' (a)", err: "s4ql: not supported: single-quoted name (at offset 18)"},
	{name: "an index of a single-quoted name", src: "create index 'i' on t (a)", err: "s4ql: not supported: single-quoted name (at offset 13)"},
	{name: "drop index of an index in main", src: "drop index main.i", err: "s4ql: not supported: schema name (at offset 11)"},
	{name: "drop index of an index in temp", src: "drop index temp.i", err: "s4ql: not supported: schema name (at offset 11)"},
	{name: "drop index of a schema before a reserved name", src: "drop index main.sqlite_autoindex_t_1", err: "s4ql: not supported: schema name (at offset 11)"},
	{name: "create index of a schema before a reserved name", src: "create index main.sqlite_i on t (a)", err: "s4ql: not supported: schema name (at offset 13)"},
	{name: "drop index of the index of a primary key", src: "drop index sqlite_autoindex_t_1", err: "s4ql: not supported: reserved index name (at offset 11)"},
	{name: "drop index of a name that starts with s4ql_", src: "drop index s4ql_x", err: "s4ql: not supported: reserved index name (at offset 11)"},
	{name: "drop index of an empty name", src: `drop index ""`, err: "s4ql: not supported: empty name (at offset 11)"},
	{name: "drop index of a name in brackets", src: "drop index [i]", err: "s4ql: not supported: name in brackets (at offset 11)"},
	{name: "drop index of a single-quoted name", src: "drop index 'i'", err: "s4ql: not supported: single-quoted name (at offset 11)"},
	{name: "reindex", src: "reindex", err: "s4ql: not supported: REINDEX (at offset 0)"},
	{name: "reindex of an index", src: "reindex i", err: "s4ql: not supported: REINDEX (at offset 0)"},
	{name: "analyze", src: "analyze", err: "s4ql: not supported: ANALYZE (at offset 0)"},
	{name: "analyze of a table", src: "analyze t", err: "s4ql: not supported: ANALYZE (at offset 0)"},
	{name: "explain create index", src: "explain create index i on t (a)", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain query plan create index", src: "explain query plan create index i on t (a)", err: "s4ql: not supported: EXPLAIN QUERY PLAN (at offset 0)"},
	{name: "explain drop index", src: "explain drop index i", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain query plan drop index", src: "explain query plan drop index i", err: "s4ql: not supported: EXPLAIN QUERY PLAN (at offset 0)"},
	{name: "a second statement after create index", src: "create index i on t (a); select 1", err: "s4ql: not supported: second statement (at offset 25)"},
	{name: "with before create index", src: "with c as (select 1) create index i on t (a)", err: `near "create": syntax error`},
	{name: "create index of no columns", src: "create index i on t ()", err: `near ")": syntax error`},
	{name: "create index on a table in main", src: "create index i on main.t (a)", err: `near ".": syntax error`},
	{name: "create temp index", src: "create temp index i on t (a)", err: `near "index": syntax error`},
	{name: "create index without columns", src: "create index i on t", err: "incomplete input"},
	{name: "create index with a comma after the last column", src: "create index i on t (a,)", err: `near ")": syntax error`},
	{name: "create index with desc after the columns", src: "create index i on t (a) desc", err: `near "desc": syntax error`},
	{name: "create index with asc and desc", src: "create index i on t (a asc desc)", err: `near "desc": syntax error`},
	{name: "create index without a name", src: "create index on t (a)", err: `near "on": syntax error`},
	{name: "create index without on", src: "create index i (a)", err: `near "(": syntax error`},
	{name: "create index with where and nothing after it", src: "create index i on t (a) where", err: "incomplete input"},
	{name: "create index of two names without a comma", src: "create index i on t (a b)", err: `near "b": syntax error`},
	{name: "drop index without a name", src: "drop index", err: "incomplete input"},
	{name: "drop index of two indexes", src: "drop index i, j", err: `near ",": syntax error`},
	{name: "drop index on a table", src: "drop index i on t", err: `near "on": syntax error`},

	// Slice 10: IF NOT EXISTS and IF EXISTS.
	{name: "create table if not exists, as the roadmap writes it", src: "create table if not exists t1 (v1 int primary key, v2 text) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 81}, IfNotExists: true, Table: name("t1", "t1", 27),
			Columns: []stmt.Column{column("v1", "v1", 31, stmt.TypeInt, true, false), column("v2", "v2", 51, stmt.TypeText, false, false)}}},
	{name: "drop table if exists, as the roadmap writes it", src: "drop table if exists t1",
		want: &stmt.DropTable{Span: stmt.Span{Start: 0, End: 23}, IfExists: true, Table: name("t1", "t1", 21)}},
	{name: "create index if not exists, as the roadmap writes it", src: "create index if not exists i on t1 (v1)",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 39}, IfNotExists: true, Index: name("i", "i", 27), Table: name("t1", "t1", 32),
			Columns: []stmt.IndexColumn{ixc(name("v1", "v1", 36), false)}}},
	{name: "drop index if exists, as the roadmap writes it", src: "drop index if exists i",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 22}, IfExists: true, Index: name("i", "i", 21)}},
	{name: "create table if not exists in upper case with comments in every gap and a semicolon",
		src: "CREATE/*a*/TABLE/*b*/IF/*c*/NOT--d\nEXISTS\t\"T\"(V INT PRIMARY KEY)WITHOUT ROWID,STRICT;",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 85}, IfNotExists: true, Table: name(`"T"`, "T", 42),
			Columns: []stmt.Column{column("V", "V", 46, stmt.TypeInt, true, false)}}},
	{name: "create table if not exists in mixed case, whose kept text does not have the clause",
		src: "create table If Not Exists t (v int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 70}, IfNotExists: true, Table: name("t", "t", 27),
			Columns: []stmt.Column{column("v", "v", 30, stmt.TypeInt, true, false)},
			SQL:     "CREATE TABLE t (v int primary key) strict, without rowid"}},
	{name: "create index if not exists in upper case with comments, whose kept text does not have the clause",
		src: "CREATE INDEX IF/**/NOT EXISTS/* x */I ON T (A DESC);",
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 52}, IfNotExists: true, Index: name("I", "I", 36), Table: name("T", "T", 41),
			Columns: []stmt.IndexColumn{ixc(name("A", "A", 44), true)}, SQL: "CREATE INDEX I ON T (A DESC)"}},
	{name: "drop table if exists with comments after empty statements", src: " ; DROP TABLE/**/IF--x\nEXISTS \"a b\";",
		want: &stmt.DropTable{Span: stmt.Span{Start: 3, End: 36}, IfExists: true, Table: name(`"a b"`, "a b", 30)}},
	{name: "drop index if exists in mixed case", src: "Drop Index iF eXiStS i",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 22}, IfExists: true, Index: name("i", "i", 21)}},
	{name: "create table if not exists of several columns with a column named twice, which SQLite refuses for a table that does not exist",
		src: "create table if not exists t (a int primary key, A text) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 78}, IfNotExists: true, Table: name("t", "t", 27),
			Columns: []stmt.Column{column("a", "a", 30, stmt.TypeInt, true, false), column("A", "A", 49, stmt.TypeText, false, false)}}},
	{name: "create table if not exists of a table named by keywords", src: "create table if not exists exists_ (not_ int primary key) strict, without rowid",
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 79}, IfNotExists: true, Table: name("exists_", "exists_", 27),
			Columns: []stmt.Column{column("not_", "not_", 36, stmt.TypeInt, true, false)}}},
	{name: "create table if not exists of a table named if in double quotes", src: `create table if not exists "if" (if int primary key) strict, without rowid`,
		want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: 74}, IfNotExists: true, Table: name(`"if"`, "if", 27),
			Columns: []stmt.Column{column("if", "if", 33, stmt.TypeInt, true, false)}}},
	{name: "create index if not exists of an index named if in double quotes on a table and a column named if", src: `create index if not exists "IF" on if (if)`,
		want: &stmt.CreateIndex{Span: stmt.Span{Start: 0, End: 42}, IfNotExists: true, Index: name(`"IF"`, "IF", 27), Table: name("if", "if", 35),
			Columns: []stmt.IndexColumn{ixc(name("if", "if", 39), false)}}},
	{name: "drop table if exists of a table named if", src: "drop table if exists if",
		want: &stmt.DropTable{Span: stmt.Span{Start: 0, End: 23}, IfExists: true, Table: name("if", "if", 21)}},
	{name: "drop index if exists of an index named IF", src: "drop index if exists IF",
		want: &stmt.DropIndex{Span: stmt.Span{Start: 0, End: 23}, IfExists: true, Index: name("IF", "IF", 21)}},
	{name: "create table if not exists of a table named if, which SQLite cannot read again from its schema",
		src: "create table if not exists if (v int primary key) strict, without rowid", err: "s4ql: not supported: bare name IF after IF NOT EXISTS (at offset 27)"},
	{name: "create table if not exists of a table named IF in upper case",
		src: "create table if not exists IF (v int primary key) strict, without rowid", err: "s4ql: not supported: bare name IF after IF NOT EXISTS (at offset 27)"},
	{name: "create index if not exists of an index named if, which SQLite cannot read again from its schema",
		src: "create index if not exists iF on t (v)", err: "s4ql: not supported: bare name IF after IF NOT EXISTS (at offset 27)"},
	{name: "create table if not exists of a table in main", src: "create table if not exists main.t (v int primary key) strict, without rowid", err: "s4ql: not supported: schema name (at offset 27)"},
	{name: "create table if not exists of a table named if in main", src: "create table if not exists main.if (v int primary key) strict, without rowid", err: "s4ql: not supported: schema name (at offset 27)"},
	{name: "create temp table if not exists", src: "create temp table if not exists t (v int primary key) strict, without rowid", err: "s4ql: not supported: TEMP (at offset 7)"},
	{name: "create temporary table if not exists", src: "create temporary table if not exists t (v int primary key) strict, without rowid", err: "s4ql: not supported: TEMP (at offset 7)"},
	{name: "create table if not exists as select", src: "create table if not exists t as select 1", err: "s4ql: not supported: CREATE TABLE AS (at offset 29)"},
	{name: "create table if not exists of a table that is not strict", src: "create table if not exists t (v int primary key) without rowid", err: "s4ql: not supported: table without STRICT (at offset 62)"},
	{name: "create table if not exists with an unknown type", src: "create table if not exists t (v foo primary key) strict, without rowid", err: "s4ql: not supported: column type other than INT, INTEGER, REAL, TEXT, or BLOB (at offset 32)"},
	{name: "create table if not exists with a second primary key", src: "create table if not exists t (v int primary key, w int primary key) strict, without rowid", err: "s4ql: not supported: second PRIMARY KEY (at offset 55)"},
	{name: "create table if not exists of a reserved name", src: "create table if not exists sqlite_x (v int primary key) strict, without rowid", err: "s4ql: not supported: reserved table name (at offset 27)"},
	{name: "create table if not exists of the name of a virtual table of SQLite", src: "create table if not exists json_each (v int primary key) strict, without rowid", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 27)"},
	{name: "drop table if exists of a table in main", src: "drop table if exists main.t", err: "s4ql: not supported: schema name (at offset 21)"},
	{name: "drop table if exists of a table in a database that does not exist, which SQLite accepts", src: "drop table if exists nosuch.t", err: "s4ql: not supported: schema name (at offset 21)"},
	{name: "drop table if exists of sqlite_schema", src: "drop table if exists sqlite_schema", err: "s4ql: not supported: reserved table name (at offset 21)"},
	{name: "drop table if exists of the name of a virtual table of SQLite", src: "drop table if exists pragma_table_list", err: "s4ql: not supported: name of a virtual table of SQLite (at offset 21)"},
	{name: "drop table if exists of an empty name", src: `drop table if exists ""`, err: "s4ql: not supported: empty name (at offset 21)"},
	{name: "drop table if exists of a name in brackets", src: "drop table if exists [t]", err: "s4ql: not supported: name in brackets (at offset 21)"},
	{name: "create unique index if not exists", src: "create unique index if not exists i on t (v)", err: "s4ql: not supported: UNIQUE INDEX (at offset 7)"},
	{name: "create index if not exists of an index in main", src: "create index if not exists main.i on t (v)", err: "s4ql: not supported: schema name (at offset 27)"},
	{name: "create index if not exists of a reserved name", src: "create index if not exists sqlite_autoindex_t_1 on t (v)", err: "s4ql: not supported: reserved index name (at offset 27)"},
	{name: "create index if not exists with a collation", src: "create index if not exists i on t (v collate nocase)", err: "s4ql: not supported: COLLATE (at offset 35)"},
	{name: "create index if not exists, partial", src: "create index if not exists i on t (v) where v > 0", err: "s4ql: not supported: partial index (at offset 38)"},
	{name: "create index if not exists of a column twice", src: "create index if not exists i on t (v, v)", err: "s4ql: not supported: column named twice (at offset 38)"},
	{name: "drop index if exists of an index in main", src: "drop index if exists main.i", err: "s4ql: not supported: schema name (at offset 21)"},
	{name: "drop index if exists of the index of a primary key", src: "drop index if exists sqlite_autoindex_t_1", err: "s4ql: not supported: reserved index name (at offset 21)"},
	{name: "explain create table if not exists", src: "explain create table if not exists t (v int primary key) strict, without rowid", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain query plan drop table if exists", src: "explain query plan drop table if exists t", err: "s4ql: not supported: EXPLAIN QUERY PLAN (at offset 0)"},
	{name: "drop view if exists", src: "drop view if exists v", err: "s4ql: not supported: DROP VIEW (at offset 5)"},
	{name: "drop trigger if exists", src: "drop trigger if exists r", err: "s4ql: not supported: DROP TRIGGER (at offset 5)"},
	{name: "create view if not exists", src: "create view if not exists v as select 1", err: "s4ql: not supported: CREATE VIEW (at offset 0)"},
	{name: "create virtual table if not exists", src: "create virtual table if not exists v using fts5 (a)", err: "s4ql: not supported: CREATE VIRTUAL TABLE (at offset 0)"},
	{name: "create trigger if not exists", src: "create trigger if not exists r after insert on t begin select 1; end", err: "s4ql: not supported: CREATE TRIGGER (at offset 0)"},
	{name: "a second statement after create table if not exists", src: "create table if not exists t (v int primary key) strict, without rowid; drop table if exists t", err: "s4ql: not supported: second statement (at offset 72)"},
	{name: "create if not exists table", src: "create if not exists table t (v int primary key) strict, without rowid", err: `near "if": syntax error`},
	{name: "drop table with if exists after the name", src: "drop table t if exists", err: `near "if": syntax error`},
	{name: "create table if exists", src: "create table if exists t (v int primary key) strict, without rowid", err: `near "exists": syntax error`},
	{name: "drop table if not exists", src: "drop table if not exists t", err: `near "not": syntax error`},
	{name: "create table if not without exists", src: "create table if not t (v int primary key) strict, without rowid", err: `near "t": syntax error`},
	{name: "create table if without not exists", src: "create table if t (v int primary key) strict, without rowid", err: `near "t": syntax error`},
	{name: "create table of a table named if", src: "create table if (v int primary key) strict, without rowid", err: `near "(": syntax error`},
	{name: "create table if not exists twice", src: "create table if not exists if not exists t (v int primary key) strict, without rowid", err: `near "not": syntax error`},
	{name: "create table if not exists of a table named not", src: "create table if not exists not (v int primary key) strict, without rowid", err: `near "not": syntax error`},
	{name: "create table if not exists of a table named exists", src: "create table if not exists exists (v int primary key) strict, without rowid", err: `near "exists": syntax error`},
	{name: "create table if not exists without a name", src: "create table if not exists (v int primary key) strict, without rowid", err: `near "(": syntax error`},
	{name: "create table if not exists at the end", src: "create table if not exists", err: "incomplete input"},
	{name: "create table if at the end", src: "create table if", err: "incomplete input"},
	{name: "drop table if without exists", src: "drop table if t", err: `near "t": syntax error`},
	{name: "drop table of a table named if", src: "drop table if", err: "incomplete input"},
	{name: "drop table if exists twice", src: "drop table if exists if exists t", err: `near "exists": syntax error`},
	{name: "drop table if exists of a table named exists", src: "drop table if exists exists", err: `near "exists": syntax error`},
	{name: "drop table if exists without a name", src: "drop table if exists", err: "incomplete input"},
	{name: "drop table if exists of two tables", src: "drop table if exists t, u", err: `near ",": syntax error`},
	{name: "create if not exists index", src: "create if not exists index i on t (v)", err: `near "if": syntax error`},
	{name: "create index if exists", src: "create index if exists i on t (v)", err: `near "exists": syntax error`},
	{name: "create index if not without exists", src: "create index if not i on t (v)", err: `near "i": syntax error`},
	{name: "create index if without not exists", src: "create index if i on t (v)", err: `near "i": syntax error`},
	{name: "create index of an index named if", src: "create index if on t (v)", err: `near "on": syntax error`},
	{name: "create index if not exists twice", src: "create index if not exists if not exists i on t (v)", err: `near "not": syntax error`},
	{name: "create index if not exists after on", src: "create index i on if not exists t (v)", err: `near "not": syntax error`},
	{name: "drop index if not exists", src: "drop index if not exists i", err: `near "not": syntax error`},
	{name: "drop index if without exists", src: "drop index if i", err: `near "i": syntax error`},
	{name: "drop index with if exists after the name", src: "drop index i if exists", err: `near "if": syntax error`},
	{name: "drop index if exists twice", src: "drop index if exists if exists i", err: `near "exists": syntax error`},
	{name: "drop index if exists of an index named exists", src: "drop index if exists exists", err: `near "exists": syntax error`},

	// Slice 12: a value on the left of a comparison. A condition reads column, operator, value, with
	// the operator swapped where the text writes the value first.
	{name: "a value on the left, as the roadmap writes it", src: "select v1 from t1 where 5 < v1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 30}, Table: name("t1", "t1", 15), Columns: []stmt.Name{name("v1", "v1", 7)}, Where: []stmt.Condition{
			condFirst(name("v1", "v1", 28), stmt.OpGreater, lit(stmt.Integer(5), 24, 25))}}},
	{name: "a parameter on the left, as the roadmap writes it", src: "select v1 from t1 where ? >= v2",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 31}, Table: name("t1", "t1", 15), Columns: []stmt.Name{name("v1", "v1", 7)}, Where: []stmt.Condition{
			condFirst(name("v2", "v2", 29), stmt.OpLessOrEqual, param(1, 24))}}},
	{name: "every operator with the value on the left", src: "select * from t where 1 = a and 2 == b and 3 <> c and 4 != d and 5 < e and 6 <= f and 7 > g and 8 >= h",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 102}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("a", "a", 26), stmt.OpEqual, lit(stmt.Integer(1), 22, 23)), condFirst(name("b", "b", 37), stmt.OpEqual, lit(stmt.Integer(2), 32, 33)),
			condFirst(name("c", "c", 48), stmt.OpNotEqual, lit(stmt.Integer(3), 43, 44)), condFirst(name("d", "d", 59), stmt.OpNotEqual, lit(stmt.Integer(4), 54, 55)),
			condFirst(name("e", "e", 69), stmt.OpGreater, lit(stmt.Integer(5), 65, 66)), condFirst(name("f", "f", 80), stmt.OpGreaterOrEqual, lit(stmt.Integer(6), 75, 76)),
			condFirst(name("g", "g", 90), stmt.OpLess, lit(stmt.Integer(7), 86, 87)), condFirst(name("h", "h", 101), stmt.OpLessOrEqual, lit(stmt.Integer(8), 96, 97))}}},
	{name: "values of every type on the left", src: "select * from t where -1.5 < a and 'x' = b and x'00' <> c and - 7 >= d and .5 > e and null = f",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 94}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("a", "a", 29), stmt.OpGreater, lit(stmt.Real(-1.5), 22, 26)), condFirst(name("b", "b", 41), stmt.OpEqual, lit(stmt.Text("x"), 35, 38)),
			condFirst(name("c", "c", 56), stmt.OpNotEqual, lit(stmt.Blob{0}, 47, 52)), condFirst(name("d", "d", 69), stmt.OpLessOrEqual, lit(stmt.Integer(-7), 62, 65)),
			condFirst(name("e", "e", 80), stmt.OpLess, lit(stmt.Real(0.5), 75, 77)), condFirst(name("f", "f", 93), stmt.OpEqual, lit(stmt.Null{}, 86, 90))}}},
	{name: "the limits of 64 bits on the left", src: "select * from t where -9223372036854775808 < a and 9223372036854775807 >= a",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 75}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("a", "a", 45), stmt.OpGreater, lit(stmt.Integer(-9223372036854775808), 22, 42)),
			condFirst(name("a", "a", 74), stmt.OpLessOrEqual, lit(stmt.Integer(9223372036854775807), 51, 70))}}},
	{name: "parameters on both sides of their columns, numbered in the order of the text", src: "select * from t where a = ? and ? < b and c > ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 47}, Table: name("t", "t", 14), Where: []stmt.Condition{
			cond(name("a", "a", 22), stmt.OpEqual, param(1, 26)), condFirst(name("b", "b", 36), stmt.OpGreater, param(2, 32)),
			cond(name("c", "c", 42), stmt.OpGreater, param(3, 46))}}},
	{name: "the receive of goqite's queue, as far as slice 12 takes it", src: "select * from goqite where queue = ? and ? >= timeout and received < ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 70}, Table: name("goqite", "goqite", 14), Where: []stmt.Condition{
			cond(name("queue", "queue", 27), stmt.OpEqual, param(1, 35)), condFirst(name("timeout", "timeout", 46), stmt.OpLessOrEqual, param(2, 41)),
			cond(name("received", "received", 58), stmt.OpLess, param(3, 69))}}},
	{name: "a value on the left and a test for NULL of its column", src: "select * from t where 5 = a and a is null",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 41}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("a", "a", 26), stmt.OpEqual, lit(stmt.Integer(5), 22, 23)), cond(name("a", "a", 32), stmt.OpIsNull, stmt.Term{})}}},
	{name: "a column in double quotes on the right of a value", src: `select * from t where 'a' < "b c"`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 33}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name(`"b c"`, "b c", 28), stmt.OpGreater, lit(stmt.Text("a"), 22, 25))}}},
	{name: "a column named by a keyword on the right", src: "select * from t where 1 = abort",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 31}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("abort", "abort", 26), stmt.OpEqual, lit(stmt.Integer(1), 22, 23))}}},
	{name: "comments in every gap of a value on the left", src: "select * from t where/*a*/-/*b*/5/*c*/>=/*d*/a/*e*/;",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 52}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("a", "a", 45), stmt.OpLessOrEqual, lit(stmt.Integer(-5), 26, 33))}}},
	{name: "a value on the left in an update, with parameters in set and where", src: "update t set v = ? where ? > w and 1 = x",
		want: &stmt.Update{Span: stmt.Span{Start: 0, End: 40}, Table: name("t", "t", 7), Set: []stmt.Assignment{set(name("v", "v", 13), param(1, 17))}, Where: []stmt.Condition{
			condFirst(name("w", "w", 29), stmt.OpLess, param(2, 25)), condFirst(name("x", "x", 39), stmt.OpEqual, lit(stmt.Integer(1), 35, 36))}}},
	{name: "a value on the left in a delete", src: "delete from t where 1 = v",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 25}, Table: name("t", "t", 12), Where: []stmt.Condition{
			condFirst(name("v", "v", 24), stmt.OpEqual, lit(stmt.Integer(1), 20, 21))}}},
	{name: "both orders in a delete", src: "delete from t where 'a' <= v and v < 'b'",
		want: &stmt.Delete{Span: stmt.Span{Start: 0, End: 40}, Table: name("t", "t", 12), Where: []stmt.Condition{
			condFirst(name("v", "v", 27), stmt.OpGreaterOrEqual, lit(stmt.Text("a"), 20, 23)), cond(name("v", "v", 33), stmt.OpLess, lit(stmt.Text("b"), 37, 40))}}},
	{name: "a value on the left without a gap before and", src: "select * from t where 1=v and 2<w and 3>=x",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 42}, Table: name("t", "t", 14), Where: []stmt.Condition{
			condFirst(name("v", "v", 24), stmt.OpEqual, lit(stmt.Integer(1), 22, 23)), condFirst(name("w", "w", 32), stmt.OpGreater, lit(stmt.Integer(2), 30, 31)),
			condFirst(name("x", "x", 41), stmt.OpLessOrEqual, lit(stmt.Integer(3), 38, 39))}}},
	// The value on the left is read as the value on the right is, with the same names.
	{name: "a positive number on the left", src: "select * from t where +1 < v", err: "s4ql: not supported: + sign (at offset 22)"},
	{name: "a hex integer on the left", src: "select * from t where 0x10 < v", err: "s4ql: not supported: hex literal (at offset 22)"},
	{name: "true on the left", src: "select * from t where true = v", err: "s4ql: not supported: TRUE or FALSE (at offset 22)"},
	{name: "false on the left", src: "select * from t where FALSE < v", err: "s4ql: not supported: TRUE or FALSE (at offset 22)"},
	{name: "a parameter after a sign on the left", src: "select * from t where -? < v", err: "s4ql: not supported: parameter after a sign (at offset 23)"},
	{name: "null after a minus on the left", src: "select * from t where -null = v", err: "s4ql: not supported: NULL after a sign (at offset 23)"},
	{name: "null after a plus on the left", src: "select * from t where +null = v", err: "s4ql: not supported: NULL after a sign (at offset 22)"},
	{name: "two signs on the left", src: "select * from t where - -5 = v", err: "s4ql: not supported: unary operator (at offset 22)"},
	{name: "a string after a sign on the left", src: "select * from t where -'x' = v", err: "s4ql: not supported: string after a sign (at offset 23)"},
	{name: "a blob after a sign on the left", src: "select * from t where - x'00' = v", err: "s4ql: not supported: blob after a sign (at offset 24)"},
	{name: "a number with a digit separator on the left", src: "select * from t where 1_000 = v", err: "s4ql: not supported: number with a digit separator (at offset 22)"},
	{name: "the current time on the left", src: "select * from t where current_time = v", err: "s4ql: not supported: current date or time (at offset 22)"},
	{name: "an infinite real on the left", src: "select * from t where 1e999 = v", err: "s4ql: not supported: infinite real number (at offset 22)"},
	{name: "an integer outside 64 bits on the left", src: "select * from t where 9223372036854775808 > v", err: "s4ql: not supported: integer outside 64 bits (at offset 22)"},
	{name: "a numbered parameter on the left", src: "select * from t where ?1 = v", err: "s4ql: not supported: numbered parameter (at offset 22)"},
	{name: "a named parameter on the left", src: "select * from t where :a = v", err: "s4ql: not supported: named parameter (at offset 22)"},
	{name: "a value on the left in parentheses", src: "select * from t where (5) < v", err: "s4ql: not supported: parentheses (at offset 22)"},
	{name: "a column on the right in parentheses", src: "select * from t where 5 < (v)", err: "s4ql: not supported: parentheses (at offset 26)"},
	{name: "a column with its table on the right of a value", src: "select * from t where 5 < t.v", err: "s4ql: not supported: column with the name of a table (at offset 26)"},
	{name: "a column with its schema and table on the right of a value", src: "select * from t where 5 < main.t.v", err: "s4ql: not supported: column with the name of a table (at offset 26)"},
	{name: "true on the right of a value", src: "select * from t where 5 < true", err: "s4ql: not supported: TRUE or FALSE (at offset 26)"},
	{name: "a column in brackets on the right of a value", src: "select * from t where 5 < [v]", err: "s4ql: not supported: name in brackets (at offset 26)"},
	{name: "a column in backticks on the right of a value", src: "select * from t where 5 < `v`", err: "s4ql: not supported: name in backticks (at offset 26)"},
	{name: "an empty name on the right of a value", src: `select * from t where 5 < ""`, err: "s4ql: not supported: empty name (at offset 26)"},
	{name: "an expression on the right of a value", src: "select * from t where 5 < v + 1", err: "s4ql: not supported: binary operator (at offset 26)"},
	{name: "an expression on the left of a column", src: "select * from t where 5 + 1 < v", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "a function on the right of a value", src: "select * from t where 5 < abs(v)", err: "s4ql: not supported: function call (at offset 26)"},
	{name: "a column after a sign on the right of a value", src: "select * from t where 5 < -v", err: "s4ql: not supported: unary operator (at offset 26)"},
	{name: "two comparisons in a row", src: "select * from t where 5 < v < 10", err: "s4ql: not supported: binary operator (at offset 22)"},
	{name: "a collation on a value on the left", src: "select * from t where 'a' collate nocase < v", err: "s4ql: not supported: COLLATE (at offset 22)"},
	{name: "a collation on a column on the right", src: "select * from t where 'a' < v collate nocase", err: "s4ql: not supported: COLLATE (at offset 28)"},
	{name: "or after a value on the left", src: "select * from t where 5 < v or 1 = w", err: "s4ql: not supported: OR (at offset 28)"},
	{name: "a hex integer in a later condition after a value on the left", src: "select * from t where 5 = v and w = 0x1", err: "s4ql: not supported: hex literal (at offset 36)"},
	// A value on both sides of a comparison, at the value on the right, as a column on both sides is
	// at the column on the right.
	{name: "a number on both sides", src: "select * from t where 1 = 1", err: "s4ql: not supported: value compared with a value (at offset 26)"},
	{name: "a parameter on both sides", src: "select * from t where ? = ?", err: "s4ql: not supported: value compared with a value (at offset 26)"},
	{name: "a number and a parameter", src: "select * from t where 1 < ?", err: "s4ql: not supported: value compared with a value (at offset 26)"},
	{name: "a string and a number", src: "select * from t where 'v' = 1", err: "s4ql: not supported: value compared with a value (at offset 28)"},
	{name: "null on both sides", src: "select * from t where null = null", err: "s4ql: not supported: value compared with a value (at offset 29)"},
	{name: "a negative number on the right of a value", src: "select * from t where 1 >= -1", err: "s4ql: not supported: value compared with a value (at offset 27)"},
	{name: "a string on the right of a value", src: "select * from t where 5 < 'v'", err: "s4ql: not supported: value compared with a value (at offset 26)"},
	{name: "a value on both sides after a condition", src: "select * from t where 5 < v and 1 = 1", err: "s4ql: not supported: value compared with a value (at offset 36)"},
	{name: "a value on both sides in an update", src: "update t set v = 1 where 2 = 2", err: "s4ql: not supported: value compared with a value (at offset 29)"},
	{name: "a value on both sides in a delete", src: "delete from t where 1 = 1", err: "s4ql: not supported: value compared with a value (at offset 24)"},
	// A column on both sides stays not supported.
	{name: "a column on both sides", src: "select * from t where a = b", err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "a column compared with itself", src: "select * from t where a = a", err: "s4ql: not supported: column compared with a column (at offset 26)"},
	{name: "a column in double quotes on both sides", src: `select * from t where "a" < "b"`, err: "s4ql: not supported: column compared with a column (at offset 28)"},
	// IS NULL and IS NOT NULL take only a column on the left.
	{name: "a number is null", src: "select * from t where 5 is null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "null is null", src: "select * from t where null is null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a parameter is null", src: "select * from t where ? is null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a number is not null", src: "select * from t where 5 is not null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a blob is not null", src: "select * from t where x'00' is not null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a negative number is null", src: "select * from t where -1 is null", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a number is a column", src: "select * from t where 5 is v", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a number is distinct from a column", src: "select * from t where 5 is distinct from v", err: "s4ql: not supported: value before IS (at offset 22)"},
	{name: "a number isnull", src: "select * from t where 5 isnull", err: "s4ql: not supported: NULL test (at offset 22)"},
	{name: "a value before IS in an update", src: "update t set v = 1 where 2 is null", err: "s4ql: not supported: value before IS (at offset 25)"},
	{name: "a value before IS in a delete", src: "delete from t where ? is not null", err: "s4ql: not supported: value before IS (at offset 20)"},
	{name: "a value on the left and nothing after its operator", src: "select * from t where 5 <", err: "incomplete input"},
	{name: "a value on the left and a keyword after its operator", src: "select * from t where 5 < select", err: `near "select": syntax error`},
	{name: "an operator without a left side", src: "select * from t where < v", err: `near "<": syntax error`},

	// Slice 13: ORDER BY and LIMIT on a select.
	{name: "the first select of the roadmap's slice 13", src: "select v1, v2 from t1 order by v2 desc, v1 limit 10",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 51}, Table: name("t1", "t1", 19), Columns: []stmt.Name{name("v1", "v1", 7), name("v2", "v2", 11)},
			OrderBy: []stmt.OrderColumn{oc(name("v2", "v2", 31), true), oc(name("v1", "v1", 40), false)}, Limit: lit(stmt.Integer(10), 49, 51)}},
	{name: "the second select of the roadmap's slice 13, its ? of LIMIT after that of WHERE", src: "select v1 from t1 where v2 > ? order by v1 limit ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 50}, Table: name("t1", "t1", 15), Columns: []stmt.Name{name("v1", "v1", 7)},
			Where: []stmt.Condition{cond(name("v2", "v2", 24), stmt.OpGreater, param(1, 29))}, OrderBy: []stmt.OrderColumn{oc(name("v1", "v1", 40), false)}, Limit: param(2, 49)}},
	{name: "the receive of goqite's queue", src: "select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 111}, Table: name("goqite", "goqite", 15), Columns: []stmt.Name{name("id", "id", 7)}, Where: []stmt.Condition{
			cond(name("queue", "queue", 28), stmt.OpEqual, param(1, 36)), condFirst(name("timeout", "timeout", 47), stmt.OpLessOrEqual, param(2, 42)),
			cond(name("received", "received", 59), stmt.OpLess, param(3, 70))},
			OrderBy: []stmt.OrderColumn{oc(name("priority", "priority", 81), true), oc(name("created", "created", 96), false)}, Limit: lit(stmt.Integer(1), 110, 111)}},
	{name: "order by and limit in upper case with ASC, a name in double quotes, leading zeros, and comments in every gap",
		src: `SELECT * FROM t ORDER/*a*/BY/*b*/"a b"/*c*/ASC/*d*/,/*e*/v/*f*/DESC/*g*/LIMIT/*h*/007;`,
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 86}, Table: name("t", "t", 14),
			OrderBy: []stmt.OrderColumn{oc(name(`"a b"`, "a b", 33), false), oc(name("v", "v", 57), true)}, Limit: lit(stmt.Integer(7), 82, 85)}},
	{name: "limit 0", src: "select * from t limit 0", want: &stmt.Select{Span: stmt.Span{Start: 0, End: 23}, Table: name("t", "t", 14), Limit: lit(stmt.Integer(0), 22, 23)}},
	{name: "the largest limit of 64 bits", src: "select * from t limit 9223372036854775807",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 41}, Table: name("t", "t", 14), Limit: lit(stmt.Integer(9223372036854775807), 22, 41)}},
	{name: "order by a column named by a keyword", src: "select * from t order by abort desc",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 35}, Table: name("t", "t", 14), OrderBy: []stmt.OrderColumn{oc(name("abort", "abort", 25), true)}}},
	{name: "order by and limit after a where of every form", src: "select a from t where a = 1 and b is null and 2 < c and d is not null and e != ? order by c, a desc limit ?",
		want: &stmt.Select{Span: stmt.Span{Start: 0, End: 107}, Table: name("t", "t", 14), Columns: []stmt.Name{name("a", "a", 7)}, Where: []stmt.Condition{
			cond(name("a", "a", 22), stmt.OpEqual, lit(stmt.Integer(1), 26, 27)), cond(name("b", "b", 32), stmt.OpIsNull, stmt.Term{}),
			condFirst(name("c", "c", 50), stmt.OpGreater, lit(stmt.Integer(2), 46, 47)), cond(name("d", "d", 56), stmt.OpIsNotNull, stmt.Term{}),
			cond(name("e", "e", 74), stmt.OpNotEqual, param(1, 79))},
			OrderBy: []stmt.OrderColumn{oc(name("c", "c", 90), false), oc(name("a", "a", 93), true)}, Limit: param(2, 106)}},
	{name: "a position in order by", src: "select * from t order by 1", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a position out of range in order by", src: "select * from t order by 0", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a large position in order by", src: "select * from t order by 99", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a negative position in order by", src: "select * from t order by -1", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a position after a column in order by", src: "select * from t order by v, 2", err: "s4ql: not supported: ordering term other than a column (at offset 28)"},
	{name: "a hex position in order by", src: "select * from t order by 0x1", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a real in order by", src: "select * from t order by 1.5", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a string in order by", src: "select * from t order by 'v'", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a blob in order by", src: "select * from t order by x'00'", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "null in order by", src: "select * from t order by null", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a parameter in order by", src: "select * from t order by ?", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "the current time in order by", src: "select * from t order by current_time", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "an expression in order by", src: "select * from t order by v + 1", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a negated column in order by", src: "select * from t order by -v", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a function in order by", src: "select * from t order by abs(v)", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a cast in order by", src: "select * from t order by cast(v as int)", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a subquery in order by", src: "select * from t order by (select 1)", err: "s4ql: not supported: ordering term other than a column (at offset 25)"},
	{name: "a column in parentheses in order by", src: "select * from t order by (v)", err: "s4ql: not supported: parentheses (at offset 25)"},
	{name: "a column with its table in order by", src: "select * from t order by t.v", err: "s4ql: not supported: column with the name of a table (at offset 25)"},
	{name: "a column with its schema and table in order by", src: "select * from t order by main.t.v", err: "s4ql: not supported: column with the name of a table (at offset 25)"},
	{name: "collate in order by", src: "select * from t order by v collate nocase", err: "s4ql: not supported: COLLATE (at offset 25)"},
	{name: "nulls first in order by", src: "select * from t order by v nulls first", err: "s4ql: not supported: NULLS FIRST or NULLS LAST (at offset 27)"},
	{name: "nulls last after desc in order by", src: "select * from t order by v desc nulls last", err: "s4ql: not supported: NULLS FIRST or NULLS LAST (at offset 32)"},
	{name: "nulls first before a second column in order by", src: "select * from t order by v asc nulls first, w", err: "s4ql: not supported: NULLS FIRST or NULLS LAST (at offset 31)"},
	{name: "true in order by", src: "select * from t order by true", err: "s4ql: not supported: TRUE or FALSE (at offset 25)"},
	{name: "false after a column in order by", src: "select * from t order by v, false desc", err: "s4ql: not supported: TRUE or FALSE (at offset 28)"},
	{name: "a column named twice in order by", src: "select * from t order by v, v", err: "s4ql: not supported: column named twice (at offset 28)"},
	{name: "a column named twice in order by in another case and direction", src: "select * from t order by v asc, w, V desc", err: "s4ql: not supported: column named twice (at offset 35)"},
	{name: "a column named twice in order by, once in double quotes", src: `select * from t order by "v", v`, err: "s4ql: not supported: column named twice (at offset 30)"},
	// A name in brackets or backticks is not supported for its quoting, before the pass looks whether
	// the list named it before: SQLite reads [v] as v, and "[v]" as [v].
	{name: "a name in brackets after a name in double quotes with brackets in it in order by", src: `select * from t order by "[v]", [v]`, err: "s4ql: not supported: name in brackets (at offset 32)"},
	{name: "a name in brackets after a name in double quotes with brackets in it in an insert", src: `insert into t ("[v]", [v]) values (1, 2)`, err: "s4ql: not supported: name in brackets (at offset 22)"},
	{name: "a name in backticks after the same name bare in a set", src: "update t set v = 1, `v` = 2", err: "s4ql: not supported: name in backticks (at offset 20)"},
	{name: "a name in brackets after the same name bare in an index", src: "create index i on t (v, [v])", err: "s4ql: not supported: name in brackets (at offset 24)"},
	{name: "a name in brackets in order by", src: "select * from t order by [v]", err: "s4ql: not supported: name in brackets (at offset 25)"},
	{name: "a name in backticks in order by", src: "select * from t order by `v`", err: "s4ql: not supported: name in backticks (at offset 25)"},
	{name: "an empty name in order by", src: `select * from t order by ""`, err: "s4ql: not supported: empty name (at offset 25)"},
	{name: "order by without a column", src: "select * from t order by", err: "incomplete input"},
	{name: "order by with a comma and no column after it", src: "select * from t order by v,", err: "incomplete input"},
	{name: "asc and desc in order by", src: "select * from t order by v asc desc", err: `near "desc": syntax error`},
	{name: "order by twice", src: "select * from t order by v order by v", err: `near "order": syntax error`},
	{name: "order by after group by", src: "select * from t group by v order by v", err: "s4ql: not supported: GROUP BY clause (at offset 16)"},
	{name: "order by after a compound", src: "select * from t union select * from t order by v", err: "s4ql: not supported: compound SELECT (at offset 16)"},
	{name: "order by after OR in where", src: "select * from t where v = 1 or v = 2 order by v", err: "s4ql: not supported: OR (at offset 28)"},
	{name: "a negative limit", src: "select * from t limit -1", err: "s4ql: not supported: negative LIMIT (at offset 22)"},
	{name: "a limit of minus zero", src: "select * from t limit -0", err: "s4ql: not supported: negative LIMIT (at offset 22)"},
	{name: "a negative parameter in limit", src: "select * from t limit -?", err: "s4ql: not supported: negative LIMIT (at offset 22)"},
	{name: "a plus sign in limit", src: "select * from t limit +1", err: "s4ql: not supported: + sign (at offset 22)"},
	{name: "null after a plus sign in limit", src: "select * from t limit +null", err: "s4ql: not supported: + sign (at offset 22)"},
	{name: "two minus signs in limit", src: "select * from t limit - -1", err: "s4ql: not supported: negative LIMIT (at offset 22)"},
	{name: "a negative string in limit", src: "select * from t limit -'a'", err: "s4ql: not supported: negative LIMIT (at offset 22)"},
	{name: "an infinite real in limit", src: "select * from t limit 1e999", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "not in limit", src: "select * from t limit not 1", err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "a real in limit", src: "select * from t limit 1.5", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "a real with an integer value in limit", src: "select * from t limit 1.0", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "a real with an exponent in limit", src: "select * from t limit 1e2", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "a string in limit", src: "select * from t limit '5'", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "a blob in limit", src: "select * from t limit x'05'", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "a hex limit", src: "select * from t limit 0x5", err: "s4ql: not supported: hex literal (at offset 22)"},
	{name: "a limit outside 64 bits", src: "select * from t limit 9223372036854775808", err: "s4ql: not supported: integer outside 64 bits (at offset 22)"},
	{name: "a limit with a digit separator", src: "select * from t limit 1_0", err: "s4ql: not supported: number with a digit separator (at offset 22)"},
	{name: "the current time in limit", src: "select * from t limit current_time", err: "s4ql: not supported: current date or time (at offset 22)"},
	{name: "true in limit", src: "select * from t limit true", err: "s4ql: not supported: TRUE or FALSE (at offset 22)"},
	{name: "a column in limit", src: "select * from t limit v", err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "a column in double quotes in limit", src: `select * from t limit "v"`, err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "an expression in limit", src: "select * from t limit 1 + 1", err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "a ~ in limit", src: "select * from t limit ~1", err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "a function in limit", src: "select * from t limit abs(1)", err: "s4ql: not supported: LIMIT of an expression (at offset 22)"},
	{name: "a limit in parentheses", src: "select * from t limit (1)", err: "s4ql: not supported: parentheses (at offset 22)"},
	{name: "a numbered parameter in limit", src: "select * from t limit ?1", err: "s4ql: not supported: numbered parameter (at offset 22)"},
	{name: "a named parameter in limit", src: "select * from t limit :n", err: "s4ql: not supported: named parameter (at offset 22)"},
	{name: "limit with offset", src: "select * from t limit 1 offset 2", err: "s4ql: not supported: OFFSET (at offset 24)"},
	{name: "limit with an offset and a comma", src: "select * from t limit 2, 1", err: "s4ql: not supported: OFFSET (at offset 23)"},
	{name: "limit with an offset and a comma, the offset a string", src: "select * from t limit 'a', 1", err: "s4ql: not supported: LIMIT other than an integer (at offset 22)"},
	{name: "limit with a parameter and offset", src: "select * from t order by v limit ? offset ?", err: "s4ql: not supported: OFFSET (at offset 35)"},
	{name: "offset alone", src: "select * from t offset 2", err: `near "2": syntax error`},
	{name: "limit without a number", src: "select * from t limit", err: "incomplete input"},
	{name: "limit before order by", src: "select * from t limit 1 order by v", err: `near "order": syntax error`},
	{name: "limit twice", src: "select * from t limit 1 limit 2", err: `near "limit": syntax error`},
	{name: "order by and limit and a second statement", src: "select * from t order by v limit 1; select 1", err: "s4ql: not supported: second statement (at offset 36)"},
}

// oc is a column of an ORDER BY.
func oc(n stmt.Name, descending bool) stmt.OrderColumn {
	return stmt.OrderColumn{Name: n, Descending: descending}
}

// ixc is a column of a CREATE INDEX.
func ixc(n stmt.Name, descending bool) stmt.IndexColumn {
	return stmt.IndexColumn{Name: n, Descending: descending}
}

// cond is a condition of a WHERE on the column, with the operator op and the value v.
func cond(column stmt.Name, op stmt.Operator, v stmt.Term) stmt.Condition {
	return stmt.Condition{Column: column, Operator: op, Value: v}
}

// condFirst is a condition of a WHERE on the column, with the operator op and the value v, whose text
// writes the value on the left, with the operator swapped.
func condFirst(column stmt.Name, op stmt.Operator, v stmt.Term) stmt.Condition {
	return stmt.Condition{Column: column, Operator: op, Value: v, ValueFirst: true}
}

// set is an assignment of an UPDATE of the value v to the column.
func set(column stmt.Name, v stmt.Term) stmt.Assignment {
	return stmt.Assignment{Column: column, Value: v}
}

// column of a CREATE TABLE.
func column(raw, value string, offset int, typ stmt.Type, primaryKey, notNull bool) stmt.Column {
	return stmt.Column{Name: name(raw, value, offset), Type: typ, PrimaryKey: primaryKey, NotNull: notNull}
}

// withDefault is the column c with a DEFAULT of the value v, whose literal is at [start, end).
func withDefault(c stmt.Column, v stmt.Value, start, end int) stmt.Column {
	c.Default = stmt.Default{Value: v, Span: stmt.Span{Start: start, End: end}}
	return c
}

// lit is a value of an INSERT that is a literal of the value v, at [start, end).
func lit(v stmt.Value, start, end int) stmt.Term {
	return stmt.Term{Value: v, Span: stmt.Span{Start: start, End: end}}
}

// param is a value of an INSERT that is the parameter of the number n, at the offset at.
func param(n, at int) stmt.Term {
	return stmt.Term{Parameter: n, Span: stmt.Span{Start: at, End: at + 1}}
}

// manyColumns is the case of a CREATE TABLE of n columns, c0 to c{n-1}, of which c0 is the primary
// key, and the last is named last if it is not "". SQLite allows 2000 columns.
func manyColumns(what string, n int, last string) checkCase {
	var b strings.Builder
	b.WriteString("create table t (")
	var columns []stmt.Column
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		cn := fmt.Sprint("c", i)
		if i == n-1 && last != "" {
			cn = last
		}
		c := column(cn, cn, b.Len(), stmt.TypeInt, i == 0, i > 0)
		b.WriteString(cn + " int ")
		if i == 0 {
			b.WriteString("primary key")
		} else {
			b.WriteString("not null")
		}
		columns = append(columns, c)
	}
	b.WriteString(") strict, without rowid")
	return checkCase{name: what, src: b.String(), want: &stmt.CreateTable{Span: stmt.Span{Start: 0, End: b.Len()}, Table: name("t", "t", 13), Columns: columns}}
}

// TestCheck pins what [stmt.Check] gives on texts that define slice 1, and checks each against SQLite.
func TestCheck(t *testing.T) {
	for _, c := range checkCases {
		t.Run("should give the right answer for "+c.name, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			if c.err != "" {
				is.True(t, err != nil, "want an error")
				is.Equal(t, c.err, err.Error())
			} else {
				is.NotError(t, err)
			}
			want := c.want
			if ct, ok := want.(*stmt.CreateTable); ok && ct.SQL == "" {
				// verify checks the text that SQLite keeps against SQLite.
				copied := *ct
				copied.SQL = s.(*stmt.CreateTable).SQL
				want = &copied
			}
			if ci, ok := want.(*stmt.CreateIndex); ok && ci.SQL == "" {
				// verify checks the text that SQLite keeps against SQLite.
				copied := *ci
				copied.SQL = s.(*stmt.CreateIndex).SQL
				want = &copied
			}
			is.Equal(t, fmt.Sprintf("%#v", want), fmt.Sprintf("%#v", s))
			verify(t, c.src)
		})
	}

	t.Run("should take a table with IF NOT EXISTS and two integer columns with DEFAULT 0, as goqite's queue has", func(t *testing.T) {
		src := "create table if not exists goqite (\n  id text primary key,\n  queue text not null,\n  body blob not null,\n" +
			"  timeout text not null,\n  received integer not null default 0,\n  priority integer not null default 0\n) strict, without rowid"
		s, err := stmt.Check(src)
		is.NotError(t, err)
		c := s.(*stmt.CreateTable)
		is.True(t, c.IfNotExists)
		var defaults []string
		for _, column := range c.Columns {
			if column.Default.Value != nil {
				defaults = append(defaults, column.Name.Value+" "+show(column.Default.Value)+" "+src[column.Default.Span.Start:column.Default.Span.End])
			}
		}
		is.EqualSlice(t, []string{"received integer 0 0", "priority integer 0 0"}, defaults)
		o, _ := verify(t, src)
		is.Equal(t, supported, o)
	})

	t.Run("should give no result code for not supported, and SQLite's codes for a syntax error", func(t *testing.T) {
		_, err := stmt.Check("drop view v")
		var ne *stmt.NotSupportedError
		is.True(t, errors.As(err, &ne))
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		_, err = stmt.Check("drop")
		var se *sqlerr.Error
		is.True(t, errors.As(err, &se))
		is.Equal(t, sqlerr.Error{Code: 1, ExtendedCode: 1, Message: "incomplete input", Offset: -1}, *se)
		is.True(t, !errors.Is(err, stmt.ErrNotSupported))
	})

	t.Run("should list every name of the cases in the vocabulary", func(t *testing.T) {
		for _, c := range checkCases {
			if m := messageForm.FindStringSubmatch(c.err); m != nil {
				is.True(t, vocabulary(t)[m[1]], m[1])
			}
		}
	})

	t.Run("should take every keyword of SQLite that the grammar reads as a name", func(t *testing.T) {
		var names []string
		for _, kw := range keywords(t) {
			src := "select * from " + kw
			if oracle.Judge(src).Kind != oracle.Parses {
				continue
			}
			names = append(names, kw)
			o, _ := verify(t, src)
			is.Equal(t, supported, o, src)
		}
		slices.Sort(names)
		is.True(t, len(names) > 80, "too few keywords are names")
		t.Logf("%v keywords are names after FROM: %v", len(names), names)
	})

	t.Run("should take as many parameters as SQLite allows, and not one more", func(t *testing.T) {
		values := func(n int) string {
			return "insert into t values (" + strings.TrimSuffix(strings.Repeat("?, ", n), ", ") + ")"
		}
		// The meaning of so many values is not checked in SQLite, since no table has that many
		// columns: the text parses, and SQLite counts its parameters.
		src := values(32766)
		s, err := stmt.Check(src)
		is.NotError(t, err)
		is.Equal(t, 32766, s.Parameters())
		is.Equal(t, 32766, s.(*stmt.Insert).Values[32765].Parameter)
		is.Equal(t, oracle.Parses, oracle.Judge(src).Kind)

		src = values(32767)
		_, err = stmt.Check(src)
		is.Equal(t, fmt.Sprintf("s4ql: not supported: parameter beyond SQLite's limit (at offset %v)", len(src)-2), err.Error())
		v := oracle.Judge(src)
		is.Equal(t, "too many SQL variables", v.Message)
		o, _ := verify(t, src)
		is.Equal(t, notSupportedNotSaid, o)
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of a column in the select list, and no other", func(t *testing.T) {
		// SQLite reads a keyword as the name of a column if it gives `no such column` for it on a table
		// without a column of that name. It reads others as a value, such as NULL and CURRENT_TIME.
		d := open(t)
		d.must("create table t (x int primary key) strict, without rowid")
		var names []string
		for _, kw := range keywords(t) {
			src := "select " + kw + " from t"
			if oracle.Judge(src).Kind != oracle.Parses {
				continue
			}
			column := false
			var e *sqlite.Error
			if err := d.exec(src); errors.As(err, &e) && e.Message == "no such column: "+kw {
				column = true
				names = append(names, kw)
			}
			o, _ := verify(t, src)
			is.Equal(t, column, o == supported, src)
		}
		slices.Sort(names)
		is.True(t, len(names) > 80, "too few keywords are names")
		t.Logf("%v keywords are names of columns in the select list: %v", len(names), names)
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of a column in WHERE, on either side of a comparison, and no other", func(t *testing.T) {
		d := open(t)
		d.must("create table t (x int primary key) strict, without rowid")
		var left []string
		for _, form := range []string{"select * from t where %v = 1", "select * from t where 1 = %v", "select * from t where ? < %v"} {
			var names []string
			for _, kw := range keywords(t) {
				src := fmt.Sprintf(form, kw)
				if oracle.Judge(src).Kind != oracle.Parses {
					continue
				}
				column := false
				var e *sqlite.Error
				if err := d.exec(src); errors.As(err, &e) && e.Message == "no such column: "+kw {
					column = true
					names = append(names, kw)
				}
				o, _ := verify(t, src)
				is.Equal(t, column, o == supported, src)
			}
			slices.Sort(names)
			is.True(t, len(names) > 80, "too few keywords are names")
			t.Logf("%v keywords are names of columns in %q: %v", len(names), form, names)
			// SQLite reads the same keywords as a column on the right of a comparison as on its left.
			if left == nil {
				left = names
			}
			is.EqualSlice(t, left, names, form)
		}
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of a column in ORDER BY, with ASC, DESC, or neither, and no other", func(t *testing.T) {
		d := open(t)
		d.must("create table t (x int primary key) strict, without rowid")
		var first []string
		for _, form := range []string{"select * from t order by %v", "select * from t order by %v desc", "select * from t order by x asc, %v", "select x from t where x = 1 order by %v asc limit 1"} {
			var names []string
			for _, kw := range keywords(t) {
				src := fmt.Sprintf(form, kw)
				if oracle.Judge(src).Kind != oracle.Parses {
					continue
				}
				column := false
				var e *sqlite.Error
				if err := d.exec(src); errors.As(err, &e) && e.Message == "no such column: "+kw {
					column = true
					names = append(names, kw)
				}
				o, _ := verify(t, src)
				is.Equal(t, column, o == supported, src)
			}
			slices.Sort(names)
			is.True(t, len(names) > 80, "too few keywords are names")
			t.Logf("%v keywords are names of columns in %q: %v", len(names), form, names)
			// SQLite reads the same keywords as a column in each place of ORDER BY.
			if first == nil {
				first = names
			}
			is.EqualSlice(t, first, names, form)
		}
		// They are those of the select list, which the generator draws the columns of ORDER BY from. A
		// keyword of the base pool, such as temp, is in the pool twice.
		pool := slices.DeleteFunc(slices.Clone(namePools(t)[stmttest.SelectColumn]), func(n string) bool {
			return !slices.Contains(keywords(t), n)
		})
		slices.Sort(pool)
		is.EqualSlice(t, first, slices.Compact(pool))
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of a table or a column of an update or a delete, and no other", func(t *testing.T) {
		// SQLite reads a keyword as a name if it gives `no such table` or `no such column` for it on a
		// table without a column of that name. The pools of the generator are those of the same names
		// in an insert and in a select.
		d := open(t)
		d.must("create table t (x int primary key) strict, without rowid")
		pools := namePools(t)
		for _, c := range []struct {
			form, message string
			pool          stmttest.Position
		}{
			{"update %v set x = 1", "no such table: ", stmttest.UpdateTable},
			{"delete from %v", "no such table: ", stmttest.DeleteTable},
			{"update t set %v = 1", "no such column: ", stmttest.UpdateColumn},
			{"update t set x = 1 where %v = 1", "no such column: ", stmttest.SelectColumn},
			{"delete from t where %v is null", "no such column: ", stmttest.SelectColumn},
			{"update t set x = 1 where 1 < %v", "no such column: ", stmttest.SelectColumn},
			{"delete from t where ? >= %v", "no such column: ", stmttest.SelectColumn},
		} {
			var names []string
			for _, kw := range keywords(t) {
				src := fmt.Sprintf(c.form, kw)
				if oracle.Judge(src).Kind != oracle.Parses {
					continue
				}
				name := false
				var e *sqlite.Error
				if err := d.exec(src); errors.As(err, &e) && e.Message == c.message+kw {
					name = true
					names = append(names, kw)
				}
				o, _ := verify(t, src)
				is.Equal(t, name, o == supported, src)
				is.Equal(t, name, slices.Contains(pools[c.pool], kw), src)
			}
			is.True(t, len(names) > 80, "too few keywords are names")
			t.Logf("%v keywords are names in %q", len(names), c.form)
		}
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of an index, a table, or a column of a create index or a drop index, and no other", func(t *testing.T) {
		// SQLite reads a keyword as a name if it gives an error that names it on a database without an
		// object of that name: `no such table` for the table, `no such column` for a column, and `no
		// such index` for DROP INDEX. The name of a new index is always a name of the grammar, so
		// SQLite reads a keyword there as a name if the text parses, and gives `no such table` for the
		// table after it.
		pools := namePools(t)
		for _, c := range []struct {
			form    string
			message func(kw string) string
			pool    stmttest.Position
		}{
			{"create index %v on u (x)", func(string) string { return "no such table: main.u" }, stmttest.IndexName},
			{"create index i on %v (x)", func(kw string) string { return "no such table: main." + kw }, stmttest.IndexTable},
			{"create index i on t (%v)", func(kw string) string { return "no such column: " + kw }, stmttest.IndexColumn},
			{"create index i on t (x, %v desc)", func(kw string) string { return "no such column: " + kw }, stmttest.IndexColumn},
			{"drop index %v", func(kw string) string { return "no such index: " + kw }, stmttest.DropIndex},
		} {
			var names []string
			for _, kw := range keywords(t) {
				src := fmt.Sprintf(c.form, kw)
				if oracle.Judge(src).Kind != oracle.Parses {
					continue
				}
				// A database of its own for each text, since SQLite creates an index on an expression.
				d := open(t)
				d.must("create table t (x int primary key) strict, without rowid")
				name := false
				var e *sqlite.Error
				if err := d.exec(src); errors.As(err, &e) && e.Message == c.message(kw) {
					name = true
					names = append(names, kw)
				}
				o, _ := verify(t, src)
				is.Equal(t, name, o == supported, src)
				is.Equal(t, name, slices.Contains(pools[c.pool], kw), src)
			}
			is.True(t, len(names) > 80, "too few keywords are names")
			slices.Sort(names)
			t.Logf("%v keywords are names in %q: %v", len(names), c.form, names)
		}
	})

	t.Run("should take every keyword of SQLite that SQLite reads as the name of a table or an index after IF NOT EXISTS or IF EXISTS, and no other", func(t *testing.T) {
		// SQLite reads a keyword as a name there if the statement creates or drops the object of that
		// name: on a database without it for IF NOT EXISTS, and on one with it, created with the name in
		// double quotes, for IF EXISTS. The names are those of the plain statement, and IF too after IF
		// EXISTS, which starts the clause in the plain statement. After IF NOT EXISTS SQLite cannot read
		// IF again from its schema, and creates nothing.
		pools := namePools(t)
		for _, c := range []struct {
			form, setup, objects string
			pool                 stmttest.Position
			created              bool
		}{
			{"create table if not exists %v (x int primary key) strict, without rowid", "", "table", stmttest.CreateTable, true},
			{"drop table if exists %v", "create table %v (x int primary key) strict, without rowid", "table", stmttest.DropTable, false},
			{"create index if not exists %v on t (x)", "", "index", stmttest.IndexName, true},
			{"drop index if exists %v", "create index %v on t (x)", "index", stmttest.DropIndex, false},
		} {
			var names []string
			for _, kw := range keywords(t) {
				src := fmt.Sprintf(c.form, kw)
				if oracle.Judge(src).Kind != oracle.Parses {
					continue
				}
				d := open(t)
				d.must("create table t (x int primary key) strict, without rowid")
				if c.setup != "" {
					d.must(fmt.Sprintf(c.setup, quote(kw)))
				}
				name := false
				if d.exec(src) == nil {
					_, rows := d.query("select count(*) from sqlite_schema where type = ? and name = ?", c.objects, kw)
					name = fmt.Sprint(rows) == fmt.Sprint([][]string{{map[bool]string{true: "integer:1", false: "integer:0"}[c.created]}})
				}
				if name {
					names = append(names, kw)
				}
				o, _ := verify(t, src)
				is.Equal(t, name, o == supported, src)
				is.Equal(t, name, slices.Contains(pools[c.pool], kw) || kw == "if" && !c.created, src)
			}
			is.True(t, len(names) > 80, "too few keywords are names")
			is.True(t, slices.Contains(names, "if") != c.created, c.form)
			t.Logf("%v keywords are names in %q", len(names), c.form)
		}
	})

	t.Run("should answer for IF NOT EXISTS and IF EXISTS what SQLite does on a database with the object or without it", func(t *testing.T) {
		// What SQLite does with the clauses where the pass's answer and SQLite's differ, or where the
		// engine needs the fact: the database has the table t of the columns v and w, the index i on
		// it, and one row, which sets SQLite's count of changes to 1. Each text runs on a database of
		// its own, under `pragma query_only` too, as s4ql's read-only transaction runs; want is SQLite's
		// error, or "" for none, and with no error the schema stays the same, but for the texts of
		// created, which create their object. pass is the pass's answer: "" for supported, or the name
		// of "not supported".
		many := make([]string, 2001)
		for i := range many {
			many[i] = fmt.Sprint("c", i, " int")
		}
		manyColumns := strings.Replace(strings.Join(many, ", "), "c0 int", "c0 int primary key", 1)
		for _, c := range []struct {
			src, want, readOnly, pass string
			created                   bool
		}{
			// With the table, SQLite reads no more of the declaration than the parser needs.
			{"create table if not exists t (v int primary key) strict, without rowid", "", "", "", false},
			{"create table if not exists T (a text primary key) strict, without rowid", "", "", "", false},
			{"create table if not exists t (a int primary key, a int) strict, without rowid", "", "", "", false},
			{"create table if not exists t (" + manyColumns + ") strict, without rowid", "", "", "", false},
			{"create table if not exists u (a int primary key, a int) strict, without rowid", "duplicate column name: a", "duplicate column name: a", "", false},
			{"create table if not exists u (" + manyColumns + ") strict, without rowid", "too many columns on u", "too many columns on u", "", false},
			{"create table if not exists u (v int primary key) strict, without rowid", "", "attempt to write a readonly database", "", true},
			{"create table if not exists t (a foo primary key) strict, without rowid", "", "", "column type other than INT, INTEGER, REAL, TEXT, or BLOB", false},
			{"create table if not exists t (a int primary key, b int primary key) strict, without rowid", "", "", "second PRIMARY KEY", false},
			{"create table if not exists t (a int) strict, without rowid", "", "", "table without PRIMARY KEY", false},
			{"create table if not exists t (a int primary key)", "", "", "table without STRICT and WITHOUT ROWID", false},
			{"create table if not exists t (a int primary key autoincrement) strict, without rowid", "", "", "AUTOINCREMENT", false},
			{"create table if not exists t (a int primary key collate nosuch) strict, without rowid", "", "", "COLLATE", false},
			{"create table if not exists t as select 1", "", "", "CREATE TABLE AS", false},
			{"create table if not exists t (a int primary key) strict, without rowid, foo", "unknown table option: foo", "unknown table option: foo", "table option", false},
			{"create table if not exists i (a int primary key) strict, without rowid", "there is already an index named i", "there is already an index named i", "", false},
			{"create table if not exists if (a int primary key) strict, without rowid", `malformed database schema (if) - near "(": syntax error`, "attempt to write a readonly database", "bare name IF after IF NOT EXISTS", false},
			{"create table if not exists sqlite_x (a int primary key) strict, without rowid", "object name reserved for internal use: sqlite_x", "object name reserved for internal use: sqlite_x", "reserved table name", false},
			// With the index, SQLite looks for the table and for a table of the name of the index first.
			{"create index if not exists i on t (v)", "", "", "", false},
			{"create index if not exists I on t (nosuch)", "", "", "", false},
			{"create index if not exists i on nosuch (v)", "no such table: main.nosuch", "no such table: main.nosuch", "", false},
			{"create index if not exists t on t (v)", "there is already a table named t", "there is already a table named t", "", false},
			{"create index if not exists j on t (nosuch)", "no such column: nosuch", "no such column: nosuch", "", false},
			{"create index if not exists j on t (w)", "", "attempt to write a readonly database", "", true},
			{"create index if not exists i on t (v, v)", "", "", "column named twice", false},
			{"create index if not exists i on t (v collate nosuch)", "", "", "COLLATE", false},
			{"create index if not exists i on t (v) where nosuch > 0", "", "", "partial index", false},
			{"create unique index if not exists i on t (v)", "", "", "UNIQUE INDEX", false},
			{"create index if not exists i on t (v nulls first)", "unsupported use of NULLS FIRST", "unsupported use of NULLS FIRST", "NULLS FIRST or NULLS LAST", false},
			{"create index if not exists if on t (v)", `malformed database schema (if) - near "on": syntax error`, "attempt to write a readonly database", "bare name IF after IF NOT EXISTS", false},
			{"create index if not exists sqlite_x on t (v)", "object name reserved for internal use: sqlite_x", "object name reserved for internal use: sqlite_x", "reserved index name", false},
			// Without the object, IF EXISTS does nothing, also for an object of the other kind.
			{"drop table if exists nosuch", "", "", "", false},
			{"drop table if exists i", "", "", "", false},
			{"drop index if exists nosuch", "", "", "", false},
			{"drop index if exists t", "", "", "", false},
			{"drop table if exists sqlite_x", "", "", "reserved table name", false},
			{"drop table if exists sqlite_schema", "table sqlite_master may not be dropped", "table sqlite_master may not be dropped", "reserved table name", false},
			{"drop table if exists nosuch.t", "", "", "schema name", false},
			{"drop index if exists sqlite_autoindex_t_1", "index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped", "index associated with UNIQUE or PRIMARY KEY constraint cannot be dropped", "reserved index name", false},
			{"drop table if exists t", "", "attempt to write a readonly database", "", true},
			{"drop index if exists i", "", "attempt to write a readonly database", "", true},
		} {
			for _, readOnly := range []bool{false, true} {
				d := open(t)
				d.must("create table t (v int primary key, w int) strict, without rowid")
				d.must("create index i on t (v)")
				d.must("insert into t values (1, 2)")
				want := c.want
				if readOnly {
					d.must("pragma query_only = 1")
					want = c.readOnly
				}
				before := d.snapshot()
				err := d.exec(c.src)
				if want == "" {
					is.NotError(t, err, c.src)
					is.Equal(t, !c.created || readOnly, d.snapshot() == before, c.src)
				} else {
					var e *sqlite.Error
					is.True(t, errors.As(err, &e), c.src)
					is.Equal(t, want, e.Message, c.src)
					is.Equal(t, before, d.snapshot(), c.src)
				}
				// No statement of the clauses sets the count of changes.
				is.Equal[stmt.Value](t, stmt.Integer(1), d.value("select changes()"), c.src)
			}
			_, err := stmt.Check(c.src)
			if c.pass == "" {
				is.NotError(t, err, c.src)
			} else {
				var ne *stmt.NotSupportedError
				is.True(t, errors.As(err, &ne), c.src)
				is.Equal(t, c.pass, ne.What, c.src)
			}
			verify(t, c.src)
		}
	})

	t.Run("should answer for DEFAULT what SQLite does when an insert, an update, or a select meets it", func(t *testing.T) {
		// What SQLite does with the defaults of a table, for the engine: the database has the tables
		// t, u, x, y, and z below, with the length limit lowered to 1,000 bytes, and t and u have a row
		// each. Each text runs on a database of its own, under `pragma query_only` too; want is SQLite's
		// error, or "" for none, readOnly its error under query_only, and rows the rows of the table
		// of the text afterwards, without query_only.
		x500, y600 := strings.Repeat("x", 500), strings.Repeat("y", 600)
		const readOnly = "attempt to write a readonly database"
		for _, c := range []struct {
			src, want, readOnly, table string
			rows                       []string
		}{
			// The left-out columns get their defaults, NULL, or the error of NOT NULL, which an explicit
			// NULL gets too.
			{"insert into t (k, c) values (1, 1.5)", "", readOnly, "t", []string{"1 0 'x' 1.5 NULL", "5 1 'b' 1.0 X'01'"}},
			{"insert into t (c, k) values (-0.0, 2)", "", readOnly, "t", []string{"2 0 'x' 0.0 NULL", "5 1 'b' 1.0 X'01'"}},
			{"insert into t (k) values (1)", "NOT NULL constraint failed: t.c", readOnly, "t", []string{"5 1 'b' 1.0 X'01'"}},
			{"insert into t (k, a, c) values (1, null, 2.5)", "NOT NULL constraint failed: t.a", readOnly, "t", []string{"5 1 'b' 1.0 X'01'"}},
			{"insert into t (k, b, c) values (1, null, 2.5)", "", readOnly, "t", []string{"1 0 NULL 2.5 NULL", "5 1 'b' 1.0 X'01'"}},
			{"insert into t (k, c) values (5, 1.5)", "UNIQUE constraint failed: t.k", readOnly, "t", []string{"5 1 'b' 1.0 X'01'"}},
			{"insert into t (k, c) values (1, 'a')", "cannot store TEXT value in REAL column t.c", readOnly, "t", []string{"5 1 'b' 1.0 X'01'"}},
			// An insert without a list of columns gives every column a value: the defaults change
			// nothing.
			{"insert into t values (1)", "table t has 5 columns but 1 values were supplied", "table t has 5 columns but 1 values were supplied", "t", []string{"5 1 'b' 1.0 X'01'"}},
			{"insert into t values (1, 2, 'c', 3.5, x'04')", "", readOnly, "t", []string{"1 2 'c' 3.5 X'04'", "5 1 'b' 1.0 X'01'"}},
			{"insert into t (k, nosuch) values (1, 2)", "table t has no column named nosuch", "table t has no column named nosuch", "t", []string{"5 1 'b' 1.0 X'01'"}},
			// The key gets its default, once.
			{"insert into x (v) values (1)", "", readOnly, "x", []string{"9 1"}},
			{"insert into u (v) values (1)", "UNIQUE constraint failed: u.k", readOnly, "u", []string{"7 0"}},
			{"insert into y (v) values (1)", "NOT NULL constraint failed: y.k", readOnly, "y", nil},
			// The record of the row holds the defaults: too big at the lowered limit, after NOT NULL.
			{"insert into z (k, a) values (1, '" + y600 + "')", "NOT NULL constraint failed: z.c", readOnly, "z", nil},
			{"insert into z (k, a, c) values (1, '" + y600 + "', 1)", "string or blob too big", readOnly, "z", nil},
			{"insert into z (k, a, c) values (1, '" + y600 + "', null)", "NOT NULL constraint failed: z.c", readOnly, "z", nil},
			// An update and a select do not meet the defaults.
			{"update t set b = null where k = 5", "", readOnly, "t", []string{"5 1 NULL 1.0 X'01'"}},
			{"update t set a = null where k = 5", "NOT NULL constraint failed: t.a", readOnly, "t", []string{"5 1 'b' 1.0 X'01'"}},
			{"select * from t where a = 0", "", "", "t", []string{"5 1 'b' 1.0 X'01'"}},
		} {
			for _, ro := range []bool{false, true} {
				d := open(t)
				d.must("create table t (k int primary key, a int not null default 0, b text default 'x', c real not null default null, e blob default null) strict, without rowid")
				d.must("create table u (k int primary key default 7, v int) strict, without rowid")
				d.must("create table x (k int primary key default 9, v int) strict, without rowid")
				d.must("create table y (k int primary key default null, v int) strict, without rowid")
				d.must("create table z (k int primary key, a text, b text default '" + x500 + "', c int not null default null) strict, without rowid")
				d.must("insert into t values (5, 1, 'b', 1.0, x'01')")
				d.must("insert into u values (7, 0)")
				is.NotError(t, d.c.SetLengthLimit(1000))
				want := c.want
				if ro {
					d.must("pragma query_only = 1")
					want = c.readOnly
				}
				err := d.exec(c.src)
				if want == "" {
					is.NotError(t, err, c.src)
				} else {
					var e *sqlite.Error
					is.True(t, errors.As(err, &e), c.src)
					is.Equal(t, want, e.Message, c.src)
				}
				if ro {
					continue
				}
				columns := map[string]string{"t": "k, a, b, c, e", "u": "k, v", "x": "k, v", "y": "k, v", "z": "k, a, b, c"}[c.table]
				quoted := "quote(" + strings.ReplaceAll(columns, ", ", ") || ' ' || quote(") + ")"
				_, rows := d.query("select " + quoted + " from " + c.table + " order by k")
				var got []string
				for _, r := range rows {
					got = append(got, strings.TrimPrefix(r[0], "text:"))
				}
				is.EqualSlice(t, c.rows, got, c.src)
			}
		}
	})

	t.Run("should read a value on the left as SQLite does: the rows of its column on the left with the operator swapped, on a column of every type", func(t *testing.T) {
		// What SQLite does with a value on the left, for the engine: on a strict table with a column of
		// each type, and on a table that is not strict, `value op column` gives the rows of `column
		// op' value`, with op' the operator that [stmt.Operator.Swapped] gives, for every operator in
		// every spelling and a value of every type, NULL included, as a literal and as a ?. SQLite
		// applies the affinity of a column to the value on either side, so the two orders also agree
		// where it converts the value, such as '5' compared with an INT column.
		values := []string{"null", "0", "5", "-5", "10", "5.0", "5.5", "-0.0", "1e300", "9223372036854775807", "-9223372036854775808",
			"'5'", "'5.0'", "' 5'", "'x'", "''", "'a'", "x''", "x'05'", "x'35'"}
		spellings := map[string]stmt.Operator{"=": stmt.OpEqual, "==": stmt.OpEqual, "<>": stmt.OpNotEqual, "!=": stmt.OpNotEqual,
			"<": stmt.OpLess, "<=": stmt.OpLessOrEqual, ">": stmt.OpGreater, ">=": stmt.OpGreaterOrEqual}
		compared, some := 0, 0
		for _, options := range []string{" strict, without rowid", ""} {
			for _, typ := range []string{"int", "real", "text", "blob", "any"} {
				if typ == "any" && options == "" {
					continue
				}
				d := open(t)
				d.must("create table c (k int primary key, v " + typ + ")" + options)
				for i, v := range values {
					// A strict table refuses a value that its column does not take, and keeps the others.
					_ = d.exec(fmt.Sprintf("insert into c values (%v, %v)", i, v))
				}
				for _, v := range values {
					arg := d.value("select " + v)
					for spelling, op := range spellings {
						for _, value := range []string{v, "?"} {
							var args []stmt.Value
							if value == "?" {
								args = []stmt.Value{arg}
							}
							left := fmt.Sprintf("select k from c where %v %v v order by k", value, spelling)
							right := fmt.Sprintf("select k from c where v %v %v order by k", op.Swapped(), value)
							_, got, err := d.run(left, args)
							is.NotError(t, err, left)
							_, want, err := d.run(right, args)
							is.NotError(t, err, right)
							is.Equal(t, showRows(want), showRows(got), typ+options+": "+left)
							compared++
							if len(got) > 0 {
								some++
							}
						}
					}
				}
			}
		}
		t.Logf("%v selects with a value on the left gave the rows of the column on the left, %v of them some rows", compared, some)
		is.True(t, some > compared/4, "too few selects gave rows")
	})

	t.Run("should order a column of each type of a strict table as SQLite does: NULL first in ASC and last in DESC, numbers by value, texts and blobs by their bytes", func(t *testing.T) {
		// What the engine of slice 13 has to give. A strict column holds one type and NULL, so values
		// of two types never meet in ORDER BY.
		for _, c := range []struct {
			typ    string
			values []stmt.Value
		}{
			{"int", []stmt.Value{stmt.Integer(5), stmt.Integer(math.MinInt64), stmt.Integer(math.MaxInt64), stmt.Integer(0), stmt.Integer(-1), stmt.Integer(5)}},
			// -0.0 and 0.0 compare equal, so their rows tie.
			{"real", []stmt.Value{stmt.Real(1.5), stmt.Real(math.Copysign(0, -1)), stmt.Real(0), stmt.Real(-math.MaxFloat64), stmt.Real(math.MaxFloat64), stmt.Real(2), stmt.Real(math.SmallestNonzeroFloat64)}},
			// By their bytes, the BINARY collation: an upper-case letter before every lower-case one, and
			// a letter outside ASCII after every ASCII one, É (C3 89) before é (C3 A9).
			{"text", []stmt.Value{stmt.Text("b"), stmt.Text("B"), stmt.Text("a"), stmt.Text("A"), stmt.Text(""), stmt.Text("é"), stmt.Text("É"), stmt.Text("z"), stmt.Text("ab"), stmt.Text("a b"), stmt.Text("😀"), stmt.Text("�")}},
			{"blob", []stmt.Value{stmt.Blob{1}, stmt.Blob{}, stmt.Blob{1, 0}, stmt.Blob{0xff}, stmt.Blob{0}, stmt.Blob{0x7f}, stmt.Blob{0x80}}},
		} {
			d := open(t)
			d.must("create table t (k int primary key, v " + c.typ + ") strict, without rowid")
			rows := [][]stmt.Value{{stmt.Integer(0), stmt.Null{}}}
			for i, v := range c.values {
				rows = append(rows, []stmt.Value{stmt.Integer(i + 1), v})
			}
			rows = append(rows, []stmt.Value{stmt.Integer(len(c.values) + 1), stmt.Null{}})
			d.insert("t", rows)
			for _, desc := range []bool{false, true} {
				sql := "select v from t order by v"
				if desc {
					sql += " desc"
				}
				_, got, err := d.run(sql, nil)
				is.NotError(t, err)
				is.Equal(t, len(rows), len(got), sql)
				nulls := []int{0, 1}
				if desc {
					nulls = []int{len(got) - 2, len(got) - 1}
				}
				for _, i := range nulls {
					is.Equal(t, "null", show(got[i][0]), c.typ, sql)
				}
				for i := 1; i < len(got); i++ {
					cmp := compareValues(got[i-1][0], got[i][0])
					if desc {
						cmp = -cmp
					}
					is.True(t, cmp <= 0, c.typ, sql, showRows(got))
				}
			}
			// A strict column refuses a value of another type, with SQLITE_CONSTRAINT_DATATYPE.
			other := map[string]string{"int": "'x'", "real": "'x'", "text": "x'01'", "blob": "1"}[c.typ]
			sqliteError(t, c.typ, d.exec("insert into t values (99, "+other+")"), 3091, fmt.Sprintf("cannot store %v value in %v column t.v",
				map[string]string{"int": "TEXT", "real": "TEXT", "text": "BLOB", "blob": "INT"}[c.typ], strings.ToUpper(c.typ)))
		}
	})

	t.Run("should limit as SQLite does: to none for 0, to every row for a negative number, and with an argument that holds an integer, and refuse every other argument with datatype mismatch", func(t *testing.T) {
		// What the engine of slice 13 has to give, or refuse in its own way. The driver hands SQLite an
		// argument as an int64, a float64, a string, a []byte, or nil, and refuses a bool or a
		// time.Time before it.
		d := open(t)
		d.must("create table t (k int primary key) strict, without rowid")
		d.must("create table e (k int primary key) strict, without rowid")
		d.insert("t", [][]stmt.Value{{stmt.Integer(1)}, {stmt.Integer(2)}, {stmt.Integer(3)}, {stmt.Integer(4)}})
		for _, c := range []struct {
			arg  stmt.Value
			rows int
		}{
			{stmt.Integer(0), 0}, {stmt.Integer(2), 2}, {stmt.Integer(4), 4}, {stmt.Integer(99), 4}, {stmt.Integer(math.MaxInt64), 4},
			{stmt.Integer(-1), 4}, {stmt.Integer(math.MinInt64), 4},
			{stmt.Real(2), 2}, {stmt.Real(math.Copysign(0, -1)), 0}, {stmt.Real(-1), 4}, {stmt.Real(1.5), -1}, {stmt.Real(1e19), -1},
			{stmt.Real(math.Inf(1)), -1}, {stmt.Real(math.NaN()), -1},
			{stmt.Text("2"), 2}, {stmt.Text("2.0"), 2}, {stmt.Text(" 2 "), 2}, {stmt.Text("-1"), 4}, {stmt.Text("x"), -1}, {stmt.Text(""), -1},
			{stmt.Text("0x2"), -1},
			{stmt.Blob("2"), -1}, {stmt.Blob{2}, -1}, {stmt.Blob{}, -1},
			{stmt.Null{}, -1},
		} {
			for _, table := range []string{"t", "e"} {
				_, got, err := d.run("select k from "+table+" limit ?", []stmt.Value{c.arg})
				switch {
				case c.rows < 0:
					// SQLite refuses the argument when it steps the statement, also on an empty table.
					sqliteError(t, show(c.arg), err, 20, "datatype mismatch")
				case table == "e":
					is.NotError(t, err)
					is.Equal(t, 0, len(got), show(c.arg))
				default:
					is.NotError(t, err)
					is.Equal(t, c.rows, len(got), show(c.arg))
				}
			}
		}
	})

	t.Run("should give SQLite's errors for ORDER BY after those of the select list and of WHERE, in the order of the text", func(t *testing.T) {
		d := open(t)
		d.must("create table t (k int primary key, a int) strict, without rowid")
		for _, c := range []struct{ src, want string }{
			{"select k from t order by nope", "no such column: nope"},
			{"select k from t order by a, nope, nope2", "no such column: nope"},
			{`select k from t order by "nope"`, `no such column: "nope" - should this be a string literal in single-quotes?`},
			{"select nope from t order by nope2", "no such column: nope"},
			{"select k from t where nope = 1 order by nope2", "no such column: nope"},
			// A test for NULL of a missing column does not stop the walk over WHERE, but its error comes
			// before ORDER BY.
			{"select k from t where a = 1 and nope is null order by nope2", "no such column: nope"},
			{"select k from t where nope is null and a = 1 order by nope2", "no such column: nope"},
			{"select k from t where a is null order by nope", "no such column: nope"},
			{"select k from nope order by nope", "no such table: nope"},
			// LIMIT is refused when the statement runs, after the names.
			{"select k from t order by nope limit ?", "no such column: nope"},
		} {
			_, _, err := d.run(c.src, []stmt.Value{stmt.Text("x")}[:strings.Count(c.src, "?")])
			sqliteError(t, c.src, err, 1, c.want)
			o, _ := verify(t, c.src)
			is.Equal(t, supported, o, c.src)
		}
	})

	t.Run("should give SQLite's errors for a value on the left in the order of the other side", func(t *testing.T) {
		// What SQLite does with the errors of a WHERE with a value on the left, for the engine, with the
		// length limit lowered to 1,000 bytes: the names come first, in the order of the text, as with
		// the column on the left, also when a value before them is too long; then a value that is too
		// long. SQLite never refuses a value for its type, so the engine's refusal of a value of another
		// type than its column comes last, as it does for the other order.
		long := "'" + strings.Repeat("x", 1200) + "'"
		for _, c := range []struct{ src, want string }{
			{"select * from t where " + long + " < a and nope = 1", "no such column: nope"},
			{"select * from t where " + long + " < a and 1 = nope", "no such column: nope"},
			{"select * from t where " + long + " < nope", "no such column: nope"},
			{"select nope from t where " + long + " < a", "no such column: nope"},
			{"select * from nope where " + long + " < a", "no such table: nope"},
			{"select * from t where 'x' < a and 1 = nope", "no such column: nope"},
			{"select * from t where 1 = nope and 2 = nope2", "no such column: nope"},
			{"select * from t where nope is null and 1 = nope2", "no such column: nope"},
			{"select * from t where nope is null and nope2 is null and 1 = nope3", "no such column: nope2"},
			{`select * from t where 5 < "nope"`, `no such column: "nope" - should this be a string literal in single-quotes?`},
			{"select * from t where 'x' < a and " + long + " = b", "string or blob too big"},
			{"select * from t where " + long + " = b and 'x' < a", "string or blob too big"},
			{"update t set a = 1 where " + long + " < a and 1 = nope", "no such column: nope"},
			{"update t set nope = 1 where 1 = nope2", "no such column: nope"},
			{"update t set a = 1 where 1 = nope2", "no such column: nope2"},
			{"update t set b = " + long + " where 'x' > a", "string or blob too big"},
			{"delete from t where " + long + " = b and 1 = nope", "no such column: nope"},
			{"delete from t where " + long + " = b", "string or blob too big"},
		} {
			o, _ := verify(t, c.src)
			is.Equal(t, supported, o, c.src)
			d := open(t)
			d.must("create table t (k int primary key, a int, b text) strict, without rowid")
			d.must("insert into t values (1, 5, 'x')")
			is.NotError(t, d.c.SetLengthLimit(1000))
			var e *sqlite.Error
			err := d.exec(c.src)
			is.True(t, errors.As(err, &e), c.src)
			is.Equal(t, c.want, e.Message, c.src)
		}
	})

	t.Run("should take as many columns of an index as SQLite allows, and not one more", func(t *testing.T) {
		index := func(n int) string {
			var names []string
			for i := range n {
				names = append(names, fmt.Sprint("c", i))
			}
			return "create index i on t (" + strings.Join(names, ", ") + ")"
		}
		// The meaning of so many columns is checked in SQLite on a table of as many columns, the most
		// that SQLite allows, without the check of checkMeaning, whose table has a key of its own.
		var columns []stmt.Column
		for i := range 2000 {
			columns = append(columns, stmt.Column{Name: stmt.Name{Value: fmt.Sprint("c", i)}, Type: stmt.TypeInt, PrimaryKey: i == 0})
		}
		src := index(2000)
		s, err := stmt.Check(src)
		is.NotError(t, err)
		is.Equal(t, 2000, len(s.(*stmt.CreateIndex).Columns))
		is.Equal(t, oracle.Parses, oracle.Judge(src).Kind)
		d := open(t)
		d.must(createColumns("t", columns))
		d.must(src)
		_, n := d.query("select count(*) from pragma_index_xinfo('i') where key")
		is.Equal(t, "[[integer:2000]]", fmt.Sprint(n))

		src = index(2001)
		_, err = stmt.Check(src)
		is.Equal(t, fmt.Sprintf("s4ql: not supported: index beyond SQLite's limit (at offset %v)", strings.LastIndex(src, "c2000")), err.Error())
		d = open(t)
		d.must(createColumns("t", columns))
		sqliteError(t, src, d.exec(src), 1, "too many columns in index")
		verify(t, src)
	})

	t.Run("should take as many assignments as SQLite allows, and not one more", func(t *testing.T) {
		update := func(n int) string {
			var b strings.Builder
			b.WriteString("update t set ")
			for i := range n {
				if i > 0 {
					b.WriteString(", ")
				}
				fmt.Fprintf(&b, "c%v = ?", i)
			}
			return b.String() + " where c0 = ?"
		}
		// The meaning of so many assignments is not checked in SQLite, since that needs a table of as
		// many columns: the text parses, and SQLite counts its parameters.
		src := update(2000)
		s, err := stmt.Check(src)
		is.NotError(t, err)
		is.Equal(t, 2001, s.Parameters())
		is.Equal(t, 2001, s.(*stmt.Update).Where[0].Value.Parameter)
		is.Equal(t, oracle.Parses, oracle.Judge(src).Kind)

		src = update(2001)
		_, err = stmt.Check(src)
		is.Equal(t, fmt.Sprintf("s4ql: not supported: SET beyond SQLite's limit (at offset %v)", strings.LastIndex(src, "c2000")), err.Error())
		is.Equal(t, "too many columns in set list", oracle.Judge(src).Message)
		verify(t, src)
	})

	t.Run("should take conditions of an update and a delete as deep as SQLite allows, and not one deeper", func(t *testing.T) {
		for _, start := range []string{"update t set v = -1 where ", "delete from t where "} {
			where := func(n int) string {
				return start + "v = 1" + strings.Repeat(" and v = 1", n-1)
			}
			src := where(999)
			_, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, oracle.Parses, oracle.Judge(src).Kind)
			src = where(1000)
			_, err = stmt.Check(src)
			is.Equal(t, fmt.Sprintf("s4ql: not supported: WHERE deeper than SQLite's limit (at offset %v)", len(src)-len("and v = 1")), err.Error())
			is.Equal(t, "Expression tree is too large (maximum depth 1000)", oracle.Judge(src).Message)
			verify(t, src)
		}
	})

	t.Run("should take conditions as deep as SQLite allows, and not one deeper", func(t *testing.T) {
		// SQLite allows an expression of 1000 levels. Each AND is one level more than the deeper of its
		// two sides, a condition is two levels, and three with a sign before its value on either side, so a sign in one
		// of the first two conditions takes a level.
		where := func(first string, n int) string {
			return "select * from t where " + first + strings.Repeat(" and v = 1", n-1)
		}
		for _, c := range []struct {
			first string
			most  int
		}{{"v = 1", 999}, {"v is null", 999}, {"v = -1", 998}, {"1 = v", 999}, {"-1 = v", 998}} {
			src := where(c.first, c.most)
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, c.most, len(s.(*stmt.Select).Where))
			is.Equal(t, oracle.Parses, oracle.Judge(src).Kind)
			o, _ := verify(t, src)
			is.Equal(t, supported, o)

			src = where(c.first, c.most+1)
			_, err = stmt.Check(src)
			is.Equal(t, fmt.Sprintf("s4ql: not supported: WHERE deeper than SQLite's limit (at offset %v)", len(src)-len("and v = 1")), err.Error())
			is.Equal(t, "Expression tree is too large (maximum depth 1000)", oracle.Judge(src).Message)
			verify(t, src)
		}
		// A sign in the second condition takes a level too, on either side of its column, and in the
		// third none.
		_, err := stmt.Check(where("v = 1 and -1 = v", 998))
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		_, err = stmt.Check(where("v = 1 and v = -1", 998))
		is.True(t, errors.Is(err, stmt.ErrNotSupported))
		_, err = stmt.Check(where("v = 1 and v = 1 and v = -1", 997))
		is.NotError(t, err)
	})
}

// BenchmarkCheck checks every statement of the corpus once per iteration.
func BenchmarkCheck(b *testing.B) {
	sqls := corpusSQL()
	n := 0
	for _, sql := range sqls {
		n += len(sql)
	}
	b.SetBytes(int64(n))
	for b.Loop() {
		for _, sql := range sqls {
			_, _ = stmt.Check(sql)
		}
	}
}

// BenchmarkCheck_long checks long texts: a SELECT with many result columns, an INSERT with many rows,
// both not supported, and a supported statement with a long comment.
func BenchmarkCheck_long(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		for _, c := range []struct{ name, src string }{
			{"columns", "select " + strings.Repeat("*, ", n) + "* from t"},
			{"rows", "insert into t (v) values " + strings.Repeat("(1), ", n) + "(1)"},
			{"comment", "select * from t /* " + strings.Repeat("x", 10*n) + " */"},
		} {
			b.Run(fmt.Sprintf("%v-%v", c.name, n), func(b *testing.B) {
				b.SetBytes(int64(len(c.src)))
				for b.Loop() {
					_, _ = stmt.Check(c.src)
				}
			})
		}
	}
}
