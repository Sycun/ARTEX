package traffic

import (
	"bytes"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestTrafficBackend(t *testing.T) {
	t.Setenv("ARTEX_TRAFFIC_STORE", "")
	if got, err := Backend("all"); err != nil || got != "sqlite" {
		t.Fatalf("all: %s %v", got, err)
	}
	if got, err := Backend(""); err != nil || got != "sqlite" {
		t.Fatalf("empty: %s %v", got, err)
	}
	if got, err := Backend("api"); err != nil || got != "pg" {
		t.Fatalf("api: %s %v", got, err)
	}
	if got, err := Backend("worker"); err != nil || got != "pg" {
		t.Fatalf("worker: %s %v", got, err)
	}
	t.Setenv("ARTEX_TRAFFIC_STORE", "sqlite")
	if got, err := Backend("api"); err != nil || got != "sqlite" {
		t.Fatalf("override sqlite: %s %v", got, err)
	}
	t.Setenv("ARTEX_TRAFFIC_STORE", "pg")
	if got, err := Backend("all"); err != nil || got != "pg" {
		t.Fatalf("override pg: %s %v", got, err)
	}
	t.Setenv("ARTEX_TRAFFIC_STORE", "clickhouse")
	if _, err := Backend("all"); err == nil {
		t.Fatal("invalid store accepted")
	}
}

func TestSQLiteStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(indexSchema); err != nil {
		t.Fatal(err)
	}
	store := NewSQLiteStore(db, dir)
	ctx := t.Context()
	when := time.Unix(1_700_000_000, 0)
	body := []byte(`{"n":1}`)
	ex := ExchangeWrite{
		ID: "e1", TS: when, Host: "app.example", Method: "POST",
		URLTemplate: "/v1/items/{id}", URL: "http://app.example/v1/items/9",
		Status: 201, ContentType: "application/json",
		ReqHead: "POST /v1/items/9", RespHead: "HTTP 201",
		ReqBody: body, RespBody: []byte("created"),
	}
	if err := store.InsertExchange(ctx, ex); err != nil {
		t.Fatal(err)
	}
	ex.ID = "e2"
	ex.URLTemplate = "/health"
	ex.Status = 204
	ex.RespBody = nil
	if err := store.InsertExchange(ctx, ex); err != nil {
		t.Fatal(err)
	}
	rows, total, err := store.ListExchanges(ctx, ListQuery{Host: "app.example", Status: "201", Page: 0, Size: 10, RespMin: -1, RespMax: -1})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(rows) != 1 || rows[0].ID != "e1" {
		t.Fatalf("list total=%d rows=%v", total, rows)
	}
	got, err := store.GetExchange(ctx, "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.ReqBody, body) || string(got.RespBody) != "created" {
		t.Fatalf("body req=%q resp=%q", got.ReqBody, got.RespBody)
	}
	stats, err := store.TemplateStats(ctx, "app.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("templates %#v", stats)
	}
	hosts, err := store.HostStats(ctx, []string{"app.example"})
	if err != nil || len(hosts) != 1 || hosts[0].Count != 2 {
		t.Fatalf("hosts %#v %v", hosts, err)
	}
	n, err := store.BlobRefCount(ctx, "unused")
	if err != nil || n != 0 {
		t.Fatalf("refs %d %v", n, err)
	}
	if err := store.DeleteExchange(ctx, "e2"); err != nil {
		t.Fatal(err)
	}
	if _, total, err = store.ListExchanges(ctx, ListQuery{Host: "app.example", Page: 0, Size: 10, RespMin: -1, RespMax: -1}); err != nil || total != 1 {
		t.Fatalf("after delete total=%d %v", total, err)
	}
}
