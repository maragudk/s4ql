//go:build s4ql_browser

package simulator

import (
	"cmp"
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
// write of the workload inserts or deletes the row of one key, and each key is inserted by one
// transaction and deleted by one at most, so a key and whether a write deletes it tell the
// transactions apart: see [txn]. A read selects the row of one key, and its position says what else
// its state holds.
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
	// invariantOnce: no transaction is in the log twice at the end, and the log deletes a row only
	// where it holds it.
	invariantOnce = "once"
	// invariantState: a read sees a state that the log had: its position is in the log, and it gives
	// the row of its key, once, if the log up to that position has inserted the key and not deleted
	// it since, and no row otherwise.
	invariantState = "state"
	// invariantFresh: a read sees every commit that was acknowledged before it started, and every
	// commit of unknown outcome that had returned before it started and that is in the log at the
	// end: the state at its position holds the commit object of each, an insert or a delete, of its
	// own key or of another. The design accepts a read that misses one on an instance whose two
	// clocks both went wrong in the direction that keeps a lease alive too long; such a violation is
	// excused.
	invariantFresh = "fresh"
)

// txn is a write transaction of the workload: the insert or the delete of the row of one key. A key
// is inserted by one transaction, the write that took its value from the counter. It is deleted by
// one at most, because a delete picks only a key that the application knows is in the store and
// that no delete has picked before: see [checker.pickDelete].
type txn struct {
	key    int64
	delete bool
}

func (w txn) String() string {
	if w.delete {
		return fmt.Sprint("the delete of ", w.key)
	}
	return fmt.Sprint("the insert of ", w.key)
}

// in reports whether the commit object o holds the write.
func (w txn) in(o *CommitObject) bool {
	if w.delete {
		return slices.Contains(o.Deletes, w.key)
	}
	return slices.Contains(o.Keys, w.key)
}

// checker of the invariants, over the store as it is and over what the calls saw. It also keeps what
// the application knows of the rows, from which the calls pick their keys.
//
// It checks a read in a time that does not grow with the log, as long as the log is one chain: it
// keeps the log as a chain that grows when the head moves on to a child of the head, with where each
// key was inserted and deleted. A read that the fast check does not pass is checked again against the
// chain from the head, which finds its violations. Once the log is not one chain, an acknowledged
// write is not in it, a key is inserted or deleted twice, or a row is deleted where the log does not
// hold it, the checker is no longer fast, and checks each read and write against the chain from the
// head, as it is then.
type checker struct {
	r *run

	mu      sync.Mutex
	commits map[string]*CommitObject
	head    string
	// acked, unknown, and failed are the writes that were acknowledged, that ended with an unknown
	// outcome, and that failed otherwise, in the order in which they ended.
	acked, unknown, failed []txn
	// reads that were served, for the checks at the end.
	reads []readSeen
	// logAtEnd is the number of commit objects in the log at the end of the run.
	logAtEnd int

	// known holds the keys whose rows the application knows are in the store, in the order in which
	// it learned them: from an acknowledged insert, from the log of a stored bucket, or from a
	// delete that ended without an unknown outcome and without being acknowledged. A key leaves it
	// when a delete picks it.
	known []int64
	// deletable holds the keys of known that a delete can pick: those that no delete has picked.
	deletable []int64
	// gone holds the keys of the newest acknowledged deletes, at most [recentDeletes] of them.
	gone []int64

	// fast is set while the fields below describe the log, from the first commit object to head.
	fast bool
	log  []*CommitObject
	// position of each commit object of the log, from 1.
	position map[string]int
	// inserted and deleted hold, for each key, the index in log of the commit object that inserts
	// it and of the one that deletes it.
	inserted, deleted map[int64]int
	// ackedAt holds, for each acknowledged write in turn, the latest index in log of the commit
	// objects of it and of the writes acknowledged before it.
	ackedAt []int
}

// readSeen is a read that was served: how many acknowledged and unknown writes had ended before it
// started, its process, the key that it selected, and its position. A read that the checker checked
// fast keeps the number of commit objects of the log up to its position in place of the position.
type readSeen struct {
	acked, unknown int
	p              *process
	at             time.Duration
	key            int64
	fast           bool
	prefix         int
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
		deleted:  map[int64]int{},
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
// checked fast get their positions.
func (c *checker) slow() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fast {
		return
	}
	c.fast = false
	for i, read := range c.reads {
		if read.fast {
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

// seed the checker with the log of a bucket that the store holds from before the run, from the first
// commit object to the head, which [checker.commit] has given it. The bucket was checked as the
// invariants check a log, so the log is one chain with each insert and each delete once, and each
// delete after its insert. Its writes count as acknowledged before the run, in the order of the log:
// S3 held them, so every read of the run must see them. The application knows the rows of the state
// at the head, in the order of their inserts.
func (c *checker) seed(log []*CommitObject, head string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, o := range log {
		c.extend(o)
		for _, w := range writesOf(o) {
			c.acked = append(c.acked, w)
			c.ackedAt = append(c.ackedAt, i)
		}
	}
	c.head = head
	rows := rowsOf(log)
	for _, o := range log {
		for _, k := range o.Keys {
			if rows[k] {
				c.learn(k)
			}
		}
	}
}

// writesOf the commit object o: its inserts, and then its deletes.
func writesOf(o *CommitObject) []txn {
	var writes []txn
	for _, k := range o.Keys {
		writes = append(writes, txn{key: k})
	}
	for _, k := range o.Deletes {
		writes = append(writes, txn{key: k, delete: true})
	}
	return writes
}

// rowsOf the state after the commit objects of chain: the keys that they insert, less those that
// they delete after.
func rowsOf(chain []*CommitObject) map[int64]bool {
	rows := map[int64]bool{}
	for _, o := range chain {
		for _, k := range o.Keys {
			rows[k] = true
		}
		for _, k := range o.Deletes {
			delete(rows, k)
		}
	}
	return rows
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
// is fast. A key that the log inserts twice, and a delete of a row that the log does not hold, make
// it slow.
func (c *checker) extend(o *CommitObject) {
	at := len(c.log)
	for _, k := range o.Keys {
		if _, ok := c.inserted[k]; ok {
			c.fast = false
		}
		c.inserted[k] = at
	}
	for _, k := range o.Deletes {
		if !c.live(k, at) {
			c.fast = false
		}
		c.deleted[k] = at
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

// live reports whether the key k is a row after the first at commit objects of the log: one of them
// inserts it, and none deletes it. It requires the mutex, and that the checker is fast.
func (c *checker) live(k int64, at int) bool {
	i, ok := c.inserted[k]
	if !ok || i >= at {
		return false
	}
	d, ok := c.deleted[k]
	return !ok || d >= at
}

// indexOf the write in the log, if the log holds it. It requires the mutex, and that the checker is
// fast.
func (c *checker) indexOf(w txn) (int, bool) {
	index := c.inserted
	if w.delete {
		index = c.deleted
	}
	i, ok := index[w.key]
	return i, ok
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

// writesAt the position: the writes whose commit objects are the commit object of the position or
// its ancestors in the store, which are the writes that the state at the position holds. A read
// selects one key, but its position says whether its state holds the other writes, so a read at a
// state without an acknowledged write is stale whatever key it selects. The writes of a position do
// not change, because a commit object names its parent. writesAt walks the ancestors once, on the
// first call of the function that it gives.
func (c *checker) writesAt(position format.Name) func() map[txn]bool {
	var writes map[txn]bool
	return func() map[txn]bool {
		if writes != nil {
			return writes
		}
		writes = map[txn]bool{}
		c.mu.Lock()
		defer c.mu.Unlock()
		empty := format.EmptyLog.String()
		for name, n := position.String(), 0; name != empty && n <= len(c.commits); n++ {
			o := c.commits[name]
			if o == nil {
				break
			}
			for _, w := range writesOf(o) {
				writes[w] = true
			}
			name = o.Parent
		}
		return writes
	}
}

// count of the commit objects in chain that hold the write.
func count(chain []*CommitObject, w txn) int {
	n := 0
	for _, o := range chain {
		if w.in(o) {
			n++
		}
	}
	return n
}

// writeEnded: the write, which the call that ended in the given way made. A write of the client that
// was lost at a crash of its process has an outcome that the client does not know, and one that
// found its instance down was never made.
func (c *checker) writeEnded(_ context.Context, w txn, how string) {
	c.mu.Lock()
	switch how {
	case "committed":
		c.acked = append(c.acked, w)
	case "unknown", "lost":
		c.unknown = append(c.unknown, w)
	case "not_committed", "down":
		c.failed = append(c.failed, w)
	}
	c.learnEnd(w, how)
	fast := c.fast
	if fast && how == "committed" {
		if i, ok := c.indexOf(w); ok {
			c.ackedAt = append(c.ackedAt, max(i, deref(last(c.ackedAt))))
		} else {
			fast = false
		}
	}
	c.mu.Unlock()
	if how == "committed" && !fast {
		// An acknowledged write that the log lacks leaves no place in the log for the fast check.
		c.slow()
		if n := count(c.chain(c.headNow()), w); n != 1 {
			c.violate(invariantAcknowledged, false, nil, "%v is acknowledged, and the log holds it %v times", w, n)
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

// recentKeys is the number of the newest known keys among which a read picks the one it selects,
// when it does not select the newest.
const recentKeys = 50

// recentDeletes is the number of the newest acknowledged deletes whose keys a read can select, to
// see that the row is gone.
const recentDeletes = 10

// goneReads: one read in goneReads selects the key of a recent acknowledged delete, if there is one.
const goneReads = 8

// learn that the row of the key is in the store. It requires the mutex.
func (c *checker) learn(k int64) {
	c.known = append(c.known, k)
	c.deletable = append(c.deletable, k)
}

// learnEnd: what the application learns of the rows from how the write ended. It requires the mutex.
// An acknowledged insert makes its row known. An acknowledged delete makes its key one that a read
// can select to see that the row is gone. A delete of unknown outcome leaves its row in limbo: the
// application selects it no more and deletes it no more. A delete that ended in another way did not
// delete the row, which is known again; no delete picks it again, so that its key stays the key of
// one delete.
func (c *checker) learnEnd(w txn, how string) {
	switch {
	case !w.delete:
		if how == "committed" {
			c.learn(w.key)
		}
	case how == "committed":
		c.gone = append(c.gone, w.key)
		if len(c.gone) > recentDeletes {
			c.gone = slices.Delete(c.gone, 0, 1)
		}
	case how == "unknown", how == "lost":
	default:
		c.known = append(c.known, w.key)
	}
}

// pickRead picks the key that a read selects, by the number u, and whether the application expects
// its row. One read in [goneReads] selects the key of one of the recent acknowledged deletes, if
// there is one, and expects no row. The others select a key that the application knows is in the
// store: half of them the newest, whose row a read that is not stale gives, and the others one of the
// [recentKeys] newest. ok is false while there is no key to pick.
func (c *checker) pickRead(u uint64) (key int64, row, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.gone) > 0 && u%goneReads == 0 {
		return c.gone[u/goneReads%uint64(len(c.gone))], false, true
	}
	if len(c.known) == 0 {
		return 0, false, false
	}
	u /= goneReads
	i := len(c.known) - 1
	if u%2 != 0 {
		i -= int(u / 2 % uint64(min(recentKeys, len(c.known))))
	}
	return c.known[i], true, true
}

// pickDelete picks the key that a delete deletes, by the number u, among the keys that the
// application knows are in the store and that no delete has picked before. The key leaves the known
// keys at once, so that no read selects it while the delete runs. It leaves the last known key alone,
// so that the reads always have a key to select. ok is false while there is no key to pick.
func (c *checker) pickDelete(u uint64) (key int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.deletable) == 0 || len(c.known) < 2 {
		return 0, false
	}
	i := int(u % uint64(len(c.deletable)))
	key = c.deletable[i]
	c.deletable[i] = c.deletable[len(c.deletable)-1]
	c.deletable = c.deletable[:len(c.deletable)-1]
	if j := slices.Index(c.known, key); j >= 0 {
		c.known = slices.Delete(c.known, j, j+1)
	}
	return key, true
}

// readStarted returns what a read that starts now must see.
func (c *checker) readStarted() readSeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return readSeen{acked: len(c.acked), unknown: len(c.unknown), at: c.r.world.Monotonic()}
}

// readEnded on p, which started as before says, selected the row of the key, and saw the keys at the
// position.
func (c *checker) readEnded(_ context.Context, p *process, before readSeen, key int64, keys []int64, position format.Name) {
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
		if rowsOf(chain[:at])[key] {
			want[key] = true
		}
		if !sameKeys(seen, want) {
			c.violate(invariantState, false, p, "a read of %v at %v saw %v rows, and the log has %v there", key, short(position.String()), len(seen), len(want))
		}
	}

	c.mu.Lock()
	acked := slices.Clone(c.acked[:before.acked])
	c.mu.Unlock()
	writes := c.writesAt(position)
	for _, w := range acked {
		if !writes()[w] {
			c.violate(invariantFresh, p.bothClocks(), p, "a read missed %v, which was acknowledged before the read started", w)
			break
		}
	}
	before.position, before.p = position, p
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
	// The read saw every acknowledged write, an insert or a delete, of its key or of another, if
	// their commit objects are among the first at.
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
	inserted, deleted := c.inserted, c.deleted
	c.mu.Unlock()

	inLog := map[txn]int{}
	for _, o := range chain {
		for _, w := range writesOf(o) {
			inLog[w]++
		}
	}
	for _, w := range acked {
		if inLog[w] == 0 {
			c.violate(invariantAcknowledged, false, nil, "%v was acknowledged, and the log does not hold it at the end", w)
		}
	}
	for _, w := range failed {
		if inLog[w] > 0 {
			c.violate(invariantNotCommitted, false, nil, "%v failed, and the log holds it at the end", w)
		}
	}
	byKey := func(a, b txn) int {
		if a.key != b.key {
			return cmp.Compare(a.key, b.key)
		}
		return cmp.Compare(fmt.Sprint(a.delete), fmt.Sprint(b.delete))
	}
	for _, w := range slices.SortedFunc(maps.Keys(inLog), byKey) {
		if inLog[w] > 1 {
			c.violate(invariantOnce, false, nil, "the log holds %v %v times at the end", w, inLog[w])
		}
	}
	rows := map[int64]bool{}
	for _, o := range chain {
		for _, k := range o.Keys {
			rows[k] = true
		}
		for _, k := range o.Deletes {
			// A delete that the log holds twice has its violation above.
			if !rows[k] && inLog[txn{key: k, delete: true}] == 1 {
				c.violate(invariantOnce, false, nil, "the log deletes the row of %v in %v, where it does not hold the row", k, short(o.Name))
			}
			delete(rows, k)
		}
	}
	for _, read := range reads {
		writes := c.writesAt(read.position)
		for _, w := range unknown[:read.unknown] {
			// A read that was checked fast saw the write if its commit object is among the first of
			// the log, which is still the log at the end, as the checker is still fast. Another saw
			// it if the state at its position holds it.
			if inLog[w] == 0 {
				continue
			}
			var missed bool
			if read.fast {
				index := inserted
				if w.delete {
					index = deleted
				}
				i, ok := index[w.key]
				missed = !ok || i >= read.prefix
			} else {
				missed = !writes()[w]
			}
			if missed {
				c.violate(invariantFresh, read.p.bothClocks(), read.p, "a read at %v missed %v, which returned with an unknown outcome before the read started, and which the log holds at the end", millis(read.at), w)
				break
			}
		}
	}
}

// short form of the name of a commit object.
func short(name string) string {
	return name[:min(len(name), 8)]
}
