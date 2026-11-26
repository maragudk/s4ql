package core_test

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
	"maragu.dev/s4ql/internal/store/simstore"
)

// walOf the cache at path, as another connection sees it: the number of frames in its
// write-ahead log, and how many of them are in the file of the cache.
func walOf(t *testing.T, path string) (log, checkpointed int64) {
	t.Helper()
	c, err := sqlite.Open(path)
	is.NotError(t, err)
	defer func() { is.NotError(t, c.Close()) }()
	s, err := c.Prepare("pragma main.wal_checkpoint(noop)")
	is.NotError(t, err)
	defer func() { is.NotError(t, s.Finalize()) }()
	ok, err := s.Step()
	is.NotError(t, err)
	is.True(t, ok)
	return s.ColumnInt64(1), s.ColumnInt64(2)
}

// rows that a commit upserts into the table t1 of two columns, with keys from first on: enough
// for a few pages of the cache.
func rows(first, n int) []step {
	var steps []step
	for i := range n {
		steps = append(steps, upsert("t1", first+i, first+i))
	}
	return steps
}

// checkpointFaults opens a VFS of faultvfs, which a cleanup registered before those of the
// instances closes after them, and options that open the cache through it, with syncs on, as an
// instance has them by default, and a checkpoint after pages of the write-ahead log.
func checkpointFaults(t *testing.T, pages int) (*faultvfs.VFS, core.Options) {
	t.Helper()
	vfs, err := faultvfs.New()
	is.NotError(t, err)
	t.Cleanup(func() { is.NotError(t, vfs.Close()) })
	var options core.Options
	options.VFS = vfs.Name()
	options.CheckpointAfter(pages)
	return vfs, options
}

// keyed is what [read] shows of the table t1 of two columns with the rows that [rows] upserts with
// the keys from 0 to n-1.
func keyed(n int) string {
	var keys []string
	for i := range n {
		keys = append(keys, fmt.Sprint(i, ",", i))
	}
	return "t1(k*0,v): " + strings.Join(keys, " | ")
}

func TestDB_checkpoint(t *testing.T) {
	t.Run("should turn SQLite's automatic checkpoint off on the write connection and on every reader", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		newReader(t, d)
		newReader(t, d)
		settings, err := d.AutoCheckpoint()
		is.NotError(t, err)
		is.EqualSlice(t, []int64{0, 0, 0}, settings)
	})

	t.Run("should checkpoint after a commit once the write-ahead log holds enough pages, and start the log again at the next commit", func(t *testing.T) {
		w := newWorld(t)
		rec := &recorder{}
		options := core.Options{Logger: rec.logger()}
		options.CheckpointAfter(8)
		d := w.serve(t, "a", options)
		transact(t, d, create("t1", 1, "k", "v"))
		path := filepath.Join(w.dir, "a.db")

		// Each commit adds a few pages, and no checkpoint runs until the log holds eight.
		var log int64
		for i := 0; len(rec.named(core.EventCheckpoint)) == 0; i++ {
			if i == 100 {
				t.Fatal("no checkpoint after 100 commits")
			}
			before, _ := walOf(t, path)
			transact(t, d, rows(i*500, 500)...)
			log, _ = walOf(t, path)
			if len(rec.named(core.EventCheckpoint)) == 0 {
				is.True(t, log < 8, fmt.Sprint("a log of ", log, " frames, and no checkpoint"))
			} else {
				is.True(t, before < 8, fmt.Sprint("a log of ", before, " frames before the commit, and no checkpoint after the commit before it"))
			}
		}
		events := rec.named(core.EventCheckpoint)
		is.Equal(t, 1, len(events))
		event := events[0]
		is.Equal(t, "checkpointed", event[core.AttrOutcome])
		is.Equal[any](t, slog.LevelDebug, event["level"])
		is.Equal[any](t, log, event[core.AttrCheckpointLog])
		is.True(t, log >= 8, fmt.Sprint(log))
		// Every frame that was in the log is now in the file of the cache, and none was there before.
		is.Equal[any](t, log, event[core.AttrCheckpointMoved])
		after, checkpointed := walOf(t, path)
		is.Equal(t, log, after)
		is.Equal(t, log, checkpointed)

		// The next write transaction starts the log from its beginning.
		transact(t, d, upsert("t1", -1, -1))
		after, checkpointed = walOf(t, path)
		is.True(t, after < log, fmt.Sprint(after, " frames after the restart, ", log, " before"))
		is.Equal(t, int64(0), checkpointed)
		is.True(t, !isDone(d))
	})

	for _, file := range []struct {
		name  string
		files faultvfs.File
	}{{"write-ahead log", faultvfs.WAL}, {"file of the cache", faultvfs.MainDB}} {
		t.Run("should stop for good with a local failure when the checkpoint's sync of the "+file.name+" fails, acknowledge the commit before it, and find that commit in the cache at the next start", func(t *testing.T) {
			w := newWorld(t)
			vfs, options := checkpointFaults(t, 8)
			rec := &recorder{}
			options.Logger = rec.logger()
			d := w.serve(t, "a", options)
			transact(t, d, create("t1", 1, "k", "v"))

			// With synchronous = full, SQLite syncs the log at every commit and at a checkpoint that
			// copies frames, and the file only at a checkpoint that copies the whole log. The fault of
			// the log is armed again before each commit, so that the sync of the commit succeeds and the
			// next one, which a checkpoint after that commit makes, fails. The log has not started again
			// since the cache was made, so no commit syncs its header.
			if file.files == faultvfs.MainDB {
				vfs.Fail(faultvfs.Fault{Ops: faultvfs.Sync, Files: file.files, Times: 1})
			}
			commits := 0
			for !isDone(d) {
				if commits == 100 {
					t.Fatal("no stop after 100 commits")
				}
				if file.files == faultvfs.WAL {
					vfs.Fail(faultvfs.Fault{Ops: faultvfs.Sync, Files: file.files, After: 1, Times: 1})
				}
				// Every commit returns nil, also the one whose checkpoint fails: it is in the log and in
				// the cache.
				transact(t, d, rows(commits*500, 500)...)
				commits++
			}
			is.Equal(t, 1, vfs.Failures())
			err := d.Err()
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrLocal, err)
			is.True(t, strings.Contains(err.Error(), "a checkpoint of the cache"), err.Error())
			se, ok := errors.AsType[*sqlite.Error](err)
			is.True(t, ok && se.Code == 1034, "not SQLITE_IOERR_FSYNC: "+err.Error())

			checkpoints := rec.named(core.EventCheckpoint)
			last := checkpoints[len(checkpoints)-1]
			is.Equal(t, "failed", last[core.AttrOutcome])
			is.Equal[any](t, slog.LevelWarn, last["level"])
			committed := rec.named(core.EventCommit)
			is.Equal(t, "committed_and_stopped", committed[len(committed)-1][core.AttrOutcome])
			stops := rec.named(core.EventStop)
			is.Equal(t, 1, len(stops))
			is.Equal(t, "local_failure", stops[0][core.AttrOutcome])

			// The commits before the stop are acknowledged, so the log has them all. The cache records
			// the last of them, so the next start has nothing to catch up, and serves them.
			want := keyed(commits * 500)
			vfs.Fail(faultvfs.Fault{})
			is.NotError(t, d.Close(t.Context()))
			restart := &recorder{}
			again := w.serve(t, "a", core.Options{Logger: restart.logger()})
			is.Equal(t, want, read(t, again))
			is.Equal(t, 0, len(restart.named(core.EventCatchUp)))
			// An instance with an empty cache replays the log to the same rows.
			is.NotError(t, again.Close(t.Context()))
			is.Equal(t, want, read(t, w.serve(t, "b", core.Options{})))
		})
	}

	t.Run("should not stop when a commit from the loop of a scan cannot checkpoint because the scan's statement is open, and checkpoint at the next commit", func(t *testing.T) {
		w := newWorld(t)
		rec := &recorder{}
		options := core.Options{Logger: rec.logger()}
		options.CheckpointAfter(1)
		d := w.serve(t, "a", options)
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 1))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		for _, err := range tx.Scan(t.Context(), "t1") {
			is.NotError(t, err)
			is.NotError(t, tx.CreateTable(t.Context(), tableOf("t2", 1, "k")))
			is.NotError(t, tx.Commit(t.Context()))
			break
		}
		is.True(t, !isDone(d))
		checkpoints := rec.named(core.EventCheckpoint)
		is.Equal(t, "busy", checkpoints[len(checkpoints)-1][core.AttrOutcome])
		_, hasLog := checkpoints[len(checkpoints)-1][core.AttrCheckpointLog]
		is.True(t, !hasLog)

		transact(t, d, upsert("t1", 2, 2))
		checkpoints = rec.named(core.EventCheckpoint)
		is.Equal(t, "checkpointed", checkpoints[len(checkpoints)-1][core.AttrOutcome])
		is.Equal(t, "t1(k*0,v): 1,1 | 2,2\nt2(k*0): ", read(t, d))
	})

	t.Run("should checkpoint at 1000 frames by default", func(t *testing.T) {
		w := newWorld(t)
		rec := &recorder{}
		d := w.serve(t, "a", core.Options{Logger: rec.logger()})
		transact(t, d, create("t1", 1, "k", "v"))
		path := filepath.Join(w.dir, "a.db")
		for i := 0; len(rec.named(core.EventCheckpoint)) == 0; i++ {
			if i == 1000 {
				t.Fatal("no checkpoint after 1000 commits")
			}
			before, _ := walOf(t, path)
			is.True(t, before < 1000, fmt.Sprint("a log of ", before, " frames, and no checkpoint"))
			transact(t, d, rows(i*2000, 2000)...)
		}
		is.True(t, rec.named(core.EventCheckpoint)[0][core.AttrCheckpointLog].(int64) >= 1000)
	})

	t.Run("should not stop while a read transaction holds the checkpoints back, and start the write-ahead log again once it has ended", func(t *testing.T) {
		w := newWorld(t)
		rec := &recorder{}
		options := core.Options{Logger: rec.logger()}
		options.CheckpointAfter(8)
		d := w.serve(t, "a", options)
		transact(t, d, create("t1", 1, "k", "v"))
		path := filepath.Join(w.dir, "a.db")

		// The read transaction fixes its state before the commits.
		r, err := startRead(t, d)
		is.NotError(t, err)
		start, _ := walOf(t, path)
		for i := range 20 {
			transact(t, d, rows(i*500, 500)...)
		}
		held, checkpointed := walOf(t, path)
		// No checkpoint copies a frame that the read transaction may need.
		is.True(t, held > 8 && checkpointed <= start, fmt.Sprint(held, " frames, ", checkpointed, " in the file, ", start, " at the start of the read"))
		is.True(t, !isDone(d))
		checkpoints := rec.named(core.EventCheckpoint)
		is.True(t, len(checkpoints) > 1, fmt.Sprint(len(checkpoints)))
		for _, event := range checkpoints {
			is.Equal(t, "held_back", event[core.AttrOutcome])
			is.Equal[any](t, slog.LevelDebug, event["level"])
		}
		state, err := dump(t.Context(), r)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0,v): ", show(state))
		is.NotError(t, r.Close(t.Context()))

		// The first commit after the read moves the whole log into the file, and the next one starts
		// the log again.
		transact(t, d, upsert("t1", -1, -1))
		checkpoints = rec.named(core.EventCheckpoint)
		is.Equal(t, "checkpointed", checkpoints[len(checkpoints)-1][core.AttrOutcome])
		transact(t, d, upsert("t1", -2, -2))
		log, _ := walOf(t, path)
		is.True(t, log < 8, fmt.Sprint(log, " frames after the restart, ", held, " before"))
		is.True(t, !isDone(d))
	})

	t.Run("should checkpoint while it catches up, and stop for good when such a checkpoint fails", func(t *testing.T) {
		w := newWorld(t)
		// The cache of the instance that fails is made before the log has a commit, so that its next
		// open writes nothing to it before the catch-up.
		is.NotError(t, w.serve(t, "b", core.Options{}).Close(t.Context()))
		first := w.serve(t, "first", core.Options{})
		transact(t, first, create("t1", 1, "k", "v"))
		for i := range 10 {
			transact(t, first, rows(i*500, 500)...)
		}
		want := read(t, first)
		is.NotError(t, first.Close(t.Context()))

		rec := &recorder{}
		options := core.Options{Logger: rec.logger()}
		options.CheckpointAfter(8)
		d := w.serve(t, "a", options)
		is.Equal(t, want, read(t, d))
		is.True(t, len(rec.named(core.EventCheckpoint)) > 1, fmt.Sprint(len(rec.named(core.EventCheckpoint))))
		log, _ := walOf(t, filepath.Join(w.dir, "a.db"))
		// The log holds the frames since the last checkpoint, which are those of one commit object at
		// most beside the eight before.
		is.True(t, log < 40, fmt.Sprint(log))
		is.NotError(t, d.Close(t.Context()))

		vfs, options := checkpointFaults(t, 8)
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Sync, Files: faultvfs.MainDB, Times: 1})
		b := w.open(t, "b", options)
		err := b.Wait(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrLocal, err)
		is.True(t, strings.Contains(err.Error(), "a checkpoint of the cache"), err.Error())
		is.Equal(t, 1, vfs.Failures())
		is.NotError(t, b.Close(t.Context()))
		is.Equal(t, want, read(t, w.serve(t, "b", core.Options{})))
	})
}

// BenchmarkDB_checkpoint measures commits of 10 and of 200 rows on an instance with syncs on, as an
// instance has them by default. With -benchtime 3000x, the commits of 10 rows pass the default
// threshold of 1000 frames several times.
func BenchmarkDB_checkpoint(b *testing.B) {
	for _, rows := range []int{10, 200} {
		b.Run(fmt.Sprint("rows=", rows), func(b *testing.B) {
			h := sim.NewFree(sim.Buggify{})
			st := simstore.New(h, simstore.Options{})
			d, err := core.Open(b.Context(), h, st.Client("bench", nil), filepath.Join(b.TempDir(), "a.db"), "db", core.Options{})
			is.NotError(b, err)
			defer func() { is.NotError(b, d.Close(b.Context())) }()
			is.NotError(b, d.Wait(b.Context()))
			tx, err := d.Begin(b.Context())
			is.NotError(b, err)
			is.NotError(b, tx.CreateTable(b.Context(), tableOf("t1", 1, "k", "v")))
			is.NotError(b, tx.Commit(b.Context()))
			k := 0
			for b.Loop() {
				tx, err := d.Begin(b.Context())
				is.NotError(b, err)
				for range rows {
					is.NotError(b, tx.Upsert(b.Context(), "t1", row(k, k)))
					k++
				}
				is.NotError(b, tx.Commit(b.Context()))
			}
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "commits/s")
		})
	}
}
