package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	BatchKindAssetRecon    = "asset_recon"
	BatchKindFindingRetest = "finding_retest"
)

// ErrBatchLeaseLost means the worker no longer owns the shard.
var ErrBatchLeaseLost = errors.New("batch shard lease lost")

// BatchJob is one fan-out of targets sliced into batch_shards.
type BatchJob struct {
	ID              string          `json:"id"`
	ProjectID       string          `json:"project_id,omitempty"`
	Kind            string          `json:"kind"`
	Zone            string          `json:"zone"`
	Status          string          `json:"status"`
	ShardSize       int             `json:"shard_size"`
	TotalTargets    int             `json:"total_targets"`
	TotalShards     int             `json:"total_shards"`
	CompletedShards int             `json:"completed_shards"`
	FailedShards    int             `json:"failed_shards"`
	ReducedSummary  json.RawMessage `json:"reduced_summary"`
	CreatedBy       string          `json:"created_by,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

// BatchShard is one leased slice of a batch.
type BatchShard struct {
	ID          int64           `json:"id"`
	BatchID     string          `json:"batch_id"`
	ShardIndex  int             `json:"shard_index"`
	Zone        string          `json:"zone"`
	Targets     json.RawMessage `json:"targets"`
	Status      string          `json:"status"`
	LeaseOwner  string          `json:"lease_owner,omitempty"`
	LeaseUntil  *time.Time      `json:"lease_until,omitempty"`
	Attempt     int             `json:"attempt"`
	MaxAttempts int             `json:"max_attempts"`
	Result      json.RawMessage `json:"result"`
	LastError   string          `json:"last_error,omitempty"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// BatchCreate is the fan-out input. Targets are trimmed and de-duplicated
// before they are sliced into shards of ShardSize.
type BatchCreate struct {
	ProjectID string
	Kind      string
	Zone      string
	ShardSize int
	Targets   []string
	CreatedBy string
}

const batchJobCols = `id, COALESCE(project_id,''), kind, zone, status, shard_size, total_targets, total_shards, completed_shards, failed_shards, reduced_summary, created_by, created_at, updated_at`

const batchShardCols = `id, batch_id, shard_index, zone, targets, status, COALESCE(lease_owner,''), lease_until, attempt, max_attempts, result, last_error, updated_at`

func scanBatchJob(sc interface{ Scan(...any) error }) (*BatchJob, error) {
	var job BatchJob
	var summary []byte
	if err := sc.Scan(&job.ID, &job.ProjectID, &job.Kind, &job.Zone, &job.Status, &job.ShardSize, &job.TotalTargets, &job.TotalShards, &job.CompletedShards, &job.FailedShards, &summary, &job.CreatedBy, &job.CreatedAt, &job.UpdatedAt); err != nil {
		return nil, err
	}
	job.ReducedSummary = summary
	if len(job.ReducedSummary) == 0 {
		job.ReducedSummary = []byte(`{}`)
	}
	return &job, nil
}

func scanBatchShard(sc interface{ Scan(...any) error }) (*BatchShard, error) {
	var shard BatchShard
	var lease sql.NullTime
	var targets, result []byte
	if err := sc.Scan(&shard.ID, &shard.BatchID, &shard.ShardIndex, &shard.Zone, &targets, &shard.Status, &shard.LeaseOwner, &lease, &shard.Attempt, &shard.MaxAttempts, &result, &shard.LastError, &shard.UpdatedAt); err != nil {
		return nil, err
	}
	shard.Targets = targets
	shard.Result = result
	if lease.Valid {
		shard.LeaseUntil = &lease.Time
	}
	return &shard, nil
}

func newBatchID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "bat_" + hex.EncodeToString(buf[:]), nil
}

func dedupeBatchTargets(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		target := strings.TrimSpace(raw)
		if target == "" {
			continue
		}
		if _, ok := seen[target]; ok {
			continue
		}
		seen[target] = struct{}{}
		out = append(out, target)
	}
	return out
}

// CreateBatchJob writes the batch and its shards in one transaction.
func (d *DB) CreateBatchJob(ctx context.Context, in BatchCreate) (*BatchJob, error) {
	kind := strings.TrimSpace(in.Kind)
	if kind == "" {
		return nil, fmt.Errorf("kind is empty")
	}
	if in.ShardSize < 1 {
		return nil, fmt.Errorf("shard_size must be >= 1")
	}
	zone, err := NormalizeZone(in.Zone)
	if err != nil {
		return nil, err
	}
	targets := dedupeBatchTargets(in.Targets)
	if len(targets) == 0 {
		return nil, fmt.Errorf("targets is empty")
	}
	id, err := newBatchID()
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	totalShards := (len(targets) + in.ShardSize - 1) / in.ShardSize
	job, err := scanBatchJob(tx.QueryRowContext(ctx, `
INSERT INTO batch_jobs(id, project_id, kind, zone, status, shard_size, total_targets, total_shards, created_by)
VALUES ($1, NULLIF($2,''), $3, $4, 'queued', $5, $6, $7, $8)
RETURNING `+batchJobCols, id, strings.TrimSpace(in.ProjectID), kind, zone, in.ShardSize, len(targets), totalShards, in.CreatedBy))
	if err != nil {
		return nil, err
	}
	for index, start := 0, 0; start < len(targets); index, start = index+1, start+in.ShardSize {
		end := start + in.ShardSize
		if end > len(targets) {
			end = len(targets)
		}
		body, err := json.Marshal(targets[start:end])
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO batch_shards(batch_id, shard_index, zone, targets, status)
VALUES ($1,$2,$3,$4::jsonb,'queued')`, id, index, zone, string(body)); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

// GetBatchJob loads one batch. sql.ErrNoRows means it does not exist.
func (d *DB) GetBatchJob(ctx context.Context, id string) (*BatchJob, error) {
	return scanBatchJob(d.QueryRowContext(ctx, `SELECT `+batchJobCols+` FROM batch_jobs WHERE id=$1`, id))
}

// ClaimBatchShard leases the oldest queued shard in zone.
func (d *DB) ClaimBatchShard(ctx context.Context, workerID, zone string, leaseTTL time.Duration) (*BatchShard, error) {
	if workerID == "" {
		return nil, fmt.Errorf("worker id is empty")
	}
	zone, err := NormalizeZone(zone)
	if err != nil {
		return nil, err
	}
	seconds := int64(leaseTTL / time.Second)
	if seconds < 1 {
		return nil, fmt.Errorf("lease ttl must be at least 1s")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	shard, err := scanBatchShard(tx.QueryRowContext(ctx, `
WITH picked AS (
    SELECT id
    FROM batch_shards
    WHERE zone=$1 AND status='queued'
    ORDER BY id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE batch_shards AS shard
SET status='running',
    lease_owner=$2,
    lease_until=now() + ($3::bigint * interval '1 second'),
    attempt=shard.attempt+1,
    updated_at=now()
FROM picked
WHERE shard.id=picked.id
RETURNING shard.id, shard.batch_id, shard.shard_index, shard.zone, shard.targets, shard.status,
          COALESCE(shard.lease_owner,''), shard.lease_until, shard.attempt, shard.max_attempts,
          shard.result, shard.last_error, shard.updated_at`, zone, workerID, seconds))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE batch_jobs SET status='running', updated_at=now()
WHERE id=$1 AND status='queued'`, shard.BatchID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return shard, nil
}

// RenewBatchShardLease extends a lease still held by workerID.
func (d *DB) RenewBatchShardLease(ctx context.Context, id int64, workerID string, leaseTTL time.Duration) error {
	seconds := int64(leaseTTL / time.Second)
	if seconds < 1 {
		return fmt.Errorf("lease ttl must be at least 1s")
	}
	res, err := d.ExecContext(ctx, `
UPDATE batch_shards
SET lease_until=now() + ($3::bigint * interval '1 second'), updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status='running' AND lease_until > now()`, id, workerID, seconds)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBatchLeaseLost
	}
	return nil
}

// CompleteBatchShard marks the shard done and increments completed_shards.
func (d *DB) CompleteBatchShard(ctx context.Context, id int64, workerID string, result []byte) error {
	if !json.Valid(result) {
		return fmt.Errorf("batch shard result is not json")
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var batchID string
	err = tx.QueryRowContext(ctx, `
UPDATE batch_shards
SET status='done', result=$3::jsonb, lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status='running'
RETURNING batch_id`, id, workerID, string(result)).Scan(&batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchLeaseLost
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE batch_jobs SET completed_shards=completed_shards+1, updated_at=now() WHERE id=$1`, batchID); err != nil {
		return err
	}
	return tx.Commit()
}

// FailBatchShard requeues the shard until max_attempts, then counts a failure.
func (d *DB) FailBatchShard(ctx context.Context, id int64, workerID, lastError string) error {
	if lastError == "" {
		lastError = "batch shard failed"
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var batchID, status string
	err = tx.QueryRowContext(ctx, `
UPDATE batch_shards
SET status=CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'queued' END,
    last_error=$3,
    lease_owner=NULL,
    lease_until=NULL,
    updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status='running'
RETURNING batch_id, status`, id, workerID, lastError).Scan(&batchID, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBatchLeaseLost
	}
	if err != nil {
		return err
	}
	if status == "failed" {
		if _, err := tx.ExecContext(ctx, `
UPDATE batch_jobs SET failed_shards=failed_shards+1, updated_at=now() WHERE id=$1`, batchID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CancelBatchJob cancels shards that have not been claimed and finalizes the
// batch when every shard is now terminal.
func (d *DB) CancelBatchJob(ctx context.Context, id string) (*BatchJob, error) {
	if _, err := d.GetBatchJob(ctx, id); err != nil {
		return nil, err
	}
	if _, err := d.ExecContext(ctx, `
UPDATE batch_shards
SET status='canceled',
    lease_owner=NULL,
    lease_until=NULL,
    last_error=CASE WHEN last_error='' THEN 'canceled' ELSE last_error END,
    updated_at=now()
WHERE batch_id=$1 AND status='queued'`, id); err != nil {
		return nil, err
	}
	job, done, err := d.TryFinalizeBatchJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if done {
		return job, nil
	}
	return d.GetBatchJob(ctx, id)
}

// ReclaimExpiredBatchShards returns expired shard leases to the queue, or fails
// them once max_attempts is exhausted. Ready batches are finalized.
func (d *DB) ReclaimExpiredBatchShards(ctx context.Context) (requeued, failed int, err error) {
	rows, err := d.QueryContext(ctx, `
WITH expired AS (
    UPDATE batch_shards
    SET status=CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'queued' END,
        last_error=CASE
            WHEN attempt >= max_attempts AND last_error='' THEN 'lease expired'
            ELSE last_error
        END,
        lease_owner=NULL,
        lease_until=NULL,
        updated_at=now()
    WHERE status='running' AND lease_until IS NOT NULL AND lease_until < now()
    RETURNING batch_id, (attempt >= max_attempts) AS exhausted
),
bumped AS (
    UPDATE batch_jobs AS job
    SET failed_shards=job.failed_shards + src.n, updated_at=now()
    FROM (
        SELECT batch_id, count(*)::int AS n
        FROM expired
        WHERE exhausted
        GROUP BY batch_id
    ) AS src
    WHERE job.id=src.batch_id
    RETURNING job.id
)
SELECT batch_id, exhausted FROM expired`)
	if err != nil {
		return 0, 0, err
	}
	seen := map[string]struct{}{}
	var failedBatches []string
	for rows.Next() {
		var id string
		var exhausted bool
		if err := rows.Scan(&id, &exhausted); err != nil {
			rows.Close()
			return 0, 0, err
		}
		if exhausted {
			failed++
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				failedBatches = append(failedBatches, id)
			}
		} else {
			requeued++
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	ready, err := d.readyBatchIDs(ctx)
	if err != nil {
		return requeued, failed, err
	}
	for _, id := range ready {
		if _, ok := seen[id]; ok {
			continue
		}
		failedBatches = append(failedBatches, id)
	}
	for _, id := range failedBatches {
		if _, _, err := d.TryFinalizeBatchJob(ctx, id); err != nil {
			return requeued, failed, err
		}
	}
	return requeued, failed, nil
}

func (d *DB) readyBatchIDs(ctx context.Context) ([]string, error) {
	rows, err := d.QueryContext(ctx, `
SELECT id FROM batch_jobs
WHERE status IN ('queued','running','reducing')
  AND total_shards > 0
  AND completed_shards + failed_shards + (
        SELECT count(*) FROM batch_shards s
        WHERE s.batch_id=batch_jobs.id AND s.status='canceled'
      ) >= total_shards`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// TryFinalizeBatchJob merges shard results once every shard is terminal.
// The boolean is true when the batch is in a terminal status.
func (d *DB) TryFinalizeBatchJob(ctx context.Context, batchID string) (*BatchJob, bool, error) {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	job, err := scanBatchJob(tx.QueryRowContext(ctx, `
SELECT `+batchJobCols+` FROM batch_jobs WHERE id=$1 FOR UPDATE`, batchID))
	if err != nil {
		return nil, false, err
	}
	if batchTerminal(job.Status) {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return job, true, nil
	}
	var canceled, open int
	if err := tx.QueryRowContext(ctx, `
SELECT count(*) FILTER (WHERE status='canceled'),
       count(*) FILTER (WHERE status IN ('queued','running'))
FROM batch_shards WHERE batch_id=$1`, batchID).Scan(&canceled, &open); err != nil {
		return nil, false, err
	}
	if open > 0 || job.CompletedShards+job.FailedShards+canceled != job.TotalShards {
		if err := tx.Commit(); err != nil {
			return nil, false, err
		}
		return job, false, nil
	}
	rows, err := tx.QueryContext(ctx, `
SELECT result FROM batch_shards
WHERE batch_id=$1 AND status='done'
ORDER BY shard_index`, batchID)
	if err != nil {
		return nil, false, err
	}
	var results [][]byte
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, false, err
		}
		results = append(results, raw)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	rows.Close()
	summary, err := mergeBatchShardResults(results, job.CompletedShards, job.FailedShards, canceled)
	if err != nil {
		return nil, false, err
	}
	status := "done"
	if job.FailedShards > 0 {
		status = "failed"
	} else if job.CompletedShards == 0 {
		status = "canceled"
	}
	job, err = scanBatchJob(tx.QueryRowContext(ctx, `
UPDATE batch_jobs
SET status='reducing', reduced_summary=$2::jsonb, updated_at=now()
WHERE id=$1
RETURNING `+batchJobCols, batchID, string(summary)))
	if err != nil {
		return nil, false, err
	}
	job, err = scanBatchJob(tx.QueryRowContext(ctx, `
UPDATE batch_jobs
SET status=$2, updated_at=now()
WHERE id=$1
RETURNING `+batchJobCols, batchID, status))
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return job, true, nil
}

func batchTerminal(status string) bool {
	switch status {
	case "done", "failed", "canceled":
		return true
	default:
		return false
	}
}

func mergeBatchShardResults(results [][]byte, completed, failed, canceled int) ([]byte, error) {
	seen := map[string]struct{}{}
	items := make([]json.RawMessage, 0)
	for _, raw := range results {
		if len(raw) == 0 || string(raw) == "{}" {
			continue
		}
		var body struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("shard result: %w", err)
		}
		for _, item := range body.Items {
			key := batchItemKey(item)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			items = append(items, item)
		}
	}
	return json.Marshal(struct {
		Items           []json.RawMessage `json:"items"`
		CompletedShards int               `json:"completed_shards"`
		FailedShards    int               `json:"failed_shards"`
		CanceledShards  int               `json:"canceled_shards"`
	}{Items: items, CompletedShards: completed, FailedShards: failed, CanceledShards: canceled})
}

func batchItemKey(raw json.RawMessage) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) == nil {
		if target, ok := obj["target"]; ok {
			var value string
			if json.Unmarshal(target, &value) == nil && value != "" {
				return "target:" + value
			}
		}
		if id, ok := obj["finding_id"]; ok {
			return "finding:" + string(id)
		}
	}
	compact, err := json.Marshal(json.RawMessage(raw))
	if err == nil {
		return "json:" + string(compact)
	}
	return "json:" + string(raw)
}
