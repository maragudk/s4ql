package stmt_test

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"maragu.dev/s4ql/internal/sql/ast"
	"maragu.dev/s4ql/internal/sql/parser"
	"maragu.dev/s4ql/internal/sql/parsetest"
	"maragu.dev/s4ql/internal/sql/stmt"
)

// allowed node types and fields of slices 1, 2, 5, 7, 8, 9, 10, 11, 13, 14, and 15, of arithmetic,
// and of text in expressions, as the README defines them: a type by its name, and a field by the name of its type and
// its path, as "CreateTable.Name.Name". A field is present when its token is, when its node is not
// nil, or when its list is not empty.
var allowed = map[string]bool{
	"Statement": true, "Statement.Cmd": true, "Statement.Semi": true,

	"CreateTable": true, "CreateTable.Create": true, "CreateTable.Table": true, "CreateTable.Name.Name": true,
	"CreateTable.IfNotExists.If": true, "CreateTable.IfNotExists.Not": true, "CreateTable.IfNotExists.Exists": true,
	"CreateTable.LParen": true, "CreateTable.Columns": true, "CreateTable.Commas": true, "CreateTable.RParen": true,
	"CreateTable.Options": true, "CreateTable.OptionCommas": true,
	"ColumnDef": true, "ColumnDef.Name": true, "ColumnDef.Type": true, "ColumnDef.Constraints": true,
	"TypeName": true, "TypeName.Names": true,
	"PrimaryKeyConstraint": true, "PrimaryKeyConstraint.Primary": true, "PrimaryKeyConstraint.Key": true,
	"NullConstraint": true, "NullConstraint.Not": true, "NullConstraint.Null": true,
	"DefaultConstraint": true, "DefaultConstraint.Default": true, "DefaultConstraint.Sign": true, "DefaultConstraint.X": true,
	"TableOption": true, "TableOption.Without": true, "TableOption.Name": true,

	"Insert": true, "Insert.Insert": true, "Insert.Into": true, "Insert.Table": true, "Insert.LParen": true,
	"Insert.Columns": true, "Insert.RParen": true, "Insert.Select": true,
	"TargetTable": true, "TargetTable.Name.Name": true,
	"IDList": true, "IDList.Names": true, "IDList.Commas": true,
	"Select": true, "Select.First": true,
	"Values": true, "Values.Values": true, "Values.Rows": true,
	"ValuesRow": true, "ValuesRow.LParen": true, "ValuesRow.Exprs": true, "ValuesRow.Commas": true, "ValuesRow.RParen": true,
	"Unary": true, "Unary.Op": true, "Unary.X": true,
	"Literal": true, "Literal.Value": true,
	"Variable": true, "Variable.Name": true,

	"SelectCmd": true, "SelectCmd.Select": true,
	"SimpleSelect": true, "SimpleSelect.Select": true, "SimpleSelect.Columns": true, "SimpleSelect.Commas": true, "SimpleSelect.From": true,
	"ResultColumn": true, "ResultColumn.Star": true, "ResultColumn.Expr": true, "ColumnRef": true, "ColumnRef.Column": true,
	"From": true, "From.From": true, "From.Items": true,
	"FromItem": true, "FromItem.Source": true,
	"TableSource": true, "TableSource.Name.Name": true,
	"SimpleSelect.Where": true, "Where": true, "Where.Where": true, "Where.X": true,
	"Binary": true, "Binary.X": true, "Binary.Op": true, "Binary.Y": true,
	"Is": true, "Is.X": true, "Is.Is": true, "Is.Not": true, "Is.Y": true,

	"Update": true, "Update.Update": true, "Update.Table": true, "Update.Set": true, "Update.Assignments": true,
	"Update.Commas": true, "Update.Where": true,
	"Assignment": true, "Assignment.Column": true, "Assignment.Eq": true, "Assignment.X": true,
	"Delete": true, "Delete.Delete": true, "Delete.From": true, "Delete.Table": true, "Delete.Where": true,

	"Drop": true, "Drop.Drop": true, "Drop.Object": true, "Drop.Name.Name": true, "Drop.IfExists.If": true,
	"Drop.IfExists.Exists": true,

	"CreateIndex": true, "CreateIndex.Create": true, "CreateIndex.Index": true, "CreateIndex.Name.Name": true,
	"CreateIndex.IfNotExists.If": true, "CreateIndex.IfNotExists.Not": true, "CreateIndex.IfNotExists.Exists": true,
	"CreateIndex.On": true, "CreateIndex.Table": true, "CreateIndex.LParen": true, "CreateIndex.Columns": true,
	"CreateIndex.Commas": true, "CreateIndex.RParen": true,
	"OrderingTerm": true, "OrderingTerm.X": true, "OrderingTerm.Order": true,

	"SimpleSelect.OrderBy": true, "OrderBy": true, "OrderBy.Order": true, "OrderBy.By": true, "OrderBy.Terms": true,
	"OrderBy.Commas": true, "SimpleSelect.Limit": true, "Limit": true, "Limit.Limit": true, "Limit.X": true,

	"Insert.Returning": true, "Update.Returning": true, "Delete.Returning": true, "Returning": true, "Returning.Returning": true,
	"Returning.Columns": true, "Returning.Commas": true,

	"Subquery": true, "Subquery.LParen": true, "Subquery.Select": true, "Subquery.RParen": true,

	"Paren": true, "Paren.LParen": true, "Paren.X": true, "Paren.RParen": true,

	"FuncCall": true, "FuncCall.Name": true, "FuncCall.LParen": true, "FuncCall.Args": true, "FuncCall.RParen": true,
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

// featureKeys of the fields of the type t, as [features] names them.
func featureKeys(t reflect.Type) []string {
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

// neverPresent are the fields that the parser never sets: DROP has IF EXISTS, never IF NOT EXISTS.
var neverPresent = map[string]bool{"Drop.IfExists.Not": true}

// allowListExtra holds statements for the fields that the corpus and the generators use too rarely.
var allowListExtra = []string{
	"explain query plan select * from t",
	"create table t (v int primary key autoincrement) strict, without rowid",
	"insert into t (v) default values",
	"insert or ignore into t (v) values (1)",
}

// TestAllowList checks that a supported statement uses only the node types and fields of slices 1 to
// 15, of arithmetic, and of text in expressions, on many statements, and that every node type of package ast and every field
// of the node types of slice 1 occurs in some statement of the test, so that the check covers each of
// them.
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
		src, _ := g.Statement()
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
					t.Errorf("%q is supported, and has %v, which slices 1 to 15, arithmetic, and text do not have", sql, k)
				}
			}
			maps.Copy(seenSupported, f)
		}
	}

	for _, name := range stmt.NodeTypes(t) {
		if !seen[name] {
			t.Errorf("no statement of the test has a node of type %v", name)
			continue
		}
		if !allowed[name] {
			continue
		}
		for _, k := range featureKeys(types[name]) {
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
