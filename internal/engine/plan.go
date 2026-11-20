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
// A plan is a [*CreateTable], a [*DropTable], an [*Insert], or a [*Query]. An insert and a query
// read their rows from a tree of [Node]s.
type Plan interface {
	plan()
}

// Node of a plan that gives rows: a [*Scan], a [*Values], or a [*Project].
type Node interface {
	node()
}

// Expr is an expression that gives a value: an [*Integer], a [*Real], a [*Text], a [*Blob], a
// [*Null], or a [*Parameter].
type Expr interface {
	expr()
}

// CreateTable creates a table, which no table of its name may exist for.
type CreateTable struct {
	Table   stmt.Name
	Columns []ColumnDef
	// PrimaryKey holds the positions in Columns of the columns of the primary key, in key order.
	PrimaryKey []int
}

// ColumnDef is a column of a [CreateTable]. A column is NOT NULL unless it is Nullable, which a
// column of the primary key never is.
type ColumnDef struct {
	Name     stmt.Name
	Type     s4qlpb.Type
	Nullable bool
}

// DropTable drops a table, which must exist.
type DropTable struct {
	Table stmt.Name
}

// Insert inserts the rows of Source into a table, each as a new row. Source gives a value for each
// of Columns, in their order, or, if Columns is nil, for each column of the table, in the order of
// the table.
type Insert struct {
	Table   stmt.Name
	Columns []stmt.Name
	Source  Node
}

// Query gives the rows of Root.
type Query struct {
	Root Node
}

// Scan gives every row of a table, in the order of its primary key.
type Scan struct {
	Table stmt.Name
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

func (*CreateTable) plan() {}
func (*DropTable) plan()   {}
func (*Insert) plan()      {}
func (*Query) plan()       {}
func (*Scan) node()        {}
func (*Values) node()      {}
func (*Project) node()     {}
func (*Integer) expr()     {}
func (*Real) expr()        {}
func (*Text) expr()        {}
func (*Blob) expr()        {}
func (*Null) expr()        {}
func (*Parameter) expr()   {}

// NewPlan of the statement s, which [stmt.Check] gives. It panics for a statement of another type. A
// column that is not the primary key and does not say NOT NULL is Nullable. The primary key is NOT
// NULL also when it does not say so, as in SQLite's WITHOUT ROWID tables, so a key with NOT NULL and
// one without have the same plan.
func NewPlan(s stmt.Statement) Plan {
	switch s := s.(type) {
	case *stmt.CreateTable:
		p := &CreateTable{Table: s.Table, PrimaryKey: []int{s.Key()}}
		for _, c := range s.Columns {
			p.Columns = append(p.Columns, ColumnDef{Name: c.Name, Type: columnType(c.Type), Nullable: !c.PrimaryKey && !c.NotNull})
		}
		return p
	case *stmt.DropTable:
		return &DropTable{Table: s.Table}
	case *stmt.Insert:
		row := make([]Expr, len(s.Values))
		for i, t := range s.Values {
			row[i] = term(t)
		}
		return &Insert{Table: s.Table, Columns: s.Columns, Source: &Values{Rows: [][]Expr{row}}}
	case *stmt.Select:
		return &Query{Root: &Project{Source: &Scan{Table: s.Table}, Star: s.Columns == nil, Columns: s.Columns}}
	}
	panic(fmt.Sprintf("engine: a statement of type %T", s))
}

// term of a row of an insert, as an expression: its literal, NULL among them, or its parameter.
func term(t stmt.Term) Expr {
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
// NULL has NULL after its type, and every other column is NOT NULL.
func Format(p Plan) string {
	var b strings.Builder
	switch p := p.(type) {
	case *CreateTable:
		fmt.Fprint(&b, "create table ", p.Table.Raw, " (")
		for i, c := range p.Columns {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprint(&b, c.Name.Raw, " ", typeName(c.Type))
			if c.Nullable {
				b.WriteString(" NULL")
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
		fmt.Fprint(&b, "drop table ", p.Table.Raw, "\n")
	case *Insert:
		fmt.Fprint(&b, "insert into ", p.Table.Raw)
		if p.Columns != nil {
			fmt.Fprint(&b, " (", names(p.Columns), ")")
		}
		b.WriteString("\n")
		formatNode(&b, p.Source, 1)
	case *Query:
		b.WriteString("query\n")
		formatNode(&b, p.Root, 1)
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

// names ns as the statement writes them, with a comma between two.
func names(ns []stmt.Name) string {
	raw := make([]string, len(ns))
	for i, n := range ns {
		raw[i] = n.Raw
	}
	return strings.Join(raw, ", ")
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
