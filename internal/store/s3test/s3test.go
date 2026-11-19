// Package s3test holds what the tests against a real bucket share: their settings, a prefix of each
// test's own that is deleted with everything under it when the test ends, and the deletion of the
// prefixes that runs which were killed left behind.
//
// Every key that a test writes is under [Root], in a prefix of the form
// s4ql-test/<suite>/<UTC time>-<8 random characters>/, which [NewPrefix] gives. Nothing deletes
// anything else: [DeletePrefix] refuses every prefix that does not have that form, and every key
// that a listing gives outside the prefix.
//
// # The settings
//
// [Load] gives the settings to a test. A test that needs the bucket skips under -short, and fails
// otherwise if a setting is missing, with what is missing. The settings are S4QL_S3_BUCKET,
// AWS_ENDPOINT_URL_S3, AWS_REGION, and S4QL_S3_PATH_STYLE=true for a store that needs the bucket in
// the path. Each one that the environment does not set, or sets empty, comes from the file .env.test
// at the root of the module, which is committed and names the bucket that development uses. The
// credentials, AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY, are the only secrets. They come as a pair
// from the environment if it sets one of them, and otherwise from the file .env at the root of the
// module, which is never committed. The files are read for these names only, as lines NAME=value
// as a shell reads plain values: with an optional export in front, quotes around the value, and a
// comment after it. No value is ever printed.
//
// # Running the tests against a real bucket
//
// go test ./... runs them with the other tests. CI's job test does, with the credentials from the
// repository's secrets. make test passes -short, so that it runs without credentials, and make
// test-s3 runs the packages that hold the tests against a real bucket, under the race detector. With
// the credentials in .env, nothing else is needed:
//
//	make test-s3
//
// The credentials need s3:GetObject, s3:PutObject, s3:ListBucket, and s3:DeleteObject.
//
// # The sweep
//
// A test deletes its prefix when it ends, and a run that was killed leaves it behind. So the first
// call of [Load] in a test process deletes, with [DeleteOld], every prefix under [Root] whose time is
// more than a day ago, and the test fails if that fails. A later call does it again only if no sweep
// has succeeded yet. Two processes that sweep at the same time may both delete the same prefix, and a
// key that the other process deleted first is no error, as a deletion in S3 of a missing key is none.
package s3test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
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
	// AccessKeyID and SecretAccessKey from AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
	AccessKeyID     string
	SecretAccessKey string
}

// sweeping is the sweep of the process: see the documentation of the package.
var sweeping once

// Load the settings of the real bucket for the test, from the environment and the files at the root
// of the module, as the documentation of the package says. It skips the test under -short, and fails
// it if a setting is missing or if the sweep fails.
func Load(t testing.TB) Config {
	t.Helper()
	if testing.Short() {
		t.Skip("-short skips the tests against a real bucket")
	}
	dir, err := moduleRoot()
	if err != nil {
		t.Fatal(err)
	}
	c, err := load(os.LookupEnv, dir)
	if err != nil {
		t.Fatal(err)
	}
	err = sweeping.do(func() error {
		// Bounded, so that a store that hangs fails the sweep, instead of holding every other test of
		// the process that waits for it until go test's timeout.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
		defer cancel()
		client, err := c.Client(ctx)
		if err != nil {
			return err
		}
		_, err = DeleteOld(ctx, client, c.Bucket, time.Now().Add(-24*time.Hour))
		return err
	})
	if err != nil {
		t.Fatalf("s3test: delete the prefixes under %v that are more than a day old: %v", Root, err)
	}
	return c
}

// load the settings with lookup, which is [os.LookupEnv] outside the tests of this package, and from
// the files .env.test and .env in dir. A name that lookup gives as empty is not set.
func load(lookup func(string) (string, bool), dir string) (Config, error) {
	env := func(name string) string {
		v, _ := lookup(name)
		return v
	}
	settingsFile, secretsFile := filepath.Join(dir, ".env.test"), filepath.Join(dir, ".env")
	settings, err := readEnvFile(settingsFile, "S4QL_S3_BUCKET", "AWS_ENDPOINT_URL_S3", "AWS_REGION", "S4QL_S3_PATH_STYLE")
	if err != nil {
		return Config{}, err
	}
	setting := func(name string) string {
		if v := env(name); v != "" {
			return v
		}
		return settings[name]
	}
	c := Config{
		Bucket:    setting("S4QL_S3_BUCKET"),
		Endpoint:  setting("AWS_ENDPOINT_URL_S3"),
		Region:    setting("AWS_REGION"),
		PathStyle: setting("S4QL_S3_PATH_STYLE") == "true",
	}
	var missing []string
	if c.Bucket == "" {
		missing = append(missing, "S4QL_S3_BUCKET is set neither in the environment nor in "+settingsFile)
	}

	// The credentials are a pair, so both come from one place.
	from := "the environment"
	c.AccessKeyID, c.SecretAccessKey = env("AWS_ACCESS_KEY_ID"), env("AWS_SECRET_ACCESS_KEY")
	if c.AccessKeyID == "" && c.SecretAccessKey == "" {
		from = secretsFile
		secrets, err := readEnvFile(secretsFile, "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
		if err != nil {
			return Config{}, err
		}
		c.AccessKeyID, c.SecretAccessKey = secrets["AWS_ACCESS_KEY_ID"], secrets["AWS_SECRET_ACCESS_KEY"]
	}
	switch {
	case c.AccessKeyID == "" && c.SecretAccessKey == "":
		missing = append(missing, "AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY are set neither in the environment nor in "+secretsFile)
	case c.AccessKeyID == "":
		missing = append(missing, fmt.Sprint("AWS_ACCESS_KEY_ID is not set in ", from, ", which sets AWS_SECRET_ACCESS_KEY"))
	case c.SecretAccessKey == "":
		missing = append(missing, fmt.Sprint("AWS_SECRET_ACCESS_KEY is not set in ", from, ", which sets AWS_ACCESS_KEY_ID"))
	}

	if len(missing) > 0 {
		return Config{}, fmt.Errorf("s3test: the tests against a real bucket cannot run: %v; run go test with -short to skip these tests, "+
			"and see the documentation of internal/store/s3test", strings.Join(missing, ", and "))
	}
	return c, nil
}

// readEnvFile gives the values of the names in the file at path, which has lines NAME=value. A line
// may have export in front. A value in single or double quotes is what is between them; any other
// value ends before a # that follows a space, which starts a comment. Every other line is passed
// over, and the last line of a name wins. A missing file has no values. An error never holds a value.
func readEnvFile(path string, names ...string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("s3test: read %v: %w", path, err)
	}
	values := map[string]string{}
	for line := range strings.Lines(string(b)) {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "export"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			line = rest
		}
		name, value, ok := strings.Cut(line, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		if !ok || !slices.Contains(names, name) {
			continue
		}
		if value != "" && (value[0] == '"' || value[0] == '\'') {
			if end := strings.IndexByte(value[1:], value[0]); end >= 0 {
				value = value[1 : end+1]
			}
		} else {
			// A comment after the value starts with a # after a space, as in a shell.
			for i := 1; i < len(value); i++ {
				if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
					value = strings.TrimSpace(value[:i])
					break
				}
			}
		}
		values[name] = value
	}
	return values, nil
}

// moduleRoot is the nearest directory at or above the working directory whose go.mod is that of the
// module maragu.dev/s4ql. A test runs in the directory of its package.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("s3test: find the root of the module: %w", err)
	}
	for dir := wd; ; {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for line := range bytes.Lines(b) {
				if string(bytes.TrimSpace(line)) == "module maragu.dev/s4ql" {
					return dir, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("s3test: no go.mod of the module maragu.dev/s4ql at or above %v", wd)
		}
		dir = parent
	}
}

// once runs a function until it has succeeded once.
type once struct {
	mu   sync.Mutex
	done bool
}

// do f, unless it has succeeded before. A call waits for one that runs f.
func (o *once) do(f func() error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.done {
		return nil
	}
	if err := f(); err != nil {
		return err
	}
	o.done = true
	return nil
}

// Client of the SDK for the bucket, for what a test does next to the code under test, such as
// deleting, with the options of the configuration and then those of optFns. It takes the credentials
// of the configuration if it has them, and otherwise the default chain's, and logs nothing.
func (c Config) Client(ctx context.Context, optFns ...func(*s3.Options)) (*s3.Client, error) {
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
	return s3.NewFromConfig(cfg, append([]func(*s3.Options){func(o *s3.Options) {
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
		o.UsePathStyle = c.PathStyle
	}}, optFns...)...), nil
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
