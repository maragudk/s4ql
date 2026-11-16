package corpus_test

import (
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/corpus"
)

func TestEncode(t *testing.T) {
	tests := []struct {
		name     string
		sql      string
		expected string
	}{
		{name: "should keep printable ASCII", sql: "select 'a', \"b\" from t", expected: "select 'a', \"b\" from t"},
		{name: "should escape line breaks, tabs, and the backslash", sql: "a\nb\tc\rd\\e", expected: `a\nb\tc\rd\\e`},
		{name: "should escape control bytes and NUL", sql: "a\x00b\x0bc\x7f", expected: `a\x00b\x0bc\x7f`},
		{name: "should keep printable UTF-8", sql: "select 'é日'", expected: "select 'é日'"},
		{name: "should escape bytes that are not UTF-8", sql: "a\xffb\xc3", expected: `a\xffb\xc3`},
		{name: "should escape characters that are not printable", sql: "\ufeffa\u00a0b", expected: `\xef\xbb\xbfa\xc2\xa0b`},
		{name: "should escape a # at the start only", sql: "#1 #2", expected: `\x231 #2`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.Equal(t, test.expected, corpus.Encode(test.sql))
			sql, err := corpus.Decode(test.expected)
			is.NotError(t, err)
			is.Equal(t, test.sql, sql)
		})
	}

	t.Run("should give back every byte through Decode", func(t *testing.T) {
		var b []byte
		for c := range 256 {
			b = append(b, byte(c), 'a')
		}
		sql, err := corpus.Decode(corpus.Encode(string(b)))
		is.NotError(t, err)
		is.Equal(t, string(b), sql)
	})
}

func TestDecode(t *testing.T) {
	t.Run("should refuse malformed lines", func(t *testing.T) {
		for _, line := range []string{"", `a\`, `a\q`, `a\x`, `a\x1`, `a\xg0`, `a\xA0`, "a\nb"} {
			_, err := corpus.Decode(line)
			is.True(t, err != nil, line)
		}
	})
}

func TestParse(t *testing.T) {
	t.Run("should skip empty lines and comments", func(t *testing.T) {
		sqls, err := corpus.Parse("# comment\n\nselect 1\n\\x23 2\n")
		is.NotError(t, err)
		is.EqualSlice(t, []string{"select 1", "# 2"}, sqls)
	})

	t.Run("should say which line is malformed", func(t *testing.T) {
		_, err := corpus.Parse("select 1\nselect \\q\n")
		is.Equal(t, `line 2: unknown escape \q`, err.Error())
	})
}

func TestRead(t *testing.T) {
	for _, source := range []string{"tcl", "doc", "fuzz"} {
		t.Run("should hold the generated "+source+" statements sorted, unique, and in the canonical form", func(t *testing.T) {
			sqls, err := corpus.Read(source)
			is.NotError(t, err)
			is.True(t, len(sqls) > 1000, "too few statements")
			is.True(t, slices.IsSorted(sqls), "not sorted")
			is.Equal(t, len(sqls), len(slices.Compact(slices.Clone(sqls))), "not unique")
			for _, sql := range sqls {
				is.Equal(t, sql, strings.Trim(sql, " \t\n\f\r"), "not trimmed")
				decoded, err := corpus.Decode(corpus.Encode(sql))
				is.NotError(t, err)
				is.Equal(t, sql, decoded)
			}
		})
	}

	t.Run("should hold the hand-written statements", func(t *testing.T) {
		sqls, err := corpus.Read("own")
		is.NotError(t, err)
		is.True(t, len(sqls) > 100)
	})

	t.Run("should fail for a source that does not exist", func(t *testing.T) {
		_, err := corpus.Read("nosuch")
		is.True(t, err != nil)
	})
}

func TestAll(t *testing.T) {
	t.Run("should give every statement once, source by source", func(t *testing.T) {
		entries := corpus.All()
		seen := map[string]bool{}
		last := 0
		for _, e := range entries {
			is.True(t, !seen[e.SQL], "duplicate: "+e.SQL)
			seen[e.SQL] = true
			i := slices.Index(corpus.Sources, e.Source)
			is.True(t, i >= last, "out of order: "+e.Source)
			last = i
		}
		is.True(t, len(entries) > 100_000)
	})
}
