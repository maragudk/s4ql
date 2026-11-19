package stmttest

import (
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// LengthLimit is the length limit, in bytes, that tests lower the cache and SQLite to. [Text] and
// [Blob] now and then give a value of about that length: a few bytes below it, at it, and one byte
// above it, so that at the lowered limit the value or the record of its row is too long.
const LengthLimit = 1000

// Types of a column, in the order of [stmt.Type].
var Types = []stmt.Type{stmt.TypeInt, stmt.TypeInteger, stmt.TypeReal, stmt.TypeText, stmt.TypeBlob}

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

// Real at random from r, finite: zero and negative zero, small numbers, numbers with few digits,
// the limits of float64 and the subnormal ones, reals that are hard to round, powers of ten, and
// any.
func Real(r Rand) float64 {
	sign := 1.0
	if r.IntN(3) == 0 {
		sign = -1
	}
	switch r.IntN(10) {
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
	case 6:
		// Hard to round: halfway between two floats in the shortest digits, or next to an integer of
		// 2^53 and above, where the floats are 2 and more apart.
		return sign * []float64{5e-324, 4.9406564584124654e-324, 2.4703282292062328e-324, 1.7976931348623157e308,
			8.98846567431158e307, 9007199254740992, 9007199254740994, 1 << 62, 1<<63 - 1024, 0.30000000000000004,
			123456789012345678, 1.0000000000000002, 0.9999999999999999}[r.IntN(13)]
	default:
		for {
			f := math.Float64frombits(r.Uint64())
			if !math.IsInf(f, 0) && !math.IsNaN(f) {
				return f
			}
		}
	}
}

// stringPool holds the content of strings: empty, with quotes, with what looks like SQL, with what
// SQLite would read as a number, and with letters of more than one byte.
var stringPool = []string{"", "a", "A", "'", "''", "a'b", "it's", `"`, "--", "/*", "*/", " ", "\t\n", "x'01'", "1",
	"1.5", "-0", "1e999", "inf", "NaN", "null", "été", "ÉTÉ", "ÆØÅ", "😀", "\U0010ffff", "日本語", "a b c", "\\", "?",
	";", "select 1;", strings.Repeat("x", 300)}

// Text at random from r: from a pool of edges, any runes but NUL and the surrogates, mostly from
// ASCII, and one time in 30 a text of about [LengthLimit] bytes. It is valid UTF-8 without a NUL.
func Text(r Rand) string {
	switch n := r.IntN(30); {
	case n == 0:
		return long(r, func(n int) string { return runes(r, n) })
	case n < 15:
		return stringPool[r.IntN(len(stringPool))]
	}
	return runes(r, r.IntN(12))
}

// runes at random from r: n of them, but never NUL or a surrogate, mostly from ASCII.
func runes(r Rand, n int) string {
	var b strings.Builder
	for range n {
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
	return b.String()
}

// long value at random from r, of about [LengthLimit] bytes: some of start(n), padded with x to
// its length.
func long(r Rand, start func(n int) string) string {
	length := LengthLimit - 6 + r.IntN(8)
	s := start(r.IntN(20))
	if len(s) > length {
		s = s[:0]
	}
	return s + strings.Repeat("x", length-len(s))
}

// Blob at random from r: empty, of zero bytes, of any bytes, and one time in 30 a blob of about
// [LengthLimit] bytes. It is never nil.
func Blob(r Rand) []byte {
	switch n := r.IntN(30); {
	case n == 0:
		b := []byte(long(r, func(int) string { return "" }))
		for i := range r.IntN(len(b)) {
			b[i] = byte(r.IntN(256))
		}
		return b
	case n < 6:
		return []byte{}
	case n < 12:
		return make([]byte, 1+r.IntN(3))
	}
	b := make([]byte, r.IntN(20))
	for i := range b {
		b[i] = byte(r.IntN(256))
	}
	return b
}

// ValueOf the type t at random from r: an [Integer] for INT and INTEGER, a [Real] for REAL, a [Text]
// for TEXT, and a [Blob] for BLOB.
func ValueOf(r Rand, t stmt.Type) stmt.Value {
	switch t {
	case stmt.TypeInt, stmt.TypeInteger:
		return stmt.Integer(Integer(r))
	case stmt.TypeReal:
		return stmt.Real(Real(r))
	case stmt.TypeText:
		return stmt.Text(Text(r))
	case stmt.TypeBlob:
		return stmt.Blob(Blob(r))
	}
	panic(fmt.Sprintf("no type %v", t))
}

// Argument of the value v, as database/sql hands it to a driver: an int64, a float64, a string, or
// a []byte that is not nil.
func Argument(v stmt.Value) any {
	switch v := v.(type) {
	case stmt.Integer:
		return int64(v)
	case stmt.Real:
		return float64(v)
	case stmt.Text:
		return string(v)
	case stmt.Blob:
		if v == nil {
			return []byte{}
		}
		return []byte(v)
	}
	panic(fmt.Sprintf("no value %#v", v))
}

// Literal of the value v at random from r: its text, with a sign before the number if it is
// negative and maybe with a gap after the sign, and the value that SQLite gives it, which is v but
// for a real in more than 17 digits, which SQLite may read as a neighbour of v. An integer is
// written with leading zeros now and then, and 0 as -0; a real in one of the forms of a real literal
// of SQLite; a string in single quotes, with two single quotes for a quote; and a blob with an x
// or an X and hex digits in lower case, upper case, or both.
func Literal(r Rand, v stmt.Value) (string, stmt.Value) {
	switch v := v.(type) {
	case stmt.Integer:
		digits := strings.TrimPrefix(fmt.Sprint(int64(v)), "-")
		if r.IntN(4) == 0 {
			digits = strings.Repeat("0", 1+r.IntN(30)) + digits
		}
		if v < 0 || (v == 0 && r.IntN(4) == 0) {
			return "-" + signGap(r) + digits, v
		}
		return digits, v
	case stmt.Real:
		text, value := realLiteral(r, math.Abs(float64(v)))
		if math.Signbit(float64(v)) {
			return "-" + signGap(r) + text, stmt.Real(-value)
		}
		return text, stmt.Real(value)
	case stmt.Text:
		return "'" + strings.ReplaceAll(string(v), "'", "''") + "'", v
	case stmt.Blob:
		digits := []byte(fmt.Sprintf("%x", []byte(v)))
		switch r.IntN(3) {
		case 0:
			digits = []byte(strings.ToUpper(string(digits)))
		case 1:
			for i, c := range digits {
				if c >= 'a' && r.IntN(2) == 0 {
					digits[i] = c - 'a' + 'A'
				}
			}
		}
		if v == nil {
			v = stmt.Blob{}
		}
		return []string{"x", "X"}[r.IntN(2)] + "'" + string(digits) + "'", v
	}
	panic(fmt.Sprintf("no value %#v", v))
}

// signGap after a sign: none, or a space, a comment, or a new line. None starts with "-", which
// would make a comment of the sign.
func signGap(r Rand) string {
	return []string{"", "", " ", "/* c */", "\n"}[r.IntN(5)]
}

// realLiteral of f, which is not negative, at random from r, in one of the forms of a real literal
// of SQLite, and the float64 that SQLite reads from it. The text has the shortest digits that
// strconv gives, which round to f, so its value is f, as SQLite rounds those digits to the nearest
// float; or, in one form, from 17 to 40 digits, whose value is what the pass reads, which is
// SQLite's bit for bit.
func realLiteral(r Rand, f float64) (string, float64) {
	digits := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(digits, "e")
	var text string
	switch r.IntN(7) {
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
		text = strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += "."
		}
	case 3:
		// Without an exponent and without a zero before the point, such as .5, if it is below 1.
		text = strconv.FormatFloat(f, 'f', -1, 64)
		text = strings.TrimPrefix(text, "0")
		if !strings.Contains(text, ".") {
			text += ".0"
		}
	case 4:
		// With leading and trailing zeros, which do not change the value.
		text = strings.Repeat("0", r.IntN(5)) + strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += "."
		}
		text += strings.Repeat("0", r.IntN(5))
	case 5:
		// With 17 to 40 digits, more than SQLite reads exactly when they are more than 19.
		text = strconv.FormatFloat(f, 'e', 16+r.IntN(24), 64)
		if v, ok := readReal(text); ok {
			return text, v
		}
		text = digits
	default:
		// In upper case, with the exponent's sign, such as 1.5E+03.
		text = mantissa + "E" + exp
	}
	return text, f
}

// readReal of the text of a real literal, as the pass reads it, and whether the pass supports it.
func readReal(text string) (float64, bool) {
	s, err := stmt.Check("insert into t (v) values (" + text + ")")
	if err != nil {
		return 0, false
	}
	v, ok := s.(*stmt.Insert).Value.(stmt.Real)
	return float64(v), ok
}

// Unsupported are arguments that no column holds, which s4ql refuses: nil, a bool, a time.Time, an
// unsigned integer that does not fit a signed 64-bit integer, a real that is not a number or is
// infinite, text that holds NUL or is not valid UTF-8, and a nil []byte.
var Unsupported = []any{nil, true, time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC), uint64(math.MaxInt64) + 1,
	math.NaN(), math.Inf(1), math.Inf(-1), float32(math.Inf(-1)), "a\x00b", "\x00", "\xff", "a\xc3", []byte(nil)}

// LiteralKind of the value of a literal, for a count of what a test reaches: its type, or an edge of
// it: -0.0, the empty text, and the empty blob.
func LiteralKind(v stmt.Value) string {
	switch v := v.(type) {
	case stmt.Integer:
		return "integer"
	case stmt.Real:
		if v == 0 && math.Signbit(float64(v)) {
			return "real -0.0"
		}
		return "real"
	case stmt.Text:
		if v == "" {
			return "empty text"
		}
		return "text"
	case stmt.Blob:
		if len(v) == 0 {
			return "empty blob"
		}
		return "blob"
	}
	return fmt.Sprintf("%T", v)
}

// ArgumentKind of the argument a, for a count of what a test reaches: its Go type, or what makes s4ql
// refuse it, and "named" for a [database/sql.NamedArg].
func ArgumentKind(a any) string {
	switch v := a.(type) {
	case nil:
		return "nil"
	case sql.NamedArg:
		return "named"
	case float64:
		switch {
		case math.IsNaN(v):
			return "NaN"
		case math.IsInf(v, 0):
			return "infinite float64"
		case v == 0 && math.Signbit(v):
			return "float64 -0.0"
		}
	case float32:
		if math.IsInf(float64(v), 0) || math.IsNaN(float64(v)) {
			return "float32 that is not finite"
		}
	case string:
		switch {
		case !utf8.ValidString(v):
			return "string that is not valid UTF-8"
		case strings.ContainsRune(v, 0):
			return "string with NUL"
		case v == "":
			return "empty string"
		}
	case []byte:
		switch {
		case v == nil:
			return "nil []byte"
		case len(v) == 0:
			return "empty []byte"
		}
	case uint64:
		if v > math.MaxInt64 {
			return "uint64 above int64"
		}
	}
	return fmt.Sprintf("%T", a)
}
