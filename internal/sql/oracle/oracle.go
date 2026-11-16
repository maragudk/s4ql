// Package oracle gives SQLite's verdict on SQL text, so that s4ql's own SQL front end can be checked
// against SQLite, the judge for everything SQL.
//
// [Judge] says whether the first statement of a text parses in the vendored SQLite 3.54.1, and if
// not, SQLite's syntax error with its byte offset, and where the statement ends. It prepares the text
// with sqlite3_prepare_v2 through [sqlite.Conn.PrepareFirst] and never steps it. SQLite's parser runs
// the actions of its grammar while it parses, and some of those actions raise errors that are not
// about syntax, such as "no such table" or "unknown table option"; such an error can stop the parse
// before the end of the statement, and then SQLite has said nothing about the syntax of the rest. The
// verdict is then [NotSaid], and the oracle never guesses: a [Parses] or a [SyntaxError] is a fact
// about SQLite's grammar. How each verdict is decided is in the documentation of [Kind].
package oracle

import (
	"errors"
	"fmt"
	"strings"

	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// Kind of verdict.
type Kind int

const (
	// Parses means that SQLite's parser read the whole first statement up to and including its end,
	// the semicolon after it or the end of the text, without a syntax error. Either the statement
	// prepared, or the error came when the parser was handed the statement's end: the actions that
	// build the statement and look up names, such as "no such table", run when the end arrives, and
	// SQLite reports a syntax error on the end itself in place of any such message, so the message
	// left is not about syntax. The parser stops after the token on which an error was raised, so its
	// tail tells which token that was.
	Parses Kind = iota + 1

	// SyntaxError means that the first statement does not parse in SQLite, with any schema: the
	// message is one of the three that SQLite's parser front end gives, `near "X": syntax error`,
	// `incomplete input`, or `unrecognized token: "X"`. They come from the grammar's syntax error
	// routine, from the parse loop on a token that the tokenizer does not know, or from the two
	// actions that check a token's text: a variable `#N` outside SQLite's nested parses, and a
	// misplaced digit separator.
	SyntaxError

	// NotSaid means that SQLite does not say whether the first statement parses: something that is
	// not the grammar stopped the parse before the statement's end, or the oracle cannot tell whether
	// it did. [Verdict.Reason] says which.
	NotSaid
)

func (k Kind) String() string {
	switch k {
	case Parses:
		return "parses"
	case SyntaxError:
		return "syntax error"
	case NotSaid:
		return "not said"
	default:
		return fmt.Sprint("Kind(", int(k), ")")
	}
}

// Reason why SQLite does not say whether a statement parses.
type Reason int

const (
	// StoppedEarly means that an error that is not a syntax error, raised by an action of the grammar
	// or by a limit such as the parser's stack depth, stopped the parse before the statement's end.
	StoppedEarly Reason = iota + 1

	// InsideTrigger means that the parse stopped on a semicolon inside the body of CREATE TRIGGER,
	// which ends a step of the trigger and not the statement.
	InsideTrigger

	// InsideArguments means that the parse stopped on a semicolon inside the arguments of CREATE
	// VIRTUAL TABLE, which the grammar reads as any other token there.
	InsideArguments

	// Overwritten means that the parse reached the statement's end with an error that is not about
	// syntax, but the statement has a token whose own error, a variable #N or a misplaced digit
	// separator, comes from an action too and may have been replaced by the later message.
	Overwritten

	// Unsettled means that the parse stopped on the last token of a text that ends right after it,
	// and the text with a space added did not settle whether the parse had reached the end.
	Unsettled

	// NotParsed means that SQLite failed with a result code that its parser does not give, such as
	// SQLITE_TOOBIG or SQLITE_NOMEM, or did not parse the text at all.
	NotParsed
)

func (r Reason) String() string {
	switch r {
	case 0:
		return ""
	case StoppedEarly:
		return "stopped early"
	case InsideTrigger:
		return "inside trigger"
	case InsideArguments:
		return "inside arguments"
	case Overwritten:
		return "overwritten"
	case Unsettled:
		return "unsettled"
	case NotParsed:
		return "not parsed"
	default:
		return fmt.Sprint("Reason(", int(r), ")")
	}
}

// Verdict of SQLite on the first statement of a text.
type Verdict struct {
	Kind Kind

	// Message of SQLite's error: the syntax error for [SyntaxError], the error that stopped the parse
	// for [NotSaid], and for [Parses] the error that came after the parse, or "" if the statement
	// prepared.
	Message string

	// Offset of the error in bytes from the start of the text, as sqlite3_error_offset gives it, or -1
	// if SQLite gives none or there is no error.
	Offset int

	// Code is SQLite's extended result code, or 0 if the statement prepared.
	Code sqlite.ResultCode

	// Tail is the offset of the first byte after the first statement for [Parses]: after its
	// semicolon, or at the end of the text if it has none, where a NUL byte ends the text. For the
	// other kinds it is where SQLite's parser stopped reading, or -1 if it did not parse.
	Tail int

	// Reason is why SQLite does not say, for [NotSaid] only.
	Reason Reason

	// Setup holds the statements that were run on the empty database before the verdict, if the
	// oracle needed them for the parse to reach further; see [Judge].
	Setup []string
}

// Judge the first statement of src: whether it parses in SQLite, SQLite's syntax error if it does
// not, and where it ends. Any bytes are allowed; SQLite reads the text up to its first NUL byte. The
// verdict is the same on every call.
//
// Each call prepares src on new connections to an empty in-memory database, with the compile options
// and the default limits of the binding in internal/sqlite, comments allowed (the default of
// SQLITE_DBCONFIG_ENABLE_COMMENTS), and [sqlite.Conn.DenyProcessPragmas], since PRAGMAs such as
// soft_heap_limit act on the whole process when they are prepared. Statements that have to name an
// existing object stop early in an empty database: CREATE TRIGGER always stops at BEGIN with "no such
// table", and a name in an unknown schema stops at the next token with "unknown database". When the
// verdict is [NotSaid] with one of those messages, Judge creates the missing table, view, or attached
// database and judges again, a few times at most, and records what it ran in [Verdict.Setup]. The
// grammar does not depend on the schema, so a [Parses] or [SyntaxError] found that way holds for the
// empty database too.
func Judge(src string) Verdict {
	var s schema
	v := s.classify(src, s.prepare(src))
	for v.Kind == NotSaid && len(s.objects) < maxObjects && s.fix(v.Message) {
		r := s.prepare(src)
		if r.failed {
			// The object could not be created, so the verdict without it stands.
			break
		}
		v = s.classify(src, r)
	}
	return v
}

// maxObjects that [Judge] creates for one statement.
const maxObjects = 3

// schema that the text is judged against: objects created in an empty in-memory database.
type schema struct {
	objects []object
}

// object in the schema: a table, a view, or an attached database.
type object struct {
	kind     objectKind
	database string // the schema of a table or view as written in SQL, with a dot after it, or "" for main
	name     string // the name as SQLite gives it in its message, not quoted, if known
	raw      string // the name of an attached database as written in SQL, a single token
}

type objectKind int

const (
	table objectKind = iota
	view
	database
)

// setup statements that create the schema.
func (s *schema) setup() []string {
	var statements []string
	for _, o := range s.objects {
		switch o.kind {
		case table:
			statements = append(statements, "create table "+o.database+quote(o.name)+" (c)")
		case view:
			statements = append(statements, "create view "+o.database+quote(o.name)+" as select 1 as c")
		case database:
			statements = append(statements, "attach ':memory:' as "+o.raw)
		}
	}
	return statements
}

// result of preparing a text.
type result struct {
	err    *sqlite.Error // nil if the statement prepared
	tail   int
	failed bool // the setup or the connection failed, which is no verdict on the text
}

// prepare src on a new connection with the schema, and give SQLite's result.
func (s *schema) prepare(src string) result {
	c, err := sqlite.Open(":memory:")
	if err != nil {
		return result{failed: true}
	}
	defer func() { _ = c.Close() }()
	if err := c.DenyProcessPragmas(); err != nil {
		return result{failed: true}
	}
	for _, statement := range s.setup() {
		if err := c.Exec(statement); err != nil {
			return result{failed: true}
		}
	}

	stmt, tail, err := c.PrepareFirst(src)
	if err == nil {
		if stmt != nil {
			_ = stmt.Finalize()
		}
		return result{tail: tail}
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return result{failed: true}
	}
	return result{err: sqliteErr, tail: tail}
}

// classify SQLite's result r for src as a verdict.
func (s *schema) classify(src string, r result) Verdict {
	v := Verdict{Offset: -1, Tail: r.tail, Setup: s.setup()}
	if r.failed {
		v.Kind, v.Reason, v.Tail = NotSaid, NotParsed, -1
		return v
	}
	if r.err == nil {
		v.Kind = Parses
		return v
	}
	v.Message, v.Offset, v.Code = r.err.Message, r.err.Offset, r.err.Code

	primary := r.err.Code.Primary()
	if r.tail < 0 || (primary != codeError && primary != codeAuth) {
		v.Kind, v.Reason = NotSaid, NotParsed
		return v
	}
	if IsSyntaxMessage(r.err.Message) {
		if primary != codeError {
			v.Kind, v.Reason = NotSaid, NotParsed
			return v
		}
		v.Kind = SyntaxError
		return v
	}

	v.Kind = NotSaid
	v.Reason = s.reachedEnd(src, r)
	if v.Reason == 0 {
		v.Kind = Parses
	}
	return v
}

// Result codes from https://sqlite.org/rescode.html.
const (
	codeError = 1
	codeAuth  = 23
)

// IsSyntaxMessage reports whether msg has the form of one of the three messages of SQLite's parser
// front end: `near "X": syntax error`, `incomplete input`, or `unrecognized token: "X"`. No other
// message of SQLite starts or ends that way.
func IsSyntaxMessage(msg string) bool {
	if msg == "incomplete input" {
		return true
	}
	if strings.HasPrefix(msg, `near "`) && strings.HasSuffix(msg, `": syntax error`) && len(msg) > len(`near "": syntax error`) {
		return true
	}
	return strings.HasPrefix(msg, `unrecognized token: "`) && strings.HasSuffix(msg, `"`) && len(msg) > len(`unrecognized token: ""`)
}

// reachedEnd tells, for an error that is not about syntax, whether SQLite's parser had been handed
// the end of the first statement when the error was raised: it gives 0 if so, and otherwise the
// reason it cannot say.
//
// The parser stops after the token it was handed when an error was raised, and sets the tail after
// it; the end of the text is handed over as an empty semicolon, which leaves the tail at the end.
// So the token that ends at the tail is the one, unless the tail is at the end of the text and the
// last token ends there too: then the text is prepared again with a space added, where the empty
// semicolon would leave the tail one byte further.
func (s *schema) reachedEnd(src string, r result) Reason {
	tokens := token.Tokens(src)
	end := 0
	if len(tokens) > 0 {
		last := tokens[len(tokens)-1]
		end = last.Offset + last.Len
	}
	fed := fedTokens(tokens)
	if r.tail > end {
		return NotParsed
	}

	// atEnd is true if the parser had been handed the empty semicolon at the end of the text.
	atEnd := false
	stop := -1 // index in fed of the token that ends at the tail
	for i, t := range fed {
		if t.Offset+t.Len == r.tail {
			stop = i
		}
	}
	switch {
	case r.tail == end && (len(fed) == 0 || fed[len(fed)-1].Offset+fed[len(fed)-1].Len < end):
		atEnd = true
	case r.tail == end:
		probe := s.prepare(src[:end] + " ")
		if probe.failed || probe.err == nil || probe.err.Message != r.err.Message || probe.err.Code != r.err.Code ||
			!sameTokensAndSpace(tokens, token.Tokens(src[:end]+" ")) {
			return Unsettled
		}
		switch probe.tail {
		case end + 1:
			atEnd = true
		case end:
		default:
			return Unsettled
		}
	}
	if !atEnd && (stop < 0 || fed[stop].Kind != token.Semi) {
		return StoppedEarly
	}
	if !atEnd {
		fed = fed[:stop] // the tokens before the semicolon that ended the parse
	}

	// Leading semicolons are empty statements; the first statement starts after them.
	for len(fed) > 0 && fed[0].Kind == token.Semi {
		fed = fed[1:]
	}
	if len(fed) == 0 {
		return StoppedEarly
	}
	if reason := insideStatement(fed); reason != 0 {
		return reason
	}
	if s.hasLexicalError(src, fed) {
		return Overwritten
	}
	return 0
}

// insideStatement tells whether a semicolon after the tokens of a statement, which the parser read
// without a syntax error, is inside the statement rather than its end, as a semicolon can be in only
// two statements of SQLite's grammar.
//
// In CREATE TRIGGER, a semicolon ends each step of the body, and the statement ends after `; END`:
// after a step's semicolon the parser takes END as the end of the body, never as a name. So a
// semicolon after `; END` is the statement's end, and any other is inside the body.
//
// In CREATE VIRTUAL TABLE, the arguments of the module are any tokens, a semicolon too, with
// parentheses that must balance, so a semicolon is inside the arguments if a parenthesis is open.
func insideStatement(fed []token.Token) Reason {
	i := 0
	if fed[i].Kind == token.Explain {
		i++
		if i+1 < len(fed) && fed[i].Kind == token.Query && fed[i+1].Kind == token.Plan {
			i += 2
		}
	}
	if i >= len(fed) || fed[i].Kind != token.Create {
		return 0
	}
	i++
	if i < len(fed) && fed[i].Kind == token.Temp {
		i++
	}
	if i < len(fed) && fed[i].Kind == token.Trigger {
		n := len(fed)
		if n < 2 || fed[n-2].Kind != token.Semi || fed[n-1].Kind != token.End {
			return InsideTrigger
		}
		return 0
	}
	if i+1 < len(fed) && fed[i].Kind == token.Virtual && fed[i+1].Kind == token.Table {
		depth := 0
		for _, t := range fed {
			switch t.Kind {
			case token.LP:
				depth++
			case token.RP:
				depth--
			}
		}
		if depth != 0 {
			return InsideArguments
		}
	}
	return 0
}

// hasLexicalError tells whether one of the tokens is a variable #N or a number with a misplaced digit
// separator, which actions of the grammar reject as syntax errors. SQLite is asked about each number
// with a digit separator on its own.
func (s *schema) hasLexicalError(src string, fed []token.Token) bool {
	for _, t := range fed {
		text := t.Text(src)
		switch t.Kind {
		case token.Variable:
			if len(text) >= 2 && text[0] == '#' && text[1] >= '0' && text[1] <= '9' {
				return true
			}
		case token.QNumber:
			var empty schema
			r := empty.prepare("select " + text)
			if r.failed || (r.err != nil && IsSyntaxMessage(r.err.Message)) {
				return true
			}
		}
	}
	return false
}

// fedTokens of the tokens: those that SQLite's parse loop hands to the parser, which are all but
// whitespace and comments.
func fedTokens(tokens []token.Token) []token.Token {
	var fed []token.Token
	for _, t := range tokens {
		if t.Kind != token.Space && t.Kind != token.Comment {
			fed = append(fed, t)
		}
	}
	return fed
}

// sameTokensAndSpace tells whether probe is tokens with one space token of one byte added at the end.
func sameTokensAndSpace(tokens, probe []token.Token) bool {
	if len(probe) != len(tokens)+1 {
		return false
	}
	for i, t := range tokens {
		if probe[i] != t {
			return false
		}
	}
	last := probe[len(probe)-1]
	return last.Kind == token.Space && last.Len == 1
}

// fix the schema for the message of an error that stopped the parse early, if it names a missing
// object that can be created, and report whether it did.
func (s *schema) fix(msg string) bool {
	if name, ok := strings.CutPrefix(msg, "no such table: "); ok {
		db, name := s.split(name)
		if s.has(db, name) {
			return false
		}
		if db == "" {
			// A name with a dot whose first part is no known schema may be a table in a schema
			// that does not exist yet, as in ALTER TABLE aux.t1, so the schema is attached first.
			if first, rest, ok := strings.Cut(name, "."); ok && first != "" && rest != "" && !strings.ContainsRune(first, 0) {
				raw := quote(first)
				s.objects = append(s.objects, object{kind: database, raw: raw, name: first}, object{kind: table, database: raw + ".", name: rest})
				return true
			}
		}
		s.objects = append(s.objects, object{kind: table, database: db, name: name})
		return true
	}
	if name, ok := strings.CutPrefix(msg, "cannot create INSTEAD OF trigger on table: "); ok {
		return s.change(name, table, view)
	}
	for _, prefix := range []string{"cannot create BEFORE trigger on view: ", "cannot create AFTER trigger on view: "} {
		if name, ok := strings.CutPrefix(msg, prefix); ok {
			return s.change(name, view, table)
		}
	}
	if raw, ok := strings.CutPrefix(msg, "unknown database "); ok {
		if len(fedTokens(token.Tokens(raw))) != 1 || strings.IndexByte(raw, 0) >= 0 {
			return false
		}
		for _, o := range s.objects {
			if o.kind == database && o.raw == raw {
				return false
			}
		}
		s.objects = append(s.objects, object{kind: database, raw: raw})
		return true
	}
	return false
}

// split a table name from a message into its schema, quoted for SQL with a dot after it, and its
// name. The schema is main if the name has no prefix of main, temp, or a database attached here.
func (s *schema) split(name string) (string, string) {
	if db, rest, ok := strings.Cut(name, "."); ok {
		if db == "main" || db == "temp" {
			return db + ".", rest
		}
		for _, o := range s.objects {
			if o.kind == database && (o.raw == db || o.name == db) {
				return o.raw + ".", rest
			}
		}
	}
	return "", name
}

// has a table or view of the name in the schema db.
func (s *schema) has(db, name string) bool {
	for _, o := range s.objects {
		if o.kind != database && o.database == db && o.name == name {
			return true
		}
	}
	return false
}

// change the object of the name from one kind to another, and report whether there was one.
func (s *schema) change(name string, from, to objectKind) bool {
	for i, o := range s.objects {
		if o.kind == from && (o.name == name || o.database+o.name == name) {
			s.objects[i].kind = to
			return true
		}
	}
	return false
}

// quote a name for SQL in double quotes.
func quote(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
