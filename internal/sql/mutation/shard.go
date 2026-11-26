package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// parseShard parses a shard written as i/n, the i-th of n, counted from 1.
func parseShard(s string) (i, n int, err error) {
	a, b, ok := strings.Cut(s, "/")
	if ok {
		i, err = strconv.Atoi(a)
	}
	if ok && err == nil {
		n, err = strconv.Atoi(b)
	}
	if !ok || err != nil || n < 1 || i < 1 || i > n {
		return 0, 0, fmt.Errorf("the shard %q is not i/n with 1 <= i <= n", s)
	}
	return i, n, nil
}

// shardOf gives the mutants of ms that the i-th of n shards runs, counted from 1. The shards are
// disjoint and together hold every mutant of ms, and the share of each depends only on the keys of
// ms and on which of them eq lists, not on the order of ms or the machine, so that shards that run
// apart agree on it.
//
// The mutants are dealt out one by one, like cards, in the order of their keys, those that eq lists
// first. A run of mutants in that order goes to the shards in turn, so each shard gets its share,
// within one, of the mutants of every file, and so of every package, and of the listed ones of every
// file. That balances the shards by expected cost without measuring it: a mutant of the engine costs
// many times one of the statement pass, and a listed equivalent mutant survives, so it runs every
// stage of its package. A shard measures the time of each stage on its own machine, and machines
// differ, so a split by measured times would not be the same in every shard.
//
// The shard's mutants come in the order of the deal, the listed ones first, so that the longest
// start first and the end of a shard does not wait on one of them.
func shardOf(ms []mutant, eq map[string]string, i, n int) []mutant {
	order := slices.Clone(ms)
	slices.SortFunc(order, func(a, b mutant) int {
		_, aEq := eq[a.Key]
		_, bEq := eq[b.Key]
		if aEq != bEq {
			if aEq {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Key, b.Key)
	})
	var share []mutant
	for j, m := range order {
		if j%n == i-1 {
			share = append(share, m)
		}
	}
	return share
}

// narrow the selection ms to the mutants of the files that the build of the tests compiles, then,
// if since is not empty, to those of the declarations that changed since that commit, or since the
// commit of [roundFile] if it is round, and then, if shard is not empty, to the share of that shard.
func narrow(root string, ms []mutant, eq map[string]string, since, shard string) ([]mutant, error) {
	ms, err := built(root, ms)
	if err != nil {
		return nil, err
	}
	if since != "" {
		rev := since
		if since == "round" {
			if rev, err = readRound(filepath.Join(root, roundFile)); err != nil {
				return nil, err
			}
		}
		if ms, err = changedSince(root, rev, ms); err != nil {
			return nil, err
		}
		fmt.Fprintf(os.Stderr, "%v mutants in the declarations that changed since %v\n", len(ms), rev)
	}
	if shard != "" {
		i, n, err := parseShard(shard)
		if err != nil {
			return nil, err
		}
		ms = shardOf(ms, eq, i, n)
	}
	return ms, nil
}

// pkgsOf the mutants ms, sorted.
func pkgsOf(ms []mutant) []string {
	var pkgs []string
	for _, m := range ms {
		pkgs = append(pkgs, m.Pkg)
	}
	slices.Sort(pkgs)
	return slices.Compact(pkgs)
}
