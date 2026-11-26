# Diary: a differential test and a fuzz target for `stmt.Split`

`stmt.Split` cuts a text of several statements the way mattn's `Exec` does with SQLite's prepare. A reviewer once compared it with SQLite on 400,000 texts with a throwaway test; nothing permanent did. This task builds that check: a generator of hard texts, a differential test against this repository's SQLite binding read two ways, a fuzz target, a look for a guard of the split's cost, and four planted faults.

## Step 1: The generator, the differential test, and the fuzz target

**Author:** builder (split-fuzz)

### Prompt Context

**Verbatim prompt:** "Your ONE small task: **a permanent differential test and a fuzz target for `stmt.Split`**, the function that cuts a text of several statements the way SQLite's own prepare does (`internal/sql/stmt/split.go`, with `parser.ParseAt` in `internal/sql/parser/parser.go`)." The brief then lists five parts: a generator of the pieces that make splitting hard; a differential test comparing `stmt.Split` with the SQLite binding read as mattn does and as SQLite alone does; `FuzzSplit` with the same check, run five minutes once; a guard for the cost of the split that does not depend on time, if a clean one exists; and four planted faults, each failing the test.
**Interpretation:** a permanent test in `/internal/sql/stmt/split_test.go`, no change to `split.go` unless a fault shows up, docs in `/docs/todo/several-statements.md`, `/docs/todo/driver.md`, and `/internal/sql/stmt/README.md`.
**Inferred intent:** trust in the split, the one piece of several statements in one `Exec` that only a throwaway test had compared with SQLite.

### What I did

- Moved the hand cases of `TestSplit` into package-level variables, so that `splitHandTexts` can seed `FuzzSplit` with them. The subtests are unchanged otherwise.
- `splitText` writes one to five statements from three lists (supported, with semicolons in strings, blobs, names in double quotes, and comments; unsupported, CREATE TRIGGER with semicolons in its body, names in brackets and backticks with semicolons, `vacuum`, `select 1`, GROUP BY; and broken, `selec`, `insert into`, `select`, unclosed `'`, `"`, `[`, `` ` ``, `x'3`, `/* ;`, `-- ;`), with spaces, comments, and empty statements around them, seven times in eight SQLite's spaces and closed comments, else `\v`, U+00A0, U+0085, U+2028, U+3000, U+FEFF, control bytes, and unclosed comments; one space piece in 60 is a NUL, `\xff`, or `\xc3`. One statement in eight has one of its spaces replaced by a space piece, and one separator in twelve is missing.
- `readText` reads a text as SQLite prepares it, one statement after another from the tail of the one before, with `oracle.Judge` (which prepares through `internal/sqlite` and handles CREATE TRIGGER and errors of names): as SQLite alone does, and as mattn does, with `strings.TrimSpace` on each tail. Each step records the verdict, the message, the offset, and the range of the statement's tokens by SQLite's own tokenizer (`sqlite.Tokenize`), without spaces, comments, and leading empty statements. The readings are compared by those, not by the tail, since mattn's last tail ends before the trimmed spaces.
- `checkSplit` checks: the parts follow one another and hold their statements; a NUL or invalid UTF-8 is refused at the first; where the readings agree, every cut, every statement's bounds, SQLite's syntax error with its offset, or "not supported" inside a statement that SQLite parses; where they differ, a refusal of spaces or "not supported" before the first difference; and the two refusals of spaces at their exact offsets, computed from SQLite's tokens (`sqliteTrimmedEnd`, `sqliteTrimmedStart`).
- `TestSplit_differential` runs 100,000 texts, 10,000 under `-short`, race, or asan, and requires ten outcomes. `FuzzSplit` runs `checkSplit` on any text.

### Why

The rule "where the readings agree, cut as SQLite; where they differ, refuse" is what the agreed refusal means, and it is stated by SQLite only: the binding's prepare, the oracle's verdicts, and SQLite's tokenizer. s4ql's own tokenizer and `stmt.Check` appear only in the decision of the end refusal (is the first statement supported), as the decided rule itself depends on that.

### What worked

The whole check is cheap: 10,000 texts take about 0.9 s, 100,000 about 8.5 s. On 100,000 texts at seed 0: 3,143 NUL byte, 6,253 invalid UTF-8, 3,791 no statement, 16,597 one statement, 22,009 several statements, 27,778 syntax errors, 14,900 not supported, 734 not supported before the readings differ, 2,024 space after a statement, 2,760 space at the end, and 11 where SQLite does not say; 105,849 parts. No fault of s4ql: every cut and error matched SQLite or a recorded refusal.

### What didn't work

- The first version of the rule required that a refusal of spaces come only where the two readings differ. It failed on `"insert into t (v) values ('a;b');/* ; */create table \"t;1\" (v text primary key) strict, without rowid/* ; */;;; create table t (v int\ufeff;select * from t;\v\v"` with `gives s4ql: not supported: space at the end of the text that SQLite reads as part of a token (at offset 153), but SQLite alone and mattn read it alike`. Both readings stop at the syntax error of the third statement and never reach the end. That is the decided behaviour, not a fault: `Split` judges the end once a tail follows a supported first statement, before it reads any later statement, as the oracle of the driver check (`spaceRule`) does too. The check now states that rule with SQLite's tokens, and keeps "only where the readings differ" for the start of a tail.
- `checkNotSupported` failed on `"... without rowid;/* "`: `the offset 221 is neither the start nor the end of a token`. The end refusal's offset is inside the token that the trim cuts (`/* `, `'abc  `), as `TestSplit` pins, so the end refusal is checked for its form and vocabulary only.
- The Write and Edit tools turned the `\u00a0`-style escapes I typed into literal characters, and once lost the U+2028 of a hand case of `TestSplit`, which then failed with `Expected "0", but got "2"`. A small Python script in the scratchpad turned every non-ASCII character of the file back into an escape.
- The first weights gave 36% syntax errors and only 557 texts of several statements in 10,000; names in brackets and backticks turned out to be unsupported by s4ql (`name in brackets`), so they moved to the unsupported list, and odd spaces became one piece in eight.

### What I learned

SQLite's `sqlite3_prepare` reading and mattn's differ only by the trim, so comparing the readings by the token range of each statement, by SQLite's tokenizer, finds every difference that matters without comparing tails.

### What was tricky

The end refusal is decided for the whole text up front, so it is broader than "the readings differ": a text whose third statement is a syntax error before a trailing `\v` is refused, where mattn would run the first two statements and fail at the third. This is a refusal, which the rule of the project allows, and it is what the agreed decision says, but the lead may want to know that it is broader than needed.

### What warrants review

`checkSplit` in `/internal/sql/stmt/split_test.go`: the comparison after the readings differ (only statements before the first difference are compared) and the two exact-offset rules.

### Future work

None from this step.
