package engine_test

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/engine"
)

// turned is the operator of a comparison, in each spelling, that compares with its sides swapped as
// the operator does: `5 < a` is `a > 5`.
var turned = map[string]string{"=": "=", "==": "==", "!=": "!=", "<>": "<>", "<": ">", "<=": ">=", ">": "<", ">=": "<="}

// valueFirst is the WHERE where, of comparisons `column op value` and tests for NULL joined by " and
// ", with the comparisons for which turn reports true, by their number among the comparisons from 0,
// written `value op' column`, as [turned] gives op'. Its parameters keep their order.
func valueFirst(where string, turn func(n int) bool) string {
	conditions := strings.Split(where, " and ")
	n := 0
	for i, c := range conditions {
		fields := strings.Fields(c)
		if len(fields) != 3 || turned[fields[1]] == "" {
			continue
		}
		if turn(n) {
			conditions[i] = fields[2] + " " + turned[fields[1]] + " " + fields[0]
		}
		n++
	}
	return strings.Join(conditions, " and ")
}

func TestValueFirst(t *testing.T) {
	t.Run("should write every comparison, or some, with its value first and its operator turned round", func(t *testing.T) {
		every := func(int) bool { return true }
		is.Equal(t, "5 > a", valueFirst("a < 5", every))
		is.Equal(t, "? <= k and b is null and x'00' != c", valueFirst("k >= ? and b is null and c != x'00'", every))
		is.Equal(t, "k = 1 and ? < k", valueFirst("k = 1 and k > ?", func(n int) bool { return n == 1 }))
	})
}

func TestEngine_valueFirst(t *testing.T) {
	t.Run("should compare a value on the left with each operator, with the edges of each type and with values beside them, as SQLite does and as the operator turned round does, on the key and on another column, by the key and with every row", func(t *testing.T) {
		ints := []edge{{"-2", int64(-2)}, {"2", int64(2)}, {"5", int64(5)}}
		beside := map[string][]edge{
			"int": ints, "integer": ints,
			"real": {{"0.5", 0.5}, {"-0.5", -0.5}, {"-1e300", -1e300}, {"2.0", 2.0}},
			"text": {{"'b'", "b"}, {"'aa'", "aa"}, {"'ä'", "ä"}, {"'~'", "~"}},
			"blob": {{"x'7f'", []byte{0x7f}}, {"x'ff00'", []byte{0xff, 0}}, {"x'000000'", []byte{0, 0, 0}}},
		}
		for _, typ := range []string{"int", "integer", "real", "text", "blob"} {
			t.Run(typ, func(t *testing.T) {
				w := newWhereTrio(t, "create table e (k "+typ+" primary key, v "+typ+") strict, without rowid")
				es := edges[typ]
				for n, e := range es {
					v := es[(n+1)%len(es)].literal
					if n%3 == 2 {
						v = "null"
					}
					w.exec(fmt.Sprintf("insert into e values (%v, %v)", e.literal, v))
				}
				some, none := 0, 0
				for _, column := range []string{"k", "V"} {
					for op, other := range turned {
						for _, p := range slices.Concat(es, beside[typ]) {
							a := w.query(fmt.Sprintf("select * from e where %v %v %v", p.literal, op, column))
							is.Equal(t, w.query(fmt.Sprintf("select * from e where %v %v %v", column, other, p.literal)).String(), a.String())
							b := w.query(fmt.Sprintf("select k from e where ? %v %v", op, column), p.arg)
							is.Equal(t, w.query(fmt.Sprintf("select k from e where %v %v ?", column, other), p.arg).String(), b.String())
							for _, a := range []answer{a, b} {
								if len(a.rows) > 0 && len(a.rows) < len(es)-1 {
									some++
								} else if len(a.rows) == 0 {
									none++
								}
							}
						}
						// NULL on the left is never true either, as a literal and as an argument.
						is.Equal(t, 0, len(w.query(fmt.Sprintf("select * from e where null %v %v", op, column)).rows))
						is.Equal(t, 0, len(w.query(fmt.Sprintf("select * from e where ? %v %v", op, column), nil).rows))
					}
				}
				is.True(t, some > 100 && none > 10, fmt.Sprint("some rows ", some, ", and none ", none))
			})
		}
	})

	t.Run("should give the rows of a table of every shape that conditions of both orders select, mixed in one WHERE, as SQLite does, by the key and with every row", func(t *testing.T) {
		for n, s := range shapes() {
			t.Run(s.String(), func(t *testing.T) {
				w := newWhereTrio(t, s.create("t"))
				for k := 10; k <= 40; k += 10 {
					var vs []string
					for p, typ := range s.types {
						v := literal(typ, k)
						if s.nullable[p] && k%20 == 0 {
							v = "null"
						}
						vs = append(vs, v)
					}
					w.exec("insert into t values (" + strings.Join(vs, ", ") + ")")
				}
				type condition struct {
					src  string
					args []any
				}
				var pool []condition
				for p, typ := range s.types {
					name := spelledNames[p]
					for op := range turned {
						for _, k := range []int{5, 20, 25, 50} {
							pool = append(pool, condition{src: fmt.Sprint(literal(typ, k), " ", op, " ", name)}, condition{src: fmt.Sprint(name, " ", op, " ", literal(typ, k))})
						}
						pool = append(pool, condition{src: "null " + op + " " + name}, condition{src: "? " + op + " " + name, args: []any{argument(typ, 30)}},
							condition{src: name + " " + op + " ?", args: []any{argument(typ, 20)}})
					}
					pool = append(pool, condition{src: name + " is null"}, condition{src: name + " is not null"})
				}
				rows := 0
				r := rand.New(rand.NewPCG(uint64(n), 12))
				for range 80 {
					var srcs []string
					var args []any
					for range 2 + r.IntN(2) {
						c := pool[r.IntN(len(pool))]
						srcs, args = append(srcs, c.src), append(args, c.args...)
					}
					rows += len(w.query("select * from t where "+strings.Join(srcs, " and "), args...).rows)
				}
				// The key twice, by bounds of both orders that contradict each other, that hold one row, and that fix it.
				key, typ := spelledNames[s.key], s.types[s.key]
				lit := func(k int) string { return literal(typ, k) }
				for _, c := range []condition{
					{src: lit(30) + " < " + key + " and " + key + " < " + lit(20)},
					{src: lit(20) + " <= " + key + " and " + lit(20) + " >= " + key},
					{src: lit(10) + " < " + key + " and " + key + " <= " + lit(30) + " and " + lit(20) + " != " + key},
					{src: lit(20) + " = " + key + " and " + lit(30) + " = " + key},
					{src: "? > " + key + " and ? <= " + key + " and " + key + " < ?", args: []any{argument(typ, 40), argument(typ, 20), argument(typ, 35)}},
					{src: key + " is null and " + lit(25) + " < " + key},
				} {
					rows += len(w.query("select * from t where "+c.src, c.args...).rows)
				}
				is.True(t, rows > 10, fmt.Sprint(rows, " rows"))
			})
		}
	})

	setup := []string{
		"create table t (k int primary key, a int not null, b text, c real) strict, without rowid",
		"insert into t values (1, 10, 'x', 1.5)", "insert into t values (2, 20, null, 2.5)", "insert into t values (3, 30, 'z', null)",
		"insert into t values (4, 40, 'y', 4.5)",
	}

	t.Run("should number the parameters in the order of the text, on either side of their columns, in a select, an update, and a delete", func(t *testing.T) {
		d := newDuo(t, 0, setup...)
		// Of other types in each place, so that a parameter numbered out of the order of the text is of
		// another type than its column.
		r := d.run("select k from t where a > ? and ? < b and c <= ?", int64(15), "w", 4.5)
		r.ok()
		is.Equal(t, `columns "k INT", rows ["4"]`, r.got.String())
		d.run("select k from t where ? = k", int64(2)).ok()
		d.run("select * from t where ? > c and a != ? and ? <= k", 3.0, int64(10), int64(1)).ok()
		d.run("update t set b = ?, c = ? where ? <= k and 'z' != b", "u", 9.5, int64(2)).ok()
		is.Equal(t, int64(1), d.i.changes)
		d.run("update t set a = 0 where ? > a", int64(25)).ok()
		is.Equal(t, int64(2), d.i.changes)
		d.run("delete from t where ? >= c and ? = b", 9.5, "u").ok()
		is.Equal(t, int64(1), d.i.changes)
		d.same()
	})

	t.Run("should change and delete the rows that SQLite does with each operator written with its value first, on the key and on another column, with the count of changes", func(t *testing.T) {
		for op := range turned {
			for _, where := range []string{"2 " + op + " k", "20 " + op + " a", "'y' " + op + " b", "2.5 " + op + " c", "null " + op + " a", "2 " + op + " k and k " + op + " 3"} {
				d := newDuo(t, 0, setup...)
				d.run("update t set b = 'u' where " + where).ok()
				d.run("delete from t where " + where).ok()
				d.same()
			}
		}
	})

	t.Run("should give SQLite's errors of the names for a column on the right, at its place in the text and with SQLite's offset", func(t *testing.T) {
		for _, c := range []struct {
			src, want string
		}{
			{"select * from t where 5 < nope", "1/1: no such column: nope"},
			{`select * from t where 5 < "nope"`, `1/1: no such column: "nope" - should this be a string literal in single-quotes?`},
			{"select * from t where 1 = a and 5 < nope and nope2 = 1", "1/1: no such column: nope"},
			{"select * from t where nope = 1 and 5 < nope2", "1/1: no such column: nope"},
			{"select * from t where nope is null and 1 = nope2", "1/1: no such column: nope"},
			{"select * from t where nope is null and nope2 is null and 1 = nope3", "1/1: no such column: nope2"},
			{"select nope from t where 1 = nope2", "1/1: no such column: nope"},
			{"select * from nope where 1 = a", "1/1: no such table: nope"},
			{"update t set a = 1 where 'x' < nope", "1/1: no such column: nope"},
			{"update t set nope = 1 where 'x' < nope2", "1/1: no such column: nope"},
			{"delete from t where - 5 >= nope", "1/1: no such column: nope"},
		} {
			t.Run(c.src, func(t *testing.T) {
				pin(t, setup, c.src, c.want)
			})
		}
	})

	t.Run("should give the names first, then string or blob too big, then the refusal of a value of another type, also for a value on the left that is too long", func(t *testing.T) {
		long := "'" + strings.Repeat("x", 1200) + "'"
		d := newDuo(t, 1000, setup...)
		for _, c := range []struct {
			src, want string
		}{
			{"select * from t where " + long + " < a and nope = 1", "1/1: no such column: nope"},
			{"select * from t where 'x' < a and 1 = nope", "1/1: no such column: nope"},
			{"select * from t where 'x' < a and " + long + " = b", "18/18: string or blob too big"},
			{"select * from t where " + long + " > b", "18/18: string or blob too big"},
		} {
			d.run(c.src).is(c.want)
		}
		// A literal that is too long in an update or a delete is the known difference of
		// docs/todo/slice-08.md, whichever side it is on, so these are arguments, which SQLite binds
		// before it runs the statement.
		longArg := strings.Repeat("y", 1001)
		d.run("select * from t where ? < b", longArg).is("18/18: string or blob too big")
		d.run("update t set a = 1 where 'x' < a and ? = b", longArg).is("18/18: string or blob too big")
		d.run("delete from t where 1 < a and ? <= b", longArg).is("18/18: string or blob too big")
		d.run("update t set a = 1 where ? = nope and ? < b", longArg, longArg).is("1/1: no such column: nope")
		d.same()
	})

	t.Run("should refuse the first value of another type than its column in the order of the text, naming the value and the column, whichever side the value is on", func(t *testing.T) {
		d := newDuo(t, 0, setup...)
		for _, c := range []struct {
			src  string
			args []any
			want string
		}{
			{src: "select * from t where '5' < a", want: "s4ql: not supported: a string literal compared with the column t.a of type INT"},
			{src: "select * from t where b = 'x' and 1.5 < a and 'y' = c", want: "s4ql: not supported: a real literal compared with the column t.a of type INT"},
			{src: "select * from t where a = 1 and ? >= b", args: []any{int64(2)}, want: "s4ql: not supported: argument 1, which is an integer, compared with the column t.b of type TEXT"},
			{src: "select * from t where ? = k and ? < b", args: []any{int64(1), 2.5}, want: "s4ql: not supported: argument 2, which is a real, compared with the column t.b of type TEXT"},
			{src: "select * from t where x'00' != c and k = 'k'", want: "s4ql: not supported: a blob literal compared with the column t.c of type REAL"},
			{src: "update t set b = 1 where 'x' < a", want: "s4ql: not supported: a string literal compared with the column t.a of type INT"},
			{src: "update t set b = ? where ? < a", args: []any{"v", "w"}, want: "s4ql: not supported: argument 2, which is text, compared with the column t.a of type INT"},
			{src: "delete from t where 1 > c", want: "s4ql: not supported: an integer literal compared with the column t.c of type REAL"},
		} {
			r := d.run(c.src, c.args...)
			r.is("not supported")
			is.Equal(t, c.want, r.got.message, c.src)
		}
		// NULL has no type, on the left too.
		d.run("select * from t where null = b and ? < a", nil).ok()
		d.same()
	})

	t.Run("should refuse the arguments of a value on the left by their number and by what they are, before the database", func(t *testing.T) {
		d := newDuo(t, 0, setup...)
		// SQLite binds NULL to a parameter without an argument, so only the engine runs this one.
		a := d.i.queryValues("select * from t where ? < a and b = ?", values([]any{int64(1)}))
		is.Equal(t, wrongArguments, a.err)
		is.Equal(t, "s4ql: wrong number of arguments: 1 for a statement with 2 parameters", a.message)
		r := d.run("delete from t where ? < c", math.NaN())
		r.is("not supported")
		is.Equal(t, "s4ql: not supported: argument 1, which is a real that is not a number", r.got.message)
		d.same()

		is.NotError(t, engine.Check("select * from t where ? = v and w < ?", 2))
		is.True(t, errors.Is(engine.Check("update t set v = ? where ? > w", 1), engine.ErrArguments))
		is.NotError(t, engine.Check(" ; DELETE FROM t WHERE - /* a */ 1 < v;", 0))
	})
}
