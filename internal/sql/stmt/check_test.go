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
	{name: "syntax error in a statement that is not supported", src: "update t set v = 1 where", err: "incomplete input"},
	{name: "syntax error after a statement that is not supported", src: "create table t (v1 int primary key) strict without rowid", err: `near "without": syntax error`},
	{name: "unrecognized token", src: "insert into t (v) values (1_)", err: `unrecognized token: "1_"`},

	// More than one statement.
	{name: "second statement", src: "select * from t; select * from t", err: "s4ql: not supported: second statement (at offset 17)"},
	{name: "second statement with a syntax error", src: "drop table t;selec", err: "s4ql: not supported: second statement (at offset 13)"},
	{name: "second statement after comments", src: "drop table t; -- x\n/* y */ ; begin", err: "s4ql: not supported: second statement (at offset 29)"},
	{name: "first statement not supported before a second one", src: "drop table if exists t; drop table t", err: "s4ql: not supported: IF EXISTS (at offset 11)"},
	{name: "first statement a syntax error before a second one", src: "drop tabl t; drop table t", err: `near "tabl": syntax error`},
	{name: "the two tokens of /* at the end", src: "drop table t; /*", err: "s4ql: not supported: second statement (at offset 14)"},

	// Commands that are not supported.
	{name: "update", src: "update t set v = 1", err: "s4ql: not supported: UPDATE (at offset 0)"},
	{name: "begin", src: "begin", err: "s4ql: not supported: BEGIN (at offset 0)"},
	{name: "create view", src: "create view v as select 1", err: "s4ql: not supported: CREATE VIEW (at offset 0)"},
	{name: "explain", src: "explain select * from t", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
	{name: "explain query plan", src: "explain query plan select * from t", err: "s4ql: not supported: EXPLAIN (at offset 0)"},
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
	{name: "if not exists", src: "create table if not exists t (v int primary key) strict, without rowid", err: "s4ql: not supported: IF NOT EXISTS (at offset 13)"},
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
	{name: "a parameter in WHERE", src: "select * from t where v = ?", err: "s4ql: not supported: WHERE clause (at offset 16)"},
	{name: "a parameter in LIMIT", src: "select * from t limit ?", err: "s4ql: not supported: LIMIT clause (at offset 16)"},
	{name: "a parameter in a default", src: "create table t (v int primary key default ?) strict, without rowid", err: `near "?": syntax error`},
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
	{name: "where", src: "select * from t where v = 1", err: "s4ql: not supported: WHERE clause (at offset 16)"},
	{name: "order by", src: "select * from t order by v", err: "s4ql: not supported: ORDER BY clause (at offset 16)"},
	{name: "limit", src: "select * from t limit 1", err: "s4ql: not supported: LIMIT clause (at offset 16)"},
	{name: "a join", src: "select * from t, u", err: "s4ql: not supported: join (at offset 15)"},
	{name: "an alias with as", src: "select * from t as u", err: "s4ql: not supported: alias (at offset 16)"},
	{name: "an alias", src: "select * from t u", err: "s4ql: not supported: alias (at offset 16)"},
	{name: "indexed by", src: "select * from t indexed by i", err: "s4ql: not supported: INDEXED BY or NOT INDEXED (at offset 16)"},
	{name: "a table in parentheses", src: "select * from (t)", err: "s4ql: not supported: join in parentheses (at offset 14)"},
	{name: "a subquery", src: "select * from (select 1)", err: "s4ql: not supported: subquery in FROM (at offset 14)"},
	{name: "a table-valued function", src: "select * from json_each('[]')", err: "s4ql: not supported: table-valued function (at offset 14)"},
	{name: "a compound", src: "select * from t union select * from t", err: "s4ql: not supported: compound SELECT (at offset 16)"},
	{name: "values", src: "values (1)", err: "s4ql: not supported: VALUES (at offset 0)"},
	{name: "with", src: "with c as (select 1) select * from c", err: "s4ql: not supported: WITH clause (at offset 0)"},
	{name: "on after the table", src: "select * from t on 1", err: "s4ql: not supported: ON clause (at offset 16)"},

	// DROP.
	{name: "if exists", src: "drop table if exists t", err: "s4ql: not supported: IF EXISTS (at offset 11)"},
	{name: "drop view", src: "drop view t", err: "s4ql: not supported: DROP VIEW (at offset 5)"},
	{name: "drop index", src: "DROP Index t", err: "s4ql: not supported: DROP INDEX (at offset 5)"},
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
	{name: "a default", src: "create table t (a int primary key, b int not null default 0) strict, without rowid", err: "s4ql: not supported: DEFAULT (at offset 50)"},
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
	{name: "a column and where", src: "select v from t where v = ?", err: "s4ql: not supported: WHERE clause (at offset 16)"},

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
	{name: "NULL as a default", src: "create table t (a int primary key, b text default null) strict, without rowid", err: "s4ql: not supported: DEFAULT (at offset 42)"},
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
	{name: "is null in where", src: "select * from t where v is null", err: "s4ql: not supported: WHERE clause (at offset 16)"},
	{name: "is not null in where", src: "select v from t where v is not null", err: "s4ql: not supported: WHERE clause (at offset 16)"},
	{name: "null in limit", src: "select * from t limit null", err: "s4ql: not supported: LIMIT clause (at offset 16)"},
	{name: "null in an upsert", src: "insert into t (v) values (1) on conflict (v) do update set v = null", err: "s4ql: not supported: ON CONFLICT clause (at offset 29)"},
}

// column of a CREATE TABLE.
func column(raw, value string, offset int, typ stmt.Type, primaryKey, notNull bool) stmt.Column {
	return stmt.Column{Name: name(raw, value, offset), Type: typ, PrimaryKey: primaryKey, NotNull: notNull}
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
			is.Equal(t, fmt.Sprintf("%#v", c.want), fmt.Sprintf("%#v", s))
			verify(t, c.src)
		})
	}

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
