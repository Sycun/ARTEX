package db

import (
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/artex/storage"
)

const defaultMonthsAhead = 2

var (
	ledgerTables = []string{"activity", "ent_audit", "llm_records"}

	partitionNameRE  = regexp.MustCompile(`^(activity|ent_audit|llm_records)_y[0-9]{4}m[0-9]{2}$`)
	partitionBoundRE = regexp.MustCompile(`FROM \('([^']+)'\) TO \('([^']+)'\)`)
	sequenceNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)
)

type rowQueryer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// EnsureMonthlyPartitions prebuilds the current UTC month and monthsAhead
// following months for every ledger that is already declaratively partitioned.
// A heap (relkind r) is left untouched so an existing database keeps starting.
func (d *DB) EnsureMonthlyPartitions(ctx context.Context, now time.Time, monthsAhead int) error {
	if monthsAhead < 0 {
		monthsAhead = defaultMonthsAhead
	}
	now = now.UTC()
	for _, table := range ledgerTables {
		if err := d.ensureLedger(ctx, d.DB, table, now, monthsAhead); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) ensureLedger(ctx context.Context, q rowQueryer, table string, now time.Time, monthsAhead int) error {
	kind, err := relationKind(ctx, q, table)
	if err != nil {
		return err
	}
	if kind != "p" {
		return nil
	}
	start := monthStart(now)
	for i := 0; i <= monthsAhead; i++ {
		name, err := ensureMonth(ctx, q, table, start.AddDate(0, i, 0))
		if err != nil {
			return err
		}
		if name == "" {
			return fmt.Errorf("%s is not partitioned", table)
		}
	}
	return nil
}

// ConvertToMonthlyPartitions swaps one ordinary ledger for a monthly RANGE
// parent. The primary key becomes (id, created_at). Already-partitioned tables
// only receive missing future children.
func (d *DB) ConvertToMonthlyPartitions(ctx context.Context, table string) error {
	if err := knownLedger(table); err != nil {
		return err
	}
	kind, err := relationKind(ctx, d.DB, table)
	if err != nil {
		return err
	}
	if kind == "p" {
		return d.ensureLedger(ctx, d.DB, table, time.Now().UTC(), defaultMonthsAhead)
	}
	if kind != "r" {
		return fmt.Errorf("%s relkind %s", table, kind)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+quoteIdent(table)+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		return err
	}
	kind, err = relationKind(ctx, tx, table)
	if err != nil {
		return err
	}
	if kind == "p" {
		if err := tx.Commit(); err != nil {
			return err
		}
		return d.ensureLedger(ctx, d.DB, table, time.Now().UTC(), defaultMonthsAhead)
	}
	if kind != "r" {
		return fmt.Errorf("%s relkind %s", table, kind)
	}
	if err := prepareCreatedAt(ctx, tx, table); err != nil {
		return err
	}

	var minT, maxT sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT min(created_at), max(created_at) FROM `+quoteIdent(table)).Scan(&minT, &maxT); err != nil {
		return err
	}
	var seq sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT pg_get_serial_sequence($1, 'id')`, table).Scan(&seq); err != nil {
		return err
	}

	newName := table + "_partnew"
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(
		`CREATE TABLE %s (LIKE %s INCLUDING DEFAULTS INCLUDING IDENTITY INCLUDING GENERATED) PARTITION BY RANGE (created_at)`,
		quoteIdent(newName), quoteIdent(table))); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+quoteIdent(newName)+` ADD PRIMARY KEY (id, created_at)`); err != nil {
		return err
	}
	start := monthStart(time.Now())
	end := start.AddDate(0, defaultMonthsAhead, 0)
	if minT.Valid {
		if ms := monthStart(minT.Time); ms.Before(start) {
			start = ms
		}
	}
	if maxT.Valid {
		if me := monthStart(maxT.Time).AddDate(0, defaultMonthsAhead, 0); me.After(end) {
			end = me
		}
	}
	for m := start; !m.After(end); m = m.AddDate(0, 1, 0) {
		if _, err := ensureMonth(ctx, tx, newName, m); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s SELECT * FROM %s`, quoteIdent(newName), quoteIdent(table))); err != nil {
		return err
	}
	if seq.Valid && seq.String != "" {
		if !sequenceNameRE.MatchString(seq.String) {
			return fmt.Errorf("refusing sequence name %q", seq.String)
		}
		if _, err := tx.ExecContext(ctx, `ALTER SEQUENCE `+seq.String+` OWNED BY `+quoteIdent(newName)+`.id`); err != nil {
			return err
		}
	}
	oldName := table + "_partold"
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+quoteIdent(table)+` RENAME TO `+quoteIdent(oldName)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+quoteIdent(newName)+` RENAME TO `+quoteIdent(table)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+quoteIdent(oldName)); err != nil {
		return err
	}
	if err := renamePrimaryKey(ctx, tx, table); err != nil {
		return err
	}
	if err := createLedgerIndexes(ctx, tx, table); err != nil {
		return err
	}
	if table == "activity" {
		if err := restoreActivityFKs(ctx, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ArchiveExpiredPartitions writes each child whose range ends at or before
// cutoff to cold-partitions/<table>/<partition>.jsonl.gz and drops the child
// after Stat matches the uploaded object. Heaps are skipped.
func (d *DB) ArchiveExpiredPartitions(ctx context.Context, blobs storage.BlobStore, table string, cutoff time.Time) ([]string, error) {
	if err := knownLedger(table); err != nil {
		return nil, err
	}
	kind, err := relationKind(ctx, d.DB, table)
	if err != nil {
		return nil, err
	}
	if kind != "p" {
		return nil, nil
	}
	parts, err := expiredPartitions(ctx, d.DB, table, cutoff.UTC())
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return nil, nil
	}
	if blobs == nil {
		return nil, errors.New("blob store is nil")
	}
	var dropped []string
	for _, part := range parts {
		if err := exportPartition(ctx, d.DB, blobs, table, part); err != nil {
			return dropped, err
		}
		if _, err := d.ExecContext(ctx, `DROP TABLE `+quoteIdent(part)); err != nil {
			return dropped, fmt.Errorf("drop %s: %w", part, err)
		}
		dropped = append(dropped, part)
	}
	return dropped, nil
}

func knownLedger(table string) error {
	switch table {
	case "activity", "ent_audit", "llm_records":
		return nil
	default:
		return fmt.Errorf("not a ledger table: %s", table)
	}
}

func relationKind(ctx context.Context, q rowQueryer, table string) (string, error) {
	var kind sql.NullString
	err := q.QueryRowContext(ctx, `SELECT c.relkind::text FROM pg_class c WHERE c.oid = to_regclass($1)`, table).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !kind.Valid) {
		return "", fmt.Errorf("relation %s not found", table)
	}
	if err != nil {
		return "", err
	}
	return kind.String, nil
}

func ensureMonth(ctx context.Context, q rowQueryer, table string, month time.Time) (string, error) {
	var name sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT artex_ensure_month_partition($1, $2)`, table, month.UTC()).Scan(&name); err != nil {
		return "", fmt.Errorf("partition %s %s: %w", table, month.UTC().Format("2006-01"), err)
	}
	if !name.Valid {
		return "", nil
	}
	return name.String, nil
}

func prepareCreatedAt(ctx context.Context, q rowQueryer, table string) error {
	cols, err := columnSet(ctx, q, table)
	if err != nil {
		return err
	}
	if !cols["created_at"] {
		if table != "llm_records" || !cols["ts"] {
			return fmt.Errorf("%s has no created_at", table)
		}
		if _, err := q.ExecContext(ctx, `ALTER TABLE llm_records ADD COLUMN created_at TIMESTAMPTZ`); err != nil {
			return err
		}
	}
	if table == "llm_records" {
		if _, err := q.ExecContext(ctx, `UPDATE llm_records SET created_at = COALESCE(created_at, ts, now()) WHERE created_at IS NULL`); err != nil {
			return err
		}
	}
	ident := quoteIdent(table)
	if _, err := q.ExecContext(ctx, `ALTER TABLE `+ident+` ALTER COLUMN created_at SET DEFAULT now()`); err != nil {
		return err
	}
	if _, err := q.ExecContext(ctx, `ALTER TABLE `+ident+` ALTER COLUMN created_at SET NOT NULL`); err != nil {
		return err
	}
	return nil
}

func columnSet(ctx context.Context, q rowQueryer, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `
SELECT column_name FROM information_schema.columns
WHERE table_schema = current_schema() AND table_name = $1`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func renamePrimaryKey(ctx context.Context, q rowQueryer, table string) error {
	var con string
	err := q.QueryRowContext(ctx, `
SELECT conname FROM pg_constraint
WHERE conrelid = to_regclass($1) AND contype = 'p'`, table).Scan(&con)
	if err != nil {
		return err
	}
	want := table + "_pkey"
	if con == want {
		return nil
	}
	_, err = q.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE %s RENAME CONSTRAINT %s TO %s`,
		quoteIdent(table), quoteIdent(con), quoteIdent(want)))
	return err
}

type ledgerIndex struct {
	cols []string
	sql  string
}

func createLedgerIndexes(ctx context.Context, q rowQueryer, table string) error {
	cols, err := columnSet(ctx, q, table)
	if err != nil {
		return err
	}
	for _, idx := range ledgerIndexes(table) {
		if !hasAll(cols, idx.cols) {
			continue
		}
		if _, err := q.ExecContext(ctx, idx.sql); err != nil {
			return err
		}
	}
	return nil
}

func hasAll(cols map[string]bool, need []string) bool {
	for _, c := range need {
		if !cols[c] {
			return false
		}
	}
	return true
}

func ledgerIndexes(table string) []ledgerIndex {
	t := quoteIdent(table)
	switch table {
	case "activity":
		return []ledgerIndex{
			{[]string{"exploration_id", "node_id", "id"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_node ON %s (exploration_id, node_id, id)`, t)},
			{[]string{"exploration_id", "id"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_since ON %s (exploration_id, id)`, t)},
			{[]string{"exploration_id", "tool_use_id", "id", "kind"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_tool_call ON %s (exploration_id, tool_use_id, id) WHERE kind IN ('tool_use', 'tool_result')`, t)},
			{[]string{"exploration_id", "worker", "id"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_worker ON %s (exploration_id, worker, id)`, t)},
			{[]string{"exploration_id", "main_seg", "id", "worker"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_main_seg ON %s (exploration_id, main_seg, id) WHERE worker='mainagent'`, t)},
			{[]string{"exploration_id", "input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "kind"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_result_usage ON %s (exploration_id) INCLUDE (input_tokens, output_tokens, cache_read_tokens, cache_write_tokens) WHERE kind='result'`, t)},
			{[]string{"exploration_id", "created_at"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_act_latest ON %s (exploration_id, created_at DESC)`, t)},
		}
	case "ent_audit":
		return []ledgerIndex{
			{[]string{"project_id", "created_at"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_ent_audit_project_created ON %s (project_id, created_at DESC)`, t)},
			{[]string{"subject_id", "created_at", "action"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_ent_audit_report_decision ON %s (subject_id, created_at DESC) WHERE action IN ('report_issued','report_returned','report_withdrawn')`, t)},
		}
	case "llm_records":
		return []ledgerIndex{
			{[]string{"ts"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_llm_records_ts ON %s (ts)`, t)},
			{[]string{"session_id"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_llm_records_session ON %s (session_id)`, t)},
			{[]string{"created_at"}, fmt.Sprintf(`CREATE INDEX IF NOT EXISTS idx_llm_records_created ON %s (created_at)`, t)},
		}
	default:
		return nil
	}
}

func restoreActivityFKs(ctx context.Context, q rowQueryer) error {
	cols, err := columnSet(ctx, q, "activity")
	if err != nil {
		return err
	}
	var explorations, nodes sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT to_regclass('explorations')::text, to_regclass('exploration_nodes')::text`).Scan(&explorations, &nodes); err != nil {
		return err
	}
	if explorations.Valid && cols["exploration_id"] {
		if _, err := q.ExecContext(ctx, `
ALTER TABLE activity
    ADD CONSTRAINT activity_exploration_id_fkey
    FOREIGN KEY (exploration_id) REFERENCES explorations(id) ON DELETE CASCADE`); err != nil {
			return err
		}
	}
	if nodes.Valid && cols["node_id"] {
		if _, err := q.ExecContext(ctx, `
ALTER TABLE activity
    ADD CONSTRAINT activity_node_id_fkey
    FOREIGN KEY (node_id) REFERENCES exploration_nodes(id) ON DELETE SET NULL`); err != nil {
			return err
		}
	}
	return nil
}

func expiredPartitions(ctx context.Context, q rowQueryer, table string, cutoff time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, `
SELECT child.relname, pg_get_expr(child.relpartbound, child.oid)
FROM pg_inherits i
JOIN pg_class parent ON parent.oid = i.inhparent
JOIN pg_class child ON child.oid = i.inhrelid
WHERE parent.oid = to_regclass($1)
  AND child.relnamespace = parent.relnamespace
ORDER BY child.relname`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, bound string
		if err := rows.Scan(&name, &bound); err != nil {
			return nil, err
		}
		if !strings.HasPrefix(name, table+"_") || !partitionNameRE.MatchString(name) {
			return nil, fmt.Errorf("refusing partition %s", name)
		}
		m := partitionBoundRE.FindStringSubmatch(bound)
		if m == nil {
			if strings.Contains(strings.ToUpper(bound), "DEFAULT") {
				continue
			}
			return nil, fmt.Errorf("partition %s bound %q", name, bound)
		}
		upper, err := parsePGTimestamp(m[2])
		if err != nil {
			return nil, fmt.Errorf("partition %s bound %q: %w", name, bound, err)
		}
		if !upper.After(cutoff) {
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

func exportPartition(ctx context.Context, q rowQueryer, blobs storage.BlobStore, table, partition string) (err error) {
	if !strings.HasPrefix(partition, table+"_") || !partitionNameRE.MatchString(partition) {
		return fmt.Errorf("refusing to export %s", partition)
	}
	key := "cold-partitions/" + table + "/" + partition + ".jsonl.gz"
	rows, err := q.QueryContext(ctx, `SELECT row_to_json(t)::text FROM `+quoteIdent(partition)+` AS t ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	f, err := os.CreateTemp("", "artex-partition-*.jsonl.gz")
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, f.Close(), os.Remove(f.Name()))
	}()
	zw := gzip.NewWriter(f)
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		if _, err := io.WriteString(zw, line); err != nil {
			return err
		}
		if _, err := zw.Write([]byte{'\n'}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("archive %s is empty", key)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if stream, ok := blobs.(storage.StreamBlobStore); ok {
		if err := stream.PutReader(ctx, key, f); err != nil {
			return err
		}
	} else {
		body, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		if err := blobs.Put(ctx, key, body); err != nil {
			return err
		}
	}
	st, err := blobs.Stat(ctx, key)
	if err != nil {
		return err
	}
	if st.Size != info.Size() {
		return fmt.Errorf("archive %s stat size %d, wrote %d", key, st.Size, info.Size())
	}
	return nil
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func parsePGTimestamp(s string) (time.Time, error) {
	layouts := []string{
		"2006-01-02 15:04:05.999999-07",
		"2006-01-02 15:04:05.999999-07:00",
		"2006-01-02 15:04:05-07",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05Z07:00",
		time.RFC3339,
		time.RFC3339Nano,
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("timestamp %q", s)
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
