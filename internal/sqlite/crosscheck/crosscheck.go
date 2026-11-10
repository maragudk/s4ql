// Package crosscheck holds tests that compare the binding in maragu.dev/s4ql/internal/sqlite with
// github.com/mattn/go-sqlite3, which bundles the same SQLite C code. The two copies are built with
// different compile options, so the tests compare only what those options do not change. Within that,
// any difference between the two is a bug in one of the bindings, not in SQLite.
//
// It is a separate module so that github.com/mattn/go-sqlite3 stays out of the main module's
// dependencies. Its tests link both copies of SQLite into one binary, which works only if one of them
// keeps its symbols local.
package crosscheck
