package oracle

import (
	"errors"
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

// TestSchema_prepare measures the behaviour of SQLite that prepare works around: on a fresh
// connection, the error of a variable #N raised with the end of a SELECT, CREATE VIEW, ATTACH, or
// DETACH has no offset, because the action of the command loads the schema, and the nested SQL of
// that load clears the offset. On the connection of prepare, whose schema is loaded, the error has
// the offset of the token, and so has every error that SQLite raises on the end of the text with
// the offset of a token. The statements of a setup load the schema themselves.
func TestSchema_prepare(t *testing.T) {
	t.Run("should give the offset of the token on a connection whose schema is loaded", func(t *testing.T) {
		aux := object{kind: database, raw: "aux", name: "aux"}
		tests := []struct {
			src           string
			setup         []object
			fresh, offset int
		}{
			{src: "select #1", fresh: -1, offset: 7},
			{src: "create view v as select #1", fresh: -1, offset: 24},
			{src: "detach #1", fresh: -1, offset: 7},
			{src: "select 1 where #1", fresh: -1, offset: 15},
			{src: "with c as (select 1) select #2", fresh: -1, offset: 28},
			{src: "explain select #1", fresh: -1, offset: 15},
			{src: "explain query plan select #1", fresh: -1, offset: 26},
			{src: "create temp view v as select #1", fresh: -1, offset: 29},
			{src: "attach 'x' as #1", fresh: -1, offset: 14},
			{src: "attach #1 as a", fresh: 7, offset: 7}, // raised on AS, before the end
			{src: "select #1;", fresh: -1, offset: 7},
			{src: "select #1; select 2", fresh: -1, offset: 7},
			{src: "select #1 ", fresh: -1, offset: 7},
			{src: "values(0),(a) union select #0", fresh: -1, offset: 27},
			{src: "values(0),(0) union select #0", fresh: 27, offset: 27}, // the rows load the schema while SQLite parses
			{src: "create view v as with c as (select 1) values(0),(0) union select #0", fresh: -1, offset: 65},
			{src: "select ?0", fresh: -1, offset: 7},
			{src: "delete from t where #1", fresh: 20, offset: 20},
			{src: "create view main.v as select * from t where #1", setup: []object{{kind: table, name: "t"}}, fresh: 44, offset: 44},
			{src: "create trigger r after insert on t begin select #1; end", setup: []object{{kind: table, name: "t"}}, fresh: 48, offset: 48},
			{src: "select * from v where #1", setup: []object{{kind: view, name: "v"}}, fresh: 22, offset: 22},
			{src: "select * from temp.t where #1", setup: []object{{kind: table, database: "temp.", name: "t"}}, fresh: 27, offset: 27},
			{src: "create view aux.v as select #1", setup: []object{aux}, fresh: 28, offset: 28},
			{src: "select * from aux.t where #1", setup: []object{aux, {kind: table, database: "aux.", name: "t"}}, fresh: 26, offset: 26},
		}
		for _, test := range tests {
			s := schema{objects: test.setup}
			fresh := s.prepareFresh(t, test.src)
			r := s.prepare(test.src)
			is.True(t, !r.failed, "failed: "+test.src)
			is.True(t, r.err != nil, "prepared: "+test.src)
			is.Equal(t, fresh.err.Message, r.err.Message, "message: "+test.src)
			is.Equal(t, fresh.err.Code, r.err.Code, "code: "+test.src)
			is.Equal(t, fresh.tail, r.tail, "tail: "+test.src)
			is.Equal(t, test.fresh, fresh.err.Offset, "offset on a fresh connection: "+test.src)
			is.Equal(t, test.offset, r.err.Offset, "offset: "+test.src)
		}
	})

	t.Run("should give another message for a #N at the end in the empty database, which the setup fixes", func(t *testing.T) {
		// So a verdict on such a #N found with setup is not SQLite's verdict on the empty database.
		var s schema
		r := s.prepare("create view aux.v as select #1")
		is.Equal(t, "unknown database aux", r.err.Message)
		is.Equal(t, 30, r.tail)
	})
}

// prepareFresh src on a new connection with the setup of the schema, as prepare does but without
// loading the schema after the setup.
func (s *schema) prepareFresh(t *testing.T, src string) result {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	defer func() { _ = c.Close() }()
	for _, statement := range s.setup() {
		is.NotError(t, c.Exec(statement))
	}
	_, tail, err := c.PrepareFirst(src)
	var sqliteErr *sqlite.Error
	is.True(t, errors.As(err, &sqliteErr), "no error")
	return result{err: sqliteErr, tail: tail}
}
