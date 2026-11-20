package engine

import (
	"context"
	"iter"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// bugs that the tests of this package plant in it, to show that their checks find them. Each field
// switches on one wrong decision, where the right one is made, and the comment there says "the
// planted bug".
//
// The zero value plants nothing. A value is the field of [Options] that is not exported, and nothing
// in this package sets it but its tests.
type bugs struct {
	// skipUnique makes an insert write its row without the check that the table has no row with its
	// primary key, so that the row replaces one.
	skipUnique bool
	// ackUnknown makes a statement whose commit has an unknown outcome succeed.
	ackUnknown bool
	// checkOutside makes an insert check its table, its column, and its key in a read transaction
	// before its write transaction, and write its row there without the check.
	checkOutside bool
	// keepOpen makes a write statement that fails leave its write transaction open.
	keepOpen bool
	// keepRead makes [Rows.Close] of rows that no loop has read leave the read transaction open.
	keepRead bool
	// annotateNext makes a statement annotate its transaction with each integer argument plus one,
	// while it runs with the argument.
	annotateNext bool
	// keepFailed makes a statement that fails in a write transaction after its savepoint annotate
	// the transaction and release the savepoint, so that the statement is kept as if it had
	// succeeded.
	keepFailed bool
	// readNewest makes each statement of a read-only transaction read the newest state that is
	// committed, and not the state of the beginning of the transaction.
	readNewest bool
	// rollbackKeeps makes the roll back of a write transaction with one write commit it instead.
	rollbackKeeps bool
	// convertReal makes an insert of a real that is a whole number into an INT or INTEGER column
	// store the integer, as SQLite converts it also in a STRICT table, where s4ql refuses a value of
	// another type than its column.
	convertReal bool
	// listOrder makes an insert with a list of columns give its values to the columns of the table in
	// the order of the list, the first value to the first column, and not to the columns that the
	// list names.
	listOrder bool
	// zeroLeftOut makes an insert with a list of columns give a column that takes NULL and that the
	// list leaves out the zero value of its type, 0, 0.0, the empty text, or the empty blob, in place
	// of NULL.
	zeroLeftOut bool
	// openBound makes a scan by one or two bounds of a key read a closed bound, of <= or >=, as
	// open, so that it leaves out the row whose key is the bound, which the filter above it would
	// give.
	openBound bool
	// changeNothing makes an update and a delete find no row, change nothing, and report a count of 0,
	// as a statement does that finds no row, so that a transaction of it alone commits nothing.
	changeNothing bool
	// keepOldKey makes an update that gives a row a new key write the row under the new key and leave
	// the row of the old key, so that the table holds the row twice, with the count that SQLite gives.
	keepOldKey bool
}

// reader of a query on r: r, or with the planted bug openBound r as [openBounds] reads it.
func (b bugs) reader(r reader) reader {
	if b.openBound {
		return openBounds{r}
	}
	return r
}

// openBounds reads the rows of its reader, but for a scan by bounds of a key, one or two, which
// reads a closed bound as open: the planted bug openBound.
type openBounds struct {
	reader
}

func (o openBounds) scan(ctx context.Context, name string, s core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	s.Lower.Open = s.Lower.Open || s.Lower.Value != nil
	s.Upper.Open = s.Upper.Open || s.Upper.Value != nil
	return o.reader.scan(ctx, name, s)
}

// zeroOf the type t: the value that the planted bug zeroLeftOut gives.
func zeroOf(t s4qlpb.Type) *s4qlpb.Value {
	switch t {
	case s4qlpb.Type_TYPE_REAL:
		return s4qlpb.Value_builder{Real: new(0.0)}.Build()
	case s4qlpb.Type_TYPE_TEXT:
		return s4qlpb.Value_builder{Text: new("")}.Build()
	case s4qlpb.Type_TYPE_BLOB:
		return s4qlpb.Value_builder{Blob: []byte{}}.Build()
	}
	return s4qlpb.Value_builder{Integer: new(int64(0))}.Build()
}
