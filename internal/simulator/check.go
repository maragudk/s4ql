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
	// end, unless a later commit up to its position deleted the row. The design accepts a read that
	// misses one on an instance whose two clocks both went wrong in the direction that keeps a lease
	// alive too long; such a violation is excused.
	invariantFresh = "fresh"
)

// checker of the invariants, over the store as it is and over what the calls saw.
//
// It checks a read in a time that does not grow with the log, as long as nothing has been found
// wrong: it keeps the log as a chain that grows when the head moves on to a child of the head, with
// where each key was inserted and deleted, and the number of rows after each commit object. Once it
// finds a violation, or the log does anything else, it is no longer fast, and checks each read and
// write against the chain from the head, as it is then.
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
	// inserted and deleted hold, for each key, the index in log of the commit object that inserts
	// it and that deletes it.
	inserted, deleted map[int64]int
	// rows after each prefix of the log: rows[i] is the number of rows after log[:i].
	rows []int
	// ackedAt holds, for each acknowledged write in turn, the latest index in log of the commit
	// objects of it and of the writes acknowledged before it.
	ackedAt []int
}

// readSeen is a read that was served: how many acknowledged and unknown writes had ended before it
// started, and its process. A read that the checker checked fast keeps the number of commit objects
// of the log up to its position, and others the keys that they saw and that the log had deleted up
// to their position.
type readSeen struct {
	acked, unknown int
	p              *process
	at             time.Duration
	fast           bool
	prefix         int
	keys, deleted  map[int64]bool
}

func newChecker(r *run) *checker {
	return &checker{
		r:        r,
		commits:  map[string]*CommitObject{},
		head:     format.EmptyLog.String(),
		fast:     true,
		position: map[string]int{},
		inserted: map[int64]int{},
		deleted:  map[int64]int{},
		rows:     []int{0},
	}
}

// violate records a violation of the invariant.
func (c *checker) violate(invariant string, excused bool, p *process, format string, args ...any) {
	c.slow()
	e := Event{Kind: "check", What: "violated", Invariant: invariant, Excused: excused, Text: fmt.Sprintf(format, args...)}
	if p != nil {
		e.Instance, e.Process = p.i.name, p.name
	}
	c.r.rec.add(e)
}

// slow makes the checker check against the chain from the head from now on. The reads that it
// checked fast get the keys that they saw, which are those of the log at their position.
func (c *checker) slow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fast {
		return
	}
	c.fast = false
	for i, read := range c.reads {
		if read.fast {
			c.reads[i].keys, c.reads[i].deleted = rowsOf(c.log[:read.prefix])
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
		c.violate(invariantChain, false, nil, "the manifest names the head %v, which is not in the store", short(m.Head))
	case o.Parent != before:
		c.violate(invariantChain, false, nil, "the manifest moves its head from %v to %v, whose parent is %v", short(before), short(m.Head), short(o.Parent))
	}
}

// extend the log by the commit object o, the new head. It requires the mutex, and that the checker
// is fast. A key that the log inserts or deletes twice makes it slow.
func (c *checker) extend(o *CommitObject) {
	at := len(c.log)
	rows := c.rows[at]
	for _, k := range o.Keys {
		if _, ok := c.inserted[k]; ok {
			c.fast = false
		}
		c.inserted[k] = at
		rows++
	}
	for _, k := range o.Deletes {
		if _, ok := c.deleted[k]; ok {
			c.fast = false
		}
		c.deleted[k] = at
		if i, ok := c.inserted[k]; ok && i < at {
			rows--
		}
	}
	c.log = append(c.log, o)
	c.position[o.Name] = at + 1
	c.rows = append(c.rows, rows)
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
	if !ok || i >= at {
		return false
	}
	d, ok := c.deleted[k]
	return !ok || d < i || d >= at
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

// rowsOf the commit objects of chain in turn: the keys that are rows after them, and the keys that
// they insert and a later one of them deletes.
func rowsOf(chain []*CommitObject) (rows, deleted map[int64]bool) {
	rows, deleted = map[int64]bool{}, map[int64]bool{}
	for _, o := range chain {
		for _, k := range o.Keys {
			rows[k] = true
		}
		for _, k := range o.Deletes {
			if rows[k] {
				delete(rows, k)
				deleted[k] = true
			}
		}
	}
	return rows, deleted
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
	fast := c.fast
	if fast && how == "committed" {
		i, ok := c.inserted[value]
		if !ok {
			fast = false
		}
		c.ackedAt = append(c.ackedAt, max(i, deref(last(c.ackedAt))))
	}
	c.mu.Unlock()
	if how == "committed" && !fast {
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

// readStarted returns what a read that starts now must see.
func (c *checker) readStarted() readSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return readSeen{acked: len(c.acked), unknown: len(c.unknown), at: c.r.world.Monotonic()}
}

// readEnded on p, which started as before says and saw the keys at the position.
func (c *checker) readEnded(ctx context.Context, p *process, before readSeen, keys []int64, position format.Name) {
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
	deleted := map[int64]bool{}
	if at < 0 {
		c.violate(invariantState, false, p, "a read saw the position %v, which is not in the log", short(position.String()))
	} else {
		var want map[int64]bool
		want, deleted = rowsOf(chain[:at])
		if !sameKeys(seen, want) {
			c.violate(invariantState, false, p, "a read at %v saw %v rows, and the log has %v rows there", short(position.String()), len(seen), len(want))
		}
	}

	c.mu.Lock()
	acked := slices.Clone(c.acked[:before.acked])
	c.mu.Unlock()
	for _, v := range acked {
		if !seen[v] && !deleted[v] {
			c.violate(invariantFresh, p.bothClocks(), p, "a read missed the write of %v, which was acknowledged before the read started", v)
			break
		}
	}
	before.keys, before.deleted, before.p = seen, deleted, p
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
	if !ok || len(keys) != c.rows[at] {
		return false
	}
	for _, k := range keys {
		if !c.live(k, at) {
			return false
		}
	}
	// The read saw the rows of the log at its position, so it saw an acknowledged write if and only
	// if its commit object is among the first at, and was not deleted since.
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
		for _, v := range unknown[:read.unknown] {
			// A read that was checked fast saw the write if its commit object is among the first of
			// the log, which is still the log at the end, as the checker is still fast.
			missed := !read.keys[v] && !read.deleted[v]
			if read.fast {
				i, ok := inserted[v]
				missed = !ok || i >= read.prefix
			}
			if inLog[v] > 0 && missed {
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
