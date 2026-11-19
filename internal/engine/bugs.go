package engine

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
}
