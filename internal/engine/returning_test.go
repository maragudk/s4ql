package engine_test

import (
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// returned reports whether s is an insert, an update, or a delete with RETURNING, of slice 14, which
// the pass supports and the engine refuses as not supported until it runs it, in [engine.Check] and
// in every run: the differential tests take that refusal for a text that is not supported.
func returned(s stmt.Statement) bool {
	switch s := s.(type) {
	case *stmt.Insert:
		return s.Returning.Present()
	case *stmt.Update:
		return s.Returning.Present()
	case *stmt.Delete:
		return s.Returning.Present()
	}
	return false
}

func TestCheck_returning(t *testing.T) {
	t.Run("should refuse an insert, an update, or a delete with RETURNING as not supported, as the pass did before it supported it", func(t *testing.T) {
		for _, c := range []struct {
			src, err string
			n        int
		}{
			{"insert into t1 (v1, v2) values (?, ?) returning v1", "s4ql: not supported: RETURNING clause (at offset 38)", 2},
			{"update t1 set v2 = ? where v1 = ? returning v1, v2", "s4ql: not supported: RETURNING clause (at offset 34)", 2},
			{"delete from t1 where v1 = ? returning *", "s4ql: not supported: RETURNING clause (at offset 28)", 1},
			{"insert into t values (1) returning *", "s4ql: not supported: RETURNING clause (at offset 25)", 0},
			{"update t set v = 1 returning *", "s4ql: not supported: RETURNING clause (at offset 19)", 0},
			{"delete from t returning *", "s4ql: not supported: RETURNING clause (at offset 14)", 0},
			{" ; DELETE FROM t /* returning */ RETURNING \"returning\";", "s4ql: not supported: RETURNING clause (at offset 33)", 0},
			{"insert into goqite (queue, body, timeout, priority) values (?, ?, ?, ?) returning id", "s4ql: not supported: RETURNING clause (at offset 72)", 4},
			// A token of RETURNING that the pass refuses gets the pass's error.
			{"delete from t returning v + 1", "s4ql: not supported: result column other than a column or * (at offset 24)", 0},
		} {
			err := engine.Check(c.src, c.n)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
			// The number of arguments comes after the text.
			err = engine.Check(c.src, c.n+1)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
		}
	})

	t.Run("should make no plan of a statement with RETURNING", func(t *testing.T) {
		for _, src := range []string{"insert into t (v) values (1) returning v", "update t set v = 1 returning *", "delete from t returning v"} {
			s, err := stmt.Check(src)
			is.NotError(t, err, src)
			panicked := func() (p any) {
				defer func() { p = recover() }()
				engine.NewPlan(s)
				return nil
			}()
			is.Equal(t, "engine: no plan of a statement with RETURNING", fmt.Sprint(panicked), src)
		}
	})

	t.Run("should check an insert, an update, or a delete without RETURNING as before", func(t *testing.T) {
		is.NotError(t, engine.Check("insert into t (v) values (?)", 1))
		is.NotError(t, engine.Check(`update "returning" set "returning" = 1 where "returning" = ?`, 1))
		is.NotError(t, engine.Check("delete from t where v = 1", 0))
	})
}

func TestPair_returning(t *testing.T) {
	t.Run("should take the refusal of a statement with RETURNING, also where a rule of its values would refuse it", func(t *testing.T) {
		for _, src := range []string{"insert into t1 (v1) values ('x') returning v1", "update t1 set v1 = 'x' returning v1", "insert into t1 (v1) values (1) returning v1",
			"delete from t1 where v1 = 'x' returning *"} {
			for _, exec := range []bool{true, false} {
				p := newPair(t, newMemoryInstance(t), nil)
				for _, s := range fuzzSetUp {
					is.Equal(t, "", p.step(stmttest.Call{Src: s, Supported: true}), s)
				}
				is.Equal(t, "", p.step(stmttest.Call{Src: src, Query: !exec}), src)
			}
		}
	})
}

func TestConn_returning(t *testing.T) {
	t.Run("should refuse a statement with RETURNING in Exec and in Query, and run nothing of it", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		is.Equal(t, "", i.exec("create table t (k int primary key, v text) strict, without rowid").err)
		is.Equal(t, "", i.exec("insert into t (k, v) values (1, 'a')").err)
		for _, src := range []string{"insert into t (k, v) values (2, 'b') returning k", "update t set v = 'c' returning *", "delete from t returning v"} {
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
