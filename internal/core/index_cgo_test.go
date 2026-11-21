//go:build cgo

// The tests in this file are of a scan whose context ends within a step of SQLite, which SQLite
// stops in the cgo binding, whose [sqlite.Stmt.StepContext] installs a progress handler. The
// browser's binding runs a step to its end, so they are not built there.

package core_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/ctxtest"
)

// progressHandler is the function of the cgo binding that looks at the context of a step of
// [sqlite.Stmt.StepContext] every 10,000 instructions of SQLite, as [ctxtest.Context.EndedBy]
// names it.
const progressHandler = "maragu.dev/s4ql/internal/sqlite.s4qlInterrupted"

// scanBig with [bigSorted] to its end, check that the rows come in the order of the scan, and give
// their number and the number of looks at the context up to the first row: in the first step of
// the scan, in which SQLite sorts, and before it.
func scanBig(t *testing.T, s bigScanner) (n, looks int) {
	t.Helper()
	ctx := ctxtest.EndAtErr(t.Context(), math.MaxInt)
	previous := int64(math.MaxInt64)
	for row, err := range s.ScanIndex(ctx, "big", bigSorted) {
		is.NotError(t, err)
		if n == 0 {
			looks = ctx.Calls()
		}
		is.True(t, row[1].GetInteger() < previous, "the rows in the order of the scan")
		previous = row[1].GetInteger()
		n++
	}
	return n, looks
}

// TestScanIndex_interrupt ends the context of a scan with an order at points of SQLite's work, with
// [ctxtest.EndAtErr]: within the sort of its first step, and within the count of the rows that it read
// and did not give, where SQLite's progress handler looks at the context every 10,000 instructions.
// So each test stops SQLite at the same point on any machine.
func TestScanIndex_interrupt(t *testing.T) {
	for _, read := range []bool{true, false} {
		t.Run(fmt.Sprint("should end a sort with the error of the context at the look of SQLite at which the context ends, give no row, and leave the transaction going on, in a read transaction ", read), func(t *testing.T) {
			// The length limit is the smallest that SQLite takes, so the step that sorts raises it, and
			// puts it back after it.
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{LengthLimit: 1000})
			fillBig(t, d, "x")
			s, tx, total := beginBig(t, d, read)

			n, looks := scanBig(t, s)
			is.Equal(t, total, n)
			is.True(t, looks >= 3, fmt.Sprint("SQLite looked at the context ", looks, " times in the sort"))

			for _, at := range []int{1, 2, 3} {
				ctx := ctxtest.EndAtErr(t.Context(), at)
				before, n := s.RowsRead(), 0
				for row, err := range s.ScanIndex(ctx, "big", bigSorted) {
					n++
					is.True(t, row == nil)
					is.Error(t, context.Canceled, err)
				}
				is.Equal(t, 1, n)
				is.Equal(t, progressHandler, ctx.EndedBy(), fmt.Sprint("ended at the look ", at))
				// The scan gave no row, and SQLite's sort counts none.
				is.Equal(t, before, s.RowsRead())
				// The same scan without the context runs to its end, on the same connection.
				n, _ = scanBig(t, s)
				is.Equal(t, total, n)
			}
			// A deadline that has passed ends the scan in the same way.
			ctx, cancel := context.WithTimeout(t.Context(), -time.Second)
			defer cancel()
			for _, err := range s.ScanIndex(ctx, "big", bigSorted) {
				is.Error(t, context.DeadlineExceeded, err)
			}
			if read {
				return
			}

			// The transaction goes on: it reads its own row and writes another, and SQLite refuses a
			// row whose record is longer than the limit of the connection, which the sort put back.
			got, err := tx.Get(t.Context(), "big", row(bigRows))
			is.NotError(t, err)
			is.Equal(t, int64(-1), got[1].GetInteger())
			is.NotError(t, tx.Upsert(t.Context(), "big", row(bigRows+1, -2, "tx", "")))
			is.Error(t, core.ErrTooBig, tx.Upsert(t.Context(), "big", row(bigRows+2, -3, strings.Repeat("a", 600), strings.Repeat("b", 600))))
			is.NotError(t, tx.Commit(t.Context()))
			r, err := startRead(t, d)
			is.NotError(t, err)
			defer func() { is.NotError(t, r.Close(t.Context())) }()
			n, _ = scanBig(t, r)
			is.Equal(t, bigRows+2, n)
		})

		t.Run(fmt.Sprint("should end the count of the rows that a sort read and did not give with the error of the context, count none of them, and leave the transaction going on, in a read transaction ", read), func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			fillBig(t, d, "x")
			s, tx, total := beginBig(t, d, read)

			// A loop that stops at the first row makes the cache count the rows. probe gives the number
			// of looks at the context up to the first row, and the count of a loop that does not end
			// it counts every row.
			probe := ctxtest.EndAtErr(t.Context(), math.MaxInt)
			before, looks := s.RowsRead(), 0
			for _, err := range s.ScanIndex(probe, "big", bigSorted) {
				is.NotError(t, err)
				looks = probe.Calls()
				break
			}
			is.Equal(t, int64(total), s.RowsRead()-before)

			// The cache looks at the context itself between the first row and the count, so the
			// context ends at each look after the first row in turn, up to the first look of SQLite's
			// progress handler, which can only be in the count. Whichever look ends it, the count
			// counts nothing, and the scan has read the row that it gave.
			var ctx *ctxtest.Context
			for at := looks + 1; at <= probe.Calls(); at++ {
				ctx = ctxtest.EndAtErr(t.Context(), at)
				before = s.RowsRead()
				for _, err := range s.ScanIndex(ctx, "big", bigSorted) {
					is.NotError(t, err)
					break
				}
				is.Error(t, context.Canceled, ctx.Err())
				is.Equal(t, int64(1), s.RowsRead()-before, fmt.Sprint("ended at the look ", at, " by ", ctx.EndedBy()))
				if ctx.EndedBy() == progressHandler {
					break
				}
			}
			is.True(t, ctx != nil && ctx.EndedBy() == progressHandler, "SQLite's progress handler looked at the context in no count")

			// The transaction goes on, and so does the instance.
			n, _ := scanBig(t, s)
			is.Equal(t, total, n)
			is.True(t, !isDone(d))
			if tx != nil {
				is.NotError(t, tx.Upsert(t.Context(), "big", row(bigRows+1, -2, "tx", "")))
				is.NotError(t, tx.Commit(t.Context()))
			}
		})

		t.Run(fmt.Sprint("should count the rows that a sort by an index read and did not give by that index, within its bounds, in a read transaction ", read), func(t *testing.T) {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			fillBig(t, d, "x")
			write(t, d, func(tx *core.Tx) { is.NotError(t, tx.CreateIndex(t.Context(), indexOf("byc1", "big", 1))) })
			s, _, _ := beginBig(t, d, read)

			// Three rows of the table have c1 below 3, and the write transaction has written a fourth.
			scan := core.IndexScan{Index: "byc1", Upper: core.Bound{Value: row(3)[0], Open: true}, Order: orderOf(desc(0))}
			want := int64(3)
			if !read {
				want = 4
			}
			probe := ctxtest.EndAtErr(t.Context(), math.MaxInt)
			before, looks := s.RowsRead(), 0
			for _, err := range s.ScanIndex(probe, "big", scan) {
				is.NotError(t, err)
				looks = probe.Calls()
				break
			}
			is.Equal(t, want, s.RowsRead()-before)
			// A count of the rows within the bounds of the index runs in fewer instructions than SQLite
			// looks at the context after, where a count of the 10,000 rows of the table runs in about
			// 40,000. The cache itself looks at the context once or twice after the first row.
			is.True(t, probe.Calls()-looks <= 2, fmt.Sprint("SQLite looked at the context ", probe.Calls()-looks, " times after the first row"))
		})
	}
}
