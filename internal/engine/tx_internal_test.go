package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/host"
)

// spanning is a fake whose write transactions record the calls of a transaction that spans
// statements, and fail them on demand.
type spanning struct {
	*fake
	// servingFails, savepointFails, and releaseFails are the errors of those calls.
	servingFails, savepointFails, releaseFails error
}

func (s *spanning) begin(ctx context.Context) (writeTx, error) {
	tx, err := s.fake.begin(ctx)
	if err != nil {
		return nil, err
	}
	return spanningTx{fakeTx: tx.(fakeTx), s: s}, nil
}

type spanningTx struct {
	fakeTx
	s *spanning
}

func (x spanningTx) serving(context.Context) error {
	x.f.calls = append(x.f.calls, "serving")
	return x.s.servingFails
}

func (x spanningTx) savepoint(context.Context) error {
	x.f.calls = append(x.f.calls, "savepoint")
	return x.s.savepointFails
}

func (x spanningTx) release(context.Context) error {
	x.f.calls = append(x.f.calls, "release")
	return x.s.releaseFails
}

func (x spanningTx) rollbackTo(context.Context) error {
	x.f.calls = append(x.f.calls, "rollback to")
	return nil
}

func TestConn_writeIn(t *testing.T) {
	newConn := func(s *spanning, h *recorder) *Conn {
		return newEngine(s, host.New(), Options{Logger: slog.New(h)}).Conn()
	}

	t.Run("should run each write of a transaction between a savepoint and its release, and roll back to the savepoint when it fails or panics", func(t *testing.T) {
		s := &spanning{fake: newFake().with("t1", []string{"v1"}, []int64{1})}
		c := newConn(s, &recorder{})
		is.NotError(t, c.Begin(t.Context()))
		_, err := c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.NotError(t, err)
		_, err = c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.True(t, err != nil)
		s.annotateFails = errors.New("the annotation fails")
		_, err = c.Exec(t.Context(), "insert into t1 (v1) values (3)")
		is.Error(t, s.annotateFails, err)
		s.annotateFails = nil
		s.upsertPanics = true
		func() {
			defer func() { is.True(t, recover() != nil, "the insert did not panic") }()
			_, _ = c.Exec(t.Context(), "insert into t1 (v1) values (4)")
		}()
		s.upsertPanics = false
		_, err = c.Exec(t.Context(), "drop table t1")
		is.NotError(t, err)
		is.NotError(t, c.Commit(t.Context()))
		is.Equal(t, strings.Join([]string{
			"serving", "savepoint", "table t1", "get t1 2", "upsert t1 2", "annotate insert into t1 (v1) values (2)", "release",
			"serving", "savepoint", "table t1", "get t1 2", "rollback to",
			"serving", "savepoint", "table t1", "get t1 3", "upsert t1 3", "annotate insert into t1 (v1) values (3)", "rollback to",
			"serving", "savepoint", "table t1", "get t1 4", "rollback to",
			"serving", "savepoint", "table t1", "drop t1", "annotate drop table t1", "release",
			"commit",
		}, "\n"), strings.Join(s.calls, "\n"))
	})

	t.Run("should refuse a statement before its savepoint when the instance does not serve, and keep the transaction open", func(t *testing.T) {
		s := &spanning{fake: newFake().with("t1", []string{"v1"})}
		c := newConn(s, &recorder{})
		is.NotError(t, c.Begin(t.Context()))
		s.servingFails = fmt.Errorf("x: %w", core.ErrNoLease)
		_, err := c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.Error(t, core.ErrNoLease, err)
		_, err = c.Query(t.Context(), "select * from t1")
		is.Error(t, core.ErrNoLease, err)
		s.servingFails = nil
		s.savepointFails = fmt.Errorf("x: %w", core.ErrStopped)
		_, err = c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.Error(t, core.ErrStopped, err)
		is.NotError(t, c.Rollback(t.Context()))
		is.Equal(t, "serving serving serving savepoint rollback", strings.Join(s.calls, " "))
	})

	t.Run("should fail a statement whose release fails, without a rollback to the savepoint, and not count it as a write", func(t *testing.T) {
		s := &spanning{fake: newFake().with("t1", []string{"v1"})}
		h := &recorder{}
		c := newConn(s, h)
		is.NotError(t, c.Begin(t.Context()))
		s.releaseFails = errors.New("the release fails")
		_, err := c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.Error(t, s.releaseFails, err)
		is.NotError(t, c.Rollback(t.Context()))
		is.Equal(t, "serving savepoint table t1 get t1 2 upsert t1 2 annotate insert into t1 (v1) values (2) release rollback", strings.Join(s.calls, " "))
		e := h.events[len(h.events)-1]
		is.Equal(t, eventTransaction, e["msg"])
		is.Equal(t, "0", e[attrTxWrites])
	})

	t.Run("should end the transaction with a commit of unknown outcome, and log it at Warn", func(t *testing.T) {
		s := &spanning{fake: newFake().with("t1", []string{"v1"})}
		h := &recorder{}
		c := newConn(s, h)
		is.NotError(t, c.Begin(t.Context()))
		_, err := c.Exec(t.Context(), "insert into t1 (v1) values (2)")
		is.NotError(t, err)
		s.commitFails = fmt.Errorf("commit: %w: x", core.ErrOutcomeUnknown)
		is.Error(t, core.ErrOutcomeUnknown, c.Commit(t.Context()))
		is.Error(t, ErrNoTx, c.Commit(t.Context()))
		is.Equal(t, 0, s.open)
		e := h.events[len(h.events)-1]
		is.Equal(t, eventTransaction, e["msg"])
		is.Equal(t, "core_error", e[attrOutcome])
	})
}
