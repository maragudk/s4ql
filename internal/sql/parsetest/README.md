# Parser test harness

`internal/sql/parsetest` compares a SQL parser under test with SQLite, through the oracle in [`internal/sql/oracle`](../oracle/README.md). It compares three things: whether the first statement of a text parses, the exact message and byte offset of a syntax error, and where the statement ends.

s4ql's own parser in [`internal/sql/parser`](../parser/README.md) is tested with this package, in the first of the four checks that [docs/todo.md](../../../docs/todo.md) lists. [What the parser must match](../oracle/README.md#what-the-parser-must-match) in the oracle's README lists the facts about SQLite that are easy to get wrong.

## Testing a parser

Implement `parsetest.Parser`, or wrap a function in `parsetest.ParserFunc`:

```go
type Parser interface {
	Parse(src string) Result
}
```

`Parse` is given the whole text, which may hold several statements, NUL bytes, and bytes that are not UTF-8. It answers for the first statement as SQLite would: it reads the text up to its first NUL byte, skips empty statements, and stops at the first syntax error. The `Result` has four fields:

- `OK` is true if the statement parses.
- `Message` is the syntax error in SQLite's words, such as `near "frm": syntax error`.
- `Offset` is the byte offset of the syntax error, or -1 where SQLite gives none, as for `incomplete input`.
- `Tail` is the offset of the first byte after a statement that parses: after its semicolon, or at the end of the text.

`Message` and `Offset` are compared only where SQLite gives a syntax error, and `Tail` only where the statement parses.

A test of a parser runs it on the corpus in [`internal/sql/corpus`](../corpus/README.md) and on mutants of it, and a fuzz test checks one text at a time. Here `parse` is a `func(src string) parsetest.Result` that calls the parser:

```go
func TestParse(t *testing.T) {
	var sqls []string
	for i, e := range corpus.All() {
		sqls = append(sqls, e.SQL)
		if i%50 == 0 {
			sqls = append(sqls, parsetest.Mutants(e.SQL, uint64(i), 3)...)
		}
	}
	parsetest.Test(t, parsetest.ParserFunc(parse), sqls)
}

func FuzzParse(f *testing.F) {
	sqls, err := corpus.Read("own")
	if err != nil {
		f.Fatal(err)
	}
	for _, sql := range sqls {
		f.Add(sql)
	}

	f.Fuzz(func(t *testing.T, src string) {
		if d, found, _ := parsetest.Check(parsetest.ParserFunc(parse), src); found {
			t.Fatal(d)
		}
	})
}
```

`make fuzz` runs the fuzz tests of the directories that the `Makefile` lists, so add the parser's directory there.

The functions, from the whole run down to one comparison:

- `Test(t, p, sqls)` runs `Run`, and fails the test with the report if the parser disagrees with SQLite on any text.
- `Run(p, sqls)` compares the parser with SQLite on every text and gives a `Report`.
- `Check(p, src)` judges one text and compares. It gives the disagreement, whether there is one, and SQLite's verdict.
- `Compare(src, verdict, result)` compares a result with a verdict that is already there.
- `Oracle()` is SQLite itself as a `Parser`, for tests of the harness and for stand-ins.

## The report

A disagreement has one of five kinds:

- `AcceptsInvalid`: the parser accepts a statement that SQLite rejects with a syntax error. It is the most dangerous kind, since s4ql must reject what it cannot read as SQLite does.
- `RejectsValid`: the parser rejects a statement that SQLite parses.
- `Message`: both reject the statement, with different messages.
- `Offset`: both reject it with the same message, at different offsets.
- `Tail`: both parse it, and differ on where it ends.

`Run` groups the disagreements by kind and key, so that those with the same likely cause can be worked through together. The key holds the pattern of SQLite's message, of the parser's, or of both, as `Pattern` gives it: the quoted token of a syntax error becomes its kind, as in `near ID: syntax error` or `unrecognized token: STRING...`, and in any other message the part after the first colon becomes `...`. For an offset or a tail the key also holds the difference: `offset +1`, `SQLite -1` or `parser -1` where one side gives no offset, and `tail +9`.

The groups are sorted with the largest first, and each keeps its five shortest texts as examples. Texts on which SQLite does not say are never compared. They are counted apart, grouped by the reason and the pattern of SQLite's message. The report is the same for the same texts in any order.

This is the report for a parser that accepts everything, on four texts:

```
4 texts: 0 agree, 3 disagree, SQLite does not say on 1

accepts what SQLite rejects, incomplete input: 1
  accepts what SQLite rejects: "select * from": SQLite: syntax error "incomplete input" at -1, tail 13; parser: ok=true "" at -1, tail 0

accepts what SQLite rejects, near ID: syntax error: 1
  accepts what SQLite rejects: "select 1 frm t": SQLite: syntax error "near \"t\": syntax error" at 13, tail 14; parser: ok=true "" at -1, tail 0

other tail, tail -9: 1
  other tail: "select 1; select 2": SQLite: parses "" at -1, tail 9; parser: ok=true "" at -1, tail 0

SQLite does not say:
  stopped early: unknown table option: ...: 1, for example "create table t (x) foo, bar"
```

## Mutants

`Mutants(src, seed, n)` gives `n` texts that each differ from `src` by one operation on one token, for inputs near valid ones:

- delete the token
- duplicate it
- swap it with the next token
- replace it by a token of another kind
- insert a token before it

The tokens to replace by or insert come from a pool of every keyword, every operator and punctuation, a literal of each kind, and names quoted in each way. The token and the operation are chosen at random from the seed and the text, so the same seed and text always give the same mutants. Only the text before the first NUL byte is mutated, since SQLite reads no further.

The harness uses no property-based testing library. The repository has none, and seeded mutants and fuzzing cover the generation of inputs that is needed now.

## Proof that the harness works

`parsetest_test.go` tests the harness with SQLite and with stand-ins, apart from any parser:

- SQLite compared with itself gives no disagreement, on the whole corpus and on 8,331 mutants, three of every fiftieth entry.
- A stand-in parser is SQLite's verdict with one known fault. Each fault is reported with the right count in the right group: a parser that accepts everything, one that is off by one in the offset, one that gives an offset where SQLite gives none, one that differs in one message, one that rejects valid statements, and one that ignores the semicolon.
- `FuzzMutants` checks that mutants are reproducible and that SQLite agrees with itself on them. `make fuzz` runs it.

## Run times

Measured on an Apple M4; the numbers under the address sanitizer are from a Linux container in Docker on the same machine.

- The whole corpus through the oracle takes about 3 s.
- `go test ./internal/sql/parsetest` takes 4 to 5 s, and about 10 s with `-race`.
- Under the address sanitizer the tests on the corpus use every tenth entry, as with `-short`, and the package takes about 14 s.

`Run` judges the texts one after the other. SQLite takes global locks when it opens a connection and when it allocates memory: with 10 goroutines the corpus took 9.5 s instead of 2.9 s.
