// Package host stands in for the production host, which is exempt from the lint, so nothing here is
// reported.
package host

import (
	"math/rand/v2"
	"time"
)

type Host interface{}

func New() Host {
	return nil
}

func Sleep(d time.Duration) uint64 {
	go func() {}()
	time.Sleep(d)
	return rand.Uint64()
}
