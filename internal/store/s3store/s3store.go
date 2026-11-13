// Package s3store is a [store.Store] on Amazon S3 and on stores that are compatible with it, through
// the AWS SDK for Go, version 2.
//
// A write is sent once, and only once. The SDK retries nothing, the HTTP transport speaks HTTP/1.1
// and follows no redirect, and a request that went out more than once anyway, which only a retry of
// net/http on a reused connection could cause, is never reported as one of the four errors of
// [store.Store]. A refusal of such a second attempt could be caused by the first.
//
// A write with a checksum is checked by the store: the request is signed with the checksum as the
// SHA-256 of its payload, which the store compares with the bytes it receives before it stores
// anything, and the checksum also goes with the object as its x-amz-checksum-sha256.
// AWS documents that S3 checks both. Tigris checks the first and stores the second without
// checking it.
//
// A read asks the store for the checksum that it holds, and the SDK compares it with the content
// as the content is read, so that content that does not match it fails at its end.
package s3store

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"maragu.dev/s4ql/internal/store"
)

// MaxSize of the content of one write: 5 GiB, the most that S3 takes in one PutObject. A larger
// write needs a multipart upload, which the client does not make.
const MaxSize = 5 << 30

// ErrTooLarge is wrapped by the error of a write whose content is larger than [MaxSize]. The
// client sent nothing.
var ErrTooLarge = errors.New("the content is larger than one write can take")

// Options of a [Client].
type Options struct {
	// Bucket that holds the objects. It is required.
	Bucket string

	// Endpoint of the store, such as https://t3.storage.dev for Tigris. If it is empty, the SDK's
	// default chain gives it, from AWS_ENDPOINT_URL_S3 or the shared configuration, and otherwise
	// it is AWS S3 in the region.
	Endpoint string

	// Region of the bucket, such as eu-north-1, or auto for Tigris. If it is empty, the SDK's
	// default chain gives it, from AWS_REGION or the shared configuration.
	Region string

	// PathStyle puts the bucket into the path of each request instead of into the host name, which
	// some stores need.
	PathStyle bool

	// RootCAs that the store's certificate must chain to. If it is nil, the system's roots are
	// used.
	RootCAs *x509.CertPool

	// Logger receives one event for each request to the store. If it is nil, the events are
	// discarded.
	Logger *slog.Logger
}

// Client of one bucket. Its credentials come from the SDK's default chain. It is safe for
// concurrent use.
type Client struct {
	s3     *s3.Client
	bucket string
	log    *slog.Logger
}

var _ store.Store = (*Client)(nil)

// New client with the given options. It sends no request. It needs the permissions s3:GetObject,
// s3:PutObject, and s3:ListBucket on the bucket: without s3:ListBucket, S3 answers a read of a
// missing object with 403 and not 404, which the client cannot tell from a refusal.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Bucket == "" {
		return nil, errors.New("s3store: no bucket")
	}

	// The transport has no timeout of its own: a call ends when its context does, and every
	// error of a write but the four means that its outcome is unknown anyway. It speaks HTTP/1.1
	// only, so that the retries of net/http that can happen are those of HTTP/1.1, and goes
	// through no proxy, which could send a request again.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	transport := &http.Transport{
		Protocols:             protocols,
		TLSClientConfig:       &tls.Config{RootCAs: opts.RootCAs, MinVersion: tls.VersionTLS12},
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	httpClient := &http.Client{
		Transport: transport,
		// A redirect is answered as it is, as an error, and not followed with a second request.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	loads := []func(*config.LoadOptions) error{
		config.WithHTTPClient(httpClient),
		config.WithLogger(logging.Nop{}),
		config.WithClientLogMode(0),
	}
	if opts.Region != "" {
		loads = append(loads, config.WithRegion(opts.Region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loads...)
	if err != nil {
		return nil, fmt.Errorf("s3store: configuration: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("s3store: no region, in the options or from the SDK's default chain")
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.PathStyle
		o.HTTPClient = httpClient
		// These come after the configuration, so that no setting of the environment, such as
		// AWS_MAX_ATTEMPTS, can turn retries on.
		o.Retryer = aws.NopRetryer{}
		o.RetryMaxAttempts = 0
		// The client sends its own checksum, and the SDK computes none.
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenSupported
		o.DisableLogOutputChecksumValidationSkipped = true
	})

	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Client{s3: client, bucket: opts.Bucket, log: log}, nil
}

// Get as [store.Store] describes. Content whose checksum the store holds is compared with it as it
// is read, and fails at its end if it does not match.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, store.Version, error) {
	r := c.start(ctx, opGet, key)
	if err := checkKey(key); err != nil {
		return nil, "", fmt.Errorf("s3store: get %q: %w", key, err)
	}
	out, err := c.s3.GetObject(r.ctx, &s3.GetObjectInput{
		Bucket:       aws.String(c.bucket),
		Key:          aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		r.failed(err)
		if r.status == http.StatusNotFound && r.code == "NoSuchKey" {
			err = r.end(outcomeNotFound, fmt.Errorf("s3store: get %q: %w", key, store.ErrNotFound))
			return nil, "", err
		}
		return nil, "", r.end(outcomeFailed, fmt.Errorf("s3store: get %q: %w", key, err))
	}
	r.succeeded(out.ResultMetadata)

	version := store.Version(aws.ToString(out.ETag))
	switch {
	case version == "":
		_ = out.Body.Close()
		return nil, "", r.end(outcomeUnexpected, fmt.Errorf("s3store: get %q: the answer has no ETag", key))
	case out.ContentLength == nil || *out.ContentLength < 0:
		_ = out.Body.Close()
		return nil, "", r.end(outcomeUnexpected, fmt.Errorf("s3store: get %q: the answer has no content length", key))
	}
	return &body{r: r, body: out.Body, size: *out.ContentLength}, version, nil
}

// Create as [store.Store] describes. It sends PutObject with If-None-Match: *.
func (c *Client) Create(ctx context.Context, key string, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return c.put(ctx, opCreate, key, "", content, checksum)
}

// Swap as [store.Store] describes. It sends PutObject with If-Match: from.
func (c *Client) Swap(ctx context.Context, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	return c.put(ctx, opSwap, key, from, content, checksum)
}

// put content at key, with If-None-Match: * for a create and If-Match: from for a swap.
func (c *Client) put(ctx context.Context, op, key string, from store.Version, content io.ReadSeeker, checksum *store.Checksum) (store.Version, error) {
	r := c.start(ctx, op, key)
	fail := func(err error) error { return fmt.Errorf("s3store: %v %q: %w", op, key, err) }
	if err := checkKey(key); err != nil {
		return "", fail(err)
	}
	if op == opSwap && from == "" {
		// An empty If-Match is "create only" on Tigris, so it is never sent.
		return "", fail(errors.New("a swap from an empty version"))
	}
	size, err := remaining(content)
	if err != nil {
		return "", fail(err)
	}
	if size > MaxSize {
		return "", fail(fmt.Errorf("%v bytes: %w", size, ErrTooLarge))
	}
	r.bytes = size

	g := &guard{r: content}

	in := &s3.PutObjectInput{
		Bucket:        aws.String(c.bucket),
		Key:           aws.String(key),
		Body:          g,
		ContentLength: aws.Int64(size),
	}
	if op == opCreate {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(string(from))
	}
	var fns []func(*s3.Options)
	if checksum != nil {
		in.ChecksumSHA256 = aws.String(base64.StdEncoding.EncodeToString(checksum[:]))
		fns = append(fns, signPayload(hex.EncodeToString(checksum[:])))
	}

	out, err := c.s3.PutObject(r.ctx, in, fns...)
	// The transport may go on reading the body after the call has returned, and must then not
	// reach the caller's content, which the caller may use again at once.
	g.close()
	if err != nil {
		r.failed(err)
		outcome, refusal := r.refusal()
		if refusal == nil {
			return "", r.end(outcomeUnknown, fail(fmt.Errorf("outcome unknown: %w", err)))
		}
		return "", r.end(outcome, fail(refusal))
	}
	r.succeeded(out.ResultMetadata)
	version := store.Version(aws.ToString(out.ETag))
	if version == "" {
		return "", r.end(outcomeUnexpected, fail(errors.New("outcome unknown: the answer has no ETag")))
	}
	_ = r.end(outcomeOK, nil)
	return version, nil
}

// refusal that the answer to a failed write of r shows, with its outcome, or a nil error if the
// answer does not show that the write was not applied.
func (r *request) refusal() (string, error) {
	if r.attempts.Load() > 1 {
		// The request went out more than once, and the first may have been applied.
		return "", nil
	}
	switch {
	case r.status == http.StatusPreconditionFailed && r.code == "PreconditionFailed":
		if r.op == opCreate {
			return outcomeExists, store.ErrExists
		}
		return outcomeChanged, store.ErrChanged
	case r.status == http.StatusConflict && r.code == "ConditionalRequestConflict" && r.op == opSwap:
		return outcomeChanged, store.ErrChanged
	case r.status == http.StatusNotFound && r.code == "NoSuchKey" && r.op == opSwap:
		return outcomeNotFound, store.ErrNotFound
	case r.status == http.StatusBadRequest && (r.code == "BadDigest" || r.code == "XAmzContentSHA256Mismatch"):
		return outcomeChecksum, store.ErrChecksum
	}
	return "", nil
}

// signPayload signs the request with hash as the SHA-256 of its payload, in hexadecimal, instead
// of one that the SDK computes. The store compares it with the payload that it receives.
func signPayload(hash string) func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(s *middleware.Stack) error {
			return s.Finalize.Insert(middleware.FinalizeMiddlewareFunc("s4qlPayloadHash",
				func(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
					return next.HandleFinalize(v4.SetPayloadHash(ctx, hash), in)
				}), "ComputePayloadHash", middleware.Before)
		})
	}
}

// remaining length of content from its current offset, which it is left at.
func remaining(content io.Seeker) (int64, error) {
	at, err := content.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, fmt.Errorf("offset of the content: %w", err)
	}
	end, err := content.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, fmt.Errorf("length of the content: %w", err)
	}
	if _, err := content.Seek(at, io.SeekStart); err != nil {
		return 0, fmt.Errorf("offset of the content: %w", err)
	}
	return end - at, nil
}

// checkKey against the rules of [store.Store] for a key.
func checkKey(key string) error {
	if len(key) == 0 || len(key) > 1024 {
		return fmt.Errorf("a key of %v bytes, not 1 to 1024", len(key))
	}
	if !utf8.ValidString(key) {
		return errors.New("a key that is not valid UTF-8")
	}
	for segment := range strings.SplitSeq(key, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("a key with the path segment %q", segment)
		}
	}
	return nil
}

// request to the store, as its event records it.
type request struct {
	c     *Client
	ctx   context.Context
	op    string
	key   string
	start time.Time
	// bytes of content that the request writes or reads.
	bytes int64
	// attempts at sending the request, counted by the HTTP transport: every time it wrote the
	// request's headers.
	attempts  atomic.Int32
	status    int
	code      string
	requestID string
}

// start a request from a call with the context ctx.
func (c *Client) start(ctx context.Context, op, key string) *request {
	r := &request{c: c, op: op, key: key, start: time.Now()}
	r.ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		WroteHeaders: func() { r.attempts.Add(1) },
	})
	return r
}

// failed records what the error of the SDK says about the answer.
func (r *request) failed(err error) {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		r.status = re.HTTPStatusCode()
		r.requestID = re.ServiceRequestID()
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		r.code = ae.ErrorCode()
	}
}

// succeeded records what the metadata of a successful answer says about it.
func (r *request) succeeded(m middleware.Metadata) {
	r.requestID, _ = awsmiddleware.GetRequestIDMetadata(m)
	if res, ok := awsmiddleware.GetRawResponse(m).(*smithyhttp.Response); ok {
		r.status = res.StatusCode
	}
}
