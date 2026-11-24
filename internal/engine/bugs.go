package engine

import (
	"context"
	"iter"
	"slices"

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
	// zeroLeftOut makes an insert with a list of columns give a column that takes NULL, has no
	// default, and that the list leaves out the zero value of its type, 0, 0.0, the empty text, or the
	// empty blob, in place of NULL.
	zeroLeftOut bool
	// nullDefault makes an insert with a list of columns give a column with a default that the list
	// leaves out NULL in place of its default, of its own and in a transaction.
	nullDefault bool
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
	// pinByPlace makes the fitting of the conditions to the key of an index or of the primary key
	// look for the pin of the n-th column of the key among the conditions on the n-th column of the
	// table, and not on the column itself, so that a scan pins a column to the value of another.
	pinByPlace bool
	// replaceExisting makes a create table with IF NOT EXISTS of a table that exists drop that table
	// and create the table of the statement, where SQLite does nothing.
	replaceExisting bool
	// unturnedBound makes the fitting of the conditions to the key of an index or of the primary key
	// bound a column by a condition with its value on the left, of slice 12, with the operator as the
	// text writes it, and not turned round, so that `5 < a` bounds a from above, and the scan reads
	// the rows below 5, of which the filter above it gives none.
	unturnedBound bool
	// turnMixed makes a key deliver the order of an ORDER BY whose columns are in the direction of the
	// key and in the other both, of slice 13, as if each were in the direction of the first of them
	// against the key, so that the rows of a column in the other direction come in the wrong order.
	turnMixed bool
	// limitFirst makes a query with ORDER BY and LIMIT whose rows are sorted take the first rows of its
	// access, as many as the LIMIT keeps, and sort those, in place of the first rows of the sort.
	limitFirst bool
	// ascendLast makes a sort of the rows of a query with ORDER BY sort by its last column ascending,
	// also where the column is descending.
	ascendLast bool
	// skipGap makes a key deliver the order of an ORDER BY that names its columns with a gap, of slice
	// 13, as if the columns of the key after the first that the ORDER BY names, and that it does not
	// name, were not in the key, so that a key (x, y, z) delivers ORDER BY x, z, and the rows come in
	// the order of y where x ties.
	skipGap bool
	// lastIndexWins makes the choice of an access, at run time, take the indexes of the table in the
	// reverse of the order in which they were created, so that of two indexes that the rules rank
	// alike, the one created last wins, where the rules choose the one created first.
	lastIndexWins bool
	// rowsOfFailed makes an insert, an update, or a delete with RETURNING of its own through
	// [Conn.Query] whose commit fails give its rows without the error, as if the commit had
	// succeeded, of slice 14.
	rowsOfFailed bool
	// oldValues makes an update with RETURNING give each row as it was before the update, and not
	// with the values that the update gave it.
	oldValues bool
	// deleteNone makes a delete with RETURNING give no row, as if it deleted none, with the count of
	// the rows that it deleted.
	deleteNone bool
	// dropLast makes a statement with RETURNING that changes two rows or more leave out the last row
	// that it changed, in the order of the key.
	dropLast bool
	// starKeyFirst makes RETURNING * give the column of the primary key first and then the others in
	// the order of the table, as the record of a row without rowid holds them, and not all of them in
	// the order of the table.
	starKeyFirst bool
	// lastRow makes a scalar subquery of slice 15 give the value of the last row of its access, and not
	// of the first.
	lastRow bool
	// firstNotNull makes a scalar subquery give the value of the first row of its access whose value is
	// not NULL, and NULL only where every row has NULL.
	firstNotNull bool
	// ownChanges makes an update or a delete with a scalar subquery change the rows that it finds one at
	// a time, and compute its subqueries again after each change, so that a subquery of the table that
	// it changes sees the rows that it changed, where SQLite computes each once, before it changes any.
	ownChanges bool
	// keepValue makes a scalar subquery keep its value from the last run of a statement whose plan
	// prints as its own, as a prepared statement could, also when the arguments or the rows changed
	// since.
	keepValue bool
	// noRowMatches makes a scalar subquery that finds no row make its comparison true of every row whose
	// column is not NULL, where the comparison of a column with NULL is never true.
	noRowMatches bool
	// outsideTx makes a scalar subquery of an update or a delete in a transaction read the newest state
	// that is committed, and not the state of its transaction with the changes of the statements before
	// it.
	outsideTx bool
	// swapSubqueries makes a statement with two scalar subqueries or more give the first the value of
	// the second and the second that of the first.
	swapSubqueries bool
	// limitZeroRow makes a scalar subquery with LIMIT 0 give the value of its first row, where it gives
	// no row and its value is NULL.
	limitZeroRow bool
	// orderIgnored makes a scalar subquery with an ORDER BY give the value of the first row of the access
	// that its conditions alone choose, read forward or backward as the engine draws it, and not of the
	// first row of its ORDER BY.
	orderIgnored bool
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

// turnedLike is the ORDER BY by with each column that the key holds in the direction of the first
// such column against the key, which the planted bug turnMixed delivers in place of by.
func turnedLike(key, by []core.KeyColumn) []core.KeyColumn {
	turned := slices.Clone(by)
	first, against := true, false
	for i, c := range turned {
		at := slices.IndexFunc(key, func(k core.KeyColumn) bool { return k.Position == c.Position })
		if at < 0 {
			continue
		}
		if first {
			first, against = false, key[at].Descending != c.Descending
		}
		turned[i].Descending = key[at].Descending != against
	}
	return turned
}

// skippedGaps is the key with each column after the first that the ORDER BY by names left out
// where by does not name it, which the planted bug skipGap delivers by in place of key.
func skippedGaps(key, by []core.KeyColumn) []core.KeyColumn {
	named := func(k core.KeyColumn) bool {
		return slices.ContainsFunc(by, func(c core.KeyColumn) bool { return c.Position == k.Position })
	}
	var kept []core.KeyColumn
	matched := false
	for _, k := range key {
		if matched && !named(k) {
			continue
		}
		matched = matched || named(k)
		kept = append(kept, k)
	}
	return kept
}

// ascendedLast is the ORDER BY by with its last column ascending, which the planted bug ascendLast
// sorts by in place of by.
func ascendedLast(by []core.KeyColumn) []core.KeyColumn {
	ascended := slices.Clone(by)
	ascended[len(ascended)-1].Descending = false
	return ascended
}

// sortedAfter gives the rows of its source, which it reads to their end first, sorted by the columns
// by, NULL before every value: the planted bug limitFirst, whose source is a [limit] of the rows of
// the access, unsorted.
type sortedAfter struct {
	src source
	by  []core.KeyColumn
}

func (s *sortedAfter) columns() []Column {
	return s.src.columns()
}

func (s *sortedAfter) scanned() *scan {
	return s.src.scanned()
}

func (s *sortedAfter) rows(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		var rows [][]*s4qlpb.Value
		for row, err := range s.src.rows(ctx) {
			if err != nil {
				yield(nil, err)
				return
			}
			rows = append(rows, row)
		}
		slices.SortStableFunc(rows, func(a, b []*s4qlpb.Value) int {
			for _, c := range s.by {
				x, y := a[c.Position], b[c.Position]
				var n int
				switch {
				case x.HasNull() || y.HasNull():
					n = map[bool]int{true: 1}[!x.HasNull()] - map[bool]int{true: 1}[!y.HasNull()]
				default:
					n, _ = compare(x, y)
				}
				if c.Descending {
					n = -n
				}
				if n != 0 {
					return n
				}
			}
			return 0
		})
		for _, row := range rows {
			if !yield(row, nil) {
				return
			}
		}
	}
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

// changedOneByOne calls change with the primary key of each row that the filter f gives, whose key is
// the column at the position key, one row at a time, and computes the subqueries of f again after
// each change, as [filter.again] does, so that the next row is the first that f gives then of those
// that it did not change: the planted bug ownChanges, in place of [changeRows].
func changedOneByOne(ctx context.Context, key int, f *filter, change func(key []*s4qlpb.Value) error) (int64, error) {
	var done []*s4qlpb.Value
	for {
		var next *s4qlpb.Value
		for row, err := range f.rows(ctx) {
			if err != nil {
				return int64(len(done)), err
			}
			if !slices.ContainsFunc(done, func(v *s4qlpb.Value) bool { n, _ := compare(v, row[key]); return n == 0 }) {
				next = row[key]
				break
			}
		}
		if next == nil {
			return int64(len(done)), nil
		}
		if err := change([]*s4qlpb.Value{next}); err != nil {
			return int64(len(done)), err
		}
		done = append(done, next)
		if err := f.again(ctx); err != nil {
			return int64(len(done)), err
		}
	}
}
