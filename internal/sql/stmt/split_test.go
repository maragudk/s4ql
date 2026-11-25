package stmt_test

import (
	"errors"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// texts of the parts of src that Split gives, each with the text of its statement after a "|".
func partTexts(src string, parts []stmt.Part) []string {
	var texts []string
	for _, p := range parts {
		span := p.Statement.Bounds()
		texts = append(texts, src[p.Span.Start:p.Span.End]+"|"+src[span.Start:span.End])
	}
	return texts
}

func TestSplit(t *testing.T) {
	t.Run("should give the one statement of a text as Check gives it, with the whole text as its part", func(t *testing.T) {
		for _, src := range []string{"select * from t", "  -- c\n ; select * from t ; -- d\n;", "insert into t (v) values (?)"} {
			parts, err := stmt.Split(src)
			is.NotError(t, err)
			is.Equal(t, 1, len(parts))
			is.Equal(t, stmt.Span{Start: 0, End: len(src)}, parts[0].Span)
			s, err := stmt.Check(src)
			is.NotError(t, err)
			is.Equal(t, fmt.Sprintf("%#v", s), fmt.Sprintf("%#v", parts[0].Statement))
		}
	})

	t.Run("should give each statement of a text, with empty statements, spaces, and comments in the part of the statement after them, and offsets in the whole text", func(t *testing.T) {
		src := "create table t (v int primary key) strict, without rowid; ;-- c\nupdate t set v = ? where v = ?;\n\tselect * from t where v = ? /* d */ ;; -- e"
		parts, err := stmt.Split(src)
		is.NotError(t, err)
		is.EqualSlice(t, []string{
			"create table t (v int primary key) strict, without rowid;|create table t (v int primary key) strict, without rowid;",
			" ;-- c\nupdate t set v = ? where v = ?;|update t set v = ? where v = ?;",
			"\n\tselect * from t where v = ? /* d */ ;; -- e|select * from t where v = ? /* d */ ;",
		}, partTexts(src, parts))
		is.Equal(t, 0, parts[0].Statement.Parameters())
		is.Equal(t, 2, parts[1].Statement.Parameters())
		is.Equal(t, 1, parts[2].Statement.Parameters())
		is.Equal(t, "CREATE TABLE t (v int primary key) strict, without rowid", parts[0].Statement.(*stmt.CreateTable).SQL)
	})

	t.Run("should give no part and no error for a text without a statement", func(t *testing.T) {
		for _, src := range []string{"", " ; -- c\n;", "/* c */"} {
			parts, err := stmt.Split(src)
			is.NotError(t, err)
			is.Equal(t, 0, len(parts))
		}
	})

	t.Run("should give the statements before the first that fails to check, and its error, with the offset in the whole text", func(t *testing.T) {
		tests := []struct {
			src   string
			parts int
			err   string
		}{
			{src: "drop table t; selec * from t; drop table u", parts: 1, err: `near "selec": syntax error`},
			{src: "drop table t; drop table u; drop view v; drop table w", parts: 2, err: "s4ql: not supported: DROP VIEW (at offset 33)"},
			{src: "drop table t; select * from t group by v", parts: 1, err: "s4ql: not supported: GROUP BY clause (at offset 30)"},
			{src: "drop table t; begin; drop table u", parts: 1, err: "s4ql: not supported: BEGIN (at offset 14)"},
			{src: "selec; drop table t", parts: 0, err: `near "selec": syntax error`},
			{src: "drop table t; insert into", parts: 1, err: "incomplete input"},
		}
		for _, test := range tests {
			parts, err := stmt.Split(test.src)
			is.Equal(t, test.parts, len(parts), test.src)
			is.True(t, err != nil, test.src)
			is.Equal(t, test.err, err.Error(), test.src)
		}
		_, err := stmt.Split("drop table t; selec * from t")
		var se *sqlerr.Error
		is.True(t, errors.As(err, &se))
		is.Equal(t, sqlerr.Error{Code: 1, ExtendedCode: 1, Message: `near "selec": syntax error`, Offset: 14}, *se)
	})

	t.Run("should refuse a text that is not valid UTF-8 or holds a NUL byte as a whole, before any statement", func(t *testing.T) {
		for src, want := range map[string]string{
			"drop table t; drop table u\x00": "s4ql: not supported: NUL byte (at offset 26)",
			"drop table t; drop table \xff":  "s4ql: not supported: invalid UTF-8 (at offset 25)",
		} {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts))
			is.True(t, err != nil)
			is.Equal(t, want, err.Error())
		}
	})

	t.Run("should refuse a space after a statement that SQLite reads as part of a token, which Go's TrimSpace removes, before any statement", func(t *testing.T) {
		for src, want := range map[string]string{
			"drop table t; drop table u":                    "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 13)",
			"drop table t;   drop table u":                  "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 14)",
			"drop table t;\vdrop table u":                   "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 13)",
			"drop table t; drop table u;\u0085drop table w": "s4ql: not supported: space after a statement that SQLite reads as part of a token (at offset 27)",
		} {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts), src)
			is.True(t, err != nil, src)
			is.Equal(t, want, err.Error(), src)
		}
	})

	t.Run("should refuse a space at the end of a text of more than one statement that SQLite reads as part of a token, before any statement", func(t *testing.T) {
		for src, want := range map[string]string{
			"drop table t; ":                  "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 13)",
			"drop table t; drop table u  ":    "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 27)",
			"drop table t; /* c */ ":          "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 21)",
			"drop table t; select 'abc  ":     "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 25)",
			"drop table t; /* ":               "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 16)",
			"drop table t; drop table \"u \n": "s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 27)",
		} {
			parts, err := stmt.Split(src)
			is.Equal(t, 0, len(parts), src)
			is.True(t, err != nil, src)
			is.Equal(t, want, err.Error(), src)
		}
	})

	t.Run("should take spaces that SQLite reads as spaces, and comments that end in spaces, at the end and between statements", func(t *testing.T) {
		for _, src := range []string{
			"drop table t; \t\r\n\f drop table u \t\r\n\f\v",
			"drop table t; drop table u; -- c \t\v ",
			"drop table t; drop table u; /* c  \n",
			"drop table t; drop table u; --  ",
			"drop table t;\ufeffdrop table u",
		} {
			parts, err := stmt.Split(src)
			is.NotError(t, err, src)
			is.Equal(t, 2, len(parts), src)
		}
	})

	t.Run("should end the text of a statement after the first at the last character that is not a space, as mattn's driver hands it to SQLite", func(t *testing.T) {
		src := "drop table t; create table u (v int primary key) strict, without rowid \n\t "
		parts, err := stmt.Split(src)
		is.NotError(t, err)
		is.Equal(t, "CREATE TABLE u (v int primary key) strict, without rowid", parts[1].Statement.(*stmt.CreateTable).SQL)
		is.Equal(t, stmt.Span{Start: 13, End: len(src)}, parts[1].Span)

		// The first statement is handed over as the text is.
		src = "create table u (v int primary key) strict, without rowid \n "
		parts, err = stmt.Split(src)
		is.NotError(t, err)
		is.Equal(t, "CREATE TABLE u (v int primary key) strict, without rowid \n ", parts[0].Statement.(*stmt.CreateTable).SQL)
	})
}
