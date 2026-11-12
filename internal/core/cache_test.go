package core_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/core"
	"maragu.dev/s4ql/internal/format"
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

func TestTx_CreateTable(t *testing.T) {
	t.Run("should keep tables and columns with hostile names apart, in the cache and in a replay of the log", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})

		// Every name is the name of a table, and every table has all the names as its columns, with
		// the name of the table as the primary key, so that mixing two of them up would show.
		var want []string
		for i, name := range hostile {
			columns := append([]string{name}, hostile[:i]...)
			columns = append(columns, hostile[i+1:]...)
			ct := tableOf(name, 1, columns...)
			values := make([]any, len(columns))
			for j := range values {
				values[j] = 1000*i + j
			}
			// A table at a time, so that a failure names it.
			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.NotError(t, tx.CreateTable(t.Context(), ct), name)
			is.NotError(t, tx.Upsert(t.Context(), name, row(values...)), name)
			got, err := tx.Get(t.Context(), name, row(1000*i))
			is.NotError(t, err, name)
			is.Equal(t, text(row(values...)), text(got), name)
			is.NotError(t, tx.Commit(t.Context()), name)
			want = append(want, fmt.Sprint(describe(ct), ": ", text(row(values...))))
		}
		got := strings.Split(read(t, d), "\n")
		// Some names hold line breaks, so the comparison is of the whole text.
		is.Equal(t, len(strings.Split(strings.Join(want, "\n"), "\n")), len(got))
		for _, table := range want {
			is.True(t, strings.Contains(read(t, d), table), table)
		}
		is.NotError(t, d.Close(t.Context()))

		// The cache as a person sees it with sqlite3: the tables have the names of the database.
		_, state, err := cacheDump(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		is.Equal(t, len(hostile), len(state))

		replayed := w.serve(t, "b", core.Options{})
		for _, table := range want {
			is.True(t, strings.Contains(read(t, replayed), table), table)
		}

		// And they can be changed and dropped by their names.
		for i, name := range hostile {
			tx, err := replayed.Begin(t.Context())
			is.NotError(t, err)
			found, err := tx.Delete(t.Context(), name, row(1000*i))
			is.NotError(t, err, name)
			is.True(t, found, name)
			is.NotError(t, tx.DropTable(t.Context(), name), name)
			is.NotError(t, tx.Commit(t.Context()), name)
		}
		is.Equal(t, "", read(t, replayed))
	})

	t.Run("should refuse a table whose name starts with s4ql_ or sqlite_, in any case of the letters, and allow names that only look like it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		tx, err := d.Begin(t.Context())
		is.NotError(t, err)

		for _, name := range []string{"s4ql_", "s4ql_cache", "s4ql_touched_1", "S4QL_x", "s4Ql_x", "S4qL_", "sqlite_", "sqlite_schema", "sqlite_master", "SQLITE_x", "SqLiTe_stat1", "sqlite_autoindex_t1_1"} {
			err := tx.CreateTable(t.Context(), tableOf(name, 1, "k"))
			is.Error(t, core.ErrReservedName, err, name)
			is.True(t, !errors.Is(err, core.ErrInvalid), name)
		}
		for _, name := range []string{"s4ql", "s4qlx", "_s4ql_", " s4ql_", "sqlite", "sqlitex", "xsqlite_", "s4ql-", "ſ4ql_x"} {
			is.NotError(t, tx.CreateTable(t.Context(), tableOf(name, 1, "k")), name)
		}
		// A column may have such a name.
		is.NotError(t, tx.CreateTable(t.Context(), tableOf("t1", 1, "s4ql_cache", "sqlite_schema")))
		is.NotError(t, tx.Commit(t.Context()))
	})
}

func TestOpen_cache(t *testing.T) {
	t.Run("should record the name of the last commit object as the position of the cache, where a person can read it", func(t *testing.T) {
		w := newWorld(t)
		d := w.serve(t, "a", core.Options{})
		path := filepath.Join(w.dir, "a.db")

		position, _, err := cacheDump(path)
		is.NotError(t, err)
		is.Equal(t, format.EmptyLog, position)

		for i := range 3 {
			transact(t, d, create(fmt.Sprint("t", i), 1, "k"))
			position, _, err := cacheDump(path)
			is.NotError(t, err)
			is.Equal(t, w.manifest(t).Head, position)

			tx, err := d.Begin(t.Context())
			is.NotError(t, err)
			is.Equal(t, position, tx.Position())
			is.NotError(t, tx.Rollback(t.Context()))
			r, err := d.Read(t.Context())
			is.NotError(t, err)
			is.Equal(t, position, r.Position())
			is.NotError(t, r.Close(t.Context()))
		}
	})

	t.Run("should fail for a file that is not a database, and leave it as it is", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		is.NotError(t, os.WriteFile(path, []byte(strings.Repeat("this is not a database\n", 100)), 0o600))

		_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
		is.Error(t, core.ErrLocal, err)
		data, err := os.ReadFile(path)
		is.NotError(t, err)
		is.True(t, strings.HasPrefix(string(data), "this is not a database"), "the file was rebuilt")
	})

	t.Run("should fail for a cache in a directory that does not exist", func(t *testing.T) {
		w := newWorld(t)
		_, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, "missing", "a.db"), "db", core.Options{})
		is.Error(t, core.ErrLocal, err)
	})

	t.Run("should fail for a cache of another version, and for one whose position is no name, and leave it as it is", func(t *testing.T) {
		for _, tamper := range []string{
			fmt.Sprint("update s4ql_cache set version = ", core.CacheVersion+1),
			"update s4ql_cache set head = 'not a name'",
			"delete from s4ql_cache",
		} {
			w := newWorld(t)
			d := w.serve(t, "a", core.Options{})
			transact(t, d, create("t1", 1, "k"), upsert("t1", 1))
			is.NotError(t, d.Close(t.Context()))

			path := filepath.Join(w.dir, "a.db")
			c, err := sqlite.Open(path)
			is.NotError(t, err)
			is.NotError(t, c.Exec(tamper))
			is.NotError(t, c.Close())

			_, err = core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.Error(t, core.ErrCacheVersion, err, tamper)

			// Nothing rebuilt it: the table and its row are still there.
			c, err = sqlite.Open(path)
			is.NotError(t, err)
			is.NotError(t, c.Exec("insert into t1 (k) values (2)"))
			is.True(t, c.Exec("insert into t1 (k) values (1)") != nil, "the row is gone")
			is.NotError(t, c.Close())
		}
	})

	t.Run("should refuse options, prefixes, and paths that make no sense", func(t *testing.T) {
		w := newWorld(t)
		path := filepath.Join(w.dir, "a.db")
		for name, options := range map[string]core.Options{
			"a renewal after the lease is over": {Lease: 10 * time.Second, RenewAfter: 10 * time.Second},
			"a lease below zero":                {Lease: -time.Second},
			"readers below zero":                {Readers: -1},
			"a timeout below zero":              {StoreTimeout: -time.Second},
		} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", options)
			is.True(t, err != nil, name)
		}
		for _, prefix := range []string{"/db", "db/", "a//b", ".", "a/../b", "a/./b", "\xff", strings.Repeat("x", 901)} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, prefix, core.Options{})
			is.True(t, err != nil, prefix)
		}
		for _, path := range []string{"", ":memory:", "file:a.db"} {
			_, err := core.Open(t.Context(), w.host, w.client(), path, "db", core.Options{})
			is.True(t, err != nil, path)
		}
		_, err := os.Stat(path)
		is.True(t, os.IsNotExist(err), "a cache was made")
	})

	t.Run("should keep the objects of a database under its prefix, and those of a database without a prefix at the top", func(t *testing.T) {
		w := newWorld(t)
		for _, prefix := range []string{"", "one", "one/two", "smørrebrød/🥪"} {
			d, err := core.Open(t.Context(), w.host, w.client(), filepath.Join(w.dir, fmt.Sprint(len(prefix), ".db")), prefix, core.Options{})
			is.NotError(t, err)
			is.NotError(t, d.Wait(t.Context()))
			transact(t, d, create("t1", 1, "k"))
			is.NotError(t, d.Close(t.Context()))
		}
		var manifests, commits []string
		for _, key := range w.store.Keys() {
			if name, ok := strings.CutSuffix(key, "manifest.json"); ok {
				manifests = append(manifests, name)
				continue
			}
			i := strings.LastIndex(key, "commits/")
			is.True(t, i >= 0 && len(key[i+len("commits/"):]) == 64, key)
			commits = append(commits, key[:i])
		}
		is.EqualSlice(t, []string{"", "one/", "one/two/", "smørrebrød/🥪/"}, manifests)
		is.EqualSlice(t, []string{"", "one/", "one/two/", "smørrebrød/🥪/"}, commits)
	})
}

func TestClassify(t *testing.T) {
	t.Run("should count as a local I/O failure the result codes of SQLite that say so, with any extended code, and busy as busy", func(t *testing.T) {
		kinds := map[int]string{
			3: "local", 7: "local", 8: "local", 10: "local", 11: "local", 13: "local", 14: "local", 15: "local", 22: "local", 26: "local",
			5: "busy",
		}
		for primary := range 29 {
			want, ok := kinds[primary]
			if !ok {
				want = "other"
			}
			for _, extended := range []int{0, 1, 2, 5, 9} {
				code := primary | extended<<8
				err := fmt.Errorf("wrapped: %w", &sqlite.Error{Code: sqlite.ResultCode(code), Message: "test"})
				is.Equal(t, want, core.Classify(err), fmt.Sprint(code))
			}
		}
		is.Equal(t, "other", core.Classify(errors.New("no error of SQLite")))
		is.Equal(t, "other", core.Classify(nil))
	})
}

func TestDB_Begin(t *testing.T) {
	t.Run("should return and log busy, and go on, when another connection holds the cache's write lock past the busy timeout", func(t *testing.T) {
		w := newWorld(t)
		events := &recorder{}
		d := w.serve(t, "a", core.Options{BusyTimeout: 50 * time.Millisecond, Logger: events.logger()})
		transact(t, d, create("t1", 1, "k"), upsert("t1", 1))

		// Another process has the cache open and holds its write lock.
		other, err := sqlite.Open(filepath.Join(w.dir, "a.db"))
		is.NotError(t, err)
		defer func() { is.NotError(t, other.Close()) }()
		is.NotError(t, other.Exec("begin immediate"))

		_, err = d.Begin(t.Context())
		is.Error(t, core.ErrBusy, err)
		is.True(t, !errors.Is(err, core.ErrLocal) && !errors.Is(err, core.ErrStopped))
		is.Equal(t, 1, len(events.named(core.EventBusy)))
		is.Equal(t, fmt.Sprint("busy"), at(events.named(core.EventBusy)[0], core.AttrOutcome))
		is.Equal(t, fmt.Sprint("begin"), at(events.named(core.EventBusy)[0], core.AttrOperation))

		// It is no local I/O failure: the instance goes on, and reads do not wait for the lock.
		is.NotError(t, d.Err())
		is.Equal(t, "t1(k*0): 1", read(t, d))
		is.NotError(t, other.Exec("rollback"))
		transact(t, d, upsert("t1", 2))
		is.Equal(t, "t1(k*0): 1 | 2", read(t, d))
	})
}
