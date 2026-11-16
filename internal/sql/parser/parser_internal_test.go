package parser

import (
	"strings"
	"testing"

	"maragu.dev/is"
)

func TestParse_length(t *testing.T) {
	t.Run("should fail on a statement longer than the limit, spaces included", func(t *testing.T) {
		defer func(l int) { lengthLimit = l }(lengthLimit)
		lengthLimit = 100
		_, _, err := Parse("select 1" + strings.Repeat(" ", 100))
		is.Equal(t, Error{Message: "s4ql: the statement is longer than the limit of 100 bytes", Offset: -1, Limit: true}, *err.(*Error))
		_, _, err = Parse("select 1" + strings.Repeat(" ", 92))
		is.NotError(t, err)
	})

	t.Run("should count the length of each statement from where its parse starts", func(t *testing.T) {
		defer func(l int) { lengthLimit = l }(lengthLimit)
		lengthLimit = 100
		stmts, err := ParseAll("select 1;" + strings.Repeat(" ", 60) + "select 2" + strings.Repeat(" ", 30))
		is.NotError(t, err)
		is.Equal(t, 2, len(stmts))
	})
}

func TestParser_run(t *testing.T) {
	t.Run("should pass on a panic that is not a syntax error", func(t *testing.T) {
		defer func() {
			is.True(t, recover() != nil, "no panic")
		}()
		// A text shorter than its end makes the tokenizer slice past it, which only a bug can do.
		_, _, _ = newParser("select", 0, 100).run()
	})
}

func TestParser_advance(t *testing.T) {
	// i is the index of the token to consume: the empty semicolon, the end of input after it, or an
	// unrecognized token.
	tests := []struct {
		name string
		src  string
		i    int
	}{
		{"should refuse to consume the empty semicolon at the end", "", 0},
		{"should refuse to consume the end of input", "", 1},
		{"should refuse to consume an unrecognized token", "'abc", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defer func() {
				is.Equal(t, "parser: advance past the end or an unrecognized token", recover())
			}()
			p := newParser(test.src, 0, len(test.src))
			p.i = test.i
			p.advance()
		})
	}
}
