package shapetest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// The comparison of what a tree declares with what SQLite made of the original text.
//
// The printed text cannot show where a column's type ends and its constraints begin, or which
// constraint a CONSTRAINT name names: a tree that puts such a boundary elsewhere prints the same
// tokens in the same order. So for CREATE TABLE, ALTER TABLE ... ADD, CREATE INDEX, and CREATE VIEW
// with a list of columns, the check derives from the tree what SQLite records, and compares that
// with what SQLite says about the schema after it ran the original text, through PRAGMAs and the
// program of an INSERT. The derivation is a model of SQLite's build.c, for the tests, and each rule
// names the function of SQLite that it follows.

// declaredFault starts the fault of a difference between what the tree declares and what SQLite
// made of the text.
const declaredFault = "the tree declares what SQLite does not make of the text: "

// declColumn is a column as SQLite records it: the fields of pragma table_xinfo, and its collation.
type declColumn struct {
	name string
	// typ is the type as sqlite3ColumnType gives it, and ctype the standard type, as an index into
	// stdTypes plus one, or 0.
	typ     string
	ctype   int
	notNull bool
	// dflt is the text of the default, as sqlite3AddDefaultValue keeps it.
	dflt    string
	hasDflt bool
	// hidden is 2 for a VIRTUAL generated column and 3 for a STORED one, as in pragma table_xinfo.
	hidden int
	// primKey is COLFLAG_PRIMKEY.
	primKey bool
	coll    string
}

// declIndexColumn is a key column of an index: its column, -1 for the rowid, or -2 for an expression.
type declIndexColumn struct {
	cid  int
	desc bool
	coll string
}

type declIndex struct {
	// origin is "pk" for a PRIMARY KEY, "u" for UNIQUE, and "c" for CREATE INDEX.
	origin string
	cols   []declIndexColumn
}

type declForeignKey struct {
	from               []int
	table              string
	to                 []string
	onUpdate, onDelete string
	deferred           bool
}

// declTable is a table as SQLite builds it from a CREATE TABLE, in the order of the parser's actions.
type declTable struct {
	src     string
	cols    []*declColumn
	indexes []*declIndex
	// fks, newest first, as SQLite links them in pTab->u.tab.pFKey.
	fks    []*declForeignKey
	checks []string
	// constraintName is pParse->u1.cr.constraintName, and named whether it is set.
	constraintName string
	named          bool
	hasPK          bool
	// iPKey is the column that is the rowid, or -1, and pkDesc pParse->iPkSortOrder.
	iPKey        int
	pkDesc       bool
	autoinc      bool
	withoutRowid bool
	strict       bool
	// err is why SQLite would fail on the table as the tree declares it, or "".
	err string
}

func (t *declTable) fail(format string, args ...any) {
	if t.err == "" {
		t.err = fmt.Sprintf(format, args...)
	}
}

// stdTypes of SQLite, sqlite3StdType in global.c.
var stdTypes = []string{"ANY", "BLOB", "INT", "INTEGER", "REAL", "TEXT"}

const ctypeInteger = 4

// isSpace is sqlite3Isspace: the space, \t, \n, \v, \f, and \r.
func isSpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

// trimSpace of s as sqlite3Isspace sees spaces.
func trimSpace(s string) string {
	for s != "" && isSpace(s[0]) {
		s = s[1:]
	}
	for s != "" && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

// foldEqual is sqlite3StrICmp and sqlite3_strnicmp on strings of the same length: equal but for the
// case of ASCII letters.
func foldEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		if lower(a[i]) != lower(b[i]) {
			return false
		}
	}
	return true
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func isQuote(c byte) bool {
	return c == '"' || c == '\'' || c == '`' || c == '['
}

// span of the source that the tree from n covers, from its first token to its last.
func span(src string, n ast.Node) string {
	ts := ast.AllTokens(n)
	if len(ts) == 0 {
		return ""
	}
	start, end := ts[0].Offset, ts[0].End()
	for _, t := range ts {
		start, end = min(start, t.Offset), max(end, t.End())
	}
	return src[start:end]
}

// declaredType gives the type of a column as sqlite3AddColumn records it from the text of the type,
// which is the source from its first token to its last (typetoken in parse.y), and the standard type.
func declaredType(src string, tn *ast.TypeName) (string, int) {
	if tn == nil {
		return "", 0
	}
	s := span(src, tn)
	// A type that ends in ALWAYS, or GENERATED ALWAYS, loses it if it is 16 bytes or more: the
	// parser can take these keywords for words of the type.
	if n := len(s); n >= 16 && foldEqual(s[n-6:], "always") {
		s = s[:n-6]
		for s != "" && isSpace(s[len(s)-1]) {
			s = s[:len(s)-1]
		}
		if n := len(s); n >= 9 && foldEqual(s[n-9:], "generated") {
			s = s[:n-9]
			for s != "" && isSpace(s[len(s)-1]) {
				s = s[:len(s)-1]
			}
		}
	}
	if len(s) >= 3 {
		// sqlite3DequoteToken: the quotes come off if there is no quote inside them; the last byte
		// goes whatever it is.
		if isQuote(s[0]) && !strings.ContainsAny(s[1:len(s)-1], "\"'`[") {
			s = s[1 : len(s)-1]
		}
		for i, std := range stdTypes {
			if foldEqual(s, std) {
				return std, i + 1
			}
		}
	}
	if s == "" {
		return "", 0
	}
	return sqliteDequote(s), 0
}

// addColumn is sqlite3AddColumn.
func (t *declTable) addColumn(d *ast.ColumnDef) {
	typ, ctype := declaredType(t.src, d.Type)
	t.cols = append(t.cols, &declColumn{name: dequote(d.Name.Text), typ: typ, ctype: ctype})
	t.named = false
}

func (t *declTable) last() (*declColumn, int) {
	return t.cols[len(t.cols)-1], len(t.cols) - 1
}

// columnConstraint applies one constraint of the last column, as the actions of ccons in parse.y do.
func (t *declTable) columnConstraint(c ast.ColumnConstraint) {
	col, i := t.last()
	switch c := c.(type) {
	case *ast.ConstraintName:
		t.constraintName, t.named = dequote(c.Name.Text), true
	case *ast.DefaultConstraint:
		// sqlite3AddDefaultValue keeps the text from zStart to zEnd, without the spaces around it
		// (sqlite3DbSpanDup).
		var text string
		switch {
		case c.LParen.Present():
			text = t.src[c.LParen.End():c.RParen.Offset]
		case c.Name.Present():
			text = c.Name.Text
		case c.X == nil:
			t.fail("a DEFAULT without a value")
			return
		default:
			lit, ok := c.X.(*ast.Literal)
			if !ok {
				t.fail("a DEFAULT with %T, which only DEFAULT ( expr ) can hold", c.X)
				return
			}
			start := lit.Value.Offset
			if c.Sign.Present() {
				start = c.Sign.Offset
			}
			text = t.src[start:lit.Value.End()]
		}
		if col.hidden != 0 {
			t.fail("cannot use DEFAULT on a generated column")
			return
		}
		col.dflt, col.hasDflt = trimSpace(text), true
	case *ast.NullConstraint:
		if c.Not.Present() {
			col.notNull = true
		}
	case *ast.PrimaryKeyConstraint:
		t.addPrimaryKey(nil, c.Autoincrement.Present(), foldEqual(c.Order.Text, "desc"))
	case *ast.UniqueConstraint:
		t.createIndex(nil, "u", false)
	case *ast.CheckConstraint:
		t.addCheck(c)
	case *ast.ReferencesConstraint:
		t.createForeignKey([]int{i}, c.References)
	case *ast.DeferConstraint:
		t.deferForeignKey(c.Defer)
	case *ast.CollateConstraint:
		// sqlite3AddCollateType, which also sets the collation of the indexes on the column made so
		// far, all of which have this column alone.
		col.coll = dequote(c.Name.Text)
		for _, idx := range t.indexes {
			if idx.cols[0].cid == i {
				idx.cols[0].coll = col.coll
			}
		}
	case *ast.GeneratedConstraint:
		// sqlite3AddGenerated.
		if col.hasDflt || col.hidden != 0 {
			t.fail("error in generated column %q", col.name)
			return
		}
		switch {
		case !c.Storage.Present() || foldEqual(c.Storage.Text, "virtual"):
			col.hidden = 2
		case foldEqual(c.Storage.Text, "stored"):
			col.hidden = 3
		default:
			t.fail("error in generated column %q", col.name)
		}
		if col.primKey {
			t.fail("generated columns cannot be part of the PRIMARY KEY")
		}
	default:
		t.fail("a column constraint of type %T", c)
	}
}

// tableConstraint applies one table constraint, as the actions of tcons in parse.y do. comma is
// whether a comma comes before it, which ends a CONSTRAINT name (tconscomma).
func (t *declTable) tableConstraint(c ast.TableConstraint, comma bool) {
	if comma {
		t.named = false
	}
	switch c := c.(type) {
	case *ast.ConstraintName:
		t.constraintName, t.named = dequote(c.Name.Text), true
	case *ast.TablePrimaryKey:
		t.addPrimaryKey(c.Columns, c.Autoincrement.Present(), false)
	case *ast.TableUnique:
		t.createIndex(c.Columns, "u", false)
	case *ast.CheckConstraint:
		t.addCheck(c)
	case *ast.TableForeignKey:
		var from []int
		if c.Columns != nil {
			for _, n := range c.Columns.Columns {
				j := t.column(dequote(n.Name.Text))
				if j < 0 {
					t.fail("unknown column %q in foreign key definition", dequote(n.Name.Text))
					return
				}
				from = append(from, j)
			}
		}
		t.createForeignKey(from, c.References)
		t.deferForeignKey(c.Defer)
	default:
		t.fail("a table constraint of type %T", c)
	}
}

// column by its name, as sqlite3ColumnIndex finds it, or -1.
func (t *declTable) column(name string) int {
	return slices.IndexFunc(t.cols, func(c *declColumn) bool { return foldEqual(c.name, name) })
}

// addCheck is sqlite3AddCheckConstraint: the name of a CHECK is the CONSTRAINT name before it, or
// else its text between the parentheses, without the spaces around it. Either is dequoted
// (sqlite3ExprListSetName).
func (t *declTable) addCheck(c *ast.CheckConstraint) {
	name := t.constraintName
	if !t.named {
		name = sqliteDequote(trimSpace(t.src[c.LParen.End():c.RParen.Offset]))
	}
	t.checks = append(t.checks, name)
}

// bare is e without the parentheses around it, which SQLite's tree does not have.
func bare(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.Paren)
		if !ok {
			return e
		}
		e = p.X
	}
}

// skipCollate is sqlite3ExprSkipCollate, with the parentheses gone too.
func skipCollate(e ast.Expr) ast.Expr {
	for {
		e = bare(e)
		c, ok := e.(*ast.Collate)
		if !ok {
			return e
		}
		e = c.X
	}
}

// termName gives the name of a column that a term of a PRIMARY KEY names, as sqlite3AddPrimaryKey
// takes it: a name, or a string (sqlite3StringToId), under any COLLATE.
func termName(e ast.Expr) (string, bool) {
	switch e := skipCollate(e).(type) {
	case *ast.ColumnRef:
		if !e.Table.Present() {
			return dequote(e.Column.Text), true
		}
	case *ast.Literal:
		if e.Value.Kind == token.String {
			return dequote(e.Value.Text), true
		}
	}
	return "", false
}

// addPrimaryKey is sqlite3AddPrimaryKey, for the last column if terms is nil. A single column of
// the type INTEGER, in ascending order, becomes the rowid; else the key is a unique index.
func (t *declTable) addPrimaryKey(terms []*ast.OrderingTerm, autoinc, desc bool) {
	if t.hasPK {
		t.fail("table has more than one primary key")
		return
	}
	t.hasPK = true
	col, iCol := (*declColumn)(nil), -1
	if terms == nil {
		col, iCol = t.last()
		t.makePrimaryKey(col)
	} else {
		for _, term := range terms {
			if name, ok := termName(term.X); ok {
				if j := t.column(name); j >= 0 {
					col, iCol = t.cols[j], j
					t.makePrimaryKey(col)
				}
			}
		}
	}
	n := 1
	if terms != nil {
		n = len(terms)
	}
	switch {
	case n == 1 && col != nil && col.ctype == ctypeInteger && !desc:
		t.iPKey = iCol
		t.autoinc = autoinc
		if terms != nil {
			t.pkDesc = foldEqual(terms[0].Order.Text, "desc")
		}
	case autoinc:
		t.fail("AUTOINCREMENT is only allowed on an INTEGER PRIMARY KEY")
	default:
		t.createIndex(terms, "pk", desc)
	}
}

// makePrimaryKey is makeColumnPartOfPrimaryKey.
func (t *declTable) makePrimaryKey(col *declColumn) {
	col.primKey = true
	if col.hidden != 0 {
		t.fail("generated columns cannot be part of the PRIMARY KEY")
	}
}

// indexColumn resolves a term of an index as sqlite3CreateIndex does: a string under at most one
// COLLATE is a name (sqlite3StringToId), or under any number for a PRIMARY KEY, pk, whose terms
// sqlite3AddPrimaryKey has converted before; a name of a column is that column; anything else is
// an expression. SQLite refuses the rowid in an index (no such column: rowid). The collation is that
// of the outer COLLATE, or the column's, or BINARY.
func indexColumn(e ast.Expr, desc, pk bool, cols []*declColumn) declIndexColumn {
	e = bare(e)
	coll := ""
	inner := e
	if c, ok := e.(*ast.Collate); ok {
		coll = dequote(c.Name.Text)
		inner = bare(c.X)
	}
	if pk {
		inner = skipCollate(e)
	}
	if lit, ok := inner.(*ast.Literal); ok && lit.Value.Kind == token.String {
		e = &ast.ColumnRef{Column: ast.Token{Kind: token.ID, Text: lit.Value.Text}}
	}
	cid := -2
	if ref, ok := skipCollate(e).(*ast.ColumnRef); ok {
		name := dequote(ref.Column.Text)
		if j := slices.IndexFunc(cols, func(c *declColumn) bool { return foldEqual(c.name, name) }); j >= 0 {
			cid = j
		}
	}
	if coll == "" && cid >= 0 {
		coll = cols[cid].coll
	}
	if coll == "" {
		coll = "BINARY"
	}
	return declIndexColumn{cid: cid, desc: desc, coll: coll}
}

// createIndex is sqlite3CreateIndex for a PRIMARY KEY or UNIQUE constraint: on the last column if
// terms is nil, in the order desc. An index with the same columns and collations as one made
// before is not made, and makes that one the primary key if it is one.
func (t *declTable) createIndex(terms []*ast.OrderingTerm, origin string, desc bool) {
	idx := &declIndex{origin: origin}
	if terms == nil {
		col, i := t.last()
		coll := col.coll
		if coll == "" {
			coll = "BINARY"
		}
		idx.cols = []declIndexColumn{{cid: i, desc: desc, coll: coll}}
	} else {
		for _, term := range terms {
			ic := indexColumn(term.X, foldEqual(term.Order.Text, "desc"), origin == "pk", t.cols)
			if ic.cid == -2 {
				t.fail("expressions prohibited in PRIMARY KEY and UNIQUE constraints")
				return
			}
			idx.cols = append(idx.cols, ic)
		}
	}
	t.addIndex(idx)
}

// addIndex unless the table has an index with the same columns and collations in the same order,
// which is the same index for sqlite3CreateIndex whatever the order of sorting. A primary key then
// makes that index the primary key.
func (t *declTable) addIndex(idx *declIndex) {
	for _, old := range t.indexes {
		if slices.EqualFunc(old.cols, idx.cols, sameColumn) {
			if idx.origin == "pk" {
				old.origin = "pk"
			}
			return
		}
	}
	t.indexes = append(t.indexes, idx)
}

// sameColumn of an index: the same column with the same collation (isDupColumn).
func sameColumn(x, y declIndexColumn) bool {
	return x.cid == y.cid && foldEqual(x.coll, y.coll)
}

// createForeignKey is sqlite3CreateForeignKey, with the actions of refargs in parse.y: the last ON
// DELETE and the last ON UPDATE count, and MATCH and ON INSERT are read and ignored.
func (t *declTable) createForeignKey(from []int, r *ast.References) {
	fk := &declForeignKey{from: from, table: dequote(r.Table.Text), onUpdate: "NO ACTION", onDelete: "NO ACTION"}
	if r.Columns != nil {
		fk.to = []string{}
		for _, c := range r.Columns.Columns {
			fk.to = append(fk.to, dequote(c.Name.Text))
		}
		if len(fk.to) != len(from) {
			t.fail("the number of columns of a foreign key does not match")
			return
		}
	}
	for _, a := range r.Args {
		if !a.On.Present() {
			continue
		}
		var words []string
		for _, w := range a.Action {
			words = append(words, strings.ToUpper(w.Text))
		}
		action := strings.Join(words, " ")
		switch {
		case foldEqual(a.Event.Text, "delete"):
			fk.onDelete = action
		case foldEqual(a.Event.Text, "update"):
			fk.onUpdate = action
		}
	}
	t.fks = append([]*declForeignKey{fk}, t.fks...)
}

// deferForeignKey is sqlite3DeferForeignKey: DEFERRABLE INITIALLY DEFERRED applies to the foreign
// key made last, of whichever column; anything else, or no clause, makes it immediate.
func (t *declTable) deferForeignKey(d *ast.Deferrable) {
	if len(t.fks) == 0 {
		return
	}
	t.fks[0].deferred = d != nil && !d.Not.Present() && foldEqual(d.Mode.Text, "deferred")
}

// endTable is what sqlite3EndTable and convertToWithoutRowidTable do with the options.
func (t *declTable) endTable(options []*ast.TableOption) {
	for _, o := range options {
		switch {
		case o.Without.Present() && len(o.Name.Text) == 5 && foldEqual(o.Name.Text, "rowid"):
			t.withoutRowid = true
		case !o.Without.Present() && len(o.Name.Text) == 6 && foldEqual(o.Name.Text, "strict"):
			t.strict = true
		default:
			t.fail("unknown table option: %s", o.Name.Text)
		}
	}
	if t.strict {
		for i, c := range t.cols {
			if c.ctype == 0 {
				t.fail("unknown or missing datatype for %s", c.name)
			}
			if c.primKey && t.iPKey != i {
				c.notNull = true
			}
		}
	}
	if t.withoutRowid {
		if t.autoinc {
			t.fail("AUTOINCREMENT not allowed on WITHOUT ROWID tables")
		}
		if !t.hasPK {
			t.fail("PRIMARY KEY missing on table")
		}
		for _, c := range t.cols {
			if c.primKey {
				c.notNull = true
			}
		}
		if t.iPKey >= 0 {
			col := t.cols[t.iPKey]
			coll := col.coll
			if coll == "" {
				coll = "BINARY"
			}
			ipk := t.iPKey
			t.iPKey = -1
			t.addIndex(&declIndex{origin: "pk", cols: []declIndexColumn{{cid: ipk, desc: t.pkDesc, coll: coll}}})
		} else if pk := t.primaryKey(); pk != nil {
			// The key loses its repeated columns, those with the same column and collation.
			var cols []declIndexColumn
			for _, c := range pk.cols {
				if !slices.ContainsFunc(cols, func(d declIndexColumn) bool { return sameColumn(c, d) }) {
					cols = append(cols, c)
				}
			}
			pk.cols = cols
		}
	}
	nonGenerated := slices.ContainsFunc(t.cols, func(c *declColumn) bool { return c.hidden == 0 })
	if !nonGenerated {
		t.fail("must have at least one non-generated column")
	}
}

// primaryKey is sqlite3PrimaryKeyIndex: the first index that is the primary key.
func (t *declTable) primaryKey() *declIndex {
	for _, idx := range t.indexes {
		if idx.origin == "pk" {
			return idx
		}
	}
	return nil
}

// declaration of a table as lines of text, which the model and the observation give alike.
type declaration struct {
	columns []string
	table   string
	fks     []string
	indexes []string
	// program says whether the program of an INSERT was read, which holds the checks and whether
	// each foreign key is deferred.
	program bool
	checks  []string
	// deferralSeen says whether the program tells the foreign keys apart. It does not when a parent
	// table is missing, or the table refers to itself.
	deferralSeen bool
	deferred     []string
}

func columnLine(cid int, name, typ string, notNull bool, dflt string, pk, hidden int, coll string) string {
	nn := 0
	if notNull {
		nn = 1
	}
	return fmt.Sprintf("column %d: name %q, type %q, not null %d, default %s, primary key %d, hidden %d, collation %q", cid, name, typ, nn, dflt, pk, hidden, coll)
}

func indexLine(origin string, unique, partial bool, cols []string) string {
	return fmt.Sprintf("index %s, unique %v, partial %v: %s", origin, unique, partial, strings.Join(cols, ", "))
}

func indexColumnLine(cid int, name string, desc bool, coll string) string {
	d := 0
	if desc {
		d = 1
	}
	return fmt.Sprintf("%d %s %d %q", cid, quoted(name), d, coll)
}

// declare a table from the tree of its CREATE TABLE, or give why SQLite would fail on it.
func declare(src string, ct *ast.CreateTable) (*declTable, error) {
	t := &declTable{src: src, iPKey: -1}
	for _, d := range ct.Columns {
		t.addColumn(d)
		for _, c := range d.Constraints {
			t.columnConstraint(c)
		}
	}
	for i, c := range ct.Constraints {
		comma := false
		if i > 0 {
			prev, next := ct.Constraints[i-1].Bounds().End, c.Bounds().Start
			comma = slices.ContainsFunc(ct.Commas, func(tok ast.Token) bool { return tok.Offset >= prev && tok.Offset < next })
		}
		t.tableConstraint(c, comma)
	}
	t.endTable(ct.Options)
	if t.err != "" {
		return t, fmt.Errorf("%s", t.err)
	}
	return t, nil
}

// lines of the model of the table.
func (t *declTable) lines() declaration {
	var d declaration
	pk := t.primaryKey()
	for i, c := range t.cols {
		k := 0
		switch {
		case !c.primKey:
		case pk == nil:
			k = 1
		default:
			// pragma table_xinfo looks for the column in the first nCol columns of the index only,
			// and gives nCol+1 for one after them, as a key with repeated columns can have.
			k = slices.IndexFunc(pk.cols, func(ic declIndexColumn) bool { return ic.cid == i }) + 1
			if k > len(t.cols) || k == 0 {
				k = len(t.cols) + 1
			}
		}
		dflt := "NULL"
		if c.hasDflt && c.hidden == 0 {
			dflt = fmt.Sprintf("%q", c.dflt)
		}
		coll := c.coll
		if coll == "" {
			coll = "BINARY"
		}
		d.columns = append(d.columns, columnLine(i, c.name, c.typ, c.notNull, dflt, k, c.hidden, coll))
	}
	d.table = tableLine(t.withoutRowid, t.strict, len(t.cols), t.autoinc)
	for id, fk := range t.fks {
		d.fks = append(d.fks, t.fkLines(id, fk)...)
		d.deferred = append(d.deferred, fmt.Sprintf("foreign key %d: deferred %v", id, fk.deferred))
	}
	for _, idx := range t.indexes {
		var cols []string
		for _, ic := range idx.cols {
			name := sqlNull
			if ic.cid >= 0 {
				name = t.cols[ic.cid].name
			}
			cols = append(cols, indexColumnLine(ic.cid, name, ic.desc, ic.coll))
		}
		d.indexes = append(d.indexes, indexLine(idx.origin, true, false, cols))
	}
	slices.Sort(d.indexes)
	for i, name := range t.checks {
		d.checks = append(d.checks, fmt.Sprintf("check %d: %q", i, name))
	}
	d.program = true
	return d
}

func tableLine(withoutRowid, strict bool, ncol int, autoinc bool) string {
	return fmt.Sprintf("table: without rowid %v, strict %v, columns %d, autoincrement %v", withoutRowid, strict, ncol, autoinc)
}

func fkLine(id, seq int, table, from, to, onUpdate, onDelete, match string) string {
	return fmt.Sprintf("foreign key %d.%d: table %q, from %s, to %s, on update %s, on delete %s, match %s", id, seq, table, from, to, onUpdate, onDelete, match)
}

func (t *declTable) fkLines(id int, fk *declForeignKey) []string {
	var lines []string
	for seq, j := range fk.from {
		to := "NULL"
		if fk.to != nil {
			to = fmt.Sprintf("%q", fk.to[seq])
		}
		lines = append(lines, fkLine(id, seq, fk.table, fmt.Sprintf("%q", t.cols[j].name), to, fk.onUpdate, fk.onDelete, "NONE"))
	}
	return lines
}

// observation of a table: what SQLite says about it.
type observation struct {
	schema string
	// xinfo holds the rows of pragma table_xinfo, each value as its text, or NULL.
	xinfo [][]string
	colls []string
	d     declaration
}

// sqlNull stands for NULL among the values that [rows] gives, which are texts otherwise.
const sqlNull = "\x00NULL"

// quoted value of [rows] for a line: the text in quotes, or NULL.
func quoted(v string) string {
	if v == sqlNull {
		return "NULL"
	}
	return fmt.Sprintf("%q", v)
}

// rows of a statement, each value as its text, or [sqlNull].
func rows(conn *sqlite.Conn, sql string) ([][]string, error) {
	rs, err := query(conn, sql)
	for _, r := range rs {
		for i, v := range r {
			if v == "null:" {
				r[i] = sqlNull
			} else {
				_, r[i], _ = strings.Cut(v, ":")
			}
		}
	}
	return rs, err
}

// schemaOf the object that a statement creates: temp for TEMP, the schema that qualifies its name,
// or else main.
func schemaOf(temp ast.Token, q ast.QualifiedName) string {
	switch {
	case temp.Present():
		return "temp"
	case q.Schema.Present():
		return dequote(q.Schema.Text)
	}
	return "main"
}

// locate the table or view of the name in the schema db, or for db "" as SQLite finds an
// unqualified name: in temp, then main, then the attached databases. It gives the row of pragma
// table_list: schema, name, type, ncol, wr, strict.
func locate(conn *sqlite.Conn, db, name string) ([]string, error) {
	q := ast.QualifiedName{Name: ast.Token{Kind: token.ID, Text: quote(name)}}
	if db != "" {
		q.Schema = ast.Token{Kind: token.ID, Text: quote(db)}
	}
	list, err := rows(conn, pragma(q, "table_list", name))
	if err != nil {
		return nil, err
	}
	for _, s := range []string{"temp", "main", ""} {
		for _, r := range list {
			if s == "" || foldEqual(r[0], s) {
				return r, nil
			}
		}
	}
	return nil, fmt.Errorf("pragma table_list does not have %s", name)
}

// observeColumns of the table name in the schema db: the rows of pragma table_xinfo, and the
// collation of each column. No PRAGMA gives the collation of a column, but an index on every column
// does, in pragma index_xinfo. The index gets a name that the schema does not have yet, and is
// dropped again.
func observeColumns(conn *sqlite.Conn, db, name string) (xinfo [][]string, colls []string, err error) {
	q := ast.QualifiedName{Schema: ast.Token{Kind: token.ID, Text: quote(db)}, Name: ast.Token{Kind: token.ID, Text: quote(name)}}
	if xinfo, err = rows(conn, pragma(q, "table_xinfo", name)); err != nil {
		return nil, nil, err
	}
	probe := "s4ql_collations"
	for i := 1; ; i++ {
		taken, err := rows(conn, "select count(*) from "+quote(db)+".sqlite_schema where name = "+quoteString(probe)+" collate nocase")
		if err != nil {
			return nil, nil, err
		}
		if taken[0][0] == "0" {
			break
		}
		probe = fmt.Sprint("s4ql_collations_", i)
	}
	var cols []string
	for _, r := range xinfo {
		cols = append(cols, quote(r[1]))
	}
	if err := conn.Exec("create index " + quote(db) + "." + quote(probe) + " on " + quote(name) + " (" + strings.Join(cols, ", ") + ")"); err != nil {
		return nil, nil, err
	}
	xs, err := rows(conn, "pragma "+quote(db)+".index_xinfo("+quoteString(probe)+")")
	if err != nil {
		return nil, nil, err
	}
	for _, x := range xs {
		if x[5] == "1" {
			colls = append(colls, x[4])
		}
	}
	if len(colls) != len(xinfo) {
		return nil, nil, fmt.Errorf("the index on every column has %d columns, and the table %d", len(colls), len(xinfo))
	}
	if err := conn.Exec("drop index " + quote(db) + "." + quote(probe)); err != nil {
		return nil, nil, err
	}
	return xinfo, colls, nil
}

// quoteString as an SQL string literal.
func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// indexes of the table q, from pragma index_list and the key columns of pragma index_xinfo, sorted.
func indexes(conn *sqlite.Conn, q ast.QualifiedName, name string) ([]string, error) {
	idxs, err := rows(conn, pragma(q, "index_list", name))
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, r := range idxs {
		xs, err := rows(conn, pragma(q, "index_xinfo", r[1]))
		if err != nil {
			return nil, err
		}
		var cols []string
		for _, x := range xs {
			if x[5] == "1" {
				cols = append(cols, indexColumnLine(atoi(x[1]), x[2], x[3] == "1", x[4]))
			}
		}
		lines = append(lines, indexLine(r[3], r[2] == "1", r[4] == "1", cols))
	}
	slices.Sort(lines)
	return lines, nil
}

// observeTable of the name in the schema db, or as SQLite finds it for db "", as SQLite made it: its
// columns and their collations, its foreign keys, its indexes, its options, and from the program of
// an INSERT its checks and which foreign keys are deferred.
func observeTable(conn *sqlite.Conn, db, name string) (*observation, error) {
	o := &observation{}
	list, err := locate(conn, db, name)
	if err != nil {
		return nil, err
	}
	if list[2] != "table" {
		return nil, fmt.Errorf("%s is a %s", name, list[2])
	}
	o.schema = list[0]
	q := ast.QualifiedName{Schema: ast.Token{Kind: token.ID, Text: quote(o.schema)}, Name: ast.Token{Kind: token.ID, Text: quote(name)}}
	if o.xinfo, o.colls, err = observeColumns(conn, o.schema, name); err != nil {
		return nil, err
	}
	// SQLite makes sqlite_sequence with the first AUTOINCREMENT table of a schema, and the schema of
	// the check has none, as schema.statements writes its tables without AUTOINCREMENT.
	seq, err := rows(conn, "select count(*) from "+quote(o.schema)+".sqlite_schema where type = 'table' and name = 'sqlite_sequence'")
	if err != nil {
		return nil, err
	}
	o.d.table = tableLine(list[4] == "1", list[5] == "1", atoi(list[3]), seq[0][0] != "0")
	fks, err := rows(conn, pragma(q, "foreign_key_list", name))
	if err != nil {
		return nil, err
	}
	for _, r := range fks {
		o.d.fks = append(o.d.fks, fkLine(atoi(r[0]), atoi(r[1]), r[2], quoted(r[3]), quoted(r[4]), r[5], r[6], r[7]))
	}
	if o.d.indexes, err = indexes(conn, q, name); err != nil {
		return nil, err
	}
	// The program of an INSERT holds a Halt for every CHECK, in order, with its name in P4; and for
	// every foreign key, newest first as pragma foreign_key_list numbers them, an FkCounter whose P1
	// says whether it is deferred, or a Halt for an immediate one (fkLookupParent).
	// A table that refers to itself, with REPLACE, also counts the rows that refer to a row it
	// deletes (fkScanChildren), so its FkCounters do not tell its foreign keys apart.
	self := slices.ContainsFunc(fks, func(r []string) bool { return foldEqual(r[2], name) })
	if prog, err := rows(conn, "explain insert into "+quote(o.schema)+"."+quote(name)+" default values"); err == nil {
		o.d.program = true
		for _, r := range prog {
			switch {
			case r[1] == "Halt" && r[2] == "275":
				o.d.checks = append(o.d.checks, fmt.Sprintf("check %d: %q", len(o.d.checks), r[5]))
			case r[1] == "Halt" && r[2] == "787":
				o.d.deferred = append(o.d.deferred, fmt.Sprintf("foreign key %d: deferred false", len(o.d.deferred)))
			case r[1] == "FkCounter" && atoi(r[3]) > 0:
				o.d.deferred = append(o.d.deferred, fmt.Sprintf("foreign key %d: deferred %v", len(o.d.deferred), r[2] != "0"))
			}
		}
		ids := 0
		for _, r := range fks {
			if r[1] == "0" {
				ids++
			}
		}
		o.d.deferralSeen = !self && len(o.d.deferred) == ids
	}
	for i, r := range o.xinfo {
		o.d.columns = append(o.d.columns, columnLine(atoi(r[0]), r[1], r[2], r[3] == "1", quoted(r[4]), atoi(r[5]), atoi(r[6]), o.colls[i]))
	}
	return o, nil
}

// atoi of an integer that a PRAGMA gives.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// differLines gives the first difference of two lists of lines, named by what they are, or "".
func differLines(what string, want, got []string) string {
	if i, ok := differ(want, got); ok {
		return fmt.Sprintf("%s %d: by the tree %s, by SQLite %s", what, i, at(want, i), at(got, i))
	}
	return ""
}

// compareDeclarations of the tree, want, with SQLite's, got.
func compareDeclarations(want, got declaration) string {
	if !got.program {
		want.checks, got.checks = nil, nil
	}
	if !got.deferralSeen {
		want.deferred, got.deferred = nil, nil
	}
	for _, d := range []struct {
		what      string
		want, got []string
	}{
		{"column", want.columns, got.columns},
		{"table", []string{want.table}, []string{got.table}},
		{"foreign key", want.fks, got.fks},
		{"deferred foreign key", want.deferred, got.deferred},
		{"index", want.indexes, got.indexes},
		{"check", want.checks, got.checks},
	} {
		if f := differLines(d.what, d.want, d.got); f != "" {
			return f
		}
	}
	return ""
}

// declared compares what the tree shown declares with what SQLite made of the original text, which
// conn has run: for CREATE TABLE, ALTER TABLE ... ADD COLUMN and ADD CHECK, CREATE INDEX, and CREATE
// VIEW with a list of columns. It reports whether it compared, and the difference.
func declared(conn *sqlite.Conn, src string, shown *ast.Statement) (bool, string) {
	switch cmd := shown.Cmd.(type) {
	case *ast.CreateTable:
		if cmd.As.Present() {
			return false, ""
		}
		t, err := declare(src, cmd)
		o, oerr := observeTable(conn, schemaOf(cmd.Temp, cmd.Name), dequote(cmd.Name.Name.Text))
		if oerr != nil {
			return true, "SQLite made a table that cannot be observed: " + oerr.Error()
		}
		if err != nil {
			return true, "SQLite made the table, but would fail on the tree: " + err.Error()
		}
		return true, compareDeclarations(t.lines(), o.d)
	case *ast.AlterTable:
		return declaredAlter(conn, src, cmd)
	case *ast.CreateIndex:
		return declaredIndex(conn, cmd)
	case *ast.CreateView:
		return declaredView(conn, cmd)
	}
	return false, ""
}

// declaredAlter compares the column that ALTER TABLE ... ADD COLUMN adds, with its foreign keys and
// checks, and the check that ADD CHECK adds. The table is one of the schema of the check, which has
// no foreign keys and no checks of its own.
func declaredAlter(conn *sqlite.Conn, src string, cmd *ast.AlterTable) (bool, string) {
	switch cmd.Action.(type) {
	case *ast.AddColumn, *ast.AddCheck:
	default:
		return false, ""
	}
	o, err := observeTable(conn, dequote(cmd.Name.Schema.Text), dequote(cmd.Name.Name.Text))
	if err != nil {
		return true, "SQLite altered a table that cannot be observed: " + err.Error()
	}
	t := &declTable{src: src, iPKey: -1}
	switch a := cmd.Action.(type) {
	case *ast.AddColumn:
		for _, r := range o.xinfo[:len(o.xinfo)-1] {
			t.cols = append(t.cols, &declColumn{name: r[1]})
		}
		t.addColumn(a.Def)
		for _, c := range a.Def.Constraints {
			t.columnConstraint(c)
		}
		if t.err != "" {
			return true, "SQLite added the column, but would fail on the tree: " + t.err
		}
		want := t.lines()
		want.columns = want.columns[len(want.columns)-1:]
		got := o.d
		got.columns = got.columns[len(got.columns)-1:]
		got.table, want.table, got.indexes, want.indexes = "", "", nil, nil
		return true, compareDeclarations(want, got)
	case *ast.AddCheck:
		if a.Name.Present() {
			t.constraintName, t.named = dequote(a.Name.Text), true
		}
		t.addCheck(a.Check)
		want := t.lines()
		if !o.d.program {
			return true, "the program of an INSERT into the table cannot be read"
		}
		return true, differLines("check", want.checks, o.d.checks)
	}
	return false, ""
}

// declaredIndex compares the key of the index that CREATE INDEX makes with pragma index_xinfo,
// and whether it is unique and partial with pragma index_list.
func declaredIndex(conn *sqlite.Conn, cmd *ast.CreateIndex) (bool, string) {
	table := dequote(cmd.Table.Text)
	list, err := locate(conn, dequote(cmd.Name.Schema.Text), table)
	if err != nil {
		return true, "SQLite indexed a table that cannot be observed: " + err.Error()
	}
	db := list[0]
	xinfo, colls, err := observeColumns(conn, db, table)
	if err != nil {
		return true, "SQLite indexed a table that cannot be observed: " + err.Error()
	}
	q := ast.QualifiedName{Schema: ast.Token{Kind: token.ID, Text: quote(db)}, Name: ast.Token{Kind: token.ID, Text: quote(table)}}
	idxs, err := rows(conn, pragma(q, "index_list", table))
	if err != nil {
		return true, "pragma index_list fails: " + err.Error()
	}
	var cols []*declColumn
	for i, r := range xinfo {
		cols = append(cols, &declColumn{name: r[1], coll: colls[i]})
	}
	var key []string
	for _, term := range cmd.Columns {
		ic := indexColumn(term.X, foldEqual(term.Order.Text, "desc"), false, cols)
		name := sqlNull
		if ic.cid >= 0 {
			name = cols[ic.cid].name
		}
		key = append(key, indexColumnLine(ic.cid, name, ic.desc, ic.coll))
	}
	want := indexLine("c", cmd.Unique.Present(), cmd.Where != nil, key)
	name := dequote(cmd.Name.Name.Text)
	got := ""
	for _, r := range idxs {
		if foldEqual(r[1], name) {
			xs, err := rows(conn, pragma(q, "index_xinfo", r[1]))
			if err != nil {
				return true, "pragma index_xinfo fails: " + err.Error()
			}
			var key []string
			for _, x := range xs {
				if x[5] == "1" {
					key = append(key, indexColumnLine(atoi(x[1]), x[2], x[3] == "1", x[4]))
				}
			}
			got = indexLine(r[3], r[2] == "1", r[4] == "1", key)
		}
	}
	return true, differLines("index", []string{want}, []string{got})
}

// declaredView compares the names of the columns of a view with a list of columns with pragma
// table_xinfo. Where two names are the same but for case, SQLite makes them unique with a number,
// which can be random (sqlite3ColumnsFromExprList), so such a view is not compared.
func declaredView(conn *sqlite.Conn, cmd *ast.CreateView) (bool, string) {
	if cmd.Columns == nil {
		return false, ""
	}
	var want []string
	for _, c := range cmd.Columns.Columns {
		name := dequote(c.Name.Text)
		if slices.ContainsFunc(want, func(w string) bool { return foldEqual(w, name) }) {
			return false, ""
		}
		want = append(want, name)
	}
	q := ast.QualifiedName{Schema: ast.Token{Kind: token.ID, Text: quote(schemaOf(cmd.Temp, cmd.Name))}, Name: cmd.Name.Name}
	xs, err := rows(conn, pragma(q, "table_xinfo", dequote(cmd.Name.Name.Text)))
	if err != nil {
		return true, "pragma table_xinfo fails on the view: " + err.Error()
	}
	var got []string
	for _, x := range xs {
		got = append(got, x[1])
	}
	return true, differLines("column of the view", want, got)
}
