package engine_test

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestCheck_valueFirst(t *testing.T) {
	t.Run("should refuse a comparison with the value on the left as not supported, as the pass did before it supported it", func(t *testing.T) {
		for _, c := range []struct {
			src, err string
			n        int
		}{
			{"select * from t where 1 = v", "s4ql: not supported: value on the left of a comparison (at offset 22)", 0},
			{"select * from t where ? = v", "s4ql: not supported: value on the left of a comparison (at offset 22)", 1},
			{"select * from t where - /* a */ 1 < v", "s4ql: not supported: value on the left of a comparison (at offset 22)", 0},
			{"select v from t where v = ? and v is null and 'a' >= w and ? < x", "s4ql: not supported: value on the left of a comparison (at offset 46)", 2},
			{" ; UPDATE t SET v = ? WHERE ? > w;", "s4ql: not supported: value on the left of a comparison (at offset 28)", 2},
			{"delete from t where 1 = v", "s4ql: not supported: value on the left of a comparison (at offset 20)", 0},
		} {
			err := engine.Check(c.src, c.n)
			is.True(t, errors.Is(err, stmt.ErrNotSupported), c.src)
			is.Equal(t, c.err, err.Error(), c.src)
		}
	})

	t.Run("should check a comparison with the column on the left as before", func(t *testing.T) {
		is.NotError(t, engine.Check("select * from t where v = ? and w < 1 and x is not null", 1))
		is.NotError(t, engine.Check("update t set v = 1 where w >= 2", 0))
		is.NotError(t, engine.Check("delete from t where v != 'a'", 0))
	})
}
