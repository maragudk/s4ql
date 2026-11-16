package shapetest

import (
	"fmt"
	"hash/fnv"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sqlite"
)

// schema that a statement needs to prepare: the tables, views, indexes, and triggers that it names,
// guessed from its tree and completed from SQLite's errors, one error at a time. It is built from the
// original statement only and used for the original and the printed text alike, so a fault in the
// guess can make a comparison fail or be skipped, but never make two different statements look the
// same.
type schema struct {
	attached []string
	tables   []*table
	// indexes, triggers, and other statements, run after the tables.
	extra []string

	// candidates for an unqualified column, in the order of the statement: the tables of FROM
	// clauses and of the target of a DML statement.
	candidates []*table
	// tried holds, for each unqualified column, the index of the next candidate to try.
	tried map[string]int
	// added holds, for each unqualified column, the tables it was added to as unqualified.
	added map[string][]*table
	// aliases of tables, from the FROM clauses and the targets, by lower-case alias.
	aliases map[string]*table
	// upserts of the statement, for a unique index that ON CONFLICT needs.
	upserts []string
	// indexed holds the table of each INDEXED BY, by lower-case name of the index, and indexes the
	// indexes that a fix made.
	indexed map[string]*table
	indexes map[string]bool
}

// table of a schema. A table that must be a view, for an INSTEAD OF trigger, is created as a view
// with the same columns.
type table struct {
	db, name string
	cols     []string
	view     bool
	// keys is the number of columns of a primary key, for a foreign key that names no columns of
	// this table, and unique holds the sets of columns that a foreign key names.
	keys   int
	unique [][]string
}

func (t *table) has(col string) bool {
	return slices.ContainsFunc(t.cols, func(c string) bool { return strings.EqualFold(c, col) })
}

func (t *table) add(col string) bool {
	if t.has(col) || isRowid(col) {
		return false
	}
	t.cols = append(t.cols, col)
	return true
}

// isRowid reports whether col is a name of the rowid, which every table without WITHOUT ROWID has.
func isRowid(col string) bool {
	switch strings.ToLower(col) {
	case "rowid", "oid", "_rowid_":
		return true
	}
	return false
}

func (t *table) remove(col string) bool {
	i := slices.IndexFunc(t.cols, func(c string) bool { return strings.EqualFold(c, col) })
	if i < 0 {
		return false
	}
	t.cols = slices.Delete(t.cols, i, i+1)
	return true
}

// qname of the table, quoted, with its schema.
func (t *table) qname() string {
	if t.db == "" {
		return quote(t.name)
	}
	return quote(t.db) + "." + quote(t.name)
}

// statements that create the schema.
func (s *schema) statements() []string {
	var stmts []string
	for _, db := range s.attached {
		stmts = append(stmts, "attach ':memory:' as "+quote(db))
	}
	for _, t := range s.tables {
		cols := t.cols
		if len(cols) == 0 {
			cols = []string{"s4ql_c"}
		}
		var b strings.Builder
		if t.view {
			b.WriteString("create view " + t.qname() + " as select ")
			for i, c := range cols {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString("null as " + quote(c))
			}
		} else {
			b.WriteString("create table " + t.qname() + " (")
			for i, c := range cols {
				if i > 0 {
					b.WriteString(", ")
				}
				b.WriteString(quote(c) + " " + columnType(c))
			}
			if t.keys > 0 {
				var keys []string
				for i := range t.keys {
					keys = append(keys, fmt.Sprint("s4ql_k", i))
				}
				b.WriteString(", " + strings.Join(keys, ", ") + ", primary key (" + strings.Join(keys, ", ") + ")")
			}
			for _, u := range t.unique {
				var qs []string
				for _, c := range u {
					qs = append(qs, quote(c))
				}
				b.WriteString(", unique (" + strings.Join(qs, ", ") + ")")
			}
			b.WriteString(")")
		}
		stmts = append(stmts, b.String())
	}
	return append(stmts, s.extra...)
}

// columnTypes that the columns of a schema get, by their names, so that the affinity and the
// collation of a column take part in the programs: SQLite compares with the collation of the left
// operand, and applies the affinity of a column that is not inside another operator, so a wrong
// grouping or a swap of operands shows in the program where untyped columns would hide it.
var columnTypes = []string{"INTEGER", "TEXT", "", "REAL", "TEXT COLLATE NOCASE", "NUMERIC", "TEXT COLLATE RTRIM", "BLOB"}

// columnType of the column named col.
func columnType(col string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(col)))
	return columnTypes[h.Sum32()%uint32(len(columnTypes))]
}

// open a connection to a new in-memory database with the schema.
func (s *schema) open() (*sqlite.Conn, error) {
	c, err := sqlite.Open(":memory:")
	if err != nil {
		return nil, err
	}
	if err := c.DenyProcessPragmas(); err != nil {
		_ = c.Close()
		return nil, err
	}
	// CREATE TABLE ... AS runs its SELECT, which may make a value of a gigabyte.
	if err := c.SetLengthLimit(100_000); err != nil {
		_ = c.Close()
		return nil, err
	}
	// REGEXP, MATCH, and GLOB ... ESCAPE then prepare and run, so they are compared by their
	// programs and values.
	if err := c.AddTestFunctions(); err != nil {
		_ = c.Close()
		return nil, err
	}
	for _, stmt := range s.statements() {
		if err := c.Exec(stmt); err != nil {
			_ = c.Close()
			return nil, fmt.Errorf("schema: %v: %w", stmt, err)
		}
	}
	return c, nil
}

// find the table by name, without regard to case, in the database db or in any if db is "".
func (s *schema) find(db, name string) *table {
	for _, t := range s.tables {
		if strings.EqualFold(t.name, name) && (db == "" || strings.EqualFold(t.db, db)) {
			return t
		}
	}
	return nil
}

// table of the name, which is added if it is not there yet.
func (s *schema) table(db, name string) *table {
	if strings.EqualFold(db, "main") {
		db = ""
	}
	if t := s.find(db, name); t != nil {
		return t
	}
	if strings.HasPrefix(strings.ToLower(name), "sqlite_") {
		// SQLite does not let such a table be created; those that exist are there already.
		return &table{db: db, name: name}
	}
	if db != "" && !strings.EqualFold(db, "temp") && !slices.ContainsFunc(s.attached, func(a string) bool { return strings.EqualFold(a, db) }) {
		s.attached = append(s.attached, db)
	}
	t := &table{db: db, name: name}
	s.tables = append(s.tables, t)
	return t
}

func (s *schema) candidate(t *table) {
	if !slices.Contains(s.candidates, t) {
		s.candidates = append(s.candidates, t)
	}
}

// guess the schema of the statement from its tree.
func guess(stmt *ast.Statement, src string) *schema {
	s := &schema{tried: map[string]int{}, added: map[string][]*table{}, aliases: map[string]*table{}, indexed: map[string]*table{}}
	ctes := map[string]bool{}
	ast.Walk(stmt, func(n ast.Node) bool {
		if c, ok := n.(*ast.CTE); ok {
			ctes[strings.ToLower(dequote(c.Name.Text))] = true
		}
		return true
	})
	named := func(q ast.QualifiedName) *table {
		if !q.Present() {
			return nil
		}
		name := dequote(q.Name.Text)
		if !q.Schema.Present() && ctes[strings.ToLower(name)] {
			return nil
		}
		return s.table(dequote(q.Schema.Text), name)
	}
	alias := func(t *table, alias ast.Token) {
		if t != nil && alias.Present() {
			s.aliases[strings.ToLower(dequote(alias.Text))] = t
		}
	}
	indexed := func(t *table, i *ast.IndexedBy) {
		if i != nil && i.Index.Present() {
			s.indexed[strings.ToLower(dequote(i.Index.Text))] = t
		}
	}
	var trigger *table
	ast.Walk(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FromItem:
			if ts, ok := n.Source.(*ast.TableSource); ok {
				if t := named(ts.Name); t != nil {
					s.candidate(t)
					alias(t, n.Alias)
					indexed(t, n.Indexed)
				}
			}
		case *ast.TargetTable:
			if t := named(n.Name); t != nil {
				s.candidate(t)
				alias(t, n.Alias)
			}
		case *ast.Update:
			if t := named(n.Table.Name); t != nil {
				indexed(t, n.Indexed)
			}
		case *ast.Delete:
			if t := named(n.Table.Name); t != nil {
				indexed(t, n.Indexed)
			}
		case *ast.InTable:
			if !n.LParen.Present() {
				named(n.Name)
			}
		case *ast.CreateIndex:
			t := s.table(dequote(n.Name.Schema.Text), dequote(n.Table.Text))
			s.candidate(t)
		case *ast.CreateTrigger:
			trigger = named(n.Table)
			if trigger != nil {
				trigger.view = n.Time.Present() && strings.EqualFold(n.Time.Text, "instead")
				if n.Columns != nil {
					for _, c := range n.Columns.Names {
						trigger.add(dequote(c.Text))
					}
				}
			}
		case *ast.AlterTable:
			t := named(n.Name)
			if t == nil {
				break
			}
			s.candidate(t)
			switch a := n.Action.(type) {
			case *ast.RenameColumn:
				t.add(dequote(a.Old.Text))
			case *ast.DropColumn:
				t.add(dequote(a.Name.Text))
				// A table must keep a column.
				t.add("s4ql_c")
			case *ast.AlterColumnNotNull:
				t.add(dequote(a.Name.Text))
			}
		case *ast.ReferencesConstraint:
			s.parent(stmt, n.References, 1)
		case *ast.TableForeignKey:
			if n.Columns != nil {
				s.parent(stmt, n.References, len(n.Columns.Columns))
			}
		case *ast.Drop:
			name := dequote(n.Name.Name.Text)
			db := dequote(n.Name.Schema.Text)
			switch strings.ToLower(n.Object.Text) {
			case "table":
				s.table(db, name)
			case "view":
				s.table(db, name).view = true
			case "index":
				t := s.table(db, "s4ql_t")
				s.extra = append(s.extra, "create index "+qualify(db, name)+" on "+quote(t.name)+" (s4ql_c)")
			case "trigger":
				t := s.table(db, "s4ql_t")
				s.extra = append(s.extra, "create trigger "+qualify(db, name)+" after insert on "+quote(t.name)+" begin select 1; end")
			}
		case *ast.Insert:
			if n.Columns != nil {
				t := named(n.Table.Name)
				for _, c := range n.Columns.Names {
					if t != nil {
						t.add(dequote(c.Text))
					}
				}
			}
			for _, u := range n.Upserts {
				if !u.LParen.Present() {
					continue
				}
				t := named(n.Table.Name)
				if t == nil {
					continue
				}
				var terms []string
				for _, term := range u.Target {
					terms = append(terms, src[term.Span.Start:term.Span.End])
				}
				idx := "create unique index " + qualify(t.db, fmt.Sprint("s4ql_u", len(s.upserts))) + " on " + quote(t.name) + " (" + strings.Join(terms, ", ") + ")"
				if u.TargetWhere != nil {
					idx += " where " + src[u.TargetWhere.X.Bounds().Start:u.TargetWhere.X.Bounds().End]
				}
				s.upserts = append(s.upserts, idx)
			}
		}
		return true
	})
	// Assignments name columns of the target, and qualified column references name a column of a
	// table or of an alias.
	ast.Walk(stmt, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Update:
			if t := named(n.Table.Name); t != nil {
				for _, a := range n.Assignments {
					assign(t, a)
				}
			}
		case *ast.Insert:
			if t := named(n.Table.Name); t != nil {
				for _, u := range n.Upserts {
					for _, a := range u.Assignments {
						assign(t, a)
					}
				}
			}
		case *ast.ColumnRef:
			if !n.Table.Present() {
				break
			}
			q := strings.ToLower(dequote(n.Table.Text))
			col := dequote(n.Column.Text)
			switch {
			case trigger != nil && (q == "new" || q == "old") && !n.Schema.Present():
				trigger.add(col)
			case !n.Schema.Present() && s.aliases[q] != nil:
				s.aliases[q].add(col)
			default:
				if t := s.find(dequote(n.Schema.Text), dequote(n.Table.Text)); t != nil {
					t.add(col)
				}
			}
		}
		return true
	})
	// A column that ALTER TABLE adds, or renames a column to, must not be there yet.
	defer func() {
		if a, ok := stmt.Cmd.(*ast.AlterTable); ok {
			t := s.find(dequote(a.Name.Schema.Text), dequote(a.Name.Name.Text))
			switch act := a.Action.(type) {
			case *ast.AddColumn:
				if t != nil {
					t.remove(dequote(act.Def.Name.Text))
				}
			case *ast.RenameColumn:
				if t != nil {
					t.remove(dequote(act.New.Text))
				}
			}
		}
	}()
	// An unqualified column is guessed to be in the first table.
	if len(s.candidates) > 0 {
		t := s.candidates[0]
		ast.Walk(stmt, func(n ast.Node) bool {
			if c, ok := n.(*ast.ColumnRef); ok && !c.Table.Present() {
				col := dequote(c.Column.Text)
				key := strings.ToLower(col)
				if key == "true" || key == "false" {
					return true
				}
				if t.add(col) {
					s.added[key] = append(s.added[key], t)
				}
				s.tried[key] = 1
			}
			return true
		})
	}
	return s
}

// holdsTarget reports whether the schema already holds a table or view of the name that a CREATE
// TABLE creates. With IF NOT EXISTS SQLite then leaves the schema as it is, and what the statement
// declares is not what SQLite has.
func (s *schema) holdsTarget(cmd ast.Cmd) bool {
	ct, ok := cmd.(*ast.CreateTable)
	if !ok {
		return false
	}
	name := dequote(ct.Name.Name.Text)
	return slices.ContainsFunc(s.tables, func(t *table) bool { return foldEqual(t.name, name) })
}

// parent table of a foreign key with n columns, with a primary key or a unique constraint that
// the foreign key can refer to. A table that refers to itself is not made here.
func (s *schema) parent(stmt *ast.Statement, r *ast.References, n int) {
	name := dequote(r.Table.Text)
	switch c := stmt.Cmd.(type) {
	case *ast.CreateTable:
		if strings.EqualFold(dequote(c.Name.Name.Text), name) {
			return
		}
	case *ast.AlterTable:
		if strings.EqualFold(dequote(c.Name.Name.Text), name) {
			return
		}
	}
	db := ""
	if c, ok := stmt.Cmd.(*ast.CreateTable); ok {
		db = dequote(c.Name.Schema.Text)
	}
	t := s.table(db, name)
	if r.Columns == nil {
		t.keys = max(t.keys, n)
		return
	}
	var cols []string
	for _, c := range r.Columns.Columns {
		col := dequote(c.Name.Text)
		t.add(col)
		cols = append(cols, col)
	}
	t.unique = append(t.unique, cols)
}

func assign(t *table, a *ast.Assignment) {
	if a.Column.Present() {
		t.add(dequote(a.Column.Text))
	}
	if a.Columns != nil {
		for _, c := range a.Columns.Names {
			t.add(dequote(c.Text))
		}
	}
}

var (
	reNoTable    = regexp.MustCompile(`^no such table: (?:([^.]+)\.)?(.+)$`)
	reNoColumn   = regexp.MustCompile(`^no such column: (.+?)(?: - should this be a string literal in single-quotes\?)?$`)
	reUsing      = regexp.MustCompile(`^cannot join using column (.+) - column not present in both tables$`)
	reAmbiguous  = regexp.MustCompile(`^ambiguous column name: (.+)$`)
	reNoNamed    = regexp.MustCompile(`^table (.+) has no column named (.+)$`)
	reValues     = regexp.MustCompile(`^table (.+) has (\d+) columns but (\d+) values were supplied$`)
	reUnknownDB  = regexp.MustCompile(`^unknown database (.+)$`)
	reNoIndex    = regexp.MustCompile(`^no such index: (?:([^.]+)\.)?(.+)$`)
	reInsteadOf  = regexp.MustCompile(`^cannot create INSTEAD OF trigger on table: (?:([^.]+)\.)?(.+)$`)
	reUpsert     = regexp.MustCompile(`^(?:\d+(?:st|nd|rd|th) )?ON CONFLICT clause does not match any PRIMARY KEY or UNIQUE constraint$`)
	reTriggerOnV = regexp.MustCompile(`^cannot create (?:BEFORE|AFTER) trigger on view: (?:([^.]+)\.)?(.+)$`)
)

// fix the schema for SQLite's error message msg, and report whether it changed.
func (s *schema) fix(msg string) bool {
	if m := reNoTable.FindStringSubmatch(msg); m != nil {
		db := m[1]
		if strings.EqualFold(db, "main") {
			db = ""
		}
		if s.find(db, m[2]) != nil || strings.HasPrefix(strings.ToLower(m[2]), "sqlite_") {
			return false
		}
		s.candidate(s.table(db, m[2]))
		return true
	}
	if m := reUnknownDB.FindStringSubmatch(msg); m != nil {
		db := strings.Trim(m[1], `"`)
		if slices.ContainsFunc(s.attached, func(a string) bool { return strings.EqualFold(a, db) }) {
			return false
		}
		s.attached = append(s.attached, db)
		return true
	}
	if m := reNoColumn.FindStringSubmatch(msg); m != nil {
		return s.fixColumn(strings.Trim(m[1], `"`))
	}
	if m := reUsing.FindStringSubmatch(msg); m != nil {
		changed := false
		for _, t := range s.candidates {
			changed = t.add(m[1]) || changed
		}
		return changed
	}
	if m := reAmbiguous.FindStringSubmatch(msg); m != nil {
		col := m[1]
		if i := strings.LastIndexByte(col, '.'); i >= 0 {
			col = col[i+1:]
		}
		ts := s.added[strings.ToLower(col)]
		if len(ts) == 0 {
			return false
		}
		t := ts[len(ts)-1]
		s.added[strings.ToLower(col)] = ts[:len(ts)-1]
		return t.remove(col)
	}
	if m := reNoNamed.FindStringSubmatch(msg); m != nil {
		db, name := split(m[1])
		if t := s.find(db, name); t != nil {
			return t.add(m[2])
		}
		return false
	}
	if m := reValues.FindStringSubmatch(msg); m != nil {
		db, name := split(m[1])
		t := s.find(db, name)
		want, _ := strconv.Atoi(m[3])
		if t == nil || want == len(t.cols) {
			return false
		}
		before := len(t.cols)
		for len(t.cols) < want {
			t.cols = append(t.cols, fmt.Sprint("s4ql_c", len(t.cols)))
		}
		// Too many columns: drop those that were only guessed for unqualified references.
		for _, col := range slices.Sorted(maps.Keys(s.added)) {
			if len(t.cols) <= want {
				break
			}
			if ts := s.added[col]; slices.Contains(ts, t) {
				i := slices.Index(ts, t)
				t.remove(col)
				s.added[col] = slices.Delete(ts, i, i+1)
			}
		}
		return len(t.cols) != before
	}
	if m := reNoIndex.FindStringSubmatch(msg); m != nil {
		key := strings.ToLower(m[2])
		if s.indexes[key] {
			return false
		}
		t := s.indexed[key]
		if t == nil {
			t = s.firstTable()
		}
		if s.indexes == nil {
			s.indexes = map[string]bool{}
		}
		s.indexes[key] = true
		s.extra = append(s.extra, "create index "+qualify(m[1], m[2])+" on "+quote(t.name)+" ("+quote(t.firstColumn())+")")
		return true
	}
	if m := reInsteadOf.FindStringSubmatch(msg); m != nil {
		if t := s.find(m[1], m[2]); t != nil && !t.view {
			t.view = true
			return true
		}
		return false
	}
	if m := reTriggerOnV.FindStringSubmatch(msg); m != nil {
		if t := s.find(m[1], m[2]); t != nil && t.view {
			t.view = false
			return true
		}
		return false
	}
	if reUpsert.MatchString(msg) && len(s.upserts) > 0 {
		s.extra = append(s.extra, s.upserts...)
		s.upserts = nil
		return true
	}
	return false
}

// fixColumn adds a column that SQLite did not find: qualified, to the table that the qualifier
// names; unqualified, to the next table that may hold it.
func (s *schema) fixColumn(ref string) bool {
	parts := strings.Split(ref, ".")
	col := parts[len(parts)-1]
	if len(parts) > 1 {
		q := parts[len(parts)-2]
		db := ""
		if len(parts) > 2 {
			db = parts[0]
		}
		if t := s.aliases[strings.ToLower(q)]; t != nil && db == "" {
			return t.add(col)
		}
		if t := s.find(db, q); t != nil {
			return t.add(col)
		}
		return false
	}
	key := strings.ToLower(col)
	for s.tried[key] < len(s.candidates) {
		t := s.candidates[s.tried[key]]
		s.tried[key]++
		if t.add(col) {
			s.added[key] = append(s.added[key], t)
			return true
		}
	}
	return false
}

func (s *schema) firstTable() *table {
	if len(s.candidates) > 0 {
		return s.candidates[0]
	}
	return s.table("", "s4ql_t")
}

func (t *table) firstColumn() string {
	if len(t.cols) == 0 {
		t.cols = append(t.cols, "s4ql_c")
	}
	return t.cols[0]
}

// split a name that SQLite wrote in a message, as schema.name or name.
func split(name string) (string, string) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[:i], name[i+1:]
	}
	return "", name
}

// quote a name for SQL.
func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func qualify(db, name string) string {
	if db == "" {
		return quote(name)
	}
	return quote(db) + "." + quote(name)
}

// dequote the raw text of a name as SQLite does, by [sqliteDequote].
func dequote(raw string) string {
	return sqliteDequote(raw)
}

// sqliteDequote is sqlite3Dequote: if s starts with a quote, ", ', `, or [, the text up to the first
// closing quote that is not doubled, with each doubled quote as one; otherwise s.
func sqliteDequote(s string) string {
	if s == "" {
		return s
	}
	q := s[0]
	switch q {
	case '"', '\'', '`':
	case '[':
		q = ']'
	default:
		return s
	}
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		if s[i] == q {
			if q != ']' && i+1 < len(s) && s[i+1] == q {
				b.WriteByte(q)
				i++
				continue
			}
			break
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
