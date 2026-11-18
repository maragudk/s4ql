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
	// upsert the row into the table with the name: insert it, or replace the row with its key.
	upsert(ctx context.Context, name string, row []*s4qlpb.Value) error
}

// database is what the engine needs of the storage core: transactions.
type database interface {
	begin(ctx context.Context) (writeTx, error)
	read(ctx context.Context) (readTx, error)
}

// readTx is a read transaction.
type readTx interface {
	reader
	// position of the state that the transaction sees: the last commit object that it holds.
	position() format.Name
	end(ctx context.Context) error
}

// writeTx is a write transaction.
type writeTx interface {
	writer
	// position of the state that the transaction began from: the parent of its commit object.
	position() format.Name
	// annotate the transaction with the SQL text of a statement that it ran.
	annotate(ctx context.Context, sql string) error
	commit(ctx context.Context) error
	rollback(ctx context.Context) error
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

func (c coreDB) read(ctx context.Context) (readTx, error) {
	r, err := c.d.Read(ctx)
	if err != nil {
		return nil, err
	}
	return coreReadTx{r: r}, nil
}

// fromCore is err of a call of the core, with a refusal of the core as an internal error, and every
// other error as it is. The engine checks for each refusal before it calls the core, and gives
// SQLite's error for it, so one that the core still gives is a fault of the engine or of the pass
// after the parser; so is a call on a transaction that has ended.
func fromCore(err error) error {
	for _, r := range []error{core.ErrTableExists, core.ErrNoTable, core.ErrReservedName, core.ErrInvalid, core.ErrTxDone} {
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

func (c coreReadTx) get(ctx context.Context, name string, key []*s4qlpb.Value) ([]*s4qlpb.Value, error) {
	row, err := c.r.Get(ctx, name, key)
	return row, fromCore(err)
}

func (c coreReadTx) position() format.Name {
	return c.r.Position()
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

func (c coreTx) position() format.Name {
	return c.tx.Position()
}

func (c coreTx) annotate(ctx context.Context, sql string) error {
	return fromCore(c.tx.Annotate(ctx, sql, nil))
}

// commit gives the error of the core as it is: [core.ErrInvalid] there is a commit time that the
// commit object cannot hold, which comes from the clock and not from a call that the engine checks.
func (c coreTx) commit(ctx context.Context) error {
	return c.tx.Commit(ctx)
}

func (c coreTx) rollback(ctx context.Context) error {
	return fromCore(c.tx.Rollback(ctx))
}
