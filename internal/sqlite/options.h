/*
** Compile-time options for SQLite, shared by the binding in this directory and by the sqlite3 shell
** that the tests build from the same vendored source.
**
** The rule for adding an option: it implements a rule in docs/spec.md or a restriction in
** docs/roadmap.md, it makes misuse of SQLite or of an obsolete feature impossible or detectable, it
** makes this build match what a normal build of SQLite has, or the build needs it. Options that only
** tune performance are left out, because every option is a way in which this build differs from a
** stock build of SQLite. The tests check that SQLite reports every option defined here.
*/
#ifndef S4QL_OPTIONS_H
#define S4QL_OPTIONS_H

/*
** Double-quoted strings are identifiers only, in DDL and DML. Without this, "abc" falls back to a
** string literal when no column of that name exists. The roadmap rejects double-quoted strings. One
** exception remains: after DEFAULT in a column definition, SQLite reads any identifier, quoted or
** not, as a string.
*/
#define SQLITE_DQS 0

/*
** Foreign keys are enforced on every new connection. The roadmap enforces them always, and this
** removes the need to remember PRAGMA foreign_keys=ON on each connection.
*/
#define SQLITE_DEFAULT_FOREIGN_KEYS 1

/*
** Serialized threading mode, which is also SQLite's default, stated here so that the choice is
** visible. It protects SQLite's own data structures if a connection is ever used from two goroutines
** by mistake. It does not make such use correct: an error message, for example, can still belong to
** the other goroutine's call.
*/
#define SQLITE_THREADSAFE 1

/*
** Extra checks that turn some misuse of the C API, such as NULL pointers or objects used after they
** are destroyed, into SQLITE_MISUSE errors instead of undefined behaviour. They do not change the
** result of any SQL.
*/
#define SQLITE_ENABLE_API_ARMOR 1

/*
** Stepping a statement that has finished, or that failed with anything but SQLITE_BUSY or
** SQLITE_LOCKED, returns SQLITE_MISUSE, instead of resetting the statement and running it again
** without being asked. A stray extra step then cannot repeat an insert.
*/
#define SQLITE_OMIT_AUTORESET 1

/*
** Remove shared-cache mode, which SQLite calls obsolete and discourages, and which changes locking
** and isolation (it allows read-uncommitted).
*/
#define SQLITE_OMIT_SHARED_CACHE 1

/*
** Remove loading of extensions from shared libraries, including the load_extension() SQL function.
** The binding has no way to load an extension, and without this the build also needs the dynamic
** loader (-ldl on Linux).
*/
#define SQLITE_OMIT_LOAD_EXTENSION 1

/*
** Memory-mapped I/O on the database file can never be switched on: PRAGMA mmap_size cannot raise the
** limit above 0. With it on, an I/O error on the file arrives as a signal that kills the process
** instead of as an error code (https://sqlite.org/mmap.html), and local I/O failures must surface as
** errors. It does not cover the wal-index (-shm) file, which SQLite always memory-maps in WAL mode.
*/
#define SQLITE_MAX_MMAP_SIZE 0

/*
** The math functions, such as sqrt() and pow(). SQLite built with its configure script on unix has
** them, but the amalgamation does not, so without this the oracle would differ from a normal build of
** SQLite.
*/
#define SQLITE_ENABLE_MATH_FUNCTIONS 1

/*
** An application-defined function that sets a result subtype without declaring SQLITE_RESULT_SUBTYPE
** fails with an error, instead of sometimes losing the subtype. It is on SQLite's recommended and
** always-tested list (https://sqlite.org/compile.html#rcmd), and it affects application-defined
** functions only, so it costs nothing while there are none.
*/
#define SQLITE_STRICT_SUBTYPE 1

/*
** Considered and left out:
**
** - SQLITE_OMIT_DEPRECATED, although it is on the recommended list: the binding calls no deprecated
**   function, and on the amalgamation it does not remove all the deprecated pragmas the docs say it
**   removes (count_changes, full_column_names and short_column_names still take effect).
** - SQLITE_OMIT_UTF16: it is outside SQLite's tested set, and an existing UTF-16 database file then
**   fails as a malformed schema, a corruption error, instead of being rejected clearly.
** - SQLITE_DEBUG: its assertions check SQLite's own invariants, which SQLite's developers already
**   test, and the project has no test-only build variant.
**
** Research on every option on https://sqlite.org/compile.html is in
** docs/research/sqlite-compile-options.md.
*/

#endif
