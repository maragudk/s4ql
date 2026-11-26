package main

import (
	"bytes"
	"fmt"
	"go/build"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// roundFile holds the commit of the last completed round, relative to the root of the module.
const roundFile = "internal/sql/mutation/round.txt"

// readRound gives the commit in the file at path: its first line that is not empty and does not
// start with #.
func readRound(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line, nil
		}
	}
	return "", fmt.Errorf("%v holds no commit", path)
}

// hunk of a diff, in the lines of the file as it is now: count lines from line are added or changed,
// or, with a count of 0, lines were removed between line and the line after it.
type hunk struct {
	line, count int
}

// touches reports whether the hunk changes the declaration on the lines from to to: it adds or
// changes one of them, or removes lines between two of them.
func (h hunk) touches(from, to int) bool {
	if h.count == 0 {
		return from <= h.line && h.line+1 <= to
	}
	return h.line <= to && from <= h.line+h.count-1
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// parseDiff reads the output of git diff -U0 with the prefixes a/ and b/, and gives the hunks of
// each file that is there now, by its path relative to the root of the repository. A line of +++ is
// the name of a file only in the header of the file's diff, before its first hunk, since an added
// line that starts with ++ looks the same in a hunk.
func parseDiff(diff string) (map[string][]hunk, error) {
	hunks := map[string][]hunk{}
	file := ""
	header := false
	for line := range strings.Lines(diff) {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "diff --git "):
			file, header = "", true
		case header && strings.HasPrefix(line, "+++ "):
			if name, ok := strings.CutPrefix(line, "+++ b/"); ok {
				file = name
			}
		case strings.HasPrefix(line, "@@ "):
			header = false
			m := hunkHeader.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("the line %q of the diff is not a hunk header", line)
			}
			if file == "" {
				continue
			}
			h := hunk{count: 1}
			h.line, _ = strconv.Atoi(m[1])
			if m[2] != "" {
				h.count, _ = strconv.Atoi(m[2])
			}
			hunks[file] = append(hunks[file], h)
		}
	}
	return hunks, nil
}

// changedSince gives the mutants of ms whose declaration changed since the commit rev, in the tree
// of root, which is the root of its git repository: a declaration that holds a line that git diff
// rev adds or changes, or the place of a removal. The diff is of the working tree, so that a change
// that is not committed counts too, and a file that git does not track counts as new.
func changedSince(root, rev string, ms []mutant) ([]mutant, error) {
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("git %v: %w: %v", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return string(out), nil
	}
	diff, err := git("diff", "-U0", "--relative", "--no-color", "--no-ext-diff", "--no-renames", "--src-prefix=a/", "--dst-prefix=b/", rev+"^{commit}", "--", "*.go")
	if err != nil {
		return nil, err
	}
	hunks, err := parseDiff(diff)
	if err != nil {
		return nil, err
	}
	untracked, err := git("ls-files", "--others", "--exclude-standard", "--", "*.go")
	if err != nil {
		return nil, err
	}
	for f := range strings.Lines(untracked) {
		hunks[strings.TrimSpace(f)] = []hunk{{line: 1, count: 1 << 30}}
	}
	var changed []mutant
	for _, m := range ms {
		for _, h := range hunks[filepath.ToSlash(m.File)] {
			if h.touches(m.declFrom, m.declTo) {
				changed = append(changed, m)
				break
			}
		}
	}
	return changed, nil
}

// built gives the mutants of ms in files that the build of the tests compiles on this machine, of
// the module in root. The generator reads every file of a package, whatever its build constraints,
// and a mutant of a file that the build leaves out, such as the binding's file for the browser,
// survives, since no test runs it.
func built(root string, ms []mutant) ([]mutant, error) {
	ok := map[string]bool{}
	var kept []mutant
	for _, m := range ms {
		match, seen := ok[m.File]
		if !seen {
			var err error
			match, err = build.Default.MatchFile(filepath.Join(root, filepath.Dir(m.File)), filepath.Base(m.File))
			if err != nil {
				return nil, err
			}
			ok[m.File] = match
		}
		if match {
			kept = append(kept, m)
		}
	}
	return kept, nil
}
