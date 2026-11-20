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
//     the application and nothing that went wrong; one that is not supported, which includes one
//     that reads on a connection whose rows of an earlier query are open; a text without a
//     statement; one with another number of arguments than it has parameters; one that the state of
//     the transaction of its connection does not allow; a commit that is too large; one that the
//     core refused for lack of a valid lease or because the instance has stopped, which the core
//     logs itself; and one whose context ended.
//   - Warn: one that failed with another error of the core, such as a commit of unknown outcome.
//   - Error: one that failed with an internal error, a fault of s4ql.
//
// The SQL text is in the event only at Debug, since it can hold the application's data. No value of
// a row or of an argument is in any event.
const eventStatement = "statement"

// The names of the attributes, in the conventions of the core: lowercase, with dots for namespaces
// and underscores inside words.
const (
	// attrKind is the kind of the statement: create_table, drop_table, create_index, drop_index,
	// insert, update, delete, or select.
	attrKind = "statement.kind"
	// attrTable is the name of the table that the statement names, as its value. A drop index names
	// none, and has the empty name.
	attrTable = "statement.table"
	// attrAccess is how a select, an update, or a delete read the rows of its table, as the rules of
	// [chooseAccess] chose it: none, for no row at all, primary_key, for a get or a scan by the primary
	// key, index, for a scan by an index, or full_scan. A statement that failed before the choice has
	// none of the three attributes of its reading.
	attrAccess = "statement.access"
	// attrIndex is the name of the index of an access by an index, as the index was created, and the
	// name of the index that a create index or a drop index names, as its value.
	attrIndex = "statement.index"
	// attrDirection is the direction that the engine drew for a query, forward or backward, as
	// [Engine.reading] draws it, so that a run whose rows depend on it can be told from its event. A
	// statement that draws none, an update, a delete, or a query whose path the tests force, has
	// none, and so has a query without the attribute [attrAccess].
	attrDirection = "statement.direction"
	// attrRowsRead is the number of rows that the statement read from the cache, whether or not its
	// conditions held for them: by its scan or its get, and, for an update, also the row that it reads
	// to check a NULL for a column that does not take it, and each row again that it changes.
	attrRowsRead = "statement.rows_read"
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

	// attrTxKind is the kind of the transaction that spans statements: write or read_only. A statement
	// in such a transaction has it too.
	attrTxKind = "transaction.kind"
	// attrTxEnd is how the transaction ended: commit or rollback.
	attrTxEnd = "transaction.end"
	// attrTxStatements is the number of statements that the connection was given while the
	// transaction was open, also those that failed.
	attrTxStatements = "transaction.statements"
	// attrTxWrites is the number of statements that changed the database in the transaction and
	// succeeded, each of which it annotated. A commit object holds their annotations only if the
	// transaction is committed and changed something in the end.
	attrTxWrites = "transaction.writes"
	// attrTxPosition is the last commit object of the state that the transaction began from.
	attrTxPosition = "transaction.position"
)

// The engine logs one event for each transaction that spans statements, when it ends, by a commit
// or a roll back, in the style of the event of a statement, with the level of [outcomeOf] for the
// error of its end. A commit that succeeded and every roll back that succeeded are at Debug. Its
// attributes are [attrTxKind], [attrTxEnd], [attrTxStatements], [attrTxWrites], [attrTxPosition],
// and for a write transaction [attrParent], which joins it to the core's event of its commit, and
// then [attrOutcome], [attrError], and [attrDuration], from the begin to the end. It has no SQL text
// and no value.
const eventTransaction = "transaction"

// outcomeOf a statement that ended with err, in a word, and the level of its event.
func outcomeOf(err error) (string, slog.Level) {
	var se *sqlerr.Error
	switch {
	case err == nil:
		return "ok", slog.LevelDebug
	case errors.As(err, &se):
		return "sqlite_error", slog.LevelDebug
	case errors.Is(err, stmt.ErrNotSupported), errors.Is(err, core.ErrReaderBusy), errors.Is(err, ErrRowsOpen):
		// A connection runs one read at a time, and a second one is what the caller asked for.
		return "not_supported", slog.LevelDebug
	case errors.Is(err, ErrTxOpen), errors.Is(err, ErrNoTx), errors.Is(err, ErrTxDone):
		// The caller ran something that the transaction of the connection does not allow.
		return "tx_state", slog.LevelDebug
	case errors.Is(err, ErrTooLarge):
		// It is the application's transaction that is too large, and nothing that went wrong.
		return "too_large", slog.LevelDebug
	case errors.Is(err, ErrNoStatement):
		return "no_statement", slog.LevelDebug
	case errors.Is(err, ErrArguments):
		return "wrong_arguments", slog.LevelDebug
	case errors.Is(err, ErrInternal):
		return "internal_error", slog.LevelError
	case errors.Is(err, core.ErrOutcomeUnknown):
		// It is not a refusal, also if it wraps the error of the context that ended.
		return "core_error", slog.LevelWarn
	case errors.Is(err, core.ErrTxLeaseLost):
		// The instance ended the write transaction, so that it could take its lease back.
		return "lease_lost", slog.LevelDebug
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
	// tx is the kind of the transaction that spans statements in which the statement runs, or empty.
	tx   string
	rows int64
	// index is the name of the index that a create index or a drop index names, or of the access.
	index string
	// access is how the statement reads the rows of its table, once that is chosen, and reader is the
	// transaction that it reads them in, which had read before rows when it was chosen.
	access string
	reader reader
	before int64
	// direction is the direction that the engine drew for a query, forward or backward, or empty for a
	// statement that drew none.
	direction string
	// ended is set once the event is logged.
	ended bool
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
	case *stmt.CreateIndex:
		r.kind, r.table, r.index = "create_index", s.Table.Value, s.Index.Value
	case *stmt.DropIndex:
		r.kind, r.index = "drop_index", s.Index.Value
	case *stmt.Insert:
		r.kind, r.table = "insert", s.Table.Value
	case *stmt.Update:
		r.kind, r.table = "update", s.Table.Value
	case *stmt.Delete:
		r.kind, r.table = "delete", s.Table.Value
	case *stmt.Select:
		r.kind, r.table = "select", s.Table.Value
	}
}

// reads records how the statement reads the rows of its table in the transaction of rd: the access
// of the scan of built, the operators of the statement, a source or a sink, if it has one.
func (r *report) reads(rd reader, built any) {
	s := scanOf(built)
	if s == nil {
		return
	}
	switch a := s.access; {
	case a.none:
		r.access = "none"
	case a.full():
		r.access = "full_scan"
	case a.index != "":
		r.access, r.index = "index", a.index
	default:
		r.access = "primary_key"
	}
	r.reader, r.before = rd, rd.rowsRead()
}

// drew records the direction of the reading how, if the engine drew it.
func (r *report) drew(how reading) {
	switch {
	case !how.drawn:
	case how.backward:
		r.direction = "backward"
	default:
		r.direction = "forward"
	}
}

// scanOf the operators built, a source or a sink: the scan that reads the rows of the table of a
// select, an update, or a delete, or nil for a statement that reads no rows of a table.
func scanOf(built any) *scan {
	switch o := built.(type) {
	case source:
		return o.scanned()
	case *update:
		return o.src.scanned()
	case *remove:
		return o.src.scanned()
	}
	return nil
}

// panicked ends the statement with [errPanicked], unless its event is logged already: then the
// statement panicked in the engine before its end. A panic of the application's code in a loop over [Rows.All] is not one: the loop
// ends there as it ends at a break.
func (r *report) panicked(ctx context.Context) {
	if !r.ended {
		r.end(ctx, errPanicked)
	}
}

// at the position of the transaction of the statement.
func (r *report) at(position format.Name, write bool) {
	r.position, r.write = &position, write
}

// end the statement with err, and log its event.
func (r *report) end(ctx context.Context, err error) {
	r.ended = true
	outcome, level := outcomeOf(err)
	if !r.e.log.Enabled(ctx, level) {
		return
	}
	var attrs []slog.Attr
	if r.kind != "" {
		attrs = append(attrs, slog.String(attrKind, r.kind), slog.String(attrTable, r.table))
	}
	if level == slog.LevelDebug {
		attrs = append(attrs, slog.String(attrSQL, r.sql))
	}
	if r.tx != "" {
		attrs = append(attrs, slog.String(attrTxKind, r.tx))
	}
	if r.position != nil {
		attrs = append(attrs, slog.String(attrPosition, r.position.String()))
		if r.write {
			attrs = append(attrs, slog.String(attrParent, r.position.String()))
		}
	}
	if r.access != "" {
		attrs = append(attrs, slog.String(attrAccess, r.access))
	}
	if r.index != "" {
		attrs = append(attrs, slog.String(attrIndex, r.index))
	}
	if r.access != "" && r.direction != "" {
		attrs = append(attrs, slog.String(attrDirection, r.direction))
	}
	if r.access != "" {
		attrs = append(attrs, slog.Int64(attrRowsRead, r.reader.rowsRead()-r.before))
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
