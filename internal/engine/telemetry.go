package engine

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The engine logs one event for each statement that it runs, when the statement ends: for a query,
// when its rows are read to the end or closed. The attributes are flat, and those that the storage
// core's events have too have the core's names, so that the event of a write statement and the event
// of the commit that it caused can be joined: by commit.parent, which is the same for both, and by
// the context, which both are logged with.
//
// The level follows the conventions of the core, so that a healthy instance logs nothing at the
// default level, Info:
//
//   - Debug: a statement that succeeded; one that failed with SQLite's error, which is an answer to
//     the application and nothing that went wrong; one that is not supported; a text without a
//     statement; one that the core refused for lack of a valid lease or because the instance has
//     stopped, which the core logs itself; and one whose context ended.
//   - Warn: one that failed with another error of the core, such as a commit of unknown outcome.
//   - Error: one that failed with an internal error, a fault of s4ql.
//
// The SQL text is in the event only at Debug, since it can hold the application's data. No value of
// a row is in any event.
const eventStatement = "statement"

// The names of the attributes, in the conventions of the core: lowercase, with dots for namespaces
// and underscores inside words.
const (
	// attrKind is the kind of the statement: create_table, drop_table, insert, or select.
	attrKind = "statement.kind"
	// attrTable is the name of the table that the statement names, as its value.
	attrTable = "statement.table"
	// attrSQL is the SQL text, at Debug only.
	attrSQL = "statement.sql"
	// attrRows is the number of rows that the statement returned or changed.
	attrRows = "statement.rows"
	// attrPosition is the last commit object of the state that the statement saw: the position of its
	// read transaction, or the parent of its write transaction.
	attrPosition = "statement.position"
	// attrParent is the core's name for the parent of a commit, which a write statement has.
	attrParent = "commit.parent"
	// attrOutcome is how the statement ended, in a word: see [outcomeOf].
	attrOutcome = "outcome"
	// attrError is the error of a statement that failed.
	attrError = "error"
	// attrCode and attrExtendedCode are the result codes of SQLite's error.
	attrCode         = "sqlite.code"
	attrExtendedCode = "sqlite.extended_code"
	// attrDuration is the duration of the statement on the monotonic clock of the host, from the call
	// to its end.
	attrDuration = "duration.total"
)

// outcomeOf a statement that ended with err, in a word, and the level of its event.
func outcomeOf(err error) (string, slog.Level) {
	var se *sqlerr.Error
	switch {
	case err == nil:
		return "ok", slog.LevelDebug
	case errors.As(err, &se):
		return "sqlite_error", slog.LevelDebug
	case errors.Is(err, stmt.ErrNotSupported):
		return "not_supported", slog.LevelDebug
	case errors.Is(err, ErrNoStatement):
		return "no_statement", slog.LevelDebug
	case errors.Is(err, ErrInternal):
		return "internal_error", slog.LevelError
	case errors.Is(err, core.ErrOutcomeUnknown):
		// An error that wraps it wraps no other error of the core, but can wrap the context's.
		return "core_error", slog.LevelWarn
	case errors.Is(err, core.ErrNoLease), errors.Is(err, core.ErrStopped):
		return "refused", slog.LevelDebug
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled", slog.LevelDebug
	}
	return "core_error", slog.LevelWarn
}

// report of one statement, which becomes its event when it ends.
type report struct {
	e     *Engine
	start time.Duration
	sql   string
	kind  string
	table string
	// position is set once the statement has a transaction, and write once it is a write.
	position *format.Name
	write    bool
	rows     int64
}

// newReport of a statement of the text sql that starts now.
func (e *Engine) newReport(sql string) *report {
	return &report{e: e, start: e.host.Monotonic(), sql: sql}
}

// of the statement s, whose kind and table it records.
func (r *report) of(s stmt.Statement) {
	switch s := s.(type) {
	case *stmt.CreateTable:
		r.kind, r.table = "create_table", s.Table.Value
	case *stmt.DropTable:
		r.kind, r.table = "drop_table", s.Table.Value
	case *stmt.Insert:
		r.kind, r.table = "insert", s.Table.Value
	case *stmt.Select:
		r.kind, r.table = "select", s.Table.Value
	}
}

// at the position of the transaction of the statement.
func (r *report) at(position format.Name, write bool) {
	r.position, r.write = &position, write
}

// end the statement with err, and log its event.
func (r *report) end(ctx context.Context, err error) {
	outcome, level := outcomeOf(err)
	if !r.e.log.Enabled(ctx, level) {
		return
	}
	attrs := make([]slog.Attr, 0, 12)
	if r.kind != "" {
		attrs = append(attrs, slog.String(attrKind, r.kind), slog.String(attrTable, r.table))
	}
	if level == slog.LevelDebug {
		attrs = append(attrs, slog.String(attrSQL, r.sql))
	}
	if r.position != nil {
		attrs = append(attrs, slog.String(attrPosition, r.position.String()))
		if r.write {
			attrs = append(attrs, slog.String(attrParent, r.position.String()))
		}
	}
	attrs = append(attrs, slog.Int64(attrRows, r.rows), slog.String(attrOutcome, outcome))
	var se *sqlerr.Error
	if errors.As(err, &se) {
		attrs = append(attrs, slog.Int(attrCode, se.Code), slog.Int(attrExtendedCode, se.ExtendedCode))
	}
	if err != nil {
		attrs = append(attrs, slog.String(attrError, err.Error()))
	}
	attrs = append(attrs, slog.Duration(attrDuration, r.e.host.Monotonic()-r.start))
	r.e.log.LogAttrs(ctx, level, eventStatement, attrs...)
}
