// Package crosscheck holds tests that compare the binding in maragu.dev/s4ql/internal/sqlite with
// github.com/mattn/go-sqlite3, which bundles the same SQLite C code. Any difference between the two is a
// bug in one of the bindings, not in SQLite.
//
// It is a separate module so that github.com/mattn/go-sqlite3 stays out of the main module's
// dependencies. Its tests link both copies of SQLite into one binary, which only works because the
// binding compiles its copy with file-local symbols.
package crosscheck
