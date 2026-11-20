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
// A read selects the row of one key, and its position says what else its state holds.
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
	// invariantState: a read sees a state that the log had: its position is in the log, and it gives
	// the row of its key if the log up to that position has it, once, and no row otherwise.
	invariantState = "state"
	// invariantFresh: a read sees every commit that was acknowledged before it started, and every
	// commit of unknown outcome that had returned before it started and that is in the log at the
	// end: it gives the row of such a commit if it selects its key, and its position holds the key
	// of every other. The design accepts a read that misses one on an instance whose two clocks both went wrong in the direction that keeps a lease alive too long;
	// such a violation is excused.
	invariantFresh = "fresh"
)

// checker of the invariants, over the store as it is and over what the calls saw.
//
// It checks a read in a time that does not grow with the log, as long as the log is one chain: it
// keeps the log as a chain that grows when the head moves on to a child of the head, with where each
// key was inserted. A read that the fast check does not pass is checked again against the chain from
// the head, which finds its violations. Once the log is not one chain, an acknowledged write is not
// in it, or a key is inserted twice, the checker is no longer fast, and checks each read and write
// against the chain from the head, as it is then.
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
	// logAtEnd is the number of commit objects in the log at the end of the run.
	logAtEnd int

	// fast is set while the fields below describe the log, from the first commit object to head.
	fast bool
	log  []*CommitObject
	// position of each commit object of the log, from 1.
	position map[string]int
	// inserted holds, for each key, the index in log of the commit object that inserts it.
	inserted map[int64]int
	// ackedAt holds, for each acknowledged write in turn, the latest index in log of the commit
	// objects of it and of the writes acknowledged before it.
	ackedAt []int
}

// readSeen is a read that was served: how many acknowledged and unknown writes had ended before it
// started, its process, and the key that it selected. A read that the checker checked fast keeps
// the number of commit objects of the log up to its position, and others the keys that they saw and
// their position.
type readSeen struct {
	acked, unknown int
	p              *process
	at             time.Duration
	key            int64
	fast           bool
	prefix         int
	keys           map[int64]bool
	position       format.Name
}

func newChecker(r *run) *checker {
	return &checker{
		r:        r,
		commits:  map[string]*CommitObject{},
		head:     format.EmptyLog.String(),
		fast:     true,
		position: map[string]int{},
		inserted: map[int64]int{},
	}
}

// violate records a violation of the invariant.
func (c *checker) violate(invariant string, excused bool, p *process, format string, args ...any) {
	e := Event{Kind: "check", What: "violated", Invariant: invariant, Excused: excused, Text: fmt.Sprintf(format, args...)}
	if p != nil {
		e.Instance, e.Process = p.i.name, p.name
	}
	c.r.rec.add(e)
}

// slow makes the checker check against the chain from the head from now on. The reads that it
// checked fast get the keys that they saw, which are the row of their key in the log at their
// position, if it has it.
func (c *checker) slow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fast {
		return
	}
	c.fast = false
	for i, read := range c.reads {
		if read.fast {
			c.reads[i].keys = map[int64]bool{}
			if holds(c.log[:read.prefix], read.key) {
				c.reads[i].keys[read.key] = true
			}
			c.reads[i].position = format.EmptyLog
			if read.prefix > 0 {
				c.reads[i].position, _ = format.ParseName(c.log[read.prefix-1].Name)
			}
			c.reads[i].fast = false
		}
	}
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
	if c.fast && m.Head != before && o != nil && o.Parent == before {
		c.extend(o)
	}
	c.mu.Unlock()
	switch {
	case m.Head == before:
	case o == nil:
		c.slow()
		c.violate(invariantChain, false, nil, "the manifest names the head %v, which is not in the store", short(m.Head))
	case o.Parent != before:
		c.slow()
		c.violate(invariantChain, false, nil, "the manifest moves its head from %v to %v, whose parent is %v", short(before), short(m.Head), short(o.Parent))
	}
}

// extend the log by the commit object o, the new head. It requires the mutex, and that the checker
// is fast. A key that the log inserts twice makes it slow.
func (c *checker) extend(o *CommitObject) {
	at := len(c.log)
	for _, k := range o.Keys {
		if _, ok := c.inserted[k]; ok {
			c.fast = false
		}
		c.inserted[k] = at
	}
	c.log = append(c.log, o)
	c.position[o.Name] = at + 1
	if !c.fast {
		// The reads that were checked fast keep what the log had at their positions.
		c.fast = true
		c.mu.Unlock()
		c.slow()
		c.mu.Lock()
	}
}

// live reports whether the key k is a row after the first at commit objects of the log. It requires
// the mutex, and that the checker is fast.
func (c *checker) live(k int64, at int) bool {
	i, ok := c.inserted[k]
	return ok && i < at
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

// stateOf gives the rows of the state at the position: the keys that the commit object of the
// position and its ancestors in the store insert. A read selects one key, but its position says
// whether its state holds the other writes, so a read at a state without an acknowledged write is
// stale whatever key it selects. The state of a position does not change, because a commit object
// names its parent. stateOf walks the ancestors once, on the first call of the function that it gives.
func (c *checker) stateOf(position format.Name) func() map[int64]bool {
	var rows map[int64]bool
	return func() map[int64]bool {
		if rows != nil {
			return rows
		}
		rows = map[int64]bool{}
		c.mu.Lock()
		defer c.mu.Unlock()
		empty := format.EmptyLog.String()
		for name, n := position.String(), 0; name != empty && n <= len(c.commits); n++ {
			o := c.commits[name]
			if o == nil {
				break
			}
			for _, k := range o.Keys {
				rows[k] = true
			}
			name = o.Parent
		}
		return rows
	}
}

// holds reports whether a commit object of chain inserts the key.
func holds(chain []*CommitObject, key int64) bool {
	return slices.ContainsFunc(chain, func(o *CommitObject) bool { return slices.Contains(o.Keys, key) })
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

// writeEnded with the value, which the call that ended in the given way wrote. A write of the client
// that was lost at a crash of its process has an outcome that the client does not know, and one that
// found its instance down was never made.
func (c *checker) writeEnded(_ context.Context, value int64, how string) {
	c.mu.Lock()
	switch how {
	case "committed":
		c.acked = append(c.acked, value)
	case "unknown", "lost":
		c.unknown = append(c.unknown, value)
	case "not_committed", "down":
		c.failed = append(c.failed, value)
	}
	fast := c.fast
	if fast && how == "committed" {
		if i, ok := c.inserted[value]; ok {
			c.ackedAt = append(c.ackedAt, max(i, deref(last(c.ackedAt))))
		} else {
			fast = false
		}
	}
	c.mu.Unlock()
	if how == "committed" && !fast {
		// An acknowledged write that the log lacks leaves no place in the log for the fast check.
		c.slow()
		if n := count(c.chain(c.headNow()), value); n != 1 {
			c.violate(invariantAcknowledged, false, nil, "the write of %v is acknowledged, and the log holds it %v times", value, n)
		}
	}
}

// last element of s, or nil.
func last[T any](s []T) *T {
	if len(s) == 0 {
		return nil
	}
	return &s[len(s)-1]
}

// lastAcked gives the key of the write that was acknowledged last, if any.
func (c *checker) lastAcked() (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.acked) == 0 {
		return 0, false
	}
	return c.acked[len(c.acked)-1], true
}

// readStarted returns what a read that starts now must see.
func (c *checker) readStarted() readSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return readSeen{acked: len(c.acked), unknown: len(c.unknown), at: c.r.world.Monotonic()}
}

// readEnded on p, which started as before says, selected the row of the key, and saw the keys at the
// position.
func (c *checker) readEnded(ctx context.Context, p *process, before readSeen, key int64, keys []int64, position format.Name) {
	before.key = key
	if c.readFast(p, before, keys, position) {
		return
	}
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
	switch {
	case at < 0:
		c.violate(invariantState, false, p, "a read saw the position %v, which is not in the log", short(position.String()))
	case len(seen) < len(keys):
		c.violate(invariantState, false, p, "a read at %v saw a row twice", short(position.String()))
	default:
		want := map[int64]bool{}
		if holds(chain[:at], key) {
			want[key] = true
		}
		if !sameKeys(seen, want) {
			c.violate(invariantState, false, p, "a read of %v at %v saw %v rows, and the log has %v there", key, short(position.String()), len(seen), len(want))
		}
	}

	c.mu.Lock()
	acked := slices.Clone(c.acked[:before.acked])
	c.mu.Unlock()
	state := c.stateOf(position)
	for _, v := range acked {
		if v == key && !seen[v] || v != key && !state()[v] {
			c.violate(invariantFresh, p.bothClocks(), p, "a read missed the write of %v, which was acknowledged before the read started", v)
			break
		}
	}
	before.keys, before.position, before.p = seen, position, p
	c.mu.Lock()
	c.reads = append(c.reads, before)
	c.mu.Unlock()
}

// readFast checks the read as [checker.readEnded] does, in a time that does not grow with the log,
// and reports whether it found nothing wrong. Otherwise the read is checked again the slow way.
func (c *checker) readFast(p *process, before readSeen, keys []int64, position format.Name) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fast {
		return false
	}
	at, ok := c.position[position.String()]
	if position == format.EmptyLog {
		at, ok = 0, true
	}
	if !ok {
		return false
	}
	// The read gives the row of its key if and only if the log has it at its position.
	if c.live(before.key, at) {
		if len(keys) != 1 || keys[0] != before.key {
			return false
		}
	} else if len(keys) != 0 {
		return false
	}
	// The read saw the row of its key in the log at its position, so it saw every acknowledged
	// write, its key by its row and every other by its position, if their commit objects are among
	// the first at.
	if before.acked > 0 && c.ackedAt[before.acked-1] >= at {
		return false
	}
	before.p, before.fast, before.prefix = p, true, at
	c.reads = append(c.reads, before)
	return true
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
	c.logAtEnd = len(chain)
	acked, unknown, failed, reads := slices.Clone(c.acked), slices.Clone(c.unknown), slices.Clone(c.failed), slices.Clone(c.reads)
	inserted := c.inserted
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
		state := c.stateOf(read.position)
		for _, v := range unknown[:read.unknown] {
			// A read that was checked fast saw the write if its commit object is among the first of
			// the log, which is still the log at the end, as the checker is still fast. Another saw
			// it if it gave its row, for its key, or if its state holds it.
			if inLog[v] == 0 {
				continue
			}
			var missed bool
			if read.fast {
				i, ok := inserted[v]
				missed = !ok || i >= read.prefix
			} else {
				missed = v == read.key && !read.keys[v] || v != read.key && !state()[v]
			}
			if missed {
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
