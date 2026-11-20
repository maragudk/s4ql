// Package stmttest is a generator of random statements of slices 1, 2, 4, 5, and 6, for tests: each
// statement is a text with the typed form of [maragu.dev/s4ql/internal/sql/stmt] that it holds, with
// names from pools, keywords and types in any case, values of every type with weight on their edges,
// and spaces and comments in every gap. The statements of slice 5, of several columns, come only when
// a test asks for them, with [Generator.Columns], and those of slice 6, with NULL, with
// [Generator.Nulls].
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

// Position of a name in the statements of slices 1 and 5.
type Position int

const (
	CreateTable Position = iota
	CreateColumn
	InsertTable
	InsertColumn
	SelectTable
	DropTable
	SelectColumn
)

// PositionTexts give, for each position, a statement of slice 1 or 5 with %v in that position.
var PositionTexts = []string{
	CreateTable:  "create table %v (v int primary key) strict, without rowid",
	CreateColumn: "create table t (%v int primary key) strict, without rowid",
	InsertTable:  "insert into %v (v) values (1)",
	InsertColumn: "insert into t (%v) values (1)",
	SelectTable:  "select * from %v",
	DropTable:    "drop table %v",
	SelectColumn: "select %v from t",
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
// In the select list, where SQLite reads some keywords as a value, such as NULL and CURRENT_TIME, a
// keyword is in the pool if SQLite reads it as the name of a column: on a table without a column of
// the name, SQLite gives `no such column` for it.
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
			if Position(p) == SelectColumn {
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
)

// Generator of random statements of slices 1, 2, 4, 5, and 6.
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
		switch {
		case !c.PrimaryKey && g.Create != nil && !shape.NotNull:
			// The column takes NULL, as the hook gives it.
		case !c.PrimaryKey && g.Create == nil && g.Nulls > 0 && g.r.IntN(g.Nulls) == 0:
			// The column takes NULL.
		case !c.PrimaryKey:
			notNull()
		case g.r.IntN(3) == 0:
			notNull()
			primaryKey()
		case g.r.IntN(2) == 0:
			primaryKey()
			notNull()
		default:
			primaryKey()
		}
		columns = append(columns, c)
	}
	return columns
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

// Statement at random: its text and the statement that it holds. An insert with a
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
		c.Table = g.name(CreateTable)
		g.gap(false)
		g.punct("(")
		g.gap(false)
		if g.Columns == 0 {
			column := stmt.Column{Name: g.name(CreateColumn), PrimaryKey: true}
			g.gap(true)
			column.Type = g.columnType(c.Table)
			g.keyword(strings.ToLower(column.Type.String()))
			g.gap(true)
			g.keyword("primary")
			g.gap(true)
			g.keyword("key")
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
			if g.Parameters > 0 && g.r.IntN(g.Parameters) == 0 {
				term.Parameter, term.Span = len(args)+1, stmt.Span{Start: g.b.Len(), End: g.b.Len() + 1}
				g.punct("?")
				args = append(args, Argument(v))
			} else {
				term.Value, term.Span = g.literal(v)
			}
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
		s, span = c, &c.Span
	default:
		c := &stmt.DropTable{}
		g.keyword("drop")
		g.gap(true)
		g.keyword("table")
		g.gap(true)
		c.Table = g.name(DropTable)
		s, span = c, &c.Span
	}
	*span = stmt.Span{Start: start, End: g.b.Len()}
	if g.r.IntN(2) == 0 {
		g.gap(false)
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
	return g.b.String(), s, args
}
