package engine_test

import (
	"fmt"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
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

// runUpserts runs the statements of setup and then the steps on the instance i and on SQLite, o, each
// through Query where it has RETURNING and through Exec otherwise, and requires of each step the
// answer of SQLite, with the offset of its error, SQLite's count of changes, and SQLite's state. A
// step "begin read only" begins a read-only transaction on both, and "rollback" ends it. It gives the
// answer of each step as the test writes it: its error, or its rows in the order of the key, or ""
// for a statement without rows, and the count of changes after it.
func runUpserts(t *testing.T, i *instance, o *oracle, setup []string, steps []upsertStep) []string {
	t.Helper()
	for _, s := range setup {
		is.Equal(t, "", i.exec(s).err, s)
		is.Equal(t, "", o.run(t, s).err, s)
	}
	var got []string
	for _, step := range steps {
		switch step.src {
		case "begin read only", "rollback":
			readOnly := step.src != "rollback"
			if readOnly {
				is.NotError(t, i.c.BeginReadOnly(t.Context()))
			} else {
				is.NotError(t, i.c.Rollback(t.Context()))
			}
			for _, s := range map[bool][]string{true: oracleBegin(true), false: oracleEnd(true, false)}[readOnly] {
				is.Equal(t, "", o.run(t, s).err, s)
			}
			continue
		}
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
			got := runUpserts(t, i, newOracle(t), upsertSetUp, steps)
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

	t.Run("should give SQLite's errors of the names in SQLite's order, at SQLite's offsets, whether the key exists or not", func(t *testing.T) {
		var steps []upsertStep
		for _, key := range []string{"1", "9"} {
			for _, clause := range []string{
				// The target: each column, and then whether it is the key.
				"on conflict (z) do nothing",
				`on conflict ("z") do nothing`,
				"on conflict (v) do nothing",
				"on conflict (k, v) do nothing",
				"on conflict (v, z) do nothing",
				"on conflict (z, y) do nothing",
				"on conflict (z) do update set zz = yy",
				"on conflict (v) do update set zz = yy",
				// SET in the order of the text, each value before its column.
				"on conflict (k) do update set zz = yy",
				"on conflict (k) do update set zz = 1, v = yy",
				"on conflict (k) do update set v = 1, zz = 1",
				`on conflict (k) do update set "zz" = 1`,
				"on conflict (k) do update set v = excluded.zz",
				"on conflict (k) do update set v = EXCLUDED.zz",
				`on conflict (k) do update set v = "excluded".zz`,
				`on conflict (k) do update set v = excluded."ZZ"`,
				"on conflict (k) do update set v = excluded.rowid",
				"on conflict (k) do update set v = rowid",
				`on conflict (k) do update set v = "yy"`,
				`on conflict (k) do update set v = 1 + "yy" * excluded.zz`,
				"on conflict (k) do update set v = excluded.v + yy, zz = 1",
				"on conflict (k) do update set v = yy where zz = 1 returning xx",
				// WHERE, after SET, and before RETURNING.
				"on conflict (k) do update set v = 1 where zz = 1",
				"on conflict (k) do update set v = 1 where excluded.zz = 1",
				"on conflict (k) do update set v = 1 where 1 = excluded.zz",
				"on conflict (k) do update set v = 1 where v = excluded.zz + yy",
				"on conflict (k) do update set v = 1 where excluded.v + yy = 1 and zz = 1",
				"on conflict (k) do update set v = 1 where 1 + yy < excluded.zz",
				"on conflict (k) do update set v = 1 where zz is null and yy = 1",
				"on conflict (k) do update set v = 1 where zz is null",
				"on conflict (k) do update set v = 1 where v = 1 and zz is null and yy is null",
				"on conflict (k) do update set v = 1 where excluded.zz is null and yy = 1",
				"on conflict (k) do update set v = 1 where excluded.zz is null",
				"on conflict (k) do update set v = 1 where zz is null and excluded.yy = 1",
				"on conflict (k) do update set v = 1 where zz is null and excluded.yy is not null",
				"on conflict (k) do update set v = 1 where excluded.zz is null and yy is null",
				"on conflict (k) do update set v = 1 where zz is null and excluded.v = yy",
				"on conflict (k) do update set v = 1 where zz is null returning yy",
				"on conflict (k) do update set v = 1 where v = 1 returning zz",
				"on conflict (k) do nothing returning zz",
			} {
				steps = append(steps, upsertStep{fmt.Sprint("insert into t (k, n) values (", key, ", 1) ", clause), nil})
			}
		}
		steps = append(steps,
			// The table, and the list and the number of the values, before the clause.
			upsertStep{"insert into nosuch values (1) on conflict (z) do nothing", nil},
			upsertStep{"insert into t (k, zz) values (1, 2) on conflict (z) do update set yy = 1", nil},
			upsertStep{"insert into t values (1, 2) on conflict (z) do nothing", nil},
			upsertStep{"insert into t (k, n) values (1) on conflict (z) do nothing", nil},
		)
		got := runUpserts(t, newInstance(t, forward()), newOracle(t), upsertSetUp, steps)
		var want []string
		for range 2 {
			want = append(want,
				"1/1: no such column: z; 1",
				`1/1: no such column: "z" - should this be a string literal in single-quotes?; 1`,
				"1/1: ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint; 1",
				"1/1: ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint; 1",
				"1/1: no such column: z; 1",
				"1/1: no such column: z; 1",
				"1/1: no such column: z; 1",
				"1/1: ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: EXCLUDED.zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: excluded.ZZ; 1",
				"1/1: no such column: excluded.rowid; 1",
				"1/1: no such column: rowid; 1",
				`1/1: no such column: "yy" - should this be a string literal in single-quotes?; 1`,
				`1/1: no such column: "yy" - should this be a string literal in single-quotes?; 1`,
				"1/1: no such column: yy; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: excluded.zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: excluded.yy; 1",
				"1/1: no such column: yy; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
				"1/1: no such column: zz; 1",
			)
		}
		want = append(want,
			"1/1: no such table: nosuch; 1",
			"1/1: table t has no column named zz; 1",
			"1/1: table t has 7 columns but 2 values were supplied; 1",
			"1/1: 1 values for 2 columns; 1",
		)
		is.EqualSlice(t, want, got)
	})

	t.Run("should fail as SQLite does for NULL where the table takes none, and in a read-only transaction", func(t *testing.T) {
		steps := []upsertStep{
			// NOT NULL of the row that the insert proposes, also where DO NOTHING would change nothing.
			{"insert into t (k) values (1) on conflict (k) do nothing", nil},
			{"insert into t (k, n) values (1, null) on conflict (k) do update set v = 1", nil},
			{"insert into t (k, n) values (null, 1) on conflict (k) do nothing", nil},
			{"insert into t (k, n) values (?, 1) on conflict (k) do nothing", []any{nil}},
			// NOT NULL of the row that DO UPDATE changes, only where it changes one.
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = null", nil},
			{"insert into t (k, n) values (9, 1) on conflict (k) do update set n = null", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = null where v = 0", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = excluded.v", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = excluded.v + 1, v = 3", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = ?", []any{nil}},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set n = d + n", nil},
			{"insert into t (k, n) values (3, 1) on conflict (k) do update set n = d + n", nil},
			{"select * from t", nil},
			// A read-only transaction refuses the write once the names are found, also where it would
			// change nothing.
			{"begin read only", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do nothing", nil},
			{"insert into t (k, n) values (9, 1) on conflict (k) do nothing", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 1 where v = 0", nil},
			{"insert into t (k) values (1) on conflict (k) do update set v = excluded.s", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 9223372036854775807 + 1", nil},
			{"insert into t (k, n) values (1, 1) on conflict (zz) do nothing", nil},
			{"insert into t (k, n) values (1, 1) on conflict (k) do update set v = 1 returning zz", nil},
			{"rollback", nil},
		}
		got := runUpserts(t, newInstance(t, forward()), newOracle(t), upsertSetUp, steps)
		is.EqualSlice(t, []string{
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"19/1299: NOT NULL constraint failed: t.k; 0",
			"19/1299: NOT NULL constraint failed: t.k; 0",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"; 1",
			"; 0",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"; 1",
			"19/1299: NOT NULL constraint failed: t.n; 0",
			"; 0",
			"8/8: attempt to write a readonly database; 0",
			"8/8: attempt to write a readonly database; 0",
			"8/8: attempt to write a readonly database; 0",
			"8/8: attempt to write a readonly database; 0",
			"8/8: attempt to write a readonly database; 0",
			"1/1: no such column: zz; 0",
			"1/1: no such column: zz; 0",
		}, got)
	})

	t.Run("should fail as SQLite does for a value and a record longer than the length limit, the record of the proposed row also where the key exists", func(t *testing.T) {
		const limit = 1000
		long, half := strings.Repeat("x", limit+1), strings.Repeat("y", 520)
		steps := []upsertStep{
			{"insert into u values (1, ?, ?) on conflict (k) do nothing", []any{half, half}},
			{"insert into u values (1, ?, ?) on conflict (k) do update set a = 'c' where a = 'z'", []any{half, half}},
			{"insert into u values (1, ?, 'b') on conflict (k) do update set b = excluded.a, a = excluded.a", []any{half}},
			{"insert into u values (2, ?, ?) on conflict (k) do nothing", []any{half, half}},
			{"insert into u values (1, ?, 'b') on conflict (k) do nothing", []any{long}},
			{"insert into u values (1, 'a', 'b') on conflict (k) do update set a = ?", []any{long}},
			{"insert into u values (9, 'a', 'b') on conflict (k) do update set a = ?", []any{long}},
			{"insert into u values (1, 'a', 'b') on conflict (k) do update set a = 'c' where b = ?", []any{long}},
			{"insert into u values (1, ?, 'b') on conflict (k) do update set a = 'c' returning *", []any{half}},
		}
		got := runUpserts(t, newInstanceLimited(t, forward(), nil, limit), newOracleLimited(t, limit),
			[]string{"create table u (k int primary key, a text, b text) strict, without rowid", "insert into u values (1, 'x', 'y')"}, steps)
		is.EqualSlice(t, []string{
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			"18/18: string or blob too big; 0",
			`1,"c","y"; 1`,
		}, got)
	})

	t.Run("should refuse by the rule of exact types, for excluded.c as for a column of its type, and give the evaluation errors of arithmetic, whether the key exists or not, and change nothing", func(t *testing.T) {
		i := newInstance(t, forward())
		for _, src := range upsertSetUp {
			is.Equal(t, "", i.exec(src).err, src)
		}
		before := i.state()
		for _, key := range []string{"1", "9"} {
			for _, c := range []struct {
				clause string
				args   []any
				want   string
			}{
				// The proposed row, as an insert.
				{"(k, n, v) values (#, 1, 'x') on conflict (k) do nothing", nil, "s4ql: not supported: a string literal for the column t.v of type INT"},
				{"(k, n, w) values (#, 1, 1) on conflict (k) do update set v = 1", nil, "s4ql: not supported: an integer literal for the column t.w of type REAL"},
				{"(k, n, s) values (#, 1, ?) on conflict (k) do nothing", []any{[]byte("x")}, "s4ql: not supported: argument 1, which is a blob, for the column t.s of type TEXT"},
				// SET, with excluded.c alone and in arithmetic.
				{"(k, n) values (#, 1) on conflict (k) do update set v = excluded.s", nil, "s4ql: not supported: the column excluded.s of type TEXT for the column t.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = excluded.w", nil, "s4ql: not supported: the column excluded.w of type REAL for the column t.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set b = excluded.s", nil, "s4ql: not supported: the column excluded.s of type TEXT for the column t.b of type BLOB"},
				{"(k, n) values (#, 1) on conflict (k) do update set w = excluded.v + 1", nil, "s4ql: not supported: an expression of type integer for the column t.w of type REAL"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = excluded.s + 1", nil, "s4ql: not supported: column of type TEXT in arithmetic (at offset 69)"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1.5", nil, "s4ql: not supported: a real literal for the column t.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = ?", []any{"x"}, "s4ql: not supported: argument 1, which is text, for the column t.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = excluded.v * ?", []any{"x"}, "s4ql: not supported: argument 1 in arithmetic, which is text (at offset 82)"},
				// WHERE, with excluded.c on either side.
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where excluded.s = 1", nil, "s4ql: not supported: an integer literal compared with the column excluded.s of type TEXT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where v = excluded.s", nil, "s4ql: not supported: the column excluded.s of type TEXT compared with the column t.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where excluded.v < w + 1", nil, "s4ql: not supported: an expression of type real compared with the column excluded.v of type INT"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where excluded.v + 1 = 1.5", nil, "s4ql: not supported: a real literal compared with an expression of type integer"},
				// SET before WHERE, and DO UPDATE before the proposed row, which an insert checks when it runs.
				{"(k, n, v) values (#, 1, 'x') on conflict (k) do update set v = 1.5 where s = 1", nil, "s4ql: not supported: a real literal for the column t.v of type INT"},
				{"(k, n, v) values (#, 1, 'x') on conflict (k) do update set v = 1 where s = 1", nil, "s4ql: not supported: an integer literal compared with the column t.s of type TEXT"},
				{"(k) values (#) on conflict (k) do update set v = excluded.s", nil, "s4ql: not supported: the column excluded.s of type TEXT for the column t.v of type INT"},
				// Evaluation errors of an expression without a column, which is computed once, also where no
				// row changes.
				{"(k, n) values (#, 1) on conflict (k) do update set v = 9223372036854775807 + 1", nil, "s4ql: integer overflow in + (at offset 69)"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where v = 9223372036854775807 * 2", nil, "s4ql: integer overflow in * (at offset 81)"},
				// Text, which the engine does not compute yet, before the database is read.
				{"(k, n) values (#, 1) on conflict (zz) do update set s = lower(excluded.s) || s", nil, "s4ql: not supported: concatenation (at offset 70)"},
				{"(k, n) values (#, 1) on conflict (k) do update set v = 1 where excluded.s || 'a' = 'b'", nil, "s4ql: not supported: concatenation (at offset 77)"},
			} {
				src := "insert into t " + strings.Replace(c.clause, "#", key, 1)
				is.Equal(t, c.want, i.execValues(src, values(c.args)).message, src)
				if strings.Contains(c.want, "call of") || strings.Contains(c.want, "concatenation") {
					is.Equal(t, c.want, fmt.Sprint(engine.Check(src, len(c.args))), src)
				}
			}
		}
		// The evaluation errors of an expression with a column, only where the row changes.
		for _, c := range []struct{ src, want string }{
			{"insert into t (k, n, v) values (1, 1, 9223372036854775807) on conflict (k) do update set v = excluded.v + v", "s4ql: integer overflow in + (at offset 93)"},
			{"insert into t (k, n, v) values (1, 1, 0) on conflict (k) do update set v = 1 where v / excluded.v = 1", "s4ql: division by zero in / (at offset 83)"},
			{"insert into t (k, n, v) values (1, 1, 0) on conflict (k) do update set v = 1 where v = 11 and v / excluded.v = 1", ""},
			{"insert into t (k, n, v) values (8, 1, 0) on conflict (k) do update set v = 1 / excluded.v", ""},
		} {
			is.Equal(t, c.want, i.exec(c.src).message, c.src)
		}
		is.Equal(t, "", i.exec("delete from t where k = 8 and v = 0").err)
		is.Equal(t, 1, i.changes)
		is.Equal(t, before, i.state())
	})

	t.Run("should plan an upsert with its clause, and print it", func(t *testing.T) {
		for _, c := range []struct{ src, want string }{
			{"insert into t (k, v) values (1, ?) on conflict (k) do nothing", "insert into t (k, v) on conflict (k) do nothing\n  values (1, ?1)\n"},
			{"insert into t values (1, 2) on conflict (K) do update set v = excluded.v + v, w = 1 where Excluded.v is null and v < excluded.\"V\" * 2 returning *",
				"insert into t on conflict (K) do update set v = (excluded.v + v), w = 1 where Excluded.v IS NULL and v < (excluded.\"V\" * 2) returning *\n  values (1, 2)\n"},
		} {
			s, err := stmt.Check(c.src)
			is.NotError(t, err)
			is.Equal(t, c.want, engine.Format(engine.NewPlan(s)), c.src)
		}
	})
}
