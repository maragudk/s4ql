package engine

import (
	"context"
	"errors"
	"iter"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// reader is what the operators read: the tables and rows of one transaction. A name of a table is
// compared as SQLite compares names, without regard to the case of the ASCII letters.
type reader interface {
	// table with the name, or nil if there is none.
	table(ctx context.Context, name string) (*s4qlpb.CreateTable, error)
	// scan the rows of the table with the name, in the order of the primary key.
	scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error]
	// scanRange is scan of the rows whose key is within the bounds lower and upper, as SQLite
	// compares the keys, of a table whose primary key is one column. Each bound has a value of the
	// type of the key, or none.
	scanRange(ctx context.Context, name string, lower, upper bound) iter.Seq2[[]*s4qlpb.Value, error]
	// get the row of the table with the name that has the primary key, or nil if there is none.
	get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error)
}

// writer is what the operators read and change: the tables and rows of one write transaction, which
// reads its own writes.
type writer interface {
	reader
	// createTable that no table of its name exists for.
	createTable(ctx context.Context, ct *s4qlpb.CreateTable) error
	// dropTable with the name, which exists.
	dropTable(ctx context.Context, name string) error
	// upsert the row into the table with the name: insert it, or replace the row with its key. Only
	// the planted bug skipUnique writes so.
	upsert(ctx context.Context, name string, row []*s4qlpb.Value) error
	// insertNew the row into the table with the name if the table has no row with its key, and report
	// whether it did. It fails with SQLite's `string or blob too big` for a row whose record is too
	// long also when the key exists, as SQLite does.
	insertNew(ctx context.Context, name string, row []*s4qlpb.Value) (bool, error)
}

// database is what the engine needs of the storage core: write transactions, and readers.
type database interface {
	begin(ctx context.Context) (writeTx, error)
	// reader of the database, which its owner closes.
	reader(ctx context.Context) (readConn, error)
	// lengthLimit is the largest text or blob, in bytes, that the database holds.
	lengthLimit() int
}

// readConn is a reader of the database: a connection that runs one read transaction at a time.
type readConn interface {
	// read starts a read transaction. It fails while the one before it on the reader is open.
	read(ctx context.Context) (readTx, error)
	// close the reader, and end the read transaction that is open.
	close(ctx context.Context) error
}

// readTx is a read transaction.
type readTx interface {
	reader
	// position of the state that the transaction sees: the last commit object that it holds.
	position() format.Name
	// serving checks that the instance serves now, as a new read transaction needs it, before a
	// further statement of a transaction that spans statements.
	serving(ctx context.Context) error
	end(ctx context.Context) error
}

// writeTx is a write transaction.
type writeTx interface {
	writer
	// position of the state that the transaction began from: the parent of its commit object.
	position() format.Name
	// annotate the transaction with a statement that it ran: its SQL text, and the arguments of its
	// parameters, by position.
	annotate(ctx context.Context, sql string, args []*s4qlpb.Value) error
	// serving checks that the instance serves now, as a new write transaction needs it, before a
	// further statement of a transaction that spans statements.
	serving(ctx context.Context) error
	// savepoint marks the state of the transaction before a statement, which rollbackTo restores and
	// release keeps. One is open at a time.
	savepoint(ctx context.Context) error
	release(ctx context.Context) error
	rollbackTo(ctx context.Context) error
	commit(ctx context.Context) error
	rollback(ctx context.Context) error
	// lost is nil unless the instance ended the transaction because it lost its lease, and then the
	// error that every call of the transaction gives, which matches [core.ErrTxLeaseLost]. The roll
	// back of such a transaction returns nil.
	lost() error
}

// bound of a scan by the key: value is the first key that a lower bound lets through and the last
// that an upper bound does, and the row whose key is value is left out if open. A bound without a
// value bounds nothing.
type bound struct {
	value *s4qlpb.Value
	open  bool
}

func (b bound) core() core.Bound {
	return core.Bound{Value: b.value, Open: b.open}
}

// coreDB is the [database] of a [*core.DB].
type coreDB struct {
	d *core.DB
}

func (c coreDB) begin(ctx context.Context) (writeTx, error) {
	tx, err := c.d.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return coreTx{tx: tx}, nil
}

func (c coreDB) reader(ctx context.Context) (readConn, error) {
	r, err := c.d.Reader(ctx)
	if err != nil {
		return nil, err
	}
	return coreReader{r: r}, nil
}

func (c coreDB) lengthLimit() int {
	return c.d.LengthLimit()
}

// coreReader is the [readConn] of a [*core.Reader].
type coreReader struct {
	r *core.Reader
}

func (c coreReader) read(ctx context.Context) (readTx, error) {
	r, err := c.r.Read(ctx)
	if err != nil {
		return nil, fromCore(err)
	}
	return coreReadTx{r: r}, nil
}

func (c coreReader) close(ctx context.Context) error {
	return c.r.Close(ctx)
}

// fromCore is err of a call of the core, with a refusal of the core as an internal error,
// [core.ErrTooBig] as SQLite's `string or blob too big`, and every other error as it is. The engine
// checks for each refusal before it calls the core, and gives SQLite's error for it, so one that the
// core still gives is a fault of the engine or of the pass after the parser; so is a call on a
// transaction that has ended, and a read on a reader that is closed. The core gives ErrTooBig also
// for a row whose record is longer than the limit, which only SQLite's write of the row finds, and
// the transaction goes on after it.
func fromCore(err error) error {
	if errors.Is(err, core.ErrTooBig) {
		return tooBig()
	}
	for _, r := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrReservedName, core.ErrInvalid, core.ErrTxDone, core.ErrReaderClosed} {
		if errors.Is(err, r) {
			return internalError("the storage core refused a call that the engine had checked: %v", err)
		}
	}
	return err
}

// lookUp the table with the name through the call table, as [reader.table] gives it.
func lookUp(ctx context.Context, name string, table func(ctx context.Context, name string) (*s4qlpb.CreateTable, error)) (*s4qlpb.CreateTable, error) {
	ct, err := table(ctx, name)
	if errors.Is(err, core.ErrNoTable) {
		return nil, nil
	}
	return ct, fromCore(err)
}

// fromCoreSeq is the scan seq with the errors of [fromCore].
func fromCoreSeq(seq iter.Seq2[[]*s4qlpb.Value, error]) iter.Seq2[[]*s4qlpb.Value, error] {
	return func(yield func([]*s4qlpb.Value, error) bool) {
		for row, err := range seq {
			if !yield(row, fromCore(err)) || err != nil {
				return
			}
		}
	}
}

// coreReadTx is the [readTx] of a [*core.ReadTx].
type coreReadTx struct {
	r *core.ReadTx
}

func (c coreReadTx) table(ctx context.Context, name string) (*s4qlpb.CreateTable, error) {
	return lookUp(ctx, name, c.r.Table)
}

func (c coreReadTx) scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.r.Scan(ctx, name))
}

func (c coreReadTx) scanRange(ctx context.Context, name string, lower, upper bound) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.r.ScanRange(ctx, name, lower.core(), upper.core()))
}

func (c coreReadTx) get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row, err := c.r.Get(ctx, name, key)
	return row, fromCore(err)
}

func (c coreReadTx) position() format.Name {
	return c.r.Position()
}

func (c coreReadTx) serving(ctx context.Context) error {
	return fromCore(c.r.Serving(ctx))
}

func (c coreReadTx) end(ctx context.Context) error {
	return c.r.Close(ctx)
}

// coreTx is the [writeTx] of a [*core.Tx].
type coreTx struct {
	tx *core.Tx
}

func (c coreTx) table(ctx context.Context, name string) (*s4qlpb.CreateTable, error) {
	return lookUp(ctx, name, c.tx.Table)
}

func (c coreTx) scan(ctx context.Context, name string) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.tx.Scan(ctx, name))
}

func (c coreTx) scanRange(ctx context.Context, name string, lower, upper bound) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.tx.ScanRange(ctx, name, lower.core(), upper.core()))
}

func (c coreTx) get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row, err := c.tx.Get(ctx, name, key)
	return row, fromCore(err)
}

func (c coreTx) createTable(ctx context.Context, ct *s4qlpb.CreateTable) error {
	return fromCore(c.tx.CreateTable(ctx, ct))
}

func (c coreTx) dropTable(ctx context.Context, name string) error {
	return fromCore(c.tx.DropTable(ctx, name))
}

func (c coreTx) upsert(ctx context.Context, name string, row []*s4qlpb.Value) error {
	return fromCore(c.tx.Upsert(ctx, name, row))
}

func (c coreTx) insertNew(ctx context.Context, name string, row []*s4qlpb.Value) (bool, error) {
	inserted, err := c.tx.Insert(ctx, name, row)
	return inserted, fromCore(err)
}

func (c coreTx) position() format.Name {
	return c.tx.Position()
}

func (c coreTx) annotate(ctx context.Context, sql string, args []*s4qlpb.Value) error {
	return fromCore(c.tx.Annotate(ctx, sql, args))
}

func (c coreTx) serving(ctx context.Context) error {
	return fromCore(c.tx.Serving(ctx))
}

func (c coreTx) savepoint(ctx context.Context) error {
	return fromCore(c.tx.Savepoint(ctx))
}

func (c coreTx) release(ctx context.Context) error {
	return fromCore(c.tx.Release(ctx))
}

func (c coreTx) rollbackTo(ctx context.Context) error {
	return fromCore(c.tx.RollbackTo(ctx))
}

// commit gives the error of the core as it is: [core.ErrInvalid] there is a commit time that the
// commit object cannot hold, which comes from the clock and not from a call that the engine checks.
func (c coreTx) commit(ctx context.Context) error {
	return c.tx.Commit(ctx)
}

func (c coreTx) rollback(ctx context.Context) error {
	return fromCore(c.tx.Rollback(ctx))
}

func (c coreTx) lost() error {
	return c.tx.Lost()
}
