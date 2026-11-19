package stmttest

import (
	"errors"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// KnownGap of the pass after the parser, which its README describes, that s4ql's answer got and
// SQLite's answer want for the text src fall in, or "". Both are messages of errors with the codes 1
// and 1. s4ql gives a syntax error, and SQLite another error of one of its grammar actions:
//
//   - SQLite does not say whether src parses, because the error of the action stopped it before the
//     end of the statement, also on an empty database, such as `unknown table option`.
//   - SQLite gives s4ql's syntax error for src on a database with the tables that src names, where
//     [oracle.Judge] runs it, and the error of the action is about what the database holds, such as
//     `no such table: main.X` for a trigger, which SQLite gives on an empty database too.
func KnownGap(src, got, want string) string {
	if !oracle.IsSyntaxMessage(got) || oracle.IsSyntaxMessage(want) {
		return ""
	}
	switch v := oracle.Judge(src); {
	case v.Kind == oracle.NotSaid && v.Reason != oracle.Unsettled && v.Reason != oracle.NotParsed:
		return "syntax error where SQLite does not say"
	case v.Kind == oracle.SyntaxError && v.Message == got:
		return "error about the database before a syntax error"
	}
	return ""
}

// ExistsAfterStart reports whether want, the message of SQLite's error with the codes 1 and 1 for
// the text src, is the one difference from SQLite that the tests accept for a text with a syntax
// error: src is a syntax error for [stmt.Check], and want is `table %s already exists`, with the
// name as written, for the table that [StartedTable] says that src starts. s4ql then gives the
// error that SQLite gives for src on an empty database, which the caller checks. The spec: "Text
// with a syntax error gets the error that SQLite gives for it on an empty database."
func ExistsAfterStart(src, want string) bool {
	_, err := stmt.Check(src)
	var se *sqlerr.Error
	if !errors.As(err, &se) {
		return false
	}
	n, ok := StartedTable(src, se)
	return ok && want == "table "+n.Raw+" already exists"
}
