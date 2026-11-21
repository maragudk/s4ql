package stmttest

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/sql/stmt"
)

func TestProfileOf(t *testing.T) {
	t.Run("should give each profile the seeds of its slot, and the mixed profile those of a slot that no profile has", func(t *testing.T) {
		is.Equal(t, Mixed, Profiles[0])
		names := map[string]bool{}
		for i, p := range Profiles {
			is.True(t, !names[p.Name], p.Name)
			names[p.Name] = true
			for k := range uint64(5) {
				is.Equal(t, uint64(i)+k*Slots, p.Seed(k))
				is.Equal(t, p, ProfileOf(p.Seed(k)))
			}
		}
		for slot := uint64(len(Profiles)); slot < Slots; slot++ {
			is.Equal(t, Mixed, ProfileOf(slot+7*Slots))
		}
	})

	t.Run("should have no weight for a create index or a drop index in a profile without indexes, and every area on in the mixed profile", func(t *testing.T) {
		for _, p := range Profiles {
			if p.Indexes == 0 {
				is.Equal(t, 0, p.Weights[KindCreateIndex]+p.Weights[KindDropIndex], p.Name)
			}
		}
		for _, area := range areas {
			is.True(t, Mixed.on(area), area)
		}
	})
}

func TestStepStream(t *testing.T) {
	t.Run("should give a statement the same numbers whatever the statements before it drew, and other numbers than another statement, area, or seed", func(t *testing.T) {
		draw := func(seed uint64, area string, step uint64, before int) [4]uint64 {
			at := uint64(0)
			s := newStepStream(seed, area, &at)
			for at = 1; at < step; at++ {
				for range before {
					s.Uint64()
				}
			}
			at = step
			return [4]uint64{s.Uint64(), s.Uint64(), uint64(s.IntN(1000)), s.Uint64()}
		}
		want := draw(7, areaInserts, 5, 0)
		is.Equal(t, want, draw(7, areaInserts, 5, 3))
		is.Equal(t, want, draw(7, areaInserts, 5, 100))
		is.True(t, want != draw(7, areaInserts, 6, 0), "another statement")
		is.True(t, want != draw(7, areaSelects, 5, 0), "another area")
		is.True(t, want != draw(8, areaInserts, 5, 0), "another seed")
	})

	t.Run("should give the thing of a name the same numbers for a seed and an area, and other numbers than another", func(t *testing.T) {
		draw := func(seed uint64, area, what string) [2]uint64 {
			r := newStream(seed, area, what)
			return [2]uint64{r.Uint64(), r.Uint64()}
		}
		want := draw(7, areaTables, "t")
		is.Equal(t, want, draw(7, areaTables, "t"))
		is.True(t, want != draw(7, areaTables, "u"))
		is.True(t, want != draw(7, areaDefaults, "t"))
		is.True(t, want != draw(8, areaTables, "t"))
	})
}

// digestOf the first n calls of the sequence of the seed with the profile p: their texts, their
// arguments, and how they run.
func digestOf(t *testing.T, seed uint64, p *Profile, n int) string {
	t.Helper()
	q, err := newSequence(seed, p)
	is.NotError(t, err)
	h := sha256.New()
	for range n {
		c := q.Next()
		fmt.Fprintf(h, "%q %#v %v %v\n", c.Src, c.Args, c.Query, c.Supported)
	}
	return fmt.Sprintf("%x", h.Sum(nil)[:8])
}

func TestNewSequence(t *testing.T) {
	t.Run("should give the same sequence twice for a seed, with the profile of the seed", func(t *testing.T) {
		for _, p := range Profiles {
			q, err := NewSequence(p.Seed(3))
			is.NotError(t, err)
			is.Equal(t, p, q.Profile())
			is.Equal(t, p.Seed(3), q.Seed())
			again, err := NewSequence(p.Seed(3))
			is.NotError(t, err)
			for range 200 {
				a, b := q.Next(), again.Next()
				is.Equal(t, fmt.Sprintf("%q %#v %v %v", a.Src, a.Args, a.Query, a.Supported), fmt.Sprintf("%q %#v %v %v", b.Src, b.Args, b.Query, b.Supported))
			}
		}
	})

	// The digests of the first 200 calls of the first three seeds of each profile. A change of the
	// generator of an area changes the digests of the profiles that have the area on, and of no
	// other: a profile that has the area off draws nothing from it, and the test below runs each
	// profile with streams that panic for the areas that it has off. So a digest that changes says
	// which profiles a change reaches, and whose least numbers in testdata/reached are to be
	// recounted, with S4QL_RECOUNT=1.
	t.Run("should give each profile the sequences that its digest pins, which change only with the areas that the profile has on", func(t *testing.T) {
		want := map[string]string{
			"mixed": "1be520de2091c146", "tables": "29ebcd8aa183db68", "where": "4bda84f3f43b104a", "changes": "e9ff49a7282d4cf7",
			"indexes": "414a9a700d02c392", "exists": "2b60aa13e8d46150", "defaults": "7691b53223e1efdf", "valuefirst": "d395629f1c472444",
			"transactions": "d131350fa1745ac7", "arguments": "8c37d49ea586cc44",
		}
		is.Equal(t, len(Profiles), len(want))
		for _, p := range Profiles {
			h := sha256.New()
			for k := range uint64(3) {
				fmt.Fprintln(h, digestOf(t, p.Seed(k), p, 200))
			}
			if got := fmt.Sprintf("%x", h.Sum(nil)[:8]); got != want[p.Name] {
				t.Errorf("the digest of the profile %v is %q, and the test pins %q", p.Name, got, want[p.Name])
			}
		}
	})

	t.Run("should draw nothing from an area that its profile has off, whose stream panics", func(t *testing.T) {
		for _, p := range Profiles {
			for k := range uint64(20) {
				q, err := NewSequence(p.Seed(k))
				is.NotError(t, err)
				for range 300 {
					c := q.Next()
					// What the driver check draws for another run of a prepared statement.
					for _, a := range c.Args {
						q.Like(a)
					}
				}
			}
		}
		defer func() {
			is.True(t, recover() != nil, "a draw from the stream of an area that is off panics")
		}()
		q, err := newSequence(1, Tables)
		is.NotError(t, err)
		q.rIndexes.IntN(2)
	})

	t.Run("should write the same statements whatever another area draws: with other numbers for the clause of slice 10 or for the value on the left", func(t *testing.T) {
		// other is a sequence of the seed with the mixed profile in which the stream of one area draws
		// other numbers, as it would after a change of the generator of that area.
		perturbed := func(seed uint64, area string) *Sequence {
			q, err := newSequence(seed, Mixed)
			is.NotError(t, err)
			r := newStepStream(seed+1_000_000, area, &q.step)
			switch area {
			case areaExists:
				q.g.clause = r
			case areaValueFirst:
				q.g.turned = r
			}
			return q
		}
		// same reports whether the statement of a call is one that the area has no part in.
		same := map[string]func(s stmt.Statement) bool{
			areaExists: func(s stmt.Statement) bool {
				switch s.(type) {
				case *stmt.CreateTable, *stmt.DropTable, *stmt.CreateIndex, *stmt.DropIndex:
					return false
				}
				return true
			},
			areaValueFirst: func(s stmt.Statement) bool {
				switch s := s.(type) {
				case *stmt.Select:
					return len(s.Where) == 0
				case *stmt.Update:
					return len(s.Where) == 0
				case *stmt.Delete:
					return len(s.Where) == 0
				}
				return true
			},
		}
		for area, same := range same {
			compared, differ := 0, 0
			for seed := range uint64(20) {
				q, err := newSequence(seed, Mixed)
				is.NotError(t, err)
				other := perturbed(seed, area)
				for range 200 {
					a, b := q.Next(), other.Next()
					if !a.Supported || !b.Supported {
						continue
					}
					if a.Src != b.Src {
						differ++
					}
					s, err := stmt.Check(a.Src)
					is.NotError(t, err)
					if !same(s) {
						continue
					}
					compared++
					is.Equal(t, fmt.Sprintf("%q %#v %v", a.Src, a.Args, a.Query), fmt.Sprintf("%q %#v %v", b.Src, b.Args, b.Query), area)
				}
			}
			is.True(t, compared > 1000, area)
			is.True(t, differ > 0, fmt.Sprint(area, ": the other numbers change the statements of the area"))
		}
	})
}
