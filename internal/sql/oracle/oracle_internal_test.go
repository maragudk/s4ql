package oracle

import (
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sqlite"
)

// These cases cannot be reached through [Judge] without texts of a gigabyte or a failing connection,
// so they give classify the result that SQLite would give.
func TestSchema_classify(t *testing.T) {
	t.Run("should not say for a result code that the parser does not give", func(t *testing.T) {
		var s schema
		r := result{err: &sqlite.Error{Code: 18, Message: "statement too long", Offset: -1}, tail: -1}
		v := s.classify("select 1", r)
		is.Equal(t, NotSaid, v.Kind)
		is.Equal(t, NotParsed, v.Reason)

		r = result{err: &sqlite.Error{Code: 18, Message: "string or blob too big", Offset: -1}, tail: 4}
		v = s.classify("select 1", r)
		is.Equal(t, NotSaid, v.Kind)
		is.Equal(t, NotParsed, v.Reason)
	})

	t.Run("should not say for a syntax message with a result code other than SQLITE_ERROR", func(t *testing.T) {
		var s schema
		r := result{err: &sqlite.Error{Code: 23, Message: "incomplete input", Offset: -1}, tail: 8}
		v := s.classify("select 1", r)
		is.Equal(t, NotSaid, v.Kind)
		is.Equal(t, NotParsed, v.Reason)
	})

	t.Run("should not say when the connection or the setup failed", func(t *testing.T) {
		var s schema
		v := s.classify("select 1", result{failed: true})
		is.Equal(t, NotSaid, v.Kind)
		is.Equal(t, NotParsed, v.Reason)
		is.Equal(t, -1, v.Tail)
	})
}
