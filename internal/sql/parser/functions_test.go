package parser_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sqlite"
)

// slowChanging functions of SQLite: those that its source declares with DFUNCTION or STR_FUNCTION,
// which have SQLITE_FUNC_SLOCHNG without SQLITE_FUNC_CONSTANT; pragma_function_list does not show
// that flag.
var slowChanging = map[string]bool{"current_date": true, "current_time": true, "current_timestamp": true,
	"sqlite_compileoption_get": true, "sqlite_compileoption_used": true, "sqlite_source_id": true, "sqlite_version": true}

// TestBuiltinFunctions checks the table of built-in functions of the parser against the binding:
// against pragma_function_list, and against SQLite's own decision of whether a call is constant,
// which shows in the offset of the error of #N after a multi-row VALUES.
func TestBuiltinFunctions(t *testing.T) {
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	defer func() { _ = c.Close() }()
	s, err := c.Prepare("select name, type, narg, flags from pragma_function_list where builtin order by name, narg, type")
	is.NotError(t, err)
	defer func() { _ = s.Finalize() }()
	var want []string
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			break
		}
		name, typ, narg, flags := s.ColumnText(0), s.ColumnText(1), s.ColumnInt64(2), s.ColumnInt64(3)
		constant := typ == "s" && (flags&0x800 != 0 || slowChanging[name])
		want = append(want, fmt.Sprintf("%v %v %v", name, narg, constant))
	}
	is.EqualSlice(t, want, parser.BuiltinFunctions())

	t.Run("should decide as SQLite whether a call of every function is constant", func(t *testing.T) {
		seen := map[string]bool{}
		for _, f := range parser.BuiltinFunctions() {
			name, _, _ := strings.Cut(f, " ")
			if seen[name] {
				continue
			}
			seen[name] = true
			for n := range 5 {
				args := strings.TrimSuffix(strings.Repeat("1,", n), ",")
				for _, src := range []string{
					fmt.Sprintf("values(0),(%v(%v)) union select #0", name, args),
					fmt.Sprintf("values(0),(\"%v\"(%v)) union select #0", strings.ToUpper(name), args),
				} {
					d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), src)
					is.True(t, !found, d.String())
				}
			}
		}
	})
}
