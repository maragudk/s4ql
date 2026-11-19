package stmttest

import (
	"database/sql"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// Sequence of random statements of slices 1, 2, and 4 that meet at a few tables and columns, each
// named in several spellings that SQLite takes for one name and in some that it takes for others,
// with values from a small set so that keys repeat, and with statements that are not supported and
// texts that SQLite refuses mixed in.
//
// Each name of a table has a type of the sequence's own, which most of its create tables declare,
// and most values of an insert into a table of the name are of that type: the others are of another
// type than the column, which s4ql refuses. The small sets hold keys that SQLite takes for one, such
// as 0.0 and -0.0, so that every type meets a key that exists.
//
// One insert in three has a plain ? with its value as the argument, and some calls, as
// [Sequence.Next] says, have arguments around those that s4ql supports: none for a ?, one for a
// statement without one, two for a ?, one that no column holds, one with a name, or a value of
// another Go type that s4ql supports.
type Sequence struct {
	r       Rand
	g       *Generator
	tables  []string
	columns []string
	// types of the tables by their names, folded as SQLite compares names.
	types map[string]stmt.Type
}

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
		common(pools, CreateColumn, InsertColumn),
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
	q := &Sequence{r: r, types: map[string]stmt.Type{}}
	tableNames, columnNames := common[0], common[1]
	for range 1 + r.IntN(2) {
		q.tables = append(q.tables, spellings(r, tableNames[r.IntN(len(tableNames))])...)
	}
	for range 1 + r.IntN(2) {
		q.columns = append(q.columns, spellings(r, columnNames[r.IntN(len(columnNames))])...)
	}
	pools := make([][]string, len(PositionTexts))
	for _, p := range []Position{CreateTable, InsertTable, SelectTable, DropTable} {
		pools[p] = q.tables
	}
	pools[CreateColumn], pools[InsertColumn] = q.columns, q.columns
	q.g = NewGenerator(r, pools)
	q.g.Type, q.g.Value = q.columnType, q.Value
	q.g.Parameters = 3
	return q, nil
}

// typeOf the tables of the name of table: the sequence's own, chosen at random the first time.
func (q *Sequence) typeOf(table stmt.Name) stmt.Type {
	name := table.Fold()
	t, ok := q.types[name]
	if !ok {
		t = Types[q.r.IntN(len(Types))]
		q.types[name] = t
	}
	return t
}

// columnType of a create table of table: the type of its name, and one time in ten any type.
func (q *Sequence) columnType(table stmt.Name) stmt.Type {
	if q.r.IntN(10) == 0 {
		return Types[q.r.IntN(len(Types))]
	}
	return q.typeOf(table)
}

// Value of an insert into table: of the type of its name, and one time in ten of another type.
func (q *Sequence) Value(table stmt.Name) stmt.Value {
	t := q.typeOf(table)
	if q.r.IntN(10) == 0 {
		t = Types[q.r.IntN(len(Types))]
	}
	return q.valueOf(t)
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

// Like a, an argument: another value of the sequence of the same Go type, if a is an int64, a
// float64, a string, or a []byte that is not nil, and otherwise a.
func (q *Sequence) Like(a any) any {
	switch v := a.(type) {
	case int64:
		return Argument(q.valueOf(stmt.TypeInt))
	case float64:
		return Argument(q.valueOf(stmt.TypeReal))
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
// NULL, texts that SQLite refuses with a syntax error, and texts without a statement.
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
			args[at] = float32(v)
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
