package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const s3RequestTimeout = 30 * time.Second

type S3Config struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// S3Store uses path-style requests for S3 and S3-compatible endpoints.
type S3Store struct {
	endpoint url.URL
	config   S3Config
	client   http.Client
}

func NewS3Store(cfg S3Config) (*S3Store, error) {
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
		endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || endpoint.Fragment != "" {
		return nil, fmt.Errorf("ARTEX_S3_ENDPOINT must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if cfg.Bucket == "" || strings.Contains(cfg.Bucket, "/") || validateKey(cfg.Bucket) != nil {
		return nil, fmt.Errorf("ARTEX_S3_BUCKET must be a bucket name")
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("ARTEX_S3_ACCESS_KEY and ARTEX_S3_SECRET_KEY are required")
	}
	if cfg.Region == "" {
		cfg.Region = "us-east-1"
	}
	return &S3Store{
		endpoint: *endpoint,
		config:   cfg,
		client: http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	return s.put(ctx, key, bytes.NewReader(data), int64(len(data)), fmt.Sprintf("%x", sha256.Sum256(data)))
}

func (s *S3Store) PutReader(ctx context.Context, key string, data io.ReadSeeker) error {
	offset, err := data.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, contextReader{ctx, data})
	if err != nil {
		return err
	}
	if _, err := data.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	// Keep ownership of the seekable source with the caller.
	return s.put(ctx, key, io.LimitReader(data, size), size, fmt.Sprintf("%x", hash.Sum(nil)))
}

func (s *S3Store) put(ctx context.Context, key string, data io.Reader, size int64, payloadHash string) error {
	resp, err := s.request(ctx, http.MethodPut, key, data, size, payloadHash)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	body, err := s.Open(ctx, key)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(body)
	if err := errors.Join(readErr, body.Close()); err != nil {
		return nil, err
	}
	return data, nil
}

func (s *S3Store) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.request(ctx, http.MethodGet, key, nil, 0, emptySHA256)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *S3Store) Delete(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	resp, err := s.request(ctx, http.MethodDelete, key, nil, 0, emptySHA256)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (s *S3Store) Stat(ctx context.Context, key string) (BlobInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, s3RequestTimeout)
	defer cancel()
	resp, err := s.request(ctx, http.MethodHead, key, nil, 0, emptySHA256)
	if err != nil {
		return BlobInfo{}, err
	}
	if err := resp.Body.Close(); err != nil {
		return BlobInfo{}, err
	}
	modTime, err := http.ParseTime(resp.Header.Get("Last-Modified"))
	if err != nil {
		return BlobInfo{}, fmt.Errorf("S3 HEAD %q: invalid Last-Modified: %w", key, err)
	}
	if resp.ContentLength < 0 {
		return BlobInfo{}, fmt.Errorf("S3 HEAD %q: missing Content-Length", key)
	}
	return BlobInfo{Size: resp.ContentLength, ModTime: modTime}, nil
}

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *S3Store) request(ctx context.Context, method, key string, data io.Reader, size int64, payloadHash string) (*http.Response, error) {
	if err := validateKey(key); err != nil {
		return nil, err
	}
	target := s.endpoint
	target.Path = strings.TrimRight(target.Path, "/") + "/" + s.config.Bucket + "/" + key
	parts := strings.Split(target.Path, "/")
	for i, part := range parts {
		parts[i] = strings.ReplaceAll(url.QueryEscape(part), "+", "%20")
	}
	target.RawPath = strings.Join(parts, "/")
	if size == 0 {
		data = nil
	}
	req, err := http.NewRequestWithContext(ctx, method, target.String(), data)
	if err != nil {
		return nil, err
	}
	req.ContentLength = size
	req.Header.Set("Accept-Encoding", "identity")
	s.sign(req, payloadHash, time.Now().UTC())
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}
	if err := resp.Body.Close(); err != nil {
		return nil, fmt.Errorf("S3 %s %q returned HTTP %d: %w", method, key, resp.StatusCode, err)
	}
	if resp.StatusCode == http.StatusNotFound && (method == http.MethodGet || method == http.MethodHead) {
		return nil, fmt.Errorf("S3 %s %q: %w", method, key, fs.ErrNotExist)
	}
	return nil, fmt.Errorf("S3 %s %q returned HTTP %d", method, key, resp.StatusCode)
}

func (s *S3Store) sign(req *http.Request, payloadHash string, now time.Time) {
	date := now.Format("20060102")
	stamp := now.Format("20060102T150405Z")
	req.Header.Set("X-Amz-Date", stamp)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	const signedHeaders = "host;x-amz-content-sha256;x-amz-date"
	canonical := strings.Join([]string{
		req.Method, req.URL.EscapedPath(), "",
		"host:" + req.Host,
		"x-amz-content-sha256:" + payloadHash,
		"x-amz-date:" + stamp, "",
		signedHeaders, payloadHash,
	}, "\n")
	scope := date + "/" + s.config.Region + "/s3/aws4_request"
	toSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%x", stamp, scope, sha256.Sum256([]byte(canonical)))
	key := s3HMAC([]byte("AWS4"+s.config.SecretKey), date)
	key = s3HMAC(key, s.config.Region)
	key = s3HMAC(key, "s3")
	key = s3HMAC(key, "aws4_request")
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s,SignedHeaders=%s,Signature=%x",
		s.config.AccessKey, scope, signedHeaders, s3HMAC(key, toSign)))
}

func s3HMAC(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}
