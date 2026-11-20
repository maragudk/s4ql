//go:build s4ql_browser

package simulator

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"testing/synctest"

	"maragu.dev/is"

	"maragu.dev/s4ql/internal/format"
	"maragu.dev/s4ql/internal/sim"
)

// The checker is tested here on what it is told directly, because the storage core cannot be made
// to break an invariant from outside the package, and planted bugs are not part of the simulator.
func TestChecker(t *testing.T) {
	// withChecker runs f with a checker of a run of its own, and returns the violations it recorded.
	withChecker := func(t *testing.T, f func(c *checker, p *process)) []Event {
		t.Helper()
		var events []Event
		synctest.Test(t, func(t *testing.T) {
			world := sim.New(1, sim.Options{})
			r := &run{world: world, rec: newRecorder(world, 1<<62, false)}
			r.check = newChecker(r)
			p := &process{i: &instance{name: "a"}, name: "a"}
			f(r.check, p)
			events = r.rec.events
		})
		return events
	}
	name := func(i int) string {
		sum := sha256.Sum256(fmt.Append(nil, i))
		n, _ := format.NameFromBytes(sum[:])
		return n.String()
	}
	empty := format.EmptyLog.String()
	// log of commits 1, 2, and 3, where commit i inserts the key i.
	log := func(c *checker) {
		parent := empty
		for i := 1; i <= 3; i++ {
			c.commit(&CommitObject{Name: name(i), Parent: parent, Keys: []int64{int64(i)}})
			c.manifest(t.Context(), &ManifestObject{Head: name(i)})
			parent = name(i)
		}
	}
	position := func(i int) format.Name {
		n, _ := format.ParseName(name(i))
		return n
	}

	t.Run("should find nothing wrong with a log, acknowledged writes, and reads that agree", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 1, "committed")
			c.writeEnded(t.Context(), 2, "committed")
			// A write lost at a crash may be missing from the log, and one that found its instance
			// down is.
			c.writeEnded(t.Context(), 8, "lost")
			c.writeEnded(t.Context(), 9, "down")
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 2}, position(2))
			// A read above a bound gives only the rows above it, and its state holds the acknowledged
			// writes at or below it.
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{2}, position(2))
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(3))
			c.final(t.Context())
		})
		is.Equal(t, 0, len(events))
	})

	tests := []struct {
		name      string
		f         func(c *checker, p *process)
		invariant string
		excused   bool
	}{
		{name: "a head that is not in the store", invariant: invariantChain, f: func(c *checker, p *process) {
			c.manifest(t.Context(), &ManifestObject{Head: name(9)})
		}},
		{name: "a head whose parent is not the head before", invariant: invariantChain, f: func(c *checker, p *process) {
			log(c)
			c.commit(&CommitObject{Name: name(4), Parent: name(1)})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
		}},
		{name: "an acknowledged write that the log does not hold", invariant: invariantAcknowledged, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 7, "committed")
		}},
		{name: "a write that failed and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "not_committed")
			c.final(t.Context())
		}},
		{name: "a write of the client that found its instance down and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "down")
			c.final(t.Context())
		}},
		{name: "a read that missed a write lost at a crash that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "lost")
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 2}, position(2))
			c.final(t.Context())
		}},
		{name: "a write that the log holds twice", invariant: invariantOnce, f: func(c *checker, p *process) {
			log(c)
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Keys: []int64{2}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.final(t.Context())
		}},
		{name: "a read at a position that is not in the log", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 0, nil, position(8))
		}},
		{name: "a read that saw rows that the log does not have at its position", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 3}, position(2))
		}},
		{name: "a read that saw a row twice", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 1}, position(2))
		}},
		{name: "a read that saw a row twice among all the rows", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 1, 2}, position(2))
		}},
		{name: "a read that missed a row above its bound", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{3}, position(3))
		}},
		{name: "a read that saw a row at its bound", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2, 3}, position(3))
		}},
		{name: "a read that saw a row below its bound", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{1, 3}, position(3))
		}},
		{name: "a read that missed an acknowledged write above its bound", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{2}, position(2))
		}},
		{name: "a read at a state without an acknowledged write at or below its bound", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
		}},
		{name: "a read at a state without a write of unknown outcome at or below its bound that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
			c.final(t.Context())
		}},
		{name: "a read that missed a write of unknown outcome above its bound that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(2))
			c.final(t.Context())
		}},
		{name: "a read that missed an acknowledged write", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 2}, position(2))
		}},
		{name: "a read that missed an acknowledged write on a process whose clocks both went wrong", invariant: invariantFresh, excused: true, f: func(c *checker, p *process) {
			log(c)
			p.monotonicStoppedEver, p.wallBackEver = true, true
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 2}, position(2))
		}},
		{name: "a read that missed a write of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 0, []int64{1, 2}, position(2))
			c.final(t.Context())
		}},
	}
	t.Run("should not count a write at or below the bound that the state holds as missed, and stay fast after a violation of a read", func(t *testing.T) {
		var fast bool
		events := withChecker(t, func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 1, "committed")
			c.writeEnded(t.Context(), 2, "unknown")
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Keys: []int64{4}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.writeEnded(t.Context(), 4, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{3, 4}, position(4))
			// A read that missed the write of 4 is a violation, and the next read is still checked fast.
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{3}, position(3))
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{3, 4}, position(4))
			fast = c.fast && c.reads[0].fast && c.reads[2].fast
			c.final(t.Context())
		})
		is.Equal(t, 1, len(events))
		is.Equal(t, invariantFresh, events[0].Invariant)
		is.True(t, fast, "fast after a violation of a read")
	})

	// Every read of the keys 1 to 4 at every position of the log, above every bound from 0 to 4, with
	// the writes 1 and 3 acknowledged and the write 2 of unknown outcome, is checked once by a checker
	// that is fast and once by one that is slow from the start. Both must find the same violations.
	t.Run("should find the same violations fast and slow", func(t *testing.T) {
		check := func(t *testing.T, slow bool, bound int64, keys []int64, at int) ([]Event, bool) {
			t.Helper()
			var fast bool
			events := withChecker(t, func(c *checker, p *process) {
				log(c)
				if slow {
					c.slow()
				}
				c.writeEnded(t.Context(), 1, "committed")
				c.writeEnded(t.Context(), 2, "unknown")
				c.writeEnded(t.Context(), 3, "committed")
				pos := format.EmptyLog
				if at > 0 {
					pos = position(at)
				}
				c.readEnded(t.Context(), p, c.readStarted(), bound, keys, pos)
				is.True(t, !slow || !c.fast, "the slow checker stays slow")
				fast = c.reads[0].fast
				c.final(t.Context())
			})
			return events, fast
		}
		reads, violations, checkedFast := 0, 0, 0
		for at := range 4 {
			for bound := range int64(5) {
				for set := range 1 << 4 {
					var keys []int64
					for k := range int64(4) {
						if set&(1<<k) != 0 {
							keys = append(keys, k+1)
						}
					}
					fast, wasFast := check(t, false, bound, keys, at)
					slow, _ := check(t, true, bound, keys, at)
					is.Equal(t, len(slow), len(fast), fmt.Sprint("the violations of the keys ", keys, " above ", bound, " at ", at))
					for i := range min(len(fast), len(slow)) {
						is.Equal(t, slow[i].Invariant, fast[i].Invariant)
						is.Equal(t, slow[i].Text, fast[i].Text)
					}
					reads++
					violations += min(1, len(slow))
					if wasFast {
						checkedFast++
						is.Equal(t, 0, len(fast), "a read that the fast check passes has no violation")
					}
				}
			}
		}
		// Most of the reads are violations, and the fast check passes every one that is not.
		is.True(t, violations > 0 && violations < reads, fmt.Sprint(violations, " of ", reads))
		is.Equal(t, reads-violations, checkedFast)
	})

	for _, test := range tests {
		t.Run("should find "+test.name, func(t *testing.T) {
			events := withChecker(t, test.f)
			is.True(t, len(events) > 0, "a violation")
			is.Equal(t, "check", events[0].Kind)
			is.Equal(t, test.invariant, events[0].Invariant)
			is.Equal(t, test.excused, events[0].Excused)
		})
	}
}
