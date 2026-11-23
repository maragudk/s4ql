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

func TestDropReturns(t *testing.T) {
	t.Run("should leave an insert, an update, and a delete with RETURNING holding none of its rows, and changing as many", func(t *testing.T) {
		for _, src := range []string{"insert into t (k, v) values (3, 30) returning *", "update t set v = 5 returning k", "delete from t returning v"} {
			for _, drop := range []bool{false, true} {
				f := newFake().with("t", []string{"k", "v"}, []int64{1, 10}, []int64{2, 20})
				s, err := stmt.Check(src)
				is.NotError(t, err, src)
				k, err := buildSink(t.Context(), fakeTx{f}, NewPlan(s), nil, bugs{}, 1_000_000_000, reading{})
				is.NotError(t, err, src)
				is.True(t, returnsOf(k) != nil, src)
				if drop {
					dropReturns(k)
					is.True(t, returnsOf(k) == nil, src)
				}
				is.NotError(t, k.check(t.Context()), src)
				n, err := k.run(t.Context())
				is.NotError(t, err, src)
				if drop {
					is.True(t, returnsOf(k) == nil, src)
					continue
				}
				is.Equal(t, n, int64(len(returnsOf(k).rows)), src)
			}
		}
	})
}
