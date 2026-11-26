package core

import (
	"context"
	"fmt"
	"log/slog"

	"maragu.dev/s4ql/internal/sqlite"
)

// checkpointAfter is the default of [Options.checkpointAfter]: the number of pages in the
// write-ahead log from which the writer checkpoints after a commit to the cache. It is SQLite's
// default for its automatic checkpoint, SQLITE_DEFAULT_WAL_AUTOCHECKPOINT, which every connection of
// the core turns off: see [openConn].
const checkpointAfter = 1000

// walState is what SQLite's pragma wal_checkpoint reports: whether the checkpoint could not start,
// because another connection checkpoints, the number of frames in the write-ahead log, and how many
// of them are in the file of the cache.
type walState struct {
	busy              bool
	log, checkpointed int
}

// walCheckpoint runs pragma wal_checkpoint in the mode, "noop" or "passive", on the schema main.
// SQLite reports a checkpoint that could not start because another connection checkpoints as busy
// in its row, and fails the statement for any other failure.
func (c *conn) walCheckpoint(mode string) (walState, error) {
	var st walState
	err := c.query("pragma main.wal_checkpoint("+mode+")", func(s *sqlite.Stmt) error {
		st = walState{busy: s.ColumnInt64(0) != 0, log: int(s.ColumnInt64(1)), checkpointed: int(s.ColumnInt64(2))}
		return nil
	})
	return st, err
}

// checkpoint the cache, after a commit to it by the writer, if its write-ahead log holds
// [Options.checkpointAfter] pages or more. It requires the writer lock.
//
// The checkpoint is passive: it copies into the file of the cache every page of the log that no read
// transaction may still need, and it never waits for a reader or the busy timeout, so a commit is
// never held up by a read. SQLite's automatic checkpoint is the same: passive, after each commit
// from the same number of pages on. A checkpoint that read transactions hold back copies what it
// can, and the next one goes on from there; once every page is in the file, the next write
// transaction starts the log from its beginning again. A checkpoint that cannot start because
// another connection checkpoints is no failure either.
//
// Any error of the checkpoint stops the instance for good with a local I/O failure, which it
// returns: a failed sync leaves the cache in a state that cannot be trusted until it is opened
// again. That is why the core checkpoints itself: SQLite's automatic checkpoint drops its error.
// "Busy" in place of an error is the one exception, as for every other call of the cache.
func (d *DB) checkpoint(ctx context.Context) error {
	before, err := d.w.walCheckpoint("noop")
	if err == nil && before.log < d.options.checkpointAfter {
		return nil
	}
	clock := d.stopwatch()
	after := before
	if err == nil {
		after, err = d.w.walCheckpoint("passive")
	}
	if err != nil && d.bugs.ignoreCheckpointError {
		// The planted bug: the error is dropped, as SQLite's automatic checkpoint drops it.
		return nil
	}

	outcome, level := "checkpointed", slog.LevelDebug
	switch {
	case err != nil && classify(err) == kindBusy:
		outcome, err = "busy", nil
	case err != nil:
		outcome, level = "failed", slog.LevelWarn
	case after.busy:
		outcome = "busy"
	case after.checkpointed < after.log:
		outcome = "held_back"
	}
	if d.enabled(ctx, level) {
		attrs := []slog.Attr{slog.String(attrOutcome, outcome), clock.total(), errAttr(err)}
		if err == nil {
			attrs = append(attrs, slog.Int(attrCheckpointLog, after.log), slog.Int(attrCheckpointMoved, max(0, after.checkpointed-before.checkpointed)))
		}
		d.event(ctx, level, eventCheckpoint, attrs...)
	}
	if err != nil {
		return d.stop(ctx, fmt.Errorf("%w: a checkpoint of the cache: %w", ErrLocal, err))
	}
	return nil
}
