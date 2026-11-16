# SQL corpus

`internal/sql/corpus` holds SQL statements to test s4ql's SQL front end with: statements from SQLite's own test scripts, documentation, and fuzz corpora, and some of s4ql's own. It stores no expectations. Whether and how a statement parses is for SQLite to say when a test runs, through the oracle in [`internal/sql/oracle`](../oracle/README.md).

## Files

| File | Statements from | Statements | Bytes |
| --- | --- | --- | --- |
| `tcl.txt` | SQLite's TCL test scripts | 54,599 | 5,232,470 |
| `doc.txt` | SQLite's documentation | 1,176 | 127,752 |
| `fuzz.txt` | SQLite's fuzz corpora | 83,247 | 6,048,569 |
| `own.txt` | s4ql, written by hand | 219 | 6,100 |

The tool in `update/` generates the first three from the archives that `source.json` pins. Together the files take 11.4 MB on disk, and about 1.4 MB compressed with `gzip -9`. `.gitattributes` marks the three generated files as generated and turns off their diffs.

`corpus.All()` gives all 139,234 distinct statements, each as a `corpus.Entry` with its `Source` and its `SQL`, in the order tcl, doc, fuzz, own. A statement that is in more than one file comes once, with its first source. `corpus.Read(source)` gives the statements of one file.

## Where the statements come from

The generated files come from two archives of SQLite 3.54.1:

- the source archive, https://sqlite.org/2026/sqlite-src-3540100.zip
- the documentation archive, https://sqlite.org/2026/sqlite-doc-3540100.zip

`source.json` records the URL, the size, the SHA3-256 as published on https://sqlite.org/download.html, and the SHA-256 of each, as `internal/sqlite/update` does for the amalgamation. The tool checks all of them before it reads an archive.

**tcl** comes from the TCL scripts `test/*.test` and `ext/**/*.test` of the source archive, 1,541 files. The tool takes the argument in braces or quotes of the commands whose argument is SQL:

- `execsql`, `catchsql`, and `eval`, as in `db eval`
- `do_execsql_test`, `do_catchsql_test`, `do_eqp_test`, `do_eqp_execsql_test`, and `do_timed_execsql_test`, after the name of the test
- `sqlite3_prepare`, `sqlite3_prepare_v2`, and `sqlite3_prepare_v3`, after the database

It runs no TCL interpreter and does no substitution, so a `$var` stays as written.

**doc** comes from the `<pre>` blocks of the 847 HTML pages of the documentation archive, without tags, with HTML entities unescaped, and without the prompts of the sqlite3 shell. Only statements whose first token is a keyword that starts a statement are kept.

**fuzz** comes from the table `xsql` of the fuzz corpora `test/fuzzdata*.db` of the source archive, read through the binding in `internal/sqlite`.

## How the text is split

The tool splits a text as the sqlite3 shell does: after each semicolon that completes a statement by `sqlite3_complete`, which is `sqlite.Complete` in the binding. A trigger's body therefore stays with its trigger. Then, for each statement:

- Spaces, tabs, line breaks, and form feeds are trimmed from both ends. A vertical tab is not, because SQLite does not skip one where a token starts.
- The final semicolon is removed, and an empty statement is dropped.
- A statement longer than 1,000 bytes (tcl, doc) or 128 bytes (fuzz) is left out, to keep the corpus small.

A NUL byte ends a text for SQLite. The text after a NUL byte is therefore split on its own, and the statement in which the NUL byte stands is kept too, with the NUL byte and up to 8 bytes after it.

The statements are deduplicated across tcl, doc, and fuzz in that order, and each file is sorted by its bytes.

## Format

Each file holds one statement per line, in the form of `corpus.Encode`:

- Printable ASCII stands for itself, and so does a character from U+00A1 up, except a few that are invisible or spaces: U+00AD, U+2000-U+200F, U+2028-U+202F, U+205F-U+206F, U+3000, U+FEFF, and U+FFF0-U+FFFF.
- A backslash is `\\`, a line feed `\n`, a tab `\t`, and a carriage return `\r`.
- Every other byte is `\xHH`: a control character, a NUL byte, a byte that is not UTF-8, and each byte of one of the characters above, such as a byte order mark or a no-break space.
- A `#` at the start of a line is `\x23`.

The rule is a fixed list and not Unicode's tables, so the same statement is written the same with any version of Go. Empty lines and lines that start with `#` are comments. Each generated file starts with two comment lines. A test checks that every line of every file, `own.txt` too, is in this form. `.gitattributes` keeps line feeds in the files on every checkout, since a carriage return in a line is an error.

## Regenerating

Run `make corpus` from the repository root. It downloads both archives from sqlite.org, about 27 MB, checks them against `source.json`, and writes the three files within a few seconds. The same archives always give byte-identical files: the output is sorted and holds no timestamps.

The tool also copies `src/parse.y` of the source archive, unchanged, to [`internal/sql/parser/testdata/parse.y`](../parser/testdata/parse.y), where the tests of the parser read SQLite's grammar from. `-check` checks that copy too.

The target runs the tool without flags. The tool takes three:

```sh
go run ./internal/sql/corpus/update -archives DIR
go run ./internal/sql/corpus/update -check
go run ./internal/sql/corpus/update -version X.Y.Z
```

- `-archives DIR` reads the archives from `DIR` and downloads nothing. They are still checked.
- `-check` writes nothing, and fails if the files differ from what it would write. It refuses `-version`, which writes `source.json`.
- `-version X.Y.Z` first pins that version: it reads both archives' URLs, sizes, and SHA3-256 checksums from the PRODUCT lines of https://sqlite.org/download.html and rewrites `source.json`.

Another version of SQLite gives another corpus. Move the vendored amalgamation in `internal/sqlite` with it, so that the corpus and the oracle are of one version.

Before a regenerated corpus is committed, check that the S3 access key and secret of `.env` do not occur in it, as [AGENTS.md](../../../AGENTS.md) asks for bulk content. The check found neither in this corpus.

## own.txt

`own.txt` holds statements for what SQLite's tests may miss:

- whitespace and the byte order mark, NUL bytes, and comments
- strings, names, and blobs that are not closed
- forms of numbers, digit separators, and variables
- keywords as names, and precedence
- statements that stop early in an empty schema
- errors of actions before and after a syntax error
- the shapes of slice 1 of [docs/roadmap.md](../../../docs/roadmap.md), and empty statements

Add to it by hand, a statement per line in the format above. The update tool never touches it. Nothing in it says what SQLite does; the oracle does.

## Licence

SQLite's source code, tests, and documentation are in the public domain (https://sqlite.org/copyright.html). Nothing here comes from any other project. `own.txt` is s4ql's.
