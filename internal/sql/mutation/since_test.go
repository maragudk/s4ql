package main

import (
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"maragu.dev/is"
)

func TestParseDiff(t *testing.T) {
	t.Run("should give the changed lines of each file, and the places of its removals", func(t *testing.T) {
		diff := `diff --git a/internal/engine/a.go b/internal/engine/a.go
index 1111111..2222222 100644
--- a/internal/engine/a.go
+++ b/internal/engine/a.go
@@ -3 +3 @@ func f() {
-	return 1
+	return 2
@@ -10,2 +9,0 @@ func g() {
-	x++
-	y++
@@ -20,0 +19,3 @@ func h() {
+	a()
+	b()
+	c()
diff --git a/internal/engine/gone.go b/internal/engine/gone.go
deleted file mode 100644
--- a/internal/engine/gone.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package engine
-
-func gone() {}
diff --git a/internal/engine/new.go b/internal/engine/new.go
new file mode 100644
--- /dev/null
+++ b/internal/engine/new.go
@@ -0,0 +1,2 @@
+package engine
+func n() {}
`
		got, err := parseDiff(diff)
		is.NotError(t, err)
		is.EqualSlice(t, []hunk{{line: 3, count: 1}, {line: 9, count: 0}, {line: 19, count: 3}}, got["internal/engine/a.go"])
		is.EqualSlice(t, []hunk{{line: 1, count: 2}}, got["internal/engine/new.go"])
		_, ok := got["internal/engine/gone.go"]
		is.True(t, !ok, "a removed file has changes")
	})

	t.Run("should refuse a hunk header that it cannot read", func(t *testing.T) {
		_, err := parseDiff("+++ b/a.go\n@@ -1 +x @@\n")
		is.True(t, err != nil, "no error")
	})
}

func TestHunk_touches(t *testing.T) {
	tests := []struct {
		name     string
		h        hunk
		from, to int
		want     bool
	}{
		{name: "should touch a declaration that holds a changed line", h: hunk{line: 5, count: 1}, from: 3, to: 7, want: true},
		{name: "should touch a declaration whose first line changed", h: hunk{line: 3, count: 1}, from: 3, to: 7, want: true},
		{name: "should touch a declaration whose last line changed", h: hunk{line: 7, count: 1}, from: 3, to: 7, want: true},
		{name: "should touch a declaration that changed lines overlap", h: hunk{line: 1, count: 3}, from: 3, to: 7, want: true},
		{name: "should not touch a declaration before the changed lines", h: hunk{line: 8, count: 2}, from: 3, to: 7},
		{name: "should not touch a declaration after the changed lines", h: hunk{line: 1, count: 2}, from: 3, to: 7},
		{name: "should touch a declaration that a removal is inside", h: hunk{line: 4, count: 0}, from: 3, to: 7, want: true},
		{name: "should touch a declaration whose last line follows a removal", h: hunk{line: 6, count: 0}, from: 3, to: 7, want: true},
		{name: "should not touch a declaration that ends where a removal starts", h: hunk{line: 7, count: 0}, from: 3, to: 7},
		{name: "should not touch a declaration that starts after a removal", h: hunk{line: 2, count: 0}, from: 3, to: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			is.Equal(t, test.want, test.h.touches(test.from, test.to))
		})
	}
}

func TestChangedSince(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		is.NotError(t, err, string(out))
	}
	write := func(name, text string) {
		t.Helper()
		path := filepath.Join(root, name)
		is.NotError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		is.NotError(t, os.WriteFile(path, []byte(text), 0o644))
	}
	write("go.mod", "module "+module+"\n\ngo 1.24\n")
	write("internal/sql/token/kind.go", "package token\n\ntype Kind uint8\n\nconst (\n\tIllegal Kind = iota\n\tSpace\n)\n\n"+
		"const (\n\tAbort Kind = iota + Space + 1\n\tAction\n\tAdd\n\tkinds\n)\n")
	write("internal/sql/parser/p.go", "package parser\n\nfunc a(x int) bool {\n\treturn x < 1\n}\n\nfunc b(x int) bool {\n\treturn x > 2\n}\n\nvar (\n\tc = 3\n\td = 4\n)\n")
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "first")
	write("internal/sql/parser/p.go", "package parser\n\nfunc a(x int) bool {\n\treturn x < 1\n}\n\nfunc b(x int) bool {\n\treturn x >= 2\n}\n\nvar (\n\tc = 3\n\td = 5\n)\n")
	write("internal/sql/parser/q.go", "package parser\n\nfunc e(x int) bool {\n\treturn x == 7\n}\n")
	write("internal/sql/parser/q_test.go", "package parser\n\nfunc f(x int) bool {\n\treturn x == 8\n}\n")

	u, err := newUniverse(root, []string{"internal/sql/parser"})
	is.NotError(t, err)
	var ms []mutant
	for _, f := range []string{"internal/sql/parser/p.go", "internal/sql/parser/q.go"} {
		fms, err := u.generate(root, "internal/sql/parser", f)
		is.NotError(t, err)
		ms = append(ms, fms...)
	}

	t.Run("should select the mutants of the declarations that changed since the commit, also in a new file that git does not track yet", func(t *testing.T) {
		got, err := changedSince(root, "HEAD", ms)
		is.NotError(t, err)
		funcs := map[string]bool{}
		for _, m := range got {
			funcs[m.File+" "+m.Func] = true
		}
		is.EqualSlice(t, []string{"internal/sql/parser/p.go b", "internal/sql/parser/p.go d", "internal/sql/parser/q.go e"}, slices.Sorted(maps.Keys(funcs)))
	})

	t.Run("should refuse a commit that the repository does not have", func(t *testing.T) {
		_, err := changedSince(root, "0123456789abcdef0123456789abcdef01234567", ms)
		is.True(t, err != nil, "no error")
	})
}

func TestReadRound(t *testing.T) {
	t.Run("should read the whole commit of the last round from its file", func(t *testing.T) {
		root, err := moduleRoot()
		is.NotError(t, err)
		rev, err := readRound(filepath.Join(root, roundFile))
		is.NotError(t, err)
		is.True(t, regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(rev), rev)
	})

	t.Run("should refuse a file without a commit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "round.txt")
		is.NotError(t, os.WriteFile(path, []byte("# only a comment\n\n"), 0o644))
		_, err := readRound(path)
		is.True(t, err != nil, "no error")
	})
}

func TestBuilt(t *testing.T) {
	t.Run("should leave out the mutants of a file that the build of the tests leaves out", func(t *testing.T) {
		root := t.TempDir()
		is.NotError(t, os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644))
		is.NotError(t, os.WriteFile(filepath.Join(root, "b.go"), []byte("//go:build s4ql_never\n\npackage a\n"), 0o644))
		is.NotError(t, os.WriteFile(filepath.Join(root, "c_plan9.go"), []byte("package a\n"), 0o644))
		ms := []mutant{{File: "a.go", Key: "a"}, {File: "b.go", Key: "b"}, {File: "c_plan9.go", Key: "c"}}
		got, err := built(root, ms)
		is.NotError(t, err)
		var keys []string
		for _, m := range got {
			keys = append(keys, m.Key)
		}
		is.Equal(t, "a", strings.Join(keys, ","))
	})
}
