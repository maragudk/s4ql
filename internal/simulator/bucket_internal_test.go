//go:build s4ql_browser

package simulator

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
)

func TestGiven(t *testing.T) {
	t.Run("should find the barcode of every value up to the counter, and of none above it", func(t *testing.T) {
		for _, n := range []int64{1, 2, 3, 49, 50, 999, 12_345, 99_999, 100_000, 7_654_321, 9_999_999_999, 10_000_000_000} {
			is.True(t, given(barcode(n), n), "the barcode of", n)
			is.True(t, given(barcode(n), n+5))
			is.True(t, !given(barcode(n), n-1), "not before", n)
		}
	})

	t.Run("should find no key that is not a barcode of a value", func(t *testing.T) {
		for _, key := range []int64{0, 1, barcode(7) + 1, barcode(7) - 10, 5_712_345_678_900} {
			is.True(t, !given(key, 10_000_000_000), key)
		}
	})
}

func TestSeed_readLog(t *testing.T) {
	// logOf the commit objects, each a parent of the next, as the log of a seed of three values.
	logOf := func(objects ...*CommitObject) *seed {
		s := &seed{commits: map[string]*CommitObject{}, values: 3}
		parent := format.EmptyLog.String()
		for i, o := range objects {
			o.Name, o.Parent = fmt.Sprintf("%064x", i+1), parent
			s.commits[o.Name] = o
			parent = o.Name
		}
		s.manifest = &ManifestObject{Head: parent}
		return s
	}

	t.Run("should take a log that deletes rows that it holds", func(t *testing.T) {
		s := logOf(&CommitObject{Keys: []int64{barcode(1)}}, &CommitObject{Keys: []int64{barcode(2)}}, &CommitObject{Deletes: []int64{barcode(1)}})
		is.NotError(t, s.readLog())
		is.Equal(t, 3, len(s.log))
	})

	tests := []struct {
		name    string
		objects []*CommitObject
	}{
		{"a delete of a row that the log never held", []*CommitObject{{Keys: []int64{barcode(1)}}, {Deletes: []int64{barcode(2)}}}},
		{"a delete of a row before its insert", []*CommitObject{{Deletes: []int64{barcode(1)}}, {Keys: []int64{barcode(1)}}}},
		{"a delete of a row twice", []*CommitObject{{Keys: []int64{barcode(1)}}, {Deletes: []int64{barcode(1)}}, {Deletes: []int64{barcode(1)}}}},
	}
	for _, test := range tests {
		t.Run("should refuse "+test.name, func(t *testing.T) {
			err := logOf(test.objects...).readLog()
			is.True(t, err != nil && strings.Contains(err.Error(), "which it does not hold there"), fmt.Sprint(err))
		})
	}
}
