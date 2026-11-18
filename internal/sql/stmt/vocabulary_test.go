package stmt_test

import (
	goast "go/ast"
	goparser "go/parser"
	gotoken "go/token"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// allowed node types and fields of slice 1, as the README defines it: a type by its name, and a field
// by the name of its type and its path, as "CreateTable.Name.Name". A field is present when its token
// is, when its node is not nil, or when its list is not empty.
var allowed = map[string]bool{
	"Statement": true, "Statement.Cmd": true, "Statement.Semi": true,

	"CreateTable": true, "CreateTable.Create": true, "CreateTable.Table": true, "CreateTable.Name.Name": true,
	"CreateTable.LParen": true, "CreateTable.Columns": true, "CreateTable.RParen": true,
	"CreateTable.Options": true, "CreateTable.OptionCommas": true,
	"ColumnDef": true, "ColumnDef.Name": true, "ColumnDef.Type": true, "ColumnDef.Constraints": true,
	"TypeName": true, "TypeName.Names": true,
	"PrimaryKeyConstraint": true, "PrimaryKeyConstraint.Primary": true, "PrimaryKeyConstraint.Key": true,
	"TableOption": true, "TableOption.Without": true, "TableOption.Name": true,

	"Insert": true, "Insert.Insert": true, "Insert.Into": true, "Insert.Table": true, "Insert.LParen": true,
	"Insert.Columns": true, "Insert.RParen": true, "Insert.Select": true,
	"TargetTable": true, "TargetTable.Name.Name": true,
	"IDList": true, "IDList.Names": true,
	"Select": true, "Select.First": true,
	"Values": true, "Values.Values": true, "Values.Rows": true,
	"ValuesRow": true, "ValuesRow.LParen": true, "ValuesRow.Exprs": true, "ValuesRow.RParen": true,
	"Unary": true, "Unary.Op": true, "Unary.X": true,
	"Literal": true, "Literal.Value": true,

	"SelectCmd": true, "SelectCmd.Select": true,
	"SimpleSelect": true, "SimpleSelect.Select": true, "SimpleSelect.Columns": true, "SimpleSelect.From": true,
	"ResultColumn": true, "ResultColumn.Star": true,
	"From": true, "From.From": true, "From.Items": true,
	"FromItem": true, "FromItem.Source": true,
	"TableSource": true, "TableSource.Name.Name": true,

	"Drop": true, "Drop.Drop": true, "Drop.Object": true, "Drop.Name.Name": true,
}

// features of the tree from n that are present: the name of the type of every node, and every field
// of a node that is present, as [allowed] names them. types collects the type of every node.
func features(n ast.Node, types map[string]reflect.Type) map[string]bool {
	f := map[string]bool{}
	ast.Walk(n, func(m ast.Node) bool {
		v := reflect.ValueOf(m).Elem()
		name := v.Type().Name()
		types[name] = v.Type()
		f[name] = true
		var fields func(v reflect.Value, path string)
		fields = func(v reflect.Value, path string) {
			for i := range v.NumField() {
				sf := v.Type().Field(i)
				if !sf.IsExported() {
					continue
				}
				key := path + "." + sf.Name
				fv := v.Field(i)
				switch {
				case fv.Type() == reflect.TypeFor[ast.Token]():
					if fv.Interface().(ast.Token).Present() {
						f[key] = true
					}
				case fv.Kind() == reflect.Slice:
					if fv.Len() > 0 {
						f[key] = true
					}
				case fv.Kind() == reflect.Struct:
					fields(fv, key)
				case fv.Kind() == reflect.Interface || fv.Kind() == reflect.Pointer:
					if !fv.IsNil() {
						f[key] = true
					}
				}
			}
		}
		fields(v, name)
		return true
	})
	return f
}

// fieldKeys of the type t, as [features] names them.
func fieldKeys(t reflect.Type) []string {
	var keys []string
	var fields func(t reflect.Type, path string)
	fields = func(t reflect.Type, path string) {
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() {
				continue
			}
			if sf.Type.Kind() == reflect.Struct && sf.Type != reflect.TypeFor[ast.Token]() {
				fields(sf.Type, path+"."+sf.Name)
				continue
			}
			keys = append(keys, path+"."+sf.Name)
		}
	}
	fields(t, t.Name())
	return keys
}

// nodeTypes of package ast, read from its source: every struct that embeds base, exprBase, or
// cmdBase.
func nodeTypes(t testing.TB) []string {
	t.Helper()
	files, err := filepath.Glob("../ast/*.go")
	is.NotError(t, err)
	var names []string
	fset := gotoken.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := goparser.ParseFile(fset, name, nil, 0)
		is.NotError(t, err)
		for _, d := range f.Decls {
			g, ok := d.(*goast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range g.Specs {
				ts, ok := spec.(*goast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*goast.StructType)
				if !ok || len(st.Fields.List) == 0 || st.Fields.List[0].Names != nil {
					continue
				}
				embedded, ok := st.Fields.List[0].Type.(*goast.Ident)
				if ok && (embedded.Name == "base" || embedded.Name == "exprBase" || embedded.Name == "cmdBase") &&
					ts.Name.Name != "exprBase" && ts.Name.Name != "cmdBase" {
					names = append(names, ts.Name.Name)
				}
			}
		}
	}
	return names
}

// neverPresent are the fields that the parser never sets: DROP has IF EXISTS, never IF NOT EXISTS.
var neverPresent = map[string]bool{"Drop.IfExists.Not": true}

// allowListExtra holds statements for the fields that the corpus and the generators use too rarely.
var allowListExtra = []string{
	"explain query plan select * from t",
	"create table t (v int primary key autoincrement) strict, without rowid",
	"insert into t (v) default values",
	"insert or ignore into t (v) values (1)",
}

// TestAllowList checks that a supported statement uses only the node types and fields of slice 1,
// on many statements, and that every node type of package ast and every field of the node types of
// slice 1 occurs in some statement of the test, so that the check covers each of them.
func TestAllowList(t *testing.T) {
	sqls := corpusSQL()
	if slow() {
		sqls = every(sqls, 10)
	}
	for _, c := range checkCases {
		sqls = append(sqls, c.src)
	}
	sqls = append(sqls, allowListExtra...)
	g := newGenerator(t, *seed+2)
	n := 2000
	if slow() {
		n /= 10
	}
	for i := range n {
		src, _ := g.statement()
		sqls = append(sqls, src)
		sqls = append(sqls, parsetest.Mutants(src, *seed+uint64(i), 5)...)
	}
	gen := parsetest.NewGenerator(readGrammar(t), *seed)
	for range n {
		sqls = append(sqls, gen.Sentence("input", 4+gen.IntN(40)))
	}

	types := map[string]reflect.Type{}
	seen := map[string]bool{}
	seenSupported := map[string]bool{}
	for _, sql := range sqls {
		tree, _, err := parser.Parse(sql)
		if err != nil || tree == nil {
			continue
		}
		f := features(tree, types)
		maps.Copy(seen, f)
		if s, err := stmt.Check(sql); err == nil && s != nil {
			for k := range f {
				if !allowed[k] {
					t.Errorf("%q is supported, and has %v, which slice 1 does not have", sql, k)
				}
			}
			maps.Copy(seenSupported, f)
		}
	}

	for _, name := range nodeTypes(t) {
		if !seen[name] {
			t.Errorf("no statement of the test has a node of type %v", name)
			continue
		}
		if !allowed[name] {
			continue
		}
		for _, k := range fieldKeys(types[name]) {
			if !allowed[k] && !seen[k] && !neverPresent[k] {
				t.Errorf("no statement of the test has the field %v", k)
			}
		}
	}
	for _, k := range slices.Sorted(maps.Keys(allowed)) {
		if !seenSupported[k] {
			t.Errorf("no supported statement of the test has %v", k)
		}
	}
}
