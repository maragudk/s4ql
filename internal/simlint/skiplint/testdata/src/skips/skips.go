// Package skips holds planted skips for the tests of skiplint, also in a file that is not a test.
package skips

import (
	"os"
	"testing"
)

// needsBucket is a helper of tests in a file that is not a test, where the rule holds too.
func needsBucket(t testing.TB) {
	if os.Getenv("S4QL_S3_BUCKET") == "" {
		t.Skip("no bucket") // want `Skip skips the test`
	}
	if testing.Short() {
		t.Skip("needs a bucket")
	}
}
