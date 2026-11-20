package stmttest

import (
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// Sequence of random statements of slices 1, 2, 4, and 5 that meet at a few tables and columns, each
// named in several spellings that SQLite takes for one name and in some that it takes for others,
// with values from a small set so that keys repeat, and with statements that are not supported and
// texts that SQLite refuses mixed in.
//
// Each name of a table has a shape of the sequence's own: from one to [sequenceColumns] columns,
// each with a name and one of the five types, of which two often have the same type, and the primary
// key at any position. Nine create tables of the name in ten declare that shape, its names in any of
// their spellings, and one of these in ten gives one column any type; the tenth has columns at
// random, whose names can repeat, which SQLite refuses. An insert into a table of the name names the
// columns of the shape in a list in any order, or one time in three without a list, in the order of
// the shape, with a value of the type of each column. Now and then it leaves a column out of its
// list, names a column that the shape may not have, has a value more or less, or has a value of any
// type, which is mostly another than its column, for one column or for two; so some inserts have two
// faults. A select names * or columns of the shape in any order, also one twice, and now and then a
// name that the shape may not have. The small sets of values hold keys that SQLite takes for one,
// such as 0.0 and -0.0, so that every type meets a key that exists.
//
// One value in three of an insert is a plain ? with the value as its argument, and some calls, as
// [Sequence.Next] says, have arguments around those that s4ql supports: none for a ?, one for a
// statement without one, two for a ?, one that no column holds, one with a name, or a value of
// another Go type that s4ql supports.
type Sequence struct {
	r      Rand
	g      *Generator
	tables []string
	// columns are the spellings of the names of columns, folds the folds of their names, in the order
	// that the spellings first have them, and spelled the spellings of each fold.
	columns []string
	folds   []string
	spelled map[string][]string
	// shapes of the tables by their names, folded as SQLite compares names: the columns of each in
	// the order of the table, with the fold of each name as its Name.Value.
	shapes map[string][]stmt.Column
}

// sequenceColumns is the most columns of a table of a [Sequence].
const sequenceColumns = 3

// spellings of the name raw: itself, and other spellings in and without quotes, of which those that
// change only ASCII letters name the same name, and an upper case that changes other letters names
// another.
func spellings(r Rand, raw string) []string {
	if raw[0] == '"' {
		v := strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
		return []string{raw, Quote(swapCase(v)), Quote(strings.ToUpper(v)), Quote(randomCase(r, v))}
	}
	return []string{raw, strings.ToUpper(raw), randomCase(r, raw), Quote(raw), Quote(randomCase(r, raw))}
}

// swapCase of the ASCII letters of s.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// randomCase of the ASCII letters of s.
func randomCase(r Rand, s string) string {
	b := []byte(s)
	for i, c := range b {
		if r.IntN(2) == 0 {
			continue
		}
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
		}
	}
	return string(b)
}

// commonPools are the names that SQLite reads as a name of a table in every position of a table, and
// those that it reads as a name of a column in every position of a column.
var commonPools = sync.OnceValues(func() ([2][]string, error) {
	pools, err := NamePools()
	if err != nil {
		return [2][]string{}, err
	}
	return [2][]string{
		common(pools, CreateTable, InsertTable, SelectTable, DropTable),
		common(pools, CreateColumn, InsertColumn, SelectColumn),
	}, nil
})

// common names of the pools at the positions.
func common(pools [][]string, positions ...Position) []string {
	var names []string
	for _, n := range pools[positions[0]] {
		in := true
		for _, p := range positions[1:] {
			in = in && slices.Contains(pools[p], n)
		}
		if in {
			names = append(names, n)
		}
	}
	return names
}

// NewSequence with the random numbers of r. It fails if [NamePools] fails.
func NewSequence(r Rand) (*Sequence, error) {
	common, err := commonPools()
	if err != nil {
		return nil, err
	}
	q := &Sequence{r: r, spelled: map[string][]string{}, shapes: map[string][]stmt.Column{}}
	tableNames, columnNames := common[0], common[1]
	for range 1 + r.IntN(2) {
		q.tables = append(q.tables, spellings(r, tableNames[r.IntN(len(tableNames))])...)
	}
	// So many names of columns that a table of the most columns lacks one of them.
	for len(q.folds) <= sequenceColumns {
		for _, raw := range spellings(r, columnNames[r.IntN(len(columnNames))]) {
			q.columns = append(q.columns, raw)
			fold := nameOf(raw, 0).Fold()
			if q.spelled[fold] == nil {
				q.folds = append(q.folds, fold)
			}
			q.spelled[fold] = append(q.spelled[fold], raw)
		}
	}
	pools := make([][]string, len(PositionTexts))
	for _, p := range []Position{CreateTable, InsertTable, SelectTable, DropTable} {
		pools[p] = q.tables
	}
	pools[CreateColumn], pools[InsertColumn], pools[SelectColumn] = q.columns, q.columns, q.columns
	q.g = NewGenerator(r, pools)
	q.g.Columns, q.g.Create, q.g.Insert, q.g.Select = sequenceColumns, q.create, q.insert, q.selectList
	q.g.Parameters = 3
	return q, nil
}

// shapeOf the tables of the name of table: the sequence's own, chosen at random the first time.
func (q *Sequence) shapeOf(table stmt.Name) []stmt.Column {
	name := table.Fold()
	shape, ok := q.shapes[name]
	if ok {
		return shape
	}
	r := q.r
	folds := slices.Clone(q.folds)
	n := 1 + r.IntN(sequenceColumns)
	key := r.IntN(n)
	for i := range n {
		j := i + r.IntN(len(folds)-i)
		folds[i], folds[j] = folds[j], folds[i]
		typ := Types[r.IntN(len(Types))]
		if i > 0 && r.IntN(2) == 0 {
			// Two columns of one type, so that a value in the place of the other has the type of its
			// column.
			typ = shape[r.IntN(i)].Type
		}
		shape = append(shape, stmt.Column{Name: stmt.Name{Value: folds[i]}, Type: typ, PrimaryKey: i == key})
	}
	q.shapes[name] = shape
	return shape
}

// spell the name of a column of the fold in one of its spellings.
func (q *Sequence) spell(fold string) string {
	raws := q.spelled[fold]
	return raws[q.r.IntN(len(raws))]
}

// anyType at random.
func (q *Sequence) anyType() stmt.Type {
	return Types[q.r.IntN(len(Types))]
}

// create gives the columns of a create table of table, as [Generator.Create] does: those of its
// shape, in any of their spellings, and one time in ten with one column of any type, or one time in
// ten columns at random, whose names can repeat.
func (q *Sequence) create(table stmt.Name) []stmt.Column {
	r := q.r
	shape := q.shapeOf(table)
	if r.IntN(10) == 0 {
		n := 1 + r.IntN(sequenceColumns)
		key := r.IntN(n)
		columns := make([]stmt.Column, n)
		for i := range columns {
			columns[i] = stmt.Column{Name: stmt.Name{Raw: q.columns[r.IntN(len(q.columns))]}, Type: q.anyType(), PrimaryKey: i == key}
		}
		return columns
	}
	retyped := -1
	if r.IntN(10) == 0 {
		retyped = r.IntN(len(shape))
	}
	var columns []stmt.Column
	for i, c := range shape {
		c.Name = stmt.Name{Raw: q.spell(c.Name.Value)}
		if i == retyped {
			c.Type = q.anyType()
		}
		columns = append(columns, c)
	}
	return columns
}

// insert gives the list of columns and the values of an insert into table, as [Generator.Insert]
// does: the columns of its shape, in a list in any order or without a list, with a value of the type
// of each. One list in eight leaves a column out, one in twelve has a name in place of one of its
// own that is no other of the list, one insert in sixteen has a value more and one a value less,
// and one in ten has a value of any type, and one in three of these a second.
func (q *Sequence) insert(table stmt.Name) ([]string, []stmt.Value) {
	r := q.r
	shape := q.shapeOf(table)
	order := make([]int, len(shape))
	for i := range order {
		order[i] = i
	}
	withList := r.IntN(3) != 0
	if withList {
		if r.IntN(3) != 0 {
			for i := len(order) - 1; i > 0; i-- {
				j := r.IntN(i + 1)
				order[i], order[j] = order[j], order[i]
			}
		}
		if len(order) > 1 && r.IntN(8) == 0 {
			at := r.IntN(len(order))
			order = slices.Delete(order, at, at+1)
		}
	}
	var list []string
	var values []stmt.Value
	for _, position := range order {
		c := shape[position]
		if withList {
			list = append(list, q.spell(c.Name.Value))
		}
		values = append(values, q.valueOf(c.Type))
	}
	if withList && r.IntN(12) == 0 {
		seen := map[string]bool{}
		for _, raw := range list {
			seen[nameOf(raw, 0).Fold()] = true
		}
		at := r.IntN(len(list))
		delete(seen, nameOf(list[at], 0).Fold())
		for range 100 {
			if raw := q.columns[r.IntN(len(q.columns))]; !seen[nameOf(raw, 0).Fold()] {
				list[at] = raw
				break
			}
		}
	}
	if r.IntN(10) == 0 {
		values[r.IntN(len(values))] = q.valueOf(q.anyType())
		if r.IntN(3) == 0 {
			values[r.IntN(len(values))] = q.valueOf(q.anyType())
		}
	}
	switch r.IntN(16) {
	case 0:
		values = append(values, q.valueOf(q.anyType()))
	case 1:
		if len(values) > 1 {
			values = values[:len(values)-1]
		}
	}
	return list, values
}

// selectList of a select of table, as [Generator.Select] gives it: * one time in two, and otherwise
// from one to [sequenceColumns]+1 names of the columns of its shape, of which one in four after the
// first is an earlier one, and one in twelve any name of a column of the sequence.
func (q *Sequence) selectList(table stmt.Name) []string {
	r := q.r
	if r.IntN(2) == 0 {
		return nil
	}
	shape := q.shapeOf(table)
	var names []string
	for i := range 1 + r.IntN(sequenceColumns+1) {
		switch {
		case i > 0 && r.IntN(4) == 0:
			names = append(names, names[r.IntN(i)])
		case r.IntN(12) == 0:
			names = append(names, q.columns[r.IntN(len(q.columns))])
		default:
			names = append(names, q.spell(shape[r.IntN(len(shape))].Name.Value))
		}
	}
	return names
}

// valueOf the type t: mostly from a small set, so that keys repeat, and otherwise as [ValueOf]
// gives it, with weight on its edges, or, for text and a blob, one time in 16 of about
// [LengthLimit] bytes.
func (q *Sequence) valueOf(t stmt.Type) stmt.Value {
	r := q.r
	if t == stmt.TypeText || t == stmt.TypeBlob {
		if r.IntN(16) == 0 {
			v := long(r, func(n int) string { return runes(r, n) })
			if t == stmt.TypeBlob {
				return stmt.Blob(v)
			}
			return stmt.Text(v)
		}
	}
	if r.IntN(4) == 0 {
		return ValueOf(r, t)
	}
	switch t {
	case stmt.TypeReal:
		return stmt.Real([]float64{0, math.Copysign(0, -1), 1, 2, 0.5, -1.5}[r.IntN(6)])
	case stmt.TypeText:
		return stmt.Text([]string{"", "a", "A", "it's", "é"}[r.IntN(5)])
	case stmt.TypeBlob:
		return stmt.Blob([][]byte{{}, {0}, {0, 0}, {'a'}}[r.IntN(4)])
	}
	return stmt.Integer(int64(r.IntN(5)) - 1)
}

// anyValue of a type at random, as an argument.
func (q *Sequence) anyValue() any {
	return Argument(q.valueOf(Types[q.r.IntN(len(Types))]))
}

// Like a, an argument: another value of the sequence of the same Go type, if a is a value that a
// column holds of type int64, float64, string, or []byte, and otherwise a, so that an argument that
// no column holds stays one.
func (q *Sequence) Like(a any) any {
	if s, ok := a.(string); ok && (!utf8.ValidString(s) || strings.ContainsRune(s, 0)) {
		return a
	}
	switch v := a.(type) {
	case int64:
		return Argument(q.valueOf(stmt.TypeInt))
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			return Argument(q.valueOf(stmt.TypeReal))
		}
	case string:
		return Argument(q.valueOf(stmt.TypeText))
	case []byte:
		if v != nil {
			return Argument(q.valueOf(stmt.TypeBlob))
		}
	}
	return a
}

// Others are texts that are not statements of slices 1, 2, and 4, with %[1]v for a table, %[2]v
// for a column, and %[3]v for a small integer: statements that the pass does not support, among them
// literals that SQLite reads as an infinite real, as a real for an integer outside 64 bits, or as
// NULL, texts that SQLite refuses with a syntax error, and texts without a statement. Two are
// statements of slice 5, an insert without a list of columns and a select of a column, which s4ql
// supports and a sequence does not mark as supported yet.
var Others = []string{
	"create table %[1]v (%[2]v any primary key) strict, without rowid",
	"create table %[1]v (%[2]v varchar primary key) strict, without rowid",
	"insert into %[1]v (%[2]v) values (1e999)",
	"insert into %[1]v (%[2]v) values (-1e400)",
	"insert into %[1]v (%[2]v) values (9223372036854775808)",
	"insert into %[1]v (%[2]v) values (0x%[3]v)",
	"insert into %[1]v (%[2]v) values (null)",
	"insert into %[1]v (%[2]v) values (-'a')",
	"insert into %[1]v (%[2]v) values (x'6')",
	"insert into %[1]v (%[2]v) values ('a''b)",
	"create table %[1]v (%[2]v int primary key) strict",
	"create table if not exists %[1]v (%[2]v int primary key) strict, without rowid",
	`create table "" (%[2]v int primary key) strict, without rowid`,
	`create table %[1]v ("" int primary key) strict, without rowid`,
	"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
	"insert into %[1]v (%[2]v) values (?), (?)",
	"insert into %[1]v (%[2]v) values (?1)",
	"insert into %[1]v (%[2]v) values (:a)",
	"insert into %[1]v (%[2]v) values (@a)",
	"insert into %[1]v (%[2]v) values ($a)",
	"insert into %[1]v (%[2]v) values (-?)",
	"insert into %[1]v (%[2]v) values (? ?)",
	"select * from %[1]v where %[2]v = ?",
	"insert or replace into %[1]v (%[2]v) values (%[3]v)",
	"insert into %[1]v (%[2]v) values (+%[3]v)",
	"insert into %[1]v values (%[3]v)",
	`insert into "" (%[2]v) values (%[3]v)`,
	"select * from %[1]v where %[2]v = %[3]v",
	"select %[2]v from %[1]v",
	`select * from ""`,
	"drop table if exists %[1]v",
	"update %[1]v set %[2]v = %[3]v",
	"begin",
	"select * from %[1]v; select * from %[1]v",
	"select * frm %[1]v",
	"insert into %[1]v (%[2]v) values (",
	"create table %[1]v (%[2]v int primary key) strict without rowid",
	"drop table",
	"",
	" ; -- nothing",
}

// Call of a text: what an application gives to database/sql.
type Call struct {
	Src string
	// Args of the call, by position: an int64, a float64, a string, or a []byte that is not nil for
	// the arguments that s4ql supports, as [Argument] gives them, and other values, such as a
	// [sql.NamedArg], an int32, a float32, or one of [Unsupported], for the cases around them.
	Args []any
	// Query reports whether to run the call as a query.
	Query bool
	// Supported reports whether the text is a statement of slices 1, 2, and 4, which s4ql supports.
	Supported bool
}

// Integers of args, and true, if each is a Go integer without a name that fits a signed 64-bit
// integer, as database/sql hands it to the driver as an int64.
func Integers(args []any) ([]int64, bool) {
	vs := make([]int64, 0, len(args))
	for _, a := range args {
		v, ok := integer(a)
		if !ok {
			return nil, false
		}
		vs = append(vs, v)
	}
	return vs, true
}

// integer of the argument a, and true, if it is a Go integer that fits a signed 64-bit integer.
func integer(a any) (int64, bool) {
	rv := reflect.ValueOf(a)
	switch rv.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return rv.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if rv.Uint() > math.MaxInt64 {
			return 0, false
		}
		return int64(rv.Uint()), true
	}
	return 0, false
}

// Next call of the sequence. Its arguments are those of its parameters, and [Sequence.around]
// changes them for one call in four of a statement with a parameter and for one in twenty of a
// statement without one.
func (q *Sequence) Next() Call {
	c := q.next()
	if c.Supported && q.r.IntN(map[bool]int{true: 4, false: 20}[len(c.Args) > 0]) == 0 {
		c.Args = q.around(c.Args)
	}
	return c
}

// around args, the arguments of a call, are arguments of the cases around them. For a statement
// without a parameter, they are one argument, a value or one that no column holds. For one with a
// parameter, the arguments lack the last, have one more, have one that no column holds, have one
// with a name, or have a value of another Go type that s4ql supports: an integer of another Go type
// than int64, a float32 for a float64, and for text or a blob one that no column holds of the same
// Go type.
func (q *Sequence) around(args []any) []any {
	r := q.r
	if len(args) == 0 {
		if r.IntN(2) == 0 {
			return []any{Unsupported[r.IntN(len(Unsupported))]}
		}
		return []any{q.anyValue()}
	}
	args = slices.Clone(args)
	at := r.IntN(len(args))
	switch r.IntN(5) {
	case 0:
		return args[:len(args)-1]
	case 1:
		return append(args, q.anyValue())
	case 2:
		args[at] = Unsupported[r.IntN(len(Unsupported))]
	case 3:
		args[at] = sql.Named("v", args[at])
	default:
		switch v := args[at].(type) {
		case int64:
			switch {
			case v >= 0 && r.IntN(2) == 0:
				args[at] = uint64(v)
			case v >= math.MinInt32 && r.IntN(2) == 0:
				args[at] = int32(v)
			default:
				args[at] = int(v)
			}
		case float64:
			// Only where a float32 holds it as a finite real, so that it stays a value.
			if f := float32(v); !math.IsInf(float64(f), 0) {
				args[at] = f
			}
		case string:
			args[at] = []string{"a\x00b", "\xff", v + "\x00"}[r.IntN(3)]
		case []byte:
			args[at] = []byte(nil)
		}
	}
	return args
}

// next call of the sequence, with an argument for each parameter of a statement of slices 1, 2,
// and 4, and for each ? of a text that is not.
func (q *Sequence) next() Call {
	r := q.r
	if r.IntN(12) == 0 {
		form := Others[r.IntN(len(Others))]
		table, column, value := q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))], r.IntN(5)
		c := Call{Src: form, Query: r.IntN(2) == 0}
		// Sprintf would add the arguments that a form without a verb does not use to its text.
		if strings.Contains(form, "%") {
			c.Src = fmt.Sprintf(form, table, column, value)
		}
		for range strings.Count(c.Src, "?") {
			c.Args = append(c.Args, q.anyValue())
		}
		return c
	}
	var k Kind
	switch n := r.IntN(20); {
	case n < 4:
		k = KindCreateTable
	case n < 14:
		k = KindInsert
	case n < 19:
		k = KindSelect
	default:
		k = KindDropTable
	}
	src, _, args := q.g.CallOf(k)
	// A query runs through Exec one time in eight, and every other statement through Query one time
	// in eight.
	return Call{Src: src, Args: args, Query: (k == KindSelect) != (r.IntN(8) == 0), Supported: true}
}
