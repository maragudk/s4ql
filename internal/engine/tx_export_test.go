package engine

// The names of the event of a transaction and of its attributes, for the tests of the telemetry.
const (
	EventTransaction = eventTransaction
	AttrTxKind       = attrTxKind
	AttrTxEnd        = attrTxEnd
	AttrTxStatements = attrTxStatements
	AttrTxWrites     = attrTxWrites
	AttrTxPosition   = attrTxPosition
)

// TxStatements is the number of statements that the connection was given while its transaction
// that spans statements is open, as the event of the transaction counts them, or -1 if none is open.
func (c *Conn) TxStatements() int {
	if c.tx == nil {
		return -1
	}
	return c.tx.statements
}
