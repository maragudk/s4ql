package stmt

import (
	"strings"

	"maragu.dev/s4ql/internal/sql/sqlerr"
	"maragu.dev/s4ql/internal/sql/token"
)

// StartedTable gives the name of the table that the first statement of the text src creates, if
// SQLite's parser starts to create the table before it meets the syntax error that [Check] gave for
// src, and reports whether it does.
//
// SQLite starts a table while it parses, in sqlite3StartTable, as soon as its parser has read
// CREATE TABLE, the name, and the ( or AS after it, or CREATE VIRTUAL TABLE, the name, USING, the
// module, and the ( after it. If a table of the name exists then, SQLite gives `table %s already
// exists`, with the name as written, and never meets the syntax error. Whether a table exists is a
// fact of the database and not of the text, so StartedTable does not say what SQLite gives. SQLite
// does not look in the main schema for a table that is TEMP or in the schema temp, and does not give
// the error with IF NOT EXISTS, so StartedTable reports false for them. It reports false for a name
// in any other schema, and for a name that starts with sqlite_ in any case of the letters, for which
// SQLite gives `unknown database` and `object name reserved for internal use` first: errors of its
// grammar actions that the pass does not give.
//
// The name's value is the name without its quotes, as SQLite dequotes it: a name in double quotes,
// backticks, or single quotes, with a doubled quote for one, or in brackets.
func StartedTable(src string, syntax *sqlerr.Error) (Name, bool) {
	var tokens []token.Token
	for _, t := range token.Tokens(src) {
		if t.Kind != token.Space && t.Kind != token.Comment {
			tokens = append(tokens, t)
		}
	}
	for len(tokens) > 0 && tokens[0].Kind == token.Semi {
		tokens = tokens[1:]
	}
	i := 0
	is := func(kinds ...token.Kind) bool {
		if i+len(kinds) > len(tokens) {
			return false
		}
		for j, k := range kinds {
			if tokens[i+j].Kind != k {
				return false
			}
		}
		return true
	}
	if is(token.Explain, token.Query, token.Plan) {
		i += 3
	} else if is(token.Explain) {
		i++
	}
	virtual := false
	switch {
	case is(token.Create, token.Table):
		i += 2
	case is(token.Create, token.Virtual, token.Table):
		i += 3
		virtual = true
	default:
		return Name{}, false
	}
	if is(token.If, token.Not, token.Exists) || i+2 > len(tokens) {
		return Name{}, false
	}
	name := tokens[i]
	i++
	if is(token.Dot) {
		if i+2 > len(tokens) || asciiLower(dequoteAny(name.Text(src))) != "main" {
			return Name{}, false
		}
		name = tokens[i+1]
		i += 2
	}
	if virtual {
		if !is(token.Using) {
			return Name{}, false
		}
		i += 2
	}
	if !is(token.LP) && (virtual || !is(token.As)) {
		return Name{}, false
	}
	// The parser read the token after the name only if the syntax error is after it.
	if syntax.Offset >= 0 && syntax.Offset <= tokens[i].Offset {
		return Name{}, false
	}
	raw := name.Text(src)
	n := Name{Raw: raw, Value: dequoteAny(raw), Offset: name.Offset}
	if strings.HasPrefix(n.Fold(), "sqlite_") {
		return Name{}, false
	}
	return n, true
}

// dequoteAny gives the value of the name raw as sqlite3Dequote does: the text between double quotes,
// backticks, or single quotes, with a doubled quote for one, or between brackets, as it is.
func dequoteAny(raw string) string {
	if len(raw) < 2 {
		return raw
	}
	switch q := raw[0]; q {
	case '"', '`', '\'':
		return strings.ReplaceAll(raw[1:len(raw)-1], string([]byte{q, q}), string(q))
	case '[':
		return raw[1 : len(raw)-1]
	}
	return raw
}
