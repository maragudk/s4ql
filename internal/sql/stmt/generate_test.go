package stmt_test

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"maragu.dev/is"

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

// newGenerator of [stmttest] from seed, which writes a plain ? in place of one value of an insert or
// of a condition in four, columns and values of every type, the forms of slice 5 with up to three
// columns, those of slice 6, with a column that takes NULL and a NULL one time in five, those of
// slice 7, with a WHERE of up to four conditions in one select in two, those of slice 8, an update
// or a delete in one statement in three, with a WHERE as a select has one, those of slice 9, a
// create index or a drop index in one statement in four, with up to three columns, those of slice
// 10, IF NOT EXISTS or IF EXISTS in one create or drop in three, those of slice 11, a DEFAULT on
// one column in three of a create table, those of slice 12, the value on the left of one
// comparison in three, those of slice 13, an ORDER BY on one select in three and a LIMIT on one in
// three, and those of slice 14, a RETURNING on one insert, update, or delete in three.
func newGenerator(t testing.TB, seed uint64) *stmttest.Generator {
	g := stmttest.NewGenerator(rand.New(rand.NewPCG(seed, 0x511ce1)), namePools(t))
	g.Parameters = 4
	g.Columns = 3
	g.Nulls = 5
	g.Conditions = 4
	g.Changes = 3
	g.Indexes = 4
	g.Exists = 3
	g.Defaults = 3
	g.ValueFirst = 3
	g.OrderBy = 3
	g.Returning = 3
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
	var before, beforeChanges, beforeReturning [3]int64
	for i := range whereReads {
		before[i] = whereReads[i].Load()
		beforeChanges[i] = changeReads[i].Load()
		beforeReturning[i] = returningReads[i].Load()
	}
	kinds := map[string]int{}
	for range n {
		src, want := g.Statement()
		s, err := stmt.Check(src)
		if err != nil || fmt.Sprintf("%#v", s) != fmt.Sprintf("%#v", want) {
			t.Fatalf("%q: gives %#v %v, want %#v", src, s, err, want)
		}
		verify(t, src)
		kinds[fmt.Sprintf("%T", s)]++
	}
	for p, pool := range namePools(t) {
		t.Logf("%v names in position %q", len(pool), positionTexts[p])
	}
	t.Logf("statements by kind: %v", kinds)
	// The check of a select, an update, or a delete with WHERE tells its conditions apart only on a
	// table where they choose some rows and not others.
	for _, c := range []struct {
		what   string
		reads  *[3]atomic.Int64
		before [3]int64
	}{{"selects", &whereReads, before}, {"updates and deletes", &changeReads, beforeChanges},
		{"updates and deletes with RETURNING", &returningReads, beforeReturning}} {
		var reads [3]int64
		for i := range c.reads {
			reads[i] = c.reads[i].Load() - c.before[i]
		}
		t.Logf("%v with WHERE that read or changed no row of the table: %v, some: %v, every row: %v", c.what, reads[0], reads[1], reads[2])
		if reads[0]+reads[1]+reads[2] >= 100 {
			is.True(t, reads[1] > (reads[0]+reads[2])/2, "too few "+c.what+" with WHERE read or changed some rows of the table and not others")
		}
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
	excused(t, c, 45, 167)
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
	// values of as many terms, a and b.
	values := func(a, b []stmt.Term) {
		for i := range a {
			a, b := a[i], b[i]
			switch {
			case a.Parameter != b.Parameter:
				diffs = append(diffs, "parameter")
			case fmt.Sprintf("%T", a.Value) != fmt.Sprintf("%T", b.Value):
				diffs = append(diffs, "kind of value")
			case a.Value != nil && show(a.Value) != show(b.Value):
				diffs = append(diffs, "value")
			}
		}
	}
	conditions := func(a, b []stmt.Condition) {
		var ac, bc []stmt.Name
		for _, c := range a {
			ac = append(ac, c.Column)
		}
		for _, c := range b {
			bc = append(bc, c.Column)
		}
		names(ac, bc, "condition")
		if len(a) != len(b) {
			return
		}
		for i := range a {
			if a[i].Operator != b[i].Operator {
				diffs = append(diffs, "operator")
				continue
			}
			if a[i].ValueFirst != b[i].ValueFirst {
				diffs = append(diffs, "side of a column")
			}
			values([]stmt.Term{a[i].Value}, []stmt.Term{b[i].Value})
		}
	}
	returning := func(a, b stmt.Returning) {
		if a.Present() != b.Present() {
			diffs = append(diffs, "returning or none")
			return
		}
		names(a.Columns, b.Columns, "returned column")
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
			ad, bd := a.Columns[i].Default.Value, b.Columns[i].Default.Value
			switch {
			case (ad == nil) != (bd == nil):
				diffs = append(diffs, "default or none")
			case ad != nil && show(ad) != show(bd):
				diffs = append(diffs, "default")
			}
		}
		names(an, bn, "column")
	case *stmt.Insert:
		b := b.(*stmt.Insert)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		names(a.Columns, b.Columns, "column")
		returning(a.Returning, b.Returning)
		if len(a.Values) != len(b.Values) {
			diffs = append(diffs, "number of values")
			break
		}
		values(a.Values, b.Values)
	case *stmt.Select:
		b := b.(*stmt.Select)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		names(a.Columns, b.Columns, "result column")
		conditions(a.Where, b.Where)
		var ao, bo []stmt.Name
		for _, c := range a.OrderBy {
			ao = append(ao, c.Name)
		}
		for _, c := range b.OrderBy {
			bo = append(bo, c.Name)
		}
		names(ao, bo, "ordering column")
		if len(a.OrderBy) == len(b.OrderBy) {
			for i := range a.OrderBy {
				if a.OrderBy[i].Descending != b.OrderBy[i].Descending {
					diffs = append(diffs, "direction")
				}
			}
		}
		if a.Limited() != b.Limited() {
			diffs = append(diffs, "limit or none")
		} else if a.Limited() {
			values([]stmt.Term{a.Limit}, []stmt.Term{b.Limit})
		}
	case *stmt.Update:
		b := b.(*stmt.Update)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		var ac, bc []stmt.Name
		var av, bv []stmt.Term
		for _, x := range a.Set {
			ac, av = append(ac, x.Column), append(av, x.Value)
		}
		for _, x := range b.Set {
			bc, bv = append(bc, x.Column), append(bv, x.Value)
		}
		names(ac, bc, "assignment")
		if len(av) == len(bv) {
			values(av, bv)
		}
		conditions(a.Where, b.Where)
		returning(a.Returning, b.Returning)
	case *stmt.Delete:
		b := b.(*stmt.Delete)
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		conditions(a.Where, b.Where)
		returning(a.Returning, b.Returning)
	case *stmt.DropTable:
		if a.Table.Value != b.(*stmt.DropTable).Table.Value {
			diffs = append(diffs, "table name")
		}
	case *stmt.CreateIndex:
		b := b.(*stmt.CreateIndex)
		if a.Index.Value != b.Index.Value {
			diffs = append(diffs, "index name")
		}
		if a.Table.Value != b.Table.Value {
			diffs = append(diffs, "table name")
		}
		var an, bn []stmt.Name
		for _, c := range a.Columns {
			an = append(an, c.Name)
		}
		for _, c := range b.Columns {
			bn = append(bn, c.Name)
		}
		names(an, bn, "index column")
		if len(a.Columns) == len(b.Columns) {
			for i := range a.Columns {
				if a.Columns[i].Descending != b.Columns[i].Descending {
					diffs = append(diffs, "order")
				}
			}
		}
	case *stmt.DropIndex:
		if a.Index.Value != b.(*stmt.DropIndex).Index.Value {
			diffs = append(diffs, "index name")
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

// nearMisses of the forms: each is the form with one clause, keyword, or item added or changed, which
// a one-token mutant cannot make, or a literal that is not supported or is a syntax error. %[1]v is
// the table, %[2]v the column, %[3]v the text of the value, and %[4]v the type of the column; for an
// index, %[1]v is the table of a create index, and %[3]v the name of the index.
var nearMisses = map[string][]string{
	"*stmt.CreateTable": {
		"explain create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"explain query plan create table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temp table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temporary table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create if not exists table %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if exists %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not exists if not exists %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not exists main.%[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create temp table if not exists %[1]v (%[2]v %[4]v primary key) strict, without rowid",
		"create table if not exists %[1]v (%[2]v %[4]v primary key) strict",
		"create table if not exists %[1]v (%[2]v %[4]v primary key, w int primary key) strict, without rowid",
		"create table if not exists %[1]v as select 1",
		"create table if not exists if (%[2]v %[4]v primary key) strict, without rowid",
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
		"create table %[1]v (%[2]v %[4]v primary key default (1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default +1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default +null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default -null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default - -1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default true) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default false not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 0x1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 1_0) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 9223372036854775808) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 1e999) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default -'a') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default -x'01') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default \"a\") strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default a) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default [a]) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default current_time) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default ?) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default (null)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default (?)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default null default null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v default null primary key default null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default null null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default null collate binary) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key default 'a' 'b') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int default 'a') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null default 1.5) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w integer default x'01') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w real default 1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w real not null default '1.5') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w text default 1) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w text default -1.5) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w blob default 'a') strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w blob default 0) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int default 1 default 2) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int default (1 + 1)) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w text default current_timestamp) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int default 1 unique) strict, without rowid",
		"create table if not exists %[1]v (%[2]v %[4]v primary key default +1) strict, without rowid",
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
		"create table %[1]v (%[2]v %[4]v, w int) strict, without rowid",
		"create table %[1]v (w int null, %[2]v %[4]v primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null not null) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v primary key, w int not null primary key) strict, without rowid",
		"create table %[1]v (%[2]v %[4]v not null, w int not null) strict, without rowid",
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
		"insert into %[1]v values (%[3]v) returning %[2]v + 1",
		"insert into %[1]v values (%[3]v) returning *, *",
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
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[2]v as x",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[2]v x",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[1]v.%[2]v",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[1]v.*",
		"insert into %[1]v (%[2]v) values (%[3]v) returning *, %[2]v",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[2]v, *",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[3]v",
		"insert into %[1]v (%[2]v) values (%[3]v) returning ?",
		"insert into %[1]v (%[2]v) values (%[3]v) returning",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[2]v,",
		"insert into %[1]v (%[2]v) values (%[3]v) returning %[2]v returning %[2]v",
		"insert into %[1]v (%[2]v) values (%[3]v), (%[3]v) returning *",
		"insert into %[1]v (%[2]v) values (%[3]v) on conflict do nothing returning *",
		"insert into %[1]v default values returning *",
		"insert into %[1]v (%[2]v) values (%[3]v) returning *; select 1",
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
		"select * from %[1]v where %[2]v isnull",
		"select %[2]v from %[1]v where %[2]v not null",
		"select count(%[2]v) from %[1]v",
		"select true from %[1]v",
		"select %[2]v, false from %[1]v",
		"select '%[2]v' from %[1]v",
		"select [%[2]v] from %[1]v",
		"select distinct %[2]v from %[1]v",
		"select %[2]v from %[1]v where 1",
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
		"select * from %[1]v limit 1 offset 1",
		"select * from %[1]v union select * from %[1]v",
		"select * from %[1]v union all select * from %[1]v",
		"select * from %[1]v except select * from %[1]v",
		"select * from %[1]v; select 1",
		"select * from %[1]v where 1 = 1",
		"select * from %[1]v where ? = ?",
		"select * from %[1]v where 1 = ?",
		"select * from %[1]v where null is null",
		"select * from %[1]v where ? is not null",
		"select * from %[1]v where 1 is %[2]v",
		"select * from %[1]v where 1 = %[2]v + 1",
		"select * from %[1]v where (1) = %[2]v",
		"select * from %[1]v where 1 = (%[2]v)",
		"select * from %[1]v where +1 = %[2]v",
		"select * from %[1]v where -? = %[2]v",
		"select * from %[1]v where ?1 = %[2]v",
		"select * from %[1]v where true = %[2]v",
		"select * from %[1]v where 0x1 = %[2]v",
		"select * from %[1]v where 1 = %[1]v.%[2]v",
		"select * from %[1]v where 1 = %[2]v collate nocase",
		"select * from %[1]v where 1 < %[2]v < 2",
		"select * from %[1]v where 1 = %[2]v or 2 = %[2]v",
		"select * from %[1]v where 1 = %[2]v and",
		"select * from %[1]v where 1 =",
		"select * from %[1]v where %[2]v = %[2]v",
		"select * from %[1]v where %[1]v.%[2]v = 1",
		"select * from %[1]v where %[2]v = 1 or %[2]v = 2",
		"select * from %[1]v where %[2]v = 1 and %[2]v = 2 or %[2]v = 3",
		"select * from %[1]v where not %[2]v = 1",
		"select * from %[1]v where (%[2]v = 1)",
		"select * from %[1]v where %[2]v = (1)",
		"select * from %[1]v where %[2]v + 1 = 2",
		"select * from %[1]v where lower(%[2]v) = 'a'",
		"select * from %[1]v where %[2]v in (1)",
		"select * from %[1]v where %[2]v between 1 and 2",
		"select * from %[1]v where %[2]v like 'a'",
		"select * from %[1]v where %[2]v glob 'a'",
		"select * from %[1]v where %[2]v is 1",
		"select * from %[1]v where %[2]v is true",
		"select * from %[1]v where %[2]v is not false",
		"select * from %[1]v where %[2]v is distinct from null",
		"select * from %[1]v where %[2]v is not distinct from null",
		"select * from %[1]v where %[2]v collate nocase = 'a'",
		"select * from %[1]v where %[2]v = 'a' collate nocase",
		"select * from %[1]v where %[2]v = +1",
		"select * from %[1]v where %[2]v = -?",
		"select * from %[1]v where %[2]v = ?1",
		"select * from %[1]v where %[2]v = true",
		"select * from %[1]v where %[2]v = 1 and",
		"select * from %[1]v where %[2]v",
		"select * from %[1]v where %[2]v = 1 returning *",
		"select %[2]v from %[1]v order by 1",
		"select * from %[1]v order by 0",
		"select * from %[1]v order by -1",
		"select * from %[1]v order by %[2]v + 1",
		"select * from %[1]v order by (%[2]v)",
		"select * from %[1]v order by -%[2]v",
		"select * from %[1]v order by %[1]v.%[2]v",
		"select * from %[1]v order by %[2]v collate nocase",
		"select * from %[1]v order by %[2]v asc desc",
		"select * from %[1]v order by %[2]v nulls first",
		"select * from %[1]v order by %[2]v desc nulls last",
		"select * from %[1]v order by %[2]v, %[2]v",
		"select * from %[1]v order by %[2]v asc, %[2]v desc",
		"select * from %[1]v order by '%[2]v'",
		"select * from %[1]v order by [%[2]v]",
		"select * from %[1]v order by ?",
		"select * from %[1]v order by null",
		"select * from %[1]v order by true",
		"select * from %[1]v order by",
		"select * from %[1]v order by %[2]v,",
		"select * from %[1]v order %[2]v",
		"select * from %[1]v order by %[2]v order by %[2]v",
		"select * from %[1]v group by %[2]v order by %[2]v",
		"select %[2]v from %[1]v union select %[2]v from %[1]v order by %[2]v",
		"select * from %[1]v where %[2]v = 1 or %[2]v = 2 order by %[2]v",
		"select * from %[1]v order by %[2]v limit",
		"select * from %[1]v limit",
		"select * from %[1]v limit -1",
		"select * from %[1]v limit -0",
		"select * from %[1]v limit +1",
		"select * from %[1]v limit 1.5",
		"select * from %[1]v limit 1.0",
		"select * from %[1]v limit 1e2",
		"select * from %[1]v limit '5'",
		"select * from %[1]v limit x'05'",
		"select * from %[1]v limit null",
		"select * from %[1]v limit 0x5",
		"select * from %[1]v limit 9223372036854775808",
		"select * from %[1]v limit 1_0",
		"select * from %[1]v limit current_time",
		"select * from %[1]v limit true",
		"select * from %[1]v limit %[2]v",
		"select * from %[1]v limit 1 + 1",
		"select * from %[1]v limit (1)",
		"select * from %[1]v limit ?1",
		"select * from %[1]v limit :a",
		"select * from %[1]v limit -?",
		"select * from %[1]v limit 1, 1",
		"select * from %[1]v offset 1",
		"select * from %[1]v limit 1 order by %[2]v",
		"select * from %[1]v limit 1 limit 1",
		"select %[2]v from %[1]v where %[2]v = 1 order by %[2]v nulls first limit 1",
		"select %[2]v from %[1]v order by %[2]v limit 1 offset 0",
		"select * from %[1]v order by %[2]v limit 1; select 1",
	},
	"*stmt.Update": {
		"explain update %[1]v set %[2]v = %[3]v",
		"explain query plan update %[1]v set %[2]v = %[3]v",
		"with x as (select 1) update %[1]v set %[2]v = %[3]v",
		"update or replace %[1]v set %[2]v = %[3]v",
		"update or ignore %[1]v set %[2]v = %[3]v",
		"update or abort %[1]v set %[2]v = %[3]v",
		"update or fail %[1]v set %[2]v = %[3]v",
		"update or rollback %[1]v set %[2]v = %[3]v",
		"update main.%[1]v set %[2]v = %[3]v",
		"update temp.%[1]v set %[2]v = %[3]v",
		"update %[1]v as x set %[2]v = %[3]v",
		"update %[1]v indexed by i set %[2]v = %[3]v",
		"update %[1]v not indexed set %[2]v = %[3]v",
		"update %[1]v set %[2]v = %[3]v, %[2]v = %[3]v",
		"update %[1]v set %[2]v = %[3]v, w = 1, %[2]v = 2",
		"update %[1]v set (%[2]v) = (%[3]v)",
		"update %[1]v set (%[2]v, w) = (%[3]v, 1)",
		"update %[1]v set w = 1, (%[2]v) = %[3]v",
		"update %[1]v set %[2]v = %[3]v from u",
		"update %[1]v set %[2]v = %[3]v from u where u.w = 1",
		"update %[1]v set %[2]v = %[3]v returning %[2]v as x",
		"update %[1]v set %[2]v = %[3]v returning abs(%[2]v)",
		"update %[1]v set %[2]v = %[3]v returning (%[2]v)",
		"update %[1]v set %[2]v = %[3]v where %[2]v = 1 returning %[2]v, *",
		"update %[1]v set %[2]v = %[3]v where %[2]v = 1 returning %[1]v.%[2]v",
		"update %[1]v set %[2]v = %[3]v returning %[2]v where %[2]v = 1",
		"update or replace %[1]v set %[2]v = %[3]v returning *",
		"update %[1]v set %[2]v = %[3]v limit 1",
		"update %[1]v set %[2]v = %[3]v order by %[2]v",
		"update %[1]v set %[2]v = %[3]v where %[2]v = 1 order by %[2]v limit 1",
		"update %[1]v set %[2]v = %[3]v + 1",
		"update %[1]v set %[2]v = (%[3]v)",
		"update %[1]v set %[2]v = - (%[3]v)",
		"update %[1]v set %[2]v = +%[3]v",
		"update %[1]v set %[2]v = %[2]v",
		"update %[1]v set %[2]v = %[1]v.%[2]v",
		`update %[1]v set %[2]v = "a"`,
		"update %[1]v set %[2]v = ?1",
		"update %[1]v set %[2]v = :a",
		"update %[1]v set %[2]v = -?",
		"update %[1]v set %[2]v = true",
		"update %[1]v set %[2]v = %[3]v collate binary",
		"update %[1]v set %[2]v = cast(%[3]v as int)",
		"update %[1]v set %[2]v = %[3]v || %[3]v",
		"update %[1]v set %[2]v = %[3]v is null",
		"update %[1]v set %[2]v = coalesce(%[3]v, 1)",
		"update %[1]v set %[2]v = (select 1)",
		"update %[1]v set %[2]v = default",
		"update %[1]v set %[2]v <> %[3]v",
		"update %[1]v set %[1]v.%[2]v = %[3]v",
		"update %[1]v set [%[2]v] = %[3]v",
		"update %[1]v set '%[2]v' = %[3]v",
		"update %[1]v set %[2]v = %[3]v,",
		"update %[1]v set , %[2]v = %[3]v",
		"update %[1]v set",
		"update %[1]v %[2]v = %[3]v",
		"update %[1]v set %[2]v = %[3]v where",
		"update %[1]v set %[2]v = %[3]v where %[2]v",
		"update %[1]v set %[2]v = %[3]v where 1",
		"update %[1]v set %[2]v = %[3]v where 1 = 1",
		"update %[1]v set %[2]v = %[3]v where 1 is null",
		"update %[1]v set %[2]v = %[3]v where (1) = %[2]v",
		"update %[1]v set %[2]v = %[3]v where 1 = %[1]v.%[2]v",
		"update %[1]v set %[2]v = %[3]v where 1 = %[2]v or 2 = %[2]v",
		"update %[1]v set %[2]v = %[3]v where %[2]v = 1 or %[2]v = 2",
		"update %[1]v set %[2]v = %[3]v where not %[2]v = 1",
		"update %[1]v set %[2]v = %[3]v where %[1]v.%[2]v = 1",
		"update %[1]v set %[2]v = %[3]v where %[2]v isnull",
		"update %[1]v set %[2]v = %[3]v where %[2]v is 1",
		"update %[1]v set %[2]v = %[3]v where %[2]v in (1)",
		"update %[1]v set %[2]v = %[3]v where %[2]v = w",
		"update %[1]v set %[2]v = %[3]v where %[2]v = 1 and",
		"update %[1]v set %[2]v = %[3]v; select 1",
	},
	"*stmt.Delete": {
		"explain delete from %[1]v",
		"explain query plan delete from %[1]v where %[2]v = %[3]v",
		"with x as (select 1) delete from %[1]v",
		"delete from main.%[1]v",
		"delete from temp.%[1]v where %[2]v = %[3]v",
		"delete from %[1]v as x",
		"delete from %[1]v as x where x.%[2]v = %[3]v",
		"delete from %[1]v indexed by i",
		"delete from %[1]v not indexed where %[2]v = %[3]v",
		"delete from %[1]v returning %[1]v.*",
		"delete from %[1]v returning %[2]v collate nocase",
		"delete from %[1]v where %[2]v = %[3]v returning (%[2]v)",
		"delete from %[1]v where %[2]v = %[3]v returning %[3]v",
		"delete from %[1]v returning * where %[2]v = %[3]v",
		"delete from %[1]v returning",
		"delete from %[1]v limit 1",
		"delete from %[1]v order by %[2]v",
		"delete from %[1]v where %[2]v = %[3]v order by %[2]v limit 1",
		"delete %[1]v",
		"delete from %[1]v, u",
		"delete from (%[1]v)",
		"delete from %[1]v where",
		"delete from %[1]v where %[2]v",
		"delete from %[1]v where 1",
		"delete from %[1]v where 1 = 1",
		"delete from %[1]v where %[3]v = ?",
		"delete from %[1]v where ? is null",
		"delete from %[1]v where %[3]v = -%[2]v",
		"delete from %[1]v where %[3]v = %[2]v or %[3]v = %[2]v",
		"delete from %[1]v where %[2]v = %[3]v or %[2]v = %[3]v",
		"delete from %[1]v where not %[2]v = %[3]v",
		"delete from %[1]v where (%[2]v = %[3]v)",
		"delete from %[1]v where %[2]v = %[3]v + 1",
		"delete from %[1]v where %[2]v = +%[3]v",
		"delete from %[1]v where %[2]v = ?1",
		"delete from %[1]v where %[2]v = -?",
		"delete from %[1]v where %[2]v = true",
		"delete from %[1]v where %[2]v in (%[3]v)",
		"delete from %[1]v where %[2]v between %[3]v and %[3]v",
		"delete from %[1]v where %[2]v like 'a'",
		"delete from %[1]v where %[2]v = %[2]v",
		"delete from %[1]v where %[1]v.%[2]v = %[3]v",
		"delete from %[1]v where %[2]v is %[3]v",
		"delete from %[1]v where %[2]v is distinct from null",
		"delete from %[1]v where %[2]v notnull",
		"delete from %[1]v where %[2]v = %[3]v collate binary",
		"delete from %[1]v where %[2]v = %[3]v and",
		"delete from %[1]v where %[2]v = %[3]v, %[2]v = %[3]v",
		"delete from %[1]v; select 1",
	},
	"*stmt.DropTable": {
		"explain drop table %[1]v",
		"drop table %[1]v if exists",
		"drop table if not exists %[1]v",
		"drop table if %[1]v",
		"drop table if exists if exists %[1]v",
		"drop table if exists main.%[1]v",
		"drop view if exists %[1]v",
		"explain drop table if exists %[1]v",
		"drop table main.%[1]v",
		"drop table temp.%[1]v",
		"drop view %[1]v",
		"drop trigger %[1]v",
		"drop table %[1]v; select 1",
	},
	"*stmt.CreateIndex": {
		"explain create index %[3]v on %[1]v (%[2]v)",
		"explain query plan create index %[3]v on %[1]v (%[2]v)",
		"create unique index %[3]v on %[1]v (%[2]v)",
		"create if not exists index %[3]v on %[1]v (%[2]v)",
		"create index if exists %[3]v on %[1]v (%[2]v)",
		"create index if not %[3]v on %[1]v (%[2]v)",
		"create index if not exists if not exists %[3]v on %[1]v (%[2]v)",
		"create index if not exists main.%[3]v on %[1]v (%[2]v)",
		"create unique index if not exists %[3]v on %[1]v (%[2]v)",
		"create index if not exists %[3]v on %[1]v (%[2]v) where %[2]v > 0",
		"create index %[3]v on if not exists %[1]v (%[2]v)",
		"create index if not exists if on %[1]v (%[2]v)",
		"create temp index %[3]v on %[1]v (%[2]v)",
		"create index main.%[3]v on %[1]v (%[2]v)",
		"create index temp.%[3]v on %[1]v (%[2]v)",
		"create index %[3]v on main.%[1]v (%[2]v)",
		"create index %[3]v on %[1]v (%[2]v) where %[2]v > 0",
		"create index %[3]v on %[1]v (%[2]v) where %[2]v is not null",
		"create index %[3]v on %[1]v (%[2]v collate nocase)",
		"create index %[3]v on %[1]v (%[2]v collate binary desc)",
		"create index %[3]v on %[1]v (%[2]v nulls first)",
		"create index %[3]v on %[1]v (%[2]v desc nulls last)",
		"create index %[3]v on %[1]v (%[2]v + 1)",
		"create index %[3]v on %[1]v (-%[2]v)",
		"create index %[3]v on %[1]v ((%[2]v))",
		"create index %[3]v on %[1]v (%[1]v.%[2]v)",
		"create index %[3]v on %[1]v (main.%[1]v.%[2]v)",
		"create index %[3]v on %[1]v (lower(%[2]v))",
		"create index %[3]v on %[1]v (%[2]v, %[2]v)",
		"create index %[3]v on %[1]v (%[2]v asc, %[2]v desc)",
		"create index %[3]v on %[1]v ('%[2]v')",
		"create index %[3]v on %[1]v ([%[2]v])",
		"create index %[3]v on %[1]v (1)",
		"create index %[3]v on %[1]v (null)",
		"create index %[3]v on %[1]v (true)",
		"create index %[3]v on %[1]v (?)",
		"create index %[3]v on %[1]v (current_time)",
		"create index %[3]v on %[1]v ()",
		"create index %[3]v on %[1]v",
		"create index %[3]v on %[1]v (%[2]v,)",
		"create index %[3]v on %[1]v (%[2]v) desc",
		"create index %[3]v on %[1]v (%[2]v asc desc)",
		"create index %[3]v (%[2]v)",
		"create index on %[1]v (%[2]v)",
		"create index %[3]v on %[1]v (%[2]v); select 1",
	},
	"*stmt.DropIndex": {
		"explain drop index %[3]v",
		"explain query plan drop index %[3]v",
		"drop index %[3]v if exists",
		"drop index if not exists %[3]v",
		"drop index if %[3]v",
		"drop index if exists if exists %[3]v",
		"drop index if exists main.%[3]v",
		"explain drop index if exists %[3]v",
		"drop index main.%[3]v",
		"drop index temp.%[3]v",
		"drop index %[3]v on %[1]v",
		"drop index %[3]v, %[3]v",
		"drop index",
		"reindex %[3]v",
		"analyze %[3]v",
		"drop index %[3]v; select 1",
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
		case *stmt.Update:
			a := s.Set[0]
			table, column, value = s.Table.Raw, a.Column.Raw, src[a.Value.Span.Start:a.Value.Span.End]
		case *stmt.Delete:
			table, value = s.Table.Raw, "1"
			if s.Where != nil {
				column = s.Where[0].Column.Raw
			}
		case *stmt.DropTable:
			table = s.Table.Raw
		case *stmt.CreateIndex:
			table, column, value = s.Table.Raw, s.Columns[0].Name.Raw, s.Index.Raw
		case *stmt.DropIndex:
			table, value = "t", s.Index.Raw
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
	excused(t, c, 0, 1983)
}
