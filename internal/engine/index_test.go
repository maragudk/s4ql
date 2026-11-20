package engine_test

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
)

func TestCheck_indexes(t *testing.T) {
	t.Run("should refuse a create index and a drop index as not supported, as the pass did before it supported them", func(t *testing.T) {
		for _, c := range []struct{ src, err string }{
			{"create index i on t (a, b desc)", "s4ql: not supported: CREATE INDEX (at offset 0)"},
			{" ;; CREATE INDEX i ON t (a);", "s4ql: not supported: CREATE INDEX (at offset 4)"},
			{"drop index i", "s4ql: not supported: DROP INDEX (at offset 5)"},
			{"; DROP/* a */ -- b\n Index i;", "s4ql: not supported: DROP INDEX (at offset 20)"},
		} {
			err := engine.Check(c.src, 0)
			is.True(t, err != nil, c.src)
			is.Equal(t, c.err, err.Error(), c.src)
		}
	})
}
