package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	maxZoneRunes      = 64
	ExecutionZoneName = "default"
)

// ErrExecutionLeaseLost means the worker no longer owns the job.
var ErrExecutionLeaseLost = errors.New("execution lease lost")

// NormalizeZone trims a zone name. An empty value is the default zone.
func NormalizeZone(zone string) (string, error) {
	zone = strings.TrimSpace(zone)
	if zone == "" {
		return ExecutionZoneName, nil
	}
	if utf8.RuneCountInString(zone) > maxZoneRunes {
		return "", fmt.Errorf("zone 最多 %d 个字符", maxZoneRunes)
	}
	for _, r := range zone {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return "", fmt.Errorf("zone 只允许字母、数字、连字符和下划线")
		}
	}
	return zone, nil
}

// WorkerNode is one registered artex-worker process.
type WorkerNode struct {
	ID            string    `json:"id"`
	Zone          string    `json:"zone"`
	Concurrency   int       `json:"concurrency"`
	ActiveJobs    int       `json:"active_jobs"`
	Status        string    `json:"status"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	StartedAt     time.Time `json:"started_at"`
}

// ExecutionJob is one queued or leased run of a task.
type ExecutionJob struct {
	ID          int64      `json:"id"`
	TaskID      int64      `json:"task_id"`
	ProjectID   string     `json:"project_id,omitempty"`
	Zone        string     `json:"zone"`
	Status      string     `json:"status"`
	LeaseOwner  string     `json:"lease_owner,omitempty"`
	LeaseUntil  *time.Time `json:"lease_until,omitempty"`
	Attempt     int        `json:"attempt"`
	MaxAttempts int        `json:"max_attempts"`
	LastError   string     `json:"last_error,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// ZoneJobStats counts claimable and in-flight jobs in one zone.
type ZoneJobStats struct {
	Zone    string `json:"zone"`
	Queued  int    `json:"queued"`
	Running int    `json:"running"`
}

const executionJobCols = `id, task_id, COALESCE(project_id,''), zone, status, COALESCE(lease_owner,''), lease_until, attempt, max_attempts, last_error, created_at, updated_at`

func scanExecutionJob(sc interface{ Scan(...any) error }) (*ExecutionJob, error) {
	var job ExecutionJob
	var lease sql.NullTime
	if err := sc.Scan(&job.ID, &job.TaskID, &job.ProjectID, &job.Zone, &job.Status, &job.LeaseOwner, &lease, &job.Attempt, &job.MaxAttempts, &job.LastError, &job.CreatedAt, &job.UpdatedAt); err != nil {
		return nil, err
	}
	if lease.Valid {
		job.LeaseUntil = &lease.Time
	}
	return &job, nil
}

// UpsertWorkerHeartbeat registers the worker or refreshes its liveness.
// started_at is kept from the first registration.
func (d *DB) UpsertWorkerHeartbeat(ctx context.Context, node WorkerNode) error {
	zone, err := NormalizeZone(node.Zone)
	if err != nil {
		return err
	}
	if node.ID == "" {
		return fmt.Errorf("worker id is empty")
	}
	if node.Concurrency < 1 {
		return fmt.Errorf("worker concurrency must be >= 1")
	}
	switch node.Status {
	case "online", "draining", "offline":
	default:
		return fmt.Errorf("worker status %q is invalid", node.Status)
	}
	started := node.StartedAt
	if started.IsZero() {
		started = time.Now()
	}
	_, err = d.ExecContext(ctx, `
INSERT INTO worker_nodes(id, zone, concurrency, active_jobs, status, last_heartbeat, started_at)
VALUES ($1,$2,$3,$4,$5,now(),$6)
ON CONFLICT (id) DO UPDATE SET
    zone=EXCLUDED.zone,
    concurrency=EXCLUDED.concurrency,
    active_jobs=EXCLUDED.active_jobs,
    status=EXCLUDED.status,
    last_heartbeat=now()`, node.ID, zone, node.Concurrency, node.ActiveJobs, node.Status, started)
	return err
}

// MarkStaleWorkersOffline flips workers whose heartbeat is older than the window.
func (d *DB) MarkStaleWorkersOffline(ctx context.Context, olderThan time.Duration) (int64, error) {
	seconds := int64(olderThan / time.Second)
	if seconds < 1 {
		return 0, fmt.Errorf("stale worker window must be at least 1s")
	}
	res, err := d.ExecContext(ctx, `
UPDATE worker_nodes
SET status='offline'
WHERE status IN ('online','draining')
  AND last_heartbeat < now() - ($1::bigint * interval '1 second')`, seconds)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListWorkerNodes returns every registered worker, oldest heartbeat last.
func (d *DB) ListWorkerNodes(ctx context.Context) ([]WorkerNode, error) {
	rows, err := d.QueryContext(ctx, `
SELECT id, zone, concurrency, active_jobs, status, last_heartbeat, started_at
FROM worker_nodes
ORDER BY zone, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkerNode
	for rows.Next() {
		var node WorkerNode
		if err := rows.Scan(&node.ID, &node.Zone, &node.Concurrency, &node.ActiveJobs, &node.Status, &node.LastHeartbeat, &node.StartedAt); err != nil {
			return nil, err
		}
		out = append(out, node)
	}
	return out, rows.Err()
}

// EnqueueExecutionJob inserts a queued job, or returns the job already active
// for the task. A paused job is returned to queued so resume can run again.
func (d *DB) EnqueueExecutionJob(ctx context.Context, taskID int64, projectID, zone string) (*ExecutionJob, error) {
	if taskID <= 0 {
		return nil, fmt.Errorf("task id must be positive")
	}
	zone, err := NormalizeZone(zone)
	if err != nil {
		return nil, err
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `
SELECT `+executionJobCols+`
FROM execution_jobs
WHERE task_id=$1 AND status IN ('queued','leased','running','paused')
ORDER BY id
FOR UPDATE`, taskID)
	job, err := scanExecutionJob(row)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if job.Status == "paused" {
			job, err = scanExecutionJob(tx.QueryRowContext(ctx, `
UPDATE execution_jobs
SET status='queued', zone=$2, project_id=COALESCE(NULLIF($3,''), project_id),
    lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE id=$1
RETURNING `+executionJobCols, job.ID, zone, projectID))
			if err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return job, nil
	}
	job, err = scanExecutionJob(tx.QueryRowContext(ctx, `
INSERT INTO execution_jobs(task_id, project_id, zone, status)
VALUES ($1, NULLIF($2,''), $3, 'queued')
RETURNING `+executionJobCols, taskID, projectID, zone))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback()
			return d.activeExecutionJob(ctx, taskID)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return job, nil
}

func (d *DB) activeExecutionJob(ctx context.Context, taskID int64) (*ExecutionJob, error) {
	job, err := scanExecutionJob(d.QueryRowContext(ctx, `
SELECT `+executionJobCols+`
FROM execution_jobs
WHERE task_id=$1 AND status IN ('queued','leased','running','paused')
ORDER BY id
LIMIT 1`, taskID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("execution job for task %d disappeared during enqueue", taskID)
	}
	return job, err
}

// ClaimExecutionJob atomically leases the oldest queued job in zone.
// The claim lands in running: the caller is the process that executes it.
func (d *DB) ClaimExecutionJob(ctx context.Context, workerID, zone string, leaseTTL time.Duration) (*ExecutionJob, error) {
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
	job, err := scanExecutionJob(d.QueryRowContext(ctx, `
WITH picked AS (
    SELECT id
    FROM execution_jobs
    WHERE zone=$1 AND status='queued'
    ORDER BY created_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE execution_jobs AS job
SET status='running',
    lease_owner=$2,
    lease_until=now() + ($3::bigint * interval '1 second'),
    attempt=job.attempt+1,
    updated_at=now()
FROM picked
WHERE job.id=picked.id
RETURNING `+executionJobReturning, zone, workerID, seconds))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return job, err
}

const executionJobReturning = `job.id, job.task_id, COALESCE(job.project_id,''), job.zone, job.status, COALESCE(job.lease_owner,''), job.lease_until, job.attempt, job.max_attempts, job.last_error, job.created_at, job.updated_at`

// RenewExecutionJobLease extends a lease still held by workerID.
func (d *DB) RenewExecutionJobLease(ctx context.Context, id int64, workerID string, leaseTTL time.Duration) error {
	seconds := int64(leaseTTL / time.Second)
	if seconds < 1 {
		return fmt.Errorf("lease ttl must be at least 1s")
	}
	res, err := d.ExecContext(ctx, `
UPDATE execution_jobs
SET lease_until=now() + ($3::bigint * interval '1 second'), updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status IN ('leased','running') AND lease_until > now()`, id, workerID, seconds)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrExecutionLeaseLost
	}
	return nil
}

// CompleteExecutionJob marks a job the worker finished successfully.
func (d *DB) CompleteExecutionJob(ctx context.Context, id int64, workerID string) error {
	return d.finishExecutionJob(ctx, id, workerID, "done", "")
}

// FailExecutionJob records lastError. Attempts still under the cap return to
// queued; the rest become failed.
func (d *DB) FailExecutionJob(ctx context.Context, id int64, workerID, lastError string) error {
	res, err := d.ExecContext(ctx, `
UPDATE execution_jobs
SET status=CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'queued' END,
    last_error=$3,
    lease_owner=NULL,
    lease_until=NULL,
    updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status IN ('leased','running')`, id, workerID, lastError)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrExecutionLeaseLost
	}
	return nil
}

func (d *DB) finishExecutionJob(ctx context.Context, id int64, workerID, status, lastError string) error {
	res, err := d.ExecContext(ctx, `
UPDATE execution_jobs
SET status=$3, last_error=CASE WHEN $4='' THEN last_error ELSE $4 END,
    lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE id=$1 AND lease_owner=$2 AND status IN ('leased','running')`, id, workerID, status, lastError)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrExecutionLeaseLost
	}
	return nil
}

// PauseExecutionJobByTask parks every active job for the task.
func (d *DB) PauseExecutionJobByTask(ctx context.Context, taskID int64) error {
	_, err := d.ExecContext(ctx, `
UPDATE execution_jobs
SET status='paused', lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE task_id=$1 AND status IN ('queued','leased','running')`, taskID)
	return err
}

// CancelExecutionJobByTask stops every job that has not reached a terminal state.
func (d *DB) CancelExecutionJobByTask(ctx context.Context, taskID int64) error {
	_, err := d.ExecContext(ctx, `
UPDATE execution_jobs
SET status='canceled', lease_owner=NULL, lease_until=NULL, updated_at=now()
WHERE task_id=$1 AND status IN ('queued','leased','running','paused')`, taskID)
	return err
}

// ReclaimExpiredExecutionJobs returns expired leases to the queue, or fails
// them once max_attempts is exhausted. It reports how many jobs took each path.
func (d *DB) ReclaimExpiredExecutionJobs(ctx context.Context) (requeued, failed int, err error) {
	rows, err := d.QueryContext(ctx, `
UPDATE execution_jobs
SET status=CASE WHEN attempt >= max_attempts THEN 'failed' ELSE 'queued' END,
    last_error=CASE
        WHEN attempt >= max_attempts AND last_error='' THEN 'lease expired'
        ELSE last_error
    END,
    lease_owner=NULL,
    lease_until=NULL,
    updated_at=now()
WHERE status IN ('leased','running') AND lease_until IS NOT NULL AND lease_until < now()
RETURNING attempt >= max_attempts`)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var exhausted bool
		if err := rows.Scan(&exhausted); err != nil {
			return 0, 0, err
		}
		if exhausted {
			failed++
		} else {
			requeued++
		}
	}
	return requeued, failed, rows.Err()
}

// ExecutionJobActive reports whether the task has a job a worker may still run.
func (d *DB) ExecutionJobActive(ctx context.Context, taskID int64) (bool, error) {
	var on bool
	err := d.QueryRowContext(ctx, `
SELECT EXISTS(
    SELECT 1 FROM execution_jobs
    WHERE task_id=$1 AND status IN ('queued','leased','running')
)`, taskID).Scan(&on)
	return on, err
}

// RetargetExecutionJobZone moves jobs that have not been claimed yet.
func (d *DB) RetargetExecutionJobZone(ctx context.Context, taskID int64, zone string) error {
	zone, err := NormalizeZone(zone)
	if err != nil {
		return err
	}
	_, err = d.ExecContext(ctx, `
UPDATE execution_jobs SET zone=$2, updated_at=now()
WHERE task_id=$1 AND status IN ('queued','paused')`, taskID, zone)
	return err
}

// ExecutionZoneStats aggregates queued and in-flight jobs per zone.
func (d *DB) ExecutionZoneStats(ctx context.Context) ([]ZoneJobStats, error) {
	rows, err := d.QueryContext(ctx, `
SELECT zone,
       count(*) FILTER (WHERE status='queued'),
       count(*) FILTER (WHERE status IN ('leased','running'))
FROM execution_jobs
WHERE status IN ('queued','leased','running')
GROUP BY zone
ORDER BY zone`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ZoneJobStats
	for rows.Next() {
		var row ZoneJobStats
		if err := rows.Scan(&row.Zone, &row.Queued, &row.Running); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}
