/*
** Compile-time options for SQLite, shared by the binding in this directory and by the sqlite3 shell
** that the tests build from the same vendored source.
**
** The rule for adding an option: it implements a restriction in docs/roadmap.md, it makes misuse of
** SQLite or of an obsolete feature impossible or detectable, or the build needs it. Options that only
** tune performance are left out, because every option is a way in which the oracle differs from a
** stock build of SQLite. Change this list only together with the expected list in sqlite_test.go.
*/
#ifndef S4QL_OPTIONS_H
#define S4QL_OPTIONS_H

/*
** Double-quoted strings are identifiers only, in DDL and DML. Without this, "abc" falls back to a
** string literal when no column of that name exists. The roadmap rejects double-quoted strings.
*/
#define SQLITE_DQS 0

/*
** Foreign keys are enforced on every new connection. The roadmap enforces them always, and this
** removes the need to remember PRAGMA foreign_keys=ON on each connection.
*/
#define SQLITE_DEFAULT_FOREIGN_KEYS 1

/*
** Serialized threading mode, which is also SQLite's default, stated here so that the choice is
** visible. Go moves goroutines between OS threads, and serialized mode keeps SQLite's own data safe
** even if a connection is ever used from two goroutines by mistake.
*/
#define SQLITE_THREADSAFE 1

/*
** Extra checks that turn some misuse of the C API, such as NULL pointers or objects used after they
** are destroyed, into SQLITE_MISUSE errors instead of undefined behaviour. They do not change the
** result of any SQL.
*/
#define SQLITE_ENABLE_API_ARMOR 1

/*
** Remove shared-cache mode, which SQLite calls obsolete and discourages, and which changes locking
** and isolation (it allows read-uncommitted).
*/
#define SQLITE_OMIT_SHARED_CACHE 1

/*
** Remove loading of extensions from shared libraries, including the load_extension() SQL function.
** Neither the cache nor the oracle loads extensions, and without this the build also needs the
** dynamic loader (-ldl on Linux).
*/
#define SQLITE_OMIT_LOAD_EXTENSION 1

#endif
