package s3test_test

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"maragu.dev/is"

	"maragu.dev/s4ql/internal/store/s3test"
)

// fake bucket "bucket", which answers ListObjectsV2 with a prefix and a delimiter, and DeleteObject,
// and records every request.
type fake struct {
	mu       sync.Mutex
	keys     map[string]bool
	requests []string
	// listing, if it is not nil, gives the keys of every listing instead of the bucket's.
	listing []string
}

func newFake(t *testing.T, keys ...string) (*fake, *s3.Client) {
	t.Helper()
	f := &fake{keys: map[string]bool{}}
	for _, k := range keys {
		f.keys[k] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		Retryer:      aws.NopRetryer{},
	})
	return f, client
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, _ = io.Copy(io.Discard, r.Body)
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	switch {
	case r.Method == http.MethodGet && (r.URL.Path == "/bucket" || r.URL.Path == "/bucket/"):
		q := r.URL.Query()
		prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
		f.requests = append(f.requests, fmt.Sprint("LIST ", prefix, " ", delimiter))
		f.list(w, prefix, delimiter)
	case r.Method == http.MethodDelete:
		f.requests = append(f.requests, "DELETE "+key)
		delete(f.keys, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		http.Error(w, "not in the fake", http.StatusNotImplemented)
	}
}

func (f *fake) list(w http.ResponseWriter, prefix, delimiter string) {
	type content struct {
		Key string
	}
	type commonPrefix struct {
		Prefix string
	}
	type result struct {
		XMLName        xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
		Name           string
		Prefix         string
		KeyCount       int
		IsTruncated    bool
		Contents       []content
		CommonPrefixes []commonPrefix
	}
	res := result{Name: "bucket", Prefix: prefix}
	keys := f.listing
	if keys == nil {
		keys = slices.Sorted(maps.Keys(f.keys))
	}
	seen := map[string]bool{}
	for _, k := range keys {
		if f.listing == nil && !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, delimiter); delimiter != "" && i >= 0 {
			p := prefix + rest[:i+len(delimiter)]
			if !seen[p] {
				seen[p] = true
				res.CommonPrefixes = append(res.CommonPrefixes, commonPrefix{Prefix: p})
			}
			continue
		}
		res.Contents = append(res.Contents, content{Key: k})
	}
	res.KeyCount = len(res.Contents) + len(res.CommonPrefixes)
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(res)
}

func (f *fake) has() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Keys(f.keys))
}

func (f *fake) sent() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func TestLoad(t *testing.T) {
	t.Run("should fail with ErrNoBucket without S4QL_S3_BUCKET, and on half a pair of credentials", func(t *testing.T) {
		t.Setenv("S4QL_S3_BUCKET", "")
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "")
		_, err := s3test.Load()
		is.Error(t, s3test.ErrNoBucket, err)

		t.Setenv("S4QL_S3_BUCKET", "bucket")
		c, err := s3test.Load()
		is.NotError(t, err)
		is.Equal(t, "bucket", c.Bucket)

		t.Setenv("AWS_ACCESS_KEY_ID", "key")
		_, err = s3test.Load()
		is.True(t, err != nil && !errors.Is(err, s3test.ErrNoBucket), fmt.Sprint(err))
		t.Setenv("AWS_ACCESS_KEY_ID", "")
		t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
		_, err = s3test.Load()
		is.True(t, err != nil && !errors.Is(err, s3test.ErrNoBucket), fmt.Sprint(err))
	})
}

func TestDeletePrefix(t *testing.T) {
	t.Run("should refuse a prefix that is not one of a test under s4ql-test/, and send nothing", func(t *testing.T) {
		for _, prefix := range []string{
			"", "/", "other/", "s4ql-test", "s4ql-test/", "s4ql-test/s3store/",
			"s4ql-test/s3store/20261119T101500-ABCDEFGH", "s4ql-test/s3store/20261119T101500-ABCDEFGH/db",
			"s4ql-test/s3store/20261119T101500-abcdefgh/", "s4ql-test/s3store/20261119T101500-ABCDEFG/",
			"s4ql-test/S3store/20261119T101500-ABCDEFGH/", "s4ql-test//20261119T101500-ABCDEFGH/",
			"s4ql-test/a/b/20261119T101500-ABCDEFGH/", "other/s4ql-test/s3store/20261119T101500-ABCDEFGH/",
			"s4ql-test/s3store/2026111T9101500-ABCDEFGH/", " s4ql-test/s3store/20261119T101500-ABCDEFGH/",
		} {
			f, client := newFake(t, "other/x", "s4ql-test/s3store/20261119T101500-ABCDEFGH/x")
			err := s3test.DeletePrefix(t.Context(), client, "bucket", prefix)
			is.True(t, err != nil && strings.Contains(err.Error(), "refused"), fmt.Sprint(prefix, ": ", err))
			is.Equal(t, 0, len(f.sent()), prefix)
			is.Equal(t, 2, len(f.has()), prefix)
		}
	})

	t.Run("should delete every key under the prefix, and nothing else", func(t *testing.T) {
		const prefix = "s4ql-test/s3store/20261119T101500-ABCDEFGH/"
		f, client := newFake(t, prefix+"db/manifest.json", prefix+"db/commits/1", prefix+"k",
			"s4ql-test/s3store/20261119T101500-ABCDEFGHI/k", "s4ql-test/s3store/x", "other/k")
		is.NotError(t, s3test.DeletePrefix(t.Context(), client, "bucket", prefix))
		is.EqualSlice(t, []string{"other/k", "s4ql-test/s3store/20261119T101500-ABCDEFGHI/k", "s4ql-test/s3store/x"}, f.has())
	})

	t.Run("should refuse a key of the listing that is not under the prefix, before it deletes anything", func(t *testing.T) {
		const prefix = "s4ql-test/s3store/20261119T101500-ABCDEFGH/"
		f, client := newFake(t, prefix+"k", "other/k")
		f.listing = []string{prefix + "k", "other/k"}
		err := s3test.DeletePrefix(t.Context(), client, "bucket", prefix)
		is.True(t, err != nil && strings.Contains(err.Error(), `refused to delete "other/k"`), fmt.Sprint(err))
		is.EqualSlice(t, []string{"other/k", prefix + "k"}, f.has())
		is.EqualSlice(t, []string{"LIST " + prefix + " "}, f.sent())
	})
}

func TestDeleteOld(t *testing.T) {
	t.Run("should delete the prefixes of tests whose time is before the given one, and nothing else", func(t *testing.T) {
		f, client := newFake(t,
			"s4ql-test/s3store/20261117T235959-ABCDEFGH/db/manifest.json",
			"s4ql-test/s3store/20261117T235959-ABCDEFGH/k",
			"s4ql-test/s4ql/20200101T000000-ZZZZ2222/db/manifest.json",
			// The time is the given one, so it is not before it.
			"s4ql-test/s4ql/20261118T000000-ABCDEFGH/k",
			"s4ql-test/s4ql/20261119T101500-ABCDEFGH/k",
			// Keys under the root that are in no prefix of a test.
			"s4ql-test/k",
			"s4ql-test/s3store/k",
			"s4ql-test/s3store/20200101T000000-ABCDEFGH",
			"s4ql-test/s3store/20200101T000000/k",
			"s4ql-test/s3store/20200101T000000-ABCDEFG/k",
			"s4ql-test/s3store/20201301T000000-ABCDEFGH/k",
			"s4ql-test/Suite/20200101T000000-ABCDEFGH/k",
			// Keys outside the root.
			"other/20200101T000000-ABCDEFGH/k",
			"s4ql-tests/s3store/20200101T000000-ABCDEFGH/k",
		)
		deleted, err := s3test.DeleteOld(t.Context(), client, "bucket", time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC))
		is.NotError(t, err)
		is.EqualSlice(t, []string{"s4ql-test/s3store/20261117T235959-ABCDEFGH/", "s4ql-test/s4ql/20200101T000000-ZZZZ2222/"}, deleted)
		is.EqualSlice(t, []string{
			"other/20200101T000000-ABCDEFGH/k",
			"s4ql-test/Suite/20200101T000000-ABCDEFGH/k",
			"s4ql-test/k",
			"s4ql-test/s3store/20200101T000000-ABCDEFG/k",
			"s4ql-test/s3store/20200101T000000-ABCDEFGH",
			"s4ql-test/s3store/20200101T000000/k",
			"s4ql-test/s3store/20201301T000000-ABCDEFGH/k",
			"s4ql-test/s3store/k",
			"s4ql-test/s4ql/20261118T000000-ABCDEFGH/k",
			"s4ql-test/s4ql/20261119T101500-ABCDEFGH/k",
			"s4ql-tests/s3store/20200101T000000-ABCDEFGH/k",
		}, f.has())
		for _, r := range f.sent() {
			is.True(t, strings.HasPrefix(r, "LIST s4ql-test/") || strings.HasPrefix(r, "DELETE s4ql-test/"), r)
		}
	})

	t.Run("should stop at a listing that gives a key outside a prefix, and delete none of it", func(t *testing.T) {
		f, client := newFake(t, "s4ql-test/s3store/20200101T000000-ABCDEFGH/k")
		// Every listing gives the same keys, whatever it asks for, so the listing of the prefix gives
		// a key under another prefix.
		f.listing = []string{"s4ql-test/s3store/20200101T000000-ABCDEFGH/k", "s4ql-test/s3store/20200101T000000-ZZZZZZZZ/k"}
		_, err := s3test.DeleteOld(t.Context(), client, "bucket", time.Now())
		is.True(t, err != nil && strings.Contains(err.Error(), "refused"), fmt.Sprint(err))
		is.EqualSlice(t, []string{"s4ql-test/s3store/20200101T000000-ABCDEFGH/k"}, f.has())
	})
}

func TestNewPrefix(t *testing.T) {
	t.Run("should give a prefix of the suite with the time of now, which DeleteOld recognizes, and delete it when the test ends", func(t *testing.T) {
		f, client := newFake(t)
		var prefix string
		t.Run("test", func(t *testing.T) {
			before := time.Now().UTC().Truncate(time.Second)
			prefix = s3test.NewPrefix(t, client, "bucket", "suite")
			after := time.Now().UTC()
			is.True(t, strings.HasPrefix(prefix, "s4ql-test/suite/"), prefix)
			made, err := time.Parse("20060102T150405", strings.TrimPrefix(prefix, "s4ql-test/suite/")[:15])
			is.NotError(t, err)
			is.True(t, !made.Before(before) && !made.After(after), prefix)

			f.mu.Lock()
			f.keys[prefix+"k"] = true
			f.mu.Unlock()
			deleted, err := s3test.DeleteOld(t.Context(), client, "bucket", made)
			is.NotError(t, err)
			is.Equal(t, 0, len(deleted))
			is.EqualSlice(t, []string{prefix + "k"}, f.has())
		})
		is.Equal(t, 0, len(f.has()), "the prefix is left after the test")

		f.mu.Lock()
		f.keys[prefix+"k"] = true
		f.mu.Unlock()
		deleted, err := s3test.DeleteOld(t.Context(), client, "bucket", time.Now().Add(time.Hour))
		is.NotError(t, err)
		is.EqualSlice(t, []string{prefix}, deleted)
	})
}

// TestDeleteOld_realBucket runs DeleteOld on a real bucket, with a prefix whose time is two days
// ago, and one of now. Another run may delete the old prefix at the same time, which is the
// outcome that the test checks anyway, and no run deletes the new one.
func TestDeleteOld_realBucket(t *testing.T) {
	c := s3test.FromEnv(t)
	client, err := c.Client(t.Context())
	is.NotError(t, err)

	fresh := s3test.NewPrefix(t, client, c.Bucket, "s3test")
	old := fmt.Sprint(s3test.Root, "s3test/", time.Now().UTC().Add(-48*time.Hour).Format("20060102T150405"), "-", strings.TrimPrefix(fresh, s3test.Root+"s3test/")[16:])
	t.Cleanup(func() {
		// In case the test fails before DeleteOld has run. The context of the test is done when its
		// cleanup runs.
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = s3test.DeletePrefix(ctx, client, c.Bucket, old)
	})
	for _, k := range []string{fresh + "k", old + "k", old + "db/manifest.json"} {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(k), Body: strings.NewReader("data")})
		is.NotError(t, err, k)
	}

	deleted, err := s3test.DeleteOld(t.Context(), client, c.Bucket, time.Now().Add(-24*time.Hour))
	is.NotError(t, err)
	t.Log("deleted:", deleted)
	for _, p := range deleted {
		is.True(t, p != fresh, "the new prefix is deleted")
	}
	for _, k := range []string{old + "k", old + "db/manifest.json"} {
		_, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(k)})
		_, missing := errors.AsType[*types.NotFound](err)
		is.True(t, missing, fmt.Sprint(k, " is left: ", err))
	}
	_, err = client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(fresh + "k")})
	is.NotError(t, err)
}
