package s3store_test

import (
	"os"
	"testing"
)

// The file is in the list of allowed files, so nothing here is reported.
func TestRealBucket(t *testing.T) {
	if os.Getenv("S4QL_S3_BUCKET") == "" {
		t.Skip("set S4QL_S3_BUCKET")
	}
}
