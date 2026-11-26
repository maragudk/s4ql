package expr

import "maragu.dev/s4ql/internal/sql/stmt"

// IntegerValue, RealValue, and TextValue make the values that tests compare with what [Bound.Eval]
// gives.
var (
	IntegerValue = integerValue
	RealValue    = realValue
	TextValue    = textValue
)

// BlobValue of the bytes b, which it copies.
func BlobValue(b []byte) Value {
	return blobValue(string(b))
}

// Arithmetic of the operator op on l and r, as a node of that kind computes it, for an operator
// that no [Program] holds, since [Check] refuses it.
func Arithmetic(op stmt.ExpressionKind, l, r Value) (Value, error) {
	return arithmetic(op, l, r, &node{kind: op})
}
