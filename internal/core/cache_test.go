package core_test

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
	"maragu.dev/s4ql/internal/sqlite/faultvfs"
)

// hostile names of tables and columns: everything that an identifier has to be quoted for, and
// what SQLite or SQL treats specially when it is not.
var hostile = []string{
	`a"b`, `"`, `""`, `"quoted"`, `a'b`, `'`, "a`b", "[a]", `a\b`, `a\`,
	"a b", " ", "  leading", "trailing  ", "tab\there", "line\nbreak", "cr\rhere", "\x01\x02", "\x7f",
	"a;drop table t1", "a--comment", "a/*comment*/", "x); drop table t1; --",
	"select", "table", "primary", "rowid", "_rowid_", "oid", "main", "temp", "null",
	"1", "1a", "-1", "1.5", "?", "?1", ":a", "@a", "$a", "%", "_", "a%b", "*", ".", "a.b", "main.t1",
	"smørrebrød", "日本語", "🥪", "e" + string(rune(0x301)), string(rune(0xe9)), string(rune(0x200b)), string(rune(0xfeff)), string(rune(0x10ffff)),
	strings.Repeat("long", 500),
}

func TestQuote(t *testing.T) {
	t.Run("should quote a name so that SQLite reads it as that name and nothing else", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()

		for i, name := range hostile {
			table := fmt.Sprint("t", i)
			is.NotError(t, c.Exec(fmt.Sprint("create table ", table, " (", core.Quote(name), " int primary key) strict, without rowid")), name)
			// The name of the one column, as SQLite read it.
			s, err := c.Prepare(fmt.Sprint("select name from pragma_table_info('", table, "')"))
			is.NotError(t, err)
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.True(t, name == s.ColumnText(0), fmt.Sprintf("%q was read as %q", name, s.ColumnText(0)))
			is.NotError(t, s.Finalize())
		}
	})
}

func TestBind(t *testing.T) {
	t.Run("should bind each argument to its parameter, in order, as its type, and -0.0 as 0.0", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		s, err := c.Prepare("select typeof(?1) || ' ' || ?1, ?2, ?3, quote(?4), quote(?5), ?6 is null, quote(?7), ?8")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		values := []*s4qlpb.Value{
			s4qlpb.Value_builder{Real: new(1.5)}.Build(), s4qlpb.Value_builder{Text: new("é")}.Build(),
			s4qlpb.Value_builder{Blob: []byte{0, 1}}.Build(), s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build(),
			s4qlpb.Value_builder{Integer: new(int64(-3))}.Build(), s4qlpb.Value_builder{Real: new(math.Copysign(0, -1))}.Build(),
		}
		is.NotError(t, core.Bind(s, values[4], "a", int64(7), values[0], values[2], values[3], values[1], values[5]))
		ok, err := s.Step()
		is.NotError(t, err)
		is.True(t, ok)
		is.Equal(t, "integer -3", s.ColumnText(0))
		is.Equal(t, "a", s.ColumnText(1))
		is.Equal(t, int64(7), s.ColumnInt64(2))
		is.Equal(t, "1.5", s.ColumnText(3))
		is.Equal(t, "X'0001'", s.ColumnText(4))
		is.Equal(t, int64(1), s.ColumnInt64(5))
		is.Equal(t, "'é'", s.ColumnText(6))
		// SQLite keeps the sign of a parameter that it only selects; a column of the cache would not.
		is.True(t, s.ColumnDouble(7) == 0 && !math.Signbit(s.ColumnDouble(7)), fmt.Sprint(s.ColumnDouble(7)))
	})

	t.Run("should refuse a value of no kind and an argument of another Go type, and give SQLite's error of a bind", func(t *testing.T) {
		c, err := sqlite.Open(":memory:")
		is.NotError(t, err)
		defer func() { is.NotError(t, c.Close()) }()
		s, err := c.Prepare("select ?")
		is.NotError(t, err)
		defer func() { is.NotError(t, s.Finalize()) }()
		is.Error(t, core.ErrInvalid, core.Bind(s, &s4qlpb.Value{}))
		err = core.Bind(s, 1)
		is.True(t, err != nil && strings.Contains(err.Error(), "cannot bind a int"), fmt.Sprint(err))
		// The statement has one parameter, so SQLite refuses the second.
		err = core.Bind(s, int64(1), int64(2))
		var se *sqlite.Error
		is.True(t, errors.As(err, &se), fmt.Sprint(err))
	})
}

func TestScan(t *testing.T) {
	// unwind runs a loop over the scan that panics, as a caller's loop can, and recovers.
	unwind := func(t *testing.T, scan iter.Seq2[[]*s4qlpb.Value, error]) {
		t.Helper()
		defer func() { _ = recover() }()
		for _, err := range scan {
			is.NotError(t, err)
			panic("in the loop over a scan")
		}
	}

	t.Run("should give back the statement of a read's scan that a panic ends, so that a later read sees a later commit and the instance closes", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		rd := newReader(t, d)
		r, err := rd.Read(t.Context())
		is.NotError(t, err)
		unwind(t, r.Scan(t.Context(), "t1"))
		is.NotError(t, r.Close(t.Context()))

		// The next read of the same reader, on the same connection.
		transact(t, d, upsert("t1", 2))
		r, err = rd.Read(t.Context())
		is.NotError(t, err)
		state, err := dump(t.Context(), r)
		is.NotError(t, err)
		is.Equal(t, "t1(k*0): 1 | 2", show(state))
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
	})

	t.Run("should give back the statement of a write's scan that a panic ends, so that a later write commits and the instance closes", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		unwind(t, tx.Scan(t.Context(), "t1"))
		is.NotError(t, tx.Rollback(t.Context()))

		transact(t, d, upsert("t1", 2))
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))
		is.NotError(t, d.Close(t.Context()))
	})
}

func TestScan_order(t *testing.T) {
	t.Run("should give the rows of a table of each type in the order in which SQLite orders the same keys in a table of its own, which is the order of the model", func(t *testing.T) {
		for _, typ := range types {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			keys := slices.Clone(edges[typ])
			rand.New(rand.NewPCG(uint64(typ), 1)).Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
			write(t, d, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), typedTable("t", typ)))
				for _, k := range keys {
					is.NotError(t, tx.Upsert(t.Context(), "t", []*s4qlpb.Value{k}), text([]*s4qlpb.Value{k}))
				}
			})
			r, err := startRead(t, d)
			is.NotError(t, err)
			var scanned []string
			for row, err := range r.Scan(t.Context(), "t") {
				is.NotError(t, err)
				scanned = append(scanned, text(row))
			}
			is.NotError(t, r.Close(t.Context()))

			// SQLite's own table of the same type, with the same keys, in the same order.
			c, err := sqlite.Open(":memory:")
			is.NotError(t, err)
			is.NotError(t, c.Exec("create table t (k "+typeName(typ)+" primary key) strict, without rowid"))
			s, err := c.Prepare("insert or replace into t (k) values (?)")
			is.NotError(t, err)
			for _, k := range keys {
				is.NotError(t, bindValue(s, 1, k))
				_, err := s.Step()
				is.NotError(t, err)
				is.NotError(t, s.Reset())
			}
			is.NotError(t, s.Finalize())
			var ordered []string
			s, err = c.Prepare("select k from t order by k")
			is.NotError(t, err)
			for {
				ok, err := s.Step()
				is.NotError(t, err)
				if !ok {
					break
				}
				ordered = append(ordered, text([]*s4qlpb.Value{columnValue(s, 0)}))
			}
			is.NotError(t, s.Finalize())
			is.NotError(t, c.Close())

			// The model's order of the keys as a column holds them, each once.
			var model []string
			sorted := slices.Clone(keys)
			slices.SortStableFunc(sorted, func(a, b *s4qlpb.Value) int { return compareKeys([]*s4qlpb.Value{a}, []*s4qlpb.Value{b}) })
			for _, k := range sorted {
				if k := text(stored([]*s4qlpb.Value{k})); len(model) == 0 || model[len(model)-1] != k {
					model = append(model, k)
				}
			}

			is.Equal(t, len(edges[typ])-strings.Count(text(edges[typ]), "r-0"), len(ordered), "the keys of "+typeName(typ)+" are not all distinct")
			is.EqualSlice(t, ordered, scanned, typeName(typ))
			is.EqualSlice(t, ordered, model, typeName(typ))
			is.NotError(t, d.Close(t.Context()))
		}
	})
}

func TestScanRange(t *testing.T) {
	t.Run("should give the rows of a table of each type whose key is within the bounds, in the order of the scan, in a read and in a write", func(t *testing.T) {
		for _, typ := range types {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			write(t, d, func(tx *core.Tx) {
				is.NotError(t, tx.CreateTable(t.Context(), typedTable("t", typ, s4qlpb.Type_TYPE_INT)))
				for i, k := range edges[typ] {
					is.NotError(t, tx.Upsert(t.Context(), "t", []*s4qlpb.Value{k, row(int64(i))[0]}), text([]*s4qlpb.Value{k}))
				}
			})
			r, err := startRead(t, d)
			is.NotError(t, err)
			var all [][]*s4qlpb.Value
			for row, err := range r.Scan(t.Context(), "t") {
				is.NotError(t, err)
				all = append(all, row)
			}
			// want is the rows of the full scan whose key is within the bounds, by the model's order.
			want := func(lower, upper core.Bound) string {
				var rows []string
				for _, row := range all {
					k := row[:1]
					if lower.Value != nil {
						if c := compareKeys(k, []*s4qlpb.Value{lower.Value}); c < 0 || c == 0 && lower.Open {
							continue
						}
					}
					if upper.Value != nil {
						if c := compareKeys(k, []*s4qlpb.Value{upper.Value}); c > 0 || c == 0 && upper.Open {
							continue
						}
					}
					rows = append(rows, text(row))
				}
				return strings.Join(rows, " | ")
			}
			scanned := func(scan iter.Seq2[[]*s4qlpb.Value, error]) string {
				var rows []string
				for row, err := range scan {
					is.NotError(t, err)
					rows = append(rows, text(row))
				}
				return strings.Join(rows, " | ")
			}
			// Every bound of every value of the edges, also -0.0, closed, open, and none.
			bounds := []core.Bound{{}}
			for _, k := range edges[typ] {
				bounds = append(bounds, core.Bound{Value: k}, core.Bound{Value: k, Open: true})
			}
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			cases := 0
			for _, lower := range bounds {
				for _, upper := range bounds {
					bound := func(b core.Bound) string {
						if b.Value == nil {
							return "none"
						}
						return fmt.Sprint(text([]*s4qlpb.Value{b.Value}), " open ", b.Open)
					}
					name := fmt.Sprintf("%v from %v to %v", typeName(typ), bound(lower), bound(upper))
					w := want(lower, upper)
					is.Equal(t, w, scanned(r.ScanRange(t.Context(), "t", lower, upper)), name)
					is.Equal(t, w, scanned(tx.ScanRange(t.Context(), "t", lower, upper)), name)
					if w != "" && w != want(core.Bound{}, core.Bound{}) {
						cases++
					}
				}
			}
			is.True(t, cases > len(bounds), "the bounds leave out some rows and not all of them")
			is.NotError(t, tx.Rollback(t.Context()))
			is.NotError(t, r.Close(t.Context()))
			is.NotError(t, d.Close(t.Context()))
		}
	})

	t.Run("should refuse a bound that is no key of the table, and a bound of a table whose key is two columns", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{LengthLimit: 1000})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1), create("t2", 2, "a", "b"), upsert("t2", 1, 2))
		cases := []struct {
			name  string
			table string
			b     core.Bound
			want  error
		}{
			{name: "NULL", table: "t1", b: core.Bound{Value: s4qlpb.Value_builder{Null: &s4qlpb.Null{}}.Build()}, want: core.ErrInvalid},
			{name: "text for an INT key", table: "t1", b: core.Bound{Value: row("1")[0]}, want: core.ErrInvalid},
			{name: "a value of no kind", table: "t1", b: core.Bound{Value: &s4qlpb.Value{}}, want: core.ErrInvalid},
			{name: "a key of two columns", table: "t2", b: core.Bound{Value: row(1)[0]}, want: core.ErrInvalid},
			{name: "no table", table: "t3", b: core.Bound{Value: row(1)[0]}, want: core.ErrNoTable},
		}
		r, err := startRead(t, d)
		is.NotError(t, err)
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		for _, c := range cases {
			for _, scan := range []func(lower, upper core.Bound) iter.Seq2[[]*s4qlpb.Value, error]{
				func(lower, upper core.Bound) iter.Seq2[[]*s4qlpb.Value, error] {
					return r.ScanRange(t.Context(), c.table, lower, upper)
				},
				func(lower, upper core.Bound) iter.Seq2[[]*s4qlpb.Value, error] {
					return tx.ScanRange(t.Context(), c.table, lower, upper)
				},
			} {
				// The bound is refused alone, and next to a bound that is a key of t1, on either side.
				valid := core.Bound{Value: row(1)[0]}
				for _, bounds := range [][2]core.Bound{{c.b, {}}, {{}, c.b}, {valid, c.b}, {c.b, valid}} {
					n := 0
					for row, err := range scan(bounds[0], bounds[1]) {
						n++
						is.True(t, row == nil, c.name)
						is.True(t, errors.Is(err, c.want), c.name+": "+fmt.Sprint(err))
					}
					is.Equal(t, 1, n, c.name)
				}
			}
		}
		is.NotError(t, tx.Rollback(t.Context()))
		is.NotError(t, r.Close(t.Context()))
		transact(t, d, func(t *testing.T, tx *core.Tx) {
			is.NotError(t, tx.CreateTable(t.Context(), typedTable("t4", s4qlpb.Type_TYPE_TEXT)))
		})
		r, err = startRead(t, d)
		is.NotError(t, err)
		tx, err = d.Begin(t.Context())
		is.NotError(t, err)
		long := core.Bound{Value: row(strings.Repeat("x", 1001))[0]}
		for _, scan := range []iter.Seq2[[]*s4qlpb.Value, error]{
			r.ScanRange(t.Context(), "t4", long, core.Bound{}),
			r.ScanRange(t.Context(), "t4", core.Bound{}, long),
			tx.ScanRange(t.Context(), "t4", long, core.Bound{}),
			tx.ScanRange(t.Context(), "t4", core.Bound{}, long),
		} {
			n := 0
			for _, err := range scan {
				n++
				is.True(t, errors.Is(err, core.ErrTooBig), fmt.Sprint(err))
			}
			is.Equal(t, 1, n)
		}
		// Of two bounds that are refused, the lower one is named.
		invalid := core.Bound{Value: &s4qlpb.Value{}}
		for _, c := range []struct {
			lower, upper core.Bound
			want         error
		}{{invalid, long, core.ErrInvalid}, {long, invalid, core.ErrTooBig}} {
			for _, scan := range []iter.Seq2[[]*s4qlpb.Value, error]{
				r.ScanRange(t.Context(), "t4", c.lower, c.upper),
				tx.ScanRange(t.Context(), "t4", c.lower, c.upper),
			} {
				n := 0
				for row, err := range scan {
					n++
					is.True(t, row == nil)
					is.True(t, errors.Is(err, c.want), fmt.Sprint(err))
				}
				is.Equal(t, 1, n)
			}
		}
		is.NotError(t, tx.Rollback(t.Context()))
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
	})

	t.Run("should end with a local failure when SQLite cannot read the rows of the scan", func(t *testing.T) {
		vfs, err := faultvfs.New()
		is.NotError(t, err)
		w := newWorld(t)
		o := core.Options{}
		o.VFS = vfs.Name()
		d := w.serve(t, "a", o)
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1), upsert("t1", 2), upsert("t1", 3))
		r, err := startRead(t, d)
		is.NotError(t, err)
		// The look-up of the table reads the schema, and the new connection of the reader has read no
		// page of the table yet, so the scan reads them from the file, and every read of it fails.
		_, err = r.Table(t.Context(), "t1")
		is.NotError(t, err)
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB | faultvfs.WAL})
		n := 0
		for row, err := range r.ScanRange(t.Context(), "t1", core.Bound{Value: row(2)[0]}, core.Bound{}) {
			n++
			is.True(t, row == nil)
			is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
		}
		is.Equal(t, 1, n)
		is.True(t, vfs.Failures() > 0, "no read failed")
		vfs.Fail(faultvfs.Fault{})
		_ = r.Close(t.Context())
		_ = d.Close(t.Context())

		// A new instance on the cache has a new connection for writes, which has read no page of the
		// table yet either.
		d = w.serve(t, "a", o)
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		_, err = tx.Table(t.Context(), "t1")
		is.NotError(t, err)
		failed := vfs.Failures()
		vfs.Fail(faultvfs.Fault{Ops: faultvfs.Read, Files: faultvfs.MainDB | faultvfs.WAL})
		n = 0
		for row, err := range tx.ScanRange(t.Context(), "t1", core.Bound{}, core.Bound{Value: row(2)[0]}) {
			n++
			is.True(t, row == nil)
			is.True(t, errors.Is(err, core.ErrLocal), fmt.Sprint(err))
		}
		is.Equal(t, 1, n)
		is.True(t, vfs.Failures() > failed, "no read failed")
		vfs.Fail(faultvfs.Fault{})
		_ = tx.Rollback(t.Context())
		_ = d.Close(t.Context())
		is.NotError(t, vfs.Close())
	})
}

func TestOpen_cache_foreign(t *testing.T) {
	// made is a cache with the table t1 of the columns k and v, with the row 1,1 and the index i1 on
	// v, that the instance closed. It returns the world and the path of the cache.
	made := func(t *testing.T) (*world, string) {
		t.Helper()
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 1), createIndex("i1", "t1", 1))
		is.NotError(t, d.Close(t.Context()))
		return w, filepath.Join(w.dir, "a.db")
	}
	// change the cache at path with plain SQLite, by the statements of statements, which a semicolon
	// and a line break separate, and return its main file after.
	change := func(t *testing.T, path, statements string) []byte {
		t.Helper()
		c, err := sqlite.Open(path)
		is.NotError(t, err)
		for statement := range strings.SplitSeq(statements, ";\n") {
			is.NotError(t, c.Exec(statement), statement)
		}
		is.NotError(t, c.Close())
		before, err := os.ReadFile(path)
		is.NotError(t, err)
		return before
	}
	// unchanged fails the test if the main file of the cache at path is not before.
	unchanged := func(t *testing.T, path string, before []byte, what string) {
		t.Helper()
		after, err := os.ReadFile(path)
		is.NotError(t, err)
		is.True(t, string(before) == string(after), "the instance changed the main file of the cache", what)
	}
	const advice = "something other than s4ql has changed the cache, so open the database with a new cache"

	t.Run("should refuse at the open a cache that holds a view, a trigger, a table of a reserved name, or an index on one, or another table s4ql_cache, and leave it as it is", func(t *testing.T) {
		for _, test := range []struct {
			statement string
			why       string
		}{
			{statement: "create view v as select 1", why: `it holds the view "v" on the table "v", which s4ql does not make`},
			{statement: "create view t2 as select * from t1", why: `it holds the view "t2"`},
			{statement: "create trigger tr after insert on t1 begin select 1; end", why: `it holds the trigger "tr" on the table "t1"`},
			{statement: "create trigger tr before delete on s4ql_cache begin select 1; end", why: `it holds the trigger "tr" on the table "s4ql_cache"`},
			{statement: "create table s4ql_other (a int)", why: `it holds the table "s4ql_other"`},
			{statement: "create table S4QL_x (a int primary key) strict, without rowid", why: `it holds the table "S4QL_x"`},
			{statement: "create index s on s4ql_cache (head)", why: `it holds the index "s" on the table "s4ql_cache"`},
			{statement: `create table saved as select * from s4ql_cache;
drop table s4ql_cache;
create table s4ql_cache (id integer primary key, version integer not null, head text not null, database text not null) strict;
insert into s4ql_cache select * from saved;
drop table saved`, why: `its table "s4ql_cache" is not declared as s4ql declares it`},
			// The adversarial review of 2026-11-26 found that a cache at the empty log was taken with its
			// tables, which replay of the log then put commit objects on.
			{statement: "update s4ql_cache set head = '" + strings.Repeat("0", 64) + "'", why: `it records no commit object, and holds the table "t1"`},
		} {
			w, path := made(t)
			before := change(t, path, test.statement)

			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.Error(t, core.ErrCacheVersion, err, test.statement)
			is.True(t, strings.Contains(err.Error(), test.why), err.Error())
			is.True(t, strings.Contains(err.Error(), advice), err.Error())
			unchanged(t, path, before, test.statement)
		}
	})

	// The adversarial review of 2026-11-26 found that such a file got the table s4ql_cache and was
	// used, with SQLite's order of text in UTF-16.
	t.Run("should refuse at the open a file whose text is not in UTF-8, and make nothing in it", func(t *testing.T) {
		for _, encoding := range []string{"UTF-16le", "UTF-16be"} {
			w := newWorld(t)
			path := filepath.Join(w.dir, "a.db")
			c, err := sqlite.Open(path)
			is.NotError(t, err)
			// The encoding is written to the file with its first table.
			is.NotError(t, c.Exec("pragma encoding = '"+encoding+"'"))
			is.NotError(t, c.Exec("create table x (a)"))
			is.NotError(t, c.Exec("drop table x"))
			is.NotError(t, c.Close())

			_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.Error(t, core.ErrCacheVersion, err, encoding)
			is.True(t, strings.Contains(err.Error(), "its text is in the encoding "+encoding+", not UTF-8"), err.Error())
			// The open puts the file into the journal mode wal before it looks, and writes nothing else.
			c, err = sqlite.Open(path)
			is.NotError(t, err)
			s, err := c.Prepare("select count(*) from sqlite_schema")
			is.NotError(t, err)
			ok, err := s.Step()
			is.NotError(t, err)
			is.True(t, ok)
			is.Equal(t, int64(0), s.ColumnInt64(0), encoding)
			is.NotError(t, s.Finalize())
			is.NotError(t, c.Close())
		}
	})

	t.Run("should accept at the open a table of SQLite's own, such as sqlite_stat1", func(t *testing.T) {
		w, path := made(t)
		change(t, path, "analyze")
		d := w.serve(t, "a", core.Options{})
		is.Equal(t, "t1(k*0,v) +i1(1): 1,1", read(t, d))
	})

	t.Run("should refuse at the first look-up a table that s4ql does not make, stop for good, and leave the cache as it is", func(t *testing.T) {
		for _, test := range []struct {
			// statement that makes the table t9 with the columns k and v, of type int, and the key k,
			// which pragma_table_info reports as a table that s4ql makes, so that only the text of the
			// statement tells it from one.
			statement string
		}{
			{statement: `create table t9 (k int primary key not null, v int default 1) strict`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null) strict`},
			{statement: `create table "t9" ("k" text primary key collate nocase, "v" int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null, "g" int generated always as ("v" + 1)) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null, "g" int as ("v" + 1) stored) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null check ("v" > 0)) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null, check ("v" > 0)) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null default (1)) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null) without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null references "t1" ("k")) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null, foreign key ("v") references "t1") strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key on conflict replace, "v" int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key desc, "v" int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int, "v" int not null, primary key ("k")) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null unique) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null collate nocase) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null constraint c) strict, without rowid`},
			{statement: `create table t9 (k int primary key, v int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null)  strict, without rowid`},
			{statement: `create table "t9" ("k" INT primary key, "v" int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int PRIMARY KEY, "v" int not null) strict, without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null) strict,without rowid`},
			{statement: `create table "t9" ("k" int primary key, "v" int not null) without rowid, strict`},
			{statement: `create table if not exists "t9" ("k" int primary key, "v" int not null) strict, without rowid /* a comment */`},
		} {
			w, path := made(t)
			change(t, path, test.statement)
			d := w.serve(t, "a", core.Options{})
			// The instance has opened the cache, and a read of t1 still answers.
			is.Equal(t, "t1(k*0,v) +i1(1)", func() string {
				r, err := startRead(t, d)
				is.NotError(t, err)
				defer func() { is.NotError(t, r.Close(t.Context())) }()
				ct, err := r.Table(t.Context(), "t1")
				is.NotError(t, err)
				indexes, err := r.Indexes(t.Context(), "t1")
				is.NotError(t, err)
				return describe(ct) + describeIndexes(indexes)
			}(), test.statement)
			is.NotError(t, d.Close(t.Context()))
			before, err := os.ReadFile(path)
			is.NotError(t, err)

			for _, write := range []bool{false, true} {
				d := w.serve(t, "a", core.Options{})
				var err error
				if write {
					tx, beginErr := d.Begin(t.Context())
					is.NotError(t, beginErr)
					_, err = tx.Get(t.Context(), "T9", row(1))
					_ = tx.Rollback(t.Context())
				} else {
					r, readErr := startRead(t, d)
					is.NotError(t, readErr)
					_, err = r.Get(t.Context(), "T9", row(1))
					_ = r.Close(t.Context())
				}
				is.Error(t, core.ErrCacheVersion, err, test.statement)
				is.Error(t, core.ErrLocal, err, test.statement)
				is.True(t, strings.Contains(err.Error(), `its table "t9" is not declared as s4ql declares a table of its columns`), err.Error())
				is.True(t, strings.Contains(err.Error(), advice), err.Error())
				is.True(t, isDone(d), test.statement)
				_ = d.Close(t.Context())
				unchanged(t, path, before, test.statement)
			}
		}
	})

	t.Run("should refuse at the first look-up a virtual table whose module is not there", func(t *testing.T) {
		w, path := made(t)
		before := change(t, path, `pragma writable_schema = on;
insert into sqlite_schema values ('table', 't9', 't9', 0, 'CREATE VIRTUAL TABLE t9 USING nomodule(k, v)')`)
		d := w.serve(t, "a", core.Options{})
		r, err := startRead(t, d)
		is.NotError(t, err)
		_, err = r.Get(t.Context(), "t9", row(1))
		is.Error(t, core.ErrCacheVersion, err)
		is.True(t, strings.Contains(err.Error(), `its table "t9" is not declared as s4ql declares a table`), err.Error())
		_ = r.Close(t.Context())
		is.True(t, isDone(d))
		_ = d.Close(t.Context())
		unchanged(t, path, before, "a virtual table")
	})

	t.Run("should refuse a table that gets a trigger while the instance runs, at its first look-up in the next transaction", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		transact(t, d, create("t1", 1, "k", "v"), upsert("t1", 1, 1))
		is.Equal(t, "t1(k*0,v): 1,1", read(t, d))
		change(t, filepath.Join(w.dir, "a.db"), "create trigger tr after insert on t1 begin select 1; end")

		tx, err := d.Begin(t.Context())
		is.NotError(t, err)
		_, err = tx.Get(t.Context(), "t1", row(1))
		is.Error(t, core.ErrCacheVersion, err)
		is.True(t, strings.Contains(err.Error(), `the table "t1" has the trigger "tr"`), err.Error())
		_ = tx.Rollback(t.Context())
		is.True(t, isDone(d))
	})

	t.Run("should refuse at the first scan an index that s4ql does not make, stop for good, and leave the cache as it is", func(t *testing.T) {
		for _, test := range []struct {
			statement string
			// why, unless the reason is that the index is not declared as s4ql declares it.
			why string
		}{
			{statement: `create index "i9" on "t1" ("v" collate binary)`, why: `the index "i9" of its table "t1" has the collation "binary"`},
			{statement: `create index "i9" on "t1" ("v" asc)`},
			{statement: `create index "i9" on "T1" ("v")`},
			{statement: `create index i9 on t1 (v)`},
			{statement: `create index "i9" on "t1" ("v", "k")  `},
			{statement: `create index "i9" on "t1"("v")`},
			{statement: `create index "i9" on "t1" ("V")`},
		} {
			statement, why := test.statement, test.why
			if why == "" {
				why = `the index "i9" of its table "t1" is not declared as s4ql declares an index of its columns`
			}
			w, path := made(t)
			before := change(t, path, statement)
			d := w.serve(t, "a", core.Options{})
			r, err := startRead(t, d)
			is.NotError(t, err)
			got, err := r.Get(t.Context(), "t1", row(1))
			is.NotError(t, err, statement)
			is.Equal(t, "1,1", text(got), statement)
			_, err = r.Indexes(t.Context(), "t1")
			is.Error(t, core.ErrCacheVersion, err, statement)
			is.True(t, strings.Contains(err.Error(), why), err.Error())
			is.True(t, strings.Contains(err.Error(), advice), err.Error())
			_ = r.Close(t.Context())
			is.True(t, isDone(d), statement)
			_ = d.Close(t.Context())
			unchanged(t, path, before, statement)
		}
	})

	t.Run("should accept every table and index that the core makes, of random shapes and names, after a close and an open and after a replay into an empty cache", func(t *testing.T) {
		rounds := 100
		if testing.Short() {
			rounds = 15
		}
		for round := range rounds {
			rng := rand.New(rand.NewPCG(uint64(round), 11))
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			// taken holds the folded names of the tables and indexes, which share their names.
			taken := map[string]bool{}
			for range 1 + rng.IntN(3) {
				write(t, d, func(tx *core.Tx) {
					for range 1 + rng.IntN(4) {
						ct := randomTable(rng, randomName(rng, taken))
						is.NotError(t, tx.CreateTable(t.Context(), ct), describe(ct))
						for range rng.IntN(5) {
							is.NotError(t, tx.Upsert(t.Context(), ct.GetTable(), randomValues(rng, ct)), describe(ct))
						}
						for range rng.IntN(4) {
							ci := randomTableIndex(rng, randomName(rng, taken), ct)
							is.NotError(t, tx.CreateIndex(t.Context(), ci), describeIndex(ci))
						}
					}
				})
			}
			want := read(t, d)
			is.NotError(t, d.Close(t.Context()))
			again := w.serve(t, "a", core.Options{})
			is.Equal(t, want, read(t, again), "after a close and an open, in round", round)
			is.NotError(t, again.Close(t.Context()))
			replayed := w.serve(t, "b", core.Options{})
			is.Equal(t, want, read(t, replayed), "after a replay, in round", round)
			is.NotError(t, replayed.Close(t.Context()))
		}
	})
}

// randomName of a table or an index that taken, the folded names that are in use, does not hold:
// one of [hostile], or random characters, which it adds to taken. The names that start with "s4ql_"
// or "sqlite_" are the core's and SQLite's, and are never drawn.
func randomName(rng *rand.Rand, taken map[string]bool) string {
	for {
		var name string
		if rng.IntN(2) == 0 {
			name = hostile[rng.IntN(len(hostile))]
		} else {
			name = randomText(rng, 1+rng.IntN(12))
		}
		if folded := foldName(name); !taken[folded] && !strings.HasPrefix(folded, "s4ql_") && !strings.HasPrefix(folded, "sqlite_") {
			taken[folded] = true
			return name
		}
	}
}

// randomText of n characters, valid UTF-8 without NUL: mostly printable ASCII, the quotes among it,
// and now and then a control character, a character of several bytes, or the largest one.
func randomText(rng *rand.Rand, n int) string {
	var b strings.Builder
	for range n {
		switch rng.IntN(10) {
		case 0:
			b.WriteRune(rune(1 + rng.IntN(31)))
		case 1:
			b.WriteRune([]rune{'é', '日', '🥪', 0x10ffff, 0x200b, '"', '\''}[rng.IntN(7)])
		default:
			b.WriteRune(rune(' ' + rng.IntN(95)))
		}
	}
	s := b.String()
	if !utf8.ValidString(s) {
		panic("not valid UTF-8")
	}
	return s
}

// randomTable of the given name: one to six columns of random names and types, a primary key of
// some of them in any order, and columns outside it that take NULL or not, each with a default of
// its type, a default of NULL, or none.
func randomTable(rng *rand.Rand, name string) *s4qlpb.CreateTable {
	types := []s4qlpb.Type{s4qlpb.Type_TYPE_INT, s4qlpb.Type_TYPE_INTEGER, s4qlpb.Type_TYPE_REAL, s4qlpb.Type_TYPE_TEXT, s4qlpb.Type_TYPE_BLOB}
	columns := 1 + rng.IntN(6)
	taken := map[string]bool{}
	ct := s4qlpb.CreateTable_builder{Table: new(name)}
	for range columns {
		column := s4qlpb.Column_builder{Name: new(randomName(rng, taken)), Type: new(types[rng.IntN(len(types))])}
		ct.Columns = append(ct.Columns, column.Build())
	}
	positions := rng.Perm(columns)
	for _, position := range positions[:1+rng.IntN(columns)] {
		ct.PrimaryKey = append(ct.PrimaryKey, uint32(position))
	}
	built := ct.Build()
	for i, c := range built.GetColumns() {
		if !slices.Contains(built.GetPrimaryKey(), uint32(i)) && rng.IntN(2) == 0 {
			c.SetNullable(true)
		}
		switch rng.IntN(4) {
		case 0:
			c.SetDefault(row(nil)[0])
		case 1, 2:
			c.SetDefault(randomValue(rng, c.GetType()))
		}
	}
	return built
}

// randomValue of the type: one of its [edges], or for a real now and then random bits that are a
// number.
func randomValue(rng *rand.Rand, typ s4qlpb.Type) *s4qlpb.Value {
	if typ == s4qlpb.Type_TYPE_REAL && rng.IntN(2) == 0 {
		for {
			if r := math.Float64frombits(rng.Uint64()); !math.IsNaN(r) && !math.IsInf(r, 0) {
				return row(r)[0]
			}
		}
	}
	pool := edges[typ]
	return pool[rng.IntN(len(pool))]
}

// randomValues of a row of the table, with NULL now and then in a column that takes it.
func randomValues(rng *rand.Rand, ct *s4qlpb.CreateTable) []*s4qlpb.Value {
	values := make([]*s4qlpb.Value, len(ct.GetColumns()))
	for i, c := range ct.GetColumns() {
		if c.GetNullable() && rng.IntN(4) == 0 {
			values[i] = row(nil)[0]
		} else {
			values[i] = randomValue(rng, c.GetType())
		}
	}
	return values
}

// randomTableIndex with the given name on the table: some of its columns, in any order, each in
// either direction.
func randomTableIndex(rng *rand.Rand, name string, ct *s4qlpb.CreateTable) *s4qlpb.CreateIndex {
	positions := rng.Perm(len(ct.GetColumns()))
	ci := s4qlpb.CreateIndex_builder{Index: new(name), Table: new(ct.GetTable())}
	for _, position := range positions[:1+rng.IntN(len(positions))] {
		column := s4qlpb.IndexColumn_builder{Column: new(uint32(position))}
		if rng.IntN(2) == 0 {
			column.Descending = new(true)
		}
		ci.Columns = append(ci.Columns, column.Build())
	}
	return ci.Build()
}
