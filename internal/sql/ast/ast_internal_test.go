package ast

import (
	goast "go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"maragu.dev/is"
)

// badNode holds a value that is neither a token nor a node.
type badNode struct {
	base
	Name  Token
	Count int
}

// badSlice holds a slice of values that are neither tokens nor nodes.
type badSlice struct {
	base
	Names []string
}

// TestPlanOf checks that the walk refuses a node with a field that could hide what was read, and
// that every node type of the package passes.
func TestPlanOf(t *testing.T) {
	t.Run("should panic on a field that holds neither tokens nor nodes", func(t *testing.T) {
		defer func() {
			r := recover()
			is.True(t, r != nil && strings.Contains(r.(string), "badNode.Count"))
		}()
		planOf(reflect.TypeFor[badNode]())
	})

	t.Run("should panic on a slice of values that are neither tokens nor nodes", func(t *testing.T) {
		defer func() {
			r := recover()
			is.True(t, r != nil && strings.Contains(r.(string), "badSlice.Names"))
		}()
		planOf(reflect.TypeFor[badSlice]())
	})

	t.Run("should list every node type of the package in allNodes", func(t *testing.T) {
		listed := map[string]bool{}
		for _, n := range allNodes {
			listed[reflect.TypeOf(n).Elem().Name()] = true
		}
		files, err := filepath.Glob("*.go")
		is.NotError(t, err)
		fset := token.NewFileSet()
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
						is.True(t, listed[ts.Name.Name], ts.Name.Name+" is not in allNodes")
					}
				}
			}
		}
	})

	t.Run("should accept every node type", func(t *testing.T) {
		for _, n := range allNodes {
			v := reflect.ValueOf(n)
			is.True(t, v.Kind() == reflect.Pointer && v.Elem().Kind() == reflect.Struct)
			_ = planOf(v.Elem().Type())
		}
	})
}

// allNodes holds one of every node type, so that a test can go through them all.
var allNodes = []Node{
	&Literal{}, &Variable{}, &ColumnRef{}, &Paren{}, &Row{}, &Subquery{}, &Exists{}, &Unary{},
	&Binary{}, &Is{}, &NullTest{}, &Like{}, &Between{}, &InList{}, &InSelect{}, &InTable{},
	&Collate{}, &Cast{}, &FuncCall{}, &Filter{}, &Over{}, &Case{}, &When{}, &Raise{},
	&Select{}, &Compound{}, &SimpleSelect{}, &Values{}, &ValuesRow{}, &ResultColumn{}, &From{},
	&FromItem{}, &TableSource{}, &TableFunction{}, &SubquerySource{}, &NestedFrom{}, &JoinOp{},
	&JoinOn{}, &JoinUsing{}, &IndexedBy{}, &Where{}, &GroupBy{}, &Having{}, &OrderBy{},
	&OrderingTerm{}, &Limit{}, &With{}, &CTE{}, &ColumnNames{}, &ColumnName{}, &IDList{},
	&WindowClause{}, &WindowDef{}, &Window{}, &Frame{}, &FrameBound{}, &Returning{},
	&Statement{}, &Begin{}, &Commit{}, &Rollback{}, &Savepoint{}, &Release{}, &SelectCmd{},
	&Insert{}, &Upsert{}, &Update{}, &Assignment{}, &Delete{}, &TargetTable{}, &Pragma{},
	&PragmaValue{}, &Vacuum{}, &Attach{}, &Detach{}, &Reindex{}, &Analyze{},
	&CreateTable{}, &TableOption{}, &ColumnDef{}, &TypeName{}, &SignedNumber{}, &ConstraintName{},
	&DefaultConstraint{}, &NullConstraint{}, &PrimaryKeyConstraint{}, &UniqueConstraint{},
	&CheckConstraint{}, &ReferencesConstraint{}, &References{}, &RefArg{}, &DeferConstraint{},
	&Deferrable{}, &CollateConstraint{}, &GeneratedConstraint{}, &OnConflict{}, &TablePrimaryKey{},
	&TableUnique{}, &TableForeignKey{}, &Drop{}, &CreateView{}, &CreateIndex{}, &CreateTrigger{},
	&CreateVirtualTable{}, &ModuleArg{}, &AlterTable{}, &RenameTable{}, &AddColumn{},
	&DropColumn{}, &RenameColumn{}, &DropConstraint{}, &AlterColumnNotNull{}, &AddCheck{},
}
