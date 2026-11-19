//go:build unix

package s4ql_test

import (
	"math"
	"syscall"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql"
)

func TestFileLimit(t *testing.T) {
	t.Run("should give the soft limit of open files of the process, and none if it is unlimited", func(t *testing.T) {
		var r syscall.Rlimit
		is.NotError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r))
		// Go raises the soft limit when the process starts, on Linux and Darwin to a finite number,
		// and on a system whose hard limit is unlimited to unlimited.
		unlimited := uint64(r.Cur) == uint64(syscall.RLIM_INFINITY&math.MaxUint64)

		limit, ok := s4ql.FileLimit()
		is.Equal(t, !unlimited, ok)
		if ok {
			is.Equal(t, uint64(r.Cur), limit)
			is.True(t, s4ql.MaxOpenConns(limit, ok) > 0)
		}
	})
}
