# Shape check of the syntax tree

`internal/sql/shapetest` checks that the tree of [`internal/sql/parser`](../parser/README.md) has the shape that SQLite gives the same text: operators are grouped as SQLite groups them, and every clause is on the node that SQLite attaches it to. The judge is the vendored SQLite 3.54.1.

The parser already agrees with SQLite on whether a text parses, through [`internal/sql/parsetest`](../parsetest/README.md). That says nothing about the tree. A parser can accept the right language and still build `(NOT x) NOT NULL` where SQLite means `NOT (x NOT NULL)`.

The check writes the tree out again as SQL, with every operation in parentheses and every clause explicit. SQLite must treat that text as it treats the original: the same `EXPLAIN` output for a statement, and the same values for an expression. This is the third of the four checks of the parser that [docs/todo.md](../../../docs/todo.md) lists.

The check has found no fault in the parser's tree: not on the corpus, not on the generated pairs and triples of operators, not on the statements written by hand, and not under fuzzing.

## Checking statements

`Test` checks a list of texts in a test. The texts need no schema, because the check [guesses one](#a-schema-for-the-statement).

```go
func TestShape(t *testing.T) {
	shapetest.Test(t, []string{
		"select not a not null, a = b < c from t",
		"insert into t(a) values (1) on conflict (a) do update set a = a + 1",
	})
}
```

`Check` checks one text:

```go
r := shapetest.Check("select -a collate nocase from t")
if r.Fault != "" {
	t.Fatalf("%q printed as %q: %s", r.SQL, r.Printed, r.Fault)
}
```

The functions:

- `Check(src)` checks the first statement of `src` and gives a `Result`.
- `Checker` checks many statements in turn. Its zero value is ready, and `Close` closes its connection. It keeps one connection for the expressions and compares each expression once. It is not safe for concurrent use.
- `Run(sqls)` checks every text in turn with one `Checker` and gives a `Report`.
- `Summarize(results)` makes a `Report` from results.
- `Test(t, sqls)` runs `Run`, logs the report, and fails the test if any statement has a fault.
- `Print(n)` writes a tree as text: a statement, a command, an expression, or a select. See [The printer](#the-printer).
- `Forms`, `Pairs(seed, n)`, and `Triples(seed, n)` generate expressions. See [Pairs and triples](#pairs-and-triples).

The engine's builders can use the same functions, with `Pairs` and `Triples` as a source of expressions that combine the forms of operators.

### The result

A `Result` has these fields:

- `SQL` is the text checked, and `Printed` is its tree as `Print` writes it.
- `Kind` is the kind of statement as `KindOf` names it, such as `select`, `create table`, or `alter table add column`.
- `Way` says how SQLite compared the statement, and `Reason` holds the error or the reason for the last two ways.
- `Exprs` holds an `ExprResult` for each expression of the statement.
- `Fault` says what differed, or is `""` if the check passed.

The ways:

| `Way` | Meaning |
| --- | --- |
| `Bytecode` | `EXPLAIN` gives the same program for both texts. |
| `Probes` | The statement changes the schema. The schema after it is the same for both, and so are the programs of statements that use what it created. |
| `SameError` | Both texts fail to prepare with the same error, which no schema avoids. |
| `Skipped` | The statement cannot be compared. |

An `ExprResult` has these fields:

- `Forms` names the forms of expression that the expression holds, such as `Binary +` or `Like NOT LIKE ESCAPE`.
- `Bytecode` is true if `EXPLAIN` gave the same program for both texts.
- `Values` counts the assignments of values for which both texts gave the same value of the same type. `Errors` counts those for which both gave the same error.
- `Skipped` says why the values were not compared, or is `""`.
- `Duplicate` is true if the same `Checker` compared the same expression before. The other fields are then those of the first comparison.

`Result.NotChecked()` is true for a text that the parser does not parse, or that SQLite does not say parses.

### The report

A `Report` counts the results:

- `Texts` is the number of texts, and `NotChecked` counts those not checked, by reason.
- `Kinds` counts the statements by kind and way.
- `ByValues` counts, by kind, the statements with at least one expression compared by values.
- `Uncompared` counts, by kind, the statements that SQLite compared in no way but by an error: not by bytecode or probes, and in none of their expressions.
- `Reasons` counts the errors of `SameError` and the reasons of `Skipped`.
- `Exprs` counts the expressions, and `Forms` counts them by form.
- `Faults` holds the results with a fault.

`Compared()` is the number of statements compared by bytecode or probes, `Checked()` is the number checked, and `String()` is the report as text.

### Many texts in parallel

`Run` checks the texts one after the other. SQLite serializes its memory allocations over the threads of a process, because the build keeps SQLite's memory statistics, which is the default. Goroutines therefore barely run in parallel: with 10 goroutines the corpus check ran on about 2 cores.

To check many texts in parallel, split them over processes and pass all results to `Summarize`. The package's own tests do that. `checkSharded` in `check_test.go` runs the test binary again in 8 processes, and `S4QL_SHAPETEST_SHARD` tells each its share. The number is fixed, so the report is the same on every machine: an expression counts as seen before within one process.

## The printer

`Print` writes the text from the fields of the nodes, never from the order of the tokens in the source.

That rule avoids a trap. Every token belongs to exactly one node, so a printer that writes all tokens in the order of the source reproduces the text whatever the shape of the tree. `Print` writes the children of a node by their role in that node only, so a tree of the wrong shape prints as a text of that shape.

The added parentheses leave no trace in SQLite's tree. `parse.y` drops the parentheses around an expression (`expr(A) ::= LP expr(X) RP. {A = X;}`), so these two texts give the same program:

```sql
select a+b*c from t where a order by 1
select (a+(b*c)) from t where (a) order by (1)
```

`ORDER BY (1)` is the column number 1, as `ORDER BY 1` is. A difference between the original and the printed text is therefore a difference in shape.

### What is wrapped

Every operation is written in parentheses of its own:

- the unary operators `NOT`, `-`, `+`, and `~`
- the binary operators: `AND`, `OR`, the comparisons, the bit operators, arithmetic, `||`, `->`, and `->>`
- `IS` in all its forms
- `ISNULL`, `NOTNULL`, and `NOT NULL`
- `LIKE`, `GLOB`, `REGEXP`, and `MATCH`, with `NOT` and `ESCAPE`
- `BETWEEN`
- `IN` with a list, a SELECT, a table, or a table function
- `COLLATE`
- `CASE`

The other forms are atoms or have parentheses of their own, and are not wrapped: names, literals, parameters, function calls with their DISTINCT, ORDER BY, FILTER, and OVER, `CAST`, subqueries, `EXISTS`, row values, and `RAISE`.

| Text | Printed |
| --- | --- |
| `select 1 + 2 * 3 - 4` | `select ( ( 1 + ( 2 * 3 ) ) - 4 )` |
| `select a = b < c` | `select ( a = ( b < c ) )` |
| `select not x not null` | `select ( not ( x not null ) )` |
| `select -x collate nocase` | `select ( ( - x ) collate nocase )` |
| `select x between a and b and c` | `select ( ( x between a and b ) and c )` |
| `select (a), ((b))` | `select ( a ) , ( b )` |
| `select ((a + b)) * c` | `select ( ( a + b ) * c )` |

The last two rows show the `Paren` node. It is written as `( x )` around a name, a literal, or another atom. Around an expression that is wrapped anyway, only the inner expression is written. Printing the printed text again then gives the same text, which the check requires.

### What is kept as written

- Tokens are written with their raw text, and nothing is normalized. One space separates them. The dots of a qualified name have no spaces: `s.t.c`, `t.*`.
- `AS` is added before an alias where it was left out: `select a b from t x` prints as `select a AS b from t AS x`. SQLite reads an alias the same with and without it. A parser that read `1_000` as `1 AS _000` therefore prints that visibly.
- `LIMIT a, b` stays as written, and does not become `LIMIT b OFFSET a`, because SQLite numbers anonymous `?` parameters in the order of the text.
- A declared type keeps the spacing of the source: between two of its tokens the printer writes as many spaces as the source has bytes there. SQLite keeps the text of a type from its first token to its last, and [the length of that text matters](#surprises). A STRICT table also checks the text: it refuses `TEXT(50)`, with the text in the message.
- In CREATE TABLE, a comma between two table constraints is written only where one was written. A comma ends a `CONSTRAINT name` (`tconscomma ::= COMMA. {pParse->constraintName.n = 0;}`), so `constraint c1, check(a>0)` gives a CHECK without a name.
- An empty first table option keeps its comma: `create table t(a), strict`.
- `DEFAULT` keeps its form: a literal, a signed literal, or `( expr )`. The expression is wrapped inside those parentheses: `default (1 + 2)` prints as `default ( ( 1 + 2 ) )`.
- Each step of a trigger is followed by `;`.
- The arguments of CREATE VIRTUAL TABLE are written as their tokens, since they have no structure.
- The statement's own semicolon is left out.

### Every token once

`checkPrinted` in `check.go` requires that the printer writes every token of the tree exactly once, or leaves it out on purpose. Three kinds of token are left out or replaced: the statement's semicolon, the parentheses of a `Paren` whose inner expression is wrapped, and the commas and semicolons that the printer writes itself.

### Where the check is weaker

Where parentheses cannot be added, the printer adds none, and the check sees less:

- **FROM.** A table, a join, or a SELECT in parentheses is a different source (`NestedFrom`, `SubquerySource`), so parentheses cannot mark the grouping of joins. Joins group left to right, and the text shows that only by their order.
- **Compound SELECT.** SQLite allows no parentheses around the parts. The operators group left to right, and an ORDER BY or LIMIT at the end applies to the whole compound. The tree holds it on the last part either way.
- **IN and row values.** `x IN (SELECT ...)` is not `x IN ((SELECT ...))`, which is a list with a scalar subquery, and a row value needs its own parentheses.
- **Text that SQLite copies.** SQLite takes four things from the text of the statement: the names of result columns without an alias, such as `count(*)`, the text of a CHECK, which goes into its Halt, the text of a DEFAULT, which `pragma table_info` shows, and the declared type. The printer changes that text, so the comparison masks it, leaves it out, or compares it without its spaces.
- **Boundaries between keywords.** A fault at the boundary between a column's type and its constraints, or between clauses that are only keywords in a row, prints the same tokens in the same order. No comparison of texts can see it, so pinned tests in the parser must cover it.
- **NOT before an operator.** SQLite lowers `x NOT BETWEEN a AND b` and `NOT (x BETWEEN a AND b)` to the same tree, and does the same for NOT LIKE, NOT GLOB, NOT REGEXP, NOT MATCH, and NOT IN.

## The comparison

`Check` parses the text and prints the tree. It then requires four things, in this order:

1. The printer writes every token of the tree once.
2. The printed text parses with the parser, and printing that tree gives the same text.
3. The oracle in [`internal/sql/oracle`](../oracle/README.md) says that the printed text parses.
4. SQLite treats both texts the same, as the sections below describe.

The third requirement has one exception. An error of SQLite's grammar actions, such as `IN(...) element has 3 terms - expected 2` or `variable number must be between ?1 and ?32766`, comes after the parse of the original. In the printed text SQLite can raise it at an added `)` and stop there. The check accepts that if the message is that of the original.

A fault counts only for a statement that SQLite parses. `Check` asks the oracle about the original text only when it has found a fault, because the verdict costs as much as the rest of the check.

### A schema for the statement

A statement comes without a schema, and most statements do not prepare without one. `Check` guesses a schema from the original tree and completes it from SQLite's errors, one error at a time, up to 40 times. It then uses that schema for the original and the printed text alike. A wrong guess can make a comparison fail or be skipped. It can never make two different statements look the same.

The guess takes:

- tables from FROM items, from the targets of INSERT, UPDATE, and DELETE, from the tables of CREATE INDEX and CREATE TRIGGER, from `IN table`, and from the targets of ALTER and DROP
- a view for DROP VIEW and for an INSTEAD OF trigger, and an index or a trigger for DROP INDEX and DROP TRIGGER
- the parent table of a foreign key, with a primary key or a unique constraint on the referenced columns
- columns from the column list of an INSERT, from SET, and from qualified references, also through aliases, NEW, and OLD
- every unqualified column reference, as a column of the first table

The fixes:

| SQLite's error | Fix |
| --- | --- |
| `no such table` | Add the table. |
| `unknown database` | Attach `:memory:` under that name. |
| `no such column` | Add the column: to the table that qualifies it, or to the next table that may hold it. |
| `ambiguous column name` | Remove the column from the last table it was guessed into. |
| `cannot join using column` | Add the column to the tables of the statement. |
| `table X has no column named` | Add the column. |
| `table X has N columns but M values were supplied` | Add columns, or remove guessed ones. |
| `no such index` | Create the index. |
| INSTEAD OF trigger on a table, BEFORE or AFTER trigger on a view | Turn the table into a view, or the view into a table. |
| ON CONFLICT clause matches no PRIMARY KEY or UNIQUE constraint | Create a unique index from the text of the conflict target. |

A column gets its type by a hash of its name, from `INTEGER`, `TEXT`, no type, `REAL`, `TEXT COLLATE NOCASE`, `NUMERIC`, `TEXT COLLATE RTRIM`, and `BLOB`. Affinity and collation then take part in the programs. SQLite compares with the collation of the left operand, so swapped operands show in the program where untyped columns would hide them.

Tables whose names start with `sqlite_` are not created.

### Bytecode

For a statement that does not change the schema, the check compares the output of `EXPLAIN`: the opcode, P1 to P5, and the comment of every instruction. The address is left out, since the instructions are in order. An `EXPLAIN` before the statement is dropped, and the command after it is compared.

SQLite copies text of the statement into three operands, which the check masks:

- P4 of `Trace` is the text of a trigger step.
- P4 of a `Halt` for a CHECK constraint without a name is the text of the constraint, as SQLite takes it: from between the parentheses, trimmed of spaces, dequoted if it starts with a quote, comments kept. Both sides get `<check>` in its place. It is the same for every CHECK, because two texts can print alike, as `a| b` and `a|b` do. A named CHECK has its name there, so the check still compares what `CONSTRAINT name` is attached to.
- P4 of the virtual-table opcodes is `vtab:` and an address in memory.

### Statements that change the schema

A statement that changes the schema is executed on two connections with the same schema, once as the original and once as printed. The check then compares what the statement created. This is the way `Probes`.

- **CREATE TABLE, CREATE INDEX, and ALTER TABLE.** The probes of the table are:
  - `pragma table_xinfo`, without the text of the default, and with the spaces of the type removed
  - `pragma index_list`, and `pragma index_xinfo` of every index
  - `pragma foreign_key_list` and `pragma table_list`
  - the programs of `insert ... default values`, of `delete`, and of an `update` of every column
  - the program of `select c = 'a', ...` over every column, which shows the affinity and collation of each
- **CREATE TABLE AS.** The program of the SELECT, and the probes of the table without the names of its columns, which come from the text.
- **CREATE VIEW.** The program of `select * from v`.
- **CREATE TRIGGER.** The program of a statement that fires the trigger. `EXPLAIN` shows the trigger's subprogram as part of it.

### Expressions on their own

Every expression is also compared on its own, with the expression at the same place in the printed tree. That adds values to the comparison of programs, and it covers the expressions of a statement that fails as a whole with an error. An expression counts if it is not inside another one. An expression in a clause of a subquery counts on its own again.

The column references become columns of `s4ql_v`, a table with one row and 64 columns, whose types cycle through the list above. They stay column references, which SQLite cannot fold as it folds literals. The replacement is the same in both texts and touches only leaves, so it cannot hide a wrong grouping.

Two things are compared:

- **Bytecode.** The program of `select e from s4ql_v`.
- **Values.** The row of the query below, for 3 assignments of values to the columns, or 1 if the expression has no column reference. `quote()` is exact for every type, and the four comparisons show the affinity and collation that the SELECT passes on.

```sql
select typeof(v), quote(v), v = 'A', v < 'b', v = 1, v = '1' from (select e as v from s4ql_v)
```

The values come from this pool: `0`, `1`, `2`, `3`, `7`, `-1`, `NULL`, `'a'`, `'B'`, `''`, `2.5`, `x'01'`, `'%'`, `'a_c'`, `'{"a":[1,2]}'`, `'12'`, and `1e300`.

The values are skipped, and the bytecode is still compared, for an expression whose value can change from one call to the next. That holds for `random`, `randomblob`, `changes`, `total_changes`, `last_insert_rowid`, and `sqlite_offset`, for the date and time functions, which read the clock for `'now'`, and for `CURRENT_DATE`, `CURRENT_TIME`, and `CURRENT_TIMESTAMP`.

An expression is skipped entirely if:

- it names a table
- it has more than 64 distinct column references
- it holds a declared type with a comment inside. SQLite keeps the comment in the type and takes the affinity from it, and a comment is no token of the tree. The fuzzer found this with `CREATE TABLE A(A A--\nA)`.

### What is skipped

A statement is `Skipped` in four cases:

- It is a CREATE VIRTUAL TABLE.
- A declared type in it holds a comment.
- The schema that it needs cannot be built.
- The two sides differ only because the printed text no longer finds a column that SQLite named by the text of an expression without an alias, as in `select [count(*)] from (select count(*) ...)`. The corpus has 5 such statements.

The expressions of a skipped statement are still compared.

## Results

The numbers are those of the full tests. Under `-short`, the race detector, or the address sanitizer, the corpus test uses every tenth entry and the other tests use smaller samples.

### The corpus

`TestCheck_corpus` checks the 139,234 texts of the corpus in [`internal/sql/corpus`](../corpus/README.md):

- 83,991 are statements that were checked, and none has a fault.
- 55,232 do not parse in the parser.
- On 11, SQLite does not say that they parse.

Of the 83,991 statements, 72,373 (86.2%) are compared by bytecode or probes. 4,552 (5.4%) are compared in no way but by an error, and 2,306 of those are CREATE VIRTUAL TABLE. The other 7,066 fail with the same error or are skipped as statements, and have at least one expression that is compared on its own.

By kind of statement. "By values" counts the statements with at least one expression compared by values, and "None" those compared in no way but by an error:

| Kind | Bytecode | Probes | Same error | Skipped | By values | None |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| alter table add column | 0 | 289 | 8 | 0 | 192 | 6 |
| alter table drop column | 0 | 48 | 1 | 0 | 0 | 1 |
| alter table other | 0 | 35 | 18 | 0 | 9 | 18 |
| alter table rename | 0 | 208 | 4 | 0 | 0 | 4 |
| alter table rename column | 0 | 121 | 2 | 0 | 0 | 2 |
| analyze | 91 | 0 | 4 | 0 | 0 | 4 |
| attach | 873 | 0 | 12 | 0 | 885 | 0 |
| create index | 0 | 2,541 | 98 | 0 | 2,589 | 14 |
| create table | 0 | 7,367 | 218 | 1 | 1,299 | 148 |
| create table as | 0 | 128 | 26 | 0 | 116 | 3 |
| create trigger | 0 | 807 | 122 | 0 | 839 | 27 |
| create view | 0 | 897 | 194 | 0 | 943 | 23 |
| create virtual table | 0 | 0 | 0 | 2,306 | 0 | 2,306 |
| delete | 923 | 0 | 29 | 0 | 684 | 13 |
| detach | 541 | 0 | 69 | 0 | 497 | 16 |
| drop index | 109 | 0 | 0 | 2 | 0 | 2 |
| drop table | 328 | 0 | 5 | 0 | 0 | 5 |
| drop trigger | 82 | 0 | 0 | 0 | 0 | 0 |
| drop view | 42 | 0 | 0 | 0 | 0 | 0 |
| insert | 19,394 | 0 | 1,731 | 10 | 20,004 | 271 |
| pragma | 3,234 | 0 | 213 | 0 | 0 | 213 |
| reindex | 20 | 0 | 51 | 0 | 0 | 51 |
| replace | 269 | 0 | 16 | 0 | 267 | 2 |
| select | 30,630 | 0 | 6,064 | 19 | 32,926 | 1,375 |
| transaction | 1,063 | 0 | 0 | 0 | 0 | 0 |
| update | 1,527 | 0 | 378 | 0 | 1,781 | 44 |
| vacuum | 714 | 0 | 7 | 0 | 12 | 4 |
| values | 92 | 0 | 10 | 0 | 100 | 0 |

The most common errors of `SameError` are functions and modules that the binding lacks, and errors that no schema avoids:

| Error | Statements |
| --- | ---: |
| `no such table: sqlite_stat4` | 493 |
| `no such table: sqlite_stat1` | 305 |
| `no such function: snippet` | 263 |
| `no such column: "a" - should this be a string literal in single-quotes?` | 227 |
| `table sqlite_master may not be modified` | 221 |
| `no such function: random_vector` | 217 |
| `no such function: offsets` | 156 |
| `'t1' is not a function` | 154 |
| `'generate_series' is not a function` | 133 |
| `no such function: REGEXP` | 119 |

The statements hold 208,989 expressions. A `Checker` had seen 134,111 of them before. Of the other 74,878, 57,994 are compared by bytecode and 53,497 by values. The values are not compared for 13,195 that name a table, and for about 2,400 that are not deterministic: `randomblob` 913, `time` 383, `strftime` 338, `datetime` 196, `random` 141, the clock 101, and smaller groups.

A selection of the 73 forms, with the expressions that hold each. Expressions seen before are not counted:

| Form | Expressions | By bytecode | By values |
| --- | ---: | ---: | ---: |
| `Binary +` | 11,768 | 4,068 | 3,872 |
| `Binary =` | 9,002 | 5,453 | 5,202 |
| `Binary AND` | 6,394 | 2,419 | 2,276 |
| `Binary OR` | 5,709 | 1,681 | 1,579 |
| `Binary \|\|` | 1,160 | 1,068 | 910 |
| `Binary ->` | 25 | 25 | 22 |
| `Binary ->>` | 116 | 116 | 110 |
| `Between BETWEEN` | 3,952 | 988 | 976 |
| `Between NOT BETWEEN` | 3,627 | 695 | 695 |
| `Case` | 7,236 | 892 | 886 |
| `Case with operand` | 6,375 | 1,475 | 1,444 |
| `Cast` | 6,079 | 2,049 | 2,041 |
| `Collate` | 500 | 429 | 291 |
| `FuncCall OVER` | 2,740 | 2,390 | 2,367 |
| `FuncCall FILTER` | 96 | 83 | 83 |
| `InList IN` | 4,766 | 1,733 | 1,692 |
| `InSelect IN` | 6,054 | 148 | 145 |
| `Is IS` | 491 | 356 | 343 |
| `Is IS NOT DISTINCT FROM` | 38 | 38 | 38 |
| `Like LIKE` | 722 | 688 | 663 |
| `Like LIKE ESCAPE` | 76 | 76 | 60 |
| `Like MATCH` | 1,791 | 1,753 | 0 |
| `Like REGEXP` | 154 | 0 | 0 |
| `NullTest ISNULL` | 64 | 50 | 48 |
| `NullTest NOT NULL` | 40 | 37 | 37 |
| `Unary NOT` | 5,833 | 879 | 872 |
| `Unary -` | 15,020 | 6,280 | 6,101 |
| `Unary ~` | 6,244 | 1,751 | 1,709 |
| `Row` | 487 | 270 | 263 |
| `Subquery` | 9,813 | 307 | 302 |

`go test -run TestCheck_corpus -v` prints the report with all 73 forms.

Where a form has far more expressions than comparisons, most of them hold a subquery that reads a table. Such an expression is not compared on its own. The bytecode of its statement compares it.

`REGEXP` and `MATCH` stand out. The binding has no `REGEXP` function, so SQLite builds no program. For `MATCH` SQLite builds a program, but the function fails when it runs, with `unable to use function MATCH in the requested context`, so there are no values.

### Pairs and triples

The corpus does not hold every combination of operators, so a generator makes them.

- `Forms` holds 61 forms of expression. Each is a template of tokens in which every `$` is an operand, such as `$ NOT BETWEEN $ AND $`.
- `Pairs(seed, n)` gives, for every ordered pair of forms A and B and every operand of A, a SELECT of A with B in that operand. It gives each `n` times, with other operands.
- `Triples(seed, n)` gives `n` random combinations of three forms.
- A `Generated` holds the `SQL` and the names of its `Forms`, the outer first.

The texts have no parentheses but those of the forms, so SQLite's precedence and associativity decide the grouping. The operands are mostly the columns `x1` to `x8`, to which the check gives values of every type, and some literals.

`TestCheck_pairs` checks all 3,721 ordered pairs, three times each in every operand, and 20,000 triples. It found no fault.

| | Pairs | Triples |
| --- | ---: | ---: |
| Texts | 21,594 | 20,000 |
| Rejected by SQLite and the parser alike | 222 | 281 |
| Compared by bytecode and values | 16,915 | 14,509 |
| Compared by bytecode only | 1,560 | 1,756 |
| Compared only by the same error | 2,897 | 3,454 |

The test also runs every text through `internal/sql/parsetest`, which found no disagreement on the rejected texts or on any other.

Counted by pair, where a pair takes the best of its texts, 3,042 of the 3,721 are compared by bytecode and values, 212 by bytecode only, and 467 only by the same error. Each of those 679 pairs involves one of these:

- `REGEXP` or `NOT REGEXP`, for which SQLite builds no program
- `MATCH` or `NOT MATCH`, which fail when they run
- `GLOB` with `ESCAPE`, which gives `wrong number of arguments to function GLOB()`
- a row value where none is allowed, which gives `row value misused`

### Statements written by hand

A generator does not fit the structure of statements. `Structure` in `export_test.go` holds 61 statements written by hand, on:

- joins of every kind, with ON and USING
- ORDER BY and LIMIT on compounds and in subqueries, and LIMIT with a comma
- VALUES
- common table expressions: recursive, MATERIALIZED, nested, and before INSERT, UPDATE, and DELETE
- windows, frames, named windows, and FILTER
- upserts with several targets and WHEREs, and INSERT ... SELECT ... ON CONFLICT
- RETURNING
- CASE
- triggers with several steps
- constraints with conflict clauses, DEFERRABLE, the actions of foreign keys, generated columns, STRICT, and WITHOUT ROWID
- indexes with expressions, COLLATE, an order, and WHERE
- views
- numbers with digit separators

`TestCheck_statements` requires that each is compared by bytecode or probes, and not skipped.

## Proof that the check can fail

A check that never fails proves nothing. `TestCheck_mutations` in `mutate_internal_test.go` damages trees on purpose and counts how often the check fails. It takes the trees of every 20th corpus entry, of the `Structure` statements, and of a sample of pairs and triples, and damages each at up to 2 sites for each kind of damage.

"Detected" means that the check fails. "Not compared" means that SQLite compared the changed part only by an error. The rate is the share detected of those compared.

| Damage | Detected | Undetected | Not compared | Rate |
| --- | ---: | ---: | ---: | ---: |
| Rotate two operators | 2,226 | 69 | 129 | 97.0% |
| Swap the operands | 4,235 | 130 | 405 | 97.0% |
| Move the NOT of IS NOT to its operand | 122 | 0 | 6 | 100% |
| Move the NOT of NOT BETWEEN, LIKE, or IN out | 0 | 650 | 102 | 0% |
| Swap the operands of LIMIT | 19 | 1 | 0 | 95.0% |
| Move ON or USING to the next join | 6 | 1 | 0 | 85.7% |
| Swap two join operators | 140 | 10 | 2 | 93.3% |
| Move ORDER BY or LIMIT | 33 | 0 | 0 | 100% |
| Move the ELSE of a nested CASE | 1 | 0 | 0 | 100% |
| Move FILTER or OVER to an argument | 2 | 0 | 0 | 100% |
| Move ORDER BY between the arguments and the window | 82 | 0 | 0 | 100% |
| Swap the WHEREs of an upsert | 7 | 0 | 0 | 100% |
| Move a column constraint to the next column | 145 | 8 | 3 | 94.8% |
| Move CONSTRAINT name after its constraint | 1 | 2 | 0 | 33.3% |
| Split a number with digit separators into a number and an alias | 2 | 0 | 0 | 100% |

A rotation groups two neighbouring operators the other way, with the tokens in the same order. An ORDER BY or LIMIT is moved from a compound to its first part, or into or out of a subquery.

The fourth row is expected: SQLite reads `x NOT BETWEEN a AND b` as `NOT (x BETWEEN a AND b)`, as [above](#where-the-check-is-weaker). The test fails if the check ever detects that damage. It also fails if the check never detects one of the other kinds.

### What stays undetected

Every undetected case is one where SQLite treats the two trees the same:

- Rotations under unary `+`, such as `+(a*b)` against `(+a)*b`. Unary plus compiles to nothing, and the result of arithmetic has no affinity to lose. With literal operands nothing changes either.
- `AND` under `AND`, and `OR` under `OR`. They are associative, and SQLite splits a WHERE into its AND terms.
- `COLLATE` moved over an arithmetic operator, a bit operator, or `~`, as in `(a % b) COLLATE NOCASE`. The result is a number, which no collation affects, and SQLite finds a COLLATE below an operator anyway.
- `COLLATE BINARY`, which is the default collation.
- `x OR <true constant>` and `x AND 0`. SQLite folds the whole expression, so the other side is dead code.
- `x IN ()`, which is the constant false.
- `-4 ISNULL` and other NULL tests of a literal. SQLite folds them to 0 or 1 when it parses, so `-(4 ISNULL)` and `(-4) ISNULL` are both 0.
- Swaps of equal operands, such as `1 < 1` and `x4 << x4`.
- Swaps of the operands of the symmetric operators `=`, `IS`, `AND`, `OR`, `+`, and `*`. SQLite also commutes a comparison in a WHERE to put the column first.
- `LIMIT 1 OFFSET 1`, where the operands are equal.
- An ON moved to an earlier inner join. SQLite treats the ON of an inner join as a term of the WHERE.
- Join operators swapped where the WHERE rejects NULLs, so that SQLite turns the outer joins into inner ones, or swapped with a CROSS JOIN where the planner picks the same order.
- A CHECK of a column moved to another column. SQLite keeps every CHECK on the table.
- A NULL constraint moved. It means nothing.
- `CONSTRAINT name` moved off a UNIQUE or PRIMARY KEY. SQLite keeps only the names of CHECK constraints, for their messages.

None of these is a hole in the check. Each is the same program and the same values for SQLite.

## Fuzzing

`FuzzCheck` checks the shape of the tree of any text that the parser parses. It is seeded with every 25th corpus entry, the `Structure` statements, and a sample of the pairs. `make fuzz` runs it.

Its one finding so far is the comment inside a declared type, which the check now [skips](#what-is-skipped). The input is in `testdata/fuzz/FuzzCheck`.

TODO(builder): fill in

## Run times

TODO(builder): fill in

## Surprises

SQLite behaviour that one would not guess:

- The length of a declared type matters. A type of 16 bytes or more that ends in ALWAYS loses that word, and a GENERATED before it. In the first statement below the column has the type `INT`, and in the second it keeps `int Always`:

  ```sql
  create table t(e int                         Always)
  create table t(e int Always)
  ```

- A comment inside a declared type is part of the type, and can change its affinity.
- The text of a CHECK without a name is the message of its Halt.
- A comma after `CONSTRAINT name` drops the name.
- `x OR 5` is folded: SQLite never reads `x`.
- P4 of the virtual-table opcodes is an address in memory.
- `ORDER BY (1)` is still a column number.
- SQLite's allocations serialize the threads of a process. See [Many texts in parallel](#many-texts-in-parallel).
