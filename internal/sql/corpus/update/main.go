// Command update regenerates the corpus of SQL statements in internal/sql/corpus from the archives of
// SQLite that internal/sql/corpus/source.json pins.
//
// Usage, from the repository root:
//
//	go run ./internal/sql/corpus/update
//	go run ./internal/sql/corpus/update -archives DIR
//	go run ./internal/sql/corpus/update -check
//	go run ./internal/sql/corpus/update -version 3.54.1
//
// It downloads the source archive and the documentation archive of the pinned version from
// sqlite.org, or reads them from DIR with -archives, and checks the size, SHA3-256, and SHA-256 of
// each against source.json. It extracts SQL from the TCL test scripts (test/*.test and ext/**/*.test
// in the source archive), from the <pre> blocks of the documentation's HTML pages, and from the table
// xsql of the fuzz corpora test/fuzzdata*.db, splits it into statements, deduplicates them, and
// writes tcl.txt, doc.txt, and fuzz.txt, each sorted by its bytes. It also copies src/parse.y of the
// source archive to internal/sql/parser/testdata/parse.y, unchanged, for the tests of the parser. The
// same archives always give the same files. With -check, it writes nothing and fails if the files differ from what it would write.
//
// With -version, it first pins that version: it reads the URLs, sizes, and SHA3-256 checksums of
// both archives from the PRODUCT lines of https://sqlite.org/download.html and rewrites source.json.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"maragu.dev/s4ql/internal/sql/corpus"
	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

const (
	dir          = "internal/sql/corpus"
	grammarPath  = "internal/sql/parser/testdata/parse.y"
	manifestPath = "internal/sql/corpus/source.json"
	downloadPage = "https://sqlite.org/download.html"
)

// maxLen of a statement in bytes, by source. Longer statements are left out, which keeps the corpus
// small. Fuzz inputs are held shorter, because there are many more of them and the long ones mostly
// repeat one construct.
var maxLen = map[string]int{"tcl": 1000, "doc": 1000, "fuzz": 128}

// manifest pins the archives that the corpus is extracted from.
type manifest struct {
	Version  string    `json:"version"`
	Archives []archive `json:"archives"`
}

// archive of SQLite on sqlite.org.
type archive struct {
	URL     string `json:"url"`
	Size    int    `json:"size"`
	SHA3256 string `json:"sha3_256"`
	SHA256  string `json:"sha256"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	version := flag.String("version", "", "SQLite version to pin, such as 3.54.1")
	archives := flag.String("archives", "", "directory to read the archives from instead of downloading them")
	check := flag.Bool("check", false, "check that the corpus files are what this would write, and write nothing")
	flag.Parse()

	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("run from the repository root: %w", err)
	}

	if *version != "" {
		if *check {
			return errors.New("-check writes nothing, so it cannot pin a version with -version")
		}
		if err := pin(*version, *archives); err != nil {
			return err
		}
	}

	b, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	number, err := versionNumber(m.Version)
	if err != nil {
		return err
	}
	srcName := fmt.Sprintf("sqlite-src-%v.zip", number)
	docName := fmt.Sprintf("sqlite-doc-%v.zip", number)

	contents := map[string][]byte{}
	for _, a := range m.Archives {
		name := path.Base(a.URL)
		if name != srcName && name != docName {
			return fmt.Errorf("%v is neither %v nor %v", a.URL, srcName, docName)
		}
		data, err := fetch(a.URL, *archives)
		if err != nil {
			return err
		}
		if err := checkArchive(data, a); err != nil {
			return fmt.Errorf("%v: %w", name, err)
		}
		contents[name] = data
	}
	if contents[srcName] == nil || contents[docName] == nil {
		return fmt.Errorf("%v must pin %v and %v", manifestPath, srcName, docName)
	}

	sources, err := extractAll(contents[srcName], contents[docName], m.Version)
	if err != nil {
		return err
	}

	outputs := map[string][]byte{}
	for _, source := range []string{"tcl", "doc", "fuzz"} {
		outputs[filepath.Join(dir, source+".txt")] = render(source, m.Version, sources[source])
	}
	grammar, err := extractGrammar(contents[srcName], m.Version)
	if err != nil {
		return err
	}
	outputs[grammarPath] = grammar

	if *check {
		for name, want := range outputs {
			got, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				return fmt.Errorf("%v differs from what the update would write; run it", name)
			}
		}
		fmt.Println("The corpus is up to date")
		return nil
	}

	for name, b := range outputs {
		if err := os.WriteFile(name+".tmp", b, 0o644); err != nil {
			return err
		}
	}
	for name := range outputs {
		if err := os.Rename(name+".tmp", name); err != nil {
			return err
		}
	}
	for _, source := range []string{"tcl", "doc", "fuzz"} {
		fmt.Printf("%v: %v statements\n", source, len(sources[source]))
	}
	return nil
}

// render the statements of a source as its corpus file.
func render(source, version string, sqls []string) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "# Code generated by internal/sql/corpus/update. DO NOT EDIT.\n")
	fmt.Fprintf(&b, "# SQL statements from the %v of SQLite %v, which is in the public domain. See README.md.\n", map[string]string{
		"tcl":  "TCL test scripts",
		"doc":  "documentation",
		"fuzz": "fuzz corpora test/fuzzdata*.db",
	}[source], version)
	for _, sql := range sqls {
		b.WriteString(corpus.Encode(sql))
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// extractAll statements from the archives, by source, deduplicated across the sources in the order
// tcl, doc, fuzz, and sorted by their bytes within each.
func extractAll(src, doc []byte, version string) (map[string][]string, error) {
	number, err := versionNumber(version)
	if err != nil {
		return nil, err
	}

	srcZip, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, err
	}
	docZip, err := zip.NewReader(bytes.NewReader(doc), int64(len(doc)))
	if err != nil {
		return nil, err
	}
	srcPrefix := fmt.Sprintf("sqlite-src-%v/", number)
	docPrefix := fmt.Sprintf("sqlite-doc-%v/", number)

	found := map[string][]string{}
	for _, f := range srcZip.File {
		name, ok := strings.CutPrefix(f.Name, srcPrefix)
		if !ok {
			continue
		}
		switch {
		case strings.HasSuffix(name, ".test") && (strings.HasPrefix(name, "test/") || strings.HasPrefix(name, "ext/")):
			b, err := readFile(f)
			if err != nil {
				return nil, err
			}
			for _, block := range tclBlocks(string(b)) {
				found["tcl"] = append(found["tcl"], statements(block)...)
			}
		case fuzzData.MatchString(name):
			b, err := readFile(f)
			if err != nil {
				return nil, err
			}
			scripts, err := fuzzScripts(b)
			if err != nil {
				return nil, fmt.Errorf("%v: %w", name, err)
			}
			for _, script := range scripts {
				found["fuzz"] = append(found["fuzz"], statements(script)...)
			}
		}
	}
	for _, f := range docZip.File {
		name, ok := strings.CutPrefix(f.Name, docPrefix)
		if !ok || !strings.HasSuffix(name, ".html") {
			continue
		}
		b, err := readFile(f)
		if err != nil {
			return nil, err
		}
		for _, block := range preBlocks(string(b)) {
			for _, sql := range statements(block) {
				if startsStatement(sql) {
					found["doc"] = append(found["doc"], sql)
				}
			}
		}
	}

	seen := map[string]bool{}
	result := map[string][]string{}
	for _, source := range []string{"tcl", "doc", "fuzz"} {
		var sqls []string
		for _, sql := range found[source] {
			if len(sql) > maxLen[source] || seen[sql] {
				continue
			}
			seen[sql] = true
			sqls = append(sqls, sql)
		}
		slices.Sort(sqls)
		result[source] = sqls
	}
	return result, nil
}

// extractGrammar gives src/parse.y of the source archive, unchanged: the grammar of SQLite's parser,
// which the tests of internal/sql/parser read.
func extractGrammar(src []byte, version string) ([]byte, error) {
	number, err := versionNumber(version)
	if err != nil {
		return nil, err
	}
	srcZip, err := zip.NewReader(bytes.NewReader(src), int64(len(src)))
	if err != nil {
		return nil, err
	}
	name := fmt.Sprintf("sqlite-src-%v/src/parse.y", number)
	for _, f := range srcZip.File {
		if f.Name == name {
			return readFile(f)
		}
	}
	return nil, fmt.Errorf("the source archive has no %v", name)
}

// fuzzData is the name of a fuzz corpus in the source archive.
var fuzzData = regexp.MustCompile(`^test/fuzzdata\d+\.db$`)

func readFile(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// tclCommand is a TCL command whose argument is SQL, with the number of arguments before the SQL.
var tclCommand = regexp.MustCompile(`(?:^|[\s\[{;])(execsql|catchsql|eval|do_execsql_test|do_catchsql_test|do_eqp_test|do_eqp_execsql_test|do_timed_execsql_test|sqlite3_prepare|sqlite3_prepare_v2|sqlite3_prepare_v3)[ \t]`)

var tclSkip = map[string]int{
	"execsql": 0, "catchsql": 0, "eval": 0,
	"do_execsql_test": 1, "do_catchsql_test": 1, "do_eqp_test": 1, "do_eqp_execsql_test": 1, "do_timed_execsql_test": 1,
	"sqlite3_prepare": 1, "sqlite3_prepare_v2": 1, "sqlite3_prepare_v3": 1,
}

// tclBlocks of SQL in a TCL test script: the argument in braces or quotes of the commands above,
// after the arguments they take before the SQL, such as the name of a test. It reads TCL's braces and
// quotes, and does no substitution.
func tclBlocks(script string) []string {
	var blocks []string
	for _, m := range tclCommand.FindAllStringSubmatchIndex(script, -1) {
		command := script[m[2]:m[3]]
		i := m[3]
		for range tclSkip[command] {
			_, next, ok := tclWord(script, i)
			if !ok {
				break
			}
			i = next
		}
		word, _, ok := tclWord(script, i)
		if ok && word.quoted {
			blocks = append(blocks, word.text)
		}
	}
	return blocks
}

// tclArg is a word of TCL.
type tclArg struct {
	text   string
	quoted bool // in braces or double quotes
}

// tclWord at i, after spaces, tabs, and escaped line breaks, and the offset after it.
func tclWord(s string, i int) (tclArg, int, bool) {
	for i < len(s) {
		if s[i] == ' ' || s[i] == '\t' {
			i++
		} else if strings.HasPrefix(s[i:], "\\\n") {
			i += 2
		} else {
			break
		}
	}
	if i >= len(s) || s[i] == '\n' || s[i] == ';' || s[i] == ']' {
		return tclArg{}, i, false
	}
	switch s[i] {
	case '{':
		depth := 0
		for j := i; j < len(s); j++ {
			switch s[j] {
			case '\\':
				j++
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					return tclArg{text: s[i+1 : j], quoted: true}, j + 1, true
				}
			}
		}
		return tclArg{}, len(s), false
	case '"':
		var b strings.Builder
		for j := i + 1; j < len(s); j++ {
			switch s[j] {
			case '\\':
				if j+1 < len(s) {
					j++
					switch s[j] {
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case '\n':
						b.WriteByte(' ')
					default:
						b.WriteByte(s[j])
					}
				}
			case '"':
				return tclArg{text: b.String(), quoted: true}, j + 1, true
			default:
				b.WriteByte(s[j])
			}
		}
		return tclArg{}, len(s), false
	default:
		j := i
		for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != '\n' && s[j] != ';' && s[j] != ']' {
			j++
		}
		return tclArg{text: s[i:j]}, j, true
	}
}

var (
	preBlock = regexp.MustCompile(`(?s)<pre[^>]*>(.*?)</pre>`)
	htmlTag  = regexp.MustCompile(`(?s)<[^>]*>`)
	prompt   = regexp.MustCompile(`(?m)^(?:sqlite|\s*\.\.\.)> ?`)
)

// preBlocks of an HTML page, without tags, with entities unescaped, and without the prompts of the
// sqlite3 shell.
func preBlocks(page string) []string {
	var blocks []string
	for _, m := range preBlock.FindAllStringSubmatch(page, -1) {
		text := html.UnescapeString(htmlTag.ReplaceAllString(m[1], ""))
		blocks = append(blocks, prompt.ReplaceAllString(text, ""))
	}
	return blocks
}

// startsStatement tells whether the first token of sql is a keyword that can start a statement.
func startsStatement(sql string) bool {
	for _, t := range token.Tokens(sql) {
		switch t.Kind {
		case token.Space, token.Comment:
			continue
		case token.Select, token.Values, token.With, token.Insert, token.Replace, token.Update, token.Delete,
			token.Create, token.Drop, token.Alter, token.Pragma, token.Begin, token.Commit, token.End,
			token.Rollback, token.Savepoint, token.Release, token.Attach, token.Detach, token.Analyze,
			token.Reindex, token.Vacuum, token.Explain:
			return true
		default:
			return false
		}
	}
	return false
}

// fuzzScripts in the table xsql of one of SQLite's fuzz corpora, in the order of their ids.
func fuzzScripts(db []byte) ([]string, error) {
	tmp, err := os.MkdirTemp("", "s4ql-corpus-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	p := filepath.Join(tmp, "fuzz.db")
	if err := os.WriteFile(p, db, 0o600); err != nil {
		return nil, err
	}

	c, err := sqlite.Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	s, err := c.Prepare("select sqltext from xsql order by sqlid")
	if err != nil {
		return nil, err
	}
	defer func() { _ = s.Finalize() }()

	var scripts []string
	for {
		ok, err := s.Step()
		if err != nil {
			return nil, err
		}
		if !ok {
			return scripts, nil
		}
		if s.ColumnType(0) != sqlite.TypeNull {
			scripts = append(scripts, s.ColumnText(0))
		}
	}
}

// statements of a text, split as the sqlite3 shell splits its input: after each semicolon that
// completes a statement, by [sqlite.Complete]. Each is trimmed of spaces, tabs, line breaks, and form
// feeds at both ends, and loses its final semicolon. A NUL byte ends a text for SQLite, so the text
// after a NUL byte is split on its own, and the statement in which a NUL byte stands is also kept
// with the NUL byte and a few bytes after it.
func statements(text string) []string {
	var sqls []string
	segments := strings.Split(text, "\x00")
	for i, segment := range segments {
		start := 0
		for _, t := range token.Tokens(segment) {
			if t.Kind != token.Semi || !sqlite.Complete(segment[start:t.Offset+1]) {
				continue
			}
			sqls = appendTrimmed(sqls, segment[start:t.Offset])
			start = t.Offset + 1
		}
		rest := segment[start:]
		sqls = appendTrimmed(sqls, rest)
		if i+1 < len(segments) && strings.Trim(rest, " \t\n\f\r;") != "" {
			after := segments[i+1]
			if len(after) > 8 {
				after = after[:8]
			}
			sqls = appendTrimmed(sqls, rest+"\x00"+after)
		}
	}
	return sqls
}

func appendTrimmed(sqls []string, sql string) []string {
	sql = strings.Trim(sql, " \t\n\f\r")
	if sql == "" || strings.Trim(sql, ";") == "" {
		return sqls
	}
	return append(sqls, sql)
}

// pin the version: read both archives from the download page, check them, and write source.json.
func pin(version, archives string) error {
	number, err := versionNumber(version)
	if err != nil {
		return err
	}
	page, err := get(downloadPage)
	if err != nil {
		return err
	}
	m := manifest{Version: version}
	for _, name := range []string{fmt.Sprintf("sqlite-src-%v.zip", number), fmt.Sprintf("sqlite-doc-%v.zip", number)} {
		p, err := findProduct(page, version, name)
		if err != nil {
			return err
		}
		a := archive{URL: "https://sqlite.org/" + p.path, Size: p.size, SHA3256: p.sha3}
		data, err := fetch(a.URL, archives)
		if err != nil {
			return err
		}
		if err := checkArchive(data, a); err != nil {
			return fmt.Errorf("%v: %w", name, err)
		}
		a.SHA256 = sha256Hex(data)
		m.Archives = append(m.Archives, a)
	}
	b, err := json.Marshal(m, jsontext.WithIndent("\t"), json.Deterministic(true))
	if err != nil {
		return err
	}
	return os.WriteFile(manifestPath, append(b, '\n'), 0o644)
}

// fetch the archive at url, or read it from the directory archives by the URL's base name.
func fetch(url, archives string) ([]byte, error) {
	if archives != "" {
		return os.ReadFile(filepath.Join(archives, path.Base(url)))
	}
	return get(url)
}

// checkArchive has the recorded size and checksums. An empty SHA-256 is not checked, which is only
// the case while a version is pinned.
func checkArchive(data []byte, a archive) error {
	if !strings.HasPrefix(a.URL, "https://sqlite.org/") {
		return fmt.Errorf("%v is not on https://sqlite.org/", a.URL)
	}
	if len(data) != a.Size {
		return fmt.Errorf("size is %v, want %v", len(data), a.Size)
	}
	if got := sha3Hex(data); got != a.SHA3256 {
		return fmt.Errorf("SHA3-256 is %v, want %v", got, a.SHA3256)
	}
	if got := sha256Hex(data); a.SHA256 != "" && got != a.SHA256 {
		return fmt.Errorf("SHA-256 is %v, want %v", got, a.SHA256)
	}
	return nil
}

type product struct {
	path string
	size int
	sha3 string
}

// findProduct in the PRODUCT lines of the SQLite download page, for the file of the given version.
func findProduct(page []byte, version, name string) (product, error) {
	productLine := regexp.MustCompile(`(?m)^PRODUCT,([^,]+),([^,]+),(\d+),([0-9a-f]{64})$`)
	for _, match := range productLine.FindAllSubmatch(page, -1) {
		if string(match[1]) != version || path.Base(string(match[2])) != name {
			continue
		}
		size, err := strconv.Atoi(string(match[3]))
		if err != nil {
			return product{}, err
		}
		return product{path: string(match[2]), size: size, sha3: string(match[4])}, nil
	}
	return product{}, fmt.Errorf("%v not on %v", name, downloadPage)
}

// versionNumber in the form SQLite uses in file names, such as 3540100 for 3.54.1.
func versionNumber(version string) (int, error) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return 0, fmt.Errorf("version %q is not of the form X.Y.Z", version)
	}
	var n [3]int
	for i, p := range parts {
		v, err := strconv.Atoi(p)
		if err != nil || v < 0 || v > 99 {
			return 0, fmt.Errorf("version %q is not of the form X.Y.Z", version)
		}
		n[i] = v
	}
	return n[0]*1000000 + n[1]*10000 + n[2]*100, nil
}

func get(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	res, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get %v: %v", url, res.Status)
	}
	return io.ReadAll(res.Body)
}

func sha3Hex(b []byte) string {
	sum := sha3.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
