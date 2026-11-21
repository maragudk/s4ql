package core

import (
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
)

func TestDefaultSQL(t *testing.T) {
	integer := func(i int64) *s4qlpb.Value { return s4qlpb.Value_builder{Integer: new(i)}.Build() }
	realValue := func(r float64) *s4qlpb.Value { return s4qlpb.Value_builder{Real: new(r)}.Build() }
	text := func(s string) *s4qlpb.Value { return s4qlpb.Value_builder{Text: new(s)}.Build() }
	blob := func(b []byte) *s4qlpb.Value { return s4qlpb.Value_builder{Blob: b}.Build() }
	null := s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()

	t.Run("should write each value in its one form, and read that form back as the value for a column of its type, and as NULL for null", func(t *testing.T) {
		for _, test := range []struct {
			v   *s4qlpb.Value
			typ s4qlpb.Type
			sql string
		}{
			{integer(0), s4qlpb.Type_TYPE_INT, "0"},
			{integer(-1), s4qlpb.Type_TYPE_INTEGER, "-1"},
			{integer(math.MinInt64), s4qlpb.Type_TYPE_INT, "-9223372036854775808"},
			{integer(math.MaxInt64), s4qlpb.Type_TYPE_INT, "9223372036854775807"},
			{realValue(0), s4qlpb.Type_TYPE_REAL, "0e+00"},
			{realValue(1), s4qlpb.Type_TYPE_REAL, "1e+00"},
			{realValue(-1.5), s4qlpb.Type_TYPE_REAL, "-1.5e+00"},
			{realValue(0.1), s4qlpb.Type_TYPE_REAL, "1e-01"},
			{realValue(math.SmallestNonzeroFloat64), s4qlpb.Type_TYPE_REAL, "5e-324"},
			{realValue(-math.MaxFloat64), s4qlpb.Type_TYPE_REAL, "-1.7976931348623157e+308"},
			{text(""), s4qlpb.Type_TYPE_TEXT, "''"},
			{text("it's"), s4qlpb.Type_TYPE_TEXT, "'it''s'"},
			{text("''"), s4qlpb.Type_TYPE_TEXT, "''''''"},
			{text("null"), s4qlpb.Type_TYPE_TEXT, "'null'"},
			{text("a\nb\"c"), s4qlpb.Type_TYPE_TEXT, "'a\nb\"c'"},
			{blob([]byte{}), s4qlpb.Type_TYPE_BLOB, "x''"},
			{blob([]byte{0, 0xab, 0xff}), s4qlpb.Type_TYPE_BLOB, "x'00abff'"},
			{null, s4qlpb.Type_TYPE_INT, "null"},
			{null, s4qlpb.Type_TYPE_REAL, "null"},
			{null, s4qlpb.Type_TYPE_TEXT, "null"},
			{null, s4qlpb.Type_TYPE_BLOB, "null"},
		} {
			is.Equal(t, test.sql, defaultSQL(test.v))
			got, ok := parseDefault(test.sql, test.typ)
			is.True(t, ok, test.sql)
			is.True(t, proto.Equal(test.v, got), test.sql)
		}
	})

	t.Run("should read back every value that it writes, bit for bit, and only for a column of the value's type", func(t *testing.T) {
		r := rand.New(rand.NewPCG(1, 20261120))
		kinds := map[s4qlpb.Type]func() *s4qlpb.Value{
			s4qlpb.Type_TYPE_INT:     func() *s4qlpb.Value { return integer(int64(r.Uint64())) },
			s4qlpb.Type_TYPE_INTEGER: func() *s4qlpb.Value { return integer(int64(r.Uint64() >> r.IntN(64))) },
			s4qlpb.Type_TYPE_REAL: func() *s4qlpb.Value {
				for {
					f := math.Float64frombits(r.Uint64())
					if r.IntN(4) == 0 {
						f = math.Ldexp(r.Float64(), -1074+r.IntN(2100))
					}
					if !math.IsNaN(f) && !math.IsInf(f, 0) && !(f == 0 && math.Signbit(f)) {
						return realValue(f)
					}
				}
			},
			s4qlpb.Type_TYPE_TEXT: func() *s4qlpb.Value {
				var b strings.Builder
				for range r.IntN(20) {
					c := rune(1 + r.IntN(utf8.MaxRune))
					if r.IntN(3) == 0 {
						c = rune("'\"x\n\\"[r.IntN(5)])
					}
					if utf8.ValidRune(c) {
						b.WriteRune(c)
					}
				}
				return text(b.String())
			},
			s4qlpb.Type_TYPE_BLOB: func() *s4qlpb.Value {
				b := make([]byte, r.IntN(20))
				for i := range b {
					b[i] = byte(r.IntN(256))
				}
				return blob(b)
			},
		}
		for range 20_000 {
			for typ, value := range kinds {
				v := value()
				sql := defaultSQL(v)
				got, ok := parseDefault(sql, typ)
				is.True(t, ok, sql)
				is.True(t, proto.Equal(v, got), sql)
				is.True(t, v.WhichKind() != s4qlpb.Value_Real_case || math.Float64bits(v.GetReal()) == math.Float64bits(got.GetReal()), sql)
				for other := range kinds {
					// INT and INTEGER hold the same values.
					same := other == typ || v.HasInteger() && (other == s4qlpb.Type_TYPE_INT || other == s4qlpb.Type_TYPE_INTEGER)
					if _, ok := parseDefault(sql, other); ok && !same {
						t.Fatalf("%v is read as a default of type %v, and was written for %v", sql, other, typ)
					}
				}
			}
		}
	})

	t.Run("should not read a text that it does not write for a column of the type", func(t *testing.T) {
		for typ, refused := range map[s4qlpb.Type][]string{
			s4qlpb.Type_TYPE_INT:     {"", "+1", "01", "00012", "- 1", "-0", "1.0", "1e0", "0x10", "1_000", "9223372036854775808", "-9223372036854775809", "'1'", "x'01'", "NULL", "Null", " null", "null ", "(1)", " 1", "1 ", "true", "current_time", `"1"`},
			s4qlpb.Type_TYPE_INTEGER: {"+0", "1.", "1e+00", "١"},
			s4qlpb.Type_TYPE_REAL: {"1", "1.0", "1.5", "0.5", "5e-1", "1E+00", "1e+0", "1e00", "+1e+00", "-0e+00", "0e-00", "NaN", "nan", "Inf", "+Inf", "-Inf", "1e+999", "1e-400", "0x1p-2",
				"1_0e+00", "'1e+00'", "1e+00 ", "10e+00", "1.50e+00", "1.7976931348623159e+308"},
			s4qlpb.Type_TYPE_TEXT: {"", "'", "a", "'a", "a'", "'a'b'", "'''", `"a"`, "X'61'", "x'61'", "1", "null'", "'it's'", "N'a'", "'\x00'", "'\xff'", "'a'' "},
			s4qlpb.Type_TYPE_BLOB: {"", "x'", "x'0'", "X'00'", "x'AB'", "x'0g'", "x'00", "x 'aa'", "''", "'ab'", "0", "x''x''"},
		} {
			for _, sql := range refused {
				_, ok := parseDefault(sql, typ)
				is.True(t, !ok, typeNames[typ], sql)
			}
		}
		_, ok := parseDefault("null", s4qlpb.Type_TYPE_UNSPECIFIED)
		is.True(t, ok, "null")
		_, ok = parseDefault("1", s4qlpb.Type_TYPE_UNSPECIFIED)
		is.True(t, !ok, "a default of an unknown type")
	})
}

func TestWithoutNegativeZero(t *testing.T) {
	t.Run("should give the table it is handed when no default is -0.0, and a copy with 0.0 where one is", func(t *testing.T) {
		column := func(typ s4qlpb.Type, v *s4qlpb.Value) *s4qlpb.Column {
			return s4qlpb.Column_builder{Name: new("c"), Type: new(typ), Default: v}.Build()
		}
		zero := s4qlpb.Value_builder{Real: new(0.0)}.Build()
		ct := s4qlpb.CreateTable_builder{Table: new("t"), Columns: []*s4qlpb.Column{
			column(s4qlpb.Type_TYPE_REAL, zero), column(s4qlpb.Type_TYPE_REAL, nil), column(s4qlpb.Type_TYPE_INT, s4qlpb.Value_builder{Integer: new(int64(0))}.Build()),
		}, PrimaryKey: []uint32{0}}.Build()
		is.True(t, withoutNegativeZero(ct) == ct, "a table without -0.0 is copied")

		ct.GetColumns()[1].SetDefault(s4qlpb.Value_builder{Real: new(math.Copysign(0, -1))}.Build())
		got := withoutNegativeZero(ct)
		is.True(t, got != ct, "the table with -0.0 is changed in place")
		is.True(t, math.Signbit(ct.GetColumns()[1].GetDefault().GetReal()), "the table that was handed in changed")
		is.True(t, !math.Signbit(got.GetColumns()[1].GetDefault().GetReal()), "the copy holds -0.0")
		ct.GetColumns()[1].SetDefault(zero)
		is.True(t, proto.Equal(ct, got), "the copy differs in another way")
	})
}
