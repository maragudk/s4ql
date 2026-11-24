package stmttest

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// TableColumn of a table as SQLite has it: its name as the table stores it, its declared type, such
// as INT, whether it is the primary key, whether it takes NULL, which the primary key never does,
// and its default, the text of the literal of its DEFAULT as SQLite keeps it, which
// pragma_table_info gives as dflt_value, or "" if it has none.
type TableColumn struct {
	Name       string
	Type       string
	PrimaryKey bool
	Nullable   bool
	Default    string
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
// the order of the table, with the arguments args, each an int64, a float64, a string, a []byte, or
// nil for NULL, as SQLite binds them: s4ql's "not supported" for the first column, in the order of
// the table, whose value, a literal or an argument, is of another type than the column, where SQLite
// would convert the value or give an error of its own. NULL has no type, so it is of no other type
// than any column. The rule is "" where every value has the type of its column, and where SQLite
// gives its own error first: for a name of the list that the table does not have, for another number
// of values, and for NULL, given or for a column that the list leaves out and whose default is NULL
// or none, in a column that does not take it, which gets SQLite's `NOT NULL constraint failed`
// before SQLite looks at a type. A column that the list leaves out gets its default, which is of the
// type of its column.
func TypeRule(table string, columns []TableColumn, insert *stmt.Insert, args []any) string {
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return ""
	}
	values := valuesOf(columns, positions)
	for position, i := range values {
		if c := columns[position]; !takesNull(c) && (i < 0 && defaultIsNull(c) || i >= 0 && isNull(insert.Values[i], args)) {
			return ""
		}
	}
	for position, i := range values {
		if i < 0 {
			continue
		}
		kind, what := ValueKind(insert.Values[i], args)
		if c := columns[position]; kind != "null" && columnHolds[c.Type] != kind {
			return fmt.Sprintf("s4ql: not supported: %v for the column %v.%v of type %v", what, table, c.Name, c.Type)
		}
	}
	return ""
}

// MissingColumn of the select s of the table of the columns, in the order of the table: the name
// that SQLite's `no such column` names, and true, or false if the table has every column that s
// names. SQLite looks the select list up first, in its order, and then the conditions, in the order
// of the text: the first comparison of a column that the table does not have gives the error. A test
// for NULL of such a column does not stop SQLite, which goes on with the error of that column: a
// later such test replaces it, and the next comparison ends the walk with it. Then come the columns
// of the ORDER BY, of slice 13, in their order.
func MissingColumn(columns []TableColumn, s *stmt.Select) (stmt.Name, bool) {
	for _, name := range s.Columns {
		if ColumnOf(columns, name) < 0 {
			return name, true
		}
	}
	var missing *stmt.Name
	for i, c := range s.Where {
		found := ColumnOf(columns, c.Column) >= 0
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
			if !found {
				missing = &s.Where[i].Column
			}
		case missing != nil:
			return *missing, true
		case !found:
			return c.Column, true
		}
	}
	if missing != nil {
		return *missing, true
	}
	for _, c := range s.OrderBy {
		if ColumnOf(columns, c.Name) < 0 {
			return c.Name, true
		}
	}
	return stmt.Name{}, false
}

// ConditionRule of the select of the table of the name, as the table stores it, and of the columns,
// in the order of the table, with the arguments args as for [TypeRule]: s4ql's "not supported" for
// the first condition, in the order of the text, whose value, a literal or an argument, is of another
// type than its column, where SQLite would convert the value or compare it as it is. NULL has no
// type, so it is of no other type than any column. The rule is "" where every value has the type of
// its column, and where SQLite gives its own error first: for a column of the select list or of a
// condition that the table does not have, and for another number of arguments than parameters.
func ConditionRule(table string, columns []TableColumn, s *stmt.Select, args []any) string {
	if _, missing := MissingColumn(columns, s); missing || len(args) != s.Parameters() {
		return ""
	}
	return WhereRule(table, columns, s.Where, args)
}

// UpdateRule of the update u of the table of the name, as the table stores it, and of the columns,
// in the order of the table, with the arguments args as for [TypeRule]: s4ql's "not supported" for
// the first condition of its WHERE whose value is of another type than its column, as for a select,
// and then for the first column of its SET, in the order of the table, whose value is of another type
// than the column, as for an insert. found reports whether the WHERE holds for a row of the table,
// and the rule asks it only when SET gives NULL to a column that does not take it, which gets SQLite's
// `NOT NULL constraint failed` first when the update finds a row. The rule is "" where every value
// has the type of its column, and where SQLite gives its own error first: for a column of SET, and
// then of WHERE, that the table does not have, and for another number of arguments than parameters.
func UpdateRule(table string, columns []TableColumn, u *stmt.Update, args []any, found func() bool) string {
	if len(args) != u.Parameters() {
		return ""
	}
	byPosition := map[int]int{}
	for i, a := range u.Set {
		position := ColumnOf(columns, a.Column)
		if position < 0 {
			return ""
		}
		byPosition[position] = i
	}
	if _, missing := MissingColumn(columns, &stmt.Select{Where: u.Where}); missing {
		return ""
	}
	if rule := WhereRule(table, columns, u.Where, args); rule != "" {
		return rule
	}
	for position, c := range columns {
		if i, ok := byPosition[position]; ok && !takesNull(c) && isNull(u.Set[i].Value, args) {
			if found() {
				return ""
			}
			break
		}
	}
	for position, c := range columns {
		i, ok := byPosition[position]
		if !ok {
			continue
		}
		if kind, what := ValueKind(u.Set[i].Value, args); kind != "null" && columnHolds[c.Type] != kind {
			return fmt.Sprintf("s4ql: not supported: %v for the column %v.%v of type %v", what, table, c.Name, c.Type)
		}
	}
	return ""
}

// DeleteRule of the delete d of the table of the name and the columns, with the arguments args, as
// [ConditionRule] gives it for a select with the same WHERE.
func DeleteRule(table string, columns []TableColumn, d *stmt.Delete, args []any) string {
	return ConditionRule(table, columns, &stmt.Select{Table: d.Table, Where: d.Where}, args)
}

// FoundQuery of the conditions of a WHERE on the table, whose scalar subqueries are subs: a select
// that gives a row if the conditions hold for a row of the table, and the arguments of its
// parameters, each as SQLite binds it, from the literals of the conditions and from args for their
// parameters, as the statement of the conditions numbers them.
func FoundQuery(table stmt.Name, where []stmt.Condition, subs []stmt.Select, args []any) (string, []any) {
	conditions, bound := whereText(where, subs, args)
	return "select 1 from " + Quote(table.Value) + conditions + " limit 1", bound
}

// SelectText of the select s with the arguments args of its parameters: the select again, with its
// select list, or *, its table, and its WHERE, each name in double quotes, a ? for each value of a
// condition, a subquery of slice 15 as [subqueryText] writes it, and orderBy after it, and the
// arguments of its parameters, as [FoundQuery] gives them.
func SelectText(s *stmt.Select, args []any, orderBy string) (string, []any) {
	list := "*"
	if s.Columns != nil {
		var names []string
		for _, c := range s.Columns {
			names = append(names, Quote(c.Value))
		}
		list = strings.Join(names, ", ")
	}
	conditions, bound := whereText(s.Where, s.Subqueries, args)
	return "select " + list + " from " + Quote(s.Table.Value) + conditions + orderBy, bound
}

// whereText of the conditions of a WHERE, whose scalar subqueries are subs, with a ? for each value,
// and a subquery as [subqueryText] writes it, after " where ", or "" for no condition, and the
// arguments of its parameters, as [FoundQuery] gives them. A condition that says
// [stmt.Condition.ValueFirst] is written so, with the ? on the left and the operator as the text
// wrote it, so that a statement of the text runs the form that its conditions came from.
func whereText(where []stmt.Condition, subs []stmt.Select, args []any) (string, []any) {
	var conditions []string
	var bound []any
	for _, c := range where {
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
			conditions = append(conditions, Quote(c.Column.Value)+" "+c.Operator.String())
			continue
		case c.Value.Subquery > 0:
			text, inner := subqueryText(subs[c.Value.Subquery-1], args)
			if c.ValueFirst {
				conditions = append(conditions, text+" "+c.Operator.Swapped().String()+" "+Quote(c.Column.Value))
			} else {
				conditions = append(conditions, Quote(c.Column.Value)+" "+c.Operator.String()+" "+text)
			}
			bound = append(bound, inner...)
			continue
		case c.ValueFirst:
			conditions = append(conditions, "? "+c.Operator.Swapped().String()+" "+Quote(c.Column.Value))
		default:
			conditions = append(conditions, Quote(c.Column.Value)+" "+c.Operator.String()+" ?")
		}
		bound = append(bound, boundValue(c.Value, args))
	}
	if len(conditions) == 0 {
		return "", nil
	}
	return " where " + strings.Join(conditions, " and "), bound
}

// subqueryText of the scalar subquery q, of slice 15, with the arguments args of the statement that
// holds it: its select in parentheses, as [SelectText] writes a select, with its ORDER BY and its
// LIMIT, a ? for an argument of LIMIT, and the arguments of its parameters, in the order of the text.
func subqueryText(q stmt.Select, args []any) (string, []any) {
	var by []string
	for _, c := range q.OrderBy {
		term := Quote(c.Name.Value)
		if c.Descending {
			term += " desc"
		}
		by = append(by, term)
	}
	orderBy := ""
	if by != nil {
		orderBy = " order by " + strings.Join(by, ", ")
	}
	text, bound := SelectText(&stmt.Select{Table: q.Table, Columns: q.Columns, Where: q.Where}, args, orderBy)
	switch {
	case q.Limit.Parameter > 0:
		text += " limit ?"
		bound = append(bound, args[q.Limit.Parameter-1])
	case q.Limited():
		text += fmt.Sprint(" limit ", int64(q.Limit.Value.(stmt.Integer)))
	}
	return "(" + text + ")", bound
}

// boundValue of a term, a literal or the argument of args for its parameter, as SQLite binds it: an
// int64, a float64, a string, a []byte, or nil.
func boundValue(term stmt.Term, args []any) any {
	switch v := term.Value.(type) {
	case stmt.Integer:
		return int64(v)
	case stmt.Real:
		return float64(v)
	case stmt.Text:
		return string(v)
	case stmt.Blob:
		return []byte(v)
	case stmt.Null:
		return nil
	}
	return args[term.Parameter-1]
}

// RowQueries of the update u of a table whose key is the column with the name key, with the
// arguments args as SQLite binds them: a create of the temporary table s4ql_found of the keys of the
// rows that the WHERE of u finds, in the order of the key, and the arguments of its parameters; and
// an update with the SET of u of the row whose key is the one at an offset of s4ql_found, and the
// arguments of its parameters but the last, which is the offset. The updates of one row each, in the
// order of the offsets, change the rows as u changes them, one at a time in the order of the key,
// and each measures the record of its row afresh, as a statement that changes many rows does not in
// every build of SQLite: see the known difference of a record that is too long in docs/todo/sql-front-end.md.
func RowQueries(u *stmt.Update, key string, args []any) (found string, foundArgs []any, one string, oneArgs []any) {
	conditions, bound := whereText(u.Where, u.Subqueries, args)
	table := Quote(u.Table.Value)
	var sets []string
	for _, a := range u.Set {
		sets = append(sets, Quote(a.Column.Value)+" = ?")
		oneArgs = append(oneArgs, boundValue(a.Value, args))
	}
	found = "create temp table s4ql_found as select " + Quote(key) + " as k from " + table + conditions + " order by " + Quote(key)
	one = "update " + table + " set " + strings.Join(sets, ", ") + " where " + Quote(key) + " = (select k from temp.s4ql_found order by rowid limit 1 offset ?)"
	return found, bound, one, oneArgs
}

// WhereRule of the conditions of a WHERE of the table of the name and the columns, which the table
// has, with the arguments args of the statement, by the numbers of the parameters: s4ql's "not
// supported" for the first condition, in the order of the text, whose value is of another type than
// its column, as [ConditionRule] gives it, or "". A scalar subquery, of slice 15, has the type of the
// column of its table, which the conditions do not hold, so the rule passes over it.
func WhereRule(table string, columns []TableColumn, where []stmt.Condition, args []any) string {
	for _, c := range where {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || c.Value.Subquery > 0 {
			continue
		}
		kind, what := ValueKind(c.Value, args)
		if column := columns[ColumnOf(columns, c.Column)]; kind != "null" && columnHolds[column.Type] != kind {
			return fmt.Sprintf("s4ql: not supported: %v compared with the column %v.%v of type %v", what, table, column.Name, column.Type)
		}
	}
	return ""
}

// valuesOf the columns: for each column, in the order of the table, the index of its value among
// the values of an insert at the positions, or -1 for a column that the insert leaves out.
func valuesOf(columns []TableColumn, positions []int) []int {
	values := make([]int, len(columns))
	for i := range values {
		values[i] = -1
	}
	for i, position := range positions {
		values[position] = i
	}
	return values
}

// takesNull reports whether the column c takes NULL: never the primary key.
func takesNull(c TableColumn) bool {
	return c.Nullable && !c.PrimaryKey
}

// defaultIsNull reports whether the column c gets NULL where an insert leaves it out: it has no
// default, or DEFAULT NULL, in any case of the letters.
func defaultIsNull(c TableColumn) bool {
	return c.Default == "" || strings.EqualFold(c.Default, "null")
}

// isNull reports whether the value of the term, a literal or the argument of args for its parameter,
// is NULL.
func isNull(term stmt.Term, args []any) bool {
	kind, _ := ValueKind(term, args)
	return kind == "null"
}

// columnHolds is the kind of value, as [ValueKind] names it, that a column of each declared type
// holds.
var columnHolds = map[string]string{"INT": "integer", "INTEGER": "integer", "REAL": "real", "TEXT": "text", "BLOB": "blob"}

// ValueKind of the value of a term of an insert, a literal or the argument of args for its
// parameter, as SQLite binds it: "integer", "real", "text", "blob", or "null", and how s4ql's message
// of a value of another type than its column names the value, which no message does for NULL. An
// argument is NULL if it is nil, and a []byte that is nil is a blob. It panics for an argument of
// another type than int64, float64, string, and []byte, and for a literal of no kind.
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
		case nil:
			return "null", fmt.Sprintf("argument %v, which is NULL,", term.Parameter)
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
	case stmt.Null:
		return "null", "a NULL literal"
	default:
		panic(fmt.Sprintf("stmttest: a literal of type %T", v))
	}
}

// Faults of the insert into the table of the columns, with the arguments args as for [TypeRule], by
// kind, in the order in which SQLite and s4ql look for them: each name of the list that the table
// does not have, "a column that the table does not have"; another number of values, "the wrong
// number of values"; each column that the list leaves out, that does not take NULL, and whose
// default is NULL or none, "a column without a value"; each NULL that the insert gives for the key,
// "NULL in the key", or for another column that does not take it, "NULL in a column that does not
// take it"; and each value of another type than its column, "a value of another type in the key" or
// "a value of another type in another column", which NULL never is. A column that takes NULL and
// that the list leaves out gets NULL or its default, and a column with a default that is not NULL
// gets the default, which is no fault. A wrong number of values, and a name that the table does not
// have, leave out the faults of the values.
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
	for position, i := range valuesOf(columns, positions) {
		if c := columns[position]; i < 0 && !takesNull(c) && defaultIsNull(c) {
			faults = append(faults, "a column without a value")
		}
	}
	for i, position := range positions {
		if c := columns[position]; !takesNull(c) && isNull(insert.Values[i], args) {
			faults = append(faults, map[bool]string{true: "NULL in the key", false: "NULL in a column that does not take it"}[c.PrimaryKey])
		}
	}
	for i, position := range positions {
		kind, _ := ValueKind(insert.Values[i], args)
		if c := columns[position]; kind != "null" && columnHolds[c.Type] != kind {
			faults = append(faults, map[bool]string{true: "a value of another type in the key", false: "a value of another type in another column"}[c.PrimaryKey])
		}
	}
	return faults
}

// NullCases of the insert into the table of the columns, with the arguments args as for [TypeRule],
// for a count of what a test reaches of NULL: each NULL that the insert gives, in the order of its
// values, as "NULL literal into" or "NULL argument into" and "the key", "a column that takes NULL",
// or "a column that does not take NULL"; "insert that leaves out a column that takes NULL" if its
// list leaves out one that gets NULL, whose default is NULL or none; and "insert of NULL beside a
// value of another type" if it gives NULL and a
// value of another type than its column, as [Faults] finds it. It is nil for an insert that SQLite
// refuses for its names or its number of values, and for another number of arguments than
// parameters.
func NullCases(columns []TableColumn, insert *stmt.Insert, args []any) []string {
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return nil
	}
	var cases []string
	for i, position := range positions {
		if !isNull(insert.Values[i], args) {
			continue
		}
		into := "a column that takes NULL"
		switch c := columns[position]; {
		case c.PrimaryKey:
			into = "the key"
		case !takesNull(c):
			into = "a column that does not take NULL"
		}
		cases = append(cases, fmt.Sprint("NULL ", map[bool]string{true: "argument", false: "literal"}[insert.Values[i].Parameter > 0], " into ", into))
	}
	given := len(cases) > 0
	for position, i := range valuesOf(columns, positions) {
		if i < 0 && takesNull(columns[position]) && defaultIsNull(columns[position]) {
			cases = append(cases, "insert that leaves out a column that takes NULL")
			break
		}
	}
	if given && slices.ContainsFunc(Faults(columns, insert, args), func(f string) bool { return strings.HasPrefix(f, "a value of another type") }) {
		cases = append(cases, "insert of NULL beside a value of another type")
	}
	return cases
}

// LongDefault is the length, in bytes, of the literal of a default from which [DefaultCases] calls
// it long: a quarter of [LengthLimit].
const LongDefault = LengthLimit / 4

// DefaultCases of the insert into the table of the columns, with the arguments args as for
// [TypeRule], for a count of what a test reaches of the defaults of slice 11. For each column that
// the list leaves out and that has a default, in the order of the table: "insert that leaves out the
// key with a default" for the key, whose default is not NULL; "insert that leaves out a column with a
// default of integer" (of real, of text, or of blob) for another column whose default is not NULL;
// and for DEFAULT NULL "insert that leaves out the key with DEFAULT NULL", "insert that leaves out a
// column that takes NULL with DEFAULT NULL", or "insert that leaves out a column that does not take
// NULL with DEFAULT NULL". Then "insert that leaves out a column with a long default" once, if the
// literal of a default of a column that the list leaves out is [LongDefault] bytes or longer; and
// for each NULL that the insert gives, as a literal or an argument, to a column with a default that
// is not NULL, "NULL given for a column with a default, which takes NULL" or "NULL given for a column
// with a default, which does not take NULL". It is nil for an insert that SQLite refuses for its
// names or its number of values, and for another number of arguments than parameters.
func DefaultCases(columns []TableColumn, insert *stmt.Insert, args []any) []string {
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return nil
	}
	var cases []string
	long := false
	for position, i := range valuesOf(columns, positions) {
		c := columns[position]
		if c.Default == "" {
			continue
		}
		if i >= 0 {
			if isNull(insert.Values[i], args) && !defaultIsNull(c) {
				cases = append(cases, "NULL given for a column with a default, which "+map[bool]string{true: "takes NULL", false: "does not take NULL"}[takesNull(c)])
			}
			continue
		}
		long = long || len(c.Default) >= LongDefault
		switch {
		case defaultIsNull(c) && c.PrimaryKey:
			cases = append(cases, "insert that leaves out the key with DEFAULT NULL")
		case defaultIsNull(c) && takesNull(c):
			cases = append(cases, "insert that leaves out a column that takes NULL with DEFAULT NULL")
		case defaultIsNull(c):
			cases = append(cases, "insert that leaves out a column that does not take NULL with DEFAULT NULL")
		case c.PrimaryKey:
			cases = append(cases, "insert that leaves out the key with a default")
		default:
			cases = append(cases, "insert that leaves out a column with a default of "+columnHolds[c.Type])
		}
	}
	if long {
		cases = append(cases, "insert that leaves out a column with a long default")
	}
	return cases
}

// LeavesOutIndexed reports whether the insert into the table of the columns, in the order of the
// table, with the indexes, leaves out a column with a default, also DEFAULT NULL, that an index
// holds, so that the index must hold the default. It is false for an insert that SQLite refuses for
// its names or its number of values. A position of an index that is no column, such as SQLite's -1
// for the rowid, holds no default.
func LeavesOutIndexed(columns []TableColumn, indexes []TableIndex, insert *stmt.Insert) bool {
	positions, ok := Positions(columns, insert)
	if !ok {
		return false
	}
	values := valuesOf(columns, positions)
	for _, ix := range indexes {
		for _, c := range ix.Columns {
			if c.Position >= 0 && c.Position < len(columns) && values[c.Position] < 0 && columns[c.Position].Default != "" {
				return true
			}
		}
	}
	return false
}

// WhereCases of the select s with WHERE of the table of the columns, in the order of the table, with
// the arguments args as for [TypeRule], for a count of what a test reaches of slice 7. For a column of
// a condition that the table does not have, where the select list has every column that it names:
// "WHERE with a column that the table does not have", in a comparison or under IS NULL or IS NOT
// NULL, and alone, before other conditions, or after them, and "WHERE with a column in double quotes
// that the table does not have" for one in double quotes; and nothing more, since SQLite gives its
// error first. Otherwise, with as many arguments as parameters: the operator of each condition on
// the key or on another column, such as "<= on the key"; the value of each comparison, a literal by
// its kind, as [LiteralKind] names it, an argument, as [ArgumentKind] names it, or "comparison with a
// subquery" for a scalar subquery of slice 15; "WHERE with a
// value of another type" if [ConditionRule] refuses the select, and nothing more; "WHERE that tests
// a column more than once"; "WHERE with contradicting conditions" if two conditions on one column
// are IS NULL and IS NOT NULL, or compare with one value by inverse operators, such as < and >=; and
// how s4ql reads the rows, as [KeyAccess] gives it, after "WHERE that reads ". It is nil for a
// select without WHERE, one of a column of the select list that the table does not have, and one
// with another number of arguments than parameters.
func WhereCases(columns []TableColumn, s *stmt.Select, args []any) []string {
	if s.Where == nil || slices.ContainsFunc(s.Columns, func(n stmt.Name) bool { return ColumnOf(columns, n) < 0 }) {
		return nil
	}
	var cases []string
	for i, c := range s.Where {
		if ColumnOf(columns, c.Column) >= 0 {
			continue
		}
		in := "in a comparison"
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			in = "under IS NULL or IS NOT NULL"
		}
		at := "after other conditions"
		switch {
		case len(s.Where) == 1:
			at = "alone"
		case i < len(s.Where)-1:
			at = "before other conditions"
		}
		cases = append(cases, fmt.Sprint("WHERE with a column that the table does not have ", in, ", ", at))
		if c.Column.Raw[0] == '"' {
			cases = append(cases, "WHERE with a column in double quotes that the table does not have")
		}
	}
	if cases != nil || len(args) != s.Parameters() {
		return cases
	}
	for _, c := range s.Where {
		on := "on another column"
		if columns[ColumnOf(columns, c.Column)].PrimaryKey {
			on = "on the key"
		}
		cases = append(cases, fmt.Sprint(c.Operator, " ", on))
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
		case c.Value.Subquery > 0:
			cases = append(cases, "comparison with a subquery")
		case c.Value.Parameter > 0:
			cases = append(cases, "comparison with argument "+ArgumentKind(args[c.Value.Parameter-1]))
		default:
			cases = append(cases, "comparison with literal "+LiteralKind(c.Value.Value))
		}
	}
	if ConditionRule("t", columns, s, args) != "" {
		return append(cases, "WHERE with a value of another type")
	}
	tested := map[int]bool{}
	twice, contradicts := false, false
	for i, c := range s.Where {
		position := ColumnOf(columns, c.Column)
		twice = twice || tested[position]
		tested[position] = true
		for _, d := range s.Where[:i] {
			if ColumnOf(columns, d.Column) != position || inverses[d.Operator] != c.Operator {
				continue
			}
			a, b := termValue(c.Value, args), termValue(d.Value, args)
			if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || a != nil && b != nil && sameValue(a, b) {
				contradicts = true
			}
		}
	}
	if twice {
		cases = append(cases, "WHERE that tests a column more than once")
	}
	if contradicts {
		cases = append(cases, "WHERE with contradicting conditions")
	}
	return append(cases, "WHERE that reads "+KeyAccess(columns, s, args))
}

// ValueFirstCases of the WHERE of the select s, with the arguments args of its parameters: for each
// comparison that its text writes with the value on the left of the column, of slice 12, "value on
// the left of" and the operator as the text writes it, which [stmt.Operator.Swapped] gives, such as
// "value on the left of <" for `5 < a`; and "value on the left compared with NULL" for one whose
// value is the literal NULL or the argument nil. Whether a parameter is NULL is known only when args
// has an argument for each parameter. It does not depend on the table, so it names also a
// comparison of a column that the table does not have.
func ValueFirstCases(s *stmt.Select, args []any) []string {
	var cases []string
	for _, c := range s.Where {
		if !c.ValueFirst {
			continue
		}
		cases = append(cases, fmt.Sprint("value on the left of ", c.Operator.Swapped()))
		_, literal := c.Value.Value.(stmt.Null)
		if literal || c.Value.Parameter > 0 && len(args) == s.Parameters() && args[c.Value.Parameter-1] == nil {
			cases = append(cases, "value on the left compared with NULL")
		}
	}
	return cases
}

// SelectOf the WHERE of the update or the delete s, with the arguments args of s: a select of * of its
// table with its conditions, whose parameters are numbered from 1, as those of a select of its own
// are, and the arguments of those parameters, as [WhereCases] and [KeyAccess] take them, with the
// scalar subqueries of s, of slice 15, whose parameters are numbered so too. It is nil for another
// statement, and for another number of arguments than s has parameters.
func SelectOf(s stmt.Statement, args []any) (*stmt.Select, []any) {
	var table stmt.Name
	var where []stmt.Condition
	offset := 0
	switch s := s.(type) {
	case *stmt.Update:
		table, where = s.Table, s.Where
		for _, a := range s.Set {
			if a.Value.Parameter > 0 {
				offset++
			}
		}
	case *stmt.Delete:
		table, where = s.Table, s.Where
	default:
		return nil, nil
	}
	if len(args) != s.Parameters() {
		return nil, nil
	}
	sel := &stmt.Select{Table: table}
	for _, c := range where {
		if c.Value.Parameter > 0 {
			c.Value.Parameter -= offset
		}
		sel.Where = append(sel.Where, c)
	}
	for _, q := range subqueriesOf(s) {
		// The parameters of a subquery come after those of SET too.
		q.Where = slices.Clone(q.Where)
		for i := range q.Where {
			if q.Where[i].Value.Parameter > 0 {
				q.Where[i].Value.Parameter -= offset
			}
		}
		if q.Limit.Parameter > 0 {
			q.Limit.Parameter -= offset
		}
		sel.Subqueries = append(sel.Subqueries, q)
	}
	return sel, args[offset:]
}

// SetCases of the update u of the table of the columns, in the order of the table, with the arguments
// args as for [TypeRule], for a count of what a test reaches of slice 8. For a column of SET that the
// table does not have: "SET of a column that the table does not have", also "in double quotes" for
// one in double quotes, and nothing more, since SQLite gives its error first. Otherwise: "SET of the
// key" if it sets the key; "SET of every column" if it sets each; "SET of NULL for a column that
// does not take it" for the key or a column with NOT NULL; with as many arguments as parameters, the
// value of each, a literal by its kind, as [LiteralKind] names it, after "SET with literal ", or an
// argument, as [ArgumentKind] names it, after "SET with argument "; and "SET with a value of another
// type" for a value that is not NULL of another type than its column.
func SetCases(columns []TableColumn, u *stmt.Update, args []any) []string {
	var cases []string
	for _, a := range u.Set {
		if ColumnOf(columns, a.Column) < 0 {
			cases = append(cases, "SET of a column that the table does not have")
			if a.Column.Raw[0] == '"' {
				cases = append(cases, "SET of a column in double quotes that the table does not have")
			}
			return cases
		}
	}
	argsOK := len(args) == u.Parameters()
	key, other, null := false, false, false
	for _, a := range u.Set {
		c := columns[ColumnOf(columns, a.Column)]
		key = key || c.PrimaryKey
		switch {
		case a.Value.Parameter == 0:
			cases = append(cases, "SET with literal "+LiteralKind(a.Value.Value))
		case argsOK:
			cases = append(cases, "SET with argument "+ArgumentKind(args[a.Value.Parameter-1]))
		}
		if !argsOK {
			continue
		}
		kind, _ := ValueKind(a.Value, args)
		switch {
		case kind == "null":
			null = null || !takesNull(c)
		case columnHolds[c.Type] != kind:
			other = true
		}
	}
	if key {
		cases = append(cases, "SET of the key")
	}
	if len(u.Set) == len(columns) {
		cases = append(cases, "SET of every column")
	}
	if null {
		cases = append(cases, "SET of NULL for a column that does not take it")
	}
	if other {
		cases = append(cases, "SET with a value of another type")
	}
	return cases
}

// KeyAccess of the select s with WHERE of the table of the columns, with the arguments args as for
// [TypeRule], by the roadmap's rule of slice 7 as the README of the engine details it: "no row" if a
// comparison is with NULL or the key is tested with IS NULL, which is never true of a row; "a get of
// one key" for a condition key = value; "a scan from a lower bound", "a scan to an upper bound", or
// "a scan between two bounds" for the tightest bounds of the conditions on the key with <, <=, >,
// and >=, where of two bounds at one value the open one is the tighter, and "a scan between two
// bounds that hold no key" if no key is within them; and otherwise "a scan of every row". It is ""
// for a select that SQLite or s4ql refuses: for a column that the table does not have, another
// number of arguments than parameters, or a value of another type than its column, as
// [ConditionRule] gives it, and for a select with a scalar subquery of slice 15, whose access depends
// on the value that the subquery gives.
func KeyAccess(columns []TableColumn, s *stmt.Select, args []any) string {
	if _, missing := MissingColumn(columns, s); missing || len(args) != s.Parameters() || ConditionRule("t", columns, s, args) != "" || s.Subqueries != nil {
		return ""
	}
	type bound struct {
		value any
		open  bool
	}
	// tighter of the bound a, which may be nil, and b, upper bounds if upper.
	tighter := func(a *bound, b bound, upper bool) *bound {
		if a == nil {
			return &b
		}
		n := compareValues(b.value, a.value)
		if upper {
			n = -n
		}
		if n > 0 || n == 0 && b.open {
			return &b
		}
		return a
	}
	var lower, upper *bound
	get := false
	for _, c := range s.Where {
		isNull := c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull
		if !isNull && termValue(c.Value, args) == nil {
			return "no row"
		}
		if !columns[ColumnOf(columns, c.Column)].PrimaryKey {
			continue
		}
		v := termValue(c.Value, args)
		switch c.Operator {
		case stmt.OpIsNull:
			return "no row"
		case stmt.OpEqual:
			get = true
		case stmt.OpLess, stmt.OpLessOrEqual:
			upper = tighter(upper, bound{value: v, open: c.Operator == stmt.OpLess}, true)
		case stmt.OpGreater, stmt.OpGreaterOrEqual:
			lower = tighter(lower, bound{value: v, open: c.Operator == stmt.OpGreater}, false)
		}
	}
	switch {
	case get:
		return "a get of one key"
	case lower != nil && upper != nil:
		if n := compareValues(lower.value, upper.value); n > 0 || n == 0 && (lower.open || upper.open) {
			return "a scan between two bounds that hold no key"
		}
		return "a scan between two bounds"
	case lower != nil:
		return "a scan from a lower bound"
	case upper != nil:
		return "a scan to an upper bound"
	}
	return "a scan of every row"
}

// termValue of a term of a condition, a literal or the argument of args for its parameter, as SQLite
// binds it: an int64, a float64, a string, a []byte, or nil for NULL and for the zero term of a test
// for NULL.
func termValue(term stmt.Term, args []any) any {
	if term.Parameter > 0 {
		return args[term.Parameter-1]
	}
	switch v := term.Value.(type) {
	case stmt.Integer:
		return int64(v)
	case stmt.Real:
		return float64(v)
	case stmt.Text:
		return string(v)
	case stmt.Blob:
		return []byte(v)
	}
	return nil
}

// sameValue reports whether a and b, values of [termValue] and not NULL, are the same value as SQLite
// compares them: an integer and a real, which a column of INT takes both, by their exact value, and
// values of two other kinds never.
func sameValue(a, b any) bool {
	x, isInt := a.(int64)
	f, isReal := b.(float64)
	if !isInt || !isReal {
		x, isInt = b.(int64)
		f, isReal = a.(float64)
	}
	switch {
	case isInt && isReal:
		return !math.IsNaN(f) && new(big.Float).SetInt64(x).Cmp(big.NewFloat(f)) == 0
	case reflect.TypeOf(a) != reflect.TypeOf(b):
		return false
	}
	return compareValues(a, b) == 0
}

// compareValues a and b, of one kind of [termValue] and not NULL, as SQLite compares them: numbers by
// their value, so that 0.0 is -0.0, and text and blobs by their bytes. It panics for values of two
// kinds.
func compareValues(a, b any) int {
	switch a := a.(type) {
	case int64:
		return cmp.Compare(a, b.(int64))
	case float64:
		return cmp.Compare(a, b.(float64))
	case string:
		return strings.Compare(a, b.(string))
	case []byte:
		return bytes.Compare(a, b.([]byte))
	}
	panic(fmt.Sprintf("stmttest: a comparison of %T with %T", a, b))
}

// TableIndex of a table as SQLite has it: its name as it was created, and its columns, each the
// position of its column in the table and whether the index holds it in descending order.
type TableIndex struct {
	Name    string
	Columns []IndexKeyColumn
}

// IndexKeyColumn of a [TableIndex].
type IndexKeyColumn struct {
	Position   int
	Descending bool
}

// Access of a WHERE to the rows of its table, as [ChosenAccess] gives it.
type Access struct {
	// None reads no row, Get reads the row of one primary key, whose values are Pins, and Full reads
	// every row. Otherwise the access is a scan by the index of the name Index, or by the primary key
	// if Index is "".
	None, Get, Full bool
	Index           string
	// Pins are the values that the leading columns of the key are pinned to, in the order of the key,
	// as [termValue] gives them: nil for a column pinned by IS NULL.
	Pins []any
	// Lower and Upper bound the column of the key after the pinned ones, if they are not nil, and
	// leave the bound itself out if open.
	Lower, Upper         any
	LowerOpen, UpperOpen bool
	// Delivered reports whether the select has an ORDER BY whose order the key of the access delivers,
	// backward if Backward, and Sorted whether it has one whose rows are sorted. A select without
	// ORDER BY has neither.
	Delivered, Backward, Sorted bool
}

// ChosenAccess of the select s with WHERE or ORDER BY of the table of the columns, in the order of
// the table, with the indexes, in the order in which they were created, and with the arguments args as
// for [TypeRule], by the rules of slice 9 as the roadmap states them: the key of an index is its
// columns followed by the columns of the primary key that it does not hold, and a column is pinned by
// = or by IS NULL. A WHERE that pins the whole primary key with = reads its row. Otherwise the index,
// or the primary key, that has the most leading columns of its key pinned wins, and of those with as
// many, one with a range on the next column; a tie goes to the primary key, then to the index created
// first; and when the primary key wins with nothing pinned and nothing bounded, every row is read.
// Before these, as in slice 7, a WHERE that compares a column with NULL, or tests the primary key with
// IS NULL, reads no row.
//
// What the roadmap leaves open is as the builder of slice 9 decided it, in the todo: a column is
// pinned by the first condition in the order of the text that pins it; IS NULL pins only a column
// that takes NULL, since no row has NULL in another; and the bounds of the range are the tightest of
// the conditions with <, <=, >, and >= on the column, where of two at one value the open one is the
// tighter.
//
// With an ORDER BY, the rules of slice 13 as they were agreed: among the candidates that pin as many
// columns, with a LIMIT one that delivers the order wins over one that only has a range, and without a
// LIMIT a range wins over the order, and then the order over none; then the primary key, and then the
// index created first. No row and the row of one primary key deliver every order, forward. A
// candidate delivers the order when, with the columns that the WHERE pins taken out of its key and
// out of the ORDER BY, what is left of the ORDER BY is the start of what is left of the key, every
// column in the direction of the key, forward, or every one in the other, backward; an ORDER BY of
// pinned columns alone is delivered forward. The access is Sorted where it does not deliver the order.
// A LIMIT without ORDER BY changes nothing in the choice.
//
// It reports false for a select that SQLite or s4ql refuses, as [KeyAccess] gives "" for one, also
// for a column of the ORDER BY that the table does not have, and for an argument of the LIMIT that is
// no int64 from 0, which s4ql refuses, and for a select with a scalar subquery of slice 15.
func ChosenAccess(columns []TableColumn, indexes []TableIndex, s *stmt.Select, args []any) (Access, bool) {
	if _, missing := MissingColumn(columns, s); missing || len(args) != s.Parameters() || ConditionRule("t", columns, s, args) != "" || s.Subqueries != nil {
		return Access{}, false
	}
	if s.Limit.Parameter > 0 {
		if n, ok := args[s.Limit.Parameter-1].(int64); !ok || n < 0 {
			return Access{}, false
		}
	}
	ordered := s.OrderBy != nil
	type term struct {
		position int
		op       stmt.Operator
		value    any
	}
	var terms []term
	for _, c := range s.Where {
		terms = append(terms, term{position: ColumnOf(columns, c.Column), op: c.Operator, value: termValue(c.Value, args)})
	}
	for _, t := range terms {
		switch {
		case t.op == stmt.OpIsNull && columns[t.position].PrimaryKey:
			return Access{None: true, Delivered: ordered}, true
		case t.op != stmt.OpIsNull && t.op != stmt.OpIsNotNull && t.value == nil:
			return Access{None: true, Delivered: ordered}, true
		}
	}
	var primary []int
	for i, c := range columns {
		if c.PrimaryKey {
			primary = append(primary, i)
		}
	}
	// pin of the column at the position: the value of the first condition that pins it, and whether
	// one does.
	pin := func(position int, byNull bool) (any, bool) {
		for _, t := range terms {
			switch {
			case t.position != position:
			case t.op == stmt.OpEqual:
				return t.value, true
			case t.op == stmt.OpIsNull && byNull && takesNull(columns[position]):
				return nil, true
			}
		}
		return nil, false
	}
	get := Access{Get: true, Delivered: ordered}
	for _, position := range primary {
		v, ok := pin(position, false)
		if !ok {
			get.Pins = nil
			break
		}
		get.Pins = append(get.Pins, v)
	}
	if get.Pins != nil {
		return get, true
	}
	// tighter reports whether the bound v, open if open, leaves out more values than the bound w,
	// open if wOpen, or w is nil, of upper bounds if upper.
	tighter := func(v any, open bool, w any, wOpen, upper bool) bool {
		if w == nil {
			return true
		}
		n := compareValues(v, w)
		if upper {
			n = -n
		}
		return n > 0 || n == 0 && open && !wOpen
	}
	// pinned reports whether a condition pins the column at the position.
	pinned := func(position int) bool {
		_, ok := pin(position, true)
		return ok
	}
	// delivers reports whether a scan by the key, whose columns hold the positions in the directions
	// of descending, gives the rows in the order of the ORDER BY, and whether it gives them backward.
	delivers := func(key []int, descending []bool) (delivered, backward bool) {
		var restKey []int
		var restDescending []bool
		for i, position := range key {
			if !pinned(position) {
				restKey, restDescending = append(restKey, position), append(restDescending, descending[i])
			}
		}
		var rest []stmt.OrderColumn
		for _, c := range s.OrderBy {
			if !pinned(ColumnOf(columns, c.Name)) {
				rest = append(rest, c)
			}
		}
		if len(rest) > len(restKey) {
			return false, false
		}
		for i, c := range rest {
			turned := c.Descending != restDescending[i]
			switch {
			case ColumnOf(columns, c.Name) != restKey[i]:
				return false, false
			case i == 0:
				backward = turned
			case turned != backward:
				return false, false
			}
		}
		return true, backward
	}
	// fitted is the access of a scan by the key of the index of the name, or of the primary key, whose
	// columns are held in the directions of descending.
	fitted := func(name string, key []int, descending []bool) Access {
		a := Access{Index: name}
		if ordered {
			a.Delivered, a.Backward = delivers(key, descending)
			a.Sorted = !a.Delivered
		}
		for _, position := range key {
			if v, ok := pin(position, true); ok {
				a.Pins = append(a.Pins, v)
				continue
			}
			for _, t := range terms {
				open := t.op == stmt.OpLess || t.op == stmt.OpGreater
				switch {
				case t.position != position:
				case (t.op == stmt.OpLess || t.op == stmt.OpLessOrEqual) && tighter(t.value, open, a.Upper, a.UpperOpen, true):
					a.Upper, a.UpperOpen = t.value, open
				case (t.op == stmt.OpGreater || t.op == stmt.OpGreaterOrEqual) && tighter(t.value, open, a.Lower, a.LowerOpen, false):
					a.Lower, a.LowerOpen = t.value, open
				}
			}
			break
		}
		return a
	}
	// score of an access: the pinned columns count first; then, with ORDER BY and LIMIT, the order and
	// then a range, and otherwise a range and then the order, which only an ORDER BY has.
	score := func(a Access) int {
		ranged, delivered := 0, 0
		if a.Lower != nil || a.Upper != nil {
			ranged = 1
		}
		if a.Delivered {
			delivered = 1
		}
		if s.Limited() {
			return 4*len(a.Pins) + 2*delivered + ranged
		}
		return 4*len(a.Pins) + 2*ranged + delivered
	}
	best := fitted("", primary, make([]bool, len(primary)))
	for _, ix := range indexes {
		var key []int
		var descending []bool
		for _, c := range ix.Columns {
			key, descending = append(key, c.Position), append(descending, c.Descending)
		}
		for _, position := range primary {
			if !slices.Contains(key, position) {
				key, descending = append(key, position), append(descending, false)
			}
		}
		if a := fitted(ix.Name, key, descending); score(a) > score(best) {
			best = a
		}
	}
	if best.Index == "" && len(best.Pins) == 0 && best.Lower == nil && best.Upper == nil {
		return Access{Full: true, Delivered: best.Delivered, Backward: best.Backward, Sorted: best.Sorted}, true
	}
	return best, true
}

// Same reports whether the accesses a and b read the same rows: of the same kind, by the same index,
// as SQLite compares names, with pins and bounds of the same values, as SQLite compares them, so that
// 0.0 is -0.0, and with the same bounds open.
func (a Access) Same(b Access) bool {
	equal := func(x, y any) bool {
		if x == nil || y == nil {
			return x == nil && y == nil
		}
		return reflect.TypeOf(x) == reflect.TypeOf(y) && compareValues(x, y) == 0
	}
	if a.None != b.None || a.Get != b.Get || a.Full != b.Full || nameOf(Quote(a.Index), 0).Fold() != nameOf(Quote(b.Index), 0).Fold() || len(a.Pins) != len(b.Pins) {
		return false
	}
	for i := range a.Pins {
		if !equal(a.Pins[i], b.Pins[i]) {
			return false
		}
	}
	return equal(a.Lower, b.Lower) && equal(a.Upper, b.Upper) && (a.Lower == nil || a.LowerOpen == b.LowerOpen) && (a.Upper == nil || a.UpperOpen == b.UpperOpen) &&
		a.Delivered == b.Delivered && a.Sorted == b.Sorted && (!a.Delivered || a.Backward == b.Backward)
}

// String of the access in words, with its values: "no row", "a get of one key" and the key, "a scan
// of every row", or a scan by the primary key or by an index, with the number of columns that it
// pins and their values, if any, and its bounds, if any, such as `a scan of the index i with 2 pinned
// []interface {}{1, "a"} from a lower bound (3`, where ( and ) are open and [ and ] closed; and for a
// select with ORDER BY ", delivered forward", ", delivered backward", or ", sorted" after that.
func (a Access) String() string {
	var s string
	switch {
	case a.None:
		s = "no row"
	case a.Get:
		s = fmt.Sprintf("a get of one key %#v", a.Pins)
	case a.Full:
		s = "a scan of every row"
	default:
		s = "a scan"
		if a.Index != "" {
			s += " of the index " + a.Index
		}
		if len(a.Pins) > 0 {
			s += fmt.Sprintf(" with %v pinned %#v", len(a.Pins), a.Pins)
		}
		if a.Lower != nil {
			s += fmt.Sprintf(" from a lower bound %v%#v", map[bool]string{true: "(", false: "["}[a.LowerOpen], a.Lower)
		}
		if a.Upper != nil {
			s += fmt.Sprintf(" to an upper bound %#v%v", a.Upper, map[bool]string{true: ")", false: "]"}[a.UpperOpen])
		}
	}
	switch {
	case a.Delivered && a.Backward:
		s += ", delivered backward"
	case a.Delivered:
		s += ", delivered forward"
	case a.Sorted:
		s += ", sorted"
	}
	return s
}

// IndexRule of the create index or the drop index s, on a database whose tables, by the folds of
// their names as [stmt.Name.Fold] gives them, have the columns, and whose indexes are the folds of
// the names in indexes: SQLite's message, or "" if SQLite runs the statement. A create index gives
// `no such table: main.<table>`, then `there is already a table named <index>`, then `index <index>
// already exists`, then `no such column` for the first column in the order of the text that the table
// does not have, with SQLite's question about quotes for a name in double quotes, in the order in
// which SQLite checks them; a drop index gives `no such index: <index>`, also for the name of a
// table. With IF NOT EXISTS and an index of the name, and with IF EXISTS and none, it is "", as
// SQLite does nothing there, which [ExistsRule] says. The names are as the statement writes them,
// without their quotes. The rule does not know the length limit, nor a read-only transaction.
func IndexRule(tables map[string][]TableColumn, indexes map[string]bool, s stmt.Statement) string {
	if ExistsRule(tables, indexes, s) != "" {
		return ""
	}
	switch s := s.(type) {
	case *stmt.CreateIndex:
		columns, ok := tables[s.Table.Fold()]
		switch {
		case !ok:
			return "no such table: main." + s.Table.Value
		case tables[s.Index.Fold()] != nil:
			return "there is already a table named " + s.Index.Value
		case indexes[s.Index.Fold()]:
			return "index " + s.Index.Value + " already exists"
		}
		for _, c := range s.Columns {
			if ColumnOf(columns, c.Name) < 0 {
				if c.Name.Raw[0] == '"' {
					return `no such column: "` + c.Name.Value + `" - should this be a string literal in single-quotes?`
				}
				return "no such column: " + c.Name.Value
			}
		}
	case *stmt.DropIndex:
		if !indexes[s.Index.Fold()] {
			return "no such index: " + s.Index.Value
		}
	}
	return ""
}

// ExistsRule of the statement s, on a database whose tables, by the folds of their names as
// [stmt.Name.Fold] gives them, have the columns, and whose indexes are the folds of the names in
// indexes: why SQLite does nothing for a statement of IF NOT EXISTS or IF EXISTS of slice 10, or ""
// where it does what the plain statement does, with its errors, and for every statement without the
// clause. A table and an index share their names.
//
//   - A create table with IF NOT EXISTS does nothing where a table has its name: "a table of the
//     name exists", whatever its columns are. Where an index has the name, SQLite gives the plain
//     statement's error.
//   - A drop table with IF EXISTS does nothing where no table has its name: "no table of the name
//     exists", or "no table of the name exists, and an index has it".
//   - A create index with IF NOT EXISTS does nothing where its table exists, no table has the name of
//     the index, and an index has it, on any table: "an index of the name exists", or "an index of
//     the name exists, and the table lacks a column of the statement", as SQLite looks for the
//     columns only after the index. Without its table, and with a table of the name of the index,
//     SQLite gives the errors of the plain statement, which [IndexRule] gives, first.
//   - A drop index with IF EXISTS does nothing where no index has its name: "no index of the name
//     exists", or "no index of the name exists, and a table has it".
//
// SQLite stops a statement that does nothing before the length limit and before it writes, so the
// rule needs neither to know the limit nor whether the transaction is read-only.
func ExistsRule(tables map[string][]TableColumn, indexes map[string]bool, s stmt.Statement) string {
	switch s := s.(type) {
	case *stmt.CreateTable:
		if s.IfNotExists && tables[s.Table.Fold()] != nil {
			return "a table of the name exists"
		}
	case *stmt.DropTable:
		switch {
		case !s.IfExists || tables[s.Table.Fold()] != nil:
		case indexes[s.Table.Fold()]:
			return "no table of the name exists, and an index has it"
		default:
			return "no table of the name exists"
		}
	case *stmt.CreateIndex:
		columns, ok := tables[s.Table.Fold()]
		if !s.IfNotExists || !ok || tables[s.Index.Fold()] != nil || !indexes[s.Index.Fold()] {
			return ""
		}
		if slices.ContainsFunc(s.Columns, func(c stmt.IndexColumn) bool { return ColumnOf(columns, c.Name) < 0 }) {
			return "an index of the name exists, and the table lacks a column of the statement"
		}
		return "an index of the name exists"
	case *stmt.DropIndex:
		switch {
		case !s.IfExists || indexes[s.Index.Fold()]:
		case tables[s.Index.Fold()] != nil:
			return "no index of the name exists, and a table has it"
		default:
			return "no index of the name exists"
		}
	}
	return ""
}
