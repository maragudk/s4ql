package stmt

import (
	"reflect"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/ast"
)

// describe the token t, which the recognizer did not take: what it is part of. From the root, the
// walk goes down through the nodes that the recognizer accepted. If t is a token of such a node, the
// name is that of its field, from [fieldNames], or else that of the node; if t is in a child that the
// recognizer did not accept, the name is that of the child, from [nodeNames]. So the name is that of
// the outermost construct around t that the recognizer did not accept.
func (r *recognizer) describe(t ast.Token) string {
	var n ast.Node = r.tree
	for {
		if keys := fieldKeys(n, t); keys != nil {
			return fieldName(n, keys)
		}
		child := childAt(n, t.Offset)
		if child == nil {
			// A token that the text has and no node of the tree holds.
			return nodeName(n)
		}
		if !r.accepted[child] {
			return nodeName(child)
		}
		n = child
	}
}

// fieldName from [fieldNames] for the first of keys that it has, or else the name of the node n.
func fieldName(n ast.Node, keys []string) string {
	for _, k := range keys {
		if what, ok := fieldNames[k]; ok {
			return what
		}
	}
	return nodeName(n)
}

// childAt gives the child of n whose span holds the byte at offset, or nil if none does.
func childAt(n ast.Node, offset int) ast.Node {
	for _, c := range ast.Children(n) {
		if s := c.Bounds(); s.Start <= offset && offset < s.End {
			return c
		}
	}
	return nil
}

// fieldKeys of the field of the node n that holds the token t, or nil if no field of n holds it. The
// keys are tried in order: the type of n and the path of the field, as "CreateTable.Name.Schema"; then
// the type of the struct that holds the field and its name, as "QualifiedName.Schema" for a field of a
// struct that is not a node.
func fieldKeys(n ast.Node, t ast.Token) []string {
	var find func(v reflect.Value, path string) []string
	find = func(v reflect.Value, path string) []string {
		for i := range v.NumField() {
			sf := v.Type().Field(i)
			if !sf.IsExported() {
				continue
			}
			f := v.Field(i)
			holds := false
			switch x := f.Interface().(type) {
			case ast.Token:
				holds = x == t
			case []ast.Token:
				holds = slices.Contains(x, t)
			default:
				if f.Kind() == reflect.Struct {
					if keys := find(f, path+"."+sf.Name); keys != nil {
						return keys
					}
				}
			}
			if holds {
				return []string{path + "." + sf.Name, v.Type().Name() + "." + sf.Name}
			}
		}
		return nil
	}
	v := reflect.ValueOf(n).Elem()
	return find(v, v.Type().Name())
}

// nodeName of the node n, from [nodeNames], which names every type of node. NOT INDEXED has a name of
// its own, apart from INDEXED BY, which is a node of the same type.
func nodeName(n ast.Node) string {
	if ib, ok := n.(*ast.IndexedBy); ok && ib.Not.Present() {
		return "NOT INDEXED"
	}
	return nodeNames[reflect.TypeOf(n).Elem().Name()]
}

// fieldNames of the tokens of accepted nodes that no slice supports, by the keys of
// [fieldKeys].
var fieldNames = map[string]string{
	"Statement.Explain":                  "EXPLAIN",
	"QualifiedName.Schema":               "schema name",
	"CreateTable.Temp":                   "TEMP",
	"CreateTable.IfNotExists.If":         "IF NOT EXISTS",
	"CreateTable.OptionCommas":           "empty table option",
	"CreateTable.As":                     "CREATE TABLE AS",
	"PrimaryKeyConstraint.Order":         "ASC or DESC",
	"PrimaryKeyConstraint.Autoincrement": "AUTOINCREMENT",
	"Drop.IfExists.If":                   "IF EXISTS",
	"CreateIndex.Unique":                 "UNIQUE INDEX",
	"CreateIndex.IfNotExists.If":         "IF NOT EXISTS",
	"OrderingTerm.Nulls":                 "NULLS FIRST or NULLS LAST",
	"Insert.Or":                          "INSERT OR",
	"Insert.Default":                     "DEFAULT VALUES",
	"TargetTable.As":                     "alias",
	"Update.Or":                          "UPDATE OR",
	"Assignment.LParen":                  "SET of a list of columns",
	"Values.Commas":                      "second row",
	"SimpleSelect.Distinct":              "DISTINCT or ALL",
	"ResultColumn.As":                    "alias",
	"ResultColumn.Alias":                 "alias",
	"FromItem.As":                        "alias",
	"FromItem.Alias":                     "alias",
}

// nodeNames of the node types, by the name of the type.
var nodeNames = map[string]string{
	// Expressions.
	"Literal":   "literal",
	"Variable":  "parameter",
	"ColumnRef": "column reference",
	"Paren":     "parentheses",
	"Row":       "row value",
	"Subquery":  "subquery",
	"Exists":    "EXISTS",
	"Unary":     "unary operator",
	"Binary":    "binary operator",
	"Is":        "IS",
	"NullTest":  "NULL test",
	"Like":      "LIKE, GLOB, REGEXP, or MATCH",
	"Between":   "BETWEEN",
	"InList":    "IN",
	"InSelect":  "IN",
	"InTable":   "IN",
	"Collate":   "COLLATE",
	"Cast":      "CAST",
	"FuncCall":  "function call",
	"Filter":    "FILTER clause",
	"Over":      "OVER clause",
	"Case":      "CASE",
	"When":      "WHEN",
	"Raise":     "RAISE",

	// SELECT.
	"Select":         "SELECT",
	"Compound":       "compound SELECT",
	"SimpleSelect":   "SELECT",
	"Values":         "VALUES",
	"ValuesRow":      "row of VALUES",
	"ResultColumn":   "result column",
	"From":           "FROM clause",
	"FromItem":       "join",
	"TableSource":    "table",
	"TableFunction":  "table-valued function",
	"SubquerySource": "subquery in FROM",
	"NestedFrom":     "join in parentheses",
	"JoinOp":         "join",
	"JoinOn":         "ON clause",
	"JoinUsing":      "USING clause",
	"IndexedBy":      "INDEXED BY",
	"Where":          "WHERE clause",
	"GroupBy":        "GROUP BY clause",
	"Having":         "HAVING clause",
	"OrderBy":        "ORDER BY clause",
	"OrderingTerm":   "ordering term",
	"Limit":          "LIMIT clause",
	"With":           "WITH clause",
	"CTE":            "common table expression",
	"ColumnNames":    "column list",
	"ColumnName":     "column name",
	"IDList":         "column list",
	"WindowClause":   "WINDOW clause",
	"WindowDef":      "window definition",
	"Window":         "window",
	"Frame":          "window frame",
	"FrameBound":     "frame bound",
	"Returning":      "RETURNING clause",

	// Commands.
	"Statement":   "statement",
	"Begin":       "BEGIN",
	"Commit":      "COMMIT",
	"Rollback":    "ROLLBACK",
	"Savepoint":   "SAVEPOINT",
	"Release":     "RELEASE",
	"SelectCmd":   "SELECT",
	"Insert":      "INSERT",
	"Upsert":      "ON CONFLICT clause",
	"Update":      "UPDATE",
	"Assignment":  "assignment",
	"Delete":      "DELETE",
	"TargetTable": "table",
	"Pragma":      "PRAGMA",
	"PragmaValue": "PRAGMA value",
	"Vacuum":      "VACUUM",
	"Attach":      "ATTACH",
	"Detach":      "DETACH",
	"Reindex":     "REINDEX",
	"Analyze":     "ANALYZE",

	// Schema.
	"CreateTable":          "CREATE TABLE",
	"TableOption":          "table option",
	"ColumnDef":            "column",
	"TypeName":             otherType,
	"SignedNumber":         "number",
	"ConstraintName":       "CONSTRAINT name",
	"DefaultConstraint":    "DEFAULT",
	"NullConstraint":       "NULL or NOT NULL constraint",
	"PrimaryKeyConstraint": "PRIMARY KEY",
	"UniqueConstraint":     "UNIQUE",
	"CheckConstraint":      "CHECK",
	"ReferencesConstraint": "REFERENCES",
	"References":           "REFERENCES",
	"RefArg":               "foreign key action",
	"DeferConstraint":      "DEFERRABLE",
	"Deferrable":           "DEFERRABLE",
	"CollateConstraint":    "COLLATE",
	"GeneratedConstraint":  "generated column",
	"OnConflict":           "ON CONFLICT clause",
	"TablePrimaryKey":      "table constraint",
	"TableUnique":          "table constraint",
	"TableForeignKey":      "table constraint",
	"Drop":                 "DROP",
	"CreateView":           "CREATE VIEW",
	"CreateIndex":          "CREATE INDEX",
	"CreateTrigger":        "CREATE TRIGGER",
	"CreateVirtualTable":   "CREATE VIRTUAL TABLE",
	"ModuleArg":            "module argument",
	"AlterTable":           "ALTER TABLE",
	"RenameTable":          "RENAME TO",
	"AddColumn":            "ADD COLUMN",
	"DropColumn":           "DROP COLUMN",
	"RenameColumn":         "RENAME COLUMN",
	"DropConstraint":       "DROP CONSTRAINT",
	"AlterColumnNotNull":   "ALTER COLUMN",
	"AddCheck":             "ADD CHECK",
}

// reservedName gives why the table name with the value v is not supported, or "" if it is. A name
// that starts with sqlite_ or s4ql_, in any case of the letters, is reserved, and so is the name of a
// virtual table that SQLite gives every connection, which a statement reads without creating it.
func reservedName(v string) string {
	if reservedPrefix(v) {
		return "reserved table name"
	}
	if eponymous[asciiLower(v)] {
		return "name of a virtual table of SQLite"
	}
	return ""
}

// reservedPrefix reports whether the name with the value v starts with sqlite_ or s4ql_, in any case
// of the ASCII letters.
func reservedPrefix(v string) bool {
	l := asciiLower(v)
	return strings.HasPrefix(l, "sqlite_") || strings.HasPrefix(l, "s4ql_")
}

// eponymous virtual tables of SQLite, which every connection has without creating them, in lower
// case: json_each, json_tree, jsonb_each, and jsonb_tree, and pragma_ and the name of each PRAGMA that
// gives rows.
var eponymous = func() map[string]bool {
	m := map[string]bool{"json_each": true, "json_tree": true, "jsonb_each": true, "jsonb_tree": true}
	for _, p := range strings.Fields(`
		analysis_limit application_id auto_vacuum automatic_index busy_timeout cache_size cache_spill
		cell_size_check checkpoint_fullfsync collation_list compile_options count_changes data_version
		database_list default_cache_size defer_foreign_keys empty_result_callbacks encoding
		foreign_key_check foreign_key_list foreign_keys freelist_count full_column_names fullfsync
		function_list hard_heap_limit ignore_check_constraints index_info index_list index_xinfo
		integrity_check journal_mode journal_size_limit legacy_alter_table locking_mode max_page_count
		module_list optimize page_count page_size pragma_list query_only quick_check read_uncommitted
		recursive_triggers reverse_unordered_selects schema_version secure_delete short_column_names
		soft_heap_limit synchronous table_info table_list table_xinfo temp_store threads trusted_schema
		user_version writable_schema`) {
		m["pragma_"+p] = true
	}
	return m
}()
