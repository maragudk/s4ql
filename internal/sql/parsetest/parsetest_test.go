package parsetest_test

import (
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/token"
)

// TestRun_corpus compares SQLite with itself on the whole corpus, or on every tenth entry with
// -short and under the address sanitizer, which makes the run about ten times slower. It must give
// no disagreement at all.
func TestRun_corpus(t *testing.T) {
	sqls := corpusSQL(t)
	if testing.Short() || asanEnabled {
		sqls = every(sqls, 10)
	}
	r := parsetest.Test(t, parsetest.Oracle(), sqls)
	is.Equal(t, len(sqls), r.Total)
	is.Equal(t, 0, r.Disagreements())
	is.Equal(t, 0, len(r.Groups))
	is.Equal(t, r.Total, r.Agreed+r.NotSaid)
	t.Logf("%v statements, SQLite does not say on %v", r.Total, r.NotSaid)
	if !testing.Short() && !asanEnabled {
		// The texts on which SQLite does not say escape every comparison with it.
		is.Equal(t, 308, r.NotSaid, "the texts of the corpus on which SQLite does not say; change the pinned number only on purpose, and say why in the commit")
	}
}

func TestRun(t *testing.T) {
	t.Run("should compare the offset of a variable #N at the end of a statement, which SQLite gives", func(t *testing.T) {
		noOffset := parsetest.ParserFunc(func(src string) parsetest.Result {
			r := parsetest.Oracle().Parse(src)
			if !r.OK {
				r.Offset = -1
			}
			return r
		})
		r := parsetest.Run(noOffset, []string{"values(0),(0) union select #0", "values(0),(a) union select #0", "select #0", "select 1",
			"create table t (x) foo, bar"})
		is.Equal(t, 5, r.Total)
		is.Equal(t, 1, r.Agreed)
		is.Equal(t, 1, r.NotSaid)
		is.Equal(t, 3, r.Disagreements())
		is.Equal(t, 1, len(r.Groups))
		is.Equal(t, parsetest.Offset, r.Groups[0].Mismatch)
		is.Equal(t, "near VARIABLE: syntax error: parser -1", r.Groups[0].Key)
		is.True(t, strings.HasPrefix(r.String(), "5 texts: 1 agree, 3 disagree, SQLite does not say on 1\n"), r.String())
	})
}

// TestRun_mutants compares SQLite with itself on one-token mutations of a sample of the corpus.
func TestRun_mutants(t *testing.T) {
	var sqls []string
	for i, sql := range every(corpusSQL(t), 50) {
		sqls = append(sqls, parsetest.Mutants(sql, uint64(i), 3)...)
	}
	r := parsetest.Test(t, parsetest.Oracle(), sqls)
	is.True(t, r.Total > 5000)
	is.Equal(t, 0, r.Disagreements())
}

// TestRun_wrong proves that the harness finds parsers that are wrong in known ways, and puts each
// disagreement in the right group.
func TestRun_wrong(t *testing.T) {
	sqls := every(corpusSQL(t), 40)
	verdicts := map[string]oracle.Verdict{}
	count := map[oracle.Kind]int{}
	for _, sql := range sqls {
		v := oracle.Judge(sql)
		verdicts[sql] = v
		count[v.Kind]++
	}
	is.True(t, count[oracle.Parses] > 1000 && count[oracle.SyntaxError] > 1000, "sample too small")

	t.Run("should find a parser that accepts everything, keyed by SQLite's message", func(t *testing.T) {
		r := parsetest.Run(parsetest.ParserFunc(func(src string) parsetest.Result {
			return parsetest.Result{OK: true, Offset: -1, Tail: verdicts[src].Tail}
		}), sqls)
		is.Equal(t, count[oracle.SyntaxError], r.Disagreements())
		is.Equal(t, count[oracle.NotSaid], r.NotSaid)
		for _, g := range r.Groups {
			is.Equal(t, parsetest.AcceptsInvalid, g.Mismatch)
			for _, d := range g.Examples {
				is.Equal(t, parsetest.Pattern(d.Verdict.Message), g.Key)
			}
		}
		is.True(t, hasGroup(r, parsetest.AcceptsInvalid, "incomplete input"))
		is.True(t, hasGroup(r, parsetest.AcceptsInvalid, "near ID: syntax error"))
	})

	t.Run("should find a parser that is off by one in the offset", func(t *testing.T) {
		r := parsetest.Run(wrong(func(v oracle.Verdict, r *parsetest.Result) {
			if !r.OK && r.Offset >= 0 {
				r.Offset++
			}
		}), sqls)
		expected := 0
		for _, v := range verdicts {
			if v.Kind == oracle.SyntaxError && v.Offset >= 0 {
				expected++
			}
		}
		is.Equal(t, expected, r.Disagreements())
		for _, g := range r.Groups {
			is.Equal(t, parsetest.Offset, g.Mismatch)
			is.True(t, strings.HasSuffix(g.Key, ": offset +1"), g.Key)
		}
	})

	t.Run("should find a parser that gives an offset where SQLite gives none", func(t *testing.T) {
		r := parsetest.Run(wrong(func(v oracle.Verdict, r *parsetest.Result) {
			if !r.OK && r.Offset == -1 {
				r.Offset = 0
			}
		}), sqls)
		is.True(t, r.Disagreements() > 0)
		is.True(t, hasGroup(r, parsetest.Offset, "incomplete input: SQLite -1"))
	})

	t.Run("should find a parser that differs in one message", func(t *testing.T) {
		r := parsetest.Run(wrong(func(v oracle.Verdict, r *parsetest.Result) {
			if r.Message == "incomplete input" {
				r.Message = "unexpected end of input"
			}
		}), sqls)
		expected := 0
		for _, v := range verdicts {
			if v.Kind == oracle.SyntaxError && v.Message == "incomplete input" {
				expected++
			}
		}
		is.True(t, expected > 0)
		is.Equal(t, expected, r.Disagreements())
		is.Equal(t, 1, len(r.Groups))
		is.Equal(t, parsetest.Message, r.Groups[0].Mismatch)
		is.Equal(t, "SQLite incomplete input, parser unexpected end of input", r.Groups[0].Key)
		is.Equal(t, expected, r.Groups[0].Count)
	})

	t.Run("should find a parser that rejects valid statements", func(t *testing.T) {
		r := parsetest.Run(wrong(func(v oracle.Verdict, r *parsetest.Result) {
			if r.OK && strings.HasPrefix(strings.ToLower(v.Message), "no such table") {
				*r = parsetest.Result{Message: `near "x": syntax error`, Offset: 0}
			}
		}), sqls)
		is.True(t, r.Disagreements() > 0)
		is.Equal(t, 1, len(r.Groups))
		is.Equal(t, parsetest.RejectsValid, r.Groups[0].Mismatch)
		is.Equal(t, "near ID: syntax error", r.Groups[0].Key)
	})

	t.Run("should find a parser that does not stop at the semicolon", func(t *testing.T) {
		toEnd := parsetest.ParserFunc(func(src string) parsetest.Result {
			r := parsetest.Oracle().Parse(src)
			if r.OK {
				r.Tail = len(src)
			}
			return r
		})
		r := parsetest.Run(toEnd, []string{"select 1; select 2", "select 1;;", "select 1;", "select 1"})
		is.Equal(t, 2, r.Disagreements())
		is.True(t, hasGroup(r, parsetest.Tail, "tail +9"))
		is.True(t, hasGroup(r, parsetest.Tail, "tail +1"))
	})

	t.Run("should keep the shortest examples of a group, and the same report in any order", func(t *testing.T) {
		accept := parsetest.ParserFunc(func(src string) parsetest.Result {
			return parsetest.Result{OK: true, Offset: -1}
		})
		reversed := slices.Clone(sqls)
		slices.Reverse(reversed)
		r1, r2 := parsetest.Run(accept, sqls), parsetest.Run(accept, reversed)
		is.Equal(t, r1.String(), r2.String())
		for _, g := range r1.Groups {
			is.True(t, len(g.Examples) <= 5)
			for i := 1; i < len(g.Examples); i++ {
				is.True(t, len(g.Examples[i-1].SQL) <= len(g.Examples[i].SQL))
			}
		}
	})
}

func TestCompare(t *testing.T) {
	syntaxError := oracle.Verdict{Kind: oracle.SyntaxError, Message: `near "frm": syntax error`, Offset: 9, Code: 1, Tail: 12}
	parses := oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: 9}
	notSaid := oracle.Verdict{Kind: oracle.NotSaid, Reason: oracle.StoppedEarly, Message: "unknown table option: foo", Offset: -1}

	tests := []struct {
		name     string
		verdict  oracle.Verdict
		result   parsetest.Result
		mismatch parsetest.Mismatch
		key      string
	}{
		{name: "should agree on a syntax error", verdict: syntaxError,
			result: parsetest.Result{Message: `near "frm": syntax error`, Offset: 9}},
		{name: "should agree on a statement that parses, whatever the result's offset", verdict: parses,
			result: parsetest.Result{OK: true, Offset: 3, Tail: 9}},
		{name: "should not compare where SQLite does not say", verdict: notSaid,
			result: parsetest.Result{OK: true}},
		{name: "should find a parser that accepts what SQLite rejects", verdict: syntaxError,
			result: parsetest.Result{OK: true}, mismatch: parsetest.AcceptsInvalid, key: "near ID: syntax error"},
		{name: "should find a parser that rejects what SQLite parses", verdict: parses,
			result: parsetest.Result{Message: "incomplete input", Offset: -1}, mismatch: parsetest.RejectsValid, key: "incomplete input"},
		{name: "should find another message", verdict: syntaxError,
			result:   parsetest.Result{Message: `near "t": syntax error`, Offset: 9},
			mismatch: parsetest.Message, key: "SQLite near ID: syntax error, parser near ID: syntax error"},
		{name: "should find another offset", verdict: syntaxError,
			result: parsetest.Result{Message: `near "frm": syntax error`, Offset: 7}, mismatch: parsetest.Offset, key: "near ID: syntax error: offset -2"},
		{name: "should find an offset where the parser gives none", verdict: syntaxError,
			result: parsetest.Result{Message: `near "frm": syntax error`, Offset: -1}, mismatch: parsetest.Offset, key: "near ID: syntax error: parser -1"},
		{name: "should find another tail", verdict: parses,
			result: parsetest.Result{OK: true, Tail: 8}, mismatch: parsetest.Tail, key: "tail -1"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, found := parsetest.Compare("select 1 frm t", test.verdict, test.result)
			is.Equal(t, test.mismatch != 0, found)
			is.Equal(t, test.mismatch, d.Mismatch)
			is.Equal(t, test.key, d.Key)
		})
	}
}

func TestPattern(t *testing.T) {
	tests := []struct {
		msg, expected string
	}{
		{`near "frm": syntax error`, "near ID: syntax error"},
		{`near "select": syntax error`, "near SELECT: syntax error"},
		{`near ")": syntax error`, "near RP: syntax error"},
		{`near "select 1": syntax error`, "near ?: syntax error"},
		{"incomplete input", "incomplete input"},
		{`unrecognized token: "'abc"`, "unrecognized token: STRING..."},
		{`unrecognized token: "x'0'"`, "unrecognized token: BLOB..."},
		{`unrecognized token: "[a"`, "unrecognized token: ID..."},
		{`unrecognized token: "1_"`, "unrecognized token: QNUMBER"},
		{`unrecognized token: "1x"`, "unrecognized token: NUMBER..."},
		{`unrecognized token: "!"`, `unrecognized token: "!"`},
		{"no such table: t1", "no such table: ..."},
		{`syntax error after column name "a"`, `syntax error after column name "..."`},
		{"Recursion limit", "Recursion limit"},
	}

	for _, test := range tests {
		t.Run("should give the pattern of "+test.msg, func(t *testing.T) {
			is.Equal(t, test.expected, parsetest.Pattern(test.msg))
		})
	}
}

func TestMutants(t *testing.T) {
	t.Run("should give the same mutants for the same seed and text", func(t *testing.T) {
		src := "select a, b from t where c = 1"
		is.EqualSlice(t, parsetest.Mutants(src, 7, 20), parsetest.Mutants(src, 7, 20))
		is.True(t, !slices.Equal(parsetest.Mutants(src, 7, 20), parsetest.Mutants(src, 8, 20)))
	})

	t.Run("should change one token each time", func(t *testing.T) {
		src := "select a, b from t where c = 1"
		original := words(src)
		for _, m := range parsetest.Mutants(src, 1, 200) {
			got := words(m)
			is.True(t, m != src)
			diff := len(got) - len(original)
			is.True(t, diff >= -1 && diff <= 1, m)
			if diff == 0 {
				changed := 0
				for i := range got {
					if got[i] != original[i] {
						changed++
					}
				}
				is.True(t, changed == 1 || changed == 2, m) // a replacement, or a swap
			}
		}
	})

	t.Run("should mutate only the text before a NUL byte", func(t *testing.T) {
		for _, m := range parsetest.Mutants("select 1\x00 frm", 1, 20) {
			is.True(t, !strings.Contains(m, "frm"), m)
		}
	})

	t.Run("should give no mutants of a text without tokens", func(t *testing.T) {
		is.Equal(t, 0, len(parsetest.Mutants(" -- comment", 1, 5)))
		is.Equal(t, 0, len(parsetest.Mutants("", 1, 5)))
	})
}

func FuzzMutants(f *testing.F) {
	sqls, err := corpus.Read("own")
	if err != nil {
		f.Fatal(err)
	}
	for i, sql := range sqls {
		f.Add(sql, uint64(i))
	}

	f.Fuzz(func(t *testing.T, src string, seed uint64) {
		if len(src) > 1_000 {
			t.Skip()
		}
		mutants := parsetest.Mutants(src, seed, 3)
		is.EqualSlice(t, mutants, parsetest.Mutants(src, seed, 3))
		for _, m := range mutants {
			if d, found, _ := parsetest.Check(parsetest.Oracle(), m); found {
				t.Fatal(d)
			}
		}
	})
}

// corpusSQL gives the statements of the corpus.
func corpusSQL(t *testing.T) []string {
	t.Helper()
	var sqls []string
	for _, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
	}
	return sqls
}

// every nth of the statements.
func every(sqls []string, n int) []string {
	var some []string
	for i, sql := range sqls {
		if i%n == 0 {
			some = append(some, sql)
		}
	}
	return some
}

// wrong is SQLite as a parser, changed by change.
func wrong(change func(v oracle.Verdict, r *parsetest.Result)) parsetest.Parser {
	return parsetest.ParserFunc(func(src string) parsetest.Result {
		v := oracle.Judge(src)
		r := parsetest.Oracle().Parse(src)
		change(v, &r)
		return r
	})
}

func hasGroup(r parsetest.Report, m parsetest.Mismatch, key string) bool {
	for _, g := range r.Groups {
		if g.Mismatch == m && g.Key == key {
			return true
		}
	}
	return false
}

// words of src, without spaces and comments.
func words(src string) []string {
	var words []string
	for _, t := range token.Tokens(src) {
		if t.Kind != token.Space && t.Kind != token.Comment {
			words = append(words, t.Text(src))
		}
	}
	return words
}
