package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestBatchRunnerFanIn(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	ctx := context.Background()
	zone := fmt.Sprintf("sbatch-%d", time.Now().UnixNano())
	job, err := pg.CreateBatchJob(ctx, db.BatchCreate{
		Kind:      db.BatchKindAssetRecon,
		Zone:      zone,
		ShardSize: 1,
		Targets:   []string{"batch-host-a", "batch-host-a", "batch-host-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.ExecContext(context.Background(), `DELETE FROM batch_jobs WHERE id=$1`, job.ID)
	})
	if job.TotalTargets != 2 || job.TotalShards != 2 {
		t.Fatalf("fan-out = %+v", job)
	}
	s := &Server{ctx: ctx, m: &Manager{pg: pg}}
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runner := &BatchRunner{
				db:       pg,
				id:       fmt.Sprintf("srv-batch-%d-%d", i, time.Now().UnixNano()),
				zone:     zone,
				leaseTTL: 30 * time.Second,
				exec:     s.executeBatchShard,
			}
			for {
				ok, err := runner.runOnce(ctx)
				if err != nil {
					errCh <- err
					return
				}
				if !ok {
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	got, err := pg.GetBatchJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.CompletedShards != 2 {
		t.Fatalf("reduced batch = %+v summary=%s", got, got.ReducedSummary)
	}
	var summary struct {
		Items []struct {
			Target  string `json:"target"`
			Matched bool   `json:"matched"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.ReducedSummary, &summary); err != nil {
		t.Fatal(err)
	}
	have := map[string]int{}
	for _, item := range summary.Items {
		have[item.Target]++
	}
	if have["batch-host-a"] != 1 || have["batch-host-b"] != 1 || len(summary.Items) != 2 {
		t.Fatalf("summary = %s", got.ReducedSummary)
	}
}

func TestFindingRetestShardReduce(t *testing.T) {
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	pg, err := db.Open(dsn)
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { pg.Close() })
	ctx := context.Background()
	findingID, err := pg.AddFinding(0, 0, "batch-retest", "批次复测", "high", "summary", "evidence", "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.DeleteFinding(findingID) })
	target := strconv.FormatInt(findingID, 10)
	zone := fmt.Sprintf("sretest-%d", time.Now().UnixNano())
	job, err := pg.CreateBatchJob(ctx, db.BatchCreate{
		Kind:      db.BatchKindFindingRetest,
		Zone:      zone,
		ShardSize: 2,
		Targets:   []string{target, target, " "},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pg.ExecContext(context.Background(), `DELETE FROM batch_jobs WHERE id=$1`, job.ID)
	})
	if job.TotalTargets != 1 || job.TotalShards != 1 {
		t.Fatalf("retest fan-out = %+v", job)
	}
	s := &Server{ctx: ctx, m: &Manager{pg: pg}}
	runner := &BatchRunner{
		db:       pg,
		id:       "srv-retest",
		zone:     zone,
		leaseTTL: 30 * time.Second,
		exec:     s.executeBatchShard,
	}
	ok, err := runner.runOnce(ctx)
	if err != nil || !ok {
		t.Fatalf("runOnce ok=%v err=%v", ok, err)
	}
	got, err := pg.GetBatchJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("status=%s summary=%s", got.Status, got.ReducedSummary)
	}
	var summary struct {
		Items []struct {
			FindingID int64  `json:"finding_id"`
			Verdict   string `json:"verdict"`
			Severity  string `json:"severity"`
		} `json:"items"`
	}
	if err := json.Unmarshal(got.ReducedSummary, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Items) != 1 || summary.Items[0].FindingID != findingID || summary.Items[0].Verdict != "open" || summary.Items[0].Severity != "high" {
		t.Fatalf("summary = %s", got.ReducedSummary)
	}
}
