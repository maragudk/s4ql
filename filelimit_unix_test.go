//go:build unix

package s4ql_test

import (
	"syscall"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql"
)

func TestFileLimit(t *testing.T) {
	t.Run("should give the soft limit of open files of the process, which Go raised to a finite number when the process started", func(t *testing.T) {
		var r syscall.Rlimit
		is.NotError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r))

		limit, ok := s4ql.FileLimit()
		is.True(t, ok, "no limit")
		is.Equal(t, uint64(r.Cur), limit)
		is.True(t, s4ql.MaxOpenConns(limit, ok) > 0)
	})
}
