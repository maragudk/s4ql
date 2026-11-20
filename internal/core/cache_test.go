package core_test

import (
	"errors"
	"fmt"
	"iter"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sqlite"
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
				for _, bounds := range [][2]core.Bound{{c.b, {}}, {{}, c.b}} {
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
		for _, err := range r.ScanRange(t.Context(), "t4", core.Bound{Value: row(strings.Repeat("x", 1001))[0]}, core.Bound{}) {
			is.True(t, errors.Is(err, core.ErrTooBig), fmt.Sprint(err))
		}
		is.NotError(t, r.Close(t.Context()))
		is.NotError(t, d.Close(t.Context()))
	})
}
