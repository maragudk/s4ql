package core_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/store"
)

// handLog writes a log into the store of a world by hand: commit objects that the core did not
// write, and the manifest that names the last of them.
type handLog struct {
	t    *testing.T
	w    *world
	s    store.Store
	head format.Name
}

func newHandLog(t *testing.T, w *world) *handLog {
	return &handLog{t: t, w: w, s: w.client()}
}

// put the bytes of a commit object under the given name, whatever the bytes are.
func (l *handLog) put(name format.Name, data []byte) {
	l.t.Helper()
	_, err := l.s.Create(l.t.Context(), commitKey("db", name), bytes.NewReader(data), nil)
	is.NotError(l.t, err)
}

// commit a commit object on top of the log, which write writes with the writer of the format.
func (l *handLog) commit(write func(cw *format.CommitWriter)) format.Name {
	l.t.Helper()
	var b bytes.Buffer
	cw, err := format.NewCommitWriter(&b, l.head, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	is.NotError(l.t, err)
	write(cw)
	name, err := cw.Close()
	is.NotError(l.t, err)
	l.put(name, b.Bytes())
	l.head = name
	return name
}

// raw commits a commit object on top of the log with exactly the given records, which the writer
// of the format may refuse to write. A nil header means the header of version 0 on top of the log.
func (l *handLog) raw(header *s4qlpb.Header, changes []*s4qlpb.Change, annotations []*s4qlpb.Annotation, extra ...byte) format.Name {
	l.t.Helper()
	if header == nil {
		header = s4qlpb.Header_builder{
			Version:    new(uint32(0)),
			Parent:     l.head[:],
			CommitTime: timestamppb.New(time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)),
		}.Build()
	}
	data, err := proto.MarshalOptions{Deterministic: true}.Marshal(s4qlpb.CommitObject_builder{Header: header, Changes: changes, Annotations: annotations}.Build())
	is.NotError(l.t, err)
	data = append(data, extra...)
	name := format.Name(sha256.Sum256(data))
	l.put(name, data)
	l.head = name
	return name
}

// publish the manifest that names the head of the log, without a lease entry.
func (l *handLog) publish() {
	l.t.Helper()
	l.publishBytes(func() []byte {
		data, err := format.Manifest{Head: l.head}.Marshal()
		is.NotError(l.t, err)
		return data
	}())
}

// publishBytes as the manifest.
func (l *handLog) publishBytes(data []byte) {
	l.t.Helper()
	_, version, err := l.s.Get(l.t.Context(), "db/manifest.json")
	if errors.Is(err, store.ErrNotFound) {
		_, err = l.s.Create(l.t.Context(), "db/manifest.json", bytes.NewReader(data), nil)
	} else if err == nil {
		_, err = l.s.Swap(l.t.Context(), "db/manifest.json", version, bytes.NewReader(data), nil)
	}
	is.NotError(l.t, err)
}

// base writes the two commit objects that the tests of refusals start from: the table t1 with the
// rows 1,10 and 2,20, and the table t2 with the row 1,1,100.
func (l *handLog) base() {
	l.t.Helper()
	l.commit(func(cw *format.CommitWriter) {
		is.NotError(l.t, cw.CreateTable(tableOf("t1", 1, "k", "v")))
		is.NotError(l.t, cw.UpsertRow("t1", []uint32{0, 1}, row(1, 10)))
		is.NotError(l.t, cw.UpsertRow("t1", []uint32{0, 1}, row(2, 20)))
	})
	l.commit(func(cw *format.CommitWriter) {
		is.NotError(l.t, cw.CreateTable(tableOf("t2", 2, "a", "b", "c")))
		is.NotError(l.t, cw.UpsertRow("t2", []uint32{0, 1, 2}, row(1, 1, 100)))
	})
}

// manyColumns are n names of columns.
func manyColumns(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprint("c", i)
	}
	return names
}

const baseState = "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,100"

// change builders for commit objects that the writer of the format does not write.
func upsertRows(table string, columns []uint32, rows ...[]*s4qlpb.Value) *s4qlpb.Change {
	ur := s4qlpb.UpsertRows_builder{Table: new(table), Columns: columns}
	for _, r := range rows {
		ur.Rows = append(ur.Rows, s4qlpb.Row_builder{Values: r}.Build())
	}
	return s4qlpb.Change_builder{UpsertRows: ur.Build()}.Build()
}

func deleteRows(table string, keys ...[]*s4qlpb.Value) *s4qlpb.Change {
	dr := s4qlpb.DeleteRows_builder{Table: new(table)}
	for _, k := range keys {
		dr.PrimaryKeys = append(dr.PrimaryKeys, s4qlpb.PrimaryKey_builder{Values: k}.Build())
	}
	return s4qlpb.Change_builder{DeleteRows: dr.Build()}.Build()
}

func createTable(ct *s4qlpb.CreateTable) *s4qlpb.Change {
	return s4qlpb.Change_builder{CreateTable: ct}.Build()
}

func dropTableChange(table string) *s4qlpb.Change {
	return s4qlpb.Change_builder{DropTable: s4qlpb.DropTable_builder{Table: new(table)}.Build()}.Build()
}

func TestReplay(t *testing.T) {
	t.Run("should apply every kind of change of a log that was written by hand, also what the core never writes", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		l.base()
		// Rows whose columns are listed in another order than the table's.
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t1", []uint32{1, 0}, row(30, 3)))
			is.NotError(t, cw.UpsertRow("t1", []uint32{1, 0}, row(11, 1)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{2, 0, 1}, row(200, 2, 2)))
			// A column that takes NULL.
			is.NotError(t, cw.CreateTable(nullableTable(tableOf("t3", 1, "k", "x", "y"), 2)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{2, 0, 1}, row(nil, 1, 1)))
			is.NotError(t, cw.Annotate(s4qlpb.Annotation_builder{Statement: s4qlpb.Statement_builder{Sql: new("insert")}.Build()}.Build()))
		})
		// Rows that list only some columns: only those change, in rows that are there.
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t2", []uint32{2, 1, 0}, row(7, 1, 1)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{0, 1, 2}, row(1, 2, 120)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{0, 1}, row(2, 2)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{1, 0}, row(1, 1)))
			is.NotError(t, cw.DeleteRow("t1", row(2)))
		})
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t2", []uint32{1, 2, 0}, row(2, 220, 2)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{0, 1, 2}, row(1, 1, 110)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{0, 1, 2}, row(2, 2, 2)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{2, 0}, row(5, 1)))
			is.NotError(t, cw.UpsertRow("t3", []uint32{2, 0}, row(nil, 2)))
		})
		// Names in another case than the table was created with, a table that is dropped and created
		// again, an annotation of a kind that this version does not know, and a field that it does
		// not know inside an annotation.
		unknownAnnotation := &s4qlpb.Annotation{}
		unknownAnnotation.ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
		l.raw(nil, []*s4qlpb.Change{
			upsertRows("T1", []uint32{0, 1}, row(5, 50)),
			deleteRows("T1", row(3)),
			dropTableChange("T2"),
			createTable(tableOf("T2", 1, "x")),
			upsertRows("t2", []uint32{0}, row(9)),
		}, []*s4qlpb.Annotation{unknownAnnotation})
		l.publish()

		d := w.serve(t, "a", core.Options{})
		is.Equal(t, "T2(x*0): 9\nt1(k*0,v): 1,11 | 5,50\nt3(k*0,x,y?): 1,1,5 | 2,2,null", read(t, d))
		position, _, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, l.head, position)

		// The core goes on from there.
		transact(t, d, upsert("t1", 6, 60), remove("T2", true, 9))
		is.Equal(t, "T2(x*0): \nt1(k*0,v): 1,11 | 5,50 | 6,60\nt3(k*0,x,y?): 1,1,5 | 2,2,null", read(t, d))
	})

	t.Run("should apply the intermediate states of the rows that list only some columns", func(t *testing.T) {
		w := newWorld(t)
		l := newHandLog(t, w)
		l.base()
		l.commit(func(cw *format.CommitWriter) {
			is.NotError(t, cw.UpsertRow("t2", []uint32{2, 1, 0}, row(7, 1, 1)))
			is.NotError(t, cw.UpsertRow("t2", []uint32{0, 1}, row(1, 1)))
		})
		l.publish()
		d := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0,v): 1,10 | 2,20\nt2(a*0,b*1,c): 1,1,7", read(t, d))
	})

	// Each of these commit objects is one that a reader of the format accepts, on top of the base,
	// and that does not fit the tables and rows that the base leaves.
	for name, test := range map[string]struct {
		changes []*s4qlpb.Change
		// reason in the error, so that the case fails when the check that it names is gone and
		// another refuses the commit object instead.
		reason string
	}{
		"creates a table that exists":                                     {changes: []*s4qlpb.Change{createTable(tableOf("t1", 1, "k"))}, reason: `it creates the table "t1", and the table "t1" exists`},
		"creates a table that exists under a name in another case":        {changes: []*s4qlpb.Change{createTable(tableOf("T1", 1, "k"))}, reason: `it creates the table "T1", and the table "t1" exists`},
		"creates a table whose name is reserved":                          {changes: []*s4qlpb.Change{createTable(tableOf("s4ql_x", 1, "k"))}, reason: `it creates the table "s4ql_x", whose name is reserved`},
		"creates a table with more columns than the cache can hold":       {changes: []*s4qlpb.Change{createTable(tableOf("t3", 1, manyColumns(2001)...))}, reason: `sqlite: too many columns on t3`},
		"creates a table with a longer primary key than a write takes":    {changes: []*s4qlpb.Change{createTable(tableOf("t3", 251, manyColumns(251)...))}, reason: `it creates the table "t3" with a primary key of 251 columns, where the limit is 250`},
		"drops a table that does not exist":                               {changes: []*s4qlpb.Change{dropTableChange("t3")}, reason: `it drops the table "t3", which does not exist`},
		"drops the cache's own table":                                     {changes: []*s4qlpb.Change{dropTableChange("s4ql_cache")}, reason: `it drops the table "s4ql_cache", which does not exist`},
		"upserts rows of a table that does not exist":                     {changes: []*s4qlpb.Change{upsertRows("t3", []uint32{0}, row(1))}, reason: `it upserts rows of the table "t3", which does not exist`},
		"upserts a column position outside the table's columns":           {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 2}, row(1, 1))}, reason: `it upserts column 2 of the table "t1", which has 2 columns`},
		"upserts more columns than the table has":                         {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1, 2}, row(1, 1, 1))}, reason: `it upserts column 2 of the table "t1", which has 2 columns`},
		"changes some columns of rows without the primary-key columns":    {changes: []*s4qlpb.Change{upsertRows("t2", []uint32{0, 2}, row(1, 5))}, reason: `it changes some columns of rows of the table "t2" without the primary-key column "b"`},
		"changes some columns of a row that is not there":                 {changes: []*s4qlpb.Change{upsertRows("t2", []uint32{0, 1}, row(1, 1)), upsertRows("t2", []uint32{0, 1}, row(5, 5))}, reason: `it changes some columns of a row of the table "t2" that is not there, row 0 of its batch`},
		"changes a column of a row that is not there":                     {changes: []*s4qlpb.Change{createTable(tableOf("t3", 1, "k", "x", "y")), upsertRows("t3", []uint32{0, 1, 2}, row(1, 1, 1)), upsertRows("t3", []uint32{2, 0}, row(5, 1), row(5, 2))}, reason: `it changes some columns of a row of the table "t3" that is not there, row 1 of its batch`},
		"upserts text into an integer column":                             {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(3, "30"))}, reason: `row 0 that it upserts: invalid: a value of kind text in the column "v" of the table "t1", whose type is TYPE_INT`},
		"upserts a real into an integer column":                           {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(3, 30.0))}, reason: `row 0 that it upserts: invalid: a value of kind real in the column "v" of the table "t1", whose type is TYPE_INT`},
		"upserts a blob into an integer column":                           {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(3, []byte{30}))}, reason: `row 0 that it upserts: invalid: a value of kind blob in the column "v" of the table "t1", whose type is TYPE_INT`},
		"upserts text that looks like an integer into the primary key":    {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row("3", 30))}, reason: `row 0 that it upserts: invalid: a value of kind text in the column "k" of the table "t1", whose type is TYPE_INT`},
		"upserts NULL into the primary key":                               {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(nil, 30))}, reason: `row 0 that it upserts: invalid: NULL in the primary-key column "k" of the table "t1"`},
		"upserts NULL into a column that does not take NULL":              {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(3, nil))}, reason: `row 0 that it upserts: invalid: NULL in the column "v" of the table "t1", which does not take NULL`},
		"changes a column that does not take NULL to NULL":                {changes: []*s4qlpb.Change{createTable(tableOf("t3", 1, "k", "x", "y")), upsertRows("t3", []uint32{0, 1, 2}, row(1, 1, 1)), upsertRows("t3", []uint32{2, 0}, row(nil, 1))}, reason: `row 0 that it upserts: invalid: NULL in the column "y" of the table "t3", which does not take NULL`},
		"upserts NULL into the primary key of a table that it creates":    {changes: []*s4qlpb.Change{createTable(nullableTable(tableOf("t3", 1, "k", "v"), 1)), upsertRows("t3", []uint32{0, 1}, row(nil, nil))}, reason: `row 0 that it upserts: invalid: NULL in the primary-key column "k" of the table "t3"`},
		"changes a column to text in a row that is there":                 {changes: []*s4qlpb.Change{createTable(tableOf("t3", 1, "k", "x", "y")), upsertRows("t3", []uint32{0, 1, 2}, row(1, 1, 1)), upsertRows("t3", []uint32{2, 0}, row("7", 1))}, reason: `row 0 that it upserts: invalid: a value of kind text in the column "y" of the table "t3", whose type is TYPE_INT`},
		"deletes rows of a table that does not exist":                     {changes: []*s4qlpb.Change{deleteRows("t3", row(1))}, reason: `it deletes rows of the table "t3", which does not exist`},
		"deletes a row that is not there":                                 {changes: []*s4qlpb.Change{deleteRows("t1", row(1), row(3))}, reason: `it deletes a row of the table "t1" that is not there, key 1 of its batch`},
		"deletes with a key of more values than the primary key has":      {changes: []*s4qlpb.Change{deleteRows("t1", row(1, 10))}, reason: `key 0 that it deletes: invalid: a key of 2 values for the table "t1", whose primary key has 1 columns`},
		"deletes with a key of fewer values than the primary key has":     {changes: []*s4qlpb.Change{deleteRows("t2", row(1))}, reason: `key 0 that it deletes: invalid: a key of 1 values for the table "t2", whose primary key has 2 columns`},
		"deletes with a key of text":                                      {changes: []*s4qlpb.Change{deleteRows("t1", row("1"))}, reason: `key 0 that it deletes: invalid: a value of kind text in the column "k" of the table "t1", whose type is TYPE_INT`},
		"is right at first and wrong later, so that a part of it applies": {changes: []*s4qlpb.Change{upsertRows("t1", []uint32{0, 1}, row(3, 30)), dropTableChange("t2"), deleteRows("t1", row(9))}, reason: `it deletes a row of the table "t1" that is not there, key 0 of its batch`},
	} {
		t.Run("should stop, apply nothing, and release the lease for a commit object that "+name, func(t *testing.T) {
			w := newWorld(t)
			l := newHandLog(t, w)
			l.base()
			base := l.head
			l.raw(nil, test.changes, nil)
			l.publish()

			d := w.open(t, "a", core.Options{})
			err := d.Wait(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.Error(t, core.ErrLogBroken, err)
			is.True(t, !errors.Is(err, core.ErrLocal))
			is.True(t, strings.Contains(err.Error(), "the log is broken: "+test.reason), err.Error())

			// The cache holds the base, and nothing of the commit object.
			position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, base, position)
			is.Equal(t, baseState, show(state))
			is.NotError(t, d.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
			is.Equal(t, l.head, w.manifest(t).Head)
		})
	}

	// What a reader of the format refuses, and what the store lacks.
	for _, test := range []struct {
		name  string
		write func(l *handLog)
		cause error
	}{
		{
			name: "that needs a newer version of s4ql by its header",
			write: func(l *handLog) {
				l.raw(s4qlpb.Header_builder{Version: new(uint32(1)), Parent: l.head[:], CommitTime: timestamppb.Now()}.Build(), nil, nil)
			},
			cause: format.ErrNewerVersion,
		},
		{
			name: "that holds a change with a field that this version does not know",
			write: func(l *handLog) {
				change := dropTableChange("t1")
				change.GetDropTable().ProtoReflect().SetUnknown(protowire.AppendVarint(protowire.AppendTag(nil, 99, protowire.VarintType), 1))
				l.raw(nil, []*s4qlpb.Change{change}, nil)
			},
			cause: format.ErrNewerVersion,
		},
		{
			name: "that holds a record that this version does not know",
			write: func(l *handLog) {
				l.raw(nil, nil, nil, protowire.AppendBytes(protowire.AppendTag(nil, 99, protowire.BytesType), []byte("x"))...)
			},
			cause: format.ErrNewerVersion,
		},
		{
			name: "that is malformed, because it holds a change of no kind",
			write: func(l *handLog) {
				l.raw(nil, []*s4qlpb.Change{{}}, nil)
			},
			cause: format.ErrMalformed,
		},
		{
			name: "that is malformed, because it deletes with a key that is NULL",
			write: func(l *handLog) {
				l.raw(nil, []*s4qlpb.Change{deleteRows("t1", row(nil))}, nil)
			},
			cause: format.ErrMalformed,
		},
		{
			name: "that is malformed, because it creates a table whose primary-key column takes NULL",
			write: func(l *handLog) {
				l.raw(nil, []*s4qlpb.Change{createTable(nullableTable(tableOf("t3", 1, "k", "v"), 0))}, nil)
			},
			cause: format.ErrMalformed,
		},
		{
			name: "that is malformed, because it breaks off",
			write: func(l *handLog) {
				l.raw(nil, []*s4qlpb.Change{dropTableChange("t1")}, nil, 0x12, 0x7f)
			},
			cause: format.ErrMalformed,
		},
		{
			name: "that is malformed, because it has no header",
			write: func(l *handLog) {
				data, err := proto.Marshal(s4qlpb.CommitObject_builder{Changes: []*s4qlpb.Change{dropTableChange("t1")}}.Build())
				is.NotError(l.t, err)
				name := format.Name(sha256.Sum256(data))
				l.put(name, data)
				l.head = name
			},
			cause: format.ErrMalformed,
		},
		{
			name: "whose bytes do not match its name",
			write: func(l *handLog) {
				var b bytes.Buffer
				cw, err := format.NewCommitWriter(&b, l.head, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
				is.NotError(l.t, err)
				is.NotError(l.t, cw.DropTable("t1"))
				name, err := cw.Close()
				is.NotError(l.t, err)
				data := b.Bytes()
				data[len(data)-1] ^= 1
				l.put(name, data)
				l.head = name
			},
			cause: format.ErrNameMismatch,
		},
		{
			name: "that the store does not have",
			write: func(l *handLog) {
				l.head = format.Name(sha256.Sum256([]byte("nothing has this name")))
			},
			cause: core.ErrLogBroken,
		},
		{
			name: "whose parent the store does not have",
			write: func(l *handLog) {
				l.head = format.Name(sha256.Sum256([]byte("nothing has this name")))
				l.commit(func(cw *format.CommitWriter) {
					is.NotError(l.t, cw.DropTable("t1"))
				})
			},
			cause: core.ErrLogBroken,
		},
	} {
		t.Run("should stop, skip nothing, and release the lease for a commit object "+test.name, func(t *testing.T) {
			w := newWorld(t)
			l := newHandLog(t, w)
			l.base()
			l.publish()
			// The instance has the base in its cache, and holds the lease when it meets the commit
			// object.
			first := w.serve(t, "a", core.Options{})
			is.Equal(t, baseState, read(t, first))
			is.NotError(t, first.Close(t.Context()))
			base := l.head

			test.write(l)
			// A commit object on top of the one that cannot be read, which must not be applied.
			l.commit(func(cw *format.CommitWriter) {
				is.NotError(t, cw.CreateTable(tableOf("t9", 1, "k")))
			})
			l.publish()

			d := w.open(t, "a", core.Options{})
			err := d.Wait(t.Context())
			is.Error(t, core.ErrStopped, err)
			is.Error(t, test.cause, err)
			is.Error(t, test.cause, d.Err())
			is.Error(t, core.ErrStopped, readErr(t, d))
			is.Error(t, core.ErrStopped, beginErr(t, d))

			position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, base, position)
			is.Equal(t, baseState, show(state))
			is.NotError(t, d.Close(t.Context()))
			is.True(t, w.manifest(t).Lease == nil, "the lease is not released")
		})
	}

	for name, manifest := range map[string]string{
		"that needs a newer version of s4ql": `{"version": 1, "branches": {"main": {"head": "0000000000000000000000000000000000000000000000000000000000000000"}}}`,
		"that is malformed":                  `{"version": 0, "branches": {}}`,
		"that is no JSON":                    `not a manifest`,
	} {
		t.Run("should stop for a manifest "+name+", and leave it as it is", func(t *testing.T) {
			w := newWorld(t)
			l := newHandLog(t, w)
			l.publishBytes([]byte(manifest))

			d := w.open(t, "a", core.Options{})
			err := d.Wait(t.Context())
			is.Error(t, core.ErrStopped, err)
			if strings.Contains(name, "newer") {
				is.Error(t, format.ErrNewerVersion, err)
			} else {
				is.Error(t, format.ErrMalformed, err)
			}
			is.NotError(t, d.Close(t.Context()))
			data, _, _ := w.store.Object("db/manifest.json")
			is.Equal(t, manifest, string(data))
		})
	}

	t.Run("should refuse a cache whose position is not in the log, loudly, and leave the cache as it is", func(t *testing.T) {
		w := newWorld(t)
		// The cache of another database: a log of its own, under another prefix.
		other, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, "a.db"), "other", core.Options{})
		is.NotError(t, err)
		is.NotError(t, other.Wait(t.Context()))
		transact(t, other, create("t7", 1, "k"), upsert("t7", 7))
		is.NotError(t, other.Close(t.Context()))
		before, _, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)

		l := newHandLog(t, w)
		l.base()
		l.publish()

		d := w.open(t, "a", core.Options{})
		err = d.Wait(t.Context())
		is.Error(t, core.ErrStopped, err)
		is.Error(t, core.ErrCacheNotInLog, err)
		is.True(t, !errors.Is(err, core.ErrLogBroken) && !errors.Is(err, core.ErrLocal))
		is.NotError(t, d.Close(t.Context()))

		position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, before, position)
		is.Equal(t, "t7(k*0): 7", show(state))
		is.True(t, w.manifest(t).Lease == nil, "the lease is not released")

		// And every time it is opened again.
		again := w.open(t, "a", core.Options{})
		is.Error(t, core.ErrCacheNotInLog, again.Wait(t.Context()))
	})

	t.Run("should refuse a cache with a position when the log is empty", func(t *testing.T) {
		w := newWorld(t)
		other, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, "a.db"), "other", core.Options{})
		is.NotError(t, err)
		is.NotError(t, other.Wait(t.Context()))
		transact(t, other, create("t7", 1, "k"))
		is.NotError(t, other.Close(t.Context()))

		d := w.open(t, "a", core.Options{})
		is.Error(t, core.ErrCacheNotInLog, d.Wait(t.Context()))
	})

	t.Run("should download a commit object again whose bytes were damaged on their way, and stop only if the second download is damaged too", func(t *testing.T) {
		for _, test := range []struct {
			damaged int
			stops   bool
		}{
			{damaged: 1, stops: false},
			{damaged: 2, stops: true},
		} {
			bubble(t, func(t *testing.T, w *world) {
				l := newHandLog(t, w)
				l.base()
				l.publish()
				s := newScriptStore(w.client())
				// The walk back through the log reads both commit objects once. The download of the
				// first one to apply is damaged then.
				s.on("get", "commits/", pass, 2)
				s.on("get", "commits/", corrupt, test.damaged)
				d := w.openOn(t, "a", s, core.Options{})
				err := d.Wait(t.Context())
				if test.stops {
					is.Error(t, format.ErrNameMismatch, err)
					is.Equal(t, 4, s.count("get", "commits/"))
					position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
					is.NotError(t, err)
					is.Equal(t, format.EmptyLog, position)
					is.Equal(t, "", show(state))
					return
				}
				is.NotError(t, err)
				is.Equal(t, baseState, read(t, d))
				is.Equal(t, 5, s.count("get", "commits/"))
			})
		}
	})

	t.Run("should not refuse a commit object for what damaged bytes seem to say, before its bytes are known to match its name", func(t *testing.T) {
		for i, find := range []string{"t1", "t2"} {
			bubble(t, func(t *testing.T, w *world) {
				l := newHandLog(t, w)
				l.base()
				l.publish()
				s := newScriptStore(w.client())
				// The walk back through the log reads both commit objects once. Then the name of the
				// table that a commit object creates is damaged in its download, so that the commit
				// object seems to upsert rows into a table that is not there.
				s.on("get", "commits/", pass, 2+i)
				s.damage("commits/", find, 1)
				d := w.openOn(t, "a", s, core.Options{})
				is.NotError(t, d.Wait(t.Context()))
				is.Equal(t, baseState, read(t, d))
			})
		}
	})

	t.Run("should not follow a parent that damaged bytes seem to name, when it walks back through the log", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			l.base()
			l.publish()
			data, _, _ := w.store.Object(commitKey("db", l.head))
			parent, _, err := changes(l.head, data)
			is.NotError(t, err)

			// The first download is the one of the head, and the name of its parent is damaged in it.
			s := newScriptStore(w.client())
			s.damage("commits/", string(parent[:8]), 1)
			d := w.openOn(t, "a", s, core.Options{})
			is.NotError(t, d.Wait(t.Context()))
			is.Equal(t, baseState, read(t, d))
		})
	})

	t.Run("should keep nothing of a commit object that breaks off, and apply it when it comes whole", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			l.commit(func(cw *format.CommitWriter) {
				is.NotError(t, cw.CreateTable(tableOf("t1", 1, "k", "v")))
				for i := range 1000 {
					is.NotError(t, cw.UpsertRow("t1", []uint32{0, 1}, row(i, i)))
				}
				is.NotError(t, cw.CreateTable(tableOf("t2", 1, "k")))
			})
			l.publish()
			s := newScriptStore(w.client())
			// The walk back through the log reads the commit object once, whole. Every download to
			// apply it breaks off in the middle of its rows.
			s.on("get", "commits/", pass, 1)
			s.on("get", "commits/", breakOff, -1)
			d := w.openOn(t, "a", s, core.Options{})

			time.Sleep(time.Minute)
			is.True(t, !serves(t, d))
			is.NotError(t, d.Err())
			is.True(t, s.count("get", "commits/") > 3, "the download was not tried again")
			position, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			is.Equal(t, format.EmptyLog, position)
			is.Equal(t, "", show(state))

			s.clear()
			is.NotError(t, d.Wait(t.Context()))
			r, err := startRead(t, d)
			is.NotError(t, err)
			var rows int
			for _, err := range r.Scan(t.Context(), "t1") {
				is.NotError(t, err)
				rows++
			}
			is.Equal(t, 1000, rows)
			is.NotError(t, r.Close(t.Context()))
		})
	})

	t.Run("should try a download again where it failed, and not walk back through the log from its head again", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			a := w.serve(t, "a", core.Options{})
			transact(t, a, create("t1", 1, "k"))
			for i := range 4 {
				transact(t, a, upsert("t1", i))
			}
			is.NotError(t, a.Close(t.Context()))

			// Of the five downloads of the walk and the five to apply, the fourth and the eighth fail.
			s := newScriptStore(w.client())
			s.on("get", "commits/", pass, 3)
			s.on("get", "commits/", drop, 1)
			s.on("get", "commits/", pass, 3)
			s.on("get", "commits/", drop, 1)
			b := w.openOn(t, "b", s, core.Options{})
			is.NotError(t, b.Wait(t.Context()))
			is.Equal(t, "t1(k*0): 0 | 1 | 2 | 3", read(t, b))
			is.Equal(t, 12, s.count("get", "commits/"))
		})
	})

	t.Run("should give a download a timeout, and try it again", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			l := newHandLog(t, w)
			l.base()
			l.publish()
			// Without a timeout the instance would wait for ever, and the bubble would report a
			// deadlock.
			s := newScriptStore(w.client())
			s.on("get", "commits/", hang, 1)
			s.on("get", "commits/", pass, 2)
			s.on("get", "commits/", hang, 1)
			d := w.openOn(t, "a", s, core.Options{ObjectTimeout: 30 * time.Second})
			start := time.Now()
			is.NotError(t, d.Wait(t.Context()))
			is.True(t, time.Since(start) >= time.Minute && time.Since(start) < 2*time.Minute, fmt.Sprint(time.Since(start)))
			is.Equal(t, baseState, read(t, d))
		})
	})

	t.Run("should catch up from the position of its cache, and not from the start", func(t *testing.T) {
		bubble(t, func(t *testing.T, w *world) {
			a := w.serve(t, "a", core.Options{})
			transact(t, a, create("t1", 1, "k"))
			is.NotError(t, a.Close(t.Context()))
			b := w.serve(t, "b", core.Options{})
			for i := range 5 {
				transact(t, b, upsert("t1", i))
			}
			is.NotError(t, b.Close(t.Context()))

			s := newScriptStore(w.client())
			events := &recorder{}
			again := w.openOn(t, "a", s, core.Options{Logger: events.logger()})
			is.NotError(t, again.Wait(t.Context()))
			is.Equal(t, "t1(k*0): 0 | 1 | 2 | 3 | 4", read(t, again))
			// Each of the five commit objects that the cache lacked is read to find it and read to
			// apply it, and the one that it had is not read.
			is.Equal(t, 10, s.count("get", "commits/"))
			is.Equal(t, fmt.Sprint(5), at(events.named(core.EventCatchUp)[0], "catch_up.commits"))
		})
	})
}
