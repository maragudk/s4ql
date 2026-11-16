package main

import (
	"testing"

	"maragu.dev/is"
)

func TestTclBlocks(t *testing.T) {
	t.Run("should take the SQL argument of each command, after its name", func(t *testing.T) {
		script := `
do_execsql_test 1.0 {
  CREATE TABLE t1(a, b);
} {}
do_catchsql_test 1.1 { SELECT * FROM t2 } {1 {no such table: t2}}
do_test 1.2 {
  execsql { SELECT 1 }
  db eval {SELECT 2}
  catchsql "SELECT \"x\"\n FROM t"
} {1}
set x [execsql {SELECT 3}]
do_eqp_test 1.3 { SELECT 4 } {SCAN t1}
sqlite3_prepare_v2 db {SELECT 5} -1 TAIL
`
		is.EqualSlice(t, []string{
			"\n  CREATE TABLE t1(a, b);\n",
			" SELECT * FROM t2 ",
			" SELECT 1 ",
			"SELECT 2",
			"SELECT \"x\"\n FROM t",
			"SELECT 3",
			" SELECT 4 ",
			"SELECT 5",
		}, tclBlocks(script))
	})

	t.Run("should keep nested braces and skip escaped ones", func(t *testing.T) {
		is.EqualSlice(t, []string{`SELECT '{a}' AS "\}"`}, tclBlocks(`execsql {SELECT '{a}' AS "\}"}`))
	})

	t.Run("should skip an argument that is a variable or a command", func(t *testing.T) {
		is.Equal(t, 0, len(tclBlocks("execsql $sql\nexecsql [list a]\n")))
	})

	t.Run("should skip a block that is not closed", func(t *testing.T) {
		is.Equal(t, 0, len(tclBlocks("execsql {SELECT 1")))
	})
}

func TestPreBlocks(t *testing.T) {
	t.Run("should take the text of pre blocks without tags, entities, and prompts", func(t *testing.T) {
		page := `<p>x</p><pre class="terminal">sqlite&gt; SELECT <b>a</b> &lt; 2;
   ...&gt; SELECT 3;</pre><pre>CREATE TABLE t(x &amp; y)</pre>`
		is.EqualSlice(t, []string{"SELECT a < 2;\nSELECT 3;", "CREATE TABLE t(x & y)"}, preBlocks(page))
	})
}

func TestStatements(t *testing.T) {
	t.Run("should split at semicolons that end a statement, trimmed and without the semicolon", func(t *testing.T) {
		is.EqualSlice(t, []string{"SELECT 1", "SELECT ';'", "SELECT 2 /* ; */"}, statements(" SELECT 1;\n SELECT ';' ; SELECT 2 /* ; */ "))
	})

	t.Run("should keep a trigger's body with the trigger", func(t *testing.T) {
		is.EqualSlice(t, []string{"CREATE TRIGGER r AFTER INSERT ON t BEGIN SELECT 1; SELECT 2; END", "SELECT 3"},
			statements("CREATE TRIGGER r AFTER INSERT ON t BEGIN SELECT 1; SELECT 2; END; SELECT 3;"))
	})

	t.Run("should leave out empty statements", func(t *testing.T) {
		is.Equal(t, 0, len(statements(" ; ;\n\t;")))
	})

	t.Run("should split the text after a NUL byte on its own, and keep the statement with the NUL byte", func(t *testing.T) {
		is.EqualSlice(t, []string{"SELECT 1", "SELECT 2", "SELECT 2\x00 SELECT", "SELECT 3", "SELECT 4"},
			statements("SELECT 1; SELECT 2\x00 SELECT 3; SELECT 4"))
	})

	t.Run("should not keep a NUL byte after a complete statement", func(t *testing.T) {
		is.EqualSlice(t, []string{"SELECT 1", "SELECT 2"}, statements("SELECT 1;\x00SELECT 2"))
	})

	t.Run("should keep a vertical tab at the start, which SQLite does not skip there", func(t *testing.T) {
		is.EqualSlice(t, []string{"\vSELECT 1"}, statements(" \vSELECT 1"))
	})
}

func TestStartsStatement(t *testing.T) {
	t.Run("should accept a keyword that starts a statement, after comments", func(t *testing.T) {
		for _, sql := range []string{"SELECT 1", "-- c\nwith x as (select 1) select 1", "PRAGMA x", "explain select 1"} {
			is.True(t, startsStatement(sql), sql)
		}
	})

	t.Run("should refuse other text", func(t *testing.T) {
		for _, sql := range []string{"int main(void)", "$ sqlite3 x.db", "", "-- only a comment", "x = 1"} {
			is.True(t, !startsStatement(sql), sql)
		}
	})
}

func TestCheckArchive(t *testing.T) {
	data := []byte("archive")
	a := archive{URL: "https://sqlite.org/2026/x.zip", Size: len(data), SHA3256: sha3Hex(data), SHA256: sha256Hex(data)}

	t.Run("should accept an archive with the recorded size and checksums", func(t *testing.T) {
		is.NotError(t, checkArchive(data, a))
	})

	t.Run("should refuse any difference", func(t *testing.T) {
		for _, b := range []archive{
			{URL: "https://example.com/x.zip", Size: a.Size, SHA3256: a.SHA3256, SHA256: a.SHA256},
			{URL: a.URL, Size: a.Size + 1, SHA3256: a.SHA3256, SHA256: a.SHA256},
			{URL: a.URL, Size: a.Size, SHA3256: sha3Hex([]byte("other")), SHA256: a.SHA256},
			{URL: a.URL, Size: a.Size, SHA3256: a.SHA3256, SHA256: sha256Hex([]byte("other"))},
		} {
			is.True(t, checkArchive(data, b) != nil, b.URL)
		}
	})
}

func TestFindProduct(t *testing.T) {
	page := []byte("PRODUCT,3.54.1,2026/sqlite-src-3540100.zip,14899528," + sha3Hex([]byte("a")) + "\n" +
		"PRODUCT,3.54.1,2026/sqlite-doc-3540100.zip,11961740," + sha3Hex([]byte("b")) + "\n")

	t.Run("should find a file of the version", func(t *testing.T) {
		p, err := findProduct(page, "3.54.1", "sqlite-doc-3540100.zip")
		is.NotError(t, err)
		is.Equal(t, product{path: "2026/sqlite-doc-3540100.zip", size: 11961740, sha3: sha3Hex([]byte("b"))}, p)
	})

	t.Run("should fail for a file that is not on the page", func(t *testing.T) {
		_, err := findProduct(page, "3.54.2", "sqlite-doc-3540200.zip")
		is.True(t, err != nil)
	})
}

func TestVersionNumber(t *testing.T) {
	n, err := versionNumber("3.54.1")
	is.NotError(t, err)
	is.Equal(t, 3540100, n)

	for _, v := range []string{"3.54", "3.54.x", "3.100.1"} {
		_, err := versionNumber(v)
		is.True(t, err != nil, v)
	}
}
