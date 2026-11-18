package stmt

import (
	"fmt"
	"testing"

	"maragu.dev/is"
)

func TestCheck_lengthLimit(t *testing.T) {
	t.Run("should not support a text longer than SQLite's limit, whatever it holds", func(t *testing.T) {
		defer func(n int) { lengthLimit = n }(lengthLimit)
		lengthLimit = 20
		_, err := Check("select * from t;    ")
		is.NotError(t, err)
		for _, src := range []string{"select * from t;     ", "selec * from t\x00      "} {
			_, err = Check(src)
			is.Equal(t, "s4ql: not supported: text longer than SQLite's limit (at offset 20)", fmt.Sprint(err), src)
		}
	})
}
