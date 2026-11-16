package shapetest

import (
	"math/rand/v2"
	"strings"
)

// Form of expression in SQLite's grammar, as a template of tokens in which each "$" is an operand.
type Form struct {
	Name   string
	Tokens []string
}

// slots of the form: the indexes of its operands in Tokens.
func (f Form) slots() []int {
	var s []int
	for i, t := range f.Tokens {
		if t == "$" {
			s = append(s, i)
		}
	}
	return s
}

func form2(name string, tokens ...string) Form {
	return Form{Name: name, Tokens: tokens}
}

func binary(op string) Form {
	return form2(op, append(append([]string{"$"}, strings.Fields(op)...), "$")...)
}

// Forms of expression of SQLite's grammar that the generator combines: every binary operator, every
// prefix and postfix form, IS in all its forms, BETWEEN, IN, LIKE and its kin with and without
// ESCAPE, COLLATE, CASE with and without an operand, and NOT in each combination, together with the
// forms that have parentheses of their own, as operands.
var Forms = func() []Form {
	var fs []Form
	for _, op := range []string{"OR", "AND", "=", "==", "<>", "!=", "<", "<=", ">", ">=", "&", "|", "<<", ">>", "+", "-",
		"*", "/", "%", "||", "->", "->>", "IS", "IS NOT", "IS DISTINCT FROM", "IS NOT DISTINCT FROM", "LIKE", "NOT LIKE",
		"GLOB", "NOT GLOB", "REGEXP", "NOT REGEXP", "MATCH", "NOT MATCH"} {
		fs = append(fs, binary(op))
	}
	fs = append(fs,
		form2("LIKE ESCAPE", "$", "LIKE", "$", "ESCAPE", "$"),
		form2("NOT LIKE ESCAPE", "$", "NOT", "LIKE", "$", "ESCAPE", "$"),
		form2("GLOB ESCAPE", "$", "GLOB", "$", "ESCAPE", "$"),
		form2("BETWEEN", "$", "BETWEEN", "$", "AND", "$"),
		form2("NOT BETWEEN", "$", "NOT", "BETWEEN", "$", "AND", "$"),
		form2("IN list", "$", "IN", "(", "$", ",", "$", ")"),
		form2("NOT IN list", "$", "NOT", "IN", "(", "$", ",", "$", ")"),
		form2("IN select", "$", "IN", "(", "SELECT", "$", ")"),
		form2("NOT IN select", "$", "NOT", "IN", "(", "SELECT", "$", ")"),
		form2("ISNULL", "$", "ISNULL"),
		form2("NOTNULL", "$", "NOTNULL"),
		form2("NOT NULL", "$", "NOT", "NULL"),
		form2("IS NULL", "$", "IS", "NULL"),
		form2("IS NOT NULL", "$", "IS", "NOT", "NULL"),
		form2("COLLATE", "$", "COLLATE", "NOCASE"),
		form2("unary NOT", "NOT", "$"),
		form2("unary -", "-", "$"),
		form2("unary +", "+", "$"),
		form2("unary ~", "~", "$"),
		form2("CASE with operand", "CASE", "$", "WHEN", "$", "THEN", "$", "ELSE", "$", "END"),
		form2("CASE", "CASE", "WHEN", "$", "THEN", "$", "ELSE", "$", "END"),
		form2("CAST", "CAST", "(", "$", "AS", "TEXT", ")"),
		form2("function", "coalesce", "(", "$", ",", "$", ")"),
		form2("EXISTS", "EXISTS", "(", "SELECT", "$", ")"),
		form2("NOT EXISTS", "NOT", "EXISTS", "(", "SELECT", "$", ")"),
		form2("subquery", "(", "SELECT", "$", ")"),
		form2("row value", "(", "$", ",", "$", ")"),
	)
	return fs
}()

// leaves for the operands that are not themselves forms: mostly columns, to which [Check] gives
// values of every type, and some literals, which SQLite folds in some places.
var leaves = []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x1", "x2", "x3", "x4", "NULL", "'a'", "2", "'$.a'", "-4"}

// fill the operands of the tokens with leaves.
func fill(r *rand.Rand, tokens []string) []string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		if t == "$" {
			t = leaves[r.IntN(len(leaves))]
		}
		out[i] = t
	}
	return out
}

// nest the tokens of inner, with its operands filled, into the operand slot of outer.
func nest(r *rand.Rand, outer []string, slot int, inner []string) []string {
	var out []string
	out = append(out, outer[:slot]...)
	out = append(out, fill(r, inner)...)
	out = append(out, outer[slot+1:]...)
	return out
}

// Generated text, and the names of the forms it combines, the outer first.
type Generated struct {
	SQL   string
	Forms []string
}

// Pairs gives, for every ordered pair of [Forms] A and B, and every operand of A, a SELECT of A with
// B in that operand, without parentheses, so that SQLite's precedence and associativity decide the
// grouping. Each such text is given n times, with other values for the operands, from the seed.
func Pairs(seed uint64, n int) []Generated {
	r := rand.New(rand.NewPCG(seed, 0))
	var gs []Generated
	for _, a := range Forms {
		for _, slot := range a.slots() {
			for _, b := range Forms {
				for range n {
					sql := "SELECT " + strings.Join(fill(r, nest(r, a.Tokens, slot, b.Tokens)), " ")
					gs = append(gs, Generated{SQL: sql, Forms: []string{a.Name, b.Name}})
				}
			}
		}
	}
	return gs
}

// Triples gives n random combinations of three of [Forms] without parentheses, from the seed: A with
// B in an operand and C in an operand of B, or A with B and C in two of its operands.
func Triples(seed uint64, n int) []Generated {
	r := rand.New(rand.NewPCG(seed, 1))
	gs := make([]Generated, 0, n)
	for len(gs) < n {
		a, b, c := Forms[r.IntN(len(Forms))], Forms[r.IntN(len(Forms))], Forms[r.IntN(len(Forms))]
		as := a.slots()
		var tokens []string
		if r.IntN(2) == 0 || len(as) < 2 {
			bs := b.slots()
			inner := nest(r, b.Tokens, bs[r.IntN(len(bs))], c.Tokens)
			tokens = nest(r, a.Tokens, as[r.IntN(len(as))], inner)
		} else {
			i, j := r.IntN(len(as)), r.IntN(len(as)-1)
			if j >= i {
				j++
			}
			// Nest into the later slot first, so that the earlier slot keeps its index.
			first, second := max(as[i], as[j]), min(as[i], as[j])
			tokens = nest(r, a.Tokens, first, c.Tokens)
			tokens = nest(r, tokens, second, b.Tokens)
		}
		gs = append(gs, Generated{SQL: "SELECT " + strings.Join(fill(r, tokens), " "), Forms: []string{a.Name, b.Name, c.Name}})
	}
	return gs
}
