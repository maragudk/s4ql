package engine_test

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestEngine_upsert(t *testing.T) {
	t.Run("should refuse an insert with ON CONFLICT, which the engine does not run yet, after the arguments and before the database, and change nothing", func(t *testing.T) {
		i := newInstance(t, engine.Options{})
		for _, src := range []string{
			"create table t (k int primary key, v int, s text) strict, without rowid",
			"insert into t values (1, 10, 'a')",
		} {
			is.Equal(t, "", i.exec(src).err, src)
		}
		before := i.state()
		for _, c := range []struct {
			src   string
			args  []any
			query bool
			want  string
		}{
			{"insert into t values (1, 20, 'b') on conflict (k) do nothing", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
			{"insert into t values (2, 20, 'b') on conflict (k) do nothing", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
			{"insert into t values (1, ?, 'b') on conflict (k) do update set v = excluded.v + ? returning *", []any{int64(20), int64(1)}, true, "s4ql: not supported: ON CONFLICT clause (at offset 33)"},
			{"insert into t (k, s) values (1, 'b') on conflict (k) do update set s = lower(excluded.s) || s", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 37)"},
			{"insert into t values (1, 20, 'b') on conflict (k) do update set v = 1 where excluded.v is null", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
			// Whatever the database holds.
			{"insert into nosuch values (1) on conflict (k) do nothing", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 30)"},
			{"insert into t values (1, 20, 'b') on conflict (nosuch) do update set v = excluded.nosuch", nil, false, "s4ql: not supported: ON CONFLICT clause (at offset 34)"},
			// The arguments come first.
			{"insert into t values (1, ?, 'b') on conflict (k) do update set v = ?", []any{int64(1)}, false, "s4ql: wrong number of arguments: 1 for a statement with 2 parameters"},
		} {
			var got answer
			if c.query {
				got = i.queryValues(c.src, values(c.args))
			} else {
				got = i.execValues(c.src, values(c.args))
			}
			is.Equal(t, c.want, got.message, c.src)
			if c.want != "" && got.err == "not supported" {
				is.Equal(t, c.want, fmt.Sprint(engine.Check(c.src, len(c.args))), c.src)
			}
		}
		_, err := i.c.ExecAll(t.Context(), "delete from t where k = 2; insert into t values (1, 30, 'c') on conflict (k) do nothing")
		is.Equal(t, "s4ql: not supported: ON CONFLICT clause (at offset 61)", fmt.Sprint(err))
		is.Equal(t, before, i.state())
	})

	t.Run("should not plan an insert with ON CONFLICT, which would write without its clause", func(t *testing.T) {
		s, err := stmt.Check("insert into t values (1) on conflict (k) do nothing")
		is.NotError(t, err)
		defer func() {
			is.Equal(t, "engine: an insert with ON CONFLICT, which upsertRefusal refuses", fmt.Sprint(recover()))
		}()
		engine.NewPlan(s)
	})
}
