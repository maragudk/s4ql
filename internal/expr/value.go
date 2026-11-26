package expr

import (
	"fmt"
	"math"
	"strings"

	"maragu.dev/s4ql/internal/s4qlpb"
)

// Value of an expression: an integer, a real, text, a blob, or NULL, which is the zero Value. Two
// values are equal with == when they have the same type and the same bits or bytes.
type Value struct {
	kind kind
	// bits of the integer, or of the real as [math.Float64bits] gives them.
	bits uint64
	// text, or the bytes of the blob.
	text string
}

// kind of a [Value].
type kind uint8

const (
	kindNull kind = iota
	kindInteger
	kindReal
	kindText
	kindBlob
)

// integerValue of i.
func integerValue(i int64) Value {
	return Value{kind: kindInteger, bits: uint64(i)}
}

// realValue of f, bit for bit.
func realValue(f float64) Value {
	return Value{kind: kindReal, bits: math.Float64bits(f)}
}

// textValue of s.
func textValue(s string) Value {
	return Value{kind: kindText, text: s}
}

// blobValue of the bytes of s.
func blobValue(s string) Value {
	return Value{kind: kindBlob, text: s}
}

// valueOf v, a value of a row or an argument, and of the type typ if typ is not 0 and v is not NULL.
// Text is not copied, and a blob is, which allocates.
func valueOf(v *s4qlpb.Value, typ Type) (Value, error) {
	var w Value
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		w = integerValue(v.GetInteger())
	case s4qlpb.Value_Real_case:
		w = realValue(v.GetReal())
	case s4qlpb.Value_Text_case:
		w = textValue(v.GetText())
	case s4qlpb.Value_Blob_case:
		w = blobValue(string(v.GetBlob()))
	case s4qlpb.Value_Null_case:
		return Value{}, nil
	default:
		return Value{}, internalError("a value of kind %v", v.WhichKind())
	}
	if typ != 0 && w.Type() != typ {
		return Value{}, internalError("a value of type %v in a column of type %v", w.Type(), typ)
	}
	return w, nil
}

// float of v, an integer or a real: the real itself, or the real nearest to the integer, the even one
// of two that are as near, as C converts an integer to a double and so SQLite.
func (v Value) float() float64 {
	if v.kind == kindInteger {
		return float64(int64(v.bits))
	}
	return math.Float64frombits(v.bits)
}

// Type of v: [TypeInteger], [TypeReal], [TypeText], [TypeBlob], or [TypeNull].
func (v Value) Type() Type {
	switch v.kind {
	case kindInteger:
		return TypeInteger
	case kindReal:
		return TypeReal
	case kindText:
		return TypeText
	case kindBlob:
		return TypeBlob
	}
	return TypeNull
}

// Null reports whether v is NULL.
func (v Value) Null() bool {
	return v.kind == kindNull
}

// Integer of v, and whether v is an integer.
func (v Value) Integer() (int64, bool) {
	if v.kind != kindInteger {
		return 0, false
	}
	return int64(v.bits), true
}

// Real of v, and whether v is a real. A real that arithmetic gives is finite.
func (v Value) Real() (float64, bool) {
	if v.kind != kindReal {
		return 0, false
	}
	return math.Float64frombits(v.bits), true
}

// Text of v, and whether v is text.
func (v Value) Text() (string, bool) {
	if v.kind != kindText {
		return "", false
	}
	return v.text, true
}

// Blob of v, which it allocates, and whether v is a blob.
func (v Value) Blob() ([]byte, bool) {
	if v.kind != kindBlob {
		return nil, false
	}
	return []byte(v.text), true
}

// Proto of v, which it allocates. Text is not copied.
func (v Value) Proto() *s4qlpb.Value {
	p := &s4qlpb.Value{}
	switch v.kind {
	case kindInteger:
		p.SetInteger(int64(v.bits))
	case kindReal:
		p.SetReal(math.Float64frombits(v.bits))
	case kindText:
		p.SetText(v.text)
	case kindBlob:
		p.SetBlob([]byte(v.text))
	default:
		p.SetNull(&s4qlpb.Null{})
	}
	return p
}

// String of v: the integer, the real as [fmt.Sprint] writes it, which writes negative zero as -0,
// text as a string literal with its quotes doubled, a blob as a blob literal in upper-case digits,
// or NULL.
func (v Value) String() string {
	switch v.kind {
	case kindInteger:
		return fmt.Sprint(int64(v.bits))
	case kindReal:
		return fmt.Sprint(math.Float64frombits(v.bits))
	case kindText:
		return "'" + strings.ReplaceAll(v.text, "'", "''") + "'"
	case kindBlob:
		return fmt.Sprintf("x'%X'", v.text)
	}
	return "NULL"
}
