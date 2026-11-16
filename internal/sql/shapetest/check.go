package shapetest

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// Way in which SQLite compared a statement with its printed text.
type Way int

const (
	// Bytecode means that EXPLAIN gave the same program for both.
	Bytecode Way = iota + 1
	// Probes means that the statement changes the schema, and that the schema after it, and the
	// programs of statements that use what it created, were the same for both.
	Probes
	// SameError means that both failed to prepare with the same error, which no schema could avoid.
	SameError
	// Skipped means that the statement could not be compared, for the reason given.
	Skipped
)

func (w Way) String() string {
	switch w {
	case Bytecode:
		return "bytecode"
	case Probes:
		return "probes"
	case SameError:
		return "same error"
	case Skipped:
		return "skipped"
	default:
		return fmt.Sprint("Way(", int(w), ")")
	}
}

// Result of the check of one statement.
type Result struct {
	// SQL is the text checked, and Printed its tree as [Print] writes it.
	SQL, Printed string

	// Kind of the statement, such as "select" or "create table".
	Kind string

	// Way in which the statement was compared, and for [SameError] and [Skipped] the Reason.
	Way    Way
	Reason string

	// Exprs holds what was compared for each expression of the statement.
	Exprs []ExprResult

	// Fault is why the check failed, or "" if it passed.
	Fault string
}

// ExprResult of the comparison of one expression, on its own, over the table of values s4ql_v.
type ExprResult struct {
	// Forms of expression that the expression holds, such as "Binary +" or "Like NOT LIKE ESCAPE".
	Forms []string

	// Bytecode is true if EXPLAIN gave the same program for both texts.
	Bytecode bool

	// Values is the number of assignments of values to the column references for which both texts
	// gave the same value, of the same type, and Errors the number for which both gave the same
	// error.
	Values, Errors int

	// Skipped is why the values were not compared, or "".
	Skipped string

	// Duplicate is true if the same expression was compared before by the same [Checker], and it
	// was not compared again: the other fields are those of the first comparison.
	Duplicate bool
}

// Check the shape of the tree that the parser in internal/sql/parser gives for the first statement
// of src, against SQLite. The statement must parse in SQLite and in the parser. The tree is printed
// with [Print], and the check requires that:
//
//   - every token of the tree is printed, once;
//   - the printed text parses with the parser, and printing its tree gives the same text;
//   - SQLite parses the printed text too;
//   - SQLite treats both texts the same: EXPLAIN gives the same program, or for a statement that
//     changes the schema, the schema after it and the programs of statements that use it are the
//     same; and each expression on its own gives the same values, with the same types, for several
//     values of its column references, and the same program.
//
// Result.Fault says what differed. Where SQLite cannot compare, Result says why.
func Check(src string) Result {
	var c Checker
	defer c.Close()
	return c.Check(src)
}

// Checker checks many statements in turn, with one connection for the expressions, and compares
// each expression only once. It is not safe for concurrent use. The zero value is ready to use.
type Checker struct {
	conn *sqlite.Conn
	// seen holds the result of each expression compared, by its original text with the column
	// references replaced.
	seen map[string]seenExpr
}

// Close the connection of the checker.
func (c *Checker) Close() {
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
}

// Check is [Check], with the expressions compared only the first time they are seen.
func (c *Checker) Check(src string) Result {
	r := Result{SQL: src}
	stmt, _, err := parser.Parse(src)
	if err != nil || stmt == nil {
		r.Way, r.Reason = Skipped, "the parser does not parse it"
		return r
	}
	r.Kind = KindOf(stmt.Cmd)
	if err := checkPrinted(stmt); err != nil {
		r.Printed, r.Fault = Print(stmt), err.Error()
	} else {
		c.compare(&r, src, stmt, stmt)
	}
	if r.Fault != "" {
		// A fault counts only for a statement that SQLite parses; SQLite's verdict is asked only
		// then, since it costs as much as the rest of the check.
		if v := oracle.Judge(src); v.Kind != oracle.Parses {
			return Result{SQL: src, Way: Skipped, Reason: "SQLite does not say that it parses"}
		}
	}
	return r
}

// NotChecked reports whether the result is of a text that was not a statement to check: one that
// the parser does not parse, or one that SQLite does not say parses.
func (r Result) NotChecked() bool {
	return r.Kind == ""
}

// compare the statement orig of src with the tree shown, which is orig itself, or a tree that was
// changed on purpose.
func (c *Checker) compare(r *Result, src string, orig, shown *ast.Statement) {
	r.Printed = Print(shown)
	again, _, err := parser.Parse(r.Printed)
	var perr *parser.Error
	switch {
	case errors.As(err, &perr) && perr.Limit:
		// Every operation adds a parenthesis, and with it a level of nesting, so a deep original
		// can print deeper than the parser allows.
		r.Way, r.Reason = Skipped, deepReason
		return
	case err != nil:
		r.Fault = fmt.Sprintf("the printed text does not parse: %v", err)
		return
	case again == nil:
		r.Fault = "the printed text has no statement"
		return
	case Print(again) != r.Printed:
		r.Fault = fmt.Sprintf("printing the printed text gives another text: %q", Print(again))
		return
	}
	if w := oracle.Judge(r.Printed); w.Kind == oracle.NotSaid && w.Message == "Recursion limit" {
		r.Way, r.Reason = Skipped, deepReason
		return
	} else if w.Kind != oracle.Parses {
		// An error of SQLite's grammar actions, which comes after the parse in the original, can come
		// at a parenthesis of the printed text, and stop the parse there: that is the same error.
		if v := oracle.Judge(src); w.Kind != oracle.NotSaid || w.Reason != oracle.StoppedEarly || w.Message != v.Message {
			r.Fault = fmt.Sprintf("SQLite does not parse the printed text: %v %v %q", w.Kind, w.Reason, w.Message)
			return
		}
	}
	r.Way, r.Reason, r.Fault = c.statement(src, orig, shown)
	if r.Fault != "" {
		return
	}
	r.Exprs, r.Fault = c.exprs(src, orig, shown)
}

const deepReason = "the printed text, with a parenthesis for every operation, is nested deeper than the limit of the parser"

// checkPrinted checks that [Print] writes every token of the tree once, or leaves it out where its
// rules say so.
func checkPrinted(stmt *ast.Statement) error {
	var p printer
	var got []ast.Token
	p.printed = func(t ast.Token) { got = append(got, t) }
	p.node(stmt)
	slices.SortFunc(got, func(a, b ast.Token) int { return a.Offset - b.Offset })
	want := ast.AllTokens(stmt)
	if !slices.Equal(got, want) {
		for i := range max(len(got), len(want)) {
			switch {
			case i >= len(got):
				return fmt.Errorf("the token %q at %v is not printed", want[i].Text, want[i].Offset)
			case i >= len(want) || got[i] != want[i]:
				return fmt.Errorf("the token %q at %v is printed more than once, or not at all", got[i].Text, got[i].Offset)
			}
		}
	}
	return nil
}

// KindOf a command, for reports: "select", "insert", "create table", "alter table add column",
// and so on.
func KindOf(c ast.Cmd) string {
	switch c := c.(type) {
	case *ast.SelectCmd:
		if _, ok := c.Select.First.(*ast.Values); ok && len(c.Select.Compounds) == 0 {
			return "values"
		}
		return "select"
	case *ast.Insert:
		if strings.EqualFold(c.Insert.Text, "replace") {
			return "replace"
		}
		return "insert"
	case *ast.Update:
		return "update"
	case *ast.Delete:
		return "delete"
	case *ast.CreateTable:
		if c.As.Present() {
			return "create table as"
		}
		return "create table"
	case *ast.CreateIndex:
		return "create index"
	case *ast.CreateView:
		return "create view"
	case *ast.CreateTrigger:
		return "create trigger"
	case *ast.CreateVirtualTable:
		return "create virtual table"
	case *ast.AlterTable:
		switch c.Action.(type) {
		case *ast.RenameTable:
			return "alter table rename"
		case *ast.AddColumn:
			return "alter table add column"
		case *ast.DropColumn:
			return "alter table drop column"
		case *ast.RenameColumn:
			return "alter table rename column"
		default:
			return "alter table other"
		}
	case *ast.Drop:
		return "drop " + strings.ToLower(c.Object.Text)
	case *ast.Pragma:
		return "pragma"
	case *ast.Begin, *ast.Commit, *ast.Rollback, *ast.Savepoint, *ast.Release:
		return "transaction"
	case *ast.Attach:
		return "attach"
	case *ast.Detach:
		return "detach"
	case *ast.Vacuum:
		return "vacuum"
	case *ast.Reindex:
		return "reindex"
	case *ast.Analyze:
		return "analyze"
	default:
		return fmt.Sprintf("%T", c)
	}
}

// side of a comparison: the original text, or the printed tree.
type side struct {
	// text of a node of the side's tree.
	text func(ast.Node) string
	// masks replaces a text that SQLite copies from the statement into a program, such as that of a
	// CHECK constraint in the message of its Halt, by a name that is the same on both sides.
	masks map[string]string
}

// maxFixes of the schema for one statement.
const maxFixes = 40

// statement compares orig with the printed tree shown, by its kind.
func (c *Checker) statement(src string, orig, shown *ast.Statement) (Way, string, string) {
	if _, ok := orig.Cmd.(*ast.CreateVirtualTable); ok {
		return Skipped, "the arguments of a virtual table are tokens, without structure", ""
	}
	if typeComment(src, orig) {
		return Skipped, typeCommentReason, ""
	}
	o := side{text: func(n ast.Node) string { return src[n.Bounds().Start:n.Bounds().End] }, masks: map[string]string{}}
	s := side{text: Print, masks: map[string]string{}}
	checksOf(orig, o, func(ch *ast.CheckConstraint) string { return src[ch.LParen.End():ch.RParen.Offset] })
	checksOf(shown, s, func(ch *ast.CheckConstraint) string { return Print(ch.X) })

	sch := guess(orig, src)
	var want []string
	var err error
	for i := range maxFixes {
		var conn *sqlite.Conn
		conn, err = sch.open()
		if err != nil {
			return Skipped, "the schema it needs cannot be built: " + err.Error(), ""
		}
		want, err = observe(conn, orig.Cmd, o)
		_ = conn.Close()
		// The observation is always of the schema as it is: the loop ends before a fix.
		if err == nil || i == maxFixes-1 || !sch.fix(message(err)) {
			break
		}
	}
	conn, err2 := sch.open()
	if err2 != nil {
		return Skipped, "the schema it needs cannot be built: " + err2.Error(), ""
	}
	got, _ := observe(conn, shown.Cmd, s)
	_ = conn.Close()
	if i, ok := differ(want, got); ok {
		if name := textName(src, orig, got); name != "" {
			return Skipped, "a column is named by the text of its expression, which the printer changes: " + name, ""
		}
		return 0, "", fmt.Sprintf("SQLite treats the printed text differently: at %d, the original gives %q and the printed text %q", i, at(want, i), at(got, i))
	}
	if err != nil {
		return SameError, message(err), ""
	}
	switch orig.Cmd.(type) {
	case *ast.CreateTable, *ast.CreateIndex, *ast.CreateView, *ast.CreateTrigger, *ast.AlterTable:
		return Probes, "", ""
	}
	return Bytecode, "", ""
}

var reNoSuchColumn = regexp.MustCompile(`^error: no such column: (.+)$`)

// textName gives the name of a column that the observations of the printed text did not find, if
// an unaliased column of a SELECT in the original has that name. SQLite names such a column by
// the text of its expression, such as count(*), and the printed text writes the expression with
// other spaces and parentheses, so a reference to the name does not find it.
func textName(src string, orig *ast.Statement, obs []string) string {
	if len(obs) == 0 {
		return ""
	}
	m := reNoSuchColumn.FindStringSubmatch(obs[len(obs)-1])
	if m == nil {
		return ""
	}
	name := m[1]
	found := ""
	ast.Walk(orig, func(n ast.Node) bool {
		if rc, ok := n.(*ast.ResultColumn); ok && rc.Expr != nil && !rc.Alias.Present() {
			if _, isRef := rc.Expr.(*ast.ColumnRef); !isRef {
				text := src[rc.Expr.Bounds().Start:rc.Expr.Bounds().End]
				if name == text || strings.HasSuffix(name, "."+text) {
					found = text
				}
			}
		}
		return found == ""
	})
	return found
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "(nothing)"
}

// differ reports the index of the first difference of a and b.
func differ(a, b []string) (int, bool) {
	for i := range max(len(a), len(b)) {
		if i >= len(a) || i >= len(b) || a[i] != b[i] {
			return i, true
		}
	}
	return 0, false
}

const typeCommentReason = "a declared type holds a comment, which SQLite keeps as part of the type, and which is no token of the tree"

// typeComment reports whether a declared type in the tree from n has a comment between its tokens.
// SQLite keeps the text of a type from its first token to its last, comments too, and derives the
// affinity from it, so `a /*int*/ x` has the affinity INTEGER; the printer writes the tokens only.
func typeComment(src string, n ast.Node) bool {
	found := false
	ast.Walk(n, func(n ast.Node) bool {
		if t, ok := n.(*ast.TypeName); ok {
			if gap := src[t.Span.Start:t.Span.End]; strings.Contains(gap, "--") || strings.Contains(gap, "/*") {
				// A string or a quoted name can hold the same bytes, so look between the tokens.
				ts := ast.Tokens(t)
				for _, c := range ast.Children(t) {
					ts = append(ts, ast.AllTokens(c)...)
				}
				slices.SortFunc(ts, func(a, b ast.Token) int { return a.Offset - b.Offset })
				for i := 1; i < len(ts); i++ {
					if strings.TrimSpace(src[ts[i-1].End():ts[i].Offset]) != "" {
						found = true
					}
				}
			}
		}
		return !found
	})
	return found
}

// checksOf the statement: the text of each CHECK constraint, which SQLite puts into the message of
// its Halt when the constraint has no name. SQLite takes that text from between the parentheses of
// the CHECK, trims the spaces around it, and dequotes it if it starts with a quote, as
// sqlite3Dequote does; comments stay. inner gives that text, before the trim, for each side: from
// the source for the original, and the printed expression for the printed tree.
func checksOf(stmt *ast.Statement, sd side, inner func(*ast.CheckConstraint) string) {
	// Two constraints can have the same text on one side and not on the other, as `a| b` and `a|b`
	// print the same, so every such text becomes the same mask. A named CHECK keeps its name, so the
	// mask still tells a named constraint from an unnamed one.
	ast.Walk(stmt, func(n ast.Node) bool {
		if ch, ok := n.(*ast.CheckConstraint); ok {
			sd.masks[sqliteDequote(strings.TrimSpace(inner(ch)))] = "<check>"
		}
		return true
	})
}

func message(err error) string {
	if err == nil {
		return ""
	}
	var e *sqlite.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// observe what SQLite does with the command, on a connection with the schema it needs: the program
// that EXPLAIN gives, or for a command that changes the schema, the schema after it and the programs
// of statements that use it. An error ends the observations, as the last one, and is returned too.
func observe(conn *sqlite.Conn, cmd ast.Cmd, sd side) ([]string, error) {
	var obs []string
	fail := func(err error) ([]string, error) {
		return append(obs, "error: "+normalizeTypes(message(err))), err
	}
	text := sd.text(cmd)
	switch cmd := cmd.(type) {
	case *ast.CreateTable:
		if cmd.As.Present() {
			// The names of the columns come from the text of the expressions, so only the program of
			// the SELECT and the types are compared.
			p, err := explain(conn, sd.text(cmd.Select), sd)
			obs = append(obs, p...)
			if err != nil {
				return fail(err)
			}
		}
		if err := conn.Exec(text); err != nil {
			return fail(err)
		}
		p, err := tableProbes(conn, cmd.Name, sd, !cmd.As.Present())
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	case *ast.CreateIndex:
		if err := conn.Exec(text); err != nil {
			return fail(err)
		}
		p, err := tableProbes(conn, ast.QualifiedName{Schema: cmd.Name.Schema, Dot: cmd.Name.Dot, Name: cmd.Table}, sd, true)
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	case *ast.CreateView:
		if err := conn.Exec(text); err != nil {
			return fail(err)
		}
		p, err := explain(conn, "select * from "+qname(cmd.Name), sd)
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	case *ast.CreateTrigger:
		if err := conn.Exec(text); err != nil {
			return fail(err)
		}
		t := qname(cmd.Table)
		var fire string
		switch strings.ToLower(cmd.Event.Text) {
		case "insert":
			fire = "insert into " + t + " select * from " + t
		case "delete":
			fire = "delete from " + t
		default:
			cols, err := columns(conn, cmd.Table)
			if err != nil {
				return fail(err)
			}
			var sets []string
			for _, col := range cols {
				sets = append(sets, quote(col)+" = "+quote(col))
			}
			fire = "update " + t + " set " + strings.Join(sets, ", ")
		}
		p, err := explain(conn, fire, sd)
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	case *ast.AlterTable:
		if err := conn.Exec(text); err != nil {
			return fail(err)
		}
		name := cmd.Name
		if a, ok := cmd.Action.(*ast.RenameTable); ok {
			name.Name = a.Name
		}
		p, err := tableProbes(conn, name, sd, true)
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	default:
		p, err := explain(conn, text, sd)
		obs = append(obs, p...)
		if err != nil {
			return fail(err)
		}
	}
	return obs, nil
}

func qname(q ast.QualifiedName) string {
	if q.Schema.Present() {
		return q.Schema.Text + "." + q.Name.Text
	}
	return q.Name.Text
}

// pragma gives the text of PRAGMA name(arg) in the schema of q.
func pragma(q ast.QualifiedName, name, arg string) string {
	db := ""
	if q.Schema.Present() {
		db = q.Schema.Text + "."
	}
	return "pragma " + db + name + "(" + quote(arg) + ")"
}

// columns of the table q that a statement can set.
func columns(conn *sqlite.Conn, q ast.QualifiedName) ([]string, error) {
	rows, err := query(conn, pragma(q, "table_xinfo", dequote(q.Name.Text)))
	if err != nil {
		return nil, err
	}
	var cols []string
	for _, r := range rows {
		if r[6] == "integer:0" {
			cols = append(cols, strings.TrimPrefix(r[1], "text:"))
		}
	}
	return cols, nil
}

// tableProbes observe the table q: its columns, indexes, and foreign keys as the PRAGMAs give them,
// and the programs of an INSERT, an UPDATE of every column, and a DELETE, which hold its constraints,
// defaults, generated columns, and indexes. The default values are left out of the columns, because
// SQLite keeps them as text; their programs are compared. With names false the names of the columns
// are left out too, for CREATE TABLE ... AS, where they come from the text of the expressions.
func tableProbes(conn *sqlite.Conn, q ast.QualifiedName, sd side, names bool) ([]string, error) {
	var obs []string
	name := dequote(q.Name.Text)
	rows, err := query(conn, pragma(q, "table_xinfo", name))
	if err != nil {
		return obs, err
	}
	var cols []string
	for _, r := range rows {
		if r[6] == "integer:0" {
			cols = append(cols, strings.TrimPrefix(r[1], "text:"))
		}
		r[2] = typeText(r[2])
		r[4] = ""
		if !names {
			r[1] = ""
		}
		obs = append(obs, "column "+strings.Join(r, "|"))
	}
	rows, err = query(conn, pragma(q, "index_list", name))
	if err != nil {
		return obs, err
	}
	for _, r := range rows {
		obs = append(obs, "index "+strings.Join(r, "|"))
		idx, err := query(conn, pragma(q, "index_xinfo", strings.TrimPrefix(r[1], "text:")))
		if err != nil {
			return obs, err
		}
		for _, x := range idx {
			obs = append(obs, "index column "+strings.Join(x, "|"))
		}
	}
	rows, err = query(conn, pragma(q, "foreign_key_list", name))
	if err != nil {
		return obs, err
	}
	for _, r := range rows {
		obs = append(obs, "foreign key "+strings.Join(r, "|"))
	}
	rows, err = query(conn, pragma(q, "table_list", name))
	if err != nil {
		return obs, err
	}
	for _, r := range rows {
		obs = append(obs, "table "+strings.Join(r, "|"))
	}
	t := qname(q)
	probes := []string{"insert into " + t + " default values", "delete from " + t}
	if names && len(cols) > 0 {
		var sets, compares []string
		for _, col := range cols {
			sets = append(sets, quote(col)+" = "+quote(col))
			compares = append(compares, quote(col)+" = 'a'")
		}
		// The comparisons hold the affinity and the collation of each column.
		probes = append(probes, "update "+t+" set "+strings.Join(sets, ", "), "select "+strings.Join(compares, ", ")+" from "+t)
	}
	for _, probe := range probes {
		p, err := explain(conn, probe, sd)
		obs = append(obs, "probe "+probe)
		if err != nil {
			obs = append(obs, "error: "+message(err))
			continue
		}
		obs = append(obs, p...)
	}
	return obs, nil
}

var reDatatype = regexp.MustCompile(`^(unknown datatype for [^:]*: )(.*)$`)

// typeText of a declared type without its spaces. SQLite keeps a declared type as written, from its
// first token to its last, and the printer writes it with other spaces than the text may have. The
// spaces between the tokens of a type mean nothing else: the affinity of a type depends on the words
// it holds, and two words of a type that are written without a space between them are quoted.
func typeText(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// normalizeTypes in an error message that quotes a declared type, as [typeText] does.
func normalizeTypes(msg string) string {
	if m := reDatatype.FindStringSubmatch(msg); m != nil {
		return m[1] + typeText(m[2])
	}
	return msg
}

// query runs the statement and gives its rows, each value as its type and text.
func query(conn *sqlite.Conn, sql string) ([][]string, error) {
	stmt, err := conn.Prepare(sql)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stmt.Finalize() }()
	var rows [][]string
	for {
		ok, err := stmt.Step()
		if err != nil {
			return rows, err
		}
		if !ok {
			return rows, nil
		}
		row := make([]string, stmt.ColumnCount())
		for i := range row {
			row[i] = stmt.ColumnType(i).String() + ":" + stmt.ColumnText(i)
		}
		rows = append(rows, row)
	}
}

// explain gives the program of the statement as EXPLAIN shows it: each instruction as its opcode,
// its operands P1 to P5, and its comment. The address is left out, since the instructions are in
// order. Two operands are masked, because SQLite copies text of the statement into them: the P4 of
// Trace, which is the text of a step of a trigger, and the P4 of a Halt that is a CHECK constraint's
// text, which is replaced by the same name on both sides.
func explain(conn *sqlite.Conn, sql string, sd side) ([]string, error) {
	rows, err := query(conn, "explain "+sql)
	if err != nil {
		return nil, err
	}
	prog := make([]string, len(rows))
	for i, r := range rows {
		switch r[1] {
		case "text:Trace":
			r[5] = ""
		case "text:VOpen", "text:VCheck", "text:VFilter", "text:VUpdate", "text:VDestroy", "text:VBegin", "text:VCreate", "text:VInitIn":
			// The P4 of a virtual table is the address of its object in memory.
			if strings.HasPrefix(r[5], "text:vtab:") {
				r[5] = "text:vtab"
			}
		case "text:Halt":
			if m, ok := sd.masks[strings.TrimPrefix(r[5], "text:")]; ok {
				r[5] = m
			}
		}
		prog[i] = strings.Join(r[1:], "|")
	}
	return prog, nil
}

// topExprs of the tree: the expressions that are not inside another expression, in the order of a
// walk of the tree.
func topExprs(n ast.Node) []ast.Expr {
	var es []ast.Expr
	var walk func(n ast.Node, inExpr bool)
	walk = func(n ast.Node, inExpr bool) {
		e, isExpr := n.(ast.Expr)
		if isExpr && !inExpr {
			es = append(es, e)
		}
		for _, c := range ast.Children(n) {
			walk(c, isExpr || (inExpr && !isSelectBoundary(n)))
		}
	}
	walk(n, false)
	return es
}

// isSelectBoundary reports whether n starts a part of the tree whose expressions stand on their own
// again: a SELECT inside an expression. The clauses of a function call, such as FILTER and OVER, are
// part of the call.
func isSelectBoundary(n ast.Node) bool {
	switch n.(type) {
	case *ast.Select:
		return true
	}
	return false
}

// pool of values for the column references of an expression: of every type, and texts that look
// like numbers, patterns, and JSON.
var pool = []string{"0", "1", "2", "3", "7", "-1", "NULL", "'a'", "'B'", "''", "2.5", "x'01'", "'%'", "'a_c'", "'{\"a\":[1,2]}'", "'12'", "1e300"}

// exprs compares each expression of orig, on its own, with the expression at the same place in
// shown, with its column references replaced by values.
func (c *Checker) exprs(src string, orig, shown *ast.Statement) ([]ExprResult, string) {
	os, ss := topExprs(orig), topExprs(shown)
	if len(os) != len(ss) {
		return []ExprResult{{Skipped: "the trees hold different numbers of expressions"}}, ""
	}
	var results []ExprResult
	for i, eo := range os {
		r, fault := c.expr(src, eo, ss[i])
		results = append(results, r)
		if fault != "" {
			return results, fault
		}
	}
	return results, ""
}

func (c *Checker) expr(src string, eo, es ast.Expr) (ExprResult, string) {
	r := ExprResult{Forms: forms(eo)}
	if typeComment(src, eo) {
		r.Skipped = typeCommentReason
		return r, ""
	}
	if reason := cannotEvaluate(eo); reason != "" {
		r.Skipped = reason
		return r, ""
	}
	// The column references, in the order of the text, named by their tokens, become the columns of
	// a table of one row, s4ql_v, which holds the values. They stay column references, which SQLite
	// cannot fold as it folds literals, and keep the affinity and collation of a column.
	refs := columnRefs(eo)
	index := map[string]int{}
	for _, ref := range refs {
		if _, ok := index[refName(ref)]; !ok {
			index[refName(ref)] = len(index)
		}
	}
	if len(index) > valueColumns {
		r.Skipped = "it has more column references than the table of values has columns"
		return r, ""
	}
	column := func(ref *ast.ColumnRef) string {
		i, ok := index[refName(ref)]
		if !ok {
			return ""
		}
		return fmt.Sprint("s4ql_v.c", i)
	}
	var b strings.Builder
	start := eo.Bounds().Start
	for _, ref := range refs {
		b.WriteString(src[start:ref.Span.Start])
		b.WriteString(" " + column(ref) + " ")
		start = ref.Span.End
	}
	b.WriteString(src[start:eo.Bounds().End])
	ot := b.String()
	if prev, ok := c.seen[ot]; ok {
		prev.r.Duplicate = true
		return prev.r, prev.fault
	}
	p := printer{column: column}
	p.expr(es)
	pt := p.b.String()
	r, fault := c.compareExpr(r, eo, ot, pt, len(index))
	if c.seen == nil {
		c.seen = map[string]seenExpr{}
	}
	c.seen[ot] = seenExpr{r, fault}
	return r, fault
}

// seenExpr is the result of the comparison of an expression, and its fault.
type seenExpr struct {
	r     ExprResult
	fault string
}

// compareExpr compares the original expression eo, as the text ot, with the printed text pt, where
// the n column references are columns of s4ql_v.
func (c *Checker) compareExpr(r ExprResult, eo ast.Expr, ot, pt string, n int) (ExprResult, string) {
	if err := c.values(n, 0); err != nil {
		panic(err)
	}
	want, werr := explain(c.conn, "select "+ot+" from s4ql_v", side{})
	got, gerr := explain(c.conn, "select "+pt+" from s4ql_v", side{})
	if i, ok := differ(want, got); ok || message(werr) != message(gerr) {
		return r, fmt.Sprintf("SQLite gives another program for the printed expression %q than for %q: at %d, %q and %q, errors %v and %v",
			pt, ot, i, at(want, i), at(got, i), werr, gerr)
	}
	r.Bytecode = werr == nil
	if reason := nondeterministic(eo); reason != "" {
		r.Skipped = reason
		return r, ""
	}
	assignments := 3
	if n == 0 {
		assignments = 1
	}
	for k := range assignments {
		if err := c.values(n, k); err != nil {
			panic(err)
		}
		want := evaluate(c.conn, ot)
		got := evaluate(c.conn, pt)
		if want != got {
			return r, fmt.Sprintf("SQLite gives another value for the printed expression %q than for %q, with the values %v: %s and %s",
				pt, ot, valueList(n, k), got, want)
		}
		if strings.HasPrefix(want, "error: ") {
			r.Errors++
		} else {
			r.Values++
		}
	}
	return r, ""
}

// valueColumns is the number of columns of the table of values, s4ql_v.
const valueColumns = 64

// valueList of the k-th assignment of values to n columns.
func valueList(n, k int) []string {
	vs := make([]string, n)
	for i := range vs {
		vs[i] = pool[(i*5+k*11+i*k)%len(pool)]
	}
	return vs
}

// values sets the first n columns of the one row of the table s4ql_v to the k-th assignment of
// values. The table is made on the first call, with [valueColumns] columns of the types of
// [columnTypes] in turn.
func (c *Checker) values(n, k int) error {
	if c.conn == nil {
		conn, err := sqlite.Open(":memory:")
		if err != nil {
			return err
		}
		if err := conn.DenyProcessPragmas(); err != nil {
			_ = conn.Close()
			return err
		}
		cols := make([]string, valueColumns)
		for i := range cols {
			cols[i] = fmt.Sprint("c", i, " ", columnTypes[i%len(columnTypes)])
		}
		for _, stmt := range []string{
			"create table s4ql_v (" + strings.Join(cols, ", ") + ")",
			"insert into s4ql_v default values",
		} {
			if err := conn.Exec(stmt); err != nil {
				_ = conn.Close()
				return err
			}
		}
		c.conn = conn
	}
	if n == 0 {
		return nil
	}
	sets := make([]string, n)
	for i, v := range valueList(n, k) {
		sets[i] = fmt.Sprint("c", i, " = ", v)
	}
	return c.conn.Exec("update s4ql_v set " + strings.Join(sets, ", "))
}

// evaluate the expression on the row of s4ql_v, and give its type and value as quote() writes it,
// which is exact for every type, or the error. Comparisons of the value show its affinity and
// collation, which a SELECT passes on from an expression to its column.
func evaluate(conn *sqlite.Conn, e string) string {
	rows, err := query(conn, "select typeof(v), quote(v), v = 'A', v < 'b', v = 1, v = '1' from (select "+e+" as v from s4ql_v)")
	if err != nil {
		return "error: " + message(err)
	}
	if len(rows) != 1 {
		return fmt.Sprint(len(rows), " rows")
	}
	return strings.Join(rows[0], " ")
}

func refName(ref *ast.ColumnRef) string {
	return ref.Schema.Text + ref.Dot1.Text + ref.Table.Text + ref.Dot2.Text + ref.Column.Text
}

// columnRefs of e that are replaced by values, sorted by offset: all but true and false, which
// SQLite reads as constants.
func columnRefs(e ast.Expr) []*ast.ColumnRef {
	var refs []*ast.ColumnRef
	ast.Walk(e, func(n ast.Node) bool {
		if ref, ok := n.(*ast.ColumnRef); ok {
			if !ref.Table.Present() && (strings.EqualFold(ref.Column.Text, "true") || strings.EqualFold(ref.Column.Text, "false")) {
				return true
			}
			refs = append(refs, ref)
		}
		return true
	})
	slices.SortFunc(refs, func(a, b *ast.ColumnRef) int { return a.Span.Start - b.Span.Start })
	return refs
}

// cannotEvaluate reports why e cannot be evaluated on its own, or "".
func cannotEvaluate(e ast.Expr) string {
	reason := ""
	ast.Walk(e, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.TableSource, *ast.TableFunction, *ast.InTable:
			reason = "it names a table"
		}
		return reason == ""
	})
	return reason
}

// nondeterministic reports why the values of e may differ from one evaluation to the next, or "".
func nondeterministic(e ast.Expr) string {
	reason := ""
	ast.Walk(e, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Literal:
			if n.Value.Kind == token.CTimeKw {
				reason = "it reads the clock"
			}
		case *ast.FuncCall:
			if nondeterministicFunctions[strings.ToLower(dequote(n.Name.Text))] {
				reason = "it calls " + strings.ToLower(n.Name.Text)
			}
		}
		return reason == ""
	})
	return reason
}

// nondeterministicFunctions of the binding, whose value can differ between two calls with the same
// arguments: those that pragma_function_list does not mark as deterministic, and those of date and
// time, which read the clock for 'now'.
var nondeterministicFunctions = map[string]bool{
	"random": true, "randomblob": true, "changes": true, "total_changes": true, "last_insert_rowid": true,
	"date": true, "time": true, "datetime": true, "julianday": true, "strftime": true, "unixepoch": true,
	"timediff": true, "sqlite_offset": true, "current_date": true, "current_time": true, "current_timestamp": true,
}

// forms of expression in e, by node type and the keywords and operator that tell the form.
func forms(e ast.Expr) []string {
	var fs []string
	ast.Walk(e, func(n ast.Node) bool {
		if f := form(n); f != "" && !slices.Contains(fs, f) {
			fs = append(fs, f)
		}
		return true
	})
	slices.Sort(fs)
	return fs
}

func form(n ast.Node) string {
	words := func(name string, ts ...ast.Token) string {
		var b strings.Builder
		b.WriteString(name)
		for _, t := range ts {
			if t.Present() {
				b.WriteString(" " + strings.ToUpper(t.Text))
			}
		}
		return b.String()
	}
	switch n := n.(type) {
	case *ast.Unary:
		return words("Unary", n.Op)
	case *ast.Binary:
		return words("Binary", n.Op)
	case *ast.Is:
		return words("Is", n.Is, n.Not, n.Distinct, n.From)
	case *ast.NullTest:
		return words("NullTest", n.Op, n.Null)
	case *ast.Like:
		return words("Like", n.Not, n.Op, n.Escape)
	case *ast.Between:
		return words("Between", n.Not, n.Between)
	case *ast.InList:
		return words("InList", n.Not, n.In)
	case *ast.InSelect:
		return words("InSelect", n.Not, n.In)
	case *ast.InTable:
		if n.LParen.Present() {
			return words("InTable function", n.Not, n.In)
		}
		return words("InTable", n.Not, n.In)
	case *ast.Collate:
		return "Collate"
	case *ast.Case:
		if n.Operand != nil {
			return "Case with operand"
		}
		return "Case"
	case *ast.Cast:
		return "Cast"
	case *ast.FuncCall:
		f := "FuncCall"
		if n.Distinct.Present() {
			f += " " + strings.ToUpper(n.Distinct.Text)
		}
		if n.OrderBy != nil {
			f += " ORDER BY"
		}
		if n.Filter != nil {
			f += " FILTER"
		}
		if n.Over != nil {
			f += " OVER"
		}
		return f
	case *ast.Row:
		return "Row"
	case *ast.Subquery:
		return "Subquery"
	case *ast.Exists:
		return "Exists"
	case *ast.Raise:
		return "Raise"
	case *ast.Paren:
		return "Paren"
	case *ast.Variable:
		return "Variable"
	}
	return ""
}
