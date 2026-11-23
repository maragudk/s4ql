package sqlite

// This file exports the one Go function that the C code of sqlite.go calls. A file with //export
// has its preamble copied into a second C file, so the preamble here must declare and not define,
// and sqlite.go, whose preamble holds all of SQLite, has no //export.

/*
#include <stdint.h>
*/
import "C"

import (
	"context"
	"runtime/cgo"
)

// progress is what the progress handler of a connection reads during a step of
// [Stmt.StepContext]: the context of the step, which is nil between steps.
type progress struct {
	ctx    context.Context
	handle cgo.Handle
}

// s4qlInterrupted is the progress handler of a step of [Stmt.StepContext], which SQLite calls from
// within the step, on the goroutine of the step, every [progressPeriod] instructions: 1, which makes
// SQLite stop the step, once the context of the step has ended, and 0 before.
//
//export s4qlInterrupted
func s4qlInterrupted(h C.uintptr_t) C.int {
	p := cgo.Handle(h).Value().(*progress)
	if p.ctx.Err() != nil {
		return 1
	}
	return 0
}
