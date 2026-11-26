package stmttest_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

func TestUpsertRefusal(t *testing.T) {
	t.Run("should refuse an insert with ON CONFLICT at its ON, and no other statement", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"insert into t values (1) on conflict (k) do nothing", "s4ql: not supported: ON CONFLICT clause (at offset 25)"},
			{"insert into t (k, v) values (?, ?) on conflict (k) do update set v = excluded.v returning *", "s4ql: not supported: ON CONFLICT clause (at offset 35)"},
			{"insert into t values (1)", ""},
			{"insert into t values (1) returning *", ""},
			{"update t set v = 1", ""},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err, c.src)
			is.Equal(t, c.want, stmttest.UpsertRefusal(s), c.src)
		}
	})
}
