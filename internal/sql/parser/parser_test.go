package parser_test

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
)

// parse src with the parser, as a result for the harness.
func parse(src string) parsetest.Result {
	_, tail, err := parser.Parse(src)
	if err != nil {
		var e *parser.Error
		if !errors.As(err, &e) {
			panic(err)
		}
		return parsetest.Result{Message: e.Message, Offset: e.Offset}
	}
	return parsetest.Result{OK: true, Offset: -1, Tail: tail}
}

// checkOwnership of the tokens of the first statement of src, if it parses.
func checkOwnership(t testing.TB, src string) {
	t.Helper()
	stmt, _, err := parser.Parse(src)
	if err != nil || stmt == nil {
		return
	}
	if err := ast.CheckOwnership(stmt, parser.Tokens(src, stmt)); err != nil {
		t.Errorf("%q: %v\n%v", src, err, ast.Dump(stmt))
	}
}

func corpusSQL(t testing.TB) []string {
	t.Helper()
	var sqls []string
	for _, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
	}
	return sqls
}

// slow reports whether the tests run with -short, the race detector, or the address sanitizer, which
// make them several times slower: the tests on the corpus then use every tenth entry.
func slow() bool {
	return testing.Short() || raceEnabled || asanEnabled
}

// every nth of sqls.
func every(sqls []string, n int) []string {
	var some []string
	for i := 0; i < len(sqls); i += n {
		some = append(some, sqls[i])
	}
	return some
}

var mutants = flag.Int("mutants", 3, "mutants of each corpus entry in TestParse_mutants")
var seed = flag.Uint64("seed", 0, "seed of the mutants in TestParse_mutants")

// TestParse pins what the parser says on texts where SQLite's behaviour is easy to get wrong, and
// checks each against SQLite too.
func TestParse(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want parsetest.Result
	}{
		{"should read the first statement and give the tail after its semicolon", "select 1; select 2", ok(9)},
		{"should skip empty statements", ";; select 1;", ok(12)},
		{"should give the end as the tail of a text with no statement", " ; -- c", ok(7)},
		{"should give the tail at a NUL byte, which ends the text", "select 1\x00 frm", ok(8)},
		{"should say incomplete input at the end", "select", syntax("incomplete input", -1)},
		{"should name the token of a syntax error", "select 1 frm t", syntax(`near "t": syntax error`, 13)},
		{"should report an unterminated string as an unrecognized token", "select 'abc", syntax(`unrecognized token: "'abc"`, 7)},
		{"should report #N at the end with its offset", "select #1", syntax(`near "#1": syntax error`, 7)},
		{"should report #N before the end with its offset", "select #1 from t", syntax(`near "#1": syntax error`, 7)},
		{"should report #N at the end of a DELETE with its offset", "delete from t where #1", syntax(`near "#1": syntax error`, 20)},
		{"should report #N at the end of a CREATE VIEW with its offset", "create view v as select #1", syntax(`near "#1": syntax error`, 24)},
		{"should report #N at the end of an ATTACH with its offset", "attach x as #1", syntax(`near "#1": syntax error`, 12)},
		{"should report #N at the end of a DETACH with its offset", "detach #1", syntax(`near "#1": syntax error`, 7)},
		{"should report #N at the end of a SELECT with a multi-row VALUES with its offset", "values(0),(a) union select #1", syntax(`near "#1": syntax error`, 27)},
		{"should report a misplaced digit separator without an offset", "select 1_", syntax(`unrecognized token: "1_"`, -1)},
		{"should report a digit separator with the number partly rewritten", "select 1__2__3", syntax(`unrecognized token: "12_2__3"`, -1)},
		{"should let a syntax error on the next token win over a digit separator", "select 1_ 2", syntax(`near "2": syntax error`, 10)},
		{"should let incomplete input win over a digit separator", "select (1_", syntax("incomplete input", -1)},
		{"should read /* as the last two bytes as two tokens", "select 1 /*", syntax(`near "*": syntax error`, 10)},
		{"should read CAST where an expression starts as the keyword", "select cast.* from t", syntax(`near ".": syntax error`, 11)},
		{"should read ABORT where an expression starts as a name", "select abort.* from t", ok(21)},
		{"should read CURRENT_TIME as a term, not a function", "select current_time()", syntax(`near "(": syntax error`, 19)},
		{"should read IF after CREATE TABLE as the keyword", "create table if(a)", syntax(`near "(": syntax error`, 15)},
		{"should read RECURSIVE after WITH as the keyword", "with recursive as (select 1) select 1", syntax(`near "as": syntax error`, 15)},
		{"should read END after a result column as an alias", "select 1 end", ok(12)},
		{"should read GENERATED after a type as a word of the type", "create table t(a int generated always as (1))", ok(45)},
		{"should allow a comma before the first table option", "create table t(a), strict", ok(25)},
		{"should read ON after a FROM item as the join's, not an upsert's", "insert into t select * from u on conflict do nothing", syntax(`near "do": syntax error`, 42)},
		{"should read any token, a semicolon too, as an argument of a virtual table", "create virtual table t using m(a;b)", ok(35)},
		{"should report the end inside the arguments of a virtual table as incomplete", "create virtual table t using m(a", syntax("incomplete input", -1)},
		{"should end a trigger only after END", "create trigger tr after insert on t begin select 1;", syntax("incomplete input", -1)},
		{"should end the statement at the semicolon after END", "create trigger tr after insert on t begin select 1; end; select 2", ok(56)},
		{"should read a WINDOW that is not followed by a name and AS as a name", "select 1 window", ok(15)},
		{"should read an OVER that does not follow a parenthesis as a name", "select 1 over", ok(13)},
		{"should read FILTER after a call but not before a parenthesis as an alias", "select f(x) filter", ok(18)},
		{"should refuse EXPLAIN twice", "explain explain select 1", syntax(`near "explain": syntax error`, 8)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.Equal(t, test.want, parse(test.src))
			d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), test.src)
			is.True(t, !found, d.String())
		})
	}
}

func TestParseAll(t *testing.T) {
	t.Run("should read every statement, with offsets in the whole text", func(t *testing.T) {
		src := "select 1;; select (2) ; ;select 3 -- end"
		stmts, err := parser.ParseAll(src)
		is.NotError(t, err)
		is.Equal(t, 3, len(stmts))
		is.Equal(t, ast.Span{Start: 0, End: 9}, stmts[0].Bounds())
		is.Equal(t, ast.Span{Start: 11, End: 23}, stmts[1].Bounds())
		is.Equal(t, ast.Span{Start: 25, End: 33}, stmts[2].Bounds())
		for _, stmt := range stmts {
			is.NotError(t, ast.CheckOwnership(stmt, parser.Tokens(src, stmt)))
		}
	})

	t.Run("should stop at the first error, with its offset in the whole text", func(t *testing.T) {
		stmts, err := parser.ParseAll("select 1; select 2 frm t; select 3")
		is.Equal(t, 1, len(stmts))
		var e *parser.Error
		is.True(t, errors.As(err, &e))
		is.Equal(t, parser.Error{Message: `near "t": syntax error`, Offset: 23}, *e)
	})

	t.Run("should read nothing from a text without statements", func(t *testing.T) {
		stmts, err := parser.ParseAll(" ;\x00select 1")
		is.NotError(t, err)
		is.Equal(t, 0, len(stmts))
	})

	t.Run("should read a window keyword after a statement as at the start of a text", func(t *testing.T) {
		// OVER is a keyword only after ")", which a new prepare has not seen.
		stmts, err := parser.ParseAll("select (1); select 1 over")
		is.NotError(t, err)
		is.Equal(t, 2, len(stmts))
	})
}

func ok(tail int) parsetest.Result {
	return parsetest.Result{OK: true, Offset: -1, Tail: tail}
}

func syntax(message string, offset int) parsetest.Result {
	return parsetest.Result{Message: message, Offset: offset}
}

// TestParse_offsetAfterValues compares the parser with SQLite on the error of #N at the end of a
// statement with a multi-row VALUES, for many forms of expression in the rows and places of the
// VALUES. SQLite reads the schema while it parses such a VALUES when its rows are constant, and on a
// fresh connection only that kept the offset of the error. On the oracle's connection, whose schema
// is loaded, every such error has the offset of its token, as the parser gives it.
func TestParse_offsetAfterValues(t *testing.T) {
	exprs := []string{
		"1", "-1", "'a'", "x'00'", "1.5", "1_0", "null", "?", "?1", ":a", "true", "FALSE", `"true"`, "[true]",
		"a", "t.a", "s.t.a", `"a"`, "current_time", "CURRENT_DATE",
		"abs(1)", "abs(a)", "random()", "abs(1) filter (where 1)", "count(*)", "max(1)", "max(1, 2)",
		"sqlite_version()", "sqlite_version(*)", "nosuch(1)", "coalesce(1)", "coalesce(1, 2)", "iif(1, 2, 3)",
		"1 is 2", "1 is not 2", "1 is null", "a is null", "1 is (null)", "1 is not distinct from null",
		"1 isnull", "1 not null", "1 in ()", "a in ()", "abs(a) in ()", "1 in (2)", "1 in (2, 3)",
		"1 in (a)", "(1, 2) in ((1, 2))", "(1) in (1)", "1 in (select 1)", "1 in t", "exists (select 1)",
		"(select 1)", "1 like 2", "1 like 2 escape 3", "1 glob 2", "1 regexp 2", "1 match 2", "1 -> 2",
		"1 ->> 2", "1 between 2 and 3", "1 between a and 3", "not 1", "~1", "+1", "1 + 2", "1 || a",
		"case when 1 then 2 end", "case a when 1 then 2 end", "case 1 when 2 then 3 else a end",
		"cast(1 as int)", "cast(a as int)", "cast(1 as)", "1 collate nocase", "cast(1 as int) collate x",
		"(1, 2)", "(cast(1 as int), 2)", "(1, cast(2 as int))", "raise(ignore)", "(1)", "((cast(1 as int)))",
		"row_number() over ()", "length('a') + 1", `"abs"(1)`, "[abs](1)", "replace('a', 'b', 'c')",
		"like(1, 2)", "json_object()", "printf('%d', 1)", "changes()",
		"(a -> 1) in ()", "current_time in ()", `"a""b"(1)`, "`abs`(1)",
	}
	shapes := []string{
		"values(0),(E) union select #0",
		"values(E),(1) union select #0",
		"values(0),(E),(1) union select #0",
		"values(E),(a),(1) union select #0",
		"select * from (values(0),(E)) union select #0",
		"with c as (select 1) select * from (values(0),(E)) union select #0",
		"select * from (values(0),(E)) union select #0 from (with c as (select 1) select 1)",
		"create view v as values(0),(E) union select #0",
		"select (values(0),(E)), #0",
		"select #0 from (values(0),(E))",
		"attach (values(0),(E)) as #0",
	}
	var sqls []string
	errs := 0
	for _, e := range exprs {
		for _, shape := range shapes {
			src := strings.ReplaceAll(shape, "E", e)
			sqls = append(sqls, src)
			if v := oracle.Judge(src); v.Kind == oracle.SyntaxError {
				errs++
				is.True(t, v.Offset >= 0, "no offset: "+src)
			}
		}
	}
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v with a syntax error, SQLite does not say on %v", r.Total, errs, r.NotSaid)
	// Most texts must give the syntax error, or the test checks little.
	is.True(t, errs > len(sqls)*9/10, fmt.Sprint(errs, " syntax errors"))
	is.True(t, r.NotSaid < len(sqls)/5, fmt.Sprint("SQLite does not say on ", r.NotSaid))
}

// TestParse_sqlite compares the parser with SQLite on texts written by hand for corners of the
// grammar that the corpus touches little: window keywords, virtual tables, constraints, upserts,
// triggers, the checks of #N and digit separators, and keywords as names.
func TestParse_sqlite(t *testing.T) {
	sqls := []string{
		"select 1 window w as ()", "select 1 window /*x*/ w /*y*/ as ()", "select 1 window filter as ()",
		"select 1 window window as ()", "select 1 window over as ()", "select 1 window 'w' as ()",
		"select 1 window left as ()", "select 1 window indexed as ()", "select 1 window abort as ()",
		"select f(x) over", "select f(x) over(", "select f(x) over w", "select f(x) over 'w'",
		"select f(x) over indexed", "select f(x) over left", "select f(x) over window", "select f(x) over over",
		"select f(x) filter (where 1) over w", "select f(x) filter", "select f(x) filter(", "select f(x) filter (",
		"select f(x) /**/ filter /**/ (where 1)", "select (1) over", "select (1) over (", "select (1) filter (where 1)",
		"select f(*) over ()", "select f(*) filter (where 1)", "select count(distinct x) filter (where 1) over ()",
		"select x from t window w as (partition by 1), v as (w order by 2)", "select over from over",
		"select window from window", "select filter from filter", "select * from t window",
		"select * from t window w", "select * from t window w as", "select * from window as window",
		"select f() over (order by x rows between current row and current row)",
		"select f() over (range unbounded preceding)", "select f() over (current)",
		"select f() over (current row)", "select f() over (w rows 1 preceding exclude group)",
		"select f() over (w rows 1 preceding exclude)", "create table window(over, filter)",
		"select 1 as window", "select 1 window", "values(1) window w as ()",
		"select 1 from t group by 1 having 1 window w as () order by 1",
		"select f(x) over (partition by y) as over from t",
		"select x filter from t", "select f(x)filter(where 1)over(w)",
		"select f(x) over w window w as (rows 1 following)", "select f(x) over (rows between 1 following and 1 preceding)",
		"select f() over (groups between unbounded preceding and unbounded following exclude no others)",
		"select f() over (exclude ties)", "select f() over (partition by a, b order by c, d)",
		"select f() over (a partition by b)", "select f() over (a order by b)", "select f() over (a rows current row)",
		"select f() over (rows unbounded following)", "select f() over (rows between 1 + 1 preceding and current row)",
		"select f() over (rows a and b preceding)", "select f() over (rows between a and b preceding and 1 following)",
		"create virtual table t using m(", "create virtual table t using m((", "create virtual table t using m(()",
		"create virtual table t using m(a))", "create virtual table t using m(a) x", "create virtual table t using m(,)",
		"create virtual table t using m(;)", "create virtual table t using m(a,(b,c),d e f)", "create virtual table t using 'm'",
		"create virtual table if not exists s.t using m", "create virtual table t using m(x'00' 'a)",
		"create virtual table t using m(1_)", "create virtual table t using m(#1)",
		"create table t(a generated)", "create table t(a generated always)", "create table t(a as (1) generated)",
		"create table t(a as (1) generated always as (2))", "create table t(a as (1) 'stored')", "create table t(a as (1) stored stored)",
		"create table t(a default -current_time)", "create table t(a default +'x')", "create table t(a default -x)",
		"create table t(a default left)", "create table t(a default indexed)", "create table t(a default 1_)",
		"create table t(a default 1_ not null)", "create table t(a default (1_))", "create table t(a int(1_0))",
		"create table t(a, constraint c)", "create table t(a, constraint c, constraint d unique(a))",
		"create table t(a, primary key(a) unique(a))", "create table t(a, primary key(a),)", "create table t(a,)",
		"create table t(a references b match full on delete no action)", "create table t(a not deferrable initially immediate)",
		"create table t(a collate)", "create table t(a collate 'x')", "create table t(a key)", "create table t(a primary)",
		"create table t(a int unsigned big int(1,-2))", "create table t(a varchar(+1))", "create table t(a varchar(1.5))",
		"insert into t select 1 on conflict(a) do nothing on conflict do nothing returning *",
		"insert into t select 1 on conflict do nothing on conflict do nothing", "insert into t values(1) returning",
		"insert into t default values on conflict do nothing", "insert into t(a) default values",
		"insert into t select * from u where 1 on conflict(a) do update set a = 1 where 1 returning a",
		"replace or ignore into t values(1)", "insert or abort into t values(1)", "update t set (a) = (1)",
		"update t set a = 1, from u", "update t as x set a = 1 from u, v where 1", "delete from t indexed by",
		"create trigger t before delete on s.x begin select 1; select 2; end",
		"create trigger t delete on x begin replace into y values(1); end",
		"create trigger t update of a on x for each row begin with c as (select 1) select * from c; end",
		"create trigger t update of a on x begin insert into y default values; end",
		"create trigger t update on x begin delete from y returning *; end",
		"create trigger t update on x begin update y set a = 1 returning *; end",
		"create trigger t update on x begin values(1); end", "create trigger t update on x begin ; end",
		"create trigger t update on x begin select 1 end", "create trigger t update on x begin select 1; end end",
		"create trigger t update on x when #1 begin select 1; end", "create trigger t update on x begin select #1; end",
		"select 1_ from t", "select 1 limit 1_", "select x'0' 1_", "select #1 #2", "select 1_ #1", "select #1 1_",
		"select 0x1_g", "select 1_.5_e1_0", "select 1e1_0", "select 0x_1", "select 0x1__1", "select ._5", "select 5._",
		"select #1abc", "select #a", "select #", "select 1 from t where #1 and 1_", "explain query select 1",
		"select #$a", "select #::a", "select #a::b", "select 0_e1", "select 0_E1", "select 01_e5", "select 0x1_e",
		"select 0x0_9", "select 0x9_0", "select 0xa_f", "select 0xf_a", "select 0xA_F", "select 0xF_A", "select 0_9",
		"select 9_0", "select 1._5", "select 1_._5", "select window(1)", "select over(1)", "select filter(1)",
		"select window.x from t", "select (1) over(1)", "with c as (select 1) delete from t returning *",
		"with c as (select 1) update t set a = 1 returning *", "with c as (select 1) insert into t default values returning *",
		"values(0),(0) union select #1", "values(0),(a) union select #1", "values(0),(0) union select #1 from t",
		"insert into t values(0),(0) returning #1", "attach (values(0),(0)) as #1",
		"explain query plan explain select 1", "pragma x.y(z)", "pragma x = +1.5", "pragma x = - 1", "pragma x = 'a'",
		"pragma x(on)", "pragma x = null", "pragma x = 1_0", "pragma x = x'00'", "pragma x = default", "pragma x(delete)",
		"attach 'a' as b key", "attach database", "detach database 'x'", "vacuum into", "vacuum x into y",
		"reindex left", "analyze s.t.u", "begin immediate transaction", "begin exclusive x", "commit to x",
		"end transaction x", "rollback to", "rollback transaction x to y", "release", "savepoint",
		"alter table t add column", "alter table t add constraint", "alter table t add constraint c",
		"alter table t add constraint c check", "alter table t drop", "alter table t alter column x",
		"alter table t alter column x drop", "alter table t alter x set not null on", "alter table t rename column to x",
		"alter table t rename to", "alter table t add column x generated always as (1)",
		"with x as (select 1) values(1)", "with x as materialized select 1", "with x(a,) as (select 1) select 1",
		"with recursive select 1", "with x as (select 1), select 1", "select * from (with x as (select 1) select 1)",
		"select * from ((select 1))", "select * from ((a))", "select * from (a) as b on 1",
		"select * from a natural join b natural join c", "select * from a left outer outer join b",
		"select * from a join b using (x, y) join c on 1", "select * from a, b as c indexed by d not indexed",
		"select * from f() as g indexed by h", "select * from s.f(1) x", "select * from 'a'.'b'",
		"select a.* from t", "select 'a'.* from t", "select left.* from t", "select raise.* from t", "select current_date.* from t",
		"select x.y.* from t", "select * , from t", "select distinct * from t", "select all from t",
	}
	for _, sql := range sqls {
		if d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), sql); found {
			t.Error(d)
		}
		checkOwnership(t, sql)
	}
}

// TestParse_depth checks the depth limit of the parser against the stack of SQLite's own automaton,
// for every kind of nesting: the parser's depth never exceeds the depth of the stack, so a text that
// the limit rejects overflows SQLite's stack too, which SQLite limits to 2500 entries; and text nested
// far deeper fails at once, without a stack overflow.
func TestParse_depth(t *testing.T) {
	l := loadLALR(t)
	nestings := []struct{ open, close string }{
		{"(", ")"}, {"- ", ""}, {"not ", ""}, {"~", ""}, {"(select ", ")"}, {"case when 1 then ", " end"},
		{"f(", ")"}, {"exists (select ", ")"}, {"1 in (", ")"}, {"(1, ", ")"}, {"x collate (", ")"},
		{"cast(", " as int)"}, {"1 between ", " and 2"}, {"1 + (", ")"},
	}
	for _, n := range nestings {
		t.Run(n.open, func(t *testing.T) {
			for _, k := range []int{1, 10, 2490, parser.MaxDepth - 1, parser.MaxDepth, parser.MaxDepth + 1} {
				src := "select " + strings.Repeat(n.open, k) + "1" + strings.Repeat(n.close, k)
				tr := parser.TraceParse(src)
				r := l.run(tr.Tokens)
				is.True(t, tr.MaxDepth <= max(r.maxStack, 1), fmt.Sprint(k, ": depth ", tr.MaxDepth, ", stack ", r.maxStack))
				// Each level of a nesting that parses is a level of depth, or the check above would prove
				// nothing.
				if tr.OK {
					is.True(t, tr.MaxDepth >= k, fmt.Sprint(k, ": depth ", tr.MaxDepth))
				}
				_, _, err := parser.Parse(src)
				var e *parser.Error
				if errors.As(err, &e) && e.Limit {
					is.True(t, r.maxStack >= 2500, fmt.Sprint(k, ": the limit rejects it, with a stack of ", r.maxStack))
					v := oracle.Judge(src)
					is.Equal(t, "Recursion limit", v.Message)
				}
			}
		})
	}

	t.Run("should reject a nesting of 2500 parentheses with its own error", func(t *testing.T) {
		_, _, err := parser.Parse("select " + strings.Repeat("(", 2500) + "1" + strings.Repeat(")", 2500))
		var e *parser.Error
		is.True(t, errors.As(err, &e))
		is.True(t, e.Limit)
		is.Equal(t, parser.Error{Message: "s4ql: the statement is nested deeper than the limit of 2500", Offset: -1, Limit: true}, *e)
		_, _, err = parser.Parse("select " + strings.Repeat("(", 2499) + "1" + strings.Repeat(")", 2499))
		is.NotError(t, err)
	})

	t.Run("should not count a sibling as a level of nesting", func(t *testing.T) {
		var ctes []string
		for i := range parser.MaxDepth + 10 {
			ctes = append(ctes, fmt.Sprintf("c%v as (select 1)", i))
		}
		for _, src := range []string{
			"with " + strings.Join(ctes, ", ") + " select 1",
			"select * from " + strings.Repeat("(select 1), ", parser.MaxDepth+10) + "t",
			"select * from " + strings.Repeat("(a join b), ", parser.MaxDepth+10) + "t",
			"select " + strings.Repeat("(1), ", parser.MaxDepth+10) + "1",
		} {
			_, _, err := parser.Parse(src)
			is.NotError(t, err)
			d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), src)
			is.True(t, !found, d.String())
		}
	})

	t.Run("should fail at once on a nesting of a million", func(t *testing.T) {
		for _, open := range []string{"(", "- ", "not ", "(select ", "f(", "case when 1 then "} {
			_, _, err := parser.Parse("select " + strings.Repeat(open, 1_000_000))
			var e *parser.Error
			is.True(t, errors.As(err, &e) && e.Limit)
		}
		_, _, err := parser.Parse("select * from " + strings.Repeat("(", 1_000_000))
		var e *parser.Error
		is.True(t, errors.As(err, &e) && e.Limit)
		_, _, err = parser.Parse(strings.Repeat("with a as (", 1_000_000))
		is.True(t, errors.As(err, &e) && e.Limit)
	})
}

// TestMaxLength checks the length limit against the vendored SQLite, since a text of that length is
// too large for a test: TestParse_length in parser_internal_test.go tests the rule at a smaller limit.
func TestMaxLength(t *testing.T) {
	t.Run("should be SQLITE_MAX_SQL_LENGTH of the vendored SQLite, which the binding does not change", func(t *testing.T) {
		src, err := os.ReadFile("../../sqlite/c/sqlite3.c")
		is.NotError(t, err)
		m := regexp.MustCompile(`(?m)^# define SQLITE_MAX_SQL_LENGTH (\d+)$`).FindSubmatch(src)
		is.True(t, m != nil, "no SQLITE_MAX_SQL_LENGTH")
		is.Equal(t, fmt.Sprint(parser.MaxLength), string(m[1]))
	})
}

// TestParse_mutants compares the parser with SQLite on one-token mutants of the corpus, -mutants of
// each entry, three by default, from -seed.
func TestParse_mutants(t *testing.T) {
	entries := corpusSQL(t)
	if slow() {
		entries = every(entries, 10)
	}
	var sqls []string
	for i, sql := range entries {
		sqls = append(sqls, parsetest.Mutants(sql, *seed+uint64(i), *mutants)...)
	}
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v agree, SQLite does not say on %v", r.Total, r.Agreed, r.NotSaid)
	for _, sql := range sqls {
		checkOwnership(t, sql)
	}
}

// TestParse_corpus compares the parser with SQLite on the corpus, and checks the ownership of the
// tokens of every statement that parses.
func TestParse_corpus(t *testing.T) {
	sqls := corpusSQL(t)
	if slow() {
		sqls = every(sqls, 10)
	}
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v agree, SQLite does not say on %v", r.Total, r.Agreed, r.NotSaid)
	for _, sql := range sqls {
		checkOwnership(t, sql)
	}
}

// FuzzParse compares the parser with SQLite on any text, and checks the ownership of the tokens of
// every statement that ParseAll reads from it.
func FuzzParse(f *testing.F) {
	for i, sql := range corpusSQL(f) {
		if i%25 == 0 {
			f.Add(sql)
		}
	}
	own, err := corpus.Read("own")
	if err != nil {
		f.Fatal(err)
	}
	for _, sql := range own {
		f.Add(sql)
	}
	f.Fuzz(func(t *testing.T, src string) {
		if d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), src); found {
			t.Fatal(d)
		}
		stmts, _ := parser.ParseAll(src)
		for _, stmt := range stmts {
			if err := ast.CheckOwnership(stmt, parser.Tokens(src, stmt)); err != nil {
				t.Fatalf("%q: %v\n%v", src, err, ast.Dump(stmt))
			}
		}
	})
}

// BenchmarkParse parses every statement of the corpus once per iteration.
func BenchmarkParse(b *testing.B) {
	sqls := corpusSQL(b)
	bytes := 0
	for _, sql := range sqls {
		bytes += len(sql)
	}
	b.SetBytes(int64(bytes))
	b.ReportAllocs()
	for b.Loop() {
		for _, sql := range sqls {
			_, _, _ = parser.Parse(sql)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(sqls)), "ns/statement")
}

// BenchmarkParse_long parses texts of growing length and nesting, to show that the time grows with
// the length of the text and not faster.
func BenchmarkParse_long(b *testing.B) {
	for _, n := range []int{1_000, 10_000, 100_000} {
		texts := map[string]string{
			"list":    "select " + strings.Repeat("1, ", n) + "1",
			"chain":   "select " + strings.Repeat("1 + ", n) + "1",
			"nesting": "select " + strings.Repeat("(", min(n, 2000)) + "1" + strings.Repeat(")", min(n, 2000)),
			"values":  "values " + strings.Repeat("(1), ", n) + "(1)",
			"many":    strings.Repeat("select 1;", n),
		}
		for name, src := range texts {
			b.Run(fmt.Sprint(name, "/", n), func(b *testing.B) {
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					_, _ = parser.ParseAll(src)
				}
			})
		}
	}
}

// TestParse_expressions pins the shape of the tree for every form of expression, and for the traps
// of SQLite's precedence: `NOT x NOT NULL` is `NOT (x NOT NULL)`, an AND inside BETWEEN belongs to
// BETWEEN, unary minus binds tighter than COLLATE, `a = b = c` is `(a = b) = c`, and so on. Each
// expression is read as `SELECT expr`, and the tree of the expression is dumped with [ast.Dump].
func TestParse_expressions(t *testing.T) {
	tests := []struct {
		expr string
		tree string
	}{
		{`NOT x NOT NULL`, `Unary{Op: NOT, X: NullTest{X: ColumnRef{Column: x}, Op: NOT, Null: NULL}}`},
		{`NOT x = y`, `Unary{Op: NOT, X: Binary{X: ColumnRef{Column: x}, Op: =, Y: ColumnRef{Column: y}}}`},
		{`NOT x AND y`, `Binary{X: Unary{Op: NOT, X: ColumnRef{Column: x}}, Op: AND, Y: ColumnRef{Column: y}}`},
		{`NOT x BETWEEN a AND b`, `Unary{Op: NOT, X: Between{X: ColumnRef{Column: x}, Between: BETWEEN, Low: ColumnRef{Column: a}, And: AND, High: ColumnRef{Column: b}}}`},
		{`x NOT NULL`, `NullTest{X: ColumnRef{Column: x}, Op: NOT, Null: NULL}`},
		{`x NOTNULL`, `NullTest{X: ColumnRef{Column: x}, Op: NOTNULL}`},
		{`x IS NOT NULL`, `Is{X: ColumnRef{Column: x}, Is: IS, Not: NOT, Y: Literal{Value: NULL}}`},
		{`x ISNULL`, `NullTest{X: ColumnRef{Column: x}, Op: ISNULL}`},
		{`x IS NULL`, `Is{X: ColumnRef{Column: x}, Is: IS, Y: Literal{Value: NULL}}`},
		{`a = b = c`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: =, Y: ColumnRef{Column: b}}, Op: =, Y: ColumnRef{Column: c}}`},
		{`a = b < c`, `Binary{X: ColumnRef{Column: a}, Op: =, Y: Binary{X: ColumnRef{Column: b}, Op: <, Y: ColumnRef{Column: c}}}`},
		{`a < b = c`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: <, Y: ColumnRef{Column: b}}, Op: =, Y: ColumnRef{Column: c}}`},
		{`- x COLLATE nocase`, `Collate{X: Unary{Op: -, X: ColumnRef{Column: x}}, Collate: COLLATE, Name: nocase}`},
		{`-x * y`, `Binary{X: Unary{Op: -, X: ColumnRef{Column: x}}, Op: *, Y: ColumnRef{Column: y}}`},
		{`~x || y`, `Binary{X: Unary{Op: ~, X: ColumnRef{Column: x}}, Op: ||, Y: ColumnRef{Column: y}}`},
		{`- - x`, `Unary{Op: -, X: Unary{Op: -, X: ColumnRef{Column: x}}}`},
		{`+ - x`, `Unary{Op: +, X: Unary{Op: -, X: ColumnRef{Column: x}}}`},
		{`x LIKE y ESCAPE z`, `Like{X: ColumnRef{Column: x}, Op: LIKE, Y: ColumnRef{Column: y}, Escape: ESCAPE, EscapeX: ColumnRef{Column: z}}`},
		{`x NOT LIKE y ESCAPE z || w`, `Like{X: ColumnRef{Column: x}, Not: NOT, Op: LIKE, Y: ColumnRef{Column: y}, Escape: ESCAPE, EscapeX: Binary{X: ColumnRef{Column: z}, Op: ||, Y: ColumnRef{Column: w}}}`},
		{`x LIKE a = b`, `Binary{X: Like{X: ColumnRef{Column: x}, Op: LIKE, Y: ColumnRef{Column: a}}, Op: =, Y: ColumnRef{Column: b}}`},
		{`x GLOB y`, `Like{X: ColumnRef{Column: x}, Op: GLOB, Y: ColumnRef{Column: y}}`},
		{`x NOT REGEXP y`, `Like{X: ColumnRef{Column: x}, Not: NOT, Op: REGEXP, Y: ColumnRef{Column: y}}`},
		{`x MATCH y`, `Like{X: ColumnRef{Column: x}, Op: MATCH, Y: ColumnRef{Column: y}}`},
		{`x IS NOT DISTINCT FROM y`, `Is{X: ColumnRef{Column: x}, Is: IS, Not: NOT, Distinct: DISTINCT, From: FROM, Y: ColumnRef{Column: y}}`},
		{`x IS DISTINCT FROM y`, `Is{X: ColumnRef{Column: x}, Is: IS, Distinct: DISTINCT, From: FROM, Y: ColumnRef{Column: y}}`},
		{`x IS NOT y`, `Is{X: ColumnRef{Column: x}, Is: IS, Not: NOT, Y: ColumnRef{Column: y}}`},
		{`a -> b ->> c`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: ->, Y: ColumnRef{Column: b}}, Op: ->>, Y: ColumnRef{Column: c}}`},
		{`a || b -> c`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: ||, Y: ColumnRef{Column: b}}, Op: ->, Y: ColumnRef{Column: c}}`},
		{`a || b COLLATE c`, `Binary{X: ColumnRef{Column: a}, Op: ||, Y: Collate{X: ColumnRef{Column: b}, Collate: COLLATE, Name: c}}`},
		{`a COLLATE b COLLATE c`, `Collate{X: Collate{X: ColumnRef{Column: a}, Collate: COLLATE, Name: b}, Collate: COLLATE, Name: c}`},
		{`NOT EXISTS (SELECT 1)`, `Unary{Op: NOT, X: Exists{Exists: EXISTS, LParen: (, Select: Select{First: SimpleSelect{Select: SELECT, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}}`},
		{`EXISTS (SELECT 1)`, `Exists{Exists: EXISTS, LParen: (, Select: Select{First: SimpleSelect{Select: SELECT, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}`},
		{`x NOT IN (1, 2)`, `InList{X: ColumnRef{Column: x}, Not: NOT, In: IN, LParen: (, List: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}`},
		{`x IN ()`, `InList{X: ColumnRef{Column: x}, In: IN, LParen: (, RParen: )}`},
		{`x IN (SELECT 1)`, `InSelect{X: ColumnRef{Column: x}, In: IN, LParen: (, Select: Select{First: SimpleSelect{Select: SELECT, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}`},
		{`x IN t`, `InTable{X: ColumnRef{Column: x}, In: IN, Name: {Name: t}}`},
		{`x NOT IN main.f(1, 2)`, `InTable{X: ColumnRef{Column: x}, Not: NOT, In: IN, Name: {Schema: main, Dot: ., Name: f}, LParen: (, Args: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}`},
		{`x IN f()`, `InTable{X: ColumnRef{Column: x}, In: IN, Name: {Name: f}, LParen: (, RParen: )}`},
		{`x NOT BETWEEN a AND b`, `Between{X: ColumnRef{Column: x}, Not: NOT, Between: BETWEEN, Low: ColumnRef{Column: a}, And: AND, High: ColumnRef{Column: b}}`},
		{`x BETWEEN a AND b AND c`, `Binary{X: Between{X: ColumnRef{Column: x}, Between: BETWEEN, Low: ColumnRef{Column: a}, And: AND, High: ColumnRef{Column: b}}, Op: AND, Y: ColumnRef{Column: c}}`},
		{`x BETWEEN 1 AND 2 = 3`, `Binary{X: Between{X: ColumnRef{Column: x}, Between: BETWEEN, Low: Literal{Value: 1}, And: AND, High: Literal{Value: 2}}, Op: =, Y: Literal{Value: 3}}`},
		{`a AND b OR c AND d`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: AND, Y: ColumnRef{Column: b}}, Op: OR, Y: Binary{X: ColumnRef{Column: c}, Op: AND, Y: ColumnRef{Column: d}}}`},
		{`a OR b AND c`, `Binary{X: ColumnRef{Column: a}, Op: OR, Y: Binary{X: ColumnRef{Column: b}, Op: AND, Y: ColumnRef{Column: c}}}`},
		{`a & b | c << d >> e`, `Binary{X: Binary{X: Binary{X: Binary{X: ColumnRef{Column: a}, Op: &, Y: ColumnRef{Column: b}}, Op: |, Y: ColumnRef{Column: c}}, Op: <<, Y: ColumnRef{Column: d}}, Op: >>, Y: ColumnRef{Column: e}}`},
		{`a + b * c - d / e % f`, `Binary{X: Binary{X: ColumnRef{Column: a}, Op: +, Y: Binary{X: ColumnRef{Column: b}, Op: *, Y: ColumnRef{Column: c}}}, Op: -, Y: Binary{X: Binary{X: ColumnRef{Column: d}, Op: /, Y: ColumnRef{Column: e}}, Op: %, Y: ColumnRef{Column: f}}}`},
		{`(a, b) = (1, 2)`, `Binary{X: Row{LParen: (, Exprs: [ColumnRef{Column: a}, ColumnRef{Column: b}], Commas: [,], RParen: )}, Op: =, Y: Row{LParen: (, Exprs: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}}`},
		{`(a)`, `Paren{LParen: (, X: ColumnRef{Column: a}, RParen: )}`},
		{`(SELECT 1)`, `Subquery{LParen: (, Select: Select{First: SimpleSelect{Select: SELECT, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}`},
		{`CAST(x AS INTEGER)`, `Cast{Cast: CAST, LParen: (, X: ColumnRef{Column: x}, As: AS, Type: TypeName{Names: [INTEGER]}, RParen: )}`},
		{`CAST(x AS varchar(10))`, `Cast{Cast: CAST, LParen: (, X: ColumnRef{Column: x}, As: AS, Type: TypeName{Names: [varchar], LParen: (, Arg1: SignedNumber{Number: 10}, RParen: )}, RParen: )}`},
		{`CAST(x AS DECIMAL(10, -2))`, `Cast{Cast: CAST, LParen: (, X: ColumnRef{Column: x}, As: AS, Type: TypeName{Names: [DECIMAL], LParen: (, Arg1: SignedNumber{Number: 10}, Comma: ,, Arg2: SignedNumber{Sign: -, Number: 2}, RParen: )}, RParen: )}`},
		{`CAST(x AS)`, `Cast{Cast: CAST, LParen: (, X: ColumnRef{Column: x}, As: AS, RParen: )}`},
		{`CASE WHEN a THEN b END`, `Case{Case: CASE, Whens: [When{When: WHEN, Condition: ColumnRef{Column: a}, Then: THEN, Result: ColumnRef{Column: b}}], End: END}`},
		{`CASE x WHEN 1 THEN 'a' WHEN 2 THEN 'b' ELSE 'c' END`, `Case{Case: CASE, Operand: ColumnRef{Column: x}, Whens: [When{When: WHEN, Condition: Literal{Value: 1}, Then: THEN, Result: Literal{Value: 'a'}}, When{When: WHEN, Condition: Literal{Value: 2}, Then: THEN, Result: Literal{Value: 'b'}}], Else: ELSE, ElseX: Literal{Value: 'c'}, End: END}`},
		{`RAISE(IGNORE)`, `Raise{Raise: RAISE, LParen: (, Action: IGNORE, RParen: )}`},
		{`RAISE(ABORT, 'msg')`, `Raise{Raise: RAISE, LParen: (, Action: ABORT, Comma: ,, Message: Literal{Value: 'msg'}, RParen: )}`},
		{`count(*)`, `FuncCall{Name: count, LParen: (, Star: *, RParen: )}`},
		{`count(DISTINCT x)`, `FuncCall{Name: count, LParen: (, Distinct: DISTINCT, Args: [ColumnRef{Column: x}], RParen: )}`},
		{`count(ALL x)`, `FuncCall{Name: count, LParen: (, Distinct: ALL, Args: [ColumnRef{Column: x}], RParen: )}`},
		{`f()`, `FuncCall{Name: f, LParen: (, RParen: )}`},
		{`f(a, b ORDER BY c DESC)`, `FuncCall{Name: f, LParen: (, Args: [ColumnRef{Column: a}, ColumnRef{Column: b}], Commas: [,], OrderBy: OrderBy{Order: ORDER, By: BY, Terms: [OrderingTerm{X: ColumnRef{Column: c}, Order: DESC}]}, RParen: )}`},
		{`sum(x) FILTER (WHERE x > 0)`, `FuncCall{Name: sum, LParen: (, Args: [ColumnRef{Column: x}], RParen: ), Filter: Filter{Filter: FILTER, LParen: (, Where: WHERE, X: Binary{X: ColumnRef{Column: x}, Op: >, Y: Literal{Value: 0}}, RParen: )}}`},
		{`sum(x) OVER w`, `FuncCall{Name: sum, LParen: (, Args: [ColumnRef{Column: x}], RParen: ), Over: Over{Over: OVER, Name: w}}`},
		{`sum(x) FILTER (WHERE y) OVER (PARTITION BY a ORDER BY b ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW EXCLUDE TIES)`, `FuncCall{Name: sum, LParen: (, Args: [ColumnRef{Column: x}], RParen: ), Filter: Filter{Filter: FILTER, LParen: (, Where: WHERE, X: ColumnRef{Column: y}, RParen: )}, Over: Over{Over: OVER, LParen: (, Window: Window{Partition: PARTITION, PartitionBy: BY, PartitionExprs: [ColumnRef{Column: a}], OrderBy: OrderBy{Order: ORDER, By: BY, Terms: [OrderingTerm{X: ColumnRef{Column: b}}]}, Frame: Frame{Unit: ROWS, Between: BETWEEN, Start: FrameBound{Unbounded: UNBOUNDED, Direction: PRECEDING}, And: AND, End: FrameBound{Current: CURRENT, Row: ROW}, Exclude: EXCLUDE, Excluded: [TIES]}}, RParen: )}}`},
		{`row_number() OVER ()`, `FuncCall{Name: row_number, LParen: (, RParen: ), Over: Over{Over: OVER, LParen: (, RParen: )}}`},
		{`f(x) OVER (w RANGE 1 PRECEDING EXCLUDE NO OTHERS)`, `FuncCall{Name: f, LParen: (, Args: [ColumnRef{Column: x}], RParen: ), Over: Over{Over: OVER, LParen: (, Window: Window{Base: w, Frame: Frame{Unit: RANGE, Start: FrameBound{X: Literal{Value: 1}, Direction: PRECEDING}, Exclude: EXCLUDE, Excluded: [NO OTHERS]}}, RParen: )}}`},
		{`f(x) OVER (GROUPS BETWEEN 1 FOLLOWING AND UNBOUNDED FOLLOWING EXCLUDE CURRENT ROW)`, `FuncCall{Name: f, LParen: (, Args: [ColumnRef{Column: x}], RParen: ), Over: Over{Over: OVER, LParen: (, Window: Window{Frame: Frame{Unit: GROUPS, Between: BETWEEN, Start: FrameBound{X: Literal{Value: 1}, Direction: FOLLOWING}, And: AND, End: FrameBound{Unbounded: UNBOUNDED, Direction: FOLLOWING}, Exclude: EXCLUDE, Excluded: [CURRENT ROW]}}, RParen: )}}`},
		{`1_000`, `Literal{Value: 1_000}`},
		{`0x1F`, `Literal{Value: 0x1F}`},
		{`1.5e3`, `Literal{Value: 1.5e3}`},
		{`x'00'`, `Literal{Value: x'00'}`},
		{`'it''s'`, `Literal{Value: 'it''s'}`},
		{`NULL`, `Literal{Value: NULL}`},
		{`current_timestamp`, `Literal{Value: current_timestamp}`},
		{`?1`, `Variable{Name: ?1}`},
		{`:name`, `Variable{Name: :name}`},
		{`"t"`, `ColumnRef{Column: "t"}`},
		{`window`, `ColumnRef{Column: window}`},
		{`window x`, `ColumnRef{Column: window}`},
		{`over`, `ColumnRef{Column: over}`},
		{`filter`, `ColumnRef{Column: filter}`},
		{`[t]`, `ColumnRef{Column: [t]}`},
		{"`t`", "ColumnRef{Column: `t`}"},
		{`t.c`, `ColumnRef{Table: t, Dot2: ., Column: c}`},
		{`s.t.c`, `ColumnRef{Schema: s, Dot1: ., Table: t, Dot2: ., Column: c}`},
		{`'t'.c`, `ColumnRef{Table: 't', Dot2: ., Column: c}`},
		{`abort`, `ColumnRef{Column: abort}`},
		{`left`, `ColumnRef{Column: left}`},
		{`left(1)`, `FuncCall{Name: left, LParen: (, Args: [Literal{Value: 1}], RParen: )}`},
		{`replace(a, b, c)`, `FuncCall{Name: replace, LParen: (, Args: [ColumnRef{Column: a}, ColumnRef{Column: b}, ColumnRef{Column: c}], Commas: [, ,], RParen: )}`},
		{`x COLLATE "nocase"`, `Collate{X: ColumnRef{Column: x}, Collate: COLLATE, Name: "nocase"}`},
	}
	for _, test := range tests {
		t.Run(test.expr, func(t *testing.T) {
			src := "select " + test.expr
			stmt, _, err := parser.Parse(src)
			is.NotError(t, err)
			x := stmt.Cmd.(*ast.SelectCmd).Select.First.(*ast.SimpleSelect).Columns[0].Expr
			is.Equal(t, test.tree, ast.Dump(x))
			is.NotError(t, ast.CheckOwnership(stmt, parser.Tokens(src, stmt)))
		})
	}
}

// TestParse_statements pins the shape of the tree for every kind of statement.
func TestParse_statements(t *testing.T) {
	tests := []struct {
		sql  string
		tree string
	}{
		{`select 1 as x, 2 y, * , t.* from t`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}, As: as, Alias: x}, ResultColumn{Expr: Literal{Value: 2}, Alias: y}, ResultColumn{Star: *}, ResultColumn{Table: t, Dot: ., Star: *}], Commas: [, , ,], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: t}}}]}}}}}`},
		{`select distinct a from t where b group by c having d window w as (partition by e) order by f nulls last limit 1 offset 2`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Distinct: distinct, Columns: [ResultColumn{Expr: ColumnRef{Column: a}}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: t}}}]}, Where: Where{Where: where, X: ColumnRef{Column: b}}, GroupBy: GroupBy{Group: group, By: by, Exprs: [ColumnRef{Column: c}]}, Having: Having{Having: having, X: ColumnRef{Column: d}}, Window: WindowClause{Window: window, Defs: [WindowDef{Name: w, As: as, LParen: (, Window: Window{Partition: partition, PartitionBy: by, PartitionExprs: [ColumnRef{Column: e}]}, RParen: )}]}, OrderBy: OrderBy{Order: order, By: by, Terms: [OrderingTerm{X: ColumnRef{Column: f}, Nulls: nulls, Which: last}]}, Limit: Limit{Limit: limit, X: Literal{Value: 1}, Sep: offset, Y: Literal{Value: 2}}}}}}`},
		{`select all a from t limit 1, 2`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Distinct: all, Columns: [ResultColumn{Expr: ColumnRef{Column: a}}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: t}}}]}, Limit: Limit{Limit: limit, X: Literal{Value: 1}, Sep: ,, Y: Literal{Value: 2}}}}}}`},
		{`select 1 union all select 2 except select 3 intersect select 4 order by 1 limit 1`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}, Compounds: [Compound{Op: union, All: all, Core: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 2}}]}}, Compound{Op: except, Core: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 3}}]}}, Compound{Op: intersect, Core: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 4}}], OrderBy: OrderBy{Order: order, By: by, Terms: [OrderingTerm{X: Literal{Value: 1}}]}, Limit: Limit{Limit: limit, X: Literal{Value: 1}}}}]}}}`},
		{`select 1 order by 1 union select 2`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}], OrderBy: OrderBy{Order: order, By: by, Terms: [OrderingTerm{X: Literal{Value: 1}}]}}, Compounds: [Compound{Op: union, Core: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 2}}]}}]}}}`},
		{`values (1, 2), (3, 4)`, `Statement{Cmd: SelectCmd{Select: Select{First: Values{Values: values, Rows: [ValuesRow{LParen: (, Exprs: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}, ValuesRow{LParen: (, Exprs: [Literal{Value: 3}, Literal{Value: 4}], Commas: [,], RParen: )}], Commas: [,]}}}}`},
		{`with recursive c(x) as materialized (values(1)), d as not materialized (select 2) select * from c, d`, `Statement{Cmd: SelectCmd{Select: Select{With: With{With: with, Recursive: recursive, CTEs: [CTE{Name: c, Columns: ColumnNames{LParen: (, Columns: [ColumnName{Name: x}], RParen: )}, As: as, Materialized: materialized, LParen: (, Select: Select{First: Values{Values: values, Rows: [ValuesRow{LParen: (, Exprs: [Literal{Value: 1}], RParen: )}]}}, RParen: )}, CTE{Name: d, As: as, Not: not, Materialized: materialized, LParen: (, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 2}}]}}, RParen: )}], Commas: [,]}, First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: c}}}, FromItem{Join: JoinOp{Comma: ,}, Source: TableSource{Name: {Name: d}}}]}}}}}`},
		{`select * from a left outer join b using (x) natural join c cross join d on 1, e as e1 indexed by i, f not indexed`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: a}}}, FromItem{Join: JoinOp{Words: [left outer], Join: join}, Source: TableSource{Name: {Name: b}}, Constraint: JoinUsing{Using: using, LParen: (, Columns: IDList{Names: [x]}, RParen: )}}, FromItem{Join: JoinOp{Words: [natural], Join: join}, Source: TableSource{Name: {Name: c}}}, FromItem{Join: JoinOp{Words: [cross], Join: join}, Source: TableSource{Name: {Name: d}}, Constraint: JoinOn{On: on, X: Literal{Value: 1}}}, FromItem{Join: JoinOp{Comma: ,}, Source: TableSource{Name: {Name: e}}, As: as, Alias: e1, Indexed: IndexedBy{Indexed: indexed, By: by, Index: i}}, FromItem{Join: JoinOp{Comma: ,}, Source: TableSource{Name: {Name: f}}, Indexed: IndexedBy{Not: not, Indexed: indexed}}]}}}}}`},
		{`select * from (select 1) as s, (a join b), f(1, 2) g`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: SubquerySource{LParen: (, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}, As: as, Alias: s}, FromItem{Join: JoinOp{Comma: ,}, Source: NestedFrom{LParen: (, Items: [FromItem{Source: TableSource{Name: {Name: a}}}, FromItem{Join: JoinOp{Join: join}, Source: TableSource{Name: {Name: b}}}], RParen: )}}, FromItem{Join: JoinOp{Comma: ,}, Source: TableFunction{Name: {Name: f}, LParen: (, Args: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}, Alias: g}]}}}}}`},
		{`select * from t on 1`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: t}}, Constraint: JoinOn{On: on, X: Literal{Value: 1}}}]}}}}}`},
		{`explain query plan select 1`, `Statement{Explain: explain, Query: query, Plan: plan, Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}}`},
		{`explain select 1;`, `Statement{Explain: explain, Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}, Semi: ;}`},
		{`insert into t default values returning *`, `Statement{Cmd: Insert{Insert: insert, Into: into, Table: TargetTable{Name: {Name: t}}, Default: default, Values: values, Returning: Returning{Returning: returning, Columns: [ResultColumn{Star: *}]}}}`},
		{`insert or replace into s.t as x (a, b) values (1, 2) on conflict (a) where a > 0 do update set b = excluded.b where 1 on conflict do nothing returning a`, `Statement{Cmd: Insert{Insert: insert, Or: or, Resolution: replace, Into: into, Table: TargetTable{Name: {Schema: s, Dot: ., Name: t}, As: as, Alias: x}, LParen: (, Columns: IDList{Names: [a b], Commas: [,]}, RParen: ), Select: Select{First: Values{Values: values, Rows: [ValuesRow{LParen: (, Exprs: [Literal{Value: 1}, Literal{Value: 2}], Commas: [,], RParen: )}]}}, Upserts: [Upsert{On: on, Conflict: conflict, LParen: (, Target: [OrderingTerm{X: ColumnRef{Column: a}}], RParen: ), TargetWhere: Where{Where: where, X: Binary{X: ColumnRef{Column: a}, Op: >, Y: Literal{Value: 0}}}, Do: do, Update: update, Set: set, Assignments: [Assignment{Column: b, Eq: =, X: ColumnRef{Table: excluded, Dot2: ., Column: b}}], Where: Where{Where: where, X: Literal{Value: 1}}}, Upsert{On: on, Conflict: conflict, Do: do, Nothing: nothing}], Returning: Returning{Returning: returning, Columns: [ResultColumn{Expr: ColumnRef{Column: a}}]}}}`},
		{`replace into t select * from u`, `Statement{Cmd: Insert{Insert: replace, Into: into, Table: TargetTable{Name: {Name: t}}, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: u}}}]}}}}}`},
		{`with c as (select 1) insert into t select * from c`, `Statement{Cmd: Insert{With: With{With: with, CTEs: [CTE{Name: c, As: as, LParen: (, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}, RParen: )}]}, Insert: insert, Into: into, Table: TargetTable{Name: {Name: t}}, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Star: *}], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: c}}}]}}}}}`},
		{`update or ignore t indexed by i set a = 1, (b, c) = (2, 3) from u where x returning *`, `Statement{Cmd: Update{Update: update, Or: or, Resolution: ignore, Table: TargetTable{Name: {Name: t}}, Indexed: IndexedBy{Indexed: indexed, By: by, Index: i}, Set: set, Assignments: [Assignment{Column: a, Eq: =, X: Literal{Value: 1}}, Assignment{LParen: (, Columns: IDList{Names: [b c], Commas: [,]}, RParen: ), Eq: =, X: Row{LParen: (, Exprs: [Literal{Value: 2}, Literal{Value: 3}], Commas: [,], RParen: )}}], Commas: [,], From: From{From: from, Items: [FromItem{Source: TableSource{Name: {Name: u}}}]}, Where: Where{Where: where, X: ColumnRef{Column: x}}, Returning: Returning{Returning: returning, Columns: [ResultColumn{Star: *}]}}}`},
		{`delete from t as x not indexed where a returning a`, `Statement{Cmd: Delete{Delete: delete, From: from, Table: TargetTable{Name: {Name: t}, As: as, Alias: x}, Indexed: IndexedBy{Not: not, Indexed: indexed}, Where: Where{Where: where, X: ColumnRef{Column: a}}, Returning: Returning{Returning: returning, Columns: [ResultColumn{Expr: ColumnRef{Column: a}}]}}}`},
		{`create temp table if not exists s.t (a integer primary key desc on conflict replace autoincrement, b text not null default 'x' collate nocase, c references u (x) on delete set null on update cascade match full deferrable initially deferred, d as (a + 1) stored, e generated always as (1) virtual, f default -1, g default current_time, h default abc, i check (i > 0) unique, constraint pk primary key (a, b) on conflict fail, unique (c), check (d) on conflict ignore foreign key (e) references u not deferrable) without rowid, strict`, `Statement{Cmd: CreateTable{Create: create, Temp: temp, Table: table, IfNotExists: {If: if, Not: not, Exists: exists}, Name: {Schema: s, Dot: ., Name: t}, LParen: (, Columns: [ColumnDef{Name: a, Type: TypeName{Names: [integer]}, Constraints: [PrimaryKeyConstraint{Primary: primary, Key: key, Order: desc, Conflict: OnConflict{On: on, Conflict: conflict, Resolution: replace}, Autoincrement: autoincrement}]}, ColumnDef{Name: b, Type: TypeName{Names: [text]}, Constraints: [NullConstraint{Not: not, Null: null}, DefaultConstraint{Default: default, X: Literal{Value: 'x'}}, CollateConstraint{Collate: collate, Name: nocase}]}, ColumnDef{Name: c, Constraints: [ReferencesConstraint{References: References{References: references, Table: u, Columns: ColumnNames{LParen: (, Columns: [ColumnName{Name: x}], RParen: )}, Args: [RefArg{On: on, Event: delete, Action: [set null]}, RefArg{On: on, Event: update, Action: [cascade]}, RefArg{Match: match, Name: full}]}}, DeferConstraint{Defer: Deferrable{Deferrable: deferrable, Initially: initially, Mode: deferred}}]}, ColumnDef{Name: d, Constraints: [GeneratedConstraint{As: as, LParen: (, X: Binary{X: ColumnRef{Column: a}, Op: +, Y: Literal{Value: 1}}, RParen: ), Storage: stored}]}, ColumnDef{Name: e, Type: TypeName{Names: [generated always]}, Constraints: [GeneratedConstraint{As: as, LParen: (, X: Literal{Value: 1}, RParen: ), Storage: virtual}]}, ColumnDef{Name: f, Constraints: [DefaultConstraint{Default: default, Sign: -, X: Literal{Value: 1}}]}, ColumnDef{Name: g, Constraints: [DefaultConstraint{Default: default, X: Literal{Value: current_time}}]}, ColumnDef{Name: h, Constraints: [DefaultConstraint{Default: default, Name: abc}]}, ColumnDef{Name: i, Constraints: [CheckConstraint{Check: check, LParen: (, X: Binary{X: ColumnRef{Column: i}, Op: >, Y: Literal{Value: 0}}, RParen: )}, UniqueConstraint{Unique: unique}]}], Constraints: [ConstraintName{Constraint: constraint, Name: pk}, TablePrimaryKey{Primary: primary, Key: key, LParen: (, Columns: [OrderingTerm{X: ColumnRef{Column: a}}, OrderingTerm{X: ColumnRef{Column: b}}], Commas: [,], RParen: ), Conflict: OnConflict{On: on, Conflict: conflict, Resolution: fail}}, TableUnique{Unique: unique, LParen: (, Columns: [OrderingTerm{X: ColumnRef{Column: c}}], RParen: )}, CheckConstraint{Check: check, LParen: (, X: ColumnRef{Column: d}, RParen: ), Conflict: OnConflict{On: on, Conflict: conflict, Resolution: ignore}}, TableForeignKey{Foreign: foreign, Key: key, Columns: ColumnNames{LParen: (, Columns: [ColumnName{Name: e}], RParen: )}, References: References{References: references, Table: u}, Defer: Deferrable{Not: not, Deferrable: deferrable}}], Commas: [, , , , , , , , , , ,], RParen: ), Options: [TableOption{Without: without, Name: rowid}, TableOption{Name: strict}], OptionCommas: [,]}}`},
		{`create table t as select 1`, `Statement{Cmd: CreateTable{Create: create, Table: table, Name: {Name: t}, As: as, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}}`},
		{`create table t(a int generated always as (1))`, `Statement{Cmd: CreateTable{Create: create, Table: table, Name: {Name: t}, LParen: (, Columns: [ColumnDef{Name: a, Type: TypeName{Names: [int generated always]}, Constraints: [GeneratedConstraint{As: as, LParen: (, X: Literal{Value: 1}, RParen: )}]}], RParen: )}}`},
		{`create table t(a), strict`, `Statement{Cmd: CreateTable{Create: create, Table: table, Name: {Name: t}, LParen: (, Columns: [ColumnDef{Name: a}], RParen: ), Options: [TableOption{Name: strict}], OptionCommas: [,]}}`},
		{`create table t(a varchar(1, 2) null)`, `Statement{Cmd: CreateTable{Create: create, Table: table, Name: {Name: t}, LParen: (, Columns: [ColumnDef{Name: a, Type: TypeName{Names: [varchar], LParen: (, Arg1: SignedNumber{Number: 1}, Comma: ,, Arg2: SignedNumber{Number: 2}, RParen: )}, Constraints: [NullConstraint{Null: null}]}], RParen: )}}`},
		{`create unique index if not exists i on t (a collate nocase desc, b + 1) where c`, `Statement{Cmd: CreateIndex{Create: create, Unique: unique, Index: index, IfNotExists: {If: if, Not: not, Exists: exists}, Name: {Name: i}, On: on, Table: t, LParen: (, Columns: [OrderingTerm{X: Collate{X: ColumnRef{Column: a}, Collate: collate, Name: nocase}, Order: desc}, OrderingTerm{X: Binary{X: ColumnRef{Column: b}, Op: +, Y: Literal{Value: 1}}}], Commas: [,], RParen: ), Where: Where{Where: where, X: ColumnRef{Column: c}}}}`},
		{`create view if not exists v (a collate x desc, b) as select 1, 2`, `Statement{Cmd: CreateView{Create: create, View: view, IfNotExists: {If: if, Not: not, Exists: exists}, Name: {Name: v}, Columns: ColumnNames{LParen: (, Columns: [ColumnName{Name: a, Collate: collate, Collation: x, Order: desc}, ColumnName{Name: b}], Commas: [,], RParen: )}, As: as, Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}, ResultColumn{Expr: Literal{Value: 2}}], Commas: [,]}}}}`},
		{`create trigger if not exists tr instead of update of a, b on v for each row when new.a begin update t set x = 1; insert into t values (1); delete from t; select 1; end`, `Statement{Cmd: CreateTrigger{Create: create, Trigger: trigger, IfNotExists: {If: if, Not: not, Exists: exists}, Name: {Name: tr}, Time: instead, TimeOf: of, Event: update, EventOf: of, Columns: IDList{Names: [a b], Commas: [,]}, On: on, Table: {Name: v}, For: for, Each: each, Row: row, When: when, WhenX: ColumnRef{Table: new, Dot2: ., Column: a}, Begin: begin, Steps: [Update{Update: update, Table: TargetTable{Name: {Name: t}}, Set: set, Assignments: [Assignment{Column: x, Eq: =, X: Literal{Value: 1}}]}, Insert{Insert: insert, Into: into, Table: TargetTable{Name: {Name: t}}, Select: Select{First: Values{Values: values, Rows: [ValuesRow{LParen: (, Exprs: [Literal{Value: 1}], RParen: )}]}}}, Delete{Delete: delete, From: from, Table: TargetTable{Name: {Name: t}}}, SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}], Semis: [; ; ; ;], End: end}}`},
		{`create temp trigger tr after insert on t begin select 1; end`, `Statement{Cmd: CreateTrigger{Create: create, Temp: temp, Trigger: trigger, Name: {Name: tr}, Time: after, Event: insert, On: on, Table: {Name: t}, Begin: begin, Steps: [SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}], Semis: [;], End: end}}`},
		{`create virtual table t using fts5 (a, b (c, d), , 'x;y')`, `Statement{Cmd: CreateVirtualTable{Create: create, Virtual: virtual, Table: table, Name: {Name: t}, Using: using, Module: fts5, LParen: (, Args: [ModuleArg{Tokens: [a]}, ModuleArg{Tokens: [b ( c , d )]}, ModuleArg{}, ModuleArg{Tokens: ['x;y']}], Commas: [, , ,], RParen: )}}`},
		{`create virtual table t using m`, `Statement{Cmd: CreateVirtualTable{Create: create, Virtual: virtual, Table: table, Name: {Name: t}, Using: using, Module: m}}`},
		{`drop table if exists s.t`, `Statement{Cmd: Drop{Drop: drop, Object: table, IfExists: {If: if, Exists: exists}, Name: {Schema: s, Dot: ., Name: t}}}`},
		{`drop view v`, `Statement{Cmd: Drop{Drop: drop, Object: view, Name: {Name: v}}}`},
		{`drop index i`, `Statement{Cmd: Drop{Drop: drop, Object: index, Name: {Name: i}}}`},
		{`drop trigger if exists tr`, `Statement{Cmd: Drop{Drop: drop, Object: trigger, IfExists: {If: if, Exists: exists}, Name: {Name: tr}}}`},
		{`alter table t rename to u`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: RenameTable{Rename: rename, To: to, Name: u}}}`},
		{`alter table t rename column a to b`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: RenameColumn{Rename: rename, Column: column, Old: a, To: to, New: b}}}`},
		{`alter table t rename a to b`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: RenameColumn{Rename: rename, Old: a, To: to, New: b}}}`},
		{`alter table t add column c int not null default 0`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AddColumn{Add: add, Column: column, Def: ColumnDef{Name: c, Type: TypeName{Names: [int]}, Constraints: [NullConstraint{Not: not, Null: null}, DefaultConstraint{Default: default, X: Literal{Value: 0}}]}}}}`},
		{`alter table t add d`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AddColumn{Add: add, Def: ColumnDef{Name: d}}}}`},
		{`alter table t drop column c`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: DropColumn{Drop: drop, Column: column, Name: c}}}`},
		{`alter table t drop constraint c`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: DropConstraint{Drop: drop, Constraint: constraint, Name: c}}}`},
		{`alter table t alter column c drop not null`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AlterColumnNotNull{Alter: alter, Column: column, Name: c, Drop: drop, Not: not, Null: null}}}`},
		{`alter table t alter c set not null on conflict abort`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AlterColumnNotNull{Alter: alter, Name: c, Set: set, Not: not, Null: null, Conflict: OnConflict{On: on, Conflict: conflict, Resolution: abort}}}}`},
		{`alter table t add constraint c check (x > 0)`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AddCheck{Add: add, Constraint: constraint, Name: c, Check: CheckConstraint{Check: check, LParen: (, X: Binary{X: ColumnRef{Column: x}, Op: >, Y: Literal{Value: 0}}, RParen: )}}}}`},
		{`alter table t add check (x > 0) on conflict fail`, `Statement{Cmd: AlterTable{Alter: alter, Table: table, Name: {Name: t}, Action: AddCheck{Add: add, Check: CheckConstraint{Check: check, LParen: (, X: Binary{X: ColumnRef{Column: x}, Op: >, Y: Literal{Value: 0}}, RParen: ), Conflict: OnConflict{On: on, Conflict: conflict, Resolution: fail}}}}}`},
		{`begin deferred transaction tx`, `Statement{Cmd: Begin{Begin: begin, Mode: deferred, Transaction: transaction, Name: tx}}`},
		{`begin`, `Statement{Cmd: Begin{Begin: begin}}`},
		{`commit transaction`, `Statement{Cmd: Commit{Commit: commit, Transaction: transaction}}`},
		{`end`, `Statement{Cmd: Commit{Commit: end}}`},
		{`rollback transaction to savepoint sp`, `Statement{Cmd: Rollback{Rollback: rollback, Transaction: transaction, To: to, Savepoint: savepoint, SavepointName: sp}}`},
		{`rollback`, `Statement{Cmd: Rollback{Rollback: rollback}}`},
		{`savepoint sp`, `Statement{Cmd: Savepoint{Savepoint: savepoint, Name: sp}}`},
		{`release savepoint sp`, `Statement{Cmd: Release{Release: release, Savepoint: savepoint, Name: sp}}`},
		{`release sp`, `Statement{Cmd: Release{Release: release, Name: sp}}`},
		{`pragma s.cache_size = -2000`, `Statement{Cmd: Pragma{Pragma: pragma, Name: {Schema: s, Dot: ., Name: cache_size}, Eq: =, Value: PragmaValue{Sign: -, Value: 2000}}}`},
		{`pragma table_info('t')`, `Statement{Cmd: Pragma{Pragma: pragma, Name: {Name: table_info}, LParen: (, Value: PragmaValue{Value: 't'}, RParen: )}}`},
		{`pragma journal_mode = wal`, `Statement{Cmd: Pragma{Pragma: pragma, Name: {Name: journal_mode}, Eq: =, Value: PragmaValue{Value: wal}}}`},
		{`pragma foreign_keys = on`, `Statement{Cmd: Pragma{Pragma: pragma, Name: {Name: foreign_keys}, Eq: =, Value: PragmaValue{Value: on}}}`},
		{`pragma x`, `Statement{Cmd: Pragma{Pragma: pragma, Name: {Name: x}}}`},
		{`vacuum s into 'file'`, `Statement{Cmd: Vacuum{Vacuum: vacuum, Schema: s, Into: into, File: Literal{Value: 'file'}}}`},
		{`vacuum`, `Statement{Cmd: Vacuum{Vacuum: vacuum}}`},
		{`attach database 'f' as a key 'k'`, `Statement{Cmd: Attach{Attach: attach, Database: database, File: Literal{Value: 'f'}, As: as, Name: ColumnRef{Column: a}, Key: key, KeyX: Literal{Value: 'k'}}}`},
		{`detach a`, `Statement{Cmd: Detach{Detach: detach, Name: ColumnRef{Column: a}}}`},
		{`reindex`, `Statement{Cmd: Reindex{Reindex: reindex}}`},
		{`reindex s.i`, `Statement{Cmd: Reindex{Reindex: reindex, Name: {Schema: s, Dot: ., Name: i}}}`},
		{`analyze t`, `Statement{Cmd: Analyze{Analyze: analyze, Name: {Name: t}}}`},
		{`;; select 1;`, `Statement{Cmd: SelectCmd{Select: Select{First: SimpleSelect{Select: select, Columns: [ResultColumn{Expr: Literal{Value: 1}}]}}}, Semi: ;}`},
	}
	for _, test := range tests {
		t.Run(test.sql, func(t *testing.T) {
			stmt, _, err := parser.Parse(test.sql)
			is.NotError(t, err)
			is.Equal(t, test.tree, ast.Dump(stmt))
			is.NotError(t, ast.CheckOwnership(stmt, parser.Tokens(test.sql, stmt)))
		})
	}
}
