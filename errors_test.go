package s4ql_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql"
)

func TestError(t *testing.T) {
	t.Run("should give SQLite's error with its result code, its extended result code, and its message, however the statement is run", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)

		cases := []struct {
			name string
			src  string
			want s4ql.Error
		}{
			{name: "a key that exists", src: insert1,
				want: s4ql.Error{Code: 19, ExtendedCode: 1555, Message: "UNIQUE constraint failed: t1.v1"}},
			{name: "a table that exists", src: `create table "T1" (x int primary key) strict, without rowid`,
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: `table "T1" already exists`}},
			{name: "no table to read", src: "select * from t2",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: "no such table: t2"}},
			{name: "no table to write", src: "insert into t2 (v1) values (1)",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: "no such table: t2"}},
			{name: "no table to drop", src: "drop table t2",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: "no such table: t2"}},
			{name: "no column", src: "insert into t1 (v2) values (1)",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: "table t1 has no column named v2"}},
			{name: "a syntax error", src: "select * frm t1",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: `near "frm": syntax error`}},
			{name: "a syntax error in SQL that is not supported", src: "select * from t1 where",
				want: s4ql.Error{Code: 1, ExtendedCode: 1, Message: "incomplete input"}},
		}
		for _, c := range cases {
			check := func(how string, err error) {
				t.Helper()
				got, ok := errors.AsType[*s4ql.Error](failed(t, err))
				is.True(t, ok, c.name+", "+how+": "+err.Error())
				is.Equal(t, c.want, *got, c.name+", "+how)
				is.Equal(t, c.want.Message, err.Error(), c.name+", "+how)
				for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped} {
					is.True(t, !errors.Is(err, sentinel), c.name+", "+how+": "+sentinel.Error())
				}
			}
			_, err := db.ExecContext(t.Context(), c.src)
			check("Exec", err)
			_, err = db.QueryContext(t.Context(), c.src)
			check("Query", err)
			check("QueryRow", db.QueryRowContext(t.Context(), c.src).Scan(new(int64)))

			// A syntax error comes when the statement is prepared, and every other error when it is run.
			prepared, err := db.PrepareContext(t.Context(), c.src)
			if err != nil {
				is.Equal(t, 1, c.want.Code, c.name)
				check("Prepare", err)
				continue
			}
			_, err = prepared.ExecContext(t.Context())
			check("Stmt.Exec", err)
			_, err = prepared.QueryContext(t.Context())
			check("Stmt.Query", err)
			is.NotError(t, prepared.Close())
		}
		is.EqualSlice(t, []int64{1}, values(t, db, select1))
		readersFree(t, db)
	})

	t.Run("should keep an error of the context apart from the errors of the instance", func(t *testing.T) {
		w := newWorld(t)
		db := w.serve(t, "a")
		exec(t, db, t1, insert1)

		// The context ends while the rows are read: database/sql gives its error for the rows.
		ctx, cancel := context.WithCancel(t.Context())
		rows, err := db.QueryContext(ctx, select1)
		is.NotError(t, err)
		cancel()
		for rows.Next() {
		}
		err = failed(t, rows.Err())
		is.Error(t, context.Canceled, err)
		for _, sentinel := range []error{s4ql.ErrNotSupported, s4ql.ErrNotServing, s4ql.ErrOutcomeUnknown, s4ql.ErrStopped, sql.ErrNoRows} {
			is.True(t, !errors.Is(err, sentinel), sentinel.Error())
		}
		readersFree(t, db)
	})
}
