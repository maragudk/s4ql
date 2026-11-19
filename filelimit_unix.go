//go:build unix

package s4ql

import (
	"math"
	"syscall"
)

// fileLimit of the process: its soft limit of open files, RLIMIT_NOFILE, and whether it has one. A
// limit that is unlimited, or that cannot be read, is none.
func fileLimit() (uint64, bool) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, false
	}
	if uint64(r.Cur) == rlimInfinity {
		return 0, false
	}
	return uint64(r.Cur), true
}

// rlimInfinity is the limit that means unlimited, as the uint64 of its 64 bits. RLIM_INFINITY is an
// untyped constant that is negative on some platforms, such as -1 on Linux and -3 on Solaris, and
// positive on others, such as 1<<63-1 on Darwin and 1<<64-1 on Linux on loong64. The mask gives
// the two's complement of a negative one, as a conversion of the field of the limit does, whose
// type is uint64 on most platforms and int64 on others, such as FreeBSD.
const rlimInfinity = uint64(syscall.RLIM_INFINITY & math.MaxUint64)
