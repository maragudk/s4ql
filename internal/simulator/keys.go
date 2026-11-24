//go:build s4ql_browser

package simulator

import (
	"slices"
	"sync"
)

// knownKeys is what the application knows of the rows of the table of the workload, from which the
// reads and the deletes pick their keys. A key is inserted by one write, the one that took its value
// from the counter, and deleted by one at most, because a delete picks only a key that no delete has
// picked before: see [knownKeys.pickDelete].
type knownKeys struct {
	mu sync.Mutex
	// known holds the keys whose rows the application knows are in the store, in the order in which
	// it learned them: from an acknowledged insert, from the log of a stored bucket, or from a delete
	// that ended without an unknown outcome and without being acknowledged. A key leaves it when a
	// delete picks it.
	known []int64
	// deletable holds the keys of known that a delete can pick: those that no delete has picked.
	deletable []int64
	// gone holds the keys of the newest acknowledged deletes, at most [recentDeletes] of them.
	gone []int64
}

// recentKeys is the number of the newest known keys among which a read picks the one it selects,
// when it does not select the newest.
const recentKeys = 50

// recentDeletes is the number of the newest acknowledged deletes whose keys a read can select, to
// see that the row is gone.
const recentDeletes = 10

// goneReads: one read in goneReads selects the key of a recent acknowledged delete, if there is one.
const goneReads = 8

// stored: the store holds the log of a stored bucket, from the first commit object to the head. The
// application knows the rows of the state at the head, in the order of their inserts.
func (k *knownKeys) stored(log []*CommitObject) {
	k.mu.Lock()
	defer k.mu.Unlock()
	rows := map[int64]bool{}
	for _, o := range log {
		for _, key := range o.Keys {
			rows[key] = true
		}
		for _, key := range o.Deletes {
			delete(rows, key)
		}
	}
	for _, o := range log {
		for _, key := range o.Keys {
			if rows[key] {
				k.learn(key)
			}
		}
	}
}

// learn that the row of the key is in the store. It requires the mutex.
func (k *knownKeys) learn(key int64) {
	k.known = append(k.known, key)
	k.deletable = append(k.deletable, key)
}

// ended: a write of the workload, the insert or the delete of the row of the key, ended in the given
// way, as the event that ends its call says. An acknowledged insert makes its row known. An
// acknowledged delete makes its key one that a read can select to see that the row is gone. A delete
// of unknown outcome, or one that was lost at a crash of its process, leaves its row in limbo: the
// application selects it no more and deletes it no more. A delete that ended in another way did not
// delete the row, which is known again; no delete picks it again, so that its key stays the key of
// one delete.
func (k *knownKeys) ended(key int64, isDelete bool, how string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	switch {
	case !isDelete:
		if how == "committed" {
			k.learn(key)
		}
	case how == "committed":
		k.gone = append(k.gone, key)
		if len(k.gone) > recentDeletes {
			k.gone = slices.Delete(k.gone, 0, 1)
		}
	case how == "unknown", how == "lost":
	default:
		k.known = append(k.known, key)
	}
}

// pickRead picks the key that a read selects, by the number u, and whether the application expects
// its row. One read in [goneReads] selects the key of one of the recent acknowledged deletes, if
// there is one, and expects no row. The others select a key that the application knows is in the
// store: half of them the newest, whose row a read that is not stale gives, and the others one of the
// [recentKeys] newest. ok is false while there is no key to pick.
func (k *knownKeys) pickRead(u uint64) (key int64, row, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.gone) > 0 && u%goneReads == 0 {
		return k.gone[u/goneReads%uint64(len(k.gone))], false, true
	}
	if len(k.known) == 0 {
		return 0, false, false
	}
	u /= goneReads
	i := len(k.known) - 1
	if u%2 != 0 {
		i -= int(u / 2 % uint64(min(recentKeys, len(k.known))))
	}
	return k.known[i], true, true
}

// pickDelete picks the key that a delete deletes, by the number u, among the keys that the
// application knows are in the store and that no delete has picked before. The key leaves the known
// keys at once, so that no read selects it while the delete runs. It leaves the last known key alone,
// so that the reads always have a key to select. ok is false while there is no key to pick.
func (k *knownKeys) pickDelete(u uint64) (key int64, ok bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.deletable) == 0 || len(k.known) < 2 {
		return 0, false
	}
	i := int(u % uint64(len(k.deletable)))
	key = k.deletable[i]
	k.deletable[i] = k.deletable[len(k.deletable)-1]
	k.deletable = k.deletable[:len(k.deletable)-1]
	if j := slices.Index(k.known, key); j >= 0 {
		k.known = slices.Delete(k.known, j, j+1)
	}
	return key, true
}
