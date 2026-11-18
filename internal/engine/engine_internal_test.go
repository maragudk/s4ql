package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
)

// newTestEngine on the fake f.
func newTestEngine(f *fake, b bugs) *Engine {
	return newEngine(f, host.New(), Options{bugs: b})
}

// newLoggedEngine on the fake f, whose events h records, each as its attributes.
func newLoggedEngine(f *fake, b bugs, h *recorder) *Engine {
	return newEngine(f, host.New(), Options{bugs: b, Logger: slog.New(h)})
}

// recorder of events, each as its attributes, with its message under "msg".
type recorder struct {
	events []map[string]string
}

func (h *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *recorder) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *recorder) WithGroup(string) slog.Handler            { return h }

func (h *recorder) Handle(_ context.Context, r slog.Record) error {
	m := map[string]string{"msg": r.Message}
	r.Attrs(func(a slog.Attr) bool {
		m[a.Key] = a.Value.String()
		return true
	})
	h.events = append(h.events, m)
	return nil
}

// last event, without its duration.
func (h *recorder) last() string {
	e := maps.Clone(h.events[len(h.events)-1])
	delete(e, attrDuration)
	delete(e, attrSQL)
	return fmt.Sprint(e)
}

// ticking is a host whose monotonic clock moves by a second at each reading.
type ticking struct {
	host.Host
	now time.Duration
}

func (h *ticking) Monotonic() time.Duration {
	h.now += time.Second
	return h.now
}

func TestEngine_report(t *testing.T) {
	t.Run("should log one event for a write through Query", func(t *testing.T) {
		h := &recorder{}
		f := newFake().with("t1", []string{"v1"})
		rows, err := newLoggedEngine(f, bugs{}, h).Query(t.Context(), "drop table t1")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
		is.Equal(t, 1, len(h.events))
		is.Equal(t, "drop_table", h.events[0][attrKind])
		is.Equal(t, "ok", h.events[0][attrOutcome])
	})

	t.Run("should log one event with the outcome internal_error for a panic in the end of the read transaction of a query", func(t *testing.T) {
		for _, end := range []string{"last row", "close"} {
			h := &recorder{}
			f := newFake().with("t1", []string{"v1"}, []int64{1})
			f.endPanics = true
			rows, err := newLoggedEngine(f, bugs{}, h).Query(t.Context(), "select * from t1")
			is.NotError(t, err)
			func() {
				defer func() { is.Equal(t, any("the end panics"), recover()) }()
				if end == "close" {
					_ = rows.Close()
					return
				}
				for _, err := range rows.All() {
					is.NotError(t, err)
				}
			}()
			is.Equal(t, 1, len(h.events), end)
			is.Equal(t, "internal_error", h.events[0][attrOutcome], end)
		}
	})

	t.Run("should log one event with the outcome internal_error at Error for a panic in the engine, and an early stop for a panic of the consumer", func(t *testing.T) {
		cases := []struct {
			name, src, panic string
			set              func(f *fake)
			query            bool
			outcome          string
		}{
			{name: "build of a query, through Query", src: "select * from t1", panic: "the lookup panics", set: func(f *fake) { f.tablePanics = true }, query: true},
			{name: "build of a query, through Exec", src: "select * from t1", panic: "the lookup panics", set: func(f *fake) { f.tablePanics = true }},
			{name: "build of a write, through Exec", src: "drop table t1", panic: "the lookup panics", set: func(f *fake) { f.tablePanics = true }},
			{name: "write, through Exec", src: "insert into t1 (v1) values (3)", panic: "the upsert panics", set: func(f *fake) { f.upsertPanics = true }},
			{name: "write, through Query", src: "insert into t1 (v1) values (3)", panic: "the upsert panics", set: func(f *fake) { f.upsertPanics = true }, query: true},
			{name: "scan, through Query", src: "select * from t1", panic: "the scan panics", set: func(f *fake) { f.scanPanics = true }, query: true},
			{name: "scan, through Exec", src: "select * from t1", panic: "the scan panics", set: func(f *fake) { f.scanPanics = true }},
			{name: "consumer, through Query", src: "select * from t1", panic: "the consumer", set: func(*fake) {}, query: true, outcome: "ok"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				h := &recorder{}
				f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
				c.set(f)
				e := newLoggedEngine(f, bugs{}, h)
				func() {
					defer func() { is.Equal(t, any(c.panic), recover()) }()
					if !c.query {
						_, _ = e.Exec(t.Context(), c.src)
						return
					}
					rows, err := e.Query(t.Context(), c.src)
					is.NotError(t, err)
					for _, err := range rows.All() {
						is.NotError(t, err)
						if c.panic == "the consumer" {
							panic("the consumer")
						}
					}
				}()
				is.Equal(t, 0, f.open)
				is.Equal(t, 1, len(h.events))
				outcome := cmp.Or(c.outcome, "internal_error")
				is.Equal(t, outcome, h.events[0][attrOutcome])
				if outcome == "internal_error" {
					is.Equal(t, "s4ql: internal error: the statement panicked", h.events[0][attrError])
					_, ok := h.events[0][attrSQL]
					is.True(t, !ok, "the text at Error")
				}
			})
		}
	})

	t.Run("should give the duration from the start of the statement to its end, the kind of a drop, and no text at Warn", func(t *testing.T) {
		h := &recorder{}
		f := newFake().with("t1", []string{"v1"})
		clock := &ticking{Host: host.New(), now: time.Hour}
		e := newEngine(f, clock, Options{Logger: slog.New(h)})
		_, err := e.Exec(t.Context(), "drop table T1")
		is.NotError(t, err)
		last := h.events[len(h.events)-1]
		is.Equal(t, "1s", last[attrDuration])
		is.Equal(t, "drop_table", last[attrKind])
		is.Equal(t, "T1", last[attrTable])
		is.Equal(t, "drop table T1", last[attrSQL])

		f.beginFails = core.ErrBusy
		_, err = e.Exec(t.Context(), "drop table t1")
		is.True(t, errors.Is(err, core.ErrBusy))
		last = h.events[len(h.events)-1]
		is.Equal(t, "core_error", last[attrOutcome])
		_, ok := last[attrSQL]
		is.True(t, !ok, "the text at Warn")
	})

	empty := format.EmptyLog.String()
	t.Run("should give the position, the rows, and the outcome of each path in the event", func(t *testing.T) {
		h := &recorder{}
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		e := newLoggedEngine(f, bugs{}, h)

		_, _ = e.Exec(t.Context(), "create table t1 (")
		is.Equal(t, "map[error:incomplete input msg:statement outcome:sqlite_error sqlite.code:1 sqlite.extended_code:1 statement.rows:0]", h.last())

		rows, err := e.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
		}
		is.Equal(t, fmt.Sprintf("map[msg:statement outcome:ok statement.kind:select statement.position:%v statement.rows:2 statement.table:t1]", empty), h.last())

		f.scanFails = 1
		_, err = e.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, fmt.Sprintf("map[error:the scan fails msg:statement outcome:core_error statement.kind:select statement.position:%v statement.rows:1 statement.table:t1]", empty), h.last())

		f.scanFails, f.endFails = 1, errors.New("the end fails")
		_, err = e.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, "the scan fails", h.events[len(h.events)-1][attrError])

		f.scanFails = -1
		rows, err = e.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		var last error
		for _, err := range rows.All() {
			last = err
		}
		is.Equal(t, "the end fails", last.Error())
		is.Equal(t, "the end fails", h.events[len(h.events)-1][attrError])

		_, err = e.Exec(t.Context(), "select * from nosuch")
		is.Equal(t, "no such table: nosuch", err.Error())
		is.Equal(t, "no such table: nosuch", h.events[len(h.events)-1][attrError])
		f.endFails = nil

		f.annotateFails = errors.New("annotate fails")
		_, err = e.Exec(t.Context(), "insert into t1 (v1) values (3)")
		is.Equal(t, "annotate fails", err.Error())
		is.Equal(t, fmt.Sprintf("map[commit.parent:%[1]v error:annotate fails msg:statement outcome:core_error statement.kind:insert statement.position:%[1]v statement.rows:0 statement.table:t1]", empty), h.last())
		f.annotateFails = nil

		f.readFails = core.ErrNoLease
		_, err = e.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrNoLease))
		is.Equal(t, "map[error:no valid lease msg:statement outcome:refused statement.kind:select statement.rows:0 statement.table:t1]", h.last())
		f.readFails = nil

		f.commitFails = core.ErrOutcomeUnknown
		_, err = newLoggedEngine(f, bugs{ackUnknown: true}, h).Exec(t.Context(), "insert into t1 (v1) values (4)")
		is.NotError(t, err)
		is.Equal(t, "1", h.events[len(h.events)-1][attrRows])
	})
}

func TestEngine_write(t *testing.T) {
	t.Run("should annotate and commit a statement that succeeds, and roll back one that fails, at each step", func(t *testing.T) {
		f := newFake()
		e := newTestEngine(f, bugs{})
		_, err := e.Exec(t.Context(), "; "+t1+" ; -- x")
		is.NotError(t, err)
		is.Equal(t, "[table t1 create t1 annotate "+t1+" ; commit]", fmt.Sprint(f.calls))

		f.calls = nil
		_, err = e.Exec(t.Context(), t1)
		is.Equal(t, "table t1 already exists", err.Error())
		is.Equal(t, "[table t1 rollback]", fmt.Sprint(f.calls))

		f.calls, f.upsertFails = nil, errors.New("upsert fails")
		_, err = e.Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "upsert fails", err.Error())
		is.Equal(t, "[table t1 get t1 1 upsert t1 1 rollback]", fmt.Sprint(f.calls))

		f.calls, f.upsertFails, f.annotateFails = nil, nil, errors.New("annotate fails")
		_, err = e.Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "annotate fails", err.Error())
		is.Equal(t, "[table t1 get t1 1 upsert t1 1 annotate insert into t1 (v1) values (1) rollback]", fmt.Sprint(f.calls))

		f.calls, f.annotateFails, f.commitFails = nil, nil, core.ErrOutcomeUnknown
		res, err := e.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, errors.Is(err, core.ErrOutcomeUnknown))
		is.Equal(t, Result{SetsChanges: true}, res)
		is.Equal(t, "commit", f.calls[len(f.calls)-1])
		is.Equal(t, 0, f.open)
	})

	t.Run("should roll back when an operator panics", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		f.upsertPanics = true
		e := newTestEngine(f, bugs{})
		func() {
			defer func() { is.Equal(t, "the upsert panics", recover()) }()
			_, _ = e.Exec(t.Context(), "insert into t1 (v1) values (1)")
		}()
		is.Equal(t, "rollback", f.calls[len(f.calls)-1])
		is.Equal(t, 0, f.open)
	})

	t.Run("should pass on the error of a begin", func(t *testing.T) {
		f := newFake()
		f.beginFails = core.ErrNoLease
		_, err := newTestEngine(f, bugs{}).Exec(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrNoLease))
		_, err = newTestEngine(f, bugs{}).Query(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrNoLease))
	})

	t.Run("should acknowledge a commit of unknown outcome, and leave a failed transaction open, with the planted bugs", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		f.commitFails = core.ErrOutcomeUnknown
		res, err := newTestEngine(f, bugs{ackUnknown: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, Result{Changes: 1, SetsChanges: true}, res)
		f.commitFails = core.ErrNoLease
		_, err = newTestEngine(f, bugs{ackUnknown: true}).Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, errors.Is(err, core.ErrNoLease))
		f.commitFails = nil

		f = newFake()
		_, err = newTestEngine(f, bugs{keepOpen: true}).Exec(t.Context(), "drop table t")
		is.True(t, err != nil)
		is.Equal(t, 1, f.open)

		f = newFake().with("t1", []string{"v1"})
		rows, err := newTestEngine(f, bugs{keepRead: true}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
		is.Equal(t, 1, f.open)
		rows, err = newTestEngine(f, bugs{keepRead: true}).Query(t.Context(), "drop table t1")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
	})

	t.Run("should check an insert in a read transaction of its own, and write without the check, with the planted bug", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		_, err := newTestEngine(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, "[table t1 get t1 1 end table t1 upsert t1 1 annotate insert into t1 (v1) values (1) commit]", fmt.Sprint(f.calls))
		_, err = newTestEngine(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", err.Error())
		_, err = newTestEngine(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t2 (v1) values (1)")
		is.Equal(t, "no such table: t2", err.Error())
		f.readFails = core.ErrNoLease
		_, err = newTestEngine(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.True(t, errors.Is(err, core.ErrNoLease))
		is.Equal(t, 0, f.open)
		// Only an insert is checked so.
		_, err = newTestEngine(f, bugs{checkOutside: true}).Exec(t.Context(), "drop table t1")
		is.NotError(t, err)
	})
}

func TestEngine_query(t *testing.T) {
	t.Run("should end the read transaction when building the query panics", func(t *testing.T) {
		f := newFake()
		f.tablePanics = true
		e := newTestEngine(f, bugs{})
		for _, src := range []string{"select * from t1", "select * from T1"} {
			func() {
				defer func() { is.Equal(t, "the lookup panics", recover()) }()
				_, _ = e.Query(t.Context(), src)
			}()
			is.Equal(t, 0, f.open)
		}
	})

	t.Run("should give a failure of the end of the read transaction from Close after an early stop, once", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		f.endFails = errors.New("the end fails")
		rows, err := newTestEngine(f, bugs{}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
			break
		}
		is.Equal(t, "the end fails", rows.Close().Error())
		is.NotError(t, rows.Close())

		rows, err = newTestEngine(f, bugs{}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		var last error
		for _, err := range rows.All() {
			last = err
		}
		is.Equal(t, "the end fails", last.Error())
		is.NotError(t, rows.Close())
	})

	t.Run("should end the read transaction when the query cannot be built, and pass on the error of a read", func(t *testing.T) {
		f := newFake()
		e := newTestEngine(f, bugs{})
		_, err := e.Query(t.Context(), "select * from t1")
		is.Equal(t, "no such table: t1", err.Error())
		is.Equal(t, "[table t1 end]", fmt.Sprint(f.calls))
		f.readFails = core.ErrStopped
		_, err = e.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		_, err = e.Exec(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		is.Equal(t, 0, f.open)
	})

	t.Run("should end the read transaction once, after an error in the middle", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		f.scanFails = 1
		e := newTestEngine(f, bugs{})
		rows, err := e.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		var last error
		for _, err := range rows.All() {
			last = err
		}
		is.Equal(t, "the scan fails", last.Error())
		is.NotError(t, rows.Close())
		is.Equal(t, 0, f.open)
		_, err = e.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, 0, f.open)
	})

	t.Run("should keep the read transaction when Close is called during the loop, until the loop ends", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		rows, err := newTestEngine(f, bugs{}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		n := 0
		for _, err := range rows.All() {
			is.NotError(t, err)
			n++
			is.NotError(t, rows.Close())
			is.Equal(t, 1, f.open)
			is.Equal(t, 1, f.scans)
		}
		is.Equal(t, 1, n)
		is.Equal(t, 0, f.open)
	})

	t.Run("should give each row once, and nothing in a second loop", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		rows, err := newTestEngine(f, bugs{}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		is.Equal(t, "[{v1 INT}]", fmt.Sprint(rows.Columns()))
		n := 0
		for _, err := range rows.All() {
			is.NotError(t, err)
			for _, err := range rows.All() {
				t.Fatal("a row in a loop inside the loop", err)
			}
			n++
		}
		for _, err := range rows.All() {
			t.Fatal("a row in a second loop", err)
		}
		is.Equal(t, 2, n)
		is.Equal(t, 0, f.open)
	})
}

func TestEngine_syntaxError(t *testing.T) {
	t.Run("should give the syntax error without a transaction, whatever the database holds", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		f.readFails, f.beginFails = core.ErrNoLease, core.ErrNoLease
		e := newTestEngine(f, bugs{})
		_, err := e.Exec(t.Context(), "create table T1 (v1 int primary key) strict without rowid")
		is.Equal(t, `near "without": syntax error`, err.Error())
		_, err = e.Query(t.Context(), "create table t1 (")
		is.Equal(t, "incomplete input", err.Error())
		is.Equal(t, 0, len(f.calls))
	})
}
