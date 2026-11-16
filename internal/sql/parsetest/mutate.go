package parsetest

import (
	"hash/fnv"
	"math/rand/v2"
	"strings"

	"maragu.dev/s4ql/internal/sql/token"
	"maragu.dev/s4ql/internal/sqlite"
)

// operation of a one-token mutation.
type operation int

const (
	// opDelete a token.
	opDelete operation = iota + 1
	// opDuplicate a token: write it twice.
	opDuplicate
	// opSwap a token with the next one.
	opSwap
	// opReplace a token by a token of another kind.
	opReplace
	// opInsert a token before a token.
	opInsert
)

// Mutants of src, n of them, each made by one operation on one of its tokens: delete it, duplicate
// it, swap it with the next, replace it by a token of another kind, or insert a token before it. The
// token and the operation are chosen at random from seed and src, so the same seed and text always
// give the same mutants. Only the text before the first NUL byte is mutated, since SQLite reads no
// further; spaces and comments are left alone, and a token that is written or moved gets a space on
// each side, so that it does not join its neighbours into another token. A text without tokens has
// no mutants.
func Mutants(src string, seed uint64, n int) []string {
	tokens := token.Tokens(src)
	var words []token.Token
	for _, t := range tokens {
		if t.Kind != token.Space && t.Kind != token.Comment {
			words = append(words, t)
		}
	}
	if len(words) == 0 {
		return nil
	}
	end := 0
	if len(tokens) > 0 {
		last := tokens[len(tokens)-1]
		end = last.Offset + last.Len
	}
	src = src[:end]

	h := fnv.New64a()
	_, _ = h.Write([]byte(src))
	r := rand.New(rand.NewPCG(seed, h.Sum64()))

	mutants := make([]string, 0, n)
	for range n {
		i := r.IntN(len(words))
		w := words[i]
		before, text, after := src[:w.Offset], w.Text(src), src[w.Offset+w.Len:]
		op := operation(1 + r.IntN(5))
		if op == opSwap && i+1 == len(words) {
			op = opDuplicate
		}
		var m string
		switch op {
		case opDelete:
			m = before + " " + after
		case opDuplicate:
			m = before + text + " " + text + after
		case opSwap:
			v := words[i+1]
			between := src[w.Offset+w.Len : v.Offset]
			if between == "" {
				between = " "
			}
			m = before + " " + v.Text(src) + between + text + " " + src[v.Offset+v.Len:]
		case opReplace:
			m = before + " " + other(r, w.Kind) + " " + after
		case opInsert:
			m = before + " " + other(r, token.Illegal) + " " + text + after
		}
		mutants = append(mutants, m)
	}
	return mutants
}

// other token from [pool], of a kind other than k.
func other(r *rand.Rand, k token.Kind) string {
	for {
		text := pool[r.IntN(len(pool))]
		if kind, _ := token.Next(text); kind != k {
			return text
		}
	}
}

// pool of tokens to replace or insert: every keyword, every operator and punctuation, a literal of
// each kind, and names quoted in each way.
var pool = func() []string {
	p := []string{
		"(", ")", ",", ".", ";", "+", "-", "*", "/", "%", "||", "->", "->>", "&", "|", "~", "<<", ">>",
		"=", "==", "<>", "!=", "<", "<=", ">", ">=",
		"1", "0x1F", "1.5", "1e3", "1_000", "'a'", "x'00'", "?", "?1", ":a", "@a", "$a", "#a",
		"t1", "rowid", `"t"`, "[t]", "`t`", "'t'",
	}
	for _, k := range sqlite.Keywords() {
		p = append(p, strings.ToLower(k))
	}
	return p
}()
