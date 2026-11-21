package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/host"
)

// newTestConn of a new engine on the fake f.
func newTestConn(f *fake, b bugs) *Conn {
	return newEngine(f, host.New(), Options{bugs: b}).Conn()
}

// newLoggedConn of a new engine on the fake f, whose events h records, each as its attributes.
func newLoggedConn(f *fake, b bugs, h *recorder) *Conn {
	return newEngine(f, host.New(), Options{bugs: b, Logger: slog.New(h)}).Conn()
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

// last event, without its duration, its SQL text, and the direction that the engine drew from its
// host, which [newLoggedConn] does not fix.
func (h *recorder) last() string {
	e := maps.Clone(h.events[len(h.events)-1])
	delete(e, attrDuration)
	delete(e, attrDirection)
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
	t.Run("should log one event for a syntax error through Query", func(t *testing.T) {
		h := &recorder{}
		_, err := newLoggedConn(newFake(), bugs{}, h).Query(t.Context(), "selec")
		is.Equal(t, `near "selec": syntax error`, err.Error())
		is.Equal(t, 1, len(h.events))
		is.Equal(t, "sqlite_error", h.events[0][attrOutcome])
	})

	t.Run("should log one event for a write through Query", func(t *testing.T) {
		h := &recorder{}
		f := newFake().with("t1", []string{"v1"})
		rows, err := newLoggedConn(f, bugs{}, h).Query(t.Context(), "drop table t1")
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
			rows, err := newLoggedConn(f, bugs{}, h).Query(t.Context(), "select * from t1")
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
			{name: "write, through Exec", src: "insert into t1 (v1) values (3)", panic: "the insert panics", set: func(f *fake) { f.insertPanics = true }},
			{name: "write, through Query", src: "insert into t1 (v1) values (3)", panic: "the insert panics", set: func(f *fake) { f.insertPanics = true }, query: true},
			{name: "scan before its first row, through Query", src: "select * from t1", panic: "the scan panics", set: func(f *fake) { f.scanPanics = 0 }, query: true},
			{name: "scan before its second row, through Query", src: "select * from t1", panic: "the scan panics", set: func(f *fake) { f.scanPanics = 1 }, query: true},
			{name: "scan, through Exec", src: "select * from t1", panic: "the scan panics", set: func(f *fake) { f.scanPanics = 1 }},
			{name: "consumer, through Query", src: "select * from t1", panic: "the consumer", set: func(*fake) {}, query: true, outcome: "ok"},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				h := &recorder{}
				f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
				c.set(f)
				conn := newLoggedConn(f, bugs{}, h)
				func() {
					defer func() { is.Equal(t, any(c.panic), recover()) }()
					if !c.query {
						_, _ = conn.Exec(t.Context(), c.src)
						return
					}
					rows, err := conn.Query(t.Context(), c.src)
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
		conn := newEngine(f, clock, Options{Logger: slog.New(h)}).Conn()
		_, err := conn.Exec(t.Context(), "drop table T1")
		is.NotError(t, err)
		last := h.events[len(h.events)-1]
		is.Equal(t, "1s", last[attrDuration])
		is.Equal(t, "drop_table", last[attrKind])
		is.Equal(t, "T1", last[attrTable])
		is.Equal(t, "drop table T1", last[attrSQL])

		f.beginFails = core.ErrBusy
		_, err = conn.Exec(t.Context(), "drop table t1")
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
		conn := newLoggedConn(f, bugs{}, h)

		_, _ = conn.Exec(t.Context(), "create table t1 (")
		is.Equal(t, "map[error:incomplete input msg:statement outcome:sqlite_error sqlite.code:1 sqlite.extended_code:1 statement.rows:0]", h.last())

		rows, err := conn.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
		}
		is.Equal(t, fmt.Sprintf("map[msg:statement outcome:ok statement.access:full_scan statement.kind:select statement.position:%v statement.rows:2 statement.rows_read:2 statement.table:t1]", empty), h.last())

		// The scan read one row before it failed.
		f.scanFails = 1
		_, err = conn.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, fmt.Sprintf("map[error:the scan fails msg:statement outcome:core_error statement.access:full_scan statement.kind:select statement.position:%v statement.rows:1 statement.rows_read:1 statement.table:t1]", empty), h.last())

		f.scanFails, f.endFails = 1, errors.New("the end fails")
		_, err = conn.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, "the scan fails", h.events[len(h.events)-1][attrError])

		f.scanFails = -1
		rows, err = conn.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		var last error
		for _, err := range rows.All() {
			last = err
		}
		is.Equal(t, "the end fails", last.Error())
		is.Equal(t, "the end fails", h.events[len(h.events)-1][attrError])

		_, err = conn.Exec(t.Context(), "select * from nosuch")
		is.Equal(t, "no such table: nosuch", err.Error())
		is.Equal(t, "no such table: nosuch", h.events[len(h.events)-1][attrError])
		f.endFails = nil

		f.annotateFails = errors.New("annotate fails")
		_, err = conn.Exec(t.Context(), "insert into t1 (v1) values (3)")
		is.Equal(t, "annotate fails", err.Error())
		is.Equal(t, fmt.Sprintf("map[commit.parent:%[1]v error:annotate fails msg:statement outcome:core_error statement.kind:insert statement.position:%[1]v statement.rows:0 statement.table:t1]", empty), h.last())
		f.annotateFails = nil

		f.readFails = core.ErrNoLease
		_, err = conn.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrNoLease))
		is.Equal(t, "map[error:no valid lease msg:statement outcome:refused statement.kind:select statement.rows:0 statement.table:t1]", h.last())
		f.readFails = nil

		f.commitFails = core.ErrOutcomeUnknown
		_, err = newLoggedConn(f, bugs{ackUnknown: true}, h).Exec(t.Context(), "insert into t1 (v1) values (4)")
		is.NotError(t, err)
		is.Equal(t, "1", h.events[len(h.events)-1][attrRows])
	})

	t.Run("should give the access, the index of an access by one, and the rows read of a select, an update, and a delete in the event, and neither an access nor rows read for a statement that reads no rows of a table and for one that failed before its access was chosen", func(t *testing.T) {
		// reading of the event e: its attributes of the reading that it has, its rows, and its outcome.
		reading := func(e map[string]string) string {
			var parts []string
			for _, key := range []string{attrAccess, attrIndex, attrRowsRead, attrRows, attrOutcome} {
				if v, ok := e[key]; ok {
					parts = append(parts, key+":"+v)
				}
			}
			return strings.Join(parts, " ")
		}
		// The table t has the primary key k and the index i on w, which two rows have 20 in. Neither
		// column takes NULL. The numbers do not depend on the direction that the engine draws for a
		// select, since each statement reads its rows to the end.
		newTable := func() *fake {
			return newFake().with("t", []string{"k", "w"}, []int64{1, 10}, []int64{2, 20}, []int64{3, 30}, []int64{4, 20}).index("i", "t", 1)
		}
		for _, c := range []struct{ src, want string }{
			// A select reads the rows of its access, whether or not the other conditions hold for them.
			{src: "select * from t", want: "statement.access:full_scan statement.rows_read:4 statement.rows:4 outcome:ok"},
			{src: "select * from t where w != 20", want: "statement.access:full_scan statement.rows_read:4 statement.rows:2 outcome:ok"},
			{src: "select * from t where k = 2", want: "statement.access:primary_key statement.rows_read:1 statement.rows:1 outcome:ok"},
			{src: "select * from t where k = 2 and w = 30", want: "statement.access:primary_key statement.rows_read:1 statement.rows:0 outcome:ok"},
			{src: "select * from t where k = 9", want: "statement.access:primary_key statement.rows_read:0 statement.rows:0 outcome:ok"},
			{src: "select * from t where k > 2", want: "statement.access:primary_key statement.rows_read:2 statement.rows:2 outcome:ok"},
			{src: "select w from t where w = 20", want: "statement.access:index statement.index:i statement.rows_read:2 statement.rows:2 outcome:ok"},
			{src: "select * from t where w = 20 and k != 2", want: "statement.access:index statement.index:i statement.rows_read:2 statement.rows:1 outcome:ok"},
			{src: "select * from t where k > 2 and w = 20", want: "statement.access:index statement.index:i statement.rows_read:1 statement.rows:1 outcome:ok"},
			{src: "select * from t where w > 10 and w < 30", want: "statement.access:index statement.index:i statement.rows_read:2 statement.rows:2 outcome:ok"},
			{src: "select * from t where k is null", want: "statement.access:none statement.rows_read:0 statement.rows:0 outcome:ok"},
			{src: "select * from t where w = null", want: "statement.access:none statement.rows_read:0 statement.rows:0 outcome:ok"},
			// An update reads the rows of its access, and then the row of each key that it changes.
			{src: "update t set w = 50", want: "statement.access:full_scan statement.rows_read:8 statement.rows:4 outcome:ok"},
			{src: "update t set w = 50 where k = 2", want: "statement.access:primary_key statement.rows_read:2 statement.rows:1 outcome:ok"},
			{src: "update t set w = 50 where k = 9", want: "statement.access:primary_key statement.rows_read:0 statement.rows:0 outcome:ok"},
			{src: "update t set w = 50 where k >= 2 and w != 30", want: "statement.access:primary_key statement.rows_read:5 statement.rows:2 outcome:ok"},
			{src: "update t set w = 50 where w = 20", want: "statement.access:index statement.index:i statement.rows_read:4 statement.rows:2 outcome:ok"},
			{src: "update t set w = 50 where k is null", want: "statement.access:none statement.rows_read:0 statement.rows:0 outcome:ok"},
			// An update that sets NULL in a column that does not take it reads one row in its check, which
			// fails it if there is one.
			{src: "update t set w = null where k = 2", want: "statement.access:primary_key statement.rows_read:1 statement.rows:0 outcome:sqlite_error"},
			{src: "update t set w = null where w = 20", want: "statement.access:index statement.index:i statement.rows_read:1 statement.rows:0 outcome:sqlite_error"},
			{src: "update t set w = null", want: "statement.access:full_scan statement.rows_read:1 statement.rows:0 outcome:sqlite_error"},
			{src: "update t set w = null where k = 9", want: "statement.access:primary_key statement.rows_read:0 statement.rows:0 outcome:ok"},
			// A value of another type fails the update after the choice of its access, before it reads.
			{src: "update t set w = 'a' where k = 2", want: "statement.access:primary_key statement.rows_read:0 statement.rows:0 outcome:not_supported"},
			// A delete reads the rows of its access.
			{src: "delete from t", want: "statement.access:full_scan statement.rows_read:4 statement.rows:4 outcome:ok"},
			{src: "delete from t where k = 2", want: "statement.access:primary_key statement.rows_read:1 statement.rows:1 outcome:ok"},
			{src: "delete from t where k < 3 and w != 10", want: "statement.access:primary_key statement.rows_read:2 statement.rows:1 outcome:ok"},
			{src: "delete from t where k < 3 and w = 10", want: "statement.access:index statement.index:i statement.rows_read:1 statement.rows:1 outcome:ok"},
			{src: "delete from t where w = 20", want: "statement.access:index statement.index:i statement.rows_read:2 statement.rows:2 outcome:ok"},
			{src: "delete from t where w != 20", want: "statement.access:full_scan statement.rows_read:4 statement.rows:2 outcome:ok"},
			{src: "delete from t where k = null", want: "statement.access:none statement.rows_read:0 statement.rows:0 outcome:ok"},
			// No rows of a table are read.
			{src: "insert into t values (5, 50)", want: "statement.rows:1 outcome:ok"},
			{src: "insert into t values (1, 50)", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "create table u (k int primary key) strict, without rowid", want: "statement.rows:0 outcome:ok"},
			{src: "drop table t", want: "statement.rows:0 outcome:ok"},
			// A create index and a drop index name their index, which is no access.
			{src: "create index j on t (w)", want: "statement.index:j statement.rows:0 outcome:ok"},
			{src: "drop index i", want: "statement.index:i statement.rows:0 outcome:ok"},
			// The statement failed before its access was chosen.
			{src: "select * from nosuch where k = 1", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "select x from t where k = 1", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "select * from t where x = 1", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "select * from t where k = 'a'", want: "statement.rows:0 outcome:not_supported"},
			{src: "select * from t where k = ?", want: "statement.rows:0 outcome:wrong_arguments"},
			{src: "update t set x = 1 where k = 1", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "update t set w = 1 where k = 'a'", want: "statement.rows:0 outcome:not_supported"},
			{src: "delete from nosuch", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "delete from t where x is null", want: "statement.rows:0 outcome:sqlite_error"},
			{src: "selec", want: "statement.rows:0 outcome:sqlite_error"},
		} {
			h := &recorder{}
			_, _ = newLoggedConn(newTable(), bugs{}, h).Exec(t.Context(), c.src)
			is.Equal(t, 1, len(h.events), c.src)
			is.Equal(t, c.want, reading(h.events[0]), c.src)
		}

		// A loop that stops at its first row has read that row only.
		h := &recorder{}
		rows, err := newLoggedConn(newTable(), bugs{}, h).Query(t.Context(), "select * from t where w = 20")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
			break
		}
		is.Equal(t, "statement.access:index statement.index:i statement.rows_read:1 statement.rows:1 outcome:ok", reading(h.events[0]))
	})

	t.Run("should give the direction that the engine drew for a query in the event, and none for a statement that drew none", func(t *testing.T) {
		newTable := func() *fake {
			return newFake().with("t", []string{"k", "w"}, []int64{1, 10}, []int64{2, 20}).index("i", "t", 1)
		}
		// query src on a connection of the engine of the host and the options, and give its event.
		query := func(h host.Host, o Options, src string) map[string]string {
			t.Helper()
			r := &recorder{}
			o.Logger = slog.New(r)
			rows, err := newEngine(newTable(), h, o).Conn().Query(t.Context(), src)
			is.NotError(t, err)
			for _, err := range rows.All() {
				is.NotError(t, err)
			}
			is.Equal(t, 1, len(r.events))
			return r.events[0]
		}

		// The host gives an even number, forward, and then an odd one, backward, for every access.
		h := &drawHost{Host: host.New(), draws: []uint64{2, 7}}
		is.Equal(t, "forward", query(h, Options{}, "select * from t")[attrDirection])
		is.Equal(t, "backward", query(h, Options{}, "select * from t where w = 20")[attrDirection])
		is.Equal(t, "forward", query(h, Options{}, "select * from t where k = 1")[attrDirection])
		is.Equal(t, "backward", query(h, Options{}, "select * from t where k is null")[attrDirection])

		// A query of an engine with a seed has the direction of the first draw of the seed.
		want := map[bool]string{false: "forward", true: "backward"}[newEngine(nil, h, Options{Directions: new(uint64(3))}).reading(true).backward]
		is.Equal(t, want, query(h, Options{Directions: new(uint64(3))}, "select * from t")[attrDirection])

		// A query whose path forces a direction draws none.
		_, ok := query(h, Options{path: path{direction: directionBackward}}, "select * from t")[attrDirection]
		is.True(t, !ok)

		// A select that Exec runs is a query too, and draws.
		r := &recorder{}
		_, err := newEngine(newTable(), h, Options{Logger: slog.New(r)}).Conn().Exec(t.Context(), "select * from t")
		is.NotError(t, err)
		is.Equal(t, "forward", r.events[0][attrDirection])

		// An update, a delete, and a statement that reads no rows draw none.
		for _, src := range []string{"update t set w = 30 where k = 1", "delete from t where k = 2", "insert into t values (3, 30)"} {
			r := &recorder{}
			_, _ = newLoggedConn(newTable(), bugs{}, r).Exec(t.Context(), src)
			is.Equal(t, 1, len(r.events), src)
			_, ok := r.events[0][attrDirection]
			is.True(t, !ok, src)
		}

		// In a write transaction and in a read-only one, a query draws, and an update and a delete
		// draw none, so the queries take the draws in turn.
		for _, readOnly := range []bool{false, true} {
			r := &recorder{}
			h := &drawHost{Host: host.New(), draws: []uint64{2, 7}}
			c := newEngine(newTable(), h, Options{Logger: slog.New(r)}).Conn()
			begin := map[bool]func(context.Context) error{false: c.Begin, true: c.BeginReadOnly}[readOnly]
			is.NotError(t, begin(t.Context()))
			var directions []string
			for _, src := range []string{"update t set w = 30 where k = 1", "select * from t where w = 20", "delete from t where k = 2", "select * from t"} {
				if strings.HasPrefix(src, "select") {
					rows, err := c.Query(t.Context(), src)
					is.NotError(t, err)
					for _, err := range rows.All() {
						is.NotError(t, err)
					}
				} else {
					_, _ = c.Exec(t.Context(), src)
				}
				directions = append(directions, r.events[len(r.events)-1][attrDirection])
				// Every statement of the transaction reads by an access, which its event names.
				is.True(t, r.events[len(r.events)-1][attrAccess] != "", src)
			}
			is.NotError(t, c.Rollback(t.Context()))
			is.Equal(t, "[ forward  backward]", fmt.Sprint(directions), fmt.Sprint("read-only: ", readOnly))
			is.Equal(t, 2, h.drawn)
		}
	})
}

// beginCounter is a fake that counts the write transactions that begin.
type beginCounter struct {
	*fake
	begins int
}

func (b *beginCounter) begin(ctx context.Context) (writeTx, error) {
	b.begins++
	return b.fake.begin(ctx)
}

// busyReader is a reader whose read transaction is open, so every read fails with
// [core.ErrReaderBusy].
type busyReader struct{}

func (busyReader) read(context.Context) (readTx, error) {
	return nil, fmt.Errorf("busy: %w", core.ErrReaderBusy)
}

func (busyReader) close(context.Context) error {
	return nil
}

func TestEngine_write(t *testing.T) {
	t.Run("should annotate and commit a statement that succeeds, and roll back one that fails, at each step", func(t *testing.T) {
		f := newFake()
		conn := newTestConn(f, bugs{})
		_, err := conn.Exec(t.Context(), "; "+t1+" ; -- x")
		is.NotError(t, err)
		is.Equal(t, "[table t1 create t1 annotate "+t1+" ; commit]", fmt.Sprint(f.calls))

		f.calls = nil
		_, err = conn.Exec(t.Context(), t1)
		is.Equal(t, "table t1 already exists", err.Error())
		is.Equal(t, "[table t1 rollback]", fmt.Sprint(f.calls))

		f.calls, f.insertFails = nil, errors.New("insert fails")
		_, err = conn.Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "insert fails", err.Error())
		is.Equal(t, "[table t1 insert t1 1 rollback]", fmt.Sprint(f.calls))

		f.calls, f.insertFails, f.annotateFails = nil, nil, errors.New("annotate fails")
		_, err = conn.Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "annotate fails", err.Error())
		is.Equal(t, "[table t1 insert t1 1 annotate insert into t1 (v1) values (1) rollback]", fmt.Sprint(f.calls))

		f.calls, f.annotateFails, f.commitFails = nil, nil, core.ErrOutcomeUnknown
		res, err := conn.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, errors.Is(err, core.ErrOutcomeUnknown))
		is.Equal(t, Result{SetsChanges: true}, res)
		is.Equal(t, "commit", f.calls[len(f.calls)-1])
		is.Equal(t, 0, f.open)
	})

	t.Run("should look up a statement of its own that does nothing on a reader of its own while the reader of the connection is busy, and never begin a write transaction", func(t *testing.T) {
		d := &beginCounter{fake: newFake().with("t1", []string{"v1"})}
		h := &recorder{}
		conn := newEngine(d, host.New(), Options{Logger: slog.New(h)}).Conn()
		// The reader of the connection is held, as by the rows of an earlier query.
		conn.rd = busyReader{}
		for _, src := range []string{"create table if not exists t1 (v1 int primary key) strict, without rowid", "drop table if exists nosuch"} {
			_, err := conn.Exec(t.Context(), src)
			is.NotError(t, err, src)
		}
		is.Equal(t, 0, d.begins, "the write transactions")
		is.Equal(t, 2, d.opened, "the readers opened")
		is.Equal(t, 0, d.readers, "the readers open")
		is.Equal(t, 0, d.open, "the transactions open")

		// A statement that has work to do still takes the write path.
		_, err := conn.Exec(t.Context(), "drop table if exists t1")
		is.NotError(t, err)
		is.Equal(t, 1, d.begins, "the write transactions")

		// Where the read cannot begin for another reason, the write transaction looks, finds nothing to
		// do, and rolls back without an annotation or a commit.
		d.readFails, d.calls = errors.New("the read fails"), nil
		_, err = conn.Exec(t.Context(), "drop table if exists nosuch")
		is.NotError(t, err)
		is.Equal(t, 2, d.begins, "the write transactions")
		is.Equal(t, "[table nosuch rollback]", fmt.Sprint(d.calls))
		// It wrote nothing, so its event has the position of the write transaction and no parent.
		is.Equal(t, fmt.Sprintf("map[msg:statement outcome:ok statement.kind:drop_table statement.nothing:true statement.position:%v statement.rows:0 statement.table:nosuch]", format.EmptyLog), h.last())

		// And where the reader of its own cannot be opened.
		d.readFails, d.readerFails, d.calls = nil, errors.New("the reader cannot be opened"), nil
		_, err = conn.Exec(t.Context(), "drop table if exists nosuch")
		is.NotError(t, err)
		is.Equal(t, 3, d.begins, "the write transactions")
		is.Equal(t, "[table nosuch rollback]", fmt.Sprint(d.calls))
	})

	t.Run("should roll back when an operator panics", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		f.insertPanics = true
		conn := newTestConn(f, bugs{})
		func() {
			defer func() { is.Equal(t, "the insert panics", recover()) }()
			_, _ = conn.Exec(t.Context(), "insert into t1 (v1) values (1)")
		}()
		is.Equal(t, "rollback", f.calls[len(f.calls)-1])
		is.Equal(t, 0, f.open)
	})

	t.Run("should pass on the error of a begin", func(t *testing.T) {
		f := newFake()
		f.beginFails = core.ErrNoLease
		_, err := newTestConn(f, bugs{}).Exec(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrNoLease))
		_, err = newTestConn(f, bugs{}).Query(t.Context(), t1)
		is.True(t, errors.Is(err, core.ErrNoLease))
	})

	t.Run("should acknowledge a commit of unknown outcome, and leave a failed transaction open, with the planted bugs", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		f.commitFails = core.ErrOutcomeUnknown
		res, err := newTestConn(f, bugs{ackUnknown: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, Result{Changes: 1, SetsChanges: true}, res)
		f.commitFails = core.ErrNoLease
		_, err = newTestConn(f, bugs{ackUnknown: true}).Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, errors.Is(err, core.ErrNoLease))
		f.commitFails = nil

		f = newFake()
		_, err = newTestConn(f, bugs{keepOpen: true}).Exec(t.Context(), "drop table t")
		is.True(t, err != nil)
		is.Equal(t, 1, f.open)

		f = newFake().with("t1", []string{"v1"})
		rows, err := newTestConn(f, bugs{keepRead: true}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
		is.Equal(t, 1, f.open)
		rows, err = newTestConn(f, bugs{keepRead: true}).Query(t.Context(), "drop table t1")
		is.NotError(t, err)
		is.NotError(t, rows.Close())
	})

	t.Run("should check an insert in a read transaction of its own, and write without the check, with the planted bug", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		_, err := newTestConn(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		is.Equal(t, "[table t1 get t1 1 end table t1 upsert t1 1 annotate insert into t1 (v1) values (1) commit]", fmt.Sprint(f.calls))
		_, err = newTestConn(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.Equal(t, "UNIQUE constraint failed: t1.v1", err.Error())
		_, err = newTestConn(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t2 (v1) values (1)")
		is.Equal(t, "no such table: t2", err.Error())
		f.readFails = core.ErrNoLease
		_, err = newTestConn(f, bugs{checkOutside: true}).Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.True(t, errors.Is(err, core.ErrNoLease))
		is.Equal(t, 0, f.open)
		// Only an insert is checked so.
		_, err = newTestConn(f, bugs{checkOutside: true}).Exec(t.Context(), "drop table t1")
		is.NotError(t, err)
	})
}

func TestEngine_query(t *testing.T) {
	t.Run("should end the read transaction when building the query panics", func(t *testing.T) {
		f := newFake()
		f.tablePanics = true
		conn := newTestConn(f, bugs{})
		for _, src := range []string{"select * from t1", "select * from T1"} {
			func() {
				defer func() { is.Equal(t, "the lookup panics", recover()) }()
				_, _ = conn.Query(t.Context(), src)
			}()
			is.Equal(t, 0, f.open)
		}
	})

	t.Run("should give a failure of the end of the read transaction from Close after an early stop, once", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		f.endFails = errors.New("the end fails")
		rows, err := newTestConn(f, bugs{}).Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
			break
		}
		is.Equal(t, "the end fails", rows.Close().Error())
		is.NotError(t, rows.Close())

		rows, err = newTestConn(f, bugs{}).Query(t.Context(), "select * from t1")
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
		conn := newTestConn(f, bugs{})
		_, err := conn.Query(t.Context(), "select * from t1")
		is.Equal(t, "no such table: t1", err.Error())
		is.Equal(t, "[table t1 end]", fmt.Sprint(f.calls))
		f.readFails = core.ErrStopped
		_, err = conn.Query(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		_, err = conn.Exec(t.Context(), "select * from t1")
		is.True(t, errors.Is(err, core.ErrStopped))
		is.Equal(t, 0, f.open)
	})

	t.Run("should end the read transaction once, after an error in the middle", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		f.scanFails = 1
		conn := newTestConn(f, bugs{})
		rows, err := conn.Query(t.Context(), "select * from t1")
		is.NotError(t, err)
		var last error
		for _, err := range rows.All() {
			last = err
		}
		is.Equal(t, "the scan fails", last.Error())
		is.NotError(t, rows.Close())
		is.Equal(t, 0, f.open)
		_, err = conn.Exec(t.Context(), "select * from t1")
		is.Equal(t, "the scan fails", err.Error())
		is.Equal(t, 0, f.open)
	})

	t.Run("should keep the read transaction when Close is called during the loop, until the loop ends", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1}, []int64{2})
		rows, err := newTestConn(f, bugs{}).Query(t.Context(), "select * from t1")
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
		rows, err := newTestConn(f, bugs{}).Query(t.Context(), "select * from t1")
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
		conn := newTestConn(f, bugs{})
		_, err := conn.Exec(t.Context(), "create table T1 (v1 int primary key) strict without rowid")
		is.Equal(t, `near "without": syntax error`, err.Error())
		_, err = conn.Query(t.Context(), "create table t1 (")
		is.Equal(t, "incomplete input", err.Error())
		is.Equal(t, 0, len(f.calls))
	})
}

func TestConn(t *testing.T) {
	const query = "select * from t1"
	// read the rows of the query on c to their end.
	read := func(t *testing.T, c *Conn) {
		t.Helper()
		rows, err := c.Query(t.Context(), query)
		is.NotError(t, err)
		for _, err := range rows.All() {
			is.NotError(t, err)
		}
	}

	t.Run("should open no reader for a connection that only writes, one at its first read, and keep that one until it is closed", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"})
		c := newTestConn(f, bugs{})
		_, err := c.Exec(t.Context(), "insert into t1 (v1) values (1)")
		is.NotError(t, err)
		_, err = c.Exec(t.Context(), "selec")
		is.True(t, err != nil)
		is.Equal(t, 0, f.opened)

		read(t, c)
		_, err = c.Exec(t.Context(), query)
		is.NotError(t, err)
		rows, err := c.Query(t.Context(), query)
		is.NotError(t, err)
		is.NotError(t, rows.Close())
		is.Equal(t, 1, f.opened)
		is.Equal(t, 1, f.readers)

		is.NotError(t, c.Close(t.Context()))
		is.Equal(t, 0, f.readers)
		is.Equal(t, 0, f.open)
		is.NotError(t, c.Close(t.Context()))
		is.Equal(t, 0, f.readers)
	})

	t.Run("should give each connection of an engine a reader of its own", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1})
		e := newEngine(f, host.New(), Options{})
		first, second := e.Conn(), e.Conn()
		read(t, first)
		read(t, second)
		read(t, first)
		is.Equal(t, 2, f.opened)
		is.NotError(t, first.Close(t.Context()))
		is.Equal(t, 1, f.readers)
		read(t, second)
		is.NotError(t, second.Close(t.Context()))
		is.Equal(t, 0, f.readers)
	})

	t.Run("should fail the statement whose reader cannot be opened, with one event, and open a reader for the next statement that reads", func(t *testing.T) {
		h := &recorder{}
		f := newFake().with("t1", []string{"v1"}, []int64{1})
		c := newLoggedConn(f, bugs{}, h)
		f.readerFails = core.ErrBusy
		_, err := c.Query(t.Context(), query)
		is.True(t, errors.Is(err, core.ErrBusy))
		_, err = c.Exec(t.Context(), query)
		is.True(t, errors.Is(err, core.ErrBusy))
		is.Equal(t, 2, len(h.events))
		is.Equal(t, "core_error", h.events[1][attrOutcome])
		is.Equal(t, 0, f.opened)
		is.Equal(t, 0, f.open)

		// A write needs no reader.
		_, err = c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.NotError(t, err)

		f.readerFails = nil
		read(t, c)
		is.Equal(t, 1, f.opened)
		is.NotError(t, c.Close(t.Context()))
	})

	t.Run("should stay open when its reader is in use, and be closed with the error of any other close of its reader", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1})
		c := newTestConn(f, bugs{})
		read(t, c)
		f.closeFails = fmt.Errorf("close: %w", core.ErrReaderInUse)
		is.True(t, errors.Is(c.Close(t.Context()), core.ErrReaderInUse))
		is.Equal(t, 1, f.readers)
		read(t, c)
		is.Equal(t, 1, f.opened)

		f.closeFails = errors.New("the close fails")
		is.Equal(t, "the close fails", c.Close(t.Context()).Error())
		is.Equal(t, 0, f.readers)
		is.NotError(t, c.Close(t.Context()))
	})

	t.Run("should refuse a statement on a connection that is closed with an internal error, and open no reader for it", func(t *testing.T) {
		f := newFake().with("t1", []string{"v1"}, []int64{1})
		c := newTestConn(f, bugs{})
		read(t, c)
		is.NotError(t, c.Close(t.Context()))
		_, err := c.Query(t.Context(), query)
		is.True(t, errors.Is(err, ErrInternal))
		_, err = c.Exec(t.Context(), query)
		is.True(t, errors.Is(err, ErrInternal))
		_, err = c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, errors.Is(err, ErrInternal))
		is.Equal(t, 1, f.opened)
		is.Equal(t, 0, f.open)
	})
}
