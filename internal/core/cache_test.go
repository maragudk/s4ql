package core_test

import (
	"fmt"
	"iter"
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
