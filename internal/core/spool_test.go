package core_test

import (
	"os"
	"slices"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
)

func TestSpool(t *testing.T) {
	t.Run("should keep no spool file under a name, neither while a transaction is open nor after its commit", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})

		// names in the directory of the cache, other than the cache's own files.
		names := func() []string {
			t.Helper()
			entries, err := os.ReadDir(w.dir)
			is.NotError(t, err)
			var others []string
			for _, e := range entries {
				if !slices.Contains([]string{"a.db", "a.db-wal", "a.db-shm"}, e.Name()) {
					others = append(others, e.Name())
				}
			}
			return others
		}

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		create("t1", 1, "k")(t, tx)
		note("insert", 1)(t, tx)
		is.EqualSlice(t, nil, names())
		is.NotError(t, tx.Commit(t.Context()))
		is.EqualSlice(t, nil, names())
	})
}
