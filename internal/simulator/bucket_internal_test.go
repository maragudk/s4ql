//go:build s4ql_browser

package simulator

import (
	"testing"

	"maragu.dev/is"
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
