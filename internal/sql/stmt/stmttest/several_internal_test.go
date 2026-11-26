package stmttest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestSequence_several(t *testing.T) {
	t.Run("should write texts of two to five statements of every kind, with what stands around them and the faults of the area", func(t *testing.T) {
		reached := map[string]int{}
		for k := range uint64(30) {
			q, err := NewSequence(Several.Seed(k))
			is.NotError(t, err)
			for range 100 {
				c := q.Next()
				if c.Statements == 0 {
					continue
				}
				is.True(t, !c.Supported, "a text of several statements is no statement alone")
				reached[fmt.Sprint(c.Statements, " statements")]++
				if c.Query {
					reached["query"]++
				}
				for _, form := range []string{"create trigger", `"c;`, "'k;", "/* a; b */", "/* ; */", "-- a;", "-- ;"} {
					if strings.Contains(c.Src, form) {
						reached[form]++
					}
				}
				if !c.Each {
					reached["a text that is no statement of the sequence"]++
				}
				is.True(t, len(c.Supports) >= c.Statements, "one of Supports for each statement")
				if len(c.Supports) > c.Statements {
					reached["a text of Others of two statements"]++
				}
				is.Equal(t, c.Each, !slices.Contains(c.Supports, false), "Each is whether all of Supports are")
				parts, err := stmt.Split(c.Src)
				if ns, ok := errors.AsType[*stmt.NotSupportedError](err); ok && strings.HasPrefix(ns.What, "space ") {
					reached[ns.What]++
					continue
				}
				n := 0
				for i, part := range parts {
					n += part.Statement.Parameters()
					kind := strings.TrimPrefix(fmt.Sprintf("%T", part.Statement), "*stmt.")
					reached[kind]++
					if i == len(parts)-1 && returns(part.Statement) {
						reached["last statement with RETURNING"]++
					}
				}
				switch {
				case err != nil:
					reached["a statement that Split refuses"]++
				case len(c.Args) < n:
					reached["fewer arguments than parameters"]++
				case len(c.Args) > n:
					reached["more arguments than parameters"]++
				case n > 0:
					reached["as many arguments as parameters"]++
				}
				if len(parts) == 0 {
					continue
				}
				if last := parts[len(parts)-1].Statement.Bounds().End; c.Src[last-1] == ';' && strings.TrimSpace(c.Src[last:]) != "" {
					reached["an empty statement or a comment at the end"]++
				}
				texts := map[string]bool{}
				for _, part := range parts {
					b := part.Statement.Bounds()
					text := strings.TrimSuffix(c.Src[b.Start:b.End], ";")
					if texts[text] {
						reached["a statement twice"]++
					}
					texts[text] = true
				}
			}
		}
		for _, what := range []string{"2 statements", "3 statements", "4 statements", "5 statements", "query", "a text that is no statement of the sequence",
			"space after a statement that SQLite reads as part of a token", "space at the end of the text that SQLite reads as part of a token",
			"CreateTable", "Insert", "Select", "Update", "Delete", "CreateIndex", "DropIndex", "DropTable", "last statement with RETURNING",
			"a statement that Split refuses", "fewer arguments than parameters", "more arguments than parameters", "as many arguments as parameters",
			"an empty statement or a comment at the end", "a statement twice",
			"create trigger", `"c;`, "'k;", "/* a; b */", "/* ; */", "-- a;", "-- ;"} {
			is.True(t, reached[what] >= 3, fmt.Sprint(what, ": ", reached[what]))
		}
		// Rare: one text of Others in 24, and one form of them.
		is.True(t, reached["a text of Others of two statements"] > 0, "a text of Others of two statements")
	})

	t.Run("should count the statements of a text of Others as SQLite reads them", func(t *testing.T) {
		is.Equal(t, 2, statementsIn("select * from t; select * from t"))
		is.Equal(t, 1, statementsIn("select * from t; -- c"))
		is.Equal(t, 1, statementsIn("insert into t (a) values ('a''b); select 1"))
		is.Equal(t, 0, statementsIn(" ; -- nothing"))
	})

	t.Run("should end no statement in a line comment that would hold the statement after it", func(t *testing.T) {
		is.True(t, openComment("drop table t; -- end"))
		is.True(t, !openComment("drop table t -- c\n"))
		is.True(t, !openComment("drop table t; /* -- */"))
		is.True(t, !openComment(""))
	})
}
