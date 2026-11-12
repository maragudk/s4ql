// Package timestamppb stands in for google.golang.org/protobuf/types/known/timestamppb, whose Now
// the lint forbids.
package timestamppb

import "time"

// Timestamp is a point in time.
type Timestamp struct{}

// Now reads the clock.
func Now() *Timestamp { return &Timestamp{} }

// New Timestamp from t.
func New(t time.Time) *Timestamp { return &Timestamp{} }
