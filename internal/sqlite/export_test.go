//go:build !s4ql_browser

package sqlite

// ProgressPeriod is the number of instructions of SQLite's virtual machine between two looks of
// [Stmt.StepContext] at its context.
const ProgressPeriod = progressPeriod

// VMSteps is the number of instructions of SQLite's virtual machine that the statement has run
// since it was prepared, by sqlite3_stmt_status with SQLITE_STMTSTATUS_VM_STEP.
func (s *Stmt) VMSteps() int {
	return s.vmSteps()
}
