package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBlobStores(t *testing.T) {
	t.Run("LocalFS", func(t *testing.T) {
		store, err := NewLocalFSStore(filepath.Join(t.TempDir(), "restricted"))
		if err != nil {
			t.Fatal(err)
		}
		testBlobStore(t, store)
	})
	t.Run("S3", func(t *testing.T) {
		var mu sync.Mutex
		objects := make(map[string][]byte)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			const key = "/minio/example-bucket/objects/%E6%8A%A5%E5%91%8A%20%2B%25.txt"
			if r.URL.EscapedPath() != key {
				t.Errorf("escaped path = %q, want %q", r.URL.EscapedPath(), key)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if got := r.Header.Get("X-Amz-Content-Sha256"); got != fmt.Sprintf("%x", sha256.Sum256(body)) {
				t.Errorf("payload hash = %q", got)
			}
			if stamp, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date")); err != nil {
				t.Error(err)
			} else if want := "AWS4-HMAC-SHA256 Credential=example-key/" + stamp.Format("20060102") +
				"/test-region/s3/aws4_request,SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature="; !strings.HasPrefix(r.Header.Get("Authorization"), want) {
				t.Errorf("unexpected Authorization header: %s", r.Header.Get("Authorization"))
			}
			switch r.Method {
			case http.MethodPut:
				objects[r.URL.Path] = body
			case http.MethodGet, http.MethodHead:
				data, exists := objects[r.URL.Path]
				if !exists {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
				if r.Method == http.MethodGet {
					if _, err := w.Write(data); err != nil {
						t.Error(err)
					}
				}
			case http.MethodDelete:
				delete(objects, r.URL.Path)
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected method %s", r.Method)
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		}))
		defer srv.Close()
		store, err := NewS3Store(S3Config{
			Endpoint: srv.URL + "/minio", Bucket: "example-bucket", Region: "test-region",
			AccessKey: "example-key", SecretKey: "example-secret",
		})
		if err != nil {
			t.Fatal(err)
		}
		testBlobStore(t, store)
	})
}

func testBlobStore(t *testing.T, store StreamBlobStore) {
	t.Helper()
	ctx := t.Context()
	const key = "objects/报告 +%.txt"
	for _, data := range [][]byte{[]byte("first"), {0, 255, 1}, {}} {
		if err := store.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
		got, err := store.Get(ctx, key)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("Get = %v, %v, want %v", got, err, data)
		}
		info, err := store.Stat(ctx, key)
		if err != nil || info.Size != int64(len(data)) || info.ModTime.IsZero() {
			t.Fatalf("Stat = %+v, %v", info, err)
		}
	}
	reader := strings.NewReader("skip stream")
	if _, err := reader.Seek(5, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := store.PutReader(ctx, key, reader); err != nil {
		t.Fatal(err)
	}
	stream, err := store.Open(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stream)
	if err := errors.Join(err, stream.Close()); err != nil || string(got) != "stream" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Get(cancelled, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Get = %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Get missing = %v", err)
	}
	if _, err := store.Stat(ctx, key); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat missing = %v", err)
	}
	if err := store.Delete(ctx, key); err != nil {
		t.Fatalf("Delete missing = %v", err)
	}
}

func TestLocalFSRejectsTraversalAndSymlinks(t *testing.T) {
	ctx := t.Context()
	outside := t.TempDir()
	root := filepath.Join(t.TempDir(), "restricted")
	store, err := NewLocalFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, "inner", []byte("inside")); err != nil {
		t.Fatal(err)
	}
	for key, target := range map[string]string{
		"file-link": secret,
		"dir-link":  outside,
		"relative":  "inner",
		"dangling":  filepath.Join(outside, "new"),
	} {
		if err := os.Symlink(target, filepath.Join(root, key)); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"", ".", "..", "../secret", "dir/../secret", "/secret", "dir//secret", `..\secret`, "C:secret",
		"file-link", "dir-link/secret", "dir-link/new", "relative", "dangling", "file-link/child"} {
		get, getErr := store.Get(ctx, key)
		_, statErr := store.Stat(ctx, key)
		putErr := store.Put(ctx, key, []byte("changed"))
		deleteErr := store.Delete(ctx, key)
		if len(get) != 0 || !errors.Is(getErr, fs.ErrInvalid) || !errors.Is(statErr, fs.ErrInvalid) ||
			!errors.Is(putErr, fs.ErrInvalid) || !errors.Is(deleteErr, fs.ErrInvalid) {
			t.Errorf("key %q: Get=%q (%v), Stat=%v, Put=%v, Delete=%v", key, get, getErr, statErr, putErr, deleteErr)
		}
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside file = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("outside file was created: %v", err)
	}
	if err := os.Rename(root, root+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, root); err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalFSStore(root); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("constructor accepted symlink root: %v", err)
	}
	if _, err := store.Get(ctx, "secret"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("existing store accepted replaced root: %v", err)
	}
}

func TestS3SignatureV4(t *testing.T) {
	cfg := S3Config{
		Endpoint: "https://s3.us-east-1.amazonaws.com", Bucket: "example-bucket",
		AccessKey: "example-key", SecretKey: "example-secret",
	}
	store, err := NewS3Store(cfg)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPut, cfg.Endpoint+"/example-bucket/a/%E6%8A%A5%E5%91%8A%20%2B%3F%25.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	store.sign(req, fmt.Sprintf("%x", sha256.Sum256([]byte("artifact"))), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	const want = "AWS4-HMAC-SHA256 Credential=example-key/20260102/us-east-1/s3/aws4_request," +
		"SignedHeaders=host;x-amz-content-sha256;x-amz-date,Signature=8f8964fbd2e0c094ee02013ec20d736644ac4c5bb84eb8f08e672d3e7a54de8f"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestS3DoesNotFollowRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("S3 request followed a redirect")
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	store, err := NewS3Store(S3Config{Endpoint: srv.URL, Bucket: "test", AccessKey: "test", SecretKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(t.Context(), "key"); err == nil || !strings.Contains(err.Error(), "HTTP 307") {
		t.Fatalf("Get after redirect = %v", err)
	}
}
