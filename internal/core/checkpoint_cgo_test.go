//go:build cgo

// The test in this file has a goroutine wait in SQLite's busy handler while another goroutine
// commits. In the browser's binding, SQLite's busy handler holds the one thread, and the commit's
// spool file, which Go's file calls make through the JavaScript event loop, then waits for good, so
// it is not built there.

package core_test

import (
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

func TestDB_checkpoint_busy(t *testing.T) {
	t.Run("should not stop when another connection holds the lock of the checkpoint, and checkpoint at the next commit after that connection's checkpoint", func(t *testing.T) {
		w := newWorld(t)
		rec := &recorder{}
		options := syncing(core.Options{Logger: rec.logger()})
		options.CheckpointAfter(1)
		d := w.serve(t, "a", options)
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 1))
		path := filepath.Join(w.dir, "a.db")

		// A read of another connection fixes its state, and a commit after it leaves frames in the
		// log that a checkpoint cannot copy while the read is open.
		reader, err := sqlite.Open(path)
		is.NotError(t, err)
		defer func() { is.NotError(t, reader.Close()) }()
		is.NotError(t, reader.Exec("begin"))
		is.NotError(t, reader.Exec("select count(*) from t1"))
		transact(t, d, upsert("t1", 2, 2))

		// A full checkpoint of a third connection takes the lock of the checkpoint, and then waits in
		// its busy handler: for the lock of the writer, which the write transaction holds, and after
		// that for the read. It holds the lock of the checkpoint all along. The sleep that its VFS
		// counts shows that it holds it.
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		defer func() { is.NotError(t, vfs.Close()) }()
		other, err := sqlite.OpenVFS(path, vfs.Name())
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("pragma busy_timeout = 3600000"))
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		is.NotError(t, tx.Upsert(t.Context(), "t1", row(3, 3)))
		full := make(chan error, 1)
		go func() { full <- other.Exec("pragma wal_checkpoint(full)") }()
		ended := sync.OnceValue(func() error { return <-full })
		defer func() {
			// After a failure, the write transaction and the read end here, so that the full
			// checkpoint ends before its connection closes.
			_ = tx.Rollback(t.Context())
			_ = reader.Exec("rollback")
			_ = ended()
		}()
		// A full checkpoint that ends without waiting ends the test.
		for sleeps, _ := vfs.Slept(); sleeps == 0; sleeps, _ = vfs.Slept() {
			select {
			case err := <-full:
				full <- err
				t.Fatalf("the full checkpoint ended without waiting for a lock: %v", err)
			case <-time.After(time.Millisecond):
			}
		}

		is.NotError(t, tx.Commit(t.Context()))
		is.True(t, !isDone(d))
		checkpoints := rec.named(core.EventCheckpoint)
		last := checkpoints[len(checkpoints)-1]
		is.Equal(t, "busy", last[core.AttrOutcome])
		is.Equal[any](t, slog.LevelDebug, last["level"])
		_, hasLog := last[core.AttrCheckpointLog]
		is.True(t, !hasLog)

		// Once the read has ended, the full checkpoint copies the whole log, and the next commit's
		// checkpoint runs.
		is.NotError(t, reader.Exec("commit"))
		is.NotError(t, ended())
		transact(t, d, upsert("t1", 4, 4))
		checkpoints = rec.named(core.EventCheckpoint)
		is.Equal(t, "checkpointed", checkpoints[len(checkpoints)-1][core.AttrOutcome])
		is.True(t, !isDone(d))
		is.Equal(t, "t1(k*0,v): 1,1 | 2,2 | 3,3 | 4,4", read(t, d))
	})
}
