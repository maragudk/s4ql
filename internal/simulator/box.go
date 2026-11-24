//go:build s4ql_browser

package simulator

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// The box is the visitor's own way into the app: one statement of SQL at a time, which the action
// "sql" sends to the instance that the client calls, as a call of the caller "box". It runs on the
// engine of the instance's process as the calls of the application do, and its writes are commit
// objects of the same log. The table of the workload is the application's, so the box may only select
// from it.

// maxBoxRows is the most rows of a select of the box that its event holds. The event counts them all.
const maxBoxRows = 50

// maxCell is the most characters of a text, and the most bytes of a blob, of a cell of a row that the
// event of a select of the box holds.
const maxCell = 200

// appTable is why the box may not run a statement other than a select that names the table of the
// workload.
const appTable = "barcodes is the app's table, so the box can only select from it"

// Cell of a row that a select of the box gave: the type of its value, and the value as text, as
// SQLite prints it, with a blob in hexadecimal. Cut says whether the value is longer than the event
// holds. NULL is no cell.
type Cell struct {
	Type  string `json:"type"`
	Value string `json:"value"`
	Cut   bool   `json:"cut,omitzero"`
}

// boxKey is the key of the call of the box on the context of its write, through which
// [run.coreEvent] gives it the name of its commit object.
type boxKey struct{}

// sql starts the call of the box with the statement text, for the action with the index i, whose
// event e it gives the instance that the call goes to. It returns why the action is ignored, if it
// is: the run has no client, a statement of the box is on its way, or the statement would change
// the table of the workload. The call runs in a task of the run, as the client's calls do, and goes to
// the client's target at the time of the action, which a switch of the client after it does not
// change.
func (r *run) sql(ctx context.Context, i int, text string, e *Event) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.target == nil:
		return "the run has no client"
	case r.box != nil:
		return "a statement of the box is on its way"
	case namesApp(text):
		return appTable
	}
	in := r.target
	e.Instance = in.name
	r.box = r.world.Go(ctx, fmt.Sprint("box-", i), func(ctx context.Context) {
		r.callTarget(ctx, in, r.newCall("box", opSQL, text))
		r.mu.Lock()
		r.box = nil
		r.mu.Unlock()
	})
	return ""
}

// boxPending returns the task of the statement of the box that is on its way, or nil.
func (r *run) boxPending() host.Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.box
}

// namesApp reports whether the statement text is one that the box may not run because it would
// change the table of the workload: a statement that is not a select, and that names the table, as
// its table or as the name of an index, as SQLite compares names. The statement pass of the engine
// reads the text. A text that it does not take, which the engine refuses before it reads the
// database, names nothing here.
func namesApp(text string) bool {
	s, err := stmt.Check(text)
	if err != nil || s == nil {
		return false
	}
	var names []stmt.Name
	switch s := s.(type) {
	case *stmt.Select:
		return false
	case *stmt.CreateTable:
		names = []stmt.Name{s.Table}
	case *stmt.Insert:
		names = []stmt.Name{s.Table}
	case *stmt.Update:
		names = []stmt.Name{s.Table}
	case *stmt.Delete:
		names = []stmt.Name{s.Table}
	case *stmt.DropTable:
		names = []stmt.Name{s.Table}
	case *stmt.CreateIndex:
		names = []stmt.Name{s.Table, s.Index}
	case *stmt.DropIndex:
		names = []stmt.Name{s.Index}
	}
	for _, n := range names {
		if n.Fold() == tableName {
			return true
		}
	}
	return false
}

// boxStatement runs the statement of the call c of the box on conn, and puts how it ended into its
// event e. A select runs as the reads of the application do, and the event has its columns, its
// first [maxBoxRows] rows, and the number of all of them. Any other statement runs as the writes of
// the application do: in a write transaction of its own, from [engine.Conn.Begin] to
// [engine.Conn.Commit], so that its outcomes are those of a write, and the event has the number of
// the rows that it changed, if it counts them. A text that the statement pass refuses, or that the
// engine refuses from the text alone, such as a statement that the pass reads and the engine does not
// run yet, goes as a select, and the engine refuses it before it reads the database.
func (r *run) boxStatement(ctx context.Context, conn *engine.Conn, c *call, e *Event) {
	s, err := stmt.Check(c.text)
	if err == nil && s != nil {
		// With as many arguments as the statement has parameters, only the text can fail the check.
		err = engine.Check(c.text, s.Parameters())
	}
	if _, isSelect := s.(*stmt.Select); isSelect || s == nil || err != nil {
		var position format.Name
		e.What, e.Error, position = r.query(ctx, conn, c.text, e)
		if e.What == "served" {
			e.Position = position.String()
		}
		return
	}
	ctx = context.WithValue(ctx, boxKey{}, c)
	if err := conn.Begin(ctx); err != nil {
		e.What, e.Error = refusal(err), err.Error()
		return
	}
	res, err := conn.Exec(ctx, c.text)
	if err != nil {
		_ = conn.Rollback(ctx)
		e.What, e.Error = "abandoned", err.Error()
		return
	}
	if res.SetsChanges {
		e.Affected = new(res.Changes)
	}
	e.What, e.Error = "committed", ""
	if err := conn.Commit(ctx); err != nil {
		e.What, e.Error = "not_committed", err.Error()
		if errors.Is(err, core.ErrOutcomeUnknown) {
			e.What = "unknown"
		}
	}
	r.mu.Lock()
	e.Commit = c.commit
	r.mu.Unlock()
}

// query runs the select text on conn, as [run.read] does, and puts its columns, its first rows, and
// the number of its rows into e. It says how the call ended, its error, and its position.
func (r *run) query(ctx context.Context, conn *engine.Conn, text string, e *Event) (string, string, format.Name) {
	seen := &statementSeen{}
	ctx = context.WithValue(ctx, statementKey{}, seen)
	rows, err := conn.Query(ctx, text)
	var out [][]*Cell
	count := 0
	if err == nil {
		for _, col := range rows.Columns() {
			e.Columns = append(e.Columns, col.Name)
		}
		for row, rerr := range rows.All() {
			if rerr != nil {
				err = rerr
				break
			}
			count++
			if len(out) < maxBoxRows {
				out = append(out, cells(row))
			}
		}
		if cerr := rows.Close(); err == nil {
			err = cerr
		}
	}
	switch {
	case err == nil && seen.position != nil:
		e.Rows, e.Count = nonNil(out), count
		return "served", "", *seen.position
	case err == nil:
		return "failed", "the engine gave no position for the select", format.Name{}
	default:
		e.Columns = nil
		return refusal(err), err.Error(), format.Name{}
	}
}

// cells of a row of a select of the box.
func cells(row []*s4qlpb.Value) []*Cell {
	out := make([]*Cell, len(row))
	for i, v := range row {
		switch v.WhichKind() {
		case s4qlpb.Value_Integer_case:
			out[i] = &Cell{Type: "integer", Value: fmt.Sprint(v.GetInteger())}
		case s4qlpb.Value_Real_case:
			out[i] = &Cell{Type: "real", Value: realText(v.GetReal())}
		case s4qlpb.Value_Text_case:
			text, cut := v.GetText(), false
			if utf8.RuneCountInString(text) > maxCell {
				text, cut = string([]rune(text)[:maxCell]), true
			}
			out[i] = &Cell{Type: "text", Value: text, Cut: cut}
		case s4qlpb.Value_Blob_case:
			b := v.GetBlob()
			out[i] = &Cell{Type: "blob", Value: hex.EncodeToString(b[:min(len(b), maxCell)]), Cut: len(b) > maxCell}
		}
	}
	return out
}

// realText of a real as SQLite prints it: with 15 significant digits, and with a decimal point, so
// that 1.0 is not read as the integer 1.
func realText(f float64) string {
	s := fmt.Sprintf("%.15g", f)
	if strings.ContainsAny(s, ".ni") {
		return s
	}
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		return s[:i] + ".0" + s[i:]
	}
	return s + ".0"
}
