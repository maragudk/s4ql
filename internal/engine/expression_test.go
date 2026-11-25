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

// expressed reports whether s is a statement with an arithmetic expression, which the pass supports
// and the engine refuses as not supported until it evaluates it, in [engine.Check] and in every run:
// the differential tests take that refusal for a text that is not supported.
func expressed(s stmt.Statement) bool {
	return engine.ExpressionRefusal(s) != nil
}

func TestCheck_expression(t *testing.T) {
	t.Run("should refuse a statement with an arithmetic expression as not supported, at its first expression", func(t *testing.T) {
		for _, c := range []struct {
			src, err string
			n        int
		}{
			{"update t set v = v + 1", "s4ql: not supported: expression (at offset 17)", 0},
			{"update t set v = w", "s4ql: not supported: expression (at offset 17)", 0},
			{"update t set v = 1, w = -? where k = ?", "s4ql: not supported: expression (at offset 24)", 2},
			{"update t set v = 1 where k = ? * 2", "s4ql: not supported: expression (at offset 29)", 1},
			{"select * from t where v + 1 = 2", "s4ql: not supported: expression (at offset 22)", 0},
			{"select * from t where 2 < v * ?", "s4ql: not supported: expression (at offset 26)", 1},
			{"select * from t where k = 1 and (v - 1) * 2 < k", "s4ql: not supported: expression (at offset 32)", 0},
			{"delete from t where k = (select k from u where x / 2 = ?) returning *", "s4ql: not supported: expression (at offset 47)", 1},
			{"update goqite set timeout = ?, received = received + 1 where queue = ? and id = (select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1) returning id, body, received",
				"s4ql: not supported: expression (at offset 42)", 5},
			// What the pass refuses keeps the pass's error.
			{"update t set v = v / 0", "s4ql: not supported: division by zero (at offset 21)", 0},
		} {
			err := engine.Check(c.src, c.n)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
			// The number of arguments comes after the text.
			err = engine.Check(c.src, c.n+1)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
		}
	})

	t.Run("should make no plan of a statement with an expression", func(t *testing.T) {
		for _, src := range []string{"update t set v = v + 1", "select * from t where v = 1 + 1", "delete from t where -v = 1", "select * from t where k = (select k from u where x + 1 = 2)"} {
			s, err := stmt.Check(src)
			is.NotError(t, err, src)
			panicked := func() (p any) {
				defer func() { p = recover() }()
				engine.NewPlan(s)
				return nil
			}()
			is.Equal(t, "engine: no plan of a statement with an expression", fmt.Sprint(panicked), src)
		}
	})

	t.Run("should check a statement without an expression as before", func(t *testing.T) {
		is.NotError(t, engine.Check("update t set v = -1 where k = ?", 1))
		is.NotError(t, engine.Check("select * from t where k = (select k from u where x = ?)", 1))
		is.NotError(t, engine.Check("delete from t where -5 < v", 0))
	})
}

func TestConn_expression(t *testing.T) {
	t.Run("should refuse a statement with an expression in Exec and in Query, and run nothing of it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v int) strict, without rowid").err)
		is.Equal(t, "", i.exec("insert into t (k, v) values (1, 2)").err)
		for _, src := range []string{"update t set v = v + 1", "select * from t where v * 2 = 4", "delete from t where k = v - 1 returning v"} {
			_, err := i.c.Exec(t.Context(), src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
			_, err = i.c.Query(t.Context(), src)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), src)
		}
		got := i.queryValues("select * from t", nil)
		is.Equal(t, "", got.err)
		is.Equal(t, 1, len(got.rows))
		is.Equal(t, `1,2`, got.rows[0])
	})
}

func TestPair_expression(t *testing.T) {
	t.Run("should take the refusal of a statement with an expression, also where a rule of its values would refuse it", func(t *testing.T) {
		for _, src := range []string{"update t1 set v1 = v1 + 1", "update t1 set v1 = 'x' where v1 = 1 + 1", "select * from t1 where v1 * 2 = 'x'",
			"delete from t1 where v1 = 'x' and -v1 = ? returning *", "select * from t1 where v1 = (select v1 from t1 where v1 - 1 = ?)"} {
			for _, exec := range []bool{true, false} {
				p := newPair(t, newMemoryInstance(t), nil)
				for _, s := range fuzzSetUp {
					is.Equal(t, "", p.step(stmttest.Call{Src: s, Supported: true}), s)
				}
				var args []any
				if strings.Contains(src, "?") {
					args = []any{int64(1)}
				}
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Args: args, Query: !exec}), src)
			}
		}
	})
}
