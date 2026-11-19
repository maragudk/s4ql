// Package s3test holds what the tests against a real bucket share: the settings of the bucket from
// the environment, a prefix of each test's own that is deleted with everything under it when the
// test ends, and the deletion of the prefixes that runs which were killed left behind.
//
// Every key that a test writes is under [Root], in a prefix of the form
// s4ql-test/<suite>/<UTC time>-<8 random characters>/, which [NewPrefix] gives. Nothing deletes
// anything else: [DeletePrefix] refuses every prefix that does not have that form, and every key
// that a listing gives outside the prefix.
//
// # Running the tests against a real bucket
//
// The tests that need a bucket are skipped unless S4QL_S3_BUCKET names one. They read AWS_REGION and
// AWS_ENDPOINT_URL_S3, and S4QL_S3_PATH_STYLE=true for a store that needs the bucket in the path,
// and the credentials come from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or from the default
// chain of the AWS SDK. make test-s3 runs all of them, under the race detector, and fails if
// S4QL_S3_BUCKET is empty or if one of them did not run and pass. CI runs it in the job s3. With the
// settings of the project's bucket in the file .env of the repository's root, which is never
// committed:
//
//	(set -a; . ./.env; set +a; S4QL_S3_BUCKET=$BUCKET make test-s3)
//
// The credentials need s3:GetObject, s3:PutObject, s3:ListBucket, and s3:DeleteObject. The command
// in internal/store/s3test/cleanup deletes the prefixes under [Root] that are more than a day old.
package s3test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Root of every key that a test writes to a real bucket.
const Root = "s4ql-test/"

// timeLayout of the time in the name of a prefix, in UTC.
const timeLayout = "20060102T150405"

// prefixPattern is the form of a prefix of [NewPrefix]: the suite, the time, and 8 characters of
// [rand.Text], whose alphabet is base32's.
var prefixPattern = regexp.MustCompile(`^s4ql-test/[a-z0-9-]+/([0-9]{8}T[0-9]{6})-[A-Z2-7]{8}/$`)

// suitePattern is the form of the name of a suite.
var suitePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ErrNoBucket is the error of [Load] when S4QL_S3_BUCKET is empty.
var ErrNoBucket = errors.New("s3test: S4QL_S3_BUCKET names no bucket")

// Config of a real bucket.
type Config struct {
	// Bucket from S4QL_S3_BUCKET.
	Bucket string
	// Endpoint from AWS_ENDPOINT_URL_S3, or empty.
	Endpoint string
	// Region from AWS_REGION, or empty.
	Region string
	// PathStyle is whether S4QL_S3_PATH_STYLE is true.
	PathStyle bool
	// AccessKeyID and SecretAccessKey from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, or empty.
	AccessKeyID     string
	SecretAccessKey string
}

// Load the configuration of the bucket from the environment. It fails with [ErrNoBucket] if
// S4QL_S3_BUCKET is empty.
func Load() (Config, error) {
	c := Config{
		Bucket:          os.Getenv("S4QL_S3_BUCKET"),
		Endpoint:        os.Getenv("AWS_ENDPOINT_URL_S3"),
		Region:          os.Getenv("AWS_REGION"),
		PathStyle:       os.Getenv("S4QL_S3_PATH_STYLE") == "true",
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
	}
	if c.Bucket == "" {
		return Config{}, ErrNoBucket
	}
	return c, nil
}

// FromEnv gives the configuration of the bucket from the environment, or skips the test if
// S4QL_S3_BUCKET is empty.
func FromEnv(t testing.TB) Config {
	t.Helper()
	c, err := Load()
	if errors.Is(err, ErrNoBucket) {
		t.Skip("set S4QL_S3_BUCKET to a bucket, and AWS_REGION, AWS_ENDPOINT_URL_S3, and credentials for the SDK, to run the tests against a real bucket; see the documentation of internal/store/s3test")
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Client of the SDK for the bucket, for what a test does next to the code under test, such as
// deleting. It takes the credentials of the configuration if it has them, and otherwise the default
// chain's, and logs nothing.
func (c Config) Client(ctx context.Context) (*s3.Client, error) {
	loads := []func(*config.LoadOptions) error{config.WithClientLogMode(0)}
	if c.Region != "" {
		loads = append(loads, config.WithRegion(c.Region))
	}
	if c.AccessKeyID != "" {
		loads = append(loads, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, "")))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loads...)
	if err != nil {
		return nil, fmt.Errorf("s3test: configuration: %w", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.PathStyle
	}), nil
}

// NewPrefix of the suite, under which a test writes, such as s4ql-test/s3store/20261119T101500-ABCDEFGH/.
// It is deleted with everything under it when the test ends, through client.
func NewPrefix(t testing.TB, client *s3.Client, bucket, suite string) string {
	t.Helper()
	if !suitePattern.MatchString(suite) {
		t.Fatalf("s3test: the name of the suite %q is not lower-case letters, digits, and dashes", suite)
	}
	prefix := fmt.Sprint(Root, suite, "/", time.Now().UTC().Format(timeLayout), "-", rand.Text()[:8], "/")
	t.Cleanup(func() {
		// The context of the test is done when its cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := DeletePrefix(ctx, client, bucket, prefix); err != nil {
			t.Error(err)
		}
	})
	return prefix
}

// DeletePrefix deletes every object under the prefix, which must have the form of a prefix of
// [NewPrefix]. It refuses any other prefix, and any key that the listing gives outside the prefix,
// before it deletes anything of them.
func DeletePrefix(ctx context.Context, client *s3.Client, bucket, prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("s3test: refused to delete %q, which is not a prefix of a test under %v", prefix, Root)
	}
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("s3test: list %v to delete it: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if !strings.HasPrefix(aws.ToString(o.Key), prefix) {
				return fmt.Errorf("s3test: refused to delete %q, which the listing of %v gave and which is not under it", aws.ToString(o.Key), prefix)
			}
		}
		for _, o := range page.Contents {
			if _, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(bucket), Key: o.Key}); err != nil {
				return fmt.Errorf("s3test: delete %v: %w", aws.ToString(o.Key), err)
			}
		}
	}
	return nil
}

// DeleteOld deletes every prefix of [NewPrefix] in the bucket whose time is before the given one,
// with everything under it, and gives the prefixes that it deleted. A key under [Root] that is in
// no such prefix is left as it is.
func DeleteOld(ctx context.Context, client *s3.Client, bucket string, before time.Time) ([]string, error) {
	suites, err := commonPrefixes(ctx, client, bucket, Root)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, suite := range suites {
		prefixes, err := commonPrefixes(ctx, client, bucket, suite)
		if err != nil {
			return deleted, err
		}
		for _, prefix := range prefixes {
			m := prefixPattern.FindStringSubmatch(prefix)
			if m == nil {
				continue
			}
			made, err := time.Parse(timeLayout, m[1])
			if err != nil || !made.Before(before) {
				continue
			}
			if err := DeletePrefix(ctx, client, bucket, prefix); err != nil {
				return deleted, err
			}
			deleted = append(deleted, prefix)
		}
	}
	return deleted, nil
}

// commonPrefixes directly under the prefix: the prefixes of the keys under it up to their next
// slash.
func commonPrefixes(ctx context.Context, client *s3.Client, bucket, prefix string) ([]string, error) {
	var prefixes []string
	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String(prefix), Delimiter: aws.String("/"),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3test: list %v: %w", prefix, err)
		}
		for _, p := range page.CommonPrefixes {
			prefixes = append(prefixes, aws.ToString(p.Prefix))
		}
	}
	return prefixes, nil
}
