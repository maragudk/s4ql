package stmttest_test

import (
	"fmt"
	"slices"
	"strings"
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
		// A value of another type in a column after the key, where the value of the key has the
		// type of the key: the rule looks at every column, not at the key only.
		{src: "insert into t values ('x', 2, 'c', null)", want: "s4ql: not supported: a string literal for the column t.b of type REAL", faults: "[a value of another type in another column]"},
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

func TestDefaultCases(t *testing.T) {
	// The table t (k int primary key default 7, a text default 'x', b real not null default -0.0,
	// c blob default null, d int not null default NULL, e text not null default '<long>', f int), as
	// SQLite has it.
	long := "'" + strings.Repeat("l", stmttest.LongDefault) + "'"
	columns := []stmttest.TableColumn{
		{Name: "k", Type: "INT", PrimaryKey: true, Default: "7"},
		{Name: "a", Type: "TEXT", Nullable: true, Default: "'x'"},
		{Name: "b", Type: "REAL", Default: "-0.0"},
		{Name: "c", Type: "BLOB", Nullable: true, Default: "null"},
		{Name: "d", Type: "INT", Default: "NULL"},
		{Name: "e", Type: "TEXT", Default: long},
		{Name: "f", Type: "INT", Nullable: true},
	}
	for _, c := range []struct {
		src    string
		args   []any
		rule   string
		faults string
		cases  string
		nulls  string
	}{
		{src: "insert into t (d) values (1)", faults: "[]",
			cases: "[insert that leaves out the key with a default insert that leaves out a column with a default of text insert that leaves out a column with a default of real insert that leaves out a column that takes NULL with DEFAULT NULL insert that leaves out a column with a default of text insert that leaves out a column with a long default]",
			nulls: "[insert that leaves out a column that takes NULL]"},
		{src: "insert into t (k, a, b, c, d, e, f) values (1, null, ?, null, 1, 'e', 2)", args: []any{nil}, faults: "[NULL in a column that does not take it]",
			cases: "[NULL given for a column with a default, which takes NULL NULL given for a column with a default, which does not take NULL]",
			nulls: "[NULL literal into a column that takes NULL NULL argument into a column that does not take NULL NULL literal into a column that takes NULL]"},
		// A default NULL in a column that does not take NULL is SQLite's NOT NULL, before a value of
		// another type.
		{src: "insert into t (k, f) values (1, 'x')", faults: "[a column without a value a value of another type in another column]",
			cases: "[insert that leaves out a column with a default of text insert that leaves out a column with a default of real insert that leaves out a column that takes NULL with DEFAULT NULL insert that leaves out a column that does not take NULL with DEFAULT NULL insert that leaves out a column with a default of text insert that leaves out a column with a long default]",
			nulls: "[insert that leaves out a column that takes NULL]"},
		// A default of the type of its column is never refused for its type.
		{src: "insert into t (d, f) values (1, 'x')", rule: "s4ql: not supported: a string literal for the column t.f of type INT", faults: "[a value of another type in another column]",
			cases: "[insert that leaves out the key with a default insert that leaves out a column with a default of text insert that leaves out a column with a default of real insert that leaves out a column that takes NULL with DEFAULT NULL insert that leaves out a column with a default of text insert that leaves out a column with a long default]",
			nulls: "[insert that leaves out a column that takes NULL]"},
		{src: "insert into t (k, d, f) values (?, 1, 2)", args: []any{nil}, faults: "[NULL in the key]",
			cases: "[NULL given for a column with a default, which does not take NULL insert that leaves out a column with a default of text insert that leaves out a column with a default of real insert that leaves out a column that takes NULL with DEFAULT NULL insert that leaves out a column with a default of text insert that leaves out a column with a long default]",
			nulls: "[NULL argument into the key insert that leaves out a column that takes NULL]"},
		{src: "insert into t values (1, 'a', 1.5, x'00', 2, 'e', 3)", faults: "[]", cases: "[]", nulls: "[]"},
		// SQLite's errors of the names and the number of values come first, and so does the error of
		// the number of arguments.
		{src: "insert into t (x) values (1)", faults: "[a column that the table does not have]", cases: "[]", nulls: "[]"},
		{src: "insert into t (d) values (1, 2)", faults: "[the wrong number of values]", cases: "[]", nulls: "[]"},
		{src: "insert into t (d) values (?)", faults: "[]", cases: "[]", nulls: "[]"},
	} {
		s, err := stmt.Check(c.src)
		is.NotError(t, err)
		insert := s.(*stmt.Insert)
		is.Equal(t, c.rule, stmttest.TypeRule("t", columns, insert, c.args), c.src)
		is.Equal(t, c.faults, fmt.Sprint(stmttest.Faults(columns, insert, c.args)), c.src)
		is.Equal(t, c.cases, fmt.Sprint(stmttest.DefaultCases(columns, insert, c.args)), c.src)
		is.Equal(t, c.nulls, fmt.Sprint(stmttest.NullCases(columns, insert, c.args)), c.src)
	}

	t.Run("should call a default long from LongDefault bytes of its literal", func(t *testing.T) {
		s, err := stmt.Check("insert into t (k) values (1)")
		is.NotError(t, err)
		for _, n := range []int{stmttest.LongDefault - 1, stmttest.LongDefault} {
			columns := []stmttest.TableColumn{{Name: "k", Type: "INT", PrimaryKey: true}, {Name: "v", Type: "BLOB", Nullable: true, Default: "x'" + strings.Repeat("0", n-3) + "'"}}
			cases := stmttest.DefaultCases(columns, s.(*stmt.Insert), nil)
			is.Equal(t, n >= stmttest.LongDefault, slices.Contains(cases, "insert that leaves out a column with a long default"), fmt.Sprint(n))
		}
	})
}

func TestLeavesOutIndexed(t *testing.T) {
	// The table t (k int primary key, a text default 'x', b real, c blob default null), as SQLite has
	// it, with the indexes i on (b, a) and j on (c).
	columns := []stmttest.TableColumn{
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "a", Type: "TEXT", Nullable: true, Default: "'x'"},
		{Name: "b", Type: "REAL", Nullable: true},
		{Name: "c", Type: "BLOB", Nullable: true, Default: "null"},
	}
	i := stmttest.TableIndex{Name: "i", Columns: []stmttest.IndexKeyColumn{{Position: 2}, {Position: 1, Descending: true}}}
	j := stmttest.TableIndex{Name: "j", Columns: []stmttest.IndexKeyColumn{{Position: 3}}}
	tests := []struct {
		name    string
		src     string
		indexes []stmttest.TableIndex
		want    bool
	}{
		{name: "should report a column with a default that the insert leaves out and an index holds", src: "insert into t (k, b) values (1, 2.5)", indexes: []stmttest.TableIndex{i}, want: true},
		{name: "should report DEFAULT NULL as a default", src: "insert into t (k, a) values (1, 'y')", indexes: []stmttest.TableIndex{i, j}, want: true},
		{name: "should not report a column with a default that the insert gives", src: "insert into t (k, a) values (1, 'y')", indexes: []stmttest.TableIndex{i}},
		{name: "should not report a column with a default that no index holds", src: "insert into t (k, b, c) values (1, 2.5, x'00')", indexes: []stmttest.TableIndex{j}},
		{name: "should not report a column without a default that an index holds", src: "insert into t (k, a, c) values (1, 'y', x'00')", indexes: []stmttest.TableIndex{i, j}},
		{name: "should not report a table without indexes", src: "insert into t (k) values (1)"},
		{name: "should not report a position of an index that is no column", src: "insert into t (k) values (1)",
			indexes: []stmttest.TableIndex{{Name: "x", Columns: []stmttest.IndexKeyColumn{{Position: -1}, {Position: -2}, {Position: 4}}}}},
		{name: "should not report an insert without a list, which leaves out nothing", src: "insert into t values (1, 'y', 2.5, x'00')", indexes: []stmttest.TableIndex{i, j}},
		{name: "should not report an insert that SQLite refuses for its names", src: "insert into t (k, z) values (1, 2)", indexes: []stmttest.TableIndex{i, j}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, err := stmt.Check(test.src)
			is.NotError(t, err)
			is.Equal(t, test.want, stmttest.LeavesOutIndexed(columns, test.indexes, s.(*stmt.Insert)))
		})
	}
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

func TestHasExpression(t *testing.T) {
	columns := []stmttest.TableColumn{
		{Name: "a", Type: "TEXT", Nullable: true},
		{Name: "k", Type: "INT", PrimaryKey: true},
	}
	for src, want := range map[string]bool{
		"select * from t where k = a":                                 true,
		"select * from t where k + 1 = 2":                             true,
		"select * from t where 2 = -k":                                true,
		"select * from t where k = 1 + ?":                             true,
		"select * from t where k = (select k from t where k * 2 = 2)": true,
		"update t set k = a":                                          true,
		"update t set a = 'x' where k - 1 = 0":                        true,
		"delete from t where k / 2 = 1":                               true,
		"select * from t where k = 1 and a is null":                   false,
		"select * from t where k = (select k from t where a = 'x')":   false,
		"update t set a = 'x' where k = ?":                            false,
		"delete from t where k = 1":                                   false,
		"insert into t values ('x', 1)":                               false,
	} {
		t.Run(src, func(t *testing.T) {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, want, stmttest.HasExpression(s))
			sel, ok := s.(*stmt.Select)
			if !want || !ok || sel.Subqueries != nil {
				// The expression of a subquery is in the WHERE of the subquery.
				return
			}
			// The rules give no model for one, and panic, with a message that names it, where a caller
			// passes one, as a fuzz test of the driver check did with `where v1 = s1`.
			_, chosen := stmttest.ChosenAccess(columns, nil, sel, []any{int64(1)}[:sel.Parameters()])
			is.True(t, !chosen)
			is.Equal(t, "", stmttest.KeyAccess(columns, sel, []any{int64(1)}[:sel.Parameters()]))
			defer func() {
				is.True(t, strings.Contains(fmt.Sprint(recover()), "see HasExpression"))
			}()
			stmttest.WhereRule("t", columns, sel.Where, []any{int64(1)}[:sel.Parameters()])
		})
	}
}

func TestUpdateRule(t *testing.T) {
	// The table t (a text, k int primary key, b real not null, c blob), as SQLite has it.
	columns := []stmttest.TableColumn{
		{Name: "a", Type: "TEXT", Nullable: true},
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "b", Type: "REAL"},
		{Name: "c", Type: "BLOB", Nullable: true},
	}
	cases := []struct {
		src   string
		args  []any
		found bool
		want  string
		// asked reports whether the rule asks whether the update finds a row.
		asked bool
	}{
		{src: "update t set a = 'x', b = 1.5, c = ?, k = 1 where k > 1", args: []any{nil}},
		{src: "update t set b = 1", want: "s4ql: not supported: an integer literal for the column t.b of type REAL"},
		// The first column of SET in the order of the table, and the conditions first.
		{src: "update t set c = 'x', a = 1", want: "s4ql: not supported: an integer literal for the column t.a of type TEXT"},
		{src: "update t set a = ? where k = ?", args: []any{int64(1), "x"}, want: "s4ql: not supported: argument 2, which is text, compared with the column t.k of type INT"},
		{src: "update t set a = ? where k = ?", args: []any{int64(1), int64(2)}, want: "s4ql: not supported: argument 1, which is an integer, for the column t.a of type TEXT"},
		// NULL in a column that does not take it comes first, if the update finds a row.
		{src: "update t set b = null, a = 1 where k = 1", found: true, asked: true},
		{src: "update t set b = null, a = 1 where k = 1", asked: true, want: "s4ql: not supported: an integer literal for the column t.a of type TEXT"},
		{src: "update t set k = ?, a = 1", args: []any{nil}, found: true, asked: true},
		{src: "update t set a = null, c = 1", want: "s4ql: not supported: an integer literal for the column t.c of type BLOB"},
		{src: "update t set b = null where k = 'x'", want: "s4ql: not supported: a string literal compared with the column t.k of type INT"},
		// SQLite's errors of the names come first, those of SET before those of WHERE, and another
		// number of arguments.
		{src: "update t set x = 1, a = 1"},
		{src: "update t set a = 1 where x = 1"},
		{src: "update t set a = 1 where k = 'x' and x = 1"},
		{src: "update t set a = ?", args: []any{int64(1), int64(2)}},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			var asked bool
			is.Equal(t, c.want, stmttest.UpdateRule("t", columns, s.(*stmt.Update), c.args, func() bool { asked = true; return c.found }))
			is.Equal(t, c.asked, asked)
		})
	}

	t.Run("should give the rule of a delete as of a select with its WHERE", func(t *testing.T) {
		for src, want := range map[string]string{
			"delete from t":                         "",
			"delete from t where k = 1 and a = 1":   "s4ql: not supported: an integer literal compared with the column t.a of type TEXT",
			"delete from t where k = 1.5 and x = 1": "",
		} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, want, stmttest.DeleteRule("t", columns, s.(*stmt.Delete), nil), src)
		}
	})

	t.Run("should give a query of the conditions, with the literals and the arguments bound in order", func(t *testing.T) {
		s, err := stmt.Check(`update t set a = ? where k >= ? and "A""b" is not null and b < 1.5 and c = x'01' and a != 'x' and a = null and k <= ?`)
		is.NotError(t, err)
		u := s.(*stmt.Update)
		src, bound := stmttest.FoundQuery(u.Table, u.Where, nil, []any{"s", int64(1), int64(9)})
		is.Equal(t, `select 1 from "t" where "k" >= ? and "A""b" IS NOT NULL and "b" < ? and "c" = ? and "a" != ? and "a" = ? and "k" <= ? limit 1`, src)
		is.Equal(t, `[1 1.5 [1] x <nil> 9]`, fmt.Sprint(bound))
		src, bound = stmttest.FoundQuery(u.Table, nil, nil, nil)
		is.Equal(t, `select 1 from "t" limit 1`, src)
		is.Equal(t, 0, len(bound))
	})

	t.Run("should write a condition with its value on the left so, with the operator as the text writes it, and bind the values in the order of the text", func(t *testing.T) {
		s, err := stmt.Check(`select * from t where ? < k and a = ? and 1.5 >= b and x'01' <> c and null = a`)
		is.NotError(t, err)
		sel := s.(*stmt.Select)
		src, bound := stmttest.SelectText(sel, []any{int64(1), "x"}, " order by k")
		is.Equal(t, `select * from "t" where ? < "k" and "a" = ? and ? >= "b" and ? != "c" and ? = "a" order by k`, src)
		is.Equal(t, `[1 x 1.5 [1] <nil>]`, fmt.Sprint(bound))
	})

	t.Run("should write a scalar subquery with its ORDER BY and its LIMIT, on either side, and bind its values where the text has them", func(t *testing.T) {
		s, err := stmt.Check(`update t set a = ? where k = ? and k = (select * from u where v > ? order by w desc, x limit ?) and (select v from "U" limit 0) <= a and b = 1.5`)
		is.NotError(t, err)
		sel, args := stmttest.SelectOf(s, []any{"s", int64(1), int64(2), int64(3)})
		src, bound := stmttest.SelectText(sel, args, "")
		is.Equal(t, `select * from "t" where "k" = ? and "k" = (select * from "u" where "v" > ? order by "w" desc, "x" limit ?) and (select "v" from "U" limit 0) <= "a" and "b" = ?`, src)
		is.Equal(t, `[1 2 3 1.5]`, fmt.Sprint(bound))
		found, bound := stmttest.FoundQuery(sel.Table, sel.Where, sel.Subqueries, args)
		is.Equal(t, `select 1 from "t" where "k" = ? and "k" = (select * from "u" where "v" > ? order by "w" desc, "x" limit ?) and (select "v" from "U" limit 0) <= "a" and "b" = ? limit 1`, found)
		is.Equal(t, `[1 2 3 1.5]`, fmt.Sprint(bound))
	})
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
		// SQLite's error of a column of ORDER BY that the table does not have comes before the rule of
		// the type of a value, and a real and an integer of the key compare by their value.
		{src: "select * from t where k > 1.0 and ? >= k order by x", args: []any{int64(1)}, want: "[> on the key comparison with literal real <= on the key comparison with argument int64 WHERE that tests a column more than once WHERE with contradicting conditions WHERE that reads ]"},
		{src: "select * from t where k < 9007199254740993 and k >= 9007199254740992.0 order by x", want: "[< on the key comparison with literal integer >= on the key comparison with literal real WHERE that tests a column more than once WHERE that reads ]"},
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
		// A value on the left is the comparison turned round.
		{src: "select * from t where 1 <= k and 'x' > a", want: "[>= on the key comparison with literal integer < on another column comparison with literal text WHERE that reads a scan from a lower bound]", access: "a scan from a lower bound"},
		{src: "select * from t where ? = k", args: []any{int64(1)}, want: "[= on the key comparison with argument int64 WHERE that reads a get of one key]", access: "a get of one key"},
		{src: "select * from t where 1 = x", want: "[WHERE with a column that the table does not have in a comparison, alone]"},
		// A scalar subquery of slice 15 has no type of its own here, and its value decides the access.
		{src: "select * from t where k = (select k from u) and a = 'x'", want: "[= on the key comparison with a subquery = on another column comparison with literal text WHERE that reads ]"},
		{src: "select * from t where (select a from u limit ?) > a", args: []any{int64(1)}, want: "[< on another column comparison with a subquery WHERE that reads ]"},
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

func TestValueFirstCases(t *testing.T) {
	cases := []struct {
		src  string
		args []any
		want string
	}{
		{src: "select * from t where k = 1 and a > 'x'", want: "[]"},
		{src: "select * from t where 1 < k", want: "[value on the left of <]"},
		{src: "select * from t where 1 = k and ? >= a and b <= 2 and c is null and x'00' != d", args: []any{int64(1)}, want: "[value on the left of = value on the left of >= value on the left of !=]"},
		{src: "select * from t where null > k and ? <> a", args: []any{nil}, want: "[value on the left of > value on the left compared with NULL value on the left of != value on the left compared with NULL]"},
		// With too few arguments, whether a parameter is NULL is not known.
		{src: "select * from t where ? > k", want: "[value on the left of >]"},
		{src: "select * from t where 5 = nope", want: "[value on the left of =]"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			is.Equal(t, c.want, fmt.Sprint(stmttest.ValueFirstCases(s.(*stmt.Select), c.args)))
		})
	}
}

func TestChosenAccess(t *testing.T) {
	// The table t (k int primary key, a int not null, b text, c real) of the README of the engine, with
	// the indexes ia on (a) and iab on (a, b), created in that order, and ic on (c desc).
	columns := []stmttest.TableColumn{
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "a", Type: "INT"},
		{Name: "b", Type: "TEXT", Nullable: true},
		{Name: "c", Type: "REAL", Nullable: true},
	}
	indexes := []stmttest.TableIndex{
		{Name: "ia", Columns: []stmttest.IndexKeyColumn{{Position: 1}}},
		{Name: "iab", Columns: []stmttest.IndexKeyColumn{{Position: 1}, {Position: 2}}},
		{Name: "ic", Columns: []stmttest.IndexKeyColumn{{Position: 3, Descending: true}}},
	}
	cases := []struct {
		src  string
		args []any
		want string
	}{
		{src: "select * from t where k = 1 and a = 2", want: "a get of one key []interface {}{1}"},
		{src: "select * from t where a = 10", want: "a scan of the index ia with 1 pinned []interface {}{10}"},
		{src: "select * from t where a = 10 and b = 'x'", want: `a scan of the index iab with 2 pinned []interface {}{10, "x"}`},
		{src: "select * from t where a = 10 and b > 'x'", want: `a scan of the index iab with 1 pinned []interface {}{10} from a lower bound ("x"`},
		{src: "select * from t where a = 10 and k > 3", want: "a scan of the index ia with 1 pinned []interface {}{10} from a lower bound (3"},
		{src: "select * from t where k > 3 and c < 1.5", want: "a scan from a lower bound (3"},
		{src: "select * from t where c < 1.5 and c <= 1.5", want: "a scan of the index ic to an upper bound 1.5)"},
		{src: "select * from t where b is null and a = ?", args: []any{int64(1)}, want: "a scan of the index iab with 2 pinned []interface {}{1, interface {}(nil)}"},
		{src: "select * from t where a is null", want: "a scan of every row"},
		{src: "select * from t where a = 1 and a = 2", want: "a scan of the index ia with 1 pinned []interface {}{1}"},
		{src: "select * from t where b != 'x'", want: "a scan of every row"},
		{src: "select * from t where c = null", want: "no row"},
		{src: "select * from t where k is null and a = 1", want: "no row"},
		// A value on the left chooses the access of the comparison turned round.
		{src: "select * from t where 1 = k and 2 = a", want: "a get of one key []interface {}{1}"},
		{src: "select * from t where 10 = a and 'x' < b", want: `a scan of the index iab with 1 pinned []interface {}{10} from a lower bound ("x"`},
		{src: "select * from t where ? < k and 1.5 > c", args: []any{int64(3)}, want: "a scan from a lower bound (3"},
		{src: "select * from t where 1.5 >= c and c > ?", args: []any{0.5}, want: "a scan of the index ic from a lower bound (0.5 to an upper bound 1.5]"},
		{src: "select * from t where null = c", want: "no row"},
	}
	for _, c := range cases {
		t.Run(c.src, func(t *testing.T) {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			a, ok := stmttest.ChosenAccess(columns, indexes, s.(*stmt.Select), c.args)
			is.True(t, ok)
			is.Equal(t, c.want, a.String())
		})
	}

	t.Run("should choose for an ORDER BY and a LIMIT as the agreed rules of slice 13 rank the candidates", func(t *testing.T) {
		// The table r (k int primary key, a int not null, b text, c real, d blob) of the README of the
		// engine, with the indexes rk on (k desc), ra on (a), rb on (b), rbc on (b, c desc), and rd on
		// (d), created in that order, and the cases of its table of examples with ORDER BY.
		columns := []stmttest.TableColumn{
			{Name: "k", Type: "INT", PrimaryKey: true},
			{Name: "a", Type: "INT"},
			{Name: "b", Type: "TEXT", Nullable: true},
			{Name: "c", Type: "REAL", Nullable: true},
			{Name: "d", Type: "BLOB", Nullable: true},
		}
		indexes := []stmttest.TableIndex{
			{Name: "rk", Columns: []stmttest.IndexKeyColumn{{Position: 0, Descending: true}}},
			{Name: "ra", Columns: []stmttest.IndexKeyColumn{{Position: 1}}},
			{Name: "rb", Columns: []stmttest.IndexKeyColumn{{Position: 2}}},
			{Name: "rbc", Columns: []stmttest.IndexKeyColumn{{Position: 2}, {Position: 3, Descending: true}}},
			{Name: "rd", Columns: []stmttest.IndexKeyColumn{{Position: 4}}},
		}
		cases := []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from r order by k", want: "a scan of every row, delivered forward"},
			{src: "select * from r order by k desc limit 3", want: "a scan of every row, delivered backward"},
			{src: "select * from r order by a", want: "a scan of the index ra, delivered forward"},
			{src: "select * from r order by c", want: "a scan of every row, sorted"},
			{src: "select * from r order by c desc, k limit 5", want: "a scan of every row, sorted"},
			{src: "select * from r order by b desc", want: "a scan of the index rb, delivered backward"},
			{src: "select * from r order by b desc, c limit 2", want: "a scan of the index rbc, delivered backward"},
			{src: "select * from r order by b, c", want: "a scan of every row, sorted"},
			{src: "select * from r where b = 'x' order by c desc", want: `a scan of the index rbc with 1 pinned []interface {}{"x"}, delivered forward`},
			{src: "select * from r where b is null order by b desc, c, k desc", want: "a scan of the index rbc with 1 pinned []interface {}{interface {}(nil)}, delivered backward"},
			{src: "select * from r where k > 3 order by a", want: "a scan from a lower bound (3, sorted"},
			{src: "select * from r where k > 3 order by a limit 1", want: "a scan of the index ra, delivered forward"},
			{src: "select * from r where a >= ? order by a desc limit ?", args: []any{int64(1), int64(2)}, want: "a scan of the index ra from a lower bound [1, delivered backward"},
			{src: "select * from r where b = 'x' order by a limit 1", want: `a scan of the index rb with 1 pinned []interface {}{"x"}, sorted`},
			{src: "select * from r order by a, k, b", want: "a scan of every row, sorted"},
			{src: "select * from r where k = 3 order by c limit 1", want: "a get of one key []interface {}{3}, delivered forward"},
			{src: "select * from r where c > null order by d", want: "no row, delivered forward"},
			{src: "select * from r where b = 'x' and c > 1.5 limit 1", want: `a scan of the index rbc with 1 pinned []interface {}{"x"} from a lower bound (1.5`},
			// A pinned column is taken out of the key wherever it is, and out of the ORDER BY.
			{src: "select * from r where c = 1.5 order by b", want: "a scan of the index rb, delivered forward"},
			{src: "select * from r where a = 1 order by a desc, k desc", want: "a scan of the index ra with 1 pinned []interface {}{1}, delivered backward"},
			{src: "select * from r where a = 1 order by a desc", want: "a scan of the index ra with 1 pinned []interface {}{1}, delivered forward"},
			// IS NULL of a column that does not take NULL pins nothing.
			{src: "select * from r where a is null order by a desc, k", want: "a scan of every row, sorted"},
			// A LIMIT without ORDER BY changes nothing in the choice.
			{src: "select * from r where k > 3 limit 1", want: "a scan from a lower bound (3"},
		}
		for _, c := range cases {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			a, ok := stmttest.ChosenAccess(columns, indexes, s.(*stmt.Select), c.args)
			is.True(t, ok, c.src)
			is.Equal(t, c.want, a.String(), c.src)
		}
	})

	t.Run("should choose goqite's index for its receive with a LIMIT, and the index of the range without one", func(t *testing.T) {
		columns := []stmttest.TableColumn{
			{Name: "id", Type: "TEXT", PrimaryKey: true},
			{Name: "queue", Type: "TEXT"},
			{Name: "timeout", Type: "TEXT"},
			{Name: "received", Type: "INT"},
			{Name: "priority", Type: "INT"},
			{Name: "created", Type: "TEXT"},
		}
		indexes := []stmttest.TableIndex{
			{Name: "goqite_queue_priority_created_idx", Columns: []stmttest.IndexKeyColumn{{Position: 1}, {Position: 4, Descending: true}, {Position: 5}}},
			{Name: "goqite_queue_timeout", Columns: []stmttest.IndexKeyColumn{{Position: 1}, {Position: 2}}},
		}
		args := []any{"q", "t", int64(3)}
		for src, want := range map[string]string{
			"select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1": `a scan of the index goqite_queue_priority_created_idx with 1 pinned []interface {}{"q"}, delivered forward`,
			"select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created":         `a scan of the index goqite_queue_timeout with 1 pinned []interface {}{"q"} to an upper bound "t"], sorted`,
		} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			a, ok := stmttest.ChosenAccess(columns, indexes, s.(*stmt.Select), args)
			is.True(t, ok, src)
			is.Equal(t, want, a.String(), src)
		}
	})

	t.Run("should report false for a select that SQLite or s4ql refuses", func(t *testing.T) {
		for src, args := range map[string][]any{"select * from t order by x": nil, "select * from t order by a limit ?": {int64(-1)}, "select * from t where a = 1 limit ?": {"2"}} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			_, ok := stmttest.ChosenAccess(columns, indexes, s.(*stmt.Select), args)
			is.True(t, !ok, src)
		}
		for _, src := range []string{"select * from t where x = 1", "select * from t where a = 'x'", "select * from t where a = ?"} {
			s, err := stmt.Check(src)
			is.NotError(t, err)
			_, ok := stmttest.ChosenAccess(columns, indexes, s.(*stmt.Select), nil)
			is.True(t, !ok, src)
		}
	})
}

func TestIndexRule(t *testing.T) {
	tables := map[string][]stmttest.TableColumn{
		"t": {{Name: "k", Type: "INT", PrimaryKey: true}, {Name: "a", Type: "INT"}},
		"u": {{Name: "v", Type: "TEXT", PrimaryKey: true}},
	}
	indexes := map[string]bool{"i": true}
	for _, c := range []struct{ src, want string }{
		{"create index j on t (a, k desc)", ""},
		{"create index t on nosuch (a)", "no such table: main.nosuch"},
		{"create index T on u (nosuch)", "there is already a table named T"},
		{"create index I on u (nosuch)", "index I already exists"},
		{`create index j on t (a, "z" desc, y)`, `no such column: "z" - should this be a string literal in single-quotes?`},
		{"create index j on t (a, z desc, y)", "no such column: z"},
		{"drop index I", ""},
		{"drop index t", "no such index: t"},
	} {
		s, err := stmt.Check(c.src)
		is.NotError(t, err, c.src)
		is.Equal(t, c.want, stmttest.IndexRule(tables, indexes, s), c.src)
	}

	t.Run("should give nothing for a statement of IF NOT EXISTS or IF EXISTS that does nothing, and the plain statement's error otherwise", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"create index if not exists I on u (nosuch)", ""},
			{"create index if not exists i on nosuch (a)", "no such table: main.nosuch"},
			{"create index if not exists t on t (a)", "there is already a table named t"},
			{"create index if not exists j on t (z)", "no such column: z"},
			{"drop index if exists j", ""},
			{"drop index if exists t", ""},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.Equal(t, c.want, stmttest.IndexRule(tables, indexes, s), c.src)
		}
	})
}

func TestExistsRule(t *testing.T) {
	tables := map[string][]stmttest.TableColumn{
		"t": {{Name: "k", Type: "INT", PrimaryKey: true}, {Name: "a", Type: "INT"}},
		"u": {{Name: "v", Type: "TEXT", PrimaryKey: true}},
	}
	indexes := map[string]bool{"i": true}
	for _, c := range []struct{ src, want string }{
		{"create table if not exists T (x blob primary key, x int) strict, without rowid", "a table of the name exists"},
		{"create table t (k int primary key) strict, without rowid", ""},
		{"create table if not exists i (k int primary key) strict, without rowid", ""},
		{"create table if not exists w (k int primary key) strict, without rowid", ""},
		{"drop table if exists w", "no table of the name exists"},
		{"drop table if exists I", "no table of the name exists, and an index has it"},
		{"drop table if exists U", ""},
		{"drop table w", ""},
		{"create index if not exists i on u (v)", "an index of the name exists"},
		{`create index if not exists "I" on t (a, nosuch)`, "an index of the name exists, and the table lacks a column of the statement"},
		{"create index if not exists i on nosuch (a)", ""},
		{"create index if not exists t on t (a)", ""},
		{"create index if not exists j on t (a)", ""},
		{"create index i on t (a)", ""},
		{"drop index if exists j", "no index of the name exists"},
		{"drop index if exists T", "no index of the name exists, and a table has it"},
		{"drop index if exists i", ""},
		{"drop index j", ""},
		{"select * from t", ""},
	} {
		s, err := stmt.Check(c.src)
		is.NotError(t, err, c.src)
		is.Equal(t, c.want, stmttest.ExistsRule(tables, indexes, s), c.src)
	}

	t.Run("should follow the tables and the indexes that it is given, as a drop table leaves them without the indexes of its table", func(t *testing.T) {
		after := map[string][]stmttest.TableColumn{"u": tables["u"]}
		s, err := stmt.Check("create index if not exists i on t (a)")
		is.NotError(t, err)
		is.Equal(t, "", stmttest.ExistsRule(after, map[string]bool{}, s))
		s, err = stmt.Check("drop index if exists i")
		is.NotError(t, err)
		is.Equal(t, "no index of the name exists", stmttest.ExistsRule(after, map[string]bool{}, s))
	})
}
