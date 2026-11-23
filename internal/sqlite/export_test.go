//go:build !s4ql_browser

package sqlite

import "weak"

// VMSteps is the number of instructions of SQLite's virtual machine that the statement has run
// since it was prepared, by sqlite3_stmt_status with SQLITE_STMTSTATUS_VM_STEP.
func (s *Stmt) VMSteps() int {
	return s.vmSteps()
}

// ProgressCollected gives a function that reports whether the garbage collector has freed what the
// progress handler of the connection reads, which the handle of the connection keeps until Close.
func (c *Conn) ProgressCollected() func() bool {
	p := weak.Make(c.progress)
	return func() bool { return p.Value() == nil }
}
