//go:build s4ql_browser

package simulator

import (
	"fmt"
	"testing"

	"maragu.dev/is"
)

func TestBarcode(t *testing.T) {
	t.Run("should give a distinct barcode of 13 digits with the prefix 57 for each of the first values", func(t *testing.T) {
		seen := map[int64]bool{}
		for n := int64(1); n <= 200_000; n++ {
			k := barcode(n)
			is.True(t, k >= 5_700_000_000_000 && k < 5_800_000_000_000, fmt.Sprint("a barcode with the prefix 57: ", k))
			if seen[k] {
				t.Fatal("a barcode twice:", k, "for", n)
			}
			seen[k] = true
		}
	})

	t.Run("should give the barcode of a value the same every time", func(t *testing.T) {
		is.Equal(t, barcode(1), barcode(1))
		is.Equal(t, barcode(10_000_000_001), barcode(1))
	})
}
