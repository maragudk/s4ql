package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	_ "maragu.dev/s4ql/internal/engine"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// upsertSetUp is the database of the tests of upserts: a table of a column of each type, one that
// does not take NULL, and one with a default, with an index, and its rows.
var upsertSetUp = []string{
	"create table t (k int primary key, v int, w real, s text, b blob, n int not null, d int default 7) strict, without rowid",
	"create index t_v on t (v)",
	"insert into t values (1, 10, 1.5, 'a', x'01', 1, 2)",
	"insert into t values (2, 20, 2.5, 'b', x'02', 2, 3)",
	"insert into t values (3, null, null, null, null, 3, null)",
}

// upsertStep is a statement of a test of upserts, with its arguments.
type upsertStep struct {
	src  string
	args []any
}

// runUpserts runs the statements of setup and then the steps on the instance i and on SQLite, each
// through Query where it has RETURNING and through Exec otherwise, and requires of each step the
// answer of SQLite, with the offset of its error, SQLite's count of changes, and SQLite's state. It
// gives the answer of each step as the test writes it: its error, or its rows in the order of the
// key, or "" for a statement without rows, and the count of changes after it.
func runUpserts(t *testing.T, i *instance, setup []string, steps []upsertStep) []string {
	t.Helper()
	o := newOracle(t)
	for _, s := range setup {
		is.Equal(t, "", i.exec(s).err, s)
		is.Equal(t, "", o.run(t, s).err, s)
	}
	var got []string
	for _, step := range steps {
		s, _ := stmt.Check(step.src)
		query := false
		if insert, ok := s.(*stmt.Insert); ok {
			query = insert.Returning.Present()
		}
		var a answer
		if query {
			a = i.queryValues(step.src, values(step.args))
		} else {
			a = i.execValues(step.src, values(step.args))
		}
		want := o.run(t, step.src, oracleArgs(step.args)...)
		if !query {
			want.columns, want.rows = "", nil
		}
		is.Equal(t, want.String(), a.String(), step.src)
		is.Equal(t, want.offset, a.offset, "the offset of SQLite's error and of the engine's: "+step.src)
		changes, _ := o.changes(t)
		is.Equal(t, changes, i.changes, "the count of changes: "+step.src)
		is.Equal(t, o.state(t), i.state(), step.src)
		text := a.err
		if a.err == "" {
			text = strings.Join(a.rows, " ")
		}
		got = append(got, fmt.Sprint(text, "; ", i.changes))
	}
	return got
}

func TestEngine_upsert(t *testing.T) {
	t.Run("should insert, do nothing, or update as SQLite does, with its rows of RETURNING and its count of changes, on the core and in memory, and replay", func(t *testing.T) {
		steps := []upsertStep{
			// No conflict: the insert, with the default and NULL of the columns that the list leaves out.
			{"insert into t (k, n) values (4, 4) on conflict (k) do nothing returning *", nil},
			{"insert into t (k, n) values (5, 5) on conflict (k) do update set v = 1 returning *", nil},
			{"insert into t values (6, ?, ?, ?, ?, ?, ?) on conflict (k) do update set v = excluded.v", []any{int64(60), 6.5, "f", []byte{6}, int64(6), nil}},
			// DO NOTHING on a conflict changes nothing, and gives no row.
			{"insert into t (k, n) values (1, 9) on conflict (k) do nothing returning *", nil},
			{"insert into t (k, n) values (1, 9) on conflict (k) do nothing", nil},
			// DO UPDATE on a conflict changes the row of the key, and gives it after the change.
			{"insert into t (k, n) values (1, 9) on conflict (k) do update set n = excluded.n returning *", nil},
			{"insert into t values (2, 21, 3.5, 'c', x'03', 22, 23) on conflict (k) do update set v = excluded.v, w = excluded.w, s = excluded.s, b = excluded.b, d = excluded.d returning k, s, d", nil},
			// A column that the list leaves out is its default in excluded, or NULL.
			{"insert into t (k, n) values (2, 0) on conflict (k) do update set d = excluded.d, v = excluded.v returning v, d", nil},
			// The columns of the row that the table holds, before the change.
			{"insert into t (k, n) values (1, 100) on conflict (k) do update set n = n + excluded.n, v = n returning n, v", nil},
			// WHERE that holds, and WHERE that does not, which changes nothing.
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 7 where n > excluded.n returning v", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 8 where n < excluded.n returning v", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 8 where excluded.v is null and v = 7 and excluded.n = 1 returning v", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 9 where excluded.v is not null", nil},
			{"insert into t (k, n) values (3, 1) on conflict (k) do update set v = 9 where v = excluded.v", nil},
			{"insert into t (k, n) values (3, 1) on conflict (k) do update set v = 9 where v is null and s is null", nil},
			// Arithmetic with excluded.c, on both sides of a condition.
			{"insert into t (k, v, w, n) values (2, 5, 0.5, 1) on conflict (k) do update set v = v * excluded.v + 1, w = excluded.w / 2 where excluded.v + 1 > 2 and 0.0 < excluded.w returning v, w", nil},
			{"insert into t (k, v, n) values (?, ?, 1) on conflict (k) do update set v = -excluded.v + ? where ? < excluded.v + v", []any{int64(1), int64(3), int64(1000), int64(0)}},
			// Text and blobs, copied and compared.
			{"insert into t (k, s, b, n) values (1, 'z', x'ff', 1) on conflict (k) do update set s = excluded.s, b = excluded.b where s < excluded.s and excluded.b > b returning s, b", nil},
			// Names in another case and in double quotes.
			{`insert into T (K, "N") values (1, 3) on conflict ("k") do update set "V" = EXCLUDED."N" + Excluded.n returning v`, nil},
			// The key of the target, also where the list of columns names it last.
			{"insert into t (n, k) values (5, 3) on conflict (k) do update set n = excluded.n returning *", nil},
		}
		for _, memory := range []bool{false, true} {
			i := newInstance(t, forward())
			if memory {
				i = newMemoryInstance(t)
			}
			got := runUpserts(t, i, upsertSetUp, steps)
			is.EqualSlice(t, []string{
				"4,null,null,null,null,4,7; 1",
				"5,null,null,null,null,5,7; 1",
				"; 1",
				"; 0",
				"; 0",
				`1,10,real 1.5,"a",x'01',9,2; 1`,
				`2,"c",23; 1`,
				"null,7; 1",
				"109,9; 1",
				"7; 1",
				"; 0",
				"8; 1",
				"; 0",
				"; 0",
				"; 1",
				"null,real 0.25; 1",
				"; 1",
				`"z",x'ff'; 1`,
				"6; 1",
				"3,9,null,null,null,5,null; 1",
			}, got)
			if !memory {
				want := i.state()
				i.restart(true, forward())
				is.Equal(t, want, i.state(), "after a replay into an empty cache")
			}
		}
	})
}
