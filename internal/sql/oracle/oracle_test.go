package oracle_test

import (
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

func TestJudge(t *testing.T) {
	tests := []struct {
		name     string
		src      string
		expected oracle.Verdict
	}{
		// Parses, with no error.
		{name: "should parse a statement that prepares", src: "select 1",
			expected: parses(8)},
		{name: "should give the tail after the semicolon of the first statement", src: "select 1; select 2",
			expected: parses(9)},
		{name: "should give the end of the text as the tail when the statement has no semicolon", src: "select 1 \n",
			expected: parses(10)},
		{name: "should skip empty statements before the first", src: ";; select 1;",
			expected: parses(12)},
		{name: "should parse a text without a statement", src: " -- comment",
			expected: parses(11)},
		{name: "should parse the empty text", src: "",
			expected: parses(0)},
		{name: "should read up to the first NUL byte and no further", src: "select 1 \x00 frm",
			expected: parses(9)},
		{name: "should parse the text before a NUL byte at the start as empty", src: "\x00select",
			expected: parses(0)},
		{name: "should parse invalid UTF-8 in a name", src: "select 1 as \xff\xfe",
			expected: parses(14)},
		{name: "should parse a semicolon inside the arguments of a virtual table", src: "create virtual table v using m(a;b)",
			expected: parses(35)},

		// Parses, with an error that SQLite raised when the statement's end arrived.
		{name: "should parse a statement with a missing table", src: "select * from nosuch",
			expected: parsesWith(20, "no such table: nosuch", -1)},
		{name: "should parse a statement with a missing column, with the offset of the column", src: "select nosuch;",
			expected: parsesWith(14, "no such column: nosuch", 7)},
		{name: "should parse a statement with an error at its semicolon, before a second statement", src: "insert into nosuch values (1); select 2",
			expected: parsesWith(30, "no such table: nosuch", -1)},
		{name: "should parse an unknown table option at the end of the text", src: "create table t (x) foo",
			expected: parsesWith(22, "unknown table option: foo", -1)},
		{name: "should parse an unknown table option at the end of the text with a space after it", src: "create table t (x) foo ",
			expected: parsesWith(23, "unknown table option: foo", -1)},
		{name: "should parse a variable number out of range at the end, with the offset of the variable", src: "select ?0",
			expected: parsesWith(9, "variable number must be between ?1 and ?32766", 7)},
		{name: "should parse an ORDER BY before UNION", src: "select 1 union select 2 order by 1 union select 3",
			expected: parsesWith(49, "ORDER BY clause should come after UNION not before", -1)},
		{name: "should parse a statement with a valid digit separator and an error at the end", src: "select 1_000 from nosuch",
			expected: parsesWith(24, "no such table: nosuch", -1)},
		{name: "should parse a denied PRAGMA, which is denied after the parse", src: "pragma soft_heap_limit = 1",
			expected: oracle.Verdict{Kind: oracle.Parses, Message: "not authorized", Offset: -1, Code: 23, Tail: 26}},

		// Syntax errors.
		{name: "should give a syntax error with its offset", src: "select 1 frm t",
			expected: syntaxError(`near "t": syntax error`, 13, 14)},
		{name: "should give incomplete input, without an offset", src: "select * from",
			expected: syntaxError("incomplete input", -1, 13)},
		{name: "should give an unrecognized token, with the tail at it", src: "select 'abc",
			expected: syntaxError(`unrecognized token: "'abc"`, 7, 7)},
		{name: "should end a string at a NUL byte", src: "select 'a\x00b'",
			expected: syntaxError(`unrecognized token: "'a"`, 7, 7)},
		{name: "should give the syntax error of a variable #N, raised by an action", src: "select #1 from t",
			expected: syntaxError(`near "#1": syntax error`, 7, 14)},
		{name: "should give the syntax error of a variable #N at the end with its offset", src: "select #1",
			expected: syntaxError(`near "#1": syntax error`, 7, 9)},
		{name: "should give a misplaced digit separator as an unrecognized token without an offset", src: "select 1_ from nosuch",
			expected: syntaxError(`unrecognized token: "1_"`, -1, 14)},
		{name: "should let a later syntax error win over a misplaced digit separator", src: "select 1_ 2",
			expected: syntaxError(`near "2": syntax error`, 10, 11)},
		{name: "should give a syntax error that comes after an action's error on the same token", src: "create table t (x) foo bar",
			expected: syntaxError(`near "bar": syntax error`, 23, 26)},
		{name: "should give a syntax error on the two tokens of /* at the end", src: "select 1 /*",
			expected: syntaxError(`near "*": syntax error`, 10, 11)},
		{name: "should give a syntax error for a keyword that cannot be a name", src: "create table select (x)",
			expected: syntaxError(`near "select": syntax error`, 13, 19)},

		// Not said.
		{name: "should not say when an action stops the parse at a later token", src: "create table t (x) foo, bar",
			expected: notSaid(oracle.StoppedEarly, "unknown table option: foo", -1, 23)},
		{name: "should not say when an action stops the parse at the last token, which ends the text", src: "create table t (x) foo,",
			expected: notSaid(oracle.StoppedEarly, "unknown table option: foo", -1, 23)},
		{name: "should not say when a name is reserved", src: "create table sqlite_x (x)",
			expected: notSaid(oracle.StoppedEarly, "object name reserved for internal use: sqlite_x", -1, 23)},
		{name: "should not say when a CTE has a collation on a column name", src: "with x(a collate nocase) as (select 1) select 1",
			expected: notSaid(oracle.StoppedEarly, `syntax error after column name "a"`, -1, 24)},
		{name: "should not say when a trigger is on a system table", src: "create trigger tr after insert on sqlite_schema begin select 1; end",
			expected: notSaid(oracle.StoppedEarly, "cannot create trigger on system table", -1, 53)},
		{name: "should not say when a variable #N may have been overwritten by a later message", src: "select 0 limit 0 union select #0",
			expected: notSaid(oracle.Overwritten, "LIMIT clause should come after UNION not before", -1, 32)},
		{name: "should not say when a misplaced digit separator may have been overwritten by a later message", src: "select 0 limit 0 union select 1_",
			expected: notSaid(oracle.Overwritten, "LIMIT clause should come after UNION not before", -1, 32)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			equal(t, test.expected, oracle.Judge(test.src))
		})
	}

	t.Run("should create the missing table of a trigger and judge again", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on t begin select 1; end")
		equal(t, oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: 55, Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should create a view for an INSTEAD OF trigger", func(t *testing.T) {
		v := oracle.Judge(`create trigger tr instead of insert on "v ""1""" begin select 1; end;`)
		equal(t, oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: 69, Setup: []string{`create view main."v ""1""" as select 1 as c`}}, v)
	})

	t.Run("should attach an unknown database", func(t *testing.T) {
		v := oracle.Judge("create table aux.t (x) foo")
		equal(t, oracle.Verdict{Kind: oracle.Parses, Message: "unknown table option: foo", Offset: -1, Code: 1, Tail: 26,
			Setup: []string{"attach ':memory:' as aux"}}, v)
	})

	t.Run("should attach an unknown database written in brackets, and create a table in it", func(t *testing.T) {
		v := oracle.Judge("create trigger [aux].tr after insert on t begin select 1; end")
		equal(t, oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: 61,
			Setup: []string{"attach ':memory:' as [aux]", `create table [aux]."t" (c)`}}, v)
	})

	t.Run("should not say for a trigger in main on a table in another schema, which no schema can fix", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on aux.t begin select 1; end")
		equal(t, notSaid(oracle.StoppedEarly, "trigger tr cannot reference objects in database aux", 15, 45), v)
	})

	t.Run("should not say on the error of a variable #N at the end that needed a schema, which another message replaces in the empty database", func(t *testing.T) {
		v := oracle.Judge("create view aux.v as select #1")
		equal(t, oracle.Verdict{Kind: oracle.NotSaid, Reason: oracle.Overwritten, Message: `near "#1": syntax error`, Offset: 28,
			Code: 1, Tail: 30, Setup: []string{"attach ':memory:' as aux"}}, v)
	})

	t.Run("should give the error of a variable #N inside a trigger, which needed a table", func(t *testing.T) {
		v := oracle.Judge("create trigger r after insert on t begin select 1 limit #1; end")
		equal(t, oracle.Verdict{Kind: oracle.SyntaxError, Message: `near "#1": syntax error`, Offset: 56,
			Code: 1, Tail: 59, Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should give the offset of a variable #N, raised with the end of the statement or before it", func(t *testing.T) {
		tests := []struct {
			src  string
			tail int
		}{
			// The commands whose action at the end loads the schema even after an error, which on a
			// fresh connection loses the offset.
			{"select #0", 9},
			{"select #0;", 10},
			{"select #0; select 1", 10},
			{";; select #0", 12},
			{"select 1 where #0", 17},
			{"explain select #0", 17},
			{"explain query plan select #0", 28},
			{"with c as (select 1) select #0", 30},
			{"create view v as select #0", 26},
			{"create temp view v as select #0", 31},
			{"attach 'x' as #0", 16},
			{"detach #0", 9},
			// A multi-row VALUES, which loads the schema while SQLite parses it when its rows are
			// constant and no common table expression came before.
			{"VALUES(0),(0)UNION SELECT#0", 27},
			{"values(0),(0) union select #0", 29},
			{"values(0),(0),(1) union select #0", 33},
			{"explain values(0),(0) union select #0", 37},
			{"select * from (values(0),(0)) union select #0", 45},
			{"create view v as values(0),(0) union select #0", 46},
			{"attach (values(0),(0)) as #0", 28},
			{"detach (values(0),(0)) + #0", 27},
			{"values((0)+(0)),(0) union select #0", 35},
			{"select with from (values(0),(0)) union select #0", 48},
			{"values(0),(a) union select #0", 29},
			{"values(0),(random()) union select #0", 36},
			{"select * from (with c as (select 1) select 1), (values(0),(0)) union select #0", 78},
			{"create view v as values(0),(a) union select #0", 46},
			{"create view v as with c as (select 1) values(0),(0) union select #0", 67},
			{"attach (values(0),(a)) as #0", 28},
			{"with c as (select 1) select * from (values(0),(0)) union select #0", 66},
			{"with c as (values(0),(0)) select #0", 35},
			{"explain with c as (select 1) values(0),(0) union select #0", 58},
			{"select * from (values(0)) union select #0", 41},
			{"select (values(0)), (1) union select #0", 39},
			{"values((0),(0)) union select #0", 31},
			{"with i(x) as (values(1) union all select x<1 from i order by 1) select x from i limit 0,#0", 90},
			// Raised on the token after the variable, before the statement's end.
			{"values(0),(0) union select #0 from t", 34},
			{"select #0 from (values(0),(0))", 14},
			// The actions at the end of these commands do not load the schema after an error.
			{"insert into t values(0),(0) returning #0", 40},
			{"delete from t where #0 in (values(0),(0))", 25},
			{"create table t as values(0),(0) union select #0", 47},
		}
		for _, test := range tests {
			offset := strings.Index(test.src, "#0")
			equal(t, syntaxError(`near "#0": syntax error`, offset, test.tail), oracle.Judge(test.src))
		}

		v := oracle.Judge("create trigger r after insert on t begin values(0),(0) union select #0; end")
		equal(t, oracle.Verdict{Kind: oracle.SyntaxError, Message: `near "#0": syntax error`, Offset: 68,
			Code: 1, Tail: 71, Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should not say on the error of a variable #N at the end of a statement with VALUES that needed a schema", func(t *testing.T) {
		v := oracle.Judge("create view aux.v as values(0),(0) union select #0")
		equal(t, oracle.Verdict{Kind: oracle.NotSaid, Reason: oracle.Overwritten, Message: `near "#0": syntax error`, Offset: 48,
			Code: 1, Tail: 50, Setup: []string{"attach ':memory:' as aux"}}, v)
	})

	t.Run("should attach the schema of a missing table in an unknown schema", func(t *testing.T) {
		v := oracle.Judge("alter table aux.t1 add column d integer not null default 1")
		equal(t, oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: 58,
			Setup: []string{`attach ':memory:' as "aux"`, `create table "aux"."t1" (c)`}}, v)
	})

	t.Run("should give a syntax error in a trigger's body once the table exists", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on t begin select 1 frm x; end")
		equal(t, oracle.Verdict{Kind: oracle.SyntaxError, Message: `near "x": syntax error`, Offset: 55, Code: 1, Tail: 56,
			Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should not say when the parse stopped at a semicolon inside a trigger's body", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on t begin select 1 as end; select ?0; end")
		equal(t, oracle.Verdict{Kind: oracle.NotSaid, Reason: oracle.InsideTrigger, Message: "variable number must be between ?1 and ?32766",
			Offset: 66, Code: 1, Tail: 69, Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should not say when the parse stopped at the end of the text inside a trigger's body", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on t begin delete from t indexed by i")
		equal(t, oracle.Verdict{Kind: oracle.NotSaid, Reason: oracle.InsideTrigger,
			Message: "the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers",
			Offset:  -1, Code: 1, Tail: 68, Setup: []string{`create table main."t" (c)`}}, v)
	})

	t.Run("should not say when the parse stopped at a semicolon inside the arguments of a virtual table", func(t *testing.T) {
		// SQLite counts the arguments as columns, and each argument is added when the first token
		// of the next arrives, which here is always a semicolon.
		src := "create virtual table v using m(" + strings.Repeat("; a, ", 2100) + ")"
		v := oracle.Judge(src)
		is.Equal(t, oracle.NotSaid, v.Kind)
		is.Equal(t, oracle.InsideArguments, v.Reason)
		is.Equal(t, "too many columns on v", v.Message)
		is.Equal(t, byte(';'), src[v.Tail-1])
	})

	t.Run("should parse a trigger whose error came at its end", func(t *testing.T) {
		v := oracle.Judge("create trigger tr after insert on t begin select 1; end; select 2")
		is.Equal(t, oracle.Parses, v.Kind)
		is.Equal(t, 56, v.Tail)
	})

	t.Run("should keep the verdict without the object when the object cannot be created", func(t *testing.T) {
		v := oracle.Judge("alter table sqlite_x add column y")
		equal(t, oracle.Verdict{Kind: oracle.Parses, Message: "no such table: sqlite_x", Offset: -1, Code: 1, Tail: 33}, v)

		v = oracle.Judge("create trigger tr after insert on sqlite_x begin select 1; end")
		equal(t, notSaid(oracle.StoppedEarly, "no such table: main.sqlite_x", -1, 48), v)
	})

	t.Run("should give SQLite's limit on the depth of the parser's stack as stopped early", func(t *testing.T) {
		// SQLITE_LIMIT_PARSER_DEPTH is 2500 by default.
		v := oracle.Judge("select " + strings.Repeat("(", 2493) + "1" + strings.Repeat(")", 2493))
		is.Equal(t, oracle.Parses, v.Kind)

		v = oracle.Judge("select " + strings.Repeat("(", 2494) + "1" + strings.Repeat(")", 2494))
		is.Equal(t, oracle.NotSaid, v.Kind)
		is.Equal(t, oracle.StoppedEarly, v.Reason)
		is.Equal(t, "Recursion limit", v.Message)
	})

	t.Run("should give SQLite's limit on the depth of an expression", func(t *testing.T) {
		// SQLITE_LIMIT_EXPR_DEPTH is 1000 by default.
		v := oracle.Judge("select " + strings.Repeat("~", 999) + "1")
		is.Equal(t, oracle.Parses, v.Kind)
		is.Equal(t, "", v.Message)

		v = oracle.Judge("select " + strings.Repeat("~", 1000) + "1")
		is.Equal(t, oracle.Parses, v.Kind)
		is.Equal(t, "Expression tree is too large (maximum depth 1000)", v.Message)
	})

	t.Run("should give the same verdict on every call", func(t *testing.T) {
		for _, src := range []string{"select 1", "select 1 frm t", "create table t (x) foo, bar", "create trigger tr after insert on t begin select 1; end"} {
			first := oracle.Judge(src)
			for range 10 {
				equal(t, first, oracle.Judge(src))
			}
		}
	})
}

func TestKind_String(t *testing.T) {
	t.Run("should name each kind and reason", func(t *testing.T) {
		is.Equal(t, "parses", oracle.Parses.String())
		is.Equal(t, "syntax error", oracle.SyntaxError.String())
		is.Equal(t, "not said", oracle.NotSaid.String())
		is.Equal(t, "Kind(9)", oracle.Kind(9).String())
		for r, name := range map[oracle.Reason]string{0: "", oracle.StoppedEarly: "stopped early", oracle.InsideTrigger: "inside trigger",
			oracle.InsideArguments: "inside arguments", oracle.Overwritten: "overwritten", oracle.Unsettled: "unsettled",
			oracle.NotParsed: "not parsed", 9: "Reason(9)"} {
			is.Equal(t, name, r.String())
		}
	})
}

func TestIsSyntaxMessage(t *testing.T) {
	t.Run("should match the three messages of SQLite's parser front end", func(t *testing.T) {
		for _, msg := range []string{`near "x": syntax error`, `near """: syntax error`, "incomplete input", `unrecognized token: "'a"`} {
			is.True(t, oracle.IsSyntaxMessage(msg), msg)
		}
	})

	t.Run("should not match other messages", func(t *testing.T) {
		for _, msg := range []string{`syntax error after column name "a"`, "no such table: t", `near "": syntax error`,
			`unrecognized token: ""`, "incomplete input ", `syntax error near "LIMIT"`} {
			is.True(t, !oracle.IsSyntaxMessage(msg), msg)
		}
	})
}

func FuzzJudge(f *testing.F) {
	sqls, err := corpus.Read("own")
	if err != nil {
		f.Fatal(err)
	}
	for _, sql := range sqls {
		f.Add(sql)
	}

	f.Fuzz(func(t *testing.T, src string) {
		if len(src) > 10_000 {
			return
		}
		v := oracle.Judge(src)
		equal(t, v, oracle.Judge(src))
		check(t, src, v)
	})
}

// TestJudge_corpus checks the properties of [check] on the whole corpus, or on every tenth entry
// with -short and under the address sanitizer, which makes the run about ten times slower.
func TestJudge_corpus(t *testing.T) {
	entries := corpus.All()
	for i, e := range entries {
		if (testing.Short() || asanEnabled) && i%10 != 0 {
			continue
		}
		check(t, e.SQL, oracle.Judge(e.SQL))
	}
}

// check that a verdict on src has the properties that every verdict must have, and those that
// follow from what it claims:
//
//   - Its fields are in range.
//   - A syntax error names the token at its offset, and one near a token always has an offset.
//   - A statement that parses is complete: with ) in place of its end, it has the syntax error
//     near ")", at that ), because no statement of SQLite's grammar can go on with ).
//   - A syntax error is the first: the text before the token that it names has no syntax error that
//     names an earlier token, but for a #N right before that token.
//   - Text after a NUL byte changes nothing.
func check(t *testing.T, src string, v oracle.Verdict) {
	t.Helper()
	end := len(src)
	if i := strings.IndexByte(src, 0); i >= 0 {
		end = i
	}
	is.True(t, v.Offset >= -1 && v.Offset < max(end, 1), "offset out of range")

	switch v.Kind {
	case oracle.Parses:
		is.True(t, v.Tail >= 0 && v.Tail <= end, "tail out of range")
		is.True(t, v.Tail == end || src[v.Tail-1] == ';', "tail not after a semicolon or at the end")
		is.True(t, v.Message == "" || !oracle.IsSyntaxMessage(v.Message), "parses with a syntax message")

		stmt := src[:v.Tail]
		tokens := token.Tokens(stmt)
		if len(tokens) > 0 && tokens[len(tokens)-1].Kind == token.Semi {
			stmt = stmt[:len(stmt)-1]
			tokens = token.Tokens(stmt)
		}
		if len(fed(tokens)) == 0 || tokens[len(tokens)-1].Kind == token.Comment {
			// A text without a statement has nothing to continue, and a comment at the end would
			// swallow the ).
			break
		}
		probe := oracle.Judge(stmt + " )")
		is.Equal(t, oracle.SyntaxError, probe.Kind, "not complete: "+stmt)
		is.Equal(t, `near ")": syntax error`, probe.Message, "not complete: "+stmt)
		is.Equal(t, len(stmt)+1, probe.Offset, "not complete: "+stmt)

	case oracle.SyntaxError:
		is.True(t, v.Tail >= 0 && v.Tail <= end, "tail out of range")
		is.True(t, oracle.IsSyntaxMessage(v.Message), "syntax error with another message")
		is.Equal(t, sqlite.ResultCode(1), v.Code)
		if v.Offset < 0 {
			is.True(t, !strings.HasPrefix(v.Message, `near "`), "a syntax error near a token without an offset")
			break
		}
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(v.Message, "near "), "unrecognized token: "), ": syntax error")
		name = name[1 : len(name)-1]
		is.True(t, strings.HasPrefix(src[v.Offset:], name), "the offset is not at the token named")

		// The error of a #N is raised when the token after it arrives, and a syntax error on that
		// token wins, so a #N right before the token may give its error in the text before it.
		prefix := oracle.Judge(src[:v.Offset])
		is.True(t, prefix.Kind != oracle.SyntaxError || prefix.Offset == -1 || prefix.Offset == lastVariable(src[:v.Offset]),
			"syntax error before the first: "+prefix.Message)

	case oracle.NotSaid:
		is.True(t, v.Reason != 0, "no reason")
		is.True(t, v.Tail >= -1 && v.Tail <= end, "tail out of range")

	default:
		t.Fatalf("kind %v", v.Kind)
	}

	if end == len(src) {
		equal(t, v, oracle.Judge(src+"\x00 drop table t; frm"))
	}
}

// lastVariable gives the offset of the last token of src if it is a variable #N, or -2.
func lastVariable(src string) int {
	tokens := fed(token.Tokens(src))
	if len(tokens) == 0 {
		return -2
	}
	last := tokens[len(tokens)-1]
	text := last.Text(src)
	if last.Kind != token.Variable || len(text) < 2 || text[0] != '#' || text[1] < '0' || text[1] > '9' {
		return -2
	}
	return last.Offset
}

func fed(tokens []token.Token) []token.Token {
	var fed []token.Token
	for _, t := range tokens {
		if t.Kind != token.Space && t.Kind != token.Comment && t.Kind != token.Semi {
			fed = append(fed, t)
		}
	}
	return fed
}

func parses(tail int) oracle.Verdict {
	return oracle.Verdict{Kind: oracle.Parses, Offset: -1, Tail: tail}
}

func parsesWith(tail int, msg string, offset int) oracle.Verdict {
	return oracle.Verdict{Kind: oracle.Parses, Message: msg, Offset: offset, Code: 1, Tail: tail}
}

func syntaxError(msg string, offset, tail int) oracle.Verdict {
	return oracle.Verdict{Kind: oracle.SyntaxError, Message: msg, Offset: offset, Code: 1, Tail: tail}
}

func notSaid(reason oracle.Reason, msg string, offset, tail int) oracle.Verdict {
	return oracle.Verdict{Kind: oracle.NotSaid, Reason: reason, Message: msg, Offset: offset, Code: 1, Tail: tail}
}

// equal verdicts, field by field, so that a failure says which field differs.
func equal(t *testing.T, expected, actual oracle.Verdict) {
	t.Helper()
	is.Equal(t, expected.Kind, actual.Kind, "kind")
	is.Equal(t, expected.Message, actual.Message, "message")
	is.Equal(t, expected.Offset, actual.Offset, "offset")
	is.Equal(t, expected.Code, actual.Code, "code")
	is.Equal(t, expected.Tail, actual.Tail, "tail")
	is.Equal(t, expected.Reason, actual.Reason, "reason")
	is.EqualSlice(t, expected.Setup, actual.Setup, "setup")
}
