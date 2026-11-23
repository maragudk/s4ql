package engine

import (
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestCheckType(t *testing.T) {
	table := func(typ s4qlpb.Type) *s4qlpb.CreateTable {
		return s4qlpb.CreateTable_builder{Table: new("t"), Columns: []*s4qlpb.Column{
			s4qlpb.Column_builder{Name: new("v"), Type: typ.Enum()}.Build(),
		}}.Build()
	}
	text := s4qlpb.Value_builder{Text: new("a")}.Build()

	t.Run("should give an internal error for a column of a type that no plan gives", func(t *testing.T) {
		err := checkType(table(s4qlpb.Type_TYPE_UNSPECIFIED), 0, text, &Text{Value: "a"}, "for")
		is.True(t, errors.Is(err, ErrInternal), err.Error())
		is.True(t, !errors.Is(err, stmt.ErrNotSupported), err.Error())
		is.Equal(t, "s4ql: internal error: a column of type TYPE_UNSPECIFIED", err.Error())
	})

	t.Run("should give an internal error for a value of another type from an expression that no plan gives", func(t *testing.T) {
		err := checkType(table(s4qlpb.Type_TYPE_INT), 0, text, nil, "compared with")
		is.True(t, errors.Is(err, ErrInternal), err.Error())
		is.True(t, !errors.Is(err, stmt.ErrNotSupported), err.Error())
		is.Equal(t, "s4ql: internal error: a value of an expression of type <nil>", err.Error())
	})
}

func TestLimitCount(t *testing.T) {
	t.Run("should give an internal error for a LIMIT of a literal that is no integer from 0, which no plan gives", func(t *testing.T) {
		v := s4qlpb.Value_builder{Integer: new(int64(-1))}.Build()
		n, err := limitCount(&Integer{Value: -1}, v)
		is.Equal(t, int64(0), n)
		is.True(t, errors.Is(err, ErrInternal), err.Error())
		is.True(t, !errors.Is(err, stmt.ErrNotSupported), err.Error())
		is.Equal(t, "s4ql: internal error: a LIMIT of an expression of type *engine.Integer, which is a negative integer", err.Error())
	})

	t.Run("should give no number with the refusal of an argument", func(t *testing.T) {
		n, err := limitCount(&Parameter{Number: 2}, s4qlpb.Value_builder{Real: new(2.0)}.Build())
		is.Equal(t, int64(0), n)
		is.Equal(t, "s4ql: not supported: argument 2, which is a real, for LIMIT, which takes an integer from 0", err.Error())
	})
}

func TestKindName(t *testing.T) {
	t.Run("should name a value of no kind as one whose kind is not set", func(t *testing.T) {
		is.Equal(t, "a value of kind not set", kindName(&s4qlpb.Value{}))
	})
}
