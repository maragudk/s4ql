// Package stmttest is a generator of random statements of slices 1, 2, and 4 to 13, for tests:
// each statement is a text with the typed form of [maragu.dev/s4ql/internal/sql/stmt] that it holds,
// with names from pools, keywords and types in any case, values of every type with weight on their
// edges, and spaces and comments in every gap. The statements of slice 5, of several columns, come
// only when a test asks for them, with [Generator.Columns], those of slice 6, with NULL, with
// [Generator.Nulls], those of slice 7, with WHERE, with [Generator.Conditions] or [Generator.Where],
// the UPDATE and DELETE of slice 8 with [Generator.Changes] or a kind of their own, the CREATE
// INDEX and DROP INDEX of slice 9 with [Generator.Indexes] or a kind of their own, the IF NOT
// EXISTS and IF EXISTS of slice 10 with [Generator.Exists], the DEFAULT of slice 11 with
// [Generator.Defaults], the value on the left of a comparison of slice 12 with [Generator.ValueFirst],
// and the ORDER BY and LIMIT of a select of slice 13 with [Generator.OrderBy] or [Generator.Order].
//
// The generator draws its random numbers from a [Rand] that the test gives it, so that a test
// chooses its seed and its generator of numbers.
package stmttest

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/stmt"
	"maragu.dev/s4ql/internal/sqlite"
)

// Rand is the source of the generator's random numbers, such as a *rand.Rand of math/rand/v2.
type Rand interface {
	// IntN is a number in [0, n).
	IntN(n int) int
	// Uint64 is any number.
	Uint64() uint64
}

// Position of a name in the statements of slices 1, 5, 8, and 9. The column of a condition of WHERE,
// in a select, an update, or a delete, is from the pool of SelectColumn.
type Position int

const (
	CreateTable Position = iota
	CreateColumn
	InsertTable
	InsertColumn
	SelectTable
	DropTable
	SelectColumn
	UpdateTable
	UpdateColumn
	DeleteTable
	IndexName
	IndexTable
	IndexColumn
	DropIndex
)

// PositionTexts give, for each position, a statement of slice 1, 5, 8, or 9 with %v in that position.
var PositionTexts = []string{
	CreateTable:  "create table %v (v int primary key) strict, without rowid",
	CreateColumn: "create table t (%v int primary key) strict, without rowid",
	InsertTable:  "insert into %v (v) values (1)",
	InsertColumn: "insert into t (%v) values (1)",
	SelectTable:  "select * from %v",
	DropTable:    "drop table %v",
	SelectColumn: "select %v from t",
	UpdateTable:  "update %v set v = 1",
	UpdateColumn: "update t set %v = 1",
	DeleteTable:  "delete from %v",
	IndexName:    "create index %v on t (v)",
	IndexTable:   "create index i on %v (v)",
	IndexColumn:  "create index i on t (%v)",
	DropIndex:    "drop index %v",
}

// BasePool of names: plain names, names in double quotes with odd content, names that differ only in
// case, and names close to the reserved ones. SQLite reads each as a name in every position, and the
// pass supports each.
var BasePool = []string{
	"t1", "T1", "a", "A", "_x", "x1$", "été", "ÉTÉ", "Ω", "😀", "x_", "a1b2", "rowid", "oid", "main", "temp",
	"sqlite", "sqlitex", "s4ql", "s4qlx", "pragma_x", "json", "json_eachx", "xsqlite_", "pragma_table_listx",
	`"t1"`, `"T1"`, `"a b"`, `"a""b"`, `""""`, `"select"`, `"from"`, `"x.y"`, `"1"`, `"  "`,
	`"ÆØÅ"`, `"æøå"`, `"t1 "`, `"-- not a comment"`, `"/* x */"`, `"'"`, `"[x]"`, "\"`x`\"", `"a\tb"`,
	`"a;b"`, `"sqlite"`, `"s4ql"`, `"Ｔ１"`, `"` + strings.Repeat("x", 200) + `"`, strings.Repeat("y", 100),
}

// Keywords of SQLite, in lower case.
func Keywords() []string {
	var kws []string
	for _, kw := range sqlite.Keywords() {
		kws = append(kws, strings.ToLower(kw))
	}
	return kws
}

// NamePools gives, for each position, the names of [BasePool] and the keywords of SQLite that SQLite
// reads as a name there. It fails if SQLite does not read a name of BasePool as a name somewhere.
//
// In the select list and in the columns of an index, where SQLite reads some keywords as a value,
// such as NULL and CURRENT_TIME, a keyword is in the pool if SQLite reads it as the name of a column:
// on a table without a column of the name, SQLite gives `no such column` for it.
var NamePools = sync.OnceValues(func() ([][]string, error) {
	c, err := sqlite.Open(":memory:")
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	if err := c.Exec("create table t (x int)"); err != nil {
		return nil, err
	}
	pools := make([][]string, len(PositionTexts))
	for p, text := range PositionTexts {
		pools[p] = BasePool
		for _, n := range BasePool {
			if v := oracle.Judge(fmt.Sprintf(text, n)); v.Kind != oracle.Parses {
				return nil, fmt.Errorf("SQLite does not read %v as a name in %q: %+v", n, text, v)
			}
		}
		for _, kw := range Keywords() {
			if oracle.Judge(fmt.Sprintf(text, kw)).Kind != oracle.Parses {
				continue
			}
			if Position(p) == SelectColumn || Position(p) == IndexColumn {
				s, err := c.Prepare(fmt.Sprintf(text, kw))
				if err == nil {
					_ = s.Finalize()
				}
				var e *sqlite.Error
				if !errors.As(err, &e) || e.Message != "no such column: "+kw {
					continue
				}
			}
			pools[p] = append(slices.Clip(pools[p]), kw)
		}
	}
	return pools, nil
})

// Quote the value of a name as a name in double quotes.
func Quote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

// Kind of a statement of the slices.
type Kind int

const (
	KindCreateTable Kind = iota
	KindInsert
	KindSelect
	KindDropTable
	KindUpdate
	KindDelete
	KindCreateIndex
	KindDropIndex
)

// Generator of random statements of slices 1, 2, and 4 to 13.
type Generator struct {
	r Rand
	// pools of names, for each [Position].
	pools [][]string
	// Type gives the type of the column of a create table of the table, or is nil for a type at
	// random.
	Type func(table stmt.Name) stmt.Type
	// Value gives the value of an insert into the table, or is nil for a value of a type at random,
	// with weight on the edges of each type.
	Value func(table stmt.Name) stmt.Value
	// Parameters, if not 0, makes one value of an insert in Parameters a plain ? in place of its
	// literal, with the value as its argument, as [Argument] gives it.
	Parameters int
	// Columns, if not 0, makes the generator write the forms of slice 5, with up to Columns columns:
	// a create table of from 1 to Columns columns, with the primary key at any position; an insert of
	// from 1 to Columns values, with a list of as many columns or without a list; and a select of *
	// or of from 1 to Columns+1 names of columns. With 0 it writes only the forms of slices 1, 2, and
	// 4, of one column, each with a list of its one column.
	Columns int
	// Create, if not nil, gives the columns of a create table of the table with [Generator.Columns], in
	// place of columns at random: the raw text of the name of each, its type, and whether it is the
	// primary key, which exactly one of them is, and for a column that is not the key whether it is
	// NOT NULL. The generator writes NOT NULL after such a column if its NotNull says so, and for the
	// key as for columns at random.
	Create func(table stmt.Name) []stmt.Column
	// Insert, if not nil, gives the list of columns of an insert into the table with
	// [Generator.Columns], as the raw text of each name, or nil for an insert without a list, and its
	// values, in place of a list and values at random. A list that it gives is not empty.
	Insert func(table stmt.Name) (list []string, values []stmt.Value)
	// Select, if not nil, gives the select list of a select of the table with [Generator.Columns], as
	// the raw text of each name, or nil for *, in place of a select list at random.
	Select func(table stmt.Name) []string
	// Nulls, if not 0, makes the generator write the forms of slice 6: one column in Nulls of a create
	// table with [Generator.Columns] that is not the primary key, and that [Generator.Create] does not
	// give, is without NOT NULL, and takes NULL, and one value in Nulls of an insert that does not come from [Generator.Insert] is NULL, as a
	// literal or, with [Generator.Parameters], as a ? with a nil argument. With 0 the generator draws as
	// it did before slice 6.
	Nulls int
	// Conditions, if not 0, makes the generator write the WHERE of slice 7: one select in two has a
	// WHERE of from 1 to Conditions conditions joined by AND, unless [Generator.Where] gives them. Each
	// is a column from the pool of [SelectColumn], one time in three that of an earlier condition or
	// of the select list, with one of the spellings of the operators and a value, NULL one time in
	// [Generator.Nulls] and otherwise of a type at random, or with IS NULL or IS NOT NULL. With 0 and
	// Where nil, the generator draws as it did before slice 7.
	Conditions int
	// Where, if not nil, gives the conditions of the WHERE of a select of the table, in place of
	// conditions at random, or nil for a select without WHERE: the raw text of the name of the column
	// of each, its operator, and, for a comparison, its value as Value.Value. The generator writes an
	// operator in any of its spellings, and with [Generator.Parameters] a value as a ? now and then,
	// with the value as its argument, as for an insert.
	Where func(table stmt.Name) []stmt.Condition
	// Changes, if not 0, makes the generator write the UPDATE and DELETE of slice 8 among the
	// statements of a kind at random: one in Changes is an update or a delete, each one time in two.
	// An update has from 1 to Columns assignments, or 1 without [Generator.Columns], each to another
	// column from the pool of [UpdateColumn], with = or == and a value as a condition has one, unless
	// [Generator.Update] gives them. An update or a delete has a WHERE as a select without
	// [Generator.Where] has one, with [Generator.Conditions], unless its hook gives its conditions. With
	// 0 the generator draws as it did before slice 8; [Generator.StatementOf] and [Generator.CallOf]
	// write an update or a delete whatever Changes is.
	Changes int
	// Update, if not nil, gives the assignments of an update of the table, in place of assignments at
	// random, one or more, each with the raw text of the name of its column and its value as
	// Value.Value, and the conditions of its WHERE, as [Generator.Where] gives them, or nil for an
	// update without WHERE. The generator writes = or == in each assignment, and with
	// [Generator.Parameters] a value as a ? now and then, with the value as its argument, as for an
	// insert, the ? of the assignments before those of the conditions.
	Update func(table stmt.Name) ([]stmt.Assignment, []stmt.Condition)
	// Delete, if not nil, gives the conditions of the WHERE of a delete from the table, as
	// [Generator.Where] gives them, or nil for a delete without WHERE, in place of conditions at random.
	Delete func(table stmt.Name) []stmt.Condition
	// Indexes, if not 0, makes the generator write the CREATE INDEX and DROP INDEX of slice 9 among
	// the statements of a kind at random: one in Indexes is a create index or a drop index, each one
	// time in two, after [Generator.Changes] has drawn. A create index has a name from the pool of
	// [IndexName], a table from that of [IndexTable], and from 1 to Columns columns, or 1 without
	// [Generator.Columns], each another column from the pool of [IndexColumn], with ASC, DESC, or
	// neither, unless [Generator.CreateIndex] gives them. A drop index has a name from the pool of
	// [DropIndex]. With 0 the generator draws as it did before slice 9; [Generator.StatementOf] and
	// [Generator.CallOf] write a create index or a drop index whatever Indexes is.
	Indexes int
	// CreateIndex, if not nil, gives the columns of a create index of the name index on the table, in
	// place of columns at random, one or more: the raw text of the name of each, and whether it is
	// Descending. The generator writes ASC or nothing for a column that is not descending.
	CreateIndex func(index, table stmt.Name) []stmt.IndexColumn
	// Exists, if not 0, makes the generator write the IF NOT EXISTS and IF EXISTS of slice 10: one
	// create table or create index in Exists says IF NOT EXISTS, and one drop table or drop index in
	// Exists says IF EXISTS, each with its keywords in any case and gaps between them. With 0 the
	// generator draws as it did before slice 10.
	Exists int
	// Defaults, if not 0, makes the generator write the DEFAULT of slice 11: one column in Defaults of
	// a create table that [Generator.Create] does not give says DEFAULT, with a literal of the type of
	// the column, or NULL one time in [Generator.Nulls] and one time in eight without Nulls, also on
	// the key and on a column that is NOT NULL, before, between, or after its other constraints. A
	// column that Create gives says DEFAULT if its Default has a Value, whatever Defaults is. With 0
	// and no such column the generator draws as it did before slice 11.
	Defaults int
	// ValueFirst, if not 0, makes the generator write the value on the left of a comparison of slice
	// 12: one comparison of a WHERE in ValueFirst, of a select, an update, or a delete, at random or
	// from a hook, is written with its value first, the operator swapped in any of its spellings, and
	// its column on the right, with ValueFirst set in the condition that it gives, which reads as the
	// comparison does with the column first. A comparison that a hook gives with ValueFirst set is
	// written so whatever ValueFirst is. A test for NULL keeps its column on the left. With 0 and no
	// such comparison the generator draws as it did before slice 12.
	ValueFirst int
	// OrderBy, if not 0, makes the generator write the ORDER BY and LIMIT of slice 13 on a select: one
	// select in OrderBy has an ORDER BY of from 1 to Columns+1 columns, or 1 or 2 without
	// [Generator.Columns], each another name from the pool of [SelectColumn], or one time in three
	// that of a column of the select list or of a condition, with ASC, DESC, or neither; and, drawn
	// apart from it, one select in OrderBy has a LIMIT of an integer from 0 to the largest of 64 bits,
	// small ones more often, with leading zeros now and then, or with [Generator.Parameters] a ? now
	// and then, with the integer as its argument, after those of the conditions. [Generator.Order]
	// gives them instead if it is not nil, whatever OrderBy is. With 0 and Order nil, the generator
	// draws as it did before slice 13.
	OrderBy int
	// Order, if not nil, gives the ORDER BY and the LIMIT of a select of the table, in place of ones
	// at random: the columns of ORDER BY, each with the raw text of its name and whether it is
	// Descending, or nil for a select without ORDER BY; and the LIMIT, with its integer as Value.Value,
	// or the zero Term for a select without LIMIT. No two of the columns may be one name. The
	// generator writes ASC or nothing for a column that is not descending.
	Order func(table stmt.Name) ([]stmt.OrderColumn, stmt.Term)
	b     strings.Builder
}

// NewGenerator with the random numbers of r and a pool of names for each [Position]. Each name of a
// pool must be one that SQLite reads as a name in its position, and that the pass supports.
func NewGenerator(r Rand, pools [][]string) *Generator {
	return &Generator{r: r, pools: pools}
}

// gaps between tokens. None starts with "-", so that a gap after a minus sign does not make it a
// comment.
var gaps = []string{" ", "  ", "\n", "\t", "\r\n", "\f", "/* c */", "/**/", "/* a\nb */", " /*-*/ ", " -- c\n", "\n--\n"}

// gap writes the text between two tokens: required if the tokens would run together without it.
func (g *Generator) gap(required bool) {
	if !required && g.r.IntN(3) == 0 {
		return
	}
	for {
		g.b.WriteString(gaps[g.r.IntN(len(gaps))])
		if g.r.IntN(4) != 0 {
			return
		}
	}
}

// keyword written in a random case.
func (g *Generator) keyword(w string) {
	switch g.r.IntN(3) {
	case 0:
		g.b.WriteString(w)
	case 1:
		g.b.WriteString(strings.ToUpper(w))
	default:
		for _, c := range w {
			if g.r.IntN(2) == 0 {
				c = []rune(strings.ToUpper(string(c)))[0]
			}
			g.b.WriteRune(c)
		}
	}
}

// punct writes a token that is not a word.
func (g *Generator) punct(p string) {
	g.b.WriteString(p)
}

// name written for the position p, with its value, as [Generator.pick] picks it.
func (g *Generator) name(p Position) stmt.Name {
	return g.write(g.pick(p))
}

// pick a name for the position p: its raw text. A bare name is in upper case one time in three, which
// is another name for a name with letters outside ASCII.
func (g *Generator) pick(p Position) string {
	pool := g.pools[p]
	raw := pool[g.r.IntN(len(pool))]
	if raw[0] != '"' && g.r.IntN(3) == 0 {
		raw = strings.ToUpper(raw)
	}
	return raw
}

// pickOther picks a name for the position p, as [Generator.pick] does, that SQLite does not take for
// any name of seen, by their folds. It gives "" if it finds none after a hundred tries.
func (g *Generator) pickOther(p Position, seen map[string]bool) string {
	for range 100 {
		raw := g.pick(p)
		if n := nameOf(raw, 0); !seen[n.Fold()] {
			return raw
		}
	}
	return ""
}

// write the name raw, and give it with its value and offset.
func (g *Generator) write(raw string) stmt.Name {
	n := nameOf(raw, g.b.Len())
	g.b.WriteString(raw)
	return n
}

// nameOf the raw text of a name at the offset: its value is the text without its double quotes, with
// "" read as ".
func nameOf(raw string, offset int) stmt.Name {
	value := raw
	if raw[0] == '"' {
		value = strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
	}
	return stmt.Name{Raw: raw, Value: value, Offset: offset}
}

// value of an insert into the table: NULL one time in [Generator.Nulls], and otherwise from
// [Generator.Value], or of a type at random.
func (g *Generator) value(table stmt.Name) stmt.Value {
	if g.Nulls > 0 && g.r.IntN(g.Nulls) == 0 {
		return stmt.Null{}
	}
	if g.Value != nil {
		return g.Value(table)
	}
	return ValueOf(g.r, Types[1+g.r.IntN(len(Types)-1)])
}

// columnType of a create table of the table: from [Generator.Type], or at random.
func (g *Generator) columnType(table stmt.Name) stmt.Type {
	if g.Type != nil {
		return g.Type(table)
	}
	return Types[g.r.IntN(len(Types))]
}

// columns of a create table of the table, of slice 5: from 1 to [Generator.Columns] of them, with
// the primary key at any position, which says NOT NULL too before PRIMARY KEY one time in three and
// after it one time in three, and every other column NOT NULL, but for one in [Generator.Nulls].
// Their names are from the pool of [CreateColumn], so that two of them can have one name, which
// SQLite refuses. [Generator.Create] can give the names, the types, the key, and which other columns
// are NOT NULL instead.
func (g *Generator) columns(table stmt.Name) []stmt.Column {
	var shapes []stmt.Column
	if g.Create != nil {
		shapes = g.Create(table)
	} else {
		n := 1 + g.r.IntN(g.Columns)
		key := g.r.IntN(n)
		shapes = make([]stmt.Column, n)
		for i := range shapes {
			shapes[i].PrimaryKey = i == key
		}
	}
	var columns []stmt.Column
	for i, shape := range shapes {
		if i > 0 {
			g.gap(false)
			g.punct(",")
			g.gap(false)
		}
		c := stmt.Column{PrimaryKey: shape.PrimaryKey}
		if g.Create != nil {
			c.Name = g.write(shape.Name.Raw)
		} else {
			c.Name = g.name(CreateColumn)
		}
		g.gap(true)
		if g.Create != nil {
			c.Type = shape.Type
		} else {
			c.Type = g.columnType(table)
		}
		g.keyword(strings.ToLower(c.Type.String()))
		notNull := func() {
			g.gap(true)
			g.keyword("not")
			g.gap(true)
			g.keyword("null")
			c.NotNull = true
		}
		primaryKey := func() {
			g.gap(true)
			g.keyword("primary")
			g.gap(true)
			g.keyword("key")
		}
		var constraints []func()
		switch {
		case !c.PrimaryKey && g.Create != nil && !shape.NotNull:
			// The column takes NULL, as the hook gives it.
		case !c.PrimaryKey && g.Create == nil && g.Nulls > 0 && g.r.IntN(g.Nulls) == 0:
			// The column takes NULL.
		case !c.PrimaryKey:
			constraints = []func(){notNull}
		case g.r.IntN(3) == 0:
			constraints = []func(){notNull, primaryKey}
		case g.r.IntN(2) == 0:
			constraints = []func(){primaryKey, notNull}
		default:
			constraints = []func(){primaryKey}
		}
		var given stmt.Value
		if g.Create != nil {
			given = shape.Default.Value
		}
		constraints = g.withDefault(&c, given, constraints)
		for _, f := range constraints {
			f()
		}
		columns = append(columns, c)
	}
	return columns
}

// withDefault gives the constraints of the column c with its DEFAULT among them, at a place at
// random, if it has one: of the value given, or with [Generator.Defaults] one time in Defaults
// without a value given, as [Generator.Defaults] says, and the constraints as they are otherwise.
// The DEFAULT, when it is written, sets the Default of c.
func (g *Generator) withDefault(c *stmt.Column, given stmt.Value, constraints []func()) []func() {
	v := given
	if v == nil && g.Create == nil && g.Defaults > 0 && g.r.IntN(g.Defaults) == 0 {
		switch {
		case g.Nulls > 0 && g.r.IntN(g.Nulls) == 0, g.Nulls == 0 && g.r.IntN(8) == 0:
			v = stmt.Null{}
		default:
			v = ValueOf(g.r, c.Type)
		}
	}
	if v == nil {
		return constraints
	}
	write := func() {
		g.gap(true)
		g.keyword("default")
		g.gap(true)
		value, span := g.literal(v)
		c.Default = stmt.Default{Value: value, Span: span}
	}
	return slices.Insert(constraints, g.r.IntN(len(constraints)+1), write)
}

// columnList of an insert of slice 5, with the gaps around it, before VALUES: none one time in three,
// and otherwise n names from the pool of [InsertColumn], of which no two are one name to SQLite. If
// the pool runs out of other names, the list is shorter, and the insert has more values than columns,
// which SQLite refuses once it has found the table.
func (g *Generator) columnList(n int) []stmt.Name {
	if g.r.IntN(3) == 0 {
		g.gap(true)
		return nil
	}
	g.gap(false)
	g.punct("(")
	g.gap(false)
	seen := map[string]bool{}
	var names []stmt.Name
	for i := range n {
		raw := g.pickOther(InsertColumn, seen)
		if raw == "" {
			break
		}
		if i > 0 {
			g.gap(false)
			g.punct(",")
			g.gap(false)
		}
		name := g.write(raw)
		seen[name.Fold()] = true
		names = append(names, name)
	}
	g.gap(false)
	g.punct(")")
	g.gap(false)
	return names
}

// list of the columns of an insert of slice 5 with the names raws, as [Generator.columnList] writes
// one, or with the gap of no list if raws is nil.
func (g *Generator) list(raws []string) []stmt.Name {
	if raws == nil {
		g.gap(true)
		return nil
	}
	g.gap(false)
	g.punct("(")
	g.gap(false)
	var names []stmt.Name
	for i, raw := range raws {
		if i > 0 {
			g.gap(false)
			g.punct(",")
			g.gap(false)
		}
		names = append(names, g.write(raw))
	}
	g.gap(false)
	g.punct(")")
	g.gap(false)
	return names
}

// resultColumnsOf a select of slice 5 with the names raws, with the gaps around them, as
// [Generator.resultColumns] writes them.
func (g *Generator) resultColumnsOf(raws []string) []stmt.Name {
	var names []stmt.Name
	for i, raw := range raws {
		if i > 0 {
			g.gap(false)
			g.punct(",")
		}
		g.gap(true)
		names = append(names, g.write(raw))
	}
	g.gap(true)
	return names
}

// resultColumns of a select of slice 5, with the gaps around them: from 1 to [Generator.Columns]+1
// names from the pool of [SelectColumn], of which one in four after the first is the name of an
// earlier one, as it is written there.
func (g *Generator) resultColumns() []stmt.Name {
	var names []stmt.Name
	for i := range 1 + g.r.IntN(g.Columns+1) {
		if i > 0 {
			g.gap(false)
			g.punct(",")
		}
		g.gap(true)
		if i > 0 && g.r.IntN(4) == 0 {
			names = append(names, g.write(names[g.r.IntN(len(names))].Raw))
			continue
		}
		names = append(names, g.name(SelectColumn))
	}
	g.gap(true)
	return names
}

// literal of the value v, with its span, and the value that SQLite gives it, as [Literal] writes
// it.
func (g *Generator) literal(v stmt.Value) (stmt.Value, stmt.Span) {
	start := g.b.Len()
	text, value := Literal(g.r, v)
	g.b.WriteString(text)
	return value, stmt.Span{Start: start, End: g.b.Len()}
}

// term of the value v, after the arguments args of the parameters before it: one time in
// [Generator.Parameters] a ? with the argument of v, as [Argument] gives it, added to args, and
// otherwise the literal of v.
func (g *Generator) term(v stmt.Value, args []any) (stmt.Term, []any) {
	var term stmt.Term
	if g.Parameters > 0 && g.r.IntN(g.Parameters) == 0 {
		term.Parameter, term.Span = len(args)+1, stmt.Span{Start: g.b.Len(), End: g.b.Len() + 1}
		g.punct("?")
		return term, append(args, Argument(v))
	}
	term.Value, term.Span = g.literal(v)
	return term, args
}

// operators of a condition, and the spellings of each.
var operators = []struct {
	op        stmt.Operator
	spellings []string
}{
	{stmt.OpEqual, []string{"=", "=="}}, {stmt.OpNotEqual, []string{"!=", "<>"}}, {stmt.OpLess, []string{"<"}},
	{stmt.OpLessOrEqual, []string{"<="}}, {stmt.OpGreater, []string{">"}}, {stmt.OpGreaterOrEqual, []string{">="}},
	{stmt.OpIsNull, nil}, {stmt.OpIsNotNull, nil},
}

// spelling at random of the operator of a comparison op.
func (g *Generator) spelling(op stmt.Operator) string {
	for _, o := range operators {
		if o.op == op && o.spellings != nil {
			return o.spellings[g.r.IntN(len(o.spellings))]
		}
	}
	panic(fmt.Sprintf("stmttest: no operator %v", op))
}

// where of the select s, of slice 7, after the name of its table, and the arguments of its
// parameters: the conditions that [Generator.Where] gives, or with [Generator.Conditions] one time in
// two conditions at random, or none.
func (g *Generator) where(s *stmt.Select) ([]stmt.Condition, []any) {
	if g.Where != nil {
		return g.conditions(g.Where(s.Table), false, s.Columns, nil)
	}
	return g.conditions(g.randomConditions(), true, s.Columns, nil)
}

// randomConditions of a WHERE: with [Generator.Conditions] one time in two from 1 to Conditions of
// them, which are zero conditions for [Generator.conditions] to draw, and otherwise none.
func (g *Generator) randomConditions() []stmt.Condition {
	if g.Conditions > 0 && g.r.IntN(2) == 0 {
		return make([]stmt.Condition, 1+g.r.IntN(g.Conditions))
	}
	return nil
}

// conditions of a WHERE, written with WHERE before them if there are any, after the arguments args of
// the parameters before them, and the arguments with those of theirs. The conditions are given, or,
// if random, drawn at random, as many as given has, with the columns of the statement before the WHERE.
func (g *Generator) conditions(given []stmt.Condition, random bool, columns []stmt.Name, args []any) ([]stmt.Condition, []any) {
	if len(given) == 0 {
		return nil, args
	}
	g.gap(true)
	g.keyword("where")
	var conditions []stmt.Condition
	for i, c := range given {
		if i > 0 {
			g.gap(true)
			g.keyword("and")
		}
		g.gap(true)
		if random {
			c = g.condition(columns, conditions)
		}
		test := c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull
		if !test && (c.ValueFirst || g.ValueFirst > 0 && g.r.IntN(g.ValueFirst) == 0) {
			// The value on the left, the operator swapped, and the column on the right, which reads as
			// the condition c.
			w := stmt.Condition{Operator: c.Operator, ValueFirst: true}
			w.Value, args = g.term(c.Value.Value, args)
			g.gap(false)
			g.punct(g.spelling(c.Operator.Swapped()))
			g.gap(false)
			w.Column = g.write(c.Column.Raw)
			conditions = append(conditions, w)
			continue
		}
		w := stmt.Condition{Column: g.write(c.Column.Raw), Operator: c.Operator}
		var spellings []string
		for _, o := range operators {
			if o.op == c.Operator {
				spellings = o.spellings
			}
		}
		switch {
		case c.Operator == stmt.OpIsNull || c.Operator == stmt.OpIsNotNull:
			g.gap(true)
			g.keyword("is")
			if c.Operator == stmt.OpIsNotNull {
				g.gap(true)
				g.keyword("not")
			}
			g.gap(true)
			g.keyword("null")
		case spellings == nil:
			panic(fmt.Sprintf("stmttest: no operator %v", c.Operator))
		default:
			g.gap(false)
			g.punct(spellings[g.r.IntN(len(spellings))])
			g.gap(false)
			w.Value, args = g.term(c.Value.Value, args)
		}
		conditions = append(conditions, w)
	}
	return conditions, args
}

// condition at random of a select with the select list columns, after the conditions before: a
// column from the pool of [SelectColumn], or one time in three that of an earlier condition or of the
// select list, with an operator at random, and for a comparison a value, NULL one time in
// [Generator.Nulls] and otherwise of a type at random. Only the raw text of the name of its column
// is set.
func (g *Generator) condition(columns []stmt.Name, before []stmt.Condition) stmt.Condition {
	names := slices.Clone(columns)
	for _, c := range before {
		names = append(names, c.Column)
	}
	var c stmt.Condition
	if len(names) > 0 && g.r.IntN(3) == 0 {
		c.Column.Raw = names[g.r.IntN(len(names))].Raw
	} else {
		c.Column.Raw = g.pick(SelectColumn)
	}
	c.Operator = operators[g.r.IntN(len(operators))].op
	if c.Operator != stmt.OpIsNull && c.Operator != stmt.OpIsNotNull {
		c.Value.Value = g.randomValue()
	}
	return c
}

// randomValue of a condition or an assignment: NULL one time in [Generator.Nulls], and otherwise of a
// type at random, as [Generator.value] draws a value without [Generator.Value], which gives values of
// an insert.
func (g *Generator) randomValue() stmt.Value {
	if g.Nulls > 0 && g.r.IntN(g.Nulls) == 0 {
		return stmt.Null{}
	}
	return ValueOf(g.r, Types[1+g.r.IntN(len(Types)-1)])
}

// update of the table, of slice 8, after SET, and the arguments of its parameters: the assignments
// and the conditions that [Generator.Update] gives, or assignments at random and conditions as for a
// select without [Generator.Where].
func (g *Generator) update(c *stmt.Update) []any {
	var given []stmt.Assignment
	var where []stmt.Condition
	if g.Update != nil {
		given, where = g.Update(c.Table)
	} else {
		given = make([]stmt.Assignment, 1+g.r.IntN(max(g.Columns, 1)))
	}
	seen := map[string]bool{}
	var args []any
	for i, a := range given {
		if g.Update == nil {
			raw := g.pickOther(UpdateColumn, seen)
			if raw == "" {
				break
			}
			a = stmt.Assignment{Column: stmt.Name{Raw: raw}, Value: stmt.Term{Value: g.randomValue()}}
		}
		if i > 0 {
			g.gap(false)
			g.punct(",")
		}
		g.gap(true)
		w := stmt.Assignment{Column: g.write(a.Column.Raw)}
		seen[w.Column.Fold()] = true
		g.gap(false)
		if g.r.IntN(4) == 0 {
			g.punct("==")
		} else {
			g.punct("=")
		}
		g.gap(false)
		w.Value, args = g.term(a.Value.Value, args)
		c.Set = append(c.Set, w)
	}
	// A condition at random can test a column of the assignments that SQLite reads as a column in
	// WHERE too: some keywords, such as CURRENT_DATE, are names after SET and values in WHERE.
	var columns []stmt.Name
	for _, a := range c.Set {
		if raw := a.Column.Raw; slices.Contains(g.pools[SelectColumn], raw) || slices.Contains(g.pools[SelectColumn], strings.ToLower(raw)) {
			columns = append(columns, a.Column)
		}
	}
	if g.Update != nil {
		c.Where, args = g.conditions(where, false, columns, args)
	} else {
		c.Where, args = g.conditions(g.randomConditions(), true, columns, args)
	}
	return args
}

// order of the select s, of slice 13, after its WHERE, after the arguments args of the parameters
// before it, and the arguments with that of its LIMIT: the ORDER BY and the LIMIT that
// [Generator.Order] gives, or with [Generator.OrderBy] ones at random, or none, without a draw.
func (g *Generator) order(s *stmt.Select, args []any) ([]stmt.OrderColumn, stmt.Term, []any) {
	var given []stmt.OrderColumn
	var limit stmt.Term
	random := g.Order == nil
	switch {
	case g.Order != nil:
		given, limit = g.Order(s.Table)
	case g.OrderBy > 0:
		if g.r.IntN(g.OrderBy) == 0 {
			given = make([]stmt.OrderColumn, 1+g.r.IntN(max(g.Columns, 1)+1))
		}
		if g.r.IntN(g.OrderBy) == 0 {
			limit.Value = g.limitValue()
		}
	default:
		return nil, stmt.Term{}, args
	}
	// A column at random is one time in three a column of the select list or of a condition.
	var named []stmt.Name
	named = append(named, s.Columns...)
	for _, c := range s.Where {
		named = append(named, c.Column)
	}
	var columns []stmt.OrderColumn
	seen := map[string]bool{}
	for i, c := range given {
		if random {
			var others []string
			for _, n := range named {
				if !seen[n.Fold()] {
					others = append(others, n.Raw)
				}
			}
			raw := ""
			if len(others) > 0 && g.r.IntN(3) == 0 {
				raw = others[g.r.IntN(len(others))]
			} else if raw = g.pickOther(SelectColumn, seen); raw == "" {
				break
			}
			c = stmt.OrderColumn{Name: stmt.Name{Raw: raw}, Descending: g.r.IntN(3) == 0}
		}
		if i == 0 {
			g.gap(true)
			g.keyword("order")
			g.gap(true)
			g.keyword("by")
			g.gap(true)
		} else {
			g.gap(false)
			g.punct(",")
			g.gap(false)
		}
		w := stmt.OrderColumn{Name: g.write(c.Name.Raw), Descending: c.Descending}
		seen[w.Name.Fold()] = true
		switch {
		case w.Descending:
			g.gap(true)
			g.keyword("desc")
		case g.r.IntN(2) == 0:
			g.gap(true)
			g.keyword("asc")
		}
		columns = append(columns, w)
	}
	if limit.Value == nil {
		return columns, stmt.Term{}, args
	}
	g.gap(true)
	g.keyword("limit")
	g.gap(true)
	if g.Parameters > 0 && g.r.IntN(g.Parameters) == 0 {
		v := limit.Value
		limit = stmt.Term{Parameter: len(args) + 1, Span: stmt.Span{Start: g.b.Len(), End: g.b.Len() + 1}}
		g.punct("?")
		return columns, limit, append(args, Argument(v))
	}
	digits := fmt.Sprint(limit.Value)
	if g.r.IntN(4) == 0 {
		digits = strings.Repeat("0", 1+g.r.IntN(30)) + digits
	}
	limit.Span = stmt.Span{Start: g.b.Len(), End: g.b.Len() + len(digits)}
	g.b.WriteString(digits)
	return columns, limit, args
}

// limitValue at random of a LIMIT: an integer from 0 to the largest of 64 bits, from 0 to 5 one time
// in two, and otherwise one of [Integer] with its bits turned over if it is negative.
func (g *Generator) limitValue() stmt.Value {
	if g.r.IntN(2) == 0 {
		return stmt.Integer(g.r.IntN(6))
	}
	i := Integer(g.r)
	if i < 0 {
		i = ^i
	}
	return stmt.Integer(i)
}

// indexColumns of a create index of the name index on the table, of slice 9, between its
// parentheses: those that [Generator.CreateIndex] gives, or from 1 to [Generator.Columns] at random,
// or 1 without Columns, each another name from the pool of [IndexColumn]. Each is descending one time
// in three, and otherwise ascending, with ASC one time in two.
func (g *Generator) indexColumns(index, table stmt.Name) []stmt.IndexColumn {
	var given []stmt.IndexColumn
	if g.CreateIndex != nil {
		given = g.CreateIndex(index, table)
	} else {
		given = make([]stmt.IndexColumn, 1+g.r.IntN(max(g.Columns, 1)))
	}
	seen := map[string]bool{}
	var columns []stmt.IndexColumn
	for i, c := range given {
		if g.CreateIndex == nil {
			raw := g.pickOther(IndexColumn, seen)
			if raw == "" {
				break
			}
			c = stmt.IndexColumn{Name: stmt.Name{Raw: raw}, Descending: g.r.IntN(3) == 0}
		}
		if i > 0 {
			g.gap(false)
			g.punct(",")
		}
		g.gap(false)
		w := stmt.IndexColumn{Name: g.write(c.Name.Raw), Descending: c.Descending}
		seen[w.Name.Fold()] = true
		switch {
		case w.Descending:
			g.gap(true)
			g.keyword("desc")
		case g.r.IntN(2) == 0:
			g.gap(true)
			g.keyword("asc")
		}
		columns = append(columns, w)
	}
	return columns
}

// exists writes IF NOT EXISTS, or IF EXISTS if not is false, and a gap after it, one time in
// [Generator.Exists], and reports whether it wrote it. With Exists 0 it draws nothing.
func (g *Generator) exists(not bool) bool {
	if g.Exists == 0 || g.r.IntN(g.Exists) != 0 {
		return false
	}
	g.keyword("if")
	g.gap(true)
	if not {
		g.keyword("not")
		g.gap(true)
	}
	g.keyword("exists")
	g.gap(true)
	return true
}

// Statement at random: its text and the statement that it holds. A statement with a
// parameter, which [Generator.Parameters] asks for, is written without its argument.
func (g *Generator) Statement() (string, stmt.Statement) {
	src, s, _ := g.statement(-1)
	return src, s
}

// StatementOf the kind: its text and the statement that it holds, as [Generator.Statement] gives it.
func (g *Generator) StatementOf(k Kind) (string, stmt.Statement) {
	src, s, _ := g.statement(k)
	return src, s
}

// CallOf the kind: its text, the statement that it holds, and an argument for each of its
// parameters, as [Argument] gives it.
func (g *Generator) CallOf(k Kind) (string, stmt.Statement, []any) {
	return g.statement(k)
}

// statement of the kind k, or of a kind at random if k is negative, with an argument for each of its
// parameters.
func (g *Generator) statement(k Kind) (string, stmt.Statement, []any) {
	g.b.Reset()
	switch g.r.IntN(4) {
	case 1:
		g.b.WriteString(";")
	case 2:
		g.gap(true)
		g.b.WriteString("; ;")
		g.gap(false)
	}
	g.gap(false)
	start := g.b.Len()
	if k < 0 {
		k = Kind(g.r.IntN(4))
		if g.Changes > 0 && g.r.IntN(g.Changes) == 0 {
			k = KindUpdate + Kind(g.r.IntN(2))
		}
		if g.Indexes > 0 && g.r.IntN(g.Indexes) == 0 {
			k = KindCreateIndex + Kind(g.r.IntN(2))
		}
	}
	var s stmt.Statement
	var span *stmt.Span
	var args []any
	switch k {
	case KindCreateTable:
		c := &stmt.CreateTable{}
		g.keyword("create")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.IfNotExists = g.exists(true)
		c.Table = g.name(CreateTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		if g.Columns == 0 {
			column := stmt.Column{Name: g.name(CreateColumn), PrimaryKey: true}
			g.gap(true)
			column.Type = g.columnType(c.Table)
			g.keyword(strings.ToLower(column.Type.String()))
			primaryKey := func() {
				g.gap(true)
				g.keyword("primary")
				g.gap(true)
				g.keyword("key")
			}
			for _, f := range g.withDefault(&column, nil, []func(){primaryKey}) {
				f()
			}
			c.Columns = []stmt.Column{column}
		} else {
			c.Columns = g.columns(c.Table)
		}
		g.gap(false)
		g.punct(")")
		options := []func(){
			func() { g.keyword("strict") },
			func() { g.keyword("without"); g.gap(true); g.keyword("rowid") },
		}
		if g.r.IntN(2) == 0 {
			options[0], options[1] = options[1], options[0]
		}
		g.gap(false)
		options[0]()
		g.gap(false)
		g.punct(",")
		g.gap(false)
		options[1]()
		s, span = c, &c.Span
	case KindInsert:
		c := &stmt.Insert{}
		g.keyword("insert")
		g.gap(true)
		g.keyword("into")
		g.gap(true)
		c.Table = g.name(InsertTable)
		values := 1
		var given []stmt.Value
		switch {
		case g.Columns > 0 && g.Insert != nil:
			var list []string
			list, given = g.Insert(c.Table)
			values = len(given)
			c.Columns = g.list(list)
		case g.Columns == 0:
			g.gap(false)
			g.punct("(")
			g.gap(false)
			c.Columns = []stmt.Name{g.name(InsertColumn)}
			g.gap(false)
			g.punct(")")
			g.gap(false)
		default:
			values = 1 + g.r.IntN(g.Columns)
			c.Columns = g.columnList(values)
		}
		g.keyword("values")
		g.gap(false)
		g.punct("(")
		g.gap(false)
		for i := range values {
			if i > 0 {
				g.gap(false)
				g.punct(",")
				g.gap(false)
			}
			var v stmt.Value
			if given != nil {
				v = given[i]
			} else {
				v = g.value(c.Table)
			}
			var term stmt.Term
			term, args = g.term(v, args)
			c.Values = append(c.Values, term)
		}
		g.gap(false)
		g.punct(")")
		s, span = c, &c.Span
	case KindSelect:
		c := &stmt.Select{}
		g.keyword("select")
		// table is the raw text of the name of the table when it is picked before the select list, for
		// [Generator.Select], or "".
		var table string
		switch {
		case g.Columns > 0 && g.Select != nil:
			table = g.pick(SelectTable)
			if names := g.Select(nameOf(table, 0)); names != nil {
				c.Columns = g.resultColumnsOf(names)
				break
			}
			g.gap(false)
			g.punct("*")
			g.gap(false)
		case g.Columns == 0 || g.r.IntN(2) == 0:
			g.gap(false)
			g.punct("*")
			g.gap(false)
		default:
			c.Columns = g.resultColumns()
		}
		g.keyword("from")
		g.gap(true)
		if table != "" {
			c.Table = g.write(table)
		} else {
			c.Table = g.name(SelectTable)
		}
		c.Where, args = g.where(c)
		c.OrderBy, c.Limit, args = g.order(c, args)
		s, span = c, &c.Span
	case KindUpdate:
		c := &stmt.Update{}
		g.keyword("update")
		g.gap(true)
		c.Table = g.name(UpdateTable)
		g.gap(true)
		g.keyword("set")
		args = g.update(c)
		s, span = c, &c.Span
	case KindDelete:
		c := &stmt.Delete{}
		g.keyword("delete")
		g.gap(true)
		g.keyword("from")
		g.gap(true)
		c.Table = g.name(DeleteTable)
		if g.Delete != nil {
			c.Where, args = g.conditions(g.Delete(c.Table), false, nil, nil)
		} else {
			c.Where, args = g.conditions(g.randomConditions(), true, nil, nil)
		}
		s, span = c, &c.Span
	case KindCreateIndex:
		c := &stmt.CreateIndex{}
		g.keyword("create")
		g.gap(true)
		g.keyword("index")
		g.gap(true)
		c.IfNotExists = g.exists(true)
		c.Index = g.name(IndexName)
		g.gap(true)
		g.keyword("on")
		g.gap(true)
		c.Table = g.name(IndexTable)
		g.gap(false)
		g.punct("(")
		c.Columns = g.indexColumns(c.Index, c.Table)
		g.gap(false)
		g.punct(")")
		s, span = c, &c.Span
	case KindDropIndex:
		c := &stmt.DropIndex{}
		g.keyword("drop")
		g.gap(true)
		g.keyword("index")
		g.gap(true)
		c.IfExists = g.exists(false)
		c.Index = g.name(DropIndex)
		s, span = c, &c.Span
	default:
		c := &stmt.DropTable{}
		g.keyword("drop")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.IfExists = g.exists(false)
		c.Table = g.name(DropTable)
		s, span = c, &c.Span
	}
	*span = stmt.Span{Start: start, End: g.b.Len()}
	// kept is where the text that SQLite keeps for a table or an index ends: at the semicolon, or at
	// the end of the text.
	kept := -1
	if g.r.IntN(2) == 0 {
		g.gap(false)
		kept = g.b.Len()
		g.punct(";")
		span.End = g.b.Len()
		switch g.r.IntN(3) {
		case 0:
			g.gap(false)
			g.b.WriteString(";")
		case 1:
			g.b.WriteString(" -- end")
		}
	} else if g.r.IntN(2) == 0 {
		g.gap(true)
	}
	src := g.b.String()
	if c, ok := s.(*stmt.CreateTable); ok {
		if kept < 0 {
			kept = len(src)
		}
		c.SQL = "CREATE TABLE " + src[c.Table.Offset:kept]
	}
	if c, ok := s.(*stmt.CreateIndex); ok {
		if kept < 0 {
			kept = len(src)
		}
		c.SQL = "CREATE INDEX " + src[c.Index.Offset:kept]
	}
	return src, s, args
}
