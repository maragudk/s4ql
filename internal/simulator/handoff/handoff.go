//go:build s4ql_browser

// Package handoff passes the commands of a page to the controller of a live session of the
// simulator, and the answers back. The controller is a task of the scheduler of internal/sim, and it
// waits here outside the scheduler on purpose: a [Line] is made outside the testing/synctest bubble
// of the session, so a wait on it is not durable, the scheduler does not go on while the controller
// waits, and fake time stands still until the next command.
package handoff

// Line between the asking side, which sends commands of the type C, and the answering side, which
// answers each with an answer of the type A. The answering side answers first, once, before the
// first command. One side at a time uses each end.
type Line[C, A any] struct {
	commands chan C
	answers  chan A
}

// New line. It must be made outside the bubble of the answering side.
func New[C, A any]() *Line[C, A] {
	return &Line[C, A]{commands: make(chan C), answers: make(chan A)}
}

// Ask the command c, and wait for its answer.
func (l *Line[C, A]) Ask(c C) A {
	l.commands <- c
	return <-l.answers
}

// Start the answering side, f, on a goroutine of its own, and wait for the first answer, which it
// gives before the first command.
func (l *Line[C, A]) Start(f func()) A {
	go f()
	return <-l.answers
}

// Answer with a, and wait for the next command.
func (l *Line[C, A]) Answer(a A) C {
	l.answers <- a
	return <-l.commands
}

// Last answer, after which no command comes.
func (l *Line[C, A]) Last(a A) {
	l.answers <- a
}
