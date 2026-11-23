package engine

import (
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestBuildReturning(t *testing.T) {
	t.Run("should give nil for a statement without RETURNING, and an internal error for a RETURNING of no columns, which no statement gives", func(t *testing.T) {
		ct := newFake().with("t", []string{"k", "v"}).tables["t"].ct

		ret, err := buildReturning(ct, nil, bugs{})
		is.NotError(t, err)
		is.True(t, ret == nil)

		for _, r := range []*Returning{{}, {Columns: []stmt.Name{}}} {
			ret, err = buildReturning(ct, r, bugs{})
			is.True(t, errors.Is(err, ErrInternal), fmt.Sprint(err))
			is.Equal(t, "s4ql: internal error: a RETURNING of no columns", err.Error())
			is.True(t, ret == nil)
		}
	})
}
