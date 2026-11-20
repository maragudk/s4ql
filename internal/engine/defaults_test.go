package engine_test

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestCheck_defaults(t *testing.T) {
	t.Run("should refuse a create table with DEFAULT as not supported, as the pass did before it supported it", func(t *testing.T) {
		for _, c := range []struct{ src, err string }{
			{"create table t (k int primary key, v int not null default 0) strict, without rowid", "s4ql: not supported: DEFAULT (at offset 50)"},
			{"create table t (k int primary key default null) strict, without rowid", "s4ql: not supported: DEFAULT (at offset 34)"},
			{" ;; CREATE TABLE t (k int primary key, v text/* a */DEFAULT -- b\n'x', w int default -/* c */1) strict, without rowid;", "s4ql: not supported: DEFAULT (at offset 52)"},
			{"create table t (k int primary key, v int, w int not null default - 1) strict, without rowid", "s4ql: not supported: DEFAULT (at offset 57)"},
			// IF NOT EXISTS comes first in the text, and the pass named it first.
			{"create table if not exists t (k int primary key default 1) strict, without rowid", "s4ql: not supported: IF NOT EXISTS (at offset 13)"},
		} {
			err := engine.Check(c.src, 0)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
		}
	})

	t.Run("should check a create table without DEFAULT as before", func(t *testing.T) {
		is.NotError(t, engine.Check("create table t (k int primary key, v int not null) strict, without rowid", 0))
	})
}
