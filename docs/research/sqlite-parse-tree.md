# Reading SQLite's parse tree

Date: 2026-11-16. Tags: **V** = verified in this run (run on SQLite 3.54.1, or read in source at the reference given). **I** = inferred. **NV** = not verified.

Question, from Markus: "would it be possible to read the SQLite parser's output out from its memory?" s4ql will have its own parser for all of SQLite's grammar, and SQLite 3.54.1 is the oracle in tests. For the tokenizer the oracle is exact: `s4ql_tokenize` in the binding's translation unit calls the private `sqlite3GetToken` (`internal/sqlite/sqlite.go`). For the parser there is only accept-or-reject and the error message. meyer's README says "There is no upstream parse-tree oracle — SQLite cannot dump its parse tree" ([sql-front-end.md](sql-front-end.md) §2.4). This note checks whether that holds for a project that compiles SQLite itself and can reach its private functions and structs. It lays out the facts and the options. It does not decide.

**Setup.**

- The vendored amalgamation `internal/sqlite/c/sqlite3.c` (3.54.1) with `internal/sqlite/options.h`. Line numbers cited as `sqlite3.c` refer to that file.
- SQLite's source tree at the tag `version-3.54.1` (https://github.com/sqlite/sqlite/tree/version-3.54.1), downloaded as a tarball, for `src/parse.y`, `src/treeview.c`, `src/select.c`, `src/build.c`, `src/alter.c`, `src/trigger.c`, `src/insert.c`, `src/expr.c`, and `tool/lemon.c`. File and line references without `sqlite3.c` are to `src/` or `tool/` at that tag.
- `src/sqliteInt.h` at the tags `version-3.45.0`, `version-3.46.0`, `version-3.50.0`, and `version-3.53.0`, for §11.
- Apple clang on macOS arm64, Go 1.27.4.

**Method.** Throwaway programs in the scratch directory, not committed:

- `sqlite3-debug`: the official shell built from the vendored `shell.c` and `sqlite3.c` with `options.h` plus `-DSQLITE_DEBUG -DSQLITE_ENABLE_TREETRACE`. A second shell with `-DSQLITE_ENABLE_TREETRACE` and without `SQLITE_DEBUG`.
- `walker.c`: one translation unit that includes `sqlite3.c` after `options.h`, as the binding does, without `SQLITE_DEBUG`, also built with `-DSQLITE_API=static` and once under `-fsanitize=address,undefined`. It calls SQLite's private functions and prints the trees it finds in SQLite's structs as S-expressions, with a walker of 350 lines of C written for this run.
- A two-file Go module that tests whether a build-tagged file can switch on C code in another file's cgo preamble (§10).

"Run on 3.54.1" below means one of these programs.

## Short answer

- **Yes, for the statements that build a tree.** `select`, `insert`, `update`, `delete`, and `create table`, `create index`, `create view`, and `create trigger` build a tree of `Select`, `Expr`, `ExprList`, `SrcList` and the like. The actions for all other statements generate bytecode straight from tokens, and no tree of them ever exists (§9). The quoted claim is right for SQLite's public API and wrong for a project that compiles SQLite itself.
- **Three ways in work, shown on 3.54.1.**
  1. *SQLite's own tree printer* (`treeview.c`), which needs `SQLITE_DEBUG` and `SQLITE_ENABLE_TREETRACE`. It prints to stdout at the entry of `sqlite3Select`, `sqlite3Insert`, `sqlite3Update`, and `sqlite3Delete`. That is after the grammar's actions, which have already folded some expressions and already compiled multi-row `VALUES` into bytecode. It prints no quote style and no positions (§2).
  2. *A walker of our own over a parse in rename mode*. The private `renameParseSql`, which `ALTER TABLE ... RENAME` uses, parses a `CREATE` statement and keeps its tree. A `select` wrapped as `CREATE VIEW` and an `insert`, `update`, or `delete` wrapped as the body of `CREATE TRIGGER` come back as trees. Rename mode also records the source position of every name and most literals. This works in the production build, in the binding's translation unit, like `s4ql_tokenize`. The wrappers refuse parameters, and the trigger body refuses `WITH`, `RETURNING`, `DEFAULT VALUES`, `INDEXED BY`, and schema-qualified targets. They refuse with an error, never silently (§3).
  3. *Lemon's parser trace* (`PRAGMA parser_trace`, `SQLITE_DEBUG` only). It prints every shift and reduction with the rule's text, for every kind of statement. That is a derivation, without token text, and with 14 unit rules of `parse.y` optimized out by Lemon (§5).
  The schema after a statement ran also holds trees, but they are reduced copies without positions (§4). No hook in the public API sees a tree (§6).
- **What the tree is.** It is SQLite's internal representation, already lowered by the grammar's actions. Some examples: `<>` and `!=` are one operator. `x IN (5)` becomes `x = +5`. `5 IS NULL` becomes the integer 0. `LIKE` becomes a function call with the operands swapped. `LIMIT a, b` is stored as `LIMIT b OFFSET a`. `count(*)` equals `count()`. Named windows are chained, and their list is reversed (§7). A comparison can therefore assert that s4ql's tree, lowered by the same rules, equals SQLite's. That catches wrong grouping and associativity, clauses attached to the wrong term of a compound, dropped or misplaced clauses that do not change results on the test data, and wrong name positions. It cannot check any distinction that SQLite lowers away (§8).
- **What the evidence favours.** Route 2 as the tree oracle for the statement kinds that have a tree, kept in the binding's translation unit and switched on for tests only, together with a lowering function on s4ql's side that is small, explicit, and reviewed against the list in §7. Reason: it uses the same compiled object as production, with no `SQLITE_DEBUG` and no second copy, and it keeps more of the input than route 1 does. Its gaps are known and reported as errors. Route 1 could check what route 2 cannot wrap, in a debug shell run as a separate process. For statements without a tree, accept-or-reject and hand-written expectations remain. §13 gives the reasons.

## 1. Why the tree is hard to get at (V)

- A top-level statement's tree lives only while its last grammar rule is reduced. `cmd ::= select(X)` calls `sqlite3Select(pParse, X, &dest)` and then `sqlite3SelectDelete(pParse->db, X)` (`parse.y` 520-527). The `insert`, `update`, and `delete` rules call `sqlite3Insert`, `sqlite3Update`, and `sqlite3Delete` the same way (`parse.y` 984-1093), and those functions free the trees they get.
- The parser engine is a local of `sqlite3RunParser`, and `Parse` holds no pointer to it (`sqliteInt.h` 3921-4050), so nothing outside the parse can reach the value stack.
- Some trees are consumed before the statement ends. Multi-row `VALUES` is compiled into a co-routine row by row as it is parsed, and each row's `ExprList` is freed (`insert.c` 673-768, `sqlite3MultiValues`). This was added in 3.46.0: `sqlite3MultiValues` is absent from `sqliteInt.h` at 3.45.0 and present at 3.46.0.
- Two parse modes keep the tree after the parse. In `PARSE_MODE_DECLARE_VTAB` and `PARSE_MODE_RENAME`, `sqlite3RunParser` does not free `pParse->pNewTable`, and in rename mode it does not free `pParse->pNewTrigger` either (`tokenize.c` 749-758).

## 2. Route 1: SQLite's own tree printer

Facts (V):

- **What exists.** `src/treeview.c` has `sqlite3TreeViewSelect`, `sqlite3TreeViewExpr`, `...ExprList`, `...SrcList`, `...With`, `...Window`, `...Upsert`, `...Insert`, `...Update`, `...Delete`, `...Trigger`, and the `sqlite3Show*` shorthands (`treeview.c` 92-1330). The whole file is inside `#ifdef SQLITE_DEBUG` (line 21). The Insert, Update, and Delete printers are also inside `#if TREETRACE_ENABLED` (1034-1236).
- **What switches it on.** `TREETRACE_ENABLED` is 1 only with `SQLITE_DEBUG` and one of `SQLITE_TEST`, `SQLITE_ENABLE_SELECTTRACE`, or `SQLITE_ENABLE_TREETRACE` (`sqliteInt.h` 1158-1169). The mask `sqlite3TreeTrace` selects what prints. Its bits are listed at `sqliteInt.h` 1171-1194: 0x10000 is "Beginning of DELETE/INSERT/UPDATE processing", 0x10 "Query name resolution", 0x4 "Query flattener", 0x100000 "Pointers are all shown as zero", and so on. The mask is set through `sqlite3_test_control(SQLITE_TESTCTRL_TRACEFLAGS, 1, &mask)` (`main.c` 4748-4768) and in the shell with `.treetrace MASK` (`shell.c` 36812-36817; `.selecttrace` is the old name). Run on 3.54.1: a shell built with `SQLITE_ENABLE_TREETRACE` and without `SQLITE_DEBUG` prints nothing, and `PRAGMA parser_trace` is unknown there.
- **Where the output goes.** `sqlite3TreeViewLine` writes with `fprintf(stdout, ...)` and flushes (`treeview.c` 57-80). The mask is a process-wide global (`global.c` 366).
- **When it prints.** With 0x10000, at the entry of `sqlite3Select` (`select.c` 7772-7781), `sqlite3Insert` (`insert.c` 986-991), `sqlite3Update` (`update.c` 395-398), and `sqlite3Delete` (`delete.c` 364-369). These run from the statement's last reduction, so the tree is the parser's output plus what the grammar's actions changed, and nothing from name resolution or the optimizer. Other bits print the tree at later stages: after name resolution (0x10, `select.c` 7813), after the flattener, and so on.
- **What it covers.** `select` with `WITH`, compounds, window definitions, and subqueries; `insert` with its column list, data source, upsert, and `WITH`; `update` and `delete` with `WITH`, `FROM`, and `WHERE` (run on 3.54.1). A trigger step, including `RETURNING`, is printed only as its source text or as the word `RETURNING` (`treeview.c` 1242-1263), so the expressions of a `RETURNING` list are not printed. `create`, `drop`, `pragma`, transactions, `attach`, `vacuum`, `analyze`, and `alter` print only the internal statements that SQLite runs for them, such as the `UPDATE` of `sqlite_master` after `create table` (run on 3.54.1).

What it keeps and loses (V, run on 3.54.1 and `treeview.c` 501-900):

- **Kept:** operator kinds; the order and nesting of operands; names after dequoting; literals (integers that fit in 32 bits as values, others as text); the attachment of `WHERE`, `GROUP BY`, `HAVING`, `ORDER BY`, `LIMIT`, and `WINDOW` to each term of a compound; the compound operators; the alias or span text of each result column; `LEFT`, `RIGHT`, `FULL`, and `CROSS` joins (`treeview.c` 202-210); parameters with their numbers (`VARIABLE(:p,2)`).
- **Lost:** the quote style of names (`"a"`, `[a]`, and `a` all print as `ID "a"`, line 609), except inside the span text of a result column; `NATURAL` on a join; every source position; rows after the first of a multi-row `VALUES` (below); the expressions of `RETURNING`.
- **Noise:** pointers (zeroed with 0x100000), `selId` numbers, hex flag words, and the file and line of the trace point.
- **Several dumps per statement.** A statement prints the trees of nested statements and of later calls of `sqlite3Select` on subqueries that SQLite has already rewritten. For `insert ... values (1,2),(3,4)` and for a top-level `values (1,2),(3,4)`, the first dump is the first row alone, printed when it is compiled as a co-routine during parsing. The dump that follows has `*` as its only result column, and the second row appears in neither dump. So "the first dump is the statement's tree" does not hold.

Example, run on 3.54.1 with `.treetrace 0x10000`:

```
select a+b*c from t where x and y or z;
In sqlite3Select() at .../internal/sqlite/c/sqlite3.c:157324
'-- SELECT (1/7B5D0099D0) selFlags=0x0 nSelectRow=0
    |-- result-set
    |   '-- SPAN("a+b*c")
    |       '-- ADD
    |           |-- ID "a"
    |           '-- MUL
    |               |-- ID "b"
    |               '-- ID "c"
    |-- FROM
    |   '-- {-1:*} t
    '-- WHERE
        '-- OR
            |-- AND
            |   |-- ID "x"
            |   '-- ID "y"
            '-- ID "z"
```

The compound `select a from t union all select b from t2 order by 1 limit 2 offset 3` prints the rightmost term first, with `ORDERBY 1` and `LIMIT 2 OFFSET 3` under it, then `UNION ALL`, then the left term. `update t set a = a+1 where b > 2` prints `UPDATE`, `FROM t`, `SET AS a (ADD (ID "a") 1)`, `WHERE GT (ID "b") 2`. `insert into t(a,b) values (1,2),(3,4)` prints a `SELECT` with result set `1, 2`, then `INSERT INTO t COLUMNS a, b DATA-SOURCE SELECT result-set ASKTERISK`. `create table t1 (v1 int primary key) strict, without rowid` prints only the nested `INSERT` and `UPDATE` of `sqlite_master` and a `SELECT` from it.

The parse-time fold that makes this route lose clauses, run on 3.54.1: `select a from t where a in () and b` prints `WHERE 0`. So does `select a from t where a != b and a <> b and a in (5) and a not between 1 and 2 and 5 is null`: `5 is null` becomes the integer 0, and `sqlite3ExprAnd` then replaces the whole conjunction with 0 (`expr.c` 1164-1182).

## 3. Route 2: a walker of our own over a parse in rename mode

### 3.1 How (V)

- `renameParseSql(Parse*, zDb, db, zSql, bTemp)` (`alter.c` 1181-1234) is a `static` function in `sqlite3.c`. Any C code in the same translation unit can call it, as `s4ql_tokenize` calls `sqlite3GetToken`. It accepts only text that starts with `CREATE ` (line 1195), sets `eParseMode = PARSE_MODE_RENAME`, runs `sqlite3RunParser`, and leaves the result in `pParse->pNewTable`, `pParse->pNewIndex`, or `pParse->pNewTrigger`. `renameParseCleanup` (`alter.c` 1517-1532) frees it all.
- In rename mode, `pParse->pRename` is a list of `RenameToken {p, Token t, pNext}` (`alter.c` 730-734). It maps tree objects to the source token that created them: every leaf built by `tokenExpr` (names, strings, floats, blobs, `NULL`, parameters; `parse.y` 1146-1176), table names in `FROM`, `IdList` names, aliases and `SET` column names (`expr.c` 2238), column and table names in `create table` (`build.c` 1300, 1606), and index names. Integer literals have no rename token, but every leaf, in every mode, has its byte offset in `Expr.w.iOfst` (`parse.y` 1170, 1208; `expr.c` 1203 for function names). Operators and keywords have no positions.
- `CREATE VIEW` in rename mode keeps the parser's own `Select` (`build.c` 3121-3125) instead of the reduced copy that normal mode stores. A trigger keeps its own steps and `WHEN` (`trigger.c` 278-283, 358-363).
- Wrappers that the walker used: `CREATE VIEW s4ql_v AS <select>`; `CREATE TRIGGER s4ql_tr AFTER INSERT ON s4ql_anchor BEGIN <stmt>; END`, which needs one existing table; and `... WHEN <expr> BEGIN SELECT 1; END` for a bare expression. The tables named inside the wrapped statement need not exist: views and trigger bodies are not resolved at creation (run on 3.54.1 with `select * from nosuch`, `insert into nosuch values (1)`, `update nosuch set q = 1`). Offsets count from the start of the wrapped text: the view prefix is 22 bytes and the trigger prefix 57.
- The `select` after `AS` in `CREATE VIEW` is the same nonterminal as a top-level `select` (`parse.y` 509 and 520), so the view wrapper does not change the grammar. A trigger body uses `trigger_cmd` (`parse.y` 1814-1830), which is not the top-level `cmd`, so the trigger wrapper does change it (§3.2).

### 3.2 What the wrappers refuse (V, run on 3.54.1)

| Input | Wrapper | Error |
|---|---|---|
| `select :p`, `select ?` | view | `parameters are not allowed in views` (`build.c` 3089-3091) |
| any parameter | trigger | `trigger cannot use variables` (`attach.c` 487) |
| `select * from temp.t` | view in `main` | `view s4ql_v cannot reference objects in database temp` (`attach.c` 511) |
| `insert into main.t ...` | trigger | `qualified table names are not allowed on INSERT, UPDATE, and DELETE statements within triggers` (`trigger.c` 490) |
| `... returning a` | trigger | `cannot use RETURNING in a trigger` (`build.c` 1504) |
| `with c as (...) insert ...` | trigger | `near "insert": syntax error` (no `with` in `trigger_cmd`) |
| `insert into t default values` | trigger | `near "default": syntax error` |
| `update t indexed by i ...` | trigger | `the INDEXED BY clause is not allowed on UPDATE or DELETE statements within triggers` (`parse.y` 1797-1808) |
| `delete ... order by a limit 1` | trigger | syntax error; also a syntax error at top level in this build, which lacks `SQLITE_ENABLE_UPDATE_DELETE_LIMIT` |
| `create table t4 as select 1` | create | `SQL logic error` (`build.c` 2926-2930 refuses it in rename mode) |

The view and trigger wrappers also run SQLite's `DbFixer` over the tree (`build.c` 3112-3113, `trigger.c` 351-356). It replaces a schema name in `FROM` with the fixed schema and leaves only the flag `hadSchema` (`attach.c` 505-521): `select * from main.t` and `select * from t` differ in the tree only in that flag.

A `RETURNING` list is the same `selcollist` nonterminal as a select's result columns (`parse.y` 1116 and 712-722), so its expressions could be checked as `select <list>` (I).

### 3.3 How rename mode differs from normal mode (V, from the 76 uses of `IN_RENAME_OBJECT` and `IN_SPECIAL_PARSE` in `src/`)

- `sqlite3ExprAnd` does not fold a conjunction with a constant false side (`expr.c` 1172-1174). Run on 3.54.1: `a in () and b and 5 is null` stays `(AND (AND false b) 0)`, where normal mode gives `0`.
- Multi-row `VALUES` becomes a left-deep `UNION ALL` of single-row selects flagged `SF_Values`, instead of a co-routine (`insert.c` 680). It is the same form that normal mode builds while reading the schema (`db->init.busy`, line 676).
- Existence checks are skipped: `create table t` gives a tree when `t` exists (`build.c` 1337), and `create trigger` does not check for an existing trigger (`trigger.c` 205).
- Nested statements are not issued (`build.c` 302), and the authorizer is not called (`auth.c` 212-213).
- The automatic index of a primary key or unique constraint is named `sqlite_butoindex_...` instead of `sqlite_autoindex_...` (`build.c` 4192).
- Rename tokens are recorded, and some objects are moved where normal mode would copy them (`build.c` 3121, `trigger.c` 278, `expr.c` 620).

None of these changes the operators, the operands, or the clauses of a tree that both modes build, apart from the first two items (I, from reading every site).

### 3.4 Output for the five statements (V, run on 3.54.1)

The walker prints each node as `(KIND ...)`, with `ofs=` from `Expr.w.iOfst`, `@offset:text` from the rename token, `q` or `dq` for the quote flags, `int=` for an integer held as a value, and `is-true` or `is-false` for the flag that `sqlite3ExprInt32` sets on every integer (`expr.c` 1000-1013). Line breaks were joined.

```
view: select a+b*c from t where x and y or z
(view s4ql_v (select (result (item span="a+b*c" (PLUS (ID "a" ofs=29 @29:a) (STAR (ID "b" ofs=31 @31:b) (ID "c" ofs=33 @33:c)))))
  (from (src schema=<fixed> name=t @40:t))
  (where (OR (AND (ID "x" ofs=48 @48:x) (ID "y" ofs=54 @54:y)) (ID "z" ofs=59 @59:z)))))

view: select a from t union all select b from t2 order by 1 limit 2 offset 3
(view s4ql_v (compound
  (select (result (item span="a" (ID "a" ofs=29 @29:a))) (from (src schema=<fixed> name=t @36:t)))
  UNION ALL
  (select (result (item span="b" (ID "b" ofs=55 @55:b))) (from (src schema=<fixed> name=t2 @62:t2))
    (orderby (item (INTEGER int=1 is-true ofs=74)))
    (limit (LIMIT (INTEGER int=2 is-true ofs=82) (INTEGER int=3 is-true ofs=91))))))

trigger: insert into t(a,b) values (1,2),(3,4)
(step INSERT orconf=11 (from (src schema=<fixed> name=t @69:t)) (columns a @71:a b @73:b)
  (compound (select values (result (item (INTEGER int=1 is-true ofs=84)) (item (INTEGER int=2 is-true ofs=86))))
    UNION ALL
    (select values multivalue (result (item (INTEGER int=3 is-true ofs=90)) (item (INTEGER int=4 is-true ofs=92))))))

trigger: update t set a = a+1 where b > 2
(step UPDATE orconf=11 (from (src schema=<fixed> name=t @64:t))
  (set (item as="a" @70:a (PLUS (ID "a" ofs=74 @74:a) (INTEGER int=1 is-true ofs=76))))
  (where (GT (ID "b" ofs=84 @84:b) (INTEGER int=2 is-true ofs=88))))

create: create table t1 (v1 int primary key) strict, without rowid
(table t1 @13:t1 strict without-rowid iPKey=-1 keyConf=0
  (column v1 @17:v1 type="INT" eCType=3 affinity=D notNull=2 pk)
  (index sqlite_butoindex_t1_1 type=2 onError=11 cols=0))
```

`orconf=11` is `OE_Default`. `notNull=2` is `OE_Abort`, which `convertToWithoutRowidTable` sets on every primary-key column (`build.c` 2448-2456). `type=2` is `SQLITE_IDXTYPE_PRIMARYKEY`. The compound is a list linked through `pPrior` from its rightmost term, which holds `ORDER BY` and `LIMIT` for the whole compound (`parse.y` 539-567, 623-643).

More, from the same runs:

- `select "a", 'a' from t` gives `(ID "a" dq @29:"a")` and `(STRING "a" q @34:'a')`. ``"a" + [a] + `a` + a`` gives `dq`, `q`, `q`, and no flag: brackets and backticks share one flag (`util.c` 327-332), and the rename token keeps the raw text of each.
- `create table t3 (a integer primary key autoincrement, b text not null default 'x' collate nocase, c as (a+1) stored, d check (d > 0) references t(a) on delete cascade, unique (b, c), constraint k check (a < 10))` gives `iPKey=0`, the declared types as text, affinities, `notNull`, the collation, `stored`, a unique index on columns 1 and 2, the two checks with their names, and the foreign key. The default is wrapped in a `SPAN` node holding its text (`build.c` 1810-1822). The check and generated-column expressions are already resolved: they hold `COLUMN` nodes, not `ID` (`build.c` 2821, 2843). Index expressions and partial-index `WHERE` clauses are resolved too (`build.c` 4265, 4299).
- `create virtual table vt using fts5(a, b, tokenize = "porter ascii")` gives only text arguments: `arg3="a" arg4="b" arg5="tokenize = "porter ascii""`. The grammar reads them with `%wildcard ANY` ([sql-front-end.md](sql-front-end.md) §2.1).
- The walker ran clean under `-fsanitize=address,undefined` over 167 statements.

### 3.5 Fit in the binding

- The walker compiles in one translation unit with `sqlite3.c`, `options.h`, and `-DSQLITE_API=static`, without warnings, as the binding's preamble is built (V). It uses only static functions and struct fields, so it adds no global symbol, and `TestSymbols` holds (I).
- cgo cannot read these structs from Go directly. "C struct fields that cannot be expressed in Go, such as bit fields or misaligned data, are omitted", and "C's union types are represented as a Go byte array with the same length" (`go doc cmd/cgo`, Go 1.27.4). `SrcItem.fg`, `ExprList.a[].fg`, and `Expr`'s `u`, `x`, `w`, and `y` are bit fields or unions (`sqliteInt.h` 3071-3135, 3271-3300, 3393-3443). So the walker is C, or Go reading through C accessors. In either case it is C in the preamble of `sqlite.go`, because only that translation unit sees the static functions.
- A Go file with a build tag can switch C code in `sqlite.go`'s preamble on and off, because `#cgo CFLAGS` apply to the whole package. Run in a two-file module: `a.go` has `#ifdef S4QL_PARSETREE` in its preamble, `b.go` has `//go:build s4ql_parsetree` and `// #cgo CFLAGS: -DS4QL_PARSETREE`. `go run .` printed 0 and `go run -tags s4ql_parsetree .` printed 1 (V). The define is read only by the walker, not by SQLite's code (I). Without the tag, the production binding carries none of the walker. With it, the binding carries the walker and a function that calls `renameParseSql`.

## 4. Route 3: the schema after a statement ran (V, run on 3.54.1)

- After `create table`, `create index`, `create view`, or `create trigger` runs on a connection, `sqlite3FindTable`, `sqlite3FindIndex`, and the schema's `trigHash` give the objects. The walker printed all four.
- They come from a second parse of the stored text while the schema is read (`db->init.busy`). Views and trigger steps are stored as `EXPRDUP_REDUCE` copies (`build.c` 3125; `trigger.c` 495, 540, 588-590, 635), which end before `iTable` and so have no room for `w.iOfst` (`EXPR_REDUCEDSIZE`, `sqliteInt.h` 3228), so they hold no positions, and there are no rename tokens. Multi-row `VALUES` in them is the `UNION ALL` form, because the schema is being read (`insert.c` 676).
- The restrictions on what can be wrapped are those of §3.2, since the same `CREATE` statements must succeed first.
- This route has nothing that route 2 lacks, apart from running the statement in normal mode (I).

## 5. Route 4: Lemon's parser trace

Facts (V):

- In a build with `SQLITE_DEBUG`, `PRAGMA parser_trace=ON` sets `SQLITE_ParserTrace` on the connection (`sqlite3.c` 145148-145152). `sqlite3RunParser` then prints `parser: [[[<sql>]]]` and turns on Lemon's trace to stdout (`tokenize.c` 621-628). Lemon's trace exists only without `NDEBUG` (`sqlite3.c` 186296-186328), and `sqliteInt.h` makes `NDEBUG` the opposite of `SQLITE_DEBUG` (lines 466-481). The trace's file and prompt are globals of the parser (`sqlite3.c` 182393-182398).
- It prints each input token by kind (`'ID'`, `'INTEGER'`), not by text, each shift, each reduction as `Reduce N [rule text]`, and each keyword that falls back to `ID` (`FALLBACK KEY => ID`). Nested parses are printed in the middle of the outer parse, such as the `INSERT` into `sqlite_master` for the primary key's index, which `create table t1 (v1 int primary key) strict, without rowid` issues while its column constraint is reduced. Each starts with `[[[` and ends with `Accept!`.
- It covers every statement kind, because every statement is parsed.
- It is a derivation of Lemon's optimized grammar, not of `parse.y` as written. Lemon drops the reduction by a unit rule that has no code when the rule's right-hand side is a nonterminal: the parser goes straight on as if the left-hand side had been shifted (`tool/lemon.c` 5252-5280). In 3.54.1, 14 of 412 rules are marked `(OPTIMIZED OUT)` and never appear in the trace, among them `expr ::= term`, `selectnowith ::= oneselect`, `table_option_set ::= table_option`, and `nmnum ::= plus_num` (`sqlite3.c`, `YYNRULE` 412 at line 181347). Token classes print expanded, as in `expr ::= ID|INDEXED|JOIN_KW`. Rule numbers change with the grammar.

Example, run on 3.54.1: the reductions of the outer parse of `create table t1 (v1 int primary key) strict, without rowid`, in order:

```
createkw ::= CREATE | temp ::= | ifnotexists ::= | nm ::= ID|INDEXED|JOIN_KW | dbnm ::= |
create_table ::= createkw temp TABLE ifnotexists nm dbnm | nm ::= ID|INDEXED|JOIN_KW |
typename ::= ID|STRING | typetoken ::= typename | columnname ::= nm typetoken | carglist ::= |
sortorder ::= | onconf ::= | autoinc ::= | ccons ::= PRIMARY KEY sortorder onconf autoinc |
carglist ::= carglist ccons | columnlist ::= columnname carglist | conslist_opt ::= |
nm ::= ID|INDEXED|JOIN_KW | table_option ::= nm | nm ::= ID|INDEXED|JOIN_KW | table_option ::= WITHOUT nm |
table_option_set ::= table_option_set COMMA table_option |
create_table_args ::= LP columnlist conslist_opt RP table_option_set | cmd ::= create_table create_table_args | cmdx ::= cmd
```

`strict` is `table_option ::= nm`: the word is a name, and the action checks it. For `select a+b*c from t where x and y or z`, the reductions show `expr STAR expr` before `expr PLUS expr`, and `expr AND expr` before `expr OR expr`, so the trace does show grouping.

What it could check (I): that s4ql's parser, if its structure follows `parse.y` rule by rule, applies the same rules in the same order, for every kind of statement. Combined with the token list from `sqlite3GetToken`, it gives a concrete syntax tree whose leaves have text. The cost: it binds s4ql's parser to the shape of `parse.y` and to Lemon's optimizations, which are not part of any interface. And it needs a debug build.

## 6. Other hooks (V unless marked)

- **The authorizer** is called with `SQLITE_SELECT` at the top of `sqlite3Select` (`select.c` 7771) and with table and column names during name resolution and code generation. It gets strings, not a tree. The `Select` is an argument on the C stack and is not reachable from `db->pParse` (§1). It is not called in rename mode (`auth.c` 212-213).
- **`sqlite3_trace_v2`** reports the statement's text, timings, rows, and closing (`sqlite3.h`, `SQLITE_TRACE_*`). No tree.
- **Test controls.** Only `SQLITE_TESTCTRL_TRACEFLAGS` concerns trees, and its masks are read only in a debug build (§2).
- **`sqlite3_declare_vtab`** parses in `PARSE_MODE_DECLARE_VTAB`, accepts only `CREATE TABLE` (`vtab.c` 830-845), and needs a virtual table under construction. It gives the column list without positions, which is less than route 2.
- **`EXPLAIN`** gives bytecode after planning. It reflects grouping and clause attachment only indirectly, through the optimizer, and SQLite says the opcodes "change from one release of SQLite to the next" (https://sqlite.org/opcode.html; [sql-front-end.md](sql-front-end.md) §6.3).

## 7. What the tree is: lowering done by the grammar's actions (V, `parse.y` and the functions it calls; run on 3.54.1)

| Input | Tree | Source |
|---|---|---|
| `!=`, `<>` | one `NE`; `=` and `==` one `EQ` | `tokenize.c` 336-371 |
| `(x)` | `x`: parentheses leave no node | `parse.y` 1182 |
| `x NOT BETWEEN a AND b` | `NOT(BETWEEN(x, list(a, b)))` | `parse.y` 1479-1492 |
| `x IN ()`, `x NOT IN ()` | `false`, `true`; with a function on the left, `AND(false, x)` and `OR(true, x)` | `parse.y` 1497-1519 |
| `x IN (5)` (one constant) | `EQ(x, UPLUS(5))`; `NOT IN` wraps it in `NOT` | `parse.y` 1521-1530 |
| `x IN (select ...)`, `x IN t`, `x IN f(args)` | `IN` with a `Select`; a table or table-valued function becomes `SELECT * FROM ...` | `parse.y` 1531-1570 |
| `(a, b) IN ((1, 2), (3, 4))` | `IN` with a `VALUES` compound | `parse.y` 1539-1545 |
| `x IS NULL`, `x ISNULL`, `x IS NOT NULL`, `x NOTNULL`, `x NOT NULL` | `ISNULL` or `NOTNULL` | `parse.y` 1394-1436 |
| `5 IS NULL`, `-5 IS NOT NULL` (literal operand) | the integers 0 and 1 | `parse.y` 1394-1419 |
| `IS NOT DISTINCT FROM`, `IS DISTINCT FROM` | `IS`, `ISNOT` | `parse.y` 1441-1452 |
| `x AND y` with a constant false side | the integer 0 in normal mode; kept in rename mode | `expr.c` 1164-1182 |
| `- +x`, `+ +x` | `UMINUS(x)`, `UPLUS(x)`: a unary operator over `UPLUS` replaces it; `+ -x` stays two nodes | `parse.y` 1458-1470 |
| `-5` | `UMINUS(5)`, not folded | same |
| integer literal | value in `u.iValue` if it fits in 32 bits, so `0x10`, `16`, `0016`, and `1_6` are equal; otherwise text | `parse.y` 1201-1209, 2151 |
| quoted name or string | dequoted text plus `EP_Quoted` or `EP_DblQuoted` | `parse.y` 1146-1176, `util.c` 327-332 |
| `true`, `false` | `ID` at parse time; name resolution turns them into constants later | run on 3.54.1 |
| `x LIKE p ESCAPE e`, `GLOB`, `REGEXP`, `MATCH` | `FUNCTION like(p, x, e)`, operands swapped, flag `EP_InfixFunc`; `NOT LIKE` wraps it in `NOT` | `parse.y` 1367-1390 |
| `x -> p`, `x ->> p` | `FUNCTION "->"(x, p)` | `parse.y` 1472-1476 |
| `current_time` and the other `CTIME_KW` | a `FUNCTION` without arguments | `parse.y` 1335-1337 |
| `count(*)`, `count()` | the same `FUNCTION "count"` without a list | `parse.y` 1248, `expr.c` 1188-1215 |
| `count(ALL x)` | the same as `count(x)`; only `DISTINCT` leaves a flag | `expr.c` 1214 |
| `CAST(x AS varchar ( 10 ))` | `CAST` holding the type's text as written | `parse.y` 1234-1239 |
| `x COLLATE "nocase"` | `COLLATE` holding the dequoted name | `parse.y` 1230-1232 |
| `LIMIT a OFFSET b`, `LIMIT b, a` | the same `LIMIT(a, b)` | `parse.y` 973-978 |
| `SELECT ALL`, `SELECT DISTINCT` | flags `SF_All`, `SF_Distinct` on the `Select` | `parse.y` 697-699 |
| compound | left-deep list through `pPrior`; a right operand that is itself a compound (multi-row `VALUES`) is wrapped as `SELECT * FROM (...)` | `parse.y` 623-643 |
| multi-row `VALUES` | normal mode: rows compiled into a co-routine during parsing; schema and rename mode: `UNION ALL` of single rows | `insert.c` 673-768 |
| `,`, `JOIN`, `INNER JOIN` | the same join type 0x1; `LEFT` and `LEFT OUTER` both 0x28 | run on 3.54.1 |
| `UPDATE t ... FROM u` | `t` and `u` in one `SrcList` | `parse.y` 1020-1060 |
| `SET (a, b) = (1, 2)` | two items, `a = 1` and `b = 2` | `parse.y` 1075-1083 |
| `WINDOW w1 AS (...), w2 AS (w1 ...)` | the list in reverse order; `w2` holds copies of `w1`'s clauses, and the base name is gone | `parse.y` 1996-2002, `window.c` 1280-1309 |
| `INSTEAD OF` trigger | stored as `BEFORE` | `trigger.c` 265-267 |
| column `DEFAULT` | wrapped in `SPAN` with its text | `build.c` 1810-1822 |
| `CHECK`, generated column, index expression, partial index | names already resolved to columns | `build.c` 2821, 2843, 4265, 4299 |
| `WITHOUT ROWID` | primary-key columns marked `NOT NULL` | `build.c` 2448-2456 |
| `select *` | `ASTERISK`; it is expanded later, in `sqlite3SelectExpand`, not by the parser | run on 3.54.1 |

## 8. What a comparison could assert

**The assertion.** For each statement that both s4ql and SQLite accept and that one of the routes can reach, `lower(tree_s4ql) = tree_SQLite`. `lower` applies the rules of §7 to s4ql's tree. It maps both trees to one normal form, for example S-expressions, without pointers, flags that only code generation reads, or parse-mode details. The rules must be the ones that SQLite applies at the pinned version, so `lower` must be checked against §7 at every version bump.

**What it would catch that accept-or-reject and comparing results do not** (I, from what the tree keeps):

- Wrong grouping or associativity: `a - b - c` against `a - (b - c)`, `a OR b AND c`, the precedence of `||`, `->`, `COLLATE`, and unary operators, `NOT` against `=`. Results catch these only when the test data happens to distinguish them.
- A clause attached to the wrong place: `ORDER BY` or `LIMIT` on the last term of a compound instead of the whole compound, `WHERE` on the wrong term, `ON` attached to the wrong join.
- Dropped or swapped clauses that do not change results on the test data: `DISTINCT` on unique rows, `NULLS FIRST`, `LIMIT` against `OFFSET` (rqlite/sql swaps them, [rqlite-sql.md](rqlite-sql.md), short answer), window frames and `EXCLUDE`, `MATERIALIZED`, the conflict target and `DO NOTHING` of an upsert, `OR IGNORE`.
- Names: the dequoted text of each name, whether it was quoted, and whether with double quotes; the alias of each result column and table; the span text of a result column, which SQLite uses as the column's name.
- Positions: the byte offset of each name and literal, which errors need ([sql-front-end.md](sql-front-end.md) §6.1).
- For `create table`: declared types as written, affinities, the primary key and its conflict clause, `STRICT`, `WITHOUT ROWID`, constraint names, defaults as text, foreign-key actions.

**What it could not assert:**

- Any distinction that §7 lowers away: `<>` against `!=`, `count(*)` against `count()`, `ALL` in a function, `IN (5)` against `= +5`, `0x10` against `16`, parentheses, `LEFT` against `LEFT OUTER`, `LIMIT a, b` against `LIMIT b OFFSET a`. These need other checks, such as hand-written cases.
- Keyword spellings (`TEMP` against `TEMPORARY`, `REPLACE` against `INSERT OR REPLACE`; both give `orconf=5`, run on 3.54.1), and the positions of operators and keywords.
- Statements that no route reaches: those without a tree (§9), and in route 2 those that the wrappers refuse (§3.2).
- Whether s4ql rejects what SQLite rejects; that stays with accept-or-reject.
- Meaning that depends on name resolution, such as which table an unqualified column belongs to. The routes give the tree before resolution (route 1 with 0x10 gives it after).

## 9. Coverage by statement kind (V, run on 3.54.1, and the actions in `parse.y`)

"Tree trace" is route 1 (debug build). "Rename walker" is route 2 (production build). "Schema" is route 3. "Lemon trace" is route 4 (debug build).

| Statement | Tree trace | Rename walker | Schema | Lemon trace | Why no tree, where none |
|---|---|---|---|---|---|
| `select`, `values`, `with ... select` | yes, after the parse-time folds; multi-row `VALUES` lost | yes, as a view; no parameters, no other schemas, schema names reduced to a flag | yes, as a view; no positions | yes | |
| `insert` | yes, with upsert and `WITH`; `RETURNING` only as a word; rows after the first of `VALUES` lost | yes, as a trigger step; no `WITH`, `DEFAULT VALUES`, `RETURNING`, qualified target, parameters | as a trigger step; no positions | yes | |
| `update`, `delete` | yes, with `WITH` and `FROM`; `RETURNING` only as a word | yes, as a trigger step; no `WITH`, `INDEXED BY`, `NOT INDEXED`, `RETURNING`, qualified target, parameters | as a trigger step; no positions | yes | |
| `create table` | no | yes, as a `Table`, except `create table ... as select` | yes | yes | |
| `create index` | no | yes, as an `Index` | yes | yes | |
| `create view` | no | yes | yes | yes | |
| `create trigger` | no | yes | yes | yes | |
| `create virtual table` | no | arguments as text only | NV | yes; arguments as `ANY` tokens | arguments are not parsed |
| `drop table`, `index`, `view`, `trigger` | no | no | no | yes | the action codes the drop from a `SrcList` (`parse.y` 499, 513, 1708, 1854) |
| `alter table` (all forms, including `ADD CHECK`) | no | no | no | yes | the action codes it directly; `ADD COLUMN`'s table copy is freed after the parse (`parse.y` 1892-1930, `tokenize.c` 749) |
| `begin`, `commit`, `end`, `rollback`, `savepoint`, `release` | no | no | no | yes | `sqlite3BeginTransaction`, `sqlite3EndTransaction`, `sqlite3Savepoint` (`parse.y` 181-203) |
| `pragma` | no | no | no | yes | `sqlite3Pragma` takes tokens (`parse.y` 1724-1730) |
| `attach`, `detach` | no | no | no | yes | the expressions are coded at once (`parse.y` 1861-1866) |
| `vacuum`, `analyze`, `reindex` | no | no | no | yes | tokens only (`parse.y` 1715-1716, 1879-1886) |
| `explain`, `explain query plan` | the inner statement's tree | no | no | yes | `explain` is a flag on the parse (`parse.y` 172-174) |

## 10. Build questions

- **Route 2 needs no other build** (V). It ran with `options.h` and without `SQLITE_DEBUG`. It fits in the binding's translation unit as `s4ql_tokenize` does, and a build tag can leave it out of production builds (§3.5).
- **Routes 1 and 4 need `SQLITE_DEBUG`** (V, §2 and §5). The research on compile options found that `SQLITE_DEBUG` turns on thousands of `assert()`s, makes SQLite "approximately three times slower", adds nine pragmas, and reports `ENABLE_EXPLAIN_COMMENTS`, and it infers that a failed assert aborts the process ([sqlite-compile-options.md](sqlite-compile-options.md), the row for `SQLITE_DEBUG`). In `parse.y` the only code under `SQLITE_DEBUG` sets and checks the field `isCreate` (lines 135-137, 1758-1761), and `Expr` gains a field `vvaFlags` (`sqliteInt.h` 3077-3079). So the trees a debug build builds should be those of the production build (I; not compared node by node in this run).
- **A debug copy and what the binding's diary settled.** The diary rejected a test-only package with its own compile of the amalgamation for the tokenizer oracle. The go command does not track files outside a package's directory, a second compile of the 9 MB amalgamation would be added to every test build, and a second copy could drift from production's options ([../diary/2026-11-13-tokenizer.md](../diary/2026-11-13-tokenizer.md), "What I did"). Two copies can share one test binary only with file-local symbols, and must never open the same file ([../diary/2026-11-10-sqlite-binding.md](../diary/2026-11-10-sqlite-binding.md), Step 2). A debug copy of route 1 also writes to the process's stdout, so in-process use would need the file descriptor redirected (I). The clean form is a separate process: the tests already build the official shell from the vendored source with `options.h` (`internal/sqlite/README.md`, "Tests"), and a second shell with `-DSQLITE_DEBUG -DSQLITE_ENABLE_TREETRACE` is the same mechanism. Run on 3.54.1: that shell builds from the vendored files unchanged and prints the dumps of §2.
- **Test-only C in the production binding.** With route 2 behind a build tag, the production binding would carry none of it. Without a tag, it would carry the walker, which was 350 lines of C in this run, plus the call of `renameParseSql`. Go's linker drops unused Go functions, but this C would stay in the binary (I).

## 11. Stability across versions

- **What breaks at compile time** (V): a renamed or removed field. Between 3.45.0 and 3.50.0, `SrcItem` lost `zDatabase`, `pSchema`, `pTab`, `pSelect`, `addrFillSub`, `regReturn`, and `regResult`, and gained `pSTab`, the flags `isSubquery`, `fixedSchema`, and `hadSchema`, and the unions `u3` and `u4` with `Subquery *pSubq`. Between 3.45.0 and 3.53.0, `TriggerStep` replaced `zTarget` and `pFrom` with `pSrc`. Between 3.50.0 and 3.54.1, `SrcItem.fg` gained `fromExists`. (Diffs of `sqliteInt.h` at those tags.) A walker that names `item->pSelect` fails to compile on 3.54.1.
- **What breaks silently** (V for the examples, I for the general claim):
  - A union read under the wrong flag. `u4` holds `zDatabase`, `pSchema`, or `pSubq` depending on `fg.fixedSchema` and `fg.isSubquery`; a walker that misses a new flag compiles and misreads. In this run, a first version of the walker read a virtual table's `u.vtab` as `u.tab` and printed a foreign key that does not exist.
  - A new field or clause that the walker does not print, such as a new `Select` member, or the `fromExists` flag above. It is dropped from both sides of the comparison.
  - A change in lowering. In 3.46.0, multi-row `VALUES` started to be compiled during parsing (§1), which changed route 1's output for every such statement without changing any field.
  - The text of route 1 and route 4. `treeview.c` is a debugging aid, and its output format is not documented as an interface (NV whether it changed between versions). Rule numbers in route 4 change whenever the grammar does.
  - Rename mode belongs to `ALTER TABLE`'s implementation. A change in how `alter.c` uses it, such as a new place where rename mode skips a fold, would change route 2's trees silently.
- **`TK_` codes** are renumbered by Lemon per version, as the tokenizer's diary notes. The walker used names generated from the `#define TK_` lines of the vendored `sqlite3.c`, not numbers.

## 12. Costs in trust, per route

- **Route 1, tree trace.** The oracle is a debug build, not the production object; its trees should be equal (§10), but that is inferred. Its output is a debugging format that must be parsed and has no promise of stability. It loses quote styles, positions, `RETURNING` expressions, and multi-row `VALUES`, and it mixes in dumps of nested and rewritten statements. In normal mode it shows the fold that can replace a whole `WHERE` with 0. A failed assert ends the run.
- **Route 2, rename walker.** The oracle is the production object, but in a parse mode that production never uses for application SQL, with the differences of §3.3. The trees are wrapped in `CREATE` statements, which refuse some inputs and change the grammar for trigger bodies (§3.2); the refusals are errors, so no case is lost silently. The walker is code of ours that reads private structs, and its own bugs look like SQLite's answers. A misread union is one such bug, shown in this run. That walker must be reviewed at each version bump, and tested, for example by printing a tree for every rule of `parse.y` and checking that each clause appears.
- **Route 3, schema.** The same costs as route 2, with fewer facts in the tree.
- **Route 4, Lemon trace.** A debug build. The derivation is of Lemon's optimized grammar, and using it ties s4ql's parser to the structure of `parse.y`.
- **Route 5, a patched copy of `sqlite3.c` for tests.** A callback at the four trace points of §2, or at the end of the parse, would see every top-level tree in normal mode, also with parameters and `RETURNING`, without `SQLITE_DEBUG`. The cost: the oracle is no longer the code that sqlite.org published and that `make sqlite-verify` checks byte for byte (`internal/sqlite/README.md`, "Updating SQLite"). It is a second, different object for tests, with the build and drift problems of a second copy (§10), and the patch must be carried to each version. Not tried in this run.

## 13. What the evidence favours

Route 2, for the statement kinds that have a tree, with `lower` on s4ql's side. Reasons:

- **The same code.** It runs the production compile of SQLite with the production options. No second copy, no `SQLITE_DEBUG`, and no change to the vendored source are needed, which is the standard the tokenizer oracle set.
- **More of the input.** It keeps quote flags, rename tokens with raw text and positions, the whole `WHERE` where normal mode folds it to 0, and every row of `VALUES`. It also gives `create table`, `create index`, `create view`, and `create trigger`, which route 1 does not print.
- **Gaps that show.** What it cannot wrap fails with an error (§3.2), so a harness can count those cases as "no tree" and never compare a wrong tree by accident.

Its costs are the walker's own correctness and its upkeep across versions (§11, §12). Both fall on code that s4ql owns and can test.

Route 1 covers what route 2 refuses: parameters, `WITH` on `insert`, `update`, and `delete`, `INDEXED BY`, qualified targets, and the normal parse mode itself. It could be a second check, run in a debug shell as a separate process, which fits the existing shell build. Route 4 is an oracle of another kind, a derivation, and the only one that covers every statement. It suits a parser that follows `parse.y` rule by rule, such as option 2 of [sql-front-end.md](sql-front-end.md) §2.4. The evidence does not favour route 5 while routes 1 and 2 reach the same trees without a patch.

For `drop`, `alter`, transactions, `pragma`, `attach`, `vacuum`, `analyze`, and `reindex`, no tree exists in SQLite. Their check stays with accept-or-reject, SQLite's errors, the effect on the schema and the data, and hand-written expectations.

Open for Markus: whether s4ql wants a tree oracle at all, given the lowering in §7. If so, which routes, and whether a debug build of SQLite may exist in the tests as a separate process.

## Not verified

- That a debug build builds the same trees as the production build for every input (§10). It holds for the statements run here.
- Whether the output format of `treeview.c` changed between versions.
- Route 3 for virtual tables.
- Route 5: no patched copy was built.
- That rename mode differs from normal mode in no other way that changes trees (§3.3). This was read from every use of the two macros, not tested exhaustively.
- Whether Go's internal or external linker drops C functions that Go never calls (§10).

## Sources

- SQLite 3.54.1: `internal/sqlite/c/sqlite3.c`, `shell.c`; `src/parse.y`, `src/treeview.c`, `src/select.c`, `src/insert.c`, `src/update.c`, `src/delete.c`, `src/build.c`, `src/alter.c`, `src/trigger.c`, `src/expr.c`, `src/util.c`, `src/tokenize.c`, `src/attach.c`, `src/auth.c`, `src/vtab.c`, `src/window.c`, `src/main.c`, `src/global.c`, `src/sqliteInt.h`, `tool/lemon.c` at https://github.com/sqlite/sqlite/tree/version-3.54.1.
- `src/sqliteInt.h` at the tags `version-3.45.0`, `version-3.46.0`, `version-3.50.0`, `version-3.53.0` of the same repository.
- https://sqlite.org/opcode.html.
- Go 1.27.4: `go doc cmd/cgo`.
- This repository: [sql-front-end.md](sql-front-end.md), [rqlite-sql.md](rqlite-sql.md), [sqlite-compile-options.md](sqlite-compile-options.md), `internal/sqlite/README.md`, `internal/sqlite/sqlite.go`, [../diary/2026-11-10-sqlite-binding.md](../diary/2026-11-10-sqlite-binding.md), [../diary/2026-11-13-tokenizer.md](../diary/2026-11-13-tokenizer.md).
