// Command cleanup deletes the prefixes that tests against a real bucket left under s4ql-test/ more
// than a day ago, with [s3test.DeleteOld], and prints each prefix that it deleted. A test deletes its
// own prefix when it ends, so these are what runs that were killed left behind. The bucket and the
// credentials come from the environment, as for the tests: see the documentation of
// internal/store/s3test. It fails if S4QL_S3_BUCKET is empty.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"maragu.dev/s4ql/internal/store/s3test"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cleanup:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	c, err := s3test.Load()
	if err != nil {
		return err
	}
	client, err := c.Client(ctx)
	if err != nil {
		return err
	}
	deleted, err := s3test.DeleteOld(ctx, client, c.Bucket, time.Now().Add(-24*time.Hour))
	for _, prefix := range deleted {
		fmt.Println("deleted", prefix)
	}
	if err != nil {
		return err
	}
	fmt.Printf("deleted %v prefixes under %v that are more than a day old\n", len(deleted), s3test.Root)
	return nil
}
