//go:build js && wasm && s4ql_browser

// This file is faultvfs for a Go program in a web browser, built with the tag s4ql_browser, on the
// official SQLite WebAssembly build. It gives the API of faultvfs.go over a VFS of the kind in
// internal/sqlite/browser/vfs.js, which works on the same files as SQLite's default VFS there, with
// a fault state of its own that the methods below read and set through syscall/js. The operations,
// the kinds of file, and the result codes of the failures are those of faultvfs.go. One difference:
// the browser's sqlite.OpenVFS reads the database's schema, so a fault that is armed before a
// connection is opened also counts the reads and locks of the open.

package faultvfs

import (
	"fmt"
	"syscall/js"
	"time"
)

// Op is a set of file operations, as bits.
type Op int

const (
	// Open a file, which fails with SQLITE_CANTOPEN.
	Open Op = 1
	// Read from a file, which fails with SQLITE_IOERR_READ.
	Read Op = 2
	// Write to a file, which fails with SQLITE_IOERR_WRITE.
	Write Op = 4
	// Truncate a file, which fails with SQLITE_IOERR_TRUNCATE.
	Truncate Op = 8
	// Sync a file, which fails with SQLITE_IOERR_FSYNC.
	Sync Op = 16
	// Size of a file, which fails with SQLITE_IOERR_FSTAT.
	Size Op = 32
	// Lock a file, or raise its lock, which fails with SQLITE_IOERR_LOCK. Unlocking never fails.
	Lock Op = 64
	// AnyOp of the operations above.
	AnyOp = Open | Read | Write | Truncate | Sync | Size | Lock
)

// File is a set of kinds of file, as SQLite's SQLITE_OPEN_ flags name them.
type File int

const (
	// MainDB is the database file.
	MainDB File = 0x00000100
	// WAL is the write-ahead log.
	WAL File = 0x00080000
	// MainJournal is the rollback journal.
	MainJournal File = 0x00000800
	// Temp is every temporary file: of a temporary database, its journal, a statement journal, and a
	// transient database.
	Temp File = 0x00000200 | 0x00001000 | 0x00002000 | 0x00000400
	// AnyFile of the kinds above, and a super-journal.
	AnyFile File = MainDB | WAL | MainJournal | Temp | 0x00004000
)

// Fault that a [VFS] makes: the operations in Ops on files of the kinds in Files fail.
type Fault struct {
	Ops   Op
	Files File
	// After is the number of matching operations that succeed before the first failure.
	After int
	// Times is the number of matching operations that then fail. Zero means every one.
	Times int
	// Code is SQLite's result code of a failure. Zero means the usual code of the operation.
	Code int
}

// VFS that works on the files of SQLite's default VFS and makes the operations of a [Fault] fail. It
// is registered under its own name, and a connection uses it only when it is opened through that
// name, with [sqlite.OpenVFS].
type VFS struct {
	name string
	// fault is the fault state of the VFS in JavaScript, and pointer the address of its sqlite3_vfs
	// in SQLite's memory.
	fault   js.Value
	pointer int
	closed  bool
}

// New VFS, registered. Nothing fails until [VFS.Fail] arms a fault. [VFS.Close] unregisters it.
func New() (*VFS, error) {
	b := js.Global().Get("s4qlSQLite")
	if b.IsUndefined() {
		return nil, fmt.Errorf("faultvfs: globalThis.s4qlSQLite is not set")
	}
	r := b.Call("newFaultVFS")
	return &VFS{name: r.Get("name").String(), fault: r.Get("fault"), pointer: r.Get("pointer").Int()}, nil
}

// Name under which the VFS is registered. It panics after [VFS.Close], as [VFS.Fail] and
// [VFS.Failures] do.
func (f *VFS) Name() string {
	f.mustBeOpen()
	return f.name
}

func (f *VFS) mustBeOpen() {
	if f.closed {
		panic("faultvfs: the VFS is closed")
	}
}

// Fail the operations of the fault from now on, in place of the fault that was armed before. A
// fault with no operations or no kinds of file disarms it.
func (f *VFS) Fail(fault Fault) {
	f.mustBeOpen()
	times := fault.Times
	if times == 0 {
		times = -1
	}
	f.fault.Set("ops", int(fault.Ops))
	f.fault.Set("files", int(fault.Files))
	f.fault.Set("skip", fault.After)
	f.fault.Set("times", times)
	f.fault.Set("code", fault.Code)
}

// Failures that the VFS has made so far.
func (f *VFS) Failures() int {
	f.mustBeOpen()
	return f.fault.Get("failed").Int()
}

// Slept is how many times SQLite asked the VFS to sleep, as its busy handler does while it waits
// for a lock, and for how long in all. The time is what SQLite asked for, not what passed.
func (f *VFS) Slept() (int, time.Duration) {
	f.mustBeOpen()
	return f.fault.Get("sleeps").Int(), time.Duration(f.fault.Get("slept").Int()) * time.Microsecond
}

// Close the VFS: unregister it. Every connection that was opened through it must be closed first,
// and as a check of that, Close fails while a file that was opened through the VFS is open. Closing a
// closed VFS does nothing.
func (f *VFS) Close() error {
	if f.closed {
		return nil
	}
	if open := f.fault.Get("open").Int(); open > 0 {
		return fmt.Errorf("faultvfs: %v files of %v are open", open, f.name)
	}
	if rc := js.Global().Get("s4qlSQLite").Call("unregisterVFS", f.pointer).Int(); rc != 0 {
		return fmt.Errorf("faultvfs: unregistering %v failed with SQLite result code %v", f.name, rc)
	}
	f.closed = true
	return nil
}
