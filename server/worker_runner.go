package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
)

const (
	executionLeaseTTL    = 30 * time.Second
	workerHeartbeatEvery = 5 * time.Second
	workerStaleAfter     = 20 * time.Second
	workerClaimWait      = 200 * time.Millisecond
)

var errTaskPaused = errors.New("task paused")

type executionDB interface {
	ClaimExecutionJob(ctx context.Context, workerID, zone string, leaseTTL time.Duration) (*db.ExecutionJob, error)
	RenewExecutionJobLease(ctx context.Context, id int64, workerID string, leaseTTL time.Duration) error
	CompleteExecutionJob(ctx context.Context, id int64, workerID string) error
	FailExecutionJob(ctx context.Context, id int64, workerID, lastError string) error
	CancelExecutionJobByTask(ctx context.Context, taskID int64) error
	PauseExecutionJobByTask(ctx context.Context, taskID int64) error
	UpsertWorkerHeartbeat(ctx context.Context, node db.WorkerNode) error
}

// WorkerRunner claims execution jobs and drives them until the stop context
// closes. In-flight jobs keep running after stop so the process can drain.
type WorkerRunner struct {
	db             executionDB
	bus            EventBus
	id             string
	zone           string
	concurrency    int
	leaseTTL       time.Duration
	heartbeatEvery time.Duration
	claimWait      time.Duration
	exec           func(context.Context, db.ExecutionJob) error

	active   atomic.Int32
	draining atomic.Bool
	started  time.Time

	mu       sync.Mutex
	inflight map[string]*runningJob
	pending  map[string]ControlEventKind
}

type runningJob struct {
	cancel context.CancelCauseFunc
	kind   ControlEventKind
}

func (s *Server) RunWorker(stop context.Context) error {
	if s.mode != ModeWorker {
		return fmt.Errorf("server mode %q cannot run the worker loop", s.mode)
	}
	if s.m == nil || s.m.pg == nil {
		return fmt.Errorf("worker requires postgres")
	}
	if s.workerID == "" {
		return fmt.Errorf("worker id is empty")
	}
	zone := s.workerZone
	if zone == "" {
		zone = db.ExecutionZoneName
	}
	n := s.workerConcurrency
	if n < 1 {
		n = 4
	}
	runner := &WorkerRunner{
		db:             s.m.pg,
		bus:            s.EventBus(),
		id:             s.workerID,
		zone:           zone,
		concurrency:    n,
		leaseTTL:       executionLeaseTTL,
		heartbeatEvery: workerHeartbeatEvery,
		claimWait:      workerClaimWait,
		exec:           s.executeClaimedJob,
	}
	return runner.Run(stop)
}

func (s *Server) executeClaimedJob(ctx context.Context, job db.ExecutionJob) error {
	task, err := s.m.EnsureTask(job.TaskID)
	if err != nil {
		return err
	}
	mode := "resume"
	snap := task.lifecycleSnapshot()
	if snap.QueueMode == "bootstrap" && snap.FirstRunAt == 0 {
		mode = "bootstrap"
	}
	s.runAdmittedTask(task, mode)
	err = waitTaskExecution(ctx, task)
	s.engine.StopTask(task.ID)
	return err
}

func waitTaskExecution(ctx context.Context, task *Task) error {
	tick := time.NewTicker(300 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state := task.lifecycleSnapshot()
		switch {
		case isTerminalStatus(state.Status):
			return nil
		case state.Paused:
			return errTaskPaused
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (w *WorkerRunner) Run(stop context.Context) error {
	if w.db == nil || w.bus == nil || w.exec == nil {
		return fmt.Errorf("worker runner is incomplete")
	}
	if w.concurrency < 1 {
		w.concurrency = 1
	}
	if w.leaseTTL <= 0 {
		w.leaseTTL = executionLeaseTTL
	}
	if w.heartbeatEvery <= 0 {
		w.heartbeatEvery = workerHeartbeatEvery
	}
	if w.claimWait <= 0 {
		w.claimWait = workerClaimWait
	}
	if w.zone == "" {
		w.zone = db.ExecutionZoneName
	}
	w.started = time.Now()
	w.inflight = map[string]*runningJob{}
	w.pending = map[string]ControlEventKind{}

	life, cancelLife := context.WithCancel(context.Background())
	defer cancelLife()
	if err := w.beat(life); err != nil {
		return err
	}
	heartErr := make(chan error, 1)
	go w.heartbeat(life, heartErr)

	events, unsub := w.bus.SubscribeControl()
	defer unsub()
	go func() {
		for {
			select {
			case <-life.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				w.onControl(event)
			}
		}
	}()

	claimErr := w.claimLoop(stop, heartErr)
	w.draining.Store(true)
	_ = w.beat(context.Background())
	w.waitIdle()
	cancelLife()
	return claimErr
}

func (w *WorkerRunner) heartbeat(ctx context.Context, heartErr chan<- error) {
	tick := time.NewTicker(w.heartbeatEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.beat(ctx); err != nil && ctx.Err() == nil {
				heartErr <- err
				return
			}
		}
	}
}

func (w *WorkerRunner) beat(ctx context.Context) error {
	status := "online"
	if w.draining.Load() {
		status = "draining"
	}
	return w.db.UpsertWorkerHeartbeat(ctx, db.WorkerNode{
		ID:          w.id,
		Zone:        w.zone,
		Concurrency: w.concurrency,
		ActiveJobs:  int(w.active.Load()),
		Status:      status,
		StartedAt:   w.started,
	})
}

func (w *WorkerRunner) claimLoop(stop context.Context, heartErr <-chan error) error {
	slots := make(chan struct{}, w.concurrency)
	var wg sync.WaitGroup
	defer func() {
		w.draining.Store(true)
		wg.Wait()
	}()
	for {
		select {
		case <-stop.Done():
			return nil
		case err := <-heartErr:
			return err
		case slots <- struct{}{}:
		}
		job, err := w.db.ClaimExecutionJob(stop, w.id, w.zone, w.leaseTTL)
		if err != nil {
			<-slots
			if stop.Err() != nil {
				return nil
			}
			return err
		}
		if job == nil {
			<-slots
			if !waitClaim(stop, w.claimWait) {
				return nil
			}
			continue
		}
		wg.Add(1)
		w.active.Add(1)
		go func(job db.ExecutionJob) {
			defer wg.Done()
			defer w.active.Add(-1)
			defer func() { <-slots }()
			if err := w.runOne(job); err != nil {
				log.Printf("[worker] job %d task %d: %v", job.ID, job.TaskID, err)
			}
		}(*job)
	}
}

func waitClaim(stop context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-stop.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (w *WorkerRunner) waitIdle() {
	for {
		if w.active.Load() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (w *WorkerRunner) runOne(job db.ExecutionJob) error {
	taskID := strconv.FormatInt(job.TaskID, 10)
	jobCtx, cancel := context.WithCancelCause(context.Background())
	w.track(taskID, cancel)
	defer w.untrack(taskID)
	defer cancel(nil)

	renewCtx, stopRenew := context.WithCancel(jobCtx)
	defer stopRenew()
	go w.renew(renewCtx, job.ID, cancel)

	err := w.exec(jobCtx, job)
	kind := w.outcome(taskID)
	switch kind {
	case ControlTaskPause:
		return w.db.PauseExecutionJobByTask(context.Background(), job.TaskID)
	case ControlTaskCancel, ControlTaskDelete:
		return w.db.CancelExecutionJobByTask(context.Background(), job.TaskID)
	}
	if errors.Is(err, errTaskPaused) {
		return w.db.PauseExecutionJobByTask(context.Background(), job.TaskID)
	}
	if err == nil {
		if completeErr := w.db.CompleteExecutionJob(context.Background(), job.ID, w.id); completeErr != nil && !errors.Is(completeErr, db.ErrExecutionLeaseLost) {
			return completeErr
		}
		return nil
	}
	if errors.Is(err, db.ErrExecutionLeaseLost) || errors.Is(context.Cause(jobCtx), db.ErrExecutionLeaseLost) {
		return nil
	}
	if failErr := w.db.FailExecutionJob(context.Background(), job.ID, w.id, err.Error()); failErr != nil && !errors.Is(failErr, db.ErrExecutionLeaseLost) {
		return failErr
	}
	return err
}

func (w *WorkerRunner) renew(ctx context.Context, id int64, cancel context.CancelCauseFunc) {
	every := w.leaseTTL / 3
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
			if err := w.db.RenewExecutionJobLease(ctx, id, w.id, w.leaseTTL); err != nil {
				cancel(err)
				return
			}
		}
	}
}

func (w *WorkerRunner) onControl(event ControlEvent) {
	switch event.Kind {
	case ControlTaskPause, ControlTaskCancel, ControlTaskDelete:
	default:
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	running := w.inflight[event.TaskID]
	if running == nil {
		w.pending[event.TaskID] = event.Kind
		return
	}
	running.kind = event.Kind
	running.cancel(errors.New(string(event.Kind)))
}

func (w *WorkerRunner) track(taskID string, cancel context.CancelCauseFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	running := &runningJob{cancel: cancel}
	if kind := w.pending[taskID]; kind != "" {
		running.kind = kind
		delete(w.pending, taskID)
		cancel(errors.New(string(kind)))
	}
	w.inflight[taskID] = running
}

func (w *WorkerRunner) untrack(taskID string) {
	w.mu.Lock()
	delete(w.inflight, taskID)
	w.mu.Unlock()
}

func (w *WorkerRunner) outcome(taskID string) ControlEventKind {
	w.mu.Lock()
	defer w.mu.Unlock()
	if running := w.inflight[taskID]; running != nil {
		return running.kind
	}
	return ""
}
