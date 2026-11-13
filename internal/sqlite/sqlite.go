// Package sqlite is a small cgo binding to the official SQLite C code vendored in the c directory,
// built with the compile options in options.h.
//
// It is deliberately small: opening and closing a database, through a VFS by its name if need be,
// finding and registering a VFS, preparing one statement, binding and reading integers, text, and
// NULL, stepping, resetting, and finalizing. Each statement runs in its
// own transaction, as SQLite does outside BEGIN and COMMIT.
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
// preamble into a second translation unit.
#define SQLITE_API static

#include "options.h"
#include "source.h"
#include "c/sqlite3.c"

#cgo !darwin LDFLAGS: -lm

// s4ql_prepare a statement from the n bytes at sql, and set *tail to the offset of the first byte
// after it.
static int s4ql_prepare(sqlite3 *db, const char *sql, int n, sqlite3_stmt **stmt, int *tail) {
	const char *end = 0;
	if (sql == 0) {
		sql = "";
	}
	int rc = sqlite3_prepare_v3(db, sql, n, 0, stmt, &end);
	*tail = end == 0 ? n : (int)(end - sql);
	return rc;
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

// s4ql_disable_lookaside on the connection, so that every allocation goes through malloc and free.
static int s4ql_disable_lookaside(sqlite3 *db) {
	return sqlite3_db_config(db, SQLITE_DBCONFIG_LOOKASIDE, (void *)0, 0, 0);
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
	"errors"
	"fmt"
	"math"
	"strings"
	"unsafe"
)

// ResultCode is an extended result code from SQLite, as listed on https://sqlite.org/rescode.html.
type ResultCode int

// Primary result code, which is the lowest 8 bits of the extended result code.
func (c ResultCode) Primary() ResultCode {
	return c & 0xff
}

// Error from SQLite, with its extended result code and its message.
type Error struct {
	Code    ResultCode
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("sqlite: %v (code %v)", e.Message, e.Code)
}

// errorFromConn for the result code rc of a call on db. The message is SQLite's message for the most
// recent failed call on db if that call failed with rc, and SQLite's description of rc otherwise,
// because not every failure sets the message.
func errorFromConn(db *C.sqlite3, rc C.int) error {
	if db == nil || C.sqlite3_extended_errcode(db) != rc {
		return &Error{Code: ResultCode(rc), Message: C.GoString(C.sqlite3_errstr(rc))}
	}
	return &Error{Code: ResultCode(rc), Message: C.GoString(C.sqlite3_errmsg(db))}
}

// Conn is a connection to a database.
type Conn struct {
	db *C.sqlite3
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
	rc := C.sqlite3_open_v2(cpath, &db, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_EXRESCODE, cvfs)
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
	return &Conn{db: db}, nil
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
	return nil
}

// Prepare one SQL statement. It refuses SQL without a statement, and SQL that continues with
// anything but spaces, tabs, line breaks and form feeds after the first statement, including
// comments and a NUL byte, because SQLite would ignore that part. Some PRAGMAs take effect when they
// are prepared, and so take effect even when the SQL after them is refused.
func (c *Conn) Prepare(sql string) (*Stmt, error) {
	if c.db == nil {
		return nil, errorFromConn(nil, C.SQLITE_MISUSE)
	}
	if len(sql) > math.MaxInt32 {
		return nil, &Error{Code: C.SQLITE_TOOBIG, Message: "statement too long"}
	}

	var stmt *C.sqlite3_stmt
	var tail C.int
	rc := C.s4ql_prepare(c.db, (*C.char)(unsafe.Pointer(unsafe.StringData(sql))), C.int(len(sql)), &stmt, &tail)
	if rc != C.SQLITE_OK {
		return nil, errorFromConn(c.db, rc)
	}

	rest := sql[tail:]
	if strings.Trim(rest, " \t\n\f\r") != "" {
		C.sqlite3_finalize(stmt)
		if len(rest) > 100 {
			rest = rest[:100] + "..."
		}
		return nil, fmt.Errorf("sqlite: SQL continues after the first statement: %q", rest)
	}
	if stmt == nil {
		return nil, errors.New("sqlite: no statement in SQL")
	}
	return &Stmt{stmt: stmt}, nil
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
	switch rc := C.sqlite3_step(s.stmt); rc {
	case C.SQLITE_ROW:
		s.row = true
		return true, nil
	case C.SQLITE_DONE:
		return false, nil
	default:
		return false, s.error(rc)
	}
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
// [Stmt.ColumnInt64], because reading a value as an integer can convert it, and then its type is
// undefined. It panics without a current row, which exists only while the most recent call to
// [Stmt.Step] returned true, and for an index out of range.
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
