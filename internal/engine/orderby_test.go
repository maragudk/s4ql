package engine_test

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// orderedOrLimited reports whether s is a select with ORDER BY or LIMIT, of slice 13, which the pass
// supports and the engine refuses as not supported until it runs it, in [engine.Check] and in every
// run: the differential tests take that refusal for a text that is not supported.
func orderedOrLimited(s stmt.Statement) bool {
	sel, ok := s.(*stmt.Select)
	return ok && (sel.OrderBy != nil || sel.Limited())
}

func TestCheck_orderLimit(t *testing.T) {
	t.Run("should refuse a select with ORDER BY or LIMIT as not supported, as the pass did before it supported them", func(t *testing.T) {
		for _, c := range []struct {
			src, err string
			n        int
		}{
			{"select * from t order by v", "s4ql: not supported: ORDER BY clause (at offset 16)", 0},
			{"select * from t limit 1", "s4ql: not supported: LIMIT clause (at offset 16)", 0},
			{"select * from t limit ?", "s4ql: not supported: LIMIT clause (at offset 16)", 1},
			{"select v from t where v = 1 order by v desc, w limit 1", "s4ql: not supported: ORDER BY clause (at offset 28)", 0},
			{"select v from t where v = ? limit ?", "s4ql: not supported: LIMIT clause (at offset 28)", 2},
			{" ; SELECT * FROM t ORDER/* order */BY \"order\" LIMIT 1;", "s4ql: not supported: ORDER BY clause (at offset 19)", 0},
			{"select \"limit\" from \"order\" where \"limit\" = 1 -- order\n limit /* limit */ 5", "s4ql: not supported: LIMIT clause (at offset 56)", 0},
			{"select id from goqite where queue = ? and ? >= timeout and received < ? order by priority desc, created limit 1",
				"s4ql: not supported: ORDER BY clause (at offset 72)", 3},
		} {
			err := engine.Check(c.src, c.n)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
		}
	})

	t.Run("should check a select without ORDER BY and LIMIT as before", func(t *testing.T) {
		is.NotError(t, engine.Check("select * from t where v = ? and w < 1 and x is not null", 1))
		is.NotError(t, engine.Check(`select "order", "limit" from "order"`, 0))
	})
}
