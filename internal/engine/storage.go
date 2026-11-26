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
	// indexes of the table with the name, which exists, in the order in which they were created,
	// which is the same on every instance.
	indexes(ctx context.Context, name string) ([]*s4qlpb.CreateIndex, error)
	// index with the name, or nil if there is none.
	index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error)
	// scan the rows of the table with the name by one of its indexes, or by its primary key, as s
	// says: see [core.IndexScan]. The rows come in the order of the key of the index, or in its
	// reverse.
	scan(ctx context.Context, name string, s core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error]
	// get the row of the table with the name that has the primary key, or nil if there is none.
	get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error)
	// rowsRead is the number of rows that the transaction has read from the storage so far, by its
	// scans and its gets, as [core.ReadTx.RowsRead] counts them.
	rowsRead() int64
}

// writer is what the operators read and change: the tables and rows of one write transaction, which
// reads its own writes.
type writer interface {
	reader
	// createTable that no table of its name exists for.
	createTable(ctx context.Context, ct *s4qlpb.CreateTable) error
	// dropTable with the name, which exists, and with it its indexes.
	dropTable(ctx context.Context, name string) error
	// createIndex that no table and no index of its name exists for, on a table that exists.
	createIndex(ctx context.Context, ci *s4qlpb.CreateIndex) error
	// dropIndex with the name, which exists.
	dropIndex(ctx context.Context, name string) error
	// upsert the row into the table with the name: insert it, or replace the row with its key. It
	// fails with SQLite's `string or blob too big` for a row whose record is too long.
	upsert(ctx context.Context, name string, row []*s4qlpb.Value) error
	// insertNew the row into the table with the name if the table has no row with its key, and report
	// whether it did. It fails with SQLite's `string or blob too big` for a row whose record is too
	// long also when the key exists, as SQLite does.
	insertNew(ctx context.Context, name string, row []*s4qlpb.Value) (bool, error)
	// delete the row with the primary key from the table with the name, and report whether there was
	// one.
	delete(ctx context.Context, name string, key []*s4qlpb.Value) (bool, error)
	// keys runs f with an empty set of primary keys of the table with the name, which lasts for the
	// call of f, and gives the error of f: see [keySet]. Its keys come in the order of the primary
	// key for a nil by, and otherwise in the order of the columns by of their rows, as
	// [core.Tx.KeysBy] says, which hold every column of the primary key.
	keys(ctx context.Context, name string, by []core.KeyColumn, f func(keySet) error) error
}

// keySet is a set of primary keys of a table, of any size, for a statement that changes the rows
// that it finds: it adds the key of each row that it finds while it scans the table, and then
// changes the row of each key that all gives. The scan reads no table that changes while it runs,
// because only the set changes, and all reads each key before the statement changes its row, so the
// statement changes each row that it found once, and none that it wrote, as [core.Tx.Keys] says.
type keySet interface {
	// add the key to the set, unless the set has it: the values of the primary key, or, for a set in
	// the order of the columns by, the values of those columns of the row of the key.
	add(ctx context.Context, key []*s4qlpb.Value) error
	// all keys of the set, in the order of the key, as SQLite compares keys. The table and the set can
	// change between two keys, and all gives the least key of the set above the one before. For a set
	// in the order of the columns by, all gives the primary keys in that order, the table can change
	// between two keys, and the set takes no key once all has begun.
	all(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error]
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

// bound of a scan on a column of a key: value is the smallest value that a lower bound lets through
// and the largest that an upper bound does, and the rows whose value is value are left out if open. A
// bound without a value bounds nothing.
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
	for _, r := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrIndexExists, core.ErrNoIndex, core.ErrReservedName, core.ErrInvalid, core.ErrTxDone, core.ErrReaderClosed} {
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

// lookUpIndex with the name through the call index, as [reader.index] gives it.
func lookUpIndex(ctx context.Context, name string, index func(ctx context.Context, name string) (*s4qlpb.CreateIndex, error)) (*s4qlpb.CreateIndex, error) {
	ci, err := index(ctx, name)
	if errors.Is(err, core.ErrNoIndex) {
		return nil, nil
	}
	return ci, fromCore(err)
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

func (c coreReadTx) indexes(ctx context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	cis, err := c.r.Indexes(ctx, name)
	return cis, fromCore(err)
}

func (c coreReadTx) index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error) {
	return lookUpIndex(ctx, name, c.r.Index)
}

func (c coreReadTx) scan(ctx context.Context, name string, s core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.r.ScanIndex(ctx, name, s))
}

func (c coreReadTx) get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row, err := c.r.Get(ctx, name, key)
	return row, fromCore(err)
}

func (c coreReadTx) rowsRead() int64 {
	return c.r.RowsRead()
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

func (c coreTx) indexes(ctx context.Context, name string) ([]*s4qlpb.CreateIndex, error) {
	cis, err := c.tx.Indexes(ctx, name)
	return cis, fromCore(err)
}

func (c coreTx) index(ctx context.Context, name string) (*s4qlpb.CreateIndex, error) {
	return lookUpIndex(ctx, name, c.tx.Index)
}

func (c coreTx) scan(ctx context.Context, name string, s core.IndexScan) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.tx.ScanIndex(ctx, name, s))
}

func (c coreTx) get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row, err := c.tx.Get(ctx, name, key)
	return row, fromCore(err)
}

func (c coreTx) rowsRead() int64 {
	return c.tx.RowsRead()
}

func (c coreTx) createIndex(ctx context.Context, ci *s4qlpb.CreateIndex) error {
	err := c.tx.CreateIndex(ctx, ci)
	if errors.Is(err, core.ErrTooLongForCache) {
		return tooLongForCache("index", ci.GetIndex())
	}
	return fromCore(err)
}

func (c coreTx) dropIndex(ctx context.Context, name string) error {
	return fromCore(c.tx.DropIndex(ctx, name))
}

func (c coreTx) createTable(ctx context.Context, ct *s4qlpb.CreateTable) error {
	err := c.tx.CreateTable(ctx, ct)
	if errors.Is(err, core.ErrTooLongForCache) {
		return tooLongForCache("table", ct.GetTable())
	}
	return fromCore(err)
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

func (c coreTx) delete(ctx context.Context, name string, key []*s4qlpb.Value) (bool, error) {
	found, err := c.tx.Delete(ctx, name, key)
	return found, fromCore(err)
}

func (c coreTx) keys(ctx context.Context, name string, by []core.KeyColumn, f func(keySet) error) error {
	g := func(k *core.KeySet) error { return f(coreKeys{k: k}) }
	if by == nil {
		return fromCore(c.tx.Keys(ctx, name, g))
	}
	return fromCore(c.tx.KeysBy(ctx, name, by, g))
}

// coreKeys is the [keySet] of a [*core.KeySet].
type coreKeys struct {
	k *core.KeySet
}

func (c coreKeys) add(ctx context.Context, key []*s4qlpb.Value) error {
	return fromCore(c.k.Add(ctx, key))
}

func (c coreKeys) all(ctx context.Context) iter.Seq2[[]*s4qlpb.Value, error] {
	return fromCoreSeq(c.k.All(ctx))
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
