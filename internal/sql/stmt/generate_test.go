package stmt_test

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"testing"

	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sql/stmt/stmttest"
)

// keywords of SQLite, in lower case.
func keywords(testing.TB) []string {
	return stmttest.Keywords()
}

// namePools from [stmttest.NamePools].
func namePools(t testing.TB) [][]string {
	t.Helper()
	pools, err := stmttest.NamePools()
	if err != nil {
		t.Fatal(err)
	}
	return pools
}

// positionTexts are those of [stmttest.PositionTexts].
var positionTexts = stmttest.PositionTexts

// newGenerator of [stmttest] from seed, which writes a plain ? in place of one value of an insert
// in four, columns and values of every type, the forms of slice 5 with up to three columns, and those
// of slice 6, with a column that takes NULL and a NULL one time in five.
func newGenerator(t testing.TB, seed uint64) *stmttest.Generator {
	g := stmttest.NewGenerator(rand.New(rand.NewPCG(seed, 0x511ce1)), namePools(t))
	g.Parameters = 4
	g.Columns = 3
	g.Nulls = 5
	return g
}

var generatedCount = flag.Int("generated", 20000, "statements of the generator of slice 1")

// TestGenerate checks that every statement of the generator of slice 1 is supported, as the statement
// that the generator wrote, and does that in SQLite.
func TestGenerate(t *testing.T) {
	n := *generatedCount
	if slow() {
		n /= 10
	}
	g := newGenerator(t, *seed)
	for range n {
		src, want := g.Statement()
		s, err := stmt.Check(src)
		if err != nil || fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
			t.Fatalf("%q: gives %#v %v, want %#v", src, s, err, want)
		}
		verify(t, src)
	}
	for p, pool := range namePools(t) {
		t.Logf("%v names in position %q", len(pool), positionTexts[p])
	}
}

var mutantsEach = flag.Int("mutants", 5, "mutants of each statement in TestGenerate_mutants")

// TestGenerate_mutants checks the pass against SQLite on one-token mutants of statements of the
// generator of slice 1: each is a syntax error as in SQLite, not supported, or supported and does in
// SQLite what it says. It logs the mutants that are still supported, by how they differ from the
// statement they come from.
func TestGenerate_mutants(t *testing.T) {
	n := *generatedCount
	if slow() {
		n /= 10
	}
	g := newGenerator(t, *seed+1)
	c := counts{}
	kinds := map[string][]string{}
	for i := range n {
		src, want := g.Statement()
		for _, m := range parsetest.Mutants(src, *seed+uint64(i), *mutantsEach) {
			o, s := verify(t, m)
			c[o]++
			if o == supported {
				kind := difference(want, s)
				kinds[kind] = append(kinds[kind], fmt.Sprintf("%q from %q", m, src))
			}
		}
	}
	t.Logf("%v mutants: %v", n**mutantsEach, c)
	excused(t, c, 77, 305)
	var names []string
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t.Logf("supported, %v: %v, for example:", k, len(kinds[k]))
		for _, e := range kinds[k][:min(3, len(kinds[k]))] {
			t.Logf("  %v", e)
		}
	}
}

// difference between the statement a, which a text of the generator holds, and the statement b,
// which a mutant of it holds.
func difference(a, b stmt.Statement) string {
	if fmt.Sprintf("%T", a) != fmt.Sprintf("%T", b) {
		return fmt.Sprintf("another kind of statement, %T", b)
	}
	var diffs []string
	names := func(a, b []stmt.Name, what string) {
		switch {
		case (a == nil) != (b == nil):
			diffs = append(diffs, what+" or none")
		case len(a) != len(b):
			diffs = append(diffs, "number of "+what+"s")
		default:
			for i := range a {
				if a[i].Value != b[i].Value {
					diffs = append(diffs, what+" name")
					break
				}
			}
		}
	}
	switch a := a.(type) {
	case *stmt.CreateTable:
		b := b.(*stmt.CreateTable)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if len(a.Columns) != len(b.Columns) {
			diffs = append(diffs, "number of columns")
			break
		}
		var an, bn []stmt.Name
		for i := range a.Columns {
			an, bn = append(an, a.Columns[i].Name), append(bn, b.Columns[i].Name)
			if a.Columns[i].Type != b.Columns[i].Type {
				diffs = append(diffs, "type")
			}
			if a.Columns[i].PrimaryKey != b.Columns[i].PrimaryKey || a.Columns[i].NotNull != b.Columns[i].NotNull {
				diffs = append(diffs, "constraint")
			}
		}
		names(an, bn, "column")
	case *stmt.Insert:
		b := b.(*stmt.Insert)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		names(a.Columns, b.Columns, "column")
		if len(a.Values) != len(b.Values) {
			diffs = append(diffs, "number of values")
			break
		}
		for i := range a.Values {
			a, b := a.Values[i], b.Values[i]
			switch {
			case a.Parameter != b.Parameter:
				diffs = append(diffs, "parameter")
			case fmt.Sprintf("%T", a.Value) != fmt.Sprintf("%T", b.Value):
				diffs = append(diffs, "kind of value")
			case show(a.Value) != show(b.Value):
				diffs = append(diffs, "value")
			}
		}
	case *stmt.Select:
		b := b.(*stmt.Select)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		names(a.Columns, b.Columns, "result column")
	case *stmt.DropTable:
		if a.Table.Value != b.(*stmt.DropTable).Table.Value {
			diffs = append(diffs, "table name")
		}
	}
	if diffs == nil {
		return fmt.Sprintf("the same %T", a)
	}
	slices.Sort(diffs)
	return fmt.Sprintf("%T with another %v", a, strings.Join(slices.Compact(diffs), " and "))
}

// FuzzGenerate checks the pass against SQLite on a statement of the generator of slice 1 and a
// mutant of it, from seeds that the fuzzer chooses.
func FuzzGenerate(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Fuzz(func(t *testing.T, seed, mutant uint64) {
		src, want := newGenerator(t, seed).Statement()
		s, err := stmt.Check(src)
		if err != nil || fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
			t.Fatalf("%q: gives %#v %v, want %#v", src, s, err, want)
		}
		verify(t, src)
		for _, m := range parsetest.Mutants(src, mutant, 1) {
			verify(t, m)
		}
	})
}

// nearMisses of the four forms: each is the form with one clause, keyword, or item added or changed,
// which a one-token mutant cannot make, or a literal that is not supported or is a syntax error. %[1]v
// is the table, %[2]v the column, %[3]v the text of the value, and %[4]v the type of the column.
var nearMisses = map[string][]string{
	"*stmt.CreateTable": {
		"explain create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"explain query plan create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temp table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temporary table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not exists %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table main.%[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table temp.%[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v unsigned primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v(10) primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v %[4]v primary key) strict, without rowid",
		`create table %[1]v (%[2]v "%[4]v" primary key) strict, without rowid`,
		"create table %[1]v (%[2]v '%[4]v' primary key) strict, without rowid",
		"create table %[1]v (%[2]v [%[4]v] primary key) strict, without rowid",
		"create table %[1]v (%[2]v any primary key) strict, without rowid",
		"create table %[1]v (%[2]v varchar primary key) strict, without rowid",
		"create table %[1]v (%[2]v double primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]vs primary key) strict, without rowid",
		"create table %[1]v (%[2]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key unique) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key collate binary) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key references u (x)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key references u deferrable initially deferred) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key asc) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key desc) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key on conflict fail) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key autoincrement) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key generated always as (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key constraint c) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v constraint c primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key not null not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v not null primary key null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key not null on conflict fail) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int default null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v, w int) strict, without rowid",
		"create table %[1]v (w int null, %[2]v %[4]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v not null, w int not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null default 1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null unique) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null check (w > 0)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null collate binary) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null references u) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null on conflict ignore) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null generated always as (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w varchar not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null, primary key (w)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null, unique (w)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key,) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, , w int not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, unique (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v, primary key (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key) strict",
		"create table %[1]v (%[2]v %[4]v primary key) without rowid",
		"create table %[1]v (%[2]v %[4]v primary key)",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid, strict",
		"create table %[1]v (%[2]v %[4]v primary key) strict, strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key), strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid, foo",
		"create table %[1]v as select 1",
		"create table %[1]v (%[2]v %[4]v primary key) strict, without rowid; select 1",
	},
	"*stmt.Insert": {
		"explain insert into %[1]v (%[2]v) values (%[3]v)",
		"with x as (select 1) insert into %[1]v (%[2]v) values (%[3]v)",
		"replace into %[1]v (%[2]v) values (%[3]v)",
		"insert or replace into %[1]v (%[2]v) values (%[3]v)",
		"insert or ignore into %[1]v (%[2]v) values (%[3]v)",
		"insert or abort into %[1]v (%[2]v) values (%[3]v)",
		"insert into main.%[1]v (%[2]v) values (%[3]v)",
		"insert into %[1]v as x (%[2]v) values (%[3]v)",
		"insert into %[1]v as x values (%[3]v)",
		"insert into main.%[1]v values (%[3]v)",
		"insert into %[1]v (%[2]v, %[2]v) values (%[3]v, %[3]v)",
		"insert into %[1]v (%[2]v, w, %[2]v) values (%[3]v, 1, %[3]v)",
		"insert into %[1]v (w, %[2]v, W) values (1, %[3]v, 2)",
		"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
		"insert into %[1]v values (%[3]v), (%[3]v)",
		"insert into %[1]v values (%[3]v, - null)",
		"insert into %[1]v values (%[3]v, nullif(1, 1))",
		"insert into %[1]v values (%[3]v, 1 + 1)",
		"insert into %[1]v values (%[3]v, ?1)",
		"insert into %[1]v values (?, :a)",
		"insert into %[1]v values (%[3]v, -?)",
		"insert into %[1]v (%[2]v, w) values (%[3]v, (1))",
		"insert into %[1]v (%[2]v, w) select %[3]v, 1",
		"insert into %[1]v select %[3]v",
		"insert into %[1]v (%[2]v,) values (%[3]v)",
		"insert into %[1]v values (%[3]v,)",
		"insert into %[1]v () values (%[3]v)",
		"insert into %[1]v values ()",
		"insert into %[1]v values (%[3]v) returning *",
		"insert into %[1]v (%[2]v, [w]) values (%[3]v, 1)",
		"insert into %[1]v (%[2]v, '') values (%[3]v, 1)",
		"insert into %[1]v (%[2]v) values ((%[3]v))",
		"insert into %[1]v (%[2]v) values (+%[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v + 0)",
		"insert into %[1]v (%[2]v) values (- (%[3]v))",
		"insert into %[1]v (%[2]v) values (%[3]v collate binary)",
		"insert into %[1]v (%[2]v) values (cast(%[3]v as int))",
		"insert into %[1]v (%[2]v) values (?1)",
		"insert into %[1]v (%[2]v) values (:a)",
		"insert into %[1]v (%[2]v) values (-?)",
		"insert into %[1]v (%[2]v) values (?, ?2)",
		"insert into %[1]v (%[2]v) select %[3]v",
		"insert into %[1]v (%[2]v) values (%[3]v) union select 1",
		"insert into %[1]v (%[2]v) values (%[3]v) returning *",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict do nothing",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict (%[2]v) do update set %[2]v = 1",
		"insert into %[1]v default values",
		"insert into %[1]v (%[2]v) values (%[3]v); select 1",
		"insert into %[1]v (%[2]v) values (%[3]v || %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]vx)",
		"insert into %[1]v (%[2]v) values (%[3]v')",
		"insert into %[1]v (%[2]v) values ('a)",
		"insert into %[1]v (%[2]v) values ('a'')",
		"insert into %[1]v (%[2]v) values ('a' 'b')",
		"insert into %[1]v (%[2]v) values (- 'a')",
		"insert into %[1]v (%[2]v) values (-x'01')",
		"insert into %[1]v (%[2]v) values (x'6')",
		"insert into %[1]v (%[2]v) values (x'6g')",
		"insert into %[1]v (%[2]v) values (x '61')",
		"insert into %[1]v (%[2]v) values (1e)",
		"insert into %[1]v (%[2]v) values (1e+)",
		"insert into %[1]v (%[2]v) values (1.5.5)",
		"insert into %[1]v (%[2]v) values (.)",
		"insert into %[1]v (%[2]v) values (1e999)",
		"insert into %[1]v (%[2]v) values (-1e999)",
		"insert into %[1]v (%[2]v) values (1_000.5)",
		"insert into %[1]v (%[2]v) values (0x1p3)",
		"insert into %[1]v (%[2]v) values (+null)",
		"insert into %[1]v (%[2]v) values (-null)",
		"insert into %[1]v (%[2]v) values (%[3]v is null)",
		"insert into %[1]v (%[2]v) values (%[3]v is not null)",
		"insert into %[1]v (%[2]v) values (%[3]v isnull)",
		"insert into %[1]v (%[2]v) values (%[3]v notnull)",
		"insert into %[1]v (%[2]v) values (%[3]v not null)",
		"insert into %[1]v (%[2]v) values (cast(null as int))",
		"insert into %[1]v (%[2]v) values (true)",
		"insert into %[1]v (%[2]v) values (- false)",
		`insert into %[1]v (%[2]v) values ("a")`,
		"insert into %[1]v (%[2]v) values (cast('a' as text))",
	},
	"*stmt.Select": {
		"explain select * from %[1]v",
		"with x as (select 1) select * from %[1]v",
		"select distinct * from %[1]v",
		"select all * from %[1]v",
		"select *, * from %[1]v",
		"select %[1]v.* from %[1]v",
		"select 1 from %[1]v",
		"select %[2]v, * from %[1]v",
		"select *, %[2]v from %[1]v",
		"select %[2]v, %[1]v.* from %[1]v",
		"select %[2]v as x from %[1]v",
		"select %[2]v x from %[1]v",
		"select %[2]v, %[2]v as x from %[1]v",
		"select %[1]v.%[2]v from %[1]v",
		"select main.%[1]v.%[2]v from %[1]v",
		"select %[2]v + 1 from %[1]v",
		"select (%[2]v) from %[1]v",
		"select -%[2]v from %[1]v",
		"select %[2]v, 1 from %[1]v",
		"select %[2]v, null from %[1]v",
		"select %[2]v, ? from %[1]v",
		"select %[2]v collate nocase from %[1]v",
		"select %[2]v is null from %[1]v",
		"select null from %[1]v",
		"select * from %[1]v where %[2]v is null",
		"select %[2]v from %[1]v where %[2]v is not null",
		"select count(%[2]v) from %[1]v",
		"select true from %[1]v",
		"select %[2]v, false from %[1]v",
		"select '%[2]v' from %[1]v",
		"select [%[2]v] from %[1]v",
		"select distinct %[2]v from %[1]v",
		"select %[2]v from %[1]v where 1",
		"select %[2]v from %[1]v order by %[2]v",
		"select %[2]v from %[1]v limit 1",
		"select %[2]v from %[1]v, u",
		"select %[2]v",
		"select %[2]v, from %[1]v",
		"select , %[2]v from %[1]v",
		"select * from main.%[1]v",
		"select * from %[1]v as x",
		"select * from %[1]v x",
		"select * from %[1]v indexed by i",
		"select * from %[1]v not indexed",
		"select * from (%[1]v)",
		"select * from (select * from %[1]v)",
		"select * from %[1]v, u",
		"select * from %[1]v join u",
		"select * from %[1]v natural join u",
		"select * from %[1]v left join u on 1",
		"select * from %[1]v where 1",
		"select * from %[1]v group by 1",
		"select * from %[1]v group by 1 having 1",
		"select * from %[1]v window w as ()",
		"select * from %[1]v order by 1",
		"select * from %[1]v limit 1",
		"select * from %[1]v limit 1 offset 1",
		"select * from %[1]v union select * from %[1]v",
		"select * from %[1]v union all select * from %[1]v",
		"select * from %[1]v except select * from %[1]v",
		"select * from %[1]v; select 1",
	},
	"*stmt.DropTable": {
		"explain drop table %[1]v",
		"drop table if exists %[1]v",
		"drop table main.%[1]v",
		"drop table temp.%[1]v",
		"drop view %[1]v",
		"drop index %[1]v",
		"drop trigger %[1]v",
		"drop table %[1]v; select 1",
	},
}

// TestGenerate_nearMisses checks that no near miss of a statement of the generator of slice 1 is
// supported: each is a syntax error as in SQLite, or not supported.
func TestGenerate_nearMisses(t *testing.T) {
	n := *generatedCount / 10
	if slow() {
		n /= 10
	}
	// The forms of the near misses write the value of an insert themselves, so the generator writes no
	// parameter.
	g := newGenerator(t, *seed+3)
	g.Parameters = 0
	c := counts{}
	for range n {
		src, s := g.Statement()
		// The column of a form is the primary key of a create table, and the first column that an
		// insert or a select names, or v.
		table, column, value, typ := "", "v", "", ""
		switch s := s.(type) {
		case *stmt.CreateTable:
			key := s.Columns[s.Key()]
			table, column, typ = s.Table.Raw, key.Name.Raw, key.Type.String()
		case *stmt.Insert:
			table, value = s.Table.Raw, src[s.Values[0].Span.Start:s.Values[0].Span.End]
			if s.Columns != nil {
				column = s.Columns[0].Raw
			}
		case *stmt.Select:
			table = s.Table.Raw
			if s.Columns != nil {
				column = s.Columns[0].Raw
			}
		case *stmt.DropTable:
			table = s.Table.Raw
		}
		for _, form := range nearMisses[fmt.Sprintf("%T", s)] {
			src := fmt.Sprintf(form, table, column, value, typ)
			o, _ := verify(t, src)
			if o == supported {
				t.Fatalf("%q, a near miss of slice 1, is supported", src)
			}
			c[o]++
		}
	}
	t.Logf("%v near misses: %v", c[syntaxError]+c[notSupported]+c[syntaxNotSaid]+c[notSupportedNotSaid], c)
	excused(t, c, 0, 2942)
}
