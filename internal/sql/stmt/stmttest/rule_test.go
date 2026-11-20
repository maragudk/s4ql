package stmttest_test

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

func TestTypeRule(t *testing.T) {
	// The table t (a text, k int primary key, b real not null, c blob), as SQLite has it.
	columns := []stmttest.TableColumn{
		{Name: "a", Type: "TEXT", Nullable: true},
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "b", Type: "REAL"},
		{Name: "c", Type: "BLOB", Nullable: true},
	}
	cases := []struct {
		src  string
		args []any
		want string
		// faults as [stmttest.Faults] gives them.
		faults string
	}{
		{src: "insert into t values ('x', 1, 1.5, x'01')", faults: "[]"},
		{src: "insert into t values (null, 1, 1.5, NULL)", faults: "[]"},
		{src: "insert into t values (?, 1, 1.5, ?)", args: []any{nil, nil}, faults: "[]"},
		{src: "insert into t (k, b) values (1, 1.5)", faults: "[]"},
		// NULL has no type, so a value of another type in another column is the rule's.
		{src: "insert into t values (null, 1, 1, null)", want: "s4ql: not supported: an integer literal for the column t.b of type REAL", faults: "[a value of another type in another column]"},
		{src: "insert into t (k, b, a) values (1, 1.5, ?)", args: []any{int64(1)}, want: "s4ql: not supported: argument 1, which is an integer, for the column t.a of type TEXT", faults: "[a value of another type in another column]"},
		{src: "insert into t (k, b) values ('x', 1.5)", want: "s4ql: not supported: a string literal for the column t.k of type INT", faults: "[a value of another type in the key]"},
		// NULL, given or for a column left out, in the key or a column that does not take it: SQLite's
		// NOT NULL comes first, also before a value of another type.
		{src: "insert into t values ('x', null, 1.5, null)", faults: "[NULL in the key]"},
		{src: "insert into t values ('x', ?, 1.5, null)", args: []any{nil}, faults: "[NULL in the key]"},
		{src: "insert into t values ('x', 1, ?, null)", args: []any{nil}, faults: "[NULL in a column that does not take it]"},
		{src: "insert into t (a, b) values ('x', 1.5)", faults: "[a column without a value]"},
		{src: "insert into t (k, a) values (1, 'x')", faults: "[a column without a value]"},
		{src: "insert into t values (1, 1, null, null)", faults: "[NULL in a column that does not take it a value of another type in another column]"},
		{src: "insert into t (k, a, c) values (null, 1, 'y')", faults: "[a column without a value NULL in the key a value of another type in another column a value of another type in another column]"},
		// SQLite's errors of the names and the number of values come first.
		{src: "insert into t (x) values (null)", faults: "[a column that the table does not have]"},
		{src: "insert into t values (null)", faults: "[the wrong number of values]"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			insert := s.(*stmt.Insert)
			is.Equal(t, c.want, stmttest.TypeRule("t", columns, insert, c.args))
			is.Equal(t, c.faults, fmt.Sprint(stmttest.Faults(columns, insert, c.args)))
		})
	}

	t.Run("should give the cases of NULL that an insert reaches", func(t *testing.T) {
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "insert into t values ('x', 1, 1.5, x'01')", want: "[]"},
			{src: "insert into t values (null, 1, 1.5, ?)", args: []any{nil}, want: "[NULL literal into a column that takes NULL NULL argument into a column that takes NULL]"},
			{src: "insert into t (k, b) values (1, 1.5)", want: "[insert that leaves out a column that takes NULL]"},
			{src: "insert into t (b, k, c) values (?, null, x'01')", args: []any{nil}, want: "[NULL argument into a column that does not take NULL NULL literal into the key insert that leaves out a column that takes NULL]"},
			{src: "insert into t values (null, 1, 1, x'01')", want: "[NULL literal into a column that takes NULL insert of NULL beside a value of another type]"},
			{src: "insert into t values (1, 1, null, x'01')", want: "[NULL literal into a column that does not take NULL insert of NULL beside a value of another type]"},
			// SQLite's errors of the names and the number of values come first, and so does the error of
			// the number of arguments.
			{src: "insert into t (x) values (null)", want: "[]"},
			{src: "insert into t values (null)", want: "[]"},
			{src: "insert into t values (?, 1, 1.5, null)", want: "[]"},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			is.Equal(t, c.want, fmt.Sprint(stmttest.NullCases(columns, s.(*stmt.Insert), c.args)), c.src)
		}
	})

	t.Run("should take NULL for no other type than any column, as a literal and as a nil argument", func(t *testing.T) {
		s, err := stmt.Check("insert into t values (null, ?)")
		is.NotError(t, err)
		insert := s.(*stmt.Insert)
		kind, _ := stmttest.ValueKind(insert.Values[0], nil)
		is.Equal(t, "null", kind)
		kind, _ = stmttest.ValueKind(insert.Values[1], []any{nil})
		is.Equal(t, "null", kind)
		kind, _ = stmttest.ValueKind(insert.Values[1], []any{[]byte(nil)})
		is.Equal(t, "blob", kind)
	})
}

func TestConditionRule(t *testing.T) {
	// The table t (a text, k int primary key, b real not null, c blob), as SQLite has it.
	columns := []stmttest.TableColumn{
		{Name: "a", Type: "TEXT", Nullable: true},
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "b", Type: "REAL"},
		{Name: "c", Type: "BLOB", Nullable: true},
	}
	cases := []struct {
		src     string
		args    []any
		want    string
		missing string
	}{
		{src: "select * from t where a = 'x' and k > 1 and b <= 1.5 and c != x'01'"},
		{src: "select * from t where a is null and k = ? and c = ?", args: []any{int64(1), nil}},
		{src: "select * from t where a = null and k = null and b = null"},
		// The first condition of another type, in the order of the text.
		{src: "select * from t where K = 1.0 and a = 1", want: "s4ql: not supported: a real literal compared with the column t.k of type INT"},
		{src: "select * from t where a is null and b > 1 and k = 'x'", want: "s4ql: not supported: an integer literal compared with the column t.b of type REAL"},
		{src: "select * from t where k = 1 and c = ? and a = ?", args: []any{"x", 1.5}, want: "s4ql: not supported: argument 1, which is text, compared with the column t.c of type BLOB"},
		// SQLite's errors of the names come first, and another number of arguments.
		{src: "select x from t where k = 'x'", missing: "x"},
		{src: "select * from t where k = 'x' and x = 1", missing: "x"},
		{src: "select * from t where x is null and y is null and k = 'x'", missing: "y"},
		{src: "select * from t where x is null and k = 'x' and y = 1", missing: "x"},
		{src: "select a, y from t where x = 1", missing: "y"},
		{src: "select * from t where k = ?", args: []any{"x", "y"}},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			sel := s.(*stmt.Select)
			is.Equal(t, c.want, stmttest.ConditionRule("t", columns, sel, c.args))
			name, missing := stmttest.MissingColumn(columns, sel)
			is.Equal(t, c.missing != "", missing)
			is.Equal(t, c.missing, name.Value)
		})
	}
}

func TestWhereCases(t *testing.T) {
	// The table t (a text, k int primary key, b real not null), as SQLite has it.
	columns := []stmttest.TableColumn{
		{Name: "a", Type: "TEXT", Nullable: true},
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "b", Type: "REAL"},
	}
	cases := []struct {
		src  string
		args []any
		// want is what [stmttest.WhereCases] gives, and access what [stmttest.KeyAccess] gives.
		want, access string
	}{
		{src: "select * from t"},
		{src: "select * from t where k = 1 and a < 'x'", want: "[= on the key comparison with literal integer < on another column comparison with literal text WHERE that reads a get of one key]", access: "a get of one key"},
		{src: "select * from t where k == ? and k > 2", args: []any{int64(1)}, want: "[= on the key comparison with argument int64 > on the key comparison with literal integer WHERE that tests a column more than once WHERE that reads a get of one key]", access: "a get of one key"},
		{src: "select * from t where k >= 1", want: "[>= on the key comparison with literal integer WHERE that reads a scan from a lower bound]", access: "a scan from a lower bound"},
		{src: "select * from t where k < ? and b > -0.0", args: []any{int64(1)}, want: "[< on the key comparison with argument int64 > on another column comparison with literal real -0.0 WHERE that reads a scan to an upper bound]", access: "a scan to an upper bound"},
		{src: "select * from t where k > 1 and k <= 5 and k > 3", want: "[> on the key comparison with literal integer <= on the key comparison with literal integer > on the key comparison with literal integer WHERE that tests a column more than once WHERE that reads a scan between two bounds]", access: "a scan between two bounds"},
		// Two bounds at one value: the open one is the tighter, and with it no key is within them. > and
		// <= with one value contradict each other.
		{src: "select * from t where k >= 3 and k > 3 and k <= 3", want: "[>= on the key comparison with literal integer > on the key comparison with literal integer <= on the key comparison with literal integer WHERE that tests a column more than once WHERE with contradicting conditions WHERE that reads a scan between two bounds that hold no key]", access: "a scan between two bounds that hold no key"},
		{src: "select * from t where k <= 3 and k < 3 and k >= 3", want: "[<= on the key comparison with literal integer < on the key comparison with literal integer >= on the key comparison with literal integer WHERE that tests a column more than once WHERE with contradicting conditions WHERE that reads a scan between two bounds that hold no key]", access: "a scan between two bounds that hold no key"},
		{src: "select * from t where k > 4 and k < 2", want: "[> on the key comparison with literal integer < on the key comparison with literal integer WHERE that tests a column more than once WHERE that reads a scan between two bounds that hold no key]", access: "a scan between two bounds that hold no key"},
		{src: "select * from t where k >= 3 and k <= 3", want: "[>= on the key comparison with literal integer <= on the key comparison with literal integer WHERE that tests a column more than once WHERE that reads a scan between two bounds]", access: "a scan between two bounds"},
		{src: "select * from t where k < 3 and k >= 3", want: "[< on the key comparison with literal integer >= on the key comparison with literal integer WHERE that tests a column more than once WHERE with contradicting conditions WHERE that reads a scan between two bounds that hold no key]", access: "a scan between two bounds that hold no key"},
		{src: "select * from t where a is null and a is not null", want: "[IS NULL on another column IS NOT NULL on another column WHERE that tests a column more than once WHERE with contradicting conditions WHERE that reads a scan of every row]", access: "a scan of every row"},
		{src: "select * from t where b != 1.5 and k is not null", want: "[!= on another column comparison with literal real IS NOT NULL on the key WHERE that reads a scan of every row]", access: "a scan of every row"},
		// A comparison with NULL, and IS NULL of the key, are never true.
		{src: "select * from t where k > 1 and a = ?", args: []any{nil}, want: "[> on the key comparison with literal integer = on another column comparison with argument nil WHERE that reads no row]", access: "no row"},
		{src: "select * from t where b = null and b != null", want: "[= on another column comparison with literal null != on another column comparison with literal null WHERE that tests a column more than once WHERE that reads no row]", access: "no row"},
		{src: "select * from t where k is null", want: "[IS NULL on the key WHERE that reads no row]", access: "no row"},
		// A value of another type, and the columns that the table does not have.
		{src: "select * from t where k = 1 and b = 1", want: "[= on the key comparison with literal integer = on another column comparison with literal integer WHERE with a value of another type]"},
		{src: "select * from t where x = 1", want: "[WHERE with a column that the table does not have in a comparison, alone]"},
		{src: `select * from t where "x" is null and k = 1`, want: "[WHERE with a column that the table does not have under IS NULL or IS NOT NULL, before other conditions WHERE with a column in double quotes that the table does not have]"},
		{src: "select * from t where k = 1 and x = 1", want: "[WHERE with a column that the table does not have in a comparison, after other conditions]"},
		// SQLite's error of the select list comes first, and another number of arguments.
		{src: "select x from t where y = 1"},
		{src: "select * from t where k = ?"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			sel := s.(*stmt.Select)
			want := c.want
			if want == "" {
				want = "[]"
			}
			is.Equal(t, want, fmt.Sprint(stmttest.WhereCases(columns, sel, c.args)))
			if sel.Where != nil {
				is.Equal(t, c.access, stmttest.KeyAccess(columns, sel, c.args))
			}
		})
	}
}
