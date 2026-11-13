package token

import (
	"slices"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

func TestKinds(t *testing.T) {
	t.Run("should end with Without, which the external tests take as the last kind", func(t *testing.T) {
		is.Equal(t, kinds, Without+1)
	})

	t.Run("should name every kind", func(t *testing.T) {
		for k := range kinds {
			is.True(t, names[k] != "", k.String())
		}
	})
}

func TestKeywordTable(t *testing.T) {
	t.Run("should hold exactly the keywords of SQLite, each with a keyword kind", func(t *testing.T) {
		want := sqlite.Keywords()
		slices.Sort(want)
		var got []string
		for word, kind := range keywords {
			got = append(got, word)
			is.True(t, kind.IsKeyword(), word)
		}
		slices.Sort(got)
		is.EqualSlice(t, want, got)
	})
}
