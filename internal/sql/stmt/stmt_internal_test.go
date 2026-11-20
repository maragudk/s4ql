package stmt

import (
	"os"
	"testing"

	"maragu.dev/is"
)

func TestNotSupportedError(t *testing.T) {
	t.Run("should match ErrNotSupported and no other error", func(t *testing.T) {
		err := &NotSupportedError{What: "x", Offset: 3}
		is.True(t, err.Is(ErrNotSupported))
		is.True(t, !err.Is(os.ErrNotExist))
		is.Equal(t, "s4ql: not supported: x (at offset 3)", err.Error())
	})
}

func TestStatement(t *testing.T) {
	t.Run("should give the span that it was given, for every kind of statement", func(t *testing.T) {
		for _, s := range []Statement{&CreateTable{}, &Insert{}, &Select{}, &Update{}, &Delete{}, &DropTable{}} {
			s.setSpan(Span{1, 2})
			is.Equal(t, Span{1, 2}, s.Bounds())
		}
	})
}
