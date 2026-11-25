package traffic

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/storage"
)

func TestPGStoreDedupQueryAndBody(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v)", err)
	}
	probe, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := probe.Ping(); err != nil {
		probe.Close()
		t.Skipf("postgres unavailable (%v)", err)
	}
	probe.Close()
	pg, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pg.Close() })

	blobs, err := storage.NewLocalFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewPGStore(pg.DB, blobs)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [4]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	host := "pgstore-" + hex.EncodeToString(nonce[:]) + ".example"
	ctx := t.Context()
	t.Cleanup(func() {
		if _, err := store.DeleteHost(context.Background(), host); err != nil {
			t.Error(err)
		}
	})

	shared := []byte("req-body-" + host)
	first := ExchangeWrite{
		ID: "ex-" + host + "-1", TaskID: "task-" + host, ProjectID: "proj-" + host,
		TS: time.Date(2026, 11, 25, 1, 2, 3, 0, time.UTC), Host: host, Method: "GET",
		URLTemplate: "/v1/items", URL: "https://" + host + "/v1/items",
		Status: 201, ContentType: "application/json",
		ReqHead: "GET /v1/items", RespHead: "HTTP 201",
		ReqBody: shared, RespBody: []byte("one"),
	}
	second := first
	second.ID = "ex-" + host + "-2"
	second.Status = 404
	second.RespHead = "HTTP 404"
	second.RespBody = []byte("missing")
	second.TS = first.TS.Add(time.Second)
	if err := store.InsertExchange(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertExchange(ctx, second); err != nil {
		t.Fatal(err)
	}

	a, err := store.GetExchange(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.GetExchange(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if a.ReqBlobKey == "" || a.ReqBlobKey != b.ReqBlobKey {
		t.Fatalf("request keys %q %q", a.ReqBlobKey, b.ReqBlobKey)
	}
	if a.RespBlobKey == b.RespBlobKey {
		t.Fatal("distinct responses collapsed")
	}
	info, err := blobs.Stat(ctx, a.ReqBlobKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(shared)) {
		t.Fatalf("blob size %d", info.Size)
	}
	n, err := store.BlobRefCount(ctx, a.ReqBlobKey)
	if err != nil || n != 2 {
		t.Fatalf("refcount %d %v", n, err)
	}
	if !bytes.Equal(a.ReqBody, shared) || !bytes.Equal(b.ReqBody, shared) || string(a.RespBody) != "one" || string(b.RespBody) != "missing" {
		t.Fatalf("bodies a=%q/%q b=%q/%q", a.ReqBody, a.RespBody, b.ReqBody, b.RespBody)
	}

	rows, total, err := store.ListExchanges(ctx, ListQuery{
		Host: host, Method: "get", Path: "/v1/items", Status: "201",
		TaskID: first.TaskID, ProjectID: first.ProjectID,
		Page: 0, Size: 1, RespMin: -1, RespMax: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != first.ID || rows[0].Status != 201 {
		t.Fatalf("filtered total=%d rows=%v", total, rows)
	}
	page, total, err := store.ListExchanges(ctx, ListQuery{Host: host, Page: 0, Size: 1, RespMin: -1, RespMax: -1, Sort: "ts", Order: "asc"})
	if err != nil || total != 2 || len(page) != 1 || page[0].ID != first.ID {
		t.Fatalf("page0 total=%d rows=%v err=%v", total, page, err)
	}
	page, _, err = store.ListExchanges(ctx, ListQuery{Host: host, Page: 1, Size: 1, RespMin: -1, RespMax: -1, Sort: "ts", Order: "asc"})
	if err != nil || len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("page1 rows=%v err=%v", page, err)
	}
	stats, err := store.TemplateStats(ctx, host)
	if err != nil || len(stats) != 1 || stats[0].Count != 2 || stats[0].URLTemplate != "/v1/items" {
		t.Fatalf("templates %#v %v", stats, err)
	}

	if err := store.DeleteExchange(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = store.BlobRefCount(ctx, a.ReqBlobKey); err != nil || n != 1 {
		t.Fatalf("refcount after one delete %d %v", n, err)
	}
	if _, err := blobs.Stat(ctx, a.ReqBlobKey); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Stat(ctx, a.RespBlobKey); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("first response blob: %v", err)
	}
	if err := store.DeleteExchange(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if n, err = store.BlobRefCount(ctx, a.ReqBlobKey); err != nil || n != 0 {
		t.Fatalf("refcount after both deletes %d %v", n, err)
	}
	if _, err := blobs.Stat(ctx, a.ReqBlobKey); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("shared request blob: %v", err)
	}
}
