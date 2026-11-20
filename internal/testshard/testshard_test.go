package testshard_test

import (
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/testshard"
)

func TestParse(t *testing.T) {
	t.Run("should give the one shard that holds every subtest for an empty value", func(t *testing.T) {
		s, err := testshard.Parse("")
		is.NotError(t, err)
		is.Equal(t, "1/1", s.String())
		for i := range 10 {
			is.True(t, s.Holds(i))
		}
	})

	t.Run("should give the shard of i/n", func(t *testing.T) {
		s, err := testshard.Parse("2/3")
		is.NotError(t, err)
		is.Equal(t, "2/3", s.String())
	})

	for _, value := range []string{"0/3", "4/3", "1/0", "3", "a/b", "1/2/3", " 1/2", "1/2 ", "-1/2", "1/-2", "+1/2"} {
		t.Run(fmt.Sprintf("should refuse %q", value), func(t *testing.T) {
			_, err := testshard.Parse(value)
			is.True(t, err != nil)
		})
	}
}

func TestShard_Holds(t *testing.T) {
	t.Run("should put every subtest in exactly one of the n shards", func(t *testing.T) {
		for n := 1; n <= 16; n++ {
			for count := range 100 {
				held := make([]int, count)
				for i := 1; i <= n; i++ {
					s, err := testshard.Parse(fmt.Sprintf("%v/%v", i, n))
					is.NotError(t, err)
					for index := range count {
						if s.Holds(index) {
							held[index]++
						}
					}
				}
				for index, times := range held {
					is.Equal(t, 1, times, fmt.Sprintf("subtest %v of %v in %v shards", index, count, n))
				}
			}
		}
	})

	t.Run("should give the shards counts of subtests that differ by one at most", func(t *testing.T) {
		for n := 1; n <= 16; n++ {
			for count := range 100 {
				least, most := count, 0
				for i := 1; i <= n; i++ {
					s, err := testshard.Parse(fmt.Sprintf("%v/%v", i, n))
					is.NotError(t, err)
					var c int
					for index := range count {
						if s.Holds(index) {
							c++
						}
					}
					least, most = min(least, c), max(most, c)
				}
				is.True(t, most-least <= 1, fmt.Sprintf("%v subtests in %v shards: from %v to %v in a shard", count, n, least, most))
			}
		}
	})
}

func TestShard_zero(t *testing.T) {
	t.Run("should be the one shard that holds every subtest", func(t *testing.T) {
		var s testshard.Shard
		is.Equal(t, "1/1", s.String())
		for i := range 10 {
			is.True(t, s.Holds(i))
		}
	})
}

func TestGet(t *testing.T) {
	t.Run("should read the shard from S4QL_TEST_SHARD", func(t *testing.T) {
		t.Setenv("S4QL_TEST_SHARD", "3/4")
		is.Equal(t, "3/4", testshard.Get(t).String())
	})

	t.Run("should give the one shard without S4QL_TEST_SHARD", func(t *testing.T) {
		t.Setenv("S4QL_TEST_SHARD", "")
		is.Equal(t, "1/1", testshard.Get(t).String())
	})
}
