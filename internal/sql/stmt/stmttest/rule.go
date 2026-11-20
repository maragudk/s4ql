package stmttest

import (
	"bytes"
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/stmt"
)

// TableColumn of a table as SQLite has it: its name as the table stores it, its declared type, such
// as INT, whether it is the primary key, and whether it takes NULL, which the primary key never
// does.
type TableColumn struct {
	Name       string
	Type       string
	PrimaryKey bool
	Nullable   bool
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
// of values, and for NULL, given or for a column that the list leaves out, in a column that does not
// take it, which gets SQLite's `NOT NULL constraint failed` before SQLite looks at a type.
func TypeRule(table string, columns []TableColumn, insert *stmt.Insert, args []any) string {
	positions, ok := Positions(columns, insert)
	if !ok || len(args) != insert.Parameters() {
		return ""
	}
	values := valuesOf(columns, positions)
	for position, i := range values {
		if c := columns[position]; !takesNull(c) && (i < 0 || isNull(insert.Values[i], args)) {
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
// later such test replaces it, and the next comparison ends the walk with it.
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

// FoundQuery of the conditions of a WHERE on the table: a select that gives a row if the conditions
// hold for a row of the table, and the arguments of its parameters, each as SQLite binds it, from the
// literals of the conditions and from args for their parameters, as the statement of the conditions
// numbers them.
func FoundQuery(table stmt.Name, where []stmt.Condition, args []any) (string, []any) {
	var conditions []string
	var bound []any
	for _, c := range where {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
			conditions = append(conditions, Quote(c.Column.Value)+" "+c.Operator.String())
			continue
		}
		conditions = append(conditions, Quote(c.Column.Value)+" "+c.Operator.String()+" ?")
		switch v := c.Value.Value.(type) {
		case stmt.Integer:
			bound = append(bound, int64(v))
		case stmt.Real:
			bound = append(bound, float64(v))
		case stmt.Text:
			bound = append(bound, string(v))
		case stmt.Blob:
			bound = append(bound, []byte(v))
		case stmt.Null:
			bound = append(bound, nil)
		default:
			bound = append(bound, args[c.Value.Parameter-1])
		}
	}
	src := "select 1 from " + Quote(table.Value)
	if len(conditions) > 0 {
		src += " where " + strings.Join(conditions, " and ")
	}
	return src + " limit 1", bound
}

// WhereRule of the conditions of a WHERE of the table of the name and the columns, which the table
// has, with the arguments args of the statement, by the numbers of the parameters: s4ql's "not
// supported" for the first condition, in the order of the text, whose value is of another type than
// its column, as [ConditionRule] gives it, or "".
func WhereRule(table string, columns []TableColumn, where []stmt.Condition, args []any) string {
	for _, c := range where {
		if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull {
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
// number of values"; each column that the list leaves out and that does not take NULL, "a column
// without a value"; each NULL that the insert gives for the key, "NULL in the key", or for another
// column that does not take it, "NULL in a column that does not take it"; and each value of another
// type than its column, "a value of another type in the key" or "a value of another type in another
// column", which NULL never is. A column that takes NULL and that the list leaves out gets NULL,
// which is no fault. A wrong number of values, and a name that the table does not have, leave out
// the faults of the values.
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
		if c := columns[position]; i < 0 && !takesNull(c) {
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
// list leaves one out; and "insert of NULL beside a value of another type" if it gives NULL and a
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
		if i < 0 && takesNull(columns[position]) {
			cases = append(cases, "insert that leaves out a column that takes NULL")
			break
		}
	}
	if given && slices.ContainsFunc(Faults(columns, insert, args), func(f string) bool { return strings.HasPrefix(f, "a value of another type") }) {
		cases = append(cases, "insert of NULL beside a value of another type")
	}
	return cases
}

// WhereCases of the select s with WHERE of the table of the columns, in the order of the table, with
// the arguments args as for [TypeRule], for a count of what a test reaches of slice 7. For a column of
// a condition that the table does not have, where the select list has every column that it names:
// "WHERE with a column that the table does not have", in a comparison or under IS NULL or IS NOT
// NULL, and alone, before other conditions, or after them, and "WHERE with a column in double quotes
// that the table does not have" for one in double quotes; and nothing more, since SQLite gives its
// error first. Otherwise, with as many arguments as parameters: the operator of each condition on
// the key or on another column, such as "<= on the key"; the value of each comparison, a literal by
// its kind, as [LiteralKind] names it, or an argument, as [ArgumentKind] names it; "WHERE with a
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
			if c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull || a != nil && b != nil && compareValues(a, b) == 0 {
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

// KeyAccess of the select s with WHERE of the table of the columns, with the arguments args as for
// [TypeRule], by the roadmap's rule of slice 7 as the README of the engine details it: "no row" if a
// comparison is with NULL or the key is tested with IS NULL, which is never true of a row; "a get of
// one key" for a condition key = value; "a scan from a lower bound", "a scan to an upper bound", or
// "a scan between two bounds" for the tightest bounds of the conditions on the key with <, <=, >,
// and >=, where of two bounds at one value the open one is the tighter, and "a scan between two
// bounds that hold no key" if no key is within them; and otherwise "a scan of every row". It is ""
// for a select that SQLite or s4ql refuses: for a column that the table does not have, another
// number of arguments than parameters, or a value of another type than its column, as
// [ConditionRule] gives it.
func KeyAccess(columns []TableColumn, s *stmt.Select, args []any) string {
	if _, missing := MissingColumn(columns, s); missing || len(args) != s.Parameters() || ConditionRule("t", columns, s, args) != "" {
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
