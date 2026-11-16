// Package parsetest compares a SQL parser under test with SQLite, through [oracle.Judge]: on whether
// the first statement of a text parses, on the exact message and byte offset of a syntax error, and
// on where the statement ends.
//
// A parser implements [Parser]. [Run] compares it with SQLite on many texts, such as the entries of
// the corpus in internal/sql/corpus, and gives a [Report] that groups the disagreements by their
// [Mismatch] and a key, so that they can be worked through group by group. [Mutants] makes one-token
// mutations of a text for more inputs near valid ones.
package parsetest

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"testing"

	"maragu.dev/s4ql/internal/sql/oracle"
	"maragu.dev/s4ql/internal/sql/token"
)

// Parser under test. Parse is given the whole text, which may hold more than one statement, NUL
// bytes, and bytes that are not UTF-8, and says what it found for the first statement, as SQLite
// would: it reads the text up to its first NUL byte, skips empty statements (semicolons with nothing
// between them), and stops at the first syntax error.
type Parser interface {
	Parse(src string) Result
}

// ParserFunc is a function that is a [Parser].
type ParserFunc func(src string) Result

// Parse src with f.
func (f ParserFunc) Parse(src string) Result {
	return f(src)
}

// Result of a parser for the first statement of a text.
type Result struct {
	// OK is true if the statement parses.
	OK bool

	// Message of the syntax error if the statement does not parse, in SQLite's words, such as
	// `near "frm": syntax error`.
	Message string

	// Offset of the syntax error in bytes from the start of the text, as sqlite3_error_offset gives
	// it, or -1 where SQLite gives none, as for `incomplete input`.
	Offset int

	// Tail is the offset of the first byte after the statement if it parses: after its semicolon, or
	// at the end of the text if it has none. It is not compared for a statement that does not parse.
	Tail int
}

// Oracle is a [Parser] that gives SQLite's own verdict. Comparing it with SQLite gives no
// disagreement, which is a check of the harness. For a text on which SQLite does not say, it gives
// SQLite's message as a syntax error; such texts are not compared.
func Oracle() Parser {
	return ParserFunc(func(src string) Result {
		v := oracle.Judge(src)
		if v.Kind == oracle.Parses {
			return Result{OK: true, Offset: -1, Tail: v.Tail}
		}
		return Result{Message: v.Message, Offset: v.Offset}
	})
}

// Mismatch is a kind of disagreement between a parser and SQLite.
type Mismatch int

const (
	// AcceptsInvalid means that the parser accepts a statement that SQLite rejects with a syntax
	// error. It is the most dangerous kind, since s4ql must reject what it cannot read as SQLite does.
	AcceptsInvalid Mismatch = iota + 1

	// RejectsValid means that the parser rejects a statement that SQLite parses.
	RejectsValid

	// Message means that both reject the statement, with different messages.
	Message

	// Offset means that both reject the statement with the same message, at different offsets.
	Offset

	// Tail means that both parse the statement, and they differ on where it ends.
	Tail
)

func (m Mismatch) String() string {
	switch m {
	case AcceptsInvalid:
		return "accepts what SQLite rejects"
	case RejectsValid:
		return "rejects what SQLite parses"
	case Message:
		return "other message"
	case Offset:
		return "other offset"
	case Tail:
		return "other tail"
	default:
		return fmt.Sprint("Mismatch(", int(m), ")")
	}
}

// Disagreement of a parser with SQLite on one text.
type Disagreement struct {
	Mismatch Mismatch

	// Key that groups disagreements of the same kind that likely have the same cause: SQLite's or the
	// parser's message with the quoted token replaced by its kind, and the difference in offset or
	// tail.
	Key string

	SQL     string
	Verdict oracle.Verdict
	Result  Result
}

func (d Disagreement) String() string {
	return fmt.Sprintf("%v: %q: SQLite: %v %q at %v, tail %v; parser: ok=%v %q at %v, tail %v",
		d.Mismatch, d.SQL, d.Verdict.Kind, d.Verdict.Message, d.Verdict.Offset, d.Verdict.Tail,
		d.Result.OK, d.Result.Message, d.Result.Offset, d.Result.Tail)
}

// Compare a parser's result for src with SQLite's verdict on it. It reports false if they agree, or
// if SQLite does not say. Where SQLite does not say the offset of a syntax error, it compares the
// rest.
func Compare(src string, v oracle.Verdict, r Result) (Disagreement, bool) {
	d := Disagreement{SQL: src, Verdict: v, Result: r}
	switch v.Kind {
	case oracle.Parses:
		switch {
		case !r.OK:
			d.Mismatch, d.Key = RejectsValid, Pattern(r.Message)
		case r.Tail != v.Tail:
			d.Mismatch, d.Key = Tail, fmt.Sprintf("tail %+d", r.Tail-v.Tail)
		default:
			return d, false
		}
	case oracle.SyntaxError:
		sqlite := Pattern(v.Message)
		switch {
		case r.OK:
			d.Mismatch, d.Key = AcceptsInvalid, sqlite
		case r.Message != v.Message:
			d.Mismatch, d.Key = Message, "SQLite "+sqlite+", parser "+Pattern(r.Message)
		case v.OffsetNotSaid:
			return d, false
		case r.Offset != v.Offset && v.Offset == -1:
			d.Mismatch, d.Key = Offset, sqlite+": SQLite -1"
		case r.Offset != v.Offset && r.Offset == -1:
			d.Mismatch, d.Key = Offset, sqlite+": parser -1"
		case r.Offset != v.Offset:
			d.Mismatch, d.Key = Offset, fmt.Sprintf("%v: offset %+d", sqlite, r.Offset-v.Offset)
		default:
			return d, false
		}
	default:
		return d, false
	}
	return d, true
}

// Check a parser on src against SQLite, and give SQLite's verdict too.
func Check(p Parser, src string) (Disagreement, bool, oracle.Verdict) {
	v := oracle.Judge(src)
	d, ok := Compare(src, v, p.Parse(src))
	return d, ok, v
}

// Pattern of an error message that groups messages of the same form: the quoted token of a syntax
// error becomes its kind, as `near ID: syntax error` or `unrecognized token: STRING...`, and for any
// other message the part after the first colon becomes `...` and anything quoted `"..."`.
func Pattern(msg string) string {
	if text, ok := strings.CutPrefix(msg, `near "`); ok {
		if text, ok := strings.CutSuffix(text, `": syntax error`); ok {
			return "near " + kindOf(text) + ": syntax error"
		}
	}
	if text, ok := strings.CutPrefix(msg, `unrecognized token: "`); ok {
		if text, ok := strings.CutSuffix(text, `"`); ok {
			return "unrecognized token: " + unrecognized(text)
		}
	}
	if before, _, ok := strings.Cut(msg, ": "); ok {
		msg = before + ": ..."
	}
	var b strings.Builder
	quoted := false
	for _, r := range msg {
		switch {
		case r == '"' && !quoted:
			quoted = true
			b.WriteString(`"..."`)
		case r == '"':
			quoted = false
		case !quoted:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// kindOf the token whose text is in a syntax error, by the name of its kind, such as ID or SELECT.
func kindOf(text string) string {
	kind, n := token.Next(text)
	if n != len(text) {
		return "?"
	}
	return kind.String()
}

// unrecognized describes an unrecognized token by its start.
func unrecognized(text string) string {
	switch {
	case text == "":
		return "empty"
	case strings.HasPrefix(text, "x'") || strings.HasPrefix(text, "X'"):
		return "BLOB..."
	case text[0] == '\'':
		return "STRING..."
	case text[0] == '"' || text[0] == '`' || text[0] == '[':
		return "ID..."
	case text[0] >= '0' && text[0] <= '9' || text[0] == '.':
		if strings.Contains(text, "_") {
			return "QNUMBER"
		}
		return "NUMBER..."
	default:
		return fmt.Sprintf("%q", text[:1])
	}
}

// Report of a run of a parser against SQLite.
type Report struct {
	// Total number of texts, Agreed on, and NotSaid by SQLite.
	Total, Agreed, NotSaid int

	// OffsetNotSaid is the number of texts with a syntax error whose offset SQLite does not say, see
	// [oracle.Verdict.OffsetNotSaid], on which the parser agrees with SQLite on everything else. They
	// are not in Agreed.
	OffsetNotSaid int

	// Groups of disagreements, the largest first.
	Groups []Group

	// NotSaidGroups of the texts on which SQLite does not say, by the reason and the pattern of
	// SQLite's message, the largest first, and of those in OffsetNotSaid, with the reason "offset not
	// said". They are not compared, or not on the offset.
	NotSaidGroups []Group
}

// Group of disagreements with the same [Mismatch] and key, or of texts on which SQLite does not say.
type Group struct {
	Mismatch Mismatch // 0 for a group of texts on which SQLite does not say
	Key      string
	Count    int

	// Examples of the group, the shortest first, at most [maxExamples].
	Examples []Disagreement
}

// maxExamples of each group in a [Report].
const maxExamples = 5

// Disagreements in the report.
func (r Report) Disagreements() int {
	return r.Total - r.Agreed - r.NotSaid - r.OffsetNotSaid
}

// String of the report, with each group and its examples.
func (r Report) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%v texts: %v agree, %v disagree, SQLite does not say on %v, nor the offset on %v\n",
		r.Total, r.Agreed, r.Disagreements(), r.NotSaid, r.OffsetNotSaid)
	for _, g := range r.Groups {
		fmt.Fprintf(&b, "\n%v, %v: %v\n", g.Mismatch, g.Key, g.Count)
		for _, d := range g.Examples {
			fmt.Fprintf(&b, "  %v\n", d)
		}
	}
	if len(r.NotSaidGroups) > 0 {
		fmt.Fprintf(&b, "\nSQLite does not say:\n")
		for _, g := range r.NotSaidGroups {
			fmt.Fprintf(&b, "  %v: %v, for example %q\n", g.Key, g.Count, g.Examples[0].SQL)
		}
	}
	return b.String()
}

// Run the parser on every text, compare it with SQLite on each, and report. The report is the same
// for the same texts in any order.
//
// The texts are judged one after the other: SQLite takes global locks when it opens a connection and
// allocates memory, and with several goroutines the run took three times as long.
func Run(p Parser, sqls []string) Report {
	r := Report{Total: len(sqls)}
	groups := map[[2]string]*Group{}
	add := func(m Mismatch, key string, d Disagreement) {
		k := [2]string{fmt.Sprint(int(m)), key}
		g := groups[k]
		if g == nil {
			g = &Group{Mismatch: m, Key: key}
			groups[k] = g
		}
		g.Count++
		g.Examples = append(g.Examples, d)
		slices.SortFunc(g.Examples, func(a, b Disagreement) int {
			return cmp.Or(cmp.Compare(len(a.SQL), len(b.SQL)), strings.Compare(a.SQL, b.SQL))
		})
		if len(g.Examples) > maxExamples {
			g.Examples = g.Examples[:maxExamples]
		}
	}
	for _, sql := range sqls {
		d, found, v := Check(p, sql)
		switch {
		case v.Kind == oracle.NotSaid:
			r.NotSaid++
			add(0, v.Reason.String()+": "+Pattern(v.Message), d)
		case found:
			add(d.Mismatch, d.Key, d)
		case v.OffsetNotSaid:
			r.OffsetNotSaid++
			add(0, "offset not said: "+Pattern(v.Message), d)
		default:
			r.Agreed++
		}
	}
	for _, g := range groups {
		if g.Mismatch == 0 {
			r.NotSaidGroups = append(r.NotSaidGroups, *g)
		} else {
			r.Groups = append(r.Groups, *g)
		}
	}
	order := func(a, b Group) int {
		return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.Mismatch, b.Mismatch), strings.Compare(a.Key, b.Key))
	}
	slices.SortFunc(r.Groups, order)
	slices.SortFunc(r.NotSaidGroups, order)
	return r
}

// Test the parser against SQLite on every text, and fail the test with the report if they disagree
// on any.
func Test(t testing.TB, p Parser, sqls []string) Report {
	t.Helper()
	r := Run(p, sqls)
	if r.Disagreements() > 0 {
		t.Errorf("the parser disagrees with SQLite:\n%v", r)
	}
	return r
}
