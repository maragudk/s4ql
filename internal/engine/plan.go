package engine

import (
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/s4qlpb"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// Plan of a statement: what to do, as plain data, which [Format] prints. A plan holds the names as
// the statement writes them, and nothing of the catalogue: the operators look the names up in the
// transaction that runs the plan, which is where SQLite's errors about names come from.
//
// A plan is a [*CreateTable], a [*DropTable], an [*Insert], an [*Update], a [*Delete], or a
// [*Query]. An insert, an update, a delete, and a query read their rows from a tree of [Node]s.
type Plan interface {
	plan()
}

// Node of a plan that gives rows: a [*Scan], a [*Filter], an [*Order], a [*Limit], a [*Values], or a
// [*Project].
type Node interface {
	node()
}

// Expr is an expression that gives a value: an [*Integer], a [*Real], a [*Text], a [*Blob], a
// [*Null], a [*Parameter], or, as the value of a condition, a [*Subquery].
type Expr interface {
	expr()
}

// CreateTable creates a table, which no table of its name may exist for, or, if IfNotExists, does
// nothing where a table of its name exists.
type CreateTable struct {
	Table       stmt.Name
	IfNotExists bool
	Columns     []ColumnDef
	// PrimaryKey holds the positions in Columns of the columns of the primary key, in key order.
	PrimaryKey []int
	// SQL is the text that SQLite keeps for the table, as [stmt.CreateTable] gives it, which
	// decides whether SQLite refuses the statement as too big. [Format] does not print it.
	SQL string
}

// ColumnDef is a column of a [CreateTable]. A column is NOT NULL unless it is Nullable, which a
// column of the primary key never is. Default is the literal of its DEFAULT, a [*Null] for DEFAULT
// NULL, or nil if it says none.
type ColumnDef struct {
	Name     stmt.Name
	Type     s4qlpb.Type
	Nullable bool
	Default  Expr
}

// DropTable drops a table, which must exist, or, if IfExists, does nothing where no table of its
// name exists.
type DropTable struct {
	Table    stmt.Name
	IfExists bool
}

// CreateIndex creates an index, which is not unique, on columns of a table, which must exist. No
// table and no index of its name may exist, but, if IfNotExists, it does nothing where an index of
// its name exists.
type CreateIndex struct {
	Index       stmt.Name
	IfNotExists bool
	Table       stmt.Name
	// Columns of the index, in the order of the index.
	Columns []stmt.IndexColumn
	// SQL is the text that SQLite keeps for the index, as [stmt.CreateIndex] gives it, which decides
	// whether SQLite refuses the statement as too big. [Format] does not print it.
	SQL string
}

// DropIndex drops an index, which must exist, or, if IfExists, does nothing where no index of its
// name exists.
type DropIndex struct {
	Index    stmt.Name
	IfExists bool
}

// Insert inserts the rows of Source into a table, each as a new row. Source gives a value for each
// of Columns, in their order, or, if Columns is nil, for each column of the table, in the order of
// the table. It gives each row that it inserts, as the table stores it, if Returning is not nil.
type Insert struct {
	Table     stmt.Name
	Columns   []stmt.Name
	Source    Node
	Returning *Returning
}

// Update sets the columns of Set in each row of Source, a [*Scan] of the table, or a [*Filter] of
// one if the statement has a WHERE. It gives each row that it updates, with the values after the
// update, if Returning is not nil. Subqueries holds the scalar subqueries of its WHERE, as for a
// [Query].
type Update struct {
	Table      stmt.Name
	Set        []Assignment
	Source     Node
	Returning  *Returning
	Subqueries []*Query
}

// Returning of an [Insert], an [Update], or a [Delete]: the columns of each row that the statement
// gives, Columns, in their order, each as often as it is named, or, if Star, `*`: every column, in
// the order of the table, as a [Project] gives the columns of a query.
type Returning struct {
	Star    bool
	Columns []stmt.Name
}

// Assignment of an [Update]: the column of the name, as the statement writes it, gets Value.
type Assignment struct {
	Column stmt.Name
	Value  Expr
}

// Delete deletes each row of Source, a [*Scan] of the table, or a [*Filter] of one if the statement
// has a WHERE. It gives each row that it deletes, with the values that the row had, if Returning is
// not nil. Subqueries holds the scalar subqueries of its WHERE, as for a [Query].
type Delete struct {
	Table      stmt.Name
	Source     Node
	Returning  *Returning
	Subqueries []*Query
}

// Query gives the rows of Root: a [*Project], over a [*Limit] if the select has a LIMIT, over an
// [*Order] if it has an ORDER BY, over a [*Filter] if it has a WHERE, over the [*Scan] of its table.
//
// Subqueries holds the scalar subqueries of its WHERE, in the order of the text, each a Query of the
// same shape whose Project has one column, or `*`, and whose own Subqueries is nil; a [*Subquery] of
// a condition names one by its number, from 1. A subquery gives the value of its column in the first
// row that it gives, or NULL if it gives none. It reads the state that the statement reads, once,
// before the statement changes any row.
type Query struct {
	Root       Node
	Subqueries []*Query
}

// Scan gives every row of a table, forward or backward in the order of its primary key, or of the
// index that the engine reads it by for the conditions of a [Filter] and the columns of an [Order]
// above it.
type Scan struct {
	Table stmt.Name
}

// Order gives the rows of Source in the order of Columns, of the ORDER BY of a select: by the first,
// and among rows that are equal in it by the next. The order of rows that are equal in all of them
// is not specified, and varies. It says what the order is, and not how the rows get into it: the
// engine reads them in that order by the primary key or by an index where one holds them so, and
// has them sorted otherwise.
type Order struct {
	Source  Node
	Columns []OrderColumn
}

// OrderColumn is a column of an [Order], by its name as the statement writes it: ascending, with
// NULL before every value, or, if Descending, descending, with NULL after every value.
type OrderColumn struct {
	Column     stmt.Name
	Descending bool
}

// Limit gives the first rows of Source, at most as many as Count says, of the LIMIT of a select:
// an [*Integer] from 0, or a [*Parameter], whose argument must be an integer from 0.
type Limit struct {
	Source Node
	Count  Expr
}

// Filter gives the rows of Source for which every one of Conditions is true, in the order of Source.
// A comparison with NULL on either side is neither true nor false, as in SQLite, so a row for which
// one is NULL is left out.
type Filter struct {
	Source     Node
	Conditions []Condition
}

// Condition of a [Filter]: the column of the name, as the statement writes it, compared with Value
// by Operator, or tested for NULL, with a Value of nil. ValueFirst reports whether the statement
// writes the value on the left of the operator and the column on the right, as in `5 < a`, of slice
// 12. Operator is then the operator turned round, as [stmt.Condition] has it, so that `5 < a` is the
// condition `a > 5`, and the engine reads every condition one way, whichever side its column is on.
// ValueFirst is there only for the planted bug unturnedBound and for tests: the statement pass has
// turned the operator round already, and nothing may turn Operator round again from ValueFirst.
type Condition struct {
	Column     stmt.Name
	Operator   stmt.Operator
	Value      Expr
	ValueFirst bool
}

// Values gives one row for each of Rows, which hold one expression for each value.
type Values struct {
	Rows [][]Expr
}

// Project gives the columns of each row of Source that a query names: Columns, in their order, each
// as often as it is named, or, if Star, `*`: every column, in the order of the table.
type Project struct {
	Source  Node
	Star    bool
	Columns []stmt.Name
}

// Integer is an integer literal.
type Integer struct {
	Value int64
}

// Real is a real literal: a finite number, which can be -0.0.
type Real struct {
	Value float64
}

// Text is a string literal: valid UTF-8 without the character NUL.
type Text struct {
	Value string
}

// Blob is a blob literal. Its value is not nil, also when it is empty.
type Blob struct {
	Value []byte
}

// Null is the literal NULL.
type Null struct{}

// Parameter is the argument that the statement is run with for its parameter of the number Number,
// which counts from 1.
type Parameter struct {
	Number int
}

// Subquery is the value of the scalar subquery of the number Number, which counts from 1, in the
// Subqueries of the statement, whose opening parenthesis is at Offset in the text. [Format] does not
// print the offset.
type Subquery struct {
	Number int
	Offset int
}

func (*CreateTable) plan() {}
func (*DropTable) plan()   {}
func (*CreateIndex) plan() {}
func (*DropIndex) plan()   {}
func (*Insert) plan()      {}
func (*Update) plan()      {}
func (*Delete) plan()      {}
func (*Query) plan()       {}
func (*Scan) node()        {}
func (*Filter) node()      {}
func (*Order) node()       {}
func (*Limit) node()       {}
func (*Values) node()      {}
func (*Project) node()     {}
func (*Integer) expr()     {}
func (*Real) expr()        {}
func (*Text) expr()        {}
func (*Blob) expr()        {}
func (*Null) expr()        {}
func (*Parameter) expr()   {}
func (*Subquery) expr()    {}

// NewPlan of the statement s, which [stmt.Check] gives. It panics for a statement of another type. A
// column that is not the primary key and does not say NOT NULL is Nullable. The primary key is NOT
// NULL also when it does not say so, as in SQLite's WITHOUT ROWID tables, so a key with NOT NULL and
// one without have the same plan. A column with DEFAULT has the literal of its DEFAULT as its
// Default. A select, an update, and a delete with WHERE read their rows through a [*Filter] of the
// conditions, in the order of the text. A select with ORDER BY has an [*Order] of its columns over
// that, and one with LIMIT a [*Limit] over that, as [Query] says. An insert, an update, and a delete
// with RETURNING have its [*Returning]. A select, an update, and a delete with scalar subqueries in
// their WHERE have the [*Query] of each in their Subqueries, and a [*Subquery] as the value of the
// condition that compares a column with it.
func NewPlan(s stmt.Statement) Plan {
	switch s := s.(type) {
	case *stmt.CreateTable:
		p := &CreateTable{Table: s.Table, IfNotExists: s.IfNotExists, PrimaryKey: []int{s.Key()}, SQL: s.SQL}
		for _, c := range s.Columns {
			column := ColumnDef{Name: c.Name, Type: columnType(c.Type), Nullable: !c.PrimaryKey && !c.NotNull}
			if c.Default.Value != nil {
				column.Default = term(stmt.Term{Value: c.Default.Value})
			}
			p.Columns = append(p.Columns, column)
		}
		return p
	case *stmt.DropTable:
		return &DropTable{Table: s.Table, IfExists: s.IfExists}
	case *stmt.CreateIndex:
		return &CreateIndex{Index: s.Index, IfNotExists: s.IfNotExists, Table: s.Table, Columns: s.Columns, SQL: s.SQL}
	case *stmt.DropIndex:
		return &DropIndex{Index: s.Index, IfExists: s.IfExists}
	case *stmt.Insert:
		row := make([]Expr, len(s.Values))
		for i, t := range s.Values {
			row[i] = term(t)
		}
		return &Insert{Table: s.Table, Columns: s.Columns, Source: &Values{Rows: [][]Expr{row}}, Returning: returningOf(s.Returning)}
	case *stmt.Update:
		p := &Update{Table: s.Table, Source: rowsOf(s.Table, s.Where), Returning: returningOf(s.Returning), Subqueries: subqueriesOf(s.Subqueries)}
		for _, a := range s.Set {
			p.Set = append(p.Set, Assignment{Column: a.Column, Value: term(a.Value)})
		}
		return p
	case *stmt.Delete:
		return &Delete{Table: s.Table, Source: rowsOf(s.Table, s.Where), Returning: returningOf(s.Returning), Subqueries: subqueriesOf(s.Subqueries)}
	case *stmt.Select:
		q := queryOf(s)
		q.Subqueries = subqueriesOf(s.Subqueries)
		return q
	}
	panic(fmt.Sprintf("engine: a statement of type %T", s))
}

// queryOf the select s, without its subqueries.
func queryOf(s *stmt.Select) *Query {
	source := rowsOf(s.Table, s.Where)
	if s.OrderBy != nil {
		o := &Order{Source: source}
		for _, c := range s.OrderBy {
			o.Columns = append(o.Columns, OrderColumn{Column: c.Name, Descending: c.Descending})
		}
		source = o
	}
	if s.Limited() {
		source = &Limit{Source: source, Count: term(s.Limit)}
	}
	return &Query{Root: &Project{Source: source, Star: s.Columns == nil, Columns: s.Columns}}
}

// subqueriesOf a statement whose subqueries are ss: the [*Query] of each, in their order, or nil if
// it has none.
func subqueriesOf(ss []stmt.Select) []*Query {
	var qs []*Query
	for i := range ss {
		if ss[i].Subqueries != nil {
			panic("engine: a subquery with subqueries")
		}
		qs = append(qs, queryOf(&ss[i]))
	}
	return qs
}

// returningOf the RETURNING clause r: its [*Returning], or nil for a statement without one.
func returningOf(r stmt.Returning) *Returning {
	if !r.Present() {
		return nil
	}
	return &Returning{Star: r.Columns == nil, Columns: r.Columns}
}

// rowsOf the table that the conditions of a WHERE hold for: a [*Scan] of the table, or a [*Filter]
// of the conditions over it, if where is not nil.
func rowsOf(table stmt.Name, where []stmt.Condition) Node {
	var source Node = &Scan{Table: table}
	if where == nil {
		return source
	}
	f := &Filter{Source: source}
	for _, c := range where {
		condition := Condition{Column: c.Column, Operator: c.Operator, ValueFirst: c.ValueFirst}
		if c.Operator != stmt.OpIsNull && c.Operator != stmt.OpIsNotNull {
			condition.Value = term(c.Value)
		}
		f.Conditions = append(f.Conditions, condition)
	}
	return f
}

// term of a row of an insert, of an assignment, of a condition, or of a LIMIT, as an expression: its
// literal, NULL among them, its parameter, or its subquery.
func term(t stmt.Term) Expr {
	if t.Subquery > 0 {
		return &Subquery{Number: t.Subquery, Offset: t.Span.Start}
	}
	switch v := t.Value.(type) {
	case stmt.Integer:
		return &Integer{Value: int64(v)}
	case stmt.Real:
		return &Real{Value: float64(v)}
	case stmt.Text:
		return &Text{Value: string(v)}
	case stmt.Blob:
		return &Blob{Value: []byte(v)}
	case stmt.Null:
		return &Null{}
	case nil:
		return &Parameter{Number: t.Parameter}
	}
	panic(fmt.Sprintf("engine: a value of type %T", t.Value))
}

// Format the plan p as text: a line for each statement and node, with the nodes that a node reads
// from indented under it, and every name as the statement writes it. A column of a table that takes
// NULL has NULL after its type, and every other column is NOT NULL; a column with a default has
// DEFAULT and its literal after that.
func Format(p Plan) string {
	var b strings.Builder
	switch p := p.(type) {
	case *CreateTable:
		fmt.Fprint(&b, "create table ", ifClause(p.IfNotExists, "if not exists "), p.Table.Raw, " (")
		for i, c := range p.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(&b, c.Name.Raw, " ", typeName(c.Type))
			if c.Nullable {
				b.WriteString(" NULL")
			}
			if c.Default != nil {
				fmt.Fprint(&b, " DEFAULT ", formatExpr(c.Default))
			}
		}
		b.WriteString(", primary key (")
		for i, position := range p.PrimaryKey {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(p.Columns[position].Name.Raw)
		}
		b.WriteString("))\n")
	case *DropTable:
		fmt.Fprint(&b, "drop table ", ifClause(p.IfExists, "if exists "), p.Table.Raw, "\n")
	case *CreateIndex:
		fmt.Fprint(&b, "create index ", ifClause(p.IfNotExists, "if not exists "), p.Index.Raw, " on ", p.Table.Raw, " (")
		for i, c := range p.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(c.Name.Raw)
			if c.Descending {
				b.WriteString(" desc")
			}
		}
		b.WriteString(")\n")
	case *DropIndex:
		fmt.Fprint(&b, "drop index ", ifClause(p.IfExists, "if exists "), p.Index.Raw, "\n")
	case *Insert:
		fmt.Fprint(&b, "insert into ", p.Table.Raw)
		if p.Columns != nil {
			fmt.Fprint(&b, " (", names(p.Columns), ")")
		}
		fmt.Fprint(&b, formatReturning(p.Returning), "\n")
		formatNode(&b, p.Source, 1)
	case *Update:
		fmt.Fprint(&b, "update ", p.Table.Raw, " set ")
		for i, a := range p.Set {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(&b, a.Column.Raw, " = ", formatExpr(a.Value))
		}
		fmt.Fprint(&b, formatReturning(p.Returning), "\n")
		formatNode(&b, p.Source, 1)
		formatSubqueries(&b, p.Subqueries)
	case *Delete:
		fmt.Fprint(&b, "delete from ", p.Table.Raw, formatReturning(p.Returning), "\n")
		formatNode(&b, p.Source, 1)
		formatSubqueries(&b, p.Subqueries)
	case *Query:
		b.WriteString("query\n")
		formatNode(&b, p.Root, 1)
		formatSubqueries(&b, p.Subqueries)
	default:
		panic(fmt.Sprintf("engine: a plan of type %T", p))
	}
	return b.String()
}

func formatNode(b *strings.Builder, n Node, depth int) {
	b.WriteString(strings.Repeat("  ", depth))
	switch n := n.(type) {
	case *Scan:
		fmt.Fprint(b, "scan ", n.Table.Raw, "\n")
	case *Values:
		b.WriteString("values")
		for i, row := range n.Rows {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString(" (")
			for j, e := range row {
				if j > 0 {
					b.WriteString(", ")
				}
				b.WriteString(formatExpr(e))
			}
			b.WriteString(")")
		}
		b.WriteString("\n")
	case *Filter:
		b.WriteString("filter")
		for i, c := range n.Conditions {
			if i > 0 {
				b.WriteString(" and")
			}
			fmt.Fprint(b, " ", c.Column.Raw, " ", c.Operator)
			if c.Value != nil {
				fmt.Fprint(b, " ", formatExpr(c.Value))
			}
		}
		b.WriteString("\n")
		formatNode(b, n.Source, depth+1)
	case *Order:
		b.WriteString("order by")
		for i, c := range n.Columns {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprint(b, " ", c.Column.Raw)
			if c.Descending {
				b.WriteString(" desc")
			}
		}
		b.WriteString("\n")
		formatNode(b, n.Source, depth+1)
	case *Limit:
		fmt.Fprint(b, "limit ", formatExpr(n.Count), "\n")
		formatNode(b, n.Source, depth+1)
	case *Project:
		b.WriteString("project")
		switch {
		case n.Star:
			b.WriteString(" *")
		case len(n.Columns) > 0:
			b.WriteString(" " + names(n.Columns))
		}
		b.WriteString("\n")
		formatNode(b, n.Source, depth+1)
	default:
		panic(fmt.Sprintf("engine: a node of type %T", n))
	}
}

// formatSubqueries of a statement: a line "subquery" and its number for each, in their order, with
// the nodes of its query indented under it.
func formatSubqueries(b *strings.Builder, qs []*Query) {
	for i, q := range qs {
		fmt.Fprint(b, "  subquery ", i+1, "\n")
		formatNode(b, q.Root, 2)
	}
}

// formatReturning of an insert, an update, or a delete: " returning" and its columns, or "" for a
// statement without RETURNING.
func formatReturning(r *Returning) string {
	switch {
	case r == nil:
		return ""
	case r.Star:
		return " returning *"
	}
	return " returning " + names(r.Columns)
}

// names ns as the statement writes them, with a comma between two.
func names(ns []stmt.Name) string {
	raw := make([]string, len(ns))
	for i, n := range ns {
		raw[i] = n.Raw
	}
	return strings.Join(raw, ", ")
}

// ifClause of a statement that says it, which [Format] prints, or "".
func ifClause(says bool, clause string) string {
	if says {
		return clause
	}
	return ""
}

func formatExpr(e Expr) string {
	switch e := e.(type) {
	case *Integer:
		return fmt.Sprint(e.Value)
	case *Real:
		// The shortest digits that give the value, with a point or an exponent, so that the text
		// reads as a real: 1.0, -0.0, 1.5, 1e+300.
		text := strconv.FormatFloat(e.Value, 'g', -1, 64)
		if !strings.ContainsAny(text, ".e") {
			text += ".0"
		}
		return text
	case *Text:
		return "'" + strings.ReplaceAll(e.Value, "'", "''") + "'"
	case *Blob:
		return "x'" + hex.EncodeToString(e.Value) + "'"
	case *Null:
		return "null"
	case *Parameter:
		return fmt.Sprint("?", e.Number)
	case *Subquery:
		return fmt.Sprint("(subquery ", e.Number, ")")
	}
	panic(fmt.Sprintf("engine: an expression of type %T", e))
}

// typeName of a column's type, as SQLite gives it for the declared type of a column of a strict
// table.
func typeName(t s4qlpb.Type) string {
	switch t {
	case s4qlpb.Type_TYPE_INT:
		return "INT"
	case s4qlpb.Type_TYPE_INTEGER:
		return "INTEGER"
	case s4qlpb.Type_TYPE_REAL:
		return "REAL"
	case s4qlpb.Type_TYPE_TEXT:
		return "TEXT"
	case s4qlpb.Type_TYPE_BLOB:
		return "BLOB"
	}
	panic(fmt.Sprintf("engine: a column of type %v", t))
}

// columnType of the commit format for the type t of a column as the statement declares it. INT and
// INTEGER stay apart, so that replay declares the column as it was declared.
func columnType(t stmt.Type) s4qlpb.Type {
	switch t {
	case stmt.TypeInt:
		return s4qlpb.Type_TYPE_INT
	case stmt.TypeInteger:
		return s4qlpb.Type_TYPE_INTEGER
	case stmt.TypeReal:
		return s4qlpb.Type_TYPE_REAL
	case stmt.TypeText:
		return s4qlpb.Type_TYPE_TEXT
	case stmt.TypeBlob:
		return s4qlpb.Type_TYPE_BLOB
	}
	panic(fmt.Sprintf("engine: a column of type %v", t))
}
