//go:build s4ql_browser

package simulator

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"maragu.dev/s4ql/internal/format"
)

// The invariants that the checker checks, by the names that the events of a violation carry. The
// checker sees what the application and the store see, and nothing inside the storage core. Each
// write of the workload inserts a row with a key of its own, so the keys tell the transactions apart.
const (
	// invariantChain: the manifest moves its head only to a commit object that is in the store and
	// whose parent is the head before, so the log is one chain.
	invariantChain = "chain"
	// invariantAcknowledged: a commit that was acknowledged is in the log, once, when it is
	// acknowledged and at the end.
	invariantAcknowledged = "acknowledged"
	// invariantNotCommitted: a commit that failed with an error other than an unknown outcome is not
	// in the log at the end.
	invariantNotCommitted = "not_committed"
	// invariantOnce: no transaction is in the log twice at the end.
	invariantOnce = "once"
	// invariantState: a read sees a state that the log had: its position is in the log, and it sees
	// the rows of the log up to that position.
	invariantState = "state"
	// invariantFresh: a read sees every commit that was acknowledged before it started, and every
	// commit of unknown outcome that had returned before it started and that is in the log at the
	// end. The design accepts a read that misses one on an instance whose two clocks both went wrong
	// in the direction that keeps a lease alive too long; such a violation is excused.
	invariantFresh = "fresh"
)

// checker of the invariants, over the store as it is and over what the calls saw.
type checker struct {
	r *run

	mu      sync.Mutex
	commits map[string]*CommitObject
	head    string
	// acked, unknown, and failed are the values of the writes that were acknowledged, that ended
	// with an unknown outcome, and that failed otherwise, in the order in which they ended.
	acked, unknown, failed []int64
	// reads that were served, for the checks at the end.
	reads []readSeen
}

// readSeen is a read that was served: how many acknowledged and unknown writes had ended before it
// started, what it saw, and its process.
type readSeen struct {
	acked, unknown int
	keys           map[int64]bool
	p              *process
	at             time.Duration
}

func newChecker(r *run) *checker {
	return &checker{r: r, commits: map[string]*CommitObject{}, head: format.EmptyLog.String()}
}

// violate records a violation of the invariant.
func (c *checker) violate(invariant string, excused bool, p *process, format string, args ...any) {
	e := Event{Kind: "check", What: "violated", Invariant: invariant, Excused: excused, Text: fmt.Sprintf(format, args...)}
	if p != nil {
		e.Instance, e.Process = p.i.name, p.name
	}
	c.r.rec.add(e)
}

// commit object that the store took.
func (c *checker) commit(o *CommitObject) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits[o.Name] = o
}

// manifest that the store applied: check that its head extends the chain.
func (c *checker) manifest(_ context.Context, m *ManifestObject) {
	c.mu.Lock()
	before := c.head
	c.head = m.Head
	o := c.commits[m.Head]
	c.mu.Unlock()
	switch {
	case m.Head == before:
	case o == nil:
		c.violate(invariantChain, false, nil, "the manifest names the head %v, which is not in the store", short(m.Head))
	case o.Parent != before:
		c.violate(invariantChain, false, nil, "the manifest moves its head from %v to %v, whose parent is %v", short(before), short(m.Head), short(o.Parent))
	}
}

func (c *checker) headNow() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.head
}

// chain of the commit objects up to head, from the first.
func (c *checker) chain(head string) []*CommitObject {
	c.mu.Lock()
	defer c.mu.Unlock()
	var chain []*CommitObject
	empty := format.EmptyLog.String()
	for name := head; name != empty && len(chain) <= len(c.commits); {
		o := c.commits[name]
		if o == nil {
			break
		}
		chain = append(chain, o)
		name = o.Parent
	}
	slices.Reverse(chain)
	return chain
}

// count of the commit objects in chain that insert value.
func count(chain []*CommitObject, value int64) int {
	n := 0
	for _, o := range chain {
		if slices.Contains(o.Keys, value) {
			n++
		}
	}
	return n
}

// writeEnded with the value, which the call that ended in the given way wrote.
func (c *checker) writeEnded(_ context.Context, value int64, how string) {
	c.mu.Lock()
	switch how {
	case "committed":
		c.acked = append(c.acked, value)
	case "unknown":
		c.unknown = append(c.unknown, value)
	case "not_committed":
		c.failed = append(c.failed, value)
	}
	c.mu.Unlock()
	if how == "committed" {
		if n := count(c.chain(c.headNow()), value); n != 1 {
			c.violate(invariantAcknowledged, false, nil, "the write of %v is acknowledged, and the log holds it %v times", value, n)
		}
	}
}

// readStarted returns what a read that starts now must see.
func (c *checker) readStarted() readSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return readSeen{acked: len(c.acked), unknown: len(c.unknown), at: c.r.world.Monotonic()}
}

// readEnded on p, which started as before says and saw the keys at the position.
func (c *checker) readEnded(_ context.Context, p *process, before readSeen, keys []int64, position format.Name) {
	chain := c.chain(c.headNow())
	at := -1
	if position == format.EmptyLog {
		at = 0
	}
	for i, o := range chain {
		if o.Name == position.String() {
			at = i + 1
		}
	}
	seen := map[int64]bool{}
	for _, k := range keys {
		seen[k] = true
	}
	if at < 0 {
		c.violate(invariantState, false, p, "a read saw the position %v, which is not in the log", short(position.String()))
	} else {
		want := map[int64]bool{}
		for _, o := range chain[:at] {
			for _, k := range o.Keys {
				want[k] = true
			}
		}
		if !sameKeys(seen, want) {
			c.violate(invariantState, false, p, "a read at %v saw %v rows, and the log has %v rows there", short(position.String()), len(seen), len(want))
		}
	}

	c.mu.Lock()
	acked := slices.Clone(c.acked[:before.acked])
	c.mu.Unlock()
	for _, v := range acked {
		if !seen[v] {
			c.violate(invariantFresh, p.bothClocks(), p, "a read missed the write of %v, which was acknowledged before the read started", v)
			break
		}
	}
	before.keys, before.p = seen, p
	c.mu.Lock()
	c.reads = append(c.reads, before)
	c.mu.Unlock()
}

func sameKeys(a, b map[int64]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// final checks at the end of the run, against the log as it is then.
func (c *checker) final(_ context.Context) {
	chain := c.chain(c.headNow())
	c.mu.Lock()
	acked, unknown, failed, reads := slices.Clone(c.acked), slices.Clone(c.unknown), slices.Clone(c.failed), slices.Clone(c.reads)
	c.mu.Unlock()

	inLog := map[int64]int{}
	for _, o := range chain {
		for _, k := range o.Keys {
			inLog[k]++
		}
	}
	for _, v := range acked {
		if inLog[v] == 0 {
			c.violate(invariantAcknowledged, false, nil, "the write of %v was acknowledged, and the log does not hold it at the end", v)
		}
	}
	for _, v := range failed {
		if inLog[v] > 0 {
			c.violate(invariantNotCommitted, false, nil, "the write of %v failed, and the log holds it at the end", v)
		}
	}
	for _, v := range slices.Sorted(maps.Keys(inLog)) {
		if inLog[v] > 1 {
			c.violate(invariantOnce, false, nil, "the log holds the write of %v %v times at the end", v, inLog[v])
		}
	}
	for _, read := range reads {
		for _, v := range unknown[:read.unknown] {
			if inLog[v] > 0 && !read.keys[v] {
				c.violate(invariantFresh, read.p.bothClocks(), read.p, "a read at %v missed the write of %v, which returned with an unknown outcome before the read started, and which the log holds at the end", millis(read.at), v)
				break
			}
		}
	}
}

// short form of the name of a commit object.
func short(name string) string {
	return name[:min(len(name), 8)]
}
