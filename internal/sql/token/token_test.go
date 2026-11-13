package token_test

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// tok is a token as the cases below write it: its kind and its text.
type tok struct {
	kind token.Kind
	text string
}

func (t tok) String() string {
	return fmt.Sprintf("%v %q", t.kind, t.text)
}

// cases of SQL text and the tokens that SQLite gives for it, written by hand. Each case is also
// compared with SQLite's own tokenizer, and they are the seeds of [FuzzTokens].
var cases = []struct {
	name   string
	src    string
	tokens []tok
}{
	// Whitespace.
	{"space between words", "a b", []tok{{token.ID, "a"}, {token.Space, " "}, {token.ID, "b"}}},
	{"every whitespace byte in one token", " \t\n\f\r\v ", []tok{{token.Space, " \t\n\f\r\v "}}},
	{"vertical tab at the start of a token", "\v", []tok{{token.Illegal, "\v"}}},
	{"vertical tab after other whitespace", " \v", []tok{{token.Space, " \v"}}},
	{"vertical tab right after a keyword", "select\v1", []tok{{token.Select, "select"}, {token.Illegal, "\v"}, {token.Integer, "1"}}},
	{"vertical tab after a space after a keyword", "select \v1", []tok{{token.Select, "select"}, {token.Space, " \v"}, {token.Integer, "1"}}},
	{"vertical tab twice", "\v\v", []tok{{token.Illegal, "\v"}, {token.Illegal, "\v"}}},

	// The byte order mark.
	{"byte order mark at the start", "\xef\xbb\xbfselect 1", []tok{{token.Space, "\xef\xbb\xbf"}, {token.Select, "select"}, {token.Space, " "}, {token.Integer, "1"}}},
	{"byte order mark inside a word", "select\xef\xbb\xbf1", []tok{{token.ID, "select\xef\xbb\xbf1"}}},
	{"byte order mark after a space", " \xef\xbb\xbf", []tok{{token.Space, " "}, {token.Space, "\xef\xbb\xbf"}}},
	{"byte order mark twice", "\xef\xbb\xbf\xef\xbb\xbf", []tok{{token.Space, "\xef\xbb\xbf"}, {token.Space, "\xef\xbb\xbf"}}},
	{"first two bytes of a byte order mark", "\xef\xbb", []tok{{token.ID, "\xef\xbb"}}},
	{"first two bytes of a byte order mark before a space", "\xef\xbb x", []tok{{token.ID, "\xef\xbb"}, {token.Space, " "}, {token.ID, "x"}}},
	{"byte order mark before a number", "\xef\xbb\xbf1", []tok{{token.Space, "\xef\xbb\xbf"}, {token.Integer, "1"}}},

	// Bytes from 0x80 up are identifier characters, also in text that is not UTF-8.
	{"no-break space alone", "\xc2\xa0", []tok{{token.ID, "\xc2\xa0"}}},
	{"no-break space inside a word", "a\xc2\xa0b", []tok{{token.ID, "a\xc2\xa0b"}}},
	{"no-break space after a number", "1\xc2\xa0", []tok{{token.Illegal, "1\xc2\xa0"}}},
	{"bytes that are not UTF-8", "\xff\xfe\x80", []tok{{token.ID, "\xff\xfe\x80"}}},
	{"keyword followed by a byte from 0x80 up", "select\x80", []tok{{token.ID, "select\x80"}}},
	{"letters that are not ASCII", "æøå ÆØÅ", []tok{{token.ID, "æøå"}, {token.Space, " "}, {token.ID, "ÆØÅ"}}},

	// Keywords and names.
	{"keyword in lower case", "select", []tok{{token.Select, "select"}}},
	{"keyword in mixed case", "SeLeCt", []tok{{token.Select, "SeLeCt"}}},
	{"keyword with a letter more", "selects", []tok{{token.ID, "selects"}}},
	{"keyword with a digit", "select1", []tok{{token.ID, "select1"}}},
	{"keyword with a dollar", "select$", []tok{{token.ID, "select$"}}},
	{"keyword with an underscore", "select_", []tok{{token.ID, "select_"}}},
	{"keyword with an underscore inside", "current_date current_time current_timestamp", []tok{{token.CTimeKw, "current_date"}, {token.Space, " "}, {token.CTimeKw, "current_time"}, {token.Space, " "}, {token.CTimeKw, "current_timestamp"}}},
	{"single letter", "a", []tok{{token.ID, "a"}}},
	{"single letter before a digit", "a1", []tok{{token.ID, "a1"}}},
	{"words starting with x, y, z, and underscore", "x xyz y z _a _", []tok{{token.ID, "x"}, {token.Space, " "}, {token.ID, "xyz"}, {token.Space, " "}, {token.ID, "y"}, {token.Space, " "}, {token.ID, "z"}, {token.Space, " "}, {token.ID, "_a"}, {token.Space, " "}, {token.ID, "_"}}},
	{"words that are not keywords", "strict rowid int integer true false", []tok{{token.ID, "strict"}, {token.Space, " "}, {token.ID, "rowid"}, {token.Space, " "}, {token.ID, "int"}, {token.Space, " "}, {token.ID, "integer"}, {token.Space, " "}, {token.ID, "true"}, {token.Space, " "}, {token.ID, "false"}}},
	{"keywords that share a kind", "temp temporary like glob regexp left natural", []tok{{token.Temp, "temp"}, {token.Space, " "}, {token.Temp, "temporary"}, {token.Space, " "}, {token.LikeKw, "like"}, {token.Space, " "}, {token.LikeKw, "glob"}, {token.Space, " "}, {token.LikeKw, "regexp"}, {token.Space, " "}, {token.JoinKw, "left"}, {token.Space, " "}, {token.JoinKw, "natural"}}},
	{"window keywords", "window over filter", []tok{{token.Window, "window"}, {token.Space, " "}, {token.Over, "over"}, {token.Space, " "}, {token.Filter, "filter"}}},
	{"keyword before a quote", "select'a'", []tok{{token.Select, "select"}, {token.String, "'a'"}}},
	{"dollar inside a name", "a$b", []tok{{token.ID, "a$b"}}},

	// Quoted names and strings.
	{"double-quoted name with an escaped quote", `"a""b"`, []tok{{token.ID, `"a""b"`}}},
	{"backquoted name with an escaped quote", "`c``d`", []tok{{token.ID, "`c``d`"}}},
	{"bracketed name", "[d e]", []tok{{token.ID, "[d e]"}}},
	{"bracketed name has no escape", "[a]]", []tok{{token.ID, "[a]"}, {token.Illegal, "]"}}},
	{"bracketed name with quotes in it", `[a"'b]`, []tok{{token.ID, `[a"'b]`}}},
	{"string with an escaped quote", "'e''f'", []tok{{token.String, "'e''f'"}}},
	{"string with other quotes in it", "'a\"`]'", []tok{{token.String, "'a\"`]'"}}},
	{"string over two lines", "'a\nb'", []tok{{token.String, "'a\nb'"}}},
	{"empty string", "''", []tok{{token.String, "''"}}},
	{"empty quoted names", "\"\" `` []", []tok{{token.ID, `""`}, {token.Space, " "}, {token.ID, "``"}, {token.Space, " "}, {token.ID, "[]"}}},
	{"unterminated string", "'abc", []tok{{token.Illegal, "'abc"}}},
	{"unterminated double-quoted name", `"abc`, []tok{{token.Illegal, `"abc`}}},
	{"unterminated backquoted name", "`abc", []tok{{token.Illegal, "`abc"}}},
	{"unterminated bracketed name", "[abc", []tok{{token.Illegal, "[abc"}}},
	{"string that ends with an escaped quote", "'it''", []tok{{token.Illegal, "'it''"}}},
	{"lone quotes", "' \" ` [", []tok{{token.Illegal, "' \" ` ["}}},
	{"quotes next to each other", "'a''b'\"c\"", []tok{{token.String, "'a''b'"}, {token.ID, `"c"`}}},

	// Blobs.
	{"blob", "x'00'", []tok{{token.Blob, "x'00'"}}},
	{"blob with upper-case X and mixed digits", "X'0aFf'", []tok{{token.Blob, "X'0aFf'"}}},
	{"empty blob", "x''", []tok{{token.Blob, "x''"}}},
	{"blob with an odd number of digits", "x'0'", []tok{{token.Illegal, "x'0'"}}},
	{"blob with a digit that is not hex", "x'0g'", []tok{{token.Illegal, "x'0g'"}}},
	{"blob with a space", "x'00 11'", []tok{{token.Illegal, "x'00 11'"}}},
	{"unterminated blob", "x'00", []tok{{token.Illegal, "x'00"}}},
	{"unterminated blob that is not hex", "x'0g", []tok{{token.Illegal, "x'0g"}}},
	{"x and a quote", "x'", []tok{{token.Illegal, "x'"}}},
	{"blob before a name", "x'00'a", []tok{{token.Blob, "x'00'"}, {token.ID, "a"}}},
	{"blob with an escaped quote", "x'00''", []tok{{token.Blob, "x'00'"}, {token.Illegal, "'"}}},
	{"x then a space then a string", "x 'a'", []tok{{token.ID, "x"}, {token.Space, " "}, {token.String, "'a'"}}},
	{"two letters then a string", "xx'00'", []tok{{token.ID, "xx"}, {token.String, "'00'"}}},

	// Numbers.
	{"integer", "123", []tok{{token.Integer, "123"}}},
	{"integer with leading zeros", "00012", []tok{{token.Integer, "00012"}}},
	{"fraction", "1.5", []tok{{token.Float, "1.5"}}},
	{"fraction without an integer part", ".5", []tok{{token.Float, ".5"}}},
	{"fraction without digits", "5.", []tok{{token.Float, "5."}}},
	{"exponent", "1e5 1E+5 1e-5 1.5e10 1.e5", []tok{{token.Float, "1e5"}, {token.Space, " "}, {token.Float, "1E+5"}, {token.Space, " "}, {token.Float, "1e-5"}, {token.Space, " "}, {token.Float, "1.5e10"}, {token.Space, " "}, {token.Float, "1.e5"}}},
	{"exponent without digits", "1e", []tok{{token.Illegal, "1e"}}},
	{"exponent with a sign and without digits", "1e+", []tok{{token.Illegal, "1e"}, {token.Plus, "+"}}},
	{"exponent followed by a letter", "1e+5x", []tok{{token.Illegal, "1e+5x"}}},
	{"two dots", "1..2", []tok{{token.Float, "1."}, {token.Float, ".2"}}},
	{"two fractions", "1.5.3", []tok{{token.Float, "1.5"}, {token.Float, ".3"}}},
	{"number followed by a letter", "1x", []tok{{token.Illegal, "1x"}}},
	{"number followed by a dollar", "1$", []tok{{token.Illegal, "1$"}}},
	{"number followed by a byte from 0x80 up", "1\x80", []tok{{token.Illegal, "1\x80"}}},
	{"hex integer", "0x1F 0X1f 0x1e5", []tok{{token.Integer, "0x1F"}, {token.Space, " "}, {token.Integer, "0X1f"}, {token.Space, " "}, {token.Integer, "0x1e5"}}},
	{"hex prefix without digits", "0x", []tok{{token.Illegal, "0x"}}},
	{"hex prefix with a digit that is not hex", "0xg", []tok{{token.Illegal, "0xg"}}},
	{"hex integer followed by a letter", "0x1g", []tok{{token.Illegal, "0x1g"}}},
	{"hex integer has no fraction", "0x1.5", []tok{{token.Integer, "0x1"}, {token.Float, ".5"}}},
	{"digit separator", "1_000", []tok{{token.QNumber, "1_000"}}},
	{"two digit separators", "1__2", []tok{{token.QNumber, "1__2"}}},
	{"digit separator at the end", "1_", []tok{{token.QNumber, "1_"}}},
	{"digit separator at the start is a name", "_1", []tok{{token.ID, "_1"}}},
	{"digit separator in the fraction", "1_000.5 1.5_0 1._5", []tok{{token.QNumber, "1_000.5"}, {token.Space, " "}, {token.QNumber, "1.5_0"}, {token.Space, " "}, {token.QNumber, "1._5"}}},
	{"digit separator in the exponent", "1e1_0", []tok{{token.QNumber, "1e1_0"}}},
	{"digit separator before the exponent", "1_e5", []tok{{token.QNumber, "1_e5"}}},
	{"digit separator in a hex integer", "0x1_F", []tok{{token.QNumber, "0x1_F"}}},
	{"digit separator right after the hex prefix", "0x_1", []tok{{token.Illegal, "0x_1"}}},
	{"digit separator followed by a letter", "1_a", []tok{{token.Illegal, "1_a"}}},
	{"dot alone", ".", []tok{{token.Dot, "."}}},
	{"dot between names", "a.b", []tok{{token.ID, "a"}, {token.Dot, "."}, {token.ID, "b"}}},
	{"dot before a letter", ".e5", []tok{{token.Dot, "."}, {token.ID, "e5"}}},
	{"dot before a digit after a name", "a.5", []tok{{token.ID, "a"}, {token.Float, ".5"}}},

	// Variables.
	{"question mark", "?", []tok{{token.Variable, "?"}}},
	{"question mark with a number", "?12", []tok{{token.Variable, "?12"}}},
	{"question mark with a name", "?a", []tok{{token.Variable, "?"}, {token.ID, "a"}}},
	{"question mark with a number and a name", "?1a", []tok{{token.Variable, "?1"}, {token.ID, "a"}}},
	{"named variables", ":a @a #a $a", []tok{{token.Variable, ":a"}, {token.Space, " "}, {token.Variable, "@a"}, {token.Space, " "}, {token.Variable, "#a"}, {token.Space, " "}, {token.Variable, "$a"}}},
	{"named variables of digits", ":1 @1 #1 $1", []tok{{token.Variable, ":1"}, {token.Space, " "}, {token.Variable, "@1"}, {token.Space, " "}, {token.Variable, "#1"}, {token.Space, " "}, {token.Variable, "$1"}}},
	{"variable prefixes alone", ": @ # $", []tok{{token.Illegal, ":"}, {token.Space, " "}, {token.Illegal, "@"}, {token.Space, " "}, {token.Illegal, "#"}, {token.Space, " "}, {token.Illegal, "$"}}},
	{"variable with Tcl suffixes", "$a::b(c)", []tok{{token.Variable, "$a::b(c)"}}},
	{"Tcl suffixes on every named variable", ":a(b) @a(b) #a(b) :a::b", []tok{{token.Variable, ":a(b)"}, {token.Space, " "}, {token.Variable, "@a(b)"}, {token.Space, " "}, {token.Variable, "#a(b)"}, {token.Space, " "}, {token.Variable, ":a::b"}}},
	{"Tcl suffix with a space in it", "$a(b c)", []tok{{token.Illegal, "$a(b"}, {token.Space, " "}, {token.ID, "c"}, {token.RP, ")"}}},
	{"unterminated Tcl suffix", "$a(", []tok{{token.Illegal, "$a("}}},
	{"Tcl suffix without a name", "$(a)", []tok{{token.Illegal, "$"}, {token.LP, "("}, {token.ID, "a"}, {token.RP, ")"}}},
	{"Tcl suffix followed by a name", "$a(b)c", []tok{{token.Variable, "$a(b)"}, {token.ID, "c"}}},
	{"Tcl suffix with parentheses in it", "$a((b)", []tok{{token.Variable, "$a((b)"}}},
	{"single colon after a name", ":a:b", []tok{{token.Variable, ":a"}, {token.Variable, ":b"}}},
	{"double colon at the end", "$a::", []tok{{token.Variable, "$a::"}}},
	{"double colon before the name", "$::a", []tok{{token.Variable, "$::a"}}},
	{"single colon after a dollar", "$:", []tok{{token.Illegal, "$"}, {token.Illegal, ":"}}},
	{"triple colon", "$a:::b", []tok{{token.Variable, "$a::"}, {token.Variable, ":b"}}},

	// Comments.
	{"line comment to the end", "-- c", []tok{{token.Comment, "-- c"}}},
	{"line comment to the line feed", "-- c\nx", []tok{{token.Comment, "-- c"}, {token.Space, "\n"}, {token.ID, "x"}}},
	{"line comment ends at the line feed, not the carriage return", "-- a\r\nb", []tok{{token.Comment, "-- a\r"}, {token.Space, "\n"}, {token.ID, "b"}}},
	{"empty line comment", "--", []tok{{token.Comment, "--"}}},
	{"block comment", "/* c */", []tok{{token.Comment, "/* c */"}}},
	{"empty block comment", "/**/", []tok{{token.Comment, "/**/"}}},
	{"block comment of stars", "/***/", []tok{{token.Comment, "/***/"}}},
	{"block comment followed by a name", "/* a */b", []tok{{token.Comment, "/* a */"}, {token.ID, "b"}}},
	{"block comments do not nest", "/* /* */ */", []tok{{token.Comment, "/* /* */"}, {token.Space, " "}, {token.Star, "*"}, {token.Slash, "/"}}},
	{"unterminated block comment", "/* open", []tok{{token.Comment, "/* open"}}},
	{"unterminated block comment of a slash", "/*/", []tok{{token.Comment, "/*/"}}},
	{"unterminated block comment of a star", "/**", []tok{{token.Comment, "/**"}}},
	{"block comment opening at the end", "/*", []tok{{token.Slash, "/"}, {token.Star, "*"}}},
	{"block comment opening at the end after a number", "select 1 /*", []tok{{token.Select, "select"}, {token.Space, " "}, {token.Integer, "1"}, {token.Space, " "}, {token.Slash, "/"}, {token.Star, "*"}}},
	{"line comment inside a block comment", "/* -- */x", []tok{{token.Comment, "/* -- */"}, {token.ID, "x"}}},
	{"block comment inside a line comment", "-- /*\nx", []tok{{token.Comment, "-- /*"}, {token.Space, "\n"}, {token.ID, "x"}}},

	// Operators, each with its longest match.
	{"minus", "-", []tok{{token.Minus, "-"}}},
	{"arrow", "->", []tok{{token.Ptr, "->"}}},
	{"double arrow", "->>", []tok{{token.Ptr, "->>"}}},
	{"double arrow and more", "->>>", []tok{{token.Ptr, "->>"}, {token.GT, ">"}}},
	{"minus minus is a comment", "-->", []tok{{token.Comment, "-->"}}},
	{"minus before a number", "-1", []tok{{token.Minus, "-"}, {token.Integer, "1"}}},
	{"equals", "=", []tok{{token.EQ, "="}}},
	{"double equals", "==", []tok{{token.EQ, "=="}}},
	{"triple equals", "===", []tok{{token.EQ, "=="}, {token.EQ, "="}}},
	{"less than", "<", []tok{{token.LT, "<"}}},
	{"less than or equal", "<=", []tok{{token.LE, "<="}}},
	{"not equal with angles", "<>", []tok{{token.NE, "<>"}}},
	{"left shift", "<<", []tok{{token.LShift, "<<"}}},
	{"left shift and equals", "<<=", []tok{{token.LShift, "<<"}, {token.EQ, "="}}},
	{"less than or equal and more", "<=>", []tok{{token.LE, "<="}, {token.GT, ">"}}},
	{"greater than", ">", []tok{{token.GT, ">"}}},
	{"greater than or equal", ">=", []tok{{token.GE, ">="}}},
	{"right shift", ">>", []tok{{token.RShift, ">>"}}},
	{"right shift and equals", ">>=", []tok{{token.RShift, ">>"}, {token.EQ, "="}}},
	{"greater than and less than", "><", []tok{{token.GT, ">"}, {token.LT, "<"}}},
	{"bang", "!", []tok{{token.Illegal, "!"}}},
	{"not equal with a bang", "!=", []tok{{token.NE, "!="}}},
	{"two bangs", "!!", []tok{{token.Illegal, "!"}, {token.Illegal, "!"}}},
	{"bar", "|", []tok{{token.BitOr, "|"}}},
	{"concatenation", "||", []tok{{token.Concat, "||"}}},
	{"three bars", "|||", []tok{{token.Concat, "||"}, {token.BitOr, "|"}}},
	{"ampersands", "&&", []tok{{token.BitAnd, "&"}, {token.BitAnd, "&"}}},
	{"single-byte operators", "~%*+,;().", []tok{{token.BitNot, "~"}, {token.Rem, "%"}, {token.Star, "*"}, {token.Plus, "+"}, {token.Comma, ","}, {token.Semi, ";"}, {token.LP, "("}, {token.RP, ")"}, {token.Dot, "."}}},
	{"slash", "/", []tok{{token.Slash, "/"}}},
	{"slash before a star and more", "/*x", []tok{{token.Comment, "/*x"}}},

	// Illegal bytes.
	{"control byte", "\x01", []tok{{token.Illegal, "\x01"}}},
	{"illegal punctuation", "\\^{}]\x7f", []tok{{token.Illegal, "\\"}, {token.Illegal, "^"}, {token.Illegal, "{"}, {token.Illegal, "}"}, {token.Illegal, "]"}, {token.Illegal, "\x7f"}}},

	// The end of the input, and NUL bytes, where SQLite's input ends.
	{"empty text", "", nil},
	{"NUL byte alone", "\x00", nil},
	{"NUL byte after a name", "a\x00b", []tok{{token.ID, "a"}}},
	{"NUL byte inside a string", "'a\x00b'", []tok{{token.Illegal, "'a"}}},
	{"NUL byte inside a block comment", "/* a \x00 */", []tok{{token.Comment, "/* a "}}},
	{"NUL byte after a block comment opening", "/*\x00", []tok{{token.Slash, "/"}, {token.Star, "*"}}},
	{"NUL byte inside a blob", "x'0\x000'", []tok{{token.Illegal, "x'0"}}},
	{"NUL byte between statements", "select 1\x00; drop table t", []tok{{token.Select, "select"}, {token.Space, " "}, {token.Integer, "1"}}},

	// Statements.
	{"slice 1 create", "create table t1 (v1 int primary key) strict, without rowid;", []tok{
		{token.Create, "create"}, {token.Space, " "}, {token.Table, "table"}, {token.Space, " "}, {token.ID, "t1"}, {token.Space, " "},
		{token.LP, "("}, {token.ID, "v1"}, {token.Space, " "}, {token.ID, "int"}, {token.Space, " "}, {token.Primary, "primary"}, {token.Space, " "},
		{token.Key, "key"}, {token.RP, ")"}, {token.Space, " "}, {token.ID, "strict"}, {token.Comma, ","}, {token.Space, " "}, {token.Without, "without"},
		{token.Space, " "}, {token.ID, "rowid"}, {token.Semi, ";"},
	}},
	{"slice 1 insert", "insert into t1 (v1) values (1);", []tok{
		{token.Insert, "insert"}, {token.Space, " "}, {token.Into, "into"}, {token.Space, " "}, {token.ID, "t1"}, {token.Space, " "},
		{token.LP, "("}, {token.ID, "v1"}, {token.RP, ")"}, {token.Space, " "}, {token.Values, "values"}, {token.Space, " "},
		{token.LP, "("}, {token.Integer, "1"}, {token.RP, ")"}, {token.Semi, ";"},
	}},
}

func TestTokens(t *testing.T) {
	for _, c := range cases {
		t.Run("should tokenize "+c.name+" as SQLite does", func(t *testing.T) {
			var got []tok
			for _, k := range token.Tokens(c.src) {
				got = append(got, tok{k.Kind, k.Text(c.src)})
			}
			is.EqualSlice(t, c.tokens, got)
			compare(t, c.src)
		})
	}

	t.Run("should give each token's offset and length", func(t *testing.T) {
		is.EqualSlice(t, []token.Token{
			{Kind: token.Select, Offset: 0, Len: 6},
			{Kind: token.Space, Offset: 6, Len: 1},
			{Kind: token.Integer, Offset: 7, Len: 1},
		}, token.Tokens("select 1"))
	})

	t.Run("should tokenize every input of one and two bytes as SQLite does", func(t *testing.T) {
		b := make([]byte, 2)
		for i := range 256 {
			b[0] = byte(i)
			compare(t, string(b[:1]))
			for j := range 256 {
				b[1] = byte(j)
				compare(t, string(b))
			}
		}
	})

	t.Run("should tokenize every input of three and four bytes from an alphabet of interesting bytes as SQLite does", func(t *testing.T) {
		alphabet := []byte("\x00\t\n\v \x80\xbb\xbf\xef!\"#$'()*-./019:<=>?@EX[]_`aefx|")
		b := make([]byte, 4)
		for _, c0 := range alphabet {
			for _, c1 := range alphabet {
				for _, c2 := range alphabet {
					b[0], b[1], b[2] = c0, c1, c2
					compare(t, string(b[:3]))
					for _, c3 := range alphabet {
						b[3] = c3
						compare(t, string(b))
					}
				}
			}
		}
	})
}

func TestNext(t *testing.T) {
	t.Run("should give an illegal token of length 0 at the end of the input", func(t *testing.T) {
		kind, n := token.Next("")
		is.Equal(t, token.Illegal, kind)
		is.Equal(t, 0, n)
	})

	t.Run("should give an illegal token of length 0 at a NUL byte", func(t *testing.T) {
		kind, n := token.Next("\x00a")
		is.Equal(t, token.Illegal, kind)
		is.Equal(t, 0, n)
	})

	t.Run("should give the first token only", func(t *testing.T) {
		kind, n := token.Next("select 1")
		is.Equal(t, token.Select, kind)
		is.Equal(t, 6, n)
	})
}

func TestKeywords(t *testing.T) {
	keywords := sqlite.Keywords()
	is.Equal(t, 147, len(keywords))

	t.Run("should give every keyword of SQLite its kind, in any case, as SQLite does", func(t *testing.T) {
		for _, k := range keywords {
			for _, word := range []string{strings.ToLower(k), k, mixed(k)} {
				kind, n := token.Next(word)
				is.True(t, kind.IsKeyword(), word)
				is.Equal(t, len(word), n, word)
				compare(t, word)
			}
		}
	})

	t.Run("should give words next to keywords their kinds as SQLite does", func(t *testing.T) {
		for _, k := range keywords {
			words := []string{k[:len(k)-1], k[1:], k + "S", k + "_", k + "1", "A" + k, k + "\x80", k[:1] + "_" + k[1:]}
			for _, word := range words {
				compare(t, word)
				compare(t, strings.ToLower(word))
			}
		}
	})

	t.Run("should end a keyword at a byte that SQLite folds onto a letter or '_' but that is no letter", func(t *testing.T) {
		// keywordCode compares bytes with the bit 0x20 cleared, which maps '\x7f' onto '_' and '@'
		// onto '`', but such bytes end the word before the comparison.
		compare(t, "current\x7fdate")
		compare(t, "current\x7fdate"[:8])
		compare(t, "SELECT\x00")
	})
}

// mixed case of s: upper and lower case in turn.
func mixed(s string) string {
	b := []byte(strings.ToLower(s))
	for i := 0; i < len(b); i += 2 {
		b[i] = strings.ToUpper(string(b[i]))[0]
	}
	return string(b)
}

func TestKind_String(t *testing.T) {
	t.Run("should give the name of the kind's TK_ code in SQLite", func(t *testing.T) {
		is.Equal(t, "SELECT", token.Select.String())
		is.Equal(t, "LIKE_KW", token.LikeKw.String())
		is.Equal(t, "ILLEGAL", token.Illegal.String())
	})

	t.Run("should give the number of a kind that does not exist", func(t *testing.T) {
		is.Equal(t, "Kind(255)", token.Kind(255).String())
	})
}

func TestKind(t *testing.T) {
	codes := tokenCodes(t)

	t.Run("should name every kind after a distinct TK_ code of SQLite", func(t *testing.T) {
		seen := map[int]token.Kind{}
		for k := range allKinds() {
			code, ok := codes[k.String()]
			is.True(t, ok, "no TK_"+k.String())
			other, ok := seen[code]
			is.True(t, !ok, fmt.Sprint(k, " and ", other, " have the same code"))
			seen[code] = k
		}
	})

	t.Run("should have a kind for every TK_ code that sqlite3GetToken can give", func(t *testing.T) {
		b, err := os.ReadFile("../../sqlite/c/sqlite3.c")
		is.NotError(t, err)
		src := string(b)

		// The non-keyword codes are assigned in sqlite3GetToken itself; the keyword codes are in
		// the table aKWCode of keywordhash.h.
		start := strings.Index(src, "SQLITE_PRIVATE i64 sqlite3GetToken(const unsigned char *z, int *tokenType){")
		is.True(t, start >= 0)
		end := strings.Index(src[start:], "\n}\n")
		is.True(t, end >= 0)
		body := src[start : start+end]
		assigned := regexp.MustCompile(`\*tokenType = (?:c==']' \? )?TK_(\w+)(?: : TK_(\w+))?;`).FindAllStringSubmatch(body, -1)
		is.True(t, len(assigned) > 30, "too few assignments; is the pattern above still right?")

		table := regexp.MustCompile(`(?s)static const unsigned char aKWCode\[148\] = \{0,(.*?)\};`).FindStringSubmatch(src)
		is.True(t, table != nil)
		keywords := regexp.MustCompile(`TK_(\w+)`).FindAllStringSubmatch(table[1], -1)
		is.Equal(t, 147, len(keywords))

		names := map[string]bool{}
		for k := range allKinds() {
			names[k.String()] = true
		}
		used := map[string]bool{}
		for _, m := range append(assigned, keywords...) {
			for _, name := range m[1:] {
				if name != "" {
					is.True(t, names[name], "no kind for TK_"+name)
					used[name] = true
				}
			}
		}
		// And no kind is left over that sqlite3GetToken never gives.
		is.Equal(t, len(names), len(used))
	})
}

// allKinds from [token.Illegal] to [token.Without], which is the last.
func allKinds() func(func(token.Kind) bool) {
	return func(yield func(token.Kind) bool) {
		for k := token.Illegal; k <= token.Without; k++ {
			if !yield(k) {
				return
			}
		}
	}
}

func FuzzTokens(f *testing.F) {
	for _, c := range cases {
		f.Add([]byte(c.src))
	}
	for _, s := range statements {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		compare(t, string(b))
	})
}

// statements of SQL for the seed corpus of [FuzzTokens], which use many kinds of tokens.
var statements = []string{
	"create table if not exists main.\"t 1\" ([a] integer primary key autoincrement, `b` text collate nocase default 'x', c real check (c > 0.5e-3)) strict;",
	"select a, b as \"B\", count(*) filter (where c is not null) over (partition by a order by b rows between unbounded preceding and current row) from t1 left outer join t2 using (a) where x glob 'a*' group by 1 having sum(c) <> 0 order by 2 desc nulls last limit 10 offset ?1;",
	"insert or replace into t (a, b) values (:a, @b), ($c::d(e), #f) on conflict (a) do update set b = excluded.b returning *;",
	"with recursive c(x) as (select 1 union all select x + 1 from c where x < 1_000) select x, x'cafe', 0xFF, -x, ~x, x || 'a', x -> '$.a', x ->> 'b', x << 2 >> 1 & 3 | 4 % 5 from c;",
	"update t set a = case when b == 1 then 'one' else null end where rowid in (select rowid from t limit 1);",
	"delete from t where a between 1 and 2 or b not like '%x%' escape '\\' and c isnull and d notnull;",
	"begin immediate transaction; savepoint s; release s; rollback to savepoint s; commit;",
	"create trigger tr after insert on t for each row when new.a > 0 begin select raise(abort, 'no'); end;",
	"create view v as select * from t natural join u cross join w; drop view if exists v; vacuum; analyze; reindex t; pragma foreign_keys = on;",
	"alter table t rename column a to b; alter table t add column c generated always as (a + 1) stored;",
	"select current_date, current_time, current_timestamp, cast('1' as integer), 1.5e+10, .5, 5., 'it''s' /* comment */ -- end\n;",
	"explain query plan select * from t indexed by i where a match 'x' and b regexp 'y';",
	"create temp table t (a int references u (a) on delete cascade on update set null deferrable initially deferred, unique (a) on conflict ignore) without rowid;",
	"window w as (order by a range between 1 preceding and 1 following exclude ties) groups others no action restrict materialized nothing",
	"attach database 'x.db' as x; detach x; select * from x.t intersect select * from y.t except select * from z.t;",
}

// compare the tokens of src with those of SQLite's own tokenizer: the same kinds, lengths, and end.
func compare(t *testing.T, src string) {
	t.Helper()
	want := oracle(t, src)
	got := token.Tokens(src)
	end := 0
	if len(got) > 0 {
		last := got[len(got)-1]
		end = last.Offset + last.Len
	}
	kind, n := token.Next(src[end:])
	got = append(got, token.Token{Kind: kind, Offset: end, Len: n})
	if !slices.Equal(want, got) {
		t.Fatalf("tokens of %q:\nSQLite: %v\nGo:     %v", src, want, got)
	}
}

// oracle tokens of src, from SQLite's own tokenizer, with the last one of length 0 at the end.
func oracle(t *testing.T, src string) []token.Token {
	t.Helper()
	kinds := kindsByCode(t)
	var tokens []token.Token
	offset := 0
	for _, s := range sqlite.Tokenize(src) {
		kind, ok := kinds[s.Type]
		if !ok {
			t.Fatalf("no kind for TK_ code %v in the tokens of %q", s.Type, src)
		}
		tokens = append(tokens, token.Token{Kind: kind, Offset: offset, Len: s.Len})
		offset += s.Len
	}
	return tokens
}

// kindsByCode of SQLite's TK_ codes.
func kindsByCode(t *testing.T) map[int]token.Kind {
	t.Helper()
	kinds, err := kindsByCodeOnce()
	is.NotError(t, err)
	return kinds
}

var kindsByCodeOnce = sync.OnceValues(func() (map[int]token.Kind, error) {
	codes, err := readTokenCodes()
	if err != nil {
		return nil, err
	}
	kinds := map[int]token.Kind{}
	for k := range allKinds() {
		code, ok := codes[k.String()]
		if !ok {
			return nil, fmt.Errorf("no TK_%v", k)
		}
		kinds[code] = k
	}
	return kinds, nil
})

// tokenCodes of SQLite's parser, by name without the TK_ prefix.
func tokenCodes(t *testing.T) map[string]int {
	t.Helper()
	codes, err := readTokenCodes()
	is.NotError(t, err)
	return codes
}

// readTokenCodes from their definitions in the vendored c/sqlite3.c, which has them twice, once from
// parse.h and once in parse.c. The two must agree.
func readTokenCodes() (map[string]int, error) {
	b, err := os.ReadFile("../../sqlite/c/sqlite3.c")
	if err != nil {
		return nil, err
	}
	codes := map[string]int{}
	matches := regexp.MustCompile(`(?m)^#define TK_(\w+) +(\d+)$`).FindAllSubmatch(b, -1)
	for _, m := range matches {
		name := string(m[1])
		code, err := strconv.Atoi(string(m[2]))
		if err != nil {
			return nil, err
		}
		if c, ok := codes[name]; ok && c != code {
			return nil, fmt.Errorf("TK_%v is both %v and %v", name, c, code)
		}
		codes[name] = code
	}
	if len(codes) < 150 || len(matches) != 2*len(codes) {
		return nil, fmt.Errorf("found %v TK_ codes in %v definitions; is the pattern still right?", len(codes), len(matches))
	}
	return codes, nil
}
