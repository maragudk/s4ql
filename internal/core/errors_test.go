package core_test

import (
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"
	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sqlite"
)

func TestClassify(t *testing.T) {
	t.Run("should count as a local I/O failure the result codes of SQLite that say so, with any extended code, and busy as busy", func(t *testing.T) {
		kinds := map[int]string{
			3: "local", 7: "local", 8: "local", 10: "local", 11: "local", 13: "local", 14: "local", 15: "local", 22: "local", 26: "local",
			5: "busy",
		}
		for primary := range 29 {
			want, ok := kinds[primary]
			if !ok {
				want = "other"
			}
			for _, extended := range []int{0, 1, 2, 5, 9} {
				code := primary | extended<<8
				err := fmt.Errorf("wrapped: %w", &sqlite.Error{Code: sqlite.ResultCode(code), Message: "test"})
				is.Equal(t, want, core.Classify(err), fmt.Sprint(code))
			}
		}
		is.Equal(t, "other", core.Classify(errors.New("no error of SQLite")))
		is.Equal(t, "other", core.Classify(nil))
	})
}
