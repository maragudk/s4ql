package parser_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parsetest"
)

// TestParse_schemaRead compares the parser with SQLite on the offset of the error of #N at the end of
// a statement with a multi-row VALUES, for which SQLite reads the schema while it parses if its rows
// are constant: for every form of expression in the rows, and every place of the VALUES.
func TestParse_schemaRead(t *testing.T) {
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
	kept := 0
	for _, e := range exprs {
		for _, shape := range shapes {
			src := strings.ReplaceAll(shape, "E", e)
			sqls = append(sqls, src)
			if r := parse(src); r.Offset >= 0 {
				kept++
			}
		}
	}
	r := parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
	t.Logf("%v texts, %v keep the offset, SQLite does not say on %v", len(sqls), kept, r.NotSaid)
	// Both outcomes must be common, or the test checks little.
	is.True(t, kept > len(sqls)/5 && kept < len(sqls)*4/5, fmt.Sprint(kept, " of ", len(sqls), " keep the offset"))
	is.True(t, r.NotSaid < len(sqls)/5, fmt.Sprint("SQLite does not say on ", r.NotSaid))
}
