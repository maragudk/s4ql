package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"
)

// newTestModule writes a module with a package token of three kinds, of which two are keywords,
// and a package parser with the file src, and gives its root.
func newTestModule(t *testing.T, src string) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"internal/sql/token/kind.go": "package token\n\ntype Kind uint8\n\nconst (\n\tIllegal Kind = iota\n\tSpace\n)\n\n" +
			"const (\n\tAbort Kind = iota + Space + 1\n\tAction\n\tAdd\n\tkinds\n)\n",
		"internal/sql/parser/p.go": src,
	}
	for name, text := range files {
		path := filepath.Join(root, name)
		is.NotError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		is.NotError(t, os.WriteFile(path, []byte(text), 0o644))
	}
	return root
}

// generateIn the test module with the file src, and give the keys of the mutants.
func generateIn(t *testing.T, src string) ([]mutant, []string) {
	t.Helper()
	root := newTestModule(t, src)
	u, err := newUniverse(root, []string{"internal/sql/parser"})
	is.NotError(t, err)
	ms, err := u.generate(root, "internal/sql/parser", "internal/sql/parser/p.go")
	is.NotError(t, err)
	var keys []string
	for _, m := range ms {
		keys = append(keys, m.Key)
	}
	return ms, keys
}

func TestGenerate(t *testing.T) {
	t.Run("should make the mutants of each operator", func(t *testing.T) {
		_, keys := generateIn(t, `package parser

import "maragu.dev/s4ql/internal/sql/token"

const (
	low = 1
	high = 2
)

func f(a, b int, k token.Kind) int {
	if a < b && k == token.Abort {
		a++
		return low
	}
	switch k {
	case token.Abort, token.Action:
		return g(a, b)
	case token.Add:
		return max(a, b)
	}
	x := []int{a, b}
	return x[0]
}
`)
		for _, want := range []string{
			"parser/p.go f operator: a < b => a <= b",
			"parser/p.go f operator: a < b => a >= b",
			"parser/p.go f operator: a < b && k == token.Abort => a < b || k == token.Abort",
			"parser/p.go f condition: a < b && k == token.Abort => true",
			"parser/p.go f condition: a < b && k == token.Abort => false",
			"parser/p.go f condition: a < b && k == token.Abort => !(a < b && k == token.Abort)",
			"parser/p.go f condition: a < b => true",
			"parser/p.go f condition: k == token.Abort => false",
			"parser/p.go f neighbour: k == token.Abort => k == token.Action",
			"parser/p.go f neighbour: return low => return high",
			"parser/p.go f increment: a++ => a--",
			"parser/p.go f remove statement: a++ =>",
			"parser/p.go f remove statement: return low =>",
			"parser/p.go f case element: token.Abort, =>",
			"parser/p.go f case element: , token.Action =>",
			"parser/p.go f swap arguments: g(a, b) => g(b, a)",
			"parser/p.go f min/max: max(a, b) => min(a, b)",
			"parser/p.go f element: a, =>",
			"parser/p.go f constant: x[0] => x[1]",
			"parser/p.go low constant: 1 => 2",
			"parser/p.go low constant: 1 => 0",
		} {
			is.True(t, slices.Contains(keys, want), "missing: "+want+"\n"+strings.Join(keys, "\n"))
		}
		is.True(t, slices.ContainsFunc(keys, func(k string) bool { return strings.HasPrefix(k, "parser/p.go f swap cases: ") }))
		is.True(t, !slices.Contains(keys, "parser/p.go f neighbour: x => "), "a variable is not a constant")
	})

	t.Run("should add each keyword that is missing from the fallback set", func(t *testing.T) {
		_, keys := generateIn(t, `package parser

import "maragu.dev/s4ql/internal/sql/token"

var fallback = []token.Kind{token.Abort}
`)
		is.True(t, slices.Contains(keys, "parser/p.go fallback add to set:  => token.Action,"), strings.Join(keys, "\n"))
		is.True(t, slices.Contains(keys, "parser/p.go fallback add to set:  => token.Add,"), strings.Join(keys, "\n"))
		is.True(t, !slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(k, "=> token.Abort,") }))
	})

	t.Run("should move each kind of the precedence table to its neighbours, and add each missing kind", func(t *testing.T) {
		_, keys := generateIn(t, `package parser

import "maragu.dev/s4ql/internal/sql/token"

func infixPrec(k token.Kind) int {
	switch k {
	case token.Abort:
		return 1
	case token.Action, token.Add:
		return 2
	}
	return 0
}
`)
		for _, want := range []string{
			"parser/p.go infixPrec move in table: case token.Abort: return 1 case token.Action, token.Add => case token.Abort, token.Action: return 1 case token.Add",
			"parser/p.go infixPrec move in table: case token.Abort: return 1 case token.Action, token.Add => case token.Abort, token.Add: return 1 case token.Action",
			"parser/p.go infixPrec add to table: case token.Abort => case token.Abort, token.Illegal",
			"parser/p.go infixPrec add to table: case token.Action, token.Add => case token.Action, token.Add, token.Illegal",
		} {
			is.True(t, slices.Contains(keys, want), "missing: "+want+"\n"+strings.Join(keys, "\n"))
		}
		is.True(t, !slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(k, "token.Space") }), "the parser never sees a space")
		is.True(t, !slices.ContainsFunc(keys, func(k string) bool { return strings.Contains(k, "token.kinds") }), "kinds is not exported")
	})

	t.Run("should give mutants of the parser that each change the text, with unique keys, and that parse as Go", func(t *testing.T) {
		root, err := moduleRoot()
		is.NotError(t, err)
		dirs := []string{"internal/sql/token", "internal/sql/ast", "internal/sql/parser"}
		u, err := newUniverse(root, dirs)
		is.NotError(t, err)
		seen := map[string]bool{}
		for _, d := range dirs {
			files, err := goFiles(filepath.Join(root, d))
			is.NotError(t, err)
			for _, f := range files {
				rel, err := filepath.Rel(root, f)
				is.NotError(t, err)
				ms, err := u.generate(root, d, rel)
				is.NotError(t, err)
				src, err := os.ReadFile(f)
				is.NotError(t, err)
				for _, m := range ms {
					is.True(t, !seen[m.Key], "twice: "+m.Key)
					seen[m.Key] = true
					out := m.apply(src)
					is.True(t, string(out) != string(src), m.Key)
					if m.Op != "remove statement" && m.Op != "case element" && m.Op != "element" {
						_, err := parser.ParseFile(token.NewFileSet(), rel, out, 0)
						is.NotError(t, err, m.Key)
					}
				}
			}
		}
		is.True(t, len(seen) > 5000, "too few mutants")
	})
}

func TestEquivalent(t *testing.T) {
	t.Run("should name only mutants that the generator makes", func(t *testing.T) {
		root, err := moduleRoot()
		is.NotError(t, err)
		eq, err := readEquivalent(filepath.Join(root, "internal/sql/mutation/equivalent.txt"))
		is.NotError(t, err)
		dirs := []string{"internal/sql/token", "internal/sql/ast", "internal/sql/parser"}
		u, err := newUniverse(root, dirs)
		is.NotError(t, err)
		keys := map[string]bool{}
		for _, d := range dirs {
			files, err := goFiles(filepath.Join(root, d))
			is.NotError(t, err)
			for _, f := range files {
				rel, err := filepath.Rel(root, f)
				is.NotError(t, err)
				ms, err := u.generate(root, d, rel)
				is.NotError(t, err)
				for _, m := range ms {
					keys[m.Key] = true
				}
			}
		}
		for k := range eq {
			is.True(t, keys[k], "no such mutant: "+k)
		}
	})

	t.Run("should read a key and its reason, and refuse a key without a reason", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "eq.txt")
		is.NotError(t, os.WriteFile(path, []byte("# comment\n\nparser/p.go f operator: a < b => a <= b\n\tbecause\n"), 0o644))
		eq, err := readEquivalent(path)
		is.NotError(t, err)
		is.Equal(t, "because", eq["parser/p.go f operator: a < b => a <= b"])

		is.NotError(t, os.WriteFile(path, []byte("parser/p.go f operator: a < b => a <= b\n"), 0o644))
		_, err = readEquivalent(path)
		is.True(t, err != nil)
	})
}
