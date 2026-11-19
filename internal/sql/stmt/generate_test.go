package stmt_test

import (
	"flag"
	"fmt"
	"math/rand/v2"
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

// generator of random statements of slice 1, from [stmttest].
type generator struct {
	*stmttest.Generator
}

func newGenerator(t testing.TB, seed uint64) generator {
	return generator{stmttest.NewGenerator(rand.New(rand.NewPCG(seed, 0x511ce1)), namePools(t))}
}

// statement of slice 1 at random: its text and the statement that it holds.
func (g generator) statement() (string, stmt.Statement) {
	return g.Statement()
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
		src, want := g.statement()
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
		src, want := g.statement()
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
	excused(t, c, 2, 375)
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
	switch a := a.(type) {
	case *stmt.CreateTable:
		b := b.(*stmt.CreateTable)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if a.Column.Value != b.Column.Value {
			diffs = append(diffs, "column name")
		}
	case *stmt.Insert:
		b := b.(*stmt.Insert)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		if a.Column.Value != b.Column.Value {
			diffs = append(diffs, "column name")
		}
		switch {
		case a.Parameter != b.Parameter:
			diffs = append(diffs, "parameter")
		case a.Value != b.Value:
			diffs = append(diffs, "value")
		}
	case *stmt.Select:
		if a.Table.Value != b.(*stmt.Select).Table.Value {
			diffs = append(diffs, "table name")
		}
	case *stmt.DropTable:
		if a.Table.Value != b.(*stmt.DropTable).Table.Value {
			diffs = append(diffs, "table name")
		}
	}
	if diffs == nil {
		return fmt.Sprintf("the same %T", a)
	}
	return fmt.Sprintf("%T with another %v", a, strings.Join(diffs, " and "))
}

// FuzzGenerate checks the pass against SQLite on a statement of the generator of slice 1 and a
// mutant of it, from seeds that the fuzzer chooses.
func FuzzGenerate(f *testing.F) {
	f.Add(uint64(1), uint64(2))
	f.Fuzz(func(t *testing.T, seed, mutant uint64) {
		src, want := newGenerator(t, seed).statement()
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

// nearMisses of the four forms of slice 1: each is the form with one clause, keyword, or item added
// or changed, which a one-token mutant cannot make. %[1]v is the table, %[2]v the column, and %[3]v
// the value.
var nearMisses = map[string][]string{
	"*stmt.CreateTable": {
		"explain create table %[1]v (%[2]v int primary key) strict, without rowid",
		"explain query plan create table %[1]v (%[2]v int primary key) strict, without rowid",
		"create temp table %[1]v (%[2]v int primary key) strict, without rowid",
		"create temporary table %[1]v (%[2]v int primary key) strict, without rowid",
		"create table if not exists %[1]v (%[2]v int primary key) strict, without rowid",
		"create table main.%[1]v (%[2]v int primary key) strict, without rowid",
		"create table temp.%[1]v (%[2]v int primary key) strict, without rowid",
		"create table %[1]v (%[2]v int unsigned primary key) strict, without rowid",
		"create table %[1]v (%[2]v int(10) primary key) strict, without rowid",
		"create table %[1]v (%[2]v integer primary key) strict, without rowid",
		"create table %[1]v (%[2]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v int) strict, without rowid",
		"create table %[1]v (%[2]v int primary key not null) strict, without rowid",
		"create table %[1]v (%[2]v int primary key null) strict, without rowid",
		"create table %[1]v (%[2]v int primary key unique) strict, without rowid",
		"create table %[1]v (%[2]v int primary key check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key default 1) strict, without rowid",
		"create table %[1]v (%[2]v int primary key default (1)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key collate binary) strict, without rowid",
		"create table %[1]v (%[2]v int primary key references u (x)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key references u deferrable initially deferred) strict, without rowid",
		"create table %[1]v (%[2]v int primary key asc) strict, without rowid",
		"create table %[1]v (%[2]v int primary key desc) strict, without rowid",
		"create table %[1]v (%[2]v int primary key on conflict fail) strict, without rowid",
		"create table %[1]v (%[2]v int primary key autoincrement) strict, without rowid",
		"create table %[1]v (%[2]v int primary key generated always as (1)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key constraint c) strict, without rowid",
		"create table %[1]v (%[2]v int constraint c primary key) strict, without rowid",
		"create table %[1]v (%[2]v int not null primary key) strict, without rowid",
		"create table %[1]v (%[2]v int primary key, w int) strict, without rowid",
		"create table %[1]v (w int, %[2]v int primary key) strict, without rowid",
		"create table %[1]v (%[2]v int primary key, unique (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key, check (%[2]v > 0)) strict, without rowid",
		"create table %[1]v (%[2]v int, primary key (%[2]v)) strict, without rowid",
		"create table %[1]v (%[2]v int primary key) strict",
		"create table %[1]v (%[2]v int primary key) without rowid",
		"create table %[1]v (%[2]v int primary key)",
		"create table %[1]v (%[2]v int primary key) strict, without rowid, strict",
		"create table %[1]v (%[2]v int primary key) strict, strict, without rowid",
		"create table %[1]v (%[2]v int primary key), strict, without rowid",
		"create table %[1]v (%[2]v int primary key) strict, without rowid, foo",
		"create table %[1]v as select 1",
		"create table %[1]v (%[2]v int primary key) strict, without rowid; select 1",
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
		"insert into %[1]v values (%[3]v)",
		"insert into %[1]v (%[2]v, w) values (%[3]v, 1)",
		"insert into %[1]v (%[2]v, %[2]v) values (%[3]v, %[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v, 1)",
		"insert into %[1]v (%[2]v) values ((%[3]v))",
		"insert into %[1]v (%[2]v) values (+%[3]v)",
		"insert into %[1]v (%[2]v) values (%[3]v + 0)",
		"insert into %[1]v (%[2]v) values (- (%[3]v))",
		"insert into %[1]v (%[2]v) values (%[3]v collate binary)",
		"insert into %[1]v (%[2]v) values (cast(%[3]v as int))",
		"insert into %[1]v (%[2]v) values (?1)",
		"insert into %[1]v (%[2]v) values (:a)",
		"insert into %[1]v (%[2]v) values (-?)",
		"insert into %[1]v (%[2]v) values (?, ?)",
		"insert into %[1]v (%[2]v) select %[3]v",
		"insert into %[1]v (%[2]v) values (%[3]v) union select 1",
		"insert into %[1]v (%[2]v) values (%[3]v) returning *",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict do nothing",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict (%[2]v) do update set %[2]v = 1",
		"insert into %[1]v default values",
		"insert into %[1]v (%[2]v) values (%[3]v); select 1",
	},
	"*stmt.Select": {
		"explain select * from %[1]v",
		"with x as (select 1) select * from %[1]v",
		"select distinct * from %[1]v",
		"select all * from %[1]v",
		"select *, * from %[1]v",
		"select %[1]v.* from %[1]v",
		"select 1 from %[1]v",
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
	g := newGenerator(t, *seed+3)
	c := counts{}
	for range n {
		_, s := g.statement()
		var table, column, value string
		switch s := s.(type) {
		case *stmt.CreateTable:
			table, column = s.Table.Raw, s.Column.Raw
		case *stmt.Insert:
			table, column, value = s.Table.Raw, s.Column.Raw, fmt.Sprint(s.Value)
		case *stmt.Select:
			table = s.Table.Raw
		case *stmt.DropTable:
			table = s.Table.Raw
		}
		for _, form := range nearMisses[fmt.Sprintf("%T", s)] {
			src := fmt.Sprintf(form, table, column, value)
			o, _ := verify(t, src)
			if o == supported {
				t.Fatalf("%q, a near miss of slice 1, is supported", src)
			}
			c[o]++
		}
	}
	t.Logf("%v near misses: %v", c[syntaxError]+c[notSupported]+c[syntaxNotSaid]+c[notSupportedNotSaid], c)
	excused(t, c, 0, 1046)
}
