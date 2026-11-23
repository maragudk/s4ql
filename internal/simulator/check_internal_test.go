//go:build s4ql_browser

package simulator

import (
	"crypto/sha256"
	"fmt"
	"slices"
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
	// made tells the checker that calls of the application made the writes of the workload in o.
	made := func(c *checker, o *CommitObject) {
		for _, w := range writesOf(o) {
			if w.commit == "" {
				c.made(w)
			}
		}
	}
	// log of commits 1, 2, and 3, where commit i inserts the key i, each made by the application.
	log := func(c *checker) {
		parent := empty
		for i := 1; i <= 3; i++ {
			o := &CommitObject{Name: name(i), Parent: parent, Keys: []int64{int64(i)}}
			made(c, o)
			c.commit(o)
			c.manifest(t.Context(), &ManifestObject{Head: name(i)})
			parent = name(i)
		}
	}
	// seeded checker, which starts on a stored log of the commits 1, 2, and 3.
	seeded := func(c *checker) {
		parent := empty
		var objects []*CommitObject
		for i := 1; i <= 3; i++ {
			o := &CommitObject{Name: name(i), Parent: parent, Keys: []int64{int64(i)}}
			c.commit(o)
			objects = append(objects, o)
			parent = name(i)
		}
		c.seed(objects, name(3))
	}
	position := func(i int) format.Name {
		n, _ := format.ParseName(name(i))
		return n
	}
	// next commit object i on the head, which inserts the keys and deletes the deletes that calls of
	// the application made, and the manifest that moves the head to it.
	next := func(c *checker, i int, keys, deletes []int64) {
		o := &CommitObject{Name: name(i), Parent: c.headNow(), Keys: keys, Deletes: deletes}
		made(c, o)
		c.commit(o)
		c.manifest(t.Context(), &ManifestObject{Head: name(i)})
	}
	// logWithDelete is the log of commits 1, 2, and 3, and commit 4, which deletes the key 1.
	logWithDelete := func(c *checker) {
		log(c)
		next(c, 4, nil, []int64{1})
	}

	t.Run("should find nothing wrong with deletes and reads that agree", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			logWithDelete(c)
			c.writeEnded(t.Context(), txn{key: 1}, "committed")
			c.writeEnded(t.Context(), txn{key: 2}, "committed")
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "committed")
			// A delete of unknown outcome that the log does not hold leaves both answers legal, and so
			// does a delete that has not ended. One that found its instance down is not in the log.
			c.writeEnded(t.Context(), txn{key: 3, delete: true}, "unknown")
			c.writeEnded(t.Context(), txn{key: 2, delete: true}, "down")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{}, position(4))
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(4))
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{3}, position(4))
			next(c, 5, []int64{5}, nil)
			c.writeEnded(t.Context(), txn{key: 5}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{}, position(5))
			c.final(t.Context())
		})
		is.Equal(t, 0, len(events))
	})

	t.Run("should find nothing wrong with a stored log that deletes", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			o1 := &CommitObject{Name: name(1), Parent: empty, Keys: []int64{1}}
			o2 := &CommitObject{Name: name(2), Parent: name(1), Keys: []int64{2}}
			o3 := &CommitObject{Name: name(3), Parent: name(2), Deletes: []int64{1}}
			for _, o := range []*CommitObject{o1, o2, o3} {
				c.commit(o)
			}
			c.seed([]*CommitObject{o1, o2, o3}, name(3))
			is.EqualSlice(t, []int64{2}, c.known)
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{}, position(3))
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(3))
			c.final(t.Context())
		})
		is.Equal(t, 0, len(events))
	})

	t.Run("should find nothing wrong with a log, acknowledged writes, and reads that agree", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 1}, "committed")
			c.writeEnded(t.Context(), txn{key: 2}, "committed")
			// A write lost at a crash may be missing from the log, and one that found its instance
			// down is.
			c.writeEnded(t.Context(), txn{key: 8}, "lost")
			c.writeEnded(t.Context(), txn{key: 9}, "down")
			// A read gives the row of its key if its state has it, and no row otherwise, also for a
			// key that the log never holds.
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(2))
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(2))
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
			c.readEnded(t.Context(), p, c.readStarted(), 9, []int64{}, position(3))
			c.final(t.Context())
		})
		is.Equal(t, 0, len(events))
	})

	t.Run("should find nothing wrong with writes and selects of the box among those of the app", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 1}, "committed")
			// A write of the box that commits, one of unknown outcome that the log holds, and one that
			// failed, which it does not.
			next(c, 4, nil, nil)
			c.writeEnded(t.Context(), txn{commit: name(4)}, "committed")
			next(c, 5, nil, nil)
			c.writeEnded(t.Context(), txn{commit: name(5)}, "unknown")
			c.writeEnded(t.Context(), txn{commit: name(9)}, "not_committed")
			c.boxRead(t.Context(), p, c.readStarted(), position(5))
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(5))
			next(c, 6, []int64{6}, nil)
			c.writeEnded(t.Context(), txn{key: 6}, "committed")
			c.boxRead(t.Context(), p, c.readStarted(), position(6))
			c.final(t.Context())
		})
		is.Equal(t, 0, len(events))
	})

	t.Run("should find nothing wrong with a stored log that holds commit objects of the box", func(t *testing.T) {
		events := withChecker(t, func(c *checker, p *process) {
			o1 := &CommitObject{Name: name(1), Parent: empty, Keys: []int64{1}}
			o2 := &CommitObject{Name: name(2), Parent: name(1)}
			o3 := &CommitObject{Name: name(3), Parent: name(2), Deletes: []int64{1}}
			for _, o := range []*CommitObject{o1, o2, o3} {
				c.commit(o)
			}
			c.seed([]*CommitObject{o1, o2, o3}, name(3))
			c.boxRead(t.Context(), p, c.readStarted(), position(3))
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
			c.writeEnded(t.Context(), txn{key: 7}, "committed")
		}},
		{name: "a write that failed and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "not_committed")
			c.final(t.Context())
		}},
		{name: "a write of the client that found its instance down and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "down")
			c.final(t.Context())
		}},
		{name: "a read at a state without a write lost at a crash that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "lost")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(2))
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
			c.readEnded(t.Context(), p, c.readStarted(), 1, nil, position(8))
		}},
		{name: "a read that saw a row that the log does not have at its position", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{3}, position(2))
		}},
		{name: "a read that saw its row twice", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1, 1}, position(2))
		}},
		{name: "a read that missed its row, which the log has at its position", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(2))
		}},
		{name: "a read that gave the row of another key", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{1}, position(2))
		}},
		{name: "a read that missed the acknowledged write of its key", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
		}},
		{name: "a read that missed the acknowledged write of its key on a process whose clocks both went wrong", invariant: invariantFresh, excused: true, f: func(c *checker, p *process) {
			log(c)
			p.monotonicStoppedEver, p.wallBackEver = true, true
			c.writeEnded(t.Context(), txn{key: 3}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
		}},
		{name: "a read at a state without an acknowledged write of another key", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(2))
		}},
		{name: "a read at the empty log after a start on a stored log", invariant: invariantFresh, f: func(c *checker, p *process) {
			seeded(c)
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{}, format.EmptyLog)
		}},
		{name: "a read that missed its row of a stored log at the head", invariant: invariantState, f: func(c *checker, p *process) {
			seeded(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(3))
		}},
		{name: "a read at the empty log after a start on a stored log, by the slow check", invariant: invariantFresh, f: func(c *checker, p *process) {
			seeded(c)
			c.slow()
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{}, format.EmptyLog)
		}},
		{name: "a read that missed its row of a stored log at the head, by the slow check", invariant: invariantState, f: func(c *checker, p *process) {
			seeded(c)
			c.slow()
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(3))
		}},
		{name: "a read that missed a write of its key of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{}, position(2))
			c.final(t.Context())
		}},
		{name: "a read at a state without a write of another key of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(2))
			c.final(t.Context())
		}},
		{name: "an acknowledged delete that the log does not hold", invariant: invariantAcknowledged, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "committed")
		}},
		{name: "an acknowledged delete that the log does not hold at the end, by the slow check", invariant: invariantAcknowledged, f: func(c *checker, p *process) {
			log(c)
			c.slow()
			c.writeEnded(t.Context(), txn{key: 2, delete: true}, "committed")
		}},
		{name: "a delete that failed and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "not_committed")
			c.final(t.Context())
		}},
		{name: "a delete that the log holds twice", invariant: invariantOnce, f: func(c *checker, p *process) {
			logWithDelete(c)
			next(c, 5, nil, []int64{1})
			c.final(t.Context())
		}},
		{name: "a delete of a row that the log does not hold", invariant: invariantOnce, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, []int64{7})
			c.final(t.Context())
		}},
		{name: "a delete before the insert of its row", invariant: invariantOnce, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, []int64{5})
			next(c, 5, []int64{5}, nil)
			c.final(t.Context())
		}},
		{name: "a read that gives a row that the log has deleted at its position", invariant: invariantState, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(4))
		}},
		{name: "a read that gives a row that the log has deleted at its position, by the slow check", invariant: invariantState, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.slow()
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(4))
		}},
		{name: "a read at a state without an acknowledged delete of its key", invariant: invariantFresh, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(3))
		}},
		{name: "a read at a state without an acknowledged delete of its key on a process whose clocks both went wrong", invariant: invariantFresh, excused: true, f: func(c *checker, p *process) {
			logWithDelete(c)
			p.monotonicStoppedEver, p.wallBackEver = true, true
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(3))
		}},
		{name: "a read at a state without an acknowledged delete of another key", invariant: invariantFresh, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{2}, position(3))
		}},
		{name: "a read at a state without a delete of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "unknown")
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(3))
			c.final(t.Context())
		}},
		{name: "a read that misses a row that the log never deleted", invariant: invariantState, f: func(c *checker, p *process) {
			logWithDelete(c)
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(4))
		}},
		{name: "a read that misses a row of a stored log that deletes another", invariant: invariantState, f: func(c *checker, p *process) {
			o1 := &CommitObject{Name: name(1), Parent: empty, Keys: []int64{1}}
			o2 := &CommitObject{Name: name(2), Parent: name(1), Keys: []int64{2}}
			o3 := &CommitObject{Name: name(3), Parent: name(2), Deletes: []int64{1}}
			for _, o := range []*CommitObject{o1, o2, o3} {
				c.commit(o)
			}
			c.seed([]*CommitObject{o1, o2, o3}, name(3))
			c.readEnded(t.Context(), p, c.readStarted(), 2, []int64{}, position(3))
		}},
		{name: "an insert into the table of the workload that the app did not make", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Keys: []int64{7}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.final(t.Context())
		}},
		{name: "a delete from the table of the workload that the app did not make", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Deletes: []int64{2}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.final(t.Context())
		}},
		{name: "an acknowledged write of the box that the log does not hold", invariant: invariantAcknowledged, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{commit: name(9)}, "committed")
		}},
		{name: "an acknowledged write of the box that the log does not hold, by the slow check", invariant: invariantAcknowledged, f: func(c *checker, p *process) {
			log(c)
			c.slow()
			c.writeEnded(t.Context(), txn{commit: name(9)}, "committed")
		}},
		{name: "a write of the box that failed and that the log holds", invariant: invariantNotCommitted, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, nil)
			c.writeEnded(t.Context(), txn{commit: name(4)}, "not_committed")
			c.final(t.Context())
		}},
		{name: "a read of the app at a state without an acknowledged write of the box", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, nil)
			c.writeEnded(t.Context(), txn{commit: name(4)}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{3}, position(3))
		}},
		{name: "a read of the app at a state without an acknowledged write of the box, by the slow check", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, nil)
			c.slow()
			c.writeEnded(t.Context(), txn{commit: name(4)}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{3}, position(3))
		}},
		{name: "a select of the box at a state without an acknowledged write of the app", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 3}, "committed")
			c.boxRead(t.Context(), p, c.readStarted(), position(2))
		}},
		{name: "a select of the box at a state without an acknowledged write of the box, on a process whose clocks both went wrong", invariant: invariantFresh, excused: true, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, nil)
			p.monotonicStoppedEver, p.wallStoppedEver = true, true
			c.writeEnded(t.Context(), txn{commit: name(4)}, "committed")
			c.boxRead(t.Context(), p, c.readStarted(), position(3))
		}},
		{name: "a select of the box at a position that is not in the log", invariant: invariantState, f: func(c *checker, p *process) {
			log(c)
			c.boxRead(t.Context(), p, c.readStarted(), position(8))
		}},
		{name: "a select of the box at a state without a write of the box of unknown outcome that the log holds at the end", invariant: invariantFresh, f: func(c *checker, p *process) {
			log(c)
			next(c, 4, nil, nil)
			c.writeEnded(t.Context(), txn{commit: name(4)}, "unknown")
			c.boxRead(t.Context(), p, c.readStarted(), position(3))
			c.final(t.Context())
		}},
		{name: "a read before the stored delete of its key", invariant: invariantFresh, f: func(c *checker, p *process) {
			o1 := &CommitObject{Name: name(1), Parent: empty, Keys: []int64{1}}
			o2 := &CommitObject{Name: name(2), Parent: name(1), Deletes: []int64{1}}
			c.commit(o1)
			c.commit(o2)
			c.seed([]*CommitObject{o1, o2}, name(2))
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(1))
		}},
	}
	t.Run("should stay fast after a violation of a read", func(t *testing.T) {
		var fast bool
		events := withChecker(t, func(c *checker, p *process) {
			log(c)
			c.writeEnded(t.Context(), txn{key: 1}, "committed")
			c.writeEnded(t.Context(), txn{key: 2}, "unknown")
			c.commit(&CommitObject{Name: name(4), Parent: name(3), Keys: []int64{4}})
			c.manifest(t.Context(), &ManifestObject{Head: name(4)})
			c.writeEnded(t.Context(), txn{key: 4}, "committed")
			c.readEnded(t.Context(), p, c.readStarted(), 4, []int64{4}, position(4))
			// A read at a state without the write of 4 is a violation, and the next read is still
			// checked fast.
			c.readEnded(t.Context(), p, c.readStarted(), 3, []int64{3}, position(3))
			c.readEnded(t.Context(), p, c.readStarted(), 1, []int64{1}, position(4))
			fast = c.fast && c.reads[0].fast && c.reads[2].fast
			c.final(t.Context())
		})
		is.Equal(t, 1, len(events))
		is.Equal(t, invariantFresh, events[0].Invariant)
		is.True(t, fast, "fast after a violation of a read")
	})

	// Every read of every key from 0 to 4, giving every set of the keys 1 to 4, at every position of
	// the log, with the inserts 1 and 3 acknowledged and the insert 2 of unknown outcome, is checked
	// once by a checker that is fast and once by one that is slow from the start, and so is a select of
	// the box at every position. Both must find the same violations. The same holds for the log with a
	// fourth commit object, which deletes 1, with that delete acknowledged, and again with it of unknown
	// outcome, and a fifth, which inserts 4, acknowledged. And each of these again with a commit object
	// of the box between the second and the third, acknowledged, of unknown outcome, and not ended, as
	// a write whose call is on its way.
	t.Run("should find the same violations fast and slow", func(t *testing.T) {
		const box = -1 // the key of a select of the box
		check := func(t *testing.T, slow bool, deletes, boxed string, key int64, keys []int64, at int) ([]Event, bool) {
			t.Helper()
			var fast bool
			events := withChecker(t, func(c *checker, p *process) {
				var names []string
				commit := func(i int, keys, deletes []int64) {
					next(c, i, keys, deletes)
					names = append(names, name(i))
				}
				commit(1, []int64{1}, nil)
				commit(2, []int64{2}, nil)
				if boxed != "" {
					commit(10, nil, nil)
				}
				commit(3, []int64{3}, nil)
				if deletes != "" {
					commit(4, nil, []int64{1})
					commit(5, []int64{4}, nil)
				}
				if slow {
					c.slow()
				}
				c.writeEnded(t.Context(), txn{key: 1}, "committed")
				c.writeEnded(t.Context(), txn{key: 2}, "unknown")
				if boxed != "" && boxed != "on its way" {
					c.writeEnded(t.Context(), txn{commit: name(10)}, boxed)
				}
				c.writeEnded(t.Context(), txn{key: 3}, "committed")
				if deletes != "" {
					c.writeEnded(t.Context(), txn{key: 1, delete: true}, deletes)
					c.writeEnded(t.Context(), txn{key: 4}, "committed")
				}
				pos := format.EmptyLog
				if at > 0 {
					pos, _ = format.ParseName(names[at-1])
				}
				if key == box {
					c.boxRead(t.Context(), p, c.readStarted(), pos)
				} else {
					c.readEnded(t.Context(), p, c.readStarted(), key, keys, pos)
				}
				is.True(t, !slow || !c.fast, "the slow checker stays slow")
				fast = c.reads[0].fast
				c.final(t.Context())
			})
			return events, fast
		}
		for _, boxed := range []string{"", "committed", "unknown", "on its way"} {
			for _, deletes := range []string{"", "committed", "unknown"} {
				positions := 4
				if deletes != "" {
					positions = 6
				}
				if boxed != "" {
					positions++
				}
				reads, violations, checkedFast := 0, 0, 0
				for at := range positions {
					for key := int64(box); key < 5; key++ {
						for set := range 1 << 4 {
							if key == box && set > 0 {
								break
							}
							var keys []int64
							for k := range int64(4) {
								if set&(1<<k) != 0 {
									keys = append(keys, k+1)
								}
							}
							fast, wasFast := check(t, false, deletes, boxed, key, keys, at)
							slow, _ := check(t, true, deletes, boxed, key, keys, at)
							is.Equal(t, len(slow), len(fast), fmt.Sprint("the violations of the keys ", keys, " of ", key, " at ", at, " with the delete ", deletes, " and the box ", boxed))
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
				is.Equal(t, reads-violations, checkedFast, "the reads checked fast with the delete "+deletes+" and the box "+boxed)
			}
		}
	})

	// What the application knows decides which keys the calls pick.
	t.Run("should pick a delete only among known keys that no delete has picked, and leave one", func(t *testing.T) {
		withChecker(t, func(c *checker, p *process) {
			c.mu.Lock()
			c.learn(1)
			c.mu.Unlock()
			_, ok := c.pickDelete(0)
			is.True(t, !ok, "no delete of the only known key")

			c.mu.Lock()
			c.learn(2)
			c.learn(3)
			c.learn(4)
			c.mu.Unlock()
			k, ok := c.pickDelete(1)
			is.True(t, ok, "a delete")
			is.Equal(t, int64(2), k)
			is.EqualSlice(t, []int64{1, 3, 4}, c.known)
			// While the delete of 2 runs, no read selects it.
			for u := range uint64(64) {
				key, row, ok := c.pickRead(u)
				is.True(t, ok && row && key != 2, "a read of a known key")
			}

			// A delete of unknown outcome leaves its key in limbo, one that failed makes it known
			// again, and neither key is picked by a delete again.
			c.writeEnded(t.Context(), txn{key: 2, delete: true}, "unknown")
			k, _ = c.pickDelete(0)
			is.Equal(t, int64(1), k)
			c.writeEnded(t.Context(), txn{key: 1, delete: true}, "not_committed")
			is.EqualSlice(t, []int64{3, 4, 1}, c.known)
			for u := range uint64(64) {
				if k, ok := c.pickDelete(u); ok {
					is.True(t, k == 3 || k == 4, "a delete of a key that no delete has picked")
					break
				}
			}
		})
	})

	t.Run("should pick one read in eight among the recent acknowledged deletes", func(t *testing.T) {
		withChecker(t, func(c *checker, p *process) {
			c.mu.Lock()
			for k := range int64(20) {
				c.learn(k + 1)
			}
			c.mu.Unlock()
			for range 12 {
				k, _ := c.pickDelete(0)
				c.writeEnded(t.Context(), txn{key: k, delete: true}, "committed")
			}
			is.Equal(t, recentDeletes, len(c.gone))
			gone := 0
			for u := range uint64(800) {
				key, row, ok := c.pickRead(u)
				is.True(t, ok, "a read")
				is.Equal(t, !row, slices.Contains(c.gone, key), "a read expects no row of a key that is gone")
				is.Equal(t, !row, !slices.Contains(c.known, key))
				if !row {
					gone++
				}
			}
			is.Equal(t, 100, gone)
		})
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
