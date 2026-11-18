// Package wiring is library code inside the module, which may import the host, and must not make
// the production host itself.
package wiring

import (
	"maragu.dev/s4ql/internal/host"
)

func hosts() {
	_ = host.New() // want `host.New makes the production host`
	f := host.New  // want `host.New makes the production host`
	_ = f

	// Library code uses the host it is given.
	var h host.Host
	_ = h
	_ = host.Sleep
}
