//go:build s4ql_never

package tagged

import "testing"

// The build leaves this file out, so it has no type information, and every selector named as a
// skip counts. The test of the analyzer expects a report on the line of the first skip alone.
func TestTagged(t *testing.T) {
	t.Skip()
	if testing.Short() {
		t.Skip()
	}
}
