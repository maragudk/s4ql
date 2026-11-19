// Package s3test stands in for the package of the same path, whose every file is allowed to skip.
package s3test

import (
	"os"
	"testing"
)

// Bucket of the environment. The directory is in the list of allowed files, so nothing here is
// reported.
func Bucket(t testing.TB) string {
	b := os.Getenv("S4QL_S3_BUCKET")
	if b == "" {
		t.Skip("set S4QL_S3_BUCKET")
	}
	return b
}
