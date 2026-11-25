package db

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/storage"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestMonthlyPartitions(t *testing.T) {
	dsn, _, err := DSN()
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

	root, err := Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })

	var publicKind string
	if err := root.QueryRow(`SELECT relkind FROM pg_class WHERE oid = to_regclass('public.activity')`).Scan(&publicKind); err != nil {
		t.Fatal(err)
	}
	if publicKind != "r" {
		t.Fatalf("public.activity relkind %q before test", publicKind)
	}

	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	schema := "artex_part_" + hex.EncodeToString(nonce[:])
	if _, err := root.Exec(`CREATE SCHEMA ` + quoteIdent(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := root.Exec(`DROP SCHEMA IF EXISTS ` + quoteIdent(schema) + ` CASCADE`); err != nil {
			t.Error(err)
		}
	})

	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["search_path"] = schema + ",public"
	pool := stdlib.OpenDB(*cfg)
	t.Cleanup(func() { pool.Close() })
	part := &DB{DB: pool}

	var current string
	if err := part.QueryRow(`SELECT current_schema()`).Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != schema {
		t.Fatalf("current_schema %q", current)
	}

	if _, err := part.Exec(`
CREATE TABLE activity (
    id BIGSERIAL,
    created_at TIMESTAMPTZ NOT NULL,
    payload TEXT NOT NULL,
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := part.Exec(`
CREATE TABLE ent_audit (
    id TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    payload TEXT NOT NULL DEFAULT ''
)`); err != nil {
		t.Fatal(err)
	}
	if _, err := part.Exec(`
CREATE TABLE llm_records (
    id BIGSERIAL PRIMARY KEY,
    ts TIMESTAMPTZ,
    model TEXT,
    created_at TIMESTAMPTZ
)`); err != nil {
		t.Fatal(err)
	}

	ctx := t.Context()
	now := time.Date(2026, 11, 25, 2, 0, 0, 0, time.UTC)
	if err := part.EnsureMonthlyPartitions(ctx, now, 2); err != nil {
		t.Fatal(err)
	}
	if err := part.EnsureMonthlyPartitions(ctx, now, 2); err != nil {
		t.Fatal(err)
	}
	if n := childCount(t, part, "activity"); n != 3 {
		t.Fatalf("activity partitions %d, want 3", n)
	}
	if n := childCount(t, part, "ent_audit"); n != 0 {
		t.Fatalf("heap ent_audit grew %d partitions", n)
	}
	if n := childCount(t, part, "llm_records"); n != 0 {
		t.Fatalf("heap llm_records grew %d partitions", n)
	}
	var entKind string
	if err := part.QueryRow(`SELECT relkind FROM pg_class WHERE oid = to_regclass('ent_audit')`).Scan(&entKind); err != nil {
		t.Fatal(err)
	}
	if entKind != "r" {
		t.Fatalf("ent_audit relkind %q", entKind)
	}

	past := time.Date(2024, 1, 15, 0, 0, 0, 0, time.UTC)
	if err := part.EnsureMonthlyPartitions(ctx, past, 1); err != nil {
		t.Fatal(err)
	}
	if n := childCount(t, part, "activity"); n != 5 {
		t.Fatalf("activity partitions %d, want 5", n)
	}
	assertIndex(t, part, "activity_y2024m01")
	assertIndex(t, part, "activity_y2026m11")

	insertActivity := func(at time.Time, payload string) string {
		t.Helper()
		var rel string
		if err := part.QueryRow(`
INSERT INTO activity(created_at, payload) VALUES ($1, $2)
RETURNING tableoid::regclass::text`, at, payload).Scan(&rel); err != nil {
			t.Fatal(err)
		}
		return rel
	}
	jan1 := insertActivity(time.Date(2024, 1, 10, 1, 0, 0, 0, time.UTC), "alpha-1")
	jan2 := insertActivity(time.Date(2024, 1, 20, 1, 0, 0, 0, time.UTC), "alpha-2")
	nov := insertActivity(time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC), "live-nov")
	if !strings.Contains(jan1, "activity_y2024m01") || !strings.Contains(jan2, "activity_y2024m01") {
		t.Fatalf("january rows routed to %s and %s", jan1, jan2)
	}
	if !strings.Contains(nov, "activity_y2026m11") {
		t.Fatalf("november row routed to %s", nov)
	}

	blobs, err := storage.NewLocalFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := part.ArchiveExpiredPartitions(ctx, blobs, "activity", time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(dropped, ",") != "activity_y2024m01,activity_y2024m02" {
		t.Fatalf("dropped %v", dropped)
	}
	assertGone(t, part, "activity_y2024m01")
	assertGone(t, part, "activity_y2024m02")
	var live int
	if err := part.QueryRow(`SELECT count(*) FROM activity WHERE payload='live-nov'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("live rows %d", live)
	}
	lines := gunzipPartition(t, blobs, "cold-partitions/activity/activity_y2024m01.jsonl.gz")
	if len(lines) != 2 || !strings.Contains(lines[0], `"alpha-1"`) || !strings.Contains(lines[1], `"alpha-2"`) {
		t.Fatalf("january archive %#v", lines)
	}
	if got := gunzipPartition(t, blobs, "cold-partitions/activity/activity_y2024m02.jsonl.gz"); len(got) != 0 {
		t.Fatalf("february archive %#v", got)
	}

	if _, err := part.Exec(`
INSERT INTO llm_records(ts, model) VALUES
    ('2024-01-15 00:00:00+00', 'old'),
    ('2026-11-02 00:00:00+00', 'new')`); err != nil {
		t.Fatal(err)
	}
	if err := part.ConvertToMonthlyPartitions(ctx, "llm_records"); err != nil {
		t.Fatal(err)
	}
	var llmKind string
	if err := part.QueryRow(`SELECT relkind FROM pg_class WHERE oid = to_regclass('llm_records')`).Scan(&llmKind); err != nil {
		t.Fatal(err)
	}
	if llmKind != "p" {
		t.Fatalf("llm_records relkind %q", llmKind)
	}
	rows, err := part.Query(`
SELECT model, created_at = ts, tableoid::regclass::text
FROM llm_records ORDER BY model`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var model, rel string
		var same bool
		if err := rows.Scan(&model, &same, &rel); err != nil {
			t.Fatal(err)
		}
		if !same {
			t.Fatalf("%s created_at != ts", model)
		}
		got[model] = rel
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got["old"], "llm_records_y2024m01") || !strings.Contains(got["new"], "llm_records_y2026m11") {
		t.Fatalf("converted routing %#v", got)
	}
	var liveRel string
	if err := part.QueryRow(`
INSERT INTO llm_records(model, ts, created_at)
VALUES ('live', '2026-11-25 00:00:00+00', '2026-11-25 00:00:00+00')
RETURNING tableoid::regclass::text`).Scan(&liveRel); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(liveRel, "llm_records_y2026m11") {
		t.Fatalf("live llm row routed to %s", liveRel)
	}
	if err := part.EnsureMonthlyPartitions(ctx, now, 2); err != nil {
		t.Fatal(err)
	}

	if err := root.QueryRow(`SELECT relkind FROM pg_class WHERE oid = to_regclass('public.activity')`).Scan(&publicKind); err != nil {
		t.Fatal(err)
	}
	if publicKind != "r" {
		t.Fatalf("public.activity relkind %q after test", publicKind)
	}
}

func childCount(t *testing.T, d *DB, table string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`
SELECT count(*) FROM pg_inherits i
JOIN pg_class parent ON parent.oid = i.inhparent
WHERE parent.oid = to_regclass($1)`, table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func assertIndex(t *testing.T, d *DB, partition string) {
	t.Helper()
	var n int
	if err := d.QueryRow(`
SELECT count(*) FROM pg_indexes
WHERE schemaname = current_schema() AND tablename = $1 AND indexdef ILIKE '%created_at%'`, partition).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("%s has no created_at index", partition)
	}
}

func assertGone(t *testing.T, d *DB, partition string) {
	t.Helper()
	var gone bool
	if err := d.QueryRow(`SELECT to_regclass($1) IS NULL`, partition).Scan(&gone); err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Fatalf("%s still exists", partition)
	}
}

func gunzipPartition(t *testing.T, blobs *storage.LocalFSStore, key string) []string {
	t.Helper()
	raw, err := blobs.Get(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if err := zr.Close(); err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(string(body))
	if text == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("archive line %q: %v", line, err)
		}
		lines = append(lines, line)
	}
	return lines
}
