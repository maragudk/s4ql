package expr_test

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"math/big"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
	"maragu.dev/s4ql/internal/sqlite"
)

// The check of the evaluator against SQLite. An expression is the value of the assignment of
// `update t set k = ...`, which [stmt.Check] reads into the tree that the engine gives the
// evaluator, over a row of the columns of [schema] and the arguments of its parameters. For each,
// s4ql's answer is compared with SQLite's and with that of [reference], which computes it apart from
// the evaluator, with exact arithmetic: s4ql must give SQLite's value, of the same type and bit for
// bit, or an error that the reference justifies, and SQLite's answer for the operation that fails
// must show the quirk that s4ql refuses.

// schema of the row of every expression of the tests: the columns by name, at their positions.
var schema = []struct {
	name string
	typ  s4qlpb.Type
}{
	{"a", s4qlpb.Type_TYPE_INT}, {"b", s4qlpb.Type_TYPE_INTEGER}, {"c", s4qlpb.Type_TYPE_REAL}, {"d", s4qlpb.Type_TYPE_REAL},
	{"s", s4qlpb.Type_TYPE_TEXT}, {"x", s4qlpb.Type_TYPE_BLOB}, {"t", s4qlpb.Type_TYPE_TEXT}, {"y", s4qlpb.Type_TYPE_BLOB},
}

// maxLength is SQLite's default length limit, and its largest.
const maxLength = 1_000_000_000

// prefix of the statement that holds an expression of the tests, as the value of its assignment.
const prefix = "update t set k = "

// exprCase of the tests: the text of an expression, without [prefix], the row, which holds a value
// for each column of [schema], the arguments of its parameters, and the length limit, or 0 for
// [maxLength].
type exprCase struct {
	src   string
	row   []*s4qlpb.Value
	args  []*s4qlpb.Value
	limit int
}

// String of the case, for a message of a failure, with long texts shortened.
func (c exprCase) String() string {
	return fmt.Sprintf("%q with the row %v, the arguments %v, and the limit %v", short(c.src), values(c.row), values(c.args), c.limit)
}

// short of the text s, for a message: s, or its first 200 bytes and its length.
func short(s string) string {
	if len(s) <= 200 {
		return s
	}
	return fmt.Sprintf("%v... (%v bytes)", s[:200], len(s))
}

// values as text, for a message: each as [expr.Value.String] writes it, with long texts and blobs
// shortened.
func values(vs []*s4qlpb.Value) string {
	texts := make([]string, len(vs))
	for i, v := range vs {
		texts[i] = short(valueOf(v).String())
	}
	return "[" + strings.Join(texts, ", ") + "]"
}

// valueOf v, an integer, a real, text, a blob, or NULL, as an [expr.Value].
func valueOf(v *s4qlpb.Value) expr.Value {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return expr.IntegerValue(v.GetInteger())
	case s4qlpb.Value_Real_case:
		return expr.RealValue(v.GetReal())
	case s4qlpb.Value_Text_case:
		return expr.TextValue(v.GetText())
	case s4qlpb.Value_Blob_case:
		return expr.BlobValue(v.GetBlob())
	}
	return expr.Value{}
}

// integer, real, text, blob, and null values of a row or an argument.
func integer(i int64) *s4qlpb.Value {
	v := &s4qlpb.Value{}
	v.SetInteger(i)
	return v
}

func float(f float64) *s4qlpb.Value {
	v := &s4qlpb.Value{}
	v.SetReal(f)
	return v
}

func text(s string) *s4qlpb.Value {
	v := &s4qlpb.Value{}
	v.SetText(s)
	return v
}

func blob(b []byte) *s4qlpb.Value {
	v := &s4qlpb.Value{}
	v.SetBlob(b)
	return v
}

func null() *s4qlpb.Value {
	v := &s4qlpb.Value{}
	v.SetNull(&s4qlpb.Null{})
	return v
}

// errNoExpression is the error of [parse] for a statement that is not an assignment of an
// expression alone.
var errNoExpression = errors.New("not an update of one assignment of an expression")

// parse the text of an expression: its tree, as [stmt.Check] reads it as the value of the
// assignment of `update t set k = ...`, and the text of the statement, which its spans are of.
func parse(src string) (stmt.Expression, string, error) {
	full := prefix + src
	s, err := stmt.Check(full)
	if err != nil {
		return stmt.Expression{}, "", err
	}
	u, ok := s.(*stmt.Update)
	if !ok || len(u.Set) != 1 || u.Where != nil || u.Returning.Present() || u.Subqueries != nil || u.Table.Value != "t" ||
		u.Set[0].Column.Value != "k" || !u.Set[0].Value.Expression.Present() {
		return stmt.Expression{}, "", errNoExpression
	}
	return u.Set[0].Value.Expression, full, nil
}

// errNoSuchColumn is the error of [resolve] for a name that is not a column of [schema].
var errNoSuchColumn = errors.New("no such column")

// resolve the name n to its column of [schema].
func resolve(n stmt.Name) (expr.Column, error) {
	for i, c := range schema {
		if n.Fold() == c.name {
			return expr.Column{Position: i, Type: c.typ}, nil
		}
	}
	return expr.Column{}, errNoSuchColumn
}

// oracle asks SQLite for the value of an expression.
type oracle struct {
	conn *sqlite.Conn
}

// newOracle on a database in memory, which the test closes.
func newOracle(t testing.TB) *oracle {
	t.Helper()
	conn, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	return &oracle{conn: conn}
}

// errTooBig is the error of [oracle.value] where SQLite fails with SQLITE_TOOBIG, `string or blob
// too big`.
var errTooBig = errors.New("string or blob too big")

// value that SQLite gives the expression of the text src over the row of the columns of [schema],
// with args bound to its parameters, in the order of the text, under the length limit: an integer, a
// real, which may be infinite, text, a blob, or NULL; or [errTooBig] where SQLite fails with
// SQLITE_TOOBIG. The columns are those of a common table expression, which have no affinity, so that
// each has the value of the row as it is.
func (o *oracle) value(src string, row, args []*s4qlpb.Value, limit int) (expr.Value, error) {
	names := make([]string, len(schema))
	params := make([]string, len(schema))
	for i, c := range schema {
		names[i], params[i] = c.name, fmt.Sprint("?", i+1)
	}
	// The result column has a name, since SQLite fails with `string or blob too big` for a column
	// whose name, the text of its expression, is longer than the limit.
	q := fmt.Sprintf("with v(%v) as (select %v) select %v as r from v", strings.Join(names, ", "), strings.Join(params, ", "), src)
	if err := o.conn.SetLengthLimit(limit); err != nil {
		return expr.Value{}, err
	}
	defer func() {
		_ = o.conn.SetLengthLimit(maxLength)
	}()
	s, err := o.conn.Prepare(q)
	if err != nil {
		return expr.Value{}, fmt.Errorf("prepare %q: %w", q, err)
	}
	defer func() {
		_ = s.Finalize()
	}()
	for i, v := range append(row[:len(row):len(row)], args...) {
		if err := bind(s, i+1, v); err != nil {
			return expr.Value{}, err
		}
	}
	ok, err := s.Step()
	if se, isSQLite := errors.AsType[*sqlite.Error](err); isSQLite && se.Code == 18 {
		return expr.Value{}, errTooBig
	}
	if err != nil || !ok {
		return expr.Value{}, fmt.Errorf("step %q: %v, %w", q, ok, err)
	}
	switch s.ColumnType(0) {
	case sqlite.TypeInteger:
		return expr.IntegerValue(s.ColumnInt64(0)), nil
	case sqlite.TypeFloat:
		return expr.RealValue(s.ColumnDouble(0)), nil
	case sqlite.TypeText:
		return expr.TextValue(s.ColumnText(0)), nil
	case sqlite.TypeBlob:
		return expr.BlobValue(s.ColumnBlob(0)), nil
	case sqlite.TypeNull:
		return expr.Value{}, nil
	}
	return expr.Value{}, fmt.Errorf("%q gives a value of type %v", q, s.ColumnType(0))
}

// bind v to the parameter i of s.
func bind(s *sqlite.Stmt, i int, v *s4qlpb.Value) error {
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		return s.BindInt64(i, v.GetInteger())
	case s4qlpb.Value_Real_case:
		return s.BindDouble(i, v.GetReal())
	case s4qlpb.Value_Text_case:
		return s.BindText(i, v.GetText())
	case s4qlpb.Value_Blob_case:
		return s.BindBlob(i, v.GetBlob())
	}
	return s.BindNull(i)
}

// refError of [reference]: the kind of the error, as [expr.Error] has it, and the node that fails.
type refError struct {
	kind error
	node stmt.Expression
}

// reference computes the value of an expression apart from the evaluator, by the rules that s4ql
// takes from SQLite: integers with math/big, exactly, which shows an overflow; reals as the real
// nearest to the exact result, which math/big computes, with the sign of a zero as IEEE 754 gives
// it; an integer operand of a real operation converted to the nearest real by math/big; - as 0
// minus its operand, as SQLite computes it; and text by the rules of [reference.text]. It counts
// what each node reaches with tally.
type reference struct {
	row, args []*s4qlpb.Value
	limit     int
	tally     func(kind string)
	// computed holds the value of each node that has one, by its span, which no two nodes of a tree
	// share.
	computed map[stmt.Span]expr.Value
}

// value of the node e: its value, or the error of the first of its nodes that fails, its operands
// before itself and the left before the right.
func (r *reference) value(e stmt.Expression) (expr.Value, *refError) {
	v, err := r.compute(e)
	if err == nil {
		r.computed[e.Span] = v
	}
	return v, err
}

func (r *reference) compute(e stmt.Expression) (expr.Value, *refError) {
	switch e.Kind {
	case stmt.ExprValue:
		switch v := e.Value.(type) {
		case stmt.Integer:
			return expr.IntegerValue(int64(v)), nil
		case stmt.Real:
			return expr.RealValue(float64(v)), nil
		case stmt.Text:
			return expr.TextValue(string(v)), nil
		case stmt.Blob:
			return expr.BlobValue(v), nil
		}
		panic(fmt.Sprintf("a literal %#v", e.Value))
	case stmt.ExprParameter:
		return valueOf(r.args[e.Parameter-1]), nil
	case stmt.ExprColumn:
		c, err := resolve(e.Column)
		if err != nil {
			panic(err)
		}
		return valueOf(r.row[c.Position]), nil
	case stmt.ExprNegate:
		v, err := r.value(e.Operands[0])
		if err != nil {
			return expr.Value{}, err
		}
		if v.Null() {
			r.tally("- of null")
			return v, nil
		}
		if i, ok := v.Integer(); ok {
			n := new(big.Int).Neg(big.NewInt(i))
			if !n.IsInt64() {
				r.tally("- of integer: integer overflow")
				return expr.Value{}, &refError{kind: expr.ErrOverflow, node: e}
			}
			r.tally("- of integer")
			return expr.IntegerValue(n.Int64()), nil
		}
		f, _ := v.Real()
		if f == 0 {
			r.tally("- of real zero")
			return expr.RealValue(0), nil
		}
		r.tally("- of real")
		return expr.RealValue(-f), nil
	case stmt.ExprConcat, stmt.ExprCall:
		return r.text(e)
	}
	l, err := r.value(e.Operands[0])
	if err != nil {
		return expr.Value{}, err
	}
	rv, err := r.value(e.Operands[1])
	if err != nil {
		if l.Null() {
			r.tally("an error on the right of null")
		}
		return expr.Value{}, err
	}
	op := e.Kind.String()
	pair := fmt.Sprintf("%v %v %v", l.Type(), op, rv.Type())
	if l.Null() || rv.Null() {
		r.tally(op + " with null")
		return expr.Value{}, nil
	}
	fail := func(kind error) (expr.Value, *refError) {
		r.tally(fmt.Sprintf("%v: %v", pair, strings.TrimPrefix(kind.Error(), "s4ql: ")))
		return expr.Value{}, &refError{kind: kind, node: e}
	}
	x, xInt := l.Integer()
	y, yInt := rv.Integer()
	if xInt && yInt {
		a, b := big.NewInt(x), big.NewInt(y)
		c := new(big.Int)
		switch e.Kind {
		case stmt.ExprAdd:
			c.Add(a, b)
		case stmt.ExprSubtract:
			c.Sub(a, b)
		case stmt.ExprMultiply:
			c.Mul(a, b)
		case stmt.ExprDivide:
			if b.Sign() == 0 {
				return fail(expr.ErrDivisionByZero)
			}
			// Quo truncates toward zero, as SQLite's integer division does.
			c.Quo(a, b)
		}
		if !c.IsInt64() {
			return fail(expr.ErrOverflow)
		}
		r.tally(pair)
		switch {
		case c.Int64() == math.MaxInt64:
			r.tally(pair + ": exactly the largest integer")
		case c.Int64() == math.MinInt64:
			r.tally(pair + ": exactly the smallest integer")
		}
		if e.Kind == stmt.ExprDivide && x == math.MinInt64 && y < -1 {
			r.tally("the smallest integer / a negative integer other than -1")
		}
		return expr.IntegerValue(c.Int64()), nil
	}
	fx, fy := r.float(l, "left"), r.float(rv, "right")
	if e.Kind == stmt.ExprDivide && fy == 0 {
		return fail(expr.ErrDivisionByZero)
	}
	a, b := new(big.Float).SetFloat64(fx), new(big.Float).SetFloat64(fy)
	// Exact for +, -, and *, whose results never need more bits, and enough that rounding the
	// quotient again to a float64 gives the nearest one.
	c := new(big.Float).SetPrec(2200)
	switch e.Kind {
	case stmt.ExprAdd:
		c.Add(a, b)
	case stmt.ExprSubtract:
		c.Sub(a, b)
	case stmt.ExprMultiply:
		c.Mul(a, b)
	case stmt.ExprDivide:
		c.Quo(a, b)
	}
	f, _ := c.Float64()
	if math.IsInf(f, 0) {
		return fail(expr.ErrNotFinite)
	}
	if c.Sign() == 0 {
		// An exact zero: of a sum, -0 only for -0 + -0 and -0 - 0, and of a product or a quotient,
		// negative if one operand is negative.
		negative := math.Signbit(fx) != math.Signbit(fy)
		switch e.Kind {
		case stmt.ExprAdd:
			negative = math.Signbit(fx) && math.Signbit(fy)
		case stmt.ExprSubtract:
			negative = math.Signbit(fx) && !math.Signbit(fy)
		}
		f = math.Copysign(0, map[bool]float64{true: -1, false: 1}[negative])
	}
	r.tally(pair)
	if f != 0 && math.Abs(f) < 0x1p-1022 {
		r.tally(pair + ": subnormal, not zero")
	}
	r.fused(e, f)
	return expr.RealValue(f), nil
}

// text of the concatenation or the call e, by the rules that s4ql takes from SQLite: || joins the
// bytes of its operands, and is NULL if one is NULL; lower changes the bytes A to Z to a to z, as
// [unicode.ToLower] changes them, and leaves every other byte, and NULL, as it is; hex writes each
// byte of a text or a blob in two digits, as [hex.EncodeToString] does, in upper case, and is the
// empty text for NULL. SQLite refuses to make a text longer than the limit, and asks for one byte
// more than lower and hex give, for the NUL at its end. Both operands of || are computed, the left
// first, before it joins them.
func (r *reference) text(e stmt.Expression) (expr.Value, *refError) {
	l, err := r.value(e.Operands[0])
	if err != nil {
		return expr.Value{}, err
	}
	bytesOf := func(v expr.Value) []byte {
		if t, ok := v.Text(); ok {
			return []byte(t)
		}
		if b, ok := v.Blob(); ok {
			return b
		}
		if v.Null() {
			return nil
		}
		panic(fmt.Sprintf("%v in a text expression", v))
	}
	name := e.Function.String()
	if e.Kind == stmt.ExprConcat {
		name = "||"
	}
	// size is the length of what SQLite allocates for the result, which it refuses where it is larger
	// than the limit.
	fits := func(size int) *refError {
		switch {
		case size > r.limit:
			r.tally(name + ": text longer than the length limit")
			return &refError{kind: expr.ErrTooLong, node: e}
		case size == r.limit:
			r.tally(name + ": at the limit")
		}
		return nil
	}
	switch e.Function {
	case stmt.FuncLower:
		if l.Null() {
			r.tally("lower of null")
			return expr.Value{}, nil
		}
		b := bytesOf(l)
		if err := fits(len(b) + 1); err != nil {
			return expr.Value{}, err
		}
		changed, ascii := false, true
		for i, c := range b {
			if c >= utf8.RuneSelf {
				ascii = false
				continue
			}
			if lc := byte(unicode.ToLower(rune(c))); lc != c {
				b[i], changed = lc, true
			}
		}
		r.tally(fmt.Sprintf("lower of text, ASCII %v, changed %v", ascii, changed))
		if t, _ := l.Text(); strings.ContainsFunc(t, func(c rune) bool { return c >= utf8.RuneSelf && unicode.ToLower(c) != c }) {
			r.tally("lower of a letter that is not ASCII and has a lower case in Unicode")
		}
		return expr.TextValue(string(b)), nil
	case stmt.FuncHex:
		kind := "text"
		switch {
		case l.Null():
			kind = "null"
		case l.Type() == expr.TypeBlob:
			kind = "a blob"
		}
		b := bytesOf(l)
		if len(b) == 0 && !l.Null() {
			kind += " that is empty"
		}
		r.tally("hex of " + kind)
		if err := fits(2*len(b) + 1); err != nil {
			return expr.Value{}, err
		}
		return expr.TextValue(strings.ToUpper(hex.EncodeToString(b))), nil
	}
	rv, err := r.value(e.Operands[1])
	if err != nil {
		if l.Null() {
			r.tally("||: an error on the right of null")
		}
		return expr.Value{}, err
	}
	if l.Null() || rv.Null() {
		r.tally("|| with null")
		return expr.Value{}, nil
	}
	a, b := bytesOf(l), bytesOf(rv)
	if err := fits(len(a) + len(b)); err != nil {
		return expr.Value{}, err
	}
	switch {
	case len(a) == 0 && len(b) == 0:
		r.tally("|| of two empty texts")
	case len(a) == 0 || len(b) == 0:
		r.tally("|| with an empty text")
	default:
		r.tally("|| of two texts")
	}
	return expr.TextValue(string(append(a, b...))), nil
}

// float of v, an integer or a real, for an operation of reals: the real itself, or the real nearest
// to the integer, the even one of two that are as near, which side counts as a conversion.
func (r *reference) float(v expr.Value, side string) float64 {
	if f, ok := v.Real(); ok {
		return f
	}
	i, _ := v.Integer()
	f, accuracy := new(big.Float).SetInt64(i).Float64()
	r.tally("integer converted to real on the " + side)
	if accuracy != big.Exact {
		r.tally("integer converted to real inexactly")
	}
	return f
}

// fused counts, for a sum or a difference e with the real result f of which an operand is a
// product of reals, whether one instruction that fused the two would give another result, which
// SQLite never gives.
func (r *reference) fused(e stmt.Expression, f float64) {
	if e.Kind != stmt.ExprAdd && e.Kind != stmt.ExprSubtract {
		return
	}
	for k, m := range e.Operands {
		other := e.Operands[1-k]
		if m.Kind != stmt.ExprMultiply {
			continue
		}
		x, okX := r.computed[m.Operands[0].Span].Real()
		y, okY := r.computed[m.Operands[1].Span].Real()
		z, okZ := r.computed[other.Span].Real()
		if !okX || !okY || !okZ {
			continue
		}
		switch {
		case e.Kind == stmt.ExprSubtract && k == 0:
			z = -z
		case e.Kind == stmt.ExprSubtract:
			x = -x
		}
		if g := math.FMA(x, y, z); math.Float64bits(g) != math.Float64bits(f) && !math.IsInf(g, 0) {
			r.tally("a fused multiply and add would differ")
		}
	}
}

// parametersOf e: the numbers of its parameters, in the order of the text.
func parametersOf(e stmt.Expression) []int {
	if e.Kind == stmt.ExprParameter {
		return []int{e.Parameter}
	}
	var ps []int
	for _, o := range e.Operands {
		ps = append(ps, parametersOf(o)...)
	}
	return ps
}

// argumentsOf the parameters of e, which are numbered in the statement, from args, the arguments of
// the statement, in the order of the text of e, as SQLite numbers the parameters of e alone.
func argumentsOf(e stmt.Expression, args []*s4qlpb.Value) []*s4qlpb.Value {
	var as []*s4qlpb.Value
	for _, n := range parametersOf(e) {
		as = append(as, args[n-1])
	}
	return as
}

// refusalOf the case c with the tree e: the refusal that s4ql must give, at [expr.Check] for the
// first column in the order of the text of a type that its place does not take, or else at
// [expr.Program.Bind] for the first parameter whose argument is of such a type; and whether it comes
// at the check. Arithmetic takes integers and reals, || and lower take text, and hex takes text and
// blobs, and NULL from an argument meets each; a column alone takes every type.
func refusalOf(e stmt.Expression, c exprCase) (*stmt.NotSupportedError, bool) {
	var column, argument *stmt.NotSupportedError
	var walk func(e stmt.Expression, place string)
	walk = func(e stmt.Expression, place string) {
		takes := map[string][]string{
			"in arithmetic":      {"INT", "INTEGER", "REAL", "an integer", "a real"},
			"in a concatenation": {"TEXT", "text"},
			"in a call of lower": {"TEXT", "text"},
			"in a call of hex":   {"TEXT", "BLOB", "text", "a blob"},
		}[place]
		switch e.Kind {
		case stmt.ExprColumn:
			col, _ := resolve(e.Column)
			t := strings.TrimPrefix(col.Type.String(), "TYPE_")
			if column == nil && place != "" && !slices.Contains(takes, t) {
				column = &stmt.NotSupportedError{What: fmt.Sprintf("column of type %v %v", t, place), Offset: e.Span.Start}
			}
		case stmt.ExprParameter:
			var which string
			switch c.args[e.Parameter-1].WhichKind() {
			case s4qlpb.Value_Integer_case:
				which = "an integer"
			case s4qlpb.Value_Real_case:
				which = "a real"
			case s4qlpb.Value_Text_case:
				which = "text"
			case s4qlpb.Value_Blob_case:
				which = "a blob"
			}
			if place == "" {
				place, takes = "in arithmetic", []string{"an integer", "a real"}
			}
			if argument == nil && which != "" && !slices.Contains(takes, which) {
				argument = &stmt.NotSupportedError{What: fmt.Sprintf("argument %v %v, which is %v", e.Parameter, place, which), Offset: e.Span.Start}
			}
		}
		place = "in arithmetic"
		switch e.Kind {
		case stmt.ExprConcat:
			place = "in a concatenation"
		case stmt.ExprCall:
			place = "in a call of " + e.Function.String()
		}
		for _, o := range e.Operands {
			walk(o, place)
		}
	}
	walk(e, "")
	if column != nil {
		return column, true
	}
	return argument, false
}

// checker of cases: the oracle, and the tally of what the cases reach.
type checker struct {
	o     *oracle
	tally map[string]int
}

// check the case c, and give what fails, or "" if nothing does. The tree of c is that of [parse],
// which must take it, and its columns must be those of [schema], and its arguments as many as its
// parameters; for a case that is none of these, check gives "" and counts it as "not a case", which
// only the fuzz test gives.
func (k *checker) check(c exprCase) string {
	e, full, err := parse(c.src)
	if err != nil {
		k.tally["not a case: "+err.Error()]++
		return ""
	}
	for _, n := range parametersOf(e) {
		if n > len(c.args) {
			k.tally["not a case: too few arguments"]++
			return ""
		}
	}
	unknown := false
	walkColumns(e, func(n stmt.Name) {
		if _, err := resolve(n); err != nil {
			unknown = true
		}
	})
	if unknown {
		x, err := expr.Check(e, resolve)
		if !errors.Is(err, errNoSuchColumn) {
			return fmt.Sprintf("%v: the check gives %v and %v, and not the error of resolve", c, x, err)
		}
		k.tally["not a case: no such column"]++
		return ""
	}

	limit := c.limit
	if limit == 0 {
		limit = maxLength
	}
	for _, v := range slices.Concat(c.row, c.args) {
		if len(v.GetText()) > limit || len(v.GetBlob()) > limit {
			// SQLite refuses to bind it.
			k.tally["not a case: a value longer than the limit"]++
			return ""
		}
	}
	refusal, atCheck := refusalOf(e, c)
	x, err := expr.Check(e, resolve)
	if atCheck {
		var ns *stmt.NotSupportedError
		if !errors.As(err, &ns) || *ns != *refusal || !errors.Is(err, stmt.ErrNotSupported) {
			return fmt.Sprintf("%v: the check gives %v, and not %v", c, err, refusal)
		}
		k.tally["case: refused, "+refusal.What]++
		return ""
	}
	if err != nil {
		return fmt.Sprintf("%v: the check gives %v", c, err)
	}
	b, err := x.Bind(c.args, limit)
	if refusal != nil {
		var ns *stmt.NotSupportedError
		if !errors.As(err, &ns) || *ns != *refusal || !errors.Is(err, stmt.ErrNotSupported) {
			return fmt.Sprintf("%v: the binding gives %v, and not %v", c, err, refusal)
		}
		k.tally["case: refused, argument "+refusal.What[strings.Index(refusal.What, " in ")+1:]]++
		return ""
	}
	if long := longLiteral(e, limit); long != nil {
		// SQLite fails for a text literal longer than the limit, and reads a blob literal as NULL.
		var ee *expr.Error
		if !errors.As(err, &ee) || ee.Kind != expr.ErrTooLong || ee.Op != stmt.ExprValue || ee.Span != long.Span {
			return fmt.Sprintf("%v: the binding gives %v, and not %v at the literal at %v", c, err, expr.ErrTooLong, long.Span.Start)
		}
		quirk, err := k.o.value(full[long.Span.Start:long.Span.End], nil, nil, limit)
		_, isText := long.Value.(stmt.Text)
		if isText && err != errTooBig || !isText && (err != nil || !quirk.Null()) {
			return fmt.Sprintf("%v: SQLite gives %v and %v for the literal at %v", c, short(quirk.String()), err, long.Span.Start)
		}
		k.tally[fmt.Sprintf("case: literal longer than the limit, text %v", isText)]++
		return ""
	}
	if err != nil {
		return fmt.Sprintf("%v: the binding gives %v", c, err)
	}
	if typ, ok := x.Type(); ok != (len(parametersOf(e)) == 0) || ok && typ != b.Type() {
		return fmt.Sprintf("%v: the program has the type %v, %v, and the bound one %v", c, typ, ok, b.Type())
	}
	got, gotErr := b.Eval(c.row)

	ref := &reference{row: c.row, args: c.args, limit: limit, tally: func(kind string) { k.tally[kind]++ }, computed: map[stmt.Span]expr.Value{}}
	want, wantErr := ref.value(e)
	if wantErr != nil {
		var ee *expr.Error
		node := wantErr.node
		if !errors.As(gotErr, &ee) || ee.Kind != wantErr.kind || ee.Span != node.Span || ee.Op != node.Kind || ee.Function != node.Function ||
			!errors.Is(gotErr, expr.ErrEvaluation) || !errors.Is(gotErr, wantErr.kind) {
			return fmt.Sprintf("%v: s4ql gives %v and %v, where %v is due at %q", c, short(got.String()), gotErr, wantErr.kind, short(full[node.Span.Start:node.Span.End]))
		}
		// SQLite's answer for the operation that fails shows the quirk that s4ql refuses: a real
		// from integer operands for an overflow, NULL from operands that are not NULL for a
		// division by zero, and an infinity for a real result that is not finite. For a text longer
		// than the limit, SQLite fails, for the operation and for the whole expression, which
		// computes every operand of every operation.
		quirk, err := k.o.value(full[node.Span.Start:node.Span.End], c.row, argumentsOf(node, c.args), limit)
		if wantErr.kind == expr.ErrTooLong {
			whole, wholeErr := k.o.value(full[e.Span.Start:e.Span.End], c.row, argumentsOf(e, c.args), limit)
			if err != errTooBig || wholeErr != errTooBig {
				return fmt.Sprintf("%v: SQLite gives %v and %v for %q, and %v and %v for the whole, which fails with %v in s4ql", c,
					short(quirk.String()), err, short(full[node.Span.Start:node.Span.End]), short(whole.String()), wholeErr, wantErr.kind)
			}
		} else if err != nil {
			return fmt.Sprintf("%v: %v", c, err)
		}
		f, isReal := quirk.Real()
		var shown bool
		switch wantErr.kind {
		case expr.ErrOverflow:
			shown = isReal
		case expr.ErrDivisionByZero:
			shown = quirk.Null()
		case expr.ErrNotFinite:
			shown = isReal && math.IsInf(f, 0)
		case expr.ErrTooLong:
			shown = true
		}
		if !shown {
			return fmt.Sprintf("%v: SQLite gives %v for %q, which fails with %v in s4ql", c, quirk, full[node.Span.Start:node.Span.End], wantErr.kind)
		}
		k.tally[fmt.Sprintf("case: %v", strings.TrimPrefix(wantErr.kind.Error(), "s4ql: "))]++
		return ""
	}
	sqliteValue, err := k.o.value(full[e.Span.Start:e.Span.End], c.row, argumentsOf(e, c.args), limit)
	if err != nil {
		return fmt.Sprintf("%v: %v", c, err)
	}
	if sqliteValue != want {
		return fmt.Sprintf("%v: SQLite gives %v, and the reference %v", c, short(sqliteValue.String()), short(want.String()))
	}
	if gotErr != nil || got != want {
		return fmt.Sprintf("%v: s4ql gives %v and %v, and SQLite %v", c, short(got.String()), gotErr, short(want.String()))
	}
	if !got.Null() && got.Type() != b.Type() || b.Type() == expr.TypeNull && !got.Null() {
		return fmt.Sprintf("%v: s4ql gives %v of the type %v", c, got, b.Type())
	}
	kind := "case: " + got.Type().String()
	if f, ok := got.Real(); ok && f == 0 {
		kind += map[bool]string{true: " -0.0", false: " 0.0"}[math.Signbit(f)]
	}
	k.tally[kind]++
	if e.Kind == stmt.ExprColumn {
		k.tally["case: column alone"]++
	}
	if depth(e) > 900 {
		k.tally["case: deeper than 900"]++
	}
	return ""
}

// longLiteral of e: the first literal in the order of the text that is a text or a blob longer than
// limit, or nil.
func longLiteral(e stmt.Expression, limit int) *stmt.Expression {
	switch v := e.Value.(type) {
	case stmt.Text:
		if len(v) > limit {
			return &e
		}
	case stmt.Blob:
		if len(v) > limit {
			return &e
		}
	}
	for _, o := range e.Operands {
		if l := longLiteral(o, limit); l != nil {
			return l
		}
	}
	return nil
}

// depth of e, as the pass counts it: 1 for a leaf, and one more than its deepest operand for an
// operator or a call.
func depth(e stmt.Expression) int {
	d := 0
	for _, o := range e.Operands {
		d = max(d, depth(o))
	}
	return d + 1
}

// walkColumns of e, the names of its columns in the order of the text.
func walkColumns(e stmt.Expression, f func(stmt.Name)) {
	if e.Kind == stmt.ExprColumn {
		f(e.Column)
	}
	for _, o := range e.Operands {
		walkColumns(o, f)
	}
}

// The number of seeds of [TestDifferential], and of cases of each.
const (
	differentialSeeds = 8
	differentialCases = 3600
)

// TestDifferential checks the evaluator against SQLite and [reference] with the cases of
// [generator], each seed a run of its own, and checks that the runs reach the kinds of
// testdata/reached/TestDifferential.kinds, as [stmttest.Reached] says: each operator with each pair
// of the types of its operands, each error of each, NULL, both sides of the conversion of an integer
// to a real, and a fused instruction that would give another result.
func TestDifferential(t *testing.T) {
	k := &checker{o: newOracle(t)}
	runs := map[string]map[string]int{}
	var names []string
	for seed := uint64(1); seed <= differentialSeeds; seed++ {
		name := fmt.Sprint("seed ", seed)
		names = append(names, name)
		k.tally = map[string]int{}
		g := newGenerator(seed)
		failures := 0
		for range differentialCases {
			if failure := k.check(g.next()); failure != "" {
				t.Error(failure)
				if failures++; failures == 10 {
					t.FailNow()
				}
			}
		}
		for kind, n := range k.tally {
			if strings.HasPrefix(kind, "not a case") {
				t.Errorf("%v gives %v cases that are %v", name, n, kind)
			}
		}
		runs[name] = k.tally
	}
	path := filepath.Join("testdata", "reached", t.Name())
	reached := stmttest.Reached{Kinds: path + ".kinds", Counts: path + ".tsv",
		Doc: "What each seed of TestDifferential of the evaluator reaches."}
	failures, err := reached.Check(names, runs)
	is.NotError(t, err)
	for _, failure := range failures {
		t.Error(failure)
	}
}

// FuzzExpr checks the evaluator against SQLite and [reference], as [TestDifferential] does, for the
// text of an expression, the values of the columns of the row and the arguments, which [decode]
// reads, and the length limit, which is 0 for [maxLength], and otherwise from 30, SQLite's least,
// to 4,000 bytes. A text that is not an expression over [schema] alone is no case, and neither is a
// value longer than the limit, nor a text with more than eight calls of hex. The seeds are cases of
// [generator], but its deep trees, and the inputs in testdata/fuzz/FuzzExpr.
func FuzzExpr(f *testing.F) {
	g := newGenerator(1)
	for range 300 {
		c := g.next()
		if len(c.src) > 2000 {
			// The deep trees, which are slow to mutate.
			continue
		}
		limit := uint16(0)
		if c.limit > 0 {
			limit = uint16(1 + c.limit - 30)
		}
		f.Add(c.src, encode(c.row), encode(c.args), limit)
	}
	f.Add("'m_' || lower(hex(x))", encode([]*s4qlpb.Value{null(), null(), null(), null(), null(), blob([]byte("\x00\x01\xfeZ"))}), []byte(nil), uint16(0))
	f.Add("hex(? || s)", []byte(nil), encode([]*s4qlpb.Value{text("aÀ")}), uint16(0))
	f.Fuzz(func(t *testing.T, src string, row, args []byte, limit uint16) {
		c := exprCase{src: src, row: make([]*s4qlpb.Value, len(schema)), args: decode(args)}
		values := decode(row)
		for i := range schema {
			var v *s4qlpb.Value
			if i < len(values) {
				v = values[i]
			}
			c.row[i] = columnOf(i, v)
		}
		if e, _, err := parse(src); err == nil {
			for len(c.args) < len(parametersOf(e)) {
				c.args = append(c.args, integer(1))
			}
		}
		if limit > 0 {
			c.limit = 30 + (int(limit)-1)%3971
		}
		if strings.Count(strings.ToLower(src), "hex") > 8 {
			// hex doubles the length of its argument, so that nine of them make gigabytes of a
			// short text under the default limit, three times over.
			return
		}
		k := &checker{o: newOracle(t), tally: map[string]int{}}
		if failure := k.check(c); failure != "" {
			t.Error(failure)
		}
	})
}

// encode the values vs for [FuzzExpr]: for each, a byte of its kind, and eight bytes of an integer
// or a real, or two of the length of a text or a blob and its bytes.
func encode(vs []*s4qlpb.Value) []byte {
	var b []byte
	for _, v := range vs {
		var kind byte
		var bits uint64
		var bytes []byte
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			kind, bits = 1, uint64(v.GetInteger())
		case s4qlpb.Value_Real_case:
			kind, bits = 2, math.Float64bits(v.GetReal())
		case s4qlpb.Value_Text_case:
			kind, bytes = 3, []byte(v.GetText())
		case s4qlpb.Value_Blob_case:
			kind, bytes = 4, v.GetBlob()
		}
		b = append(b, kind)
		if kind == 3 || kind == 4 {
			b = append(binary.LittleEndian.AppendUint16(b, uint16(len(bytes))), bytes...)
			continue
		}
		b = binary.LittleEndian.AppendUint64(b, bits)
	}
	return b
}

// decode the values of [encode]: NULL, an integer, a real, a text, or a blob, by the first byte
// modulo 5, with a real that is not finite as 0.0, and a text as valid UTF-8 without the character
// NUL, which no row or argument holds. The bytes of a text or a blob end where b ends.
func decode(b []byte) []*s4qlpb.Value {
	var vs []*s4qlpb.Value
	for len(b) > 0 {
		kind := b[0] % 5
		b = b[1:]
		if kind == 3 || kind == 4 {
			if len(b) < 2 {
				break
			}
			n := min(int(binary.LittleEndian.Uint16(b)), len(b)-2)
			bytes := b[2 : 2+n]
			b = b[2+n:]
			if kind == 3 {
				vs = append(vs, text(strings.ReplaceAll(strings.ToValidUTF8(string(bytes), "�"), "\x00", "")))
			} else {
				vs = append(vs, blob(bytes))
			}
			continue
		}
		if len(b) < 8 {
			break
		}
		bits := binary.LittleEndian.Uint64(b)
		b = b[8:]
		switch kind {
		case 0:
			vs = append(vs, null())
		case 1:
			vs = append(vs, integer(int64(bits)))
		default:
			f := math.Float64frombits(bits)
			if math.IsInf(f, 0) || math.IsNaN(f) {
				f = 0
			}
			vs = append(vs, float(f))
		}
	}
	return vs
}

// columnOf the value v, or nil, for the column at the position i of [schema], which holds only NULL
// and values of its type: v if it is NULL or of the type; for a column of a number, an integer or a
// real of the bits of v otherwise, or 0 for nil; and for a column of type TEXT or BLOB, the text '4'
// or the blob x'35' otherwise.
func columnOf(i int, v *s4qlpb.Value) *s4qlpb.Value {
	switch typ := schema[i].typ; {
	case v.HasNull(), typ == s4qlpb.Type_TYPE_TEXT && v.HasText(), typ == s4qlpb.Type_TYPE_BLOB && v.HasBlob():
		return v
	case typ == s4qlpb.Type_TYPE_TEXT:
		return text("4")
	case typ == s4qlpb.Type_TYPE_BLOB:
		return blob([]byte("5"))
	}
	var bits uint64
	switch v.WhichKind() {
	case s4qlpb.Value_Integer_case:
		bits = uint64(v.GetInteger())
	case s4qlpb.Value_Real_case:
		bits = math.Float64bits(v.GetReal())
	}
	if schema[i].typ == s4qlpb.Type_TYPE_REAL {
		f := math.Float64frombits(bits)
		if math.IsInf(f, 0) || math.IsNaN(f) {
			f = 0
		}
		return float(f)
	}
	return integer(int64(bits))
}
