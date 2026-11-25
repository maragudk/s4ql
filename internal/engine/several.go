package engine

import (
	"context"
	"fmt"

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
// connection; an error that comes before any statement runs is logged as the event of a statement of
// the whole text.
//
// It gives the result of the last statement that sets SQLite's count of changes, also of one that
// fails, or no result if none does, as sqlite3_changes counts after the statements: a statement that
// sets no count leaves the one before.
//
// A text of one statement runs as Exec runs it, with the same errors, but for what Split refuses of
// the whole text. Before any statement of a text with several runs, it fails with the errors of
// Split of the whole text, with [ErrNoStatement] for a text without a statement, and, if every
// statement of the text is one that s4ql supports, with an error that matches [ErrArguments] if
// there are more arguments than the statements have parameters. Then, statement by statement: a
// statement that gets fewer arguments than it has parameters fails with an error that matches
// ErrArguments, and an argument that no column holds fails its statement as Exec fails it, by its
// number in the call. When the statements that Split read have run, the text fails with the error
// of the first statement that Split refused, SQLite's syntax error or "not supported", if there is
// one. The offsets of all errors are in src.
func (c *Conn) ExecAll(ctx context.Context, src string, args ...*s4qlpb.Value) (Result, error) {
	parts, failed := stmt.Split(src)
	if err := before(parts, failed, len(args)); err != nil {
		c.refuse(ctx, src, err)
		return Result{}, err
	}
	one := len(parts) == 1 && failed == nil
	var res Result
	used := 0
	for _, p := range parts {
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
		r, err := c.execPart(ctx, src, p, take, used, count)
		used += len(take)
		if r.SetsChanges {
			res = r
		}
		if err != nil {
			return res, err
		}
	}
	if failed != nil {
		c.refuse(ctx, src[parts[len(parts)-1].Span.End:], failed)
		return res, failed
	}
	return res, nil
}

// CheckAll is [Check] of the text src for [Conn.ExecAll], with n arguments, without the database
// and without an event: the error of [stmt.Split] of the text, [ErrNoStatement] for a text without a
// statement, or an error of [ErrArguments] for another number of arguments than the statements of
// the text have parameters; otherwise nil. It does not check what the arguments are.
func CheckAll(src string, n int) error {
	parts, err := stmt.Split(src)
	switch {
	case err != nil:
		return err
	case len(parts) == 0:
		return ErrNoStatement
	case len(parts) == 1:
		return countArguments(parts[0].Statement.Parameters(), n)
	}
	if total := parameters(parts); n != total {
		return fmt.Errorf("%w: %v for a text whose %v statements have %v %v", ErrArguments, n, len(parts), total, parameterNoun(total))
	}
	return nil
}

// before is the error of the text of the parts, which [stmt.Split] gave with failed, with n
// arguments, that comes before any statement of it runs, as [Conn.ExecAll] says, or nil.
func before(parts []stmt.Part, failed error, n int) error {
	switch {
	case len(parts) == 0 && failed != nil:
		return failed
	case len(parts) == 0:
		return ErrNoStatement
	case len(parts) == 1 || failed != nil:
		// A statement of its own counts its arguments, and the statements before one that failed to
		// check leave the arguments after theirs to it.
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

// refuse the statement of the text src with err, which comes before it runs: log its event.
func (c *Conn) refuse(ctx context.Context, src string, err error) {
	c.newReport(src).end(ctx, err)
}

// execPart runs the statement of the part p of the text src with args, the arguments of the call
// after first, as [Conn.Exec] runs a text of one statement, with the error count of their number, or
// nil if it is right.
func (c *Conn) execPart(ctx context.Context, src string, p stmt.Part, args []*s4qlpb.Value, first int, count error) (Result, error) {
	r := c.newReport(src[p.Span.Start:p.Span.End])
	// Every statement has logged its event when execPlan returns, so one without is a panic.
	defer r.panicked(ctx)
	r.of(p.Statement)
	err := count
	if err == nil {
		err = checkValues(args, first)
	}
	if err != nil {
		r.end(ctx, err)
		return Result{}, err
	}
	plan, text := planOf(p.Statement, src)
	return c.execPlan(ctx, r, plan, text, args)
}
