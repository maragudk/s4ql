package engine

import (
	"context"
	"fmt"
	"time"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// ExecAll runs the statements of the text src one after another, as [stmt.Split] reads them, with
// args for their parameters, by position: the first statement takes the first arguments, as many as
// it has parameters, the next the arguments after those, and so on. Each statement runs as [Conn.Exec]
// runs it: as a transaction of its own, which commits by itself, or as a statement of the transaction
// that spans statements on the connection, if one is open. Nothing is wrapped around the statements.
// A statement that fails ends the run with its error: the statements before it stay as they ran, and
// none after it runs. Each statement is logged as an event of its own, as if it ran alone, with the
// text of its part of src as its text, and is counted as a statement of the transaction of the
// connection; an error that comes before any statement runs is logged as the event of a statement
// whose text is the whole text, and the error of a statement that Split refused as the event of a
// statement whose text is the rest of the text from that statement on. The duration of the event of
// the first statement, or of the error before any statement runs, starts with the call, so that it
// holds the split and the check of the whole text, as that of Exec holds the check of its text; the
// event of each later statement starts when the statement does, and that of a statement that Split
// refused when the statements before it have run.
//
// It gives the result of the last statement that sets SQLite's count of changes, also of one that
// fails, or no result if none does, as sqlite3_changes counts after the statements: a statement that
// sets no count leaves the one before.
//
// A text of one statement runs as Exec runs it, with the same errors, but for what Split refuses of
// the whole text. Before any statement of a text with several runs, it fails with the errors that
// [CheckAll] gives. Then, statement by statement: a statement that gets fewer arguments than it has
// parameters fails with an error that matches [ErrArguments], and an argument that no column holds,
// or whose value its statement refuses, fails its statement as Exec fails it, with the number of the
// argument among the arguments of its statement, as if the statement ran alone. When the statements
// that Split read have run, the text fails with the error of the first statement that Split refused,
// SQLite's syntax error or "not supported", if there is one. The offsets of all errors are in src.
func (c *Conn) ExecAll(ctx context.Context, src string, args ...*s4qlpb.Value) (Result, error) {
	if c.e.bugs.oneCommit && c.tx == nil {
		return c.inOneCommit(ctx, src, args)
	}
	return c.execAll(ctx, src, args)
}

// execAll is [Conn.ExecAll] of src with args.
func (c *Conn) execAll(ctx context.Context, src string, args []*s4qlpb.Value) (Result, error) {
	start := c.e.host.Monotonic()
	parts, failed := c.split(ctx, src, start)
	if err := before(parts, failed, len(args)); err != nil {
		c.refuse(ctx, src, start, err)
		return Result{}, err
	}
	one := len(parts) == 1 && failed == nil
	var text *textRun
	if c.e.bugs.ofText() && !one {
		text = &textRun{src: src, args: args}
		ctx = context.WithValue(ctx, textKey{}, text)
		defer text.commit(ctx)
	}
	var res Result
	// first is the error of the first statement that failed, with the planted bug goOn.
	var first error
	used := 0
	for i, p := range parts {
		if i > 0 {
			start = c.e.host.Monotonic()
		}
		if text != nil {
			text.commit(ctx)
			text.follows = i < len(parts)-1
		}
		n := p.Statement.Parameters()
		take := args[used:min(len(args), used+n)]
		var count error
		switch {
		case one:
			count = countArguments(n, len(args))
		case len(take) < n:
			count = fmt.Errorf("%w: %v for a text whose statements up to the one at offset %v have %v %v", ErrArguments, len(args),
				p.Statement.Bounds().Start, used+n, parameterNoun(used+n))
		}
		r, err := c.execPart(ctx, src, p, take, count, start)
		used += len(take)
		// The planted bug firstResult keeps the result of the first statement that sets one.
		if r.SetsChanges && (!c.e.bugs.firstResult || !res.SetsChanges) {
			res = r
		}
		if err != nil && c.e.bugs.undoInTx && i > 0 && c.tx != nil && c.tx.write != nil {
			// The planted bug: the failure undoes the transaction.
			c.undo(ctx)
		}
		if err != nil && c.e.bugs.goOn {
			// The planted bug: the statements after the one that failed run.
			if first == nil {
				first = err
			}
			continue
		}
		if err != nil {
			return res, err
		}
	}
	if failed != nil {
		c.refuse(ctx, src[parts[len(parts)-1].Span.End:], c.e.host.Monotonic(), failed)
		if first != nil {
			return res, first
		}
		return res, failed
	}
	return res, first
}

// split src with [stmt.Split], and log the event of a statement of the whole text that started at
// start if it panics, as [Conn.Exec] logs a panic of its check.
func (c *Conn) split(ctx context.Context, src string, start time.Duration) ([]stmt.Part, error) {
	r := c.e.newReportFrom(src, start)
	defer r.panicked(ctx)
	parts, err := stmt.Split(src)
	// The event of each statement is its own.
	r.ended = true
	return parts, err
}

// CheckAll the text src for [Conn.ExecAll] with n arguments, without the database and without an
// event: the errors that ExecAll gives before any statement of the text runs, or nil. They are, in
// this order, an error of the whole text from [stmt.Split], or the error of its first statement if
// that is the one that Split refused; [ErrNoStatement] for a text without a statement; the error of
// [ErrArguments] of a text of one statement, which counts its arguments as [Check] does; and, if
// every statement of the text is one that s4ql supports, the error of ErrArguments for more
// arguments than the statements have parameters. It does not check what the arguments are.
func CheckAll(src string, n int) error {
	parts, failed := stmt.Split(src)
	if err := before(parts, failed, n); err != nil {
		return err
	}
	if len(parts) == 1 && failed == nil {
		return countArguments(parts[0].Statement.Parameters(), n)
	}
	return nil
}

// before is the error that comes before any statement runs of the text whose parts [stmt.Split]
// gave with the error failed, with n arguments, but the count of the arguments of a text of one
// statement, which its statement makes: as [CheckAll] says, or nil.
func before(parts []stmt.Part, failed error, n int) error {
	switch {
	case len(parts) == 0 && failed != nil:
		return failed
	case len(parts) == 0:
		return ErrNoStatement
	case len(parts) == 1 && failed == nil:
		return nil
	case failed != nil:
		// The statements before one that failed to check leave the arguments after theirs to it.
		return nil
	}
	if total := parameters(parts); n > total {
		return fmt.Errorf("%w: %v for a text whose %v statements have %v %v", ErrArguments, n, len(parts), total, parameterNoun(total))
	}
	return nil
}

// parameters of the statements of parts, together.
func parameters(parts []stmt.Part) int {
	n := 0
	for _, p := range parts {
		n += p.Statement.Parameters()
	}
	return n
}

// parameterNoun for n parameters.
func parameterNoun(n int) string {
	if n == 1 {
		return "parameter"
	}
	return "parameters"
}

// refuse the statement of the text src that started at start with err, which comes before it runs:
// log its event.
func (c *Conn) refuse(ctx context.Context, src string, start time.Duration, err error) {
	c.newReportFrom(src, start).end(ctx, err)
}

// execPart runs the statement of the part p of the text src that started at start with args, the
// arguments of its parameters, as [Conn.Exec] runs a text of one statement, with the error count of
// their number, or nil if it is right.
func (c *Conn) execPart(ctx context.Context, src string, p stmt.Part, args []*s4qlpb.Value, count error, start time.Duration) (Result, error) {
	r := c.newReportFrom(src[p.Span.Start:p.Span.End], start)
	// Every statement has logged its event when execPlan returns, so one without is a panic.
	defer r.panicked(ctx)
	r.of(p.Statement)
	err := count
	if err == nil {
		err = checkValues(args)
	}
	if err == nil {
		err = textRefusal(p.Statement)
	}
	if err != nil {
		r.end(ctx, err)
		return Result{}, err
	}
	plan, text := planOf(p.Statement, src)
	return c.execPlan(ctx, r, plan, text, args)
}
