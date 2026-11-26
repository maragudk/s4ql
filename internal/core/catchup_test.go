package core_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/sim"
	"maragu.dev/s4ql/internal/store/simstore"
)

// BenchmarkDB_catchUp measures how long an instance with an empty cache, with syncs on as an
// instance has them by default, takes to catch up on a log of 3000 commits of 10 rows each, from its
// open until it serves. Catch-up applies each commit object in a transaction of the cache of its own.
func BenchmarkDB_catchUp(b *testing.B) {
	h := sim.NewFree(sim.Buggify{})
	st := simstore.New(h, simstore.Options{})
	dir := b.TempDir()
	var fast core.Options
	fast.SyncOff()
	writer, err := core.Open(b.Context(), h, st.Client("writer", nil), filepath.Join(dir, "writer.db"), "db", fast)
	is.NotError(b, err)
	is.NotError(b, writer.Wait(b.Context()))
	tx, err := writer.Begin(b.Context())
	is.NotError(b, err)
	is.NotError(b, tx.CreateTable(b.Context(), tableOf("t1", 1, "k", "v")))
	is.NotError(b, tx.Commit(b.Context()))
	const commits = 3000
	for i := range commits {
		tx, err := writer.Begin(b.Context())
		is.NotError(b, err)
		for j := range 10 {
			is.NotError(b, tx.Upsert(b.Context(), "t1", row(i*10+j, j)))
		}
		is.NotError(b, tx.Commit(b.Context()))
	}
	is.NotError(b, writer.Close(b.Context()))

	n := 0
	for b.Loop() {
		d, err := core.Open(b.Context(), h, st.Client(fmt.Sprint("reader-", n), nil), filepath.Join(dir, fmt.Sprint("cache-", n, ".db")), "db", core.Options{})
		is.NotError(b, err)
		is.NotError(b, d.Wait(b.Context()))
		b.StopTimer()
		is.NotError(b, d.Close(b.Context()))
		n++
		b.StartTimer()
	}
	b.ReportMetric(float64(commits)*float64(b.N)/b.Elapsed().Seconds(), "commits/s")
}
