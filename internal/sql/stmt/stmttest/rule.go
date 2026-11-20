package stmttest

import (
	"fmt"
	"reflect"
	"slices"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// TableColumn of a table as SQLite has it: its name as the table stores it, its declared type, such
// as INT, and whether it is the primary key.
type TableColumn struct {
	Name       string
	Type       string
	PrimaryKey bool
}

// Positions of the values of the insert in the table of the columns, in the order of the values:
// the position of the column of each value, by its name in the list of the insert, folded as SQLite
// compares names, or in the order of the table without a list. It is nil, and false, if a name of
// the list is no column of the table, or if the number of values is not that of the list, or of the
// columns without a list, which SQLite refuses when it prepares the insert.
func Positions(columns []TableColumn, insert *stmt.Insert) ([]int, bool) {
	want := len(insert.Columns)
	if insert.Columns == nil {
		want = len(columns)
	}
	if len(insert.Values) != want {
		return nil, false
	}
	var positions []int
	for i := range insert.Values {
		if insert.Columns == nil {
			positions = append(positions, i)
			continue
		}
		position := ColumnOf(columns, insert.Columns[i])
		if position < 0 {
			return nil, false
		}
		positions = append(positions, position)
	}
	return positions, true
}

// ColumnOf the name in the table of the columns, folded as SQLite compares names: its position, or
// -1 if the table has no column of the name.
func ColumnOf(columns []TableColumn, name stmt.Name) int {
	for position, c := range columns {
		if (stmt.Name{Value: c.Name}).Fold() == name.Fold() {
			return position
		}
	}
	return -1
}

// TypeRule of the insert into the table of the name, as the table stores it, and of the columns, in
// the order of the table, with the arguments args, each an int64, a float64, a string, or a []byte,
// as SQLite binds them: s4ql's "not supported" for the first column, in the order of the table,
// whose value, a literal or an argument, is of another type than the column, where SQLite would
// convert the value or give an error of its own. It is "" where every value has the type of its
// column, and where SQLite gives its own error first: for a name of the list that the table does not
// have, for another number of values, and for a column without a value, which gets SQLite's `NOT NULL
// constraint failed` before SQLite looks at a type.
func TypeRule(table string, columns []TableColumn, insert *stmt.Insert, args []any) string {
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return ""
	}
	values := make([]int, len(columns))
	for i := range values {
		values[i] = -1
	}
	for i, position := range positions {
		values[position] = i
	}
	if slices.Contains(values, -1) {
		return ""
	}
	for position, i := range values {
		kind, what := ValueKind(insert.Values[i], args)
		if c := columns[position]; columnHolds[c.Type] != kind {
			return fmt.Sprintf("s4ql: not supported: %v for the column %v.%v of type %v", what, table, c.Name, c.Type)
		}
	}
	return ""
}

// columnHolds is the kind of value, as [ValueKind] names it, that a column of each declared type
// holds.
var columnHolds = map[string]string{"INT": "integer", "INTEGER": "integer", "REAL": "real", "TEXT": "text", "BLOB": "blob"}

// ValueKind of the value of a term of an insert, a literal or the argument of args for its
// parameter, as SQLite binds it: "integer", "real", "text", or "blob", and how s4ql's message of a
// value of another type than its column names the value. It panics for an argument of another type
// than int64, float64, string, and []byte, and for a literal of no kind.
func ValueKind(term stmt.Term, args []any) (kind, what string) {
	if term.Parameter > 0 {
		a := args[term.Parameter-1]
		switch a.(type) {
		case int64:
			kind = "integer"
		case float64:
			kind = "real"
		case string:
			kind = "text"
		case []byte:
			kind = "blob"
		default:
			panic(fmt.Sprintf("stmttest: an argument of type %v for SQLite", reflect.TypeOf(a)))
		}
		name := map[string]string{"integer": "an integer", "real": "a real", "text": "text", "blob": "a blob"}[kind]
		return kind, fmt.Sprintf("argument %v, which is %v,", term.Parameter, name)
	}
	switch v := term.Value.(type) {
	case stmt.Integer:
		return "integer", "an integer literal"
	case stmt.Real:
		return "real", "a real literal"
	case stmt.Text:
		return "text", "a string literal"
	case stmt.Blob:
		return "blob", "a blob literal"
	default:
		panic(fmt.Sprintf("stmttest: a literal of type %T", v))
	}
}

// Faults of the insert into the table of the columns, with the arguments args as for [TypeRule], by
// kind, in the order in which SQLite and s4ql look for them: each name of the list that the table
// does not have, "a column that the table does not have"; another number of values, "the wrong number of
// values"; each column without a value, "a column without a value"; and each value of another type
// than its column, "a value of another type in the key" or "a value of another type in another
// column". A wrong number of values, and a name that the table does not have, leave out the faults of
// the values.
func Faults(columns []TableColumn, insert *stmt.Insert, args []any) []string {
	var faults []string
	for _, name := range insert.Columns {
		if ColumnOf(columns, name) < 0 {
			faults = append(faults, "a column that the table does not have")
		}
	}
	want := len(insert.Columns)
	if insert.Columns == nil {
		want = len(columns)
	}
	if len(insert.Values) != want {
		faults = append(faults, "the wrong number of values")
	}
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return faults
	}
	named := make([]bool, len(columns))
	for _, position := range positions {
		named[position] = true
	}
	for _, n := range named {
		if !n {
			faults = append(faults, "a column without a value")
		}
	}
	for i, position := range positions {
		kind, _ := ValueKind(insert.Values[i], args)
		if c := columns[position]; columnHolds[c.Type] != kind {
			faults = append(faults, map[bool]string{true: "a value of another type in the key", false: "a value of another type in another column"}[c.PrimaryKey])
		}
	}
	return faults
}
