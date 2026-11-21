package core_test

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
)

// oddTexts are texts for defaults with what the literal of a default in SQL must get right: single
// quotes, alone, doubled, and at the ends, a double quote, a backslash, the starts of comments, a
// semicolon, line breaks and other control characters, a byte-order mark, line and paragraph
// separators, combining and multi-byte characters, the largest code point, and texts that read like
// other literals.
var oddTexts = []string{"'", "''", "'a'", "a'", "'a", `"`, `""`, `\`, `\'`, "--", "/*", "*/", ";", "a;--b", "\n", "\r\n", "\t", "\x01", "\x7f",
	"\ufeff", "\u2028\u2029", "e\u0301", "日本語", "🥪", "\U0010ffff", "x'00'", "null", "NULL", "1", "-0.0", "1e+00", " ", "  a  ", "$", "?", "?1", ":a", "@a"}

// everyByte is a blob of every byte value, in order, and then in reverse.
func everyByte() []byte {
	b := make([]byte, 512)
	for i := range 256 {
		b[i], b[511-i] = byte(i), byte(i)
	}
	return b
}

// defaultTables are tables with every kind of default that a column can have: for each type, a
// column with each of the type's edges as its default, of which every other one takes NULL, and the
// primary key with the first; for text, a column for each of the odd texts too; for a blob, a blob
// of every byte value; and in each, NULL as the default of a column that takes NULL and of one that
// does not, beside a column of each kind without a default. Then a table whose primary key is
// DEFAULT NULL, one with a key of two columns that both have a default, and one with long defaults.
func defaultTables() []*s4qlpb.CreateTable {
	var tables []*s4qlpb.CreateTable
	for _, typ := range types {
		values := edges[typ]
		switch typ {
		case s4qlpb.Type_TYPE_TEXT:
			for _, s := range oddTexts {
				values = append(values, row(s)[0])
			}
		case s4qlpb.Type_TYPE_BLOB:
			values = append(values, row(everyByte())[0])
		}
		columns := make([]s4qlpb.Type, 1+len(values)+4)
		for i := range columns {
			columns[i] = typ
		}
		ct := typedTable("d_"+typeName(typ), columns...)
		ct.GetColumns()[0].SetDefault(values[0])
		for i, v := range values {
			c := ct.GetColumns()[1+i]
			c.SetDefault(v)
			if i%2 == 1 {
				c.SetNullable(true)
			}
		}
		// NULL on a column that takes NULL and on one that does not, and no default on each.
		n := 1 + len(values)
		withDefault(nullableTable(ct, n, n+2), n, nil)
		withDefault(ct, n+1, nil)
		tables = append(tables, ct)
	}
	tables = append(tables, withDefault(withDefault(nullableTable(typedTable("k_null", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL), 1), 0, nil), 1, 2.5))
	keyed := withDefault(withDefault(typedTable("k_two", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB), 0, -7), 1, "k'")
	keyed.SetPrimaryKey([]uint32{1, 0})
	tables = append(tables, withDefault(keyed, 2, []byte{}))
	long := strings.Repeat("a'b\"c\n", 20_000)
	tables = append(tables, withDefault(withDefault(typedTable("long", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB), 1, long), 2, []byte(long)))
	return tables
}

// storedTable is ct as the database stores it: a copy in which a default of -0.0 is 0.0.
func storedTable(ct *s4qlpb.CreateTable) *s4qlpb.CreateTable {
	ct = proto.CloneOf(ct)
	for _, c := range ct.GetColumns() {
		if d := c.GetDefault(); d.HasReal() && d.GetReal() == 0 {
			c.SetDefault(row(0.0)[0])
		}
	}
	return ct
}

func TestTx_CreateTable_defaults(t *testing.T) {
	t.Run("should give back every kind of default exactly, in the transaction, after the commit, after a close and an open, and after a replay into an empty cache, whose SQLite reads each as the same value", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		tables := defaultTables()
		want := map[string][]string{}
		var lines []string
		write(t, d, func(tx *core.Tx) {
			for _, ct := range tables {
				is.NotError(t, tx.CreateTable(t.Context(), ct), ct.GetTable())
				got, err := tx.Table(t.Context(), ct.GetTable())
				is.NotError(t, err)
				is.True(t, proto.Equal(storedTable(ct), got), "the table in the transaction differs", ct.GetTable())
				is.Equal(t, describe(storedTable(ct)), describe(got))

				// A row of the first default of each column, or 1 where there is none or it is NULL, in the
				// same transaction.
				values := make([]*s4qlpb.Value, len(ct.GetColumns()))
				for i, c := range got.GetColumns() {
					values[i] = c.GetDefault()
					if !c.HasDefault() || c.GetDefault().HasNull() {
						values[i] = edges[c.GetType()][1]
					}
				}
				is.NotError(t, tx.Upsert(t.Context(), ct.GetTable(), values))
				want[describe(storedTable(ct))] = []string{text(values)}
				lines = append(lines, "create "+describe(storedTable(ct)), fmt.Sprint("upsert ", ct.GetTable(), " ", columnPositions(len(values)), " ", text(values)))
			}
		})
		is.Equal(t, strings.Join(lines, "\n"), strings.Join(w.commits(t)[0], "\n"), "the commit object")
		is.Equal(t, show(want), read(t, d), "what the instance serves")
		r, err := startRead(t, d)
		is.NotError(t, err)
		for _, ct := range tables {
			got, err := r.Table(t.Context(), ct.GetTable())
			is.NotError(t, err)
			is.True(t, proto.Equal(storedTable(ct), got), "the table of a read differs", ct.GetTable())
		}
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))

		again := w.serve(t, "a", core.Options{})
		is.Equal(t, show(want), read(t, again), "what the instance serves after a close and an open")
		write(t, again, func(tx *core.Tx) {
			tables, err := tx.Tables(t.Context())
			is.NotError(t, err)
			for _, got := range tables {
				is.True(t, proto.Equal(storedTable(tableNamed(defaultTables(), got.GetTable())), got), "the table after an open differs", got.GetTable())
			}
		})
		is.NotError(t, again.Close(t.Context()))
		replayed := w.serve(t, "b", core.Options{})
		is.Equal(t, show(want), read(t, replayed), "what the instance serves after a replay into an empty cache")
		is.NotError(t, replayed.Close(t.Context()))

		for _, cache := range []string{"a", "b"} {
			_, state, err := cacheDump(filepath.Join(w.dir, cache+".db"))
			is.NotError(t, err)
			is.Equal(t, show(want), show(state), "the cache", cache)
		}
	})

	t.Run("should not fill in a default: refuse a row without a value for every column, and store an explicit NULL as NULL", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), withDefault(withDefault(nullableTable(tableOf("t", 1, "k", "v", "w"), 2), 1, 5), 2, 6)))
			is.Error(t, core.ErrInvalid, tx.Upsert(t.Context(), "t", row(1)))
			is.Error(t, core.ErrInvalid, tx.Upsert(t.Context(), "t", row(1, 2)))
			_, err := tx.Insert(t.Context(), "t", row(1))
			is.Error(t, core.ErrInvalid, err)
			is.NotError(t, tx.Upsert(t.Context(), "t", row(1, 2, nil)))
		})
		is.Equal(t, "t(k*0,v=5,w?=6): 1,2,null", read(t, d))
	})

	t.Run("should refuse a default that no column of its type holds with ErrInvalid before anything changes, and go on", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		var other int
		for name, v := range map[string]*s4qlpb.Value{
			"a real for INT":           row(1.0)[0],
			"text for INTEGER":         row("1")[0],
			"an integer for REAL":      row(1)[0],
			"a blob for TEXT":          row([]byte("a"))[0],
			"text for BLOB":            row("a")[0],
			"NaN":                      row(math.NaN())[0],
			"an infinite real":         row(math.Inf(1))[0],
			"text with NUL":            row("a\x00")[0],
			"text that is not UTF-8":   row("\xff")[0],
			"a value of no known kind": {},
			"a NULL of a newer version": func() *s4qlpb.Value {
				v := row(nil)[0]
				v.GetNull().ProtoReflect().SetUnknown([]byte{0x08, 0x01})
				return v
			}(),
		} {
			write(t, d, func(tx *core.Tx) {
				typ := s4qlpb.Type_TYPE_INT
				switch name {
				case "text for INTEGER":
					typ = s4qlpb.Type_TYPE_INTEGER
				case "an integer for REAL", "NaN", "an infinite real":
					typ = s4qlpb.Type_TYPE_REAL
				case "a blob for TEXT", "text with NUL", "text that is not UTF-8":
					typ = s4qlpb.Type_TYPE_TEXT
				case "text for BLOB":
					typ = s4qlpb.Type_TYPE_BLOB
				}
				ct := typedTable("t", s4qlpb.Type_TYPE_INT, typ)
				ct.GetColumns()[1].SetDefault(v)
				err := tx.CreateTable(t.Context(), ct)
				is.Error(t, core.ErrInvalid, err, name)
				_, err = tx.Table(t.Context(), "t")
				is.Error(t, core.ErrNoTable, err, name)
				other++
				is.NotError(t, tx.CreateTable(t.Context(), withDefault(typedTable(fmt.Sprint("u", other), s4qlpb.Type_TYPE_INT, typ), 1, nil)), name)
			})
		}
		is.True(t, !strings.Contains(read(t, d), "t("), "a refused table is there")
	})

	t.Run("should take a default of -0.0 as 0.0, which is what a column stores for it, commit 0.0, and leave the table it is handed as it is", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ct := withDefault(typedTable("t", s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_REAL), 1, math.Copysign(0, -1))
		ct.GetColumns()[0].SetDefault(row(math.Copysign(0, -1))[0])
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), ct))
			got, err := tx.Table(t.Context(), "t")
			is.NotError(t, err)
			for _, c := range got.GetColumns() {
				is.True(t, !math.Signbit(c.GetDefault().GetReal()), "the default has the sign of -0.0")
			}
		})
		is.True(t, math.Signbit(ct.GetColumns()[1].GetDefault().GetReal()), "the table that was handed in changed")
		is.Equal(t, "create t(c0*0:real=r0,c1:real=r0)", w.commits(t)[0][0])
		is.Equal(t, "t(c0*0:real=r0,c1:real=r0): ", read(t, d))
	})

	t.Run("should keep a default of its own, apart from the table it is handed and the table it gives", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ct := withDefault(tableOf("t", 1, "k", "v"), 1, 5)
		write(t, d, func(tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), ct))
			ct.GetColumns()[1].GetDefault().SetInteger(6)
			got, err := tx.Table(t.Context(), "t")
			is.NotError(t, err)
			is.Equal(t, int64(5), got.GetColumns()[1].GetDefault().GetInteger())
			got.GetColumns()[1].GetDefault().SetInteger(7)
			got, err = tx.Table(t.Context(), "t")
			is.NotError(t, err)
			is.Equal(t, int64(5), got.GetColumns()[1].GetDefault().GetInteger())
		})
		is.Equal(t, "create t(k*0,v=5)", w.commits(t)[0][0])
	})

	t.Run("should take a table whose statement in the cache is as long as the length limit by its default, of text, of text of quotes, and of a blob, and refuse it a byte longer with ErrInvalid", func(t *testing.T) {
		const limit = 1000
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: limit})
		shapes := map[string]func(n int) *s4qlpb.CreateTable{
			"text": func(n int) *s4qlpb.CreateTable {
				return withDefault(typedTable("t_text", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT), 1, strings.Repeat("x", n))
			},
			// Each single quote is two bytes in the literal, so the text ends with an x for an odd n.
			"quotes": func(n int) *s4qlpb.CreateTable {
				return withDefault(nullableTable(typedTable("t_quotes", s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_TEXT), 1), 0, strings.Repeat("'", n/2)+strings.Repeat("x", n%2))
			},
			"blob": func(n int) *s4qlpb.CreateTable {
				return withDefault(typedTable("t_blob", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_BLOB), 1, []byte(strings.Repeat("\xff", n)))
			},
		}
		want := map[string][]string{}
		for name, shape := range shapes {
			// accepted reports whether a create table of the shape with n units in its default is taken,
			// in a transaction that is rolled back.
			accepted := func(n int) bool {
				tx, err := d.Begin(t.Context())
				is.NotError(t, err)
				defer func() { is.NotError(t, tx.Rollback(t.Context())) }()
				err = tx.CreateTable(t.Context(), shape(n))
				if err != nil {
					is.Error(t, core.ErrInvalid, err, name)
					is.Error(t, core.ErrTooLongForCache, err, name)
					is.True(t, strings.Contains(err.Error(), fmt.Sprint("where the limit is ", limit)), err.Error())
				}
				return err == nil
			}
			lo, hi := 0, limit
			is.True(t, accepted(lo), name)
			is.True(t, !accepted(hi), name)
			for hi-lo > 1 {
				if mid := (lo + hi) / 2; accepted(mid) {
					lo = mid
				} else {
					hi = mid
				}
			}
			ct := shape(lo)
			write(t, d, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), ct))
				got, err := tx.Table(t.Context(), ct.GetTable())
				is.NotError(t, err)
				is.True(t, proto.Equal(ct, got), "the table at the limit differs", name)
			})
			want[describe(ct)] = []string{}

			// The statement as SQLite keeps it is as long as the limit.
			c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
			is.NotError(t, err)
			s, err := c.Prepare("select length(cast(sql as blob)) from sqlite_schema where name = ?")
			is.NotError(t, err)
			is.NotError(t, s.BindText(1, ct.GetTable()))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok, name)
			is.Equal(t, int64(limit), s.ColumnInt64(0), name)
			is.NotError(t, s.Finalize())
			is.NotError(t, c.Close())
		}
		is.Equal(t, show(want), read(t, d))
		is.NotError(t, d.Close(t.Context()))
		again := w.serve(t, "a", core.Options{LengthLimit: limit})
		is.Equal(t, show(want), read(t, again), "after a close and an open")
		is.NotError(t, again.Close(t.Context()))
		is.Equal(t, show(want), read(t, w.serve(t, "b", core.Options{LengthLimit: limit})), "after a replay into an empty cache")
	})
}

// tableNamed is the table of the given name among tables, or nil.
func tableNamed(tables []*s4qlpb.CreateTable, name string) *s4qlpb.CreateTable {
	for _, ct := range tables {
		if ct.GetTable() == name {
			return ct
		}
	}
	return nil
}

func TestOpen_cache_defaults(t *testing.T) {
	t.Run("should refuse a cache of version 3, made before defaults, and leave it as it is", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, createWith(nullableTable(tableOf("t1", 1, "k", "v"), 1)), upsert("t1", 1, nil))
		is.NotError(t, d.Close(t.Context()))
		path := filepath.Join(w.dir, "a.db")
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		is.NotError(t, c.Exec("update s4ql_cache set version = 3"))
		is.NotError(t, c.Close())
		before, err := os.ReadFile(path)
		is.NotError(t, err)

		_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
		is.Error(t, core.ErrCacheVersion, err)
		is.True(t, strings.Contains(err.Error(), fmt.Sprint("it has version 3, and this version of s4ql reads version ", core.CacheVersion)), err.Error())
		is.Equal(t, int64(3), cacheVersion(t, path))
		after, err := os.ReadFile(path)
		is.NotError(t, err)
		is.True(t, string(before) == string(after), "the open changed the main file of the cache")
	})

	t.Run("should declare each default in the cache in one form, and a column that takes NULL without not null", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		ct := typedTable("t", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB, s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_TEXT)
		for i, v := range []any{math.MinInt64, 12, -1.5e-300, "it's", []byte{0xab, 0}, nil, 1.0} {
			withDefault(ct, i, v)
		}
		nullableTable(ct, 5, 7)
		// A key of two columns, which a key of its own declares, and whose columns, which never take
		// NULL, are not declared not null.
		u := withDefault(typedTable("u", s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_REAL), 1, "k")
		u.SetPrimaryKey([]uint32{1, 0})
		transact(t, d, createWith(ct), createWith(u))
		is.NotError(t, d.Close(t.Context()))

		c, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		for name, want := range map[string]string{
			"t": `CREATE TABLE "t" ("c0" int primary key default -9223372036854775808, "c1" integer not null default 12, "c2" real not null default -1.5e-300, "c3" text not null default 'it''s', "c4" blob not null default x'ab00', "c5" int default null, "c6" real not null default 1e+00, "c7" text) strict, without rowid`,
			"u": `CREATE TABLE "u" ("c0" int, "c1" text default 'k', "c2" real not null, primary key ("c1", "c0")) strict, without rowid`,
		} {
			s, err := c.Prepare("select sql from sqlite_schema where name = ?")
			is.NotError(t, err)
			is.NotError(t, s.BindText(1, name))
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, want, s.ColumnText(0))
			is.NotError(t, s.Finalize())
		}
	})
}
