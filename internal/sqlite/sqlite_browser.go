//go:build js && wasm && s4ql_browser

// This file is the binding for a Go program in a web browser, built with GOOS=js GOARCH=wasm and the
// tag s4ql_browser, which runs the simulation of the storage core on a web page. It gives the API of
// the cgo binding in sqlite.go, as far as the storage core and its tests use it, over the official
// SQLite WebAssembly build from https://sqlite.org/download.html. That build runs as a second
// WebAssembly instance beside Go, and every call goes through syscall/js to the C functions it
// exports, with each string and out parameter copied between the two memories. The page provides
// the build and the JavaScript in the browser directory, and the README there says how.
//
// Where the official build's compile options differ from those of options.h in a way that a
// statement can see, the binding makes up for it where it can:
//
//   - Every connection that [OpenVFS] opens runs PRAGMAs for the page size of 4096 bytes, the page
//     cache of 2000 KiB for the main and the temporary database, foreign keys on, and temporary
//     files on disk, which are SQLite's defaults in the cgo build. The official build has 8192
//     bytes, 16384 KiB, foreign keys off, and temporary files in memory.
//   - The cgo build has SQLITE_OMIT_AUTORESET, so a statement that finished or failed must be
//     reset before it steps again. [Stmt.Step] keeps that rule itself.
//   - The official build does not export sqlite3_close, only sqlite3_close_v2, so [Conn.Close]
//     refuses to close a connection with a statement that is not finalized itself, as
//     sqlite3_close does.
//
// What remains different: the official build has SQLITE_THREADSAFE=0, SQLITE_OMIT_UTF16,
// SQLITE_OMIT_DEPRECATED, SQLITE_USE_URI, and more extensions, such as FTS5 and R*Tree. And the VFS
// is the one in browser/vfs.js, not SQLite's unix VFS.
//
// SQLite's C code runs on the one thread of the page, so while it sleeps in its busy handler no
// other goroutine could run, and a goroutine that holds the lock it waits for could never release
// it. The VFS therefore calls the Go function s4qlYield, made here, while it sleeps, and Go's runtime
// runs the goroutines that are ready during that call until all of them wait.

package sqlite

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"syscall/js"
	"unsafe"
)

// Result codes from https://sqlite.org/rescode.html that the binding uses.
const (
	codeOK      = 0
	codeNoMem   = 7
	codeBusy    = 5
	codeLocked  = 6
	codeTooBig  = 18
	codeMisuse  = 21
	codeRange   = 25
	codeRow     = 100
	codeDone    = 101
	limitLength = 0 // SQLITE_LIMIT_LENGTH
)

// pragmas are the PRAGMAs that every connection runs when it is opened, for the compile options of
// options.h that the official build lacks and a connection can set. The page cache is set for the
// temporary database too, which has a cache of its own, after the temp store, because a change of the
// temp store drops the temporary database.
const pragmas = "pragma page_size = 4096; pragma foreign_keys = on; pragma temp_store = file; " +
	"pragma cache_size = -2000; pragma temp.cache_size = -2000"

// asan, which noasan.go sets, is false for GOOS=js, which has no address sanitizer, so the lookaside
// allocator stays on, as in the cgo binding without -asan.
var _ = asan

func init() {
	// The function is made here, outside any testing/synctest bubble, so that it belongs to none and
	// can run on any goroutine. A function made inside a bubble panics when it is called on a
	// goroutine of another bubble, or of none.
	js.Global().Set("s4qlYield", js.FuncOf(func(js.Value, []js.Value) any { return nil }))
}

// library of SQLite's functions and of the helpers of bridge.js, looked up once.
//
// Every call returns a number, because syscall/js keeps each JavaScript object that reaches Go in a
// table until Go's garbage collector finds the [js.Value] unreachable, and Go's heap grows too little
// per call to make it collect in time: a view of SQLite's memory per call filled the table, which
// holds 2^24 entries, in a loop of SQLite calls. Bytes therefore pass through transfer, one array
// that Go keeps, and which the helpers copyIn and copyOut copy to and from SQLite's memory.
type library struct {
	alloc, copyIn, copyOut, strlen, peek32, bindInt64, columnInt64 js.Value

	free, openV2, closeV2, nextStmt, exec, prepareV3, extendedErrcode, errstr, errmsg, errorOffset,
	dbHandle, limit, bindText, bindNull, step, columnCount, columnName, columnType, columnText,
	columnBytes, reset, finalize, libversion, sourceid, complete, keywordCount, keywordName,
	compileoptionGet js.Value

	// transfer is a Uint8Array of transferLen bytes. A method uses it only between two calls into
	// JavaScript that cannot call back into Go, so no other goroutine can use it meanwhile.
	transfer    js.Value
	transferLen int
}

var lib = sync.OnceValue(func() *library {
	b := js.Global().Get("s4qlSQLite")
	if b.IsUndefined() {
		panic("sqlite: globalThis.s4qlSQLite is not set; the page must load the SQLite WebAssembly build with bridge.js before Go starts")
	}
	x := b.Get("exports")
	return &library{
		alloc:            b.Get("alloc"),
		copyIn:           b.Get("copyIn"),
		copyOut:          b.Get("copyOut"),
		strlen:           b.Get("strlen"),
		peek32:           b.Get("peek32"),
		bindInt64:        b.Get("bindInt64"),
		columnInt64:      b.Get("columnInt64"),
		free:             x.Get("sqlite3_free"),
		openV2:           x.Get("sqlite3_open_v2"),
		closeV2:          x.Get("sqlite3_close_v2"),
		nextStmt:         x.Get("sqlite3_next_stmt"),
		exec:             x.Get("sqlite3_exec"),
		prepareV3:        x.Get("sqlite3_prepare_v3"),
		extendedErrcode:  x.Get("sqlite3_extended_errcode"),
		errstr:           x.Get("sqlite3_errstr"),
		errmsg:           x.Get("sqlite3_errmsg"),
		errorOffset:      x.Get("sqlite3_error_offset"),
		dbHandle:         x.Get("sqlite3_db_handle"),
		limit:            x.Get("sqlite3_limit"),
		bindText:         x.Get("sqlite3_bind_text"),
		bindNull:         x.Get("sqlite3_bind_null"),
		step:             x.Get("sqlite3_step"),
		columnCount:      x.Get("sqlite3_column_count"),
		columnName:       x.Get("sqlite3_column_name"),
		columnType:       x.Get("sqlite3_column_type"),
		columnText:       x.Get("sqlite3_column_text"),
		columnBytes:      x.Get("sqlite3_column_bytes"),
		reset:            x.Get("sqlite3_reset"),
		finalize:         x.Get("sqlite3_finalize"),
		libversion:       x.Get("sqlite3_libversion"),
		sourceid:         x.Get("sqlite3_sourceid"),
		complete:         x.Get("sqlite3_complete"),
		keywordCount:     x.Get("sqlite3_keyword_count"),
		keywordName:      x.Get("sqlite3_keyword_name"),
		compileoptionGet: x.Get("sqlite3_compileoption_get"),
	}
})

// call the function f of SQLite with the arguments, which are integers, and return its result as
// one.
func call(f js.Value, args ...any) int {
	return f.Invoke(args...).Int()
}

// buffer gives transfer with room for n bytes at the least.
func (l *library) buffer(n int) js.Value {
	if n > l.transferLen {
		l.transferLen = max(n, 2*l.transferLen, 64<<10)
		l.transfer = js.Global().Get("Uint8Array").New(l.transferLen)
	}
	return l.transfer
}

// cString copies s into SQLite's memory with a NUL after it, and returns its address, which
// [library.release] frees.
func (l *library) cString(s string) int {
	p := call(l.alloc, len(s))
	if p == 0 {
		panic("sqlite: out of memory")
	}
	if len(s) > 0 {
		js.CopyBytesToJS(l.buffer(len(s)), unsafe.Slice(unsafe.StringData(s), len(s)))
		l.copyIn.Invoke(l.transfer, p, len(s))
	}
	return p
}

// goBytes copies the n bytes at p in SQLite's memory.
func (l *library) goBytes(p, n int) string {
	if n == 0 {
		return ""
	}
	b := make([]byte, n)
	l.copyOut.Invoke(l.buffer(n), p, n)
	js.CopyBytesToGo(b, l.transfer)
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// goString copies the C string at p in SQLite's memory, without its NUL.
func (l *library) goString(p int) string {
	return l.goBytes(p, call(l.strlen, p))
}

func (l *library) release(p int) {
	l.free.Invoke(p)
}

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
	// sqlite3_error_offset gives it. It is -1 when SQLite has none.
	Offset int
}

func (e *Error) Error() string {
	return fmt.Sprintf("sqlite: %v (code %v)", e.Message, e.Code)
}

// errorFromConn for the result code rc of a call on the connection db, which is 0 for none. The
// message is SQLite's message for the most recent failed call on db if that call failed with rc, and
// SQLite's description of rc otherwise.
func errorFromConn(db, rc int) error {
	l := lib()
	if db == 0 || call(l.extendedErrcode, db) != rc {
		return &Error{Code: ResultCode(rc), Message: l.goString(call(l.errstr, rc)), Offset: -1}
	}
	return &Error{Code: ResultCode(rc), Message: l.goString(call(l.errmsg, db)), Offset: call(l.errorOffset, db)}
}

// Conn is a connection to a database.
type Conn struct {
	// db is the address of the sqlite3 in SQLite's memory, and 0 once the connection is closed.
	db int
}

// Open the database file at path for reading and writing, creating it if it does not exist. The path
// ":memory:" opens a new in-memory database, and "" opens a new temporary database that is deleted
// when it is closed. Extended result codes are enabled.
func Open(path string) (*Conn, error) {
	return OpenVFS(path, "")
}

// OpenVFS is [Open] through the VFS registered under the name vfs, or through SQLite's default VFS
// if vfs is empty, which in a browser is the VFS of browser/vfs.js. Opening runs the PRAGMAs for the
// compile options that the official build lacks, and the PRAGMA for the page cache reads the
// database's schema, so a file that is not a database fails here and not at the first statement.
func OpenVFS(path, vfs string) (*Conn, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("sqlite: path contains a NUL byte")
	}
	if strings.IndexByte(vfs, 0) >= 0 {
		return nil, errors.New("sqlite: VFS name contains a NUL byte")
	}

	l := lib()
	cpath := l.cString(path)
	defer l.release(cpath)
	cvfs := 0
	if vfs != "" {
		cvfs = l.cString(vfs)
		defer l.release(cvfs)
	}
	out := call(l.alloc, 4)
	if out == 0 {
		panic("sqlite: out of memory")
	}
	defer l.release(out)

	const flags = 0x2 | 0x4 | 0x02000000 // SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE | SQLITE_OPEN_EXRESCODE
	rc := call(l.openV2, cpath, out, flags, cvfs)
	db := call(l.peek32, out)
	if rc == codeOK {
		copts := l.cString(pragmas)
		rc = call(l.exec, db, copts, 0, 0, 0)
		l.release(copts)
	}
	if rc != codeOK {
		err := errorFromConn(db, rc)
		// The handle is allocated even when opening fails, unless memory ran out.
		l.closeV2.Invoke(db)
		return nil, err
	}
	return &Conn{db: db}, nil
}

// Close the connection. It fails with SQLITE_BUSY, and leaves the connection open, if any statement
// prepared on it is not finalized. Closing a closed connection does nothing.
func (c *Conn) Close() error {
	if c.db == 0 {
		return nil
	}
	l := lib()
	// sqlite3_close_v2 would close a connection with statements that are not finalized later, when
	// the last of them is, so the refusal of sqlite3_close is made here.
	if call(l.nextStmt, c.db, 0) != 0 {
		return &Error{Code: codeBusy, Message: "unable to close due to unfinalized statements or unfinished backups", Offset: -1}
	}
	if rc := call(l.closeV2, c.db); rc != codeOK {
		return errorFromConn(c.db, rc)
	}
	c.db = 0
	return nil
}

// Prepare one SQL statement. It refuses SQL without a statement, and SQL that continues with
// anything but spaces, tabs, line breaks and form feeds after the first statement, including
// comments and a NUL byte, because SQLite would ignore that part.
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
// first byte after it. Unlike [Conn.Prepare], it accepts anything after the first statement. If the
// text has no statement, the statement is nil and there is no error. On an error the tail is where
// SQLite's parser stopped reading, or -1 when SQLite did not parse the text at all.
func (c *Conn) PrepareFirst(sql string) (*Stmt, int, error) {
	if c.db == 0 {
		return nil, -1, errorFromConn(0, codeMisuse)
	}
	if len(sql) > math.MaxInt32 {
		return nil, -1, &Error{Code: codeTooBig, Message: "statement too long", Offset: -1}
	}

	l := lib()
	csql := l.cString(sql)
	defer l.release(csql)
	// The statement at out and the end of the statement at out + 4, both 0 until SQLite sets them.
	out := call(l.alloc, 8)
	if out == 0 {
		panic("sqlite: out of memory")
	}
	defer l.release(out)

	rc := call(l.prepareV3, c.db, csql, len(sql), 0, out, out+4)
	stmt, end := call(l.peek32, out), call(l.peek32, out+4)
	tail := -1
	if end != 0 {
		tail = end - csql
	}
	if rc != codeOK {
		return nil, tail, errorFromConn(c.db, rc)
	}
	if stmt == 0 {
		return nil, tail, nil
	}
	return &Stmt{stmt: stmt, db: c.db}, tail, nil
}

// SetLengthLimit of the connection, SQLITE_LIMIT_LENGTH: the largest string or blob, in bytes, that
// it makes. n must be positive.
func (c *Conn) SetLengthLimit(n int) error {
	if c.db == 0 || n <= 0 {
		return errorFromConn(0, codeMisuse)
	}
	lib().limit.Invoke(c.db, limitLength, min(n, math.MaxInt32))
	return nil
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
	// stmt is the address of the sqlite3_stmt in SQLite's memory, and 0 once it is finalized, and db
	// that of its connection.
	stmt, db int
	// row is true while the most recent step returned a row.
	row bool
	// halted is true when the most recent step finished the statement or failed with anything but
	// SQLITE_BUSY or SQLITE_LOCKED, until the statement is reset.
	halted bool
}

// BindInt64 to the parameter at index i, which counts from 1 as in SQLite. A parameter that is not
// bound is NULL. It fails with SQLITE_RANGE for an index out of range, and with SQLITE_MISUSE on a
// finalized statement.
func (s *Stmt) BindInt64(i int, v int64) error {
	if s.stmt == 0 {
		return errorFromConn(0, codeMisuse)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(0, codeRange)
	}
	// A Number holds an integer exactly up to 2^53 in size, and larger ones go as decimal text.
	var arg any = v
	if v > 1<<53-1 || v < -(1<<53-1) {
		arg = strconv.FormatInt(v, 10)
	}
	if rc := call(lib().bindInt64, s.stmt, i, arg); rc != codeOK {
		return errorFromConn(s.db, rc)
	}
	return nil
}

// BindText to the parameter at index i, which counts from 1. The text is bound as its bytes, whatever
// they are, also with NUL bytes in it, and the empty text is not NULL. It fails as [Stmt.BindInt64]
// does.
func (s *Stmt) BindText(i int, v string) error {
	if s.stmt == 0 {
		return errorFromConn(0, codeMisuse)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(0, codeRange)
	}
	if len(v) > math.MaxInt32 {
		return errorFromConn(s.db, codeTooBig)
	}
	l := lib()
	p := l.cString(v)
	defer l.release(p)
	// SQLITE_TRANSIENT, -1, makes SQLite copy the bytes before the call returns.
	if rc := call(l.bindText, s.stmt, i, p, len(v), -1); rc != codeOK {
		return errorFromConn(s.db, rc)
	}
	return nil
}

// BindNull to the parameter at index i, which counts from 1. It fails as [Stmt.BindInt64] does.
func (s *Stmt) BindNull(i int) error {
	if s.stmt == 0 {
		return errorFromConn(0, codeMisuse)
	}
	if i < 1 || i > math.MaxInt32 {
		return errorFromConn(0, codeRange)
	}
	if rc := call(lib().bindNull, s.stmt, i); rc != codeOK {
		return errorFromConn(s.db, rc)
	}
	return nil
}

// Step the statement. It returns true if a row is ready to read, and false when the statement has
// finished. Stepping a statement that has finished, or that failed with anything but SQLITE_BUSY or
// SQLITE_LOCKED, fails with SQLITE_MISUSE, so a statement runs once until it is reset with
// [Stmt.Reset]. It fails with SQLITE_MISUSE on a finalized statement.
func (s *Stmt) Step() (bool, error) {
	if s.stmt == 0 {
		return false, errorFromConn(0, codeMisuse)
	}
	s.row = false
	// SQLite with SQLITE_OMIT_AUTORESET refuses this step without changing the error of the
	// connection, as errorFromConn reads it.
	if s.halted {
		return false, errorFromConn(s.db, codeMisuse)
	}
	switch rc := call(lib().step, s.stmt); rc {
	case codeRow:
		s.row = true
		return true, nil
	case codeDone:
		s.halted = true
		return false, nil
	default:
		if p := ResultCode(rc).Primary(); p != codeBusy && p != codeLocked {
			s.halted = true
		}
		return false, errorFromConn(s.db, rc)
	}
}

// ColumnCount of the statement's results, which is 0 for a statement without results. It panics on a
// finalized statement.
func (s *Stmt) ColumnCount() int {
	s.mustNotBeFinalized()
	return call(lib().columnCount, s.stmt)
}

// ColumnName of the result column at index i, which counts from 0 as in SQLite. It panics on a
// finalized statement and for an index out of range.
func (s *Stmt) ColumnName(i int) string {
	s.mustHaveColumn(i)
	l := lib()
	name := call(l.columnName, s.stmt, i)
	if name == 0 {
		panic("sqlite: out of memory")
	}
	return l.goString(name)
}

// Type of a value.
type Type int

const (
	TypeInteger Type = 1
	TypeFloat   Type = 2
	TypeText    Type = 3
	TypeBlob    Type = 4
	TypeNull    Type = 5
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
// [Stmt.ColumnInt64], because reading a value as an integer can convert it. It panics without a
// current row and for an index out of range.
func (s *Stmt) ColumnType(i int) Type {
	s.mustHaveRow(i)
	return Type(call(lib().columnType, s.stmt, i))
}

// ColumnInt64 reads the value at index i of the current row, which counts from 0, as an integer.
// SQLite converts a value of another type, and NULL reads as 0. It panics without a current row and
// for an index out of range.
func (s *Stmt) ColumnInt64(i int) int64 {
	s.mustHaveRow(i)
	v := lib().columnInt64.Invoke(s.stmt, i)
	if v.Type() == js.TypeNumber {
		return int64(v.Float())
	}
	n, err := strconv.ParseInt(v.String(), 10, 64)
	if err != nil {
		panic(fmt.Sprintf("sqlite: integer from SQLite: %v", err))
	}
	return n
}

// ColumnText reads the value at index i of the current row, which counts from 0, as text: the bytes
// of a text value as they are, also with NUL bytes in them. SQLite converts a value of another type,
// and NULL reads as the empty text. It panics without a current row and for an index out of range.
func (s *Stmt) ColumnText(i int) string {
	s.mustHaveRow(i)
	l := lib()
	// The length is read after the text, because reading the text can convert the value.
	text := call(l.columnText, s.stmt, i)
	n := call(l.columnBytes, s.stmt, i)
	if text == 0 {
		if call(l.extendedErrcode, call(l.dbHandle, s.stmt))&0xff == codeNoMem {
			panic("sqlite: out of memory")
		}
		return ""
	}
	return l.goBytes(text, n)
}

func (s *Stmt) mustNotBeFinalized() {
	if s.stmt == 0 {
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
	if s.stmt == 0 {
		return errorFromConn(0, codeMisuse)
	}
	s.row = false
	s.halted = false
	if rc := call(lib().reset, s.stmt); rc != codeOK {
		return errorFromConn(s.db, rc)
	}
	return nil
}

// Finalize the statement and free it. If the most recent step of the statement failed, Finalize
// returns that error again, as SQLite does, and the statement is still freed. Finalizing a finalized
// statement does nothing.
func (s *Stmt) Finalize() error {
	if s.stmt == 0 {
		return nil
	}
	rc := call(lib().finalize, s.stmt)
	s.stmt = 0
	s.row = false
	s.halted = false
	if rc != codeOK {
		return errorFromConn(s.db, rc)
	}
	return nil
}

// Version of SQLite, such as 3.54.1.
func Version() string {
	l := lib()
	return l.goString(call(l.libversion))
}

// SourceID of the SQLite source code: the date and time of its check-in and its SHA3-256 hash.
func SourceID() string {
	l := lib()
	return l.goString(call(l.sourceid))
}

// Complete reports whether sql ends a statement, by sqlite3_complete. It reads sql up to its first
// NUL byte.
func Complete(sql string) bool {
	l := lib()
	p := l.cString(sql)
	defer l.release(p)
	return call(l.complete, p) != 0
}

// Keywords of SQL in this build of SQLite, in upper case, as sqlite3_keyword_name gives them.
func Keywords() []string {
	l := lib()
	// The name at out and its length at out + 4.
	out := call(l.alloc, 8)
	if out == 0 {
		panic("sqlite: out of memory")
	}
	defer l.release(out)
	keywords := make([]string, call(l.keywordCount))
	for i := range keywords {
		if call(l.keywordName, i, out, out+4) != codeOK {
			panic("sqlite: no keyword at an index below sqlite3_keyword_count")
		}
		keywords[i] = l.goBytes(call(l.peek32, out), call(l.peek32, out+4))
	}
	return keywords
}

// CompileOptions that SQLite was built with, as PRAGMA compile_options reports them, without the
// SQLITE_ prefix. They are those of the official build, not those of options.h.
func CompileOptions() []string {
	l := lib()
	var options []string
	for i := 0; ; i++ {
		o := call(l.compileoptionGet, i)
		if o == 0 {
			return options
		}
		options = append(options, l.goString(o))
	}
}
