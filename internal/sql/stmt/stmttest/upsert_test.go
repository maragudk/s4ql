package stmttest_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/expr"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

func TestUpsertRefusals(t *testing.T) {
	columns := []stmttest.TableColumn{
		{Name: "k", Type: "INT", PrimaryKey: true},
		{Name: "v", Type: "INT", Nullable: true},
		{Name: "w", Type: "REAL", Nullable: true},
		{Name: "s", Type: "TEXT", Nullable: true},
	}

	t.Run("should give each refusal of the proposed row, of SET, and of WHERE, with excluded.c as a column of its type", func(t *testing.T) {
		for _, c := range []struct {
			src      string
			args     []any
			readOnly bool
			want     []string
		}{
			{"insert into t values (1, 2, 1.5, 'a') on conflict (k) do nothing", nil, false, nil},
			{"insert into t values (1, 2, 1.5, 'a') on conflict (k) do update set v = excluded.v + v, s = excluded.s where excluded.w > w", nil, false, nil},
			{"insert into t values (1, 'x', 1, 'a') on conflict (k) do nothing", nil, false, []string{"s4ql: not supported: a string literal for the column t.v of type INT"}},
			{"insert into t (k, w) values (1, ?) on conflict (k) do update set v = excluded.s, w = excluded.v + 1 where excluded.s = 1", []any{int64(1)}, false, []string{
				"s4ql: not supported: argument 1, which is an integer, for the column t.w of type REAL",
				"s4ql: not supported: the column excluded.s of type TEXT for the column t.v of type INT",
				"s4ql: not supported: an expression of type integer for the column t.w of type REAL",
				"s4ql: not supported: an integer literal compared with the column excluded.s of type TEXT",
			}},
			{"insert into t (k) values (1) on conflict (k) do update set v = excluded.s + 1 where v = excluded.w", nil, false, []string{
				"s4ql: not supported: column of type TEXT in arithmetic (at offset 63)",
				"s4ql: not supported: the column excluded.w of type REAL compared with the column t.v of type INT",
			}},
			{"insert into t (k) values (1) on conflict (k) do update set v = excluded.s", nil, true, nil},
			{"insert into t (k) values (1) on conflict (k) do update set s = lower(excluded.s)", nil, true, []string{"s4ql: not supported: call of lower (at offset 63)"}},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.EqualSlice(t, c.want, stmttest.UpsertRefusals("t", columns, s.(*stmt.Insert), c.args, c.readOnly), c.src)
		}
	})

	t.Run("should give the query of the quirk of an operator of DO UPDATE, on the row of the conflict where the operator has a column", func(t *testing.T) {
		src := "insert into t (k, v) values (?, 3) on conflict (k) do update set v = excluded.v * ? where v > 0 and excluded.v + v < 9"
		s, err := stmt.Check(src)
		is.NotError(t, err)
		insert := s.(*stmt.Insert)
		sites := stmttest.UpsertSites(insert.Conflict)
		is.Equal(t, 2, len(sites))
		q, args := stmttest.UpsertQuirkQuery(insert, src, "k", sites[0], sites[0].Root, expr.ErrOverflow, []any{int64(1), int64(2)})
		is.Equal(t, `insert into t (k, v) values (?, 3) on conflict ("k") do update set "k" = "k" where "v" > ? and (excluded."v" + "v") < ? and typeof(excluded."v") = 'integer' and typeof(?) = 'integer' and typeof((excluded."v" * ?)) = 'real' returning 1`, q)
		is.EqualSlice(t, []any{int64(1), int64(0), int64(9), int64(2), int64(2)}, args)
		q, _ = stmttest.UpsertQuirkQuery(insert, src, "k", sites[1], sites[1].Root, expr.ErrOverflow, []any{int64(1), int64(2)})
		is.Equal(t, `insert into t (k, v) values (?, 3) on conflict ("k") do update set "k" = "k" where "v" > ? and typeof(excluded."v") = 'integer' and typeof("v") = 'integer' and typeof((excluded."v" + "v")) = 'real' returning 1`, q)
	})
}
