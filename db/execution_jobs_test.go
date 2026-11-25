package db

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestExecutionJobZoneClaimIsolation(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	defTask := mustExecutionTask(t, d, "default", stamp)
	inTask := mustExecutionTask(t, d, "intranet", stamp+1)
	t.Cleanup(func() {
		_ = d.DeleteTask(defTask)
		_ = d.DeleteTask(inTask)
	})
	if _, err := d.EnqueueExecutionJob(ctx, defTask, "", "default"); err != nil {
		t.Fatal(err)
	}
	again, err := d.EnqueueExecutionJob(ctx, defTask, "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != "queued" || again.Attempt != 0 {
		t.Fatalf("idempotent enqueue = %+v", again)
	}
	if _, err := d.EnqueueExecutionJob(ctx, inTask, "", "intranet"); err != nil {
		t.Fatal(err)
	}

	intranet, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-in-%d", stamp), "intranet", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if intranet == nil || intranet.TaskID != inTask || intranet.Zone != "intranet" || intranet.Status != "running" || intranet.Attempt != 1 {
		t.Fatalf("intranet claim = %+v", intranet)
	}
	none, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-in2-%d", stamp), "intranet", 30*time.Second)
	if err != nil || none != nil {
		t.Fatalf("second intranet claim = %+v err=%v", none, err)
	}
	def, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-def-%d", stamp), "default", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if def == nil || def.TaskID != defTask || def.LeaseOwner == "" || def.LeaseUntil == nil {
		t.Fatalf("default claim = %+v", def)
	}

	const n = 6
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = mustExecutionTask(t, d, "default", stamp+int64(10+i))
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_ = d.DeleteTask(id)
		}
	})
	for _, id := range ids {
		if _, err := d.EnqueueExecutionJob(ctx, id, "", "default"); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	got := make(chan int64, n*2)
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			job, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-race-%d-%d", stamp, i), "default", 30*time.Second)
			if err != nil {
				errCh <- err
				return
			}
			if job != nil {
				got <- job.TaskID
			}
		}(i)
	}
	wg.Wait()
	close(got)
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
	seen := map[int64]int{}
	for id := range got {
		seen[id]++
	}
	if len(seen) != n {
		t.Fatalf("claimed %d distinct jobs, want %d (%v)", len(seen), n, seen)
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("task %d claimed %d times", id, count)
		}
	}
	extra, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-extra-%d", stamp), "default", 30*time.Second)
	if err != nil || extra != nil {
		t.Fatalf("claim after drain = %+v err=%v", extra, err)
	}
}

func TestExecutionJobLeaseReclaim(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	ctx := context.Background()
	stamp := time.Now().UnixNano()
	taskID := mustExecutionTask(t, d, "default", stamp)
	t.Cleanup(func() { _ = d.DeleteTask(taskID) })
	if _, err := d.EnqueueExecutionJob(ctx, taskID, "", "default"); err != nil {
		t.Fatal(err)
	}
	ownerA := fmt.Sprintf("exec-test-a-%d", stamp)
	first, err := d.ClaimExecutionJob(ctx, ownerA, "default", 30*time.Second)
	if err != nil || first == nil {
		t.Fatalf("claim = %+v err=%v", first, err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE execution_jobs SET lease_until=now() - interval '1 minute' WHERE id=$1`, first.ID); err != nil {
		t.Fatal(err)
	}
	requeued, failed, err := d.ReclaimExpiredExecutionJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 1 || failed != 0 {
		t.Fatalf("reclaim requeued=%d failed=%d", requeued, failed)
	}
	ownerB := fmt.Sprintf("exec-test-b-%d", stamp)
	second, err := d.ClaimExecutionJob(ctx, ownerB, "default", 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second == nil || second.ID != first.ID || second.TaskID != taskID || second.LeaseOwner != ownerB || second.Attempt != 2 {
		t.Fatalf("reclaimed claim = %+v", second)
	}
	if err := d.RenewExecutionJobLease(ctx, second.ID, ownerB, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ExecContext(ctx, `UPDATE execution_jobs SET attempt=max_attempts, lease_until=now() - interval '1 minute' WHERE id=$1`, second.ID); err != nil {
		t.Fatal(err)
	}
	requeued, failed, err = d.ReclaimExpiredExecutionJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if requeued != 0 || failed != 1 {
		t.Fatalf("exhausted reclaim requeued=%d failed=%d", requeued, failed)
	}
	third, err := d.ClaimExecutionJob(ctx, fmt.Sprintf("exec-test-c-%d", stamp), "default", 30*time.Second)
	if err != nil || third != nil {
		t.Fatalf("claim after failure = %+v err=%v", third, err)
	}
	var status string
	if err := d.QueryRowContext(ctx, `SELECT status FROM execution_jobs WHERE id=$1`, second.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("status=%s", status)
	}
}

func mustExecutionTask(t *testing.T, d *DB, zone string, stamp int64) int64 {
	t.Helper()
	task, err := d.CreateTaskWithOptions("execution job", "goal", TaskCreateOptions{Name: fmt.Sprintf("exec-%s-%d", zone, stamp), Zone: zone})
	if err != nil {
		t.Fatal(err)
	}
	if task.Zone != zone {
		t.Fatalf("stored zone=%s want %s", task.Zone, zone)
	}
	return task.ID
}
