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
