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
			r := &run{world: world, rec: &recorder{world: world, end: 1 << 62}}
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
			c.readEnded(t.Context(), p, c.readStarted(), []int64{1, 2}, position(2))
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
		{name: "a write that the log holds twice", invariant: invariantOnce, f: func(c *checker, p *process) {
			log(c)
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Keys: []int64{2}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.final(t.Context())
		}},
		{name: "a read at a position that is not in the log", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), nil, position(8))
		}},
		{name: "a read that saw rows that the log does not have at its position", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), []int64{1, 3}, position(2))
		}},
		{name: "a read that missed an acknowledged write", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), []int64{1, 2}, position(2))
		}},
		{name: "a read that missed an acknowledged write on a process whose clocks both went wrong", invariant: invariantFresh, excused: true, f: func(c *checker, p *process) {
			log(c)
			p.monotonicStoppedEver, p.wallBackEver = true, true
			c.writeEnded(t.Context(), 3, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), []int64{1, 2}, position(2))
		}},
		{name: "a read that missed a write of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), 3, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), []int64{1, 2}, position(2))
			c.final(t.Context())
		}},
	}
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
