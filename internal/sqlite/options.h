/*
** Compile-time options for SQLite, shared by the binding in this directory and by the sqlite3 shell
** that the tests build from the same vendored source.
**
** The rule for adding an option: it implements a restriction in docs/roadmap.md, it makes misuse of
** SQLite or of an obsolete feature impossible or detectable, or the build needs it. Options that only
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

#endif
