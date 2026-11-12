package core

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
)

// replayed is what [replay] applied from one commit object.
type replayed struct {
	// changes is the number of changes: each table created or dropped, each row upserted, and each
	// row deleted.
	changes int
	// annotations that the commit object holds, which replay does not use.
	annotations int
	// bytes of the commit object that were read.
	bytes int64
}

// countingReader counts the bytes that it reads.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// replay the commit object with the given name, read from body, into the cache through c: apply
// its changes in one transaction of the cache, record its name as the position of the cache in the
// same transaction, and commit that transaction only after the reader has reported the end of the
// commit object, which is when its bytes are known to match its name. It applies results, and runs
// no SQL but its own.
//
// The commit object must be the one after the position of the cache: its parent is that position.
// It calls tick, if it is not nil, before each change, and stops with its error.
//
// Its error wraps [format.ErrNewerVersion], [format.ErrMalformed], or [format.ErrNameMismatch] as
// the reader reports them, and [ErrLogBroken] for a commit object that the reader accepts and that
// does not fit the cache: another parent, a table that is there already or is not there, a column
// position outside the table, a row or a key that does not fit the table, a value of the wrong
// storage class, and a row to change or delete that is not there. The cache is right up to the
// parent, so such a commit object is wrong, and applying a part of it would leave the cache in a
// state that the log never had. An error of body, of tick, or of the cache is passed on as it is.
// After any error, the cache is as it was.
func replay(c *conn, name format.Name, body io.Reader, tick func() error, bugs bugs) (_ replayed, err error) {
	counted := &countingReader{r: body}
	r := format.NewCommitReader(counted, name)
	var done replayed
	// ticked is whether tick ended the replay, and not the commit object.
	var ticked bool

	if err := c.c.Exec("begin immediate"); err != nil {
		return done, err
	}
	defer func() {
		done.bytes = counted.n
		if err == nil {
			return
		}
		c.forget()
		if bugs.replayCommitsEarly && !ticked && isFromBody(err) {
			// The planted bug: what was applied before the commit object broke off is kept.
			_ = c.c.Exec("commit")
			return
		}
		_ = c.c.Exec("rollback")
	}()

	position, err := c.position()
	if err != nil {
		return done, err
	}
	header, err := r.Header()
	if err != nil {
		return done, err
	}
	parent, err := format.NameFromBytes(header.GetParent())
	if err != nil {
		return done, err
	}
	if parent != position {
		return done, authentic(r, brokenf("the commit object %v has the parent %v, and the cache is at %v", name, parent, position))
	}

	for {
		change, annotation, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return done, err
		}
		if annotation != nil {
			done.annotations++
			continue
		}
		if tick != nil {
			if err := tick(); err != nil {
				ticked = true
				return done, err
			}
		}
		n, err := apply(c, change)
		if err != nil {
			if classify(err) != kindOther || errors.Is(err, ErrCacheVersion) {
				return done, err
			}
			if !errors.Is(err, ErrLogBroken) {
				err = fmt.Errorf("%w: %w", ErrLogBroken, err)
			}
			return done, authentic(r, fmt.Errorf("commit object %v: %w", name, err))
		}
		done.changes += n
	}

	if err := c.setPosition(name); err != nil {
		return done, err
	}
	c.forget()
	if err := c.c.Exec("commit"); err != nil {
		return done, err
	}
	return done, nil
}

// isFromBody reports whether err is an error of reading a commit object, and none of the cache or
// of the checks of [replay].
func isFromBody(err error) bool {
	return classify(err) == kindOther && !isSQLite(err) && !errors.Is(err, ErrLogBroken) && !errors.Is(err, ErrCacheVersion)
}

// authentic returns refusal if the bytes of the commit object that r reads match its name, which r
// knows at the end. Until then nothing that r handed out can be trusted, so a commit object that
// was damaged on its way must not be refused for what it seems to hold. If r fails before its end,
// authentic returns that error.
func authentic(r *format.CommitReader, refusal error) error {
	for {
		_, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			return refusal
		}
		if err != nil {
			return err
		}
	}
}

// apply one change to the cache, and return how many tables and rows it changed.
func apply(c *conn, change *s4qlpb.Change) (int, error) {
	switch change.WhichKind() {
	case s4qlpb.Change_CreateTable_case:
		ct := change.GetCreateTable()
		if reserved(ct.GetTable()) {
			return 0, brokenf("it creates the table %q, whose name is reserved", ct.GetTable())
		}
		existing, err := c.lookUp(ct.GetTable())
		if err != nil {
			return 0, err
		}
		if existing != nil {
			return 0, brokenf("it creates the table %q, and the table %q exists", ct.GetTable(), existing.name)
		}
		return 1, c.create(newTable(ct))

	case s4qlpb.Change_DropTable_case:
		t, err := existingTable(c, change.GetDropTable().GetTable(), "drops")
		if err != nil {
			return 0, err
		}
		return 1, c.drop(t)

	case s4qlpb.Change_UpsertRows_case:
		return applyUpsertRows(c, change.GetUpsertRows())

	case s4qlpb.Change_DeleteRows_case:
		dr := change.GetDeleteRows()
		t, err := existingTable(c, dr.GetTable(), "deletes rows of")
		if err != nil {
			return 0, err
		}
		for i, key := range dr.GetPrimaryKeys() {
			if err := t.checkKey(key.GetValues()); err != nil {
				return 0, brokenf("key %v that it deletes: %v", i, err)
			}
			found, err := c.delete(t, key.GetValues())
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, brokenf("it deletes a row of the table %q that is not there, key %v of its batch", t.name, i)
			}
		}
		return len(dr.GetPrimaryKeys()), nil
	}
	// The reader hands out no change of another kind.
	return 0, brokenf("it holds a change of kind %v", change.WhichKind())
}

// existingTable with the given name, which the commit object does something to, as doing says.
func existingTable(c *conn, name, doing string) (*table, error) {
	t, err := c.lookUp(name)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, brokenf("it %v the table %q, which does not exist", doing, name)
	}
	return t, nil
}

// applyUpsertRows to the cache. If the batch lists every column of the table, each row is
// inserted, or replaces the row with the same primary key. If it lists only some, each row is
// there already, and only the listed columns change.
func applyUpsertRows(c *conn, ur *s4qlpb.UpsertRows) (int, error) {
	t, err := existingTable(c, ur.GetTable(), "upserts rows of")
	if err != nil {
		return 0, err
	}
	positions := make([]int, len(ur.GetColumns()))
	// parameter of each column of the table that the batch lists, from 1, or 0.
	parameter := make([]int, len(t.columns))
	for i, position := range ur.GetColumns() {
		if position >= uint32(len(t.columns)) {
			return 0, brokenf("it upserts column %v of the table %q, which has %v columns", position, t.name, len(t.columns))
		}
		positions[i] = int(position)
		parameter[position] = i + 1
	}

	// The reader refuses a column that is listed twice, so a batch with as many columns as the
	// table lists them all.
	full := len(positions) == len(t.columns)
	var sql string
	if full {
		sql = fmt.Sprint("insert or replace into main.", quote(t.name), " (", t.columnList(positions), ") values (", placeholders(len(positions)), ")")
	} else {
		var sets, conditions []string
		for position, c := range t.columns {
			switch {
			case t.inKey[position] && parameter[position] == 0:
				return 0, brokenf("it changes some columns of rows of the table %q without the primary-key column %q", t.name, c.name)
			case t.inKey[position]:
				conditions = append(conditions, fmt.Sprint(quote(c.name), " = ?", parameter[position]))
			case parameter[position] != 0:
				sets = append(sets, fmt.Sprint(quote(c.name), " = ?", parameter[position]))
			}
		}
		if len(sets) == 0 {
			// Only the primary key is listed, so nothing changes, and the row must still be there.
			sql = fmt.Sprint("select 1 from main.", quote(t.name), " where ", strings.Join(conditions, " and "))
		} else {
			sql = fmt.Sprint("update main.", quote(t.name), " set ", strings.Join(sets, ", "), " where ", strings.Join(conditions, " and "), " returning 1")
		}
	}

	for i, row := range ur.GetRows() {
		for j, v := range row.GetValues() {
			if err := t.checkValue(positions[j], v); err != nil {
				return 0, brokenf("row %v that it upserts: %v", i, err)
			}
		}
		n, err := c.count(sql, values(row.GetValues())...)
		if err != nil {
			return 0, err
		}
		if !full && n != 1 {
			return 0, brokenf("it changes some columns of a row of the table %q that is not there, row %v of its batch", t.name, i)
		}
	}
	return len(ur.GetRows()), nil
}
