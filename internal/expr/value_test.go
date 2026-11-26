package expr_test

import (
	"math"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
)

func TestValue(t *testing.T) {
	t.Run("should give an integer as itself, and no real", func(t *testing.T) {
		v := expr.IntegerValue(math.MinInt64)
		i, ok := v.Integer()
		is.True(t, ok)
		is.Equal(t, int64(math.MinInt64), i)
		f, ok := v.Real()
		is.True(t, !ok)
		is.Equal(t, 0.0, f)
		is.True(t, !v.Null())
		is.Equal(t, expr.TypeInteger, v.Type())
		is.Equal(t, "-9223372036854775808", v.String())
		is.Equal(t, int64(math.MinInt64), v.Proto().GetInteger())
		is.True(t, v.Proto().HasInteger())
	})

	t.Run("should give a real bit for bit, also negative zero, and no integer", func(t *testing.T) {
		v := expr.RealValue(math.Copysign(0, -1))
		f, ok := v.Real()
		is.True(t, ok)
		is.True(t, f == 0 && math.Signbit(f))
		i, ok := v.Integer()
		is.True(t, !ok)
		is.Equal(t, int64(0), i)
		is.Equal(t, expr.TypeReal, v.Type())
		is.Equal(t, "-0", v.String())
		is.Equal(t, "1.5", expr.RealValue(1.5).String())
		is.Equal(t, "-1.5", expr.RealValue(-1.5).String())
		is.Equal(t, "0", expr.RealValue(0).String())
		is.True(t, math.Signbit(v.Proto().GetReal()))
		is.True(t, v.Proto().HasReal())
		is.True(t, v != expr.RealValue(0), "negative zero is not zero")
	})

	t.Run("should give text as itself, quoted as a literal, and no blob", func(t *testing.T) {
		v := expr.TextValue("it's é")
		s, ok := v.Text()
		is.True(t, ok)
		is.Equal(t, "it's é", s)
		b, ok := v.Blob()
		is.True(t, !ok)
		is.True(t, b == nil)
		_, ok = v.Integer()
		is.True(t, !ok)
		is.True(t, !v.Null())
		is.Equal(t, expr.TypeText, v.Type())
		is.Equal(t, "'it''s é'", v.String())
		is.Equal(t, "it's é", v.Proto().GetText())
		is.True(t, v.Proto().HasText())
		is.True(t, v != expr.TextValue("it's e"), "texts of other bytes differ")
		is.True(t, expr.TextValue("") != expr.Value{}, "the empty text is not NULL")
	})

	t.Run("should give a blob as a copy of its bytes, as a literal, and no text", func(t *testing.T) {
		bytes := []byte{0, 0xab}
		v := expr.BlobValue(bytes)
		bytes[0] = 1
		b, ok := v.Blob()
		is.True(t, ok)
		is.EqualSlice(t, []byte{0, 0xab}, b)
		_, ok = v.Text()
		is.True(t, !ok)
		is.Equal(t, expr.TypeBlob, v.Type())
		is.Equal(t, "x'00AB'", v.String())
		is.EqualSlice(t, []byte{0, 0xab}, v.Proto().GetBlob())
		is.True(t, v.Proto().HasBlob())
		is.True(t, v != expr.TextValue("\x00\xab"), "a blob is not a text of its bytes")
		is.True(t, expr.BlobValue(nil) != expr.TextValue(""), "the empty blob is not the empty text")
	})

	t.Run("should give NULL for the zero value", func(t *testing.T) {
		var v expr.Value
		is.True(t, v.Null())
		i, ok := v.Integer()
		is.True(t, !ok)
		is.Equal(t, int64(0), i)
		f, ok := v.Real()
		is.True(t, !ok)
		is.Equal(t, 0.0, f)
		is.Equal(t, expr.TypeNull, v.Type())
		is.Equal(t, "NULL", v.String())
		is.True(t, v.Proto().HasNull())
	})
}

func TestType_String(t *testing.T) {
	t.Run("should name each type, and none for no type", func(t *testing.T) {
		is.Equal(t, "integer", expr.TypeInteger.String())
		is.Equal(t, "real", expr.TypeReal.String())
		is.Equal(t, "null", expr.TypeNull.String())
		is.Equal(t, "text", expr.TypeText.String())
		is.Equal(t, "blob", expr.TypeBlob.String())
		is.Equal(t, "", expr.Type(0).String())
	})
}
