package core_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
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
