// Package testshard splits the subtests of a long test, such as the planted bugs of a simulation,
// across jobs of CI that run at the same time. Each job sets S4QL_TEST_SHARD to i/n, and runs the
// subtests whose index in the test's list, from 0, is i-1 modulo n:
//
//	shard := testshard.Get(t)
//	for i, bug := range plantedBugs {
//		if !shard.Holds(i) {
//			continue
//		}
//		t.Run(bug.name, ...)
//	}
//
// The shard is chosen by the index, not by a list of names, so that a subtest added to the list
// falls into exactly one shard by itself, and the n shards together run every subtest once, which
// the tests of this package show for every n up to 16. Without S4QL_TEST_SHARD, a run is the one
// shard 1/1, which holds every subtest, so a run on a developer's machine runs them all. A value
// that is not i/n, with 1 <= i <= n, fails the test, so that a typo cannot run nothing, and so does
// a shard that holds no subtest, as one does when there are more shards than subtests.
package testshard

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// Shard i of n, from 1. The zero Shard is the shard 1/1.
type Shard struct {
	i, n int
	// held is set by Holds when it holds a subtest, for the check of [Get].
	held *atomic.Bool
}

// Parse the shard from a value of S4QL_TEST_SHARD: i/n, or the empty string for the shard 1/1.
func Parse(value string) (Shard, error) {
	if value == "" {
		return Shard{i: 1, n: 1}, nil
	}
	is, ns, ok := strings.Cut(value, "/")
	i, errI := strconv.Atoi(is)
	n, errN := strconv.Atoi(ns)
	if !ok || errI != nil || errN != nil || is != fmt.Sprint(i) || ns != fmt.Sprint(n) || i < 1 || i > n {
		return Shard{}, fmt.Errorf("the shard %q is not i/n with 1 <= i <= n, such as 2/4", value)
	}
	return Shard{i: i, n: n}, nil
}

// Get the shard of S4QL_TEST_SHARD, and log it if the variable is set. A value that [Parse]
// refuses fails the test, and so does a shard of the variable that has held no subtest by the end
// of the test.
func Get(t testing.TB) Shard {
	t.Helper()
	value := os.Getenv("S4QL_TEST_SHARD")
	s, err := Parse(value)
	if err != nil {
		t.Fatalf("S4QL_TEST_SHARD: %v", err)
	}
	if value != "" {
		t.Logf("S4QL_TEST_SHARD is %v: this run holds the subtests whose index is %v modulo %v, and the other shards the others", s, s.i-1, s.n)
		s.held = new(atomic.Bool)
		t.Cleanup(func() {
			if !s.held.Load() {
				t.Errorf("S4QL_TEST_SHARD is %v, and the shard held no subtest: there are more shards than subtests", s)
			}
		})
	}
	return s
}

// Holds reports whether the subtest at the index of the test's list, from 0, is in the shard.
func (s Shard) Holds(index int) bool {
	if s.n == 0 {
		return true
	}
	holds := index%s.n == s.i-1
	if holds && s.held != nil {
		s.held.Store(true)
	}
	return holds
}

// String of the shard, as i/n.
func (s Shard) String() string {
	if s.n == 0 {
		return "1/1"
	}
	return fmt.Sprintf("%v/%v", s.i, s.n)
}
