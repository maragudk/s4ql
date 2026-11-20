package engine_test

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestCheck_exists(t *testing.T) {
	t.Run("should refuse IF NOT EXISTS and IF EXISTS as not supported, as the pass did before it supported them", func(t *testing.T) {
		for _, c := range []struct{ src, err string }{
			{"create table if not exists t (v int primary key) strict, without rowid", "s4ql: not supported: IF NOT EXISTS (at offset 13)"},
			{" ;; CREATE/* a */TABLE -- b\n If Not Exists t (v int primary key) strict, without rowid;", "s4ql: not supported: IF NOT EXISTS (at offset 29)"},
			{"drop table if exists t", "s4ql: not supported: IF EXISTS (at offset 11)"},
			{"; DROP\tTABLE/**/IF EXISTS t;", "s4ql: not supported: IF EXISTS (at offset 16)"},
			{"create index if not exists i on t (v)", "s4ql: not supported: IF NOT EXISTS (at offset 13)"},
			{"create/**/index/**/if not exists i on t (v desc)", "s4ql: not supported: IF NOT EXISTS (at offset 19)"},
			{"drop index if exists i", "s4ql: not supported: IF EXISTS (at offset 11)"},
			{"\ndrop -- x\n index\n\nIF exists i", "s4ql: not supported: IF EXISTS (at offset 19)"},
		} {
			err := engine.Check(c.src, 0)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
		}
	})

	t.Run("should check the four statements without the clauses as before", func(t *testing.T) {
		for _, src := range []string{
			"create table t (v int primary key) strict, without rowid",
			"drop table t",
			"create index i on t (v)",
			"drop index i",
		} {
			is.NotError(t, engine.Check(src, 0), src)
		}
	})
}
