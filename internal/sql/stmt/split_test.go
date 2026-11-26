package stmt_test

import (
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sqlite"
)

// texts of the parts of src that Split gives, each with the text of its statement after a "|".
func partTexts(src string, parts []stmt.Part) []string {
	var texts []string
	for _, p := range parts {
		span := p.Statement.Bounds()
		texts = append(texts, src[p.Span.Start:p.Span.End]+"|"+src[span.Start:span.End])
	}
	return texts
}

// The hand cases of [TestSplit], which seed [FuzzSplit] too.
var (
	splitOne      = []string{"select * from t", "  -- c\n ; select * from t ; -- d\n;", "insert into t (v) values (?)"}
	splitSeveral  = "create table t (v int primary key) strict, without rowid; ;-- c\nupdate t set v = ? where v = ?;\n\tselect * from t where v = ? /* d */ ;; -- e"
	splitNone     = []string{"", " ; -- c\n;", "/* c */"}
	splitFailures = []struct {
		src   string
		parts int
		err   string
	}{
		{src: "drop table t; selec * from t; drop table u", parts: 1, err: `near "selec": syntax error`},
		{src: "drop table t; drop table u; drop view v; drop table w", parts: 2, err: "s4ql: not supported: DROP VIEW (at offset 33)"},
		{src: "drop table t; select * from t group by v", parts: 1, err: "s4ql: not supported: GROUP BY clause (at offset 30)"},
		{src: "drop table t; begin; drop table u", parts: 1, err: "s4ql: not supported: BEGIN (at offset 14)"},
		{src: "selec; drop table t", parts: 0, err: `near "selec": syntax error`},
		{src: "drop table t; insert into", parts: 1, err: "incomplete input"},
	}
	splitBadText = map[string]string{
		"drop table t; drop table u\x00": "s4ql: not supported: NUL byte (at offset 26)",
		"drop table t; drop table \xff":  "s4ql: not supported: invalid UTF-8 (at offset 25)",
	}
	splitSpaceAfter = map[string]string{
		"drop table t;\u00a0drop table u":               "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 13)",
		"drop table t; \u2028 drop table u":             "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 14)",
		"drop table t;\vdrop table u":                   "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 13)",
		"drop table t; drop table u;\u0085drop table w": "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 27)",
	}
	splitSpaceEnd = map[string]string{
		"drop table t;\u00a0":               "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 13)",
		"drop table t; drop table u \u00a0": "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 27)",
		// The first of two.
		"drop table t; drop table u \u00a0 \u00a0": "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 27)",
		"drop table t; /* c */\u00a0":              "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 21)",
		"drop table t; select 'abc  ":              "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 25)",
		"drop table t; /* ":                        "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 16)",
		"drop table t; drop table \"u \n":          "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 27)",
	}
	splitSpaceTaken = []string{
		"drop table t; \t\r\n\f drop table u \t\r\n\f\v",
		"drop table t; drop table u; -- c \t\v\u00a0",
		"drop table t; drop table u; /* c \u00a0\n",
		"drop table t; drop table u; --  ",
		"drop table t;\ufeffdrop table u",
		// The last statement without a semicolon, and a comment after it.
		"drop table t; drop table u -- c \v",
		"drop table t; drop table u -- c\u00a0",
		"drop table t; drop table u /* c  ",
	}
	splitTrimmedTable = "drop table t; create table u (v int primary key) strict, without rowid \n\t "
	splitFirstTable   = "create table u (v int primary key) strict, without rowid \n "
)

// splitHandTexts of the hand cases of [TestSplit].
func splitHandTexts() []string {
	texts := slices.Concat(splitOne, []string{splitSeveral, splitTrimmedTable, splitFirstTable}, splitNone, splitSpaceTaken)
	for _, f := range splitFailures {
		texts = append(texts, f.src)
	}
	for _, m := range []map[string]string{splitBadText, splitSpaceAfter, splitSpaceEnd} {
		texts = append(texts, slices.Sorted(maps.Keys(m))...)
	}
	return texts
}

func TestSplit(t *testing.T) {
	t.Run("should give the one statement of a text as Check gives it, with the whole text as its part", func(t *testing.T) {
		for _, src := range splitOne {
			parts, err := stmt.Split(src)
			is.NotError(t, err)
			is.Equal(t, 1, len(parts))
			is.Equal(t, stmt.Span{Start: 0, End: len(src)}, parts[0].Span)
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", parts[0].Statement))
		}
	})

	t.Run("should give each statement of a text, with empty statements, spaces, and comments in the part of the statement after them, and offsets in the whole text", func(t *testing.T) {
		src := splitSeveral
		parts, err := stmt.Split(src)
		is.NotError(t, err)
		is.EqualSlice(t, []string{
			"create table t (v int primary key) strict, without rowid;|create table t (v int primary key) strict, without rowid;",
			" ;-- c\nupdate t set v = ? where v = ?;|update t set v = ? where v = ?;",
			"\n\tselect * from t where v = ? /* d */ ;; -- e|select * from t where v = ? /* d */ ;",
		}, partTexts(src, parts))
		is.Equal(t, 0, parts[0].Statement.Parameters())
		is.Equal(t, 2, parts[1].Statement.Parameters())
		is.Equal(t, 1, parts[2].Statement.Parameters())
		is.Equal(t, "CREATE TABLE t (v int primary key) strict, without rowid", parts[0].Statement.(*stmt.CreateTable).SQL)
	})

	t.Run("should give no part and no error for a text without a statement", func(t *testing.T) {
		for _, src := range splitNone {
			parts, err := stmt.Split(src)
			is.NotError(t, err)
			is.Equal(t, 0, len(parts))
		}
	})

	t.Run("should give the statements before the first that fails to check, and its error, with the offset in the whole text", func(t *testing.T) {
		for _, test := range splitFailures {
			parts, err := stmt.Split(test.src)
			is.Equal(t, test.parts, len(parts), test.src)
			is.True(t, err != nil, test.src)
			is.Equal(t, test.err, err.Error(), test.src)
		}
		_, err := stmt.Split("drop table t; selec * from t")
		var se *sqlerr.Error
		is.True(t, errors.As(err, &se))
		is.Equal(t, sqlerr.Error{Code: 1, ExtendedCode: 1, Message: `near "selec": syntax error`, Offset: 14}, *se)
	})

	t.Run("should refuse a text that is not valid UTF-8 or holds a NUL byte as a whole, before any statement", func(t *testing.T) {
		for src, want := range splitBadText {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts))
			is.True(t, err != nil)
			is.Equal(t, want, err.Error())
		}
	})

	t.Run("should refuse a space after a statement that SQLite reads as part of a token, which Go's TrimSpace removes, before any statement", func(t *testing.T) {
		for src, want := range splitSpaceAfter {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts), src)
			is.True(t, err != nil, src)
			is.Equal(t, want, err.Error(), src)
		}
	})

	t.Run("should refuse a space at the end of a text of more than one statement that SQLite reads as part of a token, before any statement", func(t *testing.T) {
		for src, want := range splitSpaceEnd {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts), src)
			is.True(t, err != nil, src)
			is.Equal(t, want, err.Error(), src)
		}
	})

	t.Run("should take spaces that SQLite reads as spaces, and comments that end in spaces, at the end and between statements", func(t *testing.T) {
		for _, src := range splitSpaceTaken {
			parts, err := stmt.Split(src)
			is.NotError(t, err, src)
			is.Equal(t, 2, len(parts), src)
			is.Equal(t, stmt.Span{Start: 13, End: len(src)}, parts[1].Span, src)
		}
	})

	t.Run("should end the text of a statement after the first at the last character that is not a space, as mattn's driver hands it to SQLite", func(t *testing.T) {
		src := splitTrimmedTable
		parts, err := stmt.Split(src)
		is.NotError(t, err)
		is.Equal(t, "CREATE TABLE u (v int primary key) strict, without rowid", parts[1].Statement.(*stmt.CreateTable).SQL)
		is.Equal(t, stmt.Span{Start: 13, End: len(src)}, parts[1].Span)

		// The first statement is handed over as the text is.
		src = splitFirstTable
		parts, err = stmt.Split(src)
		is.NotError(t, err)
		is.Equal(t, "CREATE TABLE u (v int primary key) strict, without rowid \n ", parts[0].Statement.(*stmt.CreateTable).SQL)
	})

	t.Run("should name its refusals of spaces by names of the vocabulary", func(t *testing.T) {
		for _, src := range []string{"drop table t;\u00a0drop table u", "drop table t;\u00a0"} {
			_, err := stmt.Split(src)
			m := messageForm.FindStringSubmatch(err.Error())
			is.True(t, m != nil, err.Error())
			is.True(t, vocabulary(t)[m[1]], m[1])
		}
	})
}

// The pieces of the texts of [splitText]: statements that s4ql supports, with semicolons in strings,
// in blobs, in names in double quotes, and in comments; statements that SQLite parses and s4ql does
// not support, CREATE TRIGGER with semicolons in its body and names in brackets and backticks with
// semicolons among them; and statements that SQLite does not parse, unclosed strings, names, blobs,
// and comments among them.
var (
	splitSupported = []string{
		"create table t (v int primary key) strict, without rowid",
		"create table \"t;1\" (v text primary key) strict, without rowid",
		"create table if not exists \"b;r\" (v blob primary key, w int) strict, without rowid",
		"insert into t (v) values (1)",
		"insert into t (v) values ('a;b')",
		"insert into \"t;1\" (v) values (x'3b')",
		"insert into \"t;1\" (v) values ('x'';')",
		"select * from t",
		"select * from t where v = ?",
		"select * from \"t;1\" where v = ';'",
		"select /* ; */ * from t -- ;\n where v = 1",
		"update t set v = 1 where v = 2",
		"update \"b;r\" set v = x'3b' where w = ?",
		"delete from t where v = 'x;y' returning *",
		"delete from t",
		"drop table t",
		"drop table if exists \"t;1\"",
		"create index i on t (v)",
		"drop index if exists i",
	}
	splitUnsupported = []string{
		"create trigger tr after insert on t begin delete from t; select 1; end",
		"create trigger tr after insert on t begin insert into u values (';'); end",
		"drop view v",
		"begin",
		"vacuum",
		"select * from t group by v",
		"create view v as select * from t",
		"select 1",
		"insert into t select * from u",
		"create table if not exists [b;r] (v blob primary key, w int) strict, without rowid",
		"insert into `c;d` values (?, ';')",
		"update [b;r] set v = x'3b' where w = ?",
		"delete from `c;d` where v = 'x;y'",
	}
	splitBroken = []string{
		"selec * from t",
		"insert into",
		"select",
		"select * from",
		"create table t (v int",
		"select 1 2",
		"select * from t where v = 'abc",
		"select * from t where v = 'abc  ",
		"select x'3",
		"select x'3b",
		"select \"abc",
		"drop table \"u ",
		"select [abc",
		"select `abc",
		"/* ; unclosed",
		"-- ; to the end",
		"create trigger tr after insert on t begin delete from t;",
	}
	// splitSpaces are SQLite's spaces and closed comments, some with semicolons.
	splitSpaces = []string{" ", " ", " ", "  ", "\t", "\n", "\r\n", "\f", "-- c;\n", "/* ; */", "/**/", "--\n", " \v"}
	// splitOddSpaces are the spaces of Go's TrimSpace that SQLite reads as part of a token, a byte
	// order mark, control bytes, and comments without their end, which run to the end of the text.
	splitOddSpaces = []string{"\v", "\u00a0", "\u0085", "\u2028", "\u3000", "\ufeff", "\x01", "\x1f", "\x7f", "-- c", "/* ;", "/*", "/* "}
)

// splitSpace of [splitSpaces] seven times in eight, else of [splitOddSpaces].
func splitSpace(r *rand.Rand) string {
	if r.IntN(8) == 0 {
		return splitOddSpaces[r.IntN(len(splitOddSpaces))]
	}
	return splitSpaces[r.IntN(len(splitSpaces))]
}

// splitText writes a text of one to five statements of the pieces above, with spaces and empty
// statements before, between, and after them, now and then a statement with one of its spaces
// replaced, a separator missing, or a NUL byte or a byte that is not UTF-8.
func splitText(r *rand.Rand) string {
	var b strings.Builder
	spaces := func() {
		for range r.IntN(3) {
			switch {
			case r.IntN(60) == 0:
				b.WriteString([]string{"\x00", "\xff", "\xc3"}[r.IntN(3)])
			case r.IntN(4) == 0:
				b.WriteString(";")
			default:
				b.WriteString(splitSpace(r))
			}
		}
	}
	statement := func() string {
		var s string
		switch n := r.IntN(20); {
		case n < 15:
			s = splitSupported[r.IntN(len(splitSupported))]
		case n < 17:
			s = splitUnsupported[r.IntN(len(splitUnsupported))]
		case n < 18:
			s = splitBroken[r.IntN(len(splitBroken))]
		default:
			s = ""
		}
		if r.IntN(8) == 0 {
			if spaces := strings.Count(s, " "); spaces > 0 {
				i := strings.Index(s, " ")
				for range r.IntN(spaces) {
					i += 1 + strings.Index(s[i+1:], " ")
				}
				s = s[:i] + splitSpace(r) + s[i+1:]
			}
		}
		return s
	}
	spaces()
	for i := range 1 + r.IntN(5) {
		if i > 0 {
			spaces()
			if r.IntN(12) != 0 {
				b.WriteString(";")
			}
			spaces()
		}
		b.WriteString(statement())
	}
	if r.IntN(2) == 0 {
		b.WriteString(";")
	}
	spaces()
	return b.String()
}

// readStep is one statement of a reading of a text as SQLite prepares it, with offsets in the whole
// text.
type readStep struct {
	// verdict of SQLite on the statement.
	verdict oracle.Kind
	// message of SQLite's error, or "".
	message string
	// offset of SQLite's error, or -1.
	offset int
	// first and last are the start of the first token of the statement and the end of its last, of
	// the tokens before its tail that are not spaces, comments, or the semicolons of empty statements
	// before it, or -1 if SQLite gives no tail.
	first, last int
	// tail is the end of the statement, after its semicolon, or the end of the text that SQLite was
	// handed, or -1. The readings are not compared by it: the text handed to SQLite in the reading of
	// mattn ends before the spaces at the end of the whole text.
	tail int
}

// sqliteKinds of the codes of SQLite's tokenizer for a space, a comment, and a semicolon, read from
// texts of one such token each, since SQLite numbers its codes anew for each version.
var sqliteKinds = sync.OnceValue(func() [3]int {
	return [3]int{sqlite.Tokenize(" ")[0].Type, sqlite.Tokenize("/**/")[0].Type, sqlite.Tokenize(";")[0].Type}
})

// statementTokens gives the start of the first token of src, by SQLite's tokenizer, that is not a
// space, a comment, or a semicolon, and the end of the last that is not a space or a comment, or -1
// and -1 if there is none.
func statementTokens(src string) (int, int) {
	k := sqliteKinds()
	first, last, at := -1, -1, 0
	for _, tk := range sqlite.Tokenize(src) {
		if tk.Type != k[0] && tk.Type != k[1] && tk.Len > 0 {
			if first < 0 && tk.Type != k[2] {
				first = at
			}
			if first >= 0 {
				last = at + tk.Len
			}
		}
		at += tk.Len
	}
	return first, last
}

// readText reads the statements of src, a text without a NUL byte, as SQLite prepares them one after
// another, each from the tail of the one before, until a text without a statement, with the verdicts
// of [oracle.Judge], which prepares through the binding of internal/sqlite: as SQLite alone reads the
// text if trim is false, and if it is true as mattn's Exec does, which prepares the first statement of
// the text and then of each tail with the spaces of Go's strings.TrimSpace removed at both ends. The
// reading stops after a statement that does not parse, or on which SQLite does not say. A statement
// that parses with an error of its names, such as "no such table", does not stop it: the cut does not
// depend on the schema.
func readText(src string, trim bool, judge func(string) oracle.Verdict) []readStep {
	var steps []readStep
	base, q := 0, src
	for q != "" {
		if first, _ := statementTokens(q); first < 0 {
			return steps
		}
		v := judge(q)
		s := readStep{verdict: v.Kind, message: v.Message, offset: -1, first: -1, last: -1, tail: -1}
		if v.Offset >= 0 {
			s.offset = base + v.Offset
		}
		if v.Tail >= 0 {
			s.tail = base + v.Tail
			if first, last := statementTokens(q[:v.Tail]); first >= 0 {
				s.first, s.last = base+first, base+last
			}
		}
		steps = append(steps, s)
		if v.Kind != oracle.Parses || v.Tail < 0 {
			return steps
		}
		rest := q[v.Tail:]
		base += v.Tail
		if trim {
			left := strings.TrimLeftFunc(rest, unicode.IsSpace)
			base += len(rest) - len(left)
			rest = strings.TrimRightFunc(left, unicode.IsSpace)
		}
		q = rest
	}
	return steps
}

// firstDifference of the two readings: the index of the first statement at which they differ, in the
// verdict, the error, or the tokens of the statement, or at which one of them has a statement and the
// other not, or -1 if they agree.
func firstDifference(a, b []readStep) int {
	for i := range max(len(a), len(b)) {
		if i >= len(a) || i >= len(b) {
			return i
		}
		x, y := a[i], b[i]
		if x.verdict != y.verdict || x.message != y.message || x.offset != y.offset || x.first != y.first || x.last != y.last {
			return i
		}
	}
	return -1
}

// sqliteTrimmedEnd is where SQLite's tokenizer reads the end of src, which Go's TrimSpace removes,
// otherwise than it reads src without it, and true: at the first token that is not a space and runs
// past the start of the spaces, unless it is a comment that is one up to there too, or at the first
// token after them that is not a space. It reports false if SQLite reads both alike.
func sqliteTrimmedEnd(src string) (int, bool) {
	trimmed := len(strings.TrimRightFunc(src, unicode.IsSpace))
	k := sqliteKinds()
	at := 0
	for _, tk := range sqlite.Tokenize(src) {
		start := at
		at += tk.Len
		switch {
		case tk.Len == 0 || at <= trimmed || tk.Type == k[0]:
		case start < trimmed && tk.Type == k[1]:
			if inner := sqlite.Tokenize(src[start:trimmed]); inner[0].Type != k[1] || inner[0].Len != trimmed-start {
				return trimmed, true
			}
		default:
			return max(start, trimmed), true
		}
	}
	return 0, false
}

// sqliteTrimmedStart is where SQLite's tokenizer reads the spaces at the offset at of src, which Go's
// TrimSpace removes, as part of a token, and true: at the first token among them that is not a space.
// It reports false if SQLite reads them all as spaces.
func sqliteTrimmedStart(src string, at int) (int, bool) {
	end := len(src) - len(strings.TrimLeftFunc(src[at:], unicode.IsSpace))
	for _, tk := range sqlite.Tokenize(src[at:]) {
		if at >= end || tk.Len == 0 {
			break
		}
		if tk.Type != sqliteKinds()[0] {
			return at, true
		}
		at += tk.Len
	}
	return 0, false
}

// The refusals of spaces of [stmt.Split].
const (
	spaceAfter = "space after a statement that SQLite reads as part of a token"
	spaceAtEnd = "space at the end of the text that SQLite reads as part of a token"
)

// The outcomes of [checkSplit] that are no refusal of the whole text.
const (
	splitNoStatement  = "no statement"
	splitOneStatement = "one statement"
	splitStatements   = "several statements"
	splitSyntax       = "a syntax error"
	splitRefused      = "a statement that s4ql does not support"
	splitRefusedFirst = "a statement that s4ql does not support, before the readings differ"
	splitNotSaid      = "a statement on which SQLite does not say"
)

// checkSplit checks [stmt.Split] of src against SQLite, read as SQLite alone reads it and as mattn's
// Exec reads it ([readText]), and gives the kind of its outcome:
//
//   - The parts follow one another from the start of the text, and each holds its statement.
//   - A text with a NUL byte or a byte that is not UTF-8 is refused as a whole, at the first.
//   - Where the two readings agree, Split cuts each statement where SQLite does, with its statement
//     on the tokens that SQLite read for it, and the last part to the end of the text, until it gives
//     SQLite's syntax error with its offset, or "not supported" in a statement that SQLite parses.
//     At a statement on which SQLite does not say, the check stops.
//   - Where the readings differ, Split refuses the text as a whole with a refusal of spaces, or stops
//     with "not supported" at a statement before the first at which they differ.
//   - Split refuses the end of the text exactly where SQLite's tokenizer reads it otherwise without
//     the spaces that TrimSpace removes there, at the offset of [sqliteTrimmedEnd], if the first
//     statement parses and s4ql supports it and text follows it: this is decided for the whole text
//     before any later statement is read, so it holds also where a later statement fails before
//     the end and the two readings agree. It refuses the start of a tail only where the readings
//     differ at the statement after it, at the offset of [sqliteTrimmedStart].
func checkSplit(t testing.TB, src string) string {
	t.Helper()
	parts, err := stmt.Split(src)
	at := 0
	for i, p := range parts {
		b := p.Statement.Bounds()
		if p.Span.Start != at || b.Start < p.Span.Start || b.End > p.Span.End || b.Start >= b.End {
			t.Fatalf("%q: part %v has the span %+v and its statement %+v, after a part that ends at %v", src, i, p.Span, b, at)
		}
		at = p.Span.End
	}
	var ne *stmt.NotSupportedError
	var se *sqlerr.Error
	switch {
	case errors.As(err, &ne) && ne.What == spaceAtEnd:
		// The offset may be inside a token that the trim cuts, such as a string without its end.
		if m := messageForm.FindStringSubmatch(err.Error()); !errors.Is(err, stmt.ErrNotSupported) || m == nil || m[1] != ne.What || !vocabulary(t)[ne.What] {
			t.Fatalf("%q: the error %q does not have the form of not supported", src, err)
		}
	case ne != nil:
		checkNotSupported(t, src, err, ne)
	case errors.As(err, &se):
		if se.Code != 1 || se.ExtendedCode != 1 || !oracle.IsSyntaxMessage(se.Message) {
			t.Fatalf("%q: gives the syntax error %+v", src, se)
		}
	case err != nil:
		t.Fatalf("%q: gives the error %v of type %T", src, err, err)
	}

	if bad, what := badText(src); bad >= 0 {
		if ne == nil || ne.What != what || ne.Offset != bad || len(parts) > 0 {
			t.Fatalf("%q: gives %v parts and %v, want not supported %v at %v", src, len(parts), err, what, bad)
		}
		return what
	}

	verdicts := map[string]oracle.Verdict{}
	judge := func(q string) oracle.Verdict {
		v, ok := verdicts[q]
		if !ok {
			v = oracle.Judge(q)
			verdicts[q] = v
		}
		return v
	}
	alone := readText(src, false, judge)
	mattn := readText(src, true, judge)
	differ := firstDifference(alone, mattn)

	// The end of the text is judged once a tail follows a first statement that s4ql supports, before
	// any later statement is read, also one that fails before the end.
	endAt, endDiffers := sqliteTrimmedEnd(src)
	if endDiffers && len(alone) > 0 && alone[0].verdict == oracle.Parses && alone[0].tail < len(src) {
		first, err := stmt.Check(src[:alone[0].tail])
		endDiffers = err == nil && first != nil
	} else {
		endDiffers = false
	}
	if endDiffers || ne != nil && ne.What == spaceAtEnd {
		if !endDiffers || ne == nil || ne.What != spaceAtEnd || ne.Offset != endAt || len(parts) > 0 {
			t.Fatalf("%q: gives %v parts and %v, the end of the text differs in SQLite without the spaces of TrimSpace: %v, at %v", src, len(parts), err, endDiffers, endAt)
		}
		return spaceAtEnd
	}
	if ne != nil && ne.What == spaceAfter {
		if differ < 1 || len(parts) > 0 {
			t.Fatalf("%q: gives %v parts and %v, but SQLite alone reads %+v and mattn %+v", src, len(parts), err, alone, mattn)
		}
		if at, ok := sqliteTrimmedStart(src, alone[differ-1].tail); !ok || ne.Offset != at {
			t.Fatalf("%q: gives %v, the start of the text after statement %v differs in SQLite without the spaces of TrimSpace: %v, at %v", src, err, differ-1, ok, at)
		}
		return spaceAfter
	}

	steps := alone
	if differ >= 0 {
		steps = alone[:differ]
	}
	for i, s := range steps {
		start := 0
		if i > 0 {
			start = parts[i-1].Span.End
		}
		if i == len(parts) {
			switch {
			case s.verdict == oracle.NotSaid && err != nil:
				return splitNotSaid
			case se != nil:
				if s.verdict != oracle.SyntaxError || se.Message != s.message || se.Offset != s.offset {
					t.Fatalf("%q: gives the syntax error %+v at statement %v, SQLite gives %+v", src, se, i, s)
				}
				return splitSyntax
			case ne != nil:
				if s.verdict != oracle.Parses || ne.Offset < start || ne.Offset > s.tail {
					t.Fatalf("%q: gives %v at statement %v, from offset %v, SQLite gives %+v", src, err, i, start, s)
				}
				if differ >= 0 {
					return splitRefusedFirst
				}
				return splitRefused
			default:
				t.Fatalf("%q: gives %v parts and %v, SQLite reads %+v", src, len(parts), err, steps)
			}
		}
		if s.verdict == oracle.NotSaid {
			return splitNotSaid
		}
		if s.verdict != oracle.Parses {
			t.Fatalf("%q: gives a part for statement %v, SQLite gives %+v", src, i, s)
		}
		end := s.tail
		if i == len(steps)-1 && differ < 0 {
			end = len(src)
		}
		if b := parts[i].Statement.Bounds(); parts[i].Span.End != end || b.Start != s.first || b.End != s.last {
			t.Fatalf("%q: gives the part %+v with its statement %+v for statement %v, SQLite reads %+v", src, parts[i].Span, b, i, s)
		}
	}
	if differ >= 0 {
		t.Fatalf("%q: gives %v parts and %v, but SQLite alone reads %+v and mattn %+v, which differ at statement %v", src, len(parts), err, alone, mattn, differ)
	}
	if len(parts) > len(steps) || err != nil {
		t.Fatalf("%q: gives %v parts and %v, SQLite reads %+v", src, len(parts), err, steps)
	}
	switch len(parts) {
	case 0:
		return splitNoStatement
	case 1:
		return splitOneStatement
	default:
		return splitStatements
	}
}

// TestSplit_differential checks [stmt.Split] against SQLite with [checkSplit] on generated texts, and
// requires every outcome but the one where SQLite does not say.
func TestSplit_differential(t *testing.T) {
	n := 100_000
	if slow() {
		n = 10_000
	}
	r := rand.New(rand.NewPCG(*seed, 0x5b1170))
	kinds := map[string]int{}
	var examples []string
	cuts := 0
	for range n {
		src := splitText(r)
		kind := checkSplit(t, src)
		if kinds[kind] == 0 {
			examples = append(examples, fmt.Sprintf("%v: %q", kind, src))
		}
		kinds[kind]++
		parts, _ := stmt.Split(src)
		cuts += len(parts)
	}
	t.Logf("%v texts, with %v parts", n, cuts)
	for _, k := range slices.Sorted(maps.Keys(kinds)) {
		t.Logf("%v: %v", k, kinds[k])
	}
	for _, e := range examples {
		t.Logf("first of %v", e)
	}
	for _, k := range []string{"NUL byte", "invalid UTF-8", spaceAfter, spaceAtEnd, splitNoStatement, splitOneStatement, splitStatements, splitSyntax, splitRefused, splitRefusedFirst} {
		if kinds[k] == 0 {
			t.Errorf("no text of %v texts reached %q", n, k)
		}
	}
}

// FuzzSplit checks [stmt.Split] against SQLite with [checkSplit] on any text.
func FuzzSplit(f *testing.F) {
	for _, src := range splitHandTexts() {
		f.Add(src)
	}
	r := rand.New(rand.NewPCG(1, 0x5b1170))
	for range 200 {
		f.Add(splitText(r))
	}
	f.Fuzz(func(t *testing.T, src string) {
		checkSplit(t, src)
	})
}
