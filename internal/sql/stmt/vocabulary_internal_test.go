package stmt

import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sqlite"
)

// astTypes of package ast, read from its source: the name of every struct type, and the names of
// the exported fields of each.
func astTypes(t *testing.T) map[string][]string {
	t.Helper()
	files, err := filepath.Glob("../ast/*.go")
	is.NotError(t, err)
	types := map[string][]string{}
	fset := gotoken.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, name, nil, 0)
		is.NotError(t, err)
		goast.Inspect(f, func(n goast.Node) bool {
			ts, ok := n.(*goast.TypeSpec)
			if !ok {
				return true
			}
			if st, ok := ts.Type.(*goast.StructType); ok && ts.Name.IsExported() {
				types[ts.Name.Name] = []string{}
				for _, field := range st.Fields.List {
					for _, id := range field.Names {
						types[ts.Name.Name] = append(types[ts.Name.Name], id.Name)
					}
				}
			}
			return true
		})
	}
	return types
}

func TestVocabulary(t *testing.T) {
	types := astTypes(t)

	t.Run("should name every node type of package ast, and nothing else", func(t *testing.T) {
		for name := range types {
			switch name {
			case "Span", "Token", "QualifiedName", "IfNotExists":
				continue
			}
			_, ok := nodeNames[name]
			is.True(t, ok, name+" has no name")
		}
		for name := range nodeNames {
			_, ok := types[name]
			is.True(t, ok, name+" is no type of package ast")
		}
	})

	t.Run("should name only fields that exist", func(t *testing.T) {
		for key := range fieldNames {
			parts := strings.Split(key, ".")
			typ := parts[0]
			for _, f := range parts[1:] {
				is.True(t, slices.Contains(types[typ], f), key)
				typ = map[string]string{"Name": "QualifiedName", "IfNotExists": "IfNotExists", "IfExists": "IfNotExists"}[f]
			}
		}
	})

	t.Run("should describe a token in a field of a struct inside a node", func(t *testing.T) {
		d := &ast.Drop{IfExists: ast.IfNotExists{If: ast.Token{Kind: 1, Offset: 5, Text: "if"}}}
		is.EqualSlice(t, []string{"Drop.IfExists.If", "IfNotExists.If"}, fieldKeys(d, d.IfExists.If))
		is.True(t, fieldKeys(d, ast.Token{Kind: 1, Offset: 6, Text: "if"}) == nil)
	})
}

// fresh connection to SQLite, for one prepare.
func fresh(t *testing.T) *sqlite.Conn {
	t.Helper()
	c, err := sqlite.Open(":memory:")
	is.NotError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// prepares reports whether sql prepares on a new connection.
func prepares(t *testing.T, sql string) bool {
	t.Helper()
	s, err := fresh(t).Prepare(sql)
	if err != nil {
		return false
	}
	is.NotError(t, s.Finalize())
	return true
}

// column of the rows of sql on a new connection.
func column(t *testing.T, sql string) []string {
	t.Helper()
	s, err := fresh(t).Prepare(sql)
	is.NotError(t, err)
	var values []string
	for {
		ok, err := s.Step()
		is.NotError(t, err)
		if !ok {
			break
		}
		values = append(values, s.ColumnText(0))
	}
	is.NotError(t, s.Finalize())
	return values
}

func TestReservedName(t *testing.T) {
	t.Run("should list exactly the eponymous virtual tables of SQLite", func(t *testing.T) {
		// Every PRAGMA that the build has, from pragma_list and from the table of PRAGMAs in the
		// source, and every module.
		candidates := map[string]bool{}
		for _, p := range column(t, "select name from pragma_pragma_list") {
			candidates["pragma_"+p] = true
		}
		b, err := os.ReadFile("../../sqlite/c/sqlite3.c")
		is.NotError(t, err)
		for _, m := range regexp.MustCompile(`/\* zName: +\*/ "(\w+)"`).FindAllStringSubmatch(string(b), -1) {
			candidates["pragma_"+m[1]] = true
		}
		for _, m := range column(t, "select name from pragma_module_list") {
			candidates[m] = true
		}
		// The modules that sqlite3LocateTable registers when a statement names them: those of JSON,
		// whose names are in sqlite3JsonVtabRegister, and carray, which the build does not enable.
		_, json, _ := strings.Cut(string(b), "Module *sqlite3JsonVtabRegister(sqlite3 *db, const char *zName){")
		json, _, _ = strings.Cut(json, "};")
		for _, m := range regexp.MustCompile(`"(\w+)"`).FindAllStringSubmatch(json, -1) {
			candidates[m[1]] = true
		}
		candidates["carray"] = true
		is.True(t, len(candidates) > 70)
		for c := range candidates {
			is.Equal(t, prepares(t, "select * from "+c), eponymous[c], c)
		}
		for e := range eponymous {
			is.True(t, candidates[e], e)
			is.True(t, prepares(t, "select * from "+strings.ToUpper(e)), e)
		}
		is.Equal(t, 63, len(eponymous))
	})

	t.Run("should reserve sqlite_ and s4ql_ in any case of the ASCII letters", func(t *testing.T) {
		for _, v := range []string{"sqlite_", "SQLite_x", "s4ql_", "S4QL_t", "sqlite_schema"} {
			is.Equal(t, "reserved table name", reservedName(v), v)
		}
		for _, v := range []string{"sqlite", "s4ql", "xsqlite_", "sqlıte_x", "pragma_x", "json_eachx", "", "ſqlite_"} {
			is.Equal(t, "", reservedName(v), v)
		}
		is.Equal(t, "name of a virtual table of SQLite", reservedName("JSON_Each"))
	})

	t.Run("should take for reserved what SQLite reserves", func(t *testing.T) {
		for _, v := range []string{"sqlite_x", "SQLITE_x", "SqLiTe_"} {
			_, err := fresh(t).Prepare(`create table "` + v + `" (v int primary key) strict, without rowid`)
			is.True(t, err != nil && strings.Contains(err.Error(), "object name reserved for internal use"), v)
		}
	})
}

func TestNotSupportedError(t *testing.T) {
	t.Run("should match ErrNotSupported and no other error", func(t *testing.T) {
		err := &NotSupportedError{What: "x", Offset: 3}
		is.True(t, err.Is(ErrNotSupported))
		is.True(t, !err.Is(os.ErrNotExist))
		is.Equal(t, "s4ql: not supported: x (at offset 3)", err.Error())
	})
}

func TestStatement(t *testing.T) {
	t.Run("should give the span of every kind of statement", func(t *testing.T) {
		for _, s := range []Statement{&CreateTable{Span: Span{1, 2}}, &Insert{Span: Span{1, 2}}, &Select{Span: Span{1, 2}}, &DropTable{Span: Span{1, 2}}} {
			is.Equal(t, Span{1, 2}, s.Bounds())
			s.statement()
		}
		is.Equal(t, reflect.TypeFor[SyntaxError]().NumField(), 4)
	})
}
