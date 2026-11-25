package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

type fakeExecutionDB struct {
	mu       sync.Mutex
	claims   int
	paused   []int64
	canceled []int64
	heart    []string
}

func (f *fakeExecutionDB) ClaimExecutionJob(ctx context.Context, workerID, zone string, leaseTTL time.Duration) (*db.ExecutionJob, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claims++
	if f.claims > 1 {
		return nil, nil
	}
	return &db.ExecutionJob{ID: 9, TaskID: 42, Zone: zone, Status: "running"}, nil
}

func (f *fakeExecutionDB) RenewExecutionJobLease(context.Context, int64, string, time.Duration) error {
	return nil
}

func (f *fakeExecutionDB) CompleteExecutionJob(context.Context, int64, string) error { return nil }

func (f *fakeExecutionDB) FailExecutionJob(context.Context, int64, string, string) error { return nil }

func (f *fakeExecutionDB) CancelExecutionJobByTask(_ context.Context, taskID int64) error {
	f.mu.Lock()
	f.canceled = append(f.canceled, taskID)
	f.mu.Unlock()
	return nil
}

func (f *fakeExecutionDB) PauseExecutionJobByTask(_ context.Context, taskID int64) error {
	f.mu.Lock()
	f.paused = append(f.paused, taskID)
	f.mu.Unlock()
	return nil
}

func (f *fakeExecutionDB) UpsertWorkerHeartbeat(_ context.Context, node db.WorkerNode) error {
	f.mu.Lock()
	f.heart = append(f.heart, node.Status)
	f.mu.Unlock()
	return nil
}

func TestWorkerRunnerControlCancelsInflight(t *testing.T) {
	for _, kind := range []ControlEventKind{ControlTaskPause, ControlTaskCancel} {
		t.Run(string(kind), func(t *testing.T) {
			store := &fakeExecutionDB{}
			bus := &MemoryBus{}
			started := make(chan struct{})
			finished := make(chan struct{})
			runner := &WorkerRunner{
				db:             store,
				bus:            bus,
				id:             "worker-a",
				zone:           "default",
				concurrency:    1,
				leaseTTL:       30 * time.Second,
				heartbeatEvery: time.Hour,
				claimWait:      10 * time.Millisecond,
				exec: func(ctx context.Context, job db.ExecutionJob) error {
					close(started)
					<-ctx.Done()
					close(finished)
					return ctx.Err()
				},
			}
			stop, cancel := context.WithCancel(context.Background())
			defer cancel()
			errCh := make(chan error, 1)
			go func() { errCh <- runner.Run(stop) }()

			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("job did not start")
			}
			if err := bus.PublishControl(ControlEvent{Kind: kind, TaskID: "99"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
				t.Fatal("unrelated task control canceled the job")
			case <-time.After(40 * time.Millisecond):
			}
			if err := bus.PublishControl(ControlEvent{Kind: kind, TaskID: "42"}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("in-flight job was not canceled")
			}
			cancel()
			select {
			case err := <-errCh:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("runner did not stop after the claim loop closed")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			switch kind {
			case ControlTaskPause:
				if len(store.paused) != 1 || store.paused[0] != 42 || len(store.canceled) != 0 {
					t.Fatalf("paused=%v canceled=%v", store.paused, store.canceled)
				}
			case ControlTaskCancel:
				if len(store.canceled) != 1 || store.canceled[0] != 42 || len(store.paused) != 0 {
					t.Fatalf("paused=%v canceled=%v", store.paused, store.canceled)
				}
			}
		})
	}
}
