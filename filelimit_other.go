//go:build !unix

package s4ql

// fileLimit of the process: none, because the platform has no limit of open files that a process
// can read, as a Unix system has in RLIMIT_NOFILE.
func fileLimit() (uint64, bool) {
	return 0, false
}
