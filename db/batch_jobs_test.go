package db

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBatchFanOutClaimAndReduce(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	ctx := context.Background()
	zone := fmt.Sprintf("bzone-%d", time.Now().UnixNano())
	otherZone := zone + "-other"
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM batch_jobs WHERE zone=$1 OR zone=$2`, zone, otherZone)
	})

	job, err := d.CreateBatchJob(ctx, BatchCreate{
		Kind:      BatchKindAssetRecon,
		Zone:      zone,
		ShardSize: 2,
		Targets:   []string{" a ", "b", "a", "", "c", "b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" || job.TotalTargets != 3 || job.TotalShards != 2 || job.ShardSize != 2 {
		t.Fatalf("batch = %+v", job)
	}
	rows, err := d.QueryContext(ctx, `SELECT targets FROM batch_shards WHERE batch_id=$1 ORDER BY shard_index`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var slices [][]string
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		var targets []string
		if err := json.Unmarshal(raw, &targets); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		slices = append(slices, targets)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	if len(slices) != 2 || len(slices[0]) != 2 || slices[0][0] != "a" || slices[0][1] != "b" || len(slices[1]) != 1 || slices[1][0] != "c" {
		t.Fatalf("slices = %#v", slices)
	}

	if _, err := d.CreateBatchJob(ctx, BatchCreate{Kind: BatchKindAssetRecon, Zone: otherZone, ShardSize: 1, Targets: []string{"other"}}); err != nil {
		t.Fatal(err)
	}

	const workers = 6
	var wg sync.WaitGroup
	start := make(chan struct{})
	errCh := make(chan error, workers)
	var mu sync.Mutex
	var claimed []*BatchShard
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			shard, err := d.ClaimBatchShard(ctx, fmt.Sprintf("batch-worker-%d", i), zone, 30*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			if shard == nil {
				return
			}
			mu.Lock()
			claimed = append(claimed, shard)
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if len(claimed) != 2 {
		t.Fatalf("claimed %d shards", len(claimed))
	}
	seen := map[int64]int{}
	for _, shard := range claimed {
		seen[shard.ID]++
		if shard.Status != "running" || shard.Attempt != 1 || shard.LeaseOwner == "" || shard.Zone != zone {
			t.Fatalf("shard = %+v", shard)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("shard %d claimed %d times", id, n)
		}
	}
	var otherQueued int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM batch_shards WHERE zone=$1 AND status='queued'`, otherZone).Scan(&otherQueued); err != nil {
		t.Fatal(err)
	}
	if otherQueued != 1 {
		t.Fatalf("other zone queued = %d", otherQueued)
	}

	for i, shard := range claimed {
		body := fmt.Sprintf(`{"items":[{"target":"shared"},{"target":"s-%d"}]}`, i)
		if err := d.CompleteBatchShard(ctx, shard.ID, shard.LeaseOwner, []byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	var finWG sync.WaitGroup
	finErr := make(chan error, 4)
	for i := 0; i < 4; i++ {
		finWG.Add(1)
		go func() {
			defer finWG.Done()
			if _, _, err := d.TryFinalizeBatchJob(ctx, job.ID); err != nil {
				finErr <- err
			}
		}()
	}
	finWG.Wait()
	close(finErr)
	for err := range finErr {
		t.Fatal(err)
	}
	got, err := d.GetBatchJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.CompletedShards != 2 || got.FailedShards != 0 {
		t.Fatalf("final = %+v", got)
	}
	var summary struct {
		Items []struct {
			Target string `json:"target"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.ReducedSummary, &summary); err != nil {
		t.Fatal(err)
	}
	have := map[string]int{}
	for _, item := range summary.Items {
		have[item.Target]++
	}
	if have["shared"] != 1 || have["s-0"] != 1 || have["s-1"] != 1 || len(summary.Items) != 3 {
		t.Fatalf("summary = %s", got.ReducedSummary)
	}
}

func TestBatchLeaseReclaimAndCancel(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	ctx := context.Background()
	zone := fmt.Sprintf("breclaim-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM batch_jobs WHERE zone=$1`, zone)
	})
	job, err := d.CreateBatchJob(ctx, BatchCreate{
		Kind:      BatchKindFindingRetest,
		Zone:      zone,
		ShardSize: 1,
		Targets:   []string{"11", "11", "22"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if job.TotalTargets != 2 || job.TotalShards != 2 {
		t.Fatalf("deduped batch = %+v", job)
	}
	owner := "reclaim-owner"
	first, err := d.ClaimBatchShard(ctx, owner, zone, 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("claim = %+v err=%v", first, err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE batch_shards SET lease_until=now() - interval '1 minute' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReclaimExpiredBatchShards(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	var attempt int
	if err := d.QueryRowContext(ctx, `SELECT status, attempt FROM batch_shards WHERE id=$1`, first.ID).Scan(&status, &attempt); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempt != 1 {
		t.Fatalf("after reclaim status=%s attempt=%d", status, attempt)
	}
	second, err := d.ClaimBatchShard(ctx, owner, zone, 30*time.Second)
	if err != nil || second == nil || second.ID != first.ID || second.Attempt != 2 {
		t.Fatalf("second claim = %+v err=%v", second, err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE batch_shards SET attempt=max_attempts, lease_until=now() - interval '1 minute' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReclaimExpiredBatchShards(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRowContext(ctx, `SELECT status FROM batch_shards WHERE id=$1`, second.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("exhausted status=%s", status)
	}

	cancelZone := zone + "-cancel"
	t.Cleanup(func() {
		_, _ = d.ExecContext(context.Background(), `DELETE FROM batch_jobs WHERE zone=$1`, cancelZone)
	})
	cancelJob, err := d.CreateBatchJob(ctx, BatchCreate{
		Kind: BatchKindAssetRecon, Zone: cancelZone, ShardSize: 1, Targets: []string{"x", "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := d.CancelBatchJob(ctx, cancelJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "canceled" || canceled.CompletedShards != 0 {
		t.Fatalf("canceled = %+v", canceled)
	}
	var queued int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM batch_shards WHERE batch_id=$1 AND status<>'canceled'`, cancelJob.ID).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("uncanceled shards = %d", queued)
	}
}
