package storage

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
)

// BlobStore stores complete objects under slash-separated relative keys.
type BlobStore interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	Delete(ctx context.Context, key string) error
	Stat(ctx context.Context, key string) (BlobInfo, error)
}

// StreamBlobStore lets large objects use the same backend without buffering
// them in memory. S3 rewinds the reader after hashing the request payload.
type StreamBlobStore interface {
	BlobStore
	PutReader(ctx context.Context, key string, data io.ReadSeeker) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

type BlobInfo struct {
	Size    int64
	ModTime time.Time
}

func validateKey(key string) error {
	if key == "." || !fs.ValidPath(key) || strings.ContainsAny(key, "\\:\x00") {
		return fmt.Errorf("invalid blob key %q: %w", key, fs.ErrInvalid)
	}
	return nil
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}
