// Package sqlite is a small cgo binding to the official SQLite C code vendored in the c directory,
// built with the compile options in options.h.
//
// It is deliberately small: opening and closing a database, through a VFS by its name if need be,
// finding and registering a VFS, preparing one statement, binding and reading integers, reals, text,
// blobs, and NULL, stepping, also with a context that interrupts the step when it ends, resetting, and
// finalizing, and reading how much memory SQLite uses. Each statement runs in its own transaction, as
// SQLite does outside BEGIN and COMMIT.
//
// SQLite's C code is compiled into this package with file-local symbols, so this package can share a
// binary with another copy of SQLite without the two clashing at link time. Two copies of SQLite in
// one process must never open the same database file: POSIX advisory locks belong to the process,
// and each copy tracks its own, so one copy can release the other's locks.
//
// [Conn] and [Stmt] are not safe for concurrent use.
package sqlite

/*
// SQLite's public functions and variables are made static, so they are local to this translation
// unit and cannot clash with another copy of SQLite in the same binary. Because of this, all C calls
// in this package must be in this file, and this file must not use //export, which would copy this
// preamble into a second translation unit. The one Go function that C calls, s4qlInterrupted, is
// exported in progress.go, whose preamble has declarations only.
#define SQLITE_API static

#include "options.h"
#include "source.h"
#include "c/sqlite3.c"

#cgo !darwin LDFLAGS: -lm

// s4ql_prepare the first statement of the n bytes at sql, and set *tail to the offset of the first
// byte after it, or to -1 when SQLite did not say where it stopped, as when it refused the text
// before parsing it.
static int s4ql_prepare(sqlite3 *db, const char *sql, int n, sqlite3_stmt **stmt, int *tail) {
	const char *end = 0;
	if (sql == 0) {
		sql = "";
	}
	int rc = sqlite3_prepare_v3(db, sql, n, 0, stmt, &end);
	*tail = end == 0 ? -1 : (int)(end - sql);
	return rc;
}

// s4ql_deny_process_pragmas is an authorizer that denies the PRAGMAs that change settings for the
// whole process, and allows everything else.
static int s4ql_deny_process_pragmas(void *arg, int action, const char *a, const char *b, const char *c, const char *d) {
	(void)arg; (void)b; (void)c; (void)d;
	if (action != SQLITE_PRAGMA || a == 0) {
		return SQLITE_OK;
	}
	static const char *const denied[] = {"soft_heap_limit", "hard_heap_limit", "temp_store_directory", "data_store_directory"};
	for (int i = 0; i < (int)(sizeof(denied) / sizeof(denied[0])); i++) {
		if (sqlite3_stricmp(a, denied[i]) == 0) {
			return SQLITE_DENY;
		}
	}
	return SQLITE_OK;
}

static int s4ql_set_deny_process_pragmas(sqlite3 *db) {
	return sqlite3_set_authorizer(db, s4ql_deny_process_pragmas, 0);
}

// s4ql_no_checkpoint_on_close on the connection: SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE.
static int s4ql_no_checkpoint_on_close(sqlite3 *db) {
	return sqlite3_db_config(db, SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE, 1, (int *)0);
}

// s4ql_order of the texts of two values, byte by byte with a prefix first: -1, 0, or 1. It sets
// *nomem if a text cannot be made.
static int s4ql_order(sqlite3_value *a, sqlite3_value *b, int *nomem) {
	const unsigned char *x = sqlite3_value_text(a);
	int nx = sqlite3_value_bytes(a);
	const unsigned char *y = sqlite3_value_text(b);
	int ny = sqlite3_value_bytes(b);
	if (x == 0 || y == 0) {
		*nomem = 1;
		return 0;
	}
	int n = nx < ny ? nx : ny;
	int c = n > 0 ? memcmp(x, y, (size_t)n) : 0;
	if (c == 0) {
		c = nx - ny;
	}
	return c < 0 ? -1 : c > 0;
}

// s4ql_test_order is the stub of regexp, match, and glob that s4ql_add_test_functions registers:
// NULL if an argument is NULL, else the order of the first argument to the second, and with a third
// argument 3 times that plus the order of the second to the third.
static void s4ql_test_order(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
	for (int i = 0; i < argc; i++) {
		if (sqlite3_value_type(argv[i]) == SQLITE_NULL) {
			sqlite3_result_null(ctx);
			return;
		}
	}
	int nomem = 0;
	int r = s4ql_order(argv[0], argv[1], &nomem);
	if (argc == 3) {
		r = 3 * r + s4ql_order(argv[1], argv[2], &nomem);
	}
	if (nomem) {
		sqlite3_result_error_nomem(ctx);
		return;
	}
	sqlite3_result_int(ctx, r);
}

static int s4ql_add_test_functions(sqlite3 *db) {
	static const struct {
		const char *name;
		int args;
	} fs[] = {{"regexp", 2}, {"regexp", 3}, {"match", 2}, {"match", 3}, {"glob", 3}};
	for (int i = 0; i < (int)(sizeof(fs) / sizeof(fs[0])); i++) {
		int rc = sqlite3_create_function_v2(db, fs[i].name, fs[i].args, SQLITE_UTF8 | SQLITE_DETERMINISTIC | SQLITE_INNOCUOUS, 0, s4ql_test_order, 0, 0, 0);
		if (rc != SQLITE_OK) {
			return rc;
		}
	}
	return SQLITE_OK;
}

// s4ql_bind_text binds the n bytes at text to parameter i as text. SQLite copies the bytes before it
// returns. A NULL pointer with n of zero binds the empty text, where sqlite3_bind_text64 would bind
// NULL.
static int s4ql_bind_text(sqlite3_stmt *stmt, int i, const char *text, sqlite3_uint64 n) {
	if (text == 0) {
		text = "";
	}
	return sqlite3_bind_text64(stmt, i, text, n, SQLITE_TRANSIENT, SQLITE_UTF8);
}

// s4ql_bind_blob binds the n bytes at blob to parameter i as a blob. SQLite copies the bytes before it
// returns. A NULL pointer with n of zero binds the empty blob, where sqlite3_bind_blob64 would bind
// NULL.
static int s4ql_bind_blob(sqlite3_stmt *stmt, int i, const void *blob, sqlite3_uint64 n) {
	if (blob == 0) {
		blob = "";
	}
	return sqlite3_bind_blob64(stmt, i, blob, n, SQLITE_TRANSIENT);
}

// s4ql_disable_lookaside on the connection, so that every allocation goes through malloc and free.
static int s4ql_disable_lookaside(sqlite3 *db) {
	return sqlite3_db_config(db, SQLITE_DBCONFIG_LOOKASIDE, (void *)0, 0, 0);
}

// s4qlInterrupted is in progress.go: nonzero once the context of the step that the handle h is for
// has ended.
extern int s4qlInterrupted(uintptr_t h);

// s4ql_progress_arg is what s4ql_progress reads: the statement of the step and the handle of what
// Go reads.
typedef struct s4ql_progress_arg {
	sqlite3_stmt *stmt;
	uintptr_t h;
} s4ql_progress_arg;

// s4ql_progress is the progress handler of a step of s4ql_step_interruptible. It stops the step
// only while the statement runs. SQLite calls the handler once more on its way out of a step, if it
// is due then, also of a step that has ended the statement or failed, and a handler that returns
// nonzero there turns the step's result into SQLITE_INTERRUPT: an I/O error, which has rolled back
// the whole transaction of the connection, would then look like a harmless stop. A statement that
// has ended, by its end or by an error, is no longer busy (https://sqlite.org/c3ref/stmt_busy.html),
// and the handler leaves its result as it is.
static int s4ql_progress(void *arg) {
	s4ql_progress_arg *a = arg;
	if (!sqlite3_stmt_busy(a->stmt)) {
		return 0;
	}
	return s4qlInterrupted(a->h);
}

// s4ql_step_interruptible steps stmt with s4ql_progress as the progress handler of its connection,
// with the handle h, which SQLite calls every period instructions of its virtual machine and which
// ends the step with SQLITE_INTERRUPT when it returns nonzero. The handler is removed before the
// function returns, so it runs in this step and in no other. It sets *left to 1 if the connection
// was in a transaction before the step and is not after it, and to 0 otherwise.
static int s4ql_step_interruptible(sqlite3_stmt *stmt, uintptr_t h, int period, int *left) {
	sqlite3 *db = sqlite3_db_handle(stmt);
	int inTx = !sqlite3_get_autocommit(db);
	s4ql_progress_arg arg = {stmt, h};
	sqlite3_progress_handler(db, period, s4ql_progress, &arg);
	int rc = sqlite3_step(stmt);
	sqlite3_progress_handler(db, 0, 0, 0);
	*left = inTx && sqlite3_get_autocommit(db);
	return rc;
}

// s4ql_tokenize the NUL-terminated SQL text at z with sqlite3GetToken, and write the type and the
// length of each token into types and lens, which have room for max tokens. It stops after the token
// at the NUL, which has length 0, and returns the number of tokens, or -1 if they do not fit.
static int s4ql_tokenize(const unsigned char *z, int *types, sqlite3_int64 *lens, int max) {
	for (int k = 0; k < max; k++) {
		i64 n = sqlite3GetToken(z, &types[k]);
		lens[k] = n;
		if (n == 0) {
			return k + 1;
		}
		z += n;
	}
	return -1;
}
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime/cgo"
	"strings"
	"unsafe"
)

// ResultCode is an extended result code from SQLite, as listed on https://sqlite.org/rescode.html.
type ResultCode int

// Primary result code, which is the lowest 8 bits of the extended result code.
func (c ResultCode) Primary() ResultCode {
	return c & 0xff
}

// Error from SQLite, with its extended result code, its message, and the byte offset in the SQL text
// of the token that the message names.
type Error struct {
	Code    ResultCode
	Message string
	// Offset in bytes, from the start of the SQL text, of the token that the message names, as
	// sqlite3_error_offset gives it. It is -1 when SQLite has none: for a message that names no token,
	// such as "incomplete input", and for an error that is not about SQL text.
	Offset int
}

func (e *Error) Error() string {
	return fmt.Sprintf("sqlite: %v (code %v)", e.Message, e.Code)
}

// errorFromConn for the result code rc of a call on db. The message is SQLite's message for the most
// recent failed call on db if that call failed with rc, and SQLite's description of rc otherwise,
// because not every failure sets the message.
func errorFromConn(db *C.sqlite3, rc C.int) error {
	if db == nil || C.sqlite3_extended_errcode(db) != rc {
		return &Error{Code: ResultCode(rc), Message: C.GoString(C.sqlite3_errstr(rc)), Offset: -1}
	}
	return &Error{Code: ResultCode(rc), Message: C.GoString(C.sqlite3_errmsg(db)), Offset: int(C.sqlite3_error_offset(db))}
}

// Conn is a connection to a database.
type Conn struct {
	db *C.sqlite3
	// progress is what the progress handler of a step that [Stmt.StepContext] runs reads, through
	// its handle. Every statement of the connection shares it, which is one reason why a Conn and
	// its statements are not safe for concurrent use.
	progress *progress
}

// Open the database file at path for reading and writing, creating it if it does not exist. The path
// ":memory:" opens a new in-memory database, and "" opens a new temporary database on disk that is
// deleted when it is closed. Extended result codes are enabled.
func Open(path string) (*Conn, error) {
	return OpenVFS(path, "")
}

// OpenVFS is [Open] through the VFS registered under the name vfs, or through SQLite's default VFS
// if vfs is empty.
func OpenVFS(path, vfs string) (*Conn, error) {
	return open(path, vfs, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_EXRESCODE)
}

// OpenExistingVFS is [OpenVFS] of a database file that exists: it does not create the file, and
// fails with SQLITE_CANTOPEN if there is none.
func OpenExistingVFS(path, vfs string) (*Conn, error) {
	return open(path, vfs, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_EXRESCODE)
}

// open the database file at path through the VFS vfs with the flags of sqlite3_open_v2.
func open(path, vfs string, flags C.int) (*Conn, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("sqlite: path contains a NUL byte")
	}
	if strings.IndexByte(vfs, 0) >= 0 {
		return nil, errors.New("sqlite: VFS name contains a NUL byte")
	}

	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var cvfs *C.char
	if vfs != "" {
		cvfs = C.CString(vfs)
		defer C.free(unsafe.Pointer(cvfs))
	}

	var db *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &db, flags, cvfs)
	if rc == C.SQLITE_OK && asan {
		// The lookaside allocator reuses memory that the address sanitizer cannot see, so it would
		// hide a statement used after it is finalized.
		rc = C.s4ql_disable_lookaside(db)
	}
	if rc != C.SQLITE_OK {
		err := errorFromConn(db, rc)
		// The handle is allocated even when opening fails, unless memory ran out.
		C.sqlite3_close(db)
		return nil, err
	}
	p := &progress{period: progressPeriod}
	p.handle = cgo.NewHandle(p)
	return &Conn{db: db, progress: p}, nil
}

// FindVFS registered under the given name, or SQLite's default VFS for the empty name. The result
// points to an sqlite3_vfs, and is nil if there is no such VFS, as for a name with a NUL byte.
func FindVFS(name string) unsafe.Pointer {
	if strings.IndexByte(name, 0) >= 0 {
		return nil
	}
	var cname *C.char
	if name != "" {
		cname = C.CString(name)
		defer C.free(unsafe.Pointer(cname))
	}
	return unsafe.Pointer(C.sqlite3_vfs_find(cname))
}

// RegisterVFS that vfs points to, an sqlite3_vfs, under its name, and not as the default VFS. The
// memory at vfs must stay valid, and in C memory, until [UnregisterVFS] has unregistered it and the
// last connection that was opened through it is closed.
func RegisterVFS(vfs unsafe.Pointer) error {
	if rc := C.sqlite3_vfs_register((*C.sqlite3_vfs)(vfs), 0); rc != C.SQLITE_OK {
		return errorFromConn(nil, rc)
	}
	return nil
}

// UnregisterVFS that [RegisterVFS] registered. Unregistering a VFS that is not registered does
// nothing.
func UnregisterVFS(vfs unsafe.Pointer) error {
	if rc := C.sqlite3_vfs_unregister((*C.sqlite3_vfs)(vfs)); rc != C.SQLITE_OK {
		return errorFromConn(nil, rc)
	}
	return nil
}

// Close the connection. It fails with SQLITE_BUSY, and leaves the connection open, if any statement
// prepared on it is not finalized. Closing a closed connection does nothing.
func (c *Conn) Close() error {
	if c.db == nil {
		return nil
	}
	if rc := C.sqlite3_close(c.db); rc != C.SQLITE_OK {
		return errorFromConn(c.db, rc)
	}
	c.db = nil
	c.progress.handle.Delete()
	return nil
}

// Prepare one SQL statement. It refuses SQL without a statement, and SQL that continues with
// anything but spaces, tabs, line breaks and form feeds after the first statement, including
// comments and a NUL byte, because SQLite would ignore that part. Some PRAGMAs take effect when they
// are prepared, and so take effect even when the SQL after them is refused.
func (c *Conn) Prepare(sql string) (*Stmt, error) {
	s, tail, err := c.PrepareFirst(sql)
	if err != nil {
		return nil, err
	}

	// A statement that prepared always has its tail.
	rest := sql[tail:]
	if strings.Trim(rest, " \t\n\f\r") != "" {
		if s != nil {
			_ = s.Finalize()
		}
		if len(rest) > 100 {
			rest = rest[:100] + "..."
		}
		return nil, fmt.Errorf("sqlite: SQL continues after the first statement: %q", rest)
	}
	if s == nil {
		return nil, errors.New("sqlite: no statement in SQL")
	}
	return s, nil
}

// PrepareFirst prepares the first SQL statement of sql, and gives it with its tail: the offset of the
// first byte after it, which is after its semicolon, or at the end of the text if it has none. Unlike
// [Conn.Prepare], it accepts anything after the first statement, and leaves it unread. SQLite reads
// sql up to its first NUL byte, as it would read a C string, so a NUL byte ends the text. Empty
// statements, which are semicolons with nothing between them, are skipped. If the text has no
// statement, as for an empty text or one with only comments, the statement is nil and there is no
// error.
//
// On an error the tail is where SQLite's parser stopped reading: after the token it was reading when
// the error was raised, or at an unrecognized token. It is -1 when SQLite did not parse the text at
// all, as on a closed connection.
func (c *Conn) PrepareFirst(sql string) (*Stmt, int, error) {
	if c.db == nil {
		return nil, -1, errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if len(sql) > math.MaxInt32 {
		return nil, -1, &Error{Code: C.SQLITE_TOOBIG, Message: "statement too long", Offset: -1}
	}

	var stmt *C.sqlite3_stmt
	var tail C.int
	rc := C.s4ql_prepare(c.db, (*C.char)(unsafe.Pointer(unsafe.StringData(sql))), C.int(len(sql)), &stmt, &tail)
	if rc != C.SQLITE_OK {
		return nil, int(tail), errorFromConn(c.db, rc)
	}
	if stmt == nil {
		return nil, int(tail), nil
	}
	return &Stmt{stmt: stmt, progress: c.progress}, int(tail), nil
}

// DenyProcessPragmas on the connection from now on: the PRAGMAs soft_heap_limit, hard_heap_limit,
// temp_store_directory, and data_store_directory, which change settings for the whole process when
// they are prepared, fail with SQLITE_AUTH and the message "not authorized". SQLite checks this after
// it has parsed the whole statement, so the statement's syntax is still checked.
func (c *Conn) DenyProcessPragmas() error {
	if c.db == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if rc := C.s4ql_set_deny_process_pragmas(c.db); rc != C.SQLITE_OK {
		return errorFromConn(c.db, rc)
	}
	return nil
}

// NoCheckpointOnClose for the connection from now on, SQLITE_DBCONFIG_NO_CKPT_ON_CLOSE. Without
// it, the close of the last connection to a database in WAL mode checkpoints the write-ahead log
// into the database file and, if that succeeds, deletes the log. With it on the connection that
// closes last, the close copies nothing and the log stays, so that the next open recovers from it.
// It fails with SQLITE_MISUSE on a closed connection.
func (c *Conn) NoCheckpointOnClose() error {
	if c.db == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if rc := C.s4ql_no_checkpoint_on_close(c.db); rc != C.SQLITE_OK {
		return errorFromConn(c.db, rc)
	}
	return nil
}

// AddTestFunctions to the connection: stubs of the functions regexp and match, with two or three
// arguments, and glob with three, which SQLite calls for X REGEXP Y, X MATCH Y, and X GLOB Y ESCAPE
// Z, with the operands in the order Y, X, Z. SQLite has no regexp of its own, its match is a
// placeholder for virtual tables that fails when it runs, and its glob takes no ESCAPE, so without
// the stubs a statement with these operators does not prepare or does not run. They are for tests
// of how SQLite compiles and evaluates the operators, and only a test's connection may have them:
// they give the operators a meaning that SQLite does not give them.
//
// Each stub is NULL if an argument is NULL. Otherwise it is -1, 0, or 1 as the text of its first
// argument sorts before, the same as, or after the text of its second, byte by byte and a prefix
// first; with a third argument it is 3 times that plus the same order of the second to the third.
// So the value depends on every argument and on their order. The stubs are deterministic and
// innocuous.
func (c *Conn) AddTestFunctions() error {
	if c.db == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if rc := C.s4ql_add_test_functions(c.db); rc != C.SQLITE_OK {
		return errorFromConn(c.db, rc)
	}
	return nil
}

// SetLengthLimit of the connection, SQLITE_LIMIT_LENGTH: the largest string or blob, in bytes, that
// it makes. A larger one fails with SQLITE_TOOBIG and "string or blob too big". SQLite lowers a
// limit above its compile-time default, 1,000,000,000, to that default, and one below 30 to 30. n
// must be positive. It fails with SQLITE_MISUSE, and changes nothing, on a closed connection or for
// an n that is not positive, and does not fail otherwise.
func (c *Conn) SetLengthLimit(n int) error {
	if c.db == nil || n <= 0 {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	C.sqlite3_limit(c.db, C.SQLITE_LIMIT_LENGTH, C.int(min(n, math.MaxInt32)))
	return nil
}

// Autocommit reports whether the connection is outside a transaction that BEGIN started, as
// sqlite3_get_autocommit says. A connection also leaves its transaction when SQLite rolls it back
// for an error, such as an I/O error or a full disk, which
// https://sqlite.org/c3ref/get_autocommit.html names: "If certain kinds of errors occur on a
// statement within a multi-statement transaction ... then the transaction might be rolled back
// automatically." It panics on a closed connection.
func (c *Conn) Autocommit() bool {
	if c.db == nil {
		panic("sqlite: connection is closed")
	}
	return C.sqlite3_get_autocommit(c.db) != 0
}

// SetProgressPeriod of the connection: the number of instructions of SQLite's virtual machine
// between two looks of [Stmt.StepContext] at its context, which is 10,000 from the open on. A period
// of 1 looks at every jump of the program and once more at the end of each step. It panics for a
// period that is not positive.
func (c *Conn) SetProgressPeriod(n int) {
	if n <= 0 {
		panic("sqlite: progress period not positive")
	}
	c.progress.period = min(n, math.MaxInt32)
}

// Exec one SQL statement to completion, discarding any rows it returns. The SQL is checked as by
// [Conn.Prepare].
func (c *Conn) Exec(sql string) error {
	s, err := c.Prepare(sql)
	if err != nil {
		return err
	}
	for {
		ok, err := s.Step()
		if err != nil {
			_ = s.Finalize()
			return err
		}
		if !ok {
			return s.Finalize()
		}
	}
}

// Stmt is a prepared statement.
type Stmt struct {
	stmt *C.sqlite3_stmt
	// row is true while the most recent step returned a row.
	row bool
	// progress is that of the connection of the statement.
	progress *progress
}

// error for the result code rc of a call on the statement.
func (s *Stmt) error(rc C.int) error {
	if s.stmt == nil {
		return errorFromConn(nil, rc)
	}
	return errorFromConn(C.sqlite3_db_handle(s.stmt), rc)
}

// BindInt64 to the parameter at index i, which counts from 1 as in SQLite. A parameter that is not
// bound is NULL. It fails with SQLITE_RANGE for an index out of range, and with SQLITE_MISUSE on a
// finalized statement.
func (s *Stmt) BindInt64(i int, v int64) error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(nil, C.SQLITE_RANGE)
	}
	if rc := C.sqlite3_bind_int64(s.stmt, C.int(i), C.sqlite3_int64(v)); rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// BindText to the parameter at index i, which counts from 1. The text is bound as its bytes, whatever
// they are, also with NUL bytes in it, and the empty text is not NULL. It fails as [Stmt.BindInt64]
// does.
func (s *Stmt) BindText(i int, v string) error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(nil, C.SQLITE_RANGE)
	}
	// SQLite copies the bytes before the call returns, so it keeps no pointer into Go memory.
	rc := C.s4ql_bind_text(s.stmt, C.int(i), (*C.char)(unsafe.Pointer(unsafe.StringData(v))), C.sqlite3_uint64(len(v)))
	if rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// BindDouble to the parameter at index i, which counts from 1. The value is bound as SQLite binds a
// double: with its bits as they are, also -0.0 and the infinities, except that a NaN is bound as
// NULL. It fails as [Stmt.BindInt64] does.
func (s *Stmt) BindDouble(i int, v float64) error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(nil, C.SQLITE_RANGE)
	}
	if rc := C.sqlite3_bind_double(s.stmt, C.int(i), C.double(v)); rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// BindBlob to the parameter at index i, which counts from 1. The blob is bound as its bytes, and the
// empty blob is not NULL. A nil slice binds the empty blob too, so that BindBlob always binds a blob,
// and [Stmt.BindNull] binds NULL. It fails as [Stmt.BindInt64] does, and with SQLITE_TOOBIG for a blob
// longer than the length limit of the connection.
func (s *Stmt) BindBlob(i int, v []byte) error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(nil, C.SQLITE_RANGE)
	}
	// SQLite copies the bytes before the call returns, so it keeps no pointer into Go memory.
	rc := C.s4ql_bind_blob(s.stmt, C.int(i), unsafe.Pointer(unsafe.SliceData(v)), C.sqlite3_uint64(len(v)))
	if rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// BindNull to the parameter at index i, which counts from 1. A parameter is NULL until it is bound,
// and keeps its value over [Stmt.Reset], so this is for a statement that is run again. It fails as
// [Stmt.BindInt64] does.
func (s *Stmt) BindNull(i int) error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(nil, C.SQLITE_RANGE)
	}
	if rc := C.sqlite3_bind_null(s.stmt, C.int(i)); rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// Step the statement. It returns true if a row is ready to read, and false when the statement has
// finished. Stepping a statement that has finished, or that failed with anything but SQLITE_BUSY or
// SQLITE_LOCKED, fails with SQLITE_MISUSE, so a statement runs once until it is reset with
// [Stmt.Reset]. After SQLITE_BUSY or SQLITE_LOCKED, SQLite starts the statement again from the
// beginning. It fails with SQLITE_MISUSE on a finalized statement.
func (s *Stmt) Step() (bool, error) {
	if s.stmt == nil {
		return false, errorFromConn(nil, C.SQLITE_MISUSE)
	}
	s.row = false
	return s.result(C.sqlite3_step(s.stmt))
}

// progressPeriod is the number of instructions of SQLite's virtual machine between two calls of the
// progress handler in a step of [Stmt.StepContext] on a connection whose period
// [Conn.SetProgressPeriod] did not set, each of which calls Go to look at the Err of the context. A call was measured at about 75 ns on an Apple M4, as much as a few instructions of
// a sort, so the handler adds well under a tenth of a percent to a step, which is too little to
// measure, and a step stops within a period of instructions after its context ends, which is a
// fraction of a millisecond of SQLite's work.
const progressPeriod = 10_000

// StepContext is [Stmt.Step], and ends the step when ctx ends: SQLite looks at ctx.Err every 10,000
// instructions of its virtual machine, or every period that [Conn.SetProgressPeriod] set, and once
// it is not nil, it stops the step, which then fails
// with the error of ctx, [context.Canceled] or [context.DeadlineExceeded]. The statement has then
// failed with SQLITE_INTERRUPT, which [Stmt.Reset] and [Stmt.Finalize] give, and it runs again after
// a reset. A context that has ended before the step stops it at the first look, and a step that
// ends before that gives its result. A context without Done, such as [context.Background], cannot
// end, and the step runs as Step does.
//
// SQLite looks only between two instructions, so an instruction that runs long by itself runs to its
// end first. Of the statements that read, the one that matters is the count of every row of a
// table, "select count(*) from t" without WHERE, which SQLite runs in one instruction that reads the
// whole table; a count of a column that is never NULL, such as one of the primary key, gives the
// same number and is stopped. The instructions of a sort are bounded by the memory of the sort, so
// a statement that sorts is stopped within a period of instructions however many rows it sorts.
//
// Only this step runs with the progress handler, so no other step and no other statement on the
// connection is stopped for ctx, before it or after it. A statement that SQLite stops while it
// writes, an insert, an update, or a delete, rolls back the whole transaction of an explicit BEGIN,
// as https://sqlite.org/c3ref/interrupt.html says, so StepContext is for statements that only read:
// SQLite stops one of those, also in an explicit transaction, and the transaction and its writes
// stay. A stop that rolled back the transaction of the connection all the same fails with SQLite's
// error SQLITE_INTERRUPT and not with the error of ctx, so that the error of ctx always means a stop
// that left the transaction as it was.
//
// A step that ends the statement or fails, also after ctx has ended, gives its own result: the end,
// or the error that it failed with. SQLite looks at ctx once more on its way out of a step, if a
// look is due then, and StepContext stops the step there only while the statement still runs, so an
// I/O error that failed the step, and that rolled back the transaction of the connection, is given
// as such. A step that has a row is stopped there, and fails with the error of ctx.
func (s *Stmt) StepContext(ctx context.Context) (bool, error) {
	if s.stmt == nil {
		return false, errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if ctx.Done() == nil {
		return s.Step()
	}
	s.row = false
	s.progress.ctx = ctx
	var left C.int
	rc := C.s4ql_step_interruptible(s.stmt, C.uintptr_t(s.progress.handle), C.int(s.progress.period), &left)
	s.progress.ctx = nil
	// A stop for ctx that rolled back the transaction of the connection, as one of a write in an
	// explicit transaction does, gives SQLite's error, so that it is not taken for a stop that left
	// the transaction as it was.
	if rc == C.SQLITE_INTERRUPT && left == 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
	}
	return s.result(rc)
}

// result of a step that gave rc.
func (s *Stmt) result(rc C.int) (bool, error) {
	switch rc {
	case C.SQLITE_ROW:
		s.row = true
		return true, nil
	case C.SQLITE_DONE:
		return false, nil
	default:
		return false, s.error(rc)
	}
}

// vmSteps is the number of instructions of SQLite's virtual machine that the statement has run since
// it was prepared.
func (s *Stmt) vmSteps() int {
	s.mustNotBeFinalized()
	return int(C.sqlite3_stmt_status(s.stmt, C.SQLITE_STMTSTATUS_VM_STEP, 0))
}

// ColumnCount of the statement's results, which is 0 for a statement without results. It panics on a
// finalized statement.
func (s *Stmt) ColumnCount() int {
	s.mustNotBeFinalized()
	return int(C.sqlite3_column_count(s.stmt))
}

// ColumnName of the result column at index i, which counts from 0 as in SQLite. It panics on a
// finalized statement and for an index out of range.
func (s *Stmt) ColumnName(i int) string {
	s.mustHaveColumn(i)
	name := C.sqlite3_column_name(s.stmt, C.int(i))
	if name == nil {
		panic("sqlite: out of memory")
	}
	return C.GoString(name)
}

// ColumnDeclType of the result column at index i, which counts from 0: the declared type of the
// column of a table that the result column is, as sqlite3_column_decltype gives it, or "" for a
// result column that is an expression or a column without a type. It panics on a finalized statement
// and for an index out of range.
func (s *Stmt) ColumnDeclType(i int) string {
	s.mustHaveColumn(i)
	return C.GoString(C.sqlite3_column_decltype(s.stmt, C.int(i)))
}

// Type of a value.
type Type int

const (
	TypeInteger Type = C.SQLITE_INTEGER
	TypeFloat   Type = C.SQLITE_FLOAT
	TypeText    Type = C.SQLITE_TEXT
	TypeBlob    Type = C.SQLITE_BLOB
	TypeNull    Type = C.SQLITE_NULL
)

func (t Type) String() string {
	switch t {
	case TypeInteger:
		return "integer"
	case TypeFloat:
		return "float"
	case TypeText:
		return "text"
	case TypeBlob:
		return "blob"
	case TypeNull:
		return "null"
	default:
		return fmt.Sprintf("Type(%d)", int(t))
	}
}

// ColumnType of the value at index i of the current row, which counts from 0. Call it before
// [Stmt.ColumnInt64] and the other functions that read a value, because reading a value as another
// type can convert it, and then its type is undefined. It panics without a current row, which exists
// only while the most recent call to [Stmt.Step] returned true, and for an index out of range.
func (s *Stmt) ColumnType(i int) Type {
	s.mustHaveRow(i)
	return Type(C.sqlite3_column_type(s.stmt, C.int(i)))
}

// ColumnInt64 reads the value at index i of the current row, which counts from 0, as an integer.
// SQLite converts a value of another type, and NULL reads as 0, so check [Stmt.ColumnType] first. It
// panics without a current row and for an index out of range, as [Stmt.ColumnType] does.
func (s *Stmt) ColumnInt64(i int) int64 {
	s.mustHaveRow(i)
	return int64(C.sqlite3_column_int64(s.stmt, C.int(i)))
}

// ColumnText reads the value at index i of the current row, which counts from 0, as text: the bytes
// of a text value as they are, also with NUL bytes in them. SQLite converts a value of another type,
// and NULL reads as the empty text, so check [Stmt.ColumnType] first. It panics without a current row
// and for an index out of range, as [Stmt.ColumnType] does.
func (s *Stmt) ColumnText(i int) string {
	s.mustHaveRow(i)
	// The length is read after the text, because reading the text can convert the value.
	text := C.sqlite3_column_text(s.stmt, C.int(i))
	n := C.sqlite3_column_bytes(s.stmt, C.int(i))
	if text == nil {
		if C.sqlite3_errcode(C.sqlite3_db_handle(s.stmt)) == C.SQLITE_NOMEM {
			panic("sqlite: out of memory")
		}
		return ""
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(text)), n)
}

// ColumnDouble reads the value at index i of the current row, which counts from 0, as a real: the
// bits of a real value as they are, also -0.0 and the infinities. SQLite converts a value of another
// type, and NULL reads as 0, so check [Stmt.ColumnType] first. It panics without a current row and
// for an index out of range, as [Stmt.ColumnType] does.
func (s *Stmt) ColumnDouble(i int) float64 {
	s.mustHaveRow(i)
	return float64(C.sqlite3_column_double(s.stmt, C.int(i)))
}

// ColumnBlob reads the value at index i of the current row, which counts from 0, as a blob: a copy of
// its bytes, which stays valid after the next step. The empty blob reads as an empty slice that is
// not nil, and NULL as nil. A text reads as its bytes, and SQLite converts a value of another type to
// text first, so check [Stmt.ColumnType] first. It panics without a current row and for an index out
// of range, as [Stmt.ColumnType] does.
func (s *Stmt) ColumnBlob(i int) []byte {
	s.mustHaveRow(i)
	typ := C.sqlite3_column_type(s.stmt, C.int(i))
	if typ == C.SQLITE_NULL {
		return nil
	}
	// The length is read after the bytes, because reading them can convert the value.
	blob := C.sqlite3_column_blob(s.stmt, C.int(i))
	n := C.sqlite3_column_bytes(s.stmt, C.int(i))
	if blob == nil {
		// SQLite gives no pointer for a value of no bytes, and none when memory runs out. A number
		// is never the empty text, so it has no bytes only when memory ran out. The error code of
		// the connection cannot tell, because another call may have set it since the step.
		if n > 0 || typ == C.SQLITE_INTEGER || typ == C.SQLITE_FLOAT {
			panic("sqlite: out of memory")
		}
		return []byte{}
	}
	return C.GoBytes(blob, n)
}

func (s *Stmt) mustNotBeFinalized() {
	if s.stmt == nil {
		panic("sqlite: statement is finalized")
	}
}

func (s *Stmt) mustHaveColumn(i int) {
	if n := s.ColumnCount(); i < 0 || i >= n {
		panic(fmt.Sprintf("sqlite: column index %v out of range for %v columns", i, n))
	}
}

func (s *Stmt) mustHaveRow(i int) {
	s.mustHaveColumn(i)
	if !s.row {
		panic("sqlite: no current row")
	}
}

// Reset the statement, so that it can be stepped again from its beginning. Its parameters keep the
// values that were bound. If the most recent step of the statement failed, Reset returns that error
// again, as SQLite does, and the statement is still reset. It fails with SQLITE_MISUSE on a finalized
// statement.
func (s *Stmt) Reset() error {
	if s.stmt == nil {
		return errorFromConn(nil, C.SQLITE_MISUSE)
	}
	s.row = false
	if rc := C.sqlite3_reset(s.stmt); rc != C.SQLITE_OK {
		return s.error(rc)
	}
	return nil
}

// Finalize the statement and free it. If the most recent step of the statement failed, Finalize
// returns that error again, as SQLite does, and the statement is still freed. Finalizing a finalized
// statement does nothing.
func (s *Stmt) Finalize() error {
	if s.stmt == nil {
		return nil
	}
	db := C.sqlite3_db_handle(s.stmt)
	rc := C.sqlite3_finalize(s.stmt)
	s.stmt = nil
	s.row = false
	if rc != C.SQLITE_OK {
		return errorFromConn(db, rc)
	}
	return nil
}

// MemoryUsed by this copy of SQLite in the whole process, in bytes: what its allocations hold now,
// and the most they held at once since the high-water mark was last reset, by sqlite3_status64 with
// SQLITE_STATUS_MEMORY_USED. With reset, the mark is reset to what they hold now, after it is read.
// Memory that another copy of SQLite in the same process allocates does not count.
func MemoryUsed(reset bool) (used, highwater int64) {
	var cur, hi C.sqlite3_int64
	var r C.int
	if reset {
		r = 1
	}
	if C.sqlite3_status64(C.SQLITE_STATUS_MEMORY_USED, &cur, &hi, r) != C.SQLITE_OK {
		panic("sqlite: sqlite3_status64 refused SQLITE_STATUS_MEMORY_USED")
	}
	return int64(cur), int64(hi)
}

// Version of SQLite, such as 3.54.1.
func Version() string {
	return C.GoString(C.sqlite3_libversion())
}

// SourceID of the SQLite source code: the date and time of its check-in and its SHA3-256 hash.
func SourceID() string {
	return C.GoString(C.sqlite3_sourceid())
}

// Token of SQL text as SQLite's own tokenizer reads it: its type, which is one of the parser's TK_
// codes, and its length in bytes.
type Token struct {
	Type int
	Len  int
}

// Tokenize the SQL text src with SQLite's own tokenizer, sqlite3GetToken, which SQLite's parser reads
// its input with and which no public API of SQLite exposes. It is here so that another tokenizer can
// be checked against it. The types are the parser's TK_ codes, which SQLite's parser generator numbers
// anew for each version, so their names are in the TK_ definitions in c/sqlite3.c and not here.
//
// Like the parser, it reads src up to its first NUL byte, or to its end. The last token is the one
// that starts there: it has the type TK_ILLEGAL and length 0. Every other token is at least one byte
// long, and the tokens cover the bytes before the end without a gap. It panics for a text of 2 GiB
// or more, whose token count may not fit a C int.
func Tokenize(src string) []Token {
	if len(src) >= math.MaxInt32 {
		panic("sqlite: text too long to tokenize")
	}
	z := C.CString(src)
	defer C.free(unsafe.Pointer(z))

	// Every token but the last is at least one byte long, so len(src) + 1 tokens always fit.
	room := len(src) + 1
	types := make([]C.int, room)
	lens := make([]C.sqlite3_int64, room)
	k := C.s4ql_tokenize((*C.uchar)(unsafe.Pointer(z)), &types[0], &lens[0], C.int(room))
	if k < 0 {
		panic("sqlite: tokens of length 0 before the end of the text")
	}

	tokens := make([]Token, k)
	for i := range tokens {
		tokens[i] = Token{Type: int(types[i]), Len: int(lens[i])}
	}
	return tokens
}

// Complete reports whether sql ends a statement, by sqlite3_complete: whether it ends with a
// semicolon that is not inside a string, a name, a comment, or the body of CREATE TRIGGER. The
// sqlite3 shell splits its input into statements with it. It reads sql up to its first NUL byte.
func Complete(sql string) bool {
	z := C.CString(sql)
	defer C.free(unsafe.Pointer(z))
	return C.sqlite3_complete(z) != 0
}

// Keywords of SQL in this build of SQLite, in upper case, as sqlite3_keyword_name gives them.
func Keywords() []string {
	keywords := make([]string, C.sqlite3_keyword_count())
	for i := range keywords {
		var name *C.char
		var n C.int
		if C.sqlite3_keyword_name(C.int(i), &name, &n) != C.SQLITE_OK {
			panic("sqlite: no keyword at an index below sqlite3_keyword_count")
		}
		keywords[i] = C.GoStringN(name, n)
	}
	return keywords
}

// CompileOptions that SQLite was built with, as PRAGMA compile_options reports them, without the
// SQLITE_ prefix.
func CompileOptions() []string {
	var options []string
	for i := C.int(0); ; i++ {
		o := C.sqlite3_compileoption_get(i)
		if o == nil {
			return options
		}
		options = append(options, C.GoString(o))
	}
}
