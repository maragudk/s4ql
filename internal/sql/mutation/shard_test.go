package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"maragu.dev/is"
)

func TestParseShard(t *testing.T) {
	tests := []struct {
		in   string
		i, n int
		ok   bool
	}{
		{in: "1/1", i: 1, n: 1, ok: true},
		{in: "3/20", i: 3, n: 20, ok: true},
		{in: "20/20", i: 20, n: 20, ok: true},
		{in: "0/4"},
		{in: "5/4"},
		{in: "1/0"},
		{in: "-1/4"},
		{in: "4"},
		{in: "a/b"},
		{in: "1/4/5"},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("should parse %q, or refuse it", test.in), func(t *testing.T) {
			i, n, err := parseShard(test.in)
			if !test.ok {
				is.True(t, err != nil, "no error")
				return
			}
			is.NotError(t, err)
			is.Equal(t, test.i, i)
			is.Equal(t, test.n, n)
		})
	}
}

func TestShardOf(t *testing.T) {
	// Mutants of three packages, in files of different sizes, of which some are listed as equivalent.
	var ms []mutant
	eq := map[string]string{}
	for p, files := range map[string][]int{"internal/engine": {57, 3, 120}, "internal/sql/stmt": {41, 1}, ".": {9}} {
		for f, size := range files {
			for j := range size {
				m := mutant{Pkg: p, File: fmt.Sprintf("%v/f%v.go", p, f), Key: fmt.Sprintf("%v/f%v.go g operator: %v", p, f, j)}
				ms = append(ms, m)
				if j%7 == 3 {
					eq[m.Key] = "because"
				}
			}
		}
	}
	for _, n := range []int{1, 2, 3, 4, 7, 20, 300} {
		shards := make([][]mutant, n)
		for i := range n {
			shards[i] = shardOf(ms, eq, i+1, n)
		}

		t.Run(fmt.Sprintf("should split into %v disjoint shards that together hold every mutant", n), func(t *testing.T) {
			seen := map[string]int{}
			for _, s := range shards {
				for _, m := range s {
					seen[m.Key]++
				}
			}
			is.Equal(t, len(ms), len(seen))
			for _, m := range ms {
				is.Equal(t, 1, seen[m.Key], m.Key)
			}
		})

		t.Run(fmt.Sprintf("should give each of %v shards its share of every file and of the listed equivalent mutants, within one", n), func(t *testing.T) {
			type group struct {
				file string
				eq   bool
			}
			total := map[group]int{}
			for _, m := range ms {
				_, isEq := eq[m.Key]
				total[group{m.File, isEq}]++
			}
			for _, s := range shards {
				got := map[group]int{}
				for _, m := range s {
					_, isEq := eq[m.Key]
					got[group{m.File, isEq}]++
				}
				for g, k := range total {
					is.True(t, got[g] == k/n || got[g] == (k+n-1)/n, fmt.Sprintf("%v of %v of %v", got[g], k, g))
				}
			}
		})

		t.Run(fmt.Sprintf("should give each of %v shards the same mutants, whatever the order of the selection", n), func(t *testing.T) {
			shuffled := slices.Clone(ms)
			rand.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
			for i := range n {
				var want, got []string
				for _, m := range shards[i] {
					want = append(want, m.Key)
				}
				for _, m := range shardOf(shuffled, eq, i+1, n) {
					got = append(got, m.Key)
				}
				is.EqualSlice(t, want, got)
			}
		})
	}

	t.Run("should put the listed equivalent mutants, which run every stage, first", func(t *testing.T) {
		s := shardOf(ms, eq, 2, 3)
		first := true
		for _, m := range s {
			_, isEq := eq[m.Key]
			if !isEq {
				first = false
			}
			is.True(t, first || !isEq, "a listed mutant after one that is not: "+m.Key)
		}
	})
}
