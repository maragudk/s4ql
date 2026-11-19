package s3store_test

import "testing"

// Only s3store_test.go of the package is allowed, so the skip here is reported.
func TestOther(t *testing.T) {
	t.Skip() // want `Skip skips the test`
}
