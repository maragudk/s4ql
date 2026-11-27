package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/host"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// clocked records events, each with the reading of the clock of a [ticking] host when it is logged.
type clocked struct {
	recorder
	clock *ticking
	at    []time.Duration
}

func (h *clocked) Enabled(context.Context, slog.Level) bool { return true }
func (h *clocked) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *clocked) WithGroup(string) slog.Handler            { return h }

func (h *clocked) Handle(ctx context.Context, r slog.Record) error {
	h.at = append(h.at, h.clock.now)
	return h.recorder.Handle(ctx, r)
}

// duration of the event i of h.
func (h *clocked) duration(t *testing.T, i int) time.Duration {
	t.Helper()
	d, err := time.ParseDuration(h.events[i][attrDuration])
	is.NotError(t, err)
	return d
}

func TestConn_ExecAll_panic(t *testing.T) {
	t.Run("should log the event of a statement of a text that panics in the engine with the outcome internal_error, after those of the statements before it", func(t *testing.T) {
		for _, src := range []string{"insert into t1 (v1) values (3); select * from t1", "select * from t1; insert into t1 (v1) values (3); select * from t1"} {
			h := &recorder{}
			f := newFake().with("t1", []string{"v1"}, []int64{1})
			f.insertPanics = true
			c := newLoggedConn(f, bugs{}, h)
			func() {
				defer func() { is.Equal(t, any("the insert panics"), recover(), src) }()
				_, _ = c.ExecAll(t.Context(), src)
			}()
			is.Equal(t, 0, f.open, src)
			var outcomes []string
			for _, e := range h.events {
				outcomes = append(outcomes, e[attrKind]+" "+e[attrOutcome])
			}
			want := []string{"insert internal_error"}
			if src[0] == 's' {
				want = []string{"select ok", "insert internal_error"}
			}
			is.EqualSlice(t, want, outcomes, src)
			last := h.events[len(h.events)-1]
			is.Equal(t, "s4ql: internal error: the statement panicked", last[attrError], src)
		}
	})
}

func TestConn_ExecAll_duration(t *testing.T) {
	// The clock of the host moves by a second at each reading, so the first reading of a call is a
	// second after the clock before it, and the event of a statement whose duration runs from that
	// reading to the one that ends it covers the whole call up to its end: the split and the check of
	// the text too, without a dependence on real time.
	newClocked := func() (*Conn, *clocked) {
		clock := &ticking{Host: host.New(), now: time.Hour}
		h := &clocked{clock: clock}
		f := newFake().with("t1", []string{"v1"}).with("t2", []string{"v1"})
		return newEngine(f, clock, Options{Logger: slog.New(h)}).Conn(), h
	}

	t.Run("should give the event of a text of one statement the duration from before its check, as Exec gives it", func(t *testing.T) {
		for _, src := range []string{"drop table t1", "selec", "drop table t9", "drop table t1; ", "drop table t1 -- c"} {
			for _, all := range []bool{true, false} {
				c, h := newClocked()
				before := h.clock.now
				if all {
					_, _ = c.ExecAll(t.Context(), src)
				} else {
					_, _ = c.Exec(t.Context(), src)
				}
				is.Equal(t, 1, len(h.events), src)
				is.Equal(t, h.at[0]-(before+time.Second), h.duration(t, 0), src)
			}
		}
	})

	t.Run("should give the event of a text of one statement with the wrong number of arguments the duration from before its check", func(t *testing.T) {
		c, h := newClocked()
		before := h.clock.now
		_, err := c.ExecAll(t.Context(), "drop table t1", s4qlpb.Value_builder{Integer: new(int64(1))}.Build())
		is.True(t, err != nil)
		is.Equal(t, 1, len(h.events))
		is.Equal(t, h.at[0]-(before+time.Second), h.duration(t, 0))
	})

	t.Run("should give the event of the first of several statements the duration from before the split, and each later one its own", func(t *testing.T) {
		c, h := newClocked()
		before := h.clock.now
		_, err := c.ExecAll(t.Context(), "drop table t1; drop table t2; selec")
		is.True(t, err != nil)
		is.Equal(t, 3, len(h.events))
		is.Equal(t, h.at[0]-(before+time.Second), h.duration(t, 0))
		is.True(t, h.duration(t, 1) < h.at[1]-h.at[0], "the second statement starts after the first ends")
		is.True(t, h.duration(t, 2) < h.at[2]-h.at[1], "the refusal of the third starts after the second ends")

		for _, src := range []string{"drop table t9; drop table t1", "drop table t1; drop table t2; ", "drop table t1; drop table t2; \x00"} {
			c, h := newClocked()
			before := h.clock.now
			_, err := c.ExecAll(t.Context(), src)
			is.True(t, err != nil, src)
			is.Equal(t, 1, len(h.events), src)
			is.Equal(t, h.at[0]-(before+time.Second), h.duration(t, 0), src)
		}

		c, h = newClocked()
		before = h.clock.now
		_, err = c.ExecAll(t.Context(), "drop table t1; drop table t2", s4qlpb.Value_builder{Integer: new(int64(1))}.Build())
		is.True(t, err != nil)
		is.Equal(t, 1, len(h.events))
		is.Equal(t, h.at[0]-(before+time.Second), h.duration(t, 0))
	})
}
