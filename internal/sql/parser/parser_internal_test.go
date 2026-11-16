package parser

import (
	"strings"
	"testing"

	"maragu.dev/is"
)

func TestParse_length(t *testing.T) {
	t.Run("should fail on a statement longer than the limit, spaces included", func(t *testing.T) {
		defer func(l int) { lengthLimit = l }(lengthLimit)
		lengthLimit = 100
		_, _, err := Parse("select 1" + strings.Repeat(" ", 100))
		is.Equal(t, "s4ql: the statement is longer than the limit of 1000000000 bytes", err.Error())
		is.True(t, err.(*Error).Limit)
		_, _, err = Parse("select 1" + strings.Repeat(" ", 92))
		is.NotError(t, err)
	})
}
