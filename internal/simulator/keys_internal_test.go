//go:build s4ql_browser

package simulator

import (
	"slices"
	"testing"

	"maragu.dev/is"
)

func TestKnownKeys(t *testing.T) {
	t.Run("should know the rows at the head of a stored log, in the order of their inserts", func(t *testing.T) {
		var k knownKeys
		k.stored([]*CommitObject{{Keys: []int64{1, 3}}, {Keys: []int64{2}}, {Deletes: []int64{1}}})
		is.EqualSlice(t, []int64{3, 2}, k.known)
		is.EqualSlice(t, []int64{3, 2}, k.deletable)
	})

	t.Run("should pick a delete only among known keys that no delete has picked, and leave one", func(t *testing.T) {
		var k knownKeys
		k.learn(1)
		_, ok := k.pickDelete(0)
		is.True(t, !ok, "no delete of the only known key")

		k.learn(2)
		k.learn(3)
		k.learn(4)
		key, ok := k.pickDelete(1)
		is.True(t, ok, "a delete")
		is.Equal(t, int64(2), key)
		is.EqualSlice(t, []int64{1, 3, 4}, k.known)
		// While the delete of 2 runs, no read selects it.
		for u := range uint64(64) {
			key, row, ok := k.pickRead(u)
			is.True(t, ok && row && key != 2, "a read of a known key")
		}

		// A delete of unknown outcome leaves its key in limbo, one that failed makes it known again,
		// and neither key is picked by a delete again.
		k.ended(2, true, "unknown")
		key, _ = k.pickDelete(0)
		is.Equal(t, int64(1), key)
		k.ended(1, true, "not_committed")
		is.EqualSlice(t, []int64{3, 4, 1}, k.known)
		for u := range uint64(64) {
			if key, ok := k.pickDelete(u); ok {
				is.True(t, key == 3 || key == 4, "a delete of a key that no delete has picked")
				break
			}
		}
	})

	t.Run("should know a row from an acknowledged insert only", func(t *testing.T) {
		var k knownKeys
		k.ended(1, false, "committed")
		k.ended(2, false, "unknown")
		k.ended(3, false, "not_committed")
		is.EqualSlice(t, []int64{1}, k.known)
	})

	t.Run("should pick one read in eight among the recent acknowledged deletes", func(t *testing.T) {
		var k knownKeys
		for key := range int64(20) {
			k.learn(key + 1)
		}
		for range 12 {
			key, _ := k.pickDelete(0)
			k.ended(key, true, "committed")
		}
		is.Equal(t, recentDeletes, len(k.gone))
		gone := 0
		for u := range uint64(800) {
			key, row, ok := k.pickRead(u)
			is.True(t, ok, "a read")
			is.Equal(t, !row, slices.Contains(k.gone, key), "a read expects no row of a key that is gone")
			is.Equal(t, !row, !slices.Contains(k.known, key))
			if !row {
				gone++
			}
		}
		is.Equal(t, 100, gone)
	})
}
