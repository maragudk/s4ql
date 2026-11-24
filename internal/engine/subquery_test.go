package engine_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// subqueried reports whether s is a select, an update, or a delete with a scalar subquery, of slice
// 15, which the pass supports and the engine refuses as not supported until it runs it, in
// [engine.Check] and in every run: the differential tests take that refusal for a text that is not
// supported.
func subqueried(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Select:
		return s.Subqueries != nil
	case *stmt.Update:
		return s.Subqueries != nil
	case *stmt.Delete:
		return s.Subqueries != nil
	}
	return false
}

func TestCheck_subquery(t *testing.T) {
	t.Run("should refuse a select, an update, or a delete with a subquery as not supported, as the pass did before it supported it", func(t *testing.T) {
		for _, c := range []struct {
			src, err string
			n        int
		}{
			{"select * from t where k = (select k from u)", "s4ql: not supported: subquery (at offset 26)", 0},
			{"select v from t where (select k from u where x = ?) < k", "s4ql: not supported: subquery (at offset 22)", 1},
			{"select * from t where v = ? and k = (select k from u) and w = (select w from u)", "s4ql: not supported: subquery (at offset 36)", 1},
			{"update t set v = ? where k = (select k from t where v = ? order by k limit 1)", "s4ql: not supported: subquery (at offset 29)", 2},
			{"delete from t where k >= (select k from u limit ?) returning *", "s4ql: not supported: subquery (at offset 25)", 1},
			{"update goqite set timeout = ? where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received",
				"s4ql: not supported: subquery (at offset 55)", 5},
			// A token of a subquery that the pass refuses gets the pass's error.
			{"select * from t where k = (select k + 1 from u)", "s4ql: not supported: result column other than a column or * (at offset 34)", 0},
		} {
			err := engine.Check(c.src, c.n)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
			// The number of arguments comes after the text.
			err = engine.Check(c.src, c.n+1)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
		}
	})

	t.Run("should make no plan of a statement with a subquery", func(t *testing.T) {
		for _, src := range []string{"select * from t where k = (select k from u)", "update t set v = 1 where k = (select k from u)", "delete from t where k = (select k from u)"} {
			s, err := stmt.Check(src)
			is.NotError(t, err, src)
			panicked := func() (p any) {
				defer func() { p = recover() }()
				engine.NewPlan(s)
				return nil
			}()
			is.Equal(t, "engine: no plan of a statement with a subquery", fmt.Sprint(panicked), src)
		}
	})

	t.Run("should check a statement without a subquery as before", func(t *testing.T) {
		is.NotError(t, engine.Check("select * from t where k = ?", 1))
		is.NotError(t, engine.Check(`update "select" set "select" = 1 where "select" = ?`, 1))
		is.NotError(t, engine.Check("delete from t where v = 1", 0))
	})
}

func TestConn_subquery(t *testing.T) {
	t.Run("should refuse a statement with a subquery in Exec and in Query, and run nothing of it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		is.Equal(t, "", i.exec("insert into t (k, v) values (1, 'a')").err)
		for _, src := range []string{"select * from t where k = (select k from t)", "update t set v = 'c' where k = (select k from t)", "delete from t where k = (select k from t) returning v"} {
			_, err := i.c.Exec(t.Context(), src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
			_, err = i.c.Query(t.Context(), src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
		}
		got := i.queryValues("select * from t", nil)
		is.Equal(t, "", got.err)
		is.Equal(t, 1, len(got.rows))
		is.Equal(t, `1,"a"`, got.rows[0])
	})
}

func TestPair_subquery(t *testing.T) {
	t.Run("should take the refusal of a statement with a subquery, also where a rule of its values would refuse it", func(t *testing.T) {
		for _, src := range []string{"select * from t1 where v1 = (select v1 from t1)", "update t1 set v1 = 'x' where v1 = (select v1 from t1)",
			"delete from t1 where v1 = 'x' and v1 = (select v1 from t1 where v1 = 'y') returning *", "select * from t1 where v1 = (select v1 from t1 where v1 = ?)",
			"update t1 set v1 = ? where v1 = (select v1 from t1 where v1 = ?)",
			"delete from t1 where v1 = ? and v1 = (select v1 from t1 where v1 = ?)"} {
			for _, exec := range []bool{true, false} {
				// With a tally, as the generated tests and the fuzz tests keep one, which counts what a
				// statement reaches.
				p := newPair(t, newMemoryInstance(t), tally{})
				for _, s := range fuzzSetUp {
					is.Equal(t, "", p.step(stmttest.Call{Src: s, Supported: true}), s)
				}
				var args []any
				for range strings.Count(src, "?") {
					args = append(args, int64(1))
				}
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Args: args, Query: !exec}), src)
			}
		}
	})
}
