package simstore

import (
	"slices"
	"time"
)

// Landing of a write that the store held back: one whose caller had given up or whose sender had
// crashed when it arrived, and one that was stuck in the network, by [Faults.Late],
// [Faults.AfterCrash], and [Faults.Long].
type Landing struct {
	// Request is the number of the request in the trace.
	Request int
	// Client that sent the write, by its name.
	Client string
	Key    string
	// Create is true for a create, and false for a swap.
	Create bool
	// Sent and Landed are the times at which the write was sent and at which the store applied or
	// refused it, on the monotonic clock of the store's host.
	Sent, Landed time.Duration
	// Applied is true if the store applied the write, and false if it refused it.
	Applied bool
}

// Landings of the writes that the store held back and that then arrived, in the order in which
// they arrived. A write that was held back and then never arrived is not among them.
func (s *Store) Landings() []Landing {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.landings)
}

// holdBack is how long a write that the faults f hold back waits before it arrives, from the seed:
// up to [Faults.LongBy] by [Faults.Long], and otherwise up to [Faults.LateBy].
func (s *Store) holdBack(f Faults) time.Duration {
	if s.chance(f.Long) {
		return s.upTo(f.LongBy)
	}
	return s.upTo(f.LateBy)
}

// landed records the landing of the write r of c, which the store held back, after [Store.apply].
func (s *Store) landed(c *Client, r *request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.landings = append(s.landings, Landing{
		Request: r.id,
		Client:  c.name,
		Key:     r.key,
		Create:  r.kind == create,
		Sent:    r.sent,
		Landed:  s.host.Monotonic(),
		Applied: r.err == nil,
	})
}
