package engine_test

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// indexPair is the engine and SQLite, which run the same statements, for the tests of indexes.
type indexPair struct {
	t *testing.T
	i *instance
	o *oracle
	// open reports whether a transaction is open on both.
	open bool
}

// newIndexPair with the length limit limit, 0 for SQLite's default, and the statements of setup run
// on both.
func newIndexPair(t *testing.T, limit int, setup ...string) *indexPair {
	t.Helper()
	o := newOracle(t)
	if limit > 0 {
		o = newOracleLimited(t, limit)
	}
	p := &indexPair{t: t, i: newInstanceLimited(t, engine.Options{}, nil, limit), o: o}
	for _, src := range setup {
		is.Equal(t, "", p.run(src).err, src)
	}
	return p
}

// run src with args on both, as a query if it is a select, and require of the engine SQLite's
// answer, with its rows as a multiset, and SQLite's count of changes, and, outside a transaction,
// SQLite's rows and SQLite's indexes afterwards. It gives SQLite's answer.
func (p *indexPair) run(src string, args ...any) answer {
	p.t.Helper()
	what := fmt.Sprintf("%v with %#v", src, args)
	want := p.o.run(p.t, src, oracleArgs(args)...)
	var got answer
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(src)), "select") {
		got = p.i.queryValues(src, values(args))
	} else {
		want.columns, want.rows = "", nil
		got = p.i.execValues(src, values(args))
	}
	is.Equal(p.t, want.String(), got.String(), "SQLite and the engine: "+what)
	changes, _ := p.o.changes(p.t)
	is.Equal(p.t, changes, p.i.changes, "the count of changes after "+what)
	if !p.open {
		p.same(what)
	}
	return want
}

// same requires that the engine holds SQLite's rows and SQLite's indexes.
func (p *indexPair) same(what string) {
	p.t.Helper()
	is.Equal(p.t, p.o.state(p.t), p.i.state(), "the state after "+what)
	is.Equal(p.t, p.o.indexes(p.t), p.i.indexes(), "the indexes after "+what)
}

// begin a transaction on both, read-only if readOnly.
func (p *indexPair) begin(readOnly bool) {
	p.t.Helper()
	if readOnly {
		is.NotError(p.t, p.i.c.BeginReadOnly(p.t.Context()))
	} else {
		is.NotError(p.t, p.i.c.Begin(p.t.Context()))
	}
	for _, s := range oracleBegin(readOnly) {
		is.Equal(p.t, "", p.o.run(p.t, s).err, s)
	}
	p.open = true
}

// end the transaction on both, with a commit if commit and with a roll back otherwise, and require
// the same rows and indexes afterwards.
func (p *indexPair) end(readOnly, commit bool) {
	p.t.Helper()
	if commit {
		is.NotError(p.t, p.i.c.Commit(p.t.Context()))
	} else {
		is.NotError(p.t, p.i.c.Rollback(p.t.Context()))
	}
	for _, s := range oracleEnd(readOnly, commit) {
		is.Equal(p.t, "", p.o.run(p.t, s).err, s)
	}
	p.open = false
	p.same("the end of the transaction")
}

// indexes of SQLite's database: a line for each table that has one, in the order of the names as
// bytes, with its indexes in the order in which they were created, each as its name and its columns,
// with "desc" after a column in descending order.
func (o *oracle) indexes(t testing.TB) string {
	t.Helper()
	var b strings.Builder
	tables := o.run(t, "select name from sqlite_schema where type = 'table' order by name collate binary")
	is.Equal(t, "", tables.err)
	for _, table := range tables.rows {
		list := o.run(t, "select name from sqlite_schema where type = 'index' and tbl_name = ? order by rowid", unquoted(t, table))
		is.Equal(t, "", list.err)
		var indexes []string
		for _, name := range list.rows {
			columns := o.run(t, `select name, "desc" from pragma_index_xinfo(?) where key = 1 order by seqno`, unquoted(t, name))
			is.Equal(t, "", columns.err)
			var cs []string
			for _, c := range columns.rows {
				column, desc := c[:strings.LastIndex(c, ",")], c[strings.LastIndex(c, ",")+1:]
				if desc == "1" {
					column += " desc"
				}
				cs = append(cs, column)
			}
			indexes = append(indexes, name+" ("+strings.Join(cs, ", ")+")")
		}
		if len(indexes) > 0 {
			fmt.Fprintf(&b, "%v: %v\n", table, strings.Join(indexes, " "))
		}
	}
	return b.String()
}

// unquoted is the text of a value that an answer holds in quotes.
func unquoted(t testing.TB, quoted string) string {
	t.Helper()
	text, err := strconv.Unquote(quoted)
	is.NotError(t, err)
	return text
}

// indexes of the database in the engine's instance, as [oracle.indexes] gives them, read from the
// core.
func (i *instance) indexes() string {
	i.t.Helper()
	ctx := i.t.Context()
	rd, err := i.d.Reader(ctx)
	is.NotError(i.t, err)
	defer func() { is.NotError(i.t, rd.Close(ctx)) }()
	r, err := rd.Read(ctx)
	is.NotError(i.t, err)
	defer func() { is.NotError(i.t, r.Close(ctx)) }()
	tables, err := r.Tables(ctx)
	is.NotError(i.t, err)
	var b strings.Builder
	quote := strconv.Quote
	for _, ct := range tables {
		cis, err := r.Indexes(ctx, ct.GetTable())
		is.NotError(i.t, err)
		var indexes []string
		for _, ci := range cis {
			var cs []string
			for _, c := range ci.GetColumns() {
				column := quote(ct.GetColumns()[c.GetColumn()].GetName())
				if c.GetDescending() {
					column += " desc"
				}
				cs = append(cs, column)
			}
			indexes = append(indexes, quote(ci.GetIndex())+" ("+strings.Join(cs, ", ")+")")
		}
		if len(indexes) > 0 {
			fmt.Fprintf(&b, "%v: %v\n", quote(ct.GetTable()), strings.Join(indexes, " "))
		}
	}
	return b.String()
}

const indexTable = "create table t (k int primary key, a int not null, b text, c real) strict, without rowid"

func TestEngine_indexes(t *testing.T) {
	t.Run("should create an index on a table with rows and drop it, as SQLite does, and leave the rows and the count of changes as they are", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "insert into t values (1, 10, 'x', 1.5)", "insert into t values (2, 20, null, null)")

		is.Equal(t, "", p.run("create index i on t (a, b desc)").err)
		is.Equal(t, "\"t\": \"i\" (\"a\", \"b\" desc)\n", p.i.indexes())
		is.Equal(t, 2, len(p.run("select * from t where a > 5").rows))
		is.Equal(t, "", p.run("insert into t values (3, 30, 'z', 2.5)").err)
		is.Equal(t, "", p.run("update t set a = 11 where a = 10").err)
		is.Equal(t, 1, len(p.run("select * from t where a = 11").rows))
		is.Equal(t, "", p.run("delete from t where a = 20").err)
		is.Equal(t, "", p.run("drop index i").err)
		is.Equal(t, "", p.i.indexes())
		is.Equal(t, 2, len(p.run("select * from t where a > 5").rows))
	})

	t.Run("should keep the indexes of a table in the order in which they were created, with ASC as no direction, on any column and on the key", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "create table u (k text primary key, v blob) strict, without rowid")

		for _, src := range []string{
			"create index i3 on t (c asc)", "create index I1 on t (b desc, a)", "create index \"i 2\" on t (k desc, c, b, a)",
			"create index u1 on u (v)", "drop index i1", "create index i1 on T (A)", "create index i4 on u (K, V desc)",
		} {
			is.Equal(t, "", p.run(src).err, src)
		}

		want := "\"t\": \"i3\" (\"c\") \"i 2\" (\"k\" desc, \"c\", \"b\", \"a\") \"i1\" (\"a\")\n\"u\": \"u1\" (\"v\") \"i4\" (\"k\", \"v\" desc)\n"
		is.Equal(t, want, p.i.indexes())
		p.i.restart(true, engine.Options{})
		is.Equal(t, want, p.i.indexes(), "after a replay of the log into an empty cache")
		p.i.restart(false, engine.Options{})
		is.Equal(t, want, p.i.indexes(), "after a restart on the same cache")
	})

	t.Run("should give SQLite's errors of a create index and of a drop index, in SQLite's order", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "create table u (k int primary key) strict, without rowid", "create index i on t (a)")

		for _, c := range []struct{ src, want string }{
			{"create index j on nosuch (a)", "1/1: no such table: main.nosuch"},
			{`create index j on "No Such" (a)`, "1/1: no such table: main.No Such"},
			{"create index t on nosuch (a)", "1/1: no such table: main.nosuch"},
			{"create index i on nosuch (a)", "1/1: no such table: main.nosuch"},
			{"create index t on t (a)", "1/1: there is already a table named t"},
			{"create index T on u (nosuch)", "1/1: there is already a table named T"},
			{`create index "U" on t (nosuch)`, "1/1: there is already a table named U"},
			{"create index i on t (b)", "1/1: index i already exists"},
			{"create index I on u (nosuch)", "1/1: index I already exists"},
			{`create index "I" on u (k)`, "1/1: index I already exists"},
			{"create index j on t (nosuch)", "1/1: no such column: nosuch"},
			{"create index j on t (a, z desc, y)", "1/1: no such column: z"},
			{`create index j on t (a, "z", y)`, `1/1: no such column: "z" - should this be a string literal in single-quotes?`},
			{"create index j on u (a)", "1/1: no such column: a"},
			{"drop index nosuch", "1/1: no such index: nosuch"},
			{"drop index t", "1/1: no such index: t"},
			{`drop index "No Such"`, "1/1: no such index: No Such"},
			{"create table i (k int primary key) strict, without rowid", "1/1: there is already an index named i"},
			{`create table "I" (k int primary key, k int) strict, without rowid`, "1/1: there is already an index named I"},
			{"create table t (k int primary key, k int) strict, without rowid", "1/1: table t already exists"},
		} {
			is.Equal(t, c.want, p.run(c.src).err, c.src)
		}
		is.Equal(t, "\"t\": \"i\" (\"a\")\n", p.i.indexes())
	})

	t.Run("should give the offset of SQLite's error of a create index and of a drop index", func(t *testing.T) {
		for _, src := range []string{
			"create index j on nosuch (a)", "create index t on t (a)", "create index i on t (b)", "create index j on t (a,  nosuch)",
			`create index j on t ("nosuch")`, "drop index nosuch", "create table i (k int primary key) strict, without rowid",
		} {
			// A fresh connection to SQLite, since SQLite does not give the same offset on every one.
			p := newIndexPair(t, 0, indexTable, "create index i on t (a)")
			want := p.o.run(t, src)
			got := p.i.exec(src)
			is.Equal(t, want.String(), got.String(), src)
			is.Equal(t, want.offset, got.offset, src)
		}
	})

	t.Run("should drop the indexes of a table with the table, and give their names to new tables and indexes", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "create table u (k int primary key, v int) strict, without rowid",
			"create index i on t (a)", "create index j on t (b)", "create index ui on u (v)")

		is.Equal(t, "", p.run("drop table t").err)
		is.Equal(t, "\"u\": \"ui\" (\"v\")\n", p.i.indexes())
		is.Equal(t, "1/1: no such index: i", p.run("drop index i").err)
		is.Equal(t, "", p.run("create table i (k int primary key, a int) strict, without rowid").err)
		is.Equal(t, "", p.run("create index j on i (a)").err)
		is.Equal(t, "", p.run("create index t on u (k)").err)
		is.Equal(t, "1/1: there is already an index named t", p.run(indexTable).err)

		p.i.restart(true, engine.Options{})
		p.same("a replay of the log into an empty cache")
	})

	t.Run("should create and drop indexes in a transaction, undo them with its roll back, and keep the transaction open after one that fails", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "insert into t values (1, 10, 'x', 1.5)", "create index kept on t (c)")

		p.begin(false)
		is.Equal(t, "", p.run("create index i on t (a)").err)
		is.Equal(t, "1/1: index i already exists", p.run("create index i on t (b)").err)
		is.Equal(t, "", p.run("insert into t values (2, 20, 'y', 2.5)").err)
		is.Equal(t, 1, len(p.run("select k from t where a = 20").rows))
		is.Equal(t, "", p.run("drop index kept").err)
		is.Equal(t, "1/1: no such index: kept", p.run("drop index kept").err)
		p.end(false, false)
		is.Equal(t, "\"t\": \"kept\" (\"c\")\n", p.i.indexes())

		p.begin(false)
		is.Equal(t, "", p.run("create index i on t (a)").err)
		is.Equal(t, "", p.run("drop index i").err)
		is.Equal(t, "", p.run("create index i on t (b desc)").err)
		is.Equal(t, "", p.run("drop index kept").err)
		is.Equal(t, "", p.run("create index kept on t (c desc, a)").err)
		is.Equal(t, "1/1: no such column: z", p.run("create index j on t (z)").err)
		is.Equal(t, "", p.run("insert into t values (2, 20, 'y', 2.5)").err)
		p.end(false, true)
		want := "\"t\": \"i\" (\"b\" desc) \"kept\" (\"c\" desc, \"a\")\n"
		is.Equal(t, want, p.i.indexes())

		p.i.restart(true, engine.Options{})
		p.same("a replay of the log into an empty cache")
		is.Equal(t, want, p.i.indexes())
	})

	t.Run("should refuse a create index and a drop index in a read-only transaction with SQLite's error, after the errors of the names", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "create index i on t (a)")

		p.begin(true)
		for _, c := range []struct{ src, want string }{
			{"create index j on nosuch (a)", "1/1: no such table: main.nosuch"},
			{"create index t on t (a)", "1/1: there is already a table named t"},
			{"create index i on t (b)", "1/1: index i already exists"},
			{"create index j on t (z)", "1/1: no such column: z"},
			{"create index j on t (b)", "8/8: attempt to write a readonly database"},
			{"drop index nosuch", "1/1: no such index: nosuch"},
			{"drop index i", "8/8: attempt to write a readonly database"},
			{"create table i (k int primary key) strict, without rowid", "1/1: there is already an index named i"},
		} {
			is.Equal(t, c.want, p.run(c.src).err, c.src)
		}
		p.end(true, true)
		is.Equal(t, "\"t\": \"i\" (\"a\")\n", p.i.indexes())
	})

	t.Run("should refuse a create index and a drop index while rows of the transaction are open", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "insert into t values (1, 10, 'x', 1.5)", "create index i on t (a)")
		is.NotError(t, p.i.c.Begin(t.Context()))

		rows, err := p.i.c.Query(t.Context(), "select * from t")
		is.NotError(t, err)
		_, err = p.i.c.Exec(t.Context(), "create index j on t (b)")
		is.Error(t, engine.ErrRowsOpen, err)
		_, err = p.i.c.Exec(t.Context(), "drop index i")
		is.Error(t, engine.ErrRowsOpen, err)
		is.NotError(t, rows.Close())
		_, err = p.i.c.Exec(t.Context(), "create index j on t (b)")
		is.NotError(t, err)
		is.NotError(t, p.i.c.Commit(t.Context()))

		is.Equal(t, "\"t\": \"i\" (\"a\") \"j\" (\"b\")\n", p.i.indexes())
	})

	t.Run("should take an index of 250 columns, as SQLite does, and refuse one of 251 as not supported, after SQLite's errors", func(t *testing.T) {
		columns := make([]string, 252)
		for i := range columns {
			columns[i] = fmt.Sprint("c", i)
		}
		create := "create table w (" + strings.Join(columns, " int, ") + " int, k int primary key) strict, without rowid"
		p := newIndexPair(t, 0, create, "create index taken on w (c0)")
		list := func(n int) string { return strings.Join(columns[:n], ", ") }

		is.Equal(t, "", p.run("create index i on w ("+list(250)+")").err)

		for _, c := range []struct{ src, want string }{
			{"create index j on nosuch (" + list(251) + ")", "1/1: no such table: main.nosuch"},
			{"create index w on w (" + list(251) + ")", "1/1: there is already a table named w"},
			{"create index taken on w (" + list(251) + ")", "1/1: index taken already exists"},
			{"create index j on w (" + list(251) + ", nosuch)", "1/1: no such column: nosuch"},
		} {
			is.Equal(t, c.want, p.i.exec(c.src).err, c.src)
			is.Equal(t, c.want, p.o.run(t, c.src).err, c.src)
		}

		src := "create index j on w (" + list(251) + ")"
		_, err := p.i.c.Exec(t.Context(), src)
		is.Error(t, stmt.ErrNotSupported, err)
		ns, ok := errors.AsType[*stmt.NotSupportedError](err)
		is.True(t, ok)
		is.Equal(t, "index of more than 250 columns", ns.What)
		is.Equal(t, strings.Index(src, "c250"), ns.Offset)
		is.Equal(t, "", p.o.run(t, src).err, "SQLite takes it")
	})

	t.Run("should refuse a create index that SQLite cannot write into its schema under the length limit, exactly where SQLite does", func(t *testing.T) {
		const limit = 1000
		for _, form := range []string{
			"create index %v on t (a)", "create index %v on t (a) ;", "create index %v on t (a) -- end", "  create index %v on t (a, b desc, c)",
			`create index "%v" on t (a)`, `create index "it's %v" on t (a)`, "create index %v on \"it's\" (a)",
		} {
			var refused, taken int
			for n := 400; n <= 480; n++ {
				p := newIndexPair(t, limit, indexTable, `create table "it's" (a int primary key) strict, without rowid`)
				src := fmt.Sprintf(form, strings.Repeat("i", n))
				switch err := p.run(src).err; err {
				case "":
					taken++
				case "18/18: string or blob too big":
					refused++
				default:
					t.Fatalf("%q: SQLite gives %v", src, err)
				}
			}
			is.True(t, refused > 0 && taken > 0, fmt.Sprintf("%q: SQLite refuses %v and takes %v", form, refused, taken))
		}
	})

	t.Run("should use an index that another instance created, after a replay of the log, with the same choice of access", func(t *testing.T) {
		p := newIndexPair(t, 0, indexTable, "create index ia on t (a)", "create index iab on t (a, b)",
			"insert into t values (1, 10, 'x', 1.5)", "insert into t values (2, 10, 'y', 2.5)", "insert into t values (3, 20, 'x', null)")
		access := func(src string) string {
			rows, err := p.i.c.Query(t.Context(), src)
			is.NotError(t, err)
			a, ok := rows.Access()
			is.True(t, ok, src)
			is.NotError(t, rows.Close())
			return a.String()
		}
		queries := []struct{ src, want string }{
			{"select * from t where a = 10", "a scan of the index ia with 1 pinned"},
			{"select * from t where a = 10 and b = 'x'", "a scan of the index iab with 2 pinned"},
			{"select * from t where a = 10 and b > 'x'", "a scan of the index iab with 1 pinned from a lower bound"},
			{"select * from t where a = 10 and k < 2", "a scan of the index ia with 1 pinned to an upper bound"},
			{"select * from t where b = 'x'", "a scan of every row"},
			{"select * from t where k = 2 and a = 10", "a get of one key"},
		}
		for _, q := range queries {
			is.Equal(t, q.want, access(q.src), q.src)
			p.run(q.src)
		}

		p.i.restart(true, engine.Options{})

		p.same("a replay of the log into an empty cache")
		for _, q := range queries {
			is.Equal(t, q.want, access(q.src), "after the replay: "+q.src)
			p.run(q.src)
		}
	})
}
