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
}
