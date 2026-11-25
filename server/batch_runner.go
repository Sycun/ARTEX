package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/artex/db"
)

// BatchShardHandler runs one claimed shard and returns the JSON result stored
// on that shard. The object must contain an items array so fan-in can dedupe.
type BatchShardHandler interface {
	Execute(ctx context.Context, shard db.BatchShard) ([]byte, error)
}

type batchStore interface {
	ClaimBatchShard(ctx context.Context, workerID, zone string, leaseTTL time.Duration) (*db.BatchShard, error)
	RenewBatchShardLease(ctx context.Context, id int64, workerID string, leaseTTL time.Duration) error
	CompleteBatchShard(ctx context.Context, id int64, workerID string, result []byte) error
	FailBatchShard(ctx context.Context, id int64, workerID, lastError string) error
	TryFinalizeBatchJob(ctx context.Context, batchID string) (*db.BatchJob, bool, error)
}

// BatchRunner claims batch_shards until stop is closed.
type BatchRunner struct {
	db          batchStore
	id          string
	zone        string
	concurrency int
	leaseTTL    time.Duration
	claimWait   time.Duration
	exec        func(context.Context, db.BatchShard) ([]byte, error)
}

type assetReconHandler struct{ pg *db.DB }

func (h assetReconHandler) Execute(ctx context.Context, shard db.BatchShard) ([]byte, error) {
	var targets []string
	if err := json.Unmarshal(shard.Targets, &targets); err != nil {
		return nil, fmt.Errorf("asset_recon targets: %w", err)
	}
	items := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		rows, err := h.pg.QueryContext(ctx, `
SELECT id, type,
       COALESCE(domain,''), COALESCE(root_domain,''), COALESCE(ip,''), COALESCE(url,'')
FROM assets
WHERE domain=$1 OR root_domain=$1 OR ip=$1 OR url=$1
ORDER BY id
LIMIT 20`, target)
		if err != nil {
			return nil, err
		}
		assets := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var kind, domain, root, ip, rawURL string
			if err := rows.Scan(&id, &kind, &domain, &root, &ip, &rawURL); err != nil {
				rows.Close()
				return nil, err
			}
			assets = append(assets, map[string]any{
				"id": id, "type": kind, "domain": domain, "root_domain": root, "ip": ip, "url": rawURL,
			})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		items = append(items, map[string]any{
			"target":  target,
			"matched": len(assets) > 0,
			"assets":  assets,
		})
	}
	return json.Marshal(map[string]any{"items": items})
}

type findingRetestHandler struct{ pg *db.DB }

func (h findingRetestHandler) Execute(ctx context.Context, shard db.BatchShard) ([]byte, error) {
	var targets []string
	if err := json.Unmarshal(shard.Targets, &targets); err != nil {
		return nil, fmt.Errorf("finding_retest targets: %w", err)
	}
	items := make([]map[string]any, 0, len(targets))
	for _, target := range targets {
		id, err := strconv.ParseInt(target, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("finding id %q is invalid", target)
		}
		finding, err := h.pg.GetFinding(id)
		if err != nil {
			return nil, err
		}
		if finding == nil {
			items = append(items, map[string]any{
				"target": target, "finding_id": id, "verdict": "missing",
			})
			continue
		}
		verdict := "open"
		switch finding.Status {
		case "fixed", "resolved", "closed", "已修复":
			verdict = "passed"
		}
		items = append(items, map[string]any{
			"target":     target,
			"finding_id": finding.ID,
			"name":       finding.Name,
			"severity":   finding.Severity,
			"status":     finding.Status,
			"verdict":    verdict,
		})
	}
	return json.Marshal(map[string]any{"items": items})
}

func (s *Server) batchHandlers() map[string]BatchShardHandler {
	pg := s.m.pg
	return map[string]BatchShardHandler{
		db.BatchKindAssetRecon:    assetReconHandler{pg: pg},
		db.BatchKindFindingRetest: findingRetestHandler{pg: pg},
	}
}

func (s *Server) executeBatchShard(ctx context.Context, shard db.BatchShard) ([]byte, error) {
	if s.m == nil || s.m.pg == nil {
		return nil, fmt.Errorf("batch runner requires postgres")
	}
	job, err := s.m.pg.GetBatchJob(ctx, shard.BatchID)
	if err != nil {
		return nil, err
	}
	handler, ok := s.batchHandlers()[job.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown batch kind %q", job.Kind)
	}
	return handler.Execute(ctx, shard)
}

// RunLocalBatches drains batch_shards inside a --mode=all process.
func (s *Server) RunLocalBatches(stop context.Context) error {
	if s.m == nil || s.m.pg == nil {
		return fmt.Errorf("batch runner requires postgres")
	}
	id := s.workerID
	if id == "" {
		id = "local"
	}
	zone := s.workerZone
	if zone == "" {
		zone = db.ExecutionZoneName
	}
	n := s.workerConcurrency
	if n < 1 {
		n = 4
	}
	runner := &BatchRunner{
		db:          s.m.pg,
		id:          id,
		zone:        zone,
		concurrency: n,
		leaseTTL:    executionLeaseTTL,
		claimWait:   workerClaimWait,
		exec:        s.executeBatchShard,
	}
	log.Printf("[batch] local runner %s zone=%s concurrency=%d", id, zone, n)
	return runner.Run(stop)
}

func (b *BatchRunner) Run(stop context.Context) error {
	if b.db == nil || b.exec == nil {
		return fmt.Errorf("batch runner is incomplete")
	}
	if b.concurrency < 1 {
		b.concurrency = 1
	}
	if b.leaseTTL <= 0 {
		b.leaseTTL = executionLeaseTTL
	}
	if b.claimWait <= 0 {
		b.claimWait = workerClaimWait
	}
	if b.zone == "" {
		b.zone = db.ExecutionZoneName
	}
	slots := make(chan struct{}, b.concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select {
		case <-stop.Done():
			return nil
		case slots <- struct{}{}:
		}
		shard, err := b.db.ClaimBatchShard(stop, b.id, b.zone, b.leaseTTL)
		if err != nil {
			<-slots
			if stop.Err() != nil {
				return nil
			}
			return err
		}
		if shard == nil {
			<-slots
			if !waitClaim(stop, b.claimWait) {
				return nil
			}
			continue
		}
		wg.Add(1)
		go func(shard db.BatchShard) {
			defer wg.Done()
			defer func() { <-slots }()
			if err := b.finish(shard); err != nil {
				log.Printf("[batch] shard %d batch %s: %v", shard.ID, shard.BatchID, err)
			}
		}(*shard)
	}
}

func (b *BatchRunner) runOnce(ctx context.Context) (bool, error) {
	if b.leaseTTL <= 0 {
		b.leaseTTL = executionLeaseTTL
	}
	shard, err := b.db.ClaimBatchShard(ctx, b.id, b.zone, b.leaseTTL)
	if err != nil || shard == nil {
		return false, err
	}
	return true, b.finish(*shard)
}

func (b *BatchRunner) finish(shard db.BatchShard) error {
	if b.exec == nil {
		return fmt.Errorf("batch runner has no shard handler")
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	renewCtx, stopRenew := context.WithCancel(ctx)
	defer stopRenew()
	go b.renew(renewCtx, shard.ID, cancel)

	result, err := b.exec(ctx, shard)
	if errors.Is(context.Cause(ctx), db.ErrBatchLeaseLost) {
		return nil
	}
	if err != nil {
		if failErr := b.db.FailBatchShard(context.Background(), shard.ID, b.id, err.Error()); failErr != nil && !errors.Is(failErr, db.ErrBatchLeaseLost) {
			return failErr
		}
		_, _, ferr := b.db.TryFinalizeBatchJob(context.Background(), shard.BatchID)
		return errors.Join(err, ferr)
	}
	if completeErr := b.db.CompleteBatchShard(context.Background(), shard.ID, b.id, result); completeErr != nil {
		if errors.Is(completeErr, db.ErrBatchLeaseLost) {
			return nil
		}
		return completeErr
	}
	_, _, err = b.db.TryFinalizeBatchJob(context.Background(), shard.BatchID)
	return err
}

func (b *BatchRunner) renew(ctx context.Context, id int64, cancel context.CancelCauseFunc) {
	every := b.leaseTTL / 3
	if every < time.Second {
		every = time.Second
	}
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := b.db.RenewBatchShardLease(ctx, id, b.id, b.leaseTTL); err != nil {
				cancel(err)
				return
			}
		}
	}
}
