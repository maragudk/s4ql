package stmttest

import (
	"fmt"
	"slices"
	"strings"

	"maragu.dev/s4ql/internal/sql/token"
)

// severalLeads stand before the first statement of a text of several statements: spaces, comments,
// and empty statements, which SQLite and mattn skip.
var severalLeads = []string{" ", "\n", "-- c\n", "/* c */ ", ";", " ; ", ";\n-- c\n"}

// severalSeparators stand between two statements of a text of several statements: a semicolon, with
// spaces, comments, and empty statements around it.
var severalSeparators = []string{";", "; ", ";\n", " ;\t", ";\r\n", "; -- c\n", ";/* c */", "/* c */;\n", ";;", "; ;\n", ";\n-- c\n;\n", "\n;"}

// severalEnds stand after the last statement of a text of several statements: a semicolon, with
// spaces after it, which mattn trims, an empty statement or a comment after it, for which mattn's
// result is 0 rows affected, and a comment without a semicolon, which belongs to the statement.
var severalEnds = []string{";", "; ", ";\n", ";;", "; ;", "; -- c", "; /* c */", ";\n-- c\n", " -- c", " /* c */"}

// severalTrimmed are spaces that Go's strings.TrimSpace removes, which mattn trims from the tail of
// a text after each statement, and that SQLite reads as part of a token: characters of a name from
// 0x80 on, and the vertical tab, which is an illegal token where a token starts.
var severalTrimmed = []string{" ", "\v", " ", "\u0085", "　", " ", " "}

// severalTrimmedEnds end a text of several statements in a token that Go's TrimSpace changes, which
// s4ql refuses: a block comment without its end, which the trim makes a slash and a star, and a
// string without its closing quote, whose spaces the trim takes from SQLite's message.
var severalTrimmedEnds = []string{"; /* ", "; select 'abc  ", ";\n/*\t"}

// several is a call of a text of several statements, two to five, as glue's migrations and goqite's
// schema run through Exec, with the arguments of their parameters in order. Each statement is one
// that the sequence writes, as [Sequence.statement] writes it, with the number of a statement of the
// sequence of its own, so that each draws as a statement alone does, and the last one, one time in
// three, with RETURNING if it is an insert, an update, or a delete; or, one time in twelve after
// the first, an earlier statement of the text again, such as an insert that then fails on its key;
// or, as a fault one time in 24 each, a text of [Others] or of [ArithmeticRefused], which SQLite does
// not prepare, s4ql does not support, or that fails, in the middle of the text. Before the first
// statement, one time in four, and between two, stand spaces, comments, and empty statements, of
// [severalLeads] and [severalSeparators], and after the last one of [severalEnds], or nothing. As
// faults: the arguments lack the last one, one time in 16, or have one more, one time in 16; and
// one time in twelve a space of [severalTrimmed] starts the text after a semicolon or ends the
// text, or the text ends in one of [severalTrimmedEnds], which s4ql refuses. One call in eight is a
// query, which s4ql refuses for its second statement, as it refuses a prepared statement of the text.
func (q *Sequence) several() Call {
	r := q.rSeveral
	n := 2 + r.IntN(4)
	var b strings.Builder
	if r.IntN(4) == 0 {
		b.WriteString(severalLeads[r.IntN(len(severalLeads))])
	}
	// texts and args of the statements that the text holds, for one that runs again.
	var texts []string
	var args [][]any
	each := true
	trimmed, trimmedAt := q.fault(r, areaSeveral, 12), -1
	if trimmed {
		// After the semicolon of the statement of the number, or at the end for n.
		trimmedAt = 1 + r.IntN(n)
	}
	for j := range n {
		if j > 0 {
			// Each statement draws from the streams of a statement of its own.
			q.step++
			r = q.rSeveral
			sep := severalSeparators[r.IntN(len(severalSeparators))]
			b.WriteString(sep)
			if trimmedAt == j {
				b.WriteString(severalTrimmed[r.IntN(len(severalTrimmed))])
			}
		}
		var text string
		var a []any
		switch d := r.IntN(12); {
		case d == 0 && j > 0:
			i := r.IntN(len(texts))
			text, a = texts[i], args[i]
		case d == 1 && q.fault(r, areaSeveral, 2):
			text, a = q.other(r, true)
			each = false
		case d == 2 && q.profile.on(areaArithmetic) && q.fault(r, areaSeveral, 2):
			form := ArithmeticRefused[r.IntN(len(ArithmeticRefused))]
			text = fmt.Sprintf(form, q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))])
			each = false
		default:
			// One last statement in three that can have RETURNING has it.
			q.received = j == n-1 && q.profile.on(areaReturning) && r.IntN(3) == 0
			c := q.statement()
			q.received = false
			text, a = c.Src, c.Args
		}
		texts, args = append(texts, text), append(args, a)
		b.WriteString(text)
		if openComment(text) {
			// A line comment at the end would hold what follows.
			b.WriteString("\n")
		}
	}
	end := ""
	if r.IntN(3) != 0 {
		end = severalEnds[r.IntN(len(severalEnds))]
	}
	if trimmedAt == n {
		if r.IntN(3) == 0 {
			end = severalTrimmedEnds[r.IntN(len(severalTrimmedEnds))]
		} else {
			end += severalTrimmed[r.IntN(len(severalTrimmed))]
		}
	}
	b.WriteString(end)
	var all []any
	for _, a := range args {
		all = append(all, a...)
	}
	if len(all) > 0 && q.fault(r, areaSeveral, 16) {
		all = all[:len(all)-1]
	} else if q.fault(r, areaSeveral, 16) {
		all = append(all, q.anyValue(r))
	}
	return Call{Src: b.String(), Args: all, Query: r.IntN(8) == 0, Statements: n, Each: each}
}

// other is a text of [Others] at random from r, with a table and a column of the sequence, and a
// value of the sequence as the argument of each ?. If statement, it is one that holds a statement,
// for a text of several statements.
func (q *Sequence) other(r Rand, statement bool) (string, []any) {
	forms := Others
	if statement {
		forms = othersWithStatement
	}
	form := forms[r.IntN(len(forms))]
	table, column, value := q.tables[r.IntN(len(q.tables))], q.columns[r.IntN(len(q.columns))], r.IntN(5)
	src := form
	// Sprintf would add the arguments that a form without a verb does not use to its text.
	if strings.Contains(form, "%") {
		src = fmt.Sprintf(form, table, column, value)
	}
	var args []any
	for range strings.Count(src, "?") {
		args = append(args, q.anyValue(r))
	}
	return src, args
}

// othersWithStatement are the texts of [Others] that hold a statement: a token that is no space, no
// comment, and no semicolon.
var othersWithStatement = func() []string {
	var forms []string
	for _, form := range Others {
		if slices.ContainsFunc(token.Tokens(form), func(t token.Token) bool {
			return t.Kind != token.Space && t.Kind != token.Comment && t.Kind != token.Semi
		}) {
			forms = append(forms, form)
		}
	}
	return forms
}()

// openComment reports whether src ends in a line comment without the end of its line, which would
// hold a text written after it.
func openComment(src string) bool {
	ts := token.Tokens(src)
	if len(ts) == 0 {
		return false
	}
	last := ts[len(ts)-1]
	text := src[last.Offset : last.Offset+last.Len]
	return last.Kind == token.Comment && strings.HasPrefix(text, "--") && !strings.HasSuffix(text, "\n")
}
