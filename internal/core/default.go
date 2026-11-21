package core

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// defaultSQL is the literal with which the cache declares the default v of a column, after
// DEFAULT, in the one form that this package writes for each value, which [parseDefault] reads back:
// an integer in decimal with a sign only if it is negative, a real as the shortest decimal in
// exponent form that reads back to its bits, as strconv writes it with 'e' and the precision -1,
// text in single quotes with each single quote doubled, a blob as an x and its bytes in lowercase
// hexadecimal in single quotes, and null. SQLite reads each of these back as the value itself: a
// real in exponent form is a real, and SQLite takes the minus of a negative number after DEFAULT as
// part of the literal, so the smallest integer is an integer. The value is a valid default, as
// [format.CommitWriter] checks it: never -0.0, NaN, or infinite, and text is valid UTF-8 without
// the character NUL.
func defaultSQL(v *s4qlpb.Value) string {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return fmt.Sprint(v.GetInteger())
	case s4qlpb.Value_Real_case:
		return strconv.FormatFloat(v.GetReal(), 'e', -1, 64)
	case s4qlpb.Value_Text_case:
		return "'" + strings.ReplaceAll(v.GetText(), "'", "''") + "'"
	case s4qlpb.Value_Blob_case:
		return "x'" + hex.EncodeToString(v.GetBlob()) + "'"
	default:
		return "null"
	}
}

// parseDefault reads the default of a column of the type typ from the text that SQLite reports for
// it, which is the literal as the declaration wrote it. It takes only what [defaultSQL] writes for
// a valid default of the type, NULL included, and reports false for any other text: another
// spelling of the same value, such as +1, 1.0, 00012, X'AB', or a literal in double quotes, a value
// of another storage class, an expression, and a real or a text that no valid default holds. So a
// default that this package did not write is never read as one that it did.
func parseDefault(sql string, typ s4qlpb.Type) (*s4qlpb.Value, bool) {
	var v *s4qlpb.Value
	switch {
	case sql == "null":
		v = s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()
	case typ == s4qlpb.Type_TYPE_INT || typ == s4qlpb.Type_TYPE_INTEGER:
		i, err := strconv.ParseInt(sql, 10, 64)
		if err != nil {
			return nil, false
		}
		v = s4qlpb.Value_builder{Integer: new(i)}.Build()
	case typ == s4qlpb.Type_TYPE_REAL:
		r, err := strconv.ParseFloat(sql, 64)
		if err != nil || math.IsNaN(r) || math.IsInf(r, 0) || r == 0 && math.Signbit(r) {
			return nil, false
		}
		v = s4qlpb.Value_builder{Real: new(r)}.Build()
	case typ == s4qlpb.Type_TYPE_TEXT:
		if len(sql) < 2 || sql[0] != '\'' || sql[len(sql)-1] != '\'' {
			return nil, false
		}
		text := strings.ReplaceAll(sql[1:len(sql)-1], "''", "'")
		if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
			return nil, false
		}
		v = s4qlpb.Value_builder{Text: new(text)}.Build()
	case typ == s4qlpb.Type_TYPE_BLOB:
		if len(sql) < 3 || !strings.HasPrefix(sql, "x'") || sql[len(sql)-1] != '\'' {
			return nil, false
		}
		b, err := hex.DecodeString(sql[2 : len(sql)-1])
		if err != nil {
			return nil, false
		}
		// DecodeString gives an empty slice for no bytes, which the builder takes for a blob; nil would
		// be no value.
		v = s4qlpb.Value_builder{Blob: b}.Build()
	default:
		return nil, false
	}
	// A text that reads as the value and is not what defaultSQL writes for it is another spelling,
	// such as +1 or 1e0, or, for text, a lone single quote inside the quotes.
	return v, defaultSQL(v) == sql
}

// withoutNegativeZero is ct, or a copy of it in which each default that is the real -0.0 is 0.0,
// which is what SQLite stores in a column for -0.0, and what the commit format holds: see
// [format.CommitReader].
func withoutNegativeZero(ct *s4qlpb.CreateTable) *s4qlpb.CreateTable {
	var copied *s4qlpb.CreateTable
	for i, c := range ct.GetColumns() {
		d := c.GetDefault()
		if d.WhichKind() != s4qlpb.Value_Real_case || d.GetReal() != 0 || !math.Signbit(d.GetReal()) {
			continue
		}
		if copied == nil {
			copied = proto.CloneOf(ct)
		}
		copied.GetColumns()[i].SetDefault(s4qlpb.Value_builder{Real: new(0.0)}.Build())
	}
	if copied == nil {
		return ct
	}
	return copied
}
